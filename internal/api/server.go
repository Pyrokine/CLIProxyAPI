// Package api provides the HTTP API server implementation for the CLI Proxy API.
// It includes the main server struct, routing setup, middleware for CORS and authentication,
// and integration with various AI API handlers (OpenAI, Claude, Gemini).
// The server supports hot-reloading of clients and configuration.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	managementHandlers "github.com/Pyrokine/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/api/middleware"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/artifact"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	claudemodels "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/claude/models"
	codexlive "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/codex/live"
	codexmodels "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/codex/models"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/client/grokbuild"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/constant"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/managementasset"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/modelcatalog"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/safemode"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/gemini"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/openai"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	oauthCallbackSuccessHTML = `<html><head><meta charset="utf-8"><title>Authentication successful</title><script>setTimeout(function(){window.close();},5000);</script></head><body><h1>Authentication successful!</h1><p>You can close this window.</p><p>This window will close automatically in 5 seconds.</p></body></html>`
	oauthCallbackFailureHTML = `<html><head><meta charset="utf-8"><title>Authentication failed</title></head><body><h1>Authentication failed</h1><p>The authentication session is no longer available. Return to the client and start again.</p></body></html>`
)

const (
	exampleAPIKeyManagementPath = "/management.html"
	exampleAPIKeyManagementURL  = "/management.html?safe-mode=configure"
)

const (
	pluginResourceSessionCookie      = "cpa_plugin_resource_session"
	pluginResourceSessionTTL         = 5 * time.Minute
	pluginResourceSessionAbsoluteTTL = 30 * time.Minute
	oauthCallbackRateLimitWindow     = time.Minute
	oauthCallbackRateLimitMax        = 30
	oauthCallbackCleanupInterval     = 5 * time.Minute
)

type callbackRateLimiter struct {
	mu          sync.Mutex
	hits        map[string][]time.Time
	lastCleanup time.Time
}

type imageArtifactStoreAdapter struct {
	store *artifact.Store
}

func (a imageArtifactStoreAdapter) PutBytes(
	ctx context.Context,
	data []byte,
	mimeType string,
) (handlers.ImageArtifactRecord, error) {
	if a.store == nil {
		return handlers.ImageArtifactRecord{}, fmt.Errorf("artifact store is unavailable")
	}
	record, err := a.store.PutBytes(ctx, data, mimeType)
	return toImageArtifactRecord(record), err
}

func (a imageArtifactStoreAdapter) PutDataURL(ctx context.Context, raw string) (handlers.ImageArtifactRecord, error) {
	if a.store == nil {
		return handlers.ImageArtifactRecord{}, fmt.Errorf("artifact store is unavailable")
	}
	record, err := a.store.PutDataURL(ctx, raw)
	return toImageArtifactRecord(record), err
}

func (a imageArtifactStoreAdapter) PutRemoteImage(
	ctx context.Context,
	rawURL string,
) (handlers.ImageArtifactRecord, []byte, error) {
	if a.store == nil {
		return handlers.ImageArtifactRecord{}, nil, fmt.Errorf("artifact store is unavailable")
	}
	record, data, err := a.store.PutRemoteImage(ctx, rawURL)
	return toImageArtifactRecord(record), data, err
}

func toImageArtifactRecord(record artifact.Record) handlers.ImageArtifactRecord {
	return handlers.ImageArtifactRecord{
		ID:       record.ID,
		SHA256:   record.SHA256,
		MIMEType: record.MIMEType,
		Size:     record.Size,
	}
}

func newCallbackRateLimiter() *callbackRateLimiter {
	return &callbackRateLimiter{hits: make(map[string][]time.Time)}
}

func (l *callbackRateLimiter) allow(key string, now time.Time) bool {
	if l == nil {
		return true
	}
	key = strings.TrimSpace(key)
	if key == "" {
		key = "unknown"
	}
	cutoff := now.Add(-oauthCallbackRateLimitWindow)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastCleanup.IsZero() || now.Sub(l.lastCleanup) >= oauthCallbackCleanupInterval {
		l.cleanupExpired(cutoff)
		l.lastCleanup = now
	}
	hits := l.hits[key]
	kept := 0
	for _, hit := range hits {
		if hit.After(cutoff) {
			hits[kept] = hit
			kept++
		}
	}
	hits = hits[:kept]
	if len(hits) >= oauthCallbackRateLimitMax {
		l.hits[key] = hits
		return false
	}
	hits = append(hits, now)
	l.hits[key] = hits
	return true
}

func (l *callbackRateLimiter) cleanupExpired(cutoff time.Time) {
	for key, hits := range l.hits {
		kept := 0
		for _, hit := range hits {
			if hit.After(cutoff) {
				hits[kept] = hit
				kept++
			}
		}
		if kept == 0 {
			delete(l.hits, key)
			continue
		}
		l.hits[key] = hits[:kept]
	}
}

type pluginResourceSession struct {
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
}

type ManagementConfigCommit = managementHandlers.ConfigCommit

const (
	irreversibleBoolFalse  uint32 = 0
	irreversibleBoolTrue   uint32 = 1
	irreversibleBoolClosed uint32 = 2
)

type irreversibleBool struct {
	state atomic.Uint32
}

func (value *irreversibleBool) load() bool {
	return value != nil && value.state.Load() == irreversibleBoolTrue
}

func (value *irreversibleBool) store(enabled bool) {
	if value == nil {
		return
	}
	target := irreversibleBoolFalse
	if enabled {
		target = irreversibleBoolTrue
	}
	for {
		state := value.state.Load()
		if state == irreversibleBoolClosed || value.state.CompareAndSwap(state, target) {
			return
		}
	}
}

func (value *irreversibleBool) close() {
	if value != nil {
		value.state.Store(irreversibleBoolClosed)
	}
}

type serverOptionConfig struct {
	extraMiddleware           []gin.HandlerFunc
	engineConfigurator        func(*gin.Engine)
	routerConfigurator        func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)
	requestLoggerFactory      func(*config.Config, string) logging.RequestLogger
	localPassword             string
	keepAliveEnabled          bool
	keepAliveTimeout          time.Duration
	keepAliveOnTimeout        func()
	postAuthHook              auth.PostAuthHook
	postAuthPersistHook       auth.PostAuthHook
	pluginHost                *pluginhost.Host
	managementConfigCommitted func(context.Context, ManagementConfigCommit) error
	modelCatalogRuntimeSync   func(*config.Config) error
	usageManager              *coreusage.Manager
	usageQueue                *redisqueue.Queue
	usageStatisticsToggle     func(bool)
	homeClientProvider        func() *home.Client
	exampleAPIKeySafeMode     bool
}

// ServerOption customises HTTP server construction.
type ServerOption func(*serverOptionConfig)

func defaultRequestLoggerFactory(cfg *config.Config, configPath string) logging.RequestLogger {
	configDir := filepath.Dir(configPath)
	logsDir := logging.ResolveLogDirectory(cfg)
	enabled := false
	errorLogsMaxFiles := 0
	homeEnabled := false
	if cfg != nil {
		enabled = cfg.RequestLog && !cfg.CommercialMode
		errorLogsMaxFiles = cfg.ErrorLogsMaxFiles
		homeEnabled = cfg.Home.Enabled
	}
	logger := logging.NewFileRequestLogger(enabled, logsDir, configDir, errorLogsMaxFiles)
	logger.SetHomeEnabled(homeEnabled)
	return logger
}

func effectiveSDKConfig(cfg *config.Config) *config.SDKConfig {
	if cfg == nil {
		return nil
	}
	sdkCfg := cfg.SDKConfig
	sdkCfg.CodexOptimizeMultiAgentV2 = cfg.Codex.OptimizeMultiAgentV2
	sdkCfg.CodexOrphanDelegationCompatibility = cfg.Codex.OrphanDelegationCompatibility
	if cfg.CommercialMode {
		sdkCfg.RequestLog = false
	}
	return &sdkCfg
}

// WithMiddleware appends additional Gin middleware during server construction.
func WithMiddleware(mw ...gin.HandlerFunc) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.extraMiddleware = append(cfg.extraMiddleware, mw...)
	}
}

// WithEngineConfigurator allows callers to mutate the Gin engine prior to middleware setup.
func WithEngineConfigurator(fn func(*gin.Engine)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.engineConfigurator = fn
	}
}

// WithRouterConfigurator appends a callback after default routes are registered.
func WithRouterConfigurator(fn func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.routerConfigurator = fn
	}
}

// WithLocalManagementPassword stores a runtime-only management password accepted for localhost requests.
func WithLocalManagementPassword(password string) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.localPassword = password
	}
}

// WithKeepAliveEndpoint enables a keep-alive endpoint with the provided timeout and callback.
func WithKeepAliveEndpoint(timeout time.Duration, onTimeout func()) ServerOption {
	return func(cfg *serverOptionConfig) {
		if timeout <= 0 || onTimeout == nil {
			return
		}
		cfg.keepAliveEnabled = true
		cfg.keepAliveTimeout = timeout
		cfg.keepAliveOnTimeout = onTimeout
	}
}

// WithRequestLoggerFactory customises request logger creation.
func WithRequestLoggerFactory(factory func(*config.Config, string) logging.RequestLogger) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.requestLoggerFactory = factory
	}
}

// WithPostAuthHook registers a hook to be called after auth record creation.
func WithPostAuthHook(hook auth.PostAuthHook) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.postAuthHook = hook
	}
}

// WithPostAuthPersistHook registers a hook to be called after auth persistence.
func WithPostAuthPersistHook(hook auth.PostAuthHook) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.postAuthPersistHook = hook
	}
}

// WithPluginHost wires dynamic plugin management endpoints to the plugin host.
func WithPluginHost(host *pluginhost.Host) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.pluginHost = host
	}
}

func WithHomeClientProvider(provider func() *home.Client) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.homeClientProvider = provider
	}
}

func WithManagementConfigCommitted(callback func(context.Context, ManagementConfigCommit) error) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.managementConfigCommitted = callback
	}
}

func WithModelCatalogRuntimeSync(sync func(*config.Config) error) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.modelCatalogRuntimeSync = sync
	}
}

// WithUsageManager assigns the service-owned usage dispatcher to request contexts.
func WithUsageManager(manager *coreusage.Manager) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.usageManager = manager
	}
}

// WithUsageQueue assigns one service's Redis-compatible usage output queue.
func WithUsageQueue(queue *redisqueue.Queue) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.usageQueue = queue
	}
}

// WithUsageStatisticsToggle updates one service's logger runtime state.
func WithUsageStatisticsToggle(toggle func(bool)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.usageStatisticsToggle = toggle
	}
}

// WithExampleAPIKeySafeMode blocks proxy API endpoints while template API keys remain configured.
func WithExampleAPIKeySafeMode() ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.exampleAPIKeySafeMode = true
	}
}

// Server represents the main API server.
// It encapsulates the Gin engine, HTTP server, handlers, and configuration.
type Server struct {
	// engine is the Gin web framework engine instance.
	engine *gin.Engine

	// server is the underlying HTTP server.
	server *http.Server

	listenerMu sync.Mutex

	// muxBaseListener is the shared TCP listener used to serve both HTTP and Redis protocol traffic.
	muxBaseListener net.Listener

	// muxHTTPListener receives HTTP connections selected by the multiplexer.
	muxHTTPListener *muxListener

	redisConnectionsMu sync.Mutex
	redisConnections   map[net.Conn]struct{}
	redisConnectionsWG sync.WaitGroup

	// handlers contains the API handlers for processing requests.
	handlers         *handlers.BaseAPIHandler
	codexLiveHandler *codexlive.Handler

	imageArtifacts      *artifact.Store
	imageArtifactSigner *artifact.AccessSigner

	// cfg holds the current server configuration.
	cfgMu                  sync.RWMutex
	cfg                    *config.Config
	configUpdateMu         sync.Mutex
	configPublicationMu    sync.Mutex
	authPolicyMu           sync.RWMutex
	configPublicationCount int
	configPublicationIdle  chan struct{}
	stopping               atomic.Bool

	// oldConfigYaml stores a YAML snapshot of the previous configuration for change detection.
	// This prevents issues when the config object is modified in place by Management API.
	oldConfigYaml []byte

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// authRateLimiter enforces per-IP rate limiting on authentication failures.
	authRateLimiter *access.AuthRateLimiter

	// requestLogger is the request logger instance for dynamic configuration updates.
	requestLogger                logging.RequestLogger
	loggerToggle                 func(bool)
	requestLoggingRuntimeEnabled *atomic.Bool

	// configFilePath is the absolute path to the YAML config file for persistence.
	configFilePath string

	// currentPath is the absolute path to the current working directory.
	currentPath string

	// wsRoutes tracks registered websocket upgrade paths.
	wsRouteMu     sync.Mutex
	wsRoutes      map[string]struct{}
	wsAuthChanged func(bool, bool)
	wsAuthEnabled atomic.Bool

	// management handler
	mgmt *managementHandlers.Handler

	usageQueue            *redisqueue.Queue
	usageStatisticsToggle func(bool)
	homeClientProvider    func() *home.Client
	quotaScheduler        *quota.Scheduler

	// pluginHost owns dynamic plugin Management API route dispatch.
	pluginHost *pluginhost.Host

	pluginResourceSessionMu     sync.Mutex
	pluginResourceSessionTokens map[string]pluginResourceSession
	oauthCallbackLimiter        *callbackRateLimiter

	// managementRoutesRegistered tracks whether the management routes have been attached to the engine.
	managementRoutesRegistered atomic.Bool
	// managementRoutesEnabled controls whether management endpoints serve real handlers.
	managementRoutesEnabled irreversibleBool

	// envManagementSecret indicates whether MANAGEMENT_PASSWORD is configured.
	envManagementSecret bool

	localPassword string

	keepAliveEnabled   bool
	keepAliveTimeout   time.Duration
	keepAliveOnTimeout func()

	exampleAPIKeySafeModeEnabled bool
	exampleAPIKeySafeModeActive  atomic.Bool
	keepAliveHeartbeat           chan struct{}
	keepAliveStop                chan struct{}

	// redirectSrv is the HTTP→HTTPS redirect listener for graceful shutdown.
	redirectSrv *http.Server
}

// NewServer creates and initializes a new API server instance.
// It sets up the Gin engine, middleware, routes, and handlers.
//
// Parameters:
//   - cfg: The server configuration
//   - authManager: core runtime auth manager
//   - accessManager: request authentication manager
//
// Returns:
//   - *Server: A new server instance
func NewServer(
	cfg *config.Config,
	authManager *auth.Manager,
	accessManager *sdkaccess.Manager,
	configFilePath string,
	opts ...ServerOption,
) *Server {
	optionState := &serverOptionConfig{
		requestLoggerFactory: defaultRequestLoggerFactory,
	}
	for i := range opts {
		opts[i](optionState)
	}
	if optionState.usageQueue == nil {
		optionState.usageQueue = redisqueue.DefaultQueue()
	}
	optionState.usageQueue.SetUsageStatisticsEnabled(cfg.UsageStatisticsEnabled)
	optionState.usageQueue.SetRetentionSeconds(cfg.RedisUsageQueueRetentionSeconds)
	if optionState.usageStatisticsToggle != nil {
		optionState.usageStatisticsToggle(cfg.UsageStatisticsEnabled)
	}
	// Set gin mode
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}

	// Create gin engine
	engine := gin.New()
	// Proxy-aware address checks use ResolveClientAddress with explicitly trusted peers.
	if errTrusted := engine.SetTrustedProxies(nil); errTrusted != nil {
		log.WithError(errTrusted).Error("failed to disable Gin forwarded client IP handling")
	}
	if optionState.engineConfigurator != nil {
		optionState.engineConfigurator(engine)
	}

	// Add middleware
	engine.Use(logging.GinLogrusLogger())
	engine.Use(logging.GinLogrusRecovery())
	engine.Use(logging.CPATraceIDMiddleware())
	for _, mw := range optionState.extraMiddleware {
		engine.Use(mw)
	}

	// Add request logging middleware (positioned after recovery, before auth)
	// Resolve logs directory relative to the configuration file directory.
	var requestLogger logging.RequestLogger
	var toggle func(bool)
	requestLoggingRuntimeEnabled := &atomic.Bool{}
	requestLoggingRuntimeEnabled.Store(!cfg.CommercialMode)
	if optionState.requestLoggerFactory != nil {
		requestLogger = optionState.requestLoggerFactory(cfg, configFilePath)
	}
	if requestLogger != nil {
		if setter, ok := requestLogger.(interface {
			SetHomeClientProvider(func() *home.Client)
		}); ok && optionState.homeClientProvider != nil {
			setter.SetHomeClientProvider(optionState.homeClientProvider)
		}
		engine.Use(
			middleware.RequestLoggingMiddleware(
				requestLogger,
				middleware.RequestLoggingOptions{RuntimeEnabled: requestLoggingRuntimeEnabled.Load},
			),
		)
		if setter, ok := requestLogger.(interface{ SetEnabled(bool) }); ok {
			toggle = setter.SetEnabled
		}
	}

	engine.Use(corsMiddleware(cfg.CORSAllowedOrigins))
	wd, err := os.Getwd()
	if err != nil {
		wd = configFilePath
	}

	envAdminPassword, envAdminPasswordSet := os.LookupEnv("MANAGEMENT_PASSWORD")
	envAdminPassword = strings.TrimSpace(envAdminPassword)
	envManagementSecret := envAdminPasswordSet && envAdminPassword != ""

	// Create server instance
	authRateLimiter := access.NewAuthRateLimiter()
	baseHandlers := handlers.NewBaseAPIHandlers(effectiveSDKConfig(cfg), authManager)
	baseHandlers.SetUsageManager(optionState.usageManager)
	artifactNamespace := artifact.InstanceNamespace(configFilePath)
	artifactSigningKey, errArtifactSigningKey := managementHandlers.DeriveArtifactAccessSigningKey(
		configFilePath,
		artifactNamespace,
	)
	var imageArtifactSigner *artifact.AccessSigner
	if errArtifactSigningKey != nil {
		log.WithError(errArtifactSigningKey).Error("image artifact capability disabled")
	} else if signer, errSigner := artifact.NewAccessSigner(artifactNamespace, artifactSigningKey); errSigner != nil {
		log.WithError(errSigner).Error("image artifact capability disabled")
	} else {
		imageArtifactSigner = signer
	}
	var imageArtifactStore *artifact.Store
	if imageArtifactSigner != nil {
		var errArtifactStore error
		imageArtifactStore, errArtifactStore = artifact.NewStore(
			artifact.Root(configFilePath), artifact.WithProxyURL(cfg.ProxyURL),
		)
		if errArtifactStore != nil {
			log.WithError(errArtifactStore).Error("image artifact capability disabled")
			imageArtifactStore = nil
		}
	}
	if imageArtifactStore != nil {
		imageArtifactStore.ApplyCacheConfig(
			artifact.CacheConfigFromLimits(
				cfg.ImageArtifactCache.RetentionDays,
				cfg.ImageArtifactCache.MaxTotalSizeMB,
			),
		)
		if _, errCleanup := imageArtifactStore.Cleanup(context.Background()); errCleanup != nil {
			log.WithError(errCleanup).Warn("image artifact cache startup cleanup failed")
		}
		imageArtifactStore.StartCleaner()
		baseHandlers.SetImageArtifactStore(imageArtifactStoreAdapter{store: imageArtifactStore})
	}
	if optionState.pluginHost != nil {
		baseHandlers.SetPluginHost(optionState.pluginHost)
		baseHandlers.SetTranslatorPluginHooks(optionState.pluginHost)
		optionState.pluginHost.SetModelExecutor(baseHandlers)
	}
	s := &Server{
		engine:                       engine,
		handlers:                     baseHandlers,
		imageArtifacts:               imageArtifactStore,
		imageArtifactSigner:          imageArtifactSigner,
		cfg:                          cfg,
		accessManager:                accessManager,
		authRateLimiter:              authRateLimiter,
		requestLogger:                requestLogger,
		loggerToggle:                 toggle,
		requestLoggingRuntimeEnabled: requestLoggingRuntimeEnabled,
		configFilePath:               configFilePath,
		currentPath:                  wd,
		envManagementSecret:          envManagementSecret,
		wsRoutes:                     make(map[string]struct{}),
		redisConnections:             make(map[net.Conn]struct{}),
		pluginHost:                   optionState.pluginHost,
		pluginResourceSessionTokens:  make(map[string]pluginResourceSession),
		oauthCallbackLimiter:         newCallbackRateLimiter(),
		usageQueue:                   optionState.usageQueue,
		usageStatisticsToggle:        optionState.usageStatisticsToggle,
		homeClientProvider:           optionState.homeClientProvider,
		exampleAPIKeySafeModeEnabled: optionState.exampleAPIKeySafeMode,
	}
	baseHandlers.SetImageArtifactURLBuilder(s)
	s.codexLiveHandler = codexlive.NewHandler(s.handlers.AuthManager, s.configSnapshot)
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
	s.exampleAPIKeySafeModeActive.Store(s.exampleAPIKeySafeModeRequired(cfg))
	// Save initial YAML snapshot
	s.oldConfigYaml, _ = yaml.Marshal(cfg)
	s.applyAccessConfig(nil, cfg)
	if authManager != nil {
		authManager.SetRetryConfig(
			cfg.RequestRetry, time.Duration(cfg.MaxRetryInterval)*time.Second, cfg.MaxRetryCredentials,
		)
	}
	managementasset.SetCurrentConfig(cfg)
	auth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	auth.SetTransientErrorCooldownSeconds(cfg.TransientErrorCooldownSeconds)
	// Initialize management handler first (to get internalToken for quota scheduler)
	s.mgmt = managementHandlers.NewHandler(
		cfg,
		configFilePath,
		authManager,
		managementHandlers.WithConfigCommitted(
			func(ctx context.Context, commit managementHandlers.ConfigCommit) error {
				if optionState.managementConfigCommitted != nil {
					return optionState.managementConfigCommitted(ctx, commit)
				}
				s.UpdateClientsForConfigCommit(ctx, commit.Config, commit.Generation)
				if optionState.pluginHost != nil {
					if errApply := optionState.pluginHost.ApplyConfig(ctx, commit.Config); errApply != nil {
						return errApply
					}
					s.refreshPluginManagementRoutes()
				}
				return nil
			},
		),
		managementHandlers.WithPluginHost(optionState.pluginHost),
		managementHandlers.WithUsageQueue(optionState.usageQueue),
		managementHandlers.WithHomeClientProvider(optionState.homeClientProvider),
		managementHandlers.WithImageArtifacts(imageArtifactStore),
		managementHandlers.WithPluginRuntimeSync(
			func(ctx context.Context, nextCfg *config.Config) error {
				if optionState.pluginHost == nil {
					return nil
				}
				if errApply := optionState.pluginHost.ApplyConfig(ctx, nextCfg); errApply != nil {
					return errApply
				}
				s.refreshPluginManagementRoutes()
				return nil
			},
		),
		managementHandlers.WithModelCatalogRuntimeSync(
			func(nextCfg *config.Config) error {
				if !s.beginConfigPublication() {
					return errors.New("server is stopping")
				}
				defer s.endConfigPublication()
				if nextCfg != nil {
					s.setConfig(nextCfg)
				}
				if optionState.modelCatalogRuntimeSync != nil {
					if err := optionState.modelCatalogRuntimeSync(nextCfg); err != nil {
						return err
					}
				}
				s.handlers.UpdateClients(effectiveSDKConfig(s.configSnapshot()))
				return nil
			},
		),
	)

	// Create quota scheduler
	quotaCfg := quota.Config{
		Enabled:     cfg.QuotaRefresh.Enabled,
		Interval:    cfg.QuotaRefresh.Interval,
		MaxInterval: cfg.QuotaRefresh.MaxInterval,
	}
	if quotaCfg.Interval <= 0 {
		quotaCfg.Interval = 600
	}
	if quotaCfg.MaxInterval <= 0 {
		quotaCfg.MaxInterval = 1800
	}
	localAPICall := quota.NewLocalAPICallContextFunc(cfg.Port, s.mgmt.InternalToken())

	quotaScheduler := quota.NewSchedulerWithResult(
		quotaCfg, func(ctx context.Context, entry *quota.Entry) (quota.FetchResult, error) {
			if entry.AuthIndex == "" {
				return quota.FetchResult{}, fmt.Errorf("no auth_index for %s", entry.FileName)
			}

			switch entry.Type {
			case quota.TypeClaude:
				return fetchClaudeQuotaResultContext(ctx, localAPICall, entry.AuthIndex)
			case quota.TypeCodex:
				return fetchCodexQuotaResultContext(ctx, localAPICall, entry.AuthIndex, codexQuotaAccountID(authManager, entry.AuthIndex))
			case quota.TypeGeminiCli:
				return fetchGeminiQuotaResultContext(ctx, localAPICall, entry.AuthIndex)
			case quota.TypeAntigravity:
				return fetchAntigravityQuotaResultContext(ctx, localAPICall, entry.AuthIndex)
			case quota.TypeKimi:
				return fetchKimiQuotaResultContext(ctx, localAPICall, authManager, entry.AuthIndex)
			case quota.TypeXAI:
				return fetchXAIQuotaResultContext(ctx, localAPICall, entry.AuthIndex)
			case quota.TypeDevin:
				return fetchDevinQuotaResultContext(ctx, cfg, authManager, entry.AuthIndex)
			default:
				return quota.FetchResult{}, fmt.Errorf("quota fetch not implemented for type %s", entry.Type)
			}
		},
	)
	s.mgmt.SetQuotaScheduler(quotaScheduler)
	if authManager != nil {
		registerAuthManagerForQuota(quotaScheduler, authManager)
		authManager.SetQuotaSchedulingProvider(authQuotaSchedulingProvider{scheduler: quotaScheduler})
	} else if cfg.AuthDir != "" {
		registerAuthFilesForQuota(quotaScheduler, cfg.AuthDir)
	}
	s.quotaScheduler = quotaScheduler

	if quotaCfg.Enabled {
		quotaScheduler.Start()
	}

	if optionState.localPassword != "" {
		s.mgmt.SetLocalPassword(optionState.localPassword)
	}
	logDir := logging.ResolveLogDirectory(cfg)
	s.mgmt.SetLogDirectory(logDir)
	if optionState.postAuthHook != nil {
		s.mgmt.SetPostAuthHook(optionState.postAuthHook)
	}
	if optionState.postAuthPersistHook != nil {
		s.mgmt.SetPostAuthPersistHook(optionState.postAuthPersistHook)
	}
	s.localPassword = optionState.localPassword

	engine.Use(s.homeHeartbeatMiddleware())
	engine.Use(s.exampleAPIKeySafeModeMiddleware())

	// Setup routes
	s.setupRoutes()

	// Apply additional router configurators from options
	if optionState.routerConfigurator != nil {
		optionState.routerConfigurator(engine, s.handlers, cfg)
	}

	// Register management routes when configuration or environment secrets are available,
	// or when a local management password is provided (e.g. TUI mode).
	hasManagementSecret := cfg.RemoteManagement.SecretKey != "" || envManagementSecret || s.localPassword != ""
	s.managementRoutesEnabled.store(hasManagementSecret)
	s.usageQueue.SetEnabled(hasManagementSecret || (cfg != nil && cfg.Home.Enabled))
	if hasManagementSecret {
		s.registerManagementRoutes()
	}
	s.refreshPluginManagementRoutes()
	engine.NoRoute(s.pluginManagementNoRoute)

	if optionState.keepAliveEnabled {
		s.enableKeepAlive(optionState.keepAliveTimeout, optionState.keepAliveOnTimeout)
	}

	// Create HTTP server
	s.server = &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}

	return s
}

func imageArtifactPrincipal(c *gin.Context) (string, error) {
	if c == nil || c.Request == nil {
		return "", fmt.Errorf("request is unavailable")
	}
	if raw, ok := c.Get("apiKey"); ok {
		if principal := strings.TrimSpace(fmt.Sprint(raw)); principal != "" {
			return principal, nil
		}
	}
	client, resolved := access.ClientAddressFromContext(c.Request.Context())
	if !resolved {
		client = access.ResolveClientAddress(c.Request, nil)
	}
	if client.DirectLoopback {
		return "loopback", nil
	}
	return "", fmt.Errorf("authenticated principal is unavailable")
}

func (s *Server) ImageArtifactContentURL(c *gin.Context, record handlers.ImageArtifactRecord) (string, error) {
	if s == nil || s.imageArtifacts == nil || s.imageArtifactSigner == nil {
		return "", fmt.Errorf("artifact capability is unavailable")
	}
	principal, err := imageArtifactPrincipal(c)
	if err != nil {
		return "", err
	}
	accessToken, err := s.imageArtifactSigner.Sign(principal, record.ID)
	if err != nil {
		return "", err
	}
	host := strings.TrimSpace(c.Request.Host)
	if host == "" {
		return "", fmt.Errorf("request host is empty")
	}
	scheme := "http"
	cfg := s.configSnapshot()
	if c.Request.TLS != nil || requestHasTrustedForwardedHTTPS(c, cfg) {
		scheme = "https"
	}
	artifactURL := &url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   "/v1/artifacts/" + record.ID + "/content",
	}
	query := artifactURL.Query()
	query.Set("access", accessToken)
	artifactURL.RawQuery = query.Encode()
	return artifactURL.String(), nil
}

func (s *Server) imageArtifactContent(c *gin.Context) {
	if s == nil || s.imageArtifacts == nil || s.imageArtifactSigner == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "artifact store unavailable"})
		return
	}
	id := strings.TrimSpace(c.Param("artifact_id"))
	if err := artifact.ValidateID(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid artifact id"})
		return
	}
	principal, err := imageArtifactPrincipal(c)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "artifact access denied"})
		return
	}
	providedToken := strings.TrimSpace(c.Query("access"))
	if providedToken == "" || !s.imageArtifactSigner.Verify(principal, id, providedToken) {
		c.JSON(http.StatusForbidden, gin.H{"error": "artifact access denied"})
		return
	}
	file, record, err := s.imageArtifacts.Open(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusNotFound, gin.H{"error": "artifact not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to open artifact"})
		return
	}
	defer func() { _ = file.Close() }()

	c.Header("Content-Type", record.MIMEType)
	c.Header("Content-Length", fmt.Sprintf("%d", record.Size))
	c.Header("ETag", `"`+record.SHA256+`"`)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": record.ID}))
	http.ServeContent(c.Writer, c.Request, record.ID, record.ModTime, file)
}

func (s *Server) currentHomeClient() *home.Client {
	if s == nil {
		return nil
	}
	if s.homeClientProvider != nil {
		return s.homeClientProvider()
	}
	return home.Current()
}

func (s *Server) homeHeartbeatMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := s.configSnapshot()
		if cfg == nil || !cfg.Home.Enabled {
			c.Next()
			return
		}
		if c != nil && c.Request != nil {
			path := c.Request.URL.Path
			if strings.HasPrefix(path, "/v0/management/") || path == "/v0/management" || strings.HasPrefix(
				path, "/v0/resource/plugins/",
			) || path == "/management.html" {
				c.Next()
				return
			}
		}
		client := s.currentHomeClient()
		if client == nil || !client.HeartbeatOK() {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.Next()
	}
}

func (s *Server) exampleAPIKeySafeModeRequired(cfg *config.Config) bool {
	return s != nil && s.exampleAPIKeySafeModeEnabled && cfg != nil && safemode.HasExampleAPIKeys(cfg.APIKeys)
}

func (s *Server) exampleAPIKeySafeModeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || !s.exampleAPIKeySafeModeActive.Load() || c == nil || c.Request == nil || c.Request.URL == nil {
			c.Next()
			return
		}

		path := c.Request.URL.Path
		if path == exampleAPIKeyManagementPath && c.Query("safe-mode") == "configure" {
			c.Next()
			return
		}
		if (path == "/" || path == exampleAPIKeyManagementPath) && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead) {
			s.serveExampleAPIKeyWarningPage(c)
			return
		}
		if !isExampleAPIKeySafeModeProxyPath(path) {
			c.Next()
			return
		}

		c.Header("X-CPA-SAFE-MODE", "example-api-key")
		c.AbortWithStatusJSON(
			http.StatusForbidden, gin.H{
				"error":   "unsafe_example_api_key",
				"message": "Proxy API endpoints are disabled because api-keys contains template values. Open /management.html?safe-mode=configure, update api-keys in Management, then retry.",
			},
		)
	}
}

func (s *Server) serveExampleAPIKeyWarningPage(c *gin.Context) {
	cfg := s.configSnapshot()
	var keys []string
	if cfg != nil {
		keys = safemode.ExampleAPIKeys(cfg.APIKeys)
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		c.Abort()
		return
	}
	c.String(http.StatusOK, safemode.ExampleAPIKeyWarningPageHTML(keys, exampleAPIKeyManagementURL))
	c.Abort()
}

func isExampleAPIKeySafeModeProxyPath(path string) bool {
	switch {
	case path == "/v1" || strings.HasPrefix(path, "/v1/"):
		return true
	case path == "/v1beta" || strings.HasPrefix(path, "/v1beta/"):
		return true
	case path == "/openai/v1" || strings.HasPrefix(path, "/openai/v1/"):
		return true
	case path == "/backend-api/codex" || strings.HasPrefix(path, "/backend-api/codex/"):
		return true
	case path == "/responses" || strings.HasPrefix(path, "/responses/"):
		return true
	case path == "/v1internal:method":
		return true
	default:
		return false
	}
}

func (s *Server) oauthCallbackRateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.oauthCallbackLimiter == nil {
			c.Next()
			return
		}
		cfg := s.configSnapshot()
		var trustedProxies []string
		if cfg != nil {
			trustedProxies = cfg.RemoteManagement.TrustedProxies
		}
		client := access.ResolveClientAddress(c.Request, trustedProxies)
		c.Request = c.Request.WithContext(access.WithClientAddress(c.Request.Context(), client))
		if !s.oauthCallbackLimiter.allow(client.Key, time.Now()) {
			c.Header("Retry-After", "60")
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many oauth callback requests"})
			return
		}
		c.Next()
	}
}

// setupRoutes configures the API routes for the server.
// It defines the endpoints and associates them with their respective handlers.
func (s *Server) setupRoutes() {
	s.engine.GET("/management.html", s.serveManagementControlPanel)
	openaiHandlers := openai.NewAPIHandler(s.handlers)
	geminiHandlers := gemini.NewAPIHandler(s.handlers)
	geminiCLIHandlers := gemini.NewCLIAPIHandler(s.handlers)
	claudeCodeHandlers := claude.NewCodeAPIHandler(s.handlers)
	openaiResponsesHandlers := openai.NewResponsesAPIHandler(s.handlers)

	// OpenAI compatible API routes
	v1 := s.engine.Group("/v1")
	v1.Use(s.publicAuthMiddlewares()...)
	{
		v1.GET("/models", s.unifiedModelsHandler(openaiHandlers, claudeCodeHandlers))
		v1.GET("/models/*model", func(c *gin.Context) {
			c.Set(handlers.ModelDetailIDContextKey, strings.TrimPrefix(c.Param("model"), "/"))
			s.unifiedModelsHandler(openaiHandlers, claudeCodeHandlers)(c)
		})
		v1.POST("/chat/completions", openaiHandlers.ChatCompletions)
		v1.POST("/completions", openaiHandlers.Completions)
		v1.POST("/messages", claudeCodeHandlers.ClaudeMessages)
		v1.POST("/messages/count_tokens", claudeCodeHandlers.ClaudeCountTokens)
		v1.GET("/responses", openaiResponsesHandlers.ResponsesWebsocket)
		v1.POST("/responses", openaiResponsesHandlers.Responses)
		v1.POST("/responses/compact", openaiResponsesHandlers.Compact)
		v1.POST("/live", s.codexLiveHandler.Handle)
		v1.GET("/live/:call_id", s.codexLiveHandler.HandleSideband)
		v1.POST("/alpha/search", s.codexAlphaSearch)
		v1.POST("/images/generations", openaiHandlers.ImagesGenerations)
		v1.POST("/images/edits", openaiHandlers.ImagesEdits)
		v1.GET("/artifacts/:artifact_id/content", s.imageArtifactContent)
		v1.HEAD("/artifacts/:artifact_id/content", s.imageArtifactContent)
		v1.POST("/videos", openaiHandlers.VideosCreate)
		v1.POST("/videos/generations", openaiHandlers.XAIVideosGenerations)
		v1.POST("/videos/edits", openaiHandlers.XAIVideosEdits)
		v1.POST("/videos/extensions", openaiHandlers.XAIVideosExtensions)
		v1.GET("/videos/:video_id/content", openaiHandlers.VideosContent)
		v1.GET("/videos/:video_id", openaiHandlers.VideosRetrieve)
	}

	realtimeAuth := s.realtimeAuthMiddlewares(true)
	standardRealtimeAuth := s.realtimeAuthMiddlewares(false)
	registerRealtimeRoute := func(method, path string, middlewares []gin.HandlerFunc, handler gin.HandlerFunc) {
		handlers := make([]gin.HandlerFunc, 0, len(middlewares)+1)
		handlers = append(handlers, middlewares...)
		handlers = append(handlers, handler)
		s.engine.Handle(method, path, handlers...)
	}
	registerRealtimeRoute(http.MethodGet, "/v1/realtime", realtimeAuth, s.codexLiveHandler.HandleRealtimeWebsocket)
	registerRealtimeRoute(http.MethodPost, "/v1/realtime", realtimeAuth, s.codexLiveHandler.Handle)
	registerRealtimeRoute(http.MethodPost, "/v1/realtime/calls", realtimeAuth, s.codexLiveHandler.Handle)
	registerRealtimeRoute(
		http.MethodGet,
		"/v1/realtime/calls/:call_id",
		realtimeAuth,
		s.codexLiveHandler.HandleSideband,
	)
	registerRealtimeRoute(
		http.MethodPost,
		"/v1/realtime/client_secrets",
		standardRealtimeAuth,
		s.codexLiveHandler.CreateClientSecret,
	)
	registerRealtimeRoute(
		http.MethodPost,
		"/v1/realtime/sessions",
		standardRealtimeAuth,
		s.codexLiveHandler.CreateLegacySession,
	)
	registerRealtimeRoute(
		http.MethodPost,
		"/v1/realtime/transcription_sessions",
		standardRealtimeAuth,
		s.codexLiveHandler.HandleTranscriptionSession,
	)
	registerRealtimeRoute(
		http.MethodGet,
		"/v1/realtime/translations",
		realtimeAuth,
		s.codexLiveHandler.HandleTranslation,
	)
	registerRealtimeRoute(
		http.MethodPost,
		"/v1/realtime/translations",
		realtimeAuth,
		s.codexLiveHandler.HandleTranslation,
	)
	registerRealtimeRoute(
		http.MethodPost,
		"/v1/realtime/translations/client_secrets",
		standardRealtimeAuth,
		s.codexLiveHandler.HandleTranslation,
	)
	registerRealtimeRoute(
		http.MethodPost,
		"/v1/realtime/calls/:call_id/hangup",
		standardRealtimeAuth,
		s.codexLiveHandler.HandleHangup,
	)
	for _, action := range []string{"accept", "reject", "refer"} {
		registerRealtimeRoute(
			http.MethodPost,
			"/v1/realtime/calls/:call_id/"+action,
			standardRealtimeAuth,
			s.codexLiveHandler.HandleSIPControl,
		)
	}

	openaiV1 := s.engine.Group("/openai/v1")
	openaiV1.Use(s.publicAuthMiddlewares()...)
	{
		openaiV1.POST("/videos", openaiHandlers.VideosCreate)
		openaiV1.GET("/videos/:video_id/content", openaiHandlers.VideosContent)
		openaiV1.GET("/videos/:video_id", openaiHandlers.VideosRetrieve)
	}

	// Root-level Responses API routes (no /v1 prefix).
	// Codex Desktop configures base_url without /v1 and appends /responses directly.
	rootResponses := s.engine.Group("")
	rootResponses.Use(s.publicAuthMiddlewares()...)
	{
		rootResponses.GET("/responses", openaiResponsesHandlers.ResponsesWebsocket)
		rootResponses.POST("/responses", openaiResponsesHandlers.Responses)
		rootResponses.POST("/responses/compact", openaiResponsesHandlers.Compact)
	}

	// Codex Direct routes: /backend-api/codex/responses
	// Used when Codex Desktop sets chatgpt_base_url to point at this server.
	codexDirect := s.engine.Group("/backend-api/codex")
	codexDirect.Use(s.publicAuthMiddlewares()...)
	{
		codexDirect.GET("/responses", openaiResponsesHandlers.ResponsesWebsocket)
		codexDirect.POST("/responses", openaiResponsesHandlers.Responses)
		codexDirect.POST("/responses/compact", openaiResponsesHandlers.Compact)
		codexDirect.POST("/alpha/search", s.codexAlphaSearch)
	}

	// Gemini compatible API routes
	v1beta := s.engine.Group("/v1beta")
	v1beta.Use(s.publicAuthMiddlewares()...)
	{
		v1beta.GET("/models", s.geminiModelsHandler(geminiHandlers))
		v1beta.POST("/models/*action", geminiHandlers.GeminiHandler)
		v1beta.GET("/models/*action", s.geminiGetHandler(geminiHandlers))
	}

	// Root endpoint
	s.engine.GET(
		"/", func(c *gin.Context) {
			c.JSON(
				http.StatusOK, gin.H{
					"message": "CLI Proxy API Server",
					"endpoints": []string{
						"POST /v1/chat/completions",
						"POST /v1/completions",
						"GET /v1/models",
					},
				},
			)
		},
	)
	// Health check endpoints (no auth required)
	healthHandler := func(c *gin.Context) { c.Status(http.StatusOK) }
	s.engine.GET("/healthz", healthHandler)
	s.engine.HEAD("/healthz", healthHandler)
	s.engine.GET("/v0/management/update-health", noStoreMiddleware(), s.mgmt.GetUpdateHealth)
	s.engine.POST(
		"/v1internal:method",
		s.authPolicyReadLockMiddleware(),
		requireTLSForAuth(s.configSnapshot),
		authMiddleware(s.accessManager, s.authRateLimiter, s.configSnapshot),
		authPolicyUnlockMiddleware(),
		geminiCLIHandlers.CLIHandler,
	)

	// OAuth callback endpoints (reuse main server port)
	s.engine.GET(
		"/v0/management/oauth-callback",
		s.authPolicyReadLockMiddleware(),
		requireTLSForAuthRequest(s.managementConfigSnapshot),
		noStoreMiddleware(),
		s.managementAvailabilityMiddleware(),
		s.oauthCallbackRateLimitMiddleware(),
		authPolicyUnlockMiddleware(),
		s.mgmt.GetOAuthCallback,
	)
	// These endpoints receive provider redirects and persist
	// the short-lived code/state for the waiting goroutine.
	for _, cb := range []struct{ path, provider string }{
		{"/anthropic/callback", "anthropic"},
		{"/codex/callback", "codex"},
		{"/google/callback", "gemini"},
		{"/iflow/callback", "iflow"},
		{"/antigravity/callback", "antigravity"},
		{"/xai/callback", "xai"},
		{"/callback", "devin"},
		{"/devin/callback", "devin"},
	} {
		provider := cb.provider
		s.engine.GET(
			cb.path, noStoreMiddleware(), s.oauthCallbackRateLimitMiddleware(), func(c *gin.Context) {
				code := c.Query("code")
				state := c.Query("state")
				errStr := c.Query("error")
				if errStr == "" {
					errStr = c.Query("error_description")
				}
				if state == "" {
					c.Header("Content-Type", "text/html; charset=utf-8")
					c.String(http.StatusBadRequest, oauthCallbackFailureHTML)
					return
				}
				if _, errWrite := managementHandlers.WriteOAuthCallbackFileForPendingSession(
					provider, state, code, errStr,
				); errWrite != nil {
					status := http.StatusInternalServerError
					if managementHandlers.IsOAuthSessionNotPending(errWrite) {
						status = http.StatusConflict
					}
					c.Header("Content-Type", "text/html; charset=utf-8")
					c.String(status, oauthCallbackFailureHTML)
					return
				}
				c.Header("Content-Type", "text/html; charset=utf-8")
				c.String(http.StatusOK, oauthCallbackSuccessHTML)
			},
		)
	}

	// Management routes are registered lazily by registerManagementRoutes when a secret is configured.
}

// AttachWebsocketRoute registers a websocket upgrade handler on the primary Gin engine.
// When WebSocket authentication is disabled, only loopback peers may access the route.
func (s *Server) AttachWebsocketRoute(path string, handler http.Handler) {
	if s == nil || s.engine == nil || handler == nil {
		return
	}
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		trimmed = "/v1/ws"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	s.wsRouteMu.Lock()
	if _, exists := s.wsRoutes[trimmed]; exists {
		s.wsRouteMu.Unlock()
		return
	}
	s.wsRoutes[trimmed] = struct{}{}
	s.wsRouteMu.Unlock()

	authenticateRequest := authMiddleware(s.accessManager, s.authRateLimiter, s.configSnapshot)
	loopbackOnlyMiddleware := authMiddleware(nil, nil, s.configSnapshot)
	conditionalAuth := func(c *gin.Context) {
		if s.wsAuthEnabled.Load() {
			authenticateRequest(c)
			return
		}
		loopbackOnlyMiddleware(c)
	}
	finalHandler := func(c *gin.Context) {
		handler.ServeHTTP(c.Writer, c.Request)
		c.Abort()
	}

	s.engine.GET(
		trimmed,
		s.authPolicyReadLockMiddleware(),
		requireTLSForAuth(s.configSnapshot),
		conditionalAuth,
		authPolicyUnlockMiddleware(),
		finalHandler,
	)
}

func (s *Server) registerManagementRoutes() {
	if s == nil || s.engine == nil || s.mgmt == nil {
		return
	}
	if !s.managementRoutesRegistered.CompareAndSwap(false, true) {
		return
	}

	log.Info("management routes registered after secret key configuration")

	mgmt := s.engine.Group("/v0/management")
	mgmt.Use(
		s.authPolicyReadLockMiddleware(), requireTLSForAuthRequest(s.managementConfigSnapshot), noStoreMiddleware(),
		s.managementAvailabilityMiddleware(), managementBodyLimitMiddleware(),
		s.mgmt.MiddlewareWithConfig(s.managementConfigSnapshot), authPolicyUnlockMiddleware(),
		s.mgmt.MutationAuditMiddleware(),
	)
	{
		mgmt.GET("/usage", s.mgmt.GetUsageStatistics)
		mgmt.GET("/usage-queue", s.mgmt.GetUsageQueue)
		mgmt.GET("/usage/summary", s.mgmt.GetUsageSummary)
		mgmt.GET("/usage/events", s.mgmt.GetUsageEvents)
		mgmt.GET("/usage/export", s.mgmt.ExportUsageStatistics)
		mgmt.POST("/usage/import", s.mgmt.ImportUsageStatistics)
		mgmt.POST("/usage/recalculate-costs", s.mgmt.RecalculateCosts)
		mgmt.GET("/usage/recalculate-costs", s.mgmt.GetRecalculateCostsStatus)
		mgmt.GET("/model-prices", s.mgmt.GetModelPrices)
		mgmt.PUT("/model-prices", s.mgmt.PutModelPrices)
		mgmt.GET("/usage-retention", s.mgmt.GetUsageRetention)
		mgmt.PUT("/usage-retention", s.mgmt.PutUsageRetention)
		mgmt.PATCH("/usage-retention", s.mgmt.PutUsageRetention)
		mgmt.POST("/usage-retention/trim", s.mgmt.TrimUsageStatistics)
		mgmt.GET("/usage-retention/trim-preview", s.mgmt.TrimPreviewUsageStatistics)
		mgmt.GET("/usage/db-size", s.mgmt.GetUsageDBSize)
		mgmt.GET("/config", s.mgmt.GetConfig)
		mgmt.GET("/config.yaml", s.mgmt.GetConfigYAML)
		mgmt.PUT("/config.yaml", s.mgmt.PutConfigYAML)
		mgmt.POST("/config.yaml/validate", s.mgmt.ValidateConfigYAML)
		mgmt.GET("/latest-version", s.mgmt.GetLatestVersion)
		mgmt.GET("/version", s.mgmt.GetVersion)
		mgmt.GET("/releases", s.mgmt.GetReleases)
		mgmt.GET("/update-compatibility", s.mgmt.GetUpdateCompatibility)
		mgmt.POST("/update", s.mgmt.PostUpdate)
		mgmt.GET("/update-status", s.mgmt.GetUpdateStatus)
		mgmt.POST("/panel-update", s.mgmt.PostPanelUpdate)

		mgmt.GET("/debug", s.mgmt.GetDebug)
		mgmt.PUT("/debug", s.mgmt.PutDebug)
		mgmt.PATCH("/debug", s.mgmt.PutDebug)

		mgmt.GET("/logging-to-file", s.mgmt.GetLoggingToFile)
		mgmt.PUT("/logging-to-file", s.mgmt.PutLoggingToFile)
		mgmt.PATCH("/logging-to-file", s.mgmt.PutLoggingToFile)

		mgmt.GET("/logs-max-total-size-mb", s.mgmt.GetLogsMaxTotalSizeMB)
		mgmt.PUT("/logs-max-total-size-mb", s.mgmt.PutLogsMaxTotalSizeMB)
		mgmt.PATCH("/logs-max-total-size-mb", s.mgmt.PutLogsMaxTotalSizeMB)

		mgmt.GET("/image-artifact-cache", s.mgmt.GetImageArtifactCache)
		mgmt.PUT("/image-artifact-cache", s.mgmt.PutImageArtifactCache)
		mgmt.PATCH("/image-artifact-cache", s.mgmt.PutImageArtifactCache)
		mgmt.GET("/image-artifact-cache/size", s.mgmt.GetImageArtifactCacheSize)

		mgmt.GET("/error-logs-max-files", s.mgmt.GetErrorLogsMaxFiles)
		mgmt.PUT("/error-logs-max-files", s.mgmt.PutErrorLogsMaxFiles)
		mgmt.PATCH("/error-logs-max-files", s.mgmt.PutErrorLogsMaxFiles)

		mgmt.GET("/usage-statistics-enabled", s.mgmt.GetUsageStatisticsEnabled)
		mgmt.PUT("/usage-statistics-enabled", s.mgmt.PutUsageStatisticsEnabled)
		mgmt.PATCH("/usage-statistics-enabled", s.mgmt.PutUsageStatisticsEnabled)

		mgmt.GET("/proxy-url", s.mgmt.GetProxyURL)
		mgmt.PUT("/proxy-url", s.mgmt.PutProxyURL)
		mgmt.PATCH("/proxy-url", s.mgmt.PutProxyURL)
		mgmt.DELETE("/proxy-url", s.mgmt.DeleteProxyURL)

		mgmt.POST("/api-call", s.mgmt.APICall)

		mgmt.GET("/quota-exceeded/switch-project", s.mgmt.GetSwitchProject)
		mgmt.PUT("/quota-exceeded/switch-project", s.mgmt.PutSwitchProject)
		mgmt.PATCH("/quota-exceeded/switch-project", s.mgmt.PutSwitchProject)

		mgmt.GET("/quota-exceeded/switch-preview-model", s.mgmt.GetSwitchPreviewModel)
		mgmt.PUT("/quota-exceeded/switch-preview-model", s.mgmt.PutSwitchPreviewModel)
		mgmt.PATCH("/quota-exceeded/switch-preview-model", s.mgmt.PutSwitchPreviewModel)

		mgmt.POST("/reset-quota", s.mgmt.ResetQuota)
		mgmt.POST("/routing/cooldown/reset", s.mgmt.ResetCooldown)
		mgmt.GET("/quota/status", s.mgmt.GetQuotaStatus)
		mgmt.POST("/quota/refresh", s.mgmt.PostQuotaRefresh)
		mgmt.GET("/quota/refresh-tasks", s.mgmt.GetQuotaRefreshTasks)
		mgmt.GET("/quota/refresh-tasks/:task_id", s.mgmt.GetQuotaRefreshTask)
		mgmt.POST("/quota/refresh-tasks/:task_id/pause", s.mgmt.PostQuotaRefreshTaskPause)
		mgmt.POST("/quota/refresh-tasks/:task_id/resume", s.mgmt.PostQuotaRefreshTaskResume)
		mgmt.POST("/quota/refresh-tasks/:task_id/cancel", s.mgmt.PostQuotaRefreshTaskCancel)
		mgmt.GET("/quota/config", s.mgmt.GetQuotaConfig)
		mgmt.PUT("/quota/config", s.mgmt.PutQuotaConfig)

		mgmt.GET("/account-inspection/status", s.mgmt.GetAccountInspectionStatus)
		mgmt.GET("/account-inspection/schedule", s.mgmt.GetAccountInspectionSchedule)
		mgmt.PUT("/account-inspection/schedule", s.mgmt.PutAccountInspectionSchedule)
		mgmt.PATCH("/account-inspection/schedule", s.mgmt.PutAccountInspectionSchedule)
		mgmt.GET("/account-inspection/results", s.mgmt.GetAccountInspectionResults)
		mgmt.GET("/account-inspection/logs", s.mgmt.GetAccountInspectionLogs)
		mgmt.POST("/account-inspection/run", s.mgmt.PostAccountInspectionRun)
		mgmt.POST("/account-inspection/inspect-one", s.mgmt.PostAccountInspectionInspectOne)
		mgmt.POST("/account-inspection/stop", s.mgmt.PostAccountInspectionStop)
		mgmt.POST("/account-inspection/refresh-token", s.mgmt.PostAccountInspectionRefreshToken)

		mgmt.GET("/api-keys", s.mgmt.GetAPIKeys)
		mgmt.PUT("/api-keys", s.mgmt.PutAPIKeys)
		mgmt.PATCH("/api-keys", s.mgmt.PatchAPIKeys)
		mgmt.DELETE("/api-keys", s.mgmt.DeleteAPIKeys)

		mgmt.GET("/api-key-aliases", s.mgmt.GetAPIKeyAliases)
		mgmt.PUT("/api-key-aliases", s.mgmt.PutAPIKeyAliases)
		mgmt.PATCH("/api-key-aliases", s.mgmt.PatchAPIKeyAlias)
		mgmt.DELETE("/api-key-aliases", s.mgmt.DeleteAPIKeyAlias)
		mgmt.GET("/api-key-usage", s.mgmt.GetAPIKeyUsage)

		mgmt.GET("/gemini-api-key", s.mgmt.GetGeminiKeys)
		mgmt.PUT("/gemini-api-key", s.mgmt.PutGeminiKeys)
		mgmt.POST("/gemini-api-key", s.mgmt.PostGeminiKey)
		mgmt.PATCH("/gemini-api-key", s.mgmt.PatchGeminiKey)
		mgmt.DELETE("/gemini-api-key", s.mgmt.DeleteGeminiKey)

		mgmt.GET("/interactions-api-key", s.mgmt.GetInteractionsKeys)
		mgmt.PUT("/interactions-api-key", s.mgmt.PutInteractionsKeys)
		mgmt.POST("/interactions-api-key", s.mgmt.PostInteractionsKey)
		mgmt.PATCH("/interactions-api-key", s.mgmt.PatchInteractionsKey)
		mgmt.DELETE("/interactions-api-key", s.mgmt.DeleteInteractionsKey)

		mgmt.GET("/log-size", s.mgmt.GetLogSize)
		mgmt.GET("/logs", s.mgmt.GetLogs)
		mgmt.DELETE("/logs", s.mgmt.DeleteLogs)
		mgmt.GET("/request-error-logs", s.mgmt.GetRequestErrorLogs)
		mgmt.GET("/request-error-logs/:name", s.mgmt.DownloadRequestErrorLog)
		mgmt.GET("/request-log-by-id/:id", s.mgmt.GetRequestLogByID)
		mgmt.GET("/request-log", s.mgmt.GetRequestLog)
		mgmt.PUT("/request-log", s.mgmt.PutRequestLog)
		mgmt.PATCH("/request-log", s.mgmt.PutRequestLog)
		mgmt.GET("/ws-auth", s.mgmt.GetWebsocketAuth)
		mgmt.PUT("/ws-auth", s.mgmt.PutWebsocketAuth)
		mgmt.PATCH("/ws-auth", s.mgmt.PutWebsocketAuth)

		mgmt.GET("/request-retry", s.mgmt.GetRequestRetry)
		mgmt.PUT("/request-retry", s.mgmt.PutRequestRetry)
		mgmt.PATCH("/request-retry", s.mgmt.PutRequestRetry)
		mgmt.GET("/max-retry-interval", s.mgmt.GetMaxRetryInterval)
		mgmt.PUT("/max-retry-interval", s.mgmt.PutMaxRetryInterval)
		mgmt.PATCH("/max-retry-interval", s.mgmt.PutMaxRetryInterval)

		mgmt.GET("/force-model-prefix", s.mgmt.GetForceModelPrefix)
		mgmt.PUT("/force-model-prefix", s.mgmt.PutForceModelPrefix)
		mgmt.PATCH("/force-model-prefix", s.mgmt.PutForceModelPrefix)

		mgmt.GET("/routing/strategy", s.mgmt.GetRoutingStrategy)
		mgmt.PUT("/routing/strategy", s.mgmt.PutRoutingStrategy)
		mgmt.PATCH("/routing/strategy", s.mgmt.PutRoutingStrategy)

		mgmt.GET("/auto-refresh-interval", s.mgmt.GetAutoRefreshInterval)
		mgmt.PUT("/auto-refresh-interval", s.mgmt.PutAutoRefreshInterval)
		mgmt.PATCH("/auto-refresh-interval", s.mgmt.PutAutoRefreshInterval)

		mgmt.GET("/claude-api-key", s.mgmt.GetClaudeKeys)
		mgmt.PUT("/claude-api-key", s.mgmt.PutClaudeKeys)
		mgmt.POST("/claude-api-key", s.mgmt.PostClaudeKey)
		mgmt.PATCH("/claude-api-key", s.mgmt.PatchClaudeKey)
		mgmt.DELETE("/claude-api-key", s.mgmt.DeleteClaudeKey)

		mgmt.GET("/codex-api-key", s.mgmt.GetCodexKeys)
		mgmt.PUT("/codex-api-key", s.mgmt.PutCodexKeys)
		mgmt.POST("/codex-api-key", s.mgmt.PostCodexKey)
		mgmt.PATCH("/codex-api-key", s.mgmt.PatchCodexKey)
		mgmt.DELETE("/codex-api-key", s.mgmt.DeleteCodexKey)

		mgmt.GET("/xai-api-key", s.mgmt.GetXAIKeys)
		mgmt.PUT("/xai-api-key", s.mgmt.PutXAIKeys)
		mgmt.POST("/xai-api-key", s.mgmt.PostXAIKey)
		mgmt.PATCH("/xai-api-key", s.mgmt.PatchXAIKey)
		mgmt.DELETE("/xai-api-key", s.mgmt.DeleteXAIKey)

		mgmt.GET("/meta-api-key", s.mgmt.GetMetaKeys)
		mgmt.PUT("/meta-api-key", s.mgmt.PutMetaKeys)
		mgmt.POST("/meta-api-key", s.mgmt.PostMetaKey)
		mgmt.PATCH("/meta-api-key", s.mgmt.PatchMetaKey)
		mgmt.DELETE("/meta-api-key", s.mgmt.DeleteMetaKey)

		mgmt.GET("/openai-compatibility", s.mgmt.GetOpenAICompat)
		mgmt.PUT("/openai-compatibility", s.mgmt.PutOpenAICompat)
		mgmt.POST("/openai-compatibility", s.mgmt.PostOpenAICompat)
		mgmt.PATCH("/openai-compatibility", s.mgmt.PatchOpenAICompat)
		mgmt.DELETE("/openai-compatibility", s.mgmt.DeleteOpenAICompat)

		mgmt.GET("/vertex-api-key", s.mgmt.GetVertexCompatKeys)
		mgmt.PUT("/vertex-api-key", s.mgmt.PutVertexCompatKeys)
		mgmt.POST("/vertex-api-key", s.mgmt.PostVertexCompatKey)
		mgmt.PATCH("/vertex-api-key", s.mgmt.PatchVertexCompatKey)
		mgmt.DELETE("/vertex-api-key", s.mgmt.DeleteVertexCompatKey)

		mgmt.GET("/oauth-excluded-models", s.mgmt.GetOAuthExcludedModels)
		mgmt.PUT("/oauth-excluded-models", s.mgmt.PutOAuthExcludedModels)
		mgmt.PATCH("/oauth-excluded-models", s.mgmt.PatchOAuthExcludedModels)
		mgmt.DELETE("/oauth-excluded-models", s.mgmt.DeleteOAuthExcludedModels)

		mgmt.GET("/oauth-request-scoped-errors", s.mgmt.GetOAuthRequestScopedErrors)
		mgmt.PUT("/oauth-request-scoped-errors", s.mgmt.PutOAuthRequestScopedErrors)
		mgmt.PATCH("/oauth-request-scoped-errors", s.mgmt.PatchOAuthRequestScopedErrors)
		mgmt.DELETE("/oauth-request-scoped-errors", s.mgmt.DeleteOAuthRequestScopedErrors)

		mgmt.GET("/oauth-model-alias", s.mgmt.GetOAuthModelAlias)
		mgmt.PUT("/oauth-model-alias", s.mgmt.PutOAuthModelAlias)
		mgmt.PATCH("/oauth-model-alias", s.mgmt.PatchOAuthModelAlias)
		mgmt.DELETE("/oauth-model-alias", s.mgmt.DeleteOAuthModelAlias)

		mgmt.GET("/plugin-store", s.mgmt.ListPluginStore)
		mgmt.GET("/plugin-store/:id/releases", s.mgmt.ListPluginStoreReleases)
		mgmt.POST("/plugin-store/:id/install", s.mgmt.InstallPluginFromStore)
		mgmt.GET("/plugins", s.mgmt.ListPlugins)
		mgmt.DELETE("/plugins/:id", s.mgmt.DeletePlugin)
		mgmt.PATCH("/plugins/:id/enabled", s.mgmt.PatchPluginEnabled)
		mgmt.PUT("/plugins/:id/config", s.mgmt.PutPluginConfig)
		mgmt.PATCH("/plugins/:id/config", s.mgmt.PatchPluginConfig)
		mgmt.GET("/plugins/:id/quota", s.mgmt.GetPluginQuota)
		mgmt.POST("/plugins/:id/quota", s.mgmt.FetchPluginQuota)
		mgmt.DELETE("/plugins/:id/quota", s.mgmt.ResetPluginQuota)
		mgmt.POST("/plugins/:id/quota/reset", s.mgmt.ResetPluginQuota)
		mgmt.GET("/quota/providers", s.mgmt.GetQuotaProviders)
		mgmt.POST("/quota/fetch", s.mgmt.FetchCredentialQuota)
		mgmt.POST("/quota/reset", s.mgmt.ResetCredentialQuota)

		mgmt.GET("/auth-files", s.mgmt.ListAuthFiles)
		mgmt.GET("/auth-files/models", s.mgmt.GetAuthFileModels)
		mgmt.GET("/runtime-models", s.mgmt.GetRuntimeModels)
		mgmt.GET("/model-catalog", s.mgmt.GetModelCatalog)
		mgmt.PUT("/model-catalog", s.mgmt.PutModelCatalog)
		mgmt.PATCH("/model-catalog", s.mgmt.PatchModelCatalogModel)
		mgmt.DELETE("/model-catalog", s.mgmt.DeleteModelCatalogModel)
		mgmt.GET("/model-catalog/default-update", s.mgmt.GetModelCatalogDefaultUpdate)
		mgmt.POST("/model-catalog/default-update/apply", s.mgmt.ApplyModelCatalogDefaultUpdate)
		mgmt.GET("/model-definitions/:channel", s.mgmt.GetStaticModelDefinitions)
		mgmt.GET("/models-catalog-meta", s.mgmt.GetModelsCatalogMeta)
		mgmt.POST("/models-catalog-refresh", s.mgmt.PostModelsCatalogRefresh)
		mgmt.GET("/auth-files/download", s.mgmt.DownloadAuthFile)
		mgmt.POST("/auth-files", s.mgmt.UploadAuthFile)
		mgmt.DELETE("/auth-files", s.mgmt.DeleteAuthFile)
		mgmt.PATCH("/auth-files/status", s.mgmt.PatchAuthFileStatus)
		mgmt.POST("/auth-files/refresh", s.mgmt.RefreshAuthFile)
		mgmt.PATCH("/auth-files/bulk-enable", s.mgmt.PatchAuthFilesBulkEnable)
		mgmt.PATCH("/auth-files/bulk-disable", s.mgmt.PatchAuthFilesBulkDisable)
		mgmt.PATCH("/auth-files/fields", s.mgmt.PatchAuthFileFields)
		mgmt.POST("/vertex/import", s.mgmt.ImportVertexCredential)

		mgmt.GET("/rate-limit/status", s.mgmt.GetRateLimitStatus)
		mgmt.DELETE("/rate-limit/ban", s.mgmt.DeleteRateLimitBan)

		mgmt.GET("/anthropic-auth-url", s.mgmt.RequestAnthropicToken)
		mgmt.GET("/codex-auth-url", s.mgmt.RequestCodexToken)
		mgmt.GET("/gemini-cli-auth-url", s.mgmt.RequestGeminiCLIToken)
		mgmt.GET("/antigravity-auth-url", s.mgmt.RequestAntigravityToken)
		mgmt.GET("/qwen-auth-url", s.mgmt.RequestQwenToken)
		mgmt.GET("/kimi-auth-url", s.mgmt.RequestKimiToken)
		mgmt.GET("/kimi-ai-auth-url", s.mgmt.RequestKimiAIToken)
		mgmt.GET("/xai-auth-url", s.mgmt.RequestXAIToken)
		mgmt.GET("/meta-auth-url", s.mgmt.RequestMetaToken)
		mgmt.GET("/devin-auth-url", s.mgmt.RequestDevinToken)
		mgmt.GET("/iflow-auth-url", s.mgmt.RequestIFlowToken)
		mgmt.POST("/iflow-auth-url", s.mgmt.RequestIFlowCookieToken)
		mgmt.POST("/oauth-callback", s.mgmt.PostOAuthCallback)
		mgmt.GET("/get-auth-status", s.mgmt.GetAuthStatus)
		mgmt.DELETE("/oauth-session", s.mgmt.CancelAuthSession)
	}
}

func (s *Server) refreshPluginManagementRoutes() {
	if s == nil || s.pluginHost == nil || s.engine == nil {
		return
	}
	s.pluginHost.RegisterManagementRoutes(context.Background(), s.registeredManagementRouteKeys())
}

// RefreshPluginManagementRoutes rebuilds plugin-owned Management API routes.
func (s *Server) RefreshPluginManagementRoutes() {
	s.refreshPluginManagementRoutes()
}

func (s *Server) registeredManagementRouteKeys() map[string]struct{} {
	out := make(map[string]struct{})
	if s == nil || s.engine == nil {
		return out
	}
	for _, route := range s.engine.Routes() {
		if strings.HasPrefix(route.Path, "/v0/management/") || route.Path == "/v0/management" {
			out[strings.ToUpper(strings.TrimSpace(route.Method))+" "+route.Path] = struct{}{}
		}
	}
	return out
}

func (s *Server) pluginManagementNoRoute(c *gin.Context) {
	if s == nil || c == nil || c.Request == nil || c.Request.URL == nil {
		if c != nil {
			c.AbortWithStatus(http.StatusNotFound)
		}
		return
	}
	path := c.Request.URL.Path
	if strings.HasPrefix(path, "/v0/resource/plugins/") {
		s.pluginResourceNoRoute(c)
		return
	}
	if path != "/v0/management" && !strings.HasPrefix(path, "/v0/management/") {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if s.pluginHost == nil || s.mgmt == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	pluginAuthRoute := s.mgmt.HasPluginAuthURL(c.Request.Method, path)
	pluginManagementRoute := s.pluginHost.HasManagementRoute(c.Request.Method, path)
	if !pluginAuthRoute && !pluginManagementRoute {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	s.authPolicyMu.RLock()
	policyLock := &authPolicyReadLock{mu: &s.authPolicyMu}
	defer policyLock.release()
	requireTLSForAuthRequest(s.managementConfigSnapshot)(c)
	if c.IsAborted() {
		return
	}
	noStoreMiddleware()(c)
	if c.IsAborted() {
		return
	}
	s.managementAvailabilityMiddleware()(c)
	if c.IsAborted() {
		return
	}
	managementBodyLimitMiddleware()(c)
	if c.IsAborted() {
		return
	}
	s.mgmt.MiddlewareWithConfig(s.managementConfigSnapshot)(c)
	if c.IsAborted() {
		return
	}
	policyLock.release()
	if pluginAuthRoute && s.mgmt.ServePluginAuthURL(c) {
		c.Abort()
		return
	}
	if pluginManagementRoute && s.pluginHost.ServeManagementHTTP(c.Writer, c.Request) {
		c.Abort()
		return
	}
	c.AbortWithStatus(http.StatusNotFound)
}

func (s *Server) pluginResourceNoRoute(c *gin.Context) {
	if s == nil || c == nil || c.Request == nil || c.Request.URL == nil {
		if c != nil {
			c.AbortWithStatus(http.StatusNotFound)
		}
		return
	}
	path := c.Request.URL.Path
	s.authPolicyMu.RLock()
	policyLock := &authPolicyReadLock{mu: &s.authPolicyMu}
	defer policyLock.release()
	cfg := s.configSnapshot()
	if cfg == nil || cfg.Home.Enabled || s.pluginHost == nil || s.mgmt == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	requireTLSForAuthRequest(s.managementConfigSnapshot)(c)
	if c.IsAborted() {
		return
	}
	noStoreMiddleware()(c)
	if c.IsAborted() {
		return
	}
	s.managementAvailabilityMiddleware()(c)
	if c.IsAborted() {
		return
	}
	managementBodyLimitMiddleware()(c)
	if c.IsAborted() {
		return
	}
	sessionAuthenticated := s.pluginResourceSessionAuthenticated(c)
	if !sessionAuthenticated {
		s.mgmt.MiddlewareWithConfig(s.managementConfigSnapshot)(c)
		if c.IsAborted() {
			return
		}
	}
	policyLock.release()
	if !s.pluginHost.HasResourceRoute(c.Request.Method, path) {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if !sessionAuthenticated && !s.issuePluginResourceSession(c) {
		return
	}
	if s.pluginHost.ServeResourceHTTP(c.Writer, c.Request) {
		c.Abort()
		return
	}
	c.AbortWithStatus(http.StatusNotFound)
}

func (s *Server) pluginResourceSessionAuthenticated(c *gin.Context) bool {
	if s == nil || c == nil || c.Request == nil {
		return false
	}
	cookie, errCookie := c.Request.Cookie(pluginResourceSessionCookie)
	if errCookie != nil || cookie == nil || cookie.Value == "" {
		return false
	}
	now := time.Now()
	token := cookie.Value
	s.pluginResourceSessionMu.Lock()
	session, ok := s.pluginResourceSessionTokens[token]
	if !ok {
		s.pluginResourceSessionMu.Unlock()
		return false
	}
	if !session.IdleExpiresAt.After(now) || !session.AbsoluteExpiresAt.After(now) {
		delete(s.pluginResourceSessionTokens, token)
		s.pluginResourceSessionMu.Unlock()
		return false
	}
	idleExpiresAt := now.Add(pluginResourceSessionTTL)
	if idleExpiresAt.After(session.AbsoluteExpiresAt) {
		idleExpiresAt = session.AbsoluteExpiresAt
	}
	session.IdleExpiresAt = idleExpiresAt
	s.pluginResourceSessionTokens[token] = session
	s.pluginResourceSessionMu.Unlock()
	maxAge := int(idleExpiresAt.Sub(now) / time.Second)
	if maxAge <= 0 {
		maxAge = 1
	}
	s.setPluginResourceSessionCookie(c, token, maxAge)
	return true
}

func (s *Server) pluginResourceSessionCookieSecure(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	return c.Request.TLS != nil || requestHasTrustedForwardedHTTPS(c, s.configSnapshot())
}

func (s *Server) pluginResourceSessionSameSite(c *gin.Context) http.SameSite {
	if c != nil && strings.TrimSpace(c.GetHeader("Origin")) != "" && s.pluginResourceSessionCookieSecure(c) {
		return http.SameSiteNoneMode
	}
	return http.SameSiteLaxMode
}

func (s *Server) issuePluginResourceSession(c *gin.Context) bool {
	if s == nil || c == nil || c.Request == nil {
		return false
	}
	tokenBytes := make([]byte, 32)
	if _, errRead := rand.Read(tokenBytes); errRead != nil {
		log.WithError(errRead).Error("failed to generate plugin resource session token")
		c.AbortWithStatus(http.StatusInternalServerError)
		return false
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	now := time.Now()
	s.pluginResourceSessionMu.Lock()
	if s.pluginResourceSessionTokens == nil {
		s.pluginResourceSessionTokens = make(map[string]pluginResourceSession)
	}
	for existing, session := range s.pluginResourceSessionTokens {
		if !session.IdleExpiresAt.After(now) || !session.AbsoluteExpiresAt.After(now) {
			delete(s.pluginResourceSessionTokens, existing)
		}
	}
	s.pluginResourceSessionTokens[token] = pluginResourceSession{
		IdleExpiresAt:     now.Add(pluginResourceSessionTTL),
		AbsoluteExpiresAt: now.Add(pluginResourceSessionAbsoluteTTL),
	}
	s.pluginResourceSessionMu.Unlock()
	s.setPluginResourceSessionCookie(c, token, int(pluginResourceSessionTTL.Seconds()))
	return true
}

func (s *Server) setPluginResourceSessionCookie(c *gin.Context, token string, maxAge int) {
	http.SetCookie(
		c.Writer, &http.Cookie{
			Name:     pluginResourceSessionCookie,
			Value:    token,
			Path:     "/v0/resource/plugins/",
			MaxAge:   maxAge,
			HttpOnly: true,
			Secure:   s.pluginResourceSessionCookieSecure(c),
			SameSite: s.pluginResourceSessionSameSite(c),
		},
	)
}

func (s *Server) managementAvailabilityMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.managementRoutesEnabled.load() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// managementBodyLimitMiddleware caps request bodies for management endpoints.
func managementBodyLimitMiddleware() gin.HandlerFunc {
	const defaultMaxBodySize = 10 << 20 // 10 MiB
	return func(c *gin.Context) {
		maxBodySize := int64(defaultMaxBodySize)
		if c.FullPath() == "/v0/management/usage/import" {
			maxBodySize = usage.MaxPortableBackupBytes
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodySize)
		c.Next()
	}
}

func noStoreMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Next()
	}
}

const (
	authPolicyReadLockContextKey       = "cpa.auth-policy-read-lock"
	managementConfigSnapshotContextKey = "cpa.management-config-snapshot"
)

type authPolicyReadLock struct {
	mu   *sync.RWMutex
	once sync.Once
}

func (l *authPolicyReadLock) release() {
	if l == nil || l.mu == nil {
		return
	}
	l.once.Do(l.mu.RUnlock)
}

func (s *Server) authPolicyReadLockMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil {
			c.Next()
			return
		}
		s.authPolicyMu.RLock()
		lock := &authPolicyReadLock{mu: &s.authPolicyMu}
		c.Set(authPolicyReadLockContextKey, lock)
		defer lock.release()
		c.Next()
	}
}

func authPolicyUnlockMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if value, exists := c.Get(authPolicyReadLockContextKey); exists {
			if lock, ok := value.(*authPolicyReadLock); ok {
				lock.release()
			}
		}
		c.Next()
	}
}

func (s *Server) publicAuthMiddlewares() []gin.HandlerFunc {
	return []gin.HandlerFunc{
		s.authPolicyReadLockMiddleware(),
		requireTLSForAuth(s.configSnapshot),
		noStoreMiddleware(),
		authMiddleware(s.accessManager, s.authRateLimiter, s.configSnapshot),
		authPolicyUnlockMiddleware(),
	}
}

func (s *Server) realtimeAuthMiddlewares(allowClientSecret bool) []gin.HandlerFunc {
	authHandler := realtimeStandardAuthMiddleware(s.accessManager, s.authRateLimiter, s.configSnapshot)
	if allowClientSecret {
		authHandler = realtimeAuthMiddleware(
			s.accessManager,
			s.authRateLimiter,
			s.configSnapshot,
			s.codexLiveHandler,
		)
	}
	return []gin.HandlerFunc{
		s.authPolicyReadLockMiddleware(),
		requireTLSForAuth(s.configSnapshot),
		noStoreMiddleware(),
		authHandler,
		s.realtimeModelPolicyMiddleware(),
		authPolicyUnlockMiddleware(),
	}
}

func (s *Server) realtimeModelPolicyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.FullPath() == "/v1/realtime" && c.Query("call_id") == "" {
			model := strings.TrimSpace(c.Query("model"))
			if model == "" {
				model = "gpt-realtime"
			}
			principal, _ := c.Get("apiKey")
			apiKey, _ := principal.(string)
			if !codexlive.RealtimeModelAllowed(s.configSnapshot(), apiKey, model) {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": gin.H{
					"message": "Realtime request is not permitted by this API key policy",
					"type":    "permission_error",
					"param":   nil,
					"code":    "realtime_api_key_restricted",
				}})
				return
			}
		}
		c.Next()
	}
}

const (
	realtimeAuthRequestContextKey         = "realtimeAuthRequest"
	realtimeInvalidClientSecretContextKey = "realtimeInvalidClientSecret"
)

func abortInvalidRealtimeClientSecret(c *gin.Context, message string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{
		"message": message,
		"type":    "invalid_request_error",
		"param":   nil,
		"code":    "invalid_realtime_client_secret",
	}})
}

func realtimeAuthMiddleware(
	manager *sdkaccess.Manager,
	rateLimiter *access.AuthRateLimiter,
	configSnapshot func() *config.Config,
	handler *codexlive.Handler,
) gin.HandlerFunc {
	fallback := realtimeStandardAuthMiddleware(manager, rateLimiter, configSnapshot)
	return func(c *gin.Context) {
		if handler == nil {
			fallback(c)
			return
		}
		authorization, matched, errAuthenticate := handler.AuthenticateClientSecret(c.Request)
		if !matched {
			fallback(c)
			return
		}
		if errAuthenticate != nil && authManagerHasProviders(manager) {
			request := c.Request.Clone(c.Request.Context())
			request.Header = http.Header{"Authorization": {c.GetHeader("Authorization")}}
			request.Body = http.NoBody
			request.GetBody = nil
			request.ContentLength = 0
			request.Form = nil
			request.PostForm = nil
			request.MultipartForm = nil
			request.Trailer = nil
			requestURL := *request.URL
			requestURL.User = nil
			requestURL.RawQuery = ""
			requestURL.ForceQuery = false
			request.URL = &requestURL
			c.Set(realtimeAuthRequestContextKey, request)
			c.Set(realtimeInvalidClientSecretContextKey, true)
			fallback(c)
			return
		}
		if errAuthenticate != nil || strings.TrimSpace(authorization.IssuerPrincipal) == "" ||
			authorization.IssuerGeneration == 0 || authorization.IssuerGeneration != manager.Generation() {
			message := "Realtime client secret is invalid or expired"
			if errAuthenticate != nil {
				message = errAuthenticate.Error()
			}
			abortInvalidRealtimeClientSecret(c, message)
			return
		}

		issuerPrincipal := strings.TrimSpace(authorization.IssuerPrincipal)
		c.Set("apiKey", issuerPrincipal)
		c.Request = c.Request.WithContext(
			coreusage.WithAPIKeyIdentity(
				c.Request.Context(),
				coreusage.HashAPIKeyIdentity(issuerPrincipal),
			),
		)
		c.Set("accessProvider", strings.TrimSpace(authorization.IssuerProvider))
		c.Set(codexlive.ClientSecretSessionContextKey, authorization.Session)
		c.Set(codexlive.ClientSecretPrincipalContextKey, authorization.Principal)
		c.Next()
	}
}

func requestHasTrustedForwardedHTTPS(c *gin.Context, cfg *config.Config) bool {
	if c == nil || c.Request == nil || cfg == nil || !cfg.TLS.TrustForwardedProto {
		return false
	}
	client, resolved := access.ClientAddressFromContext(c.Request.Context())
	if !resolved {
		client = access.ResolveClientAddress(c.Request, cfg.RemoteManagement.TrustedProxies)
	}
	forwardedProto := c.Request.Header.Values("X-Forwarded-Proto")
	return client.TrustedPeer && len(forwardedProto) == 1 &&
		strings.EqualFold(strings.TrimSpace(forwardedProto[0]), "https")
}

// requireTLSForAuth enforces the current TLS authentication policy on every request.
// Loopback traffic is allowed based on the actual peer address so Host headers cannot bypass the gate.
func requireTLSForAuth(configSnapshot func() *config.Config) gin.HandlerFunc {
	return requireTLSForAuthRequest(
		func(*gin.Context) *config.Config {
			if configSnapshot == nil {
				return nil
			}
			return configSnapshot()
		},
	)
}

func requireTLSForAuthRequest(configSnapshot func(*gin.Context) *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var cfg *config.Config
		if configSnapshot != nil {
			cfg = configSnapshot(c)
		}
		var trustedProxies []string
		if cfg != nil {
			trustedProxies = cfg.RemoteManagement.TrustedProxies
		}
		client := access.ResolveClientAddress(c.Request, trustedProxies)
		c.Request = c.Request.WithContext(access.WithClientAddress(c.Request.Context(), client))
		if cfg == nil || !cfg.TLS.RequireForAuth {
			c.Next()
			return
		}
		if c.Request.TLS != nil {
			c.Next()
			return
		}
		if client.DirectLoopback {
			c.Next()
			return
		}
		if requestHasTrustedForwardedHTTPS(c, cfg) {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(
			http.StatusMisdirectedRequest, gin.H{
				"error":   "TLS required",
				"message": "this endpoint requires HTTPS (tls.require-for-auth=true). Retry using https:// or connect via loopback.",
			},
		)
	}
}

func (s *Server) serveManagementControlPanel(c *gin.Context) {
	cfg := s.configSnapshot()
	if cfg == nil || cfg.Home.Enabled || cfg.RemoteManagement.DisableControlPanel {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	filePath := managementasset.FilePath(s.configFilePath)
	if strings.TrimSpace(filePath) == "" {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	if _, err := os.Stat(filePath); err != nil {
		if os.IsNotExist(err) {
			// Only attempt sync download if auto-update is enabled
			if !cfg.RemoteManagement.IsAutoUpdatePanel() {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			// Synchronously ensure management.html is available with a detached context.
			// Control panel bootstrap should not be canceled by client disconnects.
			if !managementasset.EnsureManagementHTML(
				context.Background(), managementasset.StaticDir(s.configFilePath), cfg.ProxyURL,
				cfg.RemoteManagement.PanelGitHubRepository, "",
			) {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
		} else {
			log.WithError(err).Error("failed to stat management control panel asset")
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}
	}

	// CSP for the management panel SPA
	c.Header(
		"Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'self'",
	)
	c.File(filePath)
}

func (s *Server) enableKeepAlive(timeout time.Duration, onTimeout func()) {
	if timeout <= 0 || onTimeout == nil {
		return
	}

	s.keepAliveEnabled = true
	s.keepAliveTimeout = timeout
	s.keepAliveOnTimeout = onTimeout
	s.keepAliveHeartbeat = make(chan struct{}, 1)
	s.keepAliveStop = make(chan struct{}, 1)

	s.engine.GET("/keep-alive", s.handleKeepAlive)

	go s.watchKeepAlive()
}

func (s *Server) handleKeepAlive(c *gin.Context) {
	if s.localPassword != "" {
		provided := strings.TrimSpace(c.GetHeader("Authorization"))
		if provided != "" {
			parts := strings.SplitN(provided, " ", 2)
			if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
				provided = parts[1]
			}
		}
		if provided == "" {
			provided = strings.TrimSpace(c.GetHeader("X-Local-Password"))
		}
		if subtle.ConstantTimeCompare([]byte(provided), []byte(s.localPassword)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
			return
		}
	}

	s.signalKeepAlive()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (s *Server) signalKeepAlive() {
	if !s.keepAliveEnabled {
		return
	}
	select {
	case s.keepAliveHeartbeat <- struct{}{}:
	default:
	}
}

func (s *Server) watchKeepAlive() {
	if !s.keepAliveEnabled {
		return
	}

	timer := time.NewTimer(s.keepAliveTimeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			log.Warnf("keep-alive endpoint idle for %s, shutting down", s.keepAliveTimeout)
			if s.keepAliveOnTimeout != nil {
				s.keepAliveOnTimeout()
			}
			return
		case <-s.keepAliveHeartbeat:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.keepAliveTimeout)
		case <-s.keepAliveStop:
			return
		}
	}
}

// unifiedModelsHandler creates a unified handler for the /v1/models endpoint
// that routes to different handlers based on the User-Agent header.
// If User-Agent starts with "claude-cli", it routes to Claude handler,
// otherwise it routes to OpenAI handler.
func (s *Server) unifiedModelsHandler(
	openaiHandler *openai.APIHandler,
	claudeHandler *claude.CodeAPIHandler,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		if grokbuild.IsGrokShellUserAgent(c.GetHeader("User-Agent")) {
			s.handleGrokModels(c)
			return
		}

		if _, ok := c.Request.URL.Query()["client_version"]; ok {
			if cfg := s.configSnapshot(); cfg != nil && cfg.Home.Enabled {
				s.handleHomeCodexClientModels(c)
				return
			}
			openaiHandler.OpenAIModels(c)
			return
		}

		if cfg := s.configSnapshot(); cfg != nil && cfg.Home.Enabled {
			s.handleHomeModels(c)
			return
		}

		userAgent := c.GetHeader("User-Agent")

		// Route to Claude handler if User-Agent starts with "claude-cli"
		if strings.HasPrefix(userAgent, "claude-cli") {
			// log.Debugf("Routing /v1/models to Claude handler for User-Agent: %s", userAgent)
			claudeHandler.ClaudeModels(c)
		} else {
			// log.Debugf("Routing /v1/models to OpenAI handler for User-Agent: %s", userAgent)
			openaiHandler.OpenAIModels(c)
		}
	}
}

func grokModelsFromHomeEntries(entries []homeModelEntry) []grokbuild.ModelInfo {
	models := make([]grokbuild.ModelInfo, 0, len(entries))
	for _, entry := range entries {
		models = append(
			models, grokbuild.ModelInfo{
				ID:              entry.id,
				DisplayName:     entry.displayName,
				ContextLength:   entry.contextLength,
				ReasoningLevels: append([]string(nil), entry.reasoningLevels...),
			},
		)
	}
	return models
}

func grokModelsFromRegistryMaps(models []map[string]any) []grokbuild.ModelInfo {
	out := make([]grokbuild.ModelInfo, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		displayName, _ := model["display_name"].(string)
		info := grokbuild.ModelInfo{
			ID:            id,
			DisplayName:   strings.TrimSpace(displayName),
			ContextLength: modelMapInt(model, "max_context_length", "context_length"),
		}
		info.ReasoningLevels = modelMapReasoningLevels(model)
		out = append(out, info)
	}
	return out
}

func modelMapInt(model map[string]any, keys ...string) int {
	for _, key := range keys {
		var value int
		switch typed := model[key].(type) {
		case int:
			value = typed
		case int64:
			value = int(typed)
		case float64:
			value = int(typed)
		case json.Number:
			if parsed, err := typed.Int64(); err == nil {
				value = int(parsed)
			}
		}
		if value > 0 {
			return value
		}
	}
	return 0
}

func modelMapReasoningLevels(model map[string]any) []string {
	if thinking, ok := model["thinking"].(*registry.ThinkingSupport); ok && thinking != nil {
		return append([]string(nil), thinking.Levels...)
	}
	if thinking, ok := model["thinking"].(registry.ThinkingSupport); ok {
		return append([]string(nil), thinking.Levels...)
	}
	return nil
}

func (s *Server) handleGrokModels(c *gin.Context) {
	var models []grokbuild.ModelInfo
	if cfg := s.configSnapshot(); cfg != nil && cfg.Home.Enabled {
		catalog, ok := s.loadHomeModelCatalog(c)
		if !ok {
			return
		}
		models = grokModelsFromHomeEntries(homeModelEntries(catalog))
	} else {
		models = grokModelsFromRegistryMaps(registry.GetGlobalRegistry().GetAvailableModels("openai"))
	}
	models = s.filterGrokModelsForCurrentAPIKey(c, models)
	s.handlers.WriteModelListResponse(c, constant.OpenAI, grokbuild.BuildResponse(models))
}

func (s *Server) filterGrokModelsForCurrentAPIKey(c *gin.Context, models []grokbuild.ModelInfo) []grokbuild.ModelInfo {
	if len(models) == 0 {
		return models
	}
	entries := make([]map[string]any, 0, len(models))
	for _, model := range models {
		entries = append(entries, map[string]any{"id": model.ID})
	}
	allowedEntries := s.handlers.FilterModelsForCurrentAPIKey(c, entries)
	allowed := make(map[string]struct{}, len(allowedEntries))
	for _, entry := range allowedEntries {
		if id, ok := entry["id"].(string); ok {
			allowed[id] = struct{}{}
		}
	}
	filtered := make([]grokbuild.ModelInfo, 0, len(allowed))
	for _, model := range models {
		if _, ok := allowed[model.ID]; ok {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func (s *Server) handleHomeCodexClientModels(c *gin.Context) {
	catalog, ok := s.loadHomeModelCatalog(c)
	if !ok {
		return
	}
	s.writeHomeCodexClientModels(c, catalog)
}

func (s *Server) writeHomeCodexClientModels(c *gin.Context, catalog *modelcatalog.HomeCatalog) {
	if s == nil || s.handlers == nil || c == nil || catalog == nil {
		return
	}
	routes := catalog.Routes()
	devinIDs := make(map[string]struct{})
	for _, route := range routes {
		if strings.EqualFold(route.Provider, "devin") {
			if id, ok := route.Model["id"].(string); ok {
				devinIDs[id] = struct{}{}
			}
		}
	}
	available := make([]map[string]any, 0, len(routes))
	reasoningUpdates := make(map[string]bool, len(routes))
	for _, route := range routes {
		model := route.Model
		if id, ok := model["id"].(string); ok {
			if _, hasDevin := devinIDs[id]; hasDevin {
				model["type"] = "devin"
			}
			supported, explicit := model["support_configuration_update"].(bool)
			supported = explicit && supported && strings.EqualFold(route.Provider, "codex")
			id = strings.TrimSpace(id)
			if previous, exists := reasoningUpdates[id]; exists {
				supported = supported && previous
			}
			reasoningUpdates[id] = supported
		}
		available = append(available, model)
	}
	models := s.handlers.FilterModelsForCurrentAPIKey(c, available)
	cfg := s.configSnapshot()
	optimizeMultiAgentV2 := cfg != nil && cfg.Codex.OptimizeMultiAgentV2
	payload := codexmodels.BuildResponseForClientWithCPACapabilities(
		models,
		nil,
		catalog.ResponsesWebSearchCapability,
		optimizeMultiAgentV2,
		c.Query("client_version"),
		func(model string) bool {
			if cfg == nil || !cfg.Client.Codex.EnableApplyPatch || s.handlers.AuthManager == nil {
				return false
			}
			var providers []string
			for _, route := range routes {
				if id, _ := route.Model["id"].(string); strings.TrimSpace(id) == model {
					providers = append(providers, route.Provider)
				}
			}
			return s.handlers.AuthManager.SupportsApplyPatchForProviders(providers)
		},
	)
	for _, model := range payload["models"].([]map[string]any) {
		id, _ := model["slug"].(string)
		model["supports_reasoning_effort_updates"] = reasoningUpdates[strings.TrimSpace(id)]
	}
	body, err := codexmodels.MarshalCompact(payload)
	if err != nil {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{Error: handlers.ErrorDetail{
			Message: "Failed to encode model catalog",
			Type:    "server_error",
		}})
		return
	}
	s.handlers.WriteModelListResponseBody(c, constant.OpenAI, body)
}

func (s *Server) geminiModelsHandler(geminiHandler *gemini.APIHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cfg := s.configSnapshot(); cfg != nil && cfg.Home.Enabled {
			s.handleHomeGeminiModels(c)
			return
		}

		geminiHandler.GeminiModels(c)
	}
}

func (s *Server) geminiGetHandler(geminiHandler *gemini.APIHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cfg := s.configSnapshot(); cfg != nil && cfg.Home.Enabled {
			s.handleHomeGeminiModel(c)
			return
		}

		geminiHandler.GeminiGetHandler(c)
	}
}

type homeModelEntry struct {
	id                  string
	created             int64
	ownedBy             string
	displayName         string
	contextLength       int
	maxCompletionTokens int
	reasoningLevels     []string
}

func (s *Server) handleHomeModels(c *gin.Context) {
	catalog, ok := s.loadHomeModelCatalog(c)
	if !ok {
		return
	}
	entries := homeModelEntries(catalog)

	userAgent := c.GetHeader("User-Agent")
	isClaude := strings.HasPrefix(userAgent, "claude-cli")

	if isClaude {
		out := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			displayName := entry.displayName
			if displayName == "" {
				displayName = entry.id
			}
			maxInput := entry.contextLength
			if maxInput <= 0 {
				maxInput = 200000
			}
			maxOutput := entry.maxCompletionTokens
			if maxOutput <= 0 {
				maxOutput = 64000
			}
			model := map[string]any{
				"id":               entry.id,
				"object":           "model",
				"type":             "model",
				"owned_by":         entry.ownedBy,
				"display_name":     displayName,
				"max_input_tokens": maxInput,
				"max_tokens":       maxOutput,
			}
			if entry.created > 0 {
				model["created_at"] = time.Unix(entry.created, 0).UTC().Format(time.RFC3339)
			}
			out = append(out, model)
		}
		out = s.handlers.FilterModelsForCurrentAPIKey(c, out)
		cfg := s.configSnapshot()
		disableCloaking := cfg != nil && cfg.ClaudeCode.DisableCloakingModelList
		s.handlers.WriteModelListResponse(c, constant.Claude, claudemodels.NewCatalog(out, disableCloaking).Response())
		return
	}

	filtered := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		model := map[string]any{
			"id":     entry.id,
			"object": "model",
		}
		if entry.created > 0 {
			model["created"] = entry.created
		}
		if entry.ownedBy != "" {
			model["owned_by"] = entry.ownedBy
		}
		filtered = append(filtered, model)
	}
	filtered = s.handlers.FilterModelsForCurrentAPIKey(c, filtered)
	s.handlers.WriteModelListResponse(c, constant.OpenAI, gin.H{
		"object": "list",
		"data":   filtered,
	})
}

func (s *Server) handleHomeGeminiModels(c *gin.Context) {
	catalog, ok := s.loadHomeModelCatalog(c)
	if !ok {
		return
	}
	entries := homeModelEntries(catalog)

	models := s.handlers.FilterModelsForCurrentAPIKey(c, formatHomeGeminiModels(entries))
	s.handlers.WriteModelListResponse(c, constant.Gemini, gin.H{
		"models": models,
	})
}

func (s *Server) handleHomeGeminiModel(c *gin.Context) {
	catalog, ok := s.loadHomeModelCatalog(c)
	if !ok {
		return
	}
	entries := homeModelEntries(catalog)

	action := strings.TrimPrefix(c.Param("action"), "/")
	action = strings.TrimSpace(action)
	for _, entry := range entries {
		if homeGeminiModelMatches(entry, action) {
			model := formatHomeGeminiModel(entry)
			if len(s.handlers.FilterModelsForCurrentAPIKey(c, []map[string]any{model})) == 0 {
				break
			}
			c.JSON(http.StatusOK, model)
			return
		}
	}

	c.JSON(
		http.StatusNotFound, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Not Found",
				Type:    "not_found",
			},
		},
	)
}

func (s *Server) loadHomeModelCatalog(c *gin.Context) (*modelcatalog.HomeCatalog, bool) {
	if s == nil || c == nil || c.Request == nil {
		return nil, false
	}
	client := s.currentHomeClient()
	if client == nil {
		c.JSON(
			http.StatusServiceUnavailable, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: "home control center unavailable",
					Type:    "server_error",
				},
			},
		)
		return nil, false
	}

	raw, errGet := client.GetModels(c.Request.Context())
	if errGet != nil {
		c.JSON(
			http.StatusBadGateway, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: errGet.Error(),
					Type:    "server_error",
				},
			},
		)
		return nil, false
	}

	var homeError struct {
		Error *struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if errDecodeError := json.Unmarshal(raw, &homeError); errDecodeError == nil && homeError.Error != nil {
		status, message, errorType := http.StatusBadGateway, "home models request failed", "server_error"
		switch homeError.Error.Type {
		case "no_credentials":
			status, message, errorType = http.StatusUnauthorized, "Missing API key", "authentication_error"
		case "invalid_credential":
			status, message, errorType = http.StatusUnauthorized, "Invalid API key", "authentication_error"
		}
		c.JSON(status, handlers.ErrorResponse{Error: handlers.ErrorDetail{Message: message, Type: errorType}})
		return nil, false
	}

	catalog, errDecode := modelcatalog.DecodeHomeCatalog(raw)
	if errDecode != nil {
		c.JSON(
			http.StatusBadGateway, handlers.ErrorResponse{
				Error: handlers.ErrorDetail{
					Message: errDecode.Error(),
					Type:    "server_error",
				},
			},
		)
		return nil, false
	}

	return catalog, true
}

func formatHomeGeminiModels(entries []homeModelEntry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		out = append(out, formatHomeGeminiModel(entry))
	}
	return out
}

func formatHomeGeminiModel(entry homeModelEntry) map[string]any {
	name := entry.id
	if !strings.HasPrefix(name, "models/") {
		name = "models/" + name
	}
	displayName := entry.displayName
	if displayName == "" {
		displayName = entry.id
	}
	return map[string]any{
		"name":                       name,
		"displayName":                displayName,
		"description":                displayName,
		"supportedGenerationMethods": []string{"generateContent"},
	}
}

func homeGeminiModelMatches(entry homeModelEntry, action string) bool {
	id := strings.TrimSpace(entry.id)
	if id == "" || action == "" {
		return false
	}
	normalizedAction := strings.TrimPrefix(action, "models/")
	normalizedID := strings.TrimPrefix(id, "models/")
	return action == id || action == "models/"+id || normalizedAction == normalizedID
}

func homeModelEntries(catalog *modelcatalog.HomeCatalog) []homeModelEntry {
	if catalog == nil {
		return nil
	}
	models := catalog.Models()
	seen := make(map[string]struct{}, len(models))
	entries := make([]homeModelEntry, 0, len(models))
	for _, model := range models {
		id, _ := model["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}

		ownedBy, _ := model["owned_by"].(string)
		displayName, _ := model["display_name"].(string)
		if strings.TrimSpace(displayName) == "" {
			displayName, _ = model["displayName"].(string)
		}
		entries = append(entries, homeModelEntry{
			id:                  id,
			created:             modelMapInt64(model, "created"),
			ownedBy:             strings.TrimSpace(ownedBy),
			displayName:         strings.TrimSpace(displayName),
			contextLength:       modelMapInt(model, "max_context_length", "context_length"),
			maxCompletionTokens: modelMapInt(model, "max_completion_tokens"),
			reasoningLevels:     modelMapReasoningLevels(model),
		})
	}
	return entries
}

func modelMapInt64(model map[string]any, keys ...string) int64 {
	for _, key := range keys {
		var value int64
		switch typed := model[key].(type) {
		case int:
			value = int64(typed)
		case int64:
			value = typed
		case float64:
			value = int64(typed)
		case json.Number:
			if parsed, err := typed.Int64(); err == nil {
				value = parsed
			}
		}
		if value > 0 {
			return value
		}
	}
	return 0
}

// StartWithReady begins serving and notifies ready after the listener has been prepared.
func (s *Server) StartWithReady(ready chan<- struct{}) error {
	return s.start(ready)
}

func (s *Server) publishListeners(base net.Listener, httpListener *muxListener, redirect *http.Server) bool {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	if s.stopping.Load() {
		return false
	}
	s.muxBaseListener = base
	s.muxHTTPListener = httpListener
	s.redirectSrv = redirect
	return true
}

func (s *Server) listeners() (net.Listener, *muxListener, *http.Server) {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	return s.muxBaseListener, s.muxHTTPListener, s.redirectSrv
}

func (s *Server) start(ready chan<- struct{}) error {
	if s == nil || s.server == nil {
		return fmt.Errorf("failed to start HTTP server: server not initialized")
	}
	if s.stopping.Load() {
		return http.ErrServerClosed
	}

	listener, errListen := net.Listen("tcp", s.server.Addr)
	if errListen != nil {
		return fmt.Errorf("failed to start HTTP server: %v", errListen)
	}

	cfg := s.configSnapshot()
	useTLS := cfg != nil && cfg.TLS.Enable
	if !useTLS {
		log.Warnf(
			"security: TLS is disabled — API traffic (including Authorization headers) is transmitted in plaintext. " +
				"Enable tls.enable with certificate/key before exposing this server to the public network.",
		)
	}
	if cfg != nil && cfg.AllowQueryAuth {
		log.Warnf(
			"security: allow-query-auth is enabled — API keys may leak to access logs, Referer headers, and browser history. " +
				"Prefer Authorization header or x-api-key. This option remains for backward compatibility with scripted clients.",
		)
	}
	var redirectSrv *http.Server
	if useTLS {
		cert := strings.TrimSpace(cfg.TLS.Cert)
		key := strings.TrimSpace(cfg.TLS.Key)
		if cert == "" || key == "" {
			if errClose := listener.Close(); errClose != nil {
				log.Errorf("failed to close listener after TLS validation failure: %v", errClose)
			}
			return fmt.Errorf("failed to start HTTPS server: tls.cert or tls.key is empty")
		}
		certPair, errLoad := tls.LoadX509KeyPair(cert, key)
		if errLoad != nil {
			if errClose := listener.Close(); errClose != nil {
				log.Errorf("failed to close listener after TLS key pair load failure: %v", errClose)
			}
			return fmt.Errorf("failed to start HTTPS server: %v", errLoad)
		}

		// Start HTTP→HTTPS redirect listener if configured
		redirectPort := cfg.TLS.HTTPRedirectPort
		if redirectPort > 0 {
			httpsAddr := s.server.Addr
			redirectAddr := fmt.Sprintf(":%d", redirectPort)
			redirectSrv = &http.Server{
				Addr:         redirectAddr,
				ReadTimeout:  5 * time.Second,
				WriteTimeout: 10 * time.Second,
				IdleTimeout:  30 * time.Second,
				Handler: http.HandlerFunc(
					func(w http.ResponseWriter, r *http.Request) {
						target := "https://" + httpsAddr + r.URL.RequestURI()
						http.Redirect(w, r, target, http.StatusMovedPermanently)
					},
				),
			}
		}

		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{certPair},
			NextProtos:   []string{"h2", "http/1.1"},
		}
		s.server.TLSConfig = tlsConfig
		s.server.Protocols = new(http.Protocols)
		s.server.Protocols.SetHTTP1(true)
		s.server.Protocols.SetHTTP2(true)
		listener = tls.NewListener(listener, tlsConfig)
		log.Debugf("Starting API server on %s with TLS", s.server.Addr)
	} else {
		log.Debugf("Starting API server on %s", s.server.Addr)
	}

	httpListener := newMuxListener(listener.Addr(), 1024)
	if !s.publishListeners(listener, httpListener, redirectSrv) {
		_ = httpListener.Close()
		if errClose := listener.Close(); errClose != nil {
			log.Debugf("failed to close shared listener after shutdown: %v", errClose)
		}
		return http.ErrServerClosed
	}
	if redirectSrv != nil {
		go func() {
			log.Debugf("Starting HTTP→HTTPS redirect on %s → %s", redirectSrv.Addr, s.server.Addr)
			if err := redirectSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Warnf("HTTP redirect listener stopped: %v", err)
			}
		}()
	}

	httpErrCh := make(chan error, 1)
	acceptErrCh := make(chan error, 1)
	if ready != nil {
		ready <- struct{}{}
	}

	go func() {
		httpErrCh <- s.server.Serve(httpListener)
	}()
	go func() {
		acceptErrCh <- s.acceptMuxConnections(listener, httpListener)
	}()

	select {
	case errServe := <-httpErrCh:
		if errClose := listener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			log.Debugf("failed to close shared listener after HTTP serve exit: %v", errClose)
		}
		_ = httpListener.Close()
		errAccept := <-acceptErrCh
		errServe = normalizeHTTPServeError(errServe)
		errAccept = normalizeListenerError(errAccept)
		if errServe != nil {
			return fmt.Errorf("failed to start HTTP server: %v", errServe)
		}
		if errAccept != nil {
			return fmt.Errorf("failed to start HTTP server: %v", errAccept)
		}
		return nil
	case errAccept := <-acceptErrCh:
		_ = httpListener.Close()
		if errClose := listener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			log.Debugf("failed to close shared listener after accept loop exit: %v", errClose)
		}
		errServe := <-httpErrCh
		errServe = normalizeHTTPServeError(errServe)
		errAccept = normalizeListenerError(errAccept)
		if errAccept != nil {
			return fmt.Errorf("failed to start HTTP server: %v", errAccept)
		}
		if errServe != nil {
			return fmt.Errorf("failed to start HTTP server: %v", errServe)
		}
		return nil
	}
}

func (s *Server) beginConfigPublication() bool {
	if s == nil {
		return false
	}
	s.configPublicationMu.Lock()
	if s.stopping.Load() {
		s.configPublicationMu.Unlock()
		return false
	}
	if s.configPublicationCount == 0 {
		s.configPublicationIdle = make(chan struct{})
	}
	s.configPublicationCount++
	s.configPublicationMu.Unlock()

	s.configUpdateMu.Lock()
	if s.stopping.Load() {
		s.configUpdateMu.Unlock()
		s.finishConfigPublication()
		return false
	}
	return true
}

func (s *Server) finishConfigPublication() {
	s.configPublicationMu.Lock()
	s.configPublicationCount--
	if s.configPublicationCount == 0 {
		close(s.configPublicationIdle)
		s.configPublicationIdle = nil
	}
	s.configPublicationMu.Unlock()
}

func (s *Server) endConfigPublication() {
	s.configUpdateMu.Unlock()
	s.finishConfigPublication()
}

func (s *Server) WaitForConfigPublications(ctx context.Context) error {
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
		return fmt.Errorf("wait for config publications: %w", ctx.Err())
	}
}

// BeginShutdown prevents further runtime publication and closes active Redis connections.
func (s *Server) BeginShutdown() {
	if s == nil {
		return
	}
	s.configPublicationMu.Lock()
	s.stopping.Store(true)
	s.configPublicationMu.Unlock()
	s.managementRoutesEnabled.close()
	if s.usageQueue != nil {
		s.usageQueue.Close()
	}
	base, httpListener, _ := s.listeners()
	if httpListener != nil {
		_ = httpListener.Close()
	}
	if base != nil {
		if errClose := base.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			log.Debugf("failed to close shared listener: %v", errClose)
		}
	}
	s.closeRedisConnections()
}

func (s *Server) waitForRedisConnections(ctx context.Context) error {
	if s == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		s.redisConnectionsWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for Redis connections: %w", ctx.Err())
	}
}

// Stop gracefully shuts down the API server without interrupting any
// active connections.
//
// Parameters:
//   - ctx: The context for graceful shutdown
//
// Returns:
//   - error: An error if the server fails to stop
func (s *Server) Stop(ctx context.Context) error {
	log.Debug("Stopping API server...")
	if ctx == nil {
		ctx = context.Background()
	}
	var shutdownErrors []error
	s.BeginShutdown()
	if errWaitConfig := s.WaitForConfigPublications(ctx); errWaitConfig != nil {
		shutdownErrors = append(shutdownErrors, errWaitConfig)
	}
	if s.codexLiveHandler != nil {
		if errClose := s.codexLiveHandler.Close(); errClose != nil {
			shutdownErrors = append(shutdownErrors, fmt.Errorf("close Codex Live sessions: %w", errClose))
		}
	}

	if s.keepAliveEnabled {
		select {
		case s.keepAliveStop <- struct{}{}:
		default:
		}
	}

	if errWaitRedis := s.waitForRedisConnections(ctx); errWaitRedis != nil {
		shutdownErrors = append(shutdownErrors, errWaitRedis)
	}

	shutdownServer := func(server *http.Server, name string) error {
		if server == nil {
			return nil
		}
		if err := server.Shutdown(ctx); err != nil {
			shutdownErr := fmt.Errorf("failed to shutdown %s: %w", name, err)
			if ctx.Err() != nil {
				if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
					return errors.Join(shutdownErr, fmt.Errorf("close %s fallback: %w", name, closeErr))
				}
			}
			return shutdownErr
		}
		return nil
	}

	if err := shutdownServer(s.server, "HTTP server"); err != nil {
		shutdownErrors = append(shutdownErrors, err)
	}
	_, _, redirectSrv := s.listeners()
	if err := shutdownServer(redirectSrv, "redirect server"); err != nil {
		shutdownErrors = append(shutdownErrors, err)
	}

	if s.quotaScheduler != nil {
		s.quotaScheduler.Stop()
	}
	if s.imageArtifacts != nil {
		s.imageArtifacts.CloseCleaner()
	}
	if s.mgmt != nil {
		if err := s.mgmt.Close(); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Errorf("close management state: %w", err))
		}
	}

	if s.authRateLimiter != nil {
		s.authRateLimiter.Stop()
	}

	log.Debug("API server stopped")
	return errors.Join(shutdownErrors...)
}

// corsMiddleware returns a Gin middleware handler that adds CORS headers
// to every response, allowing cross-origin requests.
// When allowedOrigins is non-empty, only matching origins receive the
// Access-Control-Allow-Origin header (with Vary: Origin).
// When empty, all non-credentialed origins are allowed (*) for CLI tool compatibility.
//
// Returns:
//   - gin.HandlerFunc: The CORS middleware handler
func corsMiddleware(allowedOrigins []string) gin.HandlerFunc {
	// Build a set for O(1) lookup when the allowlist is configured.
	originSet := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		originSet[o] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		pluginResourceRequest := c.Request != nil && c.Request.URL != nil && strings.HasPrefix(
			c.Request.URL.Path, "/v0/resource/plugins/",
		)
		if len(originSet) == 0 {
			c.Header("Access-Control-Allow-Origin", "*")
		} else {
			if _, ok := originSet[origin]; ok {
				c.Header("Access-Control-Allow-Origin", origin)
				if pluginResourceRequest {
					c.Header("Access-Control-Allow-Credentials", "true")
				}
			}
			// Always set Vary when the response depends on the request Origin.
			c.Header("Vary", "Origin")
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if requestedHeaders := c.GetHeader("Access-Control-Request-Headers"); requestedHeaders != "" {
			c.Header("Access-Control-Allow-Headers", requestedHeaders)
		} else {
			c.Header("Access-Control-Allow-Headers", "*")
		}
		exposedHeaders := logging.CPATraceIDHeader +
			", X-Cpa-Version, X-Cpa-Build-Date, X-Cpa-Commit, X-CPA-SUPPORT-PLUGIN, ETag"
		if c.Request != nil && c.Request.URL != nil && strings.HasPrefix(c.Request.URL.Path, "/v1/realtime") {
			exposedHeaders += ", Location, Retry-After, X-Request-Id, OpenAI-Request-Id"
		}
		c.Header("Access-Control-Expose-Headers", exposedHeaders)
		c.Header("X-Cpa-Version", buildinfo.Version)
		c.Header("X-Cpa-Build-Date", buildinfo.BuildDate)
		c.Header("X-Cpa-Commit", buildinfo.Commit)

		// Security headers
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "SAMEORIGIN")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Cache-Control", "no-cache")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

func (s *Server) applyAccessConfig(oldCfg, newCfg *config.Config) bool {
	if s == nil || s.accessManager == nil || newCfg == nil {
		return false
	}
	if _, err := access.ApplyAccessProviders(s.accessManager, oldCfg, newCfg); err != nil {
		return false
	}
	return true
}

func (s *Server) configSnapshot() *config.Config {
	if s == nil {
		return nil
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Server) managementConfigSnapshot(c *gin.Context) *config.Config {
	if c != nil {
		if value, exists := c.Get(managementConfigSnapshotContextKey); exists {
			if snapshot, ok := value.(*config.Config); ok {
				return snapshot
			}
		}
	}
	if s == nil || s.mgmt == nil {
		return nil
	}
	snapshot := s.mgmt.ConfigState().Config
	if c != nil && snapshot != nil {
		c.Set(managementConfigSnapshotContextKey, snapshot)
	}
	return snapshot
}

func (s *Server) setConfig(cfg *config.Config) {
	if s == nil {
		return
	}
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgMu.Unlock()
}

// UpdateClients updates the server's client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (s *Server) UpdateClients(cfg *config.Config) {
	s.updateClients(context.Background(), cfg, 0)
}

func (s *Server) UpdateClientsForConfigCommit(ctx context.Context, cfg *config.Config, generation uint64) {
	s.updateClients(ctx, cfg, generation)
}

func (s *Server) updateClients(ctx context.Context, cfg *config.Config, _ uint64) {
	if s == nil || cfg == nil {
		return
	}
	if !s.beginConfigPublication() {
		return
	}
	defer s.endConfigPublication()

	// Reconstruct old config from YAML snapshot to avoid reference sharing issues
	var oldCfg *config.Config
	if len(s.oldConfigYaml) > 0 {
		_ = yaml.Unmarshal(s.oldConfigYaml, &oldCfg)
	}
	if oldCfg != nil && !reflect.DeepEqual(oldCfg.Antigravity.ConnectionPool, cfg.Antigravity.ConnectionPool) {
		executor.ResetAntigravityTransports()
	}

	// Update request logger enabled state if it has changed
	previousRequestLog := false
	if oldCfg != nil {
		previousRequestLog = oldCfg.RequestLog && !oldCfg.CommercialMode
	}
	currentRequestLog := cfg.RequestLog && !cfg.CommercialMode
	runtimeEnabled := !cfg.CommercialMode
	if s.requestLoggingRuntimeEnabled != nil && !runtimeEnabled {
		s.requestLoggingRuntimeEnabled.Store(false)
	}
	if s.requestLogger != nil && (oldCfg == nil || previousRequestLog != currentRequestLog) {
		if s.loggerToggle != nil {
			s.loggerToggle(currentRequestLog)
		} else if toggler, ok := s.requestLogger.(interface{ SetEnabled(bool) }); ok {
			toggler.SetEnabled(currentRequestLog)
		}
	}
	if s.requestLoggingRuntimeEnabled != nil && runtimeEnabled {
		s.requestLoggingRuntimeEnabled.Store(true)
	}

	if s.requestLogger != nil && (oldCfg == nil || oldCfg.Home.Enabled != cfg.Home.Enabled) {
		if setter, ok := s.requestLogger.(interface{ SetHomeEnabled(bool) }); ok {
			setter.SetHomeEnabled(cfg.Home.Enabled)
		}
	}

	if oldCfg == nil || oldCfg.LoggingToFile != cfg.LoggingToFile ||
		oldCfg.LogsMaxTotalSizeMB != cfg.LogsMaxTotalSizeMB {
		if err := logging.ConfigureLogOutput(cfg); err != nil {
			log.Errorf("failed to reconfigure log output: %v", err)
		}
	}

	if s.imageArtifacts != nil && (oldCfg == nil || oldCfg.ProxyURL != cfg.ProxyURL) {
		s.imageArtifacts.SetProxyURL(cfg.ProxyURL)
	}
	if s.imageArtifacts != nil && (oldCfg == nil ||
		oldCfg.ImageArtifactCache.RetentionDays != cfg.ImageArtifactCache.RetentionDays ||
		oldCfg.ImageArtifactCache.MaxTotalSizeMB != cfg.ImageArtifactCache.MaxTotalSizeMB) {
		s.imageArtifacts.ApplyCacheConfig(
			artifact.CacheConfigFromLimits(
				cfg.ImageArtifactCache.RetentionDays,
				cfg.ImageArtifactCache.MaxTotalSizeMB,
			),
		)
		if ctx == nil {
			ctx = context.Background()
		}
		if _, err := s.imageArtifacts.Cleanup(context.WithoutCancel(ctx)); err != nil {
			log.WithError(err).Warn("image artifact cache cleanup failed")
			managementHandlers.ReportConfigCommitCleanupError(ctx, err)
		}
		s.imageArtifacts.StartCleaner()
	}

	if oldCfg == nil || oldCfg.UsageStatisticsEnabled != cfg.UsageStatisticsEnabled {
		if s.usageStatisticsToggle != nil {
			s.usageStatisticsToggle(cfg.UsageStatisticsEnabled)
		} else {
			usage.SetStatisticsEnabled(cfg.UsageStatisticsEnabled)
		}
		s.usageQueue.SetUsageStatisticsEnabled(cfg.UsageStatisticsEnabled)
	}

	if oldCfg == nil || oldCfg.RedisUsageQueueRetentionSeconds != cfg.RedisUsageQueueRetentionSeconds {
		s.usageQueue.SetRetentionSeconds(cfg.RedisUsageQueueRetentionSeconds)
	}

	if s.requestLogger != nil && (oldCfg == nil || oldCfg.ErrorLogsMaxFiles != cfg.ErrorLogsMaxFiles) {
		if setter, ok := s.requestLogger.(interface{ SetErrorLogsMaxFiles(int) }); ok {
			setter.SetErrorLogsMaxFiles(cfg.ErrorLogsMaxFiles)
		}
	}

	if oldCfg == nil || oldCfg.DisableCooling != cfg.DisableCooling {
		auth.SetQuotaCooldownDisabled(cfg.DisableCooling)
	}
	if oldCfg == nil || oldCfg.TransientErrorCooldownSeconds != cfg.TransientErrorCooldownSeconds {
		auth.SetTransientErrorCooldownSeconds(cfg.TransientErrorCooldownSeconds)
	}

	if s.handlers != nil && s.handlers.AuthManager != nil {
		s.handlers.AuthManager.SetRetryConfig(
			cfg.RequestRetry, time.Duration(cfg.MaxRetryInterval)*time.Second, cfg.MaxRetryCredentials,
		)
	}

	// Update log level dynamically when debug flag changes
	if oldCfg == nil || oldCfg.Debug != cfg.Debug {
		util.SetLogLevel(cfg)
	}

	hasManagementSecret := s.envManagementSecret ||
		strings.TrimSpace(cfg.RemoteManagement.SecretKey) != "" ||
		strings.TrimSpace(s.localPassword) != ""
	wasManagementEnabled := s.managementRoutesEnabled.load()
	if hasManagementSecret {
		s.registerManagementRoutes()
		s.managementRoutesEnabled.store(true)
		if !wasManagementEnabled && s.managementRoutesEnabled.load() {
			log.Info("management routes enabled after credential update")
		}
	} else {
		s.managementRoutesEnabled.store(false)
		if wasManagementEnabled {
			log.Info("management routes disabled after credential removal")
		}
	}
	if s.stopping.Load() {
		s.usageQueue.SetEnabled(false)
	} else {
		s.usageQueue.SetEnabled(s.managementRoutesEnabled.load() || cfg.Home.Enabled)
	}

	exampleAPIKeySafeModeRequired := s.exampleAPIKeySafeModeRequired(cfg)
	s.authPolicyMu.Lock()
	if exampleAPIKeySafeModeRequired {
		s.exampleAPIKeySafeModeActive.Store(true)
	}
	accessConfigApplied := s.applyAccessConfig(oldCfg, cfg)
	if accessConfigApplied || exampleAPIKeySafeModeRequired {
		s.exampleAPIKeySafeModeActive.Store(exampleAPIKeySafeModeRequired)
	}
	if s.codexLiveHandler != nil {
		if errUpdate := s.codexLiveHandler.UpdateConfig(cfg); errUpdate != nil {
			log.WithError(errUpdate).Error("failed to update Codex Live media relay configuration")
		}
	}
	s.setConfig(cfg)
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
	s.authPolicyMu.Unlock()
	if oldCfg != nil && s.wsAuthChanged != nil && oldCfg.WebsocketAuth != cfg.WebsocketAuth {
		s.wsAuthChanged(oldCfg.WebsocketAuth, cfg.WebsocketAuth)
	}
	managementasset.SetCurrentConfig(cfg)
	// Save YAML snapshot for next comparison
	s.oldConfigYaml, _ = yaml.Marshal(cfg)

	s.handlers.UpdateClients(effectiveSDKConfig(cfg))

	if s.mgmt != nil {
		s.mgmt.SetAuthManager(s.handlers.AuthManager)
	}
	s.refreshPluginManagementRoutes()

	// Count client sources from configuration and auth store.
	tokenStore := sdkAuth.GetTokenStore()
	if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(cfg.AuthDir)
	}
	authEntries := util.CountAuthFiles(context.Background(), tokenStore)
	geminiAPIKeyCount := len(cfg.GeminiKey) + len(cfg.InteractionsKey)
	claudeAPIKeyCount := len(cfg.ClaudeKey)
	codexAPIKeyCount := len(cfg.CodexKey)
	xaiAPIKeyCount := len(cfg.XAIKey)
	metaAPIKeyCount := len(cfg.MetaKey)
	vertexAICompatCount := len(cfg.VertexCompatAPIKey)
	openAICompatCount := 0
	for i := range cfg.OpenAICompatibility {
		entry := cfg.OpenAICompatibility[i]
		if entry.Disabled {
			continue
		}
		openAICompatCount += len(entry.APIKeyEntries)
	}

	total := authEntries + geminiAPIKeyCount + claudeAPIKeyCount + codexAPIKeyCount + xaiAPIKeyCount + metaAPIKeyCount + vertexAICompatCount + openAICompatCount
	fmt.Printf(
		"server clients and configuration updated: %d clients (%d auth entries + %d Gemini API keys + %d Claude API keys + %d Codex keys + %d xAI keys + %d Meta API keys + %d Vertex-compat + %d OpenAI-compat)\n",
		total,
		authEntries,
		geminiAPIKeyCount,
		claudeAPIKeyCount,
		codexAPIKeyCount,
		xaiAPIKeyCount,
		metaAPIKeyCount,
		vertexAICompatCount,
		openAICompatCount,
	)
}

func (s *Server) SetWebsocketAuthChangeHandler(fn func(bool, bool)) {
	if s == nil {
		return
	}
	s.wsAuthChanged = fn
}

// RefreshModelClients reapplies current model-derived handler state without
// publishing a configuration generation.
func (s *Server) RefreshModelClients(cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	if !s.beginConfigPublication() {
		return
	}
	defer s.endConfigPublication()
	s.handlers.UpdateClients(effectiveSDKConfig(cfg))
}

// SetUsagePersister injects usage storage into management reads and request-log lifecycle writes.
func (s *Server) SetUsagePersister(p *usage.Persister) {
	if s == nil {
		return
	}
	if s.mgmt != nil {
		s.mgmt.SetUsagePersister(p)
	}
	if s.handlers != nil {
		s.handlers.SetAPIKeyPolicyStore(p)
	}
	if setter, ok := s.requestLogger.(interface {
		SetRequestLogArtifactSink(logging.RequestLogArtifactSink)
	}); ok {
		var sink logging.RequestLogArtifactSink
		if p != nil {
			sink = p
		}
		setter.SetRequestLogArtifactSink(sink)
	}
}

// SetUsageStatistics injects the service-owned in-memory usage statistics.
func (s *Server) SetUsageStatistics(stats *usage.RequestStatistics) {
	if s == nil || s.mgmt == nil {
		return
	}
	s.mgmt.SetUsageStatistics(stats)
}

// ManagementHandler exposes the management handler for lifecycle wiring.
func (s *Server) ManagementHandler() *managementHandlers.Handler {
	if s == nil {
		return nil
	}
	return s.mgmt
}

// (management handlers moved to internal/api/handlers/management)

// extractAuthCandidate returns the first non-empty credential found on the request.
// It mirrors the detection order of the config-access provider so the rate limiter
// can attribute failures to the credential the client actually offered.
// readQuery must match the current allow-query-auth config: otherwise an attacker could
// poison the per-account accountHash by forging ?key=<victim> and lock out victim's header-authed requests.
func extractAuthCandidate(r *http.Request, readQuery bool) string {
	if v := strings.TrimSpace(r.Header.Get("Authorization")); v != "" {
		if lower := strings.ToLower(v); strings.HasPrefix(lower, "bearer ") {
			return strings.TrimSpace(v[len("Bearer "):])
		}
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Goog-Api-Key")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Api-Key")); v != "" {
		return v
	}
	if !readQuery {
		return ""
	}
	q := r.URL.Query()
	if v := strings.TrimSpace(q.Get("key")); v != "" {
		return v
	}
	if v := strings.TrimSpace(q.Get("auth_token")); v != "" {
		return v
	}
	return ""
}

// authMiddleware creates an HTTP middleware that authenticates requests using
// the access manager. When no auth providers are registered (api-keys empty),
// only direct loopback clients are allowed (prevents open-proxy exposure).
// It enforces rate limiting on authentication failures along two axes:
//   - per client IP: mitigates a single host brute-forcing
//   - per account identifier (derived from the offered credential): mitigates
//     distributed attacks that rotate IPs against one account
func authMiddleware(
	manager *sdkaccess.Manager,
	rateLimiter *access.AuthRateLimiter,
	configSnapshot func() *config.Config,
) gin.HandlerFunc {
	return authMiddlewareWithErrorStyle(manager, rateLimiter, configSnapshot, false)
}

func realtimeStandardAuthMiddleware(
	manager *sdkaccess.Manager,
	rateLimiter *access.AuthRateLimiter,
	configSnapshot func() *config.Config,
) gin.HandlerFunc {
	return authMiddlewareWithErrorStyle(manager, rateLimiter, configSnapshot, true)
}

func authMiddlewareWithErrorStyle(
	manager *sdkaccess.Manager,
	rateLimiter *access.AuthRateLimiter,
	configSnapshot func() *config.Config,
	realtimeError bool,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")

		var cfg *config.Config
		if configSnapshot != nil {
			cfg = configSnapshot()
		}
		var trustedProxies []string
		if cfg != nil {
			trustedProxies = cfg.RemoteManagement.TrustedProxies
		}
		client, resolved := access.ClientAddressFromContext(c.Request.Context())
		if !resolved {
			client = access.ResolveClientAddress(c.Request, trustedProxies)
			c.Request = c.Request.WithContext(access.WithClientAddress(c.Request.Context(), client))
		}

		authorizeDirectLoopback := func() {
			if c.GetBool(realtimeInvalidClientSecretContextKey) {
				abortInvalidRealtimeClientSecret(c, "Realtime client secret is invalid or expired")
				return
			}
			if client.DirectLoopback {
				c.Next()
				return
			}
			abortAuthentication(c, http.StatusUnauthorized, "Unauthorized", realtimeError)
		}
		if !authManagerHasProviders(manager) {
			authorizeDirectLoopback()
			return
		}

		clientKey := client.Key
		if rateLimiter != nil && rateLimiter.IsLimited(clientKey) {
			abortAuthentication(
				c,
				http.StatusTooManyRequests,
				"too many authentication failures, please try again later",
				realtimeError,
			)
			return
		}

		authRequest := c.Request
		if value, exists := c.Get(realtimeAuthRequestContextKey); exists {
			if request, ok := value.(*http.Request); ok {
				authRequest = request.WithContext(c.Request.Context())
			}
		}
		// Derive an account identifier from the raw credential candidate so repeated failures
		// against a single account can be locked out even when the attacker rotates source IPs.
		queryAuthAllowed := cfg != nil && cfg.AllowQueryAuth
		accountHash := access.HashAccountKey(extractAuthCandidate(authRequest, queryAuthAllowed))
		if rateLimiter != nil && rateLimiter.IsAccountLimited(accountHash) {
			abortAuthentication(
				c,
				http.StatusTooManyRequests,
				"too many authentication failures for this credential, please try again later",
				realtimeError,
			)
			return
		}

		result, generation, err := manager.AuthenticateWithGeneration(authRequest.Context(), authRequest)
		if err == nil {
			if result == nil {
				authorizeDirectLoopback()
				return
			}
			c.Set("apiKey", result.Principal)
			c.Set(codexlive.ClientSecretIssuerGenerationContextKey, generation)
			c.Request = c.Request.WithContext(
				coreusage.WithAPIKeyIdentity(
					c.Request.Context(),
					coreusage.HashAPIKeyIdentity(result.Principal),
				),
			)
			c.Set("accessProvider", result.Provider)
			if len(result.Metadata) > 0 {
				c.Set("accessMetadata", result.Metadata)
			}
			if rateLimiter != nil {
				rateLimiter.RecordSuccess(clientKey)
				rateLimiter.RecordAccountSuccess(accountHash)
			}
			c.Next()
			return
		}

		if rateLimiter != nil && (sdkaccess.IsAuthErrorCode(err, sdkaccess.AuthErrorCodeNoCredentials) ||
			sdkaccess.IsAuthErrorCode(err, sdkaccess.AuthErrorCodeInvalidCredential)) {
			rateLimiter.RecordFailure(clientKey)
			rateLimiter.RecordAccountFailure(accountHash)
		}

		statusCode := err.HTTPStatusCode()
		message := "Unauthorized"
		if statusCode >= http.StatusInternalServerError {
			log.Errorf("authentication middleware error: %v", err)
			message = "Authentication service error"
		}
		if statusCode == http.StatusUnauthorized && c.GetBool(realtimeInvalidClientSecretContextKey) {
			abortInvalidRealtimeClientSecret(c, "Realtime client secret is invalid or expired")
			return
		}
		abortAuthentication(c, statusCode, message, realtimeError)
	}
}

func abortAuthentication(c *gin.Context, status int, message string, realtimeError bool) {
	if !realtimeError {
		c.AbortWithStatusJSON(status, gin.H{"error": message})
		return
	}
	errorType := "authentication_error"
	code := "invalid_api_key"
	switch {
	case status == http.StatusTooManyRequests:
		errorType = "rate_limit_error"
		code = "authentication_rate_limited"
	case status >= http.StatusInternalServerError:
		errorType = "server_error"
		code = "authentication_service_error"
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"message": message,
		"type":    errorType,
		"param":   nil,
		"code":    code,
	}})
}

func authManagerHasProviders(manager *sdkaccess.Manager) bool {
	if manager == nil {
		return false
	}
	for _, provider := range manager.Providers() {
		if provider != nil {
			return true
		}
	}
	return false
}
