// Package management provides the management API handlers and middleware
// for configuring the server and managing auth files.
package management

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"golang.org/x/crypto/bcrypt"
)

type attemptInfo struct {
	count        int
	blockedUntil time.Time
	lastActivity time.Time // track last activity for cleanup
}

// attemptCleanupInterval controls how often stale IP entries are purged
const attemptCleanupInterval = 1 * time.Hour

// attemptMaxIdleTime controls how long an IP can be idle before cleanup
const attemptMaxIdleTime = 2 * time.Hour

// Handler aggregates config reference, persistence path and helpers.
type Handler struct {
	cfg                 *config.Config
	configFilePath      string
	mu                  sync.Mutex
	attemptsMu          sync.Mutex
	failedAttempts      map[string]*attemptInfo // keyed by client IP
	authManager         *coreauth.Manager
	usageStats          *usage.RequestStatistics
	usagePersister      *usage.Persister
	tokenStore          coreauth.Store
	localPassword       string
	allowRemoteOverride bool
	envSecret           string
	logDir              string
	postAuthHook        coreauth.PostAuthHook
}

// Option configures a Handler.
type Option func(*Handler)

// WithEnvSecret overrides the management password (default: from MANAGEMENT_PASSWORD env).
func WithEnvSecret(secret string) Option {
	return func(h *Handler) { h.envSecret = secret; h.allowRemoteOverride = secret != "" }
}

// WithUsageStats overrides the usage statistics source.
func WithUsageStats(stats *usage.RequestStatistics) Option {
	return func(h *Handler) { h.usageStats = stats }
}

// WithTokenStore overrides the token store.
func WithTokenStore(store coreauth.Store) Option {
	return func(h *Handler) { h.tokenStore = store }
}

// WithUsagePersister sets the usage persister.
func WithUsagePersister(p *usage.Persister) Option {
	return func(h *Handler) { h.usagePersister = p }
}

// WithLogDir sets the log directory.
func WithLogDir(dir string) Option {
	return func(h *Handler) { h.logDir = dir }
}

// WithPostAuthHook sets the post-auth hook.
func WithPostAuthHook(hook coreauth.PostAuthHook) Option {
	return func(h *Handler) { h.postAuthHook = hook }
}

// NewHandler creates a new management handler instance.
func NewHandler(cfg *config.Config, configFilePath string, manager *coreauth.Manager, opts ...Option) *Handler {
	envSecret, _ := os.LookupEnv("MANAGEMENT_PASSWORD")
	envSecret = strings.TrimSpace(envSecret)

	h := &Handler{
		cfg:                 cfg,
		configFilePath:      configFilePath,
		failedAttempts:      make(map[string]*attemptInfo),
		authManager:         manager,
		usageStats:          usage.GetRequestStatistics(),
		tokenStore:          sdkAuth.GetTokenStore(),
		allowRemoteOverride: envSecret != "",
		envSecret:           envSecret,
	}

	for _, opt := range opts {
		opt(h)
	}

	h.startAttemptCleanup()
	return h
}

// startAttemptCleanup launches a background goroutine that periodically
// removes stale IP entries from failedAttempts to prevent memory leaks.
func (h *Handler) startAttemptCleanup() {
	go func() {
		ticker := time.NewTicker(attemptCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			h.purgeStaleAttempts()
		}
	}()
}

// purgeStaleAttempts removes IP entries that have been idle beyond attemptMaxIdleTime
// and whose ban (if any) has expired.
func (h *Handler) purgeStaleAttempts() {
	now := time.Now()
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()
	for ip, ai := range h.failedAttempts {
		// Skip if still banned
		if !ai.blockedUntil.IsZero() && now.Before(ai.blockedUntil) {
			continue
		}
		// Remove if idle too long
		if now.Sub(ai.lastActivity) > attemptMaxIdleTime {
			delete(h.failedAttempts, ip)
		}
	}
}

// NewHandlerWithoutConfigFilePath creates a management handler without a config file path.
func NewHandlerWithoutConfigFilePath(cfg *config.Config, manager *coreauth.Manager, opts ...Option) *Handler {
	return NewHandler(cfg, "", manager, opts...)
}

// SetConfig updates the in-memory config reference when the server hot-reloads.
func (h *Handler) SetConfig(cfg *config.Config) { h.cfg = cfg }

// SetAuthManager updates the auth manager reference used by management endpoints.
func (h *Handler) SetAuthManager(manager *coreauth.Manager) { h.authManager = manager }

// SetLocalPassword configures the runtime-local password accepted for localhost requests.
func (h *Handler) SetLocalPassword(password string) { h.localPassword = password }

// SetLogDirectory updates the directory where main.log should be looked up.
func (h *Handler) SetLogDirectory(dir string) {
	if dir == "" {
		return
	}
	if !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
	}
	h.logDir = dir
}

// SetPostAuthHook registers a hook to be called after auth record creation but before persistence.
func (h *Handler) SetPostAuthHook(hook coreauth.PostAuthHook) {
	h.postAuthHook = hook
}

// SetUsagePersister sets the usage statistics persister reference.
func (h *Handler) SetUsagePersister(p *usage.Persister) { h.usagePersister = p }

// Middleware enforces access control for management endpoints.
// All requests (local and remote) require a valid management key.
// Additionally, remote access requires allow-remote-management=true.
func (h *Handler) Middleware() gin.HandlerFunc {
	const maxFailures = 5
	const banDuration = 30 * time.Minute

	return func(c *gin.Context) {
		c.Header("X-CPA-VERSION", buildinfo.Version)
		c.Header("X-CPA-COMMIT", buildinfo.Commit)
		c.Header("X-CPA-BUILD-DATE", buildinfo.BuildDate)

		// Use RemoteAddr to prevent X-Forwarded-For spoofing (consistent with amp module)
		remoteHost, _, _ := net.SplitHostPort(c.Request.RemoteAddr)
		if remoteHost == "" {
			remoteHost = c.Request.RemoteAddr
		}
		clientIP := remoteHost
		localClient := false
		if ip := net.ParseIP(remoteHost); ip != nil {
			localClient = ip.IsLoopback()
		}
		cfg := h.cfg
		var (
			allowRemote bool
			secretHash  string
		)
		if cfg != nil {
			allowRemote = cfg.RemoteManagement.AllowRemote
			secretHash = cfg.RemoteManagement.SecretKey
		}
		if h.allowRemoteOverride {
			allowRemote = true
		}
		envSecret := h.envSecret

		h.attemptsMu.Lock()
		ai := h.failedAttempts[clientIP]
		if ai != nil {
			if !ai.blockedUntil.IsZero() {
				if time.Now().Before(ai.blockedUntil) {
					remaining := time.Until(ai.blockedUntil).Round(time.Second)
					h.attemptsMu.Unlock()
					c.AbortWithStatusJSON(
						http.StatusForbidden, gin.H{
							"error": fmt.Sprintf(
								"IP banned due to too many failed attempts. Try again in %s", remaining,
							),
						},
					)
					return
				}
				// Ban expired, reset state
				ai.blockedUntil = time.Time{}
				ai.count = 0
			}
		}
		h.attemptsMu.Unlock()

		if !localClient && !allowRemote {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "remote management disabled"})
			return
		}

		fail := func() {
			h.attemptsMu.Lock()
			aip := h.failedAttempts[clientIP]
			if aip == nil {
				aip = &attemptInfo{}
				h.failedAttempts[clientIP] = aip
			}
			aip.count++
			aip.lastActivity = time.Now()
			if aip.count >= maxFailures {
				aip.blockedUntil = time.Now().Add(banDuration)
				aip.count = 0
			}
			h.attemptsMu.Unlock()
		}
		if secretHash == "" && envSecret == "" {
			if localClient && h.localPassword != "" {
				// Allow local requests to proceed — localPassword will be validated below
			} else {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "remote management key not set"})
				return
			}
		}

		// Accept either Authorization: Bearer <key> or X-Management-Key
		var provided string
		if ah := c.GetHeader("Authorization"); ah != "" {
			parts := strings.SplitN(ah, " ", 2)
			if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
				provided = parts[1]
			} else {
				provided = ah
			}
		}
		if provided == "" {
			provided = c.GetHeader("X-Management-Key")
		}

		if provided == "" {
			fail()
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing management key"})
			return
		}

		if localClient {
			if lp := h.localPassword; lp != "" {
				if subtle.ConstantTimeCompare([]byte(provided), []byte(lp)) == 1 {
					c.Next()
					return
				}
			}
		}

		if envSecret != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(envSecret)) == 1 {
			h.attemptsMu.Lock()
			if ai := h.failedAttempts[clientIP]; ai != nil {
				ai.count = 0
				ai.blockedUntil = time.Time{}
			}
			h.attemptsMu.Unlock()
			c.Next()
			return
		}

		if secretHash == "" || bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(provided)) != nil {
			fail()
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid management key"})
			return
		}

		h.attemptsMu.Lock()
		if ai := h.failedAttempts[clientIP]; ai != nil {
			ai.count = 0
			ai.blockedUntil = time.Time{}
		}
		h.attemptsMu.Unlock()

		c.Next()
	}
}

// persist saves the current in-memory config to disk.
func (h *Handler) persist(c *gin.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	// Preserve comments when writing
	if err := config.SaveConfigPreserveComments(h.configFilePath, h.cfg); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to save config: %v", err)})
		return false
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	return true
}

// Helper methods for simple types
func (h *Handler) updateBoolField(c *gin.Context, set func(bool)) {
	var body struct {
		Value *bool `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	set(*body.Value)
	h.persist(c)
}

func (h *Handler) updateIntField(c *gin.Context, set func(int)) {
	var body struct {
		Value *int `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	set(*body.Value)
	h.persist(c)
}

func (h *Handler) updateStringField(c *gin.Context, set func(string)) {
	var body struct {
		Value *string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	set(*body.Value)
	h.persist(c)
}
