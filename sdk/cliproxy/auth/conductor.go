package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	authfile "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ProviderExecutor defines the contract required by Manager to execute provider calls.
type ProviderExecutor interface {
	// Identifier returns the provider key handled by this executor.
	Identifier() string
	// Execute handles non-streaming execution and returns the provider response payload.
	Execute(
		ctx context.Context,
		auth *Auth,
		req cliproxyexecutor.Request,
		opts cliproxyexecutor.Options,
	) (cliproxyexecutor.Response, error)
	// ExecuteStream handles streaming execution and returns a StreamResult containing
	// upstream headers and a channel of provider chunks.
	ExecuteStream(
		ctx context.Context,
		auth *Auth,
		req cliproxyexecutor.Request,
		opts cliproxyexecutor.Options,
	) (*cliproxyexecutor.StreamResult, error)
	// Refresh attempts to refresh provider credentials and returns the updated auth state.
	Refresh(ctx context.Context, auth *Auth) (*Auth, error)
	// CountTokens returns the token count for the given request.
	CountTokens(
		ctx context.Context,
		auth *Auth,
		req cliproxyexecutor.Request,
		opts cliproxyexecutor.Options,
	) (cliproxyexecutor.Response, error)
	// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
	// Callers must close the response body when non-nil.
	HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)
}

// RequestAuthPreparer lets an executor update missing auth metadata before a request.
type RequestAuthPreparer interface {
	ShouldPrepareRequestAuth(auth *Auth) bool
	PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error)
}

// UnauthorizedRefreshEvaluator lets an executor report whether it can refresh
// provider-owned credential material after an unauthorized response.
type UnauthorizedRefreshEvaluator interface {
	ShouldRefreshAfterUnauthorized(auth *Auth) bool
}

// ExecutionSessionCloser allows executors to release per-session runtime resources.
type ExecutionSessionCloser interface {
	CloseExecutionSession(sessionID string)
}

const (
	homeAuthCountMetadataKey = "__cliproxy_home_auth_count"
	// CloseAllExecutionSessionsID asks an executor to release all active execution sessions.
	// Executors that do not support this marker may ignore it.
	CloseAllExecutionSessionsID = "__all_execution_sessions__"
)

// RefreshEvaluator allows runtime state to override refresh decisions.
type RefreshEvaluator interface {
	ShouldRefresh(now time.Time, auth *Auth) bool
}

const (
	refreshCheckInterval  = 5 * time.Second
	refreshMaxConcurrency = 16
	refreshPendingBackoff = time.Minute
	refreshFailureBackoff = 5 * time.Minute
	// refreshIneffectiveBackoff throttles refresh attempts when an executor returns
	// success but the auth still evaluates as needing refresh.
	refreshIneffectiveBackoff = 30 * time.Second
	quotaBackoffBase          = time.Second
	quotaBackoffMax           = 30 * time.Minute
	transientErrorCooldown    = time.Minute
)

var refreshPersistenceRollbackTimeoutNanos = int64(5 * time.Second)

func refreshPersistenceRollbackTimeout() time.Duration {
	return time.Duration(atomic.LoadInt64(&refreshPersistenceRollbackTimeoutNanos))
}

func swapRefreshPersistenceRollbackTimeout(timeout time.Duration) time.Duration {
	return time.Duration(atomic.SwapInt64(&refreshPersistenceRollbackTimeoutNanos, int64(timeout)))
}

var refreshPersistenceCommitTimeout = 60 * time.Second
var deferredAuthRecreateRetryInitialDelay = 100 * time.Millisecond
var deferredAuthRecreateRetryMaxDelay = 5 * time.Second

// ErrAuthDurableStateUncertain reports that persistence may have partially completed.
var ErrAuthDurableStateUncertain = errors.New("credential durable state is uncertain")

var errAuthDurableStateUncertain = ErrAuthDurableStateUncertain
var errRefreshPersistedVersionChanged = errors.New("persisted credential changed during refresh")
var quotaCooldownDisabled atomic.Bool
var transientErrorCooldownSeconds atomic.Int64

// SetQuotaCooldownDisabled toggles quota cooldown scheduling globally.
func SetQuotaCooldownDisabled(disable bool) {
	quotaCooldownDisabled.Store(disable)
}

// SetTransientErrorCooldownSeconds configures cooldowns for 408/500/502/503/504.
// 0 keeps the default; negative values disable transient error cooldowns.
func SetTransientErrorCooldownSeconds(seconds int) {
	transientErrorCooldownSeconds.Store(int64(seconds))
}

func quotaCooldownDisabledForAuth(auth *Auth) bool {
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
	}
	return quotaCooldownDisabled.Load()
}

func nextTransientErrorRetryAfter(now time.Time) time.Time {
	seconds := transientErrorCooldownSeconds.Load()
	if seconds < 0 {
		return time.Time{}
	}
	if seconds == 0 {
		return now.Add(transientErrorCooldown)
	}
	return now.Add(time.Duration(seconds) * time.Second)
}

// Result captures execution outcome used to adjust auth state.
type Result struct {
	// AuthID references the auth that produced this result.
	AuthID string
	// AuthGeneration identifies the credential version used by the request.
	AuthGeneration uint64
	// RuntimeOnly prevents request-scoped credentials from mutating managed auth state.
	RuntimeOnly bool
	// Provider is copied for convenience when emitting hooks.
	Provider string
	// Model is the upstream model identifier used for the request.
	Model string
	// ModelQuotaEpoch identifies the model quota state used when the request started.
	ModelQuotaEpoch uint64
	// Success marks whether the execution succeeded.
	Success bool
	// RetryAfter carries a provider supplied retry hint (e.g. 429 retryDelay).
	RetryAfter *time.Duration
	// Error describes the failure when Success is false.
	Error *Error
}

// Selector chooses an auth candidate for execution.
type Selector interface {
	Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
}

type PluginScheduler interface {
	PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error)
}

// Hook captures lifecycle callbacks for observing auth changes.
type Hook interface {
	// OnAuthRegistered fires when a new auth is registered.
	OnAuthRegistered(ctx context.Context, auth *Auth)
	// OnAuthUpdated fires when an existing auth changes state.
	OnAuthUpdated(ctx context.Context, auth *Auth)
	// OnResult fires when execution result is recorded.
	OnResult(ctx context.Context, result Result)
}

// NoopHook provides optional hook defaults.
type NoopHook struct{}

// OnAuthRegistered implements Hook.
func (NoopHook) OnAuthRegistered(context.Context, *Auth) {}

// OnAuthUpdated implements Hook.
func (NoopHook) OnAuthUpdated(context.Context, *Auth) {}

// OnResult implements Hook.
func (NoopHook) OnResult(context.Context, Result) {}

type authDeletionIntent struct {
	pending    <-chan struct{}
	expected   *Auth
	version    *PersistedVersion
	ctx        context.Context
	cancel     context.CancelFunc
	active     atomic.Bool
	superseded atomic.Bool
}

type authRecreationIntent struct {
	auth         *Auth
	pending      <-chan struct{}
	ctx          context.Context
	cancel       context.CancelFunc
	removalEpoch uint64
	active       atomic.Bool
	superseded   atomic.Bool
	done         chan struct{}
}

type authRefreshPersistenceIntent struct {
	auth            *Auth
	expectedVersion *PersistedVersion
	nextVersion     *PersistedVersion
	generation      uint64
	ctx             context.Context
	cancel          context.CancelFunc
}

type authRemovalIntent struct {
	ready chan struct{}
	once  sync.Once
}

func newAuthRemovalIntent() *authRemovalIntent {
	return &authRemovalIntent{ready: make(chan struct{})}
}

func (i *authRemovalIntent) markReady() {
	if i != nil {
		i.once.Do(func() { close(i.ready) })
	}
}

type authRecreationIntentContextKey struct{}
type authMutationRemovalEpochContextKey struct{}

// Manager orchestrates auth lifecycle, selection, execution, and persistence.
type Manager struct {
	store                Store
	deferredAuthRecreate func(context.Context, *Auth) error
	executors            map[string]ProviderExecutor
	selector             Selector
	hook                 Hook
	mu                   sync.RWMutex
	executorMu           sync.RWMutex
	authIntentMu         sync.Mutex
	rollbackStateMu      sync.Mutex
	persistenceLock      *authMutationLock
	auths                map[string]*Auth
	scheduler            *authScheduler
	pluginScheduler      PluginScheduler
	homeRuntimeAuths     map[string]map[string]*Auth
	removedAuths         map[string]uint64
	removedAuthAt        map[string]time.Time
	pendingAuthDeletions map[string]*authDeletionIntent
	removedAuthSeq       uint64
	authGenerationSeq    uint64
	// providerOffsets tracks per-model provider rotation state for multi-provider routing.
	providerOffsets map[string]int

	// Retry controls request retry behavior.
	requestRetry        atomic.Int32
	maxRetryCredentials atomic.Int32
	maxRetryInterval    atomic.Int64

	// oauthModelAlias stores global OAuth model alias mappings (alias -> upstream name) keyed by channel.
	oauthModelAlias atomic.Value

	// apiKeyModelAlias caches resolved model alias mappings for API-key auths.
	// Keyed by auth.ID, value is alias(lower) -> upstream model (including suffix).
	apiKeyModelAlias atomic.Value

	// runtimeConfig stores the latest application config for request-time decisions.
	// It is initialized in NewManager; never Load() before first Store().
	runtimeConfig atomic.Value

	// Optional HTTP RoundTripper provider injected by host.
	rtProvider RoundTripperProvider

	homeClientProviderMu sync.RWMutex
	homeClientProvider   func() HomeClient

	requestPrepareLocksMu       sync.Mutex
	requestPrepareLocks         map[string]*requestAuthPrepareLock
	authMutationLocksMu         sync.Mutex
	authMutationLocks           map[string]*authMutationLock
	pendingPersistenceRollbacks sync.Map
	pendingRefreshPersistences  sync.Map
	pendingAuthRecreations      sync.Map
	pendingAuthRemovals         sync.Map
	authRemovalEpochs           sync.Map
	persistedAuthVersions       sync.Map
	deferredAuthMu              sync.Mutex
	deferredAuthContext         context.Context
	deferredAuthCancel          context.CancelFunc
	deferredAuthWorkers         sync.WaitGroup
	deferredAuthStopped         bool
	deferredAuthStopErr         error
	// refreshLocks serializes credential refresh per auth ID so concurrent
	// unauthorized responses reuse the first refreshed credential.
	refreshLocksMu sync.Mutex
	refreshLocks   map[string]*authRefreshLock

	// Auto refresh state
	refreshLifecycleMu sync.Mutex
	refreshCancel      context.CancelFunc
	refreshWorkers     sync.WaitGroup
	refreshStopping    bool
	refreshSemaphore   chan struct{}
}

// NewManager constructs a manager with optional custom selector and hook.
func NewManager(store Store, selector Selector, hook Hook) *Manager {
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	if hook == nil {
		hook = NoopHook{}
	}
	deferredAuthContext, deferredAuthCancel := context.WithCancel(context.Background())
	manager := &Manager{
		store:                store,
		executors:            make(map[string]ProviderExecutor),
		selector:             selector,
		hook:                 hook,
		auths:                make(map[string]*Auth),
		homeRuntimeAuths:     make(map[string]map[string]*Auth),
		removedAuths:         make(map[string]uint64),
		removedAuthAt:        make(map[string]time.Time),
		pendingAuthDeletions: make(map[string]*authDeletionIntent),
		providerOffsets:      make(map[string]int),
		requestPrepareLocks:  make(map[string]*requestAuthPrepareLock),
		authMutationLocks:    make(map[string]*authMutationLock),
		refreshLocks:         make(map[string]*authRefreshLock),
		persistenceLock:      newAuthMutationLock(),
		deferredAuthContext:  deferredAuthContext,
		deferredAuthCancel:   deferredAuthCancel,
		refreshSemaphore:     make(chan struct{}, refreshMaxConcurrency),
	}
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(&internalconfig.Config{})
	manager.apiKeyModelAlias.Store(apiKeyModelAliasTable(nil))
	manager.scheduler = newAuthScheduler(selector)
	return manager
}

// SetDeferredAuthRecreateHandler installs the service callback used after a blocked watcher recreate.
func (m *Manager) SetDeferredAuthRecreateHandler(handler func(context.Context, *Auth) error) {
	if m == nil {
		return
	}
	m.deferredAuthMu.Lock()
	defer m.deferredAuthMu.Unlock()
	if m.deferredAuthStopped {
		return
	}
	m.mu.Lock()
	m.deferredAuthRecreate = handler
	m.mu.Unlock()
}

// StopDeferredAuthRecreates cancels and waits for all deferred recreation workers.
func (m *Manager) StopDeferredAuthRecreates(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.deferredAuthMu.Lock()
	if !m.deferredAuthStopped {
		m.deferredAuthStopped = true
		if m.deferredAuthCancel != nil {
			m.deferredAuthCancel()
		}
	}
	m.mu.Lock()
	m.deferredAuthRecreate = nil
	m.mu.Unlock()
	m.deferredAuthMu.Unlock()

	done := make(chan struct{})
	go func() {
		m.deferredAuthWorkers.Wait()
		close(done)
	}()
	select {
	case <-done:
		m.deferredAuthMu.Lock()
		errStop := m.deferredAuthStopErr
		m.deferredAuthMu.Unlock()
		return errStop
	case <-ctx.Done():
		return errors.Join(ctx.Err(), errAuthDurableStateUncertain)
	}
}

func (m *Manager) recordDeferredAuthStopError(err error) {
	if m == nil || err == nil {
		return
	}
	m.deferredAuthMu.Lock()
	m.deferredAuthStopErr = errors.Join(m.deferredAuthStopErr, err)
	m.deferredAuthMu.Unlock()
}

// RefreshSchedulerEntry re-upserts a single auth into the scheduler so that its
// supportedModelSet is rebuilt from the current global model registry state.
// This must be called after models have been registered for a newly added auth,
// because the initial scheduler.upsertAuth during Register/Update runs before
// registerModelsForAuth and therefore snapshots an empty model set.
func isBuiltInSelector(selector Selector) bool {
	switch unwrapSchedulerSelector(selector).(type) {
	case *RoundRobinSelector, *FillFirstSelector:
		return true
	default:
		return false
	}
}

func (m *Manager) syncSchedulerFromSnapshot(auths []*Auth) {
	if m == nil || m.scheduler == nil {
		return
	}
	m.scheduler.rebuild(auths)
}

func (m *Manager) syncScheduler() {
	if m == nil || m.scheduler == nil {
		return
	}
	m.syncSchedulerFromSnapshot(m.snapshotAuths())
}

func (m *Manager) RefreshSchedulerEntry(authID string) {
	if m == nil || m.scheduler == nil || authID == "" {
		return
	}
	m.mu.RLock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.RUnlock()
		return
	}
	snapshot := auth.Clone()
	m.mu.RUnlock()
	m.scheduler.upsertAuth(snapshot)
}

func (m *Manager) RefreshSchedulerAll() {
	if m == nil {
		return
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.auths))
	for id := range m.auths {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	for _, id := range ids {
		m.RefreshSchedulerEntry(id)
	}
}

func (m *Manager) SetPluginScheduler(scheduler PluginScheduler) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pluginScheduler = scheduler
	m.mu.Unlock()
}

func (m *Manager) SetSelector(selector Selector) {
	if m == nil {
		return
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	m.mu.Lock()
	previous := m.selector
	m.selector = selector
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.setSelector(selector)
		m.syncScheduler()
	}
	if stoppable, ok := previous.(StoppableSelector); ok && stoppable != nil && previous != selector {
		stoppable.Stop()
	}
}

// SetStore swaps the underlying persistence store.
func (m *Manager) SetStore(store Store) {
	unlockPersistence, errLock := m.lockPersistence(context.Background())
	if errLock != nil {
		return
	}
	defer unlockPersistence()
	m.mu.Lock()
	m.store = store
	m.mu.Unlock()
}

// SetRoundTripperProvider register a provider that returns a per-auth RoundTripper.
func (m *Manager) SetRoundTripperProvider(p RoundTripperProvider) {
	m.mu.Lock()
	m.rtProvider = p
	m.mu.Unlock()
}

func (m *Manager) SetHomeClientProvider(provider func() HomeClient) {
	if m == nil {
		return
	}
	m.homeClientProviderMu.Lock()
	m.homeClientProvider = provider
	m.homeClientProviderMu.Unlock()
}

func (m *Manager) configuredHomeClient() (HomeClient, bool) {
	if m == nil {
		return nil, false
	}
	m.homeClientProviderMu.RLock()
	provider := m.homeClientProvider
	m.homeClientProviderMu.RUnlock()
	if provider == nil {
		return nil, false
	}
	return provider(), true
}

func (m *Manager) homeDispatcher() homeAuthDispatcher {
	if client, configured := m.configuredHomeClient(); configured {
		return client
	}
	return currentHomeDispatcher()
}

func (m *Manager) homeRefreshClient() HomeRefreshClient {
	if client, configured := m.configuredHomeClient(); configured {
		return client
	}
	return currentHomeRuntimeRefreshClient()
}

func (m *Manager) withHomeRefreshClient(ctx context.Context) context.Context {
	return WithHomeRefreshClient(ctx, m.homeRefreshClient())
}

// SetConfig updates the runtime config snapshot used by request-time helpers.
// Callers should provide the latest config on reload so per-credential alias mapping stays in sync.
func (m *Manager) SetConfig(cfg *config.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.runtimeConfig.Store(cfg)
	m.mu.Lock()
	m.configureRefreshSemaphoreLocked(cfg)
	m.mu.Unlock()
	if !cfg.Home.Enabled {
		m.clearHomeRuntimeAuths()
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
}

func (m *Manager) autoRefreshWorkers(cfg *internalconfig.Config) int {
	if cfg == nil {
		cfg, _ = m.runtimeConfig.Load().(*internalconfig.Config)
	}
	if cfg != nil && cfg.AuthAutoRefreshWorkers > 0 {
		return cfg.AuthAutoRefreshWorkers
	}
	return refreshMaxConcurrency
}

func (m *Manager) configureRefreshSemaphoreLocked(cfg *internalconfig.Config) {
	workers := m.autoRefreshWorkers(cfg)
	if m.refreshSemaphore == nil || cap(m.refreshSemaphore) != workers {
		m.refreshSemaphore = make(chan struct{}, workers)
	}
}

func (m *Manager) HomeEnabled() bool {
	if m == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg != nil && cfg.Home.Enabled
}

func (m *Manager) lookupAPIKeyUpstreamModel(authID, requestedModel string) string {
	if m == nil {
		return ""
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ""
	}
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return ""
	}
	table, _ := m.apiKeyModelAlias.Load().(apiKeyModelAliasTable)
	if table == nil {
		return ""
	}
	byAlias := table[authID]
	if len(byAlias) == 0 {
		return ""
	}
	key := strings.ToLower(thinking.ParseSuffix(requestedModel).ModelName)
	if key == "" {
		key = strings.ToLower(requestedModel)
	}
	resolved := strings.TrimSpace(byAlias[key])
	if resolved == "" {
		return ""
	}
	return thinking.PreserveRequestModelDecorations(resolved, requestedModel)

}

func (m *Manager) rebuildAPIKeyModelAliasFromRuntimeConfig() {
	if m == nil {
		return
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rebuildAPIKeyModelAliasLocked(cfg)
}

func (m *Manager) rebuildAPIKeyModelAliasLocked(cfg *internalconfig.Config) {
	if m == nil {
		return
	}
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	out := make(apiKeyModelAliasTable)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		if strings.TrimSpace(auth.ID) == "" {
			continue
		}
		kind, _ := auth.AccountInfo()
		if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			continue
		}

		byAlias := make(map[string]string)
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		switch provider {
		case "gemini":
			if entry := resolveGeminiAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "gemini-interactions":
			if entry := resolveInteractionsAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "claude":
			if entry := resolveClaudeAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "codex":
			if entry := resolveCodexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "vertex":
			if entry := resolveVertexAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		default:
			// OpenAI-compat uses config selection from auth.Attributes.
			providerKey := ""
			compatName := ""
			if auth.Attributes != nil {
				providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
				compatName = strings.TrimSpace(auth.Attributes["compat_name"])
			}
			if compatName != "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
				if entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider); entry != nil {
					compileAPIKeyModelAliasForModels(byAlias, entry.Models)
				}
			}
		}

		if len(byAlias) > 0 {
			out[auth.ID] = byAlias
		}
	}

	m.apiKeyModelAlias.Store(out)
}

func compileAPIKeyModelAliasForModels[T interface {
	GetName() string
	GetAlias() string
}](out map[string]string, models []T) {
	if out == nil {
		return
	}
	for i := range models {
		alias := strings.TrimSpace(models[i].GetAlias())
		name := strings.TrimSpace(models[i].GetName())
		if alias == "" || name == "" {
			continue
		}
		aliasKey := strings.ToLower(thinking.ParseSuffix(alias).ModelName)
		if aliasKey == "" {
			aliasKey = strings.ToLower(alias)
		}
		// Config priority: first alias wins.
		if _, exists := out[aliasKey]; exists {
			continue
		}
		out[aliasKey] = name
		// Also allow direct lookup by upstream name (case-insensitive), so lookups on already-upstream
		// models remain a cheap no-op.
		nameKey := strings.ToLower(thinking.ParseSuffix(name).ModelName)
		if nameKey == "" {
			nameKey = strings.ToLower(name)
		}
		if nameKey != "" {
			if _, exists := out[nameKey]; !exists {
				out[nameKey] = name
			}
		}
		// Preserve config suffix priority by seeding a base-name lookup when name already has suffix.
		nameResult := thinking.ParseSuffix(name)
		if nameResult.HasSuffix {
			baseKey := strings.ToLower(strings.TrimSpace(nameResult.ModelName))
			if baseKey != "" {
				if _, exists := out[baseKey]; !exists {
					out[baseKey] = name
				}
			}
		}
	}
}

// SetRetryConfig updates retry attempts, credential retry limit and cooldown wait interval.
func (m *Manager) SetRetryConfig(retry int, maxRetryInterval time.Duration, maxRetryCredentials int) {
	if m == nil {
		return
	}
	if retry < 0 {
		retry = 0
	}
	if maxRetryCredentials < 0 {
		maxRetryCredentials = 0
	}
	if maxRetryInterval < 0 {
		maxRetryInterval = 0
	}
	m.requestRetry.Store(int32(retry))
	m.maxRetryCredentials.Store(int32(maxRetryCredentials))
	m.maxRetryInterval.Store(maxRetryInterval.Nanoseconds())
}

// RegisterExecutor registers a provider executor with the manager.
func (m *Manager) RegisterExecutor(executor ProviderExecutor) {
	m.registerExecutorIfAuthCurrent(nil, "", 0, executor)
}

// RegisterExecutorIfAuthCurrent publishes an executor only while authID still owns generation.
func (m *Manager) RegisterExecutorIfAuthCurrent(authID string, generation uint64, executor ProviderExecutor) bool {
	return m.registerExecutorIfAuthCurrent(nil, strings.TrimSpace(authID), generation, executor)
}

// RegisterExecutorIfAuthPublicationCurrent also verifies the deferred recreation intent in ctx.
func (m *Manager) RegisterExecutorIfAuthPublicationCurrent(
	ctx context.Context,
	authID string,
	generation uint64,
	executor ProviderExecutor,
) bool {
	return m.registerExecutorIfAuthCurrent(ctx, strings.TrimSpace(authID), generation, executor)
}

func (m *Manager) registerExecutorIfAuthCurrent(
	ctx context.Context,
	authID string,
	generation uint64,
	executor ProviderExecutor,
) bool {
	if m == nil || executor == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(executor.Identifier()))
	if provider == "" {
		return false
	}

	var replaced ProviderExecutor
	m.authIntentMu.Lock()
	m.executorMu.Lock()
	m.mu.Lock()
	if authID != "" {
		current := m.auths[authID]
		if current == nil || current.Generation != generation ||
			!m.authRecreationIntentCurrentLocked(ctx, authID) {
			m.mu.Unlock()
			m.executorMu.Unlock()
			m.authIntentMu.Unlock()
			return false
		}
	}
	replaced = m.executors[provider]
	m.executors[provider] = executor
	if replaced != nil && !sameProviderExecutor(replaced, executor) {
		m.clearHomeRuntimeAuthsForProviderLocked(provider)
	}
	m.mu.Unlock()
	m.executorMu.Unlock()
	m.authIntentMu.Unlock()

	if replaced != nil && !sameProviderExecutor(replaced, executor) {
		if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
			closer.CloseExecutionSession(CloseAllExecutionSessionsID)
		}
	}
	return true
}

// RegisterExecutorIfAbsent registers an executor only when the provider has no owner.
func (m *Manager) RegisterExecutorIfAbsent(executor ProviderExecutor) bool {
	if m == nil || executor == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(executor.Identifier()))
	if provider == "" {
		return false
	}
	m.executorMu.Lock()
	m.mu.Lock()
	if current := m.executors[provider]; current != nil {
		m.mu.Unlock()
		m.executorMu.Unlock()
		return false
	}
	m.executors[provider] = executor
	m.mu.Unlock()
	m.executorMu.Unlock()
	return true
}

// ReplaceExecutorIfSame replaces an executor only when expected still owns the provider.
func (m *Manager) ReplaceExecutorIfSame(expected, replacement ProviderExecutor) bool {
	if m == nil || expected == nil || replacement == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(replacement.Identifier()))
	if provider == "" {
		return false
	}
	m.executorMu.Lock()
	m.mu.Lock()
	current := m.executors[provider]
	if !sameProviderExecutor(current, expected) {
		m.mu.Unlock()
		m.executorMu.Unlock()
		return false
	}
	m.executors[provider] = replacement
	if !sameProviderExecutor(expected, replacement) {
		m.clearHomeRuntimeAuthsForProviderLocked(provider)
	}
	m.mu.Unlock()
	m.executorMu.Unlock()
	if !sameProviderExecutor(expected, replacement) {
		if closer, ok := expected.(ExecutionSessionCloser); ok && closer != nil {
			closer.CloseExecutionSession(CloseAllExecutionSessionsID)
		}
	}
	return true
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.executorMu.Lock()
	m.mu.Lock()
	if m.executors[provider] != nil {
		delete(m.executors, provider)
		m.clearHomeRuntimeAuthsForProviderLocked(provider)
	}
	m.mu.Unlock()
	m.executorMu.Unlock()
}

// UnregisterExecutorIfSame removes an executor only when expected still owns the provider.
func (m *Manager) UnregisterExecutorIfSame(provider string, expected ProviderExecutor) bool {
	if m == nil || expected == nil {
		return false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return false
	}
	m.executorMu.Lock()
	m.mu.Lock()
	if !sameProviderExecutor(m.executors[provider], expected) {
		m.mu.Unlock()
		m.executorMu.Unlock()
		return false
	}
	delete(m.executors, provider)
	m.clearHomeRuntimeAuthsForProviderLocked(provider)
	m.mu.Unlock()
	m.executorMu.Unlock()
	return true
}

func sameProviderExecutor(left, right ProviderExecutor) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	if leftValue.Type() != rightValue.Type() {
		return false
	}
	if leftValue.Type().Comparable() {
		return leftValue.Interface() == rightValue.Interface()
	}
	switch leftValue.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return leftValue.Pointer() == rightValue.Pointer()
	default:
		return false
	}
}

func (m *Manager) authRemovalEpoch(id string) uint64 {
	if m == nil {
		return 0
	}
	value, ok := m.authRemovalEpochs.Load(strings.TrimSpace(id))
	if !ok {
		return 0
	}
	epoch, _ := value.(*atomic.Uint64)
	if epoch == nil {
		return 0
	}
	return epoch.Load()
}

func (m *Manager) advanceAuthRemovalEpoch(id string) uint64 {
	if m == nil {
		return 0
	}
	value, _ := m.authRemovalEpochs.LoadOrStore(strings.TrimSpace(id), &atomic.Uint64{})
	epoch, _ := value.(*atomic.Uint64)
	if epoch == nil {
		return 0
	}
	return epoch.Add(1)
}

func (m *Manager) queueAuthRecreation(ctx context.Context, auth *Auth, pending <-chan struct{}) bool {
	if m == nil || auth == nil || strings.TrimSpace(auth.ID) == "" || pending == nil {
		return false
	}
	id := strings.TrimSpace(auth.ID)
	m.deferredAuthMu.Lock()
	if m.deferredAuthStopped || m.deferredAuthContext == nil {
		m.deferredAuthMu.Unlock()
		return false
	}
	intentContext, cancelIntent := context.WithCancel(m.deferredAuthContext)
	removalEpoch := m.authRemovalEpoch(id)
	if ctx != nil {
		if capturedEpoch, ok := ctx.Value(authMutationRemovalEpochContextKey{}).(uint64); ok {
			removalEpoch = capturedEpoch
		}
	}
	intent := &authRecreationIntent{
		auth:         auth.Clone(),
		pending:      pending,
		ctx:          intentContext,
		cancel:       cancelIntent,
		removalEpoch: removalEpoch,
		done:         make(chan struct{}),
	}
	m.authIntentMu.Lock()
	previous, replaced := m.pendingAuthRecreations.Swap(id, intent)
	m.deferredAuthWorkers.Add(1)
	m.authIntentMu.Unlock()
	m.deferredAuthMu.Unlock()
	if replaced {
		if previousIntent, ok := previous.(*authRecreationIntent); ok && previousIntent != nil && previousIntent.cancel != nil {
			previousIntent.cancel()
		}
	}
	delay, maxDelay := deferredAuthRetryDelays()
	go m.runAuthRecreation(id, intent, delay, maxDelay)
	return true
}

func deferredAuthRetryDelays() (time.Duration, time.Duration) {
	delay := deferredAuthRecreateRetryInitialDelay
	if delay <= 0 {
		delay = 100 * time.Millisecond
	}
	maxDelay := deferredAuthRecreateRetryMaxDelay
	if maxDelay < delay {
		maxDelay = delay
	}
	return delay, maxDelay
}

func (m *Manager) recordStoppedAuthRecreation(id string, intent *authRecreationIntent) {
	if m == nil || intent == nil || intent.superseded.Load() {
		return
	}
	m.authIntentMu.Lock()
	current, ok := m.pendingAuthRecreations.Load(strings.TrimSpace(id))
	m.authIntentMu.Unlock()
	if ok && current == intent {
		m.recordDeferredAuthStopError(errors.Join(
			errAuthDurableStateUncertain,
			fmt.Errorf("deferred auth recreation stopped for %s", strings.TrimSpace(id)),
		))
	}
}

func (m *Manager) runAuthRecreation(
	id string,
	intent *authRecreationIntent,
	delay time.Duration,
	maxDelay time.Duration,
) {
	defer m.deferredAuthWorkers.Done()
	defer close(intent.done)
	defer func() {
		m.authIntentMu.Lock()
		m.pendingAuthRecreations.CompareAndDelete(id, intent)
		m.authIntentMu.Unlock()
	}()
	defer intent.cancel()
	select {
	case <-intent.pending:
	case <-intent.ctx.Done():
		m.recordStoppedAuthRecreation(id, intent)
		return
	}

	for m.authRecreationIntentCurrent(context.WithValue(intent.ctx, authRecreationIntentContextKey{}, intent), id) {
		timeout := refreshPersistenceRollbackTimeout()
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		attemptContext, cancelAttempt := context.WithTimeout(intent.ctx, timeout)
		attemptContext = WithAllowAuthRecreate(attemptContext)
		attemptContext = context.WithValue(attemptContext, authRecreationIntentContextKey{}, intent)

		m.mu.RLock()
		handler := m.deferredAuthRecreate
		m.mu.RUnlock()
		intent.active.Store(true)
		var errRecreate error
		if handler != nil {
			errRecreate = handler(attemptContext, intent.auth.Clone())
		} else if _, exists := m.GetByID(id); exists {
			_, errRecreate = m.Update(attemptContext, intent.auth.Clone())
		} else {
			_, errRecreate = m.Register(attemptContext, intent.auth.Clone())
		}
		intent.active.Store(false)
		cancelAttempt()
		if errRecreate == nil {
			m.authIntentMu.Lock()
			m.pendingAuthRecreations.CompareAndDelete(id, intent)
			m.authIntentMu.Unlock()
			return
		}
		if intent.ctx.Err() != nil || !m.authRecreationIntentCurrent(
			context.WithValue(intent.ctx, authRecreationIntentContextKey{}, intent), id,
		) {
			if intent.ctx.Err() != nil {
				m.recordStoppedAuthRecreation(id, intent)
			}
			return
		}
		log.WithError(errRecreate).Warnf("deferred auth recreation failed for %s; retrying", id)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-intent.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			m.recordStoppedAuthRecreation(id, intent)
			return
		}
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func (m *Manager) activeAuthRecreationDone(id string) <-chan struct{} {
	if m == nil {
		return nil
	}
	m.authIntentMu.Lock()
	defer m.authIntentMu.Unlock()
	current, ok := m.pendingAuthRecreations.Load(strings.TrimSpace(id))
	if !ok {
		return nil
	}
	intent, _ := current.(*authRecreationIntent)
	if intent == nil || !intent.active.Load() {
		return nil
	}
	return intent.done
}

func (m *Manager) cancelPendingAuthRecreationLocked(id string) (*authRecreationIntent, bool) {
	current, ok := m.pendingAuthRecreations.Load(id)
	if !ok {
		return nil, false
	}
	intent, _ := current.(*authRecreationIntent)
	if intent != nil {
		intent.superseded.Store(true)
	}
	if intent == nil || !intent.active.Load() {
		m.pendingAuthRecreations.CompareAndDelete(id, current)
	}
	return intent, true
}

func (m *Manager) cancelPendingAuthRecreation(id string) bool {
	if m == nil {
		return false
	}
	m.authIntentMu.Lock()
	intent, canceled := m.cancelPendingAuthRecreationLocked(strings.TrimSpace(id))
	m.authIntentMu.Unlock()
	if canceled && intent != nil && intent.cancel != nil && !intent.active.Load() {
		intent.cancel()
	}
	return canceled
}

func (m *Manager) supersedeAuthRecreation(ctx context.Context, id string) bool {
	if m == nil {
		return false
	}
	id = strings.TrimSpace(id)
	m.authIntentMu.Lock()
	if ctx != nil {
		if intent, ok := ctx.Value(authRecreationIntentContextKey{}).(*authRecreationIntent); ok && intent != nil {
			current, currentOK := m.pendingAuthRecreations.Load(id)
			if currentOK && current == intent {
				m.authIntentMu.Unlock()
				return false
			}
		}
	}
	intent, canceled := m.cancelPendingAuthRecreationLocked(id)
	m.authIntentMu.Unlock()
	if canceled && intent != nil && intent.cancel != nil && !intent.active.Load() {
		intent.cancel()
	}
	return canceled
}

// SupersedeAuthRecreation cancels an older deferred recreation unless ctx owns it.
func (m *Manager) SupersedeAuthRecreation(ctx context.Context, id string) bool {
	return m.supersedeAuthRecreation(ctx, id)
}

func (m *Manager) prepareDirectAuthMutation(ctx context.Context, id string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	m.cancelPendingRefreshPersistence(id)
	if _, captured := ctx.Value(authMutationRemovalEpochContextKey{}).(uint64); !captured {
		ctx = context.WithValue(ctx, authMutationRemovalEpochContextKey{}, m.authRemovalEpoch(id))
	}
	m.authIntentMu.Lock()
	deferredIntent, deferredMutation := ctx.Value(authRecreationIntentContextKey{}).(*authRecreationIntent)
	deferredMutationCurrent := m.authRecreationIntentCurrentLocked(ctx, id)
	_, pendingRecreation := m.pendingAuthRecreations.Load(id)
	m.authIntentMu.Unlock()
	if deferredMutation && !deferredMutationCurrent {
		return ctx
	}
	if deferredMutation && deferredIntent != nil {
		pendingRecreation = false
	}

	m.mu.RLock()
	_, pendingDeletion := m.pendingAuthDeletions[id]
	_, removed := m.removedAuths[id]
	m.mu.RUnlock()
	forcePersist := pendingDeletion || pendingRecreation || shouldAllowAuthRecreate(ctx) && shouldSkipPersist(ctx) && removed
	if forcePersist {
		return WithForcePersist(ctx)
	}
	return ctx
}

func (m *Manager) completeDirectAuthMutation(ctx context.Context, id string) {
	m.supersedeAuthDeletion(id)
	m.supersedeAuthRecreation(ctx, id)
}

func (m *Manager) authRecreationIntentCurrentLocked(ctx context.Context, id string) bool {
	if ctx == nil {
		return true
	}
	intent, ok := ctx.Value(authRecreationIntentContextKey{}).(*authRecreationIntent)
	if !ok || intent == nil {
		return true
	}
	if err := ctx.Err(); err != nil || intent.superseded.Load() {
		return false
	}
	current, currentOK := m.pendingAuthRecreations.Load(id)
	if !currentOK || current != intent {
		return false
	}
	return m.authRemovalEpoch(id) == intent.removalEpoch
}

func (m *Manager) authRecreationIntentCurrent(ctx context.Context, id string) bool {
	if m == nil {
		return false
	}
	m.authIntentMu.Lock()
	defer m.authIntentMu.Unlock()
	return m.authRecreationIntentCurrentLocked(ctx, strings.TrimSpace(id))
}

// AuthRecreationIntentCurrent reports whether a deferred recreation still owns this auth ID.
func (m *Manager) AuthRecreationIntentCurrent(ctx context.Context, id string) bool {
	return m.authRecreationIntentCurrent(ctx, strings.TrimSpace(id))
}

func (m *Manager) authPublicationContextErrorLocked(ctx context.Context, id string, persisted bool) error {
	var errPublication error
	if ctx != nil {
		errPublication = ctx.Err()
	}
	if errPublication == nil && !m.authRecreationIntentCurrentLocked(ctx, strings.TrimSpace(id)) {
		errPublication = errors.New("credential recreation was superseded")
	}
	if errPublication == nil {
		return nil
	}
	if persisted {
		return errors.Join(errAuthDurableStateUncertain, errPublication)
	}
	return errPublication
}

func (m *Manager) authPublicationContextError(ctx context.Context, id string, persisted bool) error {
	m.authIntentMu.Lock()
	defer m.authIntentMu.Unlock()
	return m.authPublicationContextErrorLocked(ctx, id, persisted)
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	if m.rejectedWatcherAuthMutation(ctx, auth) {
		return nil, errors.Join(
			errAuthDurableStateUncertain,
			fmt.Errorf("ignored watcher recreation of rejected credential %s", strings.TrimSpace(auth.ID)),
		)
	}
	checkWatcherReadTime := shouldAllowAuthRecreate(ctx) && shouldSkipPersist(ctx) && !shouldForcePersist(ctx)
	ctx = m.prepareDirectAuthMutation(ctx, auth.ID)
	allowRecreate := shouldAllowAuthRecreate(ctx)
	unlockMutation, errMutation := m.lockAuthMutation(ctx, auth.ID)
	if errMutation != nil {
		if pending, ok := m.pendingPersistenceRollback(auth.ID); allowRecreate && ok &&
			m.queueAuthRecreation(ctx, auth, pending) {
			return nil, errMutation
		}
		unlockMutation, errMutation = m.lockAuthMutation(ctx, auth.ID)
		if errMutation != nil {
			return nil, errMutation
		}
	}
	if !m.authRecreationIntentCurrent(ctx, auth.ID) {
		unlockMutation()
		return nil, nil
	}
	if checkWatcherReadTime && m.staleWatcherFileMutation(auth) {
		unlockMutation()
		return nil, fmt.Errorf("ignored stale watcher mutation for credential %s", strings.TrimSpace(auth.ID))
	}
	if checkWatcherReadTime && m.staleRemovedWatcherAuthMutation(auth) {
		unlockMutation()
		return nil, fmt.Errorf("ignored watcher mutation read before credential %s was removed", strings.TrimSpace(auth.ID))
	}
	mutationLocked := true
	defer func() {
		if mutationLocked {
			unlockMutation()
		}
	}()
	m.mu.RLock()
	removedSeq, removed := m.removedAuths[auth.ID]
	m.mu.RUnlock()
	removedAtStart := removed
	if removed && !allowRecreate {
		return nil, nil
	}
	auth.EnsureIndex()
	persisted, errPersist := m.persist(ctx, auth, allowRecreate, removedAtStart, removedSeq)
	if errPersist != nil {
		return nil, errPersist
	}
	if errPublication := m.authPublicationContextError(ctx, auth.ID, persisted); errPublication != nil {
		return nil, errPublication
	}
	snapshot := auth.Clone()
	m.authIntentMu.Lock()
	m.mu.Lock()
	if errPublication := m.authPublicationContextErrorLocked(ctx, auth.ID, persisted); errPublication != nil {
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		return nil, errPublication
	}
	if currentSeq, removed := m.removedAuths[auth.ID]; removed && (!allowRecreate || !removedAtStart || currentSeq != removedSeq) {
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		if persisted {
			return nil, m.scheduleRejectedPersistedAuthDeletion(auth.ID, snapshot)
		}
		return nil, nil
	}
	m.assignAuthGenerationLocked(snapshot)
	m.auths[auth.ID] = snapshot
	if allowRecreate && removedAtStart {
		m.clearAuthRemovalLocked(auth.ID)
	}
	m.mu.Unlock()
	m.authIntentMu.Unlock()
	m.completeDirectAuthMutation(ctx, auth.ID)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot.Clone())
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	unlockMutation()
	mutationLocked = false
	m.hook.OnAuthRegistered(ctx, snapshot.Clone())
	return snapshot.Clone(), nil
}

// Update applies persistent provider-owned fields and preserves the latest
// runtime quota and lifecycle state.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	if m.rejectedWatcherAuthMutation(ctx, auth) {
		return nil, errors.Join(
			errAuthDurableStateUncertain,
			fmt.Errorf("ignored watcher update of rejected credential %s", strings.TrimSpace(auth.ID)),
		)
	}
	checkWatcherReadTime := shouldAllowAuthRecreate(ctx) && shouldSkipPersist(ctx) && !shouldForcePersist(ctx)
	ctx = m.prepareDirectAuthMutation(ctx, auth.ID)
	allowRecreate := shouldAllowAuthRecreate(ctx)
	unlockMutation, errMutation := m.lockAuthMutation(ctx, auth.ID)
	if errMutation != nil {
		if pending, ok := m.pendingPersistenceRollback(auth.ID); allowRecreate && ok &&
			m.queueAuthRecreation(ctx, auth, pending) {
			return nil, errMutation
		}
		unlockMutation, errMutation = m.lockAuthMutation(ctx, auth.ID)
		if errMutation != nil {
			return nil, errMutation
		}
	}
	if !m.authRecreationIntentCurrent(ctx, auth.ID) {
		unlockMutation()
		return nil, nil
	}
	if checkWatcherReadTime && m.staleWatcherFileMutation(auth) {
		unlockMutation()
		return nil, fmt.Errorf("ignored stale watcher mutation for credential %s", strings.TrimSpace(auth.ID))
	}
	if checkWatcherReadTime && m.staleRemovedWatcherAuthMutation(auth) {
		unlockMutation()
		return nil, fmt.Errorf("ignored watcher mutation read before credential %s was removed", strings.TrimSpace(auth.ID))
	}
	updated, errUpdate := m.updateAuthLocked(ctx, auth, true)
	unlockMutation()
	if updated != nil {
		m.hook.OnAuthUpdated(ctx, updated.Clone())
	}
	return updated, errUpdate
}

// MutatePersistentAuth serializes an external durable credential mutation with
// manager-owned auth updates. The callback must return the persisted auth state.
func (m *Manager) MutatePersistentAuth(
	ctx context.Context,
	id string,
	mutate func(context.Context) (*Auth, error),
) (updated *Auth, err error) {
	if m == nil || mutate == nil {
		return nil, nil
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("auth id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = m.prepareDirectAuthMutation(ctx, id)

	unlockMutation, errMutation := m.lockAuthMutation(ctx, id)
	if errMutation != nil {
		return nil, errMutation
	}
	registered := false
	defer func() {
		unlockMutation()
		if updated == nil {
			return
		}
		if registered {
			m.hook.OnAuthRegistered(ctx, updated.Clone())
		} else {
			m.hook.OnAuthUpdated(ctx, updated.Clone())
		}
	}()

	allowRecreate := shouldAllowAuthRecreate(ctx)
	m.mu.RLock()
	removedSeq, removedAtStart := m.removedAuths[id]
	_, existedAtStart := m.auths[id]
	m.mu.RUnlock()
	registered = !existedAtStart
	if removedAtStart && !allowRecreate {
		return nil, nil
	}

	unlockPersistence, errPersistenceLock := m.lockPersistence(ctx)
	if errPersistenceLock != nil {
		return nil, errPersistenceLock
	}
	next, errMutate := mutate(ctx)
	unlockPersistence()
	if errMutate != nil {
		return nil, errMutate
	}
	if next == nil {
		return nil, fmt.Errorf("persistent auth mutation returned nil auth")
	}
	if strings.TrimSpace(next.ID) != id {
		return nil, fmt.Errorf("persistent auth mutation returned auth %q, want %q", next.ID, id)
	}
	persisted := !shouldSkipPersist(ctx) || shouldForcePersist(ctx)
	publicationCtx := context.WithoutCancel(ctx)
	if errPublication := m.authPublicationContextError(publicationCtx, id, persisted); errPublication != nil {
		return nil, errPublication
	}

	m.authIntentMu.Lock()
	m.mu.Lock()
	if errPublication := m.authPublicationContextErrorLocked(publicationCtx, id, persisted); errPublication != nil {
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		return nil, errPublication
	}
	if currentSeq, removed := m.removedAuths[id]; removed &&
		(!allowRecreate || !removedAtStart || currentSeq != removedSeq) {
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		if persisted {
			return nil, m.scheduleRejectedPersistedAuthDeletion(id, next)
		}
		return nil, nil
	}
	if current := m.auths[id]; current != nil {
		next = mergePersistentAuthState(current, next)
	} else {
		next = next.Clone()
	}
	next.EnsureIndex()
	snapshot := next.Clone()
	m.assignAuthGenerationLocked(snapshot)
	m.auths[id] = snapshot
	if allowRecreate && removedAtStart {
		m.clearAuthRemovalLocked(id)
	}
	m.mu.Unlock()
	m.authIntentMu.Unlock()
	m.completeDirectAuthMutation(publicationCtx, id)

	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot.Clone())
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	return snapshot.Clone(), nil
}

func (m *Manager) updateAuthLocked(
	ctx context.Context,
	auth *Auth,
	preserveRuntime bool,
) (*Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	allowRecreate := shouldAllowAuthRecreate(ctx)
	removedAtStart := false
	var removedSeq uint64
	m.mu.Lock()
	existing, ok := m.auths[auth.ID]
	next := auth.Clone()
	if !ok || existing == nil {
		if seq, removed := m.removedAuths[auth.ID]; removed {
			removedAtStart = true
			removedSeq = seq
			if !allowRecreate {
				m.mu.Unlock()
				return nil, nil
			}
		}
	} else if preserveRuntime {
		next = mergePersistentAuthState(existing, auth)
	} else {
		if !next.indexAssigned && next.Index == "" {
			next.Index = existing.Index
			next.indexAssigned = existing.indexAssigned
		}
		next.Success = existing.Success
		next.Failed = existing.Failed
		next.recentRequests = existing.recentRequests
	}
	next.EnsureIndex()
	m.mu.Unlock()
	persisted, errPersist := m.persist(ctx, next, allowRecreate, removedAtStart, removedSeq)
	if errPersist != nil {
		return nil, errPersist
	}
	if errPublication := m.authPublicationContextError(ctx, auth.ID, persisted); errPublication != nil {
		return nil, errPublication
	}
	snapshot := next.Clone()
	m.authIntentMu.Lock()
	m.mu.Lock()
	if errPublication := m.authPublicationContextErrorLocked(ctx, auth.ID, persisted); errPublication != nil {
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		return nil, errPublication
	}
	if currentSeq, removed := m.removedAuths[auth.ID]; removed && (!allowRecreate || !removedAtStart || currentSeq != removedSeq) {
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		if persisted {
			return nil, m.scheduleRejectedPersistedAuthDeletion(auth.ID, snapshot)
		}
		return nil, nil
	}
	m.assignAuthGenerationLocked(snapshot)
	m.auths[auth.ID] = snapshot
	if allowRecreate && removedAtStart {
		m.clearAuthRemovalLocked(auth.ID)
	}
	m.mu.Unlock()
	m.authIntentMu.Unlock()
	m.completeDirectAuthMutation(ctx, auth.ID)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot.Clone())
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	return snapshot.Clone(), nil
}

// MergePersistentUpdate atomically applies persistent provider-owned fields when
// the auth identity still matches expected. Runtime quota and lifecycle state
// are preserved from the latest manager entry.
func (m *Manager) MergePersistentUpdate(
	ctx context.Context,
	expected,
	update *Auth,
) (*Auth, error) {
	return m.MergePersistentUpdateConditional(ctx, expected, update, nil)
}

// MergePersistentUpdateConditional lets the caller reserve a persistent update
// while holding an external generation lock. Once reserved, persistence and
// in-memory publication complete as one serialized auth mutation.
func (m *Manager) MergePersistentUpdateConditional(
	ctx context.Context,
	expected,
	update *Auth,
	commit func(reserve func() bool) bool,
) (merged *Auth, err error) {
	if m == nil || expected == nil || update == nil || expected.ID == "" || expected.ID != update.ID {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = m.prepareDirectAuthMutation(ctx, expected.ID)

	unlockMutation, errMutation := m.lockAuthMutation(ctx, expected.ID)
	if errMutation != nil {
		return nil, errMutation
	}
	defer func() {
		unlockMutation()
		if merged != nil {
			m.hook.OnAuthUpdated(ctx, merged.Clone())
		}
	}()
	m.mu.RLock()
	current, ok := m.auths[expected.ID]
	if !ok || current == nil || !persistentAuthStateMatches(expected, current) {
		m.mu.RUnlock()
		return nil, nil
	}
	next := mergePersistentAuthState(current, update)
	m.mu.RUnlock()
	next.EnsureIndex()

	accepted := false
	reserve := func() bool {
		if accepted {
			return false
		}
		accepted = true
		return true
	}
	if commit == nil {
		accepted = true
	} else if !commit(reserve) || !accepted {
		return nil, nil
	}
	publicationCtx := context.WithoutCancel(ctx)
	persisted, errPersist := m.persist(ctx, next, false, false, 0)
	if errPersist != nil {
		return nil, errPersist
	}
	if errPublication := m.authPublicationContextError(publicationCtx, expected.ID, persisted); errPublication != nil {
		return nil, errPublication
	}

	snapshot := next.Clone()
	m.authIntentMu.Lock()
	m.mu.Lock()
	if errPublication := m.authPublicationContextErrorLocked(publicationCtx, expected.ID, persisted); errPublication != nil {
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		return nil, errPublication
	}
	current = m.auths[expected.ID]
	_, removed := m.removedAuths[expected.ID]
	if current == nil || removed || !persistentAuthStateMatches(expected, current) {
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		if persisted && removed {
			return nil, m.scheduleRejectedPersistedAuthDeletion(expected.ID, next)
		}
		return nil, nil
	}
	m.assignAuthGenerationLocked(snapshot)
	m.auths[expected.ID] = snapshot
	m.mu.Unlock()
	m.authIntentMu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot.Clone())
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	return snapshot.Clone(), nil
}

func persistentAuthStateMatches(expected, current *Auth) bool {
	if expected == nil || current == nil {
		return false
	}
	return expected.ID == current.ID &&
		expected.Provider == current.Provider &&
		expected.Prefix == current.Prefix &&
		expected.FileName == current.FileName &&
		expected.Label == current.Label &&
		expected.Disabled == current.Disabled &&
		expected.ProxyURL == current.ProxyURL &&
		reflect.DeepEqual(expected.Attributes, current.Attributes) &&
		reflect.DeepEqual(expected.Metadata, current.Metadata) &&
		reflect.DeepEqual(expected.Storage, current.Storage)
}

func authRefreshOwnerToken(auth *Auth) string {
	if auth == nil || len(auth.Metadata) == 0 {
		return ""
	}
	if refreshToken, ok := auth.Metadata["refresh_token"].(string); ok {
		if refreshToken = strings.TrimSpace(refreshToken); refreshToken != "" {
			return refreshToken
		}
	}
	if token, ok := auth.Metadata["token"].(map[string]any); ok {
		if refreshToken, okToken := token["refresh_token"].(string); okToken {
			return strings.TrimSpace(refreshToken)
		}
	}
	return ""
}

func mergePersistentAuthState(current, update *Auth) *Auth {
	next := current.Clone()
	persistent := update.Clone()
	next.Provider = persistent.Provider
	next.Prefix = persistent.Prefix
	next.FileName = persistent.FileName
	next.Storage = persistent.Storage
	next.Label = persistent.Label
	next.Disabled = persistent.Disabled
	next.ProxyURL = persistent.ProxyURL
	next.Attributes = persistent.Attributes
	next.Metadata = persistent.Metadata
	next.NextRefreshAfter = persistent.NextRefreshAfter
	if persistent.Runtime != nil {
		next.Runtime = persistent.Runtime
	} else {
		currentOwner := authRefreshOwnerToken(current)
		persistentOwner := authRefreshOwnerToken(persistent)
		ownerChanged := !strings.EqualFold(
			strings.TrimSpace(current.Provider),
			strings.TrimSpace(persistent.Provider),
		) || !reflect.DeepEqual(current.Storage, persistent.Storage)
		if persistent.Metadata != nil && currentOwner != persistentOwner &&
			(currentOwner != "" || persistentOwner != "") {
			ownerChanged = true
		}
		if ownerChanged {
			next.Runtime = nil
		}
	}
	if !persistent.LastRefreshedAt.IsZero() {
		next.LastRefreshedAt = persistent.LastRefreshedAt
	}
	if persistent.Disabled != current.Disabled {
		next.Status = persistent.Status
		next.StatusMessage = persistent.StatusMessage
		next.Unavailable = persistent.Unavailable
	}
	next.UpdatedAt = time.Now().UTC()
	return next
}

// ResetQuota clears quota/cooldown state for an auth and resumes registry routing.
func (m *Manager) ResetQuota(ctx context.Context, authID string) (*Auth, []string, error) {
	if m == nil {
		return nil, nil, nil
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, nil, fmt.Errorf("auth id is required")
	}
	unlockMutation, errMutation := m.lockAuthMutation(ctx, authID)
	if errMutation != nil {
		return nil, nil, errMutation
	}
	mutationLocked := true
	defer func() {
		if mutationLocked {
			unlockMutation()
		}
	}()

	now := time.Now()
	models := make([]string, 0)

	m.mu.RLock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.RUnlock()
		return nil, nil, nil
	}
	updated := auth.Clone()
	m.mu.RUnlock()

	hadAuthQuota := updated.Quota.Exceeded
	for modelKey, state := range updated.ModelStates {
		modelKey = strings.TrimSpace(modelKey)
		if modelKey == "" || state == nil || !state.Quota.Exceeded {
			continue
		}
		models = append(models, modelKey)
		state.QuotaEpoch++
		resetModelState(state, now)
	}
	if len(models) == 0 && !hadAuthQuota {
		return updated, nil, nil
	}
	updateAggregatedAvailability(updated, now)
	if !updated.Disabled && updated.Status != StatusDisabled {
		if hasModelError(updated, now) {
			updateAuthErrorFromModelStates(updated, now)
		} else {
			clearAuthStateOnSuccess(updated, now)
		}
	}
	if len(models) == 0 && hadAuthQuota {
		models = append(models, modelsForRegisteredAuth(authID)...)
	}
	models = dedupeStrings(models)

	persisted, errPersist := m.persist(ctx, updated, false, false, 0)
	if errPersist != nil {
		return nil, nil, errPersist
	}
	if errPublication := m.authPublicationContextError(ctx, authID, persisted); errPublication != nil {
		return nil, nil, errPublication
	}

	snapshot := updated.Clone()
	m.authIntentMu.Lock()
	m.mu.Lock()
	if errPublication := m.authPublicationContextErrorLocked(ctx, authID, persisted); errPublication != nil {
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		return nil, nil, errPublication
	}
	current := m.auths[authID]
	_, removed := m.removedAuths[authID]
	if current == nil || removed {
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		if persisted && removed {
			return nil, nil, m.scheduleRejectedPersistedAuthDeletion(authID, updated)
		}
		return nil, nil, nil
	}
	m.assignAuthGenerationLocked(snapshot)
	m.auths[authID] = snapshot.Clone()
	m.mu.Unlock()
	m.authIntentMu.Unlock()

	for _, modelKey := range models {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(authID, modelKey)
		registry.GetGlobalRegistry().ResumeClientModel(authID, modelKey)
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot.Clone())
	}
	unlockMutation()
	mutationLocked = false
	m.hook.OnAuthUpdated(ctx, snapshot.Clone())
	return snapshot, models, nil
}

func modelsForRegisteredAuth(authID string) []string {
	supportedModels := registry.GetGlobalRegistry().GetModelsForClient(authID)
	models := make([]string, 0, len(supportedModels))
	for _, supportedModel := range supportedModels {
		if supportedModel == nil || strings.TrimSpace(supportedModel.ID) == "" {
			continue
		}
		models = append(models, supportedModel.ID)
	}
	return models
}

func dedupeStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	out := values[:0]
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// RemoveIfGeneration removes the credential only while generation still owns
// the manager entry. It keeps the auth mutation lock across durable deletion
// and runtime removal so a concurrently published replacement is preserved.
func (m *Manager) RemoveIfGeneration(
	ctx context.Context,
	id string,
	generation uint64,
) (bool, error) {
	if m == nil {
		return false, nil
	}
	id = strings.TrimSpace(id)
	if id == "" || generation == 0 {
		return false, nil
	}
	m.cancelPendingRefreshPersistence(id)
	if ctx == nil {
		ctx = context.Background()
	}

	unlockMutation, errMutation := m.lockAuthMutation(ctx, id)
	if errMutation != nil {
		return false, errMutation
	}
	defer unlockMutation()

	m.mu.RLock()
	current := m.auths[id]
	if current == nil || current.Generation != generation {
		m.mu.RUnlock()
		return false, nil
	}
	expected := current.Clone()
	provider := strings.TrimSpace(current.Provider)
	_, versionedStore := m.store.(VersionedStore)
	m.mu.RUnlock()

	persistRequested := !shouldSkipPersist(ctx) || shouldForcePersist(ctx)
	runtimeOnlyRemove := shouldRuntimeOnlyRemove(ctx)
	if persistRequested || !runtimeOnlyRemove {
		var version *PersistedVersion
		if versionedStore {
			version = m.persistedAuthVersionForRemoval(id, expected)
			if version == nil {
				return false, errors.Join(
					errAuthDurableStateUncertain,
					fmt.Errorf("credential %s has no persisted version for conditional deletion", id),
				)
			}
		}
		deleted, errDelete := m.deletePersistedAuthIfMatch(ctx, id, expected, version)
		if errDelete != nil {
			return false, errDelete
		}
		if !deleted {
			return false, nil
		}
	}

	m.advanceAuthRemovalEpoch(id)
	m.cancelPendingAuthRecreation(id)
	m.supersedeAuthDeletion(id)
	removalTime := time.Now()

	m.mu.Lock()
	current = m.auths[id]
	if current == nil || current.Generation != generation {
		m.mu.Unlock()
		return false, errors.Join(
			errAuthDurableStateUncertain,
			fmt.Errorf("credential %s changed after durable deletion", id),
		)
	}
	delete(m.auths, id)
	if m.removedAuths == nil {
		m.removedAuths = make(map[string]uint64)
	}
	m.removedAuthSeq++
	m.removedAuths[id] = m.removedAuthSeq
	m.removedAuthAt[id] = removalTime
	for sessionID, sessionAuths := range m.homeRuntimeAuths {
		if sessionAuths == nil {
			continue
		}
		if runtimeAuth := sessionAuths[id]; runtimeAuth != nil {
			if runtimeProvider := strings.TrimSpace(runtimeAuth.Provider); runtimeProvider != "" {
				provider = runtimeProvider
			}
		}
		delete(sessionAuths, id)
		if len(sessionAuths) == 0 {
			delete(m.homeRuntimeAuths, sessionID)
		}
	}
	m.mu.Unlock()

	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.removeAuth(id)
	}
	m.invalidateSessionAffinity(id)
	if provider != "" {
		if exec, ok := m.Executor(provider); ok && exec != nil {
			if closer, okCloser := exec.(ExecutionSessionCloser); okCloser {
				closer.CloseExecutionSession(CloseAllExecutionSessionsID)
			}
		}
	}
	return true, nil
}

func (m *Manager) Remove(ctx context.Context, id string) error {
	if m == nil {
		return nil
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	m.cancelPendingRefreshPersistence(id)
	if ctx == nil {
		ctx = context.Background()
	}
	removalTime := time.Now()
	m.advanceAuthRemovalEpoch(id)
	removalValue, _ := m.pendingAuthRemovals.LoadOrStore(id, newAuthRemovalIntent())
	removalIntent, _ := removalValue.(*authRemovalIntent)
	defer m.pendingAuthRemovals.CompareAndDelete(id, removalValue)
	recreationDone := m.activeAuthRecreationDone(id)
	if m.cancelPendingAuthRecreation(id) {
		ctx = WithForcePersist(ctx)
	}
	persistRequested := !shouldSkipPersist(ctx) || shouldForcePersist(ctx)
	runtimeOnlyRemove := shouldRuntimeOnlyRemove(ctx)

	m.rollbackStateMu.Lock()
	m.mu.Lock()
	existing := m.auths[id]
	persistedExisting := existing.Clone()
	provider := ""
	if existing != nil {
		provider = strings.TrimSpace(existing.Provider)
		delete(m.auths, id)
	}
	if m.removedAuths == nil {
		m.removedAuths = make(map[string]uint64)
	}
	m.removedAuthSeq++
	m.removedAuths[id] = m.removedAuthSeq
	m.removedAuthAt[id] = removalTime
	for sessionID, sessionAuths := range m.homeRuntimeAuths {
		if sessionAuths == nil {
			continue
		}
		if runtimeAuth := sessionAuths[id]; provider == "" && runtimeAuth != nil {
			provider = strings.TrimSpace(runtimeAuth.Provider)
		}
		delete(sessionAuths, id)
		if len(sessionAuths) == 0 {
			delete(m.homeRuntimeAuths, sessionID)
		}
	}
	m.mu.Unlock()
	if removalIntent != nil {
		removalIntent.markReady()
	}
	pendingRollback, _ := m.pendingPersistenceRollbackLocked(id)
	m.rollbackStateMu.Unlock()

	unlockMutation, errMutation := m.lockAuthMutation(ctx, id)
	if errMutation != nil {
		if pending, ok := m.pendingPersistenceRollback(id); ok {
			pendingRollback = pending
		} else {
			unlockMutation, errMutation = m.lockAuthMutation(ctx, id)
		}
	}
	if errMutation != nil {
		log.WithError(errMutation).Warnf("remove auth %s while persistence rollback is unfinished", id)
		unlockMutation = func() {}
	}
	defer unlockMutation()

	m.mu.Lock()
	if current := m.auths[id]; current != nil {
		if currentProvider := strings.TrimSpace(current.Provider); currentProvider != "" {
			provider = currentProvider
		}
		delete(m.auths, id)
	}
	m.removedAuthSeq++
	m.removedAuths[id] = m.removedAuthSeq
	m.removedAuthAt[id] = removalTime
	for sessionID, sessionAuths := range m.homeRuntimeAuths {
		if sessionAuths == nil {
			continue
		}
		if runtimeAuth := sessionAuths[id]; runtimeAuth != nil {
			if runtimeProvider := strings.TrimSpace(runtimeAuth.Provider); runtimeProvider != "" {
				provider = runtimeProvider
			}
		}
		delete(sessionAuths, id)
		if len(sessionAuths) == 0 {
			delete(m.homeRuntimeAuths, sessionID)
		}
	}
	m.mu.Unlock()

	var errDelete error
	if pendingRollback != nil {
		m.ensureAuthDeletionAfterPersistenceRollback(id, pendingRollback, nil)
	} else if recreationDone != nil {
		m.ensureAuthDeletionAfterPersistenceRollback(id, recreationDone, nil)
		errDelete = errAuthDurableStateUncertain
	} else if persistRequested && errMutation == nil {
		errDelete = m.deletePersistedAuth(ctx, id)
		if errDelete != nil {
			log.WithError(errDelete).Warnf("failed to delete auth %s from store", id)
		}
	} else if !runtimeOnlyRemove && errMutation == nil && persistedExisting != nil {
		if version := m.persistedAuthVersion(id); version != nil {
			_, errDelete = m.deletePersistedAuthIfMatch(ctx, id, persistedExisting, version)
			if errDelete != nil {
				log.WithError(errDelete).Warnf("failed to reconcile watcher deletion for auth %s", id)
			}
		}
	}

	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if m.scheduler != nil {
		m.scheduler.removeAuth(id)
	}
	m.invalidateSessionAffinity(id)

	if provider != "" {
		if exec, ok := m.Executor(provider); ok && exec != nil {
			if closer, okCloser := exec.(ExecutionSessionCloser); okCloser {
				closer.CloseExecutionSession(CloseAllExecutionSessionsID)
			}
		}
	}
	return errors.Join(errMutation, errDelete)
}

func rejectedPersistedAuthPublicationError(id string) error {
	return errors.Join(
		errAuthDurableStateUncertain,
		fmt.Errorf("credential %s was persisted but rejected before runtime publication", strings.TrimSpace(id)),
	)
}

func (m *Manager) scheduleRejectedPersistedAuthDeletion(id string, expected *Auth) error {
	pending := make(chan struct{})
	close(pending)
	if !m.ensureAuthDeletionAfterPersistenceRollback(strings.TrimSpace(id), pending, expected) {
		m.recordDeferredAuthStopError(errors.Join(
			errAuthDurableStateUncertain,
			fmt.Errorf("could not schedule deletion for rejected credential %s", strings.TrimSpace(id)),
		))
	}
	return rejectedPersistedAuthPublicationError(id)
}

func (m *Manager) deletePersistedAuth(ctx context.Context, id string) error {
	_, err := m.deletePersistedAuthIfMatch(ctx, id, nil, nil)
	return err
}

func persistedAuthDeleteMatches(expected, current *Auth) bool {
	if expected == nil {
		return true
	}
	if current == nil {
		return true
	}
	return expected.Disabled == current.Disabled && reflect.DeepEqual(expected.Metadata, current.Metadata)
}

func (m *Manager) rejectedWatcherAuthMutation(ctx context.Context, auth *Auth) bool {
	if m == nil || auth == nil || !shouldAllowAuthRecreate(ctx) || !shouldSkipPersist(ctx) || shouldForcePersist(ctx) {
		return false
	}
	m.mu.RLock()
	intent := m.pendingAuthDeletions[strings.TrimSpace(auth.ID)]
	var version *PersistedVersion
	if intent != nil && intent.version != nil {
		versionCopy := *intent.version
		version = &versionCopy
	}
	m.mu.RUnlock()
	if intent == nil {
		return false
	}
	if version != nil && auth.Attributes != nil {
		watcherDigest := strings.TrimSpace(auth.Attributes[PersistedSHA256Attribute])
		if watcherDigest != "" {
			return strings.EqualFold(watcherDigest, fmt.Sprintf("%x", version.SHA256))
		}
	}
	return intent.expected != nil && persistedAuthDeleteMatches(intent.expected, auth)
}

func (m *Manager) staleWatcherFileMutation(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	path := strings.TrimSpace(auth.Attributes["path"])
	digestText := strings.TrimSpace(auth.Attributes[PersistedSHA256Attribute])
	if path == "" || digestText == "" {
		return false
	}
	current, errCurrent := ReadPersistedVersion(path)
	if errCurrent != nil {
		return true
	}
	return !strings.EqualFold(digestText, hex.EncodeToString(current.SHA256[:]))
}

func (m *Manager) staleRemovedWatcherAuthMutation(auth *Auth) bool {
	if m == nil || auth == nil {
		return false
	}
	readAt := auth.UpdatedAt
	if auth.Attributes != nil {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(auth.Attributes[PersistedReadAtAttribute])); err == nil {
			readAt = parsed
		}
	}
	m.mu.RLock()
	removedAt, removed := m.removedAuthAt[strings.TrimSpace(auth.ID)]
	m.mu.RUnlock()
	return removed && (readAt.IsZero() || !readAt.After(removedAt))
}

func persistedAuthIdentityMatches(id string, expected, current *Auth) bool {
	if current == nil {
		return false
	}
	id = strings.TrimSpace(id)
	if strings.TrimSpace(current.ID) == id || strings.TrimSpace(current.FileName) == id {
		return true
	}
	if expected != nil && strings.TrimSpace(expected.FileName) != "" &&
		strings.TrimSpace(expected.FileName) == strings.TrimSpace(current.FileName) {
		return true
	}
	expectedPath := ""
	if expected != nil && expected.Attributes != nil {
		expectedPath = strings.TrimSpace(expected.Attributes["path"])
	}
	currentPath := ""
	if current.Attributes != nil {
		currentPath = strings.TrimSpace(current.Attributes["path"])
	}
	if expectedPath != "" && currentPath != "" {
		return filepath.Clean(expectedPath) == filepath.Clean(currentPath)
	}
	return currentPath != "" && filepath.Clean(id) == filepath.Clean(currentPath)
}

func (m *Manager) deletePersistedAuthIfMatch(
	ctx context.Context,
	id string,
	expected *Auth,
	version *PersistedVersion,
) (bool, error) {
	unlockPersistence, errLock := m.lockPersistence(ctx)
	if errLock != nil {
		return false, errLock
	}
	defer unlockPersistence()
	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	if store == nil {
		return true, nil
	}
	if version != nil {
		versionedStore, ok := store.(VersionedStore)
		if !ok {
			return false, errors.Join(
				errAuthDurableStateUncertain,
				fmt.Errorf("auth store does not support versioned deletion for %s", strings.TrimSpace(id)),
			)
		}
		deleteID := id
		if versionPath := strings.TrimSpace(version.Path); versionPath != "" {
			deleteID = versionPath
		}
		return versionedStore.DeleteIfVersion(ctx, deleteID, *version)
	}
	if expected != nil {
		storedAuths, errList := store.List(ctx)
		if errList != nil {
			return false, errList
		}
		for _, stored := range storedAuths {
			if !persistedAuthIdentityMatches(id, expected, stored) {
				continue
			}
			if !persistedAuthDeleteMatches(expected, stored) {
				return false, nil
			}
			break
		}
	}
	return true, store.Delete(ctx, id)
}

func (m *Manager) persistedAuthVersion(id string) *PersistedVersion {
	if m == nil {
		return nil
	}
	value, ok := m.persistedAuthVersions.Load(strings.TrimSpace(id))
	if !ok {
		return nil
	}
	version, ok := value.(PersistedVersion)
	if !ok {
		return nil
	}
	return &version
}

func (m *Manager) persistedAuthVersionForRemoval(id string, expected *Auth) *PersistedVersion {
	if version := m.persistedAuthVersion(id); version != nil && strings.TrimSpace(version.Path) != "" {
		return version
	}
	if expected == nil {
		return nil
	}
	path := ""
	digestText := ""
	if expected.Attributes != nil {
		path = strings.TrimSpace(expected.Attributes["path"])
		if path == "" {
			path = strings.TrimSpace(expected.Attributes["source"])
		}
		digestText = strings.TrimSpace(expected.Attributes[PersistedSHA256Attribute])
	}
	if path == "" {
		path = strings.TrimSpace(expected.FileName)
	}
	if path == "" {
		path = strings.TrimSpace(id)
	}
	if digestText != "" {
		digest, errDecode := hex.DecodeString(digestText)
		if errDecode == nil && len(digest) == sha256.Size {
			version := PersistedVersion{Path: path}
			copy(version.SHA256[:], digest)
			return &version
		}
	}
	version, errVersion := PersistedVersionForAuth(path, expected)
	if errVersion != nil {
		return nil
	}
	return &version
}

func (m *Manager) ensureAuthDeletionAfterPersistenceRollback(
	id string,
	pending <-chan struct{},
	expected *Auth,
) bool {
	m.deferredAuthMu.Lock()
	if m.deferredAuthStopped || m.deferredAuthContext == nil {
		m.deferredAuthMu.Unlock()
		return false
	}
	intentContext, cancelIntent := context.WithCancel(m.deferredAuthContext)
	intent := &authDeletionIntent{
		pending:  pending,
		expected: expected.Clone(),
		version:  m.persistedAuthVersion(id),
		ctx:      intentContext,
		cancel:   cancelIntent,
	}
	m.mu.Lock()
	if current := m.pendingAuthDeletions[id]; current != nil {
		if current.expected == nil && expected != nil {
			current.expected = expected.Clone()
		}
		if version := m.persistedAuthVersion(id); version != nil {
			current.version = version
		}
		m.mu.Unlock()
		m.deferredAuthMu.Unlock()
		cancelIntent()
		return true
	}
	m.pendingAuthDeletions[id] = intent
	m.deferredAuthWorkers.Add(1)
	m.mu.Unlock()
	m.deferredAuthMu.Unlock()
	delay, maxDelay := deferredAuthRetryDelays()
	go m.deleteAuthAfterPersistenceRollback(id, intent, delay, maxDelay)
	return true
}

func (m *Manager) deleteAuthAfterPersistenceRollback(
	id string,
	intent *authDeletionIntent,
	delay time.Duration,
	maxDelay time.Duration,
) {
	defer m.deferredAuthWorkers.Done()
	defer intent.cancel()
	for {
		select {
		case <-intent.pending:
		case <-intent.ctx.Done():
			if !intent.superseded.Load() {
				m.recordDeferredAuthStopError(errors.Join(
					errAuthDurableStateUncertain,
					fmt.Errorf("deferred auth deletion stopped for %s", id),
				))
			}
			return
		}

		unlockMutation, errMutation := m.lockAuthMutation(intent.ctx, id)
		if errMutation != nil {
			if next, ok := m.pendingPersistenceRollback(id); ok {
				intent.pending = next
				continue
			}
			if intent.ctx.Err() != nil {
				if !intent.superseded.Load() {
					m.recordDeferredAuthStopError(errors.Join(errAuthDurableStateUncertain, errMutation))
				}
				return
			}
			log.WithError(errMutation).Warnf("deferred auth deletion could not lock %s; retrying", id)
		} else {
			m.mu.RLock()
			currentIntent := m.pendingAuthDeletions[id]
			_, removed := m.removedAuths[id]
			current := m.auths[id]
			expected := intent.expected.Clone()
			var version *PersistedVersion
			if intent.version != nil {
				versionCopy := *intent.version
				version = &versionCopy
			}
			m.mu.RUnlock()
			if currentIntent != intent || !removed || current != nil {
				unlockMutation()
				return
			}

			timeout := refreshPersistenceRollbackTimeout()
			if timeout <= 0 {
				timeout = 5 * time.Second
			}
			intent.active.Store(true)
			deleteCtx, cancelDelete := context.WithTimeout(context.WithoutCancel(intent.ctx), timeout)
			deleted, errDelete := m.deletePersistedAuthIfMatch(deleteCtx, id, expected, version)
			cancelDelete()
			intent.active.Store(false)
			unlockMutation()
			if errDelete == nil {
				if !deleted {
					log.Warnf("deferred auth deletion preserved a newer credential for %s", id)
				}
				return
			}
			log.WithError(errDelete).Warnf("deferred auth deletion failed for %s; retrying", id)
		}

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-intent.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			if !intent.superseded.Load() {
				m.recordDeferredAuthStopError(errors.Join(
					errAuthDurableStateUncertain,
					fmt.Errorf("deferred auth deletion stopped for %s", id),
				))
			}
			return
		}
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func (m *Manager) invalidateSessionAffinity(authID string) {
	if m == nil || authID == "" {
		return
	}
	if invalidator, ok := m.selector.(interface{ InvalidateAuth(string) }); ok && invalidator != nil {
		invalidator.InvalidateAuth(authID)
	}
}

// Load resets manager state from the backing store.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.store == nil {
		return nil
	}
	items, err := m.store.List(ctx)
	if err != nil {
		return err
	}
	m.auths = make(map[string]*Auth, len(items))
	loaded := make([]*Auth, 0, len(items))
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		if _, pendingDelete := m.pendingAuthDeletions[auth.ID]; pendingDelete {
			continue
		}
		if _, removed := m.removedAuths[auth.ID]; removed {
			delete(m.removedAuths, auth.ID)
		}
		auth.EnsureIndex()
		snapshot := auth.Clone()
		m.assignAuthGenerationLocked(snapshot)
		m.auths[auth.ID] = snapshot
		loaded = append(loaded, snapshot.Clone())
	}
	if m.scheduler != nil {
		m.scheduler.rebuild(loaded)
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	return nil
}

// Execute performs a non-streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) Execute(
	ctx context.Context,
	providers []string,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	ctx = m.withHomeRefreshClient(ctx)
	opts = ensureRequestedModelMetadata(opts, req.Model)
	resp, err := retryWithCooldown(
		m, ctx, providers, req, opts, func(
			innerCtx context.Context,
			innerProviders []string,
			innerReq cliproxyexecutor.Request,
			innerOpts cliproxyexecutor.Options,
			maxCreds int,
		) (cliproxyexecutor.Response, error) {
			return mixedOnce(
				m, innerCtx, innerProviders, innerReq, innerOpts, maxCreds,
				func(
					execCtx context.Context,
					auth *Auth,
					executor ProviderExecutor,
					_ string,
					execReq cliproxyexecutor.Request,
					execOpts cliproxyexecutor.Options,
					aliasResult OAuthModelAliasResult,
				) (cliproxyexecutor.Response, error) {
					resp, errExec := executor.Execute(execCtx, auth, execReq, execOpts)
					if errExec == nil {
						rewriteForceMappedResponse(&resp, aliasResult)
					}
					return resp, errExec
				},
				m.MarkResult,
			)
		},
	)
	if err != nil && hasAntigravityProvider(providers) && shouldAttemptAntigravityCreditsFallback(m, err) {
		if creditsResp, ok := m.tryAntigravityCreditsExecute(ctx, req, opts); ok {
			return creditsResp, nil
		}
	}
	return resp, err
}

// ExecuteCount performs a token-counting execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteCount(
	ctx context.Context,
	providers []string,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	ctx = m.withHomeRefreshClient(ctx)
	return retryWithCooldown(
		m, ctx, providers, req, opts, func(
			innerCtx context.Context,
			innerProviders []string,
			innerReq cliproxyexecutor.Request,
			innerOpts cliproxyexecutor.Options,
			maxCreds int,
		) (cliproxyexecutor.Response, error) {
			return mixedOnce(
				m, innerCtx, innerProviders, innerReq, innerOpts, maxCreds,
				func(
					execCtx context.Context,
					auth *Auth,
					executor ProviderExecutor,
					_ string,
					execReq cliproxyexecutor.Request,
					execOpts cliproxyexecutor.Options,
					_ OAuthModelAliasResult,
				) (cliproxyexecutor.Response, error) {
					return executor.CountTokens(execCtx, auth, execReq, execOpts)
				},
				func(resultCtx context.Context, result Result) { m.hook.OnResult(resultCtx, result) },
			)
		},
	)
}

// ExecuteStream performs a streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteStream(
	ctx context.Context,
	providers []string,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	ctx = m.withHomeRefreshClient(ctx)
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	result, err := retryWithCooldown(
		m, ctx, providers, req, opts, func(
			innerCtx context.Context,
			innerProviders []string,
			innerReq cliproxyexecutor.Request,
			innerOpts cliproxyexecutor.Options,
			maxCreds int,
		) (*cliproxyexecutor.StreamResult, error) {
			return mixedOnce(
				m, innerCtx, innerProviders, innerReq, innerOpts, maxCreds,
				func(
					execCtx context.Context,
					auth *Auth,
					executor ProviderExecutor,
					provider string,
					execReq cliproxyexecutor.Request,
					execOpts cliproxyexecutor.Options,
					aliasResult OAuthModelAliasResult,
				) (*cliproxyexecutor.StreamResult, error) {
					return m.executeStreamWithBootstrap(
						execCtx, auth, executor, provider, execReq, execOpts, routeModel, aliasResult,
					)
				},
				// Stream success recording is deferred to wrapStreamResult goroutine;
				// only record failures here to avoid double-recording.
				func(resultCtx context.Context, result Result) {
					if !result.Success {
						m.MarkResult(resultCtx, result)
					}
				},
			)
		},
	)
	if err != nil {
		if hasAntigravityProvider(providers) && shouldAttemptAntigravityCreditsFallback(m, err) {
			if creditsResult, ok := m.tryAntigravityCreditsExecuteStream(ctx, req, opts); ok {
				return creditsResult, nil
			}
		}
		if bootstrapErr, ok := errors.AsType[*streamBootstrapError](err); ok {
			return streamErrorResult(bootstrapErr.Headers(), bootstrapErr.cause), nil
		}
	}
	return result, err
}

// retryWithCooldown runs attemptFn in a retry loop, waiting for cooldown between attempts.
// It is parameterized over the return type so Execute (Response) and ExecuteStream (*StreamResult)
// share the same loop structure.
func retryWithCooldown[T any](
	m *Manager,
	ctx context.Context,
	providers []string,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	attemptFn func(
		ctx context.Context,
		providers []string,
		req cliproxyexecutor.Request,
		opts cliproxyexecutor.Options,
		maxCreds int,
	) (T, error),
) (T, error) {
	var zero T
	normalized := m.normalizeProviders(providers)
	if len(normalized) == 0 {
		return zero, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	_, maxRetryCredentials, maxWait := m.retrySettings()
	var lastErr error
	for attempt := 0; ; attempt++ {
		result, errAttempt := attemptFn(ctx, normalized, req, opts, maxRetryCredentials)
		if errAttempt == nil {
			return result, nil
		}
		lastErr = errAttempt
		wait, shouldRetry := m.shouldRetryAfterError(errAttempt, attempt, normalized, req.Model, maxWait)
		if !shouldRetry {
			break
		}
		if errWait := waitForCooldown(ctx, wait, maxWait); errWait != nil {
			return zero, errWait
		}
	}
	// noinspection GoDfaConstantCondition — lastErr is always non-nil here; intentional.
	return zero, lastErr
}

type creditsCandidateEntry struct {
	auth     *Auth
	executor ProviderExecutor
	provider string
}

func hasAntigravityProvider(providers []string) bool {
	for _, provider := range providers {
		if strings.EqualFold(strings.TrimSpace(provider), "antigravity") {
			return true
		}
	}
	return false
}

func shouldAttemptAntigravityCreditsFallback(m *Manager, lastErr error) bool {
	if m == nil || lastErr == nil {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.QuotaExceeded.AntigravityCredits {
		return false
	}
	switch statusCodeFromError(lastErr) {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	case 0:
		if authErr, ok := errors.AsType[*Error](lastErr); ok {
			return authErr.Code == "auth_not_found" || authErr.Code == "auth_unavailable" || authErr.Code == "model_cooldown"
		}
		return false
	default:
		return false
	}
}

func (m *Manager) findAllAntigravityCreditsCandidateAuths(
	routeModel string,
	opts cliproxyexecutor.Options,
) []creditsCandidateEntry {
	if m == nil || !strings.Contains(strings.ToLower(strings.TrimSpace(routeModel)), "claude") {
		return nil
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	m.mu.RLock()
	defer m.mu.RUnlock()
	var known []creditsCandidateEntry
	var unknown []creditsCandidateEntry
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled {
			continue
		}
		if pinnedAuthID != "" && auth.ID != pinnedAuthID {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
			continue
		}
		executor, ok := m.executors["antigravity"]
		if !ok || executor == nil {
			continue
		}
		entry := creditsCandidateEntry{auth: auth.Clone(), executor: executor, provider: "antigravity"}
		if HasKnownAntigravityCreditsHint(auth.ID) {
			if antigravityCreditsAvailableForModel(auth, routeModel) {
				known = append(known, entry)
			}
			continue
		}
		unknown = append(unknown, entry)
	}
	sort.Slice(known, func(i, j int) bool { return known[i].auth.ID < known[j].auth.ID })
	sort.Slice(unknown, func(i, j int) bool { return unknown[i].auth.ID < unknown[j].auth.ID })
	return append(known, unknown...)
}

func (m *Manager) prepareAntigravityCreditsExecution(
	ctx context.Context, auth *Auth, routeModel string, opts cliproxyexecutor.Options,
) (context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options) {
	creditsCtx := WithAntigravityCredits(ctx)
	if rt := m.roundTripperFor(auth); rt != nil {
		creditsCtx = context.WithValue(creditsCtx, roundTripperContextKey{}, rt)
	}
	creditsOpts := ensureRequestedModelMetadata(opts, routeModel)
	creditsCtx = contextWithRequestedModelAlias(creditsCtx, creditsOpts, routeModel)
	publishSelectedAuthMetadata(creditsOpts.Metadata, auth)
	return creditsCtx, cliproxyexecutor.Request{Model: routeModel}, creditsOpts
}

func (m *Manager) antigravityCreditsModelAliasResult(routeModel string, auth *Auth) OAuthModelAliasResult {
	model := rewriteModelForAuth(routeModel, auth)
	aliasResult := m.applyOAuthModelAliasWithResult(auth, model)
	aliasResult.UpstreamModel = m.applyAPIKeyModelAlias(auth, aliasResult.UpstreamModel)
	return aliasResult
}

func (m *Manager) tryAntigravityCreditsExecute(
	ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, bool) {
	for _, candidate := range m.findAllAntigravityCreditsCandidateAuths(req.Model, opts) {
		if errCtx := ctx.Err(); errCtx != nil {
			return cliproxyexecutor.Response{}, false
		}
		creditsCtx, execReq, creditsOpts := m.prepareAntigravityCreditsExecution(ctx, candidate.auth, req.Model, opts)
		execReq.Payload = req.Payload
		aliasResult := m.antigravityCreditsModelAliasResult(req.Model, candidate.auth)
		execReq.Model = aliasResult.UpstreamModel
		execReq, creditsOpts = applyRequestAfterAuthInterceptor(
			creditsCtx, candidate.executor, candidate.provider, candidate.auth, execReq, creditsOpts, req.Model,
		)
		resp, errExec := candidate.executor.Execute(creditsCtx, candidate.auth, execReq, creditsOpts)
		result := Result{
			AuthID:          candidate.auth.ID,
			AuthGeneration:  candidate.auth.Generation,
			Provider:        candidate.provider,
			Model:           req.Model,
			ModelQuotaEpoch: modelQuotaEpoch(candidate.auth, req.Model),
			Success:         errExec == nil,
		}
		if errExec != nil {
			result.Error = errorForResult(errExec)
			result.RetryAfter = retryAfterFromError(errExec)
			m.MarkResult(creditsCtx, result)
			continue
		}
		m.MarkResult(creditsCtx, result)
		rewriteForceMappedResponse(&resp, aliasResult)
		return resp, true
	}
	return cliproxyexecutor.Response{}, false
}

func (m *Manager) tryAntigravityCreditsExecuteStream(
	ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, bool) {
	for _, candidate := range m.findAllAntigravityCreditsCandidateAuths(req.Model, opts) {
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, false
		}
		creditsCtx, execReq, creditsOpts := m.prepareAntigravityCreditsExecution(ctx, candidate.auth, req.Model, opts)
		execReq.Payload = req.Payload
		aliasResult := m.antigravityCreditsModelAliasResult(req.Model, candidate.auth)
		execReq.Model = aliasResult.UpstreamModel
		execReq, creditsOpts = applyRequestAfterAuthInterceptor(
			creditsCtx, candidate.executor, candidate.provider, candidate.auth, execReq, creditsOpts, req.Model,
		)
		result, errStream := m.executeStreamWithBootstrap(
			creditsCtx, candidate.auth, candidate.executor, candidate.provider, execReq, creditsOpts, req.Model,
			aliasResult,
		)
		if errStream != nil {
			m.MarkResult(
				creditsCtx,
				Result{
					AuthID:          candidate.auth.ID,
					AuthGeneration:  candidate.auth.Generation,
					Provider:        candidate.provider,
					Model:           req.Model,
					ModelQuotaEpoch: modelQuotaEpoch(candidate.auth, req.Model),
					Success:         false,
					Error:           errorForResult(errStream),
					RetryAfter:      retryAfterFromError(errStream),
				},
			)
			continue
		}
		return result, true
	}
	return nil, false
}

// mixedOnce rotates credentials across providers for a single execution attempt.
// executeFn performs the actual provider call; recordResult records the outcome
// (e.g. MarkResult for state-changing calls, hook.OnResult for read-only calls like CountTokens).
func mixedOnce[T any](
	m *Manager,
	ctx context.Context,
	providers []string,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	maxRetryCredentials int,
	executeFn func(
		ctx context.Context,
		auth *Auth,
		executor ProviderExecutor,
		provider string,
		req cliproxyexecutor.Request,
		opts cliproxyexecutor.Options,
		aliasResult OAuthModelAliasResult,
	) (T, error),
	recordResult func(ctx context.Context, result Result),
) (T, error) {
	var zero T
	if len(providers) == 0 {
		return zero, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	homeAuthCount := 1
	tried := make(map[string]struct{})
	var lastErr error
	for {
		homeMode := m.HomeEnabled()
		if !homeMode && maxRetryCredentials > 0 && len(tried) >= maxRetryCredentials {
			if lastErr != nil {
				return zero, lastErr
			}
			return zero, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		pickOpts := opts
		if homeMode {
			pickOpts = withHomeAuthCount(opts, homeAuthCount)
		}
		auth, executor, provider, errPick := m.pickNextMixed(ctx, providers, routeModel, pickOpts, tried)
		if errPick != nil {
			if shouldReturnLastErrorOnPickFailure(homeMode, lastErr, errPick) {
				return zero, lastErr
			}
			return zero, errPick
		}

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth)

		tried[auth.ID] = struct{}{}
		runtimeOnly := auth.Generation == 0
		runtimeSessionID := ""
		var cachedRuntimeAuth *Auth
		if runtimeOnly {
			runtimeSessionID = homeExecutionSessionIDFromMetadata(opts.Metadata)
			if runtimeSessionID != "" {
				cachedRuntimeAuth = auth.Clone()
			}
		}
		execCtx := contextWithRequestedModelAlias(ctx, opts, routeModel)
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
		}
		var errPrepare error
		auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare != nil {
			result := Result{
				AuthID:          auth.ID,
				AuthGeneration:  auth.Generation,
				RuntimeOnly:     runtimeOnly,
				Provider:        provider,
				Model:           routeModel,
				ModelQuotaEpoch: modelQuotaEpoch(auth, routeModel),
				Success:         false,
				Error:           &Error{Message: errPrepare.Error()},
			}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](errPrepare); ok && se != nil {
				result.Error.HTTPStatus = se.StatusCode()
			}
			recordResult(execCtx, result)
			lastErr = errPrepare
			if runtimeOnly {
				homeAuthCount++
			}
			continue
		}
		execReq := req
		aliasResult := OAuthModelAliasResult{}
		if auth.Attributes != nil && strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]) != "" {
			execReq.Model = strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey])
		} else {
			execReq.Model = rewriteModelForAuth(routeModel, auth)
			aliasResult = m.applyOAuthModelAliasWithResult(auth, execReq.Model)
			execReq.Model = m.applyAPIKeyModelAlias(auth, aliasResult.UpstreamModel)
			aliasResult.UpstreamModel = execReq.Model
		}
		execOpts := opts
		execReq, execOpts = applyRequestAfterAuthInterceptor(
			execCtx, executor, provider, auth, execReq, execOpts, routeModel,
		)
		execReq = sanitizeDownstreamWebsocketFallbackRequest(execCtx, auth, execReq)
		resp, errExec := executeFn(execCtx, auth, executor, provider, execReq, execOpts, aliasResult)
		if errExec != nil {
			if errCtx := execCtx.Err(); errCtx != nil {
				return zero, errCtx
			}
			if refreshed, okRefresh := m.tryRefreshAfterUnauthorized(
				execCtx, auth, executor, errExec, false,
			); okRefresh {
				if runtimeOnly && cachedRuntimeAuth != nil &&
					!m.compareAndSwapHomeRuntimeAuth(
						runtimeSessionID, cachedRuntimeAuth, refreshed, executor,
					) {
					continue
				}
				auth = refreshed
				resp, errExec = executeFn(execCtx, auth, executor, provider, execReq, execOpts, aliasResult)
				if errExec != nil {
					if errCtx := execCtx.Err(); errCtx != nil {
						return zero, errCtx
					}
				}
			}
		}
		result := Result{
			AuthID:          auth.ID,
			AuthGeneration:  auth.Generation,
			RuntimeOnly:     runtimeOnly,
			Provider:        provider,
			Model:           routeModel,
			ModelQuotaEpoch: modelQuotaEpoch(auth, routeModel),
			Success:         errExec == nil,
		}
		if errExec != nil {
			result.Error = &Error{Message: errExec.Error()}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](errExec); ok && se != nil {
				result.Error.HTTPStatus = se.StatusCode()
			}
			if ra := retryAfterFromError(errExec); ra != nil {
				result.RetryAfter = ra
			}
			recordResult(execCtx, result)
			if isRequestInvalidError(errExec) {
				return zero, errExec
			}
			lastErr = errExec
			if runtimeOnly {
				homeAuthCount++
			}
			continue
		}
		recordResult(execCtx, result)
		return resp, nil
	}
}

type streamBootstrapError struct {
	cause   error
	headers http.Header
}

func newStreamBootstrapError(err error, headers http.Header) error {
	if err == nil {
		return nil
	}
	return &streamBootstrapError{cause: err, headers: cloneHTTPHeader(headers)}
}

func (e *streamBootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *streamBootstrapError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *streamBootstrapError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHTTPHeader(e.headers)
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func cloneSchedulerAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneURLValues(in url.Values) url.Values {
	if len(in) == 0 {
		return nil
	}
	out := make(url.Values, len(in))
	for key, values := range in {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func cloneExecutorRequest(req cliproxyexecutor.Request) cliproxyexecutor.Request {
	req.Payload = bytes.Clone(req.Payload)
	req.Metadata = cloneSchedulerAnyMap(req.Metadata)
	return req
}

func cloneExecutorOptions(opts cliproxyexecutor.Options) cliproxyexecutor.Options {
	opts.Headers = cloneHTTPHeader(opts.Headers)
	opts.Query = cloneURLValues(opts.Query)
	opts.OriginalRequest = bytes.Clone(opts.OriginalRequest)
	opts.Metadata = cloneSchedulerAnyMap(opts.Metadata)
	return opts
}

type requestFormatResolver interface {
	RequestToFormat(cliproxyexecutor.Request, cliproxyexecutor.Options) sdktranslator.Format
}

func requestToFormat(
	executor ProviderExecutor,
	provider string,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) sdktranslator.Format {
	if resolver, ok := executor.(requestFormatResolver); ok && resolver != nil {
		if format := resolver.RequestToFormat(req, opts); format != "" {
			return format
		}
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "claude":
		return sdktranslator.FormatClaude
	case "codex", "xai":
		return sdktranslator.FormatCodex
	case "gemini", "vertex", "aistudio":
		return sdktranslator.FormatGemini
	case "gemini-cli":
		return sdktranslator.FormatGeminiCLI
	case "antigravity":
		return sdktranslator.FormatAntigravity
	default:
		return sdktranslator.FormatOpenAI
	}
}

func applyRequestAfterAuthInterceptor(
	ctx context.Context,
	executor ProviderExecutor,
	provider string,
	auth *Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	requestedModel string,
) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	if opts.RequestAfterAuthInterceptor == nil {
		return req, opts
	}
	authID := ""
	authLabel := ""
	authProvider := provider
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		if strings.TrimSpace(auth.Provider) != "" {
			authProvider = auth.Provider
		}
	}
	resp := opts.RequestAfterAuthInterceptor(
		ctx, cliproxyexecutor.RequestAfterAuthInterceptRequest{
			SourceFormat:    opts.SourceFormat,
			ToFormat:        requestToFormat(executor, provider, req, opts),
			Model:           req.Model,
			RequestedModel:  requestedModelAliasFromOptions(opts, requestedModel),
			Stream:          opts.Stream,
			AuthID:          authID,
			AuthLabel:       authLabel,
			AuthProvider:    authProvider,
			OriginalRequest: bytes.Clone(opts.OriginalRequest),
			Request:         cloneExecutorRequest(req),
			Options:         cloneExecutorOptions(opts),
		},
	)
	if !resp.Handled {
		return req, opts
	}
	return cloneExecutorRequest(resp.Request), cloneExecutorOptions(resp.Options)
}

func schedulerAuthCandidates(auths []*Auth) []pluginapi.SchedulerAuthCandidate {
	if len(auths) == 0 {
		return nil
	}
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		out = append(
			out, pluginapi.SchedulerAuthCandidate{
				ID:         auth.ID,
				Provider:   strings.ToLower(strings.TrimSpace(auth.Provider)),
				Priority:   authPriority(auth),
				Status:     string(auth.Status),
				Attributes: schedulerSafeAttributes(auth.Attributes),
			},
		)
	}
	return out
}

func schedulerSafeAttributes(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]string, len(src))
	for key, value := range src {
		if schedulerAttributeSensitive(key) {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func schedulerAttributeSensitive(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		return true
	}
	switch key {
	case "provider_key", "provider-key", "compat_name", "compat-name", "auth_index", "auth-index", "label", "source", "scope":
		return false
	}
	sensitiveTokens := []string{
		"api_key",
		"api-key",
		"apikey",
		"authorization",
		"bearer",
		"cookie",
		"credential",
		"password",
		"proxy",
		"refresh",
		"secret",
		"storage",
		"token",
	}
	for _, token := range sensitiveTokens {
		if strings.Contains(key, token) {
			return true
		}
	}
	return false
}

func schedulerProviders(provider string, providers []string) []string {
	out := make([]string, 0, len(providers)+1)
	seen := make(map[string]struct{}, len(providers)+1)
	addProvider := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || value == "mixed" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	addProvider(provider)
	for _, value := range providers {
		addProvider(value)
	}
	return out
}

func schedulerOptions(opts cliproxyexecutor.Options) pluginapi.SchedulerOptions {
	return pluginapi.SchedulerOptions{
		Headers:  cloneHTTPHeader(opts.Headers),
		Metadata: cloneSchedulerAnyMap(opts.Metadata),
	}
}

func pickSchedulerAuthByID(candidates []*Auth, authID string) *Auth {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	for _, candidate := range candidates {
		if candidate != nil && candidate.ID == authID {
			return candidate
		}
	}
	return nil
}

func builtinSchedulerStrategy(delegate string) (schedulerStrategy, bool) {
	switch strings.TrimSpace(delegate) {
	case pluginapi.SchedulerBuiltinRoundRobin:
		return schedulerStrategyRoundRobin, true
	case pluginapi.SchedulerBuiltinFillFirst:
		return schedulerStrategyFillFirst, true
	default:
		return schedulerStrategyCustom, false
	}
}

func (m *Manager) pickViaPluginScheduler(
	ctx context.Context,
	scheduler PluginScheduler,
	provider string,
	providers []string,
	model string,
	opts cliproxyexecutor.Options,
	candidates []*Auth,
) (*Auth, bool, error) {
	if scheduler == nil || len(candidates) == 0 {
		return nil, false, nil
	}
	providerKey := strings.ToLower(strings.TrimSpace(provider))
	requestProvider := providerKey
	if providerKey == "mixed" {
		requestProvider = ""
	}
	req := pluginapi.SchedulerPickRequest{
		Provider:   requestProvider,
		Providers:  schedulerProviders(providerKey, providers),
		Model:      model,
		Stream:     opts.Stream,
		Options:    schedulerOptions(opts),
		Candidates: schedulerAuthCandidates(candidates),
	}
	resp, handled, errPick := scheduler.PickAuth(ctx, req)
	if errPick != nil {
		return nil, true, errPick
	}
	if !handled || !resp.Handled {
		return nil, false, nil
	}
	if selected := pickSchedulerAuthByID(candidates, resp.AuthID); selected != nil {
		return selected, true, nil
	}
	if _, ok := builtinSchedulerStrategy(resp.DelegateBuiltin); ok {
		return nil, false, nil
	}
	return nil, false, nil
}

func streamErrorResult(headers http.Header, err error) *cliproxyexecutor.StreamResult {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{Headers: cloneHTTPHeader(headers), Chunks: ch}
}

func rewriteForceMappedResponse(resp *cliproxyexecutor.Response, aliasResult OAuthModelAliasResult) {
	if resp == nil || !aliasResult.ForceMapping || strings.TrimSpace(aliasResult.OriginalAlias) == "" {
		return
	}
	resp.Payload = rewriteModelInResponse(resp.Payload, aliasResult.OriginalAlias)
}

func rewriteForceMappedStreamChunk(rewriter *StreamRewriter, payload []byte) []byte {
	if rewriter == nil || len(payload) == 0 {
		return payload
	}
	rewritten := rewriter.RewriteChunk(payload)
	if len(rewritten) > 0 {
		return rewritten
	}
	if len(rewriter.pendingBuf) > 0 {
		return nil
	}
	if lineWise := rewriteSSEPayloadLines(payload, rewriter.options.RewriteModel); len(lineWise) > 0 {
		return lineWise
	}
	return nil
}

func finishForceMappedStreamChunks(rewriter *StreamRewriter) []byte {
	if rewriter == nil {
		return nil
	}
	return rewriter.Finish()
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) (
	[]cliproxyexecutor.StreamChunk,
	bool,
	error,
) {
	if ch == nil {
		return nil, true, nil
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case chunk, ok = <-ch:
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			return buffered, true, nil
		}
		if chunk.Err != nil {
			return nil, false, chunk.Err
		}
		buffered = append(buffered, chunk)
		if len(chunk.Payload) > 0 {
			return buffered, false, nil
		}
	}
}

func discardStreamChunks(ch <-chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	go func() {
		for range ch {
		}
	}()
}

func (m *Manager) executeStreamWithBootstrap(
	execCtx context.Context,
	streamAuth *Auth,
	executor ProviderExecutor,
	streamProvider string,
	execReq cliproxyexecutor.Request,
	execOpts cliproxyexecutor.Options,
	routeModel string,
	aliasResult OAuthModelAliasResult,
) (*cliproxyexecutor.StreamResult, error) {
	streamResult, errStream := executor.ExecuteStream(execCtx, streamAuth, execReq, execOpts)
	if errStream != nil {
		return nil, errStream
	}
	buffered, closed, bootstrapErr := readStreamBootstrap(execCtx, streamResult.Chunks)
	if bootstrapErr != nil {
		discardStreamChunks(streamResult.Chunks)
		return nil, newStreamBootstrapError(bootstrapErr, streamResult.Headers)
	}
	if closed && len(buffered) == 0 {
		return nil, newStreamBootstrapError(
			&Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true},
			streamResult.Headers,
		)
	}
	remaining := streamResult.Chunks
	if closed {
		closedCh := make(chan cliproxyexecutor.StreamChunk)
		close(closedCh)
		remaining = closedCh
	}
	return m.wrapStreamResult(
		execCtx, streamAuth.Clone(), streamProvider, routeModel, streamResult.Headers, buffered, remaining, aliasResult,
	), nil
}

func (m *Manager) wrapStreamResult(
	execCtx context.Context,
	streamAuth *Auth,
	streamProvider string,
	routeModel string,
	headers http.Header,
	buffered []cliproxyexecutor.StreamChunk,
	remaining <-chan cliproxyexecutor.StreamChunk,
	aliasResult OAuthModelAliasResult,
) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	go func(streamCtx context.Context, streamChunks <-chan cliproxyexecutor.StreamChunk) {
		defer close(out)
		var failed bool
		forward := true
		var rewriter *StreamRewriter
		if aliasResult.ForceMapping && strings.TrimSpace(aliasResult.OriginalAlias) != "" {
			rewriter = NewStreamRewriter(StreamRewriteOptions{RewriteModel: aliasResult.OriginalAlias})
		}
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				rerr := errorForResult(chunk.Err)
				m.MarkResult(
					streamCtx,
					Result{
						AuthID:          streamAuth.ID,
						AuthGeneration:  streamAuth.Generation,
						RuntimeOnly:     streamAuth.Generation == 0,
						Provider:        streamProvider,
						Model:           routeModel,
						ModelQuotaEpoch: modelQuotaEpoch(streamAuth, routeModel),
						Success:         false,
						Error:           rerr,
					},
				)
			}
			if len(chunk.Payload) > 0 {
				payload := rewriteForceMappedStreamChunk(rewriter, chunk.Payload)
				if len(payload) == 0 {
					return true
				}
				chunk.Payload = payload
			}
			if !forward {
				return false
			}
			if streamCtx == nil {
				out <- chunk
				return true
			}
			select {
			case <-streamCtx.Done():
				forward = false
				return false
			case out <- chunk:
				return true
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok {
				discardStreamChunks(streamChunks)
				return
			}
			if chunk.Err != nil {
				discardStreamChunks(streamChunks)
				return
			}
		}
		for chunk := range streamChunks {
			if ok := emit(chunk); !ok {
				discardStreamChunks(streamChunks)
				return
			}
			if chunk.Err != nil {
				discardStreamChunks(streamChunks)
				return
			}
		}
		if tail := finishForceMappedStreamChunks(rewriter); len(tail) > 0 {
			if ok := emit(cliproxyexecutor.StreamChunk{Payload: tail}); !ok {
				return
			}
		}
		if !failed {
			m.MarkResult(
				streamCtx,
				Result{
					AuthID:          streamAuth.ID,
					AuthGeneration:  streamAuth.Generation,
					RuntimeOnly:     streamAuth.Generation == 0,
					Provider:        streamProvider,
					Model:           routeModel,
					ModelQuotaEpoch: modelQuotaEpoch(streamAuth, routeModel),
					Success:         true,
				},
			)
		}
	}(execCtx, remaining)
	return &cliproxyexecutor.StreamResult{
		Headers: cloneHTTPHeader(headers),
		Chunks:  out,
	}
}

func sanitizeDownstreamWebsocketFallbackRequest(
	ctx context.Context,
	auth *Auth,
	req cliproxyexecutor.Request,
) cliproxyexecutor.Request {
	if !cliproxyexecutor.DownstreamWebsocket(ctx) || authWebsocketsEnabled(auth) || len(req.Payload) == 0 {
		return req
	}
	updated, errDelete := sjson.DeleteBytes(req.Payload, "generate")
	if errDelete != nil {
		return req
	}
	req.Payload = updated
	return req
}

func ensureRequestedModelMetadata(opts cliproxyexecutor.Options, requestedModel string) cliproxyexecutor.Options {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return opts
	}
	if hasRequestedModelMetadata(opts.Metadata) {
		return opts
	}
	if len(opts.Metadata) == 0 {
		opts.Metadata = map[string]any{cliproxyexecutor.RequestedModelMetadataKey: requestedModel}
		return opts
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	maps.Copy(meta, opts.Metadata)
	meta[cliproxyexecutor.RequestedModelMetadataKey] = requestedModel
	opts.Metadata = meta
	return opts
}

func contextWithRequestedModelAlias(
	ctx context.Context,
	opts cliproxyexecutor.Options,
	fallback string,
) context.Context {
	alias := requestedModelAliasFromOptions(opts, fallback)
	ctx = coreusage.WithRequestedModelAlias(ctx, alias)
	if effort := reasoningEffortFromOptions(opts); effort != "" {
		ctx = coreusage.WithReasoningEffort(ctx, effort)
	}
	if serviceTier := serviceTierFromOptions(opts); serviceTier != "" {
		ctx = coreusage.WithServiceTier(ctx, serviceTier)
	}
	return ctx
}

func requestedModelAliasFromOptions(opts cliproxyexecutor.Options, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if len(opts.Metadata) == 0 {
		return fallback
	}
	raw, ok := opts.Metadata[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return fallback
	}
	switch value := raw.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return fallback
		}
		return strings.TrimSpace(value)
	case []byte:
		if len(value) == 0 {
			return fallback
		}
		return strings.TrimSpace(string(value))
	default:
		return fallback
	}
}

func reasoningEffortFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ReasoningEffortMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func serviceTierFromOptions(opts cliproxyexecutor.Options) string {
	if len(opts.Metadata) == 0 {
		return ""
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ServiceTierMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func withHomeAuthCount(opts cliproxyexecutor.Options, count int) cliproxyexecutor.Options {
	if count <= 0 {
		count = 1
	}
	meta := make(map[string]any, len(opts.Metadata)+1)
	maps.Copy(meta, opts.Metadata)
	meta[homeAuthCountMetadataKey] = count
	opts.Metadata = meta
	return opts
}

func homeAuthCountFromMetadata(meta map[string]any) int {
	if len(meta) == 0 {
		return 1
	}
	switch value := meta[homeAuthCountMetadataKey].(type) {
	case int:
		if value > 0 {
			return value
		}
	case int64:
		if value > 0 {
			return int(value)
		}
	case float64:
		if value > 0 {
			return int(value)
		}
	}
	return 1
}

func hasRequestedModelMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.RequestedModelMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v) != ""
	case []byte:
		return strings.TrimSpace(string(v)) != ""
	default:
		return false
	}
}

type requestAuthPrepareLock struct {
	mu   sync.Mutex
	refs int
}

type authMutationLock struct {
	token chan struct{}
	refs  int
}

func newAuthMutationLock() *authMutationLock {
	lock := &authMutationLock{token: make(chan struct{}, 1)}
	lock.token <- struct{}{}
	return lock
}

func (m *Manager) supersedeAuthDeletion(id string) bool {
	if m == nil {
		return false
	}
	id = strings.TrimSpace(id)
	m.mu.Lock()
	intent := m.pendingAuthDeletions[id]
	if intent != nil {
		intent.superseded.Store(true)
		delete(m.pendingAuthDeletions, id)
	}
	m.mu.Unlock()
	if intent != nil && intent.cancel != nil {
		intent.cancel()
	}
	return intent != nil
}

// ForgetRemovedRuntimeAuth releases tombstone state for an ephemeral runtime-only credential.
func (m *Manager) ForgetRemovedRuntimeAuth(id string) {
	if m == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	if _, pending := m.pendingPersistenceRollbacks.Load(id); pending {
		return
	}
	if _, pending := m.pendingAuthRecreations.Load(id); pending {
		return
	}
	m.mu.Lock()
	if m.auths[id] == nil && m.pendingAuthDeletions[id] == nil {
		delete(m.removedAuths, id)
		delete(m.removedAuthAt, id)
		m.authRemovalEpochs.Delete(id)
		m.persistedAuthVersions.Delete(id)
	}
	m.mu.Unlock()
}

func (m *Manager) clearAuthRemovalLocked(id string) {
	delete(m.removedAuths, id)
	delete(m.removedAuthAt, id)
	if intent := m.pendingAuthDeletions[id]; intent != nil {
		intent.superseded.Store(true)
		if intent.cancel != nil {
			intent.cancel()
		}
	}
	delete(m.pendingAuthDeletions, id)
}

func (m *Manager) assignAuthGenerationLocked(auth *Auth) {
	if m == nil || auth == nil {
		return
	}
	m.authGenerationSeq++
	if m.authGenerationSeq == 0 {
		m.authGenerationSeq++
	}
	auth.Generation = m.authGenerationSeq
}

func (m *Manager) pendingPersistenceRollbackLocked(id string) (<-chan struct{}, bool) {
	pending, ok := m.pendingPersistenceRollbacks.Load(strings.TrimSpace(id))
	if !ok {
		return nil, false
	}
	channel, ok := pending.(chan struct{})
	return channel, ok && channel != nil
}

func (m *Manager) pendingPersistenceRollback(id string) (<-chan struct{}, bool) {
	if m == nil {
		return nil, false
	}
	m.rollbackStateMu.Lock()
	defer m.rollbackStateMu.Unlock()
	return m.pendingPersistenceRollbackLocked(id)
}

func (m *Manager) lockPersistence(ctx context.Context) (func(), error) {
	if m == nil || m.persistenceLock == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return func() {}, ctx.Err()
	case <-m.persistenceLock.token:
	}
	unlock := func() { m.persistenceLock.token <- struct{}{} }
	if err := ctx.Err(); err != nil {
		unlock()
		return func() {}, err
	}
	return unlock, nil
}

func (m *Manager) lockAuthMutation(ctx context.Context, id string) (func(), error) {
	if m == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return func() {}, nil
	}
	m.authMutationLocksMu.Lock()
	lock := m.authMutationLocks[id]
	if lock == nil {
		lock = newAuthMutationLock()
		m.authMutationLocks[id] = lock
	}
	lock.refs++
	m.authMutationLocksMu.Unlock()
	releaseRef := func() {
		m.authMutationLocksMu.Lock()
		lock.refs--
		if lock.refs == 0 && m.authMutationLocks[id] == lock {
			delete(m.authMutationLocks, id)
		}
		m.authMutationLocksMu.Unlock()
	}
	select {
	case <-ctx.Done():
		releaseRef()
		return func() {}, ctx.Err()
	case <-lock.token:
	}
	var once sync.Once
	unlock := func() {
		once.Do(func() {
			lock.token <- struct{}{}
			releaseRef()
		})
	}
	if err := ctx.Err(); err != nil {
		unlock()
		return func() {}, err
	}
	if _, pending := m.pendingPersistenceRollback(id); pending {
		unlock()
		return func() {}, errors.Join(
			errAuthDurableStateUncertain,
			fmt.Errorf("credential %s has an unfinished persistence rollback", id),
		)
	}
	return unlock, nil
}

func (m *Manager) prepareRequestAuth(ctx context.Context, executor ProviderExecutor, auth *Auth) (*Auth, error) {
	if m == nil || executor == nil || auth == nil {
		return auth, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if auth.Generation > 0 {
		if evaluator, ok := executor.(RefreshEvaluator); ok && evaluator != nil && evaluator.ShouldRefresh(time.Now(), auth) {
			refreshed, errRefresh := m.refreshAuthForRequest(
				ctx,
				auth.ID,
				authAccessToken(auth),
				auth.Generation,
			)
			if errRefresh != nil {
				return auth, errRefresh
			}
			if refreshed != nil {
				auth = refreshed
			}
		}
	}
	preparer, ok := executor.(RequestAuthPreparer)
	if !ok || preparer == nil || !preparer.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}

	id := strings.TrimSpace(auth.ID)
	if id == "" {
		return preparer.PrepareRequestAuth(ctx, auth.Clone())
	}
	if auth.Generation == 0 {
		updated, errPrepare := preparer.PrepareRequestAuth(ctx, auth.Clone())
		if errPrepare != nil {
			return auth, errPrepare
		}
		if updated == nil {
			return auth, nil
		}
		if strings.TrimSpace(updated.ID) != id {
			return auth, fmt.Errorf("request auth preparation returned auth %q, want %q", updated.ID, id)
		}
		updated.Generation = 0
		return updated, nil
	}

	m.requestPrepareLocksMu.Lock()
	lock := m.requestPrepareLocks[id]
	if lock == nil {
		lock = &requestAuthPrepareLock{}
		m.requestPrepareLocks[id] = lock
	}
	lock.refs++
	m.requestPrepareLocksMu.Unlock()
	lock.mu.Lock()
	defer func() {
		lock.mu.Unlock()
		m.requestPrepareLocksMu.Lock()
		lock.refs--
		if lock.refs == 0 && m.requestPrepareLocks[id] == lock {
			delete(m.requestPrepareLocks, id)
		}
		m.requestPrepareLocksMu.Unlock()
	}()

	initial := auth.Clone()
	for {
		if errCtx := ctx.Err(); errCtx != nil {
			return initial, errCtx
		}

		target := initial.Clone()
		managed := false
		m.mu.RLock()
		current := m.auths[id]
		_, removed := m.removedAuths[id]
		if current != nil {
			target = current.Clone()
			managed = true
		}
		m.mu.RUnlock()
		if !managed && removed {
			return target, &Error{Code: "auth_removed", Message: "auth was removed during request preparation"}
		}
		if !preparer.ShouldPrepareRequestAuth(target) {
			return target, nil
		}

		expected := target.Clone()
		updated, errPrepare := preparer.PrepareRequestAuth(ctx, target.Clone())
		if errPrepare != nil {
			return target, errPrepare
		}
		if updated == nil {
			return target, nil
		}
		if strings.TrimSpace(updated.ID) != id {
			return target, fmt.Errorf("request auth preparation returned auth %q, want %q", updated.ID, id)
		}

		if !managed {
			saved, errUpdate := m.Update(ctx, updated)
			if errUpdate != nil {
				return updated, errUpdate
			}
			if saved != nil {
				return saved, nil
			}
			return target, &Error{Code: "auth_removed", Message: "auth was removed during request preparation"}
		}

		saved, errUpdate := m.MergePersistentUpdate(ctx, expected, updated)
		if errUpdate != nil {
			return updated, errUpdate
		}
		if saved != nil {
			return saved, nil
		}
	}
}

func pinnedAuthIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.PinnedAuthMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch val := raw.(type) {
	case string:
		return strings.TrimSpace(val)
	case []byte:
		return strings.TrimSpace(string(val))
	default:
		return ""
	}
}

func disallowFreeAuthFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[cliproxyexecutor.DisallowFreeAuthMetadataKey]
	if !ok || raw == nil {
		return false
	}
	switch val := raw.(type) {
	case bool:
		return val
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(val))
		return err == nil && parsed
	case []byte:
		parsed, err := strconv.ParseBool(strings.TrimSpace(string(val)))
		return err == nil && parsed
	default:
		return false
	}
}

func requireResponsesContinuationFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	value, _ := meta[cliproxyexecutor.RequireResponsesContinuationMetadataKey].(bool)
	return value
}

func authSupportsResponsesContinuation(auth *Auth) bool {
	if auth == nil {
		return false
	}
	return authWebsocketsEnabled(auth) || strings.EqualFold(strings.TrimSpace(auth.Provider), "codex")
}

func isFreeCodexAuth(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func selectionPredicate(disallowFreeAuth, requireContinuation bool) func(*Auth) bool {
	if !disallowFreeAuth && !requireContinuation {
		return nil
	}
	return func(auth *Auth) bool {
		return (!disallowFreeAuth || !isFreeCodexAuth(auth)) &&
			(!requireContinuation || authSupportsResponsesContinuation(auth))
	}
}

func ExtractSessionID(headers http.Header, body []byte, metadata map[string]any) string {
	if id := claudeCodeSessionIDFromPayload(body); id != "" {
		return id
	}
	if id := headerValueIgnoreCase(headers, "X-Session-ID"); id != "" {
		return id
	}
	if id := codexSessionIDFromPayload(body); id != "" {
		return id
	}
	if id := headerValueIgnoreCase(headers, "Session-Id"); id != "" {
		return id
	}
	if id := headerValueIgnoreCase(headers, "Session_id"); id != "" {
		return id
	}
	if id := headerValueIgnoreCase(headers, "Conversation_id"); id != "" {
		return id
	}
	if id := headerValueIgnoreCase(headers, "X-Amp-Thread-Id"); id != "" {
		return id
	}
	if id := headerValueIgnoreCase(headers, "X-Client-Request-Id"); id != "" {
		return id
	}
	return homeExecutionSessionIDFromMetadata(metadata)
}

func claudeCodeSessionIDFromPayload(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	userID := strings.TrimSpace(gjson.GetBytes(body, "metadata.user_id").String())
	if userID == "" {
		return ""
	}
	if idx := strings.LastIndex(userID, "_session_"); idx >= 0 {
		sessionID := strings.TrimSpace(userID[idx+len("_session_"):])
		if sessionID != "" {
			return "claude:" + sessionID
		}
	}
	if strings.HasPrefix(userID, "{") {
		if sessionID := strings.TrimSpace(gjson.Get(userID, "session_id").String()); sessionID != "" {
			return "claude:" + sessionID
		}
	}
	return ""
}

func codexSessionIDFromPayload(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if id := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); id != "" {
		return id
	}
	if rawTurnMetadata := strings.TrimSpace(
		gjson.GetBytes(
			body, "client_metadata.x-codex-turn-metadata",
		).String(),
	); rawTurnMetadata != "" {
		if id := strings.TrimSpace(gjson.Get(rawTurnMetadata, "prompt_cache_key").String()); id != "" {
			return id
		}
		if id := strings.TrimSpace(gjson.Get(rawTurnMetadata, "window_id").String()); id != "" {
			return id
		}
	}
	if id := strings.TrimSpace(gjson.GetBytes(body, "client_metadata.x-codex-window-id").String()); id != "" {
		return id
	}
	return ""
}

func publishSelectedAuthMetadata(meta map[string]any, auth *Auth) {
	if len(meta) == 0 || auth == nil {
		return
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return
	}
	meta[cliproxyexecutor.SelectedAuthMetadataKey] = authID
	if pinSelectedAuthFromMetadata(meta) && pinnedAuthIDFromMetadata(meta) == "" {
		meta[cliproxyexecutor.PinnedAuthMetadataKey] = authID
	}
	if callback, ok := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string)); ok && callback != nil {
		callback(authID)
	}
	if callback, ok := meta[cliproxyexecutor.SelectedAuthInfoCallbackMetadataKey].(func(cliproxyexecutor.SelectedAuthInfo)); ok && callback != nil {
		supportsWebsocket := authWebsocketsEnabled(auth)
		callback(cliproxyexecutor.SelectedAuthInfo{
			ID:                                    authID,
			SupportsIncrementalResponsesWebsocket: supportsWebsocket,
			SupportsResponsesContinuation: supportsWebsocket ||
				strings.EqualFold(strings.TrimSpace(auth.Provider), "codex"),
		})
	}
}

func pinSelectedAuthFromMetadata(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	value, _ := meta[cliproxyexecutor.PinSelectedAuthMetadataKey].(bool)
	return value
}

func homeExecutionSessionIDFromMetadata(meta map[string]any) string {
	if len(meta) == 0 {
		return ""
	}
	raw, ok := meta[cliproxyexecutor.ExecutionSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []byte:
		return strings.TrimSpace(string(value))
	default:
		return ""
	}
}

func requestedModelFromMetadata(metadata map[string]any, fallback string) string {
	if metadata != nil {
		if v, ok := metadata[cliproxyexecutor.RequestedModelMetadataKey]; ok {
			switch typed := v.(type) {
			case string:
				if trimmed := strings.TrimSpace(typed); trimmed != "" {
					return trimmed
				}
			case []byte:
				if trimmed := strings.TrimSpace(string(typed)); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		return "unknown"
	}
	return fallback
}

func rewriteModelForAuth(model string, auth *Auth) string {
	if auth == nil || model == "" {
		return model
	}
	prefix := strings.TrimSpace(auth.Prefix)
	if prefix == "" {
		return model
	}
	needle := prefix + "/"
	if !strings.HasPrefix(model, needle) {
		return model
	}
	return strings.TrimPrefix(model, needle)
}

func (m *Manager) applyAPIKeyModelAlias(auth *Auth, requestedModel string) string {
	if m == nil || auth == nil {
		return requestedModel
	}

	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return requestedModel
	}

	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return requestedModel
	}

	// Fast path: lookup per-auth mapping table (keyed by auth.ID).
	if resolved := m.lookupAPIKeyUpstreamModel(auth.ID, requestedModel); resolved != "" {
		return resolved
	}

	// Slow path: scan config for the matching credential entry and resolve alias.
	// This acts as a safety net if mappings are stale or auth.ID is missing.
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	upstreamModel := ""
	switch provider {
	case "gemini":
		upstreamModel = resolveUpstreamModelForGeminiAPIKey(cfg, auth, requestedModel)
	case "gemini-interactions":
		upstreamModel = resolveUpstreamModelForInteractionsAPIKey(cfg, auth, requestedModel)
	case "claude":
		upstreamModel = resolveUpstreamModelForClaudeAPIKey(cfg, auth, requestedModel)
	case "codex":
		upstreamModel = resolveUpstreamModelForCodexAPIKey(cfg, auth, requestedModel)
	case "vertex":
		upstreamModel = resolveUpstreamModelForVertexAPIKey(cfg, auth, requestedModel)
	default:
		upstreamModel = resolveUpstreamModelForOpenAICompatAPIKey(cfg, auth, requestedModel)
	}

	// Return upstream model if found, otherwise return requested model.
	if upstreamModel != "" {
		return upstreamModel
	}
	return requestedModel
}

// APIKeyConfigEntry is a generic interface for API key configurations.
type APIKeyConfigEntry interface {
	GetAPIKey() string
	GetBaseURL() string
}

func resolveAPIKeyConfig[T APIKeyConfigEntry](entries []T, auth *Auth) *T {
	if auth == nil || len(entries) == 0 {
		return nil
	}
	attrKey, attrBase := "", ""
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range entries {
		entry := &entries[i]
		cfgKey := strings.TrimSpace((*entry).GetAPIKey())
		cfgBase := strings.TrimSpace((*entry).GetBaseURL())
		if attrKey != "" && attrBase != "" {
			if strings.EqualFold(cfgKey, attrKey) && strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	if attrKey != "" {
		for i := range entries {
			entry := &entries[i]
			if strings.EqualFold(strings.TrimSpace((*entry).GetAPIKey()), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func resolveGeminiAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.GeminiKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.GeminiKey, auth)
}

func resolveInteractionsAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.GeminiKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.InteractionsKey, auth)
}

func resolveClaudeAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.ClaudeKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.ClaudeKey, auth)
}

func resolveCodexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.CodexKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.CodexKey, auth)
}

func resolveVertexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.VertexCompatKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.VertexCompatAPIKey, auth)
}

func resolveUpstreamModelForGeminiAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveGeminiAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForInteractionsAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveInteractionsAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForClaudeAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveClaudeAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForCodexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveCodexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForVertexAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveVertexAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForOpenAICompatAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	providerKey := ""
	compatName := ""
	if auth != nil && len(auth.Attributes) > 0 {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	authProvider := ""
	if auth != nil {
		authProvider = auth.Provider
	}
	if compatName == "" && !strings.EqualFold(strings.TrimSpace(authProvider), "openai-compatibility") {
		return ""
	}
	entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, authProvider)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

type apiKeyModelAliasTable map[string]map[string]string

func resolveOpenAICompatConfig(
	cfg *internalconfig.Config,
	providerKey, compatName, authProvider string,
) *internalconfig.OpenAICompatibility {
	if cfg == nil {
		return nil
	}
	candidates := make([]string, 0, 3)
	if v := strings.TrimSpace(compatName); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(providerKey); v != "" {
		candidates = append(candidates, v)
	}
	if v := strings.TrimSpace(authProvider); v != "" {
		candidates = append(candidates, v)
	}
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		for _, candidate := range candidates {
			if candidate != "" && strings.EqualFold(strings.TrimSpace(candidate), compat.Name) {
				return compat
			}
		}
	}
	return nil
}

func asModelAliasEntries[T interface {
	GetName() string
	GetAlias() string
}](models []T) []modelAliasEntry {
	if len(models) == 0 {
		return nil
	}
	out := make([]modelAliasEntry, 0, len(models))
	for i := range models {
		out = append(out, models[i])
	}
	return out
}

func (m *Manager) normalizeProviders(providers []string) []string {
	if len(providers) == 0 {
		return nil
	}
	result := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		result = append(result, p)
	}
	return result
}

func (m *Manager) retrySettings() (int, int, time.Duration) {
	if m == nil {
		return 0, 0, 0
	}
	return int(m.requestRetry.Load()), int(m.maxRetryCredentials.Load()), time.Duration(m.maxRetryInterval.Load())
}

func (m *Manager) closestCooldownWait(providers []string, model string, attempt int) (time.Duration, bool) {
	if m == nil || len(providers) == 0 {
		return 0, false
	}
	now := time.Now()
	defaultRetry := max(int(m.requestRetry.Load()), 0)
	providerSet := make(map[string]struct{}, len(providers))
	for i := range providers {
		key := strings.TrimSpace(strings.ToLower(providers[i]))
		if key == "" {
			continue
		}
		providerSet[key] = struct{}{}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var (
		found   bool
		minWait time.Duration
	)
	for _, auth := range m.auths {
		if auth == nil {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		effectiveRetry := defaultRetry
		if override, ok := auth.RequestRetryOverride(); ok {
			effectiveRetry = override
		}
		if effectiveRetry < 0 {
			effectiveRetry = 0
		}
		if attempt >= effectiveRetry {
			continue
		}
		blocked, reason, next := isAuthBlockedForModel(auth, model, now)
		if !blocked || next.IsZero() || reason == blockReasonDisabled {
			continue
		}
		wait := next.Sub(now)
		if wait < 0 {
			continue
		}
		if !found || wait < minWait {
			minWait = wait
			found = true
		}
	}
	return minWait, found
}

func (m *Manager) shouldRetryAfterError(
	err error,
	attempt int,
	providers []string,
	model string,
	maxWait time.Duration,
) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	if maxWait <= 0 {
		return 0, false
	}
	if status := statusCodeFromError(err); status == http.StatusOK {
		return 0, false
	}
	if isRequestInvalidError(err) {
		return 0, false
	}
	wait, found := m.closestCooldownWait(providers, model, attempt)
	if !found || wait > maxWait {
		return 0, false
	}
	return wait, true
}

const cooldownWaitJitterCap = 2 * time.Second

func jitteredCooldownWait(wait, maxWait time.Duration) time.Duration {
	if wait <= 0 {
		return wait
	}
	jitterRange := wait / 4
	if jitterRange > cooldownWaitJitterCap {
		jitterRange = cooldownWaitJitterCap
	}
	if maxWait > 0 && jitterRange > maxWait-wait {
		jitterRange = maxWait - wait
	}
	if jitterRange <= 0 {
		return wait
	}
	return wait + rand.N(jitterRange)
}

func waitForCooldown(ctx context.Context, wait, maxWait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(jitteredCooldownWait(wait, maxWait))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// MarkResult records an execution result and notifies hooks.
func (m *Manager) MarkResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}
	if result.RuntimeOnly {
		m.hook.OnResult(ctx, result)
		return
	}
	unlockMutation, errMutation := m.lockAuthMutation(ctx, result.AuthID)
	if errMutation != nil {
		log.WithError(errMutation).Warnf("skip auth result mutation for %s", result.AuthID)
		m.hook.OnResult(ctx, result)
		return
	}
	mutationLocked := true
	defer func() {
		if mutationLocked {
			unlockMutation()
		}
	}()

	shouldResumeModel := false
	shouldSuspendModel := false
	suspendReason := ""
	clearModelQuota := false
	setModelQuota := false
	var persistAuth *Auth

	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		if result.AuthGeneration != 0 && auth.Generation != result.AuthGeneration {
			m.mu.Unlock()
			unlockMutation()
			mutationLocked = false
			m.hook.OnResult(ctx, result)
			return
		}
		var resultState *ModelState
		if result.Model != "" {
			if result.ModelQuotaEpoch != modelQuotaEpoch(auth, result.Model) &&
				(result.Success || statusCodeFromResult(result.Error) == http.StatusTooManyRequests) {
				m.mu.Unlock()
				unlockMutation()
				mutationLocked = false
				m.hook.OnResult(ctx, result)
				return
			}
			resultState = ensureModelState(auth, result.Model)
		}
		now := time.Now()
		auth.recordRecentRequest(now, result.Success)
		if result.Success {
			auth.Success++
		} else {
			auth.Failed++
		}

		if result.Success {
			if result.Model != "" {
				state := resultState
				resetModelState(state, now)
				updateAggregatedAvailability(auth, now)
				if !hasModelError(auth, now) {
					auth.LastError = nil
					auth.StatusMessage = ""
					auth.Status = StatusActive
				}
				auth.UpdatedAt = now
				shouldResumeModel = true
				clearModelQuota = true
			} else {
				clearAuthStateOnSuccess(auth, now)
			}
		} else {
			if result.Model != "" {
				if !isRequestScopedNotFoundResultError(result.Error) {
					disableCooling := quotaCooldownDisabledForAuth(auth)
					state := resultState
					state.Unavailable = true
					state.Status = StatusError
					state.UpdatedAt = now
					if result.Error != nil {
						state.LastError = cloneError(result.Error)
						state.StatusMessage = result.Error.Message
						auth.LastError = cloneError(result.Error)
						auth.StatusMessage = result.Error.Message
					}

					statusCode := statusCodeFromResult(result.Error)
					if isCloudflareChallengeResultError(result.Error) {
						next, backoffLevel := nextCloudflareCooldown(state.Quota.BackoffLevel, disableCooling, now)
						state.NextRetryAfter = next
						state.StatusMessage = "cloudflare challenge"
						if auth.LastError != nil {
							auth.StatusMessage = "cloudflare challenge"
						}
						state.Quota = QuotaState{
							Exceeded:      true,
							Reason:        "cloudflare challenge",
							NextRecoverAt: next,
							BackoffLevel:  backoffLevel,
						}
					} else if isInvalidGrantResultError(result.Error) {
						state.StatusMessage = "invalid_grant"
						if disableCooling {
							state.NextRetryAfter = time.Time{}
						} else {
							state.NextRetryAfter = now.Add(30 * time.Minute)
							suspendReason = "invalid_grant"
							shouldSuspendModel = true
						}
					} else {
						switch statusCode {
						case 401:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								state.NextRetryAfter = now.Add(30 * time.Minute)
								suspendReason = "unauthorized"
								shouldSuspendModel = true
							}
						case 402, 403:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								state.NextRetryAfter = now.Add(30 * time.Minute)
								suspendReason = "payment_required"
								shouldSuspendModel = true
							}
						case 404:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								state.NextRetryAfter = now.Add(12 * time.Hour)
								suspendReason = "not_found"
								shouldSuspendModel = true
							}
						case 429:
							state.QuotaEpoch++
							var next time.Time
							backoffLevel := state.Quota.BackoffLevel
							if !disableCooling {
								if result.RetryAfter != nil {
									next = now.Add(*result.RetryAfter)
								} else {
									next, backoffLevel = quotaCooldownAfterFailure(state.Quota, now)
								}
							}
							state.NextRetryAfter = next
							state.Quota = QuotaState{
								Exceeded:      true,
								Reason:        "quota",
								NextRecoverAt: next,
								BackoffLevel:  backoffLevel,
							}
							if !disableCooling {
								suspendReason = "quota"
								shouldSuspendModel = true
								setModelQuota = true
							}
						case 408, 500, 502, 503, 504:
							if disableCooling {
								state.NextRetryAfter = time.Time{}
							} else {
								state.NextRetryAfter = nextTransientErrorRetryAfter(now)
							}
						default:
							state.NextRetryAfter = time.Time{}
						}
					}

					auth.Status = StatusError
					auth.UpdatedAt = now
					updateAggregatedAvailability(auth, now)
				}
			} else {
				applyAuthFailureState(auth, result.Error, result.RetryAfter, now, quotaCooldownDisabledForAuth(auth))
			}
		}

		persistAuth = auth.Clone()
	}
	m.mu.Unlock()
	if persistAuth != nil && m.scheduler != nil {
		m.scheduler.upsertAuth(persistAuth.Clone())
	}
	publishRegistryResult := false
	if persistAuth != nil {
		persisted, errPersist := m.persist(ctx, persistAuth, false, false, 0)
		if errPersist != nil {
			log.WithError(errPersist).Warnf("persist auth result for %s", result.AuthID)
		} else if errPublication := m.authPublicationContextError(ctx, result.AuthID, persisted); errPublication != nil {
			log.WithError(errPublication).Warnf("reject persisted auth result for %s", result.AuthID)
		} else {
			m.authIntentMu.Lock()
			m.mu.RLock()
			current := m.auths[result.AuthID]
			_, removed := m.removedAuths[result.AuthID]
			publishRegistryResult = current != nil && !removed && current.Generation == persistAuth.Generation
			m.mu.RUnlock()
			m.authIntentMu.Unlock()
			if removed && persisted {
				log.WithError(m.scheduleRejectedPersistedAuthDeletion(result.AuthID, persistAuth)).Warnf(
					"delete auth result persisted after removal for %s", result.AuthID,
				)
			}
		}
	}

	if publishRegistryResult && clearModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(result.AuthID, result.Model)
	}
	if publishRegistryResult && setModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().SetModelQuotaExceeded(result.AuthID, result.Model)
	}
	if publishRegistryResult && shouldResumeModel {
		registry.GetGlobalRegistry().ResumeClientModel(result.AuthID, result.Model)
	} else if publishRegistryResult && shouldSuspendModel {
		registry.GetGlobalRegistry().SuspendClientModel(result.AuthID, result.Model, suspendReason)
	}

	unlockMutation()
	mutationLocked = false
	m.hook.OnResult(ctx, result)
}

func findModelState(auth *Auth, model string) (*ModelState, string) {
	if auth == nil {
		return nil, ""
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, ""
	}
	if state := auth.ModelStates[model]; state != nil {
		return state, model
	}
	modelKey := canonicalModelKey(model)
	if modelKey != "" && modelKey != model {
		if state := auth.ModelStates[modelKey]; state != nil {
			return state, modelKey
		}
	}
	if modelKey == "" {
		modelKey = model
	}
	return nil, modelKey
}

func ensureModelState(auth *Auth, model string) *ModelState {
	if auth == nil {
		return nil
	}
	state, modelKey := findModelState(auth, model)
	if state != nil {
		return state
	}
	if modelKey == "" {
		return nil
	}
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState)
	}
	state = &ModelState{Status: StatusActive}
	auth.ModelStates[modelKey] = state
	return state
}

func modelQuotaEpoch(auth *Auth, model string) uint64 {
	state, _ := findModelState(auth, model)
	if state == nil {
		return 0
	}
	return state.QuotaEpoch
}

func resetModelState(state *ModelState, now time.Time) {
	if state == nil {
		return
	}
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	state.Quota = QuotaState{}
	state.UpdatedAt = now
}

func updateAggregatedAvailability(auth *Auth, now time.Time) {
	if auth == nil || len(auth.ModelStates) == 0 {
		return
	}
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		stateUnavailable := false
		if state.Status == StatusDisabled {
			stateUnavailable = true
		} else if state.Unavailable {
			if state.NextRetryAfter.IsZero() {
				stateUnavailable = false
			} else if state.NextRetryAfter.After(now) {
				stateUnavailable = true
				if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
					earliestRetry = state.NextRetryAfter
				}
			} else {
				state.Unavailable = false
				state.NextRetryAfter = time.Time{}
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			if quotaRecover.IsZero() ||
				(!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	auth.Unavailable = allUnavailable
	if allUnavailable {
		auth.NextRetryAfter = earliestRetry
	} else {
		auth.NextRetryAfter = time.Time{}
	}
	if quotaExceeded {
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		auth.Quota.NextRecoverAt = quotaRecover
		auth.Quota.BackoffLevel = maxBackoffLevel
	} else {
		auth.Quota.Exceeded = false
		auth.Quota.Reason = ""
		auth.Quota.NextRecoverAt = time.Time{}
		auth.Quota.BackoffLevel = 0
	}
}

func hasModelError(auth *Auth, now time.Time) bool {
	if auth == nil || len(auth.ModelStates) == 0 {
		return false
	}
	for _, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		if state.LastError != nil {
			return true
		}
		if state.Status == StatusError {
			if state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now)) {
				return true
			}
		}
	}
	return false
}

func updateAuthErrorFromModelStates(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	var latest *ModelState
	for _, state := range auth.ModelStates {
		if state == nil || (state.LastError == nil && state.Status != StatusError) {
			continue
		}
		if latest == nil || state.UpdatedAt.After(latest.UpdatedAt) {
			latest = state
		}
	}
	if latest == nil {
		clearAuthStateOnSuccess(auth, now)
		return
	}
	auth.Status = StatusError
	auth.StatusMessage = latest.StatusMessage
	auth.LastError = cloneError(latest.LastError)
	auth.UpdatedAt = now
}

func clearAuthStateOnSuccess(auth *Auth, now time.Time) {
	if auth == nil {
		return
	}
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.Quota.Exceeded = false
	auth.Quota.Reason = ""
	auth.Quota.NextRecoverAt = time.Time{}
	auth.Quota.BackoffLevel = 0
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.UpdatedAt = now
}

func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code:       err.Code,
		Message:    err.Message,
		Retryable:  err.Retryable,
		HTTPStatus: err.HTTPStatus,
	}
}

func statusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var sc statusCoder
	if errors.As(err, &sc) && sc != nil {
		return sc.StatusCode()
	}
	return 0
}

func errorForResult(err error) *Error {
	if err == nil {
		return nil
	}
	rerr := &Error{Message: err.Error()}
	if se, ok := errors.AsType[cliproxyexecutor.StatusError](err); ok && se != nil {
		rerr.HTTPStatus = se.StatusCode()
	}
	return rerr
}

func isUnauthorizedError(err error) bool {
	if err == nil {
		return false
	}
	if statusCodeFromError(err) == http.StatusUnauthorized {
		return true
	}
	raw := strings.ToLower(err.Error())
	return strings.Contains(raw, "status 401") || strings.Contains(raw, "401 unauthorized")
}

func hasUnauthorizedAuthFailure(auth *Auth) bool {
	if auth == nil || auth.LastError == nil {
		return false
	}
	return auth.LastError.StatusCode() == http.StatusUnauthorized || strings.EqualFold(
		auth.LastError.Code, "unauthorized",
	)
}

func refreshErrorFromError(err error) *Error {
	if err == nil {
		return nil
	}
	statusCode := statusCodeFromError(err)
	if statusCode == 0 && isUnauthorizedError(err) {
		statusCode = http.StatusUnauthorized
	}
	authErr := &Error{Message: err.Error(), HTTPStatus: statusCode}
	if statusCode == http.StatusUnauthorized {
		authErr.Code = "unauthorized"
		authErr.Retryable = false
	}
	return authErr
}

func retryAfterFromError(err error) *time.Duration {
	if err == nil {
		return nil
	}
	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	rap, ok := err.(retryAfterProvider)
	if !ok || rap == nil {
		return nil
	}
	retryAfter := rap.RetryAfter()
	if retryAfter == nil {
		return nil
	}
	return new(*retryAfter)
}

func statusCodeFromResult(err *Error) int {
	if err == nil {
		return 0
	}
	return err.StatusCode()
}

// isRequestInvalidError returns true if the error represents a client request
// error that should not be retried. Specifically, it checks for 400 Bad Request
// with "invalid_request_error" in the message, indicating the request itself is
// malformed and switching to a different auth will not help.
func isRequestScopedNotFoundMessage(message string) bool {
	if message == "" {
		return false
	}
	lower := strings.ToLower(message)
	return strings.Contains(lower, "item with id") &&
		strings.Contains(lower, "not found") &&
		strings.Contains(lower, "items are not persisted when `store` is set to false")
}

func isRequestScopedNotFoundResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusNotFound {
		return false
	}
	return isRequestScopedNotFoundMessage(err.Message)
}

func isInvalidGrantErrorMessage(message string) bool {
	return strings.Contains(strings.ToLower(message), "invalid_grant")
}

func isInvalidGrantError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest && status != http.StatusUnauthorized {
		return false
	}
	return isInvalidGrantErrorMessage(err.Error())
}

func isInvalidGrantResultError(err *Error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromResult(err)
	if status != http.StatusBadRequest && status != http.StatusUnauthorized {
		return false
	}
	return isInvalidGrantErrorMessage(err.Code) || isInvalidGrantErrorMessage(err.Message)
}

func isCloudflareChallengeErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(lower, "challenge-platform") ||
		strings.Contains(lower, "cf-mitigated") ||
		strings.Contains(lower, "cloudflare challenge") ||
		(strings.Contains(lower, "cloudflare") && strings.Contains(lower, "<html"))
}

func isCloudflareChallengeError(err error) bool {
	if err == nil {
		return false
	}
	return isCloudflareChallengeErrorMessage(err.Error())
}

func isCloudflareChallengeResultError(err *Error) bool {
	if err == nil {
		return false
	}
	return isCloudflareChallengeErrorMessage(err.Message)
}

func nextCloudflareCooldown(backoffLevel int, disableCooling bool, now time.Time) (time.Time, int) {
	var next time.Time
	if !disableCooling {
		cooldown, nextLevel := nextQuotaCooldown(backoffLevel, disableCooling)
		if cooldown < 10*time.Second {
			cooldown = 10 * time.Second
		}
		if cooldown > 0 {
			next = now.Add(cooldown)
		}
		backoffLevel = nextLevel
	}
	return next, backoffLevel
}

func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	if isCloudflareChallengeError(err) {
		return false
	}
	if isInvalidGrantError(err) {
		return false
	}
	status := statusCodeFromError(err)
	switch status {
	case http.StatusBadRequest:
		return strings.Contains(err.Error(), "invalid_request_error")
	case http.StatusNotFound:
		return isRequestScopedNotFoundMessage(err.Error())
	default:
		return false
	}
}

func applyAuthFailureState(
	auth *Auth,
	resultErr *Error,
	retryAfter *time.Duration,
	now time.Time,
	disableCoolingOverride ...bool,
) {
	if auth == nil {
		return
	}
	disableCooling := quotaCooldownDisabledForAuth(auth)
	if len(disableCoolingOverride) > 0 {
		disableCooling = disableCoolingOverride[0]
	}
	if isRequestScopedNotFoundResultError(resultErr) {
		return
	}
	auth.Unavailable = true
	auth.Status = StatusError
	auth.UpdatedAt = now
	if resultErr != nil {
		auth.LastError = cloneError(resultErr)
		if resultErr.Message != "" {
			auth.StatusMessage = resultErr.Message
		}
	}
	statusCode := statusCodeFromResult(resultErr)
	if isCloudflareChallengeResultError(resultErr) {
		auth.StatusMessage = "cloudflare challenge"
		next, backoffLevel := nextCloudflareCooldown(auth.Quota.BackoffLevel, disableCooling, now)
		auth.Quota = QuotaState{
			Exceeded:      true,
			Reason:        "cloudflare challenge",
			NextRecoverAt: next,
			BackoffLevel:  backoffLevel,
		}
		auth.NextRetryAfter = next
		return
	}
	if isInvalidGrantResultError(resultErr) {
		auth.StatusMessage = "invalid_grant"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
		return
	}
	switch statusCode {
	case 401:
		auth.StatusMessage = "unauthorized"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 402, 403:
		auth.StatusMessage = "payment_required"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(30 * time.Minute)
		}
	case 404:
		auth.StatusMessage = "not_found"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(12 * time.Hour)
		}
	case 429:
		auth.StatusMessage = "quota exhausted"
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		var next time.Time
		if !disableCooling {
			if retryAfter != nil {
				next = now.Add(*retryAfter)
			} else {
				next, auth.Quota.BackoffLevel = quotaCooldownAfterFailure(auth.Quota, now)
			}
		}
		auth.Quota.NextRecoverAt = next
		auth.NextRetryAfter = next
	case 408, 500, 502, 503, 504:
		auth.StatusMessage = "transient upstream error"
		if disableCooling {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = nextTransientErrorRetryAfter(now)
		}
	default:
		if auth.StatusMessage == "" {
			auth.StatusMessage = "request failed"
		}
	}
}

// quotaCooldownAfterFailure returns the recovery deadline and backoff level for
// a quota failure observed at now. Failures that land while a previous quota
// window is still open reuse that window instead of escalating.
func quotaCooldownAfterFailure(quota QuotaState, now time.Time) (time.Time, int) {
	if quota.NextRecoverAt.After(now) {
		return quota.NextRecoverAt, quota.BackoffLevel
	}
	cooldown, nextLevel := nextQuotaCooldown(quota.BackoffLevel, false)
	var next time.Time
	if cooldown > 0 {
		next = now.Add(cooldown)
	}
	return next, nextLevel
}

// nextQuotaCooldown returns the next cooldown duration and updated backoff level for repeated quota errors.
func nextQuotaCooldown(prevLevel int, disableCooling bool) (time.Duration, int) {
	if prevLevel < 0 {
		prevLevel = 0
	}
	if disableCooling {
		return 0, prevLevel
	}
	cooldown := max(quotaBackoffBase*time.Duration(1<<prevLevel), quotaBackoffBase)
	if cooldown >= quotaBackoffMax {
		return quotaBackoffMax, prevLevel
	}
	return cooldown, prevLevel + 1
}

// List returns all auth entries currently known by the manager.
func (m *Manager) List() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		list = append(list, auth.Clone())
	}
	return list
}

// GetByID retrieves an auth entry by its ID.

func (m *Manager) GetByID(id string) (*Auth, bool) {
	if id == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auth, ok := m.auths[id]
	if !ok {
		return nil, false
	}
	return auth.Clone(), true
}

// GetExecutionSessionAuthByID retrieves a Home runtime auth scoped to an execution session.
func (m *Manager) GetExecutionSessionAuthByID(sessionID string, authID string) (*Auth, bool) {
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if m == nil || sessionID == "" || authID == "" {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	sessionAuths := m.homeRuntimeAuths[sessionID]
	auth := sessionAuths[authID]
	if auth == nil {
		return nil, false
	}
	return auth.Clone(), true
}

// Executor returns the registered provider executor for a provider key.
func (m *Manager) Executor(provider string) (ProviderExecutor, bool) {
	if m == nil {
		return nil, false
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return nil, false
	}

	m.mu.RLock()
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		lowerProvider := strings.ToLower(provider)
		if lowerProvider != provider {
			executor, okExecutor = m.executors[lowerProvider]
		}
	}
	m.mu.RUnlock()

	if !okExecutor || executor == nil {
		return nil, false
	}
	return executor, true
}

// CloseExecutionSession asks all registered executors to release the supplied execution session.
func (m *Manager) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}

	m.mu.Lock()
	if sessionID == CloseAllExecutionSessionsID {
		m.clearHomeRuntimeAuthsLocked()
	} else {
		m.clearHomeRuntimeAuthsForSessionLocked(sessionID)
	}
	executors := make([]ProviderExecutor, 0, len(m.executors))
	for _, exec := range m.executors {
		executors = append(executors, exec)
	}
	m.mu.Unlock()

	for i := range executors {
		if closer, ok := executors[i].(ExecutionSessionCloser); ok && closer != nil {
			closer.CloseExecutionSession(sessionID)
		}
	}
}

func (m *Manager) currentSelectableAuth(
	authID,
	model string,
	providers map[string]struct{},
	requestPredicate func(*Auth) bool,
	tried map[string]struct{},
	allowModelBlock ...bool,
) (*Auth, string) {
	if m == nil || strings.TrimSpace(authID) == "" {
		return nil, ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	current := m.auths[authID]
	if current == nil || current.Disabled {
		return nil, ""
	}
	providerKey := strings.ToLower(strings.TrimSpace(current.Provider))
	if len(providers) > 0 {
		if _, allowed := providers[providerKey]; !allowed {
			return nil, ""
		}
	}
	if requestPredicate != nil && !requestPredicate(current) {
		return nil, ""
	}
	if _, used := tried[current.ID]; used {
		return nil, ""
	}
	allowBlocked := len(allowModelBlock) > 0 && allowModelBlock[0]
	if blocked, _, _ := isAuthBlockedForModel(current, model, time.Now()); blocked && !allowBlocked {
		return nil, ""
	}
	modelKey := canonicalModelKey(model)
	registryRef := registry.GetGlobalRegistry()
	if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(current.ID, modelKey) {
		return nil, ""
	}
	return current.Clone(), providerKey
}

// PickNext selects the next auth entry for a single-provider request.
func (m *Manager) PickNext(
	ctx context.Context,
	provider, model string,
	opts cliproxyexecutor.Options,
	tried map[string]struct{},
) (*Auth, ProviderExecutor, error) {
	if m.HomeEnabled() {
		auth, exec, _, err := m.pickNextViaHome(ctx, model, opts, tried)
		return auth, exec, err
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	requestPredicate := selectionPredicate(
		disallowFreeAuthFromMetadata(opts.Metadata),
		requireResponsesContinuationFromMetadata(opts.Metadata),
	)

	m.mu.RLock()
	selector := m.selector
	pluginScheduler := m.pluginScheduler
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		m.mu.RUnlock()
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate.Provider != provider || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if requestPredicate != nil && !requestPredicate(candidate) {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(candidate.ID, modelKey) {
			continue
		}
		candidates = append(candidates, candidate.Clone())
	}
	m.mu.RUnlock()
	if len(candidates) == 0 {
		return nil, nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if selected, handled, errPick := m.pickViaPluginScheduler(
		ctx, pluginScheduler, provider, []string{provider}, model, opts, candidates,
	); errPick != nil {
		return nil, nil, errPick
	} else if handled {
		allowedProviders := map[string]struct{}{strings.ToLower(strings.TrimSpace(provider)): {}}
		current, _ := m.currentSelectableAuth(
			selected.ID, model, allowedProviders, requestPredicate, tried,
		)
		if current == nil {
			return nil, nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
		}
		return current, executor, nil
	}
	if m.scheduler != nil && isBuiltInSelector(selector) {
		var stickySelector *SessionAffinitySelector
		if selector, ok := selector.(*SessionAffinitySelector); ok {
			stickySelector = selector
		}
		allowedProviders := map[string]struct{}{strings.ToLower(strings.TrimSpace(provider)): {}}
		if stickySelector != nil {
			if sticky := stickySelector.pickStickyForModel(provider, model, opts, candidates); sticky != nil {
				if current, _ := m.currentSelectableAuth(
					sticky.ID, model, allowedProviders, requestPredicate, tried,
				); current != nil {
					executor, ok := m.Executor(provider)
					if !ok || executor == nil {
						return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
					}
					return current, executor, nil
				}
				stickySelector.InvalidateAuth(sticky.ID)
			}
		}
		schedulerTried := maps.Clone(tried)
		if schedulerTried == nil {
			schedulerTried = make(map[string]struct{})
		}
		for attempt := 0; attempt < len(candidates); attempt++ {
			selected, errPick := m.scheduler.pickSingleWithPredicate(
				ctx, provider, model, opts, schedulerTried, requestPredicate,
			)
			if errPick != nil {
				return nil, nil, errPick
			}
			current, _ := m.currentSelectableAuth(
				selected.ID, model, allowedProviders, requestPredicate, tried,
			)
			if current == nil {
				schedulerTried[selected.ID] = struct{}{}
				continue
			}
			if stickySelector != nil {
				stickySelector.Remember(provider, opts, current)
			}
			executor, ok := m.Executor(provider)
			if !ok || executor == nil {
				return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
			}
			return current, executor, nil
		}
		return nil, nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}
	selected, errPick := selector.Pick(ctx, provider, model, opts, candidates)
	if errPick != nil {
		return nil, nil, errPick
	}
	if selected == nil {
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	allowedProviders := map[string]struct{}{strings.ToLower(strings.TrimSpace(provider)): {}}
	current, providerKey := m.currentSelectableAuth(
		selected.ID,
		model,
		allowedProviders,
		requestPredicate,
		tried,
		pinnedAuthID != "" && selected.ID == pinnedAuthID,
	)
	if current == nil {
		return nil, nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}
	executor, okExecutor = m.Executor(providerKey)
	if !okExecutor || executor == nil {
		return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	return current, executor, nil
}
func (m *Manager) pickNextMixed(
	ctx context.Context,
	providers []string,
	model string,
	opts cliproxyexecutor.Options,
	tried map[string]struct{},
) (*Auth, ProviderExecutor, string, error) {
	if m.HomeEnabled() {
		return m.pickNextViaHome(ctx, model, opts, tried)
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	requestPredicate := selectionPredicate(
		disallowFreeAuthFromMetadata(opts.Metadata),
		requireResponsesContinuationFromMetadata(opts.Metadata),
	)

	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		p := strings.TrimSpace(strings.ToLower(provider))
		if p == "" {
			continue
		}
		providerSet[p] = struct{}{}
	}
	if len(providerSet) == 0 {
		return nil, nil, "", &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}

	m.mu.RLock()
	selector := m.selector
	pluginScheduler := m.pluginScheduler
	candidates := make([]*Auth, 0, len(m.auths))
	modelKey := strings.TrimSpace(model)
	if modelKey != "" {
		parsed := thinking.ParseSuffix(modelKey)
		if parsed.ModelName != "" {
			modelKey = strings.TrimSpace(parsed.ModelName)
		}
	}
	registryRef := registry.GetGlobalRegistry()
	for _, candidate := range m.auths {
		if candidate == nil || candidate.Disabled {
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			continue
		}
		if requestPredicate != nil && !requestPredicate(candidate) {
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(candidate.Provider))
		if providerKey == "" {
			continue
		}
		if _, ok := providerSet[providerKey]; !ok {
			continue
		}
		if _, used := tried[candidate.ID]; used {
			continue
		}
		if _, ok := m.executors[providerKey]; !ok {
			continue
		}
		if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(candidate.ID, modelKey) {
			continue
		}
		candidates = append(candidates, candidate.Clone())
	}
	m.mu.RUnlock()
	if len(candidates) == 0 {
		if pinnedAuthID != "" {
			return nil, nil, "", pinnedAuthNotFoundError(pinnedAuthID)
		}
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if selected, handled, errPick := m.pickViaPluginScheduler(
		ctx, pluginScheduler, "mixed", providers, model, opts, candidates,
	); errPick != nil {
		return nil, nil, "", errPick
	} else if handled {
		current, providerKey := m.currentSelectableAuth(
			selected.ID, model, providerSet, requestPredicate, tried,
		)
		if current == nil {
			return nil, nil, "", &Error{Code: "auth_unavailable", Message: "no auth available"}
		}
		executor, okExecutor := m.Executor(providerKey)
		if !okExecutor {
			return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		return current, executor, providerKey, nil
	}
	if m.scheduler != nil && isBuiltInSelector(selector) {
		var stickySelector *SessionAffinitySelector
		if selector, ok := selector.(*SessionAffinitySelector); ok {
			stickySelector = selector
		}
		if stickySelector != nil {
			if sticky := stickySelector.pickStickyForModel("mixed", model, opts, candidates); sticky != nil {
				if current, providerKey := m.currentSelectableAuth(
					sticky.ID, model, providerSet, requestPredicate, tried,
				); current != nil {
					executor, ok := m.Executor(providerKey)
					if !ok || executor == nil {
						return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
					}
					return current, executor, providerKey, nil
				}
				stickySelector.InvalidateAuth(sticky.ID)
			}
		}
		filteredProviders := make([]string, 0, len(providers))
		for _, provider := range providers {
			if executor, ok := m.Executor(strings.TrimSpace(strings.ToLower(provider))); ok && executor != nil {
				filteredProviders = append(filteredProviders, provider)
			}
		}
		schedulerTried := maps.Clone(tried)
		if schedulerTried == nil {
			schedulerTried = make(map[string]struct{})
		}
		for attempt := 0; attempt < len(candidates); attempt++ {
			selected, _, errPick := m.scheduler.pickMixedWithPredicate(
				ctx, filteredProviders, model, opts, schedulerTried, requestPredicate,
			)
			if errPick != nil {
				return nil, nil, "", errPick
			}
			current, providerKey := m.currentSelectableAuth(
				selected.ID, model, providerSet, requestPredicate, tried,
			)
			if current == nil {
				schedulerTried[selected.ID] = struct{}{}
				continue
			}
			if stickySelector != nil {
				stickySelector.Remember("mixed", opts, current)
			}
			executor, ok := m.Executor(providerKey)
			if !ok || executor == nil {
				return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
			}
			return current, executor, providerKey, nil
		}
		return nil, nil, "", &Error{Code: "auth_unavailable", Message: "no auth available"}
	}
	selected, errPick := selector.Pick(ctx, "mixed", model, opts, candidates)
	if errPick != nil {
		return nil, nil, "", errPick
	}
	if selected == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	current, providerKey := m.currentSelectableAuth(
		selected.ID,
		model,
		providerSet,
		requestPredicate,
		tried,
		pinnedAuthID != "" && selected.ID == pinnedAuthID,
	)
	if current == nil {
		return nil, nil, "", &Error{Code: "auth_unavailable", Message: "no auth available"}
	}
	executor, okExecutor := m.Executor(providerKey)
	if !okExecutor || executor == nil {
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	return current, executor, providerKey, nil
}

type homeErrorEnvelope struct {
	Error *homeErrorDetail `json:"error"`
}

type homeErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

const (
	homeUpstreamModelAttributeKey     = "home_upstream_model"
	homeRequestRetryExceededErrorCode = "request_retry_exceeded"
)

func isHomeRequestRetryExceededError(err error) bool {
	var authErr *Error
	if !errors.As(err, &authErr) || authErr == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(authErr.Code), homeRequestRetryExceededErrorCode)
}

func shouldReturnLastErrorOnPickFailure(homeMode bool, lastErr error, errPick error) bool {
	if lastErr == nil {
		return false
	}
	if !homeMode {
		return true
	}
	return isHomeRequestRetryExceededError(errPick)
}

func homeAuthAlreadyTried(tried map[string]struct{}, authID string) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" || len(tried) == 0 {
		return false
	}
	_, ok := tried[authID]
	return ok
}

func repeatedHomeAuthError() *Error {
	return &Error{
		Code:       homeRequestRetryExceededErrorCode,
		Message:    "home returned a previously tried auth",
		HTTPStatus: http.StatusServiceUnavailable,
	}
}

func pinnedAuthNotFoundError(authID string) *Error {
	return &Error{
		Code:       "pinned_auth_not_found",
		Message:    fmt.Sprintf("pinned auth %q is unavailable", strings.TrimSpace(authID)),
		HTTPStatus: http.StatusConflict,
	}
}

type homeAuthDispatchResponse struct {
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	AuthIndex  string `json:"auth_index"`
	UserAPIKey string `json:"user_api_key"`
	Auth       Auth   `json:"auth"`
}

type homeAuthDispatcher interface {
	HeartbeatOK() bool
	RPopAuth(ctx context.Context, requestedModel string, sessionID string, headers http.Header, count int) (
		[]byte,
		error,
	)
}

type HomeRefreshClient interface {
	HeartbeatOK() bool
	GetRefreshAuth(ctx context.Context, authIndex string) ([]byte, error)
}

type homeRuntimeRefreshClient = HomeRefreshClient

type HomeClient interface {
	HeartbeatOK() bool
	RPopAuth(ctx context.Context, requestedModel string, sessionID string, headers http.Header, count int) (
		[]byte,
		error,
	)
	GetRefreshAuth(ctx context.Context, authIndex string) ([]byte, error)
}

type homeRefreshClientContextKey struct{}

type homeRefreshClientContextValue struct {
	client HomeRefreshClient
}

func WithHomeRefreshClient(ctx context.Context, client HomeRefreshClient) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(
		ctx,
		homeRefreshClientContextKey{},
		homeRefreshClientContextValue{client: client},
	)
}

func HomeRefreshClientFromContext(ctx context.Context) (HomeRefreshClient, bool) {
	if ctx == nil {
		return nil, false
	}
	value, ok := ctx.Value(homeRefreshClientContextKey{}).(homeRefreshClientContextValue)
	if !ok {
		return nil, false
	}
	return value.client, true
}

var currentHomeDispatcher = func() homeAuthDispatcher {
	return home.Current()
}

var currentHomeRuntimeRefreshClient = func() HomeRefreshClient {
	return home.Current()
}

func setHomeUserAPIKeyOnGinContext(ctx context.Context, apiKey string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || ctx == nil {
		return
	}
	ginCtx, ok := util.GinContextValue(ctx).(interface{ Set(string, any) })
	if !ok || ginCtx == nil {
		return
	}
	ginCtx.Set("apiKey", apiKey)
}

func homeDispatchHeaders(ctx context.Context, headers http.Header) http.Header {
	apiKey, ok := homeQueryCredentialFromContext(ctx)
	if !ok {
		return headers
	}
	out := headers.Clone()
	if out == nil {
		out = http.Header{}
	}
	if out.Get("Authorization") != "" || out.Get("X-Goog-Api-Key") != "" || out.Get("X-Api-Key") != "" {
		return out
	}
	out.Set("X-Goog-Api-Key", apiKey)
	return out
}

func homeQueryCredentialFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	if queryCtx, ok := util.GinContextValue(ctx).(interface{ Query(string) string }); ok && queryCtx != nil {
		if apiKey := strings.TrimSpace(queryCtx.Query("key")); apiKey != "" {
			return apiKey, true
		}
		if apiKey := strings.TrimSpace(queryCtx.Query("auth_token")); apiKey != "" {
			return apiKey, true
		}
	}
	ginCtx, ok := util.GinContextValue(ctx).(interface{ Get(string) (any, bool) })
	if !ok || ginCtx == nil {
		return "", false
	}
	rawMetadata, ok := ginCtx.Get("accessMetadata")
	if !ok {
		return "", false
	}
	source := accessMetadataSource(rawMetadata)
	if source != "query-key" && source != "query-auth-token" {
		return "", false
	}
	rawAPIKey, ok := ginCtx.Get("apiKey")
	if !ok {
		return "", false
	}
	apiKey := contextStringValue(rawAPIKey)
	if apiKey == "" {
		return "", false
	}
	return apiKey, true
}

func accessMetadataSource(raw any) string {
	switch v := raw.(type) {
	case map[string]string:
		return strings.TrimSpace(v["source"])
	case map[string]any:
		return contextStringValue(v["source"])
	default:
		return ""
	}
}

func contextStringValue(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func (m *Manager) clearHomeRuntimeAuths() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.clearHomeRuntimeAuthsLocked()
	m.mu.Unlock()
}

func (m *Manager) clearHomeRuntimeAuthsLocked() {
	m.homeRuntimeAuths = make(map[string]map[string]*Auth)
}

func (m *Manager) clearHomeRuntimeAuthsForSessionLocked(sessionID string) {
	if m.homeRuntimeAuths == nil {
		return
	}
	delete(m.homeRuntimeAuths, strings.TrimSpace(sessionID))
}

func homeRuntimeExecutorKey(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if executorKey := strings.ToLower(strings.TrimSpace(auth.executorKey)); executorKey != "" {
		return executorKey
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

func (m *Manager) clearHomeRuntimeAuthsForProviderLocked(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" || m.homeRuntimeAuths == nil {
		return
	}
	for sessionID, sessionAuths := range m.homeRuntimeAuths {
		for authID, auth := range sessionAuths {
			if homeRuntimeExecutorKey(auth) == provider {
				delete(sessionAuths, authID)
			}
		}
		if len(sessionAuths) == 0 {
			delete(m.homeRuntimeAuths, sessionID)
		}
	}
}

func (m *Manager) providerExecutorCurrent(provider string, expected ProviderExecutor) bool {
	if m == nil || expected == nil {
		return false
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return false
	}
	m.mu.RLock()
	current := m.executors[provider]
	m.mu.RUnlock()
	return sameProviderExecutor(current, expected)
}

func (m *Manager) rememberHomeRuntimeAuth(sessionID string, auth *Auth) {
	sessionID = strings.TrimSpace(sessionID)
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	if m == nil || auth == nil || sessionID == "" || authID == "" {
		return
	}
	m.mu.Lock()
	if _, removed := m.removedAuths[authID]; removed {
		m.mu.Unlock()
		return
	}
	if m.homeRuntimeAuths == nil {
		m.homeRuntimeAuths = make(map[string]map[string]*Auth)
	}
	sessionAuths := m.homeRuntimeAuths[sessionID]
	if sessionAuths == nil {
		sessionAuths = make(map[string]*Auth)
		m.homeRuntimeAuths[sessionID] = sessionAuths
	}
	sessionAuths[authID] = auth.Clone()
	m.mu.Unlock()
}

func (m *Manager) compareAndSwapHomeRuntimeAuth(
	sessionID string,
	expected,
	refreshed *Auth,
	expectedExecutor ProviderExecutor,
) bool {
	sessionID = strings.TrimSpace(sessionID)
	authID := ""
	if expected != nil {
		authID = strings.TrimSpace(expected.ID)
	}
	if m == nil || sessionID == "" || authID == "" || refreshed == nil ||
		expectedExecutor == nil || strings.TrimSpace(refreshed.ID) != authID {
		return false
	}
	refreshCommit := refreshed.RefreshCommit
	refreshed.RefreshCommit = nil
	m.executorMu.RLock()
	m.mu.Lock()
	executorKey := homeRuntimeExecutorKey(expected)
	if executorKey == "" || !sameProviderExecutor(m.executors[executorKey], expectedExecutor) {
		m.mu.Unlock()
		m.executorMu.RUnlock()
		return false
	}
	if _, removed := m.removedAuths[authID]; removed {
		m.mu.Unlock()
		m.executorMu.RUnlock()
		return false
	}
	sessionAuths := m.homeRuntimeAuths[sessionID]
	if sessionAuths == nil {
		m.mu.Unlock()
		m.executorMu.RUnlock()
		return false
	}
	current := sessionAuths[authID]
	if current == nil || current.Generation != 0 ||
		homeRuntimeExecutorKey(current) != executorKey ||
		strings.TrimSpace(current.Index) != strings.TrimSpace(expected.Index) ||
		!runtimeAuthCredentialsEqual(current, expected) {
		m.mu.Unlock()
		m.executorMu.RUnlock()
		return false
	}
	next := refreshed.Clone()
	if !next.indexAssigned && strings.TrimSpace(next.Index) == "" {
		next.Index = current.Index
		next.indexAssigned = current.indexAssigned
	}
	if next.Storage == nil {
		next.Storage = current.Storage
	}
	if next.Runtime == nil {
		next.Runtime = current.Runtime
	}
	next.executorKey = executorKey
	next.Generation = 0
	commit := func() bool {
		sessionAuths[authID] = next
		return true
	}
	committed := false
	if refreshCommit != nil {
		committed = refreshCommit.CommitRefresh(commit)
	} else {
		committed = commit()
	}
	if !committed {
		sessionAuths[authID] = current
	}
	m.mu.Unlock()
	m.executorMu.RUnlock()
	return committed
}

func (m *Manager) homeRuntimeAuthByID(sessionID string, authID string) (*Auth, ProviderExecutor, string, bool) {
	sessionID = strings.TrimSpace(sessionID)
	authID = strings.TrimSpace(authID)
	if m == nil || sessionID == "" || authID == "" {
		return nil, nil, "", false
	}
	m.mu.RLock()
	sessionAuths := m.homeRuntimeAuths[sessionID]
	auth := sessionAuths[authID]
	m.mu.RUnlock()
	if auth == nil {
		return nil, nil, "", false
	}
	providerKey := homeRuntimeExecutorKey(auth)
	if providerKey == "" {
		return nil, nil, "", false
	}
	executor, ok := m.Executor(providerKey)
	if !ok {
		return nil, nil, "", false
	}
	return auth.Clone(), executor, providerKey, true
}

func (m *Manager) pickNextViaHome(
	ctx context.Context,
	model string,
	opts cliproxyexecutor.Options,
	tried map[string]struct{},
) (*Auth, ProviderExecutor, string, error) {
	if m == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	executionSessionID := homeExecutionSessionIDFromMetadata(opts.Metadata)
	count := homeAuthCountFromMetadata(opts.Metadata)
	if pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata); pinnedAuthID != "" {
		if homeAuthAlreadyTried(tried, pinnedAuthID) {
			return nil, nil, "", repeatedHomeAuthError()
		}
		if executionSessionID != "" {
			if auth, executor, providerKey, ok := m.homeRuntimeAuthByID(executionSessionID, pinnedAuthID); ok {
				return auth, executor, providerKey, nil
			}
		}
		return nil, nil, "", pinnedAuthNotFoundError(pinnedAuthID)
	}

	client := m.homeDispatcher()
	if client == nil || !client.HeartbeatOK() {
		return nil, nil, "", &Error{
			Code: "home_unavailable", Message: "home control center unavailable",
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}

	requestedModel := requestedModelFromMetadata(opts.Metadata, model)
	sessionID := ExtractSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata)
	dispatchHeaders := homeDispatchHeaders(ctx, opts.Headers)
	raw, err := client.RPopAuth(ctx, requestedModel, sessionID, dispatchHeaders, count)
	if err != nil {
		return nil, nil, "", &Error{
			Code: "auth_not_found", Message: err.Error(), HTTPStatus: http.StatusServiceUnavailable,
		}
	}

	var env homeErrorEnvelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal == nil && env.Error != nil {
		code := strings.TrimSpace(env.Error.Type)
		if code == "" {
			code = strings.TrimSpace(env.Error.Code)
		}
		msg := strings.TrimSpace(env.Error.Message)
		if msg == "" {
			msg = "home returned error"
		}
		status := http.StatusBadGateway
		switch strings.ToLower(code) {
		case "model_not_found":
			status = http.StatusNotFound
		case "authentication_error", "unauthorized":
			status = http.StatusUnauthorized
		}
		return nil, nil, "", &Error{Code: code, Message: msg, HTTPStatus: status}
	}

	var dispatch homeAuthDispatchResponse
	if errUnmarshal := json.Unmarshal(raw, &dispatch); errUnmarshal != nil {
		return nil, nil, "", &Error{
			Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway,
		}
	}
	setHomeUserAPIKeyOnGinContext(ctx, dispatch.UserAPIKey)
	auth := dispatch.Auth
	if strings.TrimSpace(auth.ID) == "" {
		if errUnmarshal := json.Unmarshal(raw, &auth); errUnmarshal != nil {
			return nil, nil, "", &Error{
				Code: "invalid_auth", Message: "home returned invalid auth payload", HTTPStatus: http.StatusBadGateway,
			}
		}
	}
	if upstreamModel := strings.TrimSpace(dispatch.Model); upstreamModel != "" {
		if auth.Attributes == nil {
			auth.Attributes = make(map[string]string, 1)
		}
		auth.Attributes[homeUpstreamModelAttributeKey] = upstreamModel
	}
	if strings.TrimSpace(auth.ID) == "" {
		return nil, nil, "", &Error{
			Code: "invalid_auth", Message: "home returned auth without id", HTTPStatus: http.StatusBadGateway,
		}
	}
	if homeAuthAlreadyTried(tried, auth.ID) {
		return nil, nil, "", repeatedHomeAuthError()
	}
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	if providerKey == "" {
		return nil, nil, "", &Error{
			Code: "invalid_auth", Message: "home returned auth without provider", HTTPStatus: http.StatusBadGateway,
		}
	}
	if homeAuthIndex := strings.TrimSpace(dispatch.AuthIndex); homeAuthIndex != "" {
		auth.Index = homeAuthIndex
		auth.indexAssigned = true
	} else {
		auth.EnsureIndex()
	}
	executor, ok := m.Executor(providerKey)
	if !ok && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["base_url"]) != "" {
		executor, ok = m.Executor("openai-compatibility")
		if ok {
			providerKey = "openai-compatibility"
		}
	}
	if !ok {
		return nil, nil, "", &Error{
			Code: "executor_not_found", Message: "executor not registered", HTTPStatus: http.StatusBadGateway,
		}
	}
	if requireResponsesContinuationFromMetadata(opts.Metadata) && !authSupportsResponsesContinuation(&auth) {
		return nil, nil, "", &Error{
			Code:       "auth_unavailable",
			Message:    "selected auth does not support Responses continuation",
			HTTPStatus: http.StatusConflict,
		}
	}
	authCopy := auth.Clone()
	authCopy.executorKey = providerKey
	if cliproxyexecutor.DownstreamWebsocket(ctx) && executionSessionID != "" {
		m.rememberHomeRuntimeAuth(executionSessionID, authCopy)
	}
	return authCopy, executor, providerKey, nil
}

func authPersistenceSkipped(ctx context.Context, auth *Auth) bool {
	if auth == nil || shouldSkipPersist(ctx) && !shouldForcePersist(ctx) {
		return true
	}
	if auth.Attributes != nil {
		if value := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); value == "true" {
			return true
		}
	}
	return auth.Metadata == nil
}

func (m *Manager) persist(
	ctx context.Context,
	auth *Auth,
	allowRecreate bool,
	removedAtStart bool,
	removedSeq uint64,
) (bool, error) {
	if authPersistenceSkipped(ctx, auth) {
		return false, nil
	}
	unlockPersistence, errLock := m.lockPersistence(ctx)
	if errLock != nil {
		return false, errLock
	}
	defer unlockPersistence()
	return m.persistLocked(ctx, auth, allowRecreate, removedAtStart, removedSeq)
}

func (m *Manager) persistLocked(
	ctx context.Context,
	auth *Auth,
	allowRecreate bool,
	removedAtStart bool,
	removedSeq uint64,
) (bool, error) {
	if auth == nil || shouldSkipPersist(ctx) && !shouldForcePersist(ctx) {
		return false, nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return false, nil
		}
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
	if auth.Metadata == nil {
		return false, nil
	}
	m.mu.RLock()
	store := m.store
	currentSeq, removed := m.removedAuths[auth.ID]
	m.mu.RUnlock()
	if store == nil || removed && (!allowRecreate || !removedAtStart || currentSeq != removedSeq) {
		return false, nil
	}
	savedPath, err := store.Save(ctx, auth)
	if err == nil && strings.TrimSpace(savedPath) != "" {
		if version, errVersion := PersistedVersionForAuth(savedPath, auth); errVersion == nil {
			m.persistedAuthVersions.Store(strings.TrimSpace(auth.ID), version)
		} else {
			log.WithError(errVersion).Warnf("could not capture persisted credential version for %s", auth.ID)
			m.persistedAuthVersions.Store(
				strings.TrimSpace(auth.ID),
				PersistedVersion{Path: savedPath},
			)
		}
	}
	return strings.TrimSpace(savedPath) != "", err
}

func (m *Manager) rollbackRefreshedAuthAndUnlock(previous *Auth, unlockPersistence func()) error {
	timeout := refreshPersistenceRollbackTimeout()
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), timeout)
	rollbackDone := make(chan error, 1)
	authID := ""
	var pending chan struct{}
	if previous != nil {
		authID = strings.TrimSpace(previous.ID)
	}
	rollbackRemovalEpoch := m.authRemovalEpoch(authID)
	if authID != "" {
		pending = make(chan struct{})
		m.rollbackStateMu.Lock()
		m.pendingPersistenceRollbacks.Store(authID, pending)
		m.rollbackStateMu.Unlock()
	}
	m.deferredAuthMu.Lock()
	if m.deferredAuthStopped {
		m.deferredAuthMu.Unlock()
		cancelRollback()
		unlockPersistence()
		if pending != nil {
			m.finishPersistenceRollback(authID, pending, rollbackRemovalEpoch)
		}
		return errors.Join(
			errAuthDurableStateUncertain,
			errors.New("credential rollback rejected after auth workers stopped"),
		)
	}
	m.deferredAuthWorkers.Add(1)
	m.deferredAuthMu.Unlock()
	completeAsync := make(chan bool, 1)
	go func() {
		defer m.deferredAuthWorkers.Done()
		_, errPersist := m.persistLocked(rollbackCtx, previous, false, false, 0)
		rollbackDone <- errPersist
		if <-completeAsync && pending != nil {
			if errPersist != nil {
				log.WithError(errPersist).Warnf("credential rollback completed with error for %s", authID)
			}
			m.finishPersistenceRollback(authID, pending, rollbackRemovalEpoch)
		}
	}()

	var errRollback error
	timedOut := false
	select {
	case errRollback = <-rollbackDone:
	case <-rollbackCtx.Done():
		errRollback = rollbackCtx.Err()
		timedOut = true
	}
	completeAsync <- timedOut
	cancelRollback()
	unlockPersistence()
	if pending != nil && !timedOut {
		m.finishPersistenceRollback(authID, pending, rollbackRemovalEpoch)
	}
	if errRollback == nil {
		return nil
	}
	return errors.Join(
		errAuthDurableStateUncertain,
		fmt.Errorf("restore previous credential: %w", errRollback),
	)
}

func (m *Manager) finishPersistenceRollback(
	id string,
	pending chan struct{},
	rollbackRemovalEpoch uint64,
) {
	m.rollbackStateMu.Lock()
	m.mu.RLock()
	_, removed := m.removedAuths[id]
	current := m.auths[id]
	m.mu.RUnlock()
	removalValue, removalPending := m.pendingAuthRemovals.Load(id)
	removalIntent, _ := removalValue.(*authRemovalIntent)
	removalAdvanced := m.authRemovalEpoch(id) != rollbackRemovalEpoch
	deletionPending := removed && current == nil || removalPending && removalIntent != nil || removalAdvanced
	deletionQueued := true
	if deletionPending {
		waitFor := (<-chan struct{})(pending)
		if !removed && removalIntent != nil {
			waitFor = removalIntent.ready
		}
		deletionQueued = m.ensureAuthDeletionAfterPersistenceRollback(id, waitFor, nil)
	}
	m.pendingPersistenceRollbacks.CompareAndDelete(id, pending)
	m.rollbackStateMu.Unlock()
	close(pending)
	if deletionPending && !deletionQueued {
		m.recordDeferredAuthStopError(errors.Join(
			errAuthDurableStateUncertain,
			fmt.Errorf("credential rollback completed after auth deletion workers stopped for %s", id),
		))
	}
}

// StartAutoRefresh launches a background loop that evaluates auth freshness
// every few seconds and triggers refresh operations when required.
// Only one loop is kept alive; starting a new one cancels the previous run.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) {
	if m == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	if interval <= 0 {
		interval = refreshCheckInterval
	}
	_ = m.stopAutoRefreshWorkers(context.Background())
	ctx, cancel := context.WithCancel(parent)
	m.refreshLifecycleMu.Lock()
	m.refreshCancel = cancel
	m.refreshStopping = false
	m.refreshWorkers.Add(1)
	m.refreshLifecycleMu.Unlock()
	go func() {
		defer m.refreshWorkers.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		m.checkRefreshes(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.checkRefreshes(ctx)
			}
		}
	}()
}

func (m *Manager) stopAutoRefreshWorkers(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.refreshLifecycleMu.Lock()
	m.refreshStopping = true
	cancel := m.refreshCancel
	m.refreshCancel = nil
	if cancel != nil {
		cancel()
	}
	m.refreshLifecycleMu.Unlock()
	done := make(chan struct{})
	go func() {
		m.refreshWorkers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return errors.Join(ctx.Err(), errAuthDurableStateUncertain)
	}
}

// StopAutoRefresh cancels and waits for the background refresh lifecycle.
func (m *Manager) StopAutoRefresh(ctx context.Context) error {
	errStop := m.stopAutoRefreshWorkers(ctx)
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	if stoppable, ok := selector.(StoppableSelector); ok && stoppable != nil {
		stoppable.Stop()
	}
	return errStop
}

func (m *Manager) checkRefreshes(ctx context.Context) {
	// log.Debugf("checking refreshes")
	now := time.Now()
	snapshot := m.snapshotAuths()
	for _, a := range snapshot {
		typ, _ := a.AccountInfo()
		if typ != "api_key" {
			if !m.shouldRefresh(a, now) {
				continue
			}
			log.Debugf("checking refresh for %s, %s, %s", a.Provider, a.ID, typ)

			if exec := m.executorFor(a.Provider); exec == nil {
				continue
			}
			m.refreshLifecycleMu.Lock()
			if m.refreshStopping || ctx.Err() != nil {
				m.refreshLifecycleMu.Unlock()
				return
			}
			if !m.markRefreshPending(a.ID, now) {
				m.refreshLifecycleMu.Unlock()
				continue
			}
			m.refreshWorkers.Add(1)
			m.refreshLifecycleMu.Unlock()
			go func(id string) {
				defer m.refreshWorkers.Done()
				m.refreshAuthWithLimit(ctx, id)
			}(a.ID)
		}
	}
}

func (m *Manager) refreshAuthWithLimit(ctx context.Context, id string) {
	m.mu.RLock()
	semaphore := m.refreshSemaphore
	m.mu.RUnlock()
	if semaphore == nil {
		m.refreshAuth(ctx, id)
		return
	}
	select {
	case semaphore <- struct{}{}:
		defer func() { <-semaphore }()
	case <-ctx.Done():
		return
	}
	m.refreshAuth(ctx, id)
}

func (m *Manager) snapshotAuths() []*Auth {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Auth, 0, len(m.auths))
	for _, a := range m.auths {
		out = append(out, a.Clone())
	}
	return out
}

func (m *Manager) shouldRefresh(a *Auth, now time.Time) bool {
	if a == nil || a.Disabled || hasUnauthorizedAuthFailure(a) {
		return false
	}
	if !a.NextRefreshAfter.IsZero() && now.Before(a.NextRefreshAfter) {
		return false
	}
	if evaluator, ok := a.Runtime.(RefreshEvaluator); ok && evaluator != nil {
		return evaluator.ShouldRefresh(now, a)
	}

	lastRefresh := a.LastRefreshedAt
	if lastRefresh.IsZero() {
		if ts, ok := authLastRefreshTimestamp(a); ok {
			lastRefresh = ts
		}
	}

	expiry, hasExpiry := a.ExpirationTime()

	if interval := authPreferredInterval(a); interval > 0 {
		if hasExpiry && !expiry.IsZero() {
			if !expiry.After(now) {
				return true
			}
			if expiry.Sub(now) <= interval {
				return true
			}
		}
		if lastRefresh.IsZero() {
			return true
		}
		return now.Sub(lastRefresh) >= interval
	}

	provider := strings.ToLower(a.Provider)
	lead := ProviderRefreshLead(provider, a.Runtime)
	if lead == nil {
		return false
	}
	if *lead <= 0 {
		if hasExpiry && !expiry.IsZero() {
			return now.After(expiry)
		}
		return false
	}
	if hasExpiry && !expiry.IsZero() {
		return time.Until(expiry) <= *lead
	}
	if !lastRefresh.IsZero() {
		return now.Sub(lastRefresh) >= *lead
	}
	return true
}

func authPreferredInterval(a *Auth) time.Duration {
	if a == nil {
		return 0
	}
	if d := durationFromMetadata(
		a.Metadata, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval",
	); d > 0 {
		return d
	}
	if d := durationFromAttributes(
		a.Attributes, "refresh_interval_seconds", "refreshIntervalSeconds", "refresh_interval", "refreshInterval",
	); d > 0 {
		return d
	}
	return 0
}

func durationFromMetadata(meta map[string]any, keys ...string) time.Duration {
	if len(meta) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if dur := parseDurationValue(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func durationFromAttributes(attrs map[string]string, keys ...string) time.Duration {
	if len(attrs) == 0 {
		return 0
	}
	for _, key := range keys {
		if val, ok := attrs[key]; ok {
			if dur := parseDurationString(val); dur > 0 {
				return dur
			}
		}
	}
	return 0
}

func parseDurationValue(val any) time.Duration {
	switch v := val.(type) {
	case time.Duration:
		if v <= 0 {
			return 0
		}
		return v
	case int:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int32:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case int64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint32:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case uint64:
		if v == 0 {
			return 0
		}
		return time.Duration(v) * time.Second
	case float32:
		if v <= 0 {
			return 0
		}
		return time.Duration(float64(v) * float64(time.Second))
	case float64:
		if v <= 0 {
			return 0
		}
		return time.Duration(v * float64(time.Second))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			if i <= 0 {
				return 0
			}
			return time.Duration(i) * time.Second
		}
		if f, err := v.Float64(); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	case string:
		return parseDurationString(v)
	}
	return 0
}

func parseDurationString(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}
	if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
		return dur
	}
	if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	return 0
}

func authLastRefreshTimestamp(a *Auth) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	if a.Metadata != nil {
		if ts, ok := lookupMetadataTime(
			a.Metadata, "last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt",
		); ok {
			return ts, true
		}
	}
	if a.Attributes != nil {
		for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
			if val := strings.TrimSpace(a.Attributes[key]); val != "" {
				if ts, ok := parseTimeValue(val); ok {
					return ts, true
				}
			}
		}
	}
	return time.Time{}, false
}

func lookupMetadataTime(meta map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseTimeValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func (m *Manager) markRefreshPending(id string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	auth, ok := m.auths[id]
	if !ok || auth == nil || auth.Disabled {
		return false
	}
	if !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		return false
	}
	auth.NextRefreshAfter = now.Add(refreshPendingBackoff)
	m.auths[id] = auth
	return true
}

type authRefreshLock struct {
	token chan struct{}
	refs  int
}

func newAuthRefreshLock() *authRefreshLock {
	lock := &authRefreshLock{token: make(chan struct{}, 1)}
	lock.token <- struct{}{}
	return lock
}

func authMetadataString(auth *Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	return contextStringValue(auth.Metadata[key])
}

func authAccessToken(auth *Auth) string {
	if token := authMetadataString(auth, "access_token"); token != "" {
		return token
	}
	return authMetadataString(auth, "accessToken")
}

func authHasRefreshCredential(auth *Auth) bool {
	if authMetadataString(auth, "refresh_token") != "" {
		return true
	}
	return authMetadataString(auth, "refreshToken") != ""
}

func clearUnauthorizedModelStates(auth *Auth, now time.Time) []string {
	if auth == nil || len(auth.ModelStates) == 0 {
		return nil
	}
	resumed := make([]string, 0)
	for model, state := range auth.ModelStates {
		if state == nil || state.LastError == nil {
			continue
		}
		if state.LastError.StatusCode() != http.StatusUnauthorized && !strings.EqualFold(state.LastError.Code, "unauthorized") {
			continue
		}
		resetModelState(state, now)
		resumed = append(resumed, model)
	}
	if len(resumed) > 0 {
		updateAggregatedAvailability(auth, now)
	}
	return resumed
}

func runtimeAuthCredentialsEqual(left, right *Auth) bool {
	if left == nil || right == nil {
		return left == right
	}
	return strings.TrimSpace(left.Provider) == strings.TrimSpace(right.Provider) &&
		strings.TrimSpace(left.Prefix) == strings.TrimSpace(right.Prefix) &&
		strings.TrimSpace(left.ProxyURL) == strings.TrimSpace(right.ProxyURL) &&
		reflect.DeepEqual(left.Storage, right.Storage) &&
		reflect.DeepEqual(left.Attributes, right.Attributes) &&
		reflect.DeepEqual(left.Metadata, right.Metadata) &&
		reflect.DeepEqual(left.Runtime, right.Runtime)
}

func runtimeAuthRefreshEqual(left, right *Auth) bool {
	if !runtimeAuthCredentialsEqual(left, right) {
		return false
	}
	if left == nil || right == nil {
		return true
	}
	return strings.TrimSpace(left.Index) == strings.TrimSpace(right.Index)
}

func (m *Manager) refreshRuntimeAuthViaHome(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, errors.New("runtime auth is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client := m.homeRefreshClient()
	if client == nil || !client.HeartbeatOK() {
		return nil, errors.New("home control center unavailable")
	}
	requestAuth := auth.Clone()
	authIndex := strings.TrimSpace(requestAuth.Index)
	if authIndex == "" {
		authIndex = strings.TrimSpace(requestAuth.EnsureIndex())
	}
	if authIndex == "" {
		return nil, errors.New("home refresh auth_index is empty")
	}
	raw, errRefresh := client.GetRefreshAuth(ctx, authIndex)
	if errRefresh != nil {
		return nil, errRefresh
	}
	var rawObject map[string]json.RawMessage
	if errDecode := json.Unmarshal(raw, &rawObject); errDecode != nil {
		return nil, errDecode
	}
	if rawError, hasError := rawObject["error"]; hasError && string(rawError) != "null" {
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if errDecode := json.Unmarshal(raw, &envelope); errDecode != nil {
			return nil, errDecode
		}
		message := strings.TrimSpace(envelope.Error.Message)
		if message == "" {
			message = "home returned refresh error"
		}
		return nil, errors.New(message)
	}
	updated := &Auth{}
	returnedIndex := ""
	if _, hasAuth := rawObject["auth"]; hasAuth {
		var envelope homeAuthDispatchResponse
		if errDecode := json.Unmarshal(raw, &envelope); errDecode != nil {
			return nil, errDecode
		}
		updated = &envelope.Auth
		returnedIndex = strings.TrimSpace(envelope.AuthIndex)
	} else if errDecode := json.Unmarshal(raw, updated); errDecode != nil {
		return nil, errDecode
	}
	updated.ID = auth.ID
	updated.Generation = 0
	if returnedIndex != "" {
		authIndex = returnedIndex
	}
	updated.AssignIndex(authIndex)
	return updated, nil
}

// tryRefreshAfterUnauthorized refreshes OAuth credentials once after a 401 so the
// current auth can be retried before fallback/suspend.
func (m *Manager) tryRefreshAfterUnauthorized(
	ctx context.Context,
	auth *Auth,
	executor ProviderExecutor,
	execErr error,
	alreadyTried bool,
) (*Auth, bool) {
	if m == nil || auth == nil || executor == nil || alreadyTried || execErr == nil {
		return auth, false
	}
	if !isUnauthorizedError(execErr) {
		return auth, false
	}
	if auth.Generation == 0 {
		if strings.TrimSpace(auth.Index) == "" && !authHasRefreshCredential(auth) {
			return auth, false
		}
	} else if evaluator, ok := executor.(UnauthorizedRefreshEvaluator); ok {
		if evaluator == nil || !evaluator.ShouldRefreshAfterUnauthorized(auth) {
			return auth, false
		}
	} else if !authHasRefreshCredential(auth) {
		return auth, false
	}
	log.Debugf("unauthorized response for %s (%s), refreshing credentials before fallback", auth.Provider, auth.ID)
	if auth.Generation == 0 {
		refreshed, errHomeRefresh := m.refreshRuntimeAuthViaHome(ctx, auth)
		if errHomeRefresh == nil && refreshed != nil && !runtimeAuthRefreshEqual(refreshed, auth) {
			if !m.providerExecutorCurrent(homeRuntimeExecutorKey(auth), executor) {
				return auth, false
			}
			return refreshed, true
		}
		refreshed, errRefresh := executor.Refresh(ctx, auth.Clone())
		if errRefresh != nil || refreshed == nil ||
			strings.TrimSpace(refreshed.ID) != strings.TrimSpace(auth.ID) ||
			runtimeAuthRefreshEqual(refreshed, auth) ||
			!m.providerExecutorCurrent(homeRuntimeExecutorKey(auth), executor) {
			log.Debugf(
				"runtime credential refresh before fallback failed for %s (%s): home=%v executor=%v",
				auth.Provider, auth.ID, errHomeRefresh, errRefresh,
			)
			return auth, false
		}
		refreshed.Generation = 0
		return refreshed, true
	}
	refreshed, errRefresh := m.refreshAuthForRequest(
		ctx,
		auth.ID,
		authAccessToken(auth),
		auth.Generation,
	)
	if errRefresh != nil || refreshed == nil {
		log.Debugf("credential refresh before fallback failed for %s (%s): %v", auth.Provider, auth.ID, errRefresh)
		return auth, false
	}
	return refreshed, true
}

func (m *Manager) refreshAuth(ctx context.Context, id string) {
	_, _ = m.refreshAuthForRequest(ctx, id, "")
}

func clonePersistedVersion(version *PersistedVersion) *PersistedVersion {
	if version == nil {
		return nil
	}
	cloned := *version
	return &cloned
}

func authPersistedPath(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if path := strings.TrimSpace(auth.Attributes["path"]); path != "" {
			return path
		}
		if path := strings.TrimSpace(auth.Attributes["source"]); path != "" {
			return path
		}
	}
	return ""
}

func (m *Manager) captureRefreshPersistedVersion(auth *Auth) (*PersistedVersion, error) {
	if m == nil || auth == nil || authPersistenceSkipped(context.Background(), auth) {
		return nil, nil
	}
	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	if store == nil {
		return nil, nil
	}

	path := authPersistedPath(auth)
	pathFromAuth := path != ""
	if path == "" {
		if tracked := m.persistedAuthVersion(auth.ID); tracked != nil {
			path = strings.TrimSpace(tracked.Path)
		}
	}
	if path == "" {
		return nil, nil
	}

	unlockFile := authfile.LockFileMutation(path)
	defer unlockFile()
	current, errCurrent := ReadPersistedVersion(path)
	if errCurrent != nil {
		if !pathFromAuth && errors.Is(errCurrent, os.ErrNotExist) {
			return nil, nil
		}
		return nil, errors.Join(
			errAuthDurableStateUncertain,
			fmt.Errorf("read persisted credential before refresh: %w", errCurrent),
		)
	}
	expected, errExpected := PersistedVersionForAuth(path, auth)
	if errExpected != nil {
		return nil, errors.Join(
			errAuthDurableStateUncertain,
			fmt.Errorf("capture credential version before refresh: %w", errExpected),
		)
	}
	if current.SHA256 != expected.SHA256 {
		return nil, errors.Join(
			errAuthDurableStateUncertain,
			errRefreshPersistedVersionChanged,
		)
	}
	return &current, nil
}

func lockRefreshPersistedVersion(version *PersistedVersion) (func(), error) {
	if version == nil || strings.TrimSpace(version.Path) == "" {
		return func() {}, nil
	}
	unlockFile := authfile.LockFileMutation(version.Path)
	current, errCurrent := ReadPersistedVersion(version.Path)
	if errCurrent != nil {
		unlockFile()
		return nil, errors.Join(errRefreshPersistedVersionChanged, errCurrent)
	}
	if current.SHA256 != version.SHA256 {
		unlockFile()
		return nil, errRefreshPersistedVersionChanged
	}
	return unlockFile, nil
}

// refreshAuthForRequest performs a synchronous credential refresh for the given auth.
// failedAccessToken lets concurrent callers reuse a refresh that already replaced the
// access token that produced the unauthorized response.
func (m *Manager) refreshAuthForRequest(
	ctx context.Context,
	id,
	failedAccessToken string,
	expectedGeneration ...uint64,
) (*Auth, error) {
	if m == nil {
		return nil, errors.New("auth manager is nil")
	}
	ctx = m.withHomeRefreshClient(ctx)
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("auth id is empty")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.refreshLocksMu.Lock()
	lock := m.refreshLocks[id]
	if lock == nil {
		lock = newAuthRefreshLock()
		m.refreshLocks[id] = lock
	}
	lock.refs++
	m.refreshLocksMu.Unlock()
	releaseRef := func() {
		m.refreshLocksMu.Lock()
		lock.refs--
		if lock.refs == 0 && m.refreshLocks[id] == lock {
			delete(m.refreshLocks, id)
		}
		m.refreshLocksMu.Unlock()
	}
	select {
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	case <-lock.token:
	}
	defer func() {
		lock.token <- struct{}{}
		releaseRef()
	}()
	unlockMutation, errMutation := m.lockAuthMutation(ctx, id)
	if errMutation != nil {
		return nil, errMutation
	}
	mutationLocked := true
	defer func() {
		if mutationLocked {
			unlockMutation()
		}
	}()

	m.mu.RLock()
	auth := m.auths[id]
	var exec ProviderExecutor
	if auth != nil {
		exec = m.executors[strings.ToLower(strings.TrimSpace(auth.Provider))]
	}
	m.mu.RUnlock()
	if auth == nil || exec == nil {
		return nil, errors.New("auth or executor not found")
	}
	if len(expectedGeneration) > 0 && expectedGeneration[0] > 0 && auth.Generation != expectedGeneration[0] {
		return auth.Clone(), nil
	}

	if failedAccessToken != "" {
		if currentToken := authAccessToken(auth); currentToken != "" && currentToken != failedAccessToken {
			return auth.Clone(), nil
		}
	}

	persistenceExpected := auth.Clone()
	persistenceOwnerID := ""
	if auth.Attributes != nil {
		persistenceOwnerID = strings.TrimSpace(auth.Attributes["gemini_virtual_parent"])
	}
	if persistenceOwnerID != "" && persistenceOwnerID != auth.ID {
		unlockOwnerMutation, errOwnerMutation := m.lockAuthMutation(ctx, persistenceOwnerID)
		if errOwnerMutation != nil {
			return nil, errOwnerMutation
		}
		defer unlockOwnerMutation()
		m.mu.RLock()
		persistenceOwner := m.auths[persistenceOwnerID]
		m.mu.RUnlock()
		if persistenceOwner == nil {
			return nil, fmt.Errorf("refresh persistence owner %s not found", persistenceOwnerID)
		}
		persistenceExpected = persistenceOwner.Clone()
	}

	expectedPersistedVersion, errVersion := m.captureRefreshPersistedVersion(persistenceExpected)
	if errVersion != nil {
		return nil, errVersion
	}
	cloned := auth.Clone()
	updated, err := exec.Refresh(ctx, cloned)
	if err != nil && errors.Is(err, context.Canceled) {
		log.Debugf("refresh canceled for %s, %s", auth.Provider, auth.ID)
		return nil, err
	}
	log.Debugf("refreshed %s, %s, %v", auth.Provider, auth.ID, err)
	now := time.Now()
	var refreshCommit RefreshCommitGuard
	var refreshPersistence *Auth
	if updated != nil {
		refreshCommit = updated.RefreshCommit
		updated.RefreshCommit = nil
		refreshPersistence = updated.RefreshPersistence
		updated.RefreshPersistence = nil
	}
	if err != nil {
		if !m.publishRefreshError(auth, exec, refreshCommit, err, now) {
			return nil, errors.New("credential refresh error is stale")
		}
		return nil, err
	}
	if updated == nil || runtimeAuthCredentialsEqual(updated, auth) {
		return nil, errors.New("credential refresh returned unchanged credentials")
	}
	commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), refreshPersistenceCommitTimeout)
	defer cancelCommit()
	saved, modelsToResume, errCommit := m.commitRefreshedAuth(
		commitCtx,
		auth,
		updated,
		persistenceExpected,
		refreshPersistence,
		exec,
		refreshCommit,
		expectedPersistedVersion,
		now,
	)
	if saved != nil {
		for _, model := range modelsToResume {
			registry.GetGlobalRegistry().ResumeClientModel(id, model)
		}
		unlockMutation()
		mutationLocked = false
		m.hook.OnAuthUpdated(commitCtx, saved.Clone())
	}
	if errCommit != nil {
		log.Debugf("persist refreshed auth %s (%s) failed: %v", auth.Provider, auth.ID, errCommit)
		return saved, errCommit
	}
	if saved != nil {
		return saved, nil
	}
	return nil, errors.New("credential refresh result is stale")
}

func refreshCommitGuardCurrent(guard RefreshCommitGuard) bool {
	if guard == nil {
		return true
	}
	validator, ok := guard.(interface{ Current() bool })
	return !ok || validator.Current()
}

func (m *Manager) publishRefreshError(
	expected *Auth,
	expectedExecutor ProviderExecutor,
	guard RefreshCommitGuard,
	refreshErr error,
	now time.Time,
) bool {
	if m == nil || expected == nil || expectedExecutor == nil || refreshErr == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(expected.Provider))
	m.executorMu.RLock()
	m.mu.Lock()
	current := m.auths[expected.ID]
	if current == nil || current.Generation != expected.Generation ||
		!sameProviderExecutor(m.executors[provider], expectedExecutor) {
		m.mu.Unlock()
		m.executorMu.RUnlock()
		return false
	}
	previous := current
	var snapshot *Auth
	commit := func() bool {
		next := current.Clone()
		next.LastError = refreshErrorFromError(refreshErr)
		if isUnauthorizedError(refreshErr) {
			next.NextRefreshAfter = time.Time{}
			next.Unavailable = true
			next.Status = StatusError
			next.StatusMessage = "unauthorized"
		} else {
			next.NextRefreshAfter = now.Add(refreshFailureBackoff)
		}
		snapshot = next.Clone()
		m.auths[expected.ID] = next
		return true
	}
	committed := false
	if guard != nil {
		committed = guard.CommitRefresh(commit)
	} else {
		committed = commit()
	}
	if !committed {
		m.auths[expected.ID] = previous
		snapshot = nil
	}
	m.mu.Unlock()
	m.executorMu.RUnlock()
	if committed && snapshot != nil && m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	return committed
}

func (m *Manager) publishRefreshedAuthState(
	expected,
	next *Auth,
	expectedExecutor ProviderExecutor,
	guard RefreshCommitGuard,
) *Auth {
	saved, _ := m.publishRefreshedAuthStates(expected, next, nil, nil, expectedExecutor, guard)
	return saved
}

func (m *Manager) publishRefreshedAuthStates(
	expected,
	next,
	persistenceExpected,
	persistenceNext *Auth,
	expectedExecutor ProviderExecutor,
	guard RefreshCommitGuard,
) (*Auth, *Auth) {
	if m == nil || expected == nil || next == nil || expectedExecutor == nil || !refreshCommitGuardCurrent(guard) {
		return nil, nil
	}
	provider := strings.ToLower(strings.TrimSpace(expected.Provider))
	m.executorMu.RLock()
	m.mu.Lock()
	current := m.auths[expected.ID]
	if current == nil || current.Generation != expected.Generation ||
		!sameProviderExecutor(m.executors[provider], expectedExecutor) {
		m.mu.Unlock()
		m.executorMu.RUnlock()
		return nil, nil
	}
	previous := current
	var persistenceCurrent *Auth
	if persistenceExpected != nil && persistenceNext != nil && persistenceExpected.ID != expected.ID {
		persistenceCurrent = m.auths[persistenceExpected.ID]
		if persistenceCurrent == nil || persistenceCurrent.Generation != persistenceExpected.Generation {
			m.mu.Unlock()
			m.executorMu.RUnlock()
			return nil, nil
		}
	}
	previousPersistence := persistenceCurrent
	var saved *Auth
	var persistenceSaved *Auth
	commit := func() bool {
		if persistenceCurrent != nil {
			persistenceSaved = persistenceNext.Clone()
			m.assignAuthGenerationLocked(persistenceSaved)
			m.auths[persistenceExpected.ID] = persistenceSaved
		}
		saved = next.Clone()
		m.assignAuthGenerationLocked(saved)
		m.auths[expected.ID] = saved
		if persistenceSaved == nil && persistenceExpected != nil && persistenceExpected.ID == expected.ID {
			persistenceSaved = saved
		}
		return true
	}
	committed := false
	if guard != nil {
		committed = guard.CommitRefresh(commit)
	} else {
		committed = commit()
	}
	if !committed {
		m.auths[expected.ID] = previous
		if persistenceCurrent != nil {
			m.auths[persistenceExpected.ID] = previousPersistence
		}
		saved = nil
		persistenceSaved = nil
	}
	m.mu.Unlock()
	m.executorMu.RUnlock()
	if saved == nil {
		return nil, nil
	}
	if m.scheduler != nil {
		if persistenceSaved != nil && persistenceSaved.ID != saved.ID {
			m.scheduler.upsertAuth(persistenceSaved.Clone())
		}
		m.scheduler.upsertAuth(saved.Clone())
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	return saved.Clone(), persistenceSaved.Clone()
}

func refreshPersistenceNextVersion(expected *PersistedVersion, auth *Auth) *PersistedVersion {
	if auth == nil {
		return nil
	}
	path := authPersistedPath(auth)
	if expected != nil && strings.TrimSpace(expected.Path) != "" {
		path = expected.Path
	}
	if path == "" {
		return nil
	}
	version, errVersion := PersistedVersionForAuth(path, auth)
	if errVersion != nil {
		return nil
	}
	return &version
}

func (m *Manager) cancelPendingRefreshPersistence(id string) bool {
	if m == nil {
		return false
	}
	value, ok := m.pendingRefreshPersistences.LoadAndDelete(strings.TrimSpace(id))
	if !ok {
		return false
	}
	intent, _ := value.(*authRefreshPersistenceIntent)
	if intent != nil && intent.cancel != nil {
		intent.cancel()
	}
	return true
}

func (m *Manager) scheduleRefreshPersistenceRetry(
	auth *Auth,
	expectedVersion *PersistedVersion,
) bool {
	if m == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return false
	}
	m.deferredAuthMu.Lock()
	if m.deferredAuthStopped || m.deferredAuthContext == nil {
		m.deferredAuthMu.Unlock()
		return false
	}
	intentContext, cancelIntent := context.WithCancel(m.deferredAuthContext)
	intent := &authRefreshPersistenceIntent{
		auth:            auth.Clone(),
		expectedVersion: clonePersistedVersion(expectedVersion),
		nextVersion:     refreshPersistenceNextVersion(expectedVersion, auth),
		generation:      auth.Generation,
		ctx:             intentContext,
		cancel:          cancelIntent,
	}
	previous, replaced := m.pendingRefreshPersistences.Swap(auth.ID, intent)
	if replaced {
		if previousIntent, ok := previous.(*authRefreshPersistenceIntent); ok && previousIntent != nil &&
			previousIntent.cancel != nil {
			previousIntent.cancel()
		}
	}
	m.deferredAuthWorkers.Add(1)
	m.deferredAuthMu.Unlock()
	go m.retryRefreshPersistence(auth.ID, intent)
	return true
}

func (m *Manager) lockRefreshPersistenceRetry(intent *authRefreshPersistenceIntent) (func(), error) {
	if intent == nil {
		return func() {}, nil
	}
	version := intent.expectedVersion
	if version == nil {
		version = intent.nextVersion
	}
	if version == nil || strings.TrimSpace(version.Path) == "" {
		return func() {}, nil
	}
	unlockFile := authfile.LockFileMutation(version.Path)
	current, errCurrent := ReadPersistedVersion(version.Path)
	if errCurrent != nil {
		unlockFile()
		return nil, errCurrent
	}
	matchesExpected := intent.expectedVersion != nil && current.SHA256 == intent.expectedVersion.SHA256
	matchesNext := intent.nextVersion != nil && current.SHA256 == intent.nextVersion.SHA256
	if !matchesExpected && !matchesNext {
		unlockFile()
		return nil, errRefreshPersistedVersionChanged
	}
	return unlockFile, nil
}

func (m *Manager) persistRefreshIntent(ctx context.Context, intent *authRefreshPersistenceIntent) error {
	unlockFile, errVersion := m.lockRefreshPersistenceRetry(intent)
	if errVersion != nil {
		return errVersion
	}
	defer unlockFile()
	unlockPersistence, errLock := m.lockPersistence(ctx)
	if errLock != nil {
		return errLock
	}
	defer unlockPersistence()
	_, errPersist := m.persistLocked(ctx, intent.auth, false, false, 0)
	return errPersist
}

func (m *Manager) retryRefreshPersistence(id string, intent *authRefreshPersistenceIntent) {
	defer m.deferredAuthWorkers.Done()
	defer intent.cancel()
	defer m.pendingRefreshPersistences.CompareAndDelete(id, intent)
	delay, maxDelay := deferredAuthRetryDelays()
	for {
		unlockMutation, errMutation := m.lockAuthMutation(intent.ctx, id)
		if errMutation == nil {
			m.mu.RLock()
			current := m.auths[id]
			currentGeneration := uint64(0)
			if current != nil {
				currentGeneration = current.Generation
			}
			m.mu.RUnlock()
			if currentGeneration != intent.generation {
				unlockMutation()
				return
			}
			timeout := refreshPersistenceCommitTimeout
			if timeout <= 0 {
				timeout = 60 * time.Second
			}
			persistCtx, cancelPersist := context.WithTimeout(intent.ctx, timeout)
			errPersist := m.persistRefreshIntent(persistCtx, intent)
			cancelPersist()
			unlockMutation()
			if errPersist == nil {
				return
			}
			if errors.Is(errPersist, errRefreshPersistedVersionChanged) {
				m.recordDeferredAuthStopError(errors.Join(errAuthDurableStateUncertain, errPersist))
				return
			}
			log.WithError(errPersist).Warnf("retrying refreshed credential persistence for %s", id)
		} else if intent.ctx.Err() != nil {
			m.recordDeferredAuthStopError(errors.Join(errAuthDurableStateUncertain, errMutation))
			return
		}

		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-intent.ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			m.recordDeferredAuthStopError(errors.Join(
				errAuthDurableStateUncertain,
				fmt.Errorf("refreshed credential persistence stopped for %s", id),
			))
			return
		}
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func (m *Manager) commitRefreshedAuth(
	ctx context.Context,
	expected,
	update,
	persistenceExpected,
	persistenceUpdate *Auth,
	expectedExecutor ProviderExecutor,
	guard RefreshCommitGuard,
	expectedVersion *PersistedVersion,
	now time.Time,
) (*Auth, []string, error) {
	if m == nil || expected == nil || update == nil || expectedExecutor == nil {
		return nil, nil, errors.New("credential refresh commit is invalid")
	}
	if !refreshCommitGuardCurrent(guard) {
		return nil, nil, errors.New("credential refresh owner changed before persistence")
	}
	provider := strings.ToLower(strings.TrimSpace(expected.Provider))
	m.executorMu.RLock()
	m.mu.RLock()
	current := m.auths[expected.ID]
	if current == nil || current.Generation != expected.Generation ||
		!sameProviderExecutor(m.executors[provider], expectedExecutor) {
		m.mu.RUnlock()
		m.executorMu.RUnlock()
		return nil, nil, errors.New("credential refresh result is stale")
	}
	previous := current.Clone()
	next := mergePersistentAuthState(current, update)
	persistTarget := next
	rollbackTarget := previous
	var persistenceNext *Auth
	if persistenceUpdate != nil {
		if persistenceExpected == nil || persistenceExpected.ID == "" ||
			persistenceExpected.ID != persistenceUpdate.ID {
			m.mu.RUnlock()
			m.executorMu.RUnlock()
			return nil, nil, errors.New("credential refresh persistence owner is invalid")
		}
		persistenceCurrent := m.auths[persistenceExpected.ID]
		if persistenceCurrent == nil || persistenceCurrent.Generation != persistenceExpected.Generation {
			m.mu.RUnlock()
			m.executorMu.RUnlock()
			return nil, nil, errors.New("credential refresh persistence owner is stale")
		}
		persistenceNext = mergePersistentAuthState(persistenceCurrent, persistenceUpdate)
		persistenceNext.Runtime = persistenceCurrent.Runtime
		persistTarget = persistenceNext
		rollbackTarget = persistenceCurrent.Clone()
		next = current.Clone()
		next.Runtime = current.Runtime
	}
	m.mu.RUnlock()
	m.executorMu.RUnlock()

	if next.Runtime == nil {
		next.Runtime = previous.Runtime
	}
	next.LastRefreshedAt = now
	next.NextRefreshAfter = time.Time{}
	if hasUnauthorizedAuthFailure(next) {
		next.LastError = nil
		next.StatusMessage = ""
		if !next.Quota.Exceeded && !hasModelError(next, now) {
			if next.Status == StatusError {
				next.Status = StatusActive
			}
			next.Unavailable = false
			next.NextRetryAfter = time.Time{}
		} else if next.Quota.Reason != "" {
			next.StatusMessage = next.Quota.Reason
		}
	}
	next.UpdatedAt = now
	modelsToResume := clearUnauthorizedModelStates(next, now)
	if m.shouldRefresh(next, now) {
		next.NextRefreshAfter = now.Add(refreshIneffectiveBackoff)
	}
	if persistenceNext != nil {
		persistenceNext.LastRefreshedAt = now
		persistenceNext.NextRefreshAfter = time.Time{}
		persistenceNext.UpdatedAt = now
	}

	publishUncertain := func(cause error, retry bool) (*Auth, []string, error) {
		saved, persistenceSaved := m.publishRefreshedAuthStates(
			expected,
			next,
			persistenceExpected,
			persistenceNext,
			expectedExecutor,
			guard,
		)
		if saved == nil {
			return nil, nil, errors.Join(
				errAuthDurableStateUncertain,
				cause,
				errors.New("credential refresh result became stale before runtime publication"),
			)
		}
		if persistenceSaved == nil {
			persistenceSaved = saved
		}
		if persistenceSaved.ID != saved.ID {
			m.hook.OnAuthUpdated(ctx, persistenceSaved.Clone())
		}
		if retry && !m.scheduleRefreshPersistenceRetry(persistenceSaved, expectedVersion) {
			cause = errors.Join(cause, errors.New("could not schedule refreshed credential persistence retry"))
		}
		return saved, modelsToResume, errors.Join(errAuthDurableStateUncertain, cause)
	}

	unlockFile, errVersion := lockRefreshPersistedVersion(expectedVersion)
	if errVersion != nil {
		return publishUncertain(errVersion, false)
	}
	unlockPersistence, errPersistenceLock := m.lockPersistence(ctx)
	if errPersistenceLock != nil {
		unlockFile()
		return publishUncertain(errPersistenceLock, true)
	}
	if _, errPersist := m.persistLocked(ctx, persistTarget, false, false, 0); errPersist != nil {
		unlockPersistence()
		unlockFile()
		return publishUncertain(errPersist, true)
	}
	saved, persistenceSaved := m.publishRefreshedAuthStates(
		expected,
		next,
		persistenceExpected,
		persistenceNext,
		expectedExecutor,
		guard,
	)
	if saved == nil {
		errRollback := m.rollbackRefreshedAuthAndUnlock(rollbackTarget, unlockPersistence)
		unlockFile()
		if errRollback != nil {
			return nil, nil, errors.Join(
				errors.New("credential refresh owner changed after persistence"),
				errRollback,
			)
		}
		return nil, nil, errors.New("credential refresh owner changed after persistence")
	}
	unlockPersistence()
	unlockFile()
	if persistenceSaved != nil && persistenceSaved.ID != saved.ID {
		m.hook.OnAuthUpdated(ctx, persistenceSaved.Clone())
	}
	return saved, modelsToResume, nil
}

func (m *Manager) executorFor(provider string) ProviderExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executors[provider]
}

// roundTripperContextKey is an unexported context key type to avoid collisions.
type roundTripperContextKey struct{}

// roundTripperFor retrieves an HTTP RoundTripper for the given auth if a provider is registered.
func (m *Manager) roundTripperFor(auth *Auth) http.RoundTripper {
	m.mu.RLock()
	p := m.rtProvider
	m.mu.RUnlock()
	if p == nil || auth == nil {
		return nil
	}
	return p.RoundTripperFor(auth)
}

// RoundTripperProvider defines a minimal provider of per-auth HTTP transports.
type RoundTripperProvider interface {
	RoundTripperFor(auth *Auth) http.RoundTripper
}

// RequestPreparer is an optional interface that provider executors can implement
// to mutate outbound HTTP requests with provider credentials.
type RequestPreparer interface {
	PrepareRequest(req *http.Request, auth *Auth) error
}

func executorKeyFromAuth(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		providerKey := strings.TrimSpace(auth.Attributes["provider_key"])
		compatName := strings.TrimSpace(auth.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return strings.ToLower(providerKey)
		}
	}
	return strings.ToLower(strings.TrimSpace(auth.Provider))
}

// logEntryWithRequestID returns a logrus entry with request_id field if available in context.
func logEntryWithRequestID(ctx context.Context) *log.Entry {
	if ctx == nil {
		return log.NewEntry(log.StandardLogger())
	}
	if reqID := logging.GetRequestID(ctx); reqID != "" {
		return log.WithField("request_id", reqID)
	}
	return log.NewEntry(log.StandardLogger())
}

func debugLogAuthSelection(entry *log.Entry, auth *Auth, provider string, model string) {
	if !log.IsLevelEnabled(log.DebugLevel) {
		return
	}
	if entry == nil || auth == nil {
		return
	}
	accountType, accountInfo := auth.AccountInfo()
	proxyInfo := auth.ProxyInfo()
	suffix := ""
	if proxyInfo != "" {
		suffix = " " + proxyInfo
	}
	switch accountType {
	case "api_key":
		entry.Debugf("Use API key %s for model %s%s", util.HideAPIKey(accountInfo), model, suffix)
	case "oauth":
		ident := formatOauthIdentity(auth, provider, accountInfo)
		entry.Debugf("Use OAuth %s for model %s%s", ident, model, suffix)
	}
}

func formatOauthIdentity(auth *Auth, provider string, accountInfo string) string {
	if auth == nil {
		return ""
	}
	// Prefer the auth's provider when available.
	providerName := strings.TrimSpace(auth.Provider)
	if providerName == "" {
		providerName = strings.TrimSpace(provider)
	}
	// Only log the basename to avoid leaking host paths.
	// FileName may be unset for some auth backends; fall back to ID.
	authFile := strings.TrimSpace(auth.FileName)
	if authFile == "" {
		authFile = strings.TrimSpace(auth.ID)
	}
	if authFile != "" {
		authFile = filepath.Base(authFile)
	}
	parts := make([]string, 0, 3)
	if providerName != "" {
		parts = append(parts, "provider="+providerName)
	}
	if authFile != "" {
		parts = append(parts, "auth_file="+authFile)
	}
	if len(parts) == 0 {
		return accountInfo
	}
	return strings.Join(parts, " ")
}

// InjectCredentials delegates per-provider HTTP request preparation when supported.
// If the registered executor for the auth provider implements RequestPreparer,
// it will be invoked to modify the request (e.g., add headers).
func (m *Manager) InjectCredentials(req *http.Request, authID string) error {
	if req == nil || authID == "" {
		return nil
	}
	m.mu.RLock()
	a := m.auths[authID]
	var exec ProviderExecutor
	if a != nil {
		exec = m.executors[executorKeyFromAuth(a)]
	}
	m.mu.RUnlock()
	if a == nil || exec == nil {
		return nil
	}
	if p, ok := exec.(RequestPreparer); ok && p != nil {
		return p.PrepareRequest(req, a)
	}
	return nil
}

// PrepareHttpRequest injects provider credentials into the supplied HTTP request.
func (m *Manager) PrepareHttpRequest(ctx context.Context, auth *Auth, req *http.Request) error {
	if m == nil {
		return &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	ctx = m.withHomeRefreshClient(ctx)
	*req = *req.WithContext(ctx)
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	preparedAuth, errPrepare := m.prepareRequestAuth(ctx, exec, auth)
	if errPrepare != nil {
		return errPrepare
	}
	preparer, ok := exec.(RequestPreparer)
	if !ok || preparer == nil {
		return &Error{Code: "not_supported", Message: "executor does not support http request preparation"}
	}
	return preparer.PrepareRequest(req, preparedAuth)
}

// NewHttpRequest constructs a new HTTP request and injects provider credentials into it.
func (m *Manager) NewHttpRequest(
	ctx context.Context,
	auth *Auth,
	method, targetURL string,
	body []byte,
	headers http.Header,
) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	method = strings.TrimSpace(method)
	if method == "" {
		method = http.MethodGet
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, err
	}
	if headers != nil {
		httpReq.Header = headers.Clone()
	}
	if errPrepare := m.PrepareHttpRequest(ctx, auth, httpReq); errPrepare != nil {
		return nil, errPrepare
	}
	return httpReq, nil
}

// HttpRequest injects provider credentials into the supplied HTTP request and executes it.
func (m *Manager) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	if m == nil {
		return nil, &Error{Code: "provider_not_found", Message: "manager is nil"}
	}
	if auth == nil {
		return nil, &Error{Code: "auth_not_found", Message: "auth is nil"}
	}
	if req == nil {
		return nil, &Error{Code: "invalid_request", Message: "http request is nil"}
	}
	ctx = m.withHomeRefreshClient(ctx)
	*req = *req.WithContext(ctx)
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return nil, &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return nil, &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	preparedAuth, errPrepare := m.prepareRequestAuth(ctx, exec, auth)
	if errPrepare != nil {
		return nil, errPrepare
	}
	return exec.HttpRequest(ctx, preparedAuth, req)
}
