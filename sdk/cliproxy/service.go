// Package cliproxy provides the core service implementation for the CLI Proxy API.
// It includes service lifecycle management, authentication handling, file watching,
// and integration with various AI service providers through a unified interface.
package cliproxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internalapi "github.com/Pyrokine/CLIProxyAPI/v6/internal/api"
	internalauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/homeplugins"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	internalUsage "github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/diff"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/wsrelay"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	sdkapi "github.com/Pyrokine/CLIProxyAPI/v6/sdk/api"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

// Service wraps the proxy server lifecycle so external programs can embed the CLI proxy.
// It manages the complete lifecycle including authentication, file watching, HTTP server,
// and integration with various AI service providers.
var (
	serviceShutdownTimeout         = 30 * time.Second
	modelRefreshTimeout            = 15 * time.Second
	pluginRuntimeRetryInitialDelay = time.Second
	fetchAntigravityModels         = executor.FetchAntigravityModels
)

const (
	modelRefreshWorkerCount       = 4
	modelRefreshCacheTTL          = 3 * time.Hour
	modelRefreshRetryBase         = time.Minute
	modelRefreshRetryMax          = 30 * time.Minute
	modelRefreshScanInterval      = time.Minute
	pluginRuntimeRetryMaxAttempts = 5
)

type modelDiscoverySource string

type modelDiscoveryOutcome string

const (
	modelDiscoverySourcePlugin      modelDiscoverySource = "plugin"
	modelDiscoverySourceAntigravity modelDiscoverySource = "antigravity"

	modelDiscoveryAuthoritativePositive modelDiscoveryOutcome = "authoritative_positive"
	modelDiscoveryAuthoritativeNegative modelDiscoveryOutcome = "authoritative_negative"
	modelDiscoveryPartial               modelDiscoveryOutcome = "partial"
	modelDiscoveryInconclusive          modelDiscoveryOutcome = "inconclusive"
	modelDiscoveryFailure               modelDiscoveryOutcome = "failure"
)

type modelRefreshIdentity struct {
	key             string
	provider        string
	authID          string
	authIndex       string
	authGeneration  uint64
	endpoint        string
	mappingRevision string
	pluginSnapshot  *pluginhost.Snapshot
	tryPlugin       bool
}

func (i modelRefreshIdentity) sameSubject(other modelRefreshIdentity) bool {
	return i.provider == other.provider &&
		i.authID == other.authID &&
		i.authIndex == other.authIndex &&
		i.endpoint == other.endpoint &&
		i.mappingRevision == other.mappingRevision &&
		i.pluginSnapshot == other.pluginSnapshot &&
		i.tryPlugin == other.tryPlugin
}

type modelDiscoveryResult struct {
	source       modelDiscoverySource
	outcome      modelDiscoveryOutcome
	provider     string
	models       []*ModelInfo
	auth         *coreauth.Auth
	pluginResult pluginhost.AuthModelResult
	err          error
}

type modelRefreshCacheEntry struct {
	identity      modelRefreshIdentity
	source        modelDiscoverySource
	outcome       modelDiscoveryOutcome
	provider      string
	models        []*ModelInfo
	observedAt    time.Time
	registryEpoch uint64
	pluginResult  pluginhost.AuthModelResult
}

type modelRefreshFailureState struct {
	identityKey string
	jobVersion  uint64
	failures    uint8
	lastFailure time.Time
	nextRetry   time.Time
}

func nextModelRefreshFailure(previous modelRefreshFailureState, now time.Time, randomInt64N func(int64) int64) modelRefreshFailureState {
	if now.Sub(previous.lastFailure) > 2*modelRefreshCacheTTL {
		previous.failures = 0
	}
	failures := min(previous.failures, 4) + 1
	window := min(2*modelRefreshRetryBase<<(failures-1), modelRefreshRetryMax)
	half := window / 2
	delay := half + time.Duration(randomInt64N(int64(half)+1))
	return modelRefreshFailureState{
		identityKey: previous.identityKey,
		failures:    failures,
		lastFailure: now,
		nextRetry:   now.Add(delay),
	}
}

type modelRefreshJob struct {
	auth          *coreauth.Auth
	cfg           *config.Config
	identity      modelRefreshIdentity
	version       uint64
	registryEpoch uint64
	tryPlugin     bool
}

type serviceAuthApplyLock struct {
	token                chan struct{}
	refs                 int
	forgetRemovedRuntime bool
}

func newServiceAuthApplyLock() *serviceAuthApplyLock {
	lock := &serviceAuthApplyLock{token: make(chan struct{}, 1)}
	lock.token <- struct{}{}
	return lock
}

type Service struct {
	// cfg holds the current application configuration.
	cfg *config.Config

	// cfgMu protects concurrent access to the configuration.
	cfgMu                      sync.RWMutex
	configUpdateMu             sync.Mutex
	configPublicationMu        sync.Mutex
	configPublicationCount     int
	configPublicationIdle      chan struct{}
	authApplyMu                sync.Mutex
	authApplyLocks             map[string]*serviceAuthApplyLock
	authUpdateMu               sync.Mutex
	authUpdateRevisions        map[string]uint64
	managementConfigGeneration uint64

	// configPath is the path to the configuration file.
	configPath string

	// tokenProvider handles loading token-based clients.
	tokenProvider TokenClientProvider

	// apiKeyProvider handles loading API key-based clients.
	apiKeyProvider APIKeyClientProvider

	// watcherFactory creates file watcher instances.
	watcherFactory WatcherFactory

	// hooks provides lifecycle callbacks.
	hooks Hooks

	// serverOptions contains additional server configuration options.
	serverOptions []sdkapi.ServerOption

	// server is the HTTP API server instance.
	server *internalapi.Server

	// pprofServer manages the optional pprof HTTP debug server.
	pprofServer *pprofServer

	// serverErr channel for server startup/shutdown errors.
	serverErr chan error

	// watcher handles file system monitoring.
	watcherMu sync.RWMutex
	watcher   *WatcherWrapper

	// watcherCancel cancels the watcher context.
	watcherCancel context.CancelFunc

	// authUpdates channel for authentication updates.
	authUpdates chan AuthUpdate

	// authQueueStop cancels the auth update queue processing.
	authQueueStop    context.CancelFunc
	authQueueWorkers sync.WaitGroup

	// authManager handles legacy authentication operations.
	authManager *sdkAuth.Manager

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// coreManager handles core authentication and execution.
	coreManager *coreauth.Manager

	// cooldownStateStore persists runtime cooldown state when enabled.
	cooldownStateStore coreauth.CooldownStateStore

	// pluginHost manages dynamic plugin lifecycle and callbacks.
	pluginHost *pluginhost.Host

	// shutdownOnce starts shared shutdown work only once.
	shutdownOnce sync.Once
	shutdownMu   sync.Mutex
	shutdownDone chan struct{}
	shutdownErr  error
	shuttingDown atomic.Bool

	// wsGateway manages websocket Gemini providers.
	wsGateway *wsrelay.Manager

	// usageManager and usagePersister are owned by this service instance.
	usageManager           *usage.Manager
	usageStats             *internalUsage.RequestStatistics
	usagePersister         *internalUsage.Persister
	usageQueue             *redisqueue.Queue
	usageStatisticsEnabled atomic.Bool
	usageDataDirMigrator   func(string, string) error

	homeLifecycleMu     sync.Mutex
	homeClientMu        sync.RWMutex
	homeClient          *home.Client
	homeRegistry        *executionregistry.Registry
	homeDispatchBundle  *coreauth.HomeDispatchBundle
	homeCancel          context.CancelFunc
	homeSupervisor      *homeSubscriberSupervisor
	homeGeneration      uint64
	homeDrainBound      time.Duration
	homeLogForwarder    *logging.HomeAppLogForwarder
	homeLogForwarderFor *home.Client
	homeLogScopeMu      sync.Mutex
	homeLogScopeRelease func()
	homePluginSyncMu    sync.Mutex
	homePluginSyncKey   string

	modelRefreshMu           sync.Mutex
	modelRefreshCancel       map[string]context.CancelFunc
	modelRefreshVersion      map[string]uint64
	modelRefreshSeq          uint64
	modelRefreshCache        map[string]modelRefreshCacheEntry
	modelRefreshFailures     map[string]modelRefreshFailureState
	modelRefreshPending      map[string]modelRefreshJob
	modelRefreshActive       map[string]modelRefreshJob
	modelRefreshGroup        singleflight.Group
	modelRefreshWake         chan struct{}
	modelRefreshScheduleWake chan struct{}
	modelRefreshStop         context.CancelFunc
	modelRefreshWorkers      sync.WaitGroup
	modelRefreshDiscoveries  sync.WaitGroup
	modelRefreshSlots        chan struct{}
	modelRefreshClosed       bool

	pluginRuntimeRetryMu     sync.Mutex
	pluginRuntimeRetryCancel context.CancelFunc
	pluginRuntimeRetryWG     sync.WaitGroup
	pluginRuntimeRetryClosed bool
}

func (s *Service) currentWatcher() *WatcherWrapper {
	if s == nil {
		return nil
	}
	s.watcherMu.RLock()
	defer s.watcherMu.RUnlock()
	return s.watcher
}

func (s *Service) setWatcher(watcher *WatcherWrapper) {
	if s == nil {
		return
	}
	s.watcherMu.Lock()
	s.watcher = watcher
	s.watcherMu.Unlock()
}

// SetResultPolicy sets an execution result policy on the core auth manager.
func (s *Service) SetResultPolicy(policy coreauth.ResultPolicy) {
	if s == nil || s.coreManager == nil {
		return
	}
	s.coreManager.SetResultPolicy(policy)
}

// ResultPolicy returns the execution result policy configured on the core auth manager.
func (s *Service) ResultPolicy() coreauth.ResultPolicy {
	if s == nil || s.coreManager == nil {
		return nil
	}
	return s.coreManager.ResultPolicy()
}

// RegisterUsagePlugin registers a usage plugin on this service's dispatcher.
// This allows external code to monitor API usage and token consumption.
//
// Parameters:
//   - plugin: The usage plugin to register
func (s *Service) RegisterUsagePlugin(plugin usage.Plugin) {
	if s == nil || plugin == nil {
		return
	}
	if s.usageManager == nil {
		s.usageManager = usage.NewManager(512)
	}
	s.usageManager.Register(plugin)
}

func (s *Service) syncPluginRuntime(ctx context.Context) error {
	if err := s.syncPluginRuntimeConfig(ctx); err != nil {
		return err
	}
	s.syncPluginModelRuntime(ctx)
	return nil
}

func (s *Service) schedulePluginRuntimeRetry() {
	if s == nil || s.pluginHost == nil || s.shuttingDown.Load() {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.pluginRuntimeRetryMu.Lock()
	if s.pluginRuntimeRetryClosed {
		s.pluginRuntimeRetryMu.Unlock()
		cancel()
		return
	}
	if s.pluginRuntimeRetryCancel != nil {
		s.pluginRuntimeRetryCancel()
	}
	s.pluginRuntimeRetryCancel = cancel
	s.pluginRuntimeRetryWG.Add(1)
	s.pluginRuntimeRetryMu.Unlock()

	go func() {
		defer s.pluginRuntimeRetryWG.Done()
		delay := pluginRuntimeRetryInitialDelay
		for attempt := 1; attempt <= pluginRuntimeRetryMaxAttempts; attempt++ {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			errRetry := s.applyPluginRuntimeRetry(ctx)
			if errRetry == nil {
				log.WithField("attempt", attempt).Info("plugin runtime retry succeeded")
				return
			}
			log.WithError(errRetry).WithField("attempt", attempt).Warn(
				"plugin runtime retry failed",
			)
			delay *= 2
		}
		log.Error("plugin runtime retry attempts exhausted; restart may be required")
	}()
}

func (s *Service) applyPluginRuntimeRetry(ctx context.Context) error {
	apply := func() error {
		return s.syncPluginRuntime(ctx)
	}
	if s != nil && s.server != nil {
		if handler := s.server.ManagementHandler(); handler != nil {
			return handler.ApplyRuntimeUpdate(apply)
		}
	}
	return apply()
}

func (s *Service) stopPendingPluginRuntimeRetry() {
	if s == nil {
		return
	}
	s.pluginRuntimeRetryMu.Lock()
	if s.pluginRuntimeRetryCancel != nil {
		s.pluginRuntimeRetryCancel()
		s.pluginRuntimeRetryCancel = nil
	}
	s.pluginRuntimeRetryMu.Unlock()
}

func (s *Service) stopPluginRuntimeRetry(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.pluginRuntimeRetryMu.Lock()
	s.pluginRuntimeRetryClosed = true
	if s.pluginRuntimeRetryCancel != nil {
		s.pluginRuntimeRetryCancel()
		s.pluginRuntimeRetryCancel = nil
	}
	s.pluginRuntimeRetryMu.Unlock()

	done := make(chan struct{})
	go func() {
		s.pluginRuntimeRetryWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop plugin runtime retry: %w", ctx.Err())
	}
}

func (s *Service) syncPluginRuntimeConfig(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("cliproxy: service is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()

	watcher := s.currentWatcher()
	if s.pluginHost != nil {
		s.pluginHost.SetAccessManager(s.accessManager)
		if s.coreManager != nil {
			s.pluginHost.SetAffinityManager(s.coreManager)
		}
		if errApply := s.pluginHost.ApplyConfig(ctx, cfg); errApply != nil {
			return fmt.Errorf("cliproxy: apply plugin runtime config: %w", errApply)
		}
		if s.server != nil {
			s.server.RefreshPluginManagementRoutes()
		}
		if watcher != nil {
			watcher.SetPluginAuthParser(s.pluginHost)
		}
	}
	if s.coreManager != nil {
		s.coreManager.SetPluginScheduler(s.pluginHost)
	}
	if s.pluginHost == nil {
		if watcher != nil {
			watcher.SetPluginAuthParser(nil)
		}
		return nil
	}
	s.pluginHost.RegisterFrontendAuthProviders()
	if s.accessManager != nil {
		s.accessManager.SetProviders(sdkaccess.RegisteredProviders())
	}
	return nil
}

type catalogModelRegistryBase interface {
	RegisterClient(clientID, clientProvider string, models []*registry.ModelInfo)
	UnregisterClient(clientID string)
	GetModelProviders(modelID string) []string
}

type catalogModelRegistry struct {
	base   catalogModelRegistryBase
	filter func([]*registry.ModelInfo) []*registry.ModelInfo
}

func (r catalogModelRegistry) RegisterClient(clientID, clientProvider string, models []*registry.ModelInfo) {
	if r.base == nil {
		return
	}
	if r.filter != nil {
		models = r.filter(models)
	}
	r.base.RegisterClient(clientID, clientProvider, models)
}

func (r catalogModelRegistry) UnregisterClient(clientID string) {
	if r.base == nil {
		return
	}
	r.base.UnregisterClient(clientID)
}

func (r catalogModelRegistry) GetModelProviders(modelID string) []string {
	if r.base == nil {
		return nil
	}
	return r.base.GetModelProviders(modelID)
}

func (s *Service) pluginModelRegistry() catalogModelRegistry {
	return catalogModelRegistry{base: registry.GetGlobalRegistry(), filter: s.catalogEnabledRegistryModels}
}

func (s *Service) syncPluginModelRuntime(ctx context.Context) {
	if s == nil || s.pluginHost == nil || s.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	modelRegistry := s.pluginModelRegistry()
	s.pluginHost.RegisterModels(ctx, modelRegistry)
	s.pluginHost.RegisterExecutors(s.coreManager, modelRegistry)
	s.reprojectModelsForCurrentAuths()
}

// newDefaultAuthManager creates a default authentication manager with all supported providers.
func newDefaultAuthManager() *sdkAuth.Manager {
	return sdkAuth.NewManager(
		sdkAuth.GetTokenStore(),
		sdkAuth.NewGeminiAuthenticator(),
		sdkAuth.NewCodexAuthenticator(),
		sdkAuth.NewClaudeAuthenticator(),
		sdkAuth.NewQwenAuthenticator(),
		sdkAuth.NewXAIAuthenticator(),
		sdkAuth.NewDevinAuthenticator(),
	)
}

func (s *Service) ensureAuthUpdateQueue(ctx context.Context) {
	if s == nil {
		return
	}
	if s.authUpdates == nil {
		s.authUpdates = make(chan AuthUpdate, 256)
	}
	if s.authQueueStop != nil {
		return
	}
	queueCtx, cancel := context.WithCancel(ctx)
	s.authQueueStop = cancel
	s.authQueueWorkers.Add(1)
	go s.consumeAuthUpdates(queueCtx)
}

func (s *Service) consumeAuthUpdates(ctx context.Context) {
	defer s.authQueueWorkers.Done()
	ctx = coreauth.WithSkipPersist(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case update, ok := <-s.authUpdates:
			if !ok {
				return
			}
			s.handleAuthUpdate(ctx, update)
		labelDrain:
			for {
				select {
				case <-ctx.Done():
					return
				case nextUpdate := <-s.authUpdates:
					s.handleAuthUpdate(ctx, nextUpdate)
				default:
					break labelDrain
				}
			}
		}
	}
}

func (s *Service) stopAuthUpdateQueue(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if s.authQueueStop != nil {
		s.authQueueStop()
		s.authQueueStop = nil
	}
	done := make(chan struct{})
	go func() {
		s.authQueueWorkers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) emitAuthUpdate(ctx context.Context, update AuthUpdate) {
	if s == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if watcher := s.currentWatcher(); watcher != nil && watcher.DispatchRuntimeAuthUpdate(update) {
		return
	}
	if s.authUpdates != nil {
		select {
		case s.authUpdates <- update:
			return
		default:
			log.Debugf("auth update queue saturated, applying inline action=%v id=%s", update.Action, update.ID)
		}
	}
	s.handleAuthUpdate(ctx, update)
}

func (s *Service) handleAuthUpdate(ctx context.Context, update AuthUpdate) {
	if s == nil {
		return
	}
	s.authUpdateMu.Lock()
	errApply := s.handleAuthUpdateLocked(ctx, update)
	s.authUpdateMu.Unlock()
	if errApply != nil {
		id := strings.TrimSpace(update.ID)
		if id == "" && update.Auth != nil {
			id = strings.TrimSpace(update.Auth.ID)
		}
		log.WithError(errApply).Warnf("failed to apply auth update %s", id)
	}
}

// handleAuthUpdateLocked applies one update while the caller holds authUpdateMu.
func (s *Service) handleAuthUpdateLocked(ctx context.Context, update AuthUpdate) error {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil || s.coreManager == nil {
		return nil
	}
	if !s.acceptAuthUpdateRevisionLocked(update) {
		return nil
	}
	switch update.Action {
	case AuthUpdateActionAdd, AuthUpdateActionModify:
		if update.Auth == nil || update.Auth.ID == "" {
			return nil
		}
		updateCtx := ctx
		if update.AllowAuthRecreate {
			updateCtx = coreauth.WithAllowAuthRecreate(updateCtx)
		}
		return s.applyCoreAuthAddOrUpdate(updateCtx, update.Auth)
	case AuthUpdateActionDelete:
		id := update.ID
		if id == "" && update.Auth != nil {
			id = update.Auth.ID
		}
		if id == "" {
			return nil
		}
		s.applyCoreAuthRemoval(ctx, id)
	default:
		log.Debugf("received unknown auth update action: %v", update.Action)
	}
	return nil
}

func (s *Service) acceptAuthUpdateRevisionLocked(update AuthUpdate) bool {
	revision := update.Revision()
	if revision == 0 {
		return true
	}
	id := strings.TrimSpace(update.ID)
	if id == "" && update.Auth != nil {
		id = strings.TrimSpace(update.Auth.ID)
	}
	if id == "" {
		return true
	}
	if s.authUpdateRevisions == nil {
		s.authUpdateRevisions = make(map[string]uint64)
	}
	if previous, ok := s.authUpdateRevisions[id]; ok && revision <= previous {
		log.Debugf(
			"skipping stale auth update for %s: revision %d <= processed %d",
			id,
			revision,
			previous,
		)
		return false
	}
	s.authUpdateRevisions[id] = revision
	return true
}

func (s *Service) ensureWebsocketGateway() {
	if s == nil {
		return
	}
	if s.wsGateway != nil {
		return
	}
	opts := wsrelay.Options{
		Path:           "/v1/ws",
		OnConnected:    s.wsOnConnected,
		OnDisconnected: s.wsOnDisconnected,
		LogDebugf:      log.Debugf,
		LogInfof:       log.Infof,
		LogWarnf:       log.Warnf,
	}
	s.wsGateway = wsrelay.NewManager(opts)
}

func (s *Service) wsOnConnected(channelID string) {
	if s == nil || channelID == "" {
		return
	}
	if !strings.HasPrefix(strings.ToLower(channelID), "aistudio-") {
		return
	}
	if s.coreManager != nil {
		if existing, ok := s.coreManager.GetByID(channelID); ok && existing != nil {
			if !existing.Disabled && existing.Status == coreauth.StatusActive {
				return
			}
		}
	}
	now := time.Now().UTC()
	auth := &coreauth.Auth{
		ID:         channelID,  // keep channel identifier as ID
		Provider:   "aistudio", // logical provider for switch routing
		Label:      channelID,  // display original channel id
		Status:     coreauth.StatusActive,
		CreatedAt:  now,
		UpdatedAt:  now,
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"email": channelID}, // metadata drives logging and usage tracking
	}
	log.Infof("websocket provider connected: %s", channelID)
	s.emitAuthUpdate(
		context.Background(), AuthUpdate{
			Action: AuthUpdateActionAdd,
			ID:     auth.ID,
			Auth:   auth,
		},
	)
}

func (s *Service) wsOnDisconnected(channelID string, reason error) {
	if s == nil || channelID == "" {
		return
	}
	if reason != nil {
		if strings.Contains(reason.Error(), "replaced by new connection") {
			log.Infof("websocket provider replaced: %s", channelID)
			return
		}
		log.Warnf("websocket provider disconnected: %s (%v)", channelID, reason)
	} else {
		log.Infof("websocket provider disconnected: %s", channelID)
	}
	ctx := context.Background()
	s.emitAuthUpdate(
		ctx, AuthUpdate{
			Action: AuthUpdateActionDelete,
			ID:     channelID,
		},
	)
}

func (s *Service) lockAuthApply(ctx context.Context, id string) (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return func() {}, nil
	}
	s.authApplyMu.Lock()
	if s.authApplyLocks == nil {
		s.authApplyLocks = make(map[string]*serviceAuthApplyLock)
	}
	lock := s.authApplyLocks[id]
	if lock == nil {
		lock = newServiceAuthApplyLock()
		s.authApplyLocks[id] = lock
	}
	lock.refs++
	s.authApplyMu.Unlock()
	releaseRef := func() {
		forgetRemovedRuntime := false
		s.authApplyMu.Lock()
		lock.refs--
		if lock.refs == 0 && s.authApplyLocks[id] == lock {
			forgetRemovedRuntime = lock.forgetRemovedRuntime
			delete(s.authApplyLocks, id)
		}
		s.authApplyMu.Unlock()
		if forgetRemovedRuntime && s.coreManager != nil {
			s.coreManager.ForgetRemovedRuntimeAuth(id)
		}
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
	return unlock, nil
}

func isAntigravityAuth(auth *coreauth.Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity")
}

func (s *Service) applyCoreAuthAddOrUpdate(ctx context.Context, auth *coreauth.Auth) error {
	if s == nil || s.coreManager == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	auth = auth.Clone()
	unlockApply, errApplyLock := s.lockAuthApply(ctx, auth.ID)
	if errApplyLock != nil {
		return errApplyLock
	}
	defer unlockApply()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.coreManager.AuthRecreationIntentCurrent(ctx, auth.ID) {
		return nil
	}
	existing, hadExisting := s.coreManager.GetByID(auth.ID)
	if hadExisting && auth.Generation > 0 && auth.Generation < existing.Generation {
		log.Debugf("skipping stale auth snapshot %s: generation %d < %d", auth.ID, auth.Generation, existing.Generation)
		return s.publishCoreAuthRuntime(ctx, existing)
	}
	s.cancelModelRefresh(auth.ID)

	op := "register"
	var updated *coreauth.Auth
	var err error
	if hadExisting {
		auth.CreatedAt = existing.CreatedAt
		auth.LastRefreshedAt = existing.LastRefreshedAt
		auth.NextRefreshAfter = existing.NextRefreshAfter
		op = "update"
		if auth.Generation > 0 {
			updated, err = s.coreManager.MergePersistentUpdate(ctx, existing, auth)
		} else {
			updated, err = s.coreManager.Update(ctx, auth)
		}
	} else {
		updated, err = s.coreManager.Register(ctx, auth)
	}
	if err != nil {
		current, ok := s.coreManager.GetByID(auth.ID)
		if !ok || current.Disabled {
			GlobalModelRegistry().UnregisterClient(auth.ID)
		}
		return fmt.Errorf("%s auth %s: %w", op, auth.ID, err)
	}
	if updated == nil {
		current, ok := s.coreManager.GetByID(auth.ID)
		if !ok {
			GlobalModelRegistry().UnregisterClient(auth.ID)
		} else if auth.Generation > 0 {
			log.Debugf("auth %s changed during runtime update; publishing current state", auth.ID)
			return s.publishCoreAuthRuntime(ctx, current)
		}
		return nil
	}
	if hadExisting && (isAntigravityAuth(existing) || isAntigravityAuth(updated)) {
		executor.CloseAntigravityAuthIdleTransports(existing)
	}
	return s.publishCoreAuthRuntime(ctx, updated)
}

func (s *Service) publishCoreAuthRuntime(ctx context.Context, auth *coreauth.Auth) error {
	if s == nil || s.coreManager == nil || auth == nil {
		return nil
	}
	current := auth.Clone()
	for range 8 {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !s.coreManager.AuthRecreationIntentCurrent(ctx, current.ID) {
			return fmt.Errorf("auth runtime publication %s was superseded", current.ID)
		}
		latest, ok := s.coreManager.GetByID(current.ID)
		if !ok || latest == nil {
			GlobalModelRegistry().UnregisterClient(current.ID)
			return fmt.Errorf("auth runtime publication %s was removed", current.ID)
		}
		if latest.Generation != current.Generation {
			current = latest
			continue
		}
		if current.Disabled {
			s.cancelModelRefresh(current.ID)
			GlobalModelRegistry().UnregisterClient(current.ID)
			s.coreManager.RefreshSchedulerEntry(current.ID)
			return nil
		}

		scheduleExternal, tryPlugin := s.externalModelRefreshMode(current, true)
		s.cfgMu.RLock()
		cfg := s.cfg
		executorPublished := s.ensureExecutorsForAuthWithConfigContext(ctx, current, false, cfg)
		latest, ok = s.coreManager.GetByID(current.ID)
		if !ok || latest == nil {
			s.cfgMu.RUnlock()
			GlobalModelRegistry().UnregisterClient(current.ID)
			return fmt.Errorf("auth runtime publication %s was removed", current.ID)
		}
		if latest.Generation != current.Generation {
			s.cfgMu.RUnlock()
			current = latest
			continue
		}
		if !executorPublished {
			s.cfgMu.RUnlock()
			return fmt.Errorf("auth executor publication %s was superseded", current.ID)
		}
		if !scheduleExternal {
			s.registerModelsForAuthWithConfig(current, cfg)
		}
		latest, ok = s.coreManager.GetByID(current.ID)
		if !ok || latest == nil {
			s.cfgMu.RUnlock()
			GlobalModelRegistry().UnregisterClient(current.ID)
			return fmt.Errorf("auth runtime publication %s was removed", current.ID)
		}
		if latest.Generation != current.Generation {
			s.cfgMu.RUnlock()
			current = latest
			continue
		}
		if !s.coreManager.AuthRecreationIntentCurrent(ctx, current.ID) {
			s.cfgMu.RUnlock()
			return fmt.Errorf("auth runtime publication %s was superseded", current.ID)
		}
		s.cfgMu.RUnlock()
		if scheduleExternal {
			s.scheduleExternalModelRefresh(current, tryPlugin)
		}
		s.coreManager.RefreshSchedulerEntry(current.ID)
		return nil
	}
	return fmt.Errorf("auth runtime publication %s changed generation repeatedly", auth.ID)
}

func (s *Service) applyCoreAuthRemoval(ctx context.Context, id string) {
	if s == nil || s.coreManager == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	if s.coreManager.SupersedeAuthRecreation(ctx, id) {
		ctx = coreauth.WithForcePersist(ctx)
	}
	unlockApply, errApplyLock := s.lockAuthApply(ctx, id)
	if errApplyLock != nil {
		log.WithError(errApplyLock).Warnf("failed to lock auth removal %s", id)
		return
	}
	defer unlockApply()
	runtimeOnly := false
	current, hasCurrent := s.coreManager.GetByID(id)
	if hasCurrent && current != nil && current.Attributes != nil {
		runtimeOnly = strings.EqualFold(strings.TrimSpace(current.Attributes["runtime_only"]), "true")
	}
	s.cancelModelRefresh(id)
	if errRemove := s.coreManager.Remove(ctx, id); errRemove != nil {
		log.WithError(errRemove).Warnf("failed to remove auth %s durably", id)
	}
	if _, stillRegistered := s.coreManager.GetByID(id); !stillRegistered && isAntigravityAuth(current) {
		executor.CloseAntigravityAuthIdleTransports(current)
	}
	GlobalModelRegistry().UnregisterClient(id)
	if runtimeOnly {
		s.authApplyMu.Lock()
		if lock := s.authApplyLocks[id]; lock != nil {
			lock.forgetRemovedRuntime = true
		}
		s.authApplyMu.Unlock()
	}
}

func (s *Service) applyRetryConfig(cfg *config.Config) {
	if s == nil || s.coreManager == nil || cfg == nil {
		return
	}
	maxInterval := time.Duration(cfg.MaxRetryInterval) * time.Second
	s.coreManager.SetRetryConfig(cfg.RequestRetry, maxInterval, cfg.MaxRetryCredentials)
}

func (s *Service) configureCooldownStateStore(cfg *config.Config) {
	if s == nil || s.coreManager == nil {
		return
	}
	s.coreManager.SetCooldownStateStore(s.resolveCooldownStateStore(cfg))
}

func (s *Service) registerConfigAPIKeyAuths(ctx context.Context, cfg *config.Config) error {
	if s == nil || s.coreManager == nil || cfg == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = coreauth.WithAllowAuthRecreate(coreauth.WithSkipPersist(ctx))
	auths, errSynthesize := synthesizer.NewConfigSynthesizer().Synthesize(
		&synthesizer.SynthesisContext{
			Config:      cfg,
			Now:         time.Now(),
			IDGenerator: synthesizer.NewStableIDGenerator(),
		},
	)
	if errSynthesize != nil {
		return fmt.Errorf("synthesize config API key auths: %w", errSynthesize)
	}
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		var errApply error
		existing, exists := s.coreManager.GetByID(auth.ID)
		if exists {
			var updated *coreauth.Auth
			updated, errApply = s.coreManager.Update(ctx, auth)
			if errApply == nil && updated != nil && (isAntigravityAuth(existing) || isAntigravityAuth(updated)) {
				executor.CloseAntigravityAuthIdleTransports(existing)
			}
		} else {
			_, errApply = s.coreManager.Register(ctx, auth)
		}
		if errApply != nil {
			return fmt.Errorf("register config API key auth %s: %w", strings.TrimSpace(auth.ID), errApply)
		}
	}
	return nil
}

func (s *Service) resolveCooldownStateStore(cfg *config.Config) coreauth.CooldownStateStore {
	if cfg == nil || !cfg.SaveCooldownStatus || cfg.Home.Enabled {
		return nil
	}
	if s != nil && s.cooldownStateStore != nil {
		return s.cooldownStateStore
	}
	authDir, errResolve := util.ResolveAuthDir(cfg.AuthDir)
	if errResolve != nil {
		log.Warnf("failed to resolve cooldown state directory: %v", errResolve)
		return nil
	}
	if authDir == "" {
		return nil
	}
	return coreauth.NewFileCooldownStateStoreWithAuthDir(authDir, authDir)
}

var pluginHostHasAuthProvider = func(host *pluginhost.Host, provider string) bool {
	return host != nil && host.HasAuthProvider(provider)
}

func openAICompatInfoFromAuth(a *coreauth.Auth) (providerKey string, compatName string, ok bool) {
	if a == nil {
		return "", "", false
	}
	if len(a.Attributes) > 0 {
		providerKey = strings.TrimSpace(a.Attributes["provider_key"])
		compatName = strings.TrimSpace(a.Attributes["compat_name"])
		if compatName != "" {
			if providerKey == "" {
				providerKey = compatName
			}
			return strings.ToLower(providerKey), compatName, true
		}
	}
	if strings.EqualFold(strings.TrimSpace(a.Provider), "openai-compatibility") {
		return "openai-compatibility", strings.TrimSpace(a.Label), true
	}
	return "", "", false
}

func (s *Service) ensureExecutorsForAuth(a *coreauth.Auth) {
	s.ensureExecutorsForAuthContext(context.Background(), a)
}

func (s *Service) ensureExecutorsForAuthContext(ctx context.Context, a *coreauth.Auth) {
	s.ensureExecutorsForAuthWithModeContext(ctx, a, false)
}

func (s *Service) ensureExecutorsForAuthWithMode(a *coreauth.Auth, forceReplace bool) {
	s.ensureExecutorsForAuthWithModeContext(context.Background(), a, forceReplace)
}

func (s *Service) ensureExecutorsForAuthWithModeContext(
	ctx context.Context,
	a *coreauth.Auth,
	forceReplace bool,
) bool {
	if s == nil || s.coreManager == nil || a == nil {
		return false
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.ensureExecutorsForAuthWithConfigContext(ctx, a, forceReplace, s.cfg)
}

func (s *Service) ensureExecutorsForAuthWithConfigContext(
	ctx context.Context,
	a *coreauth.Auth,
	forceReplace bool,
	cfg *config.Config,
) bool {
	if s == nil || s.coreManager == nil || a == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	publish := func(candidate coreauth.ProviderExecutor) bool {
		if forceReplace || a.Generation == 0 {
			s.coreManager.RegisterExecutor(candidate)
			return true
		}
		return s.coreManager.RegisterExecutorIfAuthPublicationCurrent(ctx, a.ID, a.Generation, candidate)
	}
	if strings.EqualFold(strings.TrimSpace(a.Provider), "codex") {
		if !forceReplace {
			existingExecutor, hasExecutor := s.coreManager.Executor("codex")
			if hasExecutor {
				_, isCodexAutoExecutor := existingExecutor.(*executor.CodexAutoExecutor)
				if isCodexAutoExecutor {
					return true
				}
			}
		}
		return publish(executor.NewCodexAutoExecutor(cfg))
	}
	if strings.EqualFold(strings.TrimSpace(a.Provider), "xai") {
		if !forceReplace {
			existingExecutor, hasExecutor := s.coreManager.Executor("xai")
			if hasExecutor {
				_, isXAIAutoExecutor := existingExecutor.(*executor.XAIAutoExecutor)
				if isXAIAutoExecutor {
					return true
				}
			}
		}
		return publish(executor.NewXAIAutoExecutor(cfg))
	}
	// Skip disabled auth entries when (re)binding executors.
	// Disabled auths can linger during config reloads (e.g., removed OpenAI-compat entries)
	// and must not override active provider executors (such as iFlow OAuth accounts).
	if a.Disabled {
		return true
	}
	if compatProviderKey, _, isCompat := openAICompatInfoFromAuth(a); isCompat {
		if compatProviderKey == "" {
			compatProviderKey = strings.ToLower(strings.TrimSpace(a.Provider))
		}
		if compatProviderKey == "" {
			compatProviderKey = "openai-compatibility"
		}
		return s.publishOpenAICompatExecutor(a, compatProviderKey, cfg, forceReplace, true, publish)
	}
	switch strings.ToLower(a.Provider) {
	case "gemini":
		return publish(executor.NewGeminiExecutor(cfg))
	case "gemini-interactions":
		return publish(executor.NewGeminiInteractionsExecutor(cfg))
	case "vertex":
		return publish(executor.NewGeminiVertexExecutor(cfg))
	case "gemini-cli":
		return publish(executor.NewGeminiCLIExecutor(cfg))
	case "aistudio":
		if s.wsGateway == nil {
			return true
		}
		return publish(executor.NewAIStudioExecutor(cfg, a.ID, s.wsGateway))
	case "antigravity":
		return publish(executor.NewAntigravityExecutor(cfg))
	case "devin":
		return publish(executor.NewDevinExecutor(cfg))
	case "claude":
		return publish(executor.NewClaudeExecutor(cfg))
	case "qwen":
		return publish(executor.NewQwenExecutor(cfg))
	case "iflow":
		return publish(executor.NewIFlowExecutor(cfg))
	case "kimi":
		return publish(executor.NewKimiExecutor(cfg))
	case "kimi-ai":
		return publish(executor.NewKimiAIExecutor(cfg))
	case "meta":
		return publish(executor.NewMetaExecutor(cfg))
	case "xai":
		return publish(executor.NewXAIAutoExecutor(cfg))
	default:
		providerKey := strings.ToLower(strings.TrimSpace(a.Provider))
		if providerKey == "" {
			providerKey = "openai-compatibility"
		}
		if s.pluginHost != nil && s.pluginHost.HasExecutorCandidateProvider(providerKey) {
			if existing, ok := s.coreManager.Executor(providerKey); ok {
				if _, isCompat := existing.(*executor.OpenAICompatExecutor); isCompat {
					s.coreManager.UnregisterExecutorIfSame(providerKey, existing)
				}
				if pluginhost.IsPluginRefreshCompatExecutor(existing) {
					s.coreManager.UnregisterExecutorIfSame(providerKey, existing)
				}
			}
			return true
		}
		return s.publishOpenAICompatExecutor(a, providerKey, cfg, forceReplace, true, publish)
	}
}

func (s *Service) publishOpenAICompatExecutor(
	auth *coreauth.Auth,
	providerKey string,
	cfg *config.Config,
	forceReplace bool,
	preserveExternal bool,
	publish func(coreauth.ProviderExecutor) bool,
) bool {
	if s == nil || s.coreManager == nil || publish == nil {
		return false
	}
	providerKey = strings.ToLower(strings.TrimSpace(providerKey))
	if providerKey == "" {
		providerKey = "openai-compatibility"
	}
	compatExecutor := executor.NewOpenAICompatExecutor(providerKey, cfg)
	nextExecutor := s.wrapOpenAICompatIfPluginAuth(compatExecutor, auth)
	existing, hasExisting := s.coreManager.Executor(providerKey)
	if hasExisting && existing != nil {
		if !forceReplace && shouldKeepExistingOpenAICompatExecutor(existing, nextExecutor, preserveExternal) {
			return true
		}
		if preserveExternal && !serviceOwnsOpenAICompatExecutor(existing) {
			return true
		}
	}
	return publish(nextExecutor)
}

func (s *Service) wrapOpenAICompatIfPluginAuth(
	compatExecutor *executor.OpenAICompatExecutor,
	auth *coreauth.Auth,
) coreauth.ProviderExecutor {
	if compatExecutor == nil {
		return nil
	}
	for _, candidate := range pluginAuthProviderLookupKeys(auth, compatExecutor.Identifier()) {
		if pluginHostHasAuthProvider(s.pluginHost, candidate) {
			return pluginhost.NewPluginRefreshCompatExecutor(compatExecutor, s.pluginHost)
		}
	}
	return compatExecutor
}

func pluginAuthProviderLookupKeys(auth *coreauth.Auth, fallback string) []string {
	keys := make([]string, 0, 4)
	add := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return
		}
		if slices.Contains(keys, value) {
			return
		}
		keys = append(keys, value)
	}
	if auth != nil {
		add(auth.Provider)
		if auth.Attributes != nil {
			add(auth.Attributes["provider_key"])
			add(auth.Attributes["compat_name"])
		}
	}
	add(fallback)
	return keys
}

func shouldKeepExistingOpenAICompatExecutor(
	existing coreauth.ProviderExecutor,
	next coreauth.ProviderExecutor,
	preserveExternal bool,
) bool {
	if existing == nil || next == nil {
		return false
	}
	if pluginhost.IsPluginRefreshCompatExecutor(next) {
		if _, bareCompat := existing.(*executor.OpenAICompatExecutor); bareCompat {
			return false
		}
	}
	if pluginhost.IsPluginRefreshCompatExecutor(existing) && pluginhost.IsPluginRefreshCompatExecutor(next) {
		return true
	}
	_, existingBare := existing.(*executor.OpenAICompatExecutor)
	_, nextBare := next.(*executor.OpenAICompatExecutor)
	if existingBare && nextBare {
		return true
	}
	if !preserveExternal {
		return existingBare
	}
	return true
}

func serviceOwnsOpenAICompatExecutor(executorValue coreauth.ProviderExecutor) bool {
	if executorValue == nil {
		return false
	}
	if _, ok := executorValue.(*executor.OpenAICompatExecutor); ok {
		return true
	}
	return pluginhost.IsPluginRefreshCompatExecutor(executorValue)
}

func (s *Service) reprojectModelsForCurrentAuths() {
	if s == nil || s.coreManager == nil {
		return
	}
	processed := make(map[string]uint64)
	for range 8 {
		for _, snapshot := range s.coreManager.List() {
			if snapshot == nil || strings.TrimSpace(snapshot.ID) == "" {
				continue
			}
			if generation, ok := processed[snapshot.ID]; ok && generation == snapshot.Generation {
				continue
			}
			if generation, ok := s.reprojectModelsForAuthID(snapshot.ID, true); ok {
				processed[snapshot.ID] = generation
			}
		}
		pending := false
		for _, current := range s.coreManager.List() {
			if current == nil || strings.TrimSpace(current.ID) == "" {
				continue
			}
			if generation, ok := processed[current.ID]; !ok || generation != current.Generation {
				pending = true
				break
			}
		}
		if !pending {
			return
		}
	}
	for _, current := range s.coreManager.List() {
		if current == nil || strings.TrimSpace(current.ID) == "" {
			continue
		}
		if generation, ok := processed[current.ID]; !ok || generation != current.Generation {
			GlobalModelRegistry().UnregisterClient(current.ID)
			s.coreManager.RefreshSchedulerEntry(current.ID)
			log.WithField("auth_id", current.ID).Warn("auth changed repeatedly during batch model reprojection")
		}
	}
}

func (s *Service) onRefreshBindingChange(before, after *coreauth.Auth) {
	if before == nil || after == nil || !strings.EqualFold(after.Provider, "codex") ||
		strings.EqualFold(strings.TrimSpace(before.Attributes["plan_type"]), strings.TrimSpace(after.Attributes["plan_type"])) {
		return
	}
	s.reprojectModelsForAuthID(after.ID, false)
}

func (s *Service) reprojectModelsForAuthID(id string, allowExternal bool) (uint64, bool) {
	if s == nil || s.coreManager == nil || strings.TrimSpace(id) == "" {
		return 0, false
	}
	unlockApply, errLock := s.lockAuthApply(context.Background(), id)
	if errLock != nil {
		GlobalModelRegistry().UnregisterClient(id)
		log.WithError(errLock).Warnf("could not reproject models for auth %s", id)
		return 0, false
	}
	defer unlockApply()
	for range 8 {
		current, ok := s.coreManager.GetByID(id)
		if !ok || current == nil {
			GlobalModelRegistry().UnregisterClient(id)
			return 0, false
		}
		s.registerModelsForAuthWithMode(current, allowExternal)
		latest, latestOK := s.coreManager.GetByID(id)
		if latestOK && latest != nil && latest.Generation == current.Generation {
			s.coreManager.RefreshSchedulerEntry(id)
			return current.Generation, true
		}
	}
	GlobalModelRegistry().UnregisterClient(id)
	s.coreManager.RefreshSchedulerEntry(id)
	log.Warnf("auth %s changed repeatedly during model reprojection", id)
	return 0, false
}

// rebindExecutors refreshes provider executors so they observe the latest configuration.
func (s *Service) rebindExecutors() {
	if s == nil || s.coreManager == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	reboundCodex := false
	reboundXAI := false
	if existing, ok := s.coreManager.Executor("codex"); ok {
		if _, auto := existing.(*executor.CodexAutoExecutor); auto {
			s.coreManager.RegisterExecutor(executor.NewCodexAutoExecutor(cfg))
			reboundCodex = true
		}
	}
	if existing, ok := s.coreManager.Executor("xai"); ok {
		if _, auto := existing.(*executor.XAIAutoExecutor); auto {
			s.coreManager.RegisterExecutor(executor.NewXAIAutoExecutor(cfg))
			reboundXAI = true
		}
	}
	auths := s.coreManager.List()
	for _, auth := range auths {
		if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			if reboundCodex {
				continue
			}
			reboundCodex = true
		}
		if auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "xai") {
			if reboundXAI {
				continue
			}
			reboundXAI = true
		}
		s.ensureExecutorsForAuthWithMode(auth, true)
	}
}

func (s *Service) registerHomeExecutors() {
	if s == nil || s.coreManager == nil {
		return
	}
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil {
		return
	}
	s.coreManager.RegisterExecutor(executor.NewCodexAutoExecutor(cfg))
	s.coreManager.RegisterExecutor(executor.NewClaudeExecutor(cfg))
	s.coreManager.RegisterExecutor(executor.NewGeminiExecutor(cfg))
	s.coreManager.RegisterExecutor(executor.NewGeminiInteractionsExecutor(cfg))
	s.coreManager.RegisterExecutor(executor.NewGeminiVertexExecutor(cfg))
	s.coreManager.RegisterExecutor(executor.NewGeminiCLIExecutor(cfg))
	if s.wsGateway != nil {
		s.coreManager.RegisterExecutor(executor.NewAIStudioExecutor(cfg, "", s.wsGateway))
	}
	s.coreManager.RegisterExecutor(executor.NewAntigravityExecutor(cfg))
	s.coreManager.RegisterExecutor(executor.NewDevinExecutor(cfg))
	s.coreManager.RegisterExecutor(executor.NewKimiExecutor(cfg))
	s.coreManager.RegisterExecutor(executor.NewKimiAIExecutor(cfg))
	s.coreManager.RegisterExecutor(executor.NewMetaExecutor(cfg))
	s.coreManager.RegisterExecutor(executor.NewQwenExecutor(cfg))
	s.coreManager.RegisterExecutor(executor.NewIFlowExecutor(cfg))
	s.coreManager.RegisterExecutor(executor.NewXAIAutoExecutor(cfg))
	s.coreManager.RegisterExecutor(executor.NewOpenAICompatExecutor("openai-compatibility", cfg))
}

const homeConfigPrepareMaxAttempts = 3

var errHomeConfigChangedDuringPreparation = errors.New("home config base changed during plugin preparation")

func (s *Service) applyHomeOverlayContext(ctx context.Context, remoteCfg *config.Config) error {
	return s.applyHomeOverlayWithClient(ctx, remoteCfg, s.currentHomeClient())
}

func (s *Service) applyHomeOverlayWithClient(ctx context.Context, remoteCfg *config.Config, client *home.Client) error {
	if s == nil || remoteCfg == nil {
		return nil
	}
	if s.shuttingDown.Load() {
		return fmt.Errorf("cliproxy: service is shutting down")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for range homeConfigPrepareMaxAttempts {
		s.cfgMu.RLock()
		baseCfg := s.cfg
		s.cfgMu.RUnlock()
		if baseCfg == nil {
			return nil
		}
		managementState := func() (*config.Config, uint64, bool) {
			if s.server == nil {
				return nil, 0, false
			}
			handler := s.server.ManagementHandler()
			if handler == nil {
				return nil, 0, false
			}
			state := handler.ConfigState()
			return state.Config, state.Generation, true
		}
		managementCfg, baseGeneration, hasManagementBase := managementState()

		merged := *remoteCfg
		merged.Host = baseCfg.Host
		merged.Port = baseCfg.Port
		merged.TLS = baseCfg.TLS
		merged.Home = baseCfg.Home
		config.ApplyHomeRuntimeConfig(&merged)
		logHomeConfigChanges(baseCfg, &merged)
		prepared, syncKey, didSync, errPrepare := s.prepareHomePlugins(ctx, &merged)
		if didSync && errPrepare != nil {
			return errors.Join(
				fmt.Errorf("cliproxy: failed to prepare home plugins: %w", errPrepare),
				prepared.Close(),
			)
		}
		var (
			report  homeplugins.SyncReport
			errSync error
			errLoad error
		)
		errApply := s.applyExternalConfigUpdate(
			func() error {
				s.cfgMu.RLock()
				currentCfg := s.cfg
				s.cfgMu.RUnlock()
				if currentCfg != baseCfg {
					return errHomeConfigChangedDuringPreparation
				}
				if hasManagementBase {
					currentManagementCfg, currentGeneration, okState := managementState()
					if !okState || currentManagementCfg != managementCfg || currentGeneration != baseGeneration {
						return errHomeConfigChangedDuringPreparation
					}
				}
				if didSync {
					report, errSync = prepared.Apply(s.pluginHost)
					if errSync != nil {
						return fmt.Errorf("cliproxy: failed to sync home plugins: %w", errSync)
					}
				}
				if errApplyRuntime := s.applyConfigUpdateRuntime(ctx, &merged); errApplyRuntime != nil {
					return fmt.Errorf("cliproxy: failed to apply home config update: %w", errApplyRuntime)
				}
				if didSync {
					errLoad = homeplugins.MarkLoadResults(&report, s.pluginHost)
					if errLoad != nil {
						log.Warnf("failed to load home plugins after config update: %v", errLoad)
					}
				}
				return nil
			},
		)
		if errors.Is(errApply, errHomeConfigChangedDuringPreparation) {
			if errClose := prepared.Close(); errClose != nil {
				return fmt.Errorf("cliproxy: clean stale Home plugin preparation: %w", errClose)
			}
			continue
		}
		if errApply != nil {
			if errClose := prepared.Close(); errClose != nil {
				return errors.Join(errApply, fmt.Errorf("cliproxy: clean Home plugin preparation: %w", errClose))
			}
			return errApply
		}
		if didSync {
			if errReport := s.reportHomePluginStatusWithClient(ctx, &merged, report, client); errReport != nil {
				return fmt.Errorf("cliproxy: failed to report home plugin status: %w", errReport)
			}
			if errSync == nil && errLoad == nil {
				s.cfgMu.RLock()
				if s.cfg == &merged {
					s.markHomePluginsSynced(syncKey)
				}
				s.cfgMu.RUnlock()
			}
		}
		s.processHomePluginTasksWithClient(ctx, &merged, client)
		return nil
	}
	return fmt.Errorf(
		"cliproxy: home config changed during plugin preparation after %d attempts",
		homeConfigPrepareMaxAttempts,
	)
}

func (s *Service) applyModelCatalogConfig(cfg *config.Config) (map[string][]config.OAuthModelAlias, error) {
	if s == nil || cfg == nil || strings.TrimSpace(s.configPath) == "" {
		if cfg == nil {
			return nil, nil
		}
		return cfg.OAuthModelAlias, nil
	}
	models, err := modelcatalog.LoadOrInit(s.configPath, cfg)
	if err != nil {
		return nil, fmt.Errorf("model-catalog: failed to load models.json: %w", err)
	}
	if models.LoadMetadata.RecoveredFromPrevious {
		log.WithField("current_error", models.LoadMetadata.CurrentError).
			Warn("model-catalog: restored models.json from models.previous.json")
	}
	catalog := models.ToModelCatalogModels()
	aliases := models.ToOAuthModelAlias()
	cfg.ModelCatalog.Models = catalog
	cfg.OAuthModelAlias = aliases
	config.SetRuntimeModelCatalog(catalog)
	if s.coreManager != nil {
		s.coreManager.SetOAuthModelAlias(aliases)
	}
	return aliases, nil
}

func (s *Service) applyModelCatalogRuntimeUpdate(nextCfg *config.Config) error {
	if s == nil {
		return fmt.Errorf("service is nil")
	}
	if nextCfg == nil {
		return fmt.Errorf("model catalog config is nil")
	}
	if s.shuttingDown.Load() {
		return fmt.Errorf("service is shutting down")
	}
	s.publishConfig(nextCfg)
	if s.coreManager == nil {
		return nil
	}
	s.coreManager.SetConfig(nextCfg)
	s.coreManager.SetOAuthModelAlias(nextCfg.OAuthModelAlias)
	s.reprojectModelsForCurrentAuths()
	return s.syncPluginRuntime(context.Background())
}

func logHomeConfigChanges(oldCfg, newCfg *config.Config) {
	if oldCfg == nil || newCfg == nil || !newCfg.Home.Enabled || (!oldCfg.Debug && !newCfg.Debug) {
		return
	}

	details := diff.BuildConfigChangeDetails(oldCfg, newCfg)
	if len(details) == 0 {
		return
	}

	if newCfg.Debug && !log.IsLevelEnabled(log.DebugLevel) {
		util.SetLogLevel(newCfg)
	}

	log.Debugf("home config changes detected:")
	for _, detail := range details {
		log.Debugf("  %s", detail)
	}
}

func (s *Service) applyManagementConfigCommit(
	ctx context.Context,
	commit internalapi.ManagementConfigCommit,
) error {
	runtimeSource := commit.Config
	watcher := s.currentWatcher()
	if watcher != nil && commit.ConfigFileHash != "" {
		watcherCfg, errClone := internalconfig.Clone(commit.Config)
		if errClone != nil {
			return fmt.Errorf("cliproxy: clone management config for watcher: %w", errClone)
		}
		if watcher.AcknowledgeConfigFile(watcherCfg, commit.ConfigFileHash) {
			runtimeSource = watcherCfg
		} else {
			log.WithField("generation", commit.Generation).Warn(
				"management config file changed before watcher acknowledgement",
			)
		}
	}
	runtimeCfg, err := internalconfig.Clone(runtimeSource)
	if err != nil {
		return fmt.Errorf("cliproxy: clone management config commit: %w", err)
	}
	applied, errApply := s.applyConfigUpdateRuntimeGenerationOptions(
		ctx,
		runtimeCfg,
		commit.Generation,
		commit.DeferPluginRuntimeSync,
		commit.ModelCatalogPrepared,
	)
	if !applied {
		if errApply != nil {
			return errApply
		}
		return fmt.Errorf("cliproxy: failed to apply management config commit")
	}
	return errApply
}

func (s *Service) applyExternalConfigUpdate(apply func() error) error {
	if s == nil {
		return fmt.Errorf("cliproxy: service is unavailable")
	}
	if s.shuttingDown.Load() {
		return fmt.Errorf("cliproxy: service is shutting down")
	}
	if apply == nil {
		return fmt.Errorf("cliproxy: config update callback is nil")
	}
	if s.server != nil {
		if handler := s.server.ManagementHandler(); handler != nil {
			return handler.ApplyExternalConfigUpdate(apply)
		}
	}
	return apply()
}

func (s *Service) beginConfigPublication() bool {
	if s == nil {
		return false
	}
	s.configPublicationMu.Lock()
	if s.shuttingDown.Load() {
		s.configPublicationMu.Unlock()
		return false
	}
	if s.configPublicationCount == 0 {
		s.configPublicationIdle = make(chan struct{})
	}
	s.configPublicationCount++
	s.configPublicationMu.Unlock()

	s.configUpdateMu.Lock()
	if s.shuttingDown.Load() {
		s.configUpdateMu.Unlock()
		s.finishConfigPublication()
		return false
	}
	return true
}

func (s *Service) finishConfigPublication() {
	s.configPublicationMu.Lock()
	s.configPublicationCount--
	if s.configPublicationCount == 0 {
		close(s.configPublicationIdle)
		s.configPublicationIdle = nil
	}
	s.configPublicationMu.Unlock()
}

func (s *Service) endConfigPublication() {
	s.configUpdateMu.Unlock()
	s.finishConfigPublication()
}

func (s *Service) waitForConfigPublications(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.configPublicationMu.Lock()
	if s.configPublicationCount == 0 {
		s.configPublicationMu.Unlock()
		return nil
	}
	idle := s.configPublicationIdle
	s.configPublicationMu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for service config publications: %w", ctx.Err())
	}
}

func (s *Service) refreshModelClients() {
	if s == nil || s.server == nil {
		return
	}
	if errRefresh := s.applyExternalConfigUpdate(
		func() error {
			if !s.beginConfigPublication() {
				return fmt.Errorf("cliproxy: service is shutting down")
			}
			defer s.endConfigPublication()
			s.cfgMu.RLock()
			cfg := s.cfg
			s.cfgMu.RUnlock()
			if cfg != nil {
				s.reprojectModelsForCurrentAuths()
				s.server.RefreshModelClients(cfg)
			}
			return nil
		},
	); errRefresh != nil {
		log.Warnf("failed to refresh model clients: %v", errRefresh)
	}
}

func (s *Service) applyConfigUpdate(newCfg *config.Config) bool {
	return s.applyExternalConfigUpdate(
		func() error {
			if errApply := s.applyConfigUpdateRuntime(context.Background(), newCfg); errApply != nil {
				return fmt.Errorf("cliproxy: failed to apply external config update: %w", errApply)
			}
			return nil
		},
	) == nil
}

func (s *Service) applyWatcherConfigUpdate(newCfg *config.Config, expectedHash string) bool {
	if s == nil || s.server == nil {
		return false
	}
	handler := s.server.ManagementHandler()
	if handler == nil {
		return false
	}
	err := handler.ApplyExternalConfigCommit(
		newCfg,
		expectedHash,
		func(commit internalapi.ManagementConfigCommit) error {
			runtimeCfg, errClone := internalconfig.Clone(commit.Config)
			if errClone != nil {
				return fmt.Errorf("cliproxy: clone watcher config commit: %w", errClone)
			}
			applied, errApply := s.applyConfigUpdateRuntimeGeneration(
				context.Background(),
				runtimeCfg,
				commit.Generation,
			)
			if !applied {
				if errApply != nil {
					return errApply
				}
				return fmt.Errorf("cliproxy: failed to apply watcher config commit")
			}
			if errApply != nil {
				log.WithError(errApply).Error(
					"watcher config committed but plugin runtime sync failed",
				)
				s.schedulePluginRuntimeRetry()
			}
			return nil
		},
	)
	if err != nil {
		log.WithError(err).Debug("watcher config snapshot was not committed")
		return false
	}
	return true
}

func (s *Service) applyConfigUpdateRuntime(ctx context.Context, newCfg *config.Config) error {
	applied, errApply := s.applyConfigUpdateRuntimeGeneration(ctx, newCfg, 0)
	if !applied && errApply == nil {
		return fmt.Errorf("cliproxy: config update was not applied")
	}
	return errApply
}

func (s *Service) applyConfigUpdateRuntimeGeneration(
	ctx context.Context,
	newCfg *config.Config,
	managementGeneration uint64,
) (bool, error) {
	return s.applyConfigUpdateRuntimeGenerationOptions(ctx, newCfg, managementGeneration, false, false)
}

func sessionAffinitySubagentsEnabled(value *bool) bool {
	return value == nil || *value
}

func (s *Service) applyConfigUpdateRuntimeGenerationOptions(
	ctx context.Context,
	newCfg *config.Config,
	managementGeneration uint64,
	deferPluginRuntimeSync bool,
	modelCatalogPrepared bool,
) (bool, error) {
	if s == nil {
		return false, fmt.Errorf("cliproxy: service is unavailable")
	}
	if !s.beginConfigPublication() {
		return false, fmt.Errorf("cliproxy: service is shutting down")
	}
	if managementGeneration > 0 && managementGeneration <= s.managementConfigGeneration {
		s.endConfigPublication()
		return true, nil
	}

	previousStrategy := ""
	previousSessionAffinity := false
	previousSessionAffinityTTL := ""
	previousSessionAffinitySubagents := true
	homeEnabled := false
	s.cfgMu.RLock()
	if s.cfg != nil {
		previousStrategy = strings.ToLower(strings.TrimSpace(s.cfg.Routing.Strategy))
		previousSessionAffinity = s.cfg.Routing.SessionAffinity
		previousSessionAffinityTTL = strings.TrimSpace(s.cfg.Routing.SessionAffinityTTL)
		previousSessionAffinitySubagents = sessionAffinitySubagentsEnabled(s.cfg.Routing.SessionAffinitySubagents)
		homeEnabled = s.cfg.Home.Enabled
	}
	s.cfgMu.RUnlock()

	if newCfg == nil {
		s.cfgMu.RLock()
		newCfg = s.cfg
		s.cfgMu.RUnlock()
	}
	if newCfg == nil {
		s.endConfigPublication()
		return false, fmt.Errorf("cliproxy: config update is nil")
	}
	if errValidate := newCfg.ValidateCredentialWeights(); errValidate != nil {
		s.endConfigPublication()
		return false, fmt.Errorf("cliproxy: invalid credential weight: %w", errValidate)
	}
	if homeEnabled {
		config.ApplyHomeRuntimeConfig(newCfg)
	}
	var modelAliases map[string][]config.OAuthModelAlias
	if modelCatalogPrepared {
		modelAliases = newCfg.OAuthModelAlias
		config.SetRuntimeModelCatalog(newCfg.ModelCatalog.Models)
	} else {
		var err error
		modelAliases, err = s.applyModelCatalogConfig(newCfg)
		if err != nil {
			s.endConfigPublication()
			return false, err
		}
	}

	nextStrategy := strings.ToLower(strings.TrimSpace(newCfg.Routing.Strategy))
	normalizeStrategy := func(strategy string) string {
		switch strategy {
		case "fill-first", "fillfirst", "ff":
			return "fill-first"
		case "weighted-round-robin", "weightedroundrobin", "wrr":
			return "weighted-round-robin"
		case "quota-aware", "quotaaware", "qa":
			return "quota-aware"
		default:
			return "round-robin"
		}
	}
	previousStrategy = normalizeStrategy(previousStrategy)
	nextStrategy = normalizeStrategy(nextStrategy)
	nextSessionAffinity := newCfg.Routing.SessionAffinity
	nextSessionAffinityTTL := strings.TrimSpace(newCfg.Routing.SessionAffinityTTL)
	nextSessionAffinitySubagents := newCfg.Routing.SessionAffinitySubagents
	subagentAffinityChanged := nextSessionAffinity && previousSessionAffinitySubagents !=
		sessionAffinitySubagentsEnabled(nextSessionAffinitySubagents)
	selectorChanged := previousStrategy != nextStrategy ||
		previousSessionAffinity != nextSessionAffinity ||
		previousSessionAffinityTTL != nextSessionAffinityTTL ||
		subagentAffinityChanged
	if s.coreManager != nil && selectorChanged {
		selector := newRoutingSelector(nextStrategy)
		if nextSessionAffinity {
			ttl := time.Hour
			if nextSessionAffinityTTL != "" {
				if parsed, err := time.ParseDuration(nextSessionAffinityTTL); err == nil && parsed > 0 {
					ttl = parsed
				}
			}
			selector = coreauth.NewSessionAffinitySelectorWithConfig(
				coreauth.SessionAffinityConfig{
					Fallback:         selector,
					TTL:              ttl,
					SubagentAffinity: nextSessionAffinitySubagents,
				},
			)
		}
		s.coreManager.SetSelector(selector)
	}

	s.applyRetryConfig(newCfg)
	if s.coreManager != nil {
		store := s.resolveCooldownStateStore(newCfg)
		if !s.coreManager.ApplyConfigWithCooldownStateStore(ctx, newCfg, store) {
			s.endConfigPublication()
			return false, fmt.Errorf("cliproxy: cooldown state store transition failed")
		}
		if errRegister := s.registerConfigAPIKeyAuths(ctx, newCfg); errRegister != nil {
			s.endConfigPublication()
			return false, errRegister
		}
		s.coreManager.SetOAuthModelAlias(modelAliases)
	}
	s.applyPprofConfig(newCfg)
	s.publishConfig(newCfg)
	if homeEnabled {
		s.registerHomeExecutors()
	}
	s.rebindExecutors()
	s.reprojectModelsForCurrentAuths()
	if s.coreManager != nil && !newCfg.Home.Enabled && newCfg.SaveCooldownStatus {
		if errRestoreCooldown := s.coreManager.RestoreCooldownStates(ctx); errRestoreCooldown != nil && ctx.Err() == nil {
			log.Warnf("failed to restore cooldown state after config update: %v", errRestoreCooldown)
		}
	}
	if s.server != nil {
		if managementGeneration > 0 {
			s.server.UpdateClientsForConfigCommit(ctx, newCfg, managementGeneration)
		} else {
			s.server.UpdateClients(newCfg)
		}
	}
	if managementGeneration > s.managementConfigGeneration {
		s.managementConfigGeneration = managementGeneration
	}
	s.endConfigPublication()
	if deferPluginRuntimeSync {
		s.schedulePluginRuntimeRetry()
		return true, nil
	}
	if errSync := s.syncPluginRuntime(ctx); errSync != nil {
		return true, errSync
	}
	s.stopPendingPluginRuntimeRetry()
	return true, nil
}

func (s *Service) currentHomeClient() *home.Client {
	if s == nil {
		return nil
	}
	s.homeClientMu.RLock()
	client := s.homeClient
	s.homeClientMu.RUnlock()
	return client
}

func (s *Service) beginHomeAppLogScope() {
	if s == nil {
		return
	}
	s.homeLogScopeMu.Lock()
	if s.homeLogScopeRelease == nil {
		s.homeLogScopeRelease = logging.BeginHomeAppLogScope()
	}
	s.homeLogScopeMu.Unlock()
}

func (s *Service) endHomeAppLogScope() {
	if s == nil {
		return
	}
	s.homeLogScopeMu.Lock()
	release := s.homeLogScopeRelease
	s.homeLogScopeRelease = nil
	s.homeLogScopeMu.Unlock()
	if release != nil {
		release()
	}
}

func (s *Service) usagePersistenceDataDir() string {
	if s == nil || s.cfg == nil {
		return ""
	}
	baseDir := internalUsage.ResolveDataDir(s.cfg.UsageDataDir, s.configPath)
	if baseDir == "" {
		return ""
	}
	migrate := s.usageDataDirMigrator
	if migrate == nil {
		migrate = internalUsage.MigrateDataDirIfNeeded
	}
	if errMigrate := migrate(s.configPath, baseDir); errMigrate != nil {
		log.Warnf(
			"usage: data dir migration failed; leaving the target unopened for a later retry: %v",
			errMigrate,
		)
		return ""
	}
	return baseDir
}

// Run starts the service and blocks until the context is canceled or the server stops.
// It initializes all components including authentication, file watching, HTTP server,
// and starts processing requests. The method blocks until the context is canceled.
//
// Parameters:
//   - ctx: The context for controlling the service lifecycle
//
// Returns:
//   - error: An error if the service fails to start or run
func (s *Service) Run(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("cliproxy: service is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if s.cfg != nil && s.cfg.Home.Enabled {
		s.beginHomeAppLogScope()
	}

	if s.usageManager == nil {
		s.usageManager = usage.NewManager(512)
		if s.pluginHost != nil {
			s.pluginHost.SetUsageManager(s.usageManager)
		}
	}
	s.usageManager.Start(context.Background())
	defer func() {
		shutdownCtx, shutdownCancel := s.newShutdownContext()
		defer shutdownCancel()
		if err := s.Shutdown(shutdownCtx); err != nil {
			log.Errorf("service shutdown returned error: %v", err)
		}
	}()

	modelAliases, err := s.applyModelCatalogConfig(s.cfg)
	if err != nil {
		return err
	}

	homeEnabled := s.cfg != nil && s.cfg.Home.Enabled
	if s.usageQueue == nil {
		s.usageQueue = redisqueue.NewQueue()
	}
	s.usageStatisticsEnabled.Store(s.cfg.UsageStatisticsEnabled)
	s.usageQueue.SetUsageStatisticsEnabled(s.cfg.UsageStatisticsEnabled)
	s.usageQueue.SetRetentionSeconds(s.cfg.RedisUsageQueueRetentionSeconds)
	if homeEnabled {
		config.ApplyHomeRuntimeConfig(s.cfg)
		s.usageQueue.SetEnabled(true)
		if s.coreManager != nil {
			s.coreManager.SetConfig(s.cfg)
			s.coreManager.SetOAuthModelAlias(modelAliases)
		}
	}

	// Start usage statistics persistence if enabled
	if s.cfg.UsageStatisticsEnabled && !strings.EqualFold(s.cfg.UsageStatisticsFile, "disabled") {
		baseDir := s.usagePersistenceDataDir()
		if baseDir != "" {
			s.usagePersister = internalUsage.NewPersister(baseDir, s.cfg.UsageRetention)
			s.usagePersister.SetPriceProvider(internalUsage.BuildModelPriceProvider(s.configPath))
			// Persister.Start opens SQLite, runs the legacy-JSON migration in
			// place, then launches the writer + rollup goroutines.
			s.usagePersister.Start(context.Background())
			// SQLite open or migration may fail because of disk, permission, or
			// database errors. Keep the service persister nil in that case so its
			// logger plugin writes to the service-owned in-memory statistics.
			if !s.usagePersister.IsReady() {
				log.Warnf("usage: SQLite persister not ready, falling back to in-memory request statistics; legacy summary.json / detail JSON files remain untouched for manual recovery")
				s.usagePersister = nil
			}
		}
	}

	if s.usageStats == nil {
		s.usageStats = internalUsage.NewRequestStatistics()
	}
	if retirement := s.usageManager.RegisterNamedWithRetirement(
		"cliproxy:logger",
		internalUsage.NewLoggerPlugin(s.usagePersister, s.usageStats, s.usageStatisticsEnabled.Load),
	); retirement != nil {
		retirement.Wait()
	}
	if retirement := s.usageManager.RegisterNamedWithRetirement(
		"cliproxy:redis-queue", redisqueue.NewUsagePluginForQueue(s.usageQueue),
	); retirement != nil {
		retirement.Wait()
	}

	if !homeEnabled {
		if err := s.ensureAuthDir(); err != nil {
			return err
		}
	}

	s.applyRetryConfig(s.cfg)
	s.configureCooldownStateStore(s.cfg)

	if !homeEnabled {
		if s.coreManager != nil {
			loadCtx := sdkAuth.WithPluginAuthParser(ctx, s.pluginHost)
			if errLoad := s.coreManager.Load(loadCtx); errLoad != nil {
				log.Warnf("failed to load auth store: %v", errLoad)
			}
			if errRegister := s.registerConfigAPIKeyAuths(ctx, s.cfg); errRegister != nil {
				return errRegister
			}
			if s.cfg.SaveCooldownStatus {
				if errRestoreCooldown := s.coreManager.RestoreCooldownStates(ctx); errRestoreCooldown != nil {
					log.Warnf("failed to restore cooldown state: %v", errRestoreCooldown)
				}
			}
		}

		tokenResult, err := s.tokenProvider.Load(ctx, s.cfg)
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		if tokenResult == nil {
			tokenResult = &TokenClientResult{}
		}

		apiKeyResult, err := s.apiKeyProvider.Load(ctx, s.cfg)
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		if apiKeyResult == nil {
			apiKeyResult = &APIKeyClientResult{}
		}
	}

	// legacy clients removed; no caches to refresh

	// handlers no longer depend on legacy clients; pass nil slice initially
	serverOptions := []internalapi.ServerOption{
		internalapi.WithPluginHost(s.pluginHost),
		internalapi.WithUsageManager(s.usageManager),
		internalapi.WithUsageQueue(s.usageQueue),
		internalapi.WithUsageStatisticsToggle(s.usageStatisticsEnabled.Store),
		internalapi.WithHomeClientProvider(s.currentHomeClient),
		internalapi.WithManagementConfigCommitted(
			func(commitCtx context.Context, commit internalapi.ManagementConfigCommit) error {
				return s.applyManagementConfigCommit(commitCtx, commit)
			},
		),
		internalapi.WithModelCatalogRuntimeSync(s.applyModelCatalogRuntimeUpdate),
	}
	for _, option := range s.serverOptions {
		if option != nil {
			serverOptions = append(serverOptions, option)
		}
	}
	s.server = internalapi.NewServer(s.cfg, s.coreManager, s.accessManager, s.configPath, serverOptions...)
	s.server.SetUsageStatistics(s.usageStats)

	if s.usagePersister != nil {
		s.server.SetUsagePersister(s.usagePersister)
	}
	if s.server != nil && s.server.ManagementHandler() != nil {
		s.server.ManagementHandler().StartCPAAutoUpdater(ctx)
	}

	if s.authManager == nil {
		s.authManager = newDefaultAuthManager()
	}

	s.ensureWebsocketGateway()
	if s.server != nil && s.wsGateway != nil {
		s.server.AttachWebsocketRoute(s.wsGateway.Path(), s.wsGateway.Handler())
		s.server.SetWebsocketAuthChangeHandler(
			func(oldEnabled, newEnabled bool) {
				if oldEnabled == newEnabled {
					return
				}
				if !oldEnabled && newEnabled {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if errStop := s.wsGateway.Stop(ctx); errStop != nil {
						log.Warnf(
							"failed to reset websocket connections after ws-auth change %t -> %t: %v", oldEnabled,
							newEnabled, errStop,
						)
						return
					}
					log.Debugf("ws-auth enabled; existing websocket sessions terminated to enforce authentication")
					return
				}
				log.Debugf("ws-auth disabled; existing websocket sessions remain connected")
			},
		)
	}
	if homeEnabled {
		s.registerHomeExecutors()
		s.startHomeSubscriber(ctx)
	}
	if errPluginRuntime := s.syncPluginRuntime(ctx); errPluginRuntime != nil {
		return errPluginRuntime
	}
	if !homeEnabled && s.coreManager != nil {
		checkpointStore := coreauth.NewFileSchedulerCheckpointStore(s.cfg.AuthDir)
		if errCheckpoint := s.coreManager.StartSchedulerCheckpoint(ctx, checkpointStore); errCheckpoint != nil {
			log.WithError(errCheckpoint).Warn("failed to restore scheduler checkpoint")
		}
	}

	if s.hooks.OnBeforeStart != nil {
		s.hooks.OnBeforeStart(s.cfg)
	}

	s.serverErr = make(chan error, 1)
	serverReady := make(chan struct{}, 1)
	go func() {
		if errStart := s.server.StartWithReady(serverReady); errStart != nil {
			s.serverErr <- errStart
		} else {
			s.serverErr <- nil
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err = <-s.serverErr:
		return err
	case <-serverReady:
	}
	fmt.Printf("API server started successfully on: %s:%d\n", s.cfg.Host, s.cfg.Port)

	s.applyPprofConfig(s.cfg)

	if s.hooks.OnAfterStart != nil {
		s.hooks.OnAfterStart(s)
	}

	// Register model refresh callback to re-register client models when the
	// remote catalog is updated (e.g., new models like gpt-5.4 added upstream).
	registry.SetModelRefreshCallback(
		func(_ []string) {
			s.refreshModelClients()
		},
	)

	if !homeEnabled {
		var watcherWrapper *WatcherWrapper
		reloadCallback := func(newCfg *config.Config) bool {
			return s.applyConfigUpdate(newCfg)
		}

		watcherWrapper, err = s.watcherFactory(s.configPath, s.cfg.AuthDir, reloadCallback)
		if err != nil {
			return fmt.Errorf("cliproxy: failed to create watcher: %w", err)
		}
		s.setWatcher(watcherWrapper)
		watcherWrapper.SetConfigReloadCallback(s.applyWatcherConfigUpdate)
		watcherWrapper.SetPluginAuthParser(s.pluginHost)
		watcherWrapper.SetUsageQueue(s.usageQueue)
		s.ensureAuthUpdateQueue(ctx)
		if s.authUpdates != nil {
			watcherWrapper.SetAuthUpdateQueue(s.authUpdates)
		}
		watcherWrapper.SetConfig(s.cfg)

		watcherCtx, watcherCancel := context.WithCancel(context.Background())
		s.watcherCancel = watcherCancel
		if err = watcherWrapper.Start(watcherCtx); err != nil {
			return fmt.Errorf("cliproxy: failed to start watcher: %w", err)
		}
		log.Info("file watcher started for config and auth directory changes")
	} else {
		s.ensureAuthUpdateQueue(ctx)
	}

	if s.coreManager != nil {
		interval := coreauth.MaxAutoRefreshCheckInterval
		s.coreManager.StartAutoRefresh(ctx, interval)
		log.Infof("core auth auto-refresh started (interval=%s)", interval)
	}

	select {
	case <-ctx.Done():
		log.Debug("service context cancelled, shutting down...")
		return ctx.Err()
	case err = <-s.serverErr:
		return err
	}
}

func (s *Service) newShutdownContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), serviceShutdownTimeout)
}

// Shutdown gracefully stops background workers and the HTTP server.
// It ensures all resources are properly cleaned up and connections are closed.
// The shutdown is idempotent and can be called multiple times safely.
//
// Parameters:
//   - ctx: The context for controlling the shutdown timeout
//
// Returns:
//   - error: An error if shutdown fails
func (s *Service) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.shutdownOnce.Do(
		func() {
			s.shutdownMu.Lock()
			s.shutdownDone = make(chan struct{})
			done := s.shutdownDone
			s.shutdownMu.Unlock()
			go func() {
				shutdownCtx, cancel := s.newShutdownContext()
				defer cancel()
				err := s.shutdown(shutdownCtx)
				s.shutdownMu.Lock()
				s.shutdownErr = err
				close(done)
				s.shutdownMu.Unlock()
			}()
		},
	)
	s.shutdownMu.Lock()
	done := s.shutdownDone
	s.shutdownMu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		s.shutdownMu.Lock()
		defer s.shutdownMu.Unlock()
		return s.shutdownErr
	}
}

func (s *Service) shutdown(ctx context.Context) error {
	defer s.endHomeAppLogScope()
	var shutdownErrors []error
	s.configPublicationMu.Lock()
	s.shuttingDown.Store(true)
	s.configPublicationMu.Unlock()
	if s.server != nil {
		s.server.BeginShutdown()
	}
	if s.usageQueue != nil {
		s.usageQueue.Close()
	}
	if s.server != nil {
		if errWaitServerConfig := s.server.WaitForConfigPublications(ctx); errWaitServerConfig != nil {
			shutdownErrors = append(shutdownErrors, errWaitServerConfig)
		}
	}
	if errWaitConfig := s.waitForConfigPublications(ctx); errWaitConfig != nil {
		shutdownErrors = append(shutdownErrors, errWaitConfig)
	}
	if s.pluginHost != nil {
		s.pluginHost.InvalidateRuntime()
	}
	if errStopRetry := s.stopPluginRuntimeRetry(ctx); errStopRetry != nil {
		log.Errorf("failed to stop plugin runtime retry: %v", errStopRetry)
		shutdownErrors = append(shutdownErrors, errStopRetry)
	}
	if errStopHome := s.stopHomeSubscriber(ctx); errStopHome != nil {
		log.Errorf("failed to stop Home subscriber: %v", errStopHome)
		shutdownErrors = append(shutdownErrors, errStopHome)
	}
	s.homeClientMu.Lock()
	homeLogForwarder := s.homeLogForwarder
	s.homeLogForwarder = nil
	s.homeLogForwarderFor = nil
	s.homeClientMu.Unlock()
	if homeLogForwarder != nil {
		homeLogForwarder.Stop()
	}
	if s.watcherCancel != nil {
		s.watcherCancel()
	}
	if s.coreManager != nil {
		if errStopRefresh := s.coreManager.StopAutoRefresh(ctx); errStopRefresh != nil {
			log.Errorf("failed to stop auth refresh workers: %v", errStopRefresh)
			shutdownErrors = append(shutdownErrors, errStopRefresh)
		}
	}
	if watcher := s.currentWatcher(); watcher != nil {
		if err := watcher.Stop(); err != nil {
			log.Errorf("failed to stop file watcher: %v", err)
			shutdownErrors = append(shutdownErrors, err)
		}
	}
	if s.wsGateway != nil {
		if err := s.wsGateway.Stop(ctx); err != nil {
			log.Errorf("failed to stop websocket gateway: %v", err)
			shutdownErrors = append(shutdownErrors, err)
		}
	}
	if errStopAuthQueue := s.stopAuthUpdateQueue(ctx); errStopAuthQueue != nil {
		log.Errorf("failed to stop auth update queue: %v", errStopAuthQueue)
		shutdownErrors = append(shutdownErrors, errStopAuthQueue)
	}
	if s.coreManager != nil {
		if errStopDeferred := s.coreManager.StopDeferredAuthRecreates(ctx); errStopDeferred != nil {
			log.Errorf("failed to stop deferred auth recreations: %v", errStopDeferred)
			shutdownErrors = append(shutdownErrors, errStopDeferred)
		}
	}
	if errStopWorkers := s.stopModelRefreshWorkers(ctx); errStopWorkers != nil {
		log.Errorf("failed to stop model refresh workers: %v", errStopWorkers)
		shutdownErrors = append(shutdownErrors, errStopWorkers)
	}
	if errShutdownPprof := s.shutdownPprof(ctx); errShutdownPprof != nil {
		log.Errorf("failed to stop pprof server: %v", errShutdownPprof)
		shutdownErrors = append(shutdownErrors, errShutdownPprof)
	}
	if s.server != nil {
		if mgmt := s.server.ManagementHandler(); mgmt != nil {
			mgmt.StopCPAAutoUpdater()
		}
		shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if err := s.server.Stop(shutdownCtx); err != nil {
			log.Errorf("error stopping API server: %v", err)
			shutdownErrors = append(shutdownErrors, err)
		}
		cancel()
	}
	if s.coreManager != nil {
		if errCheckpoint := s.coreManager.StopSchedulerCheckpoint(ctx); errCheckpoint != nil {
			log.Errorf("failed to stop scheduler checkpoint: %v", errCheckpoint)
			shutdownErrors = append(shutdownErrors, errCheckpoint)
		}
	}
	if s.pluginHost != nil {
		modelRegistry := s.pluginModelRegistry()
		s.pluginHost.RegisterModels(ctx, modelRegistry)
		s.pluginHost.RegisterExecutors(s.coreManager, modelRegistry)
		s.pluginHost.RegisterFrontendAuthProviders()
		if s.accessManager != nil {
			s.accessManager.SetProviders(sdkaccess.RegisteredProviders())
		}
	}
	if s.usageManager != nil {
		if errStopUsage := s.usageManager.StopContext(ctx); errStopUsage != nil {
			log.Errorf("failed to stop usage dispatcher: %v", errStopUsage)
			shutdownErrors = append(shutdownErrors, errStopUsage)
		}
	}
	if s.pluginHost != nil {
		if errShutdownPlugins := s.pluginHost.ShutdownAllContext(ctx); errShutdownPlugins != nil {
			log.Errorf("failed to stop plugin host: %v", errShutdownPlugins)
			shutdownErrors = append(shutdownErrors, errShutdownPlugins)
		}
	}
	if s.usagePersister != nil {
		if errStopPersister := s.usagePersister.StopContext(ctx); errStopPersister != nil {
			log.Errorf("failed to stop usage persister: %v", errStopPersister)
			shutdownErrors = append(shutdownErrors, errStopPersister)
		}
	}
	return errors.Join(shutdownErrors...)
}

func (s *Service) ensureAuthDir() error {
	if s == nil || s.cfg == nil {
		return fmt.Errorf("cliproxy: auth directory configuration is unavailable")
	}
	authDir := strings.TrimSpace(s.cfg.AuthDir)
	if authDir == "" {
		return fmt.Errorf("cliproxy: auth directory is empty")
	}
	_, errBefore := os.Stat(authDir)
	created := os.IsNotExist(errBefore)
	root, errRoot := util.OpenOrCreateDirectoryRoot(authDir, 0o700)
	if errRoot != nil {
		return fmt.Errorf("cliproxy: secure auth directory %s: %w", authDir, errRoot)
	}
	if errClose := root.Close(); errClose != nil {
		return fmt.Errorf("cliproxy: close auth directory root %s: %w", authDir, errClose)
	}
	if created {
		log.Infof("created missing auth directory: %s", authDir)
	}
	if errWalk := filepath.WalkDir(
		authDir,
		func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("auth token path is a symlink: %s", path)
			}
			info, errInfo := entry.Info()
			if errInfo != nil {
				return errInfo
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("auth token path is not a regular file: %s", path)
			}
			if errSecure := internalauth.SecureTokenFile(path); errSecure != nil {
				return fmt.Errorf("secure auth token file %s: %w", path, errSecure)
			}
			return nil
		},
	); errWalk != nil {
		return fmt.Errorf("cliproxy: inspect auth directory %s: %w", authDir, errWalk)
	}
	return nil
}

func (s *Service) pluginModelsForProvider(providerKey string) []*ModelInfo {
	if s == nil || s.pluginHost == nil {
		return nil
	}
	return fromRegistryModelInfos(s.pluginHost.ModelsForProvider(providerKey))
}

func (s *Service) appendPluginModels(providerKey string, models []*ModelInfo) []*ModelInfo {
	pluginModels := s.pluginModelsForProvider(providerKey)
	if len(pluginModels) == 0 {
		return models
	}
	out := make([]*ModelInfo, 0, len(models)+len(pluginModels))
	seen := make(map[string]struct{}, len(models)+len(pluginModels))
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID != "" {
			seen[modelID] = struct{}{}
		}
		out = append(out, model)
	}
	for _, model := range pluginModels {
		if model == nil {
			continue
		}
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			continue
		}
		if _, exists := seen[modelID]; exists {
			continue
		}
		seen[modelID] = struct{}{}
		out = append(out, model)
	}
	return out
}

func (s *Service) catalogEnabledModels(models []*ModelInfo) []*ModelInfo {
	if s == nil || s.cfg == nil || len(models) == 0 {
		return models
	}
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if s.modelInfoCatalogEnabled(model) {
			out = append(out, model)
		}
	}
	return out
}

func (s *Service) catalogEnabledRegistryModels(models []*registry.ModelInfo) []*registry.ModelInfo {
	if s == nil || s.cfg == nil || len(models) == 0 {
		return models
	}
	out := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		if s.modelInfoCatalogEnabled(fromRegistryModelInfo(model)) {
			out = append(out, model)
		}
	}
	return out
}

func (s *Service) modelInfoCatalogEnabled(model *ModelInfo) bool {
	if s == nil || s.cfg == nil || model == nil {
		return false
	}
	ids := []string{model.ID, model.CatalogName, model.Name}
	checked := false
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		checked = true
		if !s.cfg.ModelCatalogModelEnabled(id) {
			return false
		}
	}
	return checked
}

func cloneModelInfos(models []*ModelInfo) []*ModelInfo {
	return fromRegistryModelInfos(toRegistryModelInfos(models))
}

func modelRefreshMappingRevision(cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("model refresh config is nil")
	}
	payload := []any{
		cfg.ForceModelPrefix,
		cfg.OAuthModelAlias,
		cfg.OAuthSettings,
		cfg.OAuthExcludedModels,
		cfg.ModelCatalog.Models,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode model refresh mapping: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func modelRefreshEndpointIdentity(auth *coreauth.Auth, cfg *config.Config, provider string) string {
	values := make([]string, 0, 8)
	for _, key := range []string{"base_urls", "base_url", "endpoint", "api_base", "api_endpoint", "host"} {
		value := ""
		if auth != nil && auth.Attributes != nil {
			value = strings.TrimSpace(auth.Attributes[key])
		}
		if value == "" && auth != nil && auth.Metadata != nil {
			value, _ = auth.Metadata[key].(string)
			value = strings.TrimSpace(value)
		}
		if value != "" {
			values = append(values, key+"="+strings.TrimSuffix(value, "/"))
		}
	}
	if len(values) == 0 && strings.EqualFold(provider, "antigravity") {
		values = append(
			values,
			"base_url=https://daily-cloudcode-pa.googleapis.com",
		)
	}
	if auth != nil && strings.EqualFold(provider, "antigravity") {
		project, _ := auth.Metadata["project_id"].(string)
		values = append(values, "project_id="+strings.TrimSpace(project))
	}
	if auth != nil && strings.TrimSpace(auth.ProxyURL) != "" {
		values = append(values, "auth_proxy="+strings.TrimSpace(auth.ProxyURL))
	}
	if cfg != nil && strings.TrimSpace(cfg.ProxyURL) != "" {
		values = append(values, "config_proxy="+strings.TrimSpace(cfg.ProxyURL))
	}
	return strings.Join(values, "\x00")
}

func (s *Service) buildModelRefreshIdentity(
	auth *coreauth.Auth,
	cfg *config.Config,
	tryPlugin bool,
) (modelRefreshIdentity, error) {
	if auth == nil {
		return modelRefreshIdentity{}, fmt.Errorf("model refresh auth is nil")
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if _, _, compatDetected := openAICompatInfoFromAuth(auth); compatDetected {
		provider = "openai-compatibility"
	}
	if provider == "" {
		return modelRefreshIdentity{}, fmt.Errorf("model refresh provider is empty")
	}
	mappingRevision, err := modelRefreshMappingRevision(cfg)
	if err != nil {
		return modelRefreshIdentity{}, err
	}
	authIndex := strings.TrimSpace(auth.Index)
	if authIndex == "" {
		authIndex = auth.Clone().EnsureIndex()
	}
	var pluginSnapshot *pluginhost.Snapshot
	if tryPlugin && s != nil && s.pluginHost != nil {
		pluginSnapshot = s.pluginHost.Snapshot()
	}
	identity := modelRefreshIdentity{
		provider:        provider,
		authID:          strings.TrimSpace(auth.ID),
		authIndex:       authIndex,
		authGeneration:  auth.Generation,
		endpoint:        modelRefreshEndpointIdentity(auth, cfg, provider),
		mappingRevision: mappingRevision,
		pluginSnapshot:  pluginSnapshot,
		tryPlugin:       tryPlugin,
	}
	keyMaterial := strings.Join(
		[]string{
			identity.provider,
			identity.authID,
			identity.authIndex,
			strconv.FormatUint(identity.authGeneration, 10),
			identity.endpoint,
			identity.mappingRevision,
			fmt.Sprintf("%p", identity.pluginSnapshot),
			strconv.FormatBool(identity.tryPlugin),
		},
		"\x00",
	)
	digest := sha256.Sum256([]byte(keyMaterial))
	identity.key = hex.EncodeToString(digest[:])
	return identity, nil
}

func (s *Service) applyModelDiscoveryAuthUpdate(
	ctx context.Context,
	job modelRefreshJob,
	a *coreauth.Auth,
	result modelDiscoveryResult,
) (*coreauth.Auth, bool) {
	if result.auth == nil || s.coreManager == nil {
		return a, true
	}
	providerState := result.auth.Clone()
	persistentUpdate := a.Clone()
	persistentUpdate.Storage = providerState.Storage
	persistentUpdate.Metadata = providerState.Metadata
	persistentUpdate.NextRefreshAfter = providerState.NextRefreshAfter
	updated, errUpdate := s.coreManager.MergePersistentUpdateConditional(
		ctx,
		a,
		persistentUpdate,
		func(publish func() bool) bool {
			s.modelRefreshMu.Lock()
			defer s.modelRefreshMu.Unlock()
			if !s.modelRefreshResultCurrentLocked(job, result, a) {
				return false
			}
			return publish()
		},
	)
	if errUpdate != nil {
		log.WithError(errUpdate).WithField("auth_id", a.ID).Warn(
			"model discovery auth update failed",
		)
		return nil, false
	}
	if updated == nil || ctx.Err() != nil {
		return nil, false
	}
	return updated.Clone(), true
}

func (s *Service) registerPluginDiscoveredModels(
	a *coreauth.Auth,
	cfg *config.Config,
	providerKey string,
	models []*ModelInfo,
	expectedEpoch uint64,
) bool {
	if s == nil || a == nil || cfg == nil {
		return false
	}
	if activeProvider := strings.ToLower(strings.TrimSpace(a.Provider)); activeProvider != "" {
		providerKey = activeProvider
	}
	authKind := a.AuthKind()
	if authKind == "" {
		if kind, _ := a.AccountInfo(); strings.EqualFold(kind, "api_key") {
			authKind = coreauth.AuthKindAPIKey
		}
	}
	excluded := oauthExcludedModelsForConfig(cfg, providerKey, authKind)
	if a.Attributes != nil {
		if value := strings.TrimSpace(a.Attributes["excluded_models"]); value != "" {
			excluded = strings.Split(value, ",")
		}
	}
	models = applyExcludedModels(models, excluded)
	models = applyOAuthModelAlias(cfg, providerKey, authKind, a.Attributes, models)
	models = applyExcludedModels(models, excluded)
	models = catalogEnabledModelsForConfig(cfg, models)
	models = applyOAuthSettings(cfg, providerKey, authKind, models)
	models = applyModelPrefixes(models, a.Prefix, cfg.ForceModelPrefix)
	_, applied := GlobalModelRegistry().ReplaceClientModels(a.ID, providerKey, expectedEpoch, models)
	return applied
}

func (s *Service) cancelModelRefresh(id string) {
	if s == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	s.modelRefreshMu.Lock()
	defer s.modelRefreshMu.Unlock()
	if cancel := s.modelRefreshCancel[id]; cancel != nil {
		cancel()
	}
	if pending, ok := s.modelRefreshPending[id]; ok {
		s.modelRefreshGroup.Forget(pending.identity.key)
	}
	if active, ok := s.modelRefreshActive[id]; ok {
		s.modelRefreshGroup.Forget(active.identity.key)
	}
	delete(s.modelRefreshCancel, id)
	delete(s.modelRefreshPending, id)
	delete(s.modelRefreshActive, id)
	delete(s.modelRefreshCache, id)
	delete(s.modelRefreshFailures, id)
	delete(s.modelRefreshVersion, id)
}

func (s *Service) cancelAllModelRefreshesLocked(clearCache bool) {
	for id, cancel := range s.modelRefreshCancel {
		if cancel != nil {
			cancel()
		}
		delete(s.modelRefreshCancel, id)
	}
	for id, pending := range s.modelRefreshPending {
		s.modelRefreshGroup.Forget(pending.identity.key)
		delete(s.modelRefreshPending, id)
	}
	for id, active := range s.modelRefreshActive {
		s.modelRefreshGroup.Forget(active.identity.key)
		delete(s.modelRefreshActive, id)
	}
	for id := range s.modelRefreshVersion {
		delete(s.modelRefreshVersion, id)
	}
	if clearCache {
		for id := range s.modelRefreshCache {
			delete(s.modelRefreshCache, id)
		}
		clear(s.modelRefreshFailures)
	}
}

func (s *Service) stopModelRefreshWorkers(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.modelRefreshMu.Lock()
	s.modelRefreshClosed = true
	s.cancelAllModelRefreshesLocked(true)
	if s.modelRefreshStop != nil {
		s.modelRefreshStop()
		s.modelRefreshStop = nil
	}
	s.modelRefreshMu.Unlock()

	workersDone := make(chan struct{})
	go func() {
		s.modelRefreshWorkers.Wait()
		s.modelRefreshDiscoveries.Wait()
		close(workersDone)
	}()
	select {
	case <-workersDone:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop model refresh workers: %w", ctx.Err())
	}
}

func (s *Service) publishConfig(cfg *config.Config) {
	if s == nil {
		return
	}
	s.modelRefreshMu.Lock()
	defer s.modelRefreshMu.Unlock()
	s.cancelAllModelRefreshesLocked(false)
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()
}

func (s *Service) ensureModelRefreshWorkersLocked() {
	if s.modelRefreshClosed || s.modelRefreshStop != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.modelRefreshStop = cancel
	if s.modelRefreshWake == nil {
		s.modelRefreshWake = make(chan struct{}, 1)
	}
	if s.modelRefreshSlots == nil {
		s.modelRefreshSlots = make(chan struct{}, modelRefreshWorkerCount)
	}
	if s.modelRefreshScheduleWake == nil {
		s.modelRefreshScheduleWake = make(chan struct{}, 1)
	}
	s.modelRefreshWorkers.Add(modelRefreshWorkerCount + 1)
	for range modelRefreshWorkerCount {
		go s.modelRefreshWorker(ctx)
	}
	go s.runModelRefreshSchedule(ctx)
}

func (s *Service) wakeModelRefreshWorkerLocked() {
	select {
	case s.modelRefreshWake <- struct{}{}:
	default:
	}
	select {
	case s.modelRefreshScheduleWake <- struct{}{}:
	default:
	}
}

func (s *Service) nextModelRefreshDelay() time.Duration {
	now := time.Now()
	next := now.Add(modelRefreshScanInterval)
	s.modelRefreshMu.Lock()
	defer s.modelRefreshMu.Unlock()
	for _, entry := range s.modelRefreshCache {
		if entry.source == modelDiscoverySourceAntigravity {
			if expiry := entry.observedAt.Add(modelRefreshCacheTTL); expiry.After(now) && expiry.Before(next) {
				next = expiry
			}
		}
	}
	for _, failure := range s.modelRefreshFailures {
		if failure.nextRetry.After(now) && failure.nextRetry.Before(next) {
			next = failure.nextRetry
		}
	}
	return next.Sub(now)
}

func (s *Service) refreshExternalModelClients(ctx context.Context) {
	if s.coreManager == nil {
		return
	}
	for _, auth := range s.coreManager.List() {
		if ctx.Err() != nil {
			return
		}
		if !isAntigravityAuth(auth) || auth.Disabled {
			continue
		}
		if _, tryPlugin := s.externalModelRefreshMode(auth, true); !tryPlugin {
			s.scheduleExternalModelRefresh(auth, false)
		}
	}
	s.modelRefreshMu.Lock()
	defer s.modelRefreshMu.Unlock()
	for id, failure := range s.modelRefreshFailures {
		auth, exists := s.coreManager.GetByID(id)
		if !exists || auth.Disabled {
			delete(s.modelRefreshFailures, id)
			delete(s.modelRefreshCache, id)
		} else if time.Since(failure.lastFailure) > 2*modelRefreshCacheTTL {
			delete(s.modelRefreshFailures, id)
		}
	}
}

func (s *Service) runModelRefreshSchedule(ctx context.Context) {
	defer s.modelRefreshWorkers.Done()
	delay := s.nextModelRefreshDelay()
	deadline := time.Now().Add(delay)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.modelRefreshScheduleWake:
			delay = s.nextModelRefreshDelay()
			if earlier := time.Now().Add(delay); earlier.Before(deadline) {
				deadline = earlier
				timer.Reset(delay)
			}
			continue
		case <-timer.C:
			s.refreshExternalModelClients(ctx)
		}
		delay = s.nextModelRefreshDelay()
		deadline = time.Now().Add(delay)
		timer.Reset(delay)
	}
}

func (s *Service) recordModelRefreshFailureLocked(identity modelRefreshIdentity, version uint64) {
	if identity.provider != "antigravity" || identity.tryPlugin {
		return
	}
	if s.modelRefreshFailures == nil {
		s.modelRefreshFailures = make(map[string]modelRefreshFailureState)
	}
	previous := s.modelRefreshFailures[identity.authID]
	if previous.identityKey != identity.key {
		previous = modelRefreshFailureState{identityKey: identity.key}
	} else if previous.jobVersion == version {
		return
	}
	failure := nextModelRefreshFailure(previous, time.Now(), rand.Int64N)
	failure.jobVersion = version
	s.modelRefreshFailures[identity.authID] = failure
	log.WithFields(log.Fields{
		"auth_id": identity.authID, "failures": failure.failures, "next_retry": failure.nextRetry,
	}).Warn("model discovery scheduled a retry")
	s.wakeModelRefreshWorkerLocked()
}

func (s *Service) nextModelRefreshJob(
	workerCtx context.Context,
) (context.Context, context.CancelFunc, modelRefreshJob, bool) {
	s.modelRefreshMu.Lock()
	defer s.modelRefreshMu.Unlock()
	for id, job := range s.modelRefreshPending {
		delete(s.modelRefreshPending, id)
		if len(s.modelRefreshPending) > 0 {
			s.wakeModelRefreshWorkerLocked()
		}
		if s.modelRefreshVersion[id] != job.version {
			continue
		}
		jobCtx, cancel := context.WithTimeout(workerCtx, modelRefreshTimeout)
		if previous := s.modelRefreshCancel[id]; previous != nil {
			previous()
		}
		s.modelRefreshCancel[id] = cancel
		if s.modelRefreshActive == nil {
			s.modelRefreshActive = make(map[string]modelRefreshJob)
		}
		s.modelRefreshActive[id] = job
		return jobCtx, cancel, job, true
	}
	return nil, nil, modelRefreshJob{}, false
}

func (s *Service) finishModelRefreshJob(job modelRefreshJob) {
	id := strings.TrimSpace(job.auth.ID)
	s.modelRefreshMu.Lock()
	defer s.modelRefreshMu.Unlock()
	if s.modelRefreshVersion[id] == job.version {
		delete(s.modelRefreshCancel, id)
	}
	if active, ok := s.modelRefreshActive[id]; ok && active.version == job.version {
		s.modelRefreshGroup.Forget(active.identity.key)
		delete(s.modelRefreshActive, id)
	}
}

func (s *Service) modelRefreshWorker(ctx context.Context) {
	defer s.modelRefreshWorkers.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.modelRefreshWake:
		}
		for {
			jobCtx, cancel, job, ok := s.nextModelRefreshJob(ctx)
			if !ok {
				break
			}
			s.executeModelRefreshJob(jobCtx, job)
			cancel()
			s.finishModelRefreshJob(job)
		}
	}
}

func (s *Service) executeModelRefreshJob(ctx context.Context, job modelRefreshJob) {
	changes, unsubscribe := s.coreManager.WatchAuthChanges(job.auth.ID)
	defer unsubscribe()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return
		}
		s.modelRefreshMu.Lock()
		defer s.modelRefreshMu.Unlock()
		if _, current := s.modelRefreshJobCurrentLocked(job, job.auth); current {
			s.recordModelRefreshFailureLocked(job.identity, job.version)
		}
	}()

	s.modelRefreshMu.Lock()
	_, current := s.modelRefreshJobCurrentLocked(job, job.auth)
	s.modelRefreshMu.Unlock()
	if !current {
		return
	}

	resultCh := s.modelRefreshGroup.DoChan(
		job.identity.key,
		func() (any, error) {
			return s.runModelDiscovery(ctx, job), nil
		},
	)
	for {
		select {
		case <-ctx.Done():
			logModelRefreshContextError(job.auth.ID, "discovery", ctx.Err())
			return
		case _, watching := <-changes:
			s.modelRefreshMu.Lock()
			_, current = s.modelRefreshJobCurrentLocked(job, job.auth)
			s.modelRefreshMu.Unlock()
			if !watching || !current {
				return
			}
		case sharedResult := <-resultCh:
			if sharedResult.Err != nil {
				log.WithError(sharedResult.Err).WithField("auth_id", job.auth.ID).Warn(
					"model discovery singleflight failed",
				)
				return
			}
			result, ok := sharedResult.Val.(modelDiscoveryResult)
			if !ok {
				log.WithField("auth_id", job.auth.ID).Error("model discovery returned an invalid result")
				return
			}
			s.publishModelRefreshResult(ctx, job, cloneModelDiscoveryResult(result))
			return
		}
	}
}

func (s *Service) runModelDiscovery(ctx context.Context, job modelRefreshJob) modelDiscoveryResult {
	if ctx == nil {
		ctx = context.Background()
	}
	s.modelRefreshMu.Lock()
	if s.modelRefreshClosed {
		s.modelRefreshMu.Unlock()
		return modelDiscoveryResult{
			outcome: modelDiscoveryInconclusive,
			err:     fmt.Errorf("model discovery service is stopped"),
		}
	}
	if s.modelRefreshSlots == nil {
		s.modelRefreshSlots = make(chan struct{}, modelRefreshWorkerCount)
	}
	slots := s.modelRefreshSlots
	s.modelRefreshDiscoveries.Add(1)
	s.modelRefreshMu.Unlock()
	defer s.modelRefreshDiscoveries.Done()

	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return modelDiscoveryResult{outcome: modelDiscoveryInconclusive, err: ctx.Err()}
	}
	return s.discoverModelsForRefresh(ctx, job)
}

func (s *Service) discoverModelsForRefresh(ctx context.Context, job modelRefreshJob) modelDiscoveryResult {
	var pluginResult pluginhost.AuthModelResult
	if job.tryPlugin && s.pluginHost != nil {
		pluginResult = s.pluginHost.ModelsForAuth(ctx, job.auth)
		if ctx.Err() != nil {
			return modelDiscoveryResult{
				source:       modelDiscoverySourcePlugin,
				outcome:      modelDiscoveryInconclusive,
				pluginResult: pluginResult,
				err:          ctx.Err(),
			}
		}
		if !s.pluginHost.AuthModelResultCurrent(pluginResult) {
			return modelDiscoveryResult{
				source:       modelDiscoverySourcePlugin,
				outcome:      modelDiscoveryInconclusive,
				pluginResult: pluginResult,
				err:          fmt.Errorf("plugin model discovery snapshot changed"),
			}
		}
	}
	if pluginResult.Handled {
		return modelDiscoveryResultFromPlugin(pluginResult)
	}
	if strings.EqualFold(strings.TrimSpace(job.auth.Provider), "antigravity") &&
		(job.cfg == nil || !job.cfg.Home.Enabled) {
		fetched := fetchAntigravityModels(ctx, job.auth, job.cfg)
		result := modelDiscoveryResult{
			source:       modelDiscoverySourceAntigravity,
			models:       fromRegistryModelInfos(fetched.Models),
			auth:         fetched.Auth,
			pluginResult: pluginResult,
			err:          fetched.Err,
		}
		switch fetched.Outcome {
		case executor.ModelDiscoveryComplete:
			if len(result.models) == 0 {
				result.outcome = modelDiscoveryAuthoritativeNegative
			} else {
				result.outcome = modelDiscoveryAuthoritativePositive
			}
		case executor.ModelDiscoveryInconclusive:
			result.outcome = modelDiscoveryInconclusive
		case executor.ModelDiscoveryFailed:
			result.outcome = modelDiscoveryFailure
		default:
			result.outcome = modelDiscoveryFailure
			result.err = fmt.Errorf("unsupported Antigravity model discovery outcome %q", fetched.Outcome)
		}
		return result
	}
	return modelDiscoveryResult{
		outcome:      modelDiscoveryInconclusive,
		pluginResult: pluginResult,
		err:          fmt.Errorf("no external model provider handled auth %s", job.auth.ID),
	}
}

func modelDiscoveryResultFromPlugin(pluginResult pluginhost.AuthModelResult) modelDiscoveryResult {
	result := modelDiscoveryResult{
		source:       modelDiscoverySourcePlugin,
		provider:     strings.ToLower(strings.TrimSpace(pluginResult.Provider)),
		models:       fromRegistryModelInfos(pluginResult.Models),
		auth:         pluginResult.Auth,
		pluginResult: pluginResult,
		err:          pluginResult.Err,
	}
	if pluginResult.Err != nil {
		result.outcome = modelDiscoveryFailure
		return result
	}
	switch pluginResult.Outcome {
	case "", pluginapi.ModelDiscoveryComplete:
		if len(result.models) == 0 {
			result.outcome = modelDiscoveryAuthoritativeNegative
		} else {
			result.outcome = modelDiscoveryAuthoritativePositive
		}
	case pluginapi.ModelDiscoveryPartial:
		result.outcome = modelDiscoveryPartial
	case pluginapi.ModelDiscoveryInconclusive:
		result.outcome = modelDiscoveryInconclusive
	default:
		result.outcome = modelDiscoveryFailure
		result.err = fmt.Errorf("unsupported plugin model discovery outcome %q", pluginResult.Outcome)
	}
	return result
}

func cloneModelDiscoveryResult(result modelDiscoveryResult) modelDiscoveryResult {
	result.models = cloneModelInfos(result.models)
	if result.auth != nil {
		result.auth = result.auth.Clone()
	}
	result.pluginResult.Models = toRegistryModelInfos(result.models)
	if result.pluginResult.Auth != nil {
		result.pluginResult.Auth = result.pluginResult.Auth.Clone()
	}
	return result
}

func logModelRefreshContextError(authID, source string, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		log.WithError(err).WithFields(log.Fields{"auth_id": authID, "source": source}).Warn(
			"model discovery timed out; previous authoritative result remains active",
		)
		return
	}
	log.WithError(err).WithFields(log.Fields{"auth_id": authID, "source": source}).Debug(
		"model discovery canceled",
	)
}

func modelRefreshAuthMatches(expected, current *coreauth.Auth) bool {
	if expected == nil || current == nil {
		return false
	}
	return expected.ID == current.ID &&
		expected.Generation == current.Generation &&
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

func (s *Service) modelRefreshJobCurrentLocked(
	job modelRefreshJob,
	expectedAuth *coreauth.Auth,
) (*coreauth.Auth, bool) {
	id := strings.TrimSpace(job.auth.ID)
	if s.modelRefreshVersion[id] != job.version ||
		GlobalModelRegistry().ClientRegistrationEpoch(id) != job.registryEpoch {
		return nil, false
	}
	s.cfgMu.RLock()
	currentCfg := s.cfg
	s.cfgMu.RUnlock()
	if currentCfg != job.cfg {
		return nil, false
	}
	currentAuth, ok := s.coreManager.GetByID(id)
	if !ok || currentAuth.Disabled || !modelRefreshAuthMatches(expectedAuth, currentAuth) {
		return nil, false
	}
	if job.tryPlugin && (s.pluginHost == nil || s.pluginHost.Snapshot() != job.identity.pluginSnapshot) {
		return nil, false
	}
	return currentAuth, true
}

func (s *Service) modelRefreshCurrentAuthLocked(
	job modelRefreshJob,
	result modelDiscoveryResult,
	expectedAuth *coreauth.Auth,
) (*coreauth.Auth, bool) {
	currentAuth, current := s.modelRefreshJobCurrentLocked(job, expectedAuth)
	if !current {
		return nil, false
	}
	if job.tryPlugin &&
		(s.pluginHost == nil || !s.pluginHost.AuthModelResultCurrent(result.pluginResult)) {
		return nil, false
	}
	return currentAuth, true
}

func (s *Service) modelRefreshResultCurrentLocked(
	job modelRefreshJob,
	result modelDiscoveryResult,
	expectedAuth *coreauth.Auth,
) bool {
	_, current := s.modelRefreshCurrentAuthLocked(job, result, expectedAuth)
	return current
}

func (s *Service) publishModelRefreshResult(
	ctx context.Context,
	job modelRefreshJob,
	result modelDiscoveryResult,
) {
	if ctx == nil || ctx.Err() != nil {
		return
	}
	id := strings.TrimSpace(job.auth.ID)
	s.modelRefreshMu.Lock()
	currentAuth, current := s.modelRefreshCurrentAuthLocked(job, result, job.auth)
	if !current {
		s.modelRefreshMu.Unlock()
		return
	}
	if s.modelRefreshCache == nil {
		s.modelRefreshCache = make(map[string]modelRefreshCacheEntry)
	}
	cached, hasCached := s.modelRefreshCache[id]
	if !hasCached || cached.identity != job.identity {
		hasCached = false
	}
	s.modelRefreshMu.Unlock()

	activeAuth := currentAuth
	if result.auth != nil {
		var applied bool
		activeAuth, applied = s.applyModelDiscoveryAuthUpdate(ctx, job, currentAuth, result)
		if !applied {
			return
		}
	}
	if ctx.Err() != nil {
		return
	}

	s.modelRefreshMu.Lock()
	defer s.modelRefreshMu.Unlock()
	if _, current = s.modelRefreshCurrentAuthLocked(job, result, activeAuth); !current {
		return
	}
	activeIdentity, errIdentity := s.buildModelRefreshIdentity(activeAuth, job.cfg, job.tryPlugin)
	if errIdentity != nil {
		log.WithError(errIdentity).WithField("auth_id", id).Error("build model discovery identity")
		return
	}
	if !job.identity.sameSubject(activeIdentity) {
		delete(s.modelRefreshCache, id)
		delete(s.modelRefreshFailures, id)
		s.projectDefaultModelRefreshLocked(activeAuth, job.cfg)
		log.WithFields(
			log.Fields{"auth_id": id, "outcome": result.outcome},
		).Warn("model discovery result was discarded after its credential subject changed")
		return
	}

	pluginResult := result.pluginResult
	pluginResult.Models, pluginResult.Auth, pluginResult.Err = nil, nil, nil
	switch result.outcome {
	case modelDiscoveryAuthoritativePositive, modelDiscoveryAuthoritativeNegative:
		entry := modelRefreshCacheEntry{
			identity:      activeIdentity,
			source:        result.source,
			outcome:       result.outcome,
			provider:      result.provider,
			models:        cloneModelInfos(result.models),
			observedAt:    time.Now().UTC(),
			registryEpoch: job.registryEpoch + 1,
			pluginResult:  pluginResult,
		}
		if s.projectModelDiscoveryLocked(activeAuth, job.cfg, entry, job.registryEpoch) {
			s.modelRefreshCache[id] = entry
			delete(s.modelRefreshFailures, id)
			s.wakeModelRefreshWorkerLocked()
		}
	case modelDiscoveryPartial, modelDiscoveryInconclusive, modelDiscoveryFailure:
		if result.source == modelDiscoverySourceAntigravity && !errors.Is(result.err, context.Canceled) {
			s.recordModelRefreshFailureLocked(activeIdentity, job.version)
		}
		if hasCached {
			cached.identity = activeIdentity
			cached.registryEpoch = job.registryEpoch + 1
			if s.projectModelDiscoveryLocked(activeAuth, job.cfg, cached, job.registryEpoch) {
				s.modelRefreshCache[id] = cached
			}
			log.WithError(result.err).WithFields(
				log.Fields{
					"auth_id":   id,
					"outcome":   result.outcome,
					"cache_age": time.Since(cached.observedAt).Round(time.Second),
				},
			).Warn("model discovery retained the previous authoritative result")
			return
		}
		if result.outcome == modelDiscoveryPartial && len(result.models) > 0 {
			s.projectModelDiscoveryLocked(
				activeAuth,
				job.cfg,
				modelRefreshCacheEntry{
					identity:     activeIdentity,
					source:       result.source,
					outcome:      modelDiscoveryPartial,
					provider:     result.provider,
					models:       cloneModelInfos(result.models),
					pluginResult: pluginResult,
				},
				job.registryEpoch,
			)
		}
		log.WithError(result.err).WithFields(
			log.Fields{"auth_id": id, "outcome": result.outcome},
		).Warn("model discovery did not produce an authoritative result")
	}
}

func (s *Service) projectModelDiscoveryLocked(
	auth *coreauth.Auth,
	cfg *config.Config,
	entry modelRefreshCacheEntry,
	expectedEpoch uint64,
) bool {
	if auth == nil {
		return false
	}
	modelRegistry := GlobalModelRegistry()
	publish := func() bool {
		if entry.outcome == modelDiscoveryAuthoritativeNegative {
			_, applied := modelRegistry.ReplaceClientModels(auth.ID, auth.Provider, expectedEpoch, nil)
			return applied
		}
		switch entry.source {
		case modelDiscoverySourceAntigravity:
			return s.registerAntigravityModels(auth, cfg, cloneModelInfos(entry.models), expectedEpoch)
		case modelDiscoverySourcePlugin:
			return s.registerPluginDiscoveredModels(auth, cfg, entry.provider, cloneModelInfos(entry.models), expectedEpoch)
		}
		return false
	}
	var applied bool
	if entry.pluginResult.Snapshot != nil {
		applied = s.pluginHost != nil && s.pluginHost.PublishAuthModelsIfCurrent(entry.pluginResult, publish)
		if !applied {
			if cached, ok := s.modelRefreshCache[auth.ID]; ok && cached.identity == entry.identity {
				delete(s.modelRefreshCache, auth.ID)
			}
		}
	} else {
		applied = publish()
	}
	if !applied {
		log.WithField("auth_id", auth.ID).Warn("model discovery publication was rejected")
		return false
	}
	if s.coreManager != nil {
		s.coreManager.RefreshSchedulerEntry(auth.ID)
	}
	return true
}

func (s *Service) scheduleExternalModelRefresh(a *coreauth.Auth, tryPlugin bool) {
	if s == nil || a == nil || strings.TrimSpace(a.ID) == "" {
		return
	}
	auth := a.Clone()
	id := strings.TrimSpace(auth.ID)
	s.modelRefreshMu.Lock()
	if s.coreManager != nil {
		current, ok := s.coreManager.GetByID(id)
		if !ok || current == nil || current.Disabled {
			s.modelRefreshMu.Unlock()
			s.cancelModelRefresh(id)
			GlobalModelRegistry().UnregisterClient(id)
			s.coreManager.RefreshSchedulerEntry(id)
			return
		}
		auth = current
	} else if auth.Disabled {
		s.modelRefreshMu.Unlock()
		GlobalModelRegistry().UnregisterClient(id)
		return
	}

	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil || (cfg.Home.Enabled && !tryPlugin) {
		s.modelRefreshMu.Unlock()
		return
	}
	identity, errIdentity := s.buildModelRefreshIdentity(auth, cfg, tryPlugin)
	if errIdentity != nil {
		s.modelRefreshMu.Unlock()
		log.WithError(errIdentity).WithField("auth_id", id).Error("build model discovery identity")
		return
	}
	if s.modelRefreshCancel == nil {
		s.modelRefreshCancel = make(map[string]context.CancelFunc)
	}
	if s.modelRefreshVersion == nil {
		s.modelRefreshVersion = make(map[string]uint64)
	}
	if s.modelRefreshPending == nil {
		s.modelRefreshPending = make(map[string]modelRefreshJob)
	}
	if s.modelRefreshActive == nil {
		s.modelRefreshActive = make(map[string]modelRefreshJob)
	}
	if s.modelRefreshCache == nil {
		s.modelRefreshCache = make(map[string]modelRefreshCacheEntry)
	}
	s.ensureModelRefreshWorkersLocked()
	if s.modelRefreshClosed {
		s.modelRefreshMu.Unlock()
		GlobalModelRegistry().UnregisterClient(id)
		if s.coreManager != nil {
			s.coreManager.RefreshSchedulerEntry(id)
		}
		return
	}

	if pending, ok := s.modelRefreshPending[id]; ok && pending.identity.key == identity.key {
		s.modelRefreshMu.Unlock()
		return
	}
	if active, ok := s.modelRefreshActive[id]; ok && active.identity.key == identity.key {
		s.modelRefreshMu.Unlock()
		return
	}
	if pending, ok := s.modelRefreshPending[id]; ok {
		s.modelRefreshGroup.Forget(pending.identity.key)
		delete(s.modelRefreshPending, id)
	}
	if active, ok := s.modelRefreshActive[id]; ok {
		s.modelRefreshGroup.Forget(active.identity.key)
	}
	if cancel := s.modelRefreshCancel[id]; cancel != nil {
		cancel()
	}
	delete(s.modelRefreshCancel, id)

	s.modelRefreshSeq++
	if s.modelRefreshSeq == 0 {
		s.modelRefreshSeq++
	}
	version := s.modelRefreshSeq
	s.modelRefreshVersion[id] = version
	now := time.Now()
	if failure, ok := s.modelRefreshFailures[id]; ok {
		if failure.identityKey != identity.key {
			delete(s.modelRefreshFailures, id)
		} else if now.Before(failure.nextRetry) {
			if entry, cached := s.modelRefreshCache[id]; cached && entry.identity == identity {
				s.projectCachedModelRefreshLocked(auth, cfg, identity)
			}
			s.modelRefreshMu.Unlock()
			return
		}
	}
	if entry, cached := s.modelRefreshCache[id]; cached && entry.identity == identity &&
		entry.source == modelDiscoverySourceAntigravity && now.Before(entry.observedAt.Add(modelRefreshCacheTTL)) {
		s.projectCachedModelRefreshLocked(auth, cfg, identity)
		s.modelRefreshMu.Unlock()
		return
	}
	s.projectCachedModelRefreshLocked(auth, cfg, identity)
	s.modelRefreshPending[id] = modelRefreshJob{
		auth:          auth,
		cfg:           cfg,
		identity:      identity,
		version:       version,
		registryEpoch: GlobalModelRegistry().ClientRegistrationEpoch(id),
		tryPlugin:     tryPlugin,
	}
	s.wakeModelRefreshWorkerLocked()
	s.modelRefreshMu.Unlock()
}

func (s *Service) projectCachedModelRefreshLocked(
	auth *coreauth.Auth,
	cfg *config.Config,
	identity modelRefreshIdentity,
) {
	if auth == nil {
		return
	}
	entry, ok := s.modelRefreshCache[auth.ID]
	if ok && entry.identity == identity {
		epoch := GlobalModelRegistry().ClientRegistrationEpoch(auth.ID)
		if entry.registryEpoch == epoch {
			if s.coreManager != nil {
				s.coreManager.RefreshSchedulerEntry(auth.ID)
			}
			return
		}
		if s.projectModelDiscoveryLocked(auth, cfg, entry, epoch) {
			entry.registryEpoch = epoch + 1
			s.modelRefreshCache[auth.ID] = entry
		}
		return
	}
	if ok {
		delete(s.modelRefreshCache, auth.ID)
	}
	s.projectDefaultModelRefreshLocked(auth, cfg)
}

func (s *Service) projectDefaultModelRefreshLocked(auth *coreauth.Auth, cfg *config.Config) {
	if auth == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
		GlobalModelRegistry().UnregisterClient(auth.ID)
	} else {
		s.registerModelsForAuthWithConfig(auth, cfg)
	}
	if s.coreManager != nil {
		s.coreManager.RefreshSchedulerEntry(auth.ID)
	}
}

func (s *Service) registerAntigravityModels(
	a *coreauth.Auth,
	cfg *config.Config,
	models []*ModelInfo,
	expectedEpoch uint64,
) bool {
	if s == nil || a == nil || cfg == nil {
		return false
	}
	authKind := a.AuthKind()
	if authKind == "" {
		if kind, _ := a.AccountInfo(); strings.EqualFold(kind, "api_key") {
			authKind = coreauth.AuthKindAPIKey
		}
	}
	excluded := oauthExcludedModelsForConfig(cfg, "antigravity", authKind)
	if a.Attributes != nil {
		if value := strings.TrimSpace(a.Attributes["excluded_models"]); value != "" {
			excluded = strings.Split(value, ",")
		}
	}
	models = applyExcludedModels(models, excluded)
	models = applyOAuthModelAlias(cfg, "antigravity", authKind, a.Attributes, models)
	models = s.appendPluginModels("antigravity", models)
	models = applyExcludedModels(models, excluded)
	models = catalogEnabledModelsForConfig(cfg, models)
	models = applyOAuthSettings(cfg, "antigravity", authKind, models)
	models = applyModelPrefixes(models, a.Prefix, cfg.ForceModelPrefix)
	_, applied := GlobalModelRegistry().ReplaceClientModels(a.ID, "antigravity", expectedEpoch, models)
	return applied
}

func oauthExcludedModelsForConfig(cfg *config.Config, provider, authKind string) []string {
	if cfg == nil || strings.EqualFold(strings.TrimSpace(authKind), "apikey") {
		return nil
	}
	return cfg.OAuthExcludedModels[strings.ToLower(strings.TrimSpace(provider))]
}

func catalogEnabledModelsForConfig(cfg *config.Config, models []*ModelInfo) []*ModelInfo {
	if cfg == nil || len(models) == 0 {
		return models
	}
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		ids := []string{model.ID, model.CatalogName, model.Name}
		checked := false
		enabled := true
		for _, id := range ids {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			checked = true
			if !cfg.ModelCatalogModelEnabled(id) {
				enabled = false
				break
			}
		}
		if checked && enabled {
			out = append(out, model)
		}
	}
	return out
}

// registerModelsForAuth (re)binds provider models in the global registry using the core auth ID as client identifier.
func (s *Service) registerModelsForAuth(a *coreauth.Auth) {
	s.registerModelsForAuthWithMode(a, true)
}

func (s *Service) externalModelRefreshMode(a *coreauth.Auth, allowExternal bool) (bool, bool) {
	if s == nil || a == nil || !allowExternal {
		return false, false
	}
	provider := strings.ToLower(strings.TrimSpace(a.Provider))
	if _, _, compatDetected := openAICompatInfoFromAuth(a); compatDetected {
		provider = "openai-compatibility"
	}
	tryPlugin := s.pluginHost != nil && s.pluginHost.HasOAuthModelProvider(provider)
	return tryPlugin || provider == "antigravity", tryPlugin
}

func (s *Service) registerModelsForAuthWithMode(a *coreauth.Auth, allowExternal bool) {
	if s == nil || a == nil || a.ID == "" {
		return
	}
	if a.Disabled {
		s.cancelModelRefresh(a.ID)
		GlobalModelRegistry().UnregisterClient(a.ID)
		return
	}
	if schedule, tryPlugin := s.externalModelRefreshMode(a, allowExternal); schedule {
		s.scheduleExternalModelRefresh(a, tryPlugin)
		return
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	s.registerModelsForAuthWithConfig(a, s.cfg)
}

func (s *Service) registerModelsForAuthWithConfig(a *coreauth.Auth, cfg *config.Config) {
	if a == nil || a.ID == "" {
		return
	}
	if a.Disabled {
		GlobalModelRegistry().UnregisterClient(a.ID)
		return
	}
	authKind := a.AuthKind()
	if authKind == "" {
		if kind, _ := a.AccountInfo(); strings.EqualFold(kind, "api_key") {
			authKind = coreauth.AuthKindAPIKey
		}
	}
	if a.Attributes != nil {
		if v := strings.TrimSpace(a.Attributes["gemini_virtual_primary"]); strings.EqualFold(v, "true") {
			GlobalModelRegistry().UnregisterClient(a.ID)
			return
		}
	}
	// Unregister legacy client ID (if present) to avoid double counting
	if a.Runtime != nil {
		if idGetter, ok := a.Runtime.(interface{ GetClientID() string }); ok {
			if rid := idGetter.GetClientID(); rid != "" && rid != a.ID {
				GlobalModelRegistry().UnregisterClient(rid)
			}
		}
	}
	provider := strings.ToLower(strings.TrimSpace(a.Provider))
	compatProviderKey, compatDisplayName, compatDetected := openAICompatInfoFromAuth(a)
	if compatDetected {
		provider = "openai-compatibility"
	}
	excluded := oauthExcludedModelsForConfig(cfg, provider, authKind)
	// The synthesizer pre-merges per-account and global exclusions into the "excluded_models" attribute.
	// If this attribute is present, it represents the complete list of exclusions and overrides the global config.
	if a.Attributes != nil {
		if val, ok := a.Attributes["excluded_models"]; ok && strings.TrimSpace(val) != "" {
			excluded = strings.Split(val, ",")
		}
	}
	var models []*ModelInfo
	switch provider {
	case "gemini":
		models = fromRegistryModelInfos(registry.GetGeminiModels())
		if entry := resolveConfigGeminiKey(cfg, a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildGeminiConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "gemini-interactions":
		models = fromRegistryModelInfos(registry.GetGeminiModels())
		if entry := resolveConfigInteractionsKey(cfg, a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildGeminiConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "vertex":
		// Vertex AI Gemini supports the same model identifiers as Gemini.
		models = fromRegistryModelInfos(registry.GetGeminiVertexModels())
		if authKind == "apikey" {
			if entry := resolveConfigVertexCompatKey(cfg, a); entry != nil {
				if len(entry.Models) > 0 {
					models = buildVertexCompatConfigModels(entry)
				}
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "gemini-cli":
		models = fromRegistryModelInfos(registry.GetGeminiCLIModels())
		models = applyExcludedModels(models, excluded)
	case "aistudio":
		models = fromRegistryModelInfos(registry.GetAIStudioModels())
		models = applyExcludedModels(models, excluded)
	case "claude":
		models = fromRegistryModelInfos(registry.GetClaudeModels())
		if entry := resolveConfigClaudeKey(cfg, a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildClaudeConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "codex":
		codexPlanType := ""
		if a.Attributes != nil {
			codexPlanType = strings.TrimSpace(a.Attributes["plan_type"])
		}
		switch strings.ToLower(codexPlanType) {
		case "free":
			models = fromRegistryModelInfos(registry.GetCodexFreeModels())
		case "team", "business", "go":
			models = fromRegistryModelInfos(registry.GetCodexTeamModels())
		case "plus":
			models = fromRegistryModelInfos(registry.GetCodexPlusModels())
		case "pro":
			models = fromRegistryModelInfos(registry.GetCodexProModels())
		default:
			if authKind == "apikey" {
				models = fromRegistryModelInfos(registry.GetCodexProModels())
			} else {
				models = fromRegistryModelInfos(registry.GetCodexFreeModels())
			}
		}
		if authKind == "apikey" {
			for _, model := range models {
				if model != nil {
					model.SupportConfigurationUpdate = false
				}
			}
		}
		if authKind == "apikey" {
			if entry := resolveConfigCodexKey(cfg, a); entry != nil {
				if len(entry.Models) > 0 {
					models = buildCodexConfigModels(entry)
				}
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "qwen":
		models = fromRegistryModelInfos(registry.GetQwenModels())
		models = applyExcludedModels(models, excluded)
	case "iflow":
		models = fromRegistryModelInfos(registry.GetIFlowModels())
		models = applyExcludedModels(models, excluded)
	case "kimi", "kimi-ai":
		models = fromRegistryModelInfos(registry.GetKimiModels())
		models = applyExcludedModels(models, excluded)
	case "meta":
		models = fromRegistryModelInfos(registry.GetMetaModels())
		if authKind == "apikey" {
			if entry := resolveConfigMetaKey(cfg, a); entry != nil {
				if len(entry.Models) > 0 {
					models = buildMetaConfigModels(entry)
				}
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	case "devin":
		models = fromRegistryModelInfos(registry.GetDevinModels())
		models = applyExcludedModels(models, excluded)
	case "xai":
		models = fromRegistryModelInfos(registry.GetXAIModels())
		if entry := resolveConfigXAIKey(cfg, a); entry != nil {
			if len(entry.Models) > 0 {
				models = buildXAIConfigModels(entry)
			}
			if authKind == "apikey" {
				excluded = entry.ExcludedModels
			}
		}
		models = applyExcludedModels(models, excluded)
	default:
		// Handle OpenAI-compatibility providers by name using config
		if cfg != nil {
			providerKey := provider
			compatName := strings.TrimSpace(a.Provider)
			isCompatAuth := false
			if compatDetected {
				if compatProviderKey != "" {
					providerKey = compatProviderKey
				}
				if compatDisplayName != "" {
					compatName = compatDisplayName
				}
				isCompatAuth = true
			}
			if strings.EqualFold(providerKey, "openai-compatibility") {
				isCompatAuth = true
				if a.Attributes != nil {
					if v := strings.TrimSpace(a.Attributes["compat_name"]); v != "" {
						compatName = v
					}
					if v := strings.TrimSpace(a.Attributes["provider_key"]); v != "" {
						providerKey = strings.ToLower(v)
						isCompatAuth = true
					}
				}
				if providerKey == "openai-compatibility" && compatName != "" {
					providerKey = strings.ToLower(compatName)
				}
			} else if a.Attributes != nil {
				if v := strings.TrimSpace(a.Attributes["compat_name"]); v != "" {
					compatName = v
					isCompatAuth = true
				}
				if v := strings.TrimSpace(a.Attributes["provider_key"]); v != "" {
					providerKey = strings.ToLower(v)
					isCompatAuth = true
				}
			}
			registerCompat := func(compat *config.OpenAICompatibility) bool {
				if compat == nil || compat.Disabled {
					return false
				}
				isCompatAuth = true
				ms := buildOpenAICompatibilityConfigModels(compat)
				if providerKey == "" {
					providerKey = "openai-compatibility"
				}
				ms = s.appendPluginModels(providerKey, ms)
				ms = applyExcludedModels(ms, excluded)
				ms = catalogEnabledModelsForConfig(cfg, ms)
				if len(ms) > 0 {
					GlobalModelRegistry().RegisterClient(
						a.ID, providerKey, applyModelPrefixes(ms, a.Prefix, cfg.ForceModelPrefix),
					)
				} else {
					GlobalModelRegistry().UnregisterClient(a.ID)
				}
				return true
			}
			if indexed := configEntryForAuthIndex(
				a, cfg.OpenAICompatibility,
			); indexed != nil && registerCompat(indexed) {
				return
			}
			for i := range cfg.OpenAICompatibility {
				compat := &cfg.OpenAICompatibility[i]
				if strings.EqualFold(compat.Name, compatName) && registerCompat(compat) {
					return
				}
			}
			if isCompatAuth {
				models = s.appendPluginModels(providerKey, nil)
				models = applyExcludedModels(models, excluded)
				models = catalogEnabledModelsForConfig(cfg, models)
				if len(models) > 0 {
					GlobalModelRegistry().RegisterClient(
						a.ID, providerKey, applyModelPrefixes(models, a.Prefix, cfg != nil && cfg.ForceModelPrefix),
					)
				} else {
					GlobalModelRegistry().UnregisterClient(a.ID)
				}
				return
			}
		}
	}
	models = applyOAuthModelAlias(cfg, provider, authKind, a.Attributes, models)
	key := provider
	if key == "" {
		key = strings.ToLower(strings.TrimSpace(a.Provider))
	}
	models = s.appendPluginModels(key, models)
	models = applyExcludedModels(models, excluded)
	models = catalogEnabledModelsForConfig(cfg, models)
	models = applyOAuthSettings(cfg, provider, authKind, models)
	if len(models) > 0 {
		GlobalModelRegistry().RegisterClient(
			a.ID, key, applyModelPrefixes(models, a.Prefix, cfg != nil && cfg.ForceModelPrefix),
		)
		return
	}

	GlobalModelRegistry().UnregisterClient(a.ID)
}

func configEntryForAuthIndex[T any](auth *coreauth.Auth, entries []T) *T {
	if auth == nil || auth.AuthSourceKind() != coreauth.AuthSourceConfig || auth.Attributes == nil {
		return nil
	}
	index, err := strconv.Atoi(strings.TrimSpace(auth.Attributes[coreauth.AttributeConfigIndex]))
	if err != nil || index < 0 || index >= len(entries) {
		return nil
	}
	return &entries[index]
}

func resolveConfigClaudeKey(cfg *config.Config, auth *coreauth.Auth) *config.ClaudeKey {
	if auth == nil || cfg == nil {
		return nil
	}
	if entry := configEntryForAuthIndex(auth, cfg.ClaudeKey); entry != nil {
		return entry
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range cfg.ClaudeKey {
		entry := &cfg.ClaudeKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
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
		for i := range cfg.ClaudeKey {
			entry := &cfg.ClaudeKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

// keyEntry abstracts a config entry with APIKey and BaseURL for the generic resolver.
type keyEntry interface {
	GetAPIKey() string
	GetBaseURL() string
}

// resolveConfigKey performs the common key+baseURL matching logic shared by
// resolveConfigGeminiKey, resolveConfigVertexCompatKey, and resolveConfigCodexKey.
func resolveConfigKey[T keyEntry](entries []T, attrKey, attrBase string) *T {
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
	return nil
}

func resolveConfigGeminiKey(cfg *config.Config, auth *coreauth.Auth) *config.GeminiKey {
	if auth == nil || cfg == nil {
		return nil
	}
	if entry := configEntryForAuthIndex(auth, cfg.GeminiKey); entry != nil {
		return entry
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	return resolveConfigKey(cfg.GeminiKey, attrKey, attrBase)
}

func resolveConfigInteractionsKey(cfg *config.Config, auth *coreauth.Auth) *config.GeminiKey {
	if auth == nil || cfg == nil {
		return nil
	}
	if entry := configEntryForAuthIndex(auth, cfg.InteractionsKey); entry != nil {
		return entry
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	return resolveConfigKey(cfg.InteractionsKey, attrKey, attrBase)
}

func resolveConfigVertexCompatKey(cfg *config.Config, auth *coreauth.Auth) *config.VertexCompatKey {
	if auth == nil || cfg == nil {
		return nil
	}
	if entry := configEntryForAuthIndex(auth, cfg.VertexCompatAPIKey); entry != nil {
		return entry
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	if entry := resolveConfigKey(cfg.VertexCompatAPIKey, attrKey, attrBase); entry != nil {
		return entry
	}
	if attrKey != "" {
		for i := range cfg.VertexCompatAPIKey {
			entry := &cfg.VertexCompatAPIKey[i]
			if strings.EqualFold(strings.TrimSpace(entry.APIKey), attrKey) {
				return entry
			}
		}
	}
	return nil
}

func resolveConfigCodexKey(cfg *config.Config, auth *coreauth.Auth) *config.CodexKey {
	if auth == nil || cfg == nil {
		return nil
	}
	if entry := configEntryForAuthIndex(auth, cfg.CodexKey); entry != nil {
		return entry
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimRight(strings.TrimSpace(auth.Attributes["base_url"]), "/")
	}
	matchesCredentials := func(entry *config.CodexKey) bool {
		if entry == nil {
			return false
		}
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimRight(strings.TrimSpace(entry.BaseURL), "/")
		if attrKey != "" {
			return strings.EqualFold(cfgKey, attrKey) && (cfgBase == "" || strings.EqualFold(cfgBase, attrBase))
		}
		return attrBase != "" && strings.EqualFold(cfgBase, attrBase)
	}
	for i := range cfg.CodexKey {
		if entry := &cfg.CodexKey[i]; matchesCredentials(entry) {
			return entry
		}
	}
	return nil
}

func resolveConfigXAIKey(cfg *config.Config, auth *coreauth.Auth) *config.XAIKey {
	if auth == nil || cfg == nil {
		return nil
	}
	if entry := configEntryForAuthIndex(auth, cfg.XAIKey); entry != nil {
		return entry
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	return resolveConfigKey(cfg.XAIKey, attrKey, attrBase)
}

func resolveConfigMetaKey(cfg *config.Config, auth *coreauth.Auth) *config.MetaKey {
	if auth == nil || cfg == nil || !coreauth.IsConfigAPIKeyAuth(auth) {
		return nil
	}
	if entry := configEntryForAuthIndex(auth, cfg.MetaKey); entry != nil {
		return entry
	}
	return resolveConfigKey(cfg.MetaKey, strings.TrimSpace(auth.Attributes["api_key"]), strings.TrimSpace(auth.Attributes["base_url"]))
}

func applyExcludedModels(models []*ModelInfo, excluded []string) []*ModelInfo {
	if len(models) == 0 || len(excluded) == 0 {
		return models
	}

	patterns := make([]string, 0, len(excluded)*2)
	for _, item := range excluded {
		trimmed := strings.ToLower(strings.TrimSpace(item))
		if trimmed == "" {
			continue
		}
		addModelInfoAccessCandidate(&patterns, trimmed)
		addModelInfoAccessCandidate(&patterns, strings.TrimPrefix(trimmed, "models/"))
	}
	if len(patterns) == 0 {
		return models
	}

	filtered := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		candidates := modelInfoAccessCandidates(model)
		blocked := false
		for _, pattern := range patterns {
			for _, candidate := range candidates {
				if matchWildcard(pattern, candidate) {
					blocked = true
					break
				}
			}
			if blocked {
				break
			}
		}
		if !blocked {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func modelInfoAccessCandidates(model *ModelInfo) []string {
	if model == nil {
		return nil
	}
	ids := []string{model.ID, model.CatalogName, model.Name}
	candidates := make([]string, 0, len(ids)*2)
	for _, id := range ids {
		id = strings.ToLower(strings.TrimSpace(id))
		if id == "" {
			continue
		}
		addModelInfoAccessCandidate(&candidates, id)
		addModelInfoAccessCandidate(&candidates, strings.TrimPrefix(id, "models/"))
	}
	return candidates
}

func addModelInfoAccessCandidate(candidates *[]string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if slices.Contains(*candidates, value) {
		return
	}
	*candidates = append(*candidates, value)
}

func applyModelPrefixes(models []*ModelInfo, prefix string, forceModelPrefix bool) []*ModelInfo {
	trimmedPrefix := strings.TrimSpace(prefix)
	if trimmedPrefix == "" || len(models) == 0 {
		return models
	}

	out := make([]*ModelInfo, 0, len(models)*2)
	seen := make(map[string]struct{}, len(models)*2)

	addModel := func(model *ModelInfo) {
		if model == nil {
			return
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		out = append(out, model)
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		baseID := strings.TrimSpace(model.ID)
		if baseID == "" {
			continue
		}
		if !forceModelPrefix || trimmedPrefix == baseID {
			addModel(model)
		}
		clone := *model
		clone.ID = trimmedPrefix + "/" + baseID
		addModel(&clone)
	}
	return out
}

// matchWildcard performs case-insensitive wildcard matching where '*' matches any substring.
func matchWildcard(pattern, value string) bool {
	if pattern == "" {
		return false
	}

	// Fast path for exact match (no wildcard present).
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}

	parts := strings.Split(pattern, "*")
	// Handle prefix.
	if prefix := parts[0]; prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = value[len(prefix):]
	}

	// Handle suffix.
	if suffix := parts[len(parts)-1]; suffix != "" {
		if !strings.HasSuffix(value, suffix) {
			return false
		}
		value = value[:len(value)-len(suffix)]
	}

	// Handle middle segments in order.
	for i := 1; i < len(parts)-1; i++ {
		segment := parts[i]
		if segment == "" {
			continue
		}
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}

	return true
}

type modelEntry interface {
	GetName() string
	GetAlias() string
	GetThinking() *registry.ThinkingSupport
}

type modelDisplayNameEntry interface {
	GetDisplayName() string
}

type modelMaxContextLengthEntry interface {
	GetMaxContextLength() int
}

type modelCompatEntry interface {
	GetIsCompat() bool
}

func normalizeConfiguredThinkingSupport(raw *registry.ThinkingSupport) *registry.ThinkingSupport {
	if raw == nil {
		return nil
	}
	normalized := *raw
	normalized.Levels = nil
	seen := make(map[string]struct{}, len(raw.Levels))
	for _, value := range raw.Levels {
		level := strings.ToLower(strings.TrimSpace(value))
		if level == "" {
			continue
		}
		switch level {
		case "none":
			normalized.ZeroAllowed = true
		case "auto":
			normalized.DynamicAllowed = true
		}
		if _, exists := seen[level]; exists {
			continue
		}
		seen[level] = struct{}{}
		normalized.Levels = append(normalized.Levels, level)
	}
	return &normalized
}

func buildOpenAICompatibilityConfigModels(compat *config.OpenAICompatibility) []*ModelInfo {
	if compat == nil || len(compat.Models) == 0 {
		return nil
	}
	now := time.Now().Unix()
	out := make([]*ModelInfo, 0, len(compat.Models))
	seen := make(map[string]struct{}, len(compat.Models))
	for i := range compat.Models {
		model := compat.Models[i]
		name := strings.TrimSpace(model.Name)
		alias := strings.TrimSpace(model.Alias)
		if alias == "" {
			alias = name
		}
		if alias == "" {
			continue
		}
		key := strings.ToLower(alias)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		display := strings.TrimSpace(model.DisplayName)
		if display == "" {
			display = name
		}
		if display == "" {
			display = alias
		}
		modelType := "openai-compatibility"
		if model.Image {
			modelType = registry.OpenAIImageModelType
		}
		info := &ModelInfo{
			ID:          alias,
			Object:      "model",
			Created:     now,
			OwnedBy:     compat.Name,
			Type:        modelType,
			DisplayName: display,
			CatalogName: name,
			IsCompat:    model.IsCompat,
			UserDefined: true,
		}
		if name != "" && !model.Image {
			if upstream := registry.LookupStaticModelInfo(name); upstream != nil {
				info.ContextLength = upstream.ContextLength
				info.MaxCompletionTokens = upstream.MaxCompletionTokens
				info.SupportedParameters = append([]string(nil), upstream.SupportedParameters...)
				info.NativeCapabilities = fromRegistryNativeCapabilities(upstream.NativeCapabilities)
				info.Thinking = fromRegistryThinkingSupport(upstream.Thinking)
				info.IsCompat = info.IsCompat || upstream.IsCompat
			}
		}
		if thinkingSupport := model.Thinking; thinkingSupport != nil {
			info.Thinking = fromRegistryThinkingSupport(normalizeConfiguredThinkingSupport(thinkingSupport))
		}
		if model.MaxContextLength > 0 {
			info.ContextLength = model.MaxContextLength
			info.MaxContextLength = model.MaxContextLength
		}
		info.SupportedInputModalities = normalizeCompatConfigModalities(model.InputModalities)
		info.SupportedOutputModalities = normalizeCompatConfigModalities(model.OutputModalities)
		out = append(out, info)
	}
	return out
}

func normalizeCompatConfigModalities(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		modality := strings.ToLower(strings.TrimSpace(item))
		if modality == "" {
			continue
		}
		if _, exists := seen[modality]; exists {
			continue
		}
		seen[modality] = struct{}{}
		out = append(out, modality)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func buildConfigModels[T modelEntry](models []T, ownedBy, modelType string) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}
	now := time.Now().Unix()
	out := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for i := range models {
		model := models[i]
		name := strings.TrimSpace(model.GetName())
		alias := strings.TrimSpace(model.GetAlias())
		if alias == "" {
			alias = name
		}
		if alias == "" {
			continue
		}
		key := strings.ToLower(alias)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		display := name
		if displayModel, ok := any(model).(modelDisplayNameEntry); ok {
			if configuredDisplay := strings.TrimSpace(displayModel.GetDisplayName()); configuredDisplay != "" {
				display = configuredDisplay
			}
		}
		if display == "" {
			display = alias
		}
		info := &ModelInfo{
			ID:          alias,
			Object:      "model",
			Created:     now,
			OwnedBy:     ownedBy,
			Type:        modelType,
			DisplayName: display,
			CatalogName: name,
			UserDefined: true,
		}
		if compatModel, ok := any(model).(modelCompatEntry); ok {
			info.IsCompat = compatModel.GetIsCompat()
		}
		if name != "" {
			if upstream := registry.LookupStaticModelInfo(name); upstream != nil {
				info.ContextLength = upstream.ContextLength
				info.MaxCompletionTokens = upstream.MaxCompletionTokens
				info.SupportedParameters = append([]string(nil), upstream.SupportedParameters...)
				info.NativeCapabilities = fromRegistryNativeCapabilities(upstream.NativeCapabilities)
				info.Thinking = fromRegistryThinkingSupport(upstream.Thinking)
				info.IsCompat = info.IsCompat || upstream.IsCompat
			}
		}
		if support := normalizeConfiguredThinkingSupport(model.GetThinking()); support != nil {
			info.Thinking = fromRegistryThinkingSupport(support)
		}
		if maxContextModel, ok := any(model).(modelMaxContextLengthEntry); ok {
			if maxContextLength := maxContextModel.GetMaxContextLength(); maxContextLength > 0 {
				info.ContextLength = maxContextLength
				info.MaxContextLength = maxContextLength
			}
		}
		out = append(out, info)
	}
	return out
}

func buildVertexCompatConfigModels(entry *config.VertexCompatKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "google", "vertex")
}

func buildGeminiConfigModels(entry *config.GeminiKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "google", "gemini")
}

func buildClaudeConfigModels(entry *config.ClaudeKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "anthropic", "claude")
}

func buildCodexConfigModels(entry *config.CodexKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	models := buildConfigModels(entry.Models, "openai", "openai")
	configured := make(map[string]bool, len(entry.Models))
	for _, model := range entry.Models {
		alias := strings.TrimSpace(model.Alias)
		if alias == "" {
			alias = strings.TrimSpace(model.Name)
		}
		key := strings.ToLower(alias)
		if _, exists := configured[key]; key != "" && !exists {
			configured[key] = model.SupportConfigurationUpdate
		}
	}
	for _, model := range models {
		model.SupportConfigurationUpdate = configured[strings.ToLower(model.ID)]
	}
	return models
}

func buildXAIConfigModels(entry *config.XAIKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "xai", "xai")
}

func buildMetaConfigModels(entry *config.MetaKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	return buildConfigModels(entry.Models, "meta", "meta")
}

func rewriteModelInfoName(name, oldID, newID string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return name
	}
	oldID = strings.TrimSpace(oldID)
	newID = strings.TrimSpace(newID)
	if oldID == "" || newID == "" {
		return name
	}
	if strings.EqualFold(oldID, newID) {
		return name
	}
	if strings.EqualFold(trimmed, oldID) {
		return newID
	}
	if strings.HasSuffix(trimmed, "/"+oldID) {
		prefix := strings.TrimSuffix(trimmed, oldID)
		return prefix + newID
	}
	if trimmed == "models/"+oldID {
		return "models/" + newID
	}
	return name
}

func applyOAuthModelAlias(cfg *config.Config, provider, authKind string, attributes map[string]string, models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return models
	}
	channel := coreauth.OAuthModelAliasChannel(provider, authKind)
	if channel == "" {
		return models
	}
	aliases := coreauth.OAuthModelAliasesFromAttributes(attributes)
	if cfg != nil {
		global := cfg.OAuthModelAlias[channel]
		if len(aliases) == 0 {
			aliases = global
		} else if len(global) > 0 {
			seen := make(map[string]struct{}, len(aliases)+len(global))
			for _, entry := range aliases {
				seen[strings.ToLower(entry.Alias)] = struct{}{}
			}
			for _, entry := range global {
				key := strings.ToLower(strings.TrimSpace(entry.Alias))
				if _, exists := seen[key]; !exists {
					seen[key] = struct{}{}
					aliases = append(aliases, entry)
				}
			}
		}
	}
	if len(aliases) == 0 {
		return models
	}

	type aliasEntry struct {
		alias       string
		displayName string
		fork        bool
	}

	forward := make(map[string][]aliasEntry, len(aliases))
	for i := range aliases {
		name := strings.TrimSpace(aliases[i].Name)
		alias := strings.TrimSpace(aliases[i].Alias)
		if name == "" || alias == "" {
			continue
		}
		if strings.EqualFold(name, alias) {
			continue
		}
		key := strings.ToLower(name)
		forward[key] = append(
			forward[key], aliasEntry{
				alias:       alias,
				displayName: strings.TrimSpace(aliases[i].DisplayName),
				fork:        aliases[i].Fork,
			},
		)
	}
	if len(forward) == 0 {
		return models
	}

	out := make([]*ModelInfo, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		entries := forward[key]
		if len(entries) == 0 {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, model)
			continue
		}

		keepOriginal := false
		for _, entry := range entries {
			if entry.fork {
				keepOriginal = true
				break
			}
		}
		if keepOriginal {
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				out = append(out, model)
			}
		}

		addedAlias := false
		for _, entry := range entries {
			mappedID := strings.TrimSpace(entry.alias)
			if mappedID == "" {
				continue
			}
			if strings.EqualFold(mappedID, id) {
				continue
			}
			aliasKey := strings.ToLower(mappedID)
			if _, exists := seen[aliasKey]; exists {
				continue
			}
			seen[aliasKey] = struct{}{}
			clone := *model
			clone.ID = mappedID
			if entry.displayName != "" {
				clone.DisplayName = entry.displayName
			}
			if strings.TrimSpace(clone.CatalogName) == "" {
				clone.CatalogName = id
			}
			if clone.Name != "" {
				clone.Name = rewriteModelInfoName(clone.Name, id, mappedID)
			}
			out = append(out, &clone)
			addedAlias = true
		}

		if !keepOriginal && !addedAlias {
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, model)
		}
	}
	return out
}

func applyOAuthSettings(cfg *config.Config, provider, authKind string, models []*ModelInfo) []*ModelInfo {
	if cfg == nil || len(models) == 0 {
		return models
	}
	channel := coreauth.OAuthModelAliasChannel(provider, authKind)
	if channel == "" {
		return models
	}
	settings := cfg.OAuthSettings[channel]
	if len(settings) == 0 {
		return models
	}
	out := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		setting := config.ResolveOAuthModelSetting(settings, model.ID, model.CatalogName, model.Name)
		if setting != nil && setting.MaxContextLength > 0 {
			copy := *model
			copy.ContextLength = setting.MaxContextLength
			copy.MaxContextLength = setting.MaxContextLength
			out = append(out, &copy)
		} else {
			out = append(out, model)
		}
	}
	return out
}
