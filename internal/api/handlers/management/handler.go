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
	"sync/atomic"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/artifact"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
)

type attemptInfo struct {
	failures               []time.Time // sliding window of failure timestamps
	blockedUntil           time.Time
	lastActivity           time.Time // track last activity for cleanup
	banCount               int
	pendingNoticeFailures  int
	lastFailureAt          time.Time
	lastSuccessfulAuthAt   time.Time
	lastFailureFingerprint string
	lastCountedFailureAt   time.Time
}

const (
	managementFailureWindow                    = 5 * time.Minute
	managementDuplicateFailureWindow           = 5 * time.Second
	managementMaxFailures                      = 10
	managementBanDuration                      = 30 * time.Minute
	managementAuthFailureMessage               = "Unauthorized"
	managementRateLimitMessage                 = "too many authentication failures, please try again later"
	managementRateLimitCode                    = "management_rate_limited"
	maxManagementRateLimitClients              = 4096
	maxManagementRateLimitStateBytes           = 8 << 20
	managementRateLimitCapacityWarningInterval = time.Minute
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
	ModelCatalogPrepared   bool
}

// ConfigCommittedFunc applies a persisted management config to the running server.
type ConfigCommittedFunc func(context.Context, ConfigCommit) error

type configCommitWarningContextKey struct{}

type configCommitWarnings struct {
	mu           sync.Mutex
	cleanupError error
}

func beginConfigCommitWarnings(c *gin.Context) *configCommitWarnings {
	warnings := &configCommitWarnings{}
	if c != nil && c.Request != nil {
		ctx := context.WithValue(c.Request.Context(), configCommitWarningContextKey{}, warnings)
		c.Request = c.Request.WithContext(ctx)
	}
	return warnings
}

func ReportConfigCommitCleanupError(ctx context.Context, err error) {
	if ctx == nil || err == nil {
		return
	}
	warnings, _ := ctx.Value(configCommitWarningContextKey{}).(*configCommitWarnings)
	if warnings == nil {
		return
	}
	warnings.mu.Lock()
	warnings.cleanupError = errors.Join(warnings.cleanupError, err)
	warnings.mu.Unlock()
}

func (w *configCommitWarnings) cleanupErrorMessage() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cleanupError == nil {
		return ""
	}
	return w.cleanupError.Error()
}

// Handler aggregates config reference, persistence path and helpers.
type Handler struct {
	cfg                            *config.Config
	configFilePath                 string
	commitMu                       sync.Mutex
	runtimeCommitMu                sync.Mutex
	authStatusMu                   sync.Mutex
	pluginVirtualStatusMu          sync.Mutex
	mu                             sync.RWMutex
	configGeneration               uint64
	configCommitted                ConfigCommittedFunc
	configYAMLKeyMu                sync.Mutex
	configYAMLKey                  [32]byte
	configYAMLKeyLoaded            bool
	apiKeyAliasesMu                sync.RWMutex
	apiKeyAliases                  map[string]string
	attemptsMu                     sync.Mutex
	failedAttempts                 map[string]*attemptInfo // keyed by client IP
	rateLimitEvents                []rateLimitEventEntry
	rateLimitRevision              int64
	rateLimitCapacityWarningAt     time.Time
	rateLimitPersistenceAvailable  atomic.Bool
	rateLimitPersistenceMu         sync.Mutex
	rateLimitPendingState          *rateLimitPersistedState
	rateLimitPersistenceDirty      bool
	rateLimitPersistenceWake       chan struct{}
	rateLimitPersistenceStop       chan struct{}
	rateLimitPersistenceWG         sync.WaitGroup
	rateLimitPersistenceCloseErr   error
	rateLimitPersistenceTimeout    time.Duration
	rateLimitPersistenceRetryDelay time.Duration
	authManager                    *coreauth.Manager
	usageStats                     *usage.RequestStatistics
	usagePersister                 *usage.Persister
	usageQueue                     *redisqueue.Queue
	homeClientProvider             func() *home.Client
	imageArtifacts                 *artifact.Store
	tokenStore                     coreauth.Store
	localPassword                  string
	envSecret                      string
	logDir                         string
	postAuthHook                   coreauth.PostAuthHook
	postAuthPersistHook            coreauth.PostAuthHook
	quotaScheduler                 *quota.Scheduler
	metaQuotaGeneration            atomic.Uint64
	quotaRefreshTasks              *quotaRefreshTaskService
	pluginHost                     pluginRuntimeHost
	pluginRuntimeSync              func(context.Context, *config.Config) error
	pluginStoreRegistryURL         string
	pluginStoreHTTPClient          pluginstore.HTTPDoer
	pluginReleaseCacheMu           sync.Mutex
	pluginReleaseCache             map[string]pluginReleaseCacheEntry
	modelCatalogRuntimeSync        func(*config.Config) error
	accountInspection              *accountInspectionService
	stateStore                     *managementStateStore
	inferenceAdmission             *executionregistry.Admission
	internalToken                  string // random token for internal localhost requests (quota scheduler)
	attemptCleanupCancel           context.CancelFunc
	attemptCleanupWG               sync.WaitGroup
	closeOnce                      sync.Once
	closeErr                       error
}

// Option configures a Handler.
type Option func(*Handler)

func WithInferenceAdmission(admission *executionregistry.Admission) Option {
	return func(h *Handler) { h.inferenceAdmission = admission }
}

// withEnvSecret overrides the management password (default: from MANAGEMENT_PASSWORD env).
func withEnvSecret(secret string) Option {
	return func(h *Handler) { h.envSecret = strings.TrimSpace(secret) }
}

// withUsageStats overrides the usage statistics source.
func withUsageStats(stats *usage.RequestStatistics) Option {
	return func(h *Handler) { h.usageStats = stats }
}

// withTokenStore overrides the token store.
func withTokenStore(store coreauth.Store) Option {
	return func(h *Handler) { h.tokenStore = store }
}

// withUsagePersister sets the usage persister.
func withUsagePersister(p *usage.Persister) Option {
	return func(h *Handler) { h.usagePersister = p }
}

// WithUsageQueue sets the service-owned usage output queue.
func WithUsageQueue(queue *redisqueue.Queue) Option {
	return func(h *Handler) { h.usageQueue = queue }
}

func WithHomeClientProvider(provider func() *home.Client) Option {
	return func(h *Handler) { h.homeClientProvider = provider }
}

// WithImageArtifacts sets the image artifact store.
func WithImageArtifacts(store *artifact.Store) Option {
	return func(h *Handler) { h.imageArtifacts = store }
}

// withQuotaScheduler sets the quota refresh scheduler.
func withQuotaScheduler(s *quota.Scheduler) Option {
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

func WithModelCatalogRuntimeSync(sync func(*config.Config) error) Option {
	return func(h *Handler) { h.modelCatalogRuntimeSync = sync }
}

// withLogDir sets the log directory.
func withLogDir(dir string) Option {
	return func(h *Handler) { h.logDir = dir }
}

// NewHandler creates a new management handler instance.
func NewHandler(cfg *config.Config, configFilePath string, manager *coreauth.Manager, opts ...Option) *Handler {
	envSecret, _ := os.LookupEnv("MANAGEMENT_PASSWORD")
	envSecret = strings.TrimSpace(envSecret)

	h := &Handler{
		cfg:                            cfg,
		configFilePath:                 configFilePath,
		usageQueue:                     redisqueue.DefaultQueue(),
		apiKeyAliases:                  copyAPIKeyAliases(configAPIKeyAliases(cfg)),
		failedAttempts:                 make(map[string]*attemptInfo),
		rateLimitPersistenceTimeout:    defaultRateLimitPersistenceTimeout,
		rateLimitPersistenceRetryDelay: defaultRateLimitPersistenceRetryDelay,
		authManager:                    manager,
		usageStats:                     usage.GetRequestStatistics(),
		tokenStore:                     sdkAuth.GetTokenStore(),
		envSecret:                      envSecret,
		internalToken:                  generateInternalToken(),
		pluginReleaseCache:             make(map[string]pluginReleaseCacheEntry),
	}

	for _, opt := range opts {
		opt(h)
	}
	stateStore, err := openManagementStateStore(configFilePath)
	if err != nil {
		if h.inferenceAdmission != nil {
			h.inferenceAdmission.Hold(updateSessionStateKey)
			log.WithError(err).Error("management state unavailable; inference paused until persistence is restored and the service restarts")
		} else {
			log.WithError(err).Warn("management state persistence disabled")
		}
	} else {
		h.stateStore = stateStore
		h.rateLimitPersistenceAvailable.Store(stateStore != nil)
		if stateStore != nil {
			h.rateLimitPersistenceWake = make(chan struct{}, 1)
			h.rateLimitPersistenceStop = make(chan struct{})
			h.restoreRateLimitState()
			h.startRateLimitPersistence()
		}
	}
	h.configureQuotaScheduler(h.quotaScheduler)
	h.quotaRefreshTasks = newQuotaRefreshTaskService(h)
	h.accountInspection = newAccountInspectionService(h)

	if h.inferenceAdmission != nil && h.stateStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := h.restoreUpdateAdmission(ctx); err != nil {
			log.WithError(err).Error("inference paused while update state is unavailable")
		}
		cancel()
	}
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
			if h.quotaRefreshTasks != nil {
				h.closeErr = errors.Join(h.closeErr, h.quotaRefreshTasks.close())
			}
			if h.accountInspection != nil {
				h.accountInspection.close()
			}
			if err := h.stopRateLimitPersistence(); err != nil {
				h.closeErr = errors.Join(h.closeErr, fmt.Errorf("flush management rate limit state: %w", err))
			}
			if h.stateStore != nil {
				h.closeErr = errors.Join(h.closeErr, h.stateStore.Close())
			}
		},
	)
	return h.closeErr
}

func (h *Handler) configureQuotaScheduler(s *quota.Scheduler) {
	if h == nil || s == nil {
		return
	}
	s.SetRefreshContextFinalizer(h.finalizeQuotaRefreshContext)
	if h.stateStore == nil {
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

func (h *Handler) deleteQuotaEntry(entry quota.EntryState) {
	if h == nil || h.stateStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.stateStore.deleteQuotaEntry(ctx, entry); err != nil {
		log.WithError(err).Warn("failed to save quota tombstone")
	}
}

// startAttemptCleanup launches a background goroutine that periodically
// removes stale IP entries from failedAttempts to prevent memory leaks.
func (h *Handler) startAttemptCleanup() {
	ctx, cancel := context.WithCancel(context.Background())
	h.attemptCleanupCancel = cancel
	h.startUpdateReconciliation(ctx)
	h.attemptCleanupWG.Go(func() {
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
	})
}

// purgeStaleAttempts removes IP entries that have been idle beyond attemptMaxIdleTime
// and whose ban (if any) has expired.
func (h *Handler) purgeStaleAttempts() {
	now := time.Now()
	changed := false
	h.attemptsMu.Lock()
	cutoff := now.Add(-managementFailureWindow)
	for ip, ai := range h.failedAttempts {
		changed = h.expireRateLimitBanLocked(ip, ai, now) || changed
		kept := 0
		for _, failureAt := range ai.failures {
			if failureAt.After(cutoff) {
				ai.failures[kept] = failureAt
				kept++
			}
		}
		if kept != len(ai.failures) {
			ai.failures = ai.failures[:kept]
			changed = true
		}
		if !ai.blockedUntil.IsZero() && now.Before(ai.blockedUntil) {
			continue
		}
		if now.Sub(ai.lastActivity) > attemptMaxIdleTime {
			delete(h.failedAttempts, ip)
			changed = true
		}
	}
	h.attemptsMu.Unlock()
	if changed {
		h.persistRateLimitState()
	}
}

func managementRetryAfterSeconds(remaining time.Duration) int {
	if remaining <= 0 {
		return 1
	}
	seconds := int((remaining + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

func managementFailureFingerprint(reason string, provided string) string {
	digest := sha256.Sum256([]byte(reason + "\x00" + provided))
	return hex.EncodeToString(digest[:])
}

func (h *Handler) currentRateLimitBan(clientIP string, now time.Time) (time.Duration, bool) {
	changed := false
	h.attemptsMu.Lock()
	ai := h.failedAttempts[clientIP]
	if ai == nil {
		remaining, blocked := h.rateLimitCapacityBanLocked(now)
		h.attemptsMu.Unlock()
		return remaining, blocked
	}
	if ai.blockedUntil.IsZero() {
		h.attemptsMu.Unlock()
		return 0, false
	}
	if now.Before(ai.blockedUntil) {
		remaining := ai.blockedUntil.Sub(now)
		h.attemptsMu.Unlock()
		return remaining, true
	}
	changed = h.expireRateLimitBanLocked(clientIP, ai, now)
	h.attemptsMu.Unlock()
	if changed {
		h.persistRateLimitState()
	}
	return 0, false
}

func (h *Handler) recordFailedManagementAttempt(clientIP string, fingerprint string, now time.Time) {
	h.attemptsMu.Lock()
	ai := h.failedAttempts[clientIP]
	if ai == nil {
		ai = &attemptInfo{}
		h.failedAttempts[clientIP] = ai
	}
	if now.After(ai.lastActivity) {
		ai.lastActivity = now
	}
	if fingerprint != "" &&
		ai.lastFailureFingerprint == fingerprint &&
		!ai.lastCountedFailureAt.IsZero() {
		if now.Sub(ai.lastCountedFailureAt).Abs() < managementDuplicateFailureWindow {
			h.attemptsMu.Unlock()
			return
		}
	}
	ai.lastFailureFingerprint = fingerprint
	ai.lastCountedFailureAt = now
	ai.pendingNoticeFailures++
	ai.lastFailureAt = now

	cutoff := now.Add(-managementFailureWindow)
	kept := 0
	for _, failureAt := range ai.failures {
		if failureAt.After(cutoff) {
			ai.failures[kept] = failureAt
			kept++
		}
	}
	ai.failures = ai.failures[:kept]
	ai.failures = append(ai.failures, now)

	if len(ai.failures) >= managementMaxFailures {
		ai.banCount++
		ai.blockedUntil = now.Add(managementBanDuration)
		ai.failures = ai.failures[:0]
		h.appendRateLimitEventLocked(
			rateLimitEventBanStarted,
			clientIP,
			now,
			ai.blockedUntil,
			ai.banCount,
		)
	}
	dropped := h.trimRateLimitAttemptsLocked(now)
	retained := h.failedAttempts[clientIP] == ai
	warnCapacity := dropped > 0 && h.rateLimitCapacityWarningDueLocked(now)
	h.attemptsMu.Unlock()
	if warnCapacity {
		log.WithFields(
			log.Fields{
				"capacity":  maxManagementRateLimitClients,
				"discarded": dropped,
			},
		).Warn("management rate limit client tracking reached capacity; discarded lower-priority state")
	}
	if retained {
		h.persistRateLimitState()
	}
}

func (h *Handler) resetManagementAttempts(clientIP string, now time.Time) {
	changed := false
	h.attemptsMu.Lock()
	if ai := h.failedAttempts[clientIP]; ai != nil {
		changed = len(ai.failures) > 0 ||
			!ai.blockedUntil.IsZero() ||
			ai.lastFailureFingerprint != "" ||
			!ai.lastCountedFailureAt.IsZero() ||
			ai.pendingNoticeFailures > 0
		ai.failures = ai.failures[:0]
		ai.blockedUntil = time.Time{}
		ai.lastActivity = now
		ai.lastSuccessfulAuthAt = now
		ai.lastFailureFingerprint = ""
		ai.lastCountedFailureAt = time.Time{}
	}
	h.attemptsMu.Unlock()
	if changed {
		h.persistRateLimitState()
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

// SetPostAuthPersistHook registers a hook to be called after auth persistence.
func (h *Handler) SetPostAuthPersistHook(hook coreauth.PostAuthHook) {
	h.postAuthPersistHook = hook
}

// SetUsagePersister sets the usage statistics persister reference.
func (h *Handler) SetUsagePersister(p *usage.Persister) { h.usagePersister = p }

func (h *Handler) currentHomeClient() *home.Client {
	if h == nil {
		return nil
	}
	if h.homeClientProvider != nil {
		return h.homeClientProvider()
	}
	return home.Current()
}

func managementKeyFromRequest(c *gin.Context) string {
	if authorization := c.GetHeader("Authorization"); authorization != "" {
		parts := strings.SplitN(authorization, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			return parts[1]
		}
		return authorization
	}
	return c.GetHeader("X-Management-Key")
}

func abortManagementRateLimit(c *gin.Context, now time.Time, remaining time.Duration) {
	retryAfterSeconds := managementRetryAfterSeconds(remaining)
	c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSeconds))
	c.AbortWithStatusJSON(
		http.StatusTooManyRequests, gin.H{
			"error":               managementRateLimitMessage,
			"code":                managementRateLimitCode,
			"retry_after_seconds": retryAfterSeconds,
			"banned_until":        now.Add(time.Duration(retryAfterSeconds) * time.Second).UTC(),
		},
	)
}

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

	fail := func(reason string) {
		h.recordFailedManagementAttempt(
			clientIP,
			managementFailureFingerprint(reason, provided),
			time.Now(),
		)
	}
	if !localClient && !allowRemote {
		fail("remote-disabled")
		return false, http.StatusUnauthorized, managementAuthFailureMessage
	}

	reset := func() {
		h.resetManagementAttempts(clientIP, time.Now())
	}
	if secretHash == "" && envSecret == "" {
		if !(localClient && h.localPassword != "") {
			fail("management-secret-unavailable")
			return false, http.StatusUnauthorized, managementAuthFailureMessage
		}
	}
	if provided == "" {
		fail("missing-credential")
		return false, http.StatusUnauthorized, managementAuthFailureMessage
	}

	if localClient {
		if lp := h.localPassword; lp != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(lp)) == 1 {
			reset()
			return true, 0, ""
		}
		if it := h.internalToken; it != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(it)) == 1 {
			reset()
			return true, 0, ""
		}
	}
	if envSecret != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(envSecret)) == 1 {
		reset()
		return true, 0, ""
	}
	if secretHash == "" || bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(provided)) != nil {
		fail("invalid-credential")
		return false, http.StatusUnauthorized, managementAuthFailureMessage
	}

	reset()
	return true, 0, ""
}

// middleware enforces access control for management endpoints.
// All requests (local and remote) require a valid management key.
// Additionally, remote access requires allow-remote-management=true.
func (h *Handler) middleware() gin.HandlerFunc {
	return h.MiddlewareWithConfig(nil)
}

// MiddlewareWithConfig authenticates against a request-scoped config snapshot when provided.
func (h *Handler) MiddlewareWithConfig(configSnapshot func(*gin.Context) *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-CPA-VERSION", buildinfo.Version)
		c.Header("X-CPA-COMMIT", buildinfo.Commit)
		c.Header("X-CPA-BUILD-DATE", buildinfo.BuildDate)
		c.Header(pluginhost.SupportPluginHeader, pluginhost.SupportPluginHeaderValue())

		cfg := h.configSnapshot()
		if configSnapshot != nil {
			cfg = configSnapshot(c)
		}
		client := resolveManagementClientAddress(c.Request, cfg)
		if c.Request != nil {
			c.Request = c.Request.WithContext(
				access.WithClientAddress(c.Request.Context(), client.resolved),
			)
		}
		c.Set(managementClientAddressContextKey, client)
		clientIP := client.ip
		localClient := client.local
		provided := managementKeyFromRequest(c)
		var (
			allowRemote bool
			secretHash  string
		)
		if cfg != nil {
			allowRemote = cfg.RemoteManagement.AllowRemote
			secretHash = cfg.RemoteManagement.SecretKey
		}
		envSecret := h.envSecret

		if client.peerLocal {
			if it := h.internalToken; it != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(it)) == 1 {
				setManagementMutationActor(c, managementMutationActorInternal)
				c.Next()
				return
			}
		}

		now := time.Now()
		if remaining, banned := h.currentRateLimitBan(clientIP, now); banned {
			abortManagementRateLimit(c, now, remaining)
			return
		}

		fail := func(reason string) {
			h.recordFailedManagementAttempt(
				clientIP,
				managementFailureFingerprint(reason, provided),
				time.Now(),
			)
		}
		if !localClient && !allowRemote {
			fail("remote-disabled")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": managementAuthFailureMessage})
			return
		}

		reset := func() {
			h.resetManagementAttempts(clientIP, time.Now())
		}
		if secretHash == "" && envSecret == "" {
			if !(localClient && h.localPassword != "") {
				fail("management-secret-unavailable")
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": managementAuthFailureMessage})
				return
			}
		}
		if provided == "" {
			fail("missing-credential")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": managementAuthFailureMessage})
			return
		}

		if localClient {
			if lp := h.localPassword; lp != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(lp)) == 1 {
				reset()
				setManagementMutationActor(c, managementMutationActorLocal)
				c.Next()
				return
			}
		}
		if envSecret != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(envSecret)) == 1 {
			reset()
			setManagementMutationActor(c, managementMutationActorKey)
			c.Next()
			return
		}
		if secretHash == "" || bcrypt.CompareHashAndPassword([]byte(secretHash), []byte(provided)) != nil {
			fail("invalid-credential")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": managementAuthFailureMessage})
			return
		}

		reset()
		setManagementMutationActor(c, managementMutationActorKey)
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
			if mutationError, ok := errors.AsType[*configMutationError](err); ok {
				status = mutationError.status
			}
			c.JSON(status, gin.H{"error": err.Error()})
			return ConfigCommit{}, nil, false
		}
	}
	if err = next.ValidateCredentialWeights(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return ConfigCommit{}, nil, false
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
	if persistenceError, ok := errors.AsType[*configPersistenceError](err); ok {
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

// Helper methods for simple types
func (h *Handler) updateBoolField(c *gin.Context, set func(*config.Config, bool)) {
	var body struct {
		Value *bool `json:"value"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if _, ok := h.mutateConfig(
		c, func(next *config.Config) error {
			set(next, *body.Value)
			return nil
		},
	); ok {
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
	if _, ok := h.mutateConfig(
		c, func(next *config.Config) error {
			set(next, *body.Value)
			return nil
		},
	); ok {
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
	if _, ok := h.mutateConfig(
		c, func(next *config.Config) error {
			set(next, *body.Value)
			return nil
		},
	); ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}
