package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
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
	quotaBackoffBase      = time.Second
	quotaBackoffMax       = 30 * time.Minute
)

var quotaCooldownDisabled atomic.Bool

// SetQuotaCooldownDisabled toggles quota cooldown scheduling globally.
func SetQuotaCooldownDisabled(disable bool) {
	quotaCooldownDisabled.Store(disable)
}

func quotaCooldownDisabledForAuth(auth *Auth) bool {
	if auth != nil {
		if override, ok := auth.DisableCoolingOverride(); ok {
			return override
		}
	}
	return quotaCooldownDisabled.Load()
}

// Result captures execution outcome used to adjust auth state.
type Result struct {
	// AuthID references the auth that produced this result.
	AuthID string
	// Provider is copied for convenience when emitting hooks.
	Provider string
	// Model is the upstream model identifier used for the request.
	Model string
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

// Manager orchestrates auth lifecycle, selection, execution, and persistence.
type Manager struct {
	store            Store
	executors        map[string]ProviderExecutor
	selector         Selector
	hook             Hook
	mu               sync.RWMutex
	persistMu        sync.Mutex
	auths            map[string]*Auth
	scheduler        *authScheduler
	pluginScheduler  PluginScheduler
	homeRuntimeAuths map[string]map[string]*Auth
	removedAuths     map[string]uint64
	removedAuthSeq   uint64
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

	requestPrepareLocks sync.Map

	// Auto refresh state
	refreshCancel    context.CancelFunc
	refreshSemaphore chan struct{}
}

// NewManager constructs a manager with optional custom selector and hook.
func NewManager(store Store, selector Selector, hook Hook) *Manager {
	if selector == nil {
		selector = &RoundRobinSelector{}
	}
	if hook == nil {
		hook = NoopHook{}
	}
	manager := &Manager{
		store:            store,
		executors:        make(map[string]ProviderExecutor),
		selector:         selector,
		hook:             hook,
		auths:            make(map[string]*Auth),
		homeRuntimeAuths: make(map[string]map[string]*Auth),
		removedAuths:     make(map[string]uint64),
		providerOffsets:  make(map[string]int),
		refreshSemaphore: make(chan struct{}, refreshMaxConcurrency),
	}
	// atomic.Value requires non-nil initial value.
	manager.runtimeConfig.Store(&internalconfig.Config{})
	manager.apiKeyModelAlias.Store(apiKeyModelAliasTable(nil))
	manager.scheduler = newAuthScheduler(selector)
	return manager
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
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
}

// SetRoundTripperProvider register a provider that returns a per-auth RoundTripper.
func (m *Manager) SetRoundTripperProvider(p RoundTripperProvider) {
	m.mu.Lock()
	m.rtProvider = p
	m.mu.Unlock()
}

// SetConfig updates the runtime config snapshot used by request-time helpers.
// Callers should provide the latest config on reload so per-credential alias mapping stays in sync.
func (m *Manager) SetConfig(cfg *internalconfig.Config) {
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
	if executor == nil {
		return
	}
	provider := strings.TrimSpace(executor.Identifier())
	if provider == "" {
		return
	}

	var replaced ProviderExecutor
	m.mu.Lock()
	replaced = m.executors[provider]
	m.executors[provider] = executor
	m.mu.Unlock()

	if replaced == nil || replaced == executor {
		return
	}
	if closer, ok := replaced.(ExecutionSessionCloser); ok && closer != nil {
		closer.CloseExecutionSession(CloseAllExecutionSessionsID)
	}
}

// UnregisterExecutor removes the executor associated with the provider key.
func (m *Manager) UnregisterExecutor(provider string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return
	}
	m.mu.Lock()
	delete(m.executors, provider)
	m.mu.Unlock()
}

// Register inserts a new auth entry into the manager.
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil {
		return nil, nil
	}
	if auth.ID == "" {
		auth.ID = uuid.NewString()
	}
	m.mu.RLock()
	removedSeq, removed := m.removedAuths[auth.ID]
	m.mu.RUnlock()
	allowRecreate := shouldAllowAuthRecreate(ctx)
	removedAtStart := removed
	if removed && !allowRecreate {
		return nil, nil
	}
	auth.EnsureIndex()
	if err := m.persist(ctx, auth, allowRecreate, removedAtStart, removedSeq); err != nil {
		return nil, err
	}
	snapshot := auth.Clone()
	m.mu.Lock()
	if currentSeq, removed := m.removedAuths[auth.ID]; removed && (!allowRecreate || !removedAtStart || currentSeq != removedSeq) {
		m.mu.Unlock()
		return nil, nil
	}
	m.auths[auth.ID] = snapshot
	if allowRecreate && removedAtStart {
		delete(m.removedAuths, auth.ID)
	}
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot.Clone())
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	m.hook.OnAuthRegistered(ctx, auth.Clone())
	return auth.Clone(), nil
}

// Update replaces an existing auth entry and notifies hooks.
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error) {
	if auth == nil || auth.ID == "" {
		return nil, nil
	}
	allowRecreate := shouldAllowAuthRecreate(ctx)
	removedAtStart := false
	var removedSeq uint64
	m.mu.Lock()
	existing, ok := m.auths[auth.ID]
	if !ok || existing == nil {
		if seq, removed := m.removedAuths[auth.ID]; removed {
			removedAtStart = true
			removedSeq = seq
			if !allowRecreate {
				m.mu.Unlock()
				return nil, nil
			}
		}
	} else {
		if !auth.indexAssigned && auth.Index == "" {
			auth.Index = existing.Index
			auth.indexAssigned = existing.indexAssigned
		}
		auth.Success = existing.Success
		auth.Failed = existing.Failed
		auth.recentRequests = existing.recentRequests
	}
	auth.EnsureIndex()
	m.mu.Unlock()
	if err := m.persist(ctx, auth, allowRecreate, removedAtStart, removedSeq); err != nil {
		return nil, err
	}
	snapshot := auth.Clone()
	m.mu.Lock()
	if currentSeq, removed := m.removedAuths[auth.ID]; removed && (!allowRecreate || !removedAtStart || currentSeq != removedSeq) {
		m.mu.Unlock()
		return nil, nil
	}
	m.auths[auth.ID] = snapshot
	if allowRecreate && removedAtStart {
		delete(m.removedAuths, auth.ID)
	}
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot.Clone())
	}
	m.rebuildAPIKeyModelAliasFromRuntimeConfig()
	m.hook.OnAuthUpdated(ctx, auth.Clone())
	return auth.Clone(), nil
}

func (m *Manager) Remove(ctx context.Context, id string) {
	if m == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	deletePersisted := m.store != nil && !shouldSkipPersist(ctx)

	m.mu.Lock()
	existing := m.auths[id]
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

	if deletePersisted {
		m.persistMu.Lock()
		defer m.persistMu.Unlock()
		if err := m.store.Delete(ctx, id); err != nil {
			log.WithError(err).Warnf("failed to delete auth %s from store", id)
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
		auth.EnsureIndex()
		snapshot := auth.Clone()
		m.auths[auth.ID] = snapshot
		if len(m.removedAuths) > 0 {
			delete(m.removedAuths, auth.ID)
		}
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
				) (cliproxyexecutor.Response, error) {
					return executor.Execute(execCtx, auth, execReq, execOpts)
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
	routeModel := req.Model
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
				) (*cliproxyexecutor.StreamResult, error) {
					return m.executeStreamWithBootstrap(
						execCtx, auth, executor, provider, execReq, execOpts, routeModel,
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
		if errWait := waitForCooldown(ctx, wait); errWait != nil {
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
	publishSelectedAuthMetadata(creditsOpts.Metadata, auth.ID)
	return creditsCtx, cliproxyexecutor.Request{Model: routeModel}, creditsOpts
}

func (m *Manager) antigravityCreditsModel(routeModel string, auth *Auth) string {
	model := rewriteModelForAuth(routeModel, auth)
	model = m.applyOAuthModelAlias(auth, model)
	model = m.applyAPIKeyModelAlias(auth, model)
	return model
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
		execReq.Model = m.antigravityCreditsModel(req.Model, candidate.auth)
		execReq, creditsOpts = applyRequestAfterAuthInterceptor(
			creditsCtx, candidate.executor, candidate.provider, candidate.auth, execReq, creditsOpts, req.Model,
		)
		resp, errExec := candidate.executor.Execute(creditsCtx, candidate.auth, execReq, creditsOpts)
		result := Result{
			AuthID: candidate.auth.ID, Provider: candidate.provider, Model: req.Model, Success: errExec == nil,
		}
		if errExec != nil {
			result.Error = errorForResult(errExec)
			result.RetryAfter = retryAfterFromError(errExec)
			m.MarkResult(creditsCtx, result)
			continue
		}
		m.MarkResult(creditsCtx, result)
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
		execReq.Model = m.antigravityCreditsModel(req.Model, candidate.auth)
		execReq, creditsOpts = applyRequestAfterAuthInterceptor(
			creditsCtx, candidate.executor, candidate.provider, candidate.auth, execReq, creditsOpts, req.Model,
		)
		result, errStream := m.executeStreamWithBootstrap(
			creditsCtx, candidate.auth, candidate.executor, candidate.provider, execReq, creditsOpts, req.Model,
		)
		if errStream != nil {
			m.MarkResult(
				creditsCtx,
				Result{
					AuthID: candidate.auth.ID, Provider: candidate.provider, Model: req.Model, Success: false,
					Error: errorForResult(errStream), RetryAfter: retryAfterFromError(errStream),
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
	) (T, error),
	recordResult func(ctx context.Context, result Result),
) (T, error) {
	var zero T
	if len(providers) == 0 {
		return zero, &Error{Code: "provider_not_found", Message: "no provider supplied"}
	}
	routeModel := req.Model
	opts = ensureRequestedModelMetadata(opts, routeModel)
	homeMode := m.HomeEnabled()
	homeAuthCount := 1
	tried := make(map[string]struct{})
	var lastErr error
	for {
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
		publishSelectedAuthMetadata(opts.Metadata, auth.ID)

		tried[auth.ID] = struct{}{}
		execCtx := contextWithRequestedModelAlias(ctx, opts, routeModel)
		if rt := m.roundTripperFor(auth); rt != nil {
			execCtx = context.WithValue(execCtx, roundTripperContextKey{}, rt)
		}
		var errPrepare error
		auth, errPrepare = m.prepareRequestAuth(execCtx, executor, auth)
		if errPrepare != nil {
			result := Result{
				AuthID: auth.ID, Provider: provider, Model: routeModel, Success: false,
				Error: &Error{Message: errPrepare.Error()},
			}
			if se, ok := errors.AsType[cliproxyexecutor.StatusError](errPrepare); ok && se != nil {
				result.Error.HTTPStatus = se.StatusCode()
			}
			recordResult(execCtx, result)
			lastErr = errPrepare
			if homeMode {
				homeAuthCount++
			}
			continue
		}
		execReq := req
		if auth.Attributes != nil && strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey]) != "" {
			execReq.Model = strings.TrimSpace(auth.Attributes[homeUpstreamModelAttributeKey])
		} else {
			execReq.Model = rewriteModelForAuth(routeModel, auth)
			execReq.Model = m.applyOAuthModelAlias(auth, execReq.Model)
			execReq.Model = m.applyAPIKeyModelAlias(auth, execReq.Model)
		}
		execOpts := opts
		execReq, execOpts = applyRequestAfterAuthInterceptor(
			execCtx, executor, provider, auth, execReq, execOpts, routeModel,
		)
		execReq = sanitizeDownstreamWebsocketFallbackRequest(execCtx, auth, execReq)
		resp, errExec := executeFn(execCtx, auth, executor, provider, execReq, execOpts)
		result := Result{AuthID: auth.ID, Provider: provider, Model: routeModel, Success: errExec == nil}
		if errExec != nil {
			if errCtx := execCtx.Err(); errCtx != nil {
				return zero, errCtx
			}
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
			if homeMode {
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
				Attributes: maps.Clone(auth.Attributes),
				Metadata:   cloneSchedulerAnyMap(auth.Metadata),
			},
		)
	}
	return out
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
		execCtx, streamAuth.Clone(), streamProvider, routeModel, streamResult.Headers, buffered, remaining,
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
) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	go func(streamCtx context.Context, streamChunks <-chan cliproxyexecutor.StreamChunk) {
		defer close(out)
		var failed bool
		forward := true
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				rerr := errorForResult(chunk.Err)
				m.MarkResult(
					streamCtx,
					Result{
						AuthID:   streamAuth.ID,
						Provider: streamProvider,
						Model:    routeModel,
						Success:  false,
						Error:    rerr,
					},
				)
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
		}
		for chunk := range streamChunks {
			if ok := emit(chunk); !ok {
				discardStreamChunks(streamChunks)
				return
			}
		}
		if !failed {
			m.MarkResult(
				streamCtx,
				Result{AuthID: streamAuth.ID, Provider: streamProvider, Model: routeModel, Success: true},
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
	mu sync.Mutex
}

func (m *Manager) prepareRequestAuth(ctx context.Context, executor ProviderExecutor, auth *Auth) (*Auth, error) {
	if m == nil || executor == nil || auth == nil {
		return auth, nil
	}
	preparer, ok := executor.(RequestAuthPreparer)
	if !ok || preparer == nil || !preparer.ShouldPrepareRequestAuth(auth) {
		return auth, nil
	}

	id := strings.TrimSpace(auth.ID)
	if id == "" {
		return preparer.PrepareRequestAuth(ctx, auth.Clone())
	}

	lockValue, _ := m.requestPrepareLocks.LoadOrStore(id, &requestAuthPrepareLock{})
	lock, ok := lockValue.(*requestAuthPrepareLock)
	if !ok || lock == nil {
		return preparer.PrepareRequestAuth(ctx, auth.Clone())
	}

	lock.mu.Lock()
	defer lock.mu.Unlock()

	target := auth.Clone()
	m.mu.RLock()
	if current := m.auths[id]; current != nil {
		target = current.Clone()
	}
	m.mu.RUnlock()

	if !preparer.ShouldPrepareRequestAuth(target) {
		return target, nil
	}

	updated, errPrepare := preparer.PrepareRequestAuth(ctx, target)
	if errPrepare != nil {
		return auth, errPrepare
	}
	if updated == nil {
		return target, nil
	}

	saved, errUpdate := m.Update(ctx, updated)
	if errUpdate != nil {
		return updated, errUpdate
	}
	if saved != nil {
		return saved, nil
	}
	return target, &Error{Code: "auth_removed", Message: "auth was removed during request preparation"}
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

func isFreeCodexAuth(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["plan_type"]), "free")
}

func selectionPredicate(disallowFreeAuth bool) func(*Auth) bool {
	if !disallowFreeAuth {
		return nil
	}
	return func(auth *Auth) bool {
		return !isFreeCodexAuth(auth)
	}
}

func ExtractSessionID(headers http.Header, body []byte, metadata map[string]any) string {
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

func publishSelectedAuthMetadata(meta map[string]any, authID string) {
	if len(meta) == 0 {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	meta[cliproxyexecutor.SelectedAuthMetadataKey] = authID
	if callback, ok := meta[cliproxyexecutor.SelectedAuthCallbackMetadataKey].(func(string)); ok && callback != nil {
		callback(authID)
	}
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

func waitForCooldown(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
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

	shouldResumeModel := false
	shouldSuspendModel := false
	suspendReason := ""
	clearModelQuota := false
	setModelQuota := false
	var persistAuth *Auth

	m.mu.Lock()
	if auth, ok := m.auths[result.AuthID]; ok && auth != nil {
		now := time.Now()
		auth.recordRecentRequest(now, result.Success)
		if result.Success {
			auth.Success++
		} else {
			auth.Failed++
		}

		if result.Success {
			if result.Model != "" {
				state := ensureModelState(auth, result.Model)
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
					state := ensureModelState(auth, result.Model)
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
					switch statusCode {
					case 401:
						next := now.Add(30 * time.Minute)
						state.NextRetryAfter = next
						suspendReason = "unauthorized"
						shouldSuspendModel = true
					case 402, 403:
						if isCloudflareChallengeResultError(result.Error) {
							next, backoffLevel := nextCloudflareCooldown(
								state.Quota.BackoffLevel,
								quotaCooldownDisabledForAuth(auth),
								now,
							)
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
						} else {
							next := now.Add(30 * time.Minute)
							state.NextRetryAfter = next
							suspendReason = "payment_required"
							shouldSuspendModel = true
						}
					case 404:
						next := now.Add(12 * time.Hour)
						state.NextRetryAfter = next
						suspendReason = "not_found"
						shouldSuspendModel = true
					case 429:
						var next time.Time
						backoffLevel := state.Quota.BackoffLevel
						if result.RetryAfter != nil {
							next = now.Add(*result.RetryAfter)
						} else {
							cooldown, nextLevel := nextQuotaCooldown(backoffLevel, quotaCooldownDisabledForAuth(auth))
							if cooldown > 0 {
								next = now.Add(cooldown)
							}
							backoffLevel = nextLevel
						}
						state.NextRetryAfter = next
						state.Quota = QuotaState{
							Exceeded:      true,
							Reason:        "quota",
							NextRecoverAt: next,
							BackoffLevel:  backoffLevel,
						}
						suspendReason = "quota"
						shouldSuspendModel = true
						setModelQuota = true
					case 408, 500, 502, 503, 504:
						if quotaCooldownDisabledForAuth(auth) {
							state.NextRetryAfter = time.Time{}
						} else {
							next := now.Add(1 * time.Minute)
							state.NextRetryAfter = next
						}
					default:
						state.NextRetryAfter = time.Time{}
					}

					auth.Status = StatusError
					auth.UpdatedAt = now
					updateAggregatedAvailability(auth, now)
				}
			} else {
				applyAuthFailureState(auth, result.Error, result.RetryAfter, now)
			}
		}

		persistAuth = auth.Clone()
	}
	m.mu.Unlock()
	if persistAuth != nil {
		_ = m.persist(ctx, persistAuth, false, false, 0)
	}

	if clearModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().ClearModelQuotaExceeded(result.AuthID, result.Model)
	}
	if setModelQuota && result.Model != "" {
		registry.GetGlobalRegistry().SetModelQuotaExceeded(result.AuthID, result.Model)
	}
	if shouldResumeModel {
		registry.GetGlobalRegistry().ResumeClientModel(result.AuthID, result.Model)
	} else if shouldSuspendModel {
		registry.GetGlobalRegistry().SuspendClientModel(result.AuthID, result.Model, suspendReason)
	}

	m.hook.OnResult(ctx, result)
}

func ensureModelState(auth *Auth, model string) *ModelState {
	if auth == nil || model == "" {
		return nil
	}
	if auth.ModelStates == nil {
		auth.ModelStates = make(map[string]*ModelState)
	}
	if state, ok := auth.ModelStates[model]; ok && state != nil {
		return state
	}
	state := &ModelState{Status: StatusActive}
	auth.ModelStates[model] = state
	return state
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

func applyAuthFailureState(auth *Auth, resultErr *Error, retryAfter *time.Duration, now time.Time) {
	if auth == nil {
		return
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
		next, backoffLevel := nextCloudflareCooldown(auth.Quota.BackoffLevel, quotaCooldownDisabledForAuth(auth), now)
		auth.Quota = QuotaState{
			Exceeded:      true,
			Reason:        "cloudflare challenge",
			NextRecoverAt: next,
			BackoffLevel:  backoffLevel,
		}
		auth.NextRetryAfter = next
		return
	}
	switch statusCode {
	case 401:
		auth.StatusMessage = "unauthorized"
		auth.NextRetryAfter = now.Add(30 * time.Minute)
	case 402, 403:
		auth.StatusMessage = "payment_required"
		auth.NextRetryAfter = now.Add(30 * time.Minute)
	case 404:
		auth.StatusMessage = "not_found"
		auth.NextRetryAfter = now.Add(12 * time.Hour)
	case 429:
		auth.StatusMessage = "quota exhausted"
		auth.Quota.Exceeded = true
		auth.Quota.Reason = "quota"
		var next time.Time
		if retryAfter != nil {
			next = now.Add(*retryAfter)
		} else {
			cooldown, nextLevel := nextQuotaCooldown(auth.Quota.BackoffLevel, quotaCooldownDisabledForAuth(auth))
			if cooldown > 0 {
				next = now.Add(cooldown)
			}
			auth.Quota.BackoffLevel = nextLevel
		}
		auth.Quota.NextRecoverAt = next
		auth.NextRetryAfter = next
	case 408, 500, 502, 503, 504:
		auth.StatusMessage = "transient upstream error"
		if quotaCooldownDisabledForAuth(auth) {
			auth.NextRetryAfter = time.Time{}
		} else {
			auth.NextRetryAfter = now.Add(1 * time.Minute)
		}
	default:
		if auth.StatusMessage == "" {
			auth.StatusMessage = "request failed"
		}
	}
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
	requestPredicate := selectionPredicate(disallowFreeAuthFromMetadata(opts.Metadata))

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
		authCopy := selected.Clone()
		return authCopy, executor, nil
	}
	if m.scheduler != nil && isBuiltInSelector(selector) {
		var stickySelector *SessionAffinitySelector
		if selector, ok := selector.(*SessionAffinitySelector); ok {
			stickySelector = selector
		}
		if stickySelector != nil {
			if sticky := stickySelector.PickSticky(provider, opts, candidates); sticky != nil {
				executor, ok := m.Executor(provider)
				if !ok || executor == nil {
					return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
				}
				return sticky.Clone(), executor, nil
			}
		}
		selected, errPick := m.scheduler.pickSingleWithPredicate(ctx, provider, model, opts, tried, requestPredicate)
		if errPick != nil {
			return nil, nil, errPick
		}
		if stickySelector != nil {
			stickySelector.Remember(provider, opts, selected)
		}
		executor, ok := m.Executor(provider)
		if !ok || executor == nil {
			return nil, nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		return selected, executor, nil
	}
	selected, errPick := selector.Pick(ctx, provider, model, opts, candidates)
	if errPick != nil {
		return nil, nil, errPick
	}
	if selected == nil {
		return nil, nil, &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	authCopy := selected.Clone()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, nil
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
	requestPredicate := selectionPredicate(disallowFreeAuthFromMetadata(opts.Metadata))

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
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "no auth available"}
	}
	if selected, handled, errPick := m.pickViaPluginScheduler(
		ctx, pluginScheduler, "mixed", providers, model, opts, candidates,
	); errPick != nil {
		return nil, nil, "", errPick
	} else if handled {
		providerKey := strings.TrimSpace(strings.ToLower(selected.Provider))
		executor, okExecutor := m.Executor(providerKey)
		if !okExecutor {
			return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		return selected.Clone(), executor, providerKey, nil
	}
	if m.scheduler != nil && isBuiltInSelector(selector) {
		var stickySelector *SessionAffinitySelector
		if selector, ok := selector.(*SessionAffinitySelector); ok {
			stickySelector = selector
		}
		if stickySelector != nil {
			if sticky := stickySelector.PickSticky("mixed", opts, candidates); sticky != nil {
				providerKey := strings.TrimSpace(strings.ToLower(sticky.Provider))
				executor, ok := m.Executor(providerKey)
				if !ok || executor == nil {
					return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
				}
				return sticky.Clone(), executor, providerKey, nil
			}
		}
		filteredProviders := make([]string, 0, len(providers))
		for _, provider := range providers {
			if executor, ok := m.Executor(strings.TrimSpace(strings.ToLower(provider))); ok && executor != nil {
				filteredProviders = append(filteredProviders, provider)
			}
		}
		selected, providerKey, errPick := m.scheduler.pickMixedWithPredicate(
			ctx, filteredProviders, model, opts, tried, requestPredicate,
		)
		if errPick != nil {
			return nil, nil, "", errPick
		}
		if stickySelector != nil {
			stickySelector.Remember("mixed", opts, selected)
		}
		executor, ok := m.Executor(providerKey)
		if !ok || executor == nil {
			return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
		}
		return selected, executor, providerKey, nil
	}
	selected, errPick := selector.Pick(ctx, "mixed", model, opts, candidates)
	if errPick != nil {
		return nil, nil, "", errPick
	}
	if selected == nil {
		return nil, nil, "", &Error{Code: "auth_not_found", Message: "selector returned no auth"}
	}
	providerKey := strings.TrimSpace(strings.ToLower(selected.Provider))
	executor, okExecutor := m.Executor(providerKey)
	if !okExecutor {
		return nil, nil, "", &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	authCopy := selected.Clone()
	if !selected.indexAssigned {
		m.mu.Lock()
		if current := m.auths[authCopy.ID]; current != nil && !current.indexAssigned {
			current.EnsureIndex()
			authCopy = current.Clone()
		}
		m.mu.Unlock()
	}
	return authCopy, executor, providerKey, nil
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

var currentHomeDispatcher = func() homeAuthDispatcher {
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

func (m *Manager) rememberHomeRuntimeAuth(sessionID string, auth *Auth) {
	sessionID = strings.TrimSpace(sessionID)
	authID := ""
	if auth != nil {
		authID = strings.TrimSpace(auth.ID)
	}
	if m == nil || auth == nil || sessionID == "" || authID == "" || !authWebsocketsEnabled(auth) {
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
	if auth == nil || !authWebsocketsEnabled(auth) {
		return nil, nil, "", false
	}
	providerKey := strings.ToLower(strings.TrimSpace(auth.Provider))
	if providerKey == "" {
		return nil, nil, "", false
	}
	executor, ok := m.Executor(providerKey)
	if !ok && auth.Attributes != nil && strings.TrimSpace(auth.Attributes["base_url"]) != "" {
		executor, ok = m.Executor("openai-compatibility")
		if ok {
			providerKey = "openai-compatibility"
		}
	}
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
	if cliproxyexecutor.DownstreamWebsocket(ctx) && executionSessionID != "" && count <= 1 {
		if pinnedAuthID := pinnedAuthIDFromMetadata(opts.Metadata); pinnedAuthID != "" {
			_, alreadyTried := tried[pinnedAuthID]
			if !alreadyTried {
				if auth, executor, providerKey, ok := m.homeRuntimeAuthByID(executionSessionID, pinnedAuthID); ok {
					return auth, executor, providerKey, nil
				}
			}
		}
	}

	client := currentHomeDispatcher()
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
	authCopy := auth.Clone()
	if cliproxyexecutor.DownstreamWebsocket(ctx) && executionSessionID != "" && authWebsocketsEnabled(authCopy) {
		m.rememberHomeRuntimeAuth(executionSessionID, authCopy)
	}
	return authCopy, executor, providerKey, nil
}

func (m *Manager) persist(
	ctx context.Context,
	auth *Auth,
	allowRecreate bool,
	removedAtStart bool,
	removedSeq uint64,
) error {
	if m.store == nil || auth == nil {
		return nil
	}
	if shouldSkipPersist(ctx) {
		return nil
	}
	if auth.Attributes != nil {
		if v := strings.ToLower(strings.TrimSpace(auth.Attributes["runtime_only"])); v == "true" {
			return nil
		}
	}
	// Skip persistence when metadata is absent (e.g., runtime-only auths).
	if auth.Metadata == nil {
		return nil
	}
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	m.mu.RLock()
	currentSeq, removed := m.removedAuths[auth.ID]
	m.mu.RUnlock()
	if removed && (!allowRecreate || !removedAtStart || currentSeq != removedSeq) {
		return nil
	}
	_, err := m.store.Save(ctx, auth)
	return err
}

// StartAutoRefresh launches a background loop that evaluates auth freshness
// every few seconds and triggers refresh operations when required.
// Only one loop is kept alive; starting a new one cancels the previous run.
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = refreshCheckInterval
	}
	if m.refreshCancel != nil {
		m.refreshCancel()
		m.refreshCancel = nil
	}
	ctx, cancel := context.WithCancel(parent)
	m.refreshCancel = cancel
	go func() {
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

// StopAutoRefresh cancels the background refresh loop, if running.
func (m *Manager) StopAutoRefresh() {
	if m.refreshCancel != nil {
		m.refreshCancel()
		m.refreshCancel = nil
	}
	m.mu.RLock()
	selector := m.selector
	m.mu.RUnlock()
	if stoppable, ok := selector.(StoppableSelector); ok && stoppable != nil {
		stoppable.Stop()
	}
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
			if !m.markRefreshPending(a.ID, now) {
				continue
			}
			go m.refreshAuthWithLimit(ctx, a.ID)
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

func (m *Manager) refreshAuth(ctx context.Context, id string) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	auth := m.auths[id]
	var exec ProviderExecutor
	if auth != nil {
		exec = m.executors[auth.Provider]
	}
	m.mu.RUnlock()
	if auth == nil || exec == nil {
		return
	}
	cloned := auth.Clone()
	updated, err := exec.Refresh(ctx, cloned)
	if err != nil && errors.Is(err, context.Canceled) {
		log.Debugf("refresh canceled for %s, %s", auth.Provider, auth.ID)
		return
	}
	log.Debugf("refreshed %s, %s, %v", auth.Provider, auth.ID, err)
	now := time.Now()
	if err != nil {
		unauthorized := isUnauthorizedError(err)
		var snapshot *Auth
		m.mu.Lock()
		if current := m.auths[id]; current != nil {
			current.LastError = refreshErrorFromError(err)
			if unauthorized {
				current.NextRefreshAfter = time.Time{}
				current.Unavailable = true
				current.Status = StatusError
				current.StatusMessage = "unauthorized"
			} else {
				current.NextRefreshAfter = now.Add(refreshFailureBackoff)
			}
			snapshot = current.Clone()
			m.auths[id] = current
		}
		m.mu.Unlock()
		if snapshot != nil && m.scheduler != nil {
			m.scheduler.upsertAuth(snapshot)
		}
		return
	}
	if updated == nil {
		updated = cloned
	}
	// Preserve runtime created by the executor during Refresh.
	// If executor didn't set one, fall back to the previous runtime.
	if updated.Runtime == nil {
		updated.Runtime = auth.Runtime
	}
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.UpdatedAt = now
	_, _ = m.Update(ctx, updated)
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
	if ctx != nil {
		*req = *req.WithContext(ctx)
	}
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	preparer, ok := exec.(RequestPreparer)
	if !ok || preparer == nil {
		return &Error{Code: "not_supported", Message: "executor does not support http request preparation"}
	}
	return preparer.PrepareRequest(req, auth)
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
	providerKey := executorKeyFromAuth(auth)
	if providerKey == "" {
		return nil, &Error{Code: "provider_not_found", Message: "auth provider is empty"}
	}
	exec := m.executorFor(providerKey)
	if exec == nil {
		return nil, &Error{Code: "provider_not_found", Message: "executor not registered for provider: " + providerKey}
	}
	return exec.HttpRequest(ctx, auth, req)
}
