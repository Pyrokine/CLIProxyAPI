// Package cliproxy provides the core service implementation for the CLI Proxy API.
// It includes service lifecycle management, authentication handling, file watching,
// and integration with various AI service providers through a unified interface.
package cliproxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	configaccess "github.com/Pyrokine/CLIProxyAPI/v6/internal/access/config_access"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/redisqueue"
	internalUsage "github.com/Pyrokine/CLIProxyAPI/v6/internal/usage"
	sdkaccess "github.com/Pyrokine/CLIProxyAPI/v6/sdk/access"
	sdkapi "github.com/Pyrokine/CLIProxyAPI/v6/sdk/api"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

// Builder constructs a Service instance with customizable providers.
// It provides a fluent interface for configuring all aspects of the service
// including authentication, file watching, HTTP server options, and lifecycle hooks.
type Builder struct {
	// cfg holds the application configuration.
	cfg *config.Config

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

	// authManager handles legacy authentication operations.
	authManager *sdkAuth.Manager

	// accessManager handles request authentication providers.
	accessManager *sdkaccess.Manager

	// coreManager handles core authentication and execution.
	coreManager *coreauth.Manager

	// cooldownStateStore overrides runtime cooldown persistence.
	cooldownStateStore coreauth.CooldownStateStore

	// resultPolicy intercepts execution results before state mutation and persistence.
	resultPolicy coreauth.ResultPolicy

	// pluginHost manages dynamic plugin lifecycle and callbacks.
	pluginHost *PluginHost

	// serverOptions contains additional server configuration options.
	serverOptions []sdkapi.ServerOption
}

// Hooks allows callers to plug into service lifecycle stages.
// These callbacks provide opportunities to perform custom initialization
// and cleanup operations during service startup and shutdown.
type Hooks struct {
	// OnBeforeStart is called before the service starts, allowing configuration
	// modifications or additional setup.
	OnBeforeStart func(*config.Config)

	// OnAfterStart is called after the service has started successfully,
	// providing access to the service instance for additional operations.
	OnAfterStart func(*Service)
}

// NewBuilder creates a Builder with default dependencies left unset.
// Use the fluent interface methods to configure the service before calling Build().
//
// Returns:
//   - *Builder: A new builder instance ready for configuration
func NewBuilder() *Builder {
	return &Builder{}
}

// WithConfig sets the configuration instance used by the service.
//
// Parameters:
//   - cfg: The application configuration
//
// Returns:
//   - *Builder: The builder instance for method chaining
func (b *Builder) WithConfig(cfg *config.Config) *Builder {
	b.cfg = cfg
	return b
}

// WithConfigPath sets the absolute configuration file path used for reload watching.
//
// Parameters:
//   - path: The absolute path to the configuration file
//
// Returns:
//   - *Builder: The builder instance for method chaining
func (b *Builder) WithConfigPath(path string) *Builder {
	b.configPath = path
	return b
}

// WithTokenClientProvider overrides the provider responsible for token-backed clients.
func (b *Builder) WithTokenClientProvider(provider TokenClientProvider) *Builder {
	b.tokenProvider = provider
	return b
}

// WithAPIKeyClientProvider overrides the provider responsible for API key-backed clients.
func (b *Builder) WithAPIKeyClientProvider(provider APIKeyClientProvider) *Builder {
	b.apiKeyProvider = provider
	return b
}

// WithWatcherFactory allows customizing the watcher factory that handles reloads.
func (b *Builder) WithWatcherFactory(factory WatcherFactory) *Builder {
	b.watcherFactory = factory
	return b
}

// WithHooks registers lifecycle hooks executed around service startup.
func (b *Builder) WithHooks(h Hooks) *Builder {
	b.hooks = h
	return b
}

// WithAuthManager overrides the authentication manager used for token lifecycle operations.
func (b *Builder) WithAuthManager(mgr *sdkAuth.Manager) *Builder {
	b.authManager = mgr
	return b
}

// WithRequestAccessManager overrides the request authentication manager.
func (b *Builder) WithRequestAccessManager(mgr *sdkaccess.Manager) *Builder {
	b.accessManager = mgr
	return b
}

// WithCoreAuthManager overrides the runtime auth manager responsible for request execution.
func (b *Builder) WithCoreAuthManager(mgr *coreauth.Manager) *Builder {
	b.coreManager = mgr
	return b
}

// WithCooldownStateStore overrides the store used for runtime cooldown persistence.
func (b *Builder) WithCooldownStateStore(store coreauth.CooldownStateStore) *Builder {
	b.cooldownStateStore = store
	return b
}

// WithResultPolicy sets an execution result policy invoked before state mutation and persistence.
func (b *Builder) WithResultPolicy(policy coreauth.ResultPolicy) *Builder {
	b.resultPolicy = policy
	return b
}

// WithPluginHost overrides the dynamic plugin host used by the service.
func (b *Builder) WithPluginHost(host *PluginHost) *Builder {
	b.pluginHost = host
	return b
}

// WithServerOptions appends server configuration options used during construction.
func (b *Builder) WithServerOptions(opts ...sdkapi.ServerOption) *Builder {
	b.serverOptions = append(b.serverOptions, opts...)
	return b
}

// WithLocalManagementPassword configures a password that is only accepted from localhost management requests.
func (b *Builder) WithLocalManagementPassword(password string) *Builder {
	if password == "" {
		return b
	}
	b.serverOptions = append(b.serverOptions, sdkapi.WithLocalManagementPassword(password))
	return b
}

// WithPostAuthHook registers a hook to be called after an Auth record is created
// but before it is persisted to storage.
func (b *Builder) WithPostAuthHook(hook coreauth.PostAuthHook) *Builder {
	if hook == nil {
		return b
	}
	b.serverOptions = append(b.serverOptions, sdkapi.WithPostAuthHook(hook))
	return b
}

func newRoutingSelector(strategy string) coreauth.Selector {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "fill-first", "fillfirst", "ff":
		return &coreauth.FillFirstSelector{}
	case "weighted-round-robin", "weightedroundrobin", "wrr":
		return &coreauth.WeightedRoundRobinSelector{}
	case "quota-aware", "quotaaware", "qa":
		return &coreauth.QuotaAwareSelector{}
	default:
		return &coreauth.RoundRobinSelector{}
	}
}

// Build validates inputs, applies defaults, and returns a ready-to-run service.
func (b *Builder) Build() (*Service, error) {
	if b.cfg == nil {
		return nil, fmt.Errorf("cliproxy: configuration is required")
	}
	if b.configPath == "" {
		return nil, fmt.Errorf("cliproxy: configuration path is required")
	}
	b.cfg.NormalizePluginsConfig()
	if errResolvePluginsDir := b.cfg.ResolvePluginsDir(); errResolvePluginsDir != nil && b.cfg.Plugins.Enabled {
		return nil, fmt.Errorf("cliproxy: %w", errResolvePluginsDir)
	}

	tokenProvider := b.tokenProvider
	if tokenProvider == nil {
		tokenProvider = NewFileTokenClientProvider()
	}

	apiKeyProvider := b.apiKeyProvider
	if apiKeyProvider == nil {
		apiKeyProvider = NewAPIKeyClientProvider()
	}

	watcherFactory := b.watcherFactory
	if watcherFactory == nil {
		watcherFactory = defaultWatcherFactory
	}

	authManager := b.authManager
	if authManager == nil {
		authManager = newDefaultAuthManager()
	}

	accessManager := b.accessManager
	if accessManager == nil {
		accessManager = sdkaccess.NewManager()
	}

	configaccess.Register(&b.cfg.SDKConfig)
	accessManager.SetProviders(sdkaccess.RegisteredProviders())
	if errValidate := b.cfg.ValidateCredentialWeights(); errValidate != nil {
		return nil, fmt.Errorf("cliproxy: invalid credential weight: %w", errValidate)
	}

	coreManager := b.coreManager
	cooldownStateStore := b.cooldownStateStore
	if coreManager == nil {
		tokenStore := sdkAuth.GetTokenStore()
		if dirSetter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok && b.cfg != nil {
			dirSetter.SetBaseDir(b.cfg.AuthDir)
		}
		if cooldownStateStore == nil {
			if provider, ok := tokenStore.(coreauth.CooldownStateStoreProvider); ok {
				cooldownStateStore = provider.CooldownStateStore()
			}
		}

		strategy := ""
		sessionAffinity := false
		sessionAffinityTTL := time.Hour
		var sessionAffinitySubagents *bool
		if b.cfg != nil {
			strategy = strings.ToLower(strings.TrimSpace(b.cfg.Routing.Strategy))
			sessionAffinity = b.cfg.Routing.SessionAffinity
			sessionAffinitySubagents = b.cfg.Routing.SessionAffinitySubagents
			if ttl := strings.TrimSpace(b.cfg.Routing.SessionAffinityTTL); ttl != "" {
				if parsed, err := time.ParseDuration(ttl); err == nil && parsed > 0 {
					sessionAffinityTTL = parsed
				}
			}
		}
		selector := newRoutingSelector(strategy)
		if sessionAffinity {
			selector = coreauth.NewSessionAffinitySelectorWithConfig(
				coreauth.SessionAffinityConfig{
					Fallback:         selector,
					TTL:              sessionAffinityTTL,
					SubagentAffinity: sessionAffinitySubagents,
				},
			)
		}

		coreManager = coreauth.NewManager(tokenStore, selector, nil)
	}
	// Attach a default RoundTripper provider so providers can opt in per-auth transports.
	coreManager.SetRoundTripperProvider(newDefaultRoundTripperProvider())
	coreManager.SetConfig(b.cfg)
	coreManager.SetOAuthModelAlias(b.cfg.OAuthModelAlias)
	if b.resultPolicy != nil {
		coreManager.SetResultPolicy(b.resultPolicy)
	}

	pluginHost := b.pluginHost
	if pluginHost == nil {
		pluginHost = NewPluginHost()
	}
	usageManager := coreusage.NewManager(512)
	usageQueue := redisqueue.NewQueue()
	usageQueue.SetUsageStatisticsEnabled(b.cfg.UsageStatisticsEnabled)
	usageQueue.SetRetentionSeconds(b.cfg.RedisUsageQueueRetentionSeconds)
	internalPluginHost := pluginHost.internal()
	if internalPluginHost != nil {
		internalPluginHost.SetUsageManager(usageManager)
		internalPluginHost.SetAccessManager(accessManager)
		if errApply := internalPluginHost.ApplyConfig(context.Background(), b.cfg); errApply != nil {
			return nil, fmt.Errorf("cliproxy: apply plugin config during build: %w", errApply)
		}
		internalPluginHost.RegisterFrontendAuthProviders()
		accessManager.SetProviders(sdkaccess.RegisteredProviders())
		coreManager.SetPluginScheduler(internalPluginHost)
	}

	service := &Service{
		cfg:                b.cfg,
		configPath:         b.configPath,
		tokenProvider:      tokenProvider,
		apiKeyProvider:     apiKeyProvider,
		watcherFactory:     watcherFactory,
		hooks:              b.hooks,
		authManager:        authManager,
		accessManager:      accessManager,
		coreManager:        coreManager,
		cooldownStateStore: cooldownStateStore,
		pluginHost:         internalPluginHost,
		usageManager:       usageManager,
		usageStats:         internalUsage.NewRequestStatistics(),
		usageQueue:         usageQueue,
		serverOptions:      append([]sdkapi.ServerOption(nil), b.serverOptions...),
	}
	service.usageStatisticsEnabled.Store(b.cfg.UsageStatisticsEnabled)
	coreManager.SetHomeClientProvider(
		func() coreauth.HomeClient {
			client := service.currentHomeClient()
			if client == nil {
				return nil
			}
			return client
		},
	)
	coreManager.SetDeferredAuthRecreateHandler(service.applyCoreAuthAddOrUpdate)
	service.serverOptions = append(
		service.serverOptions,
		sdkapi.WithPostAuthPersistHook(service.runtimeAuthSyncHook()),
	)
	return service, nil
}

func (s *Service) runtimeAuthSyncHook() coreauth.PostAuthHook {
	return func(_ context.Context, auth *coreauth.Auth) error {
		if s == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
			return nil
		}

		s.authUpdateMu.Lock()
		defer s.authUpdateMu.Unlock()

		action := AuthUpdateActionAdd
		if s.coreManager != nil {
			if _, ok := s.coreManager.GetByID(auth.ID); ok {
				action = AuthUpdateActionModify
			}
		}
		update := AuthUpdate{Action: action, ID: auth.ID, Auth: auth}
		if watcher := s.currentWatcher(); watcher != nil {
			_, revision := watcher.DispatchPersistedAuthUpdateWithRevision(&update)
			update.SetRevision(revision)
		}

		syncCtx := coreauth.WithSkipPersist(context.Background())
		return s.handleAuthUpdateLocked(syncCtx, update)
	}
}
