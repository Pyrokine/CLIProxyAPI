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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/access"
	managementHandlers "github.com/Pyrokine/CLIProxyAPI/v6/internal/api/handlers/management"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/api/middleware"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/api/modules"
	ampmodule "github.com/Pyrokine/CLIProxyAPI/v6/internal/api/modules/amp"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/managementasset"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/gemini"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/openai"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"gopkg.in/yaml.v3"
)

const oauthCallbackSuccessHTML = `<html><head><meta charset="utf-8"><title>Authentication successful</title><script>setTimeout(function(){window.close();},5000);</script></head><body><h1>Authentication successful!</h1><p>You can close this window.</p><p>This window will close automatically in 5 seconds.</p></body></html>`

const (
	pluginResourceSessionCookie      = "cpa_plugin_resource_session"
	pluginResourceSessionTTL         = 5 * time.Minute
	pluginResourceSessionAbsoluteTTL = 30 * time.Minute
)

type pluginResourceSession struct {
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
}

type serverOptionConfig struct {
	extraMiddleware         []gin.HandlerFunc
	engineConfigurator      func(*gin.Engine)
	routerConfigurator      func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)
	requestLoggerFactory    func(*config.Config, string) logging.RequestLogger
	localPassword           string
	keepAliveEnabled        bool
	keepAliveTimeout        time.Duration
	keepAliveOnTimeout      func()
	postAuthHook            auth.PostAuthHook
	pluginHost              *pluginhost.Host
	modelCatalogRuntimeSync func(*config.Config)
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

// WithPluginHost wires dynamic plugin management endpoints to the plugin host.
func WithPluginHost(host *pluginhost.Host) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.pluginHost = host
	}
}

func WithModelCatalogRuntimeSync(sync func(*config.Config)) ServerOption {
	return func(cfg *serverOptionConfig) {
		cfg.modelCatalogRuntimeSync = sync
	}
}

// Server represents the main API server.
// It encapsulates the Gin engine, HTTP server, handlers, and configuration.
type Server struct {
	// engine is the Gin web framework engine instance.
	engine *gin.Engine

	// server is the underlying HTTP server.
	server *http.Server

	// muxBaseListener is the shared TCP listener used to serve both HTTP and Redis protocol traffic.
	muxBaseListener net.Listener

	// muxHTTPListener receives HTTP connections selected by the multiplexer.
	muxHTTPListener *muxListener

	// handlers contains the API handlers for processing requests.
	handlers *handlers.BaseAPIHandler

	// cfg holds the current server configuration.
	cfg *config.Config

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

	quotaScheduler *quota.Scheduler

	// ampModule is the Amp routing module for model mapping hot-reload
	ampModule *ampmodule.Module

	// pluginHost owns dynamic plugin Management API route dispatch.
	pluginHost *pluginhost.Host

	pluginResourceSessionMu     sync.Mutex
	pluginResourceSessionTokens map[string]pluginResourceSession

	// managementRoutesRegistered tracks whether the management routes have been attached to the engine.
	managementRoutesRegistered atomic.Bool
	// managementRoutesEnabled controls whether management endpoints serve real handlers.
	managementRoutesEnabled atomic.Bool

	// envManagementSecret indicates whether MANAGEMENT_PASSWORD is configured.
	envManagementSecret bool

	localPassword string

	keepAliveEnabled   bool
	keepAliveTimeout   time.Duration
	keepAliveOnTimeout func()
	keepAliveHeartbeat chan struct{}
	keepAliveStop      chan struct{}

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
	// Set gin mode
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}

	// Create gin engine
	engine := gin.New()
	if optionState.engineConfigurator != nil {
		optionState.engineConfigurator(engine)
	}

	// Add middleware
	engine.Use(logging.GinLogrusLogger())
	engine.Use(logging.GinLogrusRecovery())
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
	if optionState.pluginHost != nil {
		baseHandlers.SetPluginHost(optionState.pluginHost)
		optionState.pluginHost.SetModelExecutor(baseHandlers)
	}
	s := &Server{
		engine:                       engine,
		handlers:                     baseHandlers,
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
		pluginHost:                   optionState.pluginHost,
		pluginResourceSessionTokens:  make(map[string]pluginResourceSession),
	}
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
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
	// Initialize management handler first (to get internalToken for quota scheduler)
	s.mgmt = managementHandlers.NewHandler(
		cfg,
		configFilePath,
		authManager,
		managementHandlers.WithPluginHost(optionState.pluginHost),
		managementHandlers.WithPluginRuntimeSync(
			func(ctx context.Context, nextCfg *config.Config) {
				if optionState.pluginHost == nil {
					return
				}
				optionState.pluginHost.ApplyConfig(ctx, nextCfg)
				s.refreshPluginManagementRoutes()
			},
		),
		managementHandlers.WithModelCatalogRuntimeSync(
			func(nextCfg *config.Config) {
				if nextCfg != nil {
					s.cfg = nextCfg
				}
				if optionState.modelCatalogRuntimeSync != nil {
					optionState.modelCatalogRuntimeSync(nextCfg)
				}
				s.handlers.UpdateClients(effectiveSDKConfig(s.cfg))
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
	localAPICall := quota.NewLocalAPICallFunc(cfg.Port, s.mgmt.InternalToken())

	quotaScheduler := quota.NewScheduler(
		quotaCfg, func(entry *quota.Entry) ([]byte, error) {
			if entry.AuthIndex == "" {
				return nil, fmt.Errorf("no auth_index for %s", entry.FileName)
			}

			switch entry.Type {
			case quota.TypeClaude:
				return fetchClaudeQuota(localAPICall, entry.AuthIndex)
			case quota.TypeCodex:
				return fetchCodexQuota(localAPICall, entry.AuthIndex)
			case quota.TypeGeminiCli:
				return fetchGeminiQuota(localAPICall, entry.AuthIndex)
			case quota.TypeAntigravity:
				return fetchAntigravityQuota(localAPICall, entry.AuthIndex)
			case quota.TypeKimi:
				return fetchKimiQuota(localAPICall, entry.AuthIndex)
			case quota.TypeXAI:
				return fetchXAIQuota(localAPICall, entry.AuthIndex)
			default:
				return nil, fmt.Errorf("quota fetch not implemented for type %s", entry.Type)
			}
		},
	)
	if authManager != nil {
		registerAuthManagerForQuota(quotaScheduler, authManager)
	} else if cfg.AuthDir != "" {
		registerAuthFilesForQuota(quotaScheduler, cfg.AuthDir)
	}
	s.mgmt.SetQuotaScheduler(quotaScheduler)
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
	s.localPassword = optionState.localPassword

	engine.Use(s.homeHeartbeatMiddleware())

	// Setup routes
	s.setupRoutes()

	// Register Amp module using V2 interface with Context
	s.ampModule = ampmodule.New(
		ampmodule.WithAccessManager(accessManager),
		ampmodule.WithAuthMiddleware(authMiddleware(accessManager, authRateLimiter, cfg.AllowQueryAuth)),
	)
	ctx := modules.Context{
		Engine:         engine,
		BaseHandler:    s.handlers,
		Config:         cfg,
		AuthMiddleware: authMiddleware(accessManager, authRateLimiter, cfg.AllowQueryAuth),
	}
	if err := modules.RegisterModule(ctx, s.ampModule); err != nil {
		log.Errorf("Failed to register Amp module: %v", err)
	}

	// Apply additional router configurators from options
	if optionState.routerConfigurator != nil {
		optionState.routerConfigurator(engine, s.handlers, cfg)
	}

	// Register management routes when configuration or environment secrets are available,
	// or when a local management password is provided (e.g. TUI mode).
	hasManagementSecret := cfg.RemoteManagement.SecretKey != "" || envManagementSecret || s.localPassword != ""
	s.managementRoutesEnabled.Store(hasManagementSecret)
	redisqueue.SetEnabled(hasManagementSecret || (cfg != nil && cfg.Home.Enabled))
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
		IdleTimeout:       120 * time.Second,
	}

	return s
}

func (s *Server) homeHeartbeatMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s == nil || s.cfg == nil || !s.cfg.Home.Enabled {
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
		client := home.Current()
		if client == nil || !client.HeartbeatOK() {
			c.AbortWithStatus(http.StatusServiceUnavailable)
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
	v1.Use(requireTLSForAuth(s.cfg), noStoreMiddleware())
	v1.Use(authMiddleware(s.accessManager, s.authRateLimiter, s.cfg.AllowQueryAuth))
	{
		v1.GET("/models", s.unifiedModelsHandler(openaiHandlers, claudeCodeHandlers))
		v1.POST("/chat/completions", openaiHandlers.ChatCompletions)
		v1.POST("/completions", openaiHandlers.Completions)
		v1.POST("/messages", claudeCodeHandlers.ClaudeMessages)
		v1.POST("/messages/count_tokens", claudeCodeHandlers.ClaudeCountTokens)
		v1.GET("/responses", openaiResponsesHandlers.ResponsesWebsocket)
		v1.POST("/responses", openaiResponsesHandlers.Responses)
		v1.POST("/responses/compact", openaiResponsesHandlers.Compact)
		v1.POST("/images/generations", openaiHandlers.ImagesGenerations)
		v1.POST("/images/edits", openaiHandlers.ImagesEdits)
		v1.POST("/videos", openaiHandlers.VideosCreate)
		v1.POST("/videos/generations", openaiHandlers.XAIVideosGenerations)
		v1.POST("/videos/edits", openaiHandlers.XAIVideosEdits)
		v1.POST("/videos/extensions", openaiHandlers.XAIVideosExtensions)
		v1.GET("/videos/:video_id", openaiHandlers.VideosRetrieve)
	}

	// Root-level Responses API routes (no /v1 prefix).
	// Codex Desktop configures base_url without /v1 and appends /responses directly.
	rootResponses := s.engine.Group("")
	rootResponses.Use(requireTLSForAuth(s.cfg), noStoreMiddleware())
	rootResponses.Use(authMiddleware(s.accessManager, s.authRateLimiter, s.cfg.AllowQueryAuth))
	{
		rootResponses.GET("/responses", openaiResponsesHandlers.ResponsesWebsocket)
		rootResponses.POST("/responses", openaiResponsesHandlers.Responses)
		rootResponses.POST("/responses/compact", openaiResponsesHandlers.Compact)
	}

	// Codex Direct routes: /backend-api/codex/responses
	// Used when Codex Desktop sets chatgpt_base_url to point at this server.
	codexDirect := s.engine.Group("/backend-api/codex")
	codexDirect.Use(requireTLSForAuth(s.cfg), noStoreMiddleware())
	codexDirect.Use(authMiddleware(s.accessManager, s.authRateLimiter, s.cfg.AllowQueryAuth))
	{
		codexDirect.GET("/responses", openaiResponsesHandlers.ResponsesWebsocket)
		codexDirect.POST("/responses", openaiResponsesHandlers.Responses)
		codexDirect.POST("/responses/compact", openaiResponsesHandlers.Compact)
	}

	// Gemini compatible API routes
	v1beta := s.engine.Group("/v1beta")
	v1beta.Use(requireTLSForAuth(s.cfg), noStoreMiddleware())
	v1beta.Use(authMiddleware(s.accessManager, s.authRateLimiter, s.cfg.AllowQueryAuth))
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
	s.engine.POST(
		"/v1internal:method",
		requireTLSForAuth(s.cfg),
		authMiddleware(s.accessManager, s.authRateLimiter, s.cfg.AllowQueryAuth),
		geminiCLIHandlers.CLIHandler,
	)

	// OAuth callback endpoints (reuse main server port)
	// These endpoints receive provider redirects and persist
	// the short-lived code/state for the waiting goroutine.
	for _, cb := range []struct{ path, provider string }{
		{"/anthropic/callback", "anthropic"},
		{"/codex/callback", "codex"},
		{"/google/callback", "gemini"},
		{"/iflow/callback", "iflow"},
		{"/antigravity/callback", "antigravity"},
		{"/xai/callback", "xai"},
	} {
		provider := cb.provider
		s.engine.GET(
			cb.path, noStoreMiddleware(), func(c *gin.Context) {
				code := c.Query("code")
				state := c.Query("state")
				errStr := c.Query("error")
				if errStr == "" {
					errStr = c.Query("error_description")
				}
				if state != "" {
					_, _ = managementHandlers.WriteOAuthCallbackFileForPendingSession(
						s.cfg.AuthDir, provider, state, code, errStr,
					)
				}
				c.Header("Content-Type", "text/html; charset=utf-8")
				c.String(http.StatusOK, oauthCallbackSuccessHTML)
			},
		)
	}

	// Management routes are registered lazily by registerManagementRoutes when a secret is configured.
}

// AttachWebsocketRoute registers a websocket upgrade handler on the primary Gin engine.
// The handler is served as-is without additional middleware beyond the standard stack already configured.
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

	authMiddleware := authMiddleware(s.accessManager, s.authRateLimiter, s.cfg.AllowQueryAuth)
	conditionalAuth := func(c *gin.Context) {
		if !s.wsAuthEnabled.Load() {
			c.Next()
			return
		}
		authMiddleware(c)
	}
	finalHandler := func(c *gin.Context) {
		handler.ServeHTTP(c.Writer, c.Request)
		c.Abort()
	}

	s.engine.GET(trimmed, conditionalAuth, finalHandler)
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
		requireTLSForAuth(s.cfg), noStoreMiddleware(), s.managementAvailabilityMiddleware(),
		managementBodyLimitMiddleware(), s.mgmt.Middleware(),
	)
	{
		mgmt.GET("/usage", s.mgmt.GetUsageStatistics)
		mgmt.GET("/usage-queue", s.mgmt.GetUsageQueue)
		mgmt.GET("/usage/summary", s.mgmt.GetUsageSummary)
		mgmt.GET("/usage/events", s.mgmt.GetUsageEvents)
		mgmt.GET("/usage/export", s.mgmt.ExportUsageStatistics)
		mgmt.POST("/usage/import", s.mgmt.ImportUsageStatistics)
		mgmt.POST("/usage/recalculate-costs", s.mgmt.RecalculateCosts)
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

		mgmt.GET("/quota/status", s.mgmt.GetQuotaStatus)
		mgmt.POST("/quota/refresh", s.mgmt.PostQuotaRefresh)
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
		mgmt.PATCH("/gemini-api-key", s.mgmt.PatchGeminiKey)
		mgmt.DELETE("/gemini-api-key", s.mgmt.DeleteGeminiKey)

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

		mgmt.GET("/ampcode", s.mgmt.GetAmpCode)
		mgmt.GET("/ampcode/upstream-url", s.mgmt.GetAmpUpstreamURL)
		mgmt.PUT("/ampcode/upstream-url", s.mgmt.PutAmpUpstreamURL)
		mgmt.PATCH("/ampcode/upstream-url", s.mgmt.PutAmpUpstreamURL)
		mgmt.DELETE("/ampcode/upstream-url", s.mgmt.DeleteAmpUpstreamURL)
		mgmt.GET("/ampcode/upstream-api-key", s.mgmt.GetAmpUpstreamAPIKey)
		mgmt.PUT("/ampcode/upstream-api-key", s.mgmt.PutAmpUpstreamAPIKey)
		mgmt.PATCH("/ampcode/upstream-api-key", s.mgmt.PutAmpUpstreamAPIKey)
		mgmt.DELETE("/ampcode/upstream-api-key", s.mgmt.DeleteAmpUpstreamAPIKey)
		mgmt.GET("/ampcode/restrict-management-to-localhost", s.mgmt.GetAmpRestrictManagementToLocalhost)
		mgmt.PUT("/ampcode/restrict-management-to-localhost", s.mgmt.PutAmpRestrictManagementToLocalhost)
		mgmt.PATCH("/ampcode/restrict-management-to-localhost", s.mgmt.PutAmpRestrictManagementToLocalhost)
		mgmt.GET("/ampcode/model-mappings", s.mgmt.GetAmpModelMappings)
		mgmt.PUT("/ampcode/model-mappings", s.mgmt.PutAmpModelMappings)
		mgmt.PATCH("/ampcode/model-mappings", s.mgmt.PatchAmpModelMappings)
		mgmt.DELETE("/ampcode/model-mappings", s.mgmt.DeleteAmpModelMappings)
		mgmt.GET("/ampcode/force-model-mappings", s.mgmt.GetAmpForceModelMappings)
		mgmt.PUT("/ampcode/force-model-mappings", s.mgmt.PutAmpForceModelMappings)
		mgmt.PATCH("/ampcode/force-model-mappings", s.mgmt.PutAmpForceModelMappings)
		mgmt.GET("/ampcode/upstream-api-keys", s.mgmt.GetAmpUpstreamAPIKeys)
		mgmt.PUT("/ampcode/upstream-api-keys", s.mgmt.PutAmpUpstreamAPIKeys)
		mgmt.PATCH("/ampcode/upstream-api-keys", s.mgmt.PatchAmpUpstreamAPIKeys)
		mgmt.DELETE("/ampcode/upstream-api-keys", s.mgmt.DeleteAmpUpstreamAPIKeys)

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
		mgmt.PATCH("/claude-api-key", s.mgmt.PatchClaudeKey)
		mgmt.DELETE("/claude-api-key", s.mgmt.DeleteClaudeKey)

		mgmt.GET("/codex-api-key", s.mgmt.GetCodexKeys)
		mgmt.PUT("/codex-api-key", s.mgmt.PutCodexKeys)
		mgmt.PATCH("/codex-api-key", s.mgmt.PatchCodexKey)
		mgmt.DELETE("/codex-api-key", s.mgmt.DeleteCodexKey)

		mgmt.GET("/openai-compatibility", s.mgmt.GetOpenAICompat)
		mgmt.PUT("/openai-compatibility", s.mgmt.PutOpenAICompat)
		mgmt.PATCH("/openai-compatibility", s.mgmt.PatchOpenAICompat)
		mgmt.DELETE("/openai-compatibility", s.mgmt.DeleteOpenAICompat)

		mgmt.GET("/vertex-api-key", s.mgmt.GetVertexCompatKeys)
		mgmt.PUT("/vertex-api-key", s.mgmt.PutVertexCompatKeys)
		mgmt.PATCH("/vertex-api-key", s.mgmt.PatchVertexCompatKey)
		mgmt.DELETE("/vertex-api-key", s.mgmt.DeleteVertexCompatKey)

		mgmt.GET("/oauth-excluded-models", s.mgmt.GetOAuthExcludedModels)
		mgmt.PUT("/oauth-excluded-models", s.mgmt.PutOAuthExcludedModels)
		mgmt.PATCH("/oauth-excluded-models", s.mgmt.PatchOAuthExcludedModels)
		mgmt.DELETE("/oauth-excluded-models", s.mgmt.DeleteOAuthExcludedModels)

		mgmt.GET("/oauth-model-alias", s.mgmt.GetOAuthModelAlias)
		mgmt.PUT("/oauth-model-alias", s.mgmt.PutOAuthModelAlias)
		mgmt.PATCH("/oauth-model-alias", s.mgmt.PatchOAuthModelAlias)
		mgmt.DELETE("/oauth-model-alias", s.mgmt.DeleteOAuthModelAlias)

		mgmt.GET("/plugins", s.mgmt.ListPlugins)
		mgmt.PATCH("/plugins/:id/enabled", s.mgmt.PatchPluginEnabled)
		mgmt.PUT("/plugins/:id/config", s.mgmt.PutPluginConfig)
		mgmt.PATCH("/plugins/:id/config", s.mgmt.PatchPluginConfig)

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
		mgmt.GET("/xai-auth-url", s.mgmt.RequestXAIToken)
		mgmt.GET("/iflow-auth-url", s.mgmt.RequestIFlowToken)
		mgmt.POST("/iflow-auth-url", s.mgmt.RequestIFlowCookieToken)
		mgmt.POST("/oauth-callback", s.mgmt.PostOAuthCallback)
		mgmt.GET("/get-auth-status", s.mgmt.GetAuthStatus)
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
	if s.pluginHost == nil || s.mgmt == nil || !s.pluginHost.HasManagementRoute(c.Request.Method, path) {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	requireTLSForAuth(s.cfg)(c)
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
	s.mgmt.Middleware()(c)
	if c.IsAborted() {
		return
	}
	if s.pluginHost.ServeManagementHTTP(c.Writer, c.Request) {
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
	if s.cfg == nil || s.cfg.Home.Enabled || s.pluginHost == nil || s.mgmt == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	requireTLSForAuth(s.cfg)(c)
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
		s.mgmt.Middleware()(c)
		if c.IsAborted() {
			return
		}
	}
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
	if c.Request.TLS != nil {
		return true
	}
	if s != nil && s.cfg != nil && s.cfg.TLS.TrustForwardedProto {
		return strings.EqualFold(strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")), "https")
	}
	return false
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
		if !s.managementRoutesEnabled.Load() {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// managementBodyLimitMiddleware caps request bodies for management endpoints.
func managementBodyLimitMiddleware() gin.HandlerFunc {
	const defaultMaxBodySize = 10 << 20 // 10 MiB
	const importMaxBodySize = 100 << 20 // 100 MiB
	return func(c *gin.Context) {
		maxBodySize := int64(defaultMaxBodySize)
		if c.FullPath() == "/v0/management/usage/import" {
			maxBodySize = importMaxBodySize
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

// requireTLSForAuth enforces cfg.TLS.RequireForAuth: if enabled, reject non-TLS
// requests to authenticated routes with 421. Loopback traffic is allowed based
// on the actual peer address so Host headers cannot bypass the TLS gate.
func requireTLSForAuth(cfg *config.Config) gin.HandlerFunc {
	if cfg == nil || !cfg.TLS.RequireForAuth {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		if c.Request.TLS != nil {
			c.Next()
			return
		}
		if isLoopbackRequest(c.Request) {
			c.Next()
			return
		}
		if cfg.TLS.TrustForwardedProto {
			forwardedProto := strings.ToLower(strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")))
			if forwardedProto == "https" {
				c.Next()
				return
			}
		}
		c.AbortWithStatusJSON(
			http.StatusMisdirectedRequest, gin.H{
				"error":   "TLS required",
				"message": "this endpoint requires HTTPS (tls.require-for-auth=true). Retry using https:// or connect via loopback.",
			},
		)
	}
}

func isLoopbackRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) serveManagementControlPanel(c *gin.Context) {
	cfg := s.cfg
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
		if _, ok := c.Request.URL.Query()["client_version"]; ok {
			if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
				s.handleHomeCodexClientModels(c)
				return
			}
			openaiHandler.OpenAIModels(c)
			return
		}

		if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
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

func (s *Server) handleHomeCodexClientModels(c *gin.Context) {
	entries, ok := s.loadHomeModelEntries(c)
	if !ok {
		return
	}

	models := make([]map[string]any, 0, len(entries))
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
		if entry.displayName != "" {
			model["display_name"] = entry.displayName
			model["description"] = entry.displayName
		}
		models = append(models, model)
	}

	models = s.handlers.FilterModelsForCurrentAPIKey(c, models)
	c.JSON(http.StatusOK, openai.CodexClientModelsResponse(models))
}

func (s *Server) geminiModelsHandler(geminiHandler *gemini.APIHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
			s.handleHomeGeminiModels(c)
			return
		}

		geminiHandler.GeminiModels(c)
	}
}

func (s *Server) geminiGetHandler(geminiHandler *gemini.APIHandler) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s != nil && s.cfg != nil && s.cfg.Home.Enabled {
			s.handleHomeGeminiModel(c)
			return
		}

		geminiHandler.GeminiGetHandler(c)
	}
}

type homeModelEntry struct {
	id          string
	created     int64
	ownedBy     string
	displayName string
}

func (s *Server) handleHomeModels(c *gin.Context) {
	entries, ok := s.loadHomeModelEntries(c)
	if !ok {
		return
	}

	userAgent := c.GetHeader("User-Agent")
	isClaude := strings.HasPrefix(userAgent, "claude-cli")

	if isClaude {
		out := make([]map[string]any, 0, len(entries))
		for _, entry := range entries {
			model := map[string]any{
				"id":       entry.id,
				"object":   "model",
				"owned_by": entry.ownedBy,
			}
			if entry.created > 0 {
				model["created_at"] = entry.created
			}
			if entry.displayName != "" {
				model["display_name"] = entry.displayName
			}
			out = append(out, model)
		}
		out = s.handlers.FilterModelsForCurrentAPIKey(c, out)
		firstID := ""
		lastID := ""
		if len(out) > 0 {
			if id, okID := out[0]["id"].(string); okID {
				firstID = id
			}
			if id, okID := out[len(out)-1]["id"].(string); okID {
				lastID = id
			}
		}
		c.JSON(
			http.StatusOK, gin.H{
				"data":     out,
				"has_more": false,
				"first_id": firstID,
				"last_id":  lastID,
			},
		)
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
	c.JSON(
		http.StatusOK, gin.H{
			"object": "list",
			"data":   filtered,
		},
	)
}

func (s *Server) handleHomeGeminiModels(c *gin.Context) {
	entries, ok := s.loadHomeModelEntries(c)
	if !ok {
		return
	}

	models := s.handlers.FilterModelsForCurrentAPIKey(c, formatHomeGeminiModels(entries))
	c.JSON(
		http.StatusOK, gin.H{
			"models": models,
		},
	)
}

func (s *Server) handleHomeGeminiModel(c *gin.Context) {
	entries, ok := s.loadHomeModelEntries(c)
	if !ok {
		return
	}

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

func (s *Server) loadHomeModelEntries(c *gin.Context) ([]homeModelEntry, bool) {
	if s == nil || c == nil || c.Request == nil {
		return nil, false
	}
	client := home.Current()
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

	entries, errDecode := decodeHomeModels(raw)
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

	return entries, true
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

func decodeHomeModels(raw []byte) ([]homeModelEntry, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("home models payload is empty")
	}

	var bySection map[string][]map[string]any
	if err := json.Unmarshal(raw, &bySection); err != nil {
		return nil, fmt.Errorf("parse home models payload: %w", err)
	}
	if len(bySection) == 0 {
		return nil, fmt.Errorf("home models payload has no sections")
	}

	seen := make(map[string]struct{})
	out := make([]homeModelEntry, 0, 256)
	for _, models := range bySection {
		for _, model := range models {
			id, _ := model["id"].(string)
			id = strings.TrimSpace(id)
			if id == "" {
				name, _ := model["name"].(string)
				name = strings.TrimSpace(name)
				id = strings.TrimPrefix(name, "models/")
			}
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}

			created := int64(0)
			switch v := model["created"].(type) {
			case float64:
				created = int64(v)
			case int64:
				created = v
			case int:
				created = int64(v)
			case json.Number:
				if n, err := v.Int64(); err == nil {
					created = n
				}
			}

			ownedBy, _ := model["owned_by"].(string)
			ownedBy = strings.TrimSpace(ownedBy)
			displayName, _ := model["display_name"].(string)
			displayName = strings.TrimSpace(displayName)
			if displayName == "" {
				displayName, _ = model["displayName"].(string)
				displayName = strings.TrimSpace(displayName)
			}

			out = append(
				out, homeModelEntry{
					id:          id,
					created:     created,
					ownedBy:     ownedBy,
					displayName: displayName,
				},
			)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	if len(out) == 0 {
		return nil, fmt.Errorf("home models payload contains no models")
	}
	return out, nil
}

// Start begins listening for and serving HTTP or HTTPS requests.
// It's a blocking call and will only return on an unrecoverable error.
//
// Returns:
//   - error: An error if the server fails to start
func (s *Server) Start() error {
	if s == nil || s.server == nil {
		return fmt.Errorf("failed to start HTTP server: server not initialized")
	}

	listener, errListen := net.Listen("tcp", s.server.Addr)
	if errListen != nil {
		return fmt.Errorf("failed to start HTTP server: %v", errListen)
	}

	useTLS := s.cfg != nil && s.cfg.TLS.Enable
	if !useTLS {
		log.Warnf(
			"security: TLS is disabled — API traffic (including Authorization headers) is transmitted in plaintext. " +
				"Enable tls.enable with certificate/key before exposing this server to the public network.",
		)
	}
	if s.cfg != nil && s.cfg.AllowQueryAuth {
		log.Warnf(
			"security: allow-query-auth is enabled — API keys may leak to access logs, Referer headers, and browser history. " +
				"Prefer Authorization header or x-api-key. This option remains for backward compatibility with scripted clients.",
		)
	}
	if useTLS {
		cert := strings.TrimSpace(s.cfg.TLS.Cert)
		key := strings.TrimSpace(s.cfg.TLS.Key)
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
		redirectPort := s.cfg.TLS.HTTPRedirectPort
		if redirectPort > 0 {
			httpsAddr := s.server.Addr
			redirectAddr := fmt.Sprintf(":%d", redirectPort)
			redirectSrv := &http.Server{
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
			s.redirectSrv = redirectSrv
			go func() {
				log.Debugf("Starting HTTP→HTTPS redirect on %s → %s", redirectAddr, httpsAddr)
				if err := redirectSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Warnf("HTTP redirect listener stopped: %v", err)
				}
			}()
		}

		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{certPair},
			NextProtos:   []string{"h2", "http/1.1"},
		}
		s.server.TLSConfig = tlsConfig
		if errHTTP2 := http2.ConfigureServer(s.server, &http2.Server{}); errHTTP2 != nil {
			log.Warnf("failed to configure HTTP/2: %v", errHTTP2)
		}
		listener = tls.NewListener(listener, tlsConfig)
		log.Debugf("Starting API server on %s with TLS", s.server.Addr)
	} else {
		log.Debugf("Starting API server on %s", s.server.Addr)
	}

	httpListener := newMuxListener(listener.Addr(), 1024)
	s.muxBaseListener = listener
	s.muxHTTPListener = httpListener

	httpErrCh := make(chan error, 1)
	acceptErrCh := make(chan error, 1)

	go func() {
		httpErrCh <- s.server.Serve(httpListener)
	}()
	go func() {
		acceptErrCh <- s.acceptMuxConnections(listener, httpListener)
	}()

	select {
	case errServe := <-httpErrCh:
		if s.muxBaseListener != nil {
			if errClose := s.muxBaseListener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
				log.Debugf("failed to close shared listener after HTTP serve exit: %v", errClose)
			}
		}
		if s.muxHTTPListener != nil {
			_ = s.muxHTTPListener.Close()
		}
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
		if s.muxHTTPListener != nil {
			_ = s.muxHTTPListener.Close()
		}
		if s.muxBaseListener != nil {
			if errClose := s.muxBaseListener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
				log.Debugf("failed to close shared listener after accept loop exit: %v", errClose)
			}
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

	if s.keepAliveEnabled {
		select {
		case s.keepAliveStop <- struct{}{}:
		default:
		}
	}

	if s.muxHTTPListener != nil {
		_ = s.muxHTTPListener.Close()
	}
	if s.muxBaseListener != nil {
		if errClose := s.muxBaseListener.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			log.Debugf("failed to close shared listener: %v", errClose)
		}
	}

	shutdownServer := func(server *http.Server, name string) error {
		if server == nil {
			return nil
		}
		if err := server.Shutdown(ctx); err != nil {
			if ctx != nil && ctx.Err() != nil {
				if closeErr := server.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
					return fmt.Errorf("failed to shutdown %s: %v (close fallback: %v)", name, err, closeErr)
				}
				return fmt.Errorf("failed to shutdown %s: %v", name, err)
			}
			return fmt.Errorf("failed to shutdown %s: %v", name, err)
		}
		return nil
	}

	if err := shutdownServer(s.server, "HTTP server"); err != nil {
		return err
	}
	if err := shutdownServer(s.redirectSrv, "redirect server"); err != nil {
		return err
	}

	if s.quotaScheduler != nil {
		s.quotaScheduler.Stop()
	}
	if s.mgmt != nil {
		if err := s.mgmt.Close(); err != nil {
			return err
		}
	}

	if s.authRateLimiter != nil {
		s.authRateLimiter.Stop()
	}

	log.Debug("API server stopped")
	return nil
}

// corsMiddleware returns a Gin middleware handler that adds CORS headers
// to every response, allowing cross-origin requests.
// When allowedOrigins is non-empty, only matching origins receive the
// Access-Control-Allow-Origin header (with Vary: Origin).
// When empty, all origins are allowed (*) for CLI tool compatibility.
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
			if origin != "" && pluginResourceRequest {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Access-Control-Allow-Credentials", "true")
				c.Header("Vary", "Origin")
			} else {
				c.Header("Access-Control-Allow-Origin", "*")
			}
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
		c.Header(
			"Access-Control-Expose-Headers", "X-Cpa-Version, X-Cpa-Build-Date, X-Cpa-Commit",
		)
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

func (s *Server) applyAccessConfig(oldCfg, newCfg *config.Config) {
	if s == nil || s.accessManager == nil || newCfg == nil {
		return
	}
	if _, err := access.ApplyAccessProviders(s.accessManager, oldCfg, newCfg); err != nil {
		return
	}
}

// UpdateClients updates the server's client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (s *Server) UpdateClients(cfg *config.Config) {
	// Reconstruct old config from YAML snapshot to avoid reference sharing issues
	var oldCfg *config.Config
	if len(s.oldConfigYaml) > 0 {
		_ = yaml.Unmarshal(s.oldConfigYaml, &oldCfg)
	}

	// Update request logger enabled state if it has changed
	previousRequestLog := false
	if oldCfg != nil {
		previousRequestLog = oldCfg.RequestLog && !oldCfg.CommercialMode
	}
	currentRequestLog := cfg.RequestLog && !cfg.CommercialMode
	if s.requestLoggingRuntimeEnabled != nil {
		s.requestLoggingRuntimeEnabled.Store(!cfg.CommercialMode)
	}
	if s.requestLogger != nil && (oldCfg == nil || previousRequestLog != currentRequestLog) {
		if s.loggerToggle != nil {
			s.loggerToggle(currentRequestLog)
		} else if toggler, ok := s.requestLogger.(interface{ SetEnabled(bool) }); ok {
			toggler.SetEnabled(currentRequestLog)
		}
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

	if oldCfg == nil || oldCfg.UsageStatisticsEnabled != cfg.UsageStatisticsEnabled {
		usage.SetStatisticsEnabled(cfg.UsageStatisticsEnabled)
		redisqueue.SetUsageStatisticsEnabled(cfg.UsageStatisticsEnabled)
	}

	if oldCfg == nil || oldCfg.RedisUsageQueueRetentionSeconds != cfg.RedisUsageQueueRetentionSeconds {
		redisqueue.SetRetentionSeconds(cfg.RedisUsageQueueRetentionSeconds)
	}

	if s.requestLogger != nil && (oldCfg == nil || oldCfg.ErrorLogsMaxFiles != cfg.ErrorLogsMaxFiles) {
		if setter, ok := s.requestLogger.(interface{ SetErrorLogsMaxFiles(int) }); ok {
			setter.SetErrorLogsMaxFiles(cfg.ErrorLogsMaxFiles)
		}
	}

	if oldCfg == nil || oldCfg.DisableCooling != cfg.DisableCooling {
		auth.SetQuotaCooldownDisabled(cfg.DisableCooling)
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

	prevSecretEmpty := true
	if oldCfg != nil {
		prevSecretEmpty = oldCfg.RemoteManagement.SecretKey == ""
	}
	newSecretEmpty := cfg.RemoteManagement.SecretKey == ""
	if s.envManagementSecret {
		s.registerManagementRoutes()
		if s.managementRoutesEnabled.CompareAndSwap(false, true) {
			log.Info("management routes enabled via MANAGEMENT_PASSWORD")
		} else {
			s.managementRoutesEnabled.Store(true)
		}
	} else {
		switch {
		case prevSecretEmpty && !newSecretEmpty:
			s.registerManagementRoutes()
			if s.managementRoutesEnabled.CompareAndSwap(false, true) {
				log.Info("management routes enabled after secret key update")
			} else {
				s.managementRoutesEnabled.Store(true)
			}
		case !prevSecretEmpty && newSecretEmpty:
			if s.managementRoutesEnabled.CompareAndSwap(true, false) {
				log.Info("management routes disabled after secret key removal")
			} else {
				s.managementRoutesEnabled.Store(false)
			}
		default:
			s.managementRoutesEnabled.Store(!newSecretEmpty)
		}
	}
	redisqueue.SetEnabled(s.managementRoutesEnabled.Load() || (cfg != nil && cfg.Home.Enabled))

	s.applyAccessConfig(oldCfg, cfg)
	s.cfg = cfg
	s.wsAuthEnabled.Store(cfg.WebsocketAuth)
	if oldCfg != nil && s.wsAuthChanged != nil && oldCfg.WebsocketAuth != cfg.WebsocketAuth {
		s.wsAuthChanged(oldCfg.WebsocketAuth, cfg.WebsocketAuth)
	}
	managementasset.SetCurrentConfig(cfg)
	// Save YAML snapshot for next comparison
	s.oldConfigYaml, _ = yaml.Marshal(cfg)

	s.handlers.UpdateClients(effectiveSDKConfig(cfg))

	if s.mgmt != nil {
		s.mgmt.SetConfig(cfg)
		s.mgmt.SetAuthManager(s.handlers.AuthManager)
	}
	s.refreshPluginManagementRoutes()

	// Notify Amp module only when Amp config has changed.
	ampConfigChanged := oldCfg == nil || !reflect.DeepEqual(oldCfg.AmpCode, cfg.AmpCode)
	if ampConfigChanged {
		if s.ampModule != nil {
			log.Debugf("triggering amp module config update")
			if err := s.ampModule.OnConfigUpdated(cfg); err != nil {
				log.Errorf("failed to update Amp module config: %v", err)
			}
		} else {
			log.Warnf("amp module is nil, skipping config update")
		}
	}

	// Count client sources from configuration and auth store.
	tokenStore := sdkAuth.GetTokenStore()
	if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(cfg.AuthDir)
	}
	authEntries := util.CountAuthFiles(context.Background(), tokenStore)
	geminiAPIKeyCount := len(cfg.GeminiKey)
	claudeAPIKeyCount := len(cfg.ClaudeKey)
	codexAPIKeyCount := len(cfg.CodexKey)
	vertexAICompatCount := len(cfg.VertexCompatAPIKey)
	openAICompatCount := 0
	for i := range cfg.OpenAICompatibility {
		entry := cfg.OpenAICompatibility[i]
		if entry.Disabled {
			continue
		}
		openAICompatCount += len(entry.APIKeyEntries)
	}

	total := authEntries + geminiAPIKeyCount + claudeAPIKeyCount + codexAPIKeyCount + vertexAICompatCount + openAICompatCount
	fmt.Printf(
		"server clients and configuration updated: %d clients (%d auth entries + %d Gemini API keys + %d Claude API keys + %d Codex keys + %d Vertex-compat + %d OpenAI-compat)\n",
		total,
		authEntries,
		geminiAPIKeyCount,
		claudeAPIKeyCount,
		codexAPIKeyCount,
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

// SetUsagePersister injects the usage statistics persister into the management handler.
func (s *Server) SetUsagePersister(p *usage.Persister) {
	if s == nil || s.mgmt == nil {
		return
	}
	s.mgmt.SetUsagePersister(p)
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
// only loopback clients are allowed (prevents open-proxy when api-keys is empty).
// It enforces rate limiting on authentication failures along two axes:
//   - per client IP: mitigates a single host brute-forcing
//   - per account identifier (derived from the offered credential): mitigates
//     distributed attacks that rotate IPs against one account
func authMiddleware(
	manager *sdkaccess.Manager,
	rateLimiter *access.AuthRateLimiter,
	allowQueryAuth bool,
) gin.HandlerFunc {
	return func(c *gin.Context) {
		if manager == nil {
			// No auth configured — restrict to loopback to prevent open-proxy exposure
			remoteHost, _, _ := net.SplitHostPort(c.Request.RemoteAddr)
			if remoteHost == "" {
				remoteHost = c.Request.RemoteAddr
			}
			if ip := net.ParseIP(remoteHost); ip == nil || !ip.IsLoopback() {
				c.AbortWithStatusJSON(
					http.StatusUnauthorized, gin.H{
						"error": "Unauthorized",
					},
				)
				return
			}
			c.Next()
			return
		}

		// Use RemoteAddr to prevent X-Forwarded-For spoofing.
		remoteHost, _, _ := net.SplitHostPort(c.Request.RemoteAddr)
		if remoteHost == "" {
			remoteHost = c.Request.RemoteAddr
		}
		clientIP := remoteHost
		if rateLimiter != nil && rateLimiter.IsLimited(clientIP) {
			c.AbortWithStatusJSON(
				http.StatusTooManyRequests, gin.H{
					"error": "too many authentication failures, please try again later",
				},
			)
			return
		}

		// Derive an account identifier from the raw credential candidate so repeated failures
		// against a single account can be locked out even when the attacker rotates source IPs.
		accountHash := access.HashAccountKey(extractAuthCandidate(c.Request, allowQueryAuth))
		if rateLimiter != nil && rateLimiter.IsAccountLimited(accountHash) {
			c.AbortWithStatusJSON(
				http.StatusTooManyRequests, gin.H{
					"error": "too many authentication failures for this credential, please try again later",
				},
			)
			return
		}

		result, err := manager.Authenticate(c.Request.Context(), c.Request)
		if err == nil {
			if result == nil {
				// No providers matched (api-keys empty) — restrict to loopback
				if ip := net.ParseIP(clientIP); ip == nil || !ip.IsLoopback() {
					c.AbortWithStatusJSON(
						http.StatusUnauthorized, gin.H{
							"error": "Unauthorized",
						},
					)
					return
				}
			} else {
				c.Set("apiKey", result.Principal)
				c.Set("accessProvider", result.Provider)
				if len(result.Metadata) > 0 {
					c.Set("accessMetadata", result.Metadata)
				}
			}
			if rateLimiter != nil {
				rateLimiter.RecordSuccess(clientIP)
				rateLimiter.RecordAccountSuccess(accountHash)
			}
			c.Next()
			return
		}

		if rateLimiter != nil {
			rateLimiter.RecordFailure(clientIP)
			rateLimiter.RecordAccountFailure(accountHash)
		}

		statusCode := err.HTTPStatusCode()
		if statusCode >= http.StatusInternalServerError {
			log.Errorf("authentication middleware error: %v", err)
		}
		c.AbortWithStatusJSON(statusCode, gin.H{"error": err.Message})
	}
}
