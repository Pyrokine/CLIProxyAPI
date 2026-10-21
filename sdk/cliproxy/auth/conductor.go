package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"maps"
	"math/rand/v2"
	"net"
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
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	cliproxysession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/semaphore"
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

// APIKeyConfigExecutor provides an execution-local view without OAuth-only configuration.
// The registered executor and its shared session state remain unchanged.
type APIKeyConfigExecutor interface {
	ForAPIKey() ProviderExecutor
}

// ApplyPatchSupport 表示执行器实际支持 freeform apply_patch
type ApplyPatchSupport interface {
	SupportsApplyPatch() bool
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

// ExecutionSessionAuthCloser allows executors to release sessions owned by one auth.
type ExecutionSessionAuthCloser interface {
	CloseExecutionSessionsForAuth(authID string)
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

// MaxAutoRefreshCheckInterval 限制自动刷新检查的最长等待
const MaxAutoRefreshCheckInterval = 30 * time.Second

const (
	refreshCheckInterval    = 5 * time.Second
	refreshMaxConcurrency   = 16
	refreshPendingBackoff   = time.Minute
	refreshFailureBackoff   = 5 * time.Minute
	invalidGrantBackoffBase = time.Minute
	invalidGrantBackoffMax  = 30 * time.Minute
	// refreshIneffectiveBackoff throttles refresh attempts when an executor returns
	// success but the auth still evaluates as needing refresh.
	refreshIneffectiveBackoff = 30 * time.Second
	quotaBackoffBase          = time.Second
	quotaBackoffMax           = 30 * time.Minute
	minQuotaCooldownFloor     = 10 * time.Second
	transientErrorCooldown    = time.Minute
)

func invalidGrantBackoffDuration(failures int) time.Duration {
	if failures <= 1 {
		return invalidGrantBackoffBase
	}
	return min(invalidGrantBackoffBase*time.Duration(1<<min(failures-1, 5)), invalidGrantBackoffMax)
}

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
	const maxSeconds = int64((1<<63 - 1) / time.Second)
	configured := int64(seconds)
	if configured > maxSeconds {
		log.Warnf("transient-error-cooldown-seconds %d exceeds duration limit; using %d", configured, maxSeconds)
		configured = maxSeconds
	}
	transientErrorCooldownSeconds.Store(configured)
}

func quotaCooldownDisabledForAuth(auth *Auth) bool {
	return quotaCooldownDisabledForAuthWithConfig(auth, nil)
}

func recoverableFailureRetryAfterWithHint(
	now time.Time,
	retryAfter *time.Duration,
	disableCooling bool,
) time.Time {
	if disableCooling {
		return time.Time{}
	}
	seconds := transientErrorCooldownSeconds.Load()
	if seconds < 0 {
		return time.Time{}
	}
	if retryAfter != nil && *retryAfter > 0 {
		return now.Add(*retryAfter)
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
	// Disposition records dispatch, outcome, retry, and state-effect semantics.
	Disposition cliproxyexecutor.ExecutionDisposition
	// StateGeneration identifies the runtime state version observed by the request.
	StateGeneration uint64
	// SkipQuotaObservation prevents read-only or pre-dispatch results from advancing passive quota snapshots.
	SkipQuotaObservation bool
	// Options carries request-scoped affinity identities used to update sticky routing.
	Options cliproxyexecutor.Options

	generationSnapshot resultGenerationSnapshot
}

// Selector chooses an auth candidate for execution.
type Selector interface {
	Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
}

type PluginScheduler interface {
	PickAuth(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error)
}

// PluginSchedulerAcrossPriorities allows a plugin scheduler to consider every available priority tier.
type PluginSchedulerAcrossPriorities interface {
	SchedulerWantsAcrossPriorities() bool
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

const authLoadGateCapacity int64 = 1 << 30

// Manager orchestrates auth lifecycle, selection, execution, and persistence.
type Manager struct {
	store                  Store
	cooldownStore          CooldownStateStore
	configCooldownMu       sync.Mutex
	deferredAuthRecreate   func(context.Context, *Auth) error
	executors              map[string]ProviderExecutor
	selector               Selector
	selectorMu             sync.Mutex
	hook                   Hook
	refreshBindingObserver func(before, after *Auth)
	resultPolicy           atomic.Pointer[resultPolicyHolder]
	mu                     sync.RWMutex
	executorMu             sync.RWMutex
	authIntentMu           sync.Mutex
	rollbackStateMu        sync.Mutex
	persistenceLock        *authMutationLock
	authLoadGate           *semaphore.Weighted
	auths                  map[string]*Auth
	authChangeWatchers     map[string]map[chan struct{}]struct{}
	// Retain each ID's first binding so unversioned results cannot affect replacements.
	firstCredentialGenerations  map[string]uint64
	scheduler                   *authScheduler
	pluginScheduler             PluginScheduler
	homeRuntimeAuths            map[string]map[string]*Auth
	homeRuntimeAuthOwners       map[string]map[string]*HomeDispatchSelection
	homeSessionSelections       map[string]map[homeSessionSelectionKey]*HomeDispatchSelection
	homeSessionAliases          homeSessionAliasCache
	homeSessionLocks            sync.Map
	homeDispatchBundle          atomic.Pointer[HomeDispatchBundle]
	homeInFlightPublisherConfig atomic.Pointer[HomeInFlightPublisherConfig]
	removedAuths                map[string]uint64
	removedAuthAt               map[string]time.Time
	pendingAuthDeletions        map[string]*authDeletionIntent
	removedAuthSeq              uint64
	// authGenerationSeq orders live auth, removal, and registry state publications.
	authGenerationSeq uint64
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
	runtimeScope         string

	requestPrepareLocksMu            sync.Mutex
	requestPrepareLocks              map[string]*authRefreshLock
	authMutationLocksMu              sync.Mutex
	authMutationLocks                map[string]*authMutationLock
	pendingPersistenceRollbacks      sync.Map
	pendingRefreshPersistenceRetries sync.Map
	pendingAuthRecreations           sync.Map
	pendingAuthRemovals              sync.Map
	authRemovalEpochs                sync.Map
	persistedAuthVersions            sync.Map
	deferredAuthMu                   sync.Mutex
	deferredAuthContext              context.Context
	deferredAuthCancel               context.CancelFunc
	deferredAuthWorkers              sync.WaitGroup
	deferredAuthStopped              bool
	deferredAuthStopErr              error
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
	autoRefreshJobs    map[string]*autoRefreshJob // guarded by m.mu
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
		store:                      store,
		executors:                  make(map[string]ProviderExecutor),
		selector:                   selector,
		hook:                       hook,
		auths:                      make(map[string]*Auth),
		firstCredentialGenerations: make(map[string]uint64),
		homeRuntimeAuths:           make(map[string]map[string]*Auth),
		homeRuntimeAuthOwners:      make(map[string]map[string]*HomeDispatchSelection),
		homeSessionSelections:      make(map[string]map[homeSessionSelectionKey]*HomeDispatchSelection),
		removedAuths:               make(map[string]uint64),
		removedAuthAt:              make(map[string]time.Time),
		pendingAuthDeletions:       make(map[string]*authDeletionIntent),
		providerOffsets:            make(map[string]int),
		requestPrepareLocks:        make(map[string]*authRefreshLock),
		authMutationLocks:          make(map[string]*authMutationLock),
		refreshLocks:               make(map[string]*authRefreshLock),
		autoRefreshJobs:            make(map[string]*autoRefreshJob),
		persistenceLock:            newAuthMutationLock(),
		authLoadGate:               semaphore.NewWeighted(authLoadGateCapacity),
		deferredAuthContext:        deferredAuthContext,
		deferredAuthCancel:         deferredAuthCancel,
		refreshSemaphore:           make(chan struct{}, refreshMaxConcurrency),
		runtimeScope:               uuid.NewString(),
	}
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(&internalconfig.Config{})
	manager.apiKeyModelAlias.Store(apiKeyModelAliasTable(nil))
	if defaultInFlightConfig, errInFlightConfig := HomeInFlightPublisherConfigFromConfig(internalconfig.DefaultCredentialInFlightConfig()); errInFlightConfig == nil {
		manager.ApplyHomeInFlightPublisherConfig(defaultInFlightConfig)
	}
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

func isBuiltInSelector(selector Selector) bool {
	switch unwrapSchedulerSelector(selector).(type) {
	case *RoundRobinSelector, *WeightedRoundRobinSelector, *QuotaAwareSelector, *FillFirstSelector:
		return true
	default:
		return false
	}
}

func (m *Manager) syncScheduler() {
	if m == nil || m.scheduler == nil {
		return
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	auths := make([]*Auth, 0, len(m.auths))
	for _, auth := range m.auths {
		auths = append(auths, auth.Clone())
	}
	m.scheduler.rebuild(auths)
}

func (m *Manager) recordSchedulerRemovalTombstone(authID string, generation uint64, removedAt time.Time) {
	if m == nil || m.scheduler == nil {
		return
	}
	m.scheduler.recordRemovalTombstone(authID, generation, removedAt)
}

// RefreshSchedulerEntry republishes a single auth after its model registration changes.
// The registry projection and scheduler snapshot are built from the same auth state.
func (m *Manager) RefreshSchedulerEntry(authID string) {
	if m == nil || authID == "" {
		return
	}
	m.mu.Lock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.Unlock()
		return
	}
	snapshot := auth.Clone()
	projectionGeneration := m.nextAuthGenerationLocked()
	m.mu.Unlock()
	m.publishClientModelProjections(snapshot, projectionGeneration, time.Now())
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
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

func isSameSelector(left, right Selector) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftType := reflect.TypeOf(left)
	if leftType != reflect.TypeOf(right) || !leftType.Comparable() {
		return false
	}
	return left == right
}

func (m *Manager) SetSelector(selector Selector) {
	if m == nil {
		return
	}
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	m.selectorMu.Lock()
	defer m.selectorMu.Unlock()

	m.mu.Lock()
	previous := m.selector
	if isSameSelector(previous, selector) {
		m.mu.Unlock()
		return
	}
	m.selector = selector
	m.mu.Unlock()

	if previous != nil {
		if stoppable, ok := previous.(StoppableSelector); ok {
			stoppable.Stop()
		}
	}
	if m.scheduler != nil {
		m.scheduler.setSelector(selector)
		m.syncScheduler()
	}
}

// Selector returns the current credential selector.
func (m *Manager) Selector() Selector {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.selector
}

// LookupSessionAffinity observes the current session binding without selecting or refreshing it.
func (m *Manager) LookupSessionAffinity(provider, model, sessionID string) (*Auth, string) {
	if m == nil {
		return nil, "unsupported"
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	m.mu.RLock()
	if m.pluginScheduler != nil {
		m.mu.RUnlock()
		return nil, "unsupported"
	}
	selector := m.selector
	authProviders := make(map[string]string, len(m.auths))
	for authID, auth := range m.auths {
		if auth != nil {
			authProviders[authID] = strings.ToLower(strings.TrimSpace(auth.Provider))
		}
	}
	m.mu.RUnlock()
	if selector == nil {
		return nil, "unsupported"
	}
	authFilter := func(authID string) bool {
		if provider == "mixed" {
			_, ok := authProviders[authID]
			return ok
		}
		authProvider, ok := authProviders[authID]
		return ok && authProvider == provider
	}
	var authID, status string
	if observer, ok := selector.(interface {
		LookupAffinity(string, string, string, ...func(string) bool) (string, string)
	}); ok && observer != nil {
		authID, status = observer.LookupAffinity(provider, model, sessionID, authFilter)
	} else if observer, ok := selector.(interface {
		LookupAffinity(string, string, string) (string, string)
	}); ok && observer != nil {
		authID, status = observer.LookupAffinity(provider, model, sessionID)
	} else {
		return nil, "unsupported"
	}
	if status != "bound" || authID == "" {
		return nil, status
	}
	m.mu.RLock()
	auth := m.auths[authID]
	if auth == nil {
		m.mu.RUnlock()
		return nil, "unbound"
	}
	snapshot := auth.Clone()
	m.mu.RUnlock()
	if provider != "mixed" && !strings.EqualFold(snapshot.Provider, provider) {
		return nil, "unbound"
	}
	snapshot.EnsureIndex()
	return snapshot, "bound"
}

// SetQuotaSchedulingProvider supplies read-only quota signals for quota-aware routing.
func (m *Manager) SetQuotaSchedulingProvider(provider QuotaSchedulingProvider) {
	if m == nil || m.scheduler == nil {
		return
	}
	m.scheduler.setQuotaSchedulingProvider(provider)
}

// StartSchedulerCheckpoint restores and starts asynchronous scheduler progress persistence.
func (m *Manager) StartSchedulerCheckpoint(ctx context.Context, store SchedulerCheckpointStore) error {
	if m == nil || m.scheduler == nil {
		return nil
	}
	return m.scheduler.startCheckpoint(ctx, store)
}

// StopSchedulerCheckpoint stops asynchronous persistence and saves the final scheduler progress.
func (m *Manager) StopSchedulerCheckpoint(ctx context.Context) error {
	if m == nil || m.scheduler == nil {
		return nil
	}
	return m.scheduler.stopCheckpoint(ctx)
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

func (m *Manager) withHomeRefreshClient(ctx context.Context) context.Context {
	configuredClient, configured := m.configuredHomeClient()
	var client HomeRefreshClient
	if configured {
		client = configuredClient
	} else {
		client = currentHomeRuntimeRefreshClient()
	}
	ctx = WithHomeRefreshClient(ctx, client)
	kvClient, _ := client.(home.KVClient)
	scopeEnabled := m.HomeEnabled() || configuredClient != nil || (!configured && client != nil)
	return home.WithClientScope(
		ctx, home.ClientScope{
			Client:  kvClient,
			ID:      m.runtimeScope,
			Enabled: scopeEnabled,
		},
	)
}

// SetRefreshBindingObserver notifies the host after a refreshed credential changes its binding.
// The callback runs after credential publication and the per-auth mutation lock is released.
func (m *Manager) SetRefreshBindingObserver(observer func(before, after *Auth)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.refreshBindingObserver = observer
	m.mu.Unlock()
}

// SetConfig updates the runtime config snapshot used by request-time helpers.
// Callers should provide the latest config on reload so per-credential alias mapping stays in sync.
func (m *Manager) SetConfig(cfg *config.Config) {
	if m == nil {
		return
	}
	m.configCooldownMu.Lock()
	defer m.configCooldownMu.Unlock()
	if m.setConfigSnapshotLocked(cfg) {
		m.mu.RLock()
		store := m.cooldownStore
		m.mu.RUnlock()
		persistCtx, cancel := newCooldownStatePersistenceContext(context.Background())
		defer cancel()
		m.persistCooldownStatesToLocked(persistCtx, store)
	}
}

func (m *Manager) setConfigSnapshotLocked(cfg *internalconfig.Config) bool {
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	previousCfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if homeSessionAliasTTL(previousCfg) != homeSessionAliasTTL(cfg) {
		m.homeSessionAliases.clear()
	}
	m.runtimeConfig.Store(cfg)
	clearedCooldowns := m.clearDisabledCooldownStates(cfg)
	m.mu.Lock()
	m.configureRefreshSemaphoreLocked(cfg)
	m.mu.Unlock()
	if !cfg.Home.Enabled {
		m.clearHomeRuntimeAuths()
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	return clearedCooldowns
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
	_, candidates := modelAliasLookupCandidates(requestedModel)
	for _, candidate := range candidates {
		if resolved := strings.TrimSpace(byAlias[strings.ToLower(candidate)]); resolved != "" {
			return thinking.PreserveRequestModelDecorations(resolved, requestedModel)
		}
	}
	return ""

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
		case "xai":
			if entry := resolveXAIAPIKeyConfig(cfg, auth); entry != nil {
				compileAPIKeyModelAliasForModels(byAlias, entry.Models)
			}
		case "meta":
			if entry := resolveAPIKeyConfig(cfg.MetaKey, auth); entry != nil {
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
		aliasKey := strings.ToLower(alias)
		// Config priority: first alias wins.
		if _, exists := out[aliasKey]; exists {
			continue
		}
		out[aliasKey] = name
		// Also allow direct lookup by upstream name (case-insensitive), so lookups on already-upstream
		// models remain a cheap no-op.
		nameKey := strings.ToLower(name)
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
	m.registerExecutorIfAuthCurrent(context.Background(), "", 0, executor)
}

// RegisterExecutorIfAuthCurrent publishes an executor only while authID still owns generation.
func (m *Manager) RegisterExecutorIfAuthCurrent(authID string, generation uint64, executor ProviderExecutor) bool {
	return m.registerExecutorIfAuthCurrent(context.Background(), strings.TrimSpace(authID), generation, executor)
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
	maxDelay := max(deferredAuthRecreateRetryMaxDelay, delay)
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
		m.recordDeferredAuthStopError(
			errors.Join(
				errAuthDurableStateUncertain,
				fmt.Errorf("deferred auth recreation stopped for %s", strings.TrimSpace(id)),
			),
		)
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
	NormalizeCredentialMetadata(auth.Metadata)
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return nil, errWeight
	}
	if _, _, errProfile := ClaudeFingerprintProfile(auth); errProfile != nil {
		return nil, errProfile
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
		return nil, fmt.Errorf(
			"ignored watcher mutation read before credential %s was removed", strings.TrimSpace(auth.ID),
		)
	}
	if checkWatcherReadTime && m.staleRefreshPersistenceWatcherMutation(auth) {
		unlockMutation()
		return nil, fmt.Errorf("ignored watcher replay of unpersisted refreshed credential %s", auth.ID)
	}
	m.cancelPendingRefreshPersistence(auth.ID)
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
	snapshot.credentialGeneration = snapshot.Generation
	m.auths[auth.ID] = snapshot
	m.notifyAuthChangeLocked(auth.ID)
	published := snapshot.Clone()
	if allowRecreate && removedAtStart {
		m.clearAuthRemovalLocked(auth.ID)
	}
	m.mu.Unlock()
	m.authIntentMu.Unlock()
	m.completeDirectAuthMutation(ctx, auth.ID)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(published.Clone())
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	unlockMutation()
	mutationLocked = false
	m.hook.OnAuthRegistered(ctx, published.Clone())
	return published.Clone(), nil
}

// Update applies persistent provider-owned fields and preserves the latest
// runtime quota and lifecycle state.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	NormalizeCredentialMetadata(auth.Metadata)
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return nil, errWeight
	}
	if _, _, errProfile := ClaudeFingerprintProfile(auth); errProfile != nil {
		return nil, errProfile
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
		return nil, fmt.Errorf(
			"ignored watcher mutation read before credential %s was removed", strings.TrimSpace(auth.ID),
		)
	}
	if checkWatcherReadTime && m.staleRefreshPersistenceWatcherMutation(auth) {
		unlockMutation()
		return nil, fmt.Errorf("ignored watcher replay of unpersisted refreshed credential %s", auth.ID)
	}
	m.cancelPendingRefreshPersistence(auth.ID)
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
	m.cancelPendingRefreshPersistence(id)
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
	currentBeforeMutation := m.auths[id]
	currentRefreshBeforeMutation := time.Time{}
	if currentBeforeMutation != nil {
		currentRefreshBeforeMutation = currentBeforeMutation.NextRefreshAfter
	}
	m.mu.RUnlock()
	registered = currentBeforeMutation == nil
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
	NormalizeCredentialMetadata(next.Metadata)
	if strings.TrimSpace(next.ID) != id {
		return nil, fmt.Errorf("persistent auth mutation returned auth %q, want %q", next.ID, id)
	}
	nextRefreshBeforePublication := next.NextRefreshAfter
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
	bindingChanged := false
	var previousBinding *Auth
	current := m.auths[id]
	if current != nil {
		previousBinding = current.Clone()
		next, bindingChanged = mergePersistentAuthState(current, next)
	} else {
		next = next.Clone()
	}
	next.EnsureIndex()
	snapshot := next.Clone()
	m.assignAuthGenerationLocked(snapshot)
	if current != nil && current == currentBeforeMutation && !bindingChanged &&
		nextRefreshBeforePublication.Equal(currentRefreshBeforeMutation) &&
		!current.NextRefreshAfter.Equal(currentRefreshBeforeMutation) {
		snapshot.NextRefreshAfter = current.NextRefreshAfter
	}
	m.auths[id] = snapshot
	m.notifyAuthChangeLocked(id)
	published := snapshot.Clone()
	if persisted {
		m.persistedAuthVersions.Delete(id)
		if version := persistedVersionFromAuthAttributes(snapshot); version != nil {
			m.persistedAuthVersions.Store(id, *version)
		}
	}
	if bindingChanged {
		m.clearHomeRuntimeAuthBindingLocked(id)
	}
	if allowRecreate && removedAtStart {
		m.clearAuthRemovalLocked(id)
	}
	m.mu.Unlock()
	m.authIntentMu.Unlock()
	m.completeDirectAuthMutation(publicationCtx, id)

	if m.scheduler != nil {
		m.scheduler.upsertAuth(published.Clone())
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if bindingChanged {
		m.completeAuthBindingChange(publicationCtx, previousBinding, published)
	}
	return published.Clone(), nil
}

func (m *Manager) updateAuthLocked(
	ctx context.Context,
	auth *Auth,
	preserveRuntime bool,
) (*Auth, error) {
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return nil, errWeight
	}
	if _, _, errProfile := ClaudeFingerprintProfile(auth); errProfile != nil {
		return nil, errProfile
	}
	if ctx == nil {
		ctx = context.Background()
	}
	allowRecreate := shouldAllowAuthRecreate(ctx)
	removedAtStart := false
	var removedSeq uint64
	m.mu.Lock()
	existing, ok := m.auths[auth.ID]
	next := auth.Clone()
	bindingChanged := false
	var previousBinding *Auth
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
		previousBinding = existing.Clone()
		next, bindingChanged = mergePersistentAuthState(existing, auth)
	} else {
		if !next.indexAssigned && next.Index == "" {
			next.Index = existing.Index
			next.indexAssigned = existing.indexAssigned
		}
		next.Success = existing.Success
		next.Failed = existing.Failed
		next.recentRequests = existing.recentRequests
	}
	clearedCooldown := false
	if m.cooldownDisabledForAuth(next) || next.Disabled || next.Status == StatusDisabled {
		clearedCooldown = clearCooldownStateForAuth(next, time.Now())
	}
	next.EnsureIndex()
	refreshBeforeSave := next.NextRefreshAfter
	currentRefreshBeforeSave := time.Time{}
	if existing != nil {
		currentRefreshBeforeSave = existing.NextRefreshAfter
	}
	m.mu.Unlock()
	persisted, errPersist := m.persist(ctx, next, allowRecreate, removedAtStart, removedSeq)
	if errPersist != nil {
		return nil, errPersist
	}
	publicationCtx := ctx
	if persisted && shouldCompleteDurableAuthPublication(ctx) {
		publicationCtx = context.WithoutCancel(ctx)
	}
	if errPublication := m.authPublicationContextError(publicationCtx, auth.ID, persisted); errPublication != nil {
		return nil, errPublication
	}
	snapshot := next.Clone()
	m.authIntentMu.Lock()
	m.mu.Lock()
	if errPublication := m.authPublicationContextErrorLocked(publicationCtx, auth.ID, persisted); errPublication != nil {
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
	if existing != nil && m.auths[auth.ID] == existing && !bindingChanged &&
		next.NextRefreshAfter.Equal(refreshBeforeSave) &&
		!existing.NextRefreshAfter.Equal(currentRefreshBeforeSave) {
		snapshot.NextRefreshAfter = existing.NextRefreshAfter
	}
	m.auths[auth.ID] = snapshot
	m.notifyAuthChangeLocked(auth.ID)
	published := snapshot.Clone()
	if bindingChanged {
		m.clearHomeRuntimeAuthBindingLocked(auth.ID)
	}
	if allowRecreate && removedAtStart {
		m.clearAuthRemovalLocked(auth.ID)
	}
	m.mu.Unlock()
	m.authIntentMu.Unlock()
	m.completeDirectAuthMutation(publicationCtx, auth.ID)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(published.Clone())
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if bindingChanged {
		m.completeAuthBindingChange(publicationCtx, previousBinding, published)
	} else if clearedCooldown {
		m.persistCooldownStatesAfterMutation(publicationCtx)
	}
	return published.Clone(), nil
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
	NormalizeCredentialMetadata(update.Metadata)
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = m.prepareDirectAuthMutation(ctx, expected.ID)

	unlockMutation, errMutation := m.lockAuthMutation(ctx, expected.ID)
	if errMutation != nil {
		return nil, errMutation
	}
	m.cancelPendingRefreshPersistence(expected.ID)
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
	previousBinding := current.Clone()
	currentBeforeSave := current
	next, bindingChanged := mergePersistentAuthState(current, update)
	currentRefreshBeforeSave := current.NextRefreshAfter
	refreshBeforeSave := next.NextRefreshAfter
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
	publicationCtx, cancelPublication := context.WithTimeout(
		context.WithoutCancel(ctx),
		refreshPersistenceCommitTimeout,
	)
	defer cancelPublication()
	persisted, errPersist := m.persist(publicationCtx, next, false, false, 0)
	if errPersist != nil {
		return nil, errPersist
	}
	if errPublication := m.authPublicationContextError(publicationCtx, expected.ID, persisted); errPublication != nil {
		return nil, errPublication
	}

	snapshot := next.Clone()
	m.authIntentMu.Lock()
	m.mu.Lock()
	if errPublication := m.authPublicationContextErrorLocked(
		publicationCtx, expected.ID, persisted,
	); errPublication != nil {
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
	if current == currentBeforeSave && !bindingChanged &&
		next.NextRefreshAfter.Equal(refreshBeforeSave) &&
		!current.NextRefreshAfter.Equal(currentRefreshBeforeSave) {
		snapshot.NextRefreshAfter = current.NextRefreshAfter
	}
	m.auths[expected.ID] = snapshot
	m.notifyAuthChangeLocked(expected.ID)
	published := snapshot.Clone()
	if bindingChanged {
		m.clearHomeRuntimeAuthBindingLocked(expected.ID)
	}
	m.mu.Unlock()
	m.authIntentMu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(published.Clone())
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	if bindingChanged {
		m.completeAuthBindingChange(publicationCtx, previousBinding, published)
	}
	return published.Clone(), nil
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

func persistentAuthEndpointChanged(current, update *Auth) bool {
	if current == nil || update == nil {
		return current != update
	}
	if !strings.EqualFold(strings.TrimSpace(current.Provider), strings.TrimSpace(update.Provider)) ||
		executorKeyFromAuth(current) != executorKeyFromAuth(update) {
		return true
	}
	currentBaseURL, updateBaseURL := "", ""
	if current.Attributes != nil {
		currentBaseURL = strings.TrimSpace(current.Attributes["base_url"])
	}
	if update.Attributes != nil {
		updateBaseURL = strings.TrimSpace(update.Attributes["base_url"])
	}
	return currentBaseURL != updateBaseURL
}

func persistentAuthBindingChanged(current, update *Auth) bool {
	if persistentAuthEndpointChanged(current, update) {
		return true
	}
	currentBinding := credentialBindingSHA256(current)
	updateBinding := credentialBindingSHA256(update)
	if currentBinding != updateBinding {
		return true
	}
	currentRefreshOwner := authRefreshOwnerToken(current)
	updateRefreshOwner := authRefreshOwnerToken(update)
	if currentRefreshOwner != "" || updateRefreshOwner != "" {
		return currentRefreshOwner != updateRefreshOwner
	}
	currentAccessToken := AccessTokenSHA256(current)
	updateAccessToken := AccessTokenSHA256(update)
	if currentAccessToken != "" || updateAccessToken != "" {
		return currentAccessToken != updateAccessToken
	}
	return currentBinding == "" && !reflect.DeepEqual(current.Storage, update.Storage)
}

func refreshedAuthBindingChanged(current, update *Auth) bool {
	if persistentAuthEndpointChanged(current, update) {
		return true
	}
	if current == nil || update == nil || !strings.EqualFold(strings.TrimSpace(current.Provider), "codex") {
		return false
	}
	currentAccountID, currentPlanType := codexBindingClaims(credentialBindingMetadata(current))
	updateAccountID, updatePlanType := codexBindingClaims(credentialBindingMetadata(update))
	if currentAccountID == "" && updateAccountID == "" && currentPlanType == "" && updatePlanType == "" {
		return false
	}
	return currentAccountID != updateAccountID || currentPlanType != updatePlanType
}

func mergePersistentAuthState(current, update *Auth) (*Auth, bool) {
	return mergePersistentAuthStateWithBinding(current, update, persistentAuthBindingChanged(current, update))
}

func mergeRefreshedAuthState(current, update *Auth) (*Auth, bool) {
	bindingChanged := refreshedAuthBindingChanged(current, update)
	if !bindingChanged {
		return mergePersistentAuthStateWithBinding(current, update, false)
	}
	persistent := update.Clone()
	persistent.Status = StatusActive
	if persistent.Disabled {
		persistent.Status = StatusDisabled
	}
	persistent.StatusMessage = ""
	persistent.Unavailable = false
	persistent.NextRetryAfter = time.Time{}
	persistent.Quota = QuotaState{}
	persistent.LastError = nil
	persistent.ModelStates = nil
	persistent.StateGeneration = 0
	persistent.StateEffectScope = ""
	persistent.StateEffectKey = ""
	persistent.ResetAuthority = ""
	persistent.Runtime = nil
	persistent.RefreshCommit = nil
	persistent.LastRefreshedAt = time.Time{}
	return mergePersistentAuthStateWithBinding(current, persistent, true)
}

func mergePersistentAuthStateWithBinding(current, update *Auth, bindingChanged bool) (*Auth, bool) {
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
	if persistentAuthEndpointChanged(current, next) || current.ProxyURL != next.ProxyURL ||
		next.rejectedAccessTokenSHA256 != AccessTokenSHA256(next) {
		next.rejectedAccessTokenSHA256 = ""
	}
	if bindingChanged || !hasTerminalUnauthorizedAuthFailure(current) {
		next.NextRefreshAfter = persistent.NextRefreshAfter
	}
	if bindingChanged {
		next.Status = persistent.Status
		next.StatusMessage = persistent.StatusMessage
		next.Unavailable = persistent.Unavailable
		next.NextRetryAfter = persistent.NextRetryAfter
		next.Quota = persistent.Quota
		next.LastError = persistent.LastError
		next.ModelStates = persistent.ModelStates
		next.StateGeneration = persistent.StateGeneration
		next.StateEffectScope = persistent.StateEffectScope
		next.StateEffectKey = persistent.StateEffectKey
		next.ResetAuthority = persistent.ResetAuthority
		next.Runtime = persistent.Runtime
		next.RefreshCommit = persistent.RefreshCommit
		next.RefreshPersistence = persistent.RefreshPersistence
		next.executorKey = persistent.executorKey
		next.LastRefreshedAt = persistent.LastRefreshedAt
	} else {
		if persistent.Runtime != nil {
			next.Runtime = persistent.Runtime
		}
		if !persistent.LastRefreshedAt.IsZero() {
			next.LastRefreshedAt = persistent.LastRefreshedAt
		}
		if persistent.Disabled != current.Disabled {
			next.Status = persistent.Status
			next.StatusMessage = persistent.StatusMessage
			next.Unavailable = persistent.Unavailable
		}
		if hasTerminalUnauthorizedAuthFailure(current) {
			next.Unavailable = true
			next.StatusMessage = current.StatusMessage
			next.Status = StatusError
			if next.Disabled {
				next.Status = StatusDisabled
			}
		}
	}
	next.UpdatedAt = time.Now().UTC()
	return next, bindingChanged
}

func (m *Manager) clearHomeRuntimeAuthBindingLocked(authID string) {
	authID = strings.TrimSpace(authID)
	if m == nil || authID == "" {
		return
	}
	for sessionID, sessionAuths := range m.homeRuntimeAuths {
		delete(sessionAuths, authID)
		if len(sessionAuths) == 0 {
			delete(m.homeRuntimeAuths, sessionID)
		}
	}
}

func (m *Manager) completeAuthBindingChange(ctx context.Context, before, after *Auth) {
	if m == nil || after == nil {
		return
	}
	authID := strings.TrimSpace(after.ID)
	if authID == "" {
		return
	}
	m.mu.Lock()
	m.clearHomeRuntimeAuthBindingLocked(authID)
	m.mu.Unlock()
	m.invalidateSessionAffinity(authID)
	m.publishClientModelProjections(after, after.Generation, time.Now())

	providers := []string{executorKeyFromAuth(before), executorKeyFromAuth(after)}
	for _, provider := range dedupeStrings(providers) {
		if exec, ok := m.Executor(provider); ok && exec != nil {
			if closer, okCloser := exec.(ExecutionSessionAuthCloser); okCloser {
				closer.CloseExecutionSessionsForAuth(authID)
			} else if closer, okCloser := exec.(ExecutionSessionCloser); okCloser {
				closer.CloseExecutionSession(CloseAllExecutionSessionsID)
			}
		}
	}
	m.persistCooldownStatesAfterMutation(ctx)
}

// ResetQuota clears quota state for an auth and resumes registry routing.
func (m *Manager) ResetQuota(ctx context.Context, authID string) (*Auth, []string, error) {
	var expected *Auth
	if ctx != nil {
		var scoped bool
		expected, scoped = ctx.Value(quotaResetCredentialKey{}).(*Auth)
		if scoped && expected == nil {
			return nil, nil, fmt.Errorf("expected quota reset credential is required")
		}
	}
	return m.resetRoutingCooldown(ctx, authID, false, expected)
}

type quotaResetCredentialKey struct{}

// WithQuotaResetCredential 将本次操作的凭证快照绑定到本地额度清理
func WithQuotaResetCredential(ctx context.Context, expected *Auth) context.Context {
	return context.WithValue(ctx, quotaResetCredentialKey{}, expected.Clone())
}

// ResetCooldown clears local quota and timed retry restrictions without changing upstream quota.
func (m *Manager) ResetCooldown(ctx context.Context, authID string) (*Auth, []string, error) {
	return m.resetRoutingCooldown(ctx, authID, true, nil)
}

func (m *Manager) resetRoutingCooldown(ctx context.Context, authID string, includeTransient bool, expected *Auth) (*Auth, []string, error) {
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
	if expected != nil && (auth.Generation != expected.Generation || !persistentAuthStateMatches(expected, auth)) {
		m.mu.RUnlock()
		return nil, nil, fmt.Errorf("credential changed before local quota reset")
	}
	updated := auth.Clone()
	m.mu.RUnlock()

	hadAuthCooldown := hasQuotaCooldownState(updated.Quota, updated.LastError)
	clearCredentialCooldown := false
	if includeTransient && !updated.Disabled && updated.Status != StatusDisabled {
		clearCredentialCooldown = updated.StateEffectScope == cliproxyexecutor.StateEffectScopeCredential && hadAuthCooldown
		for _, view := range CooldownSnapshotForAuth(updated, now) {
			if view.Scope == "credential" {
				hadAuthCooldown = true
				clearCredentialCooldown = true
				break
			}
		}
	}
	for modelKey, state := range updated.ModelStates {
		modelKey = strings.TrimSpace(modelKey)
		if modelKey == "" || state == nil || includeTransient && state.Status == StatusDisabled {
			continue
		}
		quotaBlocked := hasQuotaCooldownState(state.Quota, state.LastError)
		blocked, _, retryAt := cooldownViewBlock(state.Unavailable, state.Quota.Exceeded,
			state.NextRetryAfter, state.Quota.NextRecoverAt, now)
		if !quotaBlocked && !(includeTransient && blocked && retryAt.After(now)) {
			continue
		}
		models = append(models, modelKey)
		state.QuotaEpoch++
		clearModelState(state, now, StateResetAuthorityOperator)
	}
	if len(models) == 0 && !hadAuthCooldown {
		return updated, nil, nil
	}
	wasDisabled := updated.Disabled
	previousStatus := updated.Status
	if clearCredentialCooldown && hasModelError(updated, now) {
		clearAuthState(updated, now, StateResetAuthorityOperator)
	}
	updateAggregatedAvailability(updated, now)
	if hasModelError(updated, now) {
		updateAuthErrorFromModelStates(updated, now)
	} else {
		clearAuthState(updated, now, StateResetAuthorityOperator)
	}
	if wasDisabled || previousStatus == StatusDisabled {
		updated.Disabled = wasDisabled
		updated.Status = previousStatus
	}
	if len(models) == 0 && hadAuthCooldown {
		for _, modelKey := range modelsForRegisteredAuth(authID) {
			state, _ := findModelState(updated, modelKey)
			if modelHasNonQuotaBlock(state, now) {
				continue
			}
			models = append(models, modelKey)
		}
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

	m.publishClientModelProjections(snapshot, snapshot.Generation, now)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot.Clone())
	}
	m.persistCooldownStatesAfterMutation(ctx)
	unlockMutation()
	mutationLocked = false
	m.hook.OnAuthUpdated(ctx, snapshot.Clone())
	return snapshot, models, nil
}

// RecoverCredentialQuota clears only credential-scoped quota state after an authoritative usage refresh.
func (m *Manager) RecoverCredentialQuota(ctx context.Context, authID string) (*Auth, error) {
	return m.RecoverQuotaAfterRefresh(ctx, authID, time.Time{}, nil)
}

// RecoverQuotaAfterRefresh conditionally clears fresh, authoritative quota state after a provider usage refresh.
func (m *Manager) RecoverQuotaAfterRefresh(
	ctx context.Context,
	authID string,
	refreshStartedAt time.Time,
	families []QuotaFamilyRecoveryCandidate,
) (*Auth, error) {
	if m == nil {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil, fmt.Errorf("auth id is required")
	}
	unlockMutation, errMutation := m.lockAuthMutation(ctx, authID)
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
	auth := m.auths[authID]
	if auth == nil {
		m.mu.RUnlock()
		return nil, nil
	}
	updated := auth.Clone()
	m.mu.RUnlock()

	now := time.Now()
	wasDisabled := updated.Disabled
	previousStatus := updated.Status
	changed := false
	if !wasDisabled && previousStatus != StatusDisabled && !refreshStartedAt.IsZero() {
		refreshStartedAt = refreshStartedAt.UTC()
		for _, candidate := range families {
			if recoverQuotaFamilyAfterRefresh(updated, candidate, refreshStartedAt, now) {
				changed = true
			}
		}
	}
	if hasCredentialQuotaCooldownState(updated) &&
		clearAuthState(updated, now, StateResetAuthoritySuccess) {
		refreshAuthFromModelStates(updated, now)
		changed = true
	}
	if wasDisabled || previousStatus == StatusDisabled {
		updated.Disabled = wasDisabled
		updated.Status = previousStatus
	}
	if !changed {
		return updated, nil
	}

	persisted, errPersist := m.persist(ctx, updated, false, false, 0)
	if errPersist != nil {
		return nil, errPersist
	}
	if errPublication := m.authPublicationContextError(ctx, authID, persisted); errPublication != nil {
		return nil, errPublication
	}

	snapshot := updated.Clone()
	m.authIntentMu.Lock()
	m.mu.Lock()
	if errPublication := m.authPublicationContextErrorLocked(ctx, authID, persisted); errPublication != nil {
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		return nil, errPublication
	}
	current := m.auths[authID]
	_, removed := m.removedAuths[authID]
	if current == nil || removed {
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		if persisted && removed {
			return nil, m.scheduleRejectedPersistedAuthDeletion(authID, updated)
		}
		return nil, nil
	}
	m.assignAuthGenerationLocked(snapshot)
	m.auths[authID] = snapshot.Clone()
	m.mu.Unlock()
	m.authIntentMu.Unlock()

	m.publishClientModelProjections(snapshot, snapshot.Generation, now)
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot.Clone())
	}
	m.persistCooldownStatesAfterMutation(ctx)
	unlockMutation()
	mutationLocked = false
	m.hook.OnAuthUpdated(ctx, snapshot.Clone())
	return snapshot, nil
}

func recoverQuotaFamilyAfterRefresh(
	auth *Auth,
	candidate QuotaFamilyRecoveryCandidate,
	refreshStartedAt,
	now time.Time,
) bool {
	family := strings.TrimSpace(candidate.Family)
	if auth == nil || family == "" || candidate.ExpectedGeneration == 0 || refreshStartedAt.IsZero() ||
		quotaFamilyGeneration(auth, family) != candidate.ExpectedGeneration {
		return false
	}
	models := make(map[string]struct{}, len(candidate.Models))
	for _, model := range candidate.Models {
		model = strings.TrimSpace(model)
		if model != "" {
			models[model] = struct{}{}
		}
	}
	states := quotaFamilyModelStates(auth, "", family, false)
	if len(states) == 0 || len(states) != len(models) {
		return false
	}
	firstModel := ""
	for model, state := range states {
		if _, expected := models[model]; !expected ||
			!quotaFamilyStateAllowsRefreshRecovery(model, state, family) ||
			state.UpdatedAt.After(refreshStartedAt) {
			return false
		}
		if firstModel == "" || model < firstModel {
			firstModel = model
		}
	}
	if firstModel == "" {
		return false
	}
	applyQuotaFamilySuccess(
		auth,
		Result{
			Model:   firstModel,
			Success: true,
			Disposition: cliproxyexecutor.ExecutionDisposition{
				EffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
				EffectKey:   family,
			},
		},
		now,
	)
	for _, state := range states {
		state.QuotaEpoch++
	}
	return true
}

func hasCredentialQuotaCooldownState(auth *Auth) bool {
	if auth == nil || !hasQuotaCooldownState(auth.Quota, auth.LastError) {
		return false
	}
	switch auth.StateEffectScope {
	case cliproxyexecutor.StateEffectScopeCredential:
		return true
	case cliproxyexecutor.StateEffectScopeExactModel,
		cliproxyexecutor.StateEffectScopeQuotaFamily,
		cliproxyexecutor.StateEffectScopeGroup,
		cliproxyexecutor.StateEffectScopeProvider,
		cliproxyexecutor.StateEffectScopeRequest:
		return false
	}
	for _, state := range auth.ModelStates {
		if state != nil && hasQuotaCooldownState(state.Quota, state.LastError) {
			return false
		}
	}
	return true
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

func clientModelProjectionForAuth(auth *Auth, routeModel string, now time.Time) registry.ClientModelProjection {
	routeModel = strings.TrimSpace(routeModel)
	projection := registry.ClientModelProjection{ModelID: routeModel}
	if auth == nil || routeModel == "" {
		return projection
	}

	state, _ := findModelState(auth, routeModel)
	projection.Suspended, _, _ = isAuthBlockedForModel(auth, routeModel, now)
	if state != nil {
		projection.QuotaExceeded = state.Unavailable && state.Quota.Exceeded &&
			(state.Quota.NextRecoverAt.IsZero() || state.Quota.NextRecoverAt.After(now))
		if projection.Suspended {
			projection.SuspendReason = cooldownReason(state.StatusMessage, state.Quota, state.LastError)
		}
	}
	if projection.Suspended && projection.SuspendReason == "" {
		projection.SuspendReason = cooldownReason(auth.StatusMessage, auth.Quota, auth.LastError)
	}
	return projection
}

func (m *Manager) publishClientModelProjections(auth *Auth, generation uint64, now time.Time) {
	if m == nil || auth == nil || strings.TrimSpace(auth.ID) == "" || generation == 0 {
		return
	}
	reg := registry.GetGlobalRegistry()
	models, epoch := reg.GetModelsAndEpochForClient(auth.ID)
	projections := make([]registry.ClientModelProjection, 0, len(models))
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		projections = append(projections, clientModelProjectionForAuth(auth, model.ID, now))
	}
	if len(projections) == 0 {
		return
	}
	reg.ApplyClientModelProjections(auth.ID, epoch, generation, projections)
}

func hasQuotaCooldownState(quota QuotaState, lastError *Error) bool {
	return quota.Exceeded || lastError != nil && lastError.HTTPStatus == http.StatusTooManyRequests
}

func modelHasNonQuotaBlock(state *ModelState, now time.Time) bool {
	if state == nil || hasQuotaCooldownState(state.Quota, state.LastError) {
		return false
	}
	if state.Status == StatusDisabled || state.Status == StatusError || state.LastError != nil {
		return true
	}
	return state.Unavailable && (state.NextRetryAfter.IsZero() || state.NextRetryAfter.After(now))
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
	m.notifyAuthChangeLocked(id)
	removalGeneration := m.nextAuthGenerationLocked()
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

	m.recordSchedulerRemovalTombstone(id, removalGeneration, removalTime)
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	m.invalidateSessionAffinity(id)
	if provider != "" {
		if exec, ok := m.Executor(provider); ok && exec != nil {
			if closer, okCloser := exec.(ExecutionSessionCloser); okCloser {
				closer.CloseExecutionSession(CloseAllExecutionSessionsID)
			}
		}
	}
	m.persistCooldownStatesAfterMutation(ctx)
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
		m.notifyAuthChangeLocked(id)
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
	initialRemovalGeneration := m.nextAuthGenerationLocked()
	m.mu.Unlock()
	m.recordSchedulerRemovalTombstone(id, initialRemovalGeneration, removalTime)
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
		m.notifyAuthChangeLocked(id)
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
	removalGeneration := m.nextAuthGenerationLocked()
	m.mu.Unlock()
	m.recordSchedulerRemovalTombstone(id, removalGeneration, removalTime)

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
	m.invalidateSessionAffinity(id)

	if provider != "" {
		if exec, ok := m.Executor(provider); ok && exec != nil {
			if closer, okCloser := exec.(ExecutionSessionCloser); okCloser {
				closer.CloseExecutionSession(CloseAllExecutionSessionsID)
			}
		}
	}
	m.persistCooldownStatesAfterMutation(ctx)
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
		m.recordDeferredAuthStopError(
			errors.Join(
				errAuthDurableStateUncertain,
				fmt.Errorf("could not schedule deletion for rejected credential %s", strings.TrimSpace(id)),
			),
		)
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

func (m *Manager) staleRefreshPersistenceWatcherMutation(auth *Auth) bool {
	if m == nil || auth == nil {
		return false
	}
	version := persistedVersionFromAuthAttributes(auth)
	if version == nil {
		return false
	}
	value, ok := m.pendingRefreshPersistenceRetries.Load(strings.TrimSpace(auth.ID))
	if !ok {
		return false
	}
	intent, ok := value.(*authRefreshPersistenceIntent)
	if !ok || intent == nil || intent.expectedVersion == nil || intent.ctx.Err() != nil ||
		filepath.Clean(version.Path) != filepath.Clean(intent.expectedVersion.Path) ||
		version.SHA256 != intent.expectedVersion.SHA256 {
		return false
	}
	m.mu.RLock()
	current := m.auths[auth.ID]
	matches := current != nil && current.Generation == intent.generation
	m.mu.RUnlock()
	return matches
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
		if parsed, err := time.Parse(
			time.RFC3339Nano, strings.TrimSpace(auth.Attributes[PersistedReadAtAttribute]),
		); err == nil {
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

func persistedVersionFromAuthAttributes(auth *Auth) *PersistedVersion {
	if auth == nil || auth.Attributes == nil {
		return nil
	}
	digestText := strings.TrimSpace(auth.Attributes[PersistedSHA256Attribute])
	if digestText == "" {
		return nil
	}
	path := strings.TrimSpace(auth.Attributes["path"])
	if path == "" {
		path = strings.TrimSpace(auth.Attributes["source"])
	}
	if path == "" {
		path = strings.TrimSpace(auth.FileName)
	}
	if path == "" {
		path = strings.TrimSpace(auth.ID)
	}
	digest, errDecode := hex.DecodeString(digestText)
	if errDecode != nil || len(digest) != sha256.Size {
		log.Warnf("credential %s has an invalid persisted file digest", auth.ID)
		return nil
	}
	version := PersistedVersion{Path: path}
	copy(version.SHA256[:], digest)
	return &version
}

func (m *Manager) persistedAuthVersionForRemoval(id string, expected *Auth) *PersistedVersion {
	if version := m.persistedAuthVersion(id); version != nil && strings.TrimSpace(version.Path) != "" {
		return version
	}
	if expected == nil {
		return nil
	}
	if version := persistedVersionFromAuthAttributes(expected); version != nil {
		return version
	}
	path := authPersistedPath(expected)
	if path == "" {
		path = strings.TrimSpace(expected.FileName)
	}
	if path == "" {
		path = strings.TrimSpace(id)
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
				m.recordDeferredAuthStopError(
					errors.Join(
						errAuthDurableStateUncertain,
						fmt.Errorf("deferred auth deletion stopped for %s", id),
					),
				)
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
				m.recordDeferredAuthStopError(
					errors.Join(
						errAuthDurableStateUncertain,
						fmt.Errorf("deferred auth deletion stopped for %s", id),
					),
				)
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
	if ctx == nil {
		ctx = context.Background()
	}
	if m.authLoadGate != nil {
		if err := m.authLoadGate.Acquire(ctx, authLoadGateCapacity); err != nil {
			return err
		}
		defer m.authLoadGate.Release(authLoadGateCapacity)
	}
	var unpersistedID string
	m.pendingRefreshPersistenceRetries.Range(func(key, value any) bool {
		id, _ := key.(string)
		intent, _ := value.(*authRefreshPersistenceIntent)
		if intent == nil || intent.ctx == nil || intent.ctx.Err() != nil {
			return true
		}
		m.mu.RLock()
		current := m.auths[id]
		active := current != nil && current.Generation == intent.generation
		m.mu.RUnlock()
		if active {
			unpersistedID = id
		}
		return !active
	})
	if unpersistedID != "" {
		return errors.Join(errAuthDurableStateUncertain,
			fmt.Errorf("credential %s has an unpersisted refresh", unpersistedID))
	}
	m.mu.RLock()
	store := m.store
	m.mu.RUnlock()
	if store == nil {
		return nil
	}
	items, err := store.List(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	previousAuths := m.auths
	m.auths = make(map[string]*Auth, len(items))
	loaded := make([]*Auth, 0, len(items))
	for _, auth := range items {
		if auth == nil || auth.ID == "" {
			continue
		}
		NormalizeCredentialMetadata(auth.Metadata)
		if errWeight := ValidateAuthWeight(auth); errWeight != nil {
			log.WithError(errWeight).Warnf("skip auth %s with invalid credential weight", auth.ID)
			continue
		}
		if _, _, errProfile := ClaudeFingerprintProfile(auth); errProfile != nil {
			log.WithError(errProfile).Warnf("skip auth %s with invalid fingerprint-profile", auth.ID)
			continue
		}
		if _, pendingDelete := m.pendingAuthDeletions[auth.ID]; pendingDelete {
			continue
		}
		delete(m.removedAuths, auth.ID)
		auth.EnsureIndex()
		snapshot := auth.Clone()
		m.assignAuthGenerationLocked(snapshot)
		if previous := previousAuths[auth.ID]; previous != nil && previous.credentialGeneration != 0 &&
			!persistentAuthBindingChanged(previous, snapshot) && previous.ProxyURL == snapshot.ProxyURL {
			snapshot.credentialGeneration = previous.credentialGeneration
		}
		m.auths[auth.ID] = snapshot
		loaded = append(loaded, snapshot.Clone())
	}
	if m.scheduler != nil {
		removalTime := time.Now()
		for authID := range previousAuths {
			if _, loadedAuth := m.auths[authID]; loadedAuth {
				continue
			}
			generation := m.nextAuthGenerationLocked()
			m.recordSchedulerRemovalTombstone(authID, generation, removalTime)
		}
		m.scheduler.rebuild(loaded)
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}
	m.rebuildAPIKeyModelAliasLocked(cfg)
	for authID := range m.authChangeWatchers {
		m.notifyAuthChangeLocked(authID)
	}
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
	ctx = cliproxyexecutor.WithRequestProxyURL(ctx, opts.ProxyURL)
	opts.CountTokens = false
	opts = ensureRouteTraceState(opts)
	req, opts = cliproxysession.Enrich(req, opts)
	if !util.HasExplicitSessionID(ctx) {
		ctx = util.WithInferredSessionID(ctx, ExtractSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata))
	}
	ctx = m.withHomeRefreshClient(ctx)
	opts = ensureRequestedModelMetadata(opts, req.Model)
	opts = bindRouteTraceRootMetadata(opts)
	resp, err := retryWithCooldown(
		m, ctx, providers, req, opts, func(
			innerCtx context.Context,
			innerProviders []string,
			innerReq cliproxyexecutor.Request,
			innerOpts cliproxyexecutor.Options,
			maxCreds int,
		) (cliproxyexecutor.Response, error) {
			if m.HomeEnabled() {
				return m.executeHome(innerCtx, innerProviders, innerReq, innerOpts, false)
			}
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
					resp, errExec := executor.Execute(
						execCtx, auth, execReq, externalRouteTraceOptions(execOpts),
					)
					if errExec == nil {
						rewriteForceMappedResponse(&resp, aliasResult)
					}
					return resp, errExec
				},
				m.MarkResult,
			)
		},
	)
	if err != nil && !isRequestStopError(err) && !m.HomeEnabled() &&
		hasAntigravityProvider(providers) && shouldAttemptAntigravityCreditsFallback(m, err) {
		if creditsResp, creditsErr, ok := m.tryAntigravityCreditsExecute(ctx, req, opts); creditsErr != nil {
			return cliproxyexecutor.Response{}, unwrapRequestScopedActionError(creditsErr)
		} else if ok {
			return creditsResp, nil
		}
	}
	return resp, unwrapRequestScopedActionError(err)
}

// ExecuteCount performs a token-counting execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteCount(
	ctx context.Context,
	providers []string,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	ctx = cliproxyexecutor.WithRequestProxyURL(ctx, opts.ProxyURL)
	opts.CountTokens = true
	opts = ensureRouteTraceState(opts)
	req, opts = cliproxysession.Enrich(req, opts)
	if !util.HasExplicitSessionID(ctx) {
		ctx = util.WithInferredSessionID(ctx, ExtractSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata))
	}
	ctx = m.withHomeRefreshClient(ctx)
	opts = ensureRequestedModelMetadata(opts, req.Model)
	opts = bindRouteTraceRootMetadata(opts)
	response, err := retryWithCooldown(
		m, ctx, providers, req, opts, func(
			innerCtx context.Context,
			innerProviders []string,
			innerReq cliproxyexecutor.Request,
			innerOpts cliproxyexecutor.Options,
			maxCreds int,
		) (cliproxyexecutor.Response, error) {
			if m.HomeEnabled() {
				return m.executeHome(innerCtx, innerProviders, innerReq, innerOpts, true)
			}
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
					return executor.CountTokens(
						execCtx, auth, execReq, externalRouteTraceOptions(execOpts),
					)
				},
				func(resultCtx context.Context, result Result) {
					result.SkipQuotaObservation = true
					if result.Success || result.Error != nil && !isForceCooldownResult(result) &&
						isCountTokensEndpointNotFoundError(result.Error, result.Model) {
						m.recordAvailabilityNeutralResult(resultCtx, result)
						return
					}
					m.MarkResult(resultCtx, result)
				},
			)
		},
	)
	return response, unwrapRequestScopedActionError(err)
}

func claudeOAuthRequestCancellation(ctx context.Context, auth *Auth, err error) error {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") ||
		!strings.EqualFold(strings.TrimSpace(auth.Attributes["auth_kind"]), "oauth") {
		return nil
	}
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// ExecuteStream performs a streaming execution using the configured selector and executor.
// It supports multiple providers for the same model and round-robins the starting provider per model.
func (m *Manager) ExecuteStream(
	ctx context.Context,
	providers []string,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	ctx = cliproxyexecutor.WithRequestProxyURL(ctx, opts.ProxyURL)
	opts.CountTokens = false
	opts = ensureRouteTraceState(opts)
	req, opts = cliproxysession.Enrich(req, opts)
	if !util.HasExplicitSessionID(ctx) {
		ctx = util.WithInferredSessionID(ctx, ExtractSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata))
	}
	ctx = m.withHomeRefreshClient(ctx)
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	opts = bindRouteTraceRootMetadata(opts)
	result, err := retryWithCooldown(
		m, ctx, providers, req, opts, func(
			innerCtx context.Context,
			innerProviders []string,
			innerReq cliproxyexecutor.Request,
			innerOpts cliproxyexecutor.Options,
			maxCreds int,
		) (*cliproxyexecutor.StreamResult, error) {
			if m.HomeEnabled() {
				return m.executeHomeStream(innerCtx, innerProviders, innerReq, innerOpts)
			}
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
		publicErr := unwrapRequestScopedActionError(err)
		if !isRequestStopError(err) && !m.HomeEnabled() &&
			hasAntigravityProvider(providers) && shouldAttemptAntigravityCreditsFallback(m, err) {
			if creditsResult, creditsErr, ok := m.tryAntigravityCreditsExecuteStream(
				ctx, req, opts,
			); creditsErr != nil {
				return nil, unwrapRequestScopedActionError(creditsErr)
			} else if ok {
				return creditsResult, nil
			}
		}
		if bootstrapErr, ok := errors.AsType[*streamBootstrapError](publicErr); ok {
			headers := bootstrapErr.Headers()
			if retryAfter := SafeResponseHeaders(err).Get("Retry-After"); retryAfter != "" {
				if headers == nil {
					headers = make(http.Header)
				}
				headers.Set("Retry-After", retryAfter)
			}
			return streamErrorResult(headers, publicErr), nil
		}
		return result, publicErr
	}
	return result, nil
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
	defaultRetry, maxRetryCredentials, maxWait := m.retrySettings()
	if ctx == nil {
		ctx = context.Background()
	}
	homeLimit, homeAuthority := -1, false
	var lastErr error
	traceProvider := "mixed"
	if len(normalized) == 1 {
		traceProvider = normalized[0]
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		state := &retryRoundState{round: attempt, defaultRetry: defaultRetry, maxCredentials: maxRetryCredentials, attempted: make(map[string]struct{}), homeLimit: homeLimit, homeAuthority: homeAuthority}
		roundCtx := context.WithValue(ctx, retryRoundContextKey{}, state)
		result, errAttempt := attemptFn(roundCtx, normalized, req, opts, maxRetryCredentials)
		homeLimit, homeAuthority = state.homeLimit, state.homeAuthority
		if errAttempt == nil {
			return result, nil
		}
		lastErr = errAttempt
		if isRequestStopError(errAttempt) {
			break
		}
		wait, shouldRetry := m.shouldRetryAfterError(roundCtx, opts, errAttempt, attempt, normalized, req.Model, maxWait)
		traceAttempt, previousAttempt := currentRouteTraceAttempt(opts)
		resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errAttempt, "retry_stopped")
		if !shouldRetry {
			publishRouteTraceEvent(
				opts.Metadata, cliproxyexecutor.RouteTraceEvent{
					Attempt:         traceAttempt,
					PreviousAttempt: previousAttempt,
					Provider:        traceProvider,
					Model:           req.Model,
					Action:          "retry.stopped",
					Reason:          "not_retryable",
					ResultCode:      resultCode,
					HTTPStatus:      httpStatus,
					RetryAfterMS:    retryAfterMS,
				},
			)
			break
		}
		publishRouteTraceEvent(
			opts.Metadata, cliproxyexecutor.RouteTraceEvent{
				Attempt:         traceAttempt,
				PreviousAttempt: previousAttempt,
				Provider:        traceProvider,
				Model:           req.Model,
				Action:          "cooldown.wait",
				Reason:          "retry_after_failure",
				ResultCode:      resultCode,
				HTTPStatus:      httpStatus,
				RetryAfterMS:    wait.Milliseconds(),
			},
		)
		if errWait := waitForCooldown(ctx, wait, maxWait); errWait != nil {
			waitCode, waitStatus, _ := routeTraceError(opts.Metadata, errWait, "cooldown_wait_failed")
			publishRouteTraceEvent(
				opts.Metadata, cliproxyexecutor.RouteTraceEvent{
					Attempt:         traceAttempt,
					PreviousAttempt: previousAttempt,
					Provider:        traceProvider,
					Model:           req.Model,
					Action:          "cooldown.failed",
					Reason:          "wait_interrupted",
					ResultCode:      waitCode,
					HTTPStatus:      waitStatus,
				},
			)
			return zero, errWait
		}
		publishRouteTraceEvent(
			opts.Metadata, cliproxyexecutor.RouteTraceEvent{
				Attempt:         traceAttempt,
				PreviousAttempt: previousAttempt,
				Provider:        traceProvider,
				Model:           req.Model,
				Action:          "cooldown.completed",
				ResultCode:      "success",
			},
		)
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
	if m == nil || lastErr == nil || isRequestStopError(lastErr) || isRequestScopedError(lastErr) {
		return false
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil || !cfg.QuotaExceeded.AntigravityCredits {
		return false
	}
	switch statusCodeFromError(lastErr) {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		disposition := executionDispositionForError(lastErr, cliproxyexecutor.DispatchPhaseMaybeSent)
		return retryDirectiveAllowsRedispatch(disposition)
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
		creditsCtx = util.WithRoundTripper(creditsCtx, rt)
	}
	creditsOpts := ensureRequestedModelMetadata(opts, routeModel)
	creditsCtx = contextWithRequestedModelAlias(creditsCtx, creditsOpts, routeModel)
	publishSelectedAuthMetadata(creditsOpts.Metadata, auth)
	return creditsCtx, cliproxyexecutor.Request{Model: routeModel}, creditsOpts
}

func (m *Manager) antigravityCreditsModelAliasResult(routeModel string, auth *Auth) OAuthModelAliasResult {
	model := rewriteModelForAuth(routeModel, auth)
	aliasResult := m.applyOAuthModelAliasWithResult(auth, model)
	apiKeyResult := m.resolveAPIKeyModelAliasWithResult(auth, aliasResult.UpstreamModel)
	if apiKeyResult.UpstreamModel != "" {
		return apiKeyResult
	}
	return aliasResult
}

func (m *Manager) tryAntigravityCreditsExecute(
	ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error, bool) {
	var modelSupportErr error
	attemptedUpstream := false
	for _, candidate := range m.findAllAntigravityCreditsCandidateAuths(req.Model, opts) {
		if errCtx := ctx.Err(); errCtx != nil {
			return cliproxyexecutor.Response{}, errCtx, false
		}
		attempt, previousAttempt := nextRouteTraceAttempt(opts)
		attemptOpts := withRouteTraceAttempt(opts, attempt, previousAttempt)
		publishAuthRouteTraceEvent(
			attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
				Provider: candidate.provider,
				Model:    req.Model,
				Action:   "candidate.selected",
				Reason:   "antigravity_credits_fallback",
			},
		)
		publishAuthRouteTraceEvent(
			attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
				Provider: candidate.provider,
				Model:    req.Model,
				Action:   "route.selected",
				Reason:   "antigravity_credits_fallback",
			},
		)
		publishAuthRouteTraceEvent(
			attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
				Provider: candidate.provider,
				Model:    req.Model,
				Action:   "attempt.prepare_started",
			},
		)
		creditsCtx, execReq, creditsOpts := m.prepareAntigravityCreditsExecution(
			ctx, candidate.auth, req.Model, attemptOpts,
		)
		execReq.Payload = req.Payload
		aliasResult := m.antigravityCreditsModelAliasResult(req.Model, candidate.auth)
		execReq.Model = aliasResult.UpstreamModel
		var errIntercept error
		creditsCtx, execReq, creditsOpts, errIntercept = applyRequestAfterAuthInterceptor(
			creditsCtx, candidate.executor, candidate.provider, candidate.auth, execReq, creditsOpts, req.Model,
		)
		if errIntercept != nil {
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errIntercept, "request_intercepted")
			publishAuthRouteTraceEvent(
				attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
					Provider:     candidate.provider,
					Model:        req.Model,
					TargetModel:  execReq.Model,
					Action:       "attempt.intercept_failed",
					ResultCode:   resultCode,
					HTTPStatus:   httpStatus,
					RetryAfterMS: retryAfterMS,
				},
			)
			return cliproxyexecutor.Response{}, errIntercept, true
		}
		execReq, errIntercept = attachResolvedAPIKeyModelInfo(
			execReq, candidate.auth, req.Model, execReq.Model,
		)
		if errIntercept != nil {
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errIntercept, "model_not_supported")
			publishAuthRouteTraceEvent(
				attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
					Provider:     candidate.provider,
					Model:        req.Model,
					TargetModel:  execReq.Model,
					Action:       "attempt.model_rejected",
					ResultCode:   resultCode,
					HTTPStatus:   httpStatus,
					RetryAfterMS: retryAfterMS,
				},
			)
			if isModelSupportError(errIntercept) {
				modelSupportErr = errIntercept
				continue
			}
			return cliproxyexecutor.Response{}, errIntercept, true
		}
		publishAuthRouteTraceEvent(
			attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
				Provider:    candidate.provider,
				Model:       req.Model,
				TargetModel: execReq.Model,
				Action:      "attempt.prepare_completed",
				ResultCode:  "success",
			},
		)
		publishAuthRouteTraceEvent(
			attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
				Provider:    candidate.provider,
				Model:       req.Model,
				TargetModel: execReq.Model,
				Action:      "attempt.execute_started",
			},
		)
		attemptedUpstream = true
		resp, errExec := candidate.executor.Execute(
			creditsCtx, candidate.auth, execReq, externalRouteTraceOptions(creditsOpts),
		)
		phase := cliproxyexecutor.DispatchPhaseResponseStarted
		if errExec != nil {
			phase = cliproxyexecutor.DispatchPhaseMaybeSent
		}
		result := executionResult(candidate.auth, candidate.provider, req.Model, errExec, phase)
		decision := requestScopedActionDecision{err: errExec}
		if errExec != nil {
			result.RetryAfter = retryAfterFromError(errExec)
			decision = m.applyRequestScopedErrorRule(candidate.auth, errExec, &result)
			errExec = decision.err
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errExec, "execution_failed")
			publishResultRouteTraceEvent(
				attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
					Provider:     candidate.provider,
					Model:        req.Model,
					TargetModel:  execReq.Model,
					Action:       "attempt.failed",
					Reason:       "upstream_failure",
					ResultCode:   resultCode,
					HTTPStatus:   httpStatus,
					RetryAfterMS: retryAfterMS,
					FinalTarget:  !retryDirectiveAllowsRedispatch(result.Disposition),
				}, result,
			)
			m.MarkResult(creditsCtx, result)
			recordedEvent := cliproxyexecutor.RouteTraceEvent{
				Provider:     candidate.provider,
				Model:        req.Model,
				TargetModel:  execReq.Model,
				Action:       "result.recorded",
				ResultCode:   resultCode,
				HTTPStatus:   httpStatus,
				RetryAfterMS: retryAfterMS,
			}
			applyDispositionToRouteTrace(&recordedEvent, result.Disposition, result.StateGeneration)
			publishAuthRouteTraceEvent(attemptOpts, candidate.auth, recordedEvent)
			if decision.stop {
				return cliproxyexecutor.Response{}, wrapRequestStopError(errExec), true
			}
			if !retryDirectiveAllowsRedispatch(result.Disposition) {
				return cliproxyexecutor.Response{}, errExec, true
			}
			continue
		}
		resultCode, _, _ := routeTraceError(opts.Metadata, nil, "execution_failed")
		publishResultRouteTraceEvent(
			attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
				Provider:    candidate.provider,
				Model:       req.Model,
				TargetModel: execReq.Model,
				Action:      "attempt.completed",
				ResultCode:  resultCode,
				HTTPStatus:  http.StatusOK,
				FinalTarget: true,
			}, result,
		)
		m.MarkResult(creditsCtx, result)
		publishResultRouteTraceEvent(
			attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
				Provider:    candidate.provider,
				Model:       req.Model,
				TargetModel: execReq.Model,
				Action:      "result.recorded",
				ResultCode:  resultCode,
				HTTPStatus:  http.StatusOK,
				FinalTarget: true,
			}, result,
		)
		rewriteForceMappedResponse(&resp, aliasResult)
		return resp, nil, true
	}
	if !attemptedUpstream && modelSupportErr != nil {
		return cliproxyexecutor.Response{}, modelSupportErr, true
	}
	return cliproxyexecutor.Response{}, nil, false
}

func (m *Manager) tryAntigravityCreditsExecuteStream(
	ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error, bool) {
	var modelSupportErr error
	attemptedUpstream := false
	for _, candidate := range m.findAllAntigravityCreditsCandidateAuths(req.Model, opts) {
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx, false
		}
		attempt, previousAttempt := nextRouteTraceAttempt(opts)
		attemptOpts := withRouteTraceAttempt(opts, attempt, previousAttempt)
		publishAuthRouteTraceEvent(
			attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
				Provider: candidate.provider,
				Model:    req.Model,
				Action:   "candidate.selected",
				Reason:   "antigravity_credits_fallback",
			},
		)
		publishAuthRouteTraceEvent(
			attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
				Provider: candidate.provider,
				Model:    req.Model,
				Action:   "route.selected",
				Reason:   "antigravity_credits_fallback",
			},
		)
		publishAuthRouteTraceEvent(
			attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
				Provider: candidate.provider,
				Model:    req.Model,
				Action:   "attempt.prepare_started",
			},
		)
		creditsCtx, execReq, creditsOpts := m.prepareAntigravityCreditsExecution(
			ctx, candidate.auth, req.Model, attemptOpts,
		)
		execReq.Payload = req.Payload
		aliasResult := m.antigravityCreditsModelAliasResult(req.Model, candidate.auth)
		execReq.Model = aliasResult.UpstreamModel
		var errIntercept error
		creditsCtx, execReq, creditsOpts, errIntercept = applyRequestAfterAuthInterceptor(
			creditsCtx, candidate.executor, candidate.provider, candidate.auth, execReq, creditsOpts, req.Model,
		)
		if errIntercept != nil {
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errIntercept, "request_intercepted")
			publishAuthRouteTraceEvent(
				attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
					Provider:     candidate.provider,
					Model:        req.Model,
					TargetModel:  execReq.Model,
					Action:       "attempt.intercept_failed",
					ResultCode:   resultCode,
					HTTPStatus:   httpStatus,
					RetryAfterMS: retryAfterMS,
				},
			)
			return nil, errIntercept, true
		}
		execReq, errIntercept = attachResolvedAPIKeyModelInfo(
			execReq, candidate.auth, req.Model, execReq.Model,
		)
		if errIntercept != nil {
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errIntercept, "model_not_supported")
			publishAuthRouteTraceEvent(
				attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
					Provider:     candidate.provider,
					Model:        req.Model,
					TargetModel:  execReq.Model,
					Action:       "attempt.model_rejected",
					ResultCode:   resultCode,
					HTTPStatus:   httpStatus,
					RetryAfterMS: retryAfterMS,
				},
			)
			if isModelSupportError(errIntercept) {
				modelSupportErr = errIntercept
				continue
			}
			return nil, errIntercept, true
		}
		publishAuthRouteTraceEvent(
			attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
				Provider:    candidate.provider,
				Model:       req.Model,
				TargetModel: execReq.Model,
				Action:      "attempt.prepare_completed",
				ResultCode:  "success",
			},
		)
		publishAuthRouteTraceEvent(
			attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
				Provider:    candidate.provider,
				Model:       req.Model,
				TargetModel: execReq.Model,
				Action:      "attempt.execute_started",
			},
		)
		attemptedUpstream = true
		creditsCtx = contextWithRouteTraceStream(creditsCtx, attemptOpts, execReq.Model)
		result, errStream := m.executeStreamWithBootstrap(
			creditsCtx, candidate.auth, candidate.executor, candidate.provider, execReq, creditsOpts, req.Model,
			aliasResult,
		)
		if errStream != nil {
			failure := executionResult(
				candidate.auth,
				candidate.provider,
				req.Model,
				errStream,
				cliproxyexecutor.DispatchPhaseMaybeSent,
			)
			failure.RetryAfter = retryAfterFromError(errStream)
			decision := m.applyRequestScopedErrorRule(candidate.auth, errStream, &failure)
			errStream = decision.err
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errStream, "stream_bootstrap_failed")
			publishResultRouteTraceEvent(
				attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
					Provider:     candidate.provider,
					Model:        req.Model,
					TargetModel:  execReq.Model,
					Action:       "attempt.failed",
					Reason:       "stream_bootstrap_failed",
					ResultCode:   resultCode,
					HTTPStatus:   httpStatus,
					RetryAfterMS: retryAfterMS,
					FinalTarget:  !retryDirectiveAllowsRedispatch(failure.Disposition),
				}, failure,
			)
			m.MarkResult(creditsCtx, failure)
			recordedEvent := cliproxyexecutor.RouteTraceEvent{
				Provider:     candidate.provider,
				Model:        req.Model,
				TargetModel:  execReq.Model,
				Action:       "result.recorded",
				ResultCode:   resultCode,
				HTTPStatus:   httpStatus,
				RetryAfterMS: retryAfterMS,
			}
			applyDispositionToRouteTrace(&recordedEvent, failure.Disposition, failure.StateGeneration)
			publishAuthRouteTraceEvent(attemptOpts, candidate.auth, recordedEvent)
			if decision.stop {
				return nil, wrapRequestStopError(errStream), true
			}
			if !retryDirectiveAllowsRedispatch(failure.Disposition) {
				return nil, errStream, true
			}
			continue
		}
		publishAuthRouteTraceEvent(
			attemptOpts, candidate.auth, cliproxyexecutor.RouteTraceEvent{
				Provider:    candidate.provider,
				Model:       req.Model,
				TargetModel: execReq.Model,
				Action:      "attempt.stream_started",
				ResultCode:  "stream_started",
				HTTPStatus:  http.StatusOK,
			},
		)
		return result, nil, true
	}
	if !attemptedUpstream && modelSupportErr != nil {
		return nil, modelSupportErr, true
	}
	return nil, nil, false
}

func executorForAuth(executor ProviderExecutor, auth *Auth) (ProviderExecutor, error) {
	if auth != nil && auth.AuthKind() == AuthKindAPIKey {
		if scoped, ok := executor.(APIKeyConfigExecutor); ok {
			configured := scoped.ForAPIKey()
			if configured == nil {
				return nil, fmt.Errorf("API key executor is unavailable")
			}
			return configured, nil
		}
	}
	return executor, nil
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
	opts = ensureRouteTraceState(opts)
	homeAuthCount := 1
	tried := make(map[string]struct{})
	if state := retryRoundFromContext(ctx); state != nil {
		tried = state.attempted
	}
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		homeMode := m.HomeEnabled()
		if maxRetryCredentials > 0 && len(tried) >= maxRetryCredentials {
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, lastErr, "credential_limit_reached")
			attempt, previousAttempt := currentRouteTraceAttempt(opts)
			publishRouteTraceEvent(
				opts.Metadata, cliproxyexecutor.RouteTraceEvent{
					Attempt:         attempt,
					PreviousAttempt: previousAttempt,
					Provider:        "mixed",
					Model:           routeModel,
					Action:          "route.stopped",
					Reason:          "credential_limit_reached",
					ResultCode:      resultCode,
					HTTPStatus:      httpStatus,
					RetryAfterMS:    retryAfterMS,
				},
			)
			if lastErr != nil {
				return zero, lastErr
			}
			return zero, &Error{Code: "auth_not_found", Message: "no auth available"}
		}
		attempt, previousAttempt := nextRouteTraceAttempt(opts)
		pickOpts := withRouteTraceAttempt(opts, attempt, previousAttempt)
		if homeMode {
			pickOpts = withHomeAuthCount(pickOpts, homeAuthCount)
		}
		pickCtx, leaseRequest := withQuotaInFlightRequest(ctx)
		auth, executor, provider, errPick := m.pickNextMixed(pickCtx, providers, routeModel, pickOpts, tried)
		if errPick != nil {
			leaseRequest.release()
			returnedErr := errPick
			if shouldReturnLastErrorOnPickFailure(homeMode, lastErr, errPick) {
				returnedErr = lastErr
			}
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, returnedErr, "selection_failed")
			publishRouteTraceEvent(
				opts.Metadata, cliproxyexecutor.RouteTraceEvent{
					Attempt:         attempt,
					PreviousAttempt: previousAttempt,
					Provider:        "mixed",
					Model:           routeModel,
					Action:          "route.selection_failed",
					Reason:          "no_selectable_credential",
					ResultCode:      resultCode,
					HTTPStatus:      httpStatus,
					RetryAfterMS:    retryAfterMS,
				},
			)
			return zero, returnedErr
		}
		if m.scheduler != nil && leaseRequest.get() == nil {
			m.scheduler.captureQuotaInFlightLease(pickCtx, auth, provider)
		}
		lease := leaseRequest.take()
		releaseLease := lease != nil
		releaseLeaseNow := func() {
			if releaseLease {
				lease.Release()
				releaseLease = false
			}
		}
		// Retry branches release immediately; this defer covers returns, panics, and stream ownership transfer.
		// noinspection GoDeferInLoop
		defer releaseLeaseNow()

		entry := logEntryWithRequestID(ctx)
		debugLogAuthSelection(entry, auth, provider, req.Model)
		publishSelectedAuthMetadata(opts.Metadata, auth)
		publishSelectedAuthMetadata(pickOpts.Metadata, auth)
		selectionReason := "local_scheduler"
		if homeMode {
			selectionReason = "home_dispatch"
		}
		publishAuthRouteTraceEvent(
			pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
				Provider: provider,
				Model:    routeModel,
				Action:   "route.selected",
				Reason:   selectionReason,
			},
		)

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
		execCtx, leaseTransfer := withQuotaInFlightTransfer(execCtx, lease)
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = util.WithRoundTripper(execCtx, rt)
		}
		var errPrepare error
		publishAuthRouteTraceEvent(
			pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
				Provider: provider,
				Model:    routeModel,
				Action:   "attempt.prepare_started",
			},
		)
		prepareStarted := time.Now()
		auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare == nil {
			executor, errPrepare = executorForAuth(executor, auth)
		}
		warnLogUpstreamFailure(execCtx, auth, provider, routeModel, prepareStarted, errPrepare, cliproxyexecutor.DispatchPhaseNotSent)
		if errPrepare != nil {
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errPrepare, "prepare_failed")
			publishAuthRouteTraceEvent(
				pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
					Provider:     provider,
					Model:        routeModel,
					Action:       "attempt.prepare_failed",
					ResultCode:   resultCode,
					HTTPStatus:   httpStatus,
					RetryAfterMS: retryAfterMS,
				},
			)
			if errCancel := claudeOAuthRequestCancellation(execCtx, auth, errPrepare); errCancel != nil {
				return zero, errCancel
			}
			result := executionResult(
				auth, provider, routeModel, errPrepare, cliproxyexecutor.DispatchPhaseNotSent,
			)
			result.RuntimeOnly = runtimeOnly
			result.Options = cloneExecutorOptions(pickOpts)
			recordResult(execCtx, result)
			publishResultRouteTraceEvent(
				pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
					Provider:   provider,
					Model:      routeModel,
					Action:     "result.recorded",
					ResultCode: resultCode,
					HTTPStatus: httpStatus,
				}, result,
			)
			lastErr = errPrepare
			publishAuthRouteTraceEvent(
				pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
					Provider: provider,
					Model:    routeModel,
					Action:   "attempt.retry",
					Reason:   "next_credential",
				},
			)
			if runtimeOnly {
				homeAuthCount++
			}
			releaseLeaseNow()
			continue
		}
		publishAuthRouteTraceEvent(
			pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
				Provider:   provider,
				Model:      routeModel,
				Action:     "attempt.prepare_completed",
				ResultCode: "success",
			},
		)
		execReq := req
		aliasResult := OAuthModelAliasResult{}
		if auth.Attributes != nil && strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]) != "" {
			execReq.Model = strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey])
			aliasResult = homeForceMappingAliasResult(auth, execReq.Model)
		} else {
			execReq.Model = rewriteModelForAuth(routeModel, auth)
			aliasResult = m.resolveExecutionAliasResultForRequested(auth, execReq.Model)
			execReq.Model = aliasResult.UpstreamModel
		}
		execOpts := cloneExecutorOptions(pickOpts)
		var errIntercept error
		execCtx, execReq, execOpts, errIntercept = applyRequestAfterAuthInterceptor(
			execCtx, executor, provider, auth, execReq, execOpts, routeModel,
		)
		if errIntercept != nil {
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errIntercept, "request_intercepted")
			publishAuthRouteTraceEvent(
				pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
					Provider:     provider,
					Model:        routeModel,
					TargetModel:  execReq.Model,
					Action:       "attempt.intercept_failed",
					ResultCode:   resultCode,
					HTTPStatus:   httpStatus,
					RetryAfterMS: retryAfterMS,
				},
			)
			return zero, errIntercept
		}
		execReq, errIntercept = attachResolvedAPIKeyModelInfo(execReq, auth, routeModel, execReq.Model)
		if errIntercept != nil {
			result := executionResult(
				auth, provider, routeModel, errIntercept, cliproxyexecutor.DispatchPhaseNotSent,
			)
			result.RuntimeOnly = runtimeOnly
			result.Options = cloneExecutorOptions(execOpts)
			recordResult(execCtx, result)
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errIntercept, "model_not_supported")
			publishResultRouteTraceEvent(
				pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
					Provider:     provider,
					Model:        routeModel,
					TargetModel:  execReq.Model,
					Action:       "attempt.model_rejected",
					ResultCode:   resultCode,
					HTTPStatus:   httpStatus,
					RetryAfterMS: retryAfterMS,
				}, result,
			)
			publishResultRouteTraceEvent(
				pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
					Provider:   provider,
					Model:      routeModel,
					Action:     "result.recorded",
					ResultCode: resultCode,
					HTTPStatus: httpStatus,
				}, result,
			)
			lastErr = errIntercept
			publishAuthRouteTraceEvent(
				pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
					Provider: provider,
					Model:    routeModel,
					Action:   "attempt.retry",
					Reason:   "next_credential",
				},
			)
			if runtimeOnly {
				homeAuthCount++
			}
			releaseLeaseNow()
			continue
		}
		execReq = sanitizeDownstreamWebsocketFallbackRequest(execCtx, auth, execReq)
		if opts.Stream {
			execCtx = contextWithRouteTraceStream(execCtx, pickOpts, execReq.Model)
		}
		publishAuthRouteTraceEvent(
			pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
				Provider:    provider,
				Model:       routeModel,
				TargetModel: execReq.Model,
				Action:      "attempt.execute_started",
			},
		)
		attemptCtx := newUpstreamAttemptContext(execCtx, execOpts.Stream)
		started := time.Now()
		resp, errExec := executeFn(attemptCtx, auth, executor, provider, execReq, execOpts, aliasResult)
		warnLogUpstreamFailure(attemptCtx, auth, provider, execReq.Model, started, errExec, cliproxyexecutor.DispatchPhaseMaybeSent)
		if errExec != nil {
			if errCtx := attemptCtx.Err(); errCtx != nil {
				errExec = errCtx
			} else {
				disposition := executionDispositionForAttempt(
					errExec, cliproxyexecutor.DispatchPhaseMaybeSent, auth, provider, routeModel,
				)
				if retryDirectiveAllowsRefresh(disposition) {
					refreshCtx := newUpstreamAttemptContext(execCtx, false)
					if refreshed, okRefresh := m.tryRefreshAfterUnauthorized(
						refreshCtx, auth, executor, errExec, false,
					); okRefresh {
						if runtimeOnly && cachedRuntimeAuth != nil &&
							!m.compareAndSwapHomeRuntimeAuth(
								runtimeSessionID, cachedRuntimeAuth, refreshed, executor,
							) {
							publishAuthRouteTraceEvent(
								pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
									Provider:   provider,
									Model:      routeModel,
									Action:     "attempt.retry",
									Reason:     "runtime_auth_changed",
									ResultCode: "stale_selection",
								},
							)
							releaseLeaseNow()
							continue
						}
						auth = refreshed
						publishAuthRouteTraceEvent(
							pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
								Provider:    provider,
								Model:       routeModel,
								TargetModel: execReq.Model,
								Action:      "auth.refresh_completed",
								ResultCode:  "success",
							},
						)
						publishAuthRouteTraceEvent(
							pickOpts, auth, cliproxyexecutor.RouteTraceEvent{
								Provider:    provider,
								Model:       routeModel,
								TargetModel: execReq.Model,
								Action:      "attempt.execute_retried",
								Reason:      "auth_refreshed",
							},
						)
						attemptCtx = newUpstreamAttemptContext(execCtx, execOpts.Stream)
						started = time.Now()
						resp, errExec = executeFn(attemptCtx, auth, executor, provider, execReq, execOpts, aliasResult)
						warnLogUpstreamFailure(attemptCtx, auth, provider, execReq.Model, started, errExec, cliproxyexecutor.DispatchPhaseMaybeSent)
						if errCtx := attemptCtx.Err(); errExec != nil && errCtx != nil {
							errExec = errCtx
						}
					}
				}
			}
		}
		if errCancel := claudeOAuthRequestCancellation(attemptCtx, auth, errExec); errCancel != nil {
			errExec = errCancel
		}
		phase := cliproxyexecutor.DispatchPhaseResponseStarted
		if errExec != nil {
			phase = cliproxyexecutor.DispatchPhaseMaybeSent
		}
		result := executionResult(auth, provider, routeModel, errExec, phase)
		if retryDirectiveAllowsRefresh(result.Disposition) {
			result.Disposition.RetryDirective = cliproxyexecutor.RetryDirectiveNextAuth
			if result.Error != nil {
				result.Error.RetryDirective = cliproxyexecutor.RetryDirectiveNextAuth
			}
		}
		result.RuntimeOnly = runtimeOnly
		result.Options = cloneExecutorOptions(execOpts)
		decision := requestScopedActionDecision{err: errExec}
		compactRequestFault := false
		if errExec != nil {
			result.RetryAfter = retryAfterFromError(errExec)
			decision = m.applyRequestScopedErrorRule(auth, errExec, &result)
			errExec = decision.err
			if !decision.matched {
				compactRequestFault = applyResponsesCompactErrorDisposition(execOpts, errExec, &result)
			}
			resultCode, httpStatus, retryAfterMS := routeTraceError(opts.Metadata, errExec, "execution_failed")
			failureReason := "upstream_failure"
			if isConnectionLifecycleError(errExec) {
				failureReason = "request_canceled"
			}
			failureEvent := cliproxyexecutor.RouteTraceEvent{
				Provider:     provider,
				Model:        routeModel,
				TargetModel:  execReq.Model,
				Action:       "attempt.failed",
				Reason:       failureReason,
				ResultCode:   resultCode,
				HTTPStatus:   httpStatus,
				RetryAfterMS: retryAfterMS,
				FinalTarget:  !retryDirectiveAllowsRedispatch(result.Disposition),
			}
			applyDispositionToRouteTrace(&failureEvent, result.Disposition, result.StateGeneration)
			publishAuthRouteTraceEvent(pickOpts, auth, failureEvent)
			resultAction := "result.recorded"
			if isConnectionLifecycleError(errExec) {
				resultAction = "result.observed"
			} else {
				recordResult(attemptCtx, result)
			}
			recordedEvent := cliproxyexecutor.RouteTraceEvent{
				Provider:     provider,
				Model:        routeModel,
				TargetModel:  execReq.Model,
				Action:       resultAction,
				ResultCode:   resultCode,
				HTTPStatus:   httpStatus,
				RetryAfterMS: retryAfterMS,
			}
			applyDispositionToRouteTrace(&recordedEvent, result.Disposition, result.StateGeneration)
			publishAuthRouteTraceEvent(pickOpts, auth, recordedEvent)
			requestInvalid := !decision.matched && (compactRequestFault || isRequestInvalidError(errExec))
			if decision.stop || requestInvalid || !retryDirectiveAllowsRedispatch(result.Disposition) {
				reason := "retry_forbidden"
				if decision.stop {
					reason = "request_scoped_rule"
				} else if requestInvalid {
					reason = "request_invalid"
				} else if result.Disposition.Outcome == cliproxyexecutor.ExecutionOutcomeUnknown {
					reason = "outcome_unknown"
				}
				stoppedEvent := cliproxyexecutor.RouteTraceEvent{
					Provider:   provider,
					Model:      routeModel,
					Action:     "route.stopped",
					Reason:     reason,
					ResultCode: resultCode,
					HTTPStatus: httpStatus,
				}
				applyDispositionToRouteTrace(&stoppedEvent, result.Disposition, result.StateGeneration)
				publishAuthRouteTraceEvent(pickOpts, auth, stoppedEvent)
				if decision.stop {
					return zero, wrapRequestStopError(errExec)
				}
				return zero, errExec
			}
			lastErr = errExec
			retryEvent := cliproxyexecutor.RouteTraceEvent{
				Provider: provider,
				Model:    routeModel,
				Action:   "attempt.retry",
				Reason:   string(result.Disposition.RetryDirective),
			}
			applyDispositionToRouteTrace(&retryEvent, result.Disposition, result.StateGeneration)
			publishAuthRouteTraceEvent(pickOpts, auth, retryEvent)
			if runtimeOnly {
				homeAuthCount++
			}
			releaseLeaseNow()
			continue
		}
		if opts.Stream {
			streamEvent := cliproxyexecutor.RouteTraceEvent{
				Provider:    provider,
				Model:       routeModel,
				TargetModel: execReq.Model,
				Action:      "attempt.stream_started",
				ResultCode:  "stream_started",
				HTTPStatus:  http.StatusOK,
			}
			applyDispositionToRouteTrace(&streamEvent, result.Disposition, result.StateGeneration)
			publishAuthRouteTraceEvent(pickOpts, auth, streamEvent)
			if leaseTransfer != nil && leaseTransfer.claimed.Load() {
				releaseLease = false
			}
			return resp, nil
		}
		completedEvent := cliproxyexecutor.RouteTraceEvent{
			Provider:    provider,
			Model:       routeModel,
			TargetModel: execReq.Model,
			Action:      "attempt.completed",
			ResultCode:  "success",
			HTTPStatus:  http.StatusOK,
			FinalTarget: true,
		}
		applyDispositionToRouteTrace(&completedEvent, result.Disposition, result.StateGeneration)
		publishAuthRouteTraceEvent(pickOpts, auth, completedEvent)
		recordResult(attemptCtx, result)
		recordedEvent := cliproxyexecutor.RouteTraceEvent{
			Provider:    provider,
			Model:       routeModel,
			TargetModel: execReq.Model,
			Action:      "result.recorded",
			ResultCode:  "success",
			HTTPStatus:  http.StatusOK,
			FinalTarget: true,
		}
		applyDispositionToRouteTrace(&recordedEvent, result.Disposition, result.StateGeneration)
		publishAuthRouteTraceEvent(pickOpts, auth, recordedEvent)
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

func (e *streamBootstrapError) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	if e == nil {
		return cliproxyexecutor.ExecutionDisposition{}
	}
	return executionDispositionForError(e.cause, cliproxyexecutor.DispatchPhaseMaybeSent)
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
	maps.Copy(out, in)
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

type authRequestFormatResolver interface {
	RequestToFormatForAuth(*Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) sdktranslator.Format
}

func requestToFormat(
	executor ProviderExecutor,
	provider string,
	auth *Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) sdktranslator.Format {
	if resolver, ok := executor.(authRequestFormatResolver); ok && resolver != nil {
		if format := resolver.RequestToFormatForAuth(auth, req, opts); format != "" {
			return format
		}
	}
	if resolver, ok := executor.(requestFormatResolver); ok && resolver != nil {
		if format := resolver.RequestToFormat(req, opts); format != "" {
			return format
		}
	}
	switch opts.SourceFormat.String() {
	case "openai-image", "openai-video", "openai-speech":
		return opts.SourceFormat
	}
	if opts.Alt == "responses/compact" && !opts.Stream {
		return sdktranslator.FormatOpenAIResponse
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "claude":
		return sdktranslator.FormatClaude
	case "codex", "xai", "meta":
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
) (context.Context, cliproxyexecutor.Request, cliproxyexecutor.Options, error) {
	if opts.RequestAfterAuthInterceptor == nil {
		return syncMetadataSessionToContext(ctx, opts.Metadata), req, opts, nil
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
			ToFormat:        requestToFormat(executor, provider, auth, req, opts),
			Model:           req.Model,
			RequestedModel:  requestedModelAliasFromOptions(opts, requestedModel),
			Stream:          opts.Stream,
			AuthID:          authID,
			AuthLabel:       authLabel,
			AuthProvider:    authProvider,
			OriginalRequest: bytes.Clone(opts.OriginalRequest),
			Request:         cloneExecutorRequest(req),
			Options:         cloneExecutorOptions(externalRouteTraceOptions(opts)),
		},
	)
	if resp.Terminate {
		return ctx, req, opts, &cliproxyexecutor.RequestTerminatedError{
			HTTPStatus: resp.StatusCode,
			Header:     resp.ResponseHeaders.Clone(),
			Body:       bytes.Clone(resp.ResponseBody),
		}
	}
	if !resp.Handled {
		return syncMetadataSessionToContext(ctx, opts.Metadata), req, opts, nil
	}
	nextOpts := cloneExecutorOptions(resp.Options)
	nextOpts.CountTokens = opts.CountTokens
	nextOpts.Metadata = restoreRouteTraceMetadata(nextOpts.Metadata, opts.Metadata)
	previousSession := CanonicalSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata)
	currentSession := CanonicalSessionID(nextOpts.Headers, nextOpts.OriginalRequest, nextOpts.Metadata)
	if previousSession != currentSession {
		ctx = util.WithInferredSessionID(ctx, currentSession)
	}
	return syncMetadataSessionToContext(ctx, nextOpts.Metadata), cloneExecutorRequest(resp.Request), nextOpts, nil
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
		Metadata: externalRouteTraceMetadata(opts.Metadata),
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

func pluginSchedulerCandidates(scheduler PluginScheduler, candidates []*Auth, model string) []*Auth {
	if scheduler == nil {
		return nil
	}
	across := false
	if opt, ok := scheduler.(PluginSchedulerAcrossPriorities); ok {
		across = opt.SchedulerWantsAcrossPriorities()
	}
	available := make([]*Auth, 0, len(candidates))
	priority := 0
	found := false
	now := time.Now()
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		if blocked, _, _ := isAuthBlockedForModel(candidate, model, now); blocked {
			continue
		}
		candidatePriority := authPriority(candidate)
		if !across {
			if found && candidatePriority < priority {
				continue
			}
			if !found || candidatePriority > priority {
				available = available[:0]
				priority = candidatePriority
				found = true
			}
		}
		available = append(available, candidate)
	}
	return available
}

func (m *Manager) pickViaPluginScheduler(
	ctx context.Context,
	scheduler PluginScheduler,
	provider string,
	providers []string,
	model string,
	opts cliproxyexecutor.Options,
	candidates []*Auth,
) (*Auth, bool, schedulerStrategy, error) {
	if scheduler == nil || len(candidates) == 0 {
		return nil, false, schedulerStrategyCustom, nil
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
		return nil, true, schedulerStrategyCustom, errPick
	}
	if !handled || !resp.Handled {
		return nil, false, schedulerStrategyCustom, nil
	}
	if resp.Reject {
		code := strings.TrimSpace(resp.RejectCode)
		if code == "" {
			code = "auth_unavailable"
		}
		message := strings.TrimSpace(resp.RejectReason)
		if message == "" {
			message = "scheduler rejected candidate selection"
		}
		return nil, true, schedulerStrategyCustom, &Error{Code: code, Message: message}
	}
	if selected := pickSchedulerAuthByID(candidates, resp.AuthID); selected != nil {
		return selected, true, schedulerStrategyCustom, nil
	}
	if strategy, ok := builtinSchedulerStrategy(resp.DelegateBuiltin); ok {
		candidateIDs := make(map[string]struct{}, len(candidates))
		for _, candidate := range candidates {
			if candidate != nil {
				candidateIDs[candidate.ID] = struct{}{}
			}
		}
		predicate := func(auth *Auth) bool {
			_, allowed := candidateIDs[auth.ID]
			return allowed
		}
		if providerKey == "mixed" {
			selected, _, errSelect := m.scheduler.pickMixedWithPredicate(
				ctx, req.Providers, model, opts, nil, predicate, strategy,
			)
			return selected, true, strategy, errSelect
		}
		selected, errSelect := m.scheduler.pickSingleWithPredicate(
			ctx, providerKey, model, opts, nil, predicate, strategy,
		)
		return selected, true, strategy, errSelect
	}
	return nil, false, schedulerStrategyCustom, nil
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
	if lineWise := rewriter.rewriteSSEPayloadLines(payload); len(lineWise) > 0 {
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
	streamResult, errStream := executor.ExecuteStream(
		execCtx, streamAuth, execReq, externalRouteTraceOptions(execOpts),
	)
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
	return m.wrapStreamResultWithOptions(
		execCtx,
		streamAuth.Clone(),
		streamProvider,
		routeModel,
		execOpts,
		streamResult.Headers,
		buffered,
		remaining,
		aliasResult,
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
	return m.wrapStreamResultWithOptions(
		execCtx,
		streamAuth,
		streamProvider,
		routeModel,
		cliproxyexecutor.Options{},
		headers,
		buffered,
		remaining,
		aliasResult,
	)
}

func (m *Manager) wrapStreamResultWithOptions(
	execCtx context.Context,
	streamAuth *Auth,
	streamProvider string,
	routeModel string,
	execOpts cliproxyexecutor.Options,
	headers http.Header,
	buffered []cliproxyexecutor.StreamChunk,
	remaining <-chan cliproxyexecutor.StreamChunk,
	aliasResult OAuthModelAliasResult,
) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	quotaLease := takeQuotaInFlightLease(execCtx)
	started := time.Now()
	go func(streamCtx context.Context, streamChunks <-chan cliproxyexecutor.StreamChunk) {
		defer close(out)
		if quotaLease != nil {
			defer quotaLease.Release()
		}
		deliveryAware := cliproxyexecutor.StreamDeliveryAcknowledgement(streamCtx)
		failed := false
		forward := true
		sourceTerminalDelivered := false
		var rewriter *StreamRewriter
		if aliasResult.ForceMapping && strings.TrimSpace(aliasResult.OriginalAlias) != "" {
			rewriter = NewStreamRewriter(StreamRewriteOptions{RewriteModel: aliasResult.OriginalAlias})
		}
		newResult := func(err error, phase cliproxyexecutor.DispatchPhase) Result {
			result := executionResult(streamAuth, streamProvider, routeModel, err, phase)
			result.Options = cloneExecutorOptions(execOpts)
			return result
		}
		recordUpstreamFailure := func(err error) {
			if failed {
				return
			}
			failed = true
			warnLogUpstreamFailure(streamCtx, streamAuth, streamProvider, routeModel, started, err, cliproxyexecutor.DispatchPhaseResponseStarted)
			result := newResult(err, cliproxyexecutor.DispatchPhaseResponseStarted)
			result.RetryAfter = retryAfterFromError(err)
			m.applyRequestScopedErrorRule(streamAuth, err, &result)
			m.MarkResult(streamCtx, result)
			publishStreamRouteTraceResult(streamCtx, streamAuth, streamProvider, routeModel, err)
		}
		recordDeliveryFailure := func(err error) {
			if failed {
				return
			}
			failed = true
			result := newResult(err, cliproxyexecutor.DispatchPhaseResponseStarted)
			if result.Error != nil {
				result.Error.Code = requestScopedErrorCode
			}
			m.recordAvailabilityNeutralResult(streamCtx, result)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				publishStreamRouteTraceCancellation(
					streamCtx, streamAuth, streamProvider, routeModel, err,
				)
				return
			}
			publishStreamRouteTraceResult(streamCtx, streamAuth, streamProvider, routeModel, err)
		}
		deliver := func(chunk cliproxyexecutor.StreamChunk) (bool, error) {
			if !forward {
				return false, nil
			}
			if !deliveryAware {
				if streamCtx == nil {
					out <- chunk
					return true, nil
				}
				select {
				case <-streamCtx.Done():
					forward = false
					publishStreamRouteTraceCancellation(
						streamCtx, streamAuth, streamProvider, routeModel, streamCtx.Err(),
					)
					return false, nil
				case out <- chunk:
					return true, nil
				}
			}

			delivery := make(chan error, 1)
			originalAcknowledge := chunk.Acknowledge
			var acknowledgeOnce sync.Once
			chunk.Acknowledge = func(err error) {
				acknowledgeOnce.Do(
					func() {
						if originalAcknowledge != nil {
							originalAcknowledge(err)
						}
						delivery <- err
					},
				)
			}
			select {
			case <-streamCtx.Done():
				forward = false
				chunk.Acknowledge(streamCtx.Err())
				return false, streamCtx.Err()
			case out <- chunk:
			}
			select {
			case errDelivery := <-delivery:
				if errDelivery != nil {
					forward = false
				}
				return errDelivery == nil, errDelivery
			case <-streamCtx.Done():
				forward = false
				chunk.Acknowledge(streamCtx.Err())
				select {
				case errDelivery := <-delivery:
					return errDelivery == nil, errDelivery
				default:
					return false, streamCtx.Err()
				}
			}
		}
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err == nil && !chunk.Terminal && len(chunk.Payload) == 0 {
				if chunk.Acknowledge != nil {
					chunk.Acknowledge(nil)
				}
				return true
			}
			if chunk.Err != nil && !deliveryAware {
				recordUpstreamFailure(chunk.Err)
			}
			if len(chunk.Payload) > 0 {
				payload := rewriteForceMappedStreamChunk(rewriter, chunk.Payload)
				if len(payload) == 0 {
					if chunk.Acknowledge != nil {
						chunk.Acknowledge(nil)
					}
					return true
				}
				chunk.Payload = payload
			}
			delivered, errDelivery := deliver(chunk)
			if errDelivery != nil {
				recordDeliveryFailure(errDelivery)
				return false
			}
			if !delivered {
				return false
			}
			if chunk.Terminal {
				sourceTerminalDelivered = true
			}
			if chunk.Err != nil && deliveryAware {
				recordUpstreamFailure(chunk.Err)
			}
			return true
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
		if !deliveryAware {
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
		} else {
			for {
				var (
					chunk cliproxyexecutor.StreamChunk
					ok    bool
				)
				select {
				case <-streamCtx.Done():
					recordDeliveryFailure(streamCtx.Err())
					return
				case chunk, ok = <-streamChunks:
				}
				if !ok {
					break
				}
				if ok := emit(chunk); !ok {
					discardStreamChunks(streamChunks)
					return
				}
				if chunk.Err != nil {
					discardStreamChunks(streamChunks)
					return
				}
			}
			if streamCtx.Err() != nil {
				recordDeliveryFailure(streamCtx.Err())
				return
			}
		}
		if tail := finishForceMappedStreamChunks(rewriter); len(tail) > 0 {
			if ok := emit(cliproxyexecutor.StreamChunk{Payload: tail}); !ok {
				return
			}
		}
		if failed {
			return
		}
		if errCancel := claudeOAuthRequestCancellation(streamCtx, streamAuth, nil); errCancel != nil {
			publishStreamRouteTraceCancellation(
				streamCtx, streamAuth, streamProvider, routeModel, errCancel,
			)
			return
		}
		if deliveryAware && !sourceTerminalDelivered {
			if ok := emit(cliproxyexecutor.StreamChunk{Terminal: true}); !ok {
				return
			}
		}
		m.MarkResult(
			streamCtx,
			newResult(nil, cliproxyexecutor.DispatchPhaseDownstreamCommitted),
		)
		publishStreamRouteTraceResult(streamCtx, streamAuth, streamProvider, routeModel, nil)
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
	if generate, ok := generateFromOptions(opts); ok {
		ctx = coreusage.WithGenerate(ctx, generate)
	}
	return ctx
}

func syncMetadataSessionToContext(ctx context.Context, metadata map[string]any) context.Context {
	if ctx == nil {
		return nil
	}
	sessionID := sessionMetadataString(metadata, cliproxyexecutor.CanonicalSessionIDMetadataKey)
	if sessionID == "" {
		sessionID = sessionMetadataString(metadata, cliproxyexecutor.LCPAffinitySessionIDMetadataKey)
	}
	if sessionID == "" {
		if executionID := sessionMetadataString(metadata, cliproxyexecutor.ExecutionSessionMetadataKey); executionID != "" {
			sessionID = executionID
			if !strings.HasPrefix(sessionID, "execution:") {
				sessionID = "execution:" + sessionID
			}
		} else if derivedID := sessionMetadataString(metadata, cliproxyexecutor.DerivedSessionIDMetadataKey); derivedID != "" {
			sessionID = derivedID
			if !strings.HasPrefix(sessionID, "derived:") {
				sessionID = "derived:" + sessionID
			}
		}
	}
	clientMeta := logging.GetClientRequestMetadata(ctx)
	if sessionID == "" {
		clientMeta.SessionID = ""
		clientMeta.ParentSessionID = ""
		clientMeta.NodeKind = ""
		clientMeta.IsFork = false
		clientMeta.IsCompaction = false
	} else {
		clientMeta.SessionID = cliproxysession.BoundSessionIdentity(sessionID)
		clientMeta.ParentSessionID = cliproxysession.BoundSessionIdentity(
			sessionMetadataString(metadata, cliproxyexecutor.ParentSessionIDMetadataKey),
		)
		if clientMeta.SessionID == clientMeta.ParentSessionID {
			clientMeta.ParentSessionID = ""
		}
		clientMeta.NodeKind = sessionMetadataString(metadata, cliproxyexecutor.NodeKindMetadataKey)
		clientMeta.IsFork = metadataBool(metadata, cliproxyexecutor.IsForkMetadataKey)
		clientMeta.IsCompaction = metadataBool(metadata, cliproxyexecutor.IsCompactionMetadataKey)
	}
	ctx = logging.WithClientRequestMetadata(ctx, clientMeta)
	if sessionMetadataString(metadata, cliproxyexecutor.LCPAffinitySessionIDMetadataKey) != "" || sessionID == "" {
		return util.WithInferredSessionID(ctx, clientMeta.SessionID)
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

func generateFromOptions(opts cliproxyexecutor.Options) (bool, bool) {
	if len(opts.Metadata) == 0 {
		return false, false
	}
	value, ok := opts.Metadata[cliproxyexecutor.GenerateMetadataKey].(bool)
	return value, ok
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

func (m *Manager) nextAuthGenerationLocked() uint64 {
	if m == nil {
		return 0
	}
	m.authGenerationSeq++
	if m.authGenerationSeq == 0 {
		m.authGenerationSeq++
	}
	return m.authGenerationSeq
}

func (m *Manager) assignAuthGenerationLocked(auth *Auth) {
	if m == nil || auth == nil {
		return
	}
	if job := m.autoRefreshJobs[auth.ID]; job != nil {
		current := m.auths[auth.ID]
		if current != nil && current.Generation == job.generation &&
			current.NextRefreshAfter.Equal(job.pendingUntil) && auth.NextRefreshAfter.Equal(job.pendingUntil) {
			auth.NextRefreshAfter = time.Time{}
		}
	}
	auth.Generation = m.nextAuthGenerationLocked()
	current := m.auths[auth.ID]
	auth.creditsHintManager = m
	auth.creditsHintRemovalEpoch = m.authRemovalEpoch(auth.ID)
	auth.creditsHintGeneration = auth.Generation
	if current != nil {
		auth.creditsHintGeneration = current.creditsHintGeneration
	}
	invalidateAntigravityCreditsHint(auth, current)
	if current != nil && current.credentialGeneration != 0 &&
		!persistentAuthBindingChanged(current, auth) && current.ProxyURL == auth.ProxyURL {
		auth.credentialGeneration = current.credentialGeneration
	} else {
		auth.credentialGeneration = auth.Generation
	}
	if m.firstCredentialGenerations == nil {
		m.firstCredentialGenerations = make(map[string]uint64)
	}
	if _, seen := m.firstCredentialGenerations[auth.ID]; !seen {
		m.firstCredentialGenerations[auth.ID] = auth.credentialGeneration
	}
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
	if m == nil || strings.TrimSpace(id) == "" {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if m.authLoadGate == nil {
		return m.lockAuthMutationWithinLoadGate(ctx, id)
	}
	if err := m.authLoadGate.Acquire(ctx, 1); err != nil {
		return func() {}, err
	}
	unlockAuth, err := m.lockAuthMutationWithinLoadGate(ctx, id)
	if err != nil {
		m.authLoadGate.Release(1)
		return func() {}, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			unlockAuth()
			m.authLoadGate.Release(1)
		})
	}, nil
}

// lockAuthMutationWithinLoadGate is used when a refresh holds the load gate
// while locking the separate persistent owner of a virtual credential.
func (m *Manager) lockAuthMutationWithinLoadGate(ctx context.Context, id string) (func(), error) {
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
		once.Do(
			func() {
				lock.token <- struct{}{}
				releaseRef()
			},
		)
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

// PrepareRequestAuth prepares an auth using the same lifecycle rules as normal request execution.
func (m *Manager) PrepareRequestAuth(ctx context.Context, executor ProviderExecutor, auth *Auth) (*Auth, error) {
	return m.prepareRequestAuth(ctx, executor, auth)
}

func (m *Manager) prepareRequestAuth(ctx context.Context, executor ProviderExecutor, auth *Auth) (*Auth, error) {
	if m == nil || executor == nil || auth == nil {
		return auth, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if auth.Generation > 0 {
		if evaluator, ok := executor.(RefreshEvaluator); ok && evaluator != nil && evaluator.ShouldRefresh(
			time.Now(), auth,
		) {
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

	if errCtx := ctx.Err(); errCtx != nil {
		return auth, errCtx
	}
	m.requestPrepareLocksMu.Lock()
	lock := m.requestPrepareLocks[id]
	if lock == nil {
		lock = newAuthRefreshLock()
		m.requestPrepareLocks[id] = lock
	}
	lock.refs++
	m.requestPrepareLocksMu.Unlock()
	releaseRef := func() {
		m.requestPrepareLocksMu.Lock()
		lock.refs--
		if lock.refs == 0 && m.requestPrepareLocks[id] == lock {
			delete(m.requestPrepareLocks, id)
		}
		m.requestPrepareLocksMu.Unlock()
	}
	select {
	case <-ctx.Done():
		releaseRef()
		return auth, ctx.Err()
	case <-lock.token:
	}
	defer func() {
		lock.token <- struct{}{}
		releaseRef()
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

func selectionPredicate(ctx context.Context, opts cliproxyexecutor.Options) func(*Auth) bool {
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	if eligibility.unrestricted() {
		return nil
	}
	return eligibility.allows
}

func ExtractSessionID(headers http.Header, body []byte, metadata map[string]any) string {
	primary, _ := extractSessionIDs(headers, body, metadata)
	for _, prefix := range []string{
		"codex:", "header:", "affinity:", "conv:", "amp:", "clientreq:", "session:", "pck:",
	} {
		if after, ok := strings.CutPrefix(primary, prefix); ok {
			return after
		}
	}
	return primary
}

func extractSessionIDs(headers http.Header, body []byte, metadata map[string]any) (string, string) {
	if id := sessionHeaderValue(headers, "X-Claude-Code-Session-Id"); id != "" {
		return "claude:" + id, ""
	}
	if id := claudeCodeSessionIDFromPayload(body); id != "" {
		return id, ""
	}
	if id := sessionHeaderValue(headers, "Session-Id"); id != "" {
		return "codex:" + id, ""
	}
	if id := sessionHeaderValue(headers, "Session_id"); id != "" {
		return "codex:" + id, ""
	}
	if id := sessionHeaderValue(headers, "X-Session-ID"); id != "" {
		return "header:" + id, ""
	}
	if id := sessionHeaderValue(headers, "X-Session-Affinity"); id != "" {
		return "affinity:" + id, ""
	}
	if id := sessionHeaderValue(headers, "Conversation_id"); id != "" {
		return "conv:" + id, ""
	}
	if id := sessionHeaderValue(headers, "X-Amp-Thread-Id"); id != "" {
		return "amp:" + id, ""
	}
	if id := sessionHeaderValue(headers, "X-Client-Request-Id"); id != "" {
		return "clientreq:" + id, ""
	}

	if len(body) > 0 {
		conversationID := ""
		conversation := gjson.GetBytes(body, "conversation")
		if id := normalizedSessionCandidate(conversation.Get("id").String()); id != "" {
			conversationID = "conv:" + id
		} else if conversation.Type == gjson.String {
			if id := normalizedSessionCandidate(conversation.String()); id != "" {
				conversationID = "conv:" + id
			}
		}
		if id := codexPromptCacheKeyFromPayload(body); id != "" {
			return "pck:" + id, conversationID
		}
		for _, path := range []string{"session_id", "sessionId"} {
			if id := normalizedSessionCandidate(gjson.GetBytes(body, path).String()); id != "" {
				return "session:" + id, ""
			}
		}
		if conversationID != "" {
			return conversationID, ""
		}
		if id := normalizedSessionCandidate(gjson.GetBytes(body, "metadata.user_id").String()); id != "" {
			return "user:" + id, ""
		}
		if id := normalizedSessionCandidate(gjson.GetBytes(body, "conversation_id").String()); id != "" {
			return "conv:" + id, ""
		}
	}

	if id := normalizedMetadataSessionCandidate(metadata, cliproxyexecutor.ExecutionSessionMetadataKey); id != "" {
		return "execution:" + id, ""
	}
	for _, key := range []string{"derived_session_id", "session_root_id"} {
		if raw, ok := metadata[key].(string); ok {
			if id := normalizedSessionCandidate(raw); id != "" {
				return "derived:" + id, ""
			}
		}
	}
	return extractMessageHashIDs(body)
}

func normalizedMetadataSessionCandidate(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch value := raw.(type) {
	case string:
		return normalizedSessionCandidate(value)
	case []byte:
		return normalizedSessionCandidate(string(value))
	default:
		return ""
	}
}

func claudeCodeSessionIDFromPayload(body []byte) string {
	if id := cliproxysession.ClaudeMetadataSessionID(body); id != "" {
		return "claude:" + id
	}
	return ""
}

func codexPromptCacheKeyFromPayload(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if id := normalizedSessionCandidate(gjson.GetBytes(body, "prompt_cache_key").String()); id != "" {
		return id
	}
	if rawTurnMetadata := strings.TrimSpace(
		gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String(),
	); rawTurnMetadata != "" {
		if id := normalizedSessionCandidate(gjson.Get(rawTurnMetadata, "prompt_cache_key").String()); id != "" {
			return id
		}
		if id := normalizedSessionCandidate(gjson.Get(rawTurnMetadata, "window_id").String()); id != "" {
			return id
		}
	}
	return normalizedSessionCandidate(gjson.GetBytes(body, "client_metadata.x-codex-window-id").String())
}

func extractMessageHashIDs(body []byte) (string, string) {
	if len(body) == 0 {
		return "", ""
	}
	var systemPrompt, firstUserMsg, firstAssistantMsg string
	messages := gjson.GetBytes(body, "messages")
	if messages.IsArray() {
		messages.ForEach(
			func(_, message gjson.Result) bool {
				content := extractMessageContent(message.Get("content"))
				if content == "" {
					return true
				}
				switch strings.ToLower(strings.TrimSpace(message.Get("role").String())) {
				case "system", "developer":
					if systemPrompt == "" {
						systemPrompt = truncateSessionText(content)
					}
				case "user":
					if firstUserMsg == "" {
						firstUserMsg = truncateSessionText(content)
					}
				case "assistant":
					if firstAssistantMsg == "" {
						firstAssistantMsg = truncateSessionText(content)
					}
				}
				return systemPrompt == "" || firstUserMsg == "" || firstAssistantMsg == ""
			},
		)
	}
	if systemPrompt == "" {
		systemPrompt = truncateSessionText(extractMessageContent(gjson.GetBytes(body, "system")))
	}
	if systemPrompt == "" && firstUserMsg == "" {
		systemPrompt = truncateSessionText(extractMessageContent(gjson.GetBytes(body, "systemInstruction.parts")))
		gjson.GetBytes(body, "contents").ForEach(
			func(_, message gjson.Result) bool {
				content := extractMessageContent(message.Get("parts"))
				switch strings.ToLower(strings.TrimSpace(message.Get("role").String())) {
				case "user":
					if firstUserMsg == "" {
						firstUserMsg = truncateSessionText(content)
					}
				case "model":
					if firstAssistantMsg == "" {
						firstAssistantMsg = truncateSessionText(content)
					}
				}
				return firstUserMsg == "" || firstAssistantMsg == ""
			},
		)
	}
	if systemPrompt == "" && firstUserMsg == "" {
		systemPrompt = truncateSessionText(gjson.GetBytes(body, "instructions").String())
		input := gjson.GetBytes(body, "input")
		if input.Type == gjson.String {
			firstUserMsg = truncateSessionText(input.String())
		} else {
			input.ForEach(
				func(_, item gjson.Result) bool {
					content := extractMessageContent(item.Get("content"))
					switch strings.ToLower(strings.TrimSpace(item.Get("role").String())) {
					case "system", "developer":
						if systemPrompt == "" {
							systemPrompt = truncateSessionText(content)
						}
					case "user":
						if firstUserMsg == "" {
							firstUserMsg = truncateSessionText(content)
						}
					case "assistant":
						if firstAssistantMsg == "" {
							firstAssistantMsg = truncateSessionText(content)
						}
					}
					return firstUserMsg == "" || firstAssistantMsg == ""
				},
			)
		}
	}
	if systemPrompt == "" && firstUserMsg == "" {
		return "", ""
	}
	fallback := computeSessionHash(systemPrompt, firstUserMsg, "")
	if firstAssistantMsg == "" {
		return fallback, ""
	}
	return computeSessionHash(systemPrompt, firstUserMsg, firstAssistantMsg), fallback
}

func extractMessageContent(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	var texts []string
	content.ForEach(
		func(_, part gjson.Result) bool {
			if text := strings.TrimSpace(part.Get("text").String()); text != "" {
				texts = append(texts, text)
			}
			return true
		},
	)
	return strings.Join(texts, " ")
}

func truncateSessionText(value string) string {
	const maxLength = 100
	if len(value) > maxLength {
		return value[:maxLength]
	}
	return value
}

func computeSessionHash(systemPrompt, userMsg, assistantMsg string) string {
	hash := fnv.New64a()
	if systemPrompt != "" {
		_, _ = hash.Write([]byte("sys:" + systemPrompt + "\n"))
	}
	if userMsg != "" {
		_, _ = hash.Write([]byte("usr:" + userMsg + "\n"))
	}
	if assistantMsg != "" {
		_, _ = hash.Write([]byte("ast:" + assistantMsg + "\n"))
	}
	return fmt.Sprintf("msg:%016x", hash.Sum64())
}

func publishSelectedAuthMetadata(meta map[string]any, auth *Auth) {
	meta = routeTraceRootMetadata(meta)
	if len(meta) == 0 || auth == nil {
		return
	}
	if authID := strings.TrimSpace(auth.ID); authID != "" {
		meta[cliproxyexecutor.SelectedAuthMetadataKey] = authID
		if pinSelectedAuthFromMetadata(meta) && pinnedAuthIDFromMetadata(meta) == "" {
			meta[cliproxyexecutor.PinnedAuthMetadataKey] = authID
		}
		if callback, ok := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string)); ok && callback != nil {
			callback(authID)
		}
		if callback, ok := meta[cliproxyexecutor.SelectedAuthInfoCallbackMetadataKey].(func(cliproxyexecutor.SelectedAuthInfo)); ok && callback != nil {
			supportsWebsocket := authWebsocketsEnabled(auth)
			callback(
				cliproxyexecutor.SelectedAuthInfo{
					ID:                                    authID,
					SupportsIncrementalResponsesWebsocket: supportsWebsocket,
					SupportsResponsesContinuation: supportsWebsocket ||
						strings.EqualFold(strings.TrimSpace(auth.Provider), "codex"),
				},
			)
		}
	}
	if authIndex := strings.TrimSpace(auth.EnsureIndex()); authIndex != "" {
		meta[cliproxyexecutor.SelectedAuthIndexMetadataKey] = authIndex
		if callback, ok := meta[cliproxyexecutor.SelectedAuthIndexCallbackMetadataKey].(func(string)); ok && callback != nil {
			callback(authIndex)
		}
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

func (m *Manager) recordRouteDecisionCandidate(
	opts cliproxyexecutor.Options,
	auth *Auth,
	model string,
) {
	if routeTraceCallback(opts.Metadata) == nil || auth == nil {
		return
	}
	routedModel := rewriteModelForAuth(model, auth)
	aliasResult := m.resolveExecutionAliasResultForRequested(auth, routedModel)
	targetModel := strings.TrimSpace(aliasResult.UpstreamModel)
	if targetModel == "" {
		targetModel = routedModel
	}
	converted := aliasResult.ForceMapping || targetModel != strings.TrimSpace(model)
	recordRouteDecisionCandidate(opts, auth, model, targetModel, !converted, converted)
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

func (m *Manager) resolveExecutionAliasResultForRequested(auth *Auth, requestedModel string) OAuthModelAliasResult {
	if result := homeForceMappingAliasResult(auth, requestedModel); result.ForceMapping {
		return result
	}
	if auth != nil {
		kind, _ := auth.AccountInfo()
		if strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			return m.resolveAPIKeyModelAliasWithResult(auth, requestedModel)
		}
	}
	return m.applyOAuthModelAliasWithResult(auth, requestedModel)
}

func homeForceMappingAliasResult(auth *Auth, requestedModel string) OAuthModelAliasResult {
	if auth == nil || auth.Attributes == nil ||
		!strings.EqualFold(strings.TrimSpace(auth.Attributes[homeForceMappingAttributeKey]), "true") {
		return OAuthModelAliasResult{}
	}
	originalAlias := strings.TrimSpace(auth.Attributes[homeOriginalAliasAttributeKey])
	if originalAlias == "" {
		return OAuthModelAliasResult{}
	}
	upstreamModel := strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey])
	if upstreamModel == "" {
		upstreamModel = strings.TrimSpace(requestedModel)
	}
	return OAuthModelAliasResult{
		UpstreamModel: upstreamModel,
		ForceMapping:  true,
		OriginalAlias: originalAlias,
	}
}

func (m *Manager) applyAPIKeyModelAlias(auth *Auth, requestedModel string) string {
	result := m.resolveAPIKeyModelAliasWithResult(auth, requestedModel)
	if result.UpstreamModel == "" {
		return requestedModel
	}
	return result.UpstreamModel
}

func (m *Manager) resolveAPIKeyModelAliasWithResult(auth *Auth, requestedModel string) OAuthModelAliasResult {
	if m == nil || auth == nil {
		return OAuthModelAliasResult{}
	}

	kind, _ := auth.AccountInfo()
	if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
		return OAuthModelAliasResult{}
	}

	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return OAuthModelAliasResult{}
	}

	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	if cfg == nil {
		cfg = &internalconfig.Config{}
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	var models []modelAliasEntry
	switch provider {
	case "gemini":
		if entry := resolveGeminiAPIKeyConfig(cfg, auth); entry != nil {
			models = asModelAliasEntries(entry.Models)
		}
	case "gemini-interactions":
		if entry := resolveInteractionsAPIKeyConfig(cfg, auth); entry != nil {
			models = asModelAliasEntries(entry.Models)
		}
	case "claude":
		if entry := resolveClaudeAPIKeyConfig(cfg, auth); entry != nil {
			models = asModelAliasEntries(entry.Models)
		}
	case "codex":
		if entry := resolveCodexAPIKeyConfig(cfg, auth); entry != nil {
			models = asModelAliasEntries(entry.Models)
		}
	case "xai":
		if entry := resolveXAIAPIKeyConfig(cfg, auth); entry != nil {
			models = asModelAliasEntries(entry.Models)
		}
	case "meta":
		if entry := resolveAPIKeyConfig(cfg.MetaKey, auth); entry != nil {
			models = asModelAliasEntries(entry.Models)
		}
	case "vertex":
		if entry := resolveVertexAPIKeyConfig(cfg, auth); entry != nil {
			models = asModelAliasEntries(entry.Models)
		}
	default:
		providerKey := ""
		compatName := ""
		if auth.Attributes != nil {
			providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
			compatName = strings.TrimSpace(auth.Attributes["compat_name"])
		}
		if compatName != "" || strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
			if entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider); entry != nil {
				models = asModelAliasEntries(entry.Models)
			}
		}
	}
	if len(models) == 0 {
		return OAuthModelAliasResult{UpstreamModel: requestedModel}
	}
	result := resolveModelAliasResultFromConfigModels(requestedModel, models)
	if result.UpstreamModel == "" {
		result.UpstreamModel = requestedModel
	}
	return result
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

func resolveXAIAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.XAIKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.XAIKey, auth)
}

func resolveVertexAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.VertexCompatKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.VertexCompatAPIKey, auth)
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

func (m *Manager) unavailableSelectionError(candidates []*Auth, provider string, model string) error {
	candidateIDs := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate != nil && candidate.ID != "" {
			candidateIDs[candidate.ID] = struct{}{}
		}
	}

	m.mu.RLock()
	current := make([]*Auth, 0, len(candidateIDs))
	for candidateID := range candidateIDs {
		if auth := m.auths[candidateID]; auth != nil {
			current = append(current, auth.Clone())
		}
	}
	m.mu.RUnlock()

	now := time.Now()
	_, cooldownCount, earliest := collectAvailableByPriority(
		current, provider, model, cliproxyexecutor.Options{}, now,
	)
	lastCandidateErr := latestSelectionCandidateError(current, model)
	if len(current) > 0 && cooldownCount == len(current) && !earliest.IsZero() {
		providerForError := strings.TrimSpace(strings.ToLower(provider))
		if providerForError == "mixed" {
			providerForError = ""
		}
		return newModelCooldownErrorWithCause(
			model, providerForError, max(earliest.Sub(now), 0), lastCandidateErr,
		)
	}
	return newAuthUnavailableErrorWithCause(earliest, now, lastCandidateErr)
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

func (m *Manager) closestCooldownWait(ctx context.Context, opts cliproxyexecutor.Options, providers []string, model string, attempt int, status int) (time.Duration, bool) {
	if m == nil || len(providers) == 0 {
		return 0, false
	}
	now := time.Now()
	defaultRetry := int(m.requestRetry.Load())
	state := retryRoundFromContext(ctx)
	if state != nil {
		defaultRetry = state.defaultRetry
	}
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerSet[strings.ToLower(strings.TrimSpace(provider))] = struct{}{}
	}
	eligibility := authSelectionEligibilityForRequest(ctx, opts)
	eligibility.retryRound = 0
	pinned := pinnedAuthIDFromMetadata(opts.Metadata)
	modelKey := canonicalModelKey(model)
	registryRef := registry.GetGlobalRegistry()
	m.mu.RLock()
	defer m.mu.RUnlock()
	found := false
	var minWait time.Duration
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled || (pinned != "" && auth.ID != pinned) || !eligibility.allows(auth) {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if _, ok := providerSet[provider]; !ok {
			continue
		}
		if m.executors[provider] == nil {
			continue
		}
		if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(auth.ID, modelKey) {
			continue
		}
		if thinking.HasContext1MTag(model) && !authSupportsContext1MModel(auth, model) {
			continue
		}
		if attempt >= effectiveRequestRetryLimit(auth, defaultRetry) {
			continue
		}
		next, allowed := retryRoundStateEligible(auth, model, now)
		if !allowed {
			continue
		}
		wait := time.Duration(0)
		if !next.IsZero() {
			wait = max(next.Sub(now), 0)
		}
		if state != nil && status == http.StatusTooManyRequests && !m.cooldownDisabledForAuth(auth) {
			if _, attempted := state.attempted[auth.ID]; attempted {
				wait = max(wait, 10*time.Second)
			}
		}
		if !found || wait < minWait {
			found, minWait = true, wait
		}
	}
	return minWait, found
}

func (m *Manager) shouldRetryAfterError(ctx context.Context, opts cliproxyexecutor.Options, err error, attempt int, providers []string, model string, maxWait time.Duration) (time.Duration, bool) {
	if err == nil || (ctx != nil && ctx.Err() != nil) || isRequestScopedError(err) || isRequestInvalidError(err) ||
		isResponsesCompactRequestFaultError(opts, err) {
		return 0, false
	}
	if statusCodeFromError(err) == http.StatusOK {
		return 0, false
	}
	var homeBusy *HomeConcurrencyBusyError
	if errors.As(err, &homeBusy) {
		return 0, false
	}
	disposition := executionDispositionForError(err, cliproxyexecutor.DispatchPhaseMaybeSent)
	if !retryDirectiveAllowsCooldownRetry(disposition) {
		return 0, false
	}
	var wait time.Duration
	var found bool
	if m.HomeEnabled() {
		state := retryRoundFromContext(ctx)
		if state == nil || state.homeStopped {
			return 0, false
		}
		limit := state.homeLimit
		if limit < 0 {
			limit = max(state.defaultRetry, 0)
		}
		if attempt >= limit {
			return 0, false
		}
		if attempt > 0 && len(state.attempted) == 0 {
			return 0, false
		}
		wait, found = state.homeWait, true
	} else {
		wait, found = m.closestCooldownWait(ctx, opts, providers, model, attempt, statusCodeFromError(err))
	}
	if !found || wait < 0 || (wait > 0 && (maxWait <= 0 || wait > maxWait)) {
		return 0, false
	}
	return wait, true
}

const cooldownWaitJitterCap = 2 * time.Second

func jitteredCooldownWait(wait, maxWait time.Duration) time.Duration {
	if wait <= 0 {
		return wait
	}
	jitterRange := min(wait/4, cooldownWaitJitterCap)
	if maxWait > 0 && jitterRange > maxWait-wait {
		jitterRange = maxWait - wait
	}
	if jitterRange <= 0 {
		return wait
	}
	return wait + rand.N(jitterRange)
}

func waitForCooldown(ctx context.Context, wait, maxWait time.Duration) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
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

func (m *Manager) resultCredentialMatches(auth *Auth, result Result) bool {
	if auth == nil {
		return false
	}
	if result.generationSnapshot.valid &&
		result.generationSnapshot.accessTokenSHA256 != AccessTokenSHA256(auth) {
		return false
	}
	if result.generationSnapshot.credentialGeneration != 0 {
		return auth.credentialGeneration == result.generationSnapshot.credentialGeneration
	}
	if result.AuthGeneration != 0 {
		return auth.Generation == result.AuthGeneration
	}
	first := m.firstCredentialGenerations[auth.ID]
	return first != 0 && auth.credentialGeneration == first
}

// MarkResult records an execution result and notifies hooks.
func (m *Manager) MarkResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if policy := m.ResultPolicy(); policy != nil {
		var keep bool
		result, keep = applyResultPolicy(ctx, policy, result)
		if !keep {
			return
		}
	}
	forceCooldown := isForceCooldownResult(result)
	result.Disposition = normalizeResultDisposition(result)
	if result.Error != nil {
		result.Error = errorForResultWithDisposition(result.Error, result.Disposition, result.StateGeneration)
		if forceCooldown {
			result.Error.Code = ErrorCodeForceCooldown
		}
	}
	if !forceCooldown && dispositionIsAvailabilityNeutral(result.Disposition) {
		m.recordAvailabilityNeutralResult(ctx, result)
		return
	}
	if result.RuntimeOnly {
		m.hook.OnResult(ctx, result)
		m.updateSessionAffinity(result)
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

	var persistAuth *Auth
	publicationTime := time.Time{}
	cooldownStateChanged := false

	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		if !m.resultCredentialMatches(auth, result) {
			m.mu.Unlock()
			unlockMutation()
			mutationLocked = false
			m.hook.OnResult(ctx, result)
			return
		}
		if !resultStateGenerationMatches(auth, result) {
			m.mu.Unlock()
			unlockMutation()
			mutationLocked = false
			m.hook.OnResult(ctx, result)
			return
		}
		if resultScopeUsesModelQuotaEpoch(result.Disposition.EffectScope) &&
			result.ModelQuotaEpoch != modelQuotaEpoch(auth, result.Model) &&
			(result.Success || statusCodeFromResult(result.Error) == http.StatusTooManyRequests) {
			m.mu.Unlock()
			unlockMutation()
			mutationLocked = false
			m.hook.OnResult(ctx, result)
			return
		}

		now := time.Now()
		trackCooldownState := m.cooldownStore != nil
		var cooldownRecordsBefore []CooldownStateRecord
		if trackCooldownState {
			cooldownRecordsBefore = m.cooldownStateRecordsForAuthLocked(auth, now)
		}
		auth.recordRecentRequest(now, result.Success)
		if result.Success {
			auth.Success++
		} else {
			auth.Failed++
		}
		disableCooling := m.cooldownDisabledForAuth(auth)
		if isForceCooldownResult(result) {
			disableCooling = false
		}
		applyResultStateLocked(auth, result, now, disableCooling)
		observeQuotaResultLocked(auth, result, ctx, now)
		persistAuth = auth.Clone()
		publicationTime = now
		if trackCooldownState {
			cooldownRecordsAfter := m.cooldownStateRecordsForAuthLocked(auth, now)
			cooldownStateChanged = !cooldownStateRecordsEqual(cooldownRecordsBefore, cooldownRecordsAfter)
		}
	}
	m.mu.Unlock()
	if persistAuth != nil && m.scheduler != nil {
		targetModels, fullClient := schedulerTargetsForResult(persistAuth, result)
		m.scheduler.upsertAuthResult(persistAuth, targetModels, fullClient)
	}
	publishRegistryResult := false
	registryProjectionGeneration := uint64(0)
	var savedFieldsAuth *Auth
	if persistAuth != nil {
		persisted, errPersist := m.persist(ctx, persistAuth, false, false, 0)
		if errPersist != nil {
			log.WithError(errPersist).Warnf("persist auth result for %s", result.AuthID)
		} else if errPublication := m.authPublicationContextError(
			ctx, result.AuthID, persisted,
		); errPublication != nil {
			log.WithError(errPublication).Warnf("reject persisted auth result for %s", result.AuthID)
		}

		m.authIntentMu.Lock()
		m.mu.Lock()
		current := m.auths[result.AuthID]
		_, removed := m.removedAuths[result.AuthID]
		publishRegistryResult = current != nil && !removed && current.Generation == persistAuth.Generation &&
			stateGenerationForDisposition(current, result.Model, result.Disposition) ==
				stateGenerationForDisposition(persistAuth, result.Model, result.Disposition)
		if publishRegistryResult {
			if persisted && errPersist == nil {
				current.Metadata = persistAuth.Metadata
				current.Attributes = persistAuth.Attributes
				current.FileName = persistAuth.FileName
				current.Storage = persistAuth.Storage
				savedFieldsAuth = current.Clone()
			}
			registryProjectionGeneration = m.nextAuthGenerationLocked()
		}
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		if removed && persisted {
			log.WithError(m.scheduleRejectedPersistedAuthDeletion(result.AuthID, persistAuth)).Warnf(
				"delete auth result persisted after removal for %s", result.AuthID,
			)
		}
	}
	if savedFieldsAuth != nil && m.scheduler != nil {
		m.scheduler.upsertAuth(savedFieldsAuth)
	}
	if cooldownStateChanged {
		m.persistCooldownStatesAfterMutation(context.Background())
	}
	if publishRegistryResult {
		m.publishClientModelProjections(persistAuth, registryProjectionGeneration, publicationTime)
	}

	unlockMutation()
	mutationLocked = false
	m.hook.OnResult(ctx, result)
	m.updateSessionAffinity(result)
}

func (m *Manager) updateSessionAffinity(result Result) {
	if m == nil {
		return
	}
	selector := m.Selector()
	if affinity, ok := selector.(interface{ OnResult(Result) }); ok && affinity != nil {
		affinity.OnResult(result)
	}
}

func observeQuotaResultLocked(
	auth *Auth,
	result Result,
	ctx context.Context,
	observedAt time.Time,
) bool {
	if auth == nil || result.SkipQuotaObservation {
		return false
	}
	headers := upstreamResponseHeaders(ctx)
	changed := auth.Quota.ObserveResponseHeadersForProvider(result.Provider, headers, observedAt)
	if state, _ := findModelState(auth, result.Model); state != nil {
		changed = state.Quota.ObserveResponseHeadersForProvider(result.Provider, headers, observedAt) || changed
	}
	return changed
}

func schedulerTargetsForResult(auth *Auth, result Result) ([]string, bool) {
	switch result.Disposition.EffectScope {
	case cliproxyexecutor.StateEffectScopeExactModel:
		_, model := findModelState(auth, result.Model)
		if model == "" {
			model = canonicalModelKey(result.Model)
		}
		if model == "" {
			return nil, true
		}
		return []string{model}, false
	case cliproxyexecutor.StateEffectScopeQuotaFamily:
		family := strings.TrimSpace(result.Disposition.EffectKey)
		if family == "" {
			family = quotaFamilyKey(auth, result.Model)
		}
		states := quotaFamilyModelStates(auth, result.Model, family, false)
		models := make([]string, 0, len(states))
		for model := range states {
			if model = canonicalModelKey(model); model != "" {
				models = append(models, model)
			}
		}
		if len(models) == 0 {
			if model := canonicalModelKey(result.Model); model != "" {
				models = append(models, model)
			}
		}
		sort.Strings(models)
		if len(models) == 0 {
			return nil, true
		}
		return models, false
	case cliproxyexecutor.StateEffectScopeCredential:
		return nil, true
	default:
		return nil, true
	}
}

func resultScopeUsesModelQuotaEpoch(scope cliproxyexecutor.StateEffectScope) bool {
	return scope == cliproxyexecutor.StateEffectScopeExactModel ||
		scope == cliproxyexecutor.StateEffectScopeQuotaFamily
}

func applyResultStateLocked(
	auth *Auth,
	result Result,
	now time.Time,
	disableCooling bool,
) {
	if auth == nil || hasTerminalUnauthorizedAuthFailure(auth) {
		return
	}

	if result.Success {
		switch result.Disposition.EffectScope {
		case cliproxyexecutor.StateEffectScopeCredential:
			clearAuthStateOnSuccess(auth, now)
		case cliproxyexecutor.StateEffectScopeQuotaFamily:
			applyQuotaFamilySuccess(auth, result, now)
		case cliproxyexecutor.StateEffectScopeExactModel:
			state, _ := modelStateForDisposition(auth, result.Model, result.Disposition, false)
			if state != nil && clearModelStateOnSuccess(state, now) {
				refreshAuthFromModelStates(auth, now)
			}
		}
		return
	}

	switch result.Disposition.EffectScope {
	case cliproxyexecutor.StateEffectScopeCredential:
		applyAuthFailureState(auth, result.Error, result.RetryAfter, now, disableCooling)
	case cliproxyexecutor.StateEffectScopeQuotaFamily:
		applyQuotaFamilyFailure(auth, result, now, disableCooling)
	case cliproxyexecutor.StateEffectScopeExactModel:
		state, model := modelStateForDisposition(auth, result.Model, result.Disposition, true)
		if state == nil {
			return
		}
		applyExactModelFailure(state, model, result, now, disableCooling)
		refreshAuthFromModelStates(auth, now)
	}
}

func applyExactModelFailure(
	state *ModelState,
	model string,
	result Result,
	now time.Time,
	disableCooling bool,
) {
	if state == nil {
		return
	}
	model = canonicalModelKey(model)
	if model == "" {
		model = canonicalModelKey(result.Model)
	}
	previousRetryAfter := state.NextRetryAfter
	state.Status = StatusError
	state.Unavailable = !disableCooling
	state.NextRetryAfter = time.Time{}
	state.StatusMessage = "model unavailable"
	state.LastError = cloneError(result.Error)
	state.EffectScope = cliproxyexecutor.StateEffectScopeExactModel
	state.EffectKey = model
	state.ResetAuthority = StateResetAuthoritySuccess
	state.Generation = nextStateGeneration(state.Generation)
	state.UpdatedAt = now

	if result.Error != nil {
		state.StatusMessage = result.Error.Message
	}
	switch statusCodeFromResult(result.Error) {
	case http.StatusNotFound:
		if !disableCooling {
			if result.RetryAfter != nil && *result.RetryAfter > 0 {
				state.NextRetryAfter = now.Add(*result.RetryAfter)
			} else {
				state.NextRetryAfter = now.Add(12 * time.Hour)
			}
		}
	case http.StatusTooManyRequests:
		quota := state.Quota
		var next time.Time
		backoffLevel := quota.BackoffLevel
		if !disableCooling {
			if result.RetryAfter != nil {
				next = now.Add(max(*result.RetryAfter, minQuotaCooldownFloor))
			} else {
				next, backoffLevel = quotaCooldownAfterFailure(quota, now)
			}
			if quota.Exceeded && quota.NextRecoverAt.After(next) {
				next = quota.NextRecoverAt
			}
			next = next.Round(0)
		}
		state.QuotaEpoch++
		state.NextRetryAfter = next
		applyCooldownFields(&state.Quota, QuotaState{
			Exceeded:       true,
			Reason:         "quota",
			NextRecoverAt:  next,
			BackoffLevel:   backoffLevel,
			Generation:     nextStateGeneration(quota.Generation),
			ResetAuthority: StateResetAuthoritySuccess,
		})
	case 408, 500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526:
		state.NextRetryAfter = recoverableFailureRetryAfterWithHint(now, result.RetryAfter, disableCooling)
		state.Unavailable = !state.NextRetryAfter.IsZero()
	}
	if isForceCooldownResult(result) && state.NextRetryAfter.IsZero() {
		state.NextRetryAfter = now.Add(transientErrorCooldown)
	}
	if !state.NextRetryAfter.IsZero() && previousRetryAfter.After(now) && previousRetryAfter.After(state.NextRetryAfter) {
		state.NextRetryAfter = previousRetryAfter
	}
	if state.LastError != nil {
		state.LastError.StateGeneration = state.Generation
	}
}

func applyQuotaFamilyFailure(
	auth *Auth,
	result Result,
	now time.Time,
	disableCooling bool,
) {
	family := strings.TrimSpace(result.Disposition.EffectKey)
	if family == "" {
		family = quotaFamilyKey(auth, result.Model)
	}
	states := quotaFamilyModelStates(auth, result.Model, family, true)
	if len(states) == 0 {
		return
	}
	quota := quotaFamilyQuotaState(states)
	var next time.Time
	backoffLevel := quota.BackoffLevel
	if !disableCooling {
		if result.RetryAfter != nil {
			next = now.Add(max(*result.RetryAfter, minQuotaCooldownFloor))
		} else {
			next, backoffLevel = quotaCooldownAfterFailure(quota, now)
		}
		for _, state := range states {
			if state.Quota.Exceeded && state.Quota.NextRecoverAt.After(next) {
				next = state.Quota.NextRecoverAt
			}
		}
		next = next.Round(0)
	}
	generation := nextStateGeneration(quotaFamilyGeneration(auth, family))
	models := make([]string, 0, len(states))
	for model := range states {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		state := states[model]
		state.QuotaEpoch++
		state.Status = StatusError
		state.StatusMessage = "quota exhausted"
		state.Unavailable = !disableCooling
		previousRetryAfter := state.NextRetryAfter
		state.NextRetryAfter = next
		if !next.IsZero() && previousRetryAfter.After(now) && previousRetryAfter.After(next) {
			state.NextRetryAfter = previousRetryAfter.Round(0)
		}
		state.LastError = cloneError(result.Error)
		state.EffectScope = cliproxyexecutor.StateEffectScopeQuotaFamily
		state.EffectKey = family
		state.ResetAuthority = StateResetAuthoritySuccess
		state.Generation = nextStateGeneration(state.Generation)
		applyCooldownFields(&state.Quota, QuotaState{
			Exceeded:       true,
			Reason:         "quota",
			Family:         family,
			NextRecoverAt:  next,
			BackoffLevel:   backoffLevel,
			Generation:     generation,
			ResetAuthority: StateResetAuthoritySuccess,
		})
		state.UpdatedAt = now
		if state.LastError != nil {
			state.LastError.StateGeneration = generation
		}
	}
	refreshAuthFromModelStates(auth, now)
}

func applyQuotaFamilySuccess(
	auth *Auth,
	result Result,
	now time.Time,
) {
	family := strings.TrimSpace(result.Disposition.EffectKey)
	if family == "" {
		family = quotaFamilyKey(auth, result.Model)
	}
	states := quotaFamilyModelStates(auth, result.Model, family, false)
	if len(states) == 0 {
		return
	}
	for _, state := range states {
		if !resetAuthorityAllows(state.ResetAuthority, StateResetAuthoritySuccess) ||
			!resetAuthorityAllows(state.Quota.ResetAuthority, StateResetAuthoritySuccess) {
			return
		}
	}
	generation := nextStateGeneration(quotaFamilyGeneration(auth, family))
	models := make([]string, 0, len(states))
	for model := range states {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		state := states[model]
		backoffLevel := state.Quota.BackoffLevel
		if backoffLevel > 0 {
			backoffLevel--
		}
		state.Unavailable = false
		state.Status = StatusActive
		state.StatusMessage = ""
		state.NextRetryAfter = time.Time{}
		state.LastError = nil
		applyCooldownFields(&state.Quota, QuotaState{
			Family:       family,
			BackoffLevel: backoffLevel,
			Generation:   generation,
		})
		state.EffectScope = ""
		state.EffectKey = ""
		state.ResetAuthority = ""
		state.Generation = nextStateGeneration(state.Generation)
		state.UpdatedAt = now
	}
	refreshAuthFromModelStates(auth, now)
}

func quotaFamilyQuotaState(states map[string]*ModelState) QuotaState {
	var selected QuotaState
	for _, state := range states {
		if state == nil {
			continue
		}
		quota := state.Quota
		if quota.BackoffLevel > selected.BackoffLevel ||
			(quota.BackoffLevel == selected.BackoffLevel && quota.NextRecoverAt.After(selected.NextRecoverAt)) {
			selected = quota
		}
	}
	return selected
}

func refreshAuthFromModelStates(auth *Auth, now time.Time) {
	if auth == nil || hasTerminalUnauthorizedAuthFailure(auth) {
		return
	}
	updateAggregatedAvailability(auth, now)
	if auth.StateEffectScope == cliproxyexecutor.StateEffectScopeCredential && auth.LastError != nil {
		return
	}
	if hasModelError(auth, now) {
		updateAuthErrorFromModelStates(auth, now)
		return
	}
	auth.Status = StatusActive
	auth.StatusMessage = ""
	auth.LastError = nil
	auth.UpdatedAt = now
}

const availabilityNeutralResultPersistenceTimeout = 5 * time.Second

func (m *Manager) recordAvailabilityNeutralResult(ctx context.Context, result Result) {
	if result.AuthID == "" {
		return
	}
	if result.RuntimeOnly {
		m.hook.OnResult(ctx, result)
		return
	}

	persistenceParent := context.Background()
	if ctx != nil {
		persistenceParent = context.WithoutCancel(ctx)
	}
	persistenceCtx, cancelPersistence := context.WithTimeout(
		persistenceParent, availabilityNeutralResultPersistenceTimeout,
	)
	defer cancelPersistence()

	unlockMutation, errMutation := m.lockAuthMutation(persistenceCtx, result.AuthID)
	if errMutation != nil {
		log.WithError(errMutation).Warnf("skip availability-neutral auth result for %s", result.AuthID)
		m.hook.OnResult(ctx, result)
		return
	}
	mutationLocked := true
	defer func() {
		if mutationLocked {
			unlockMutation()
		}
	}()

	var persistAuth *Auth
	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		if !m.resultCredentialMatches(auth, result) {
			m.mu.Unlock()
			unlockMutation()
			mutationLocked = false
			m.hook.OnResult(ctx, result)
			return
		}
		now := time.Now()
		auth.recordRecentRequest(now, result.Success)
		if result.Success {
			auth.Success++
		} else {
			auth.Failed++
		}
		observeQuotaResultLocked(auth, result, ctx, now)
		persistAuth = auth.Clone()
	}
	m.mu.Unlock()
	if persistAuth != nil {
		persisted, errPersist := m.persist(persistenceCtx, persistAuth, false, false, 0)
		if errPersist != nil {
			log.WithError(errPersist).Warnf("persist availability-neutral auth result for %s", result.AuthID)
		}
		var savedFieldsAuth *Auth
		m.authIntentMu.Lock()
		m.mu.Lock()
		current := m.auths[result.AuthID]
		_, removed := m.removedAuths[result.AuthID]
		persistedResultRejected := current == nil || removed || current.Generation != persistAuth.Generation
		if persisted && errPersist == nil && !persistedResultRejected {
			current.Metadata = persistAuth.Metadata
			current.Attributes = persistAuth.Attributes
			current.FileName = persistAuth.FileName
			savedFieldsAuth = current.Clone()
		}
		m.mu.Unlock()
		m.authIntentMu.Unlock()
		if persisted && persistedResultRejected {
			log.WithError(m.scheduleRejectedPersistedAuthDeletion(result.AuthID, persistAuth)).Warnf(
				"delete availability-neutral auth result persisted after removal or replacement for %s", result.AuthID,
			)
		}
		if savedFieldsAuth != nil && m.scheduler != nil {
			m.scheduler.upsertAuth(savedFieldsAuth)
		}
	}
	unlockMutation()
	mutationLocked = false
	m.hook.OnResult(ctx, result)
}

func isCountTokensEndpointNotFoundError(err error, requestedModel string) bool {
	if err == nil || statusCodeFromError(err) != http.StatusNotFound {
		return false
	}
	baseModel := thinking.ParseSuffix(requestedModel).ModelName
	return !isExplicitModelNotFoundError(err, baseModel)
}

func isExplicitModelNotFoundError(err error, requestedModel string) bool {
	if err == nil {
		return false
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		if isModelNotFoundIdentifier(authErr.Code) || isStructuredModelNotFoundError(authErr.Message, requestedModel) {
			return true
		}
	} else if isStructuredModelNotFoundError(err.Error(), requestedModel) {
		return true
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		for _, nested := range wrapped.Unwrap() {
			if isExplicitModelNotFoundError(nested, requestedModel) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return isExplicitModelNotFoundError(wrapped.Unwrap(), requestedModel)
	}
	return false
}

func isStructuredModelNotFoundError(message, requestedModel string) bool {
	var payload any
	if err := json.Unmarshal([]byte(strings.TrimSpace(message)), &payload); err != nil {
		return false
	}
	return containsStructuredModelNotFound(payload, requestedModel)
}

func containsStructuredModelNotFound(value any, requestedModel string) bool {
	switch typed := value.(type) {
	case map[string]any:
		notFoundType := false
		exactModelReference := false
		for key, item := range typed {
			text, isString := item.(string)
			if isString {
				switch strings.ToLower(strings.TrimSpace(key)) {
				case "code":
					if isModelNotFoundIdentifier(text) {
						return true
					}
				case "type":
					if isModelNotFoundIdentifier(text) {
						return true
					}
					notFoundType = notFoundType || isNotFoundErrorIdentifier(text)
				case "error", "message", "detail", "error_description", "title":
					if isExplicitModelNotFoundMessage(text, requestedModel) {
						return true
					}
					exactModelReference = exactModelReference || isExactRequestedModelReference(text, requestedModel)
				}
			}
			switch item.(type) {
			case map[string]any, []any:
				if containsStructuredModelNotFound(item, requestedModel) {
					return true
				}
			}
		}
		return notFoundType && exactModelReference
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok && isExplicitModelNotFoundMessage(text, requestedModel) {
				return true
			}
			if containsStructuredModelNotFound(item, requestedModel) {
				return true
			}
		}
	}
	return false
}

func isModelNotFoundIdentifier(value string) bool {
	candidate := strings.ToLower(strings.TrimSpace(value))
	if fragment := strings.LastIndex(candidate, "#"); fragment >= 0 && fragment+1 < len(candidate) {
		candidate = candidate[fragment+1:]
	} else {
		if query := strings.Index(candidate, "?"); query >= 0 {
			candidate = candidate[:query]
		}
		candidate = strings.TrimRight(candidate, "/")
		if separator := strings.LastIndexAny(candidate, "/:"); separator >= 0 {
			candidate = candidate[separator+1:]
		}
	}
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(candidate)
	switch normalized {
	case "model_not_found", "model_not_found_error", "unknown_model", "model_does_not_exist", "model_not_exist":
		return true
	default:
		return false
	}
}

func isNotFoundErrorIdentifier(value string) bool {
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
	return normalized == "not_found" || normalized == "not_found_error"
}

func isExplicitModelNotFoundMessage(message, requestedModel string) bool {
	lower := strings.Trim(strings.ToLower(strings.TrimSpace(message)), " .!;\t\r\n")
	if lower == "" {
		return false
	}
	normalized := strings.NewReplacer("-", "_", " ", "_").Replace(lower)
	if strings.Contains(normalized, "model_not_found") || strings.Contains(normalized, "unknown_model") {
		return true
	}
	requested := strings.ToLower(strings.TrimSpace(requestedModel))
	for _, phrase := range []string{"does not exist", "was not found", "could not be found", "is unavailable"} {
		if strings.Contains(lower, phrase) && (requested == "" || strings.Contains(
			lower, requested,
		) || strings.Contains(lower, "requested model")) {
			return true
		}
	}
	return strings.HasPrefix(lower, "no such model") || strings.HasPrefix(lower, "unknown model")
}

func isExactRequestedModelReference(message, requestedModel string) bool {
	message = strings.Trim(strings.ToLower(strings.TrimSpace(message)), " .!;\t\r\n\"'`")
	requestedModel = strings.ToLower(strings.TrimSpace(requestedModel))
	if requestedModel == "" {
		return false
	}
	for _, prefix := range []string{"the requested model", "requested model", "the model", "model"} {
		if message == prefix {
			return false
		}
		if strings.HasPrefix(message, prefix+":") || strings.HasPrefix(message, prefix+" ") {
			remainder := strings.TrimSpace(strings.TrimPrefix(message, prefix))
			remainder = strings.Trim(strings.TrimSpace(strings.TrimPrefix(remainder, ":")), "\"'`")
			return remainder == requestedModel
		}
	}
	return false
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
	clearModelState(state, now, StateResetAuthorityReset)
}

func clearModelState(state *ModelState, now time.Time, action StateResetAuthority) bool {
	if state == nil || !resetAuthorityAllows(state.ResetAuthority, action) ||
		!resetAuthorityAllows(state.Quota.ResetAuthority, action) {
		return false
	}
	quotaGeneration := nextStateGeneration(state.Quota.Generation)
	family := state.Quota.Family
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	applyCooldownFields(&state.Quota, QuotaState{Family: family, Generation: quotaGeneration})
	state.EffectScope = ""
	state.EffectKey = ""
	state.ResetAuthority = ""
	state.Generation = nextStateGeneration(state.Generation)
	state.UpdatedAt = now
	return true
}

func clearModelStateOnSuccess(state *ModelState, now time.Time) bool {
	if state == nil || !resetAuthorityAllows(state.ResetAuthority, StateResetAuthoritySuccess) ||
		!resetAuthorityAllows(state.Quota.ResetAuthority, StateResetAuthoritySuccess) {
		return false
	}
	backoffLevel := state.Quota.BackoffLevel
	if backoffLevel > 0 {
		backoffLevel--
	}
	quotaGeneration := nextStateGeneration(state.Quota.Generation)
	family := state.Quota.Family
	state.Unavailable = false
	state.Status = StatusActive
	state.StatusMessage = ""
	state.NextRetryAfter = time.Time{}
	state.LastError = nil
	applyCooldownFields(&state.Quota, QuotaState{
		Family:       family,
		BackoffLevel: backoffLevel,
		Generation:   quotaGeneration,
	})
	state.EffectScope = ""
	state.EffectKey = ""
	state.ResetAuthority = ""
	state.Generation = nextStateGeneration(state.Generation)
	state.UpdatedAt = now
	return true
}

func updateAggregatedAvailability(auth *Auth, now time.Time) {
	if auth == nil || hasTerminalUnauthorizedAuthFailure(auth) || len(auth.ModelStates) == 0 {
		return
	}
	credentialState := auth.StateEffectScope == cliproxyexecutor.StateEffectScopeCredential &&
		(auth.Unavailable || auth.LastError != nil || auth.ResetAuthority != "")
	credentialQuota := auth.StateEffectScope == cliproxyexecutor.StateEffectScopeCredential && auth.Quota.Exceeded
	allUnavailable := true
	earliestRetry := time.Time{}
	quotaExceeded := false
	quotaRecover := time.Time{}
	maxBackoffLevel := 0
	quotaFamily := ""
	quotaGeneration := uint64(0)
	quotaResetAuthority := StateResetAuthority("")
	multipleQuotaFamilies := false
	for model, state := range auth.ModelStates {
		if state == nil {
			continue
		}
		stateUnavailable := false
		if state.Status == StatusDisabled {
			stateUnavailable = true
		} else if state.Unavailable {
			if state.NextRetryAfter.IsZero() {
				stateUnavailable = true
			} else if state.NextRetryAfter.After(now) {
				stateUnavailable = true
				if earliestRetry.IsZero() || state.NextRetryAfter.Before(earliestRetry) {
					earliestRetry = state.NextRetryAfter
				}
			}
		}
		if !stateUnavailable {
			allUnavailable = false
		}
		if state.Quota.Exceeded {
			quotaExceeded = true
			family := quotaFamilyKeyForState(auth, model, state)
			if quotaFamily == "" {
				quotaFamily = family
			} else if family != quotaFamily {
				multipleQuotaFamilies = true
			}
			if generation := normalizedStateGeneration(state.Quota.Generation); generation > quotaGeneration {
				quotaGeneration = generation
			}
			if resetAuthorityRank(state.Quota.ResetAuthority) > resetAuthorityRank(quotaResetAuthority) {
				quotaResetAuthority = state.Quota.ResetAuthority
			}
			if quotaRecover.IsZero() ||
				(!state.Quota.NextRecoverAt.IsZero() && state.Quota.NextRecoverAt.Before(quotaRecover)) {
				quotaRecover = state.Quota.NextRecoverAt
			}
			if state.Quota.BackoffLevel > maxBackoffLevel {
				maxBackoffLevel = state.Quota.BackoffLevel
			}
		}
	}
	if !credentialState {
		auth.Unavailable = allUnavailable
		if allUnavailable {
			auth.NextRetryAfter = earliestRetry
		} else {
			auth.NextRetryAfter = time.Time{}
		}
	}
	if credentialQuota {
		return
	}
	if quotaExceeded {
		if multipleQuotaFamilies {
			quotaFamily = ""
		}
		applyCooldownFields(&auth.Quota, QuotaState{
			Exceeded:       true,
			Reason:         "quota",
			Family:         quotaFamily,
			NextRecoverAt:  quotaRecover,
			BackoffLevel:   maxBackoffLevel,
			Generation:     quotaGeneration,
			ResetAuthority: quotaResetAuthority,
		})
	} else {
		applyCooldownFields(&auth.Quota, QuotaState{})
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
	if auth == nil || hasTerminalUnauthorizedAuthFailure(auth) {
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

func clearAuthStateOnSuccess(auth *Auth, now time.Time) bool {
	return clearAuthState(auth, now, StateResetAuthoritySuccess)
}

func clearAuthState(auth *Auth, now time.Time, action StateResetAuthority) bool {
	if auth == nil || hasTerminalUnauthorizedAuthFailure(auth) || !resetAuthorityAllows(auth.ResetAuthority, action) ||
		!resetAuthorityAllows(auth.Quota.ResetAuthority, action) {
		return false
	}
	backoffLevel := auth.Quota.BackoffLevel
	if action == StateResetAuthoritySuccess && backoffLevel > 0 {
		backoffLevel--
	} else if action != StateResetAuthoritySuccess {
		backoffLevel = 0
	}
	quotaGeneration := nextStateGeneration(auth.Quota.Generation)
	family := auth.Quota.Family
	auth.Unavailable = false
	auth.Status = StatusActive
	auth.StatusMessage = ""
	applyCooldownFields(&auth.Quota, QuotaState{
		Family:       family,
		BackoffLevel: backoffLevel,
		Generation:   quotaGeneration,
	})
	auth.LastError = nil
	auth.NextRetryAfter = time.Time{}
	auth.StateEffectScope = ""
	auth.StateEffectKey = ""
	auth.ResetAuthority = ""
	auth.StateGeneration = nextStateGeneration(auth.StateGeneration)
	auth.UpdatedAt = now
	return true
}

func cloneError(err *Error) *Error {
	if err == nil {
		return nil
	}
	return &Error{
		Code:            err.Code,
		Message:         err.Message,
		Retryable:       err.Retryable,
		HTTPStatus:      err.HTTPStatus,
		DispatchPhase:   err.DispatchPhase,
		Outcome:         err.Outcome,
		RetryDirective:  err.RetryDirective,
		EffectScope:     err.EffectScope,
		EffectKey:       err.EffectKey,
		StateGeneration: err.StateGeneration,
		cause:           err.cause,
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
	var resultErr *Error
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil {
		resultErr = cloneError(authErr)
		if resultErr.HTTPStatus == 0 {
			resultErr.HTTPStatus = statusCodeFromError(err)
		}
		if err != authErr {
			resultErr.cause = err
		}
	} else {
		resultErr = &Error{
			Message:    err.Error(),
			HTTPStatus: statusCodeFromError(err),
			cause:      err,
		}
	}
	switch {
	case isRequestScopedError(err) || isRequestInvalidError(err):
		// 请求本身导致的错误优先于范围更广的连接生命周期分类。
		resultErr.Code = requestScopedErrorCode
	case isTransientTransportError(err):
		resultErr.Code = transientTransportErrorCode
	case isConnectionLifecycleError(err):
		// 保持与 request_scoped 分离，连接中断仍可尝试其他凭据。
		if resultErr.Code == "" || resultErr.Code == connectionLifecycleErrorCode {
			resultErr.Code = connectionLifecycleErrorCode
		}
	}
	disposition := executionDispositionForError(err, cliproxyexecutor.DispatchPhaseMaybeSent)
	resultErr.DispatchPhase = disposition.DispatchPhase
	resultErr.Outcome = disposition.Outcome
	resultErr.RetryDirective = disposition.RetryDirective
	resultErr.EffectScope = disposition.EffectScope
	resultErr.EffectKey = disposition.EffectKey
	return resultErr
}

func shouldSkipCredentialCooldown(err *Error) bool {
	return isRequestScopedResultError(err) || isTransientTransportResultError(err) || isConnectionLifecycleResultError(err)
}

func isTransientTransportError(err error) bool {
	if err == nil || statusCodeFromError(err) != 0 ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var dispositionErr cliproxyexecutor.ExecutionDispositionError
	if errors.As(err, &dispositionErr) && dispositionErr != nil {
		disposition := dispositionErr.ExecutionDisposition()
		if disposition.DispatchPhase != "" && disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseNotSent {
			return false
		}
		if disposition.Outcome == cliproxyexecutor.ExecutionOutcomeUnknown ||
			disposition.Outcome == cliproxyexecutor.ExecutionOutcomeSucceeded {
			return false
		}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr != nil && (dnsErr.IsTimeout || dnsErr.IsTemporary) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr != nil && strings.EqualFold(opErr.Op, "dial") {
		return true
	}
	var urlErr *url.Error
	return errors.As(err, &urlErr) && urlErr != nil && urlErr.Err != nil &&
		strings.Contains(urlErr.Err.Error(), "net/http: TLS handshake timeout")
}

func isTransientTransportResultError(err *Error) bool {
	return err != nil && err.HTTPStatus == 0 &&
		(err.Code == transientTransportErrorCode || isTransientTransportError(err.cause))
}

func isConnectionLifecycleError(err error) bool {
	if err == nil {
		return false
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) && closeErr != nil {
		switch closeErr.Code {
		case websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure:
			return true
		}
	}
	// 带状态码的响应仍按认证、配额或上游错误处理，不能根据正文重新分类。
	if statusCodeFromError(err) != 0 {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return isConnectionLifecycleMessage(err.Error())
}

func isConnectionLifecycleResultError(err *Error) bool {
	if err == nil {
		return false
	}
	if err.Code == connectionLifecycleErrorCode {
		return true
	}
	// 只对无状态码错误使用消息识别，避免 401、429 和 5xx 被误判。
	if statusCodeFromResult(err) != 0 {
		return false
	}
	return isConnectionLifecycleMessage(err.Message)
}

func isConnectionLifecycleMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	switch lower {
	case "context canceled", "context deadline exceeded", "eof", "unexpected eof":
		return true
	}
	if strings.Contains(lower, "websocket: close 1000") ||
		strings.Contains(lower, "websocket: close 1001") ||
		strings.Contains(lower, "websocket: close 1006") {
		return true
	}
	return strings.Contains(lower, "unexpected eof")
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
	if auth == nil || auth.LastError == nil || !auth.Unavailable ||
		(auth.Status != StatusError && auth.Status != StatusDisabled) ||
		!auth.NextRefreshAfter.IsZero() || !auth.NextRetryAfter.IsZero() {
		return false
	}
	return auth.LastError.StatusCode() == http.StatusUnauthorized || strings.EqualFold(
		auth.LastError.Code, "unauthorized",
	)
}

func hasTerminalUnauthorizedAuthFailure(auth *Auth) bool {
	return auth != nil && auth.Unavailable && (auth.Status == StatusError || auth.Status == StatusDisabled) &&
		auth.NextRefreshAfter.IsZero() && auth.NextRetryAfter.IsZero() && hasUnauthorizedAuthFailure(auth) &&
		isInvalidGrantResultError(auth.LastError)
}

func refreshErrorFromError(err error) *Error {
	if err == nil {
		return nil
	}
	statusCode := statusCodeFromError(err)
	if statusCode == 0 && isUnauthorizedError(err) {
		statusCode = http.StatusUnauthorized
	}
	var source *Error
	var authErr *Error
	if errors.As(err, &source) && source != nil {
		authErr = cloneError(source)
		if err != source {
			authErr.cause = err
		}
	} else {
		authErr = &Error{Message: err.Error(), cause: err}
	}
	authErr.HTTPStatus = statusCode
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
	var rap retryAfterProvider
	if !errors.As(err, &rap) || rap == nil {
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

func isRequestScopedNotFoundResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) != http.StatusNotFound {
		return false
	}
	return clienterror.IsItemNotPersisted(err.Message)
}

func isRequestScopedResultError(err *Error) bool {
	return err != nil && (err.IsRequestScoped() || isRequestScopedNotFoundResultError(err))
}

func isModelSupportErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	patterns := [...]string{
		"model_not_supported",
		"requested model is not supported",
		"requested model is unsupported",
		"requested model is unavailable",
		"model is not supported",
		"model not supported",
		"unsupported model",
		"model unavailable",
		"not available for your plan",
		"not available for your account",
	}
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func isModelSupportError(err error) bool {
	if err == nil {
		return false
	}
	status := statusCodeFromError(err)
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	return isModelSupportErrorMessage(err.Error())
}

func isInvalidGrantErrorMessage(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "invalid_grant") || strings.Contains(lower, "encountered invalidated oauth token")
}

func isInvalidGrantError(err error) bool {
	if err == nil || !isInvalidGrantErrorMessage(err.Error()) {
		return false
	}
	status := statusCodeFromError(err)
	return status == 0 || status == http.StatusBadRequest || status == http.StatusUnauthorized
}

func isInvalidGrantResultError(err *Error) bool {
	if err == nil || (!isInvalidGrantErrorMessage(err.Code) && !isInvalidGrantErrorMessage(err.Message)) {
		return false
	}
	status := statusCodeFromResult(err)
	return status == 0 || status == http.StatusBadRequest || status == http.StatusUnauthorized
}

func isCloudflareChallengeErrorMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	justAMoment := strings.Contains(lower, "just-a-moment") || strings.Contains(lower, "just a moment")
	return strings.Contains(lower, "challenge-platform") ||
		strings.Contains(lower, "cf-mitigated") ||
		strings.Contains(lower, "cloudflare challenge") ||
		(justAMoment && strings.Contains(lower, "cloudflare"))
}

func isCloudflareChallengeError(err error) bool {
	if err == nil || statusCodeFromError(err) >= http.StatusInternalServerError {
		return false
	}
	return isCloudflareChallengeErrorMessage(err.Error())
}

func isCloudflareChallengeResultError(err *Error) bool {
	if err == nil || statusCodeFromResult(err) >= http.StatusInternalServerError {
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

func isRequestScopedError(err error) bool {
	if err == nil {
		return false
	}
	var requestErr cliproxyexecutor.RequestScopedError
	return errors.As(err, &requestErr) && requestErr.IsRequestScoped()
}

// isRequestInvalidError reports whether a failure is caused by the request and
// should neither rotate nor penalize credentials. Model-support errors remain
// eligible for alternate routing and keep their model-level state.
func isRequestInvalidError(err error) bool {
	if err == nil {
		return false
	}
	if isRequestScopedError(err) {
		return true
	}
	if isCloudflareChallengeError(err) {
		return false
	}
	if isInvalidGrantError(err) {
		return false
	}
	if isExplicitModelNotFoundError(err, "") || isModelSupportError(err) {
		return false
	}
	status := statusCodeFromError(err)
	if clienterror.IsRequestFault(status, err) {
		return true
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && authErr.Message != "" &&
		clienterror.IsRequestFault(status, errors.New(authErr.Message)) {
		return true
	}
	return false
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
	if shouldSkipCredentialCooldown(resultErr) {
		return
	}
	forceCooldown := resultErr != nil && resultErr.Code == ErrorCodeForceCooldown
	resetAuthority := StateResetAuthoritySuccess
	previousRetryAfter := auth.NextRetryAfter
	previousScope := auth.StateEffectScope
	if previousScope == "" && auth.LastError != nil {
		previousScope = auth.LastError.EffectScope
	}
	credentialState := previousScope == cliproxyexecutor.StateEffectScopeCredential ||
		previousScope == "" && (!hasModelError(auth, now) || hasCredentialQuotaCooldownState(auth))
	if !credentialState {
		previousRetryAfter = time.Time{}
		applyCooldownFields(&auth.Quota, QuotaState{Generation: auth.Quota.Generation})
	}
	defer func() {
		if !auth.NextRetryAfter.IsZero() && previousRetryAfter.After(now) && previousRetryAfter.After(auth.NextRetryAfter) {
			auth.NextRetryAfter = previousRetryAfter.Round(0)
		}
		auth.StateEffectScope = cliproxyexecutor.StateEffectScopeCredential
		auth.StateEffectKey = auth.ID
		auth.ResetAuthority = resetAuthority
		auth.StateGeneration = nextStateGeneration(auth.StateGeneration)
		auth.UpdatedAt = now
		if auth.LastError != nil {
			auth.LastError.StateGeneration = auth.StateGeneration
		}
	}()

	auth.Unavailable = !disableCooling
	auth.Status = StatusError
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
		applyCooldownFields(&auth.Quota, QuotaState{
			Exceeded:       true,
			Reason:         "cloudflare challenge",
			NextRecoverAt:  next,
			BackoffLevel:   backoffLevel,
			Generation:     nextStateGeneration(auth.Quota.Generation),
			ResetAuthority: StateResetAuthoritySuccess,
		})
		auth.NextRetryAfter = next
		return
	}
	if isInvalidGrantResultError(resultErr) {
		auth.StatusMessage = "invalid_grant"
		if forceCooldown {
			auth.NextRetryAfter = now.Add(transientErrorCooldown)
		} else {
			auth.NextRetryAfter = time.Time{}
			resetAuthority = StateResetAuthorityReset
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
		} else if retryAfter != nil && *retryAfter > 0 {
			auth.NextRetryAfter = now.Add(*retryAfter)
		} else {
			auth.NextRetryAfter = now.Add(12 * time.Hour)
		}
	case 429:
		previousQuota := auth.Quota
		auth.StatusMessage = "quota exhausted"
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		auth.Quota.Generation = nextStateGeneration(auth.Quota.Generation)
		auth.Quota.ResetAuthority = StateResetAuthoritySuccess
		var next time.Time
		if !disableCooling {
			if retryAfter != nil {
				next = now.Add(max(*retryAfter, minQuotaCooldownFloor))
			} else {
				next, auth.Quota.BackoffLevel = quotaCooldownAfterFailure(previousQuota, now)
			}
			if previousQuota.Exceeded && previousQuota.NextRecoverAt.After(next) {
				next = previousQuota.NextRecoverAt
			}
			next = next.Round(0)
		}
		auth.Quota.NextRecoverAt = next
		auth.NextRetryAfter = next
	case 408, 500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526:
		auth.StatusMessage = "transient upstream error"
		auth.NextRetryAfter = recoverableFailureRetryAfterWithHint(now, retryAfter, disableCooling)
		auth.Unavailable = !auth.NextRetryAfter.IsZero()
	default:
		if auth.StatusMessage == "" {
			auth.StatusMessage = "request failed"
		}
	}
	if forceCooldown && auth.NextRetryAfter.IsZero() {
		auth.NextRetryAfter = now.Add(transientErrorCooldown)
	}
}

// quotaCooldownAfterFailure returns the recovery deadline and backoff level for
// a quota failure observed at now. Failures that land while a previous quota
// window is still open reuse that window instead of escalating.
func quotaCooldownAfterFailure(quota QuotaState, now time.Time) (time.Time, int) {
	if quota.NextRecoverAt.After(now) {
		return quota.NextRecoverAt.Round(0), quota.BackoffLevel
	}
	cooldown, nextLevel := nextQuotaCooldown(quota.BackoffLevel, false)
	var next time.Time
	if cooldown > 0 {
		next = now.Add(cooldown).Round(0)
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

func (m *Manager) SupportsApplyPatchForProviders(providers []string) bool {
	if m == nil || len(providers) == 0 {
		return false
	}
	for _, provider := range providers {
		executor, exists := m.Executor(provider)
		if !exists {
			return false
		}
		support, supported := executor.(ApplyPatchSupport)
		if !supported || !support.SupportsApplyPatch() {
			return false
		}
		if scoped, hasView := executor.(APIKeyConfigExecutor); hasView {
			support, supported = scoped.ForAPIKey().(ApplyPatchSupport)
			if !supported || !support.SupportsApplyPatch() {
				return false
			}
		}
	}
	return true
}

// CloseExecutionSession asks all registered executors to release the supplied execution session.
func (m *Manager) CloseExecutionSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if m == nil || sessionID == "" {
		return
	}

	m.mu.Lock()
	var selections []*HomeDispatchSelection
	if sessionID == CloseAllExecutionSessionsID {
		m.clearHomeRuntimeAuthsLocked()
		selections = m.takeAllHomeSessionSelectionsLocked()
	} else {
		m.clearHomeRuntimeAuthsForSessionLocked(sessionID)
		selections = m.takeHomeSessionSelectionsLocked(sessionID)
	}
	executors := make([]ProviderExecutor, 0, len(m.executors))
	for _, exec := range m.executors {
		executors = append(executors, exec)
	}
	m.mu.Unlock()

	for _, selection := range selections {
		selection.End("execution_session_closed")
	}
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
	if thinking.HasContext1MTag(model) && !authSupportsContext1MModel(current, model) {
		return nil, ""
	}
	return current.Clone(), providerKey
}

func (m *Manager) context1MUnavailableSelectionError(candidates []*Auth, provider string, model string) error {
	if !thinking.HasContext1MTag(model) {
		return m.unavailableSelectionError(candidates, provider, model)
	}
	modelKey := canonicalModelKey(model)
	registryRef := registry.GetGlobalRegistry()
	hasBaseSupport := false
	hasContext1MSupport := false
	for _, candidate := range candidates {
		if candidate == nil || modelKey == "" || registryRef == nil ||
			!registryRef.ClientSupportsModel(candidate.ID, modelKey) {
			continue
		}
		hasBaseSupport = true
		if authSupportsContext1MModel(candidate, model) {
			hasContext1MSupport = true
			break
		}
	}
	if hasBaseSupport && !hasContext1MSupport {
		return context1MModelSupportError(model)
	}
	return m.unavailableSelectionError(candidates, provider, model)
}

func (m *Manager) context1MSchedulerSelectionError(
	candidates []*Auth,
	provider string,
	model string,
	err error,
) error {
	if err == nil || !thinking.HasContext1MTag(model) {
		return err
	}
	return m.context1MUnavailableSelectionError(candidates, provider, model)
}

func sortedAuthIDs(auths map[string]*Auth) []string {
	ids := make([]string, 0, len(auths))
	for authID := range auths {
		ids = append(ids, authID)
	}
	sort.Strings(ids)
	return ids
}

func authCandidatesWithoutID(candidates []*Auth, authID string) []*Auth {
	remaining := make([]*Auth, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil || candidate.ID == authID {
			continue
		}
		remaining = append(remaining, candidate)
	}
	return remaining
}

// PickNext selects the next auth entry for a single-provider request.
func (m *Manager) PickNext(
	ctx context.Context,
	provider, model string,
	opts cliproxyexecutor.Options,
	tried map[string]struct{},
) (*Auth, ProviderExecutor, error) {
	opts = ensureRouteTraceState(opts)
	if m.HomeEnabled() {
		auth, exec, _, err := m.pickNextViaHome(ctx, model, opts, tried)
		return auth, exec, err
	}
	pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata)
	requestPredicate := selectionPredicate(ctx, opts)
	schedulerPredicate := combineSelectionPredicates(requestPredicate, context1MSelectionPredicate(model))

	var traceEvents []cliproxyexecutor.RouteTraceEvent
	m.mu.RLock()
	selector := m.selector
	pluginScheduler := m.pluginScheduler
	executor, okExecutor := m.executors[provider]
	if !okExecutor {
		m.mu.RUnlock()
		attempt, previousAttempt := routeTraceAttempt(opts)
		publishRouteTraceEvent(
			opts.Metadata, cliproxyexecutor.RouteTraceEvent{
				Attempt:         attempt,
				PreviousAttempt: previousAttempt,
				Provider:        provider,
				Model:           model,
				Action:          "provider.excluded",
				Reason:          "executor_not_registered",
				ResultCode:      "executor_not_found",
			},
		)
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
	context1MUnsupported := false
	for _, authID := range sortedAuthIDs(m.auths) {
		candidate := m.auths[authID]
		if candidate == nil || candidate.Provider != provider {
			continue
		}
		m.recordRouteDecisionCandidate(opts, candidate, model)
		if candidate.Disabled {
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "disabled")
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "pinned_auth_mismatch")
			continue
		}
		if requestPredicate != nil && !requestPredicate(candidate) {
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "request_filter")
			continue
		}
		if _, used := tried[candidate.ID]; used {
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "already_tried")
			continue
		}
		if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(candidate.ID, modelKey) {
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "model_unsupported")
			continue
		}
		if thinking.HasContext1MTag(model) && !authSupportsContext1MModel(candidate, model) {
			context1MUnsupported = true
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "context_1m_unsupported")
			continue
		}
		candidates = append(candidates, candidate.Clone())
	}
	m.mu.RUnlock()
	flushRouteTraceEvents(opts.Metadata, traceEvents)
	if len(candidates) == 0 {
		if context1MUnsupported {
			return nil, nil, context1MModelSupportError(model)
		}
		return nil, nil, &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	pluginCandidates := pluginSchedulerCandidates(pluginScheduler, candidates, model)
	pluginHandled := false
	allowedProviders := map[string]struct{}{strings.ToLower(strings.TrimSpace(provider)): {}}
	for len(pluginCandidates) > 0 {
		selected, handled, strategy, errPick := m.pickViaPluginScheduler(
			ctx, pluginScheduler, provider, []string{provider}, model, opts, pluginCandidates,
		)
		if errPick != nil {
			return nil, nil, errPick
		}
		if !handled {
			break
		}
		pluginHandled = true
		current, _ := m.currentSelectableAuth(
			selected.ID, model, allowedProviders, requestPredicate, tried,
		)
		if current != nil {
			if strategy == schedulerStrategyCustom {
				publishObservedRouteDecision(
					opts,
					[]string{strings.ToLower(strings.TrimSpace(provider))},
					model,
					schedulerStrategyCustom,
					current,
					"custom",
					"",
				)
			}
			return current, executor, nil
		}
		recordRouteDecisionCandidateExclusion(opts, selected, "runtime_auth_changed")
		traceCandidateExcluded(opts, selected, model, "runtime_auth_changed")
		pluginCandidates = authCandidatesWithoutID(pluginCandidates, selected.ID)
	}
	if pluginHandled && len(pluginCandidates) == 0 {
		publishObservedRouteDecision(
			opts,
			[]string{strings.ToLower(strings.TrimSpace(provider))},
			model,
			schedulerStrategyCustom,
			nil,
			"",
			"",
		)
		return nil, nil, m.context1MUnavailableSelectionError(candidates, provider, model)
	}
	selectionSelector := selector
	var stickySelector *SessionAffinitySelector
	if sessionSelector, ok := selector.(*SessionAffinitySelector); ok {
		stickySelector = sessionSelector
		if sticky, rule := stickySelector.pickStickyForModelWithRule(provider, model, opts, candidates); sticky != nil {
			if current, _ := m.currentSelectableAuth(
				sticky.ID, model, allowedProviders, requestPredicate, tried,
			); current != nil {
				executor, ok := m.Executor(provider)
				if !ok || executor == nil {
					return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
				}
				publishObservedRouteDecision(
					opts,
					[]string{strings.ToLower(strings.TrimSpace(provider))},
					model,
					selectorStrategy(selector),
					current,
					rule,
					rule,
				)
				return current, executor, nil
			}
			recordRouteDecisionCandidateExclusion(opts, sticky, "runtime_auth_changed")
			traceCandidateExcluded(opts, sticky, model, "runtime_auth_changed")
			stickySelector.InvalidateAuth(sticky.ID)
		}
		if fallback := stickySelector.FallbackSelector(); fallback != nil {
			selectionSelector = fallback
		}
	}
	if m.scheduler != nil && isBuiltInSelector(selectionSelector) {
		schedulerTried := maps.Clone(tried)
		if schedulerTried == nil {
			schedulerTried = make(map[string]struct{})
		}
		for attempt := 0; attempt < len(candidates); attempt++ {
			selected, errPick := m.scheduler.pickSingleWithPredicate(
				ctx, provider, model, opts, schedulerTried, schedulerPredicate,
			)
			if errPick != nil {
				return nil, nil, m.context1MSchedulerSelectionError(candidates, provider, model, errPick)
			}
			current, _ := m.currentSelectableAuth(
				selected.ID, model, allowedProviders, requestPredicate, tried,
			)
			if current == nil {
				if leaseRequest := quotaInFlightRequestFromContext(ctx); leaseRequest != nil {
					leaseRequest.release()
				}
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
		return nil, nil, m.context1MUnavailableSelectionError(candidates, provider, model)
	}
	selectorCandidates := candidates
	for len(selectorCandidates) > 0 {
		selected, errPick := selectionSelector.Pick(
			ctx, provider, model, externalRouteTraceOptions(opts), selectorCandidates,
		)
		if errPick != nil {
			publishObservedRouteDecision(
				opts,
				[]string{strings.ToLower(strings.TrimSpace(provider))},
				model,
				selectorStrategy(selectionSelector),
				nil,
				"",
				"",
			)
			return nil, nil, errPick
		}
		if selected == nil {
			publishObservedRouteDecision(
				opts,
				[]string{strings.ToLower(strings.TrimSpace(provider))},
				model,
				selectorStrategy(selectionSelector),
				nil,
				"",
				"",
			)
			return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
		}
		current, providerKey := m.currentSelectableAuth(
			selected.ID,
			model,
			allowedProviders,
			requestPredicate,
			tried,
			pinnedAuthID != "" && selected.ID == pinnedAuthID,
		)
		if current != nil {
			executor, okExecutor = m.Executor(providerKey)
			if !okExecutor || executor == nil {
				return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
			}
			if stickySelector != nil {
				stickySelector.Remember(provider, opts, current)
			}
			publishObservedRouteDecision(
				opts,
				[]string{providerKey},
				model,
				selectorStrategy(selectionSelector),
				current,
				"custom",
				"",
			)
			return current, executor, nil
		}
		recordRouteDecisionCandidateExclusion(opts, selected, "runtime_auth_changed")
		traceCandidateExcluded(opts, selected, model, "runtime_auth_changed")
		remaining := authCandidatesWithoutID(selectorCandidates, selected.ID)
		if len(remaining) == len(selectorCandidates) {
			break
		}
		selectorCandidates = remaining
	}
	publishObservedRouteDecision(
		opts,
		[]string{strings.ToLower(strings.TrimSpace(provider))},
		model,
		selectorStrategy(selectionSelector),
		nil,
		"",
		"",
	)
	return nil, nil, m.context1MUnavailableSelectionError(candidates, provider, model)
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
	requestPredicate := selectionPredicate(ctx, opts)
	schedulerPredicate := combineSelectionPredicates(requestPredicate, context1MSelectionPredicate(model))

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

	var traceEvents []cliproxyexecutor.RouteTraceEvent
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
	context1MUnsupported := false
	for _, authID := range sortedAuthIDs(m.auths) {
		candidate := m.auths[authID]
		if candidate == nil {
			continue
		}
		m.recordRouteDecisionCandidate(opts, candidate, model)
		if candidate.Disabled {
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "disabled")
			continue
		}
		if pinnedAuthID != "" && candidate.ID != pinnedAuthID {
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "pinned_auth_mismatch")
			continue
		}
		if requestPredicate != nil && !requestPredicate(candidate) {
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "request_filter")
			continue
		}
		providerKey := strings.TrimSpace(strings.ToLower(candidate.Provider))
		if providerKey == "" {
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "provider_missing")
			continue
		}
		if _, ok := providerSet[providerKey]; !ok {
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "provider_not_requested")
			continue
		}
		if _, used := tried[candidate.ID]; used {
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "already_tried")
			continue
		}
		if _, ok := m.executors[providerKey]; !ok {
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "executor_not_registered")
			continue
		}
		if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(candidate.ID, modelKey) {
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "model_unsupported")
			continue
		}
		if thinking.HasContext1MTag(model) && !authSupportsContext1MModel(candidate, model) {
			context1MUnsupported = true
			appendCandidateExcluded(&traceEvents, opts, candidate, model, "context_1m_unsupported")
			continue
		}
		candidates = append(candidates, candidate.Clone())
	}
	m.mu.RUnlock()
	flushRouteTraceEvents(opts.Metadata, traceEvents)
	if len(candidates) == 0 {
		if context1MUnsupported {
			return nil, nil, "", context1MModelSupportError(model)
		}
		if pinnedAuthID != "" {
			return nil, nil, "", pinnedAuthNotFoundError(pinnedAuthID)
		}
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	pluginCandidates := pluginSchedulerCandidates(pluginScheduler, candidates, model)
	pluginHandled := false
	for len(pluginCandidates) > 0 {
		selected, handled, strategy, errPick := m.pickViaPluginScheduler(
			ctx, pluginScheduler, "mixed", providers, model, opts, pluginCandidates,
		)
		if errPick != nil {
			return nil, nil, "", errPick
		}
		if !handled {
			break
		}
		pluginHandled = true
		current, providerKey := m.currentSelectableAuth(
			selected.ID, model, providerSet, requestPredicate, tried,
		)
		if current != nil {
			executor, okExecutor := m.Executor(providerKey)
			if !okExecutor {
				return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
			}
			if strategy == schedulerStrategyCustom {
				publishObservedRouteDecision(
					opts,
					normalizeProviderKeys(providers),
					model,
					schedulerStrategyCustom,
					current,
					"custom",
					"",
				)
			}
			return current, executor, providerKey, nil
		}
		recordRouteDecisionCandidateExclusion(opts, selected, "runtime_auth_changed")
		traceCandidateExcluded(opts, selected, model, "runtime_auth_changed")
		pluginCandidates = authCandidatesWithoutID(pluginCandidates, selected.ID)
	}
	if pluginHandled && len(pluginCandidates) == 0 {
		publishObservedRouteDecision(
			opts,
			normalizeProviderKeys(providers),
			model,
			schedulerStrategyCustom,
			nil,
			"",
			"",
		)
		return nil, nil, "", m.context1MUnavailableSelectionError(candidates, "mixed", model)
	}
	selectionSelector := selector
	var stickySelector *SessionAffinitySelector
	if sessionSelector, ok := selector.(*SessionAffinitySelector); ok {
		stickySelector = sessionSelector
		if sticky, rule := stickySelector.pickStickyForModelWithRule("mixed", model, opts, candidates); sticky != nil {
			if current, providerKey := m.currentSelectableAuth(
				sticky.ID, model, providerSet, requestPredicate, tried,
			); current != nil {
				executor, ok := m.Executor(providerKey)
				if !ok || executor == nil {
					return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
				}
				publishObservedRouteDecision(
					opts,
					normalizeProviderKeys(providers),
					model,
					selectorStrategy(selector),
					current,
					rule,
					rule,
				)
				return current, executor, providerKey, nil
			}
			recordRouteDecisionCandidateExclusion(opts, sticky, "runtime_auth_changed")
			traceCandidateExcluded(opts, sticky, model, "runtime_auth_changed")
			stickySelector.InvalidateAuth(sticky.ID)
		}
		if fallback := stickySelector.FallbackSelector(); fallback != nil {
			selectionSelector = fallback
		}
	}
	if m.scheduler != nil && isBuiltInSelector(selectionSelector) {
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
				ctx, filteredProviders, model, opts, schedulerTried, schedulerPredicate,
			)
			if errPick != nil {
				return nil, nil, "", m.context1MSchedulerSelectionError(candidates, "mixed", model, errPick)
			}
			current, providerKey := m.currentSelectableAuth(
				selected.ID, model, providerSet, requestPredicate, tried,
			)
			if current == nil {
				if leaseRequest := quotaInFlightRequestFromContext(ctx); leaseRequest != nil {
					leaseRequest.release()
				}
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
		return nil, nil, "", m.context1MUnavailableSelectionError(candidates, "mixed", model)
	}
	selectorCandidates := candidates
	for len(selectorCandidates) > 0 {
		selected, errPick := selectionSelector.Pick(
			ctx, "mixed", model, externalRouteTraceOptions(opts), selectorCandidates,
		)
		if errPick != nil {
			publishObservedRouteDecision(
				opts,
				normalizeProviderKeys(providers),
				model,
				selectorStrategy(selectionSelector),
				nil,
				"",
				"",
			)
			return nil, nil, "", errPick
		}
		if selected == nil {
			publishObservedRouteDecision(
				opts,
				normalizeProviderKeys(providers),
				model,
				selectorStrategy(selectionSelector),
				nil,
				"",
				"",
			)
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
		if current != nil {
			executor, okExecutor := m.Executor(providerKey)
			if !okExecutor || executor == nil {
				return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
			}
			if stickySelector != nil {
				stickySelector.Remember("mixed", opts, current)
			}
			publishObservedRouteDecision(
				opts,
				normalizeProviderKeys(providers),
				model,
				selectorStrategy(selectionSelector),
				current,
				"custom",
				"",
			)
			return current, executor, providerKey, nil
		}
		recordRouteDecisionCandidateExclusion(opts, selected, "runtime_auth_changed")
		traceCandidateExcluded(opts, selected, model, "runtime_auth_changed")
		remaining := authCandidatesWithoutID(selectorCandidates, selected.ID)
		if len(remaining) == len(selectorCandidates) {
			break
		}
		selectorCandidates = remaining
	}
	publishObservedRouteDecision(
		opts,
		normalizeProviderKeys(providers),
		model,
		selectorStrategy(selectionSelector),
		nil,
		"",
		"",
	)
	return nil, nil, "", m.context1MUnavailableSelectionError(candidates, "mixed", model)
}

type homeErrorDetail struct {
	RequestRetry *int   `json:"request_retry,omitempty"`
	Type         string `json:"type"`
	Message      string `json:"message"`
	Code         string `json:"code,omitempty"`
	Retryable    bool   `json:"retryable,omitempty"`
	RetryAfterMS int64  `json:"retry_after_ms,omitempty"`
}

const (
	homeUpstreamModelAttributeKey     = "home_upstream_model"
	homeForceMappingAttributeKey      = "home_force_mapping"
	homeOriginalAliasAttributeKey     = "home_original_alias"
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
	if isHomeRequestRetryExceededError(errPick) {
		return true
	}
	var authErr *Error
	if !errors.As(errPick, &authErr) || authErr == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(authErr.Code)) {
	case "auth_not_found", "auth_unavailable":
		return true
	default:
		return false
	}
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
	RequestRetry  *int                   `json:"request_retry,omitempty"`
	Model         string                 `json:"model"`
	Provider      string                 `json:"provider"`
	AuthIndex     string                 `json:"auth_index"`
	UserAPIKey    string                 `json:"user_api_key"`
	ForceMapping  bool                   `json:"force_mapping"`
	OriginalAlias string                 `json:"original_alias"`
	ModelInfo     *homeDispatchModelInfo `json:"model_info,omitempty"`
	Auth          Auth                   `json:"auth"`
}

type homeDispatchModelInfo struct {
	ID                         string                       `json:"id"`
	Type                       string                       `json:"type,omitempty"`
	InputTokenLimit            int                          `json:"inputTokenLimit,omitempty"`
	OutputTokenLimit           int                          `json:"outputTokenLimit,omitempty"`
	ContextLength              int                          `json:"context_length,omitempty"`
	MaxCompletionTokens        int                          `json:"max_completion_tokens,omitempty"`
	Thinking                   *registry.ThinkingSupport    `json:"thinking,omitempty"`
	NativeCapabilities         *registry.NativeCapabilities `json:"native_capabilities,omitempty"`
	SupportConfigurationUpdate *bool                        `json:"support_configuration_update,omitempty"`
	UserDefined                bool                         `json:"user_defined"`
}

func (m *homeDispatchModelInfo) registryModelInfo() *registry.ModelInfo {
	if m == nil || strings.TrimSpace(m.ID) == "" {
		return nil
	}
	return &registry.ModelInfo{
		ID:                         strings.TrimSpace(m.ID),
		Type:                       strings.TrimSpace(m.Type),
		InputTokenLimit:            m.InputTokenLimit,
		OutputTokenLimit:           m.OutputTokenLimit,
		ContextLength:              m.ContextLength,
		MaxCompletionTokens:        m.MaxCompletionTokens,
		Thinking:                   m.Thinking,
		NativeCapabilities:         m.NativeCapabilities,
		SupportConfigurationUpdate: m.SupportConfigurationUpdate != nil && *m.SupportConfigurationUpdate,
		UserDefined:                m.UserDefined,
	}
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
	GetRefreshAuth(ctx context.Context, authIndex string, accessTokenSHA256 string) ([]byte, error)
}

type homeRuntimeRefreshClient = HomeRefreshClient

type HomeClient interface {
	HeartbeatOK() bool
	RPopAuth(ctx context.Context, requestedModel string, sessionID string, headers http.Header, count int) (
		[]byte,
		error,
	)
	GetRefreshAuth(ctx context.Context, authIndex string, accessTokenSHA256 string) ([]byte, error)
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
	client := home.Current()
	if client == nil {
		return nil
	}
	return client
}

var currentHomeRuntimeRefreshClient = func() HomeRefreshClient {
	client := home.Current()
	if client == nil {
		return nil
	}
	return client
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
	selections := m.takeAllHomeSessionSelectionsLocked()
	m.mu.Unlock()
	m.homeSessionAliases.clear()
	for _, selection := range selections {
		selection.End("home_disabled")
	}
}

func (m *Manager) clearHomeRuntimeAuthsLocked() {
	m.homeRuntimeAuths = make(map[string]map[string]*Auth)
	m.homeRuntimeAuthOwners = make(map[string]map[string]*HomeDispatchSelection)
}

func (m *Manager) clearHomeRuntimeAuthsForSessionLocked(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	delete(m.homeRuntimeAuths, sessionID)
	delete(m.homeRuntimeAuthOwners, sessionID)
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
	executionSessionID := homeExecutionSessionIDFromMetadata(opts.Metadata)
	if pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata); pinnedAuthID != "" {
		if homeAuthAlreadyTried(tried, pinnedAuthID) {
			return nil, nil, "", repeatedHomeAuthError()
		}
		if executionSessionID != "" {
			if auth, executor, providerKey, ok := m.homeRuntimeAuthByID(executionSessionID, pinnedAuthID); ok {
				m.recordRouteDecisionCandidate(opts, auth, model)
				publishObservedRouteDecision(
					opts,
					[]string{providerKey},
					model,
					schedulerStrategyCustom,
					auth,
					"pinned_auth",
					"pinned",
				)
				return auth, executor, providerKey, nil
			}
		}
		return nil, nil, "", pinnedAuthNotFoundError(pinnedAuthID)
	}
	selection, errSelection := m.pickHomeDispatchSelection(ctx, model, opts)
	if errSelection != nil {
		return nil, nil, "", errSelection
	}
	if selection == nil || selection.Auth == nil {
		if selection != nil {
			selection.End("invalid_selection")
		}
		return nil, nil, "", &Error{
			Code: "invalid_auth", Message: "home returned invalid auth selection", HTTPStatus: http.StatusBadGateway,
		}
	}
	if homeAuthAlreadyTried(tried, selection.Auth.ID) {
		selection.End("repeated_auth")
		return nil, nil, "", repeatedHomeAuthError()
	}
	auth := selection.CloneAuthForRoute(model)
	executor := selection.Executor
	provider := selection.Provider
	m.recordRouteDecisionCandidate(opts, auth, model)
	publishObservedRouteDecision(
		opts,
		[]string{provider},
		model,
		schedulerStrategyCustom,
		auth,
		"home_dispatch",
		"",
	)
	selection.End("legacy_selection_unbound")
	return auth, executor, provider, nil
}

func authPersistenceSkipped(ctx context.Context, auth *Auth) bool {
	if auth == nil || shouldSkipPersist(ctx) && !shouldForcePersist(ctx) {
		return true
	}
	if IsConfigAPIKeyAuth(auth) || IsPluginVirtualAuth(auth) {
		return true
	}
	if auth.Attributes != nil {
		if value := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); value == "true" {
			return true
		}
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
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
	if authPersistenceSkipped(ctx, auth) {
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
		m.recordDeferredAuthStopError(
			errors.Join(
				errAuthDurableStateUncertain,
				fmt.Errorf("credential rollback completed after auth deletion workers stopped for %s", id),
			),
		)
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
	interval = min(interval, MaxAutoRefreshCheckInterval)
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

			if exec := m.executorFor(executorKeyFromAuth(a)); exec == nil {
				continue
			}
			m.refreshLifecycleMu.Lock()
			if m.refreshStopping || ctx.Err() != nil {
				m.refreshLifecycleMu.Unlock()
				return
			}
			job := m.markRefreshPending(a.ID, a.Generation, now)
			if job == nil {
				m.refreshLifecycleMu.Unlock()
				continue
			}
			m.refreshWorkers.Add(1)
			m.refreshLifecycleMu.Unlock()
			go func() {
				defer m.refreshWorkers.Done()
				m.refreshAuthWithLimit(ctx, job)
			}()
		}
	}
}

func (m *Manager) refreshAuthWithLimit(ctx context.Context, job *autoRefreshJob) {
	defer m.finishAutoRefreshJob(job)
	m.mu.RLock()
	semaphore := m.refreshSemaphore
	m.mu.RUnlock()
	if semaphore != nil {
		select {
		case semaphore <- struct{}{}:
			defer func() { <-semaphore }()
		case <-ctx.Done():
			return
		}
	}
	m.mu.RLock()
	auth := m.auths[job.id]
	current := m.autoRefreshJobs[job.id] == job && auth != nil && auth.Generation == job.generation && !auth.Disabled
	m.mu.RUnlock()
	if ctx.Err() == nil && current {
		_, _ = m.refreshAuthForRequest(ctx, job.id, "", job.generation)
	}
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
		return expiry.Sub(now) <= *lead
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

type autoRefreshJob struct {
	id           string
	generation   uint64
	pendingUntil time.Time
}

func (m *Manager) markRefreshPending(id string, generation uint64, now time.Time) *autoRefreshJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	auth := m.auths[id]
	if auth == nil || auth.Disabled || hasTerminalUnauthorizedAuthFailure(auth) || auth.Generation != generation {
		return nil
	}
	if m.autoRefreshJobs[id] != nil || !auth.NextRefreshAfter.IsZero() && now.Before(auth.NextRefreshAfter) {
		return nil
	}
	capacity := cap(m.refreshSemaphore)
	if capacity == 0 {
		capacity = refreshMaxConcurrency
	}
	if len(m.autoRefreshJobs) >= capacity {
		return nil
	}
	job := &autoRefreshJob{id: id, generation: generation, pendingUntil: now.Add(refreshPendingBackoff)}
	if m.autoRefreshJobs == nil {
		m.autoRefreshJobs = make(map[string]*autoRefreshJob)
	}
	m.autoRefreshJobs[id] = job
	auth.NextRefreshAfter = job.pendingUntil
	return job
}

func (m *Manager) finishAutoRefreshJob(job *autoRefreshJob) {
	if m == nil || job == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.autoRefreshJobs[job.id] != job {
		return
	}
	delete(m.autoRefreshJobs, job.id)
	if auth := m.auths[job.id]; auth != nil && auth.Generation == job.generation &&
		auth.NextRefreshAfter.Equal(job.pendingUntil) {
		auth.NextRefreshAfter = time.Time{}
	}
}

type authRefreshReasonKey struct{}

type authRefreshReason uint8

const (
	authRefreshAfterUnauthorized authRefreshReason = iota + 1
	authRefreshManual
)

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
	if authMetadataString(auth, "refreshToken") != "" {
		return true
	}
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "meta") &&
		!IsConfigAPIKeyAuth(auth) &&
		(authMetadataString(auth, "dca_token") != "" || authAttribute(auth, "dca_token") != "")
}

func clearUnauthorizedModelStates(auth *Auth, now time.Time) {
	if auth == nil || len(auth.ModelStates) == 0 {
		return
	}
	changed := false
	for _, state := range auth.ModelStates {
		if state == nil || state.LastError == nil {
			continue
		}
		if state.LastError.StatusCode() != http.StatusUnauthorized && !strings.EqualFold(
			state.LastError.Code, "unauthorized",
		) {
			continue
		}
		if state.Quota.Exceeded && (state.Quota.NextRecoverAt.IsZero() || state.Quota.NextRecoverAt.After(now)) {
			state.LastError = nil
			if strings.Contains(strings.ToLower(state.StatusMessage), "unauthorized") {
				state.StatusMessage = state.Quota.Reason
			}
			state.Generation = nextStateGeneration(state.Generation)
			state.UpdatedAt = now
			changed = true
			continue
		}
		resetModelState(state, now)
		changed = true
	}
	if changed {
		updateAggregatedAvailability(auth, now)
	}
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

func (m *Manager) markRejectedAccessToken(auth *Auth, executor ProviderExecutor) {
	fingerprint := AccessTokenSHA256(auth)
	if m == nil || auth == nil || executor == nil || fingerprint == "" {
		return
	}
	m.executorMu.RLock()
	m.mu.Lock()
	current := m.auths[auth.ID]
	var snapshot *Auth
	if current != nil && !persistentAuthEndpointChanged(auth, current) && auth.ProxyURL == current.ProxyURL &&
		sameProviderExecutor(m.executors[executorKeyFromAuth(current)], executor) &&
		AccessTokenSHA256(current) == fingerprint && !current.hasRejectedAccessToken() {
		snapshot = current.Clone()
		snapshot.rejectedAccessTokenSHA256 = fingerprint
		snapshot.UpdatedAt = time.Now().UTC()
		if !snapshot.UpdatedAt.After(current.UpdatedAt) {
			snapshot.UpdatedAt = current.UpdatedAt.Add(time.Nanosecond)
		}
		m.auths[auth.ID] = snapshot
	}
	m.mu.Unlock()
	m.executorMu.RUnlock()
	if snapshot != nil && m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
}

// tryRefreshAfterUnauthorized 只允许本地 OAuth 凭证在 401 后刷新一次，Home 凭证不在本地刷新
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
	// Request-scoped failures describe this request, not stale credentials.
	// Refreshing would turn a direct error response into an implicit retry.
	if isRequestScopedError(execErr) {
		return auth, false
	}
	if !isUnauthorizedError(execErr) {
		return auth, false
	}
	if auth.Generation == 0 || hasTerminalUnauthorizedAuthFailure(auth) {
		return auth, false
	}
	if evaluator, ok := executor.(UnauthorizedRefreshEvaluator); ok {
		if evaluator == nil || !evaluator.ShouldRefreshAfterUnauthorized(auth) {
			return auth, false
		}
	} else if !authHasRefreshCredential(auth) {
		return auth, false
	}
	log.Debugf("unauthorized response for %s (%s), refreshing credentials before fallback", auth.Provider, auth.ID)
	m.markRejectedAccessToken(auth, executor)
	refreshed, errRefresh := m.refreshAuthForRequest(
		context.WithValue(ctx, authRefreshReasonKey{}, authRefreshAfterUnauthorized),
		auth.ID,
		accessTokenForFingerprint(auth),
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

// ForceRefreshResult is the non-sensitive result of an explicit credential refresh.
type ForceRefreshResult struct {
	FileName    string    `json:"filename"`
	Provider    string    `json:"provider"`
	AuthIndex   string    `json:"auth_index"`
	Status      Status    `json:"status"`
	RefreshedAt time.Time `json:"refreshed_at"`
}

func forceRefreshResult(auth *Auth) ForceRefreshResult {
	if auth == nil {
		return ForceRefreshResult{}
	}
	copyAuth := auth.Clone()
	copyAuth.EnsureIndex()
	fileName := strings.TrimSpace(copyAuth.FileName)
	if fileName == "" {
		fileName = strings.TrimSpace(copyAuth.ID)
	}
	if fileName != "" {
		fileName = filepath.Base(fileName)
	}
	return ForceRefreshResult{
		FileName:    fileName,
		Provider:    strings.TrimSpace(copyAuth.Provider),
		AuthIndex:   strings.TrimSpace(copyAuth.Index),
		Status:      copyAuth.Status,
		RefreshedAt: copyAuth.LastRefreshedAt,
	}
}

// ForceRefreshAuth synchronously refreshes one credential without exposing its secrets.
func (m *Manager) ForceRefreshAuth(ctx context.Context, id string) (ForceRefreshResult, error) {
	if m == nil {
		return ForceRefreshResult{}, errors.New("auth manager is nil")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return ForceRefreshResult{}, errors.New("auth id is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	refreshed, errRefresh := m.refreshAuthForRequest(
		context.WithValue(ctx, authRefreshReasonKey{}, authRefreshManual), id, "",
	)
	if refreshed != nil {
		return forceRefreshResult(refreshed), errRefresh
	}
	if current, ok := m.GetByID(id); ok {
		if errRefresh == nil {
			errRefresh = errors.New("credential refresh returned no result")
		}
		return forceRefreshResult(current), errRefresh
	}
	if errRefresh == nil {
		errRefresh = errors.New("credential refresh returned no result")
	}
	return ForceRefreshResult{}, errRefresh
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
	ctx = cliproxyexecutor.WithoutRequestProxyURL(ctx)
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
		exec = m.executors[executorKeyFromAuth(auth)]
	}
	m.mu.RUnlock()
	if auth == nil || exec == nil {
		return nil, errors.New("auth or executor not found")
	}
	refreshReason, _ := ctx.Value(authRefreshReasonKey{}).(authRefreshReason)
	if hasTerminalUnauthorizedAuthFailure(auth) && refreshReason != authRefreshManual {
		return nil, errors.New("credential refresh rejected: invalid_grant")
	}
	if len(expectedGeneration) > 0 && expectedGeneration[0] > 0 && auth.Generation != expectedGeneration[0] {
		if refreshReason != authRefreshAfterUnauthorized || failedAccessToken == "" {
			return auth.Clone(), nil
		}
		currentToken := accessTokenForFingerprint(auth)
		if currentToken != failedAccessToken {
			if currentToken == "" || auth.Disabled || auth.hasRejectedAccessToken() {
				return nil, errors.New("replacement access token is unavailable")
			}
			return auth.Clone(), nil
		}
		if !auth.hasRejectedAccessToken() {
			return nil, errors.New("credential changed without marking rejected access token")
		}
	}

	if failedAccessToken != "" {
		if currentToken := accessTokenForFingerprint(auth); currentToken != "" && currentToken != failedAccessToken {
			return auth.Clone(), nil
		}
	}

	persistenceExpected := auth.Clone()
	persistenceOwnerID := ""
	if auth.Attributes != nil {
		persistenceOwnerID = strings.TrimSpace(auth.Attributes["gemini_virtual_parent"])
	}
	if persistenceOwnerID != "" && persistenceOwnerID != auth.ID {
		unlockOwnerMutation, errOwnerMutation := m.lockAuthMutationWithinLoadGate(ctx, persistenceOwnerID)
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
		rejectedAccess := refreshReason == authRefreshAfterUnauthorized && failedAccessToken != "" &&
			failedAccessToken == accessTokenForFingerprint(auth)
		if !m.publishRefreshError(auth, exec, refreshCommit, err, now, rejectedAccess) {
			return nil, errors.New("credential refresh error is stale")
		}
		return nil, err
	}
	if updated == nil || runtimeAuthCredentialsEqual(updated, auth) {
		return nil, errors.New("credential refresh returned unchanged credentials")
	}
	commitCtx, cancelCommit := context.WithTimeout(context.WithoutCancel(ctx), refreshPersistenceCommitTimeout)
	defer cancelCommit()
	saved, errCommit := m.commitRefreshedAuth(
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
		m.publishClientModelProjections(saved, saved.Generation, now)
		unlockMutation()
		mutationLocked = false
		m.hook.OnAuthUpdated(commitCtx, saved.Clone())
		if refreshedAuthBindingChanged(auth, saved) {
			m.mu.RLock()
			observer := m.refreshBindingObserver
			m.mu.RUnlock()
			if observer != nil {
				observer(auth.Clone(), saved.Clone())
			}
		}
	}
	if errCommit != nil {
		if errors.Is(errCommit, errAuthDurableStateUncertain) {
			log.WithFields(log.Fields{"auth_id": auth.ID, "provider": auth.Provider}).Warn("refreshed credential persistence is uncertain")
		} else {
			log.Debugf("refresh commit for %s (%s) failed: %v", auth.Provider, auth.ID, errCommit)
		}
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
	rejectedAccess bool,
) bool {
	if m == nil || expected == nil || expectedExecutor == nil || refreshErr == nil {
		return false
	}
	provider := executorKeyFromAuth(expected)
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
	var projectionGeneration uint64
	commit := func() bool {
		if hasTerminalUnauthorizedAuthFailure(current) {
			return true
		}
		next := current.Clone()
		next.LastError = refreshErrorFromError(refreshErr)
		if rejectedAccess && isInvalidGrantError(refreshErr) {
			next.LastError = &Error{Code: "unauthorized", Message: "invalid_grant", HTTPStatus: http.StatusUnauthorized}
			next.Unavailable = true
			next.Status = StatusError
			if next.Disabled || current.Status == StatusDisabled {
				next.Status = StatusDisabled
			}
			next.StatusMessage = "invalid_grant"
			next.NextRefreshAfter = time.Time{}
			next.NextRetryAfter = time.Time{}
			next.RefreshFailures = 0
			next.StateEffectScope = cliproxyexecutor.StateEffectScopeCredential
			next.StateEffectKey = next.ID
			next.StateGeneration = nextStateGeneration(next.StateGeneration)
			next.UpdatedAt = now
			projectionGeneration = m.nextAuthGenerationLocked()
			snapshot = next.Clone()
			m.auths[expected.ID] = next
			return true
		}
		invalidGrant := isInvalidGrantError(refreshErr)
		if (next.Disabled || current.Status == StatusDisabled) && invalidGrant {
			next.Unavailable = true
			next.Status = StatusDisabled
			next.NextRefreshAfter = time.Time{}
			next.RefreshFailures = 0
			next.StatusMessage = "disabled (invalid grant)"
		} else if !next.HasValidAccessToken(now) {
			next.Unavailable = true
			next.Status = StatusError
			if isUnauthorizedError(refreshErr) {
				next.NextRefreshAfter = time.Time{}
				next.RefreshFailures = 0
				next.StatusMessage = "unauthorized"
			} else if invalidGrant {
				next.RefreshFailures = min(next.RefreshFailures+1, 6)
				next.NextRefreshAfter = now.Add(invalidGrantBackoffDuration(next.RefreshFailures))
				next.StatusMessage = "invalid grant (retrying)"
			} else {
				next.RefreshFailures = 0
				next.NextRefreshAfter = now.Add(refreshFailureBackoff)
				next.StatusMessage = "token expired"
			}
		} else {
			nextRefresh := now.Add(refreshFailureBackoff)
			if invalidGrant {
				next.RefreshFailures = min(next.RefreshFailures+1, 6)
				nextRefresh = now.Add(invalidGrantBackoffDuration(next.RefreshFailures))
			} else {
				next.RefreshFailures = 0
			}
			if expiresAt, ok := next.AccessTokenExpirationTime(); ok &&
				!expiresAt.IsZero() && nextRefresh.After(expiresAt) {
				nextRefresh = expiresAt
			}
			next.NextRefreshAfter = nextRefresh
			if next.hasRejectedAccessToken() {
				log.WithFields(log.Fields{"auth_id": next.ID, "provider": next.Provider}).Warn("credential refresh failed; rejected access token remains unavailable")
			} else if !next.Unavailable {
				log.Warnf(
					"credential refresh failed for %s (%s): %s; retaining active credential as access token is unexpired",
					next.Provider,
					next.ID,
					logging.SafeDiagnosticForLog(refreshErr.Error()),
				)
			}
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
	if committed && snapshot != nil && projectionGeneration != 0 {
		m.publishClientModelProjections(snapshot, projectionGeneration, now)
		m.persistCooldownStatesAfterMutation(context.Background())
	}
	return committed
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
	provider := executorKeyFromAuth(expected)
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
	if saved != nil {
		m.notifyAuthChangeLocked(expected.ID)
		if persistenceSaved != nil && persistenceSaved.ID != expected.ID {
			m.notifyAuthChangeLocked(persistenceSaved.ID)
		}
	}
	published := saved.Clone()
	publishedPersistence := persistenceSaved.Clone()
	m.mu.Unlock()
	m.executorMu.RUnlock()
	if published == nil {
		return nil, nil
	}
	if m.scheduler != nil {
		if publishedPersistence != nil && publishedPersistence.ID != published.ID {
			m.scheduler.upsertAuth(publishedPersistence.Clone())
		}
		m.scheduler.upsertAuth(published.Clone())
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	return published, publishedPersistence
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
	value, ok := m.pendingRefreshPersistenceRetries.LoadAndDelete(strings.TrimSpace(id))
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
	previous, replaced := m.pendingRefreshPersistenceRetries.Swap(auth.ID, intent)
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
	defer m.pendingRefreshPersistenceRetries.CompareAndDelete(id, intent)
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
			persistCtx, cancelPersist := context.WithTimeout(intent.ctx, refreshPersistenceCommitTimeout)
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
			m.recordDeferredAuthStopError(
				errors.Join(
					errAuthDurableStateUncertain,
					fmt.Errorf("refreshed credential persistence stopped for %s", id),
				),
			)
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
) (*Auth, error) {
	if m == nil || expected == nil || update == nil || expectedExecutor == nil {
		return nil, errors.New("credential refresh commit is invalid")
	}
	if !refreshCommitGuardCurrent(guard) {
		return nil, errors.New("credential refresh owner changed before persistence")
	}
	provider := executorKeyFromAuth(expected)
	m.executorMu.RLock()
	m.mu.RLock()
	current := m.auths[expected.ID]
	if current == nil || current.Generation != expected.Generation ||
		!sameProviderExecutor(m.executors[provider], expectedExecutor) {
		m.mu.RUnlock()
		m.executorMu.RUnlock()
		return nil, errors.New("credential refresh result is stale")
	}
	previous := current.Clone()
	next, bindingChanged := mergeRefreshedAuthState(current, update)
	persistTarget := next
	rollbackTarget := previous
	var persistencePrevious *Auth
	var persistenceNext *Auth
	persistenceBindingChanged := false
	if persistenceUpdate != nil {
		if persistenceExpected == nil || persistenceExpected.ID == "" ||
			persistenceExpected.ID != persistenceUpdate.ID {
			m.mu.RUnlock()
			m.executorMu.RUnlock()
			return nil, errors.New("credential refresh persistence owner is invalid")
		}
		persistenceCurrent := m.auths[persistenceExpected.ID]
		if persistenceCurrent == nil || persistenceCurrent.Generation != persistenceExpected.Generation {
			m.mu.RUnlock()
			m.executorMu.RUnlock()
			return nil, errors.New("credential refresh persistence owner is stale")
		}
		persistencePrevious = persistenceCurrent.Clone()
		persistenceNext, persistenceBindingChanged = mergeRefreshedAuthState(persistenceCurrent, persistenceUpdate)
		persistenceNext.Runtime = persistenceCurrent.Runtime
		persistTarget = persistenceNext
		rollbackTarget = persistenceCurrent.Clone()
		next = current.Clone()
		next.Runtime = current.Runtime
		bindingChanged = false
	}
	m.mu.RUnlock()
	m.executorMu.RUnlock()

	if next.Runtime == nil {
		next.Runtime = previous.Runtime
	}
	next.LastRefreshedAt = now
	next.NextRefreshAfter = time.Time{}
	next.RefreshFailures = 0
	next.rejectedAccessTokenSHA256 = ""
	if hasUnauthorizedAuthFailure(next) || isInvalidGrantResultError(next.LastError) {
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
	clearUnauthorizedModelStates(next, now)
	if m.shouldRefresh(next, now) {
		next.NextRefreshAfter = now.Add(refreshIneffectiveBackoff)
	}
	if persistenceNext != nil {
		persistenceNext.LastRefreshedAt = now
		persistenceNext.NextRefreshAfter = time.Time{}
		persistenceNext.RefreshFailures = 0
		persistenceNext.rejectedAccessTokenSHA256 = ""
		persistenceNext.UpdatedAt = now
	}
	completeBindingChanges := func(saved, persistenceSaved *Auth) {
		if bindingChanged && saved != nil {
			m.completeAuthBindingChange(ctx, previous, saved)
		}
		if persistenceBindingChanged && persistenceSaved != nil {
			m.completeAuthBindingChange(ctx, persistencePrevious, persistenceSaved)
		}
	}

	publishUncertain := func(cause error, retry bool) (*Auth, error) {
		saved, persistenceSaved := m.publishRefreshedAuthStates(
			expected,
			next,
			persistenceExpected,
			persistenceNext,
			expectedExecutor,
			guard,
		)
		if saved == nil {
			return nil, errors.Join(
				errAuthDurableStateUncertain,
				cause,
				errors.New("credential refresh result became stale before runtime publication"),
			)
		}
		if persistenceSaved == nil {
			persistenceSaved = saved
		}
		completeBindingChanges(saved, persistenceSaved)
		if persistenceSaved.ID != saved.ID {
			m.hook.OnAuthUpdated(ctx, persistenceSaved.Clone())
		}
		if retry && !m.scheduleRefreshPersistenceRetry(persistenceSaved, expectedVersion) {
			cause = errors.Join(cause, errors.New("could not schedule refreshed credential persistence retry"))
		}
		return saved, errors.Join(errAuthDurableStateUncertain, cause)
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
			return nil, errors.Join(
				errors.New("credential refresh owner changed after persistence"),
				errRollback,
			)
		}
		return nil, errors.New("credential refresh owner changed after persistence")
	}
	unlockPersistence()
	unlockFile()
	completeBindingChanges(saved, persistenceSaved)
	if persistenceSaved != nil && persistenceSaved.ID != saved.ID {
		m.hook.OnAuthUpdated(ctx, persistenceSaved.Clone())
	}
	return saved, nil
}

func (m *Manager) executorFor(provider string) ProviderExecutor {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.executors[provider]
}

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
