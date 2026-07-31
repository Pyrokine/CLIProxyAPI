// Package management provides the management API handlers and middleware
// for configuring the server and managing auth files.
package management

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/artifact"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

type attemptInfo struct {
	failures              []time.Time // sliding window of failure timestamps
	blockedUntil          time.Time
	lastActivity          time.Time // track last activity for cleanup
	banCount              int
	pendingNoticeFailures int
	lastFailureAt         time.Time
	lastSuccessfulAuthAt  time.Time
}

const (
	managementFailureWindow      = 5 * time.Minute
	managementMaxFailures        = 10
	managementBanDuration        = 30 * time.Minute
	managementAuthFailureMessage = "Unauthorized"
	managementRateLimitMessage   = "too many authentication failures, please try again later"
)

// attemptCleanupInterval controls how often stale IP entries are purged
const attemptCleanupInterval = 1 * time.Hour

// attemptMaxIdleTime controls how long an IP can be idle before cleanup
const attemptMaxIdleTime = 2 * time.Hour

type pluginRuntimeHost interface {
	RegisteredPlugins() []pluginhost.RegisteredPluginInfo
	PluginBusy(string) bool
	PluginRuntimeIdentity(string) pluginhost.PluginRuntimeIdentity
	UnloadPluginWithError(string) (bool, error)
	UnloadPluginIfCurrentWithError(pluginhost.PluginRuntimeIdentity) (bool, error)
}

// ConfigCommit identifies a successfully persisted management config generation.
type ConfigCommit struct {
	Config                 *config.Config
	Generation             uint64
	ConfigFileHash         string
	DeferPluginRuntimeSync bool
}

// ConfigCommittedFunc applies a persisted management config to the running server.
type ConfigCommittedFunc func(context.Context, ConfigCommit) error

// Handler aggregates config reference, persistence path and helpers.
type Handler struct {
	cfg                     *config.Config
	configFilePath          string
	commitMu                sync.Mutex
	runtimeCommitMu         sync.Mutex
	mu                      sync.RWMutex
	configGeneration        uint64
	configCommitted         ConfigCommittedFunc
	configYAMLKeyMu         sync.Mutex
	configYAMLKey           [32]byte
	configYAMLKeyLoaded     bool
	apiKeyAliasesMu         sync.RWMutex
	apiKeyAliases           map[string]string
	attemptsMu              sync.Mutex
	failedAttempts          map[string]*attemptInfo // keyed by client IP
	rateLimitUnbanHistory   []rateLimitUnbanHistoryEntry
	authManager             *coreauth.Manager
	usageStats              *usage.RequestStatistics
	usagePersister          *usage.Persister
	usageQueue              *redisqueue.Queue
	imageArtifacts          *artifact.Store
	tokenStore              coreauth.Store
	localPassword           string
	envSecret               string
	logDir                  string
	postAuthHook            coreauth.PostAuthHook
	quotaScheduler          *quota.Scheduler
	pluginHost              pluginRuntimeHost
	pluginRuntimeSync       func(context.Context, *config.Config) error
	pluginStoreRegistryURL  string
	pluginStoreHTTPClient   pluginstore.HTTPDoer
	pluginReleaseCacheMu    sync.Mutex
	pluginReleaseCache      map[string]pluginReleaseCacheEntry
	modelCatalogRuntimeSync func(*config.Config)
	accountInspection       *accountInspectionService
	stateStore              *managementStateStore
	internalToken           string // random token for internal localhost requests (quota scheduler)
	attemptCleanupCancel    context.CancelFunc
	attemptCleanupWG        sync.WaitGroup
	closeOnce               sync.Once
	closeErr                error
}

// Option configures a Handler.
type Option func(*Handler)

// WithEnvSecret overrides the management password (default: from MANAGEMENT_PASSWORD env).
func WithEnvSecret(secret string) Option {
	return func(h *Handler) { h.envSecret = strings.TrimSpace(secret) }
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

// WithUsageQueue sets the service-owned usage output queue.
func WithUsageQueue(queue *redisqueue.Queue) Option {
	return func(h *Handler) { h.usageQueue = queue }
}

// WithImageArtifacts sets the image artifact store.
func WithImageArtifacts(store *artifact.Store) Option {
	return func(h *Handler) { h.imageArtifacts = store }
}

// WithQuotaScheduler sets the quota refresh scheduler.
func WithQuotaScheduler(s *quota.Scheduler) Option {
	return func(h *Handler) { h.quotaScheduler = s }
}

// WithConfigCommitted synchronizes persisted management config commits with the running server.
func WithConfigCommitted(callback ConfigCommittedFunc) Option {
	return func(h *Handler) { h.configCommitted = callback }
}

// WithPluginHost sets the dynamic plugin host used by management plugin endpoints.
func WithPluginHost(host *pluginhost.Host) Option {
	return func(h *Handler) { h.pluginHost = host }
}

// WithPluginRuntimeSync refreshes plugin runtime state after plugin config mutations.
func WithPluginRuntimeSync(sync func(context.Context, *config.Config) error) Option {
	return func(h *Handler) { h.pluginRuntimeSync = sync }
}

func WithModelCatalogRuntimeSync(sync func(*config.Config)) Option {
	return func(h *Handler) { h.modelCatalogRuntimeSync = sync }
}

// WithLogDir sets the log directory.
func WithLogDir(dir string) Option {
	return func(h *Handler) { h.logDir = dir }
}

// NewHandler creates a new management handler instance.
func NewHandler(cfg *config.Config, configFilePath string, manager *coreauth.Manager, opts ...Option) *Handler {
	envSecret, _ := os.LookupEnv("MANAGEMENT_PASSWORD")
	envSecret = strings.TrimSpace(envSecret)

	h := &Handler{
		cfg:                cfg,
		configFilePath:     configFilePath,
		usageQueue:         redisqueue.DefaultQueue(),
		apiKeyAliases:      copyAPIKeyAliases(configAPIKeyAliases(cfg)),
		failedAttempts:     make(map[string]*attemptInfo),
		authManager:        manager,
		usageStats:         usage.GetRequestStatistics(),
		tokenStore:         sdkAuth.GetTokenStore(),
		envSecret:          envSecret,
		internalToken:      generateInternalToken(),
		pluginReleaseCache: make(map[string]pluginReleaseCacheEntry),
	}

	for _, opt := range opts {
		opt(h)
	}
	stateStore, err := openManagementStateStore(configFilePath)
	if err != nil {
		log.WithError(err).Warn("management state persistence disabled")
	} else {
		h.stateStore = stateStore
	}
	h.accountInspection = newAccountInspectionService(h)
	h.configureQuotaScheduler(h.quotaScheduler)

	h.startAttemptCleanup()
	return h
}

// generateInternalToken creates a random 32-byte hex token for internal requests.
func generateInternalToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("crypto/rand.Read failed, system entropy source is broken: %v", err)
	}
	return hex.EncodeToString(b)
}

// InternalToken returns the token for authenticating internal localhost requests.
func (h *Handler) InternalToken() string {
	return h.internalToken
}

// SetQuotaScheduler sets the quota refresh scheduler after handler creation.
// Must be called before Server.Start() — not safe for concurrent use.
func (h *Handler) SetQuotaScheduler(s *quota.Scheduler) {
	h.quotaScheduler = s
	h.configureQuotaScheduler(s)
}

func (h *Handler) Close() error {
	if h == nil {
		return nil
	}
	h.closeOnce.Do(
		func() {
			if h.attemptCleanupCancel != nil {
				h.attemptCleanupCancel()
			}
			h.attemptCleanupWG.Wait()
			if h.accountInspection != nil {
				h.accountInspection.close()
			}
			if h.stateStore != nil {
				h.closeErr = h.stateStore.Close()
			}
		},
	)
	return h.closeErr
}

func (h *Handler) configureQuotaScheduler(s *quota.Scheduler) {
	if h == nil || s == nil || h.stateStore == nil {
		return
	}
	s.SetPersistence(h.saveQuotaEntry, h.deleteQuotaEntry)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries, err := h.stateStore.loadQuotaEntries(ctx)
	if err != nil {
		log.WithError(err).Warn("failed to load quota state")
		return
	}
	s.Restore(entries)
}

func (h *Handler) saveQuotaEntry(entry quota.EntryState) {
	if h == nil || h.stateStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.stateStore.saveQuotaEntry(ctx, entry); err != nil {
		log.WithError(err).Warn("failed to save quota state")
	}
}

func (h *Handler) deleteQuotaEntry(entryKey string) {
	if h == nil || h.stateStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.stateStore.deleteQuotaEntry(ctx, entryKey); err != nil {
		log.WithError(err).Warn("failed to delete quota state")
	}
}

// startAttemptCleanup launches a background goroutine that periodically
// removes stale IP entries from failedAttempts to prevent memory leaks.
func (h *Handler) startAttemptCleanup() {
	ctx, cancel := context.WithCancel(context.Background())
	h.attemptCleanupCancel = cancel
	h.attemptCleanupWG.Add(1)
	go func() {
		defer h.attemptCleanupWG.Done()
		ticker := time.NewTicker(attemptCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.purgeStaleAttempts()
			}
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

func (h *Handler) currentRateLimitBan(clientIP string, now time.Time) (time.Duration, bool) {
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()

	ai := h.failedAttempts[clientIP]
	if ai == nil || ai.blockedUntil.IsZero() {
		return 0, false
	}
	if now.Before(ai.blockedUntil) {
		return ai.blockedUntil.Sub(now).Round(time.Second), true
	}
	ai.blockedUntil = time.Time{}
	ai.failures = ai.failures[:0]
	return 0, false
}

func (h *Handler) recordFailedManagementAttempt(clientIP string, now time.Time) {
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()

	ai := h.failedAttempts[clientIP]
	if ai == nil {
		ai = &attemptInfo{}
		h.failedAttempts[clientIP] = ai
	}
	ai.lastActivity = now
	ai.pendingNoticeFailures++
	ai.lastFailureAt = now

	cutoff := now.Add(-managementFailureWindow)
	kept := 0
	for _, t := range ai.failures {
		if t.After(cutoff) {
			ai.failures[kept] = t
			kept++
		}
	}
	ai.failures = ai.failures[:kept]
	ai.failures = append(ai.failures, now)

	if len(ai.failures) >= managementMaxFailures {
		ai.banCount++
		ai.blockedUntil = now.Add(managementBanDuration)
		ai.failures = ai.failures[:0]
	}
}

func (h *Handler) resetManagementAttempts(clientIP string, now time.Time) {
	h.attemptsMu.Lock()
	defer h.attemptsMu.Unlock()

	if ai := h.failedAttempts[clientIP]; ai != nil {
		ai.failures = ai.failures[:0]
		ai.blockedUntil = time.Time{}
		ai.lastActivity = now
		ai.lastSuccessfulAuthAt = now
	}
}

// NewHandlerWithoutConfigFilePath creates a management handler without a config file path.
func NewHandlerWithoutConfigFilePath(cfg *config.Config, manager *coreauth.Manager, opts ...Option) *Handler {
	return NewHandler(cfg, "", manager, opts...)
}

// ApplyExternalConfigUpdate serializes Home and runtime-only updates with
// management config mutations. The callback must not start another external update.
func (h *Handler) ApplyExternalConfigUpdate(apply func() error) error {
	if h == nil {
		return fmt.Errorf("management handler is unavailable")
	}
	if apply == nil {
		return fmt.Errorf("external config update callback is nil")
	}
	h.runtimeCommitMu.Lock()
	defer h.runtimeCommitMu.Unlock()
	h.commitMu.Lock()
	defer h.commitMu.Unlock()
	return apply()
}

// ApplyRuntimeUpdate serializes runtime-only retries with config commit callbacks
// without holding the config persistence lock around plugin callbacks.
func (h *Handler) ApplyRuntimeUpdate(apply func() error) error {
	if h == nil {
		return fmt.Errorf("management handler is unavailable")
	}
	if apply == nil {
		return fmt.Errorf("runtime update callback is nil")
	}
	h.runtimeCommitMu.Lock()
	defer h.runtimeCommitMu.Unlock()
	return apply()
}

// ApplyExternalConfigCommit validates and publishes one watcher snapshot before
// applying it to the runtime. Runtime work does not hold the management commit lock.
func (h *Handler) ApplyExternalConfigCommit(
	cfg *config.Config,
	expectedHash string,
	apply func(ConfigCommit) error,
) error {
	if h == nil {
		return fmt.Errorf("management handler is unavailable")
	}
	if cfg == nil {
		return fmt.Errorf("external config is nil")
	}
	expectedHash = strings.TrimSpace(expectedHash)
	if expectedHash == "" {
		return fmt.Errorf("external config hash is empty")
	}
	if apply == nil {
		return fmt.Errorf("external config commit callback is nil")
	}

	h.runtimeCommitMu.Lock()
	defer h.runtimeCommitMu.Unlock()
	h.commitMu.Lock()
	if h.configFileHash() != expectedHash {
		h.commitMu.Unlock()
		return fmt.Errorf("external config changed before commit")
	}
	h.mu.Lock()
	commit, _ := h.publishConfigLocked(cfg, expectedHash)
	h.mu.Unlock()
	h.commitMu.Unlock()
	return apply(commit)
}

// SetConfig updates the in-memory config reference when the server hot-reloads.
func (h *Handler) SetConfig(cfg *config.Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg != cfg {
		h.configGeneration++
	}
	h.setConfigLocked(cfg)
}

func (h *Handler) SetConfigCommit(commit ConfigCommit) {
	if h == nil || commit.Config == nil || commit.Generation == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if commit.Generation < h.configGeneration {
		return
	}
	h.configGeneration = commit.Generation
	h.setConfigLocked(commit.Config)
}

func (h *Handler) ConfigState() ConfigCommit {
	if h == nil {
		return ConfigCommit{}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return ConfigCommit{Config: h.cfg, Generation: h.configGeneration}
}

func (h *Handler) configSnapshot() *config.Config {
	return h.ConfigState().Config
}

// setConfigLocked publishes config-derived handler snapshots together.
// Caller must hold h.mu.
func (h *Handler) setConfigLocked(cfg *config.Config) {
	h.cfg = cfg
	h.replaceAPIKeyAliasesSnapshot(configAPIKeyAliases(cfg))
}

// SetAuthManager updates the auth manager reference used by management endpoints.
func (h *Handler) SetAuthManager(manager *coreauth.Manager) { h.authManager = manager }

// SetUsageStatistics allows replacing the usage statistics reference.
func (h *Handler) SetUsageStatistics(stats *usage.RequestStatistics) { h.usageStats = stats }

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

// SetImageArtifacts sets the image artifact store reference.
func (h *Handler) SetImageArtifacts(store *artifact.Store) { h.imageArtifacts = store }

// AuthenticateManagementKey verifies a management key outside Gin middleware.
func (h *Handler) AuthenticateManagementKey(clientIP string, localClient bool, provided string) (bool, int, string) {
	cfg := h.configSnapshot()
	var (
		allowRemote bool
		secretHash  string
	)
	if cfg != nil {
		allowRemote = cfg.RemoteManagement.AllowRemote
		secretHash = cfg.RemoteManagement.SecretKey
	}
	envSecret := h.envSecret

	now := time.Now()
	if _, banned := h.currentRateLimitBan(clientIP, now); banned {
		return false, http.StatusTooManyRequests, managementRateLimitMessage
	}

	fail := func() {
		h.recordFailedManagementAttempt(clientIP, time.Now())
	}

	if !localClient && !allowRemote {
		fail()
		return false, http.StatusUnauthorized, managementAuthFailureMessage
	}

	reset := func() {
		h.resetManagementAttempts(clientIP, time.Now())
	}

	if secretHash == "" && envSecret == "" {
		if !(localClient && h.localPassword != "") {
			fail()
			return false, http.StatusUnauthorized, managementAuthFailureMessage
		}
	}

	if provided == "" {
		fail()
		return false, http.StatusUnauthorized, managementAuthFailureMessage
	}

	if localClient {
		if lp := h.localPassword; lp != "" {
			if subtle.ConstantTimeCompare([]byte(provided), []byte(lp)) == 1 {
				reset()
				return true, 0, ""
			}
		}
		if it := h.internalToken; it != "" {
			if subtle.ConstantTimeCompare([]byte(provided), []byte(it)) == 1 {
				reset()
				return true, 0, ""
			}
		}
	}

	if envSecret != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(envSecret)) == 1 {
		reset()
		return true, 0, ""
	}

	if secretHash == "" || bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(provided)) != nil {
		fail()
		return false, http.StatusUnauthorized, managementAuthFailureMessage
	}

	reset()
	return true, 0, ""
}

// Middleware enforces access control for management endpoints.
// All requests (local and remote) require a valid management key.
// Additionally, remote access requires allow-remote-management=true.
func (h *Handler) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-CPA-VERSION", buildinfo.Version)
		c.Header("X-CPA-COMMIT", buildinfo.Commit)
		c.Header("X-CPA-BUILD-DATE", buildinfo.BuildDate)
		c.Header(pluginhost.SupportPluginHeader, pluginhost.SupportPluginHeaderValue())

		cfg := h.configSnapshot()
		client := resolveManagementClientAddress(c.Request, cfg)
		clientIP := client.ip
		localClient := client.local
		var (
			allowRemote bool
			secretHash  string
		)
		if cfg != nil {
			allowRemote = cfg.RemoteManagement.AllowRemote
			secretHash = cfg.RemoteManagement.SecretKey
		}
		envSecret := h.envSecret

		now := time.Now()
		if remaining, banned := h.currentRateLimitBan(clientIP, now); banned {
			c.Header("Retry-After", fmt.Sprintf("%d", int(remaining.Seconds())))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": managementRateLimitMessage})
			return
		}

		fail := func() {
			h.recordFailedManagementAttempt(clientIP, time.Now())
		}
		if !localClient && !allowRemote {
			fail()
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": managementAuthFailureMessage})
			return
		}

		reset := func() {
			h.resetManagementAttempts(clientIP, time.Now())
		}
		if secretHash == "" && envSecret == "" {
			if localClient && h.localPassword != "" {
				// Allow local requests to proceed — localPassword will be validated below
			} else {
				fail()
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": managementAuthFailureMessage})
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
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": managementAuthFailureMessage})
			return
		}

		if localClient {
			if lp := h.localPassword; lp != "" {
				if subtle.ConstantTimeCompare([]byte(provided), []byte(lp)) == 1 {
					reset()
					c.Next()
					return
				}
			}
			// Internal token for quota scheduler and other internal services
			if it := h.internalToken; it != "" {
				if subtle.ConstantTimeCompare([]byte(provided), []byte(it)) == 1 {
					reset()
					c.Next()
					return
				}
			}
		}

		if envSecret != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(envSecret)) == 1 {
			reset()
			c.Next()
			return
		}

		if secretHash == "" || bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(provided)) != nil {
			fail()
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": managementAuthFailureMessage})
			return
		}

		reset()
		c.Next()
	}
}

type configMutationError struct {
	status  int
	message string
}

func (e *configMutationError) Error() string {
	return e.message
}

func newConfigMutationError(status int, message string) error {
	return &configMutationError{status: status, message: message}
}

func (h *Handler) mutateConfig(
	c *gin.Context,
	mutate func(*config.Config) error,
) (*config.Config, bool) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return nil, false
	}

	h.commitMu.Lock()
	h.mu.Lock()
	commit, callback, ok := h.mutateConfigLocked(c, mutate)
	h.mu.Unlock()
	h.commitMu.Unlock()
	if !ok {
		return nil, false
	}
	if !h.notifyConfigCommitted(c, callback, commit) {
		return nil, false
	}
	return commit.Config, true
}

// mutateConfigLocked persists and publishes a candidate config.
// Caller must hold h.mu and h.commitMu.
func (h *Handler) mutateConfigLocked(
	c *gin.Context,
	mutate func(*config.Config) error,
) (ConfigCommit, ConfigCommittedFunc, bool) {
	if h.cfg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return ConfigCommit{}, nil, false
	}

	next, err := config.Clone(h.cfg)
	if err != nil {
		log.WithError(err).Error("failed to clone config for management update")
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prepare config update"})
		return ConfigCommit{}, nil, false
	}
	if mutate != nil {
		if err = mutate(next); err != nil {
			status := http.StatusBadRequest
			var mutationError *configMutationError
			if errors.As(err, &mutationError) {
				status = mutationError.status
			}
			c.JSON(status, gin.H{"error": err.Error()})
			return ConfigCommit{}, nil, false
		}
	}
	persistedHash, ok := h.saveConfigCandidateLocked(c, next)
	if !ok {
		return ConfigCommit{}, nil, false
	}
	commit, callback := h.publishConfigLocked(next, persistedHash)
	return commit, callback, true
}

// publishConfigLocked publishes one immutable config generation.
// Caller must hold h.mu.
func (h *Handler) publishConfigLocked(
	cfg *config.Config,
	configFileHash string,
) (ConfigCommit, ConfigCommittedFunc) {
	h.setConfigLocked(cfg)
	h.configGeneration++
	return ConfigCommit{
		Config:         cfg,
		Generation:     h.configGeneration,
		ConfigFileHash: strings.TrimSpace(configFileHash),
	}, h.configCommitted
}

func (h *Handler) configFileHash() string {
	if h == nil || strings.TrimSpace(h.configFilePath) == "" {
		return ""
	}
	data, errRead := os.ReadFile(h.configFilePath)
	if errRead != nil || len(data) == 0 {
		return ""
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (h *Handler) notifyConfigCommitted(
	c *gin.Context,
	callback ConfigCommittedFunc,
	commit ConfigCommit,
) bool {
	ctx := context.Background()
	if c != nil && c.Request != nil {
		ctx = context.WithoutCancel(c.Request.Context())
	}
	return h.notifyConfigCommittedContext(c, ctx, callback, commit)
}

func (h *Handler) configCommitCurrent(commit ConfigCommit) bool {
	if h == nil {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.configGeneration == commit.Generation
}

func (h *Handler) runPluginRuntimeSyncForCommit(
	ctx context.Context,
	commit ConfigCommit,
	syncRuntime func(context.Context, *config.Config) error,
) error {
	if syncRuntime == nil {
		return nil
	}
	h.runtimeCommitMu.Lock()
	defer h.runtimeCommitMu.Unlock()
	if !h.configCommitCurrent(commit) {
		return nil
	}
	return runPluginRuntimeSync(ctx, syncRuntime, pluginRuntimeConfigSnapshot(commit.Config))
}

func (h *Handler) notifyConfigCommittedContext(
	c *gin.Context,
	ctx context.Context,
	callback ConfigCommittedFunc,
	commit ConfigCommit,
) bool {
	if callback == nil {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.runtimeCommitMu.Lock()
	defer h.runtimeCommitMu.Unlock()
	if !h.configCommitCurrent(commit) {
		return true
	}
	if err := invokeConfigCommitted(ctx, callback, commit); err != nil {
		log.WithError(err).WithField("generation", commit.Generation).
			Error("management config committed but runtime sync failed")
		if c != nil {
			c.JSON(
				http.StatusServiceUnavailable,
				gin.H{"error": "runtime_sync_failed", "generation": commit.Generation},
			)
		}
		return false
	}
	return true
}

func invokeConfigCommitted(
	ctx context.Context,
	callback ConfigCommittedFunc,
	commit ConfigCommit,
) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("config committed callback panicked: %v", recovered)
		}
	}()
	return callback(ctx, commit)
}

type configPersistenceError struct {
	logMessage  string
	userMessage string
	err         error
}

func (e *configPersistenceError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %v", e.userMessage, e.err)
}

func (e *configPersistenceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (h *Handler) persistConfigCandidate(candidate *config.Config) (string, error) {
	if strings.TrimSpace(h.configFilePath) == "" {
		return "", nil
	}
	if _, err := os.Stat(h.configFilePath); err == nil {
		if err = config.SaveLastGood(h.configFilePath); err != nil {
			return "", &configPersistenceError{
				logMessage:  "failed to save last-good config",
				userMessage: "failed to save config backup",
				err:         err,
			}
		}
	} else if !os.IsNotExist(err) {
		return "", &configPersistenceError{
			logMessage:  "failed to inspect config before saving backup",
			userMessage: "failed to inspect config",
			err:         err,
		}
	}
	persistedHash, err := config.SaveConfigPreserveCommentsWithDigest(h.configFilePath, candidate)
	if err != nil {
		return "", &configPersistenceError{
			logMessage:  "failed to save config",
			userMessage: "failed to save config",
			err:         err,
		}
	}
	return persistedHash, nil
}

func respondConfigPersistenceError(c *gin.Context, err error) {
	var persistenceError *configPersistenceError
	if errors.As(err, &persistenceError) {
		log.WithError(persistenceError.err).Error(persistenceError.logMessage)
		c.JSON(http.StatusInternalServerError, gin.H{"error": persistenceError.userMessage})
		return
	}
	log.WithError(err).Error("failed to save config")
	c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save config"})
}

func (h *Handler) saveConfigCandidateLocked(
	c *gin.Context,
	candidate *config.Config,
) (string, bool) {
	persistedHash, err := h.persistConfigCandidate(candidate)
	if err != nil {
		respondConfigPersistenceError(c, err)
		return "", false
	}
	return persistedHash, true
}

// persist saves the current in-memory config to disk with last-good protection.
func (h *Handler) persist(c *gin.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.persistLocked(c)
}

func (h *Handler) persistLocked(c *gin.Context) bool {
	if !h.saveConfigLocked(c) {
		return false
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
	return true
}

func (h *Handler) saveConfigLocked(c *gin.Context) bool {
	_, ok := h.saveConfigCandidateLocked(c, h.cfg)
	return ok
}

// rollbackLastGood restores config.yaml from the last-good backup and reloads it into memory.
// Caller must hold h.mu.
func (h *Handler) rollbackLastGood() {
	lastGood := config.LastGoodPath(h.configFilePath)
	data, err := os.ReadFile(lastGood)
	if err != nil {
		log.WithError(err).Error("rollback: failed to read last-good backup")
		return
	}
	if errWrite := writeConfigFileAtomically(h.configFilePath, data); errWrite != nil {
		log.WithError(errWrite).Error("rollback: failed to restore config from last-good")
		return
	}
	if restored, errLoad := config.LoadConfig(h.configFilePath); errLoad == nil {
		h.setConfigLocked(restored)
	} else {
		log.WithError(errLoad).Error("rollback: failed to reload config after restore")
	}
}

// Helper methods for simple types
func (h *Handler) updateBoolField(c *gin.Context, set func(*config.Config, bool)) {
	var body struct {
		Value *bool `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if _, ok := h.mutateConfig(c, func(next *config.Config) error {
		set(next, *body.Value)
		return nil
	}); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func (h *Handler) updateIntField(c *gin.Context, set func(*config.Config, int)) {
	var body struct {
		Value *int `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if _, ok := h.mutateConfig(c, func(next *config.Config) error {
		set(next, *body.Value)
		return nil
	}); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func (h *Handler) updateStringField(c *gin.Context, set func(*config.Config, string)) {
	var body struct {
		Value *string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if _, ok := h.mutateConfig(c, func(next *config.Config) error {
		set(next, *body.Value)
		return nil
	}); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func (h *Handler) updateStringFieldValidated(
	c *gin.Context,
	validate func(string) error,
	set func(*config.Config, string),
) {
	var body struct {
		Value *string `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if err := validate(*body.Value); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}
	if _, ok := h.mutateConfig(c, func(next *config.Config) error {
		set(next, *body.Value)
		return nil
	}); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}
