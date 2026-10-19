// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	sdkpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginstore"
	"gopkg.in/yaml.v3"
)

const (
	DefaultPanelGitHubRepository = "https://github.com/Pyrokine/Cli-Proxy-API-Management-Center"
	DefaultAuthDir               = "~/.cli-proxy-api"
	defaultCPAGitHubRepository   = "https://github.com/Pyrokine/CLIProxyAPI"
	DefaultPprofAddr             = "127.0.0.1:8316"
)

// RequestScopedErrorRule configures custom classification and handling for upstream errors.
type RequestScopedErrorRule struct {
	// Status matches the HTTP status code of the upstream response.
	Status int `yaml:"status,omitempty" json:"status,omitempty"`
	// Match matches case-sensitive substrings in the upstream error body.
	Match []string `yaml:"match,omitempty" json:"match,omitempty"`
	// MatchRegexr matches regular expressions in the upstream error body.
	MatchRegexr []string `yaml:"match-regexr,omitempty" json:"match-regexr,omitempty"`
	// Action specifies stop, stop-and-cooldown, continue, or continue-and-cooldown.
	Action string `yaml:"action,omitempty" json:"action,omitempty"`
}

// Config represents the application's configuration, loaded from a YAML file.
type Config struct {
	SDKConfig `yaml:",inline"`
	// Host is the network host/interface on which the API server will bind.
	// Default is "127.0.0.1" (loopback only). Set to "0.0.0.0" to expose on all interfaces.
	Host string `yaml:"host" json:"-"`
	// Port is the network port on which the API server will listen.
	Port int `yaml:"port" json:"-"`

	// TLS config controls HTTPS server settings.
	TLS TLSConfig `yaml:"tls" json:"tls"`

	// Home config is runtime-only and is populated from -home-jwt.
	Home HomeConfig `yaml:"-" json:"-"`

	// CredentialConcurrency contains Home-authoritative credential lifecycle settings.
	CredentialConcurrency CredentialConcurrencyConfig `yaml:"credential-concurrency,omitempty" json:"credential-concurrency,omitempty"`

	// CredentialInFlight configures credential observation snapshots.
	CredentialInFlight CredentialInFlightConfig `yaml:"credential-in-flight,omitempty" json:"credential-in-flight,omitempty"`

	// RemoteManagement nests management-related options under 'remote-management'.
	RemoteManagement RemoteManagement `yaml:"remote-management" json:"-"`

	// Plugins configures dynamic plugin discovery and per-plugin settings.
	Plugins PluginsConfig `yaml:"plugins" json:"plugins"`

	// AuthDir is the directory where authentication token files are stored.
	AuthDir string `yaml:"auth-dir" json:"-"`

	// Debug enables or disables debug-level logging and other debug features.
	Debug bool `yaml:"debug" json:"debug"`

	// Pprof config controls the optional pprof HTTP debug server.
	Pprof pprofConfig `yaml:"pprof" json:"pprof"`

	// CommercialMode disables high-overhead HTTP middleware features to minimize per-request memory usage.
	CommercialMode bool `yaml:"commercial-mode" json:"commercial-mode"`

	// DisableClaudeCloakMode disables Claude Code cloaking for every Claude credential.
	DisableClaudeCloakMode bool `yaml:"disable-claude-cloak-mode" json:"disable-claude-cloak-mode"`

	// LoggingToFile controls whether application logs are written to rotating files or stdout.
	LoggingToFile bool `yaml:"logging-to-file" json:"logging-to-file"`

	// LogsMaxTotalSizeMB limits the total size (in MB) of log files under the logs directory.
	// When exceeded, the oldest log files are deleted until within the limit. Set to 0 to disable.
	LogsMaxTotalSizeMB int `yaml:"logs-max-total-size-mb" json:"logs-max-total-size-mb"`

	// ErrorLogsMaxFiles limits the number of error log files retained when request logging is disabled.
	// When exceeded, the oldest error log files are deleted. Default is 10. Set to 0 to disable cleanup.
	ErrorLogsMaxFiles int `yaml:"error-logs-max-files" json:"error-logs-max-files"`

	// UsageStatisticsEnabled toggles in-memory usage aggregation; when false, usage data is discarded.
	UsageStatisticsEnabled bool `yaml:"usage-statistics-enabled" json:"usage-statistics-enabled"`

	// RedisUsageQueueRetentionSeconds controls how long usage queue items are retained in memory.
	// Default: 60. Max: 3600.
	RedisUsageQueueRetentionSeconds int `yaml:"redis-usage-queue-retention-seconds" json:"redis-usage-queue-retention-seconds"`

	// UsageStatisticsFile is the legacy file path for usage statistics (superseded by UsageDataDir).
	// Retained for backward-compatible YAML parsing of old configs.
	UsageStatisticsFile string `yaml:"usage-statistics-file" json:"usage-statistics-file"`

	// UsageDataDir specifies the directory for usage data storage (summary, today, archives).
	// When empty, defaults to WRITABLE_PATH/usage-data/ or config directory/usage-data/.
	UsageDataDir string `yaml:"usage-data-dir" json:"usage-data-dir"`

	// UsageRetention configures automatic cleanup and archival of detailed usage records.
	UsageRetention UsageRetention `yaml:"usage-retention" json:"usage-retention"`

	// ImageArtifactCache configures local generated image artifact retention.
	ImageArtifactCache ImageArtifactCache `yaml:"image-artifact-cache" json:"image-artifact-cache"`

	// DisableCooling disables quota cooldown scheduling when true.
	DisableCooling bool `yaml:"disable-cooling" json:"disable-cooling"`

	// SaveCooldownStatus persists runtime cooldown state independently of auth credentials.
	SaveCooldownStatus bool `yaml:"save-cooldown-status" json:"save-cooldown-status"`

	// TransientErrorCooldownSeconds 控制临时上游错误的冷却时长，0 使用默认值，负数停用
	TransientErrorCooldownSeconds int `yaml:"transient-error-cooldown-seconds" json:"transient-error-cooldown-seconds"`

	// AuthAutoRefreshWorkers overrides the core auth auto-refresh concurrency.
	// When <= 0, the default worker count is used.
	AuthAutoRefreshWorkers int `yaml:"auth-auto-refresh-workers" json:"auth-auto-refresh-workers"`

	// RequestRetry defines the retry times when the request failed.
	RequestRetry int `yaml:"request-retry" json:"request-retry"`
	// MaxRetryCredentials defines the maximum number of credentials to try for a failed request.
	// Set to 0 or a negative value to keep trying all available credentials (legacy behavior).
	MaxRetryCredentials int `yaml:"max-retry-credentials" json:"max-retry-credentials"`
	// MaxRetryInterval defines the maximum wait time in seconds before retrying a cooled-down credential.
	MaxRetryInterval int `yaml:"max-retry-interval" json:"max-retry-interval"`

	// QuotaExceeded defines the behavior when a quota is exceeded.
	QuotaExceeded QuotaExceeded `yaml:"quota-exceeded" json:"quota-exceeded"`

	// QuotaRefresh configures the backend quota polling scheduler.
	QuotaRefresh QuotaRefresh `yaml:"quota-refresh" json:"quota-refresh"`

	// Routing controls credential selection behavior.
	Routing routingConfig `yaml:"routing" json:"routing"`

	// CORSAllowedOrigins is a list of allowed origins for CORS.
	// When empty, allows all origins (*) for CLI tool compatibility.
	// Set to specific origins (e.g., ["http://localhost:5173"]) to restrict browser access.
	CORSAllowedOrigins []string `yaml:"cors-allowed-origins,omitempty" json:"cors-allowed-origins,omitempty"`

	// WebsocketAuth enables or disables authentication for the WebSocket API.
	WebsocketAuth bool `yaml:"ws-auth" json:"ws-auth"`

	// GeminiKey defines Gemini API key configurations with optional routing overrides.
	GeminiKey []GeminiKey `yaml:"gemini-api-key" json:"gemini-api-key"`

	// InteractionsKey defines native Google Interactions API key configurations.
	InteractionsKey []GeminiKey `yaml:"interactions-api-key" json:"interactions-api-key"`

	// Codex defines a list of Codex API key configurations as specified in the YAML configuration file.
	CodexKey []CodexKey `yaml:"codex-api-key" json:"codex-api-key"`

	// XAIKey defines native xAI API key configurations.
	XAIKey []XAIKey `yaml:"xai-api-key" json:"xai-api-key"`

	// MetaKey defines native Meta Muse API key configurations.
	MetaKey []MetaKey `yaml:"meta-api-key" json:"meta-api-key"`

	// Antigravity configures provider-wide Antigravity request behavior.
	Antigravity AntigravityConfig `yaml:"antigravity" json:"antigravity"`

	// Devin configures provider-wide Devin request behavior.
	Devin DevinConfig `yaml:"devin" json:"devin"`

	// XAI configures provider-wide xAI request behavior.
	XAI XAIConfig `yaml:"xai" json:"xai"`

	Codex CodexConfig `yaml:"codex" json:"codex"`

	Claude ClaudeConfig `yaml:"claude" json:"claude"`

	// CodexHeaderDefaults configures fallback headers for Codex OAuth model requests.
	CodexHeaderDefaults CodexHeaderDefaults `yaml:"codex-header-defaults" json:"codex-header-defaults"`

	// ClaudeKey defines a list of Claude API key configurations as specified in the YAML configuration file.
	ClaudeKey []ClaudeKey `yaml:"claude-api-key" json:"claude-api-key"`

	// ClaudeHeaderDefaults configures default header values for Claude API requests.
	// These are used as fallbacks when the client does not send its own headers.
	ClaudeHeaderDefaults ClaudeHeaderDefaults `yaml:"claude-header-defaults" json:"claude-header-defaults"`

	// OpenAICompatibility defines OpenAI API compatibility configurations for external providers.
	OpenAICompatibility []OpenAICompatibility `yaml:"openai-compatibility" json:"openai-compatibility"`

	// VertexCompatAPIKey defines Vertex AI-compatible API key configurations for third-party providers.
	// Used for services that use Vertex AI-style paths but with simple API key authentication.
	VertexCompatAPIKey []VertexCompatKey `yaml:"vertex-api-key" json:"vertex-api-key"`

	// OAuthExcludedModels defines per-provider global model exclusions applied to OAuth/file-backed auth entries.
	OAuthExcludedModels map[string][]string `yaml:"oauth-excluded-models,omitempty" json:"oauth-excluded-models,omitempty"`

	// OAuthModelAlias defines global model name aliases for OAuth/file-backed auth channels.
	// These aliases affect both model listing and model routing for supported channels:
	// gemini-cli, vertex, aistudio, antigravity, claude, codex, qwen, iflow, kimi, xai.
	//
	// NOTE: This does not apply to existing per-credential model alias features under:
	// gemini-api-key, interactions-api-key, codex-api-key, xai-api-key, claude-api-key,
	// openai-compatibility, and vertex-api-key.
	OAuthModelAlias map[string][]OAuthModelAlias `yaml:"oauth-model-alias,omitempty" json:"oauth-model-alias,omitempty"`

	// OAuthSettings overrides model metadata only for OAuth/file-backed credentials.
	OAuthSettings map[string][]OAuthModelSetting `yaml:"oauth-settings,omitempty" json:"oauth-settings,omitempty"`

	// OAuthRequestScopedErrors defines per-provider error rules for OAuth/file-backed auth entries.
	OAuthRequestScopedErrors map[string][]RequestScopedErrorRule `yaml:"oauth-request-scoped-errors,omitempty" json:"oauth-request-scoped-errors,omitempty"`

	// AutoRefreshInterval is the interval (in seconds) for the management panel to auto-refresh data
	// when the browser window regains focus. Set to 0 to disable auto-refresh. Default: 3.
	AutoRefreshInterval int `yaml:"auto-refresh-interval" json:"auto-refresh-interval"`

	// ModelRefreshInterval is the interval (in hours) for fetching the upstream model catalog.
	// Set to 0 to disable auto-refresh. Default: 3.
	ModelRefreshInterval int `yaml:"model-refresh-interval" json:"model-refresh-interval"`

	// Payload defines default and override rules for provider payload parameters.
	Payload PayloadConfig `yaml:"payload" json:"payload"`
}

// PluginsConfig holds dynamic plugin system settings.
type PluginsConfig struct {
	// Enabled toggles dynamic plugin loading.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Dir is the plugin discovery directory.
	Dir string `yaml:"dir" json:"dir"`
	// StoreSources appends third-party plugin store registries to the built-in official source.
	StoreSources []string `yaml:"store-sources,omitempty" json:"store-sources,omitempty"`
	// StoreAuth defines optional auth rules for plugin store registry, metadata, and artifact requests.
	StoreAuth []sdkpluginstore.AuthConfig `yaml:"store-auth,omitempty" json:"store-auth,omitempty"`
	// AuthRevision changes when Home-managed plugin credentials change.
	AuthRevision int64 `yaml:"auth-revision,omitempty" json:"auth-revision,omitempty"`
	// Configs stores per-plugin instance configuration by plugin ID.
	Configs map[string]PluginInstanceConfig `yaml:"configs" json:"configs"`
}

// PluginInstanceConfig stores host-owned plugin settings and the original plugin YAML subtree.
type PluginInstanceConfig struct {
	// Enabled toggles this plugin instance. Nil is normalized to true during YAML parsing.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Priority controls plugin startup and routing order.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`
	// Raw preserves the full original plugin configuration YAML subtree.
	Raw yaml.Node `yaml:"-" json:"-"`
}

// PluginInstanceConfigFingerprint returns a stable digest of the preserved plugin subtree.
func PluginInstanceConfigFingerprint(item PluginInstanceConfig) (string, error) {
	data, errMarshal := json.Marshal(item.Raw)
	if errMarshal != nil {
		return "", errMarshal
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// UnmarshalYAML extracts host-owned fields while preserving the full original YAML node.
func (c *PluginInstanceConfig) UnmarshalYAML(value *yaml.Node) error {
	if c == nil {
		return nil
	}

	c.Priority = 0
	defaultEnabled := true
	c.Enabled = &defaultEnabled

	if value == nil || value.Kind == 0 {
		c.Raw = *defaultPluginInstanceConfigNode()
		return nil
	}
	c.Raw = *deepCopyNode(value)
	for i := 0; i+1 < len(value.Content); i += 2 {
		key := strings.TrimSpace(value.Content[i].Value)
		node := value.Content[i+1]
		switch key {
		case "enabled":
			var enabled bool
			if err := node.Decode(&enabled); err != nil {
				return err
			}
			c.Enabled = &enabled
		case "priority":
			var priority int
			if err := node.Decode(&priority); err != nil {
				return err
			}
			c.Priority = priority
		}
	}
	return nil
}

// MarshalYAML returns the preserved plugin YAML subtree for lossless config output.
func (c PluginInstanceConfig) MarshalYAML() (any, error) {
	if c.Raw.Kind == 0 {
		return defaultPluginInstanceConfigNode(), nil
	}
	return deepCopyNode(&c.Raw), nil
}

func defaultPluginInstanceConfigNode() *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "enabled", Tag: "!!str"},
			{Kind: yaml.ScalarNode, Value: "true", Tag: "!!bool"},
		},
	}
}

// ClaudeHeaderDefaults configures the measured Claude Code software baseline.
type ClaudeHeaderDefaults struct {
	UserAgent              string `yaml:"user-agent" json:"user-agent"`
	PackageVersion         string `yaml:"package-version" json:"package-version"`
	RuntimeVersion         string `yaml:"runtime-version" json:"runtime-version"`
	OS                     string `yaml:"os" json:"os"`
	Arch                   string `yaml:"arch" json:"arch"`
	Timeout                string `yaml:"timeout" json:"timeout"`
	Timezone               string `yaml:"timezone" json:"timezone"`
	StabilizeDeviceProfile *bool  `yaml:"stabilize-device-profile,omitempty" json:"stabilize-device-profile,omitempty"`
}

type CodexHeaderDefaults struct {
	UserAgent    string `yaml:"user-agent" json:"user-agent"`
	BetaFeatures string `yaml:"beta-features" json:"beta-features"`
}

// AntigravityConfig configures provider-wide Antigravity request behavior.
type AntigravityConfig struct {
	// SensitiveWords lists words obfuscated with zero-width characters in system instructions.
	SensitiveWords []string `yaml:"sensitive-words,omitempty" json:"sensitive-words,omitempty"`

	// ConnectionPool configures upstream HTTP connection pooling behavior for Antigravity.
	ConnectionPool AntigravityConnectionPoolConfig `yaml:"connection-pool,omitempty" json:"connection-pool,omitempty"`
}

// AntigravityConnectionPoolConfig controls upstream HTTP/1.1 connection pooling behavior for Antigravity.
type AntigravityConnectionPoolConfig struct {
	// Enabled controls whether upstream connection pooling is enabled.
	// Defaults to false (short-lived connection mode). Set to true to enable connection pooling.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`

	// IdleConnTimeout specifies how long an idle connection stays in the pool before expiring.
	// Defaults to "30s". Capped at 210s to prevent exceeding Google Frontend (GFE) 240s cutoff.
	IdleConnTimeout string `yaml:"idle-conn-timeout,omitempty" json:"idle-conn-timeout,omitempty"`

	// MaxIdleConnsPerHost specifies the maximum number of idle connections to retain per host per credential.
	// Defaults to 2.
	MaxIdleConnsPerHost *int `yaml:"max-idle-conns-per-host,omitempty" json:"max-idle-conns-per-host,omitempty"`
}

// DevinConfig configures provider-wide Devin request behavior.
type DevinConfig struct {
	// SensitiveWords lists words whose matching system-prompt lines are removed before obfuscation.
	SensitiveWords []string `yaml:"sensitive-words,omitempty" json:"sensitive-words,omitempty"`
}

// XAIConfig configures provider-wide xAI request behavior.
type XAIConfig struct {
	// InjectXSearch injects xAI's native x_search tool when the request does not declare it.
	InjectXSearch bool `yaml:"inject-x-search" json:"inject-x-search"`
}

// ClaudeConfig configures provider-wide Claude request behavior.
type ClaudeConfig struct {
	// ModelLevelCooling limits Claude quota cooldowns to the requested model.
	ModelLevelCooling bool `yaml:"model-level-cooling" json:"model-level-cooling"`
}

// TLSConfig holds HTTPS server settings.
type TLSConfig struct {
	// Enable toggles HTTPS server mode.
	Enable bool `yaml:"enable" json:"enable"`
	// Cert is the path to the TLS certificate file.
	Cert string `yaml:"cert" json:"cert"`
	// Key is the path to the TLS private key file.
	Key string `yaml:"key" json:"key"`
	// HTTPRedirectPort starts an HTTP listener on this port that redirects to HTTPS.
	// Only effective when Enable is true. Set to 0 to disable redirect. Default: 80.
	HTTPRedirectPort int `yaml:"http-redirect-port" json:"http-redirect-port"`
	// RequireForAuth, when true, rejects plaintext requests to authenticated HTTP
	// endpoints and the Redis usage-output protocol. Loopback addresses
	// (127.0.0.0/8, ::1) are always allowed so local clients still work when
	// tls.enable=false. Default: false (backward compatible).
	RequireForAuth bool `yaml:"require-for-auth" json:"require-for-auth"`
	// TrustForwardedProto controls whether the X-Forwarded-Proto header is honored
	// when deciding if a request is over HTTPS. The header is accepted only from
	// peers listed in remote-management.trusted-proxies. Default: false.
	TrustForwardedProto bool `yaml:"trust-forwarded-proto" json:"trust-forwarded-proto"`
}

// UsageRetention configures automatic cleanup of usage records stored in the
// SQLite database. Zero applies the default (-1, disabled).
type UsageRetention struct {
	// Days is the number of days to keep usage records. Records older than
	// this are deleted from the events / hour_bucket / day_bucket tables.
	// Default: -1 (disabled, keep forever). Use -1 explicitly to disable.
	Days int `yaml:"days" json:"days"`
	// MaxDBSizeMB is the write cutoff for events.db + WAL + SHM combined.
	// When the observed size reaches this value, new live writes are rejected and
	// imports are refused. An in-flight SQLite transaction can temporarily exceed
	// the cutoff before the next size check. Set to 0 to disable the cutoff.
	MaxDBSizeMB int `yaml:"max-db-size-mb" json:"max-db-size-mb"`
	// WarningThresholdPct defines when the panel should start warning about
	// approaching MaxDBSizeMB. Effective only when MaxDBSizeMB > 0.
	// Default: 80.
	WarningThresholdPct int `yaml:"warning-threshold-pct" json:"warning-threshold-pct"`
}

// ImageArtifactCache configures cleanup of generated image artifacts stored on disk.
type ImageArtifactCache struct {
	// RetentionDays is the number of days to keep image artifacts since last use.
	// Set to 0 to disable time-based cleanup.
	RetentionDays int `yaml:"retention-days" json:"retention-days"`
	// MaxTotalSizeMB limits total image artifact disk usage. Oldest artifacts are
	// deleted first when exceeded. Set to 0 to disable size-based cleanup.
	MaxTotalSizeMB int `yaml:"max-total-size-mb" json:"max-total-size-mb"`
}

// pprofConfig holds pprof HTTP server settings.
type pprofConfig struct {
	// Enable toggles the pprof HTTP debug server.
	Enable bool `yaml:"enable" json:"enable"`
	// Addr is the host:port address for the pprof HTTP server.
	Addr string `yaml:"addr" json:"addr"`
}

// RemoteManagement holds management API configuration under 'remote-management'.
type RemoteManagement struct {
	// AllowRemote toggles remote (non-localhost) access to management API.
	AllowRemote bool `yaml:"allow-remote"`
	// TrustedProxies lists direct reverse-proxy IP addresses or CIDR ranges whose
	// forwarded client address headers may identify clients for HTTP authentication.
	TrustedProxies []string `yaml:"trusted-proxies,omitempty"`
	// SecretKey is the management key (plaintext or bcrypt hashed). YAML key intentionally 'secret-key'.
	SecretKey string `yaml:"secret-key"`
	// DisableControlPanel skips serving and syncing the bundled management UI when true.
	DisableControlPanel bool `yaml:"disable-control-panel"`
	// DisableAutoUpdatePanel disables automatic management panel asset updates when true.
	DisableAutoUpdatePanel *bool `yaml:"disable-auto-update-panel,omitempty"`
	// AutoUpdatePanel controls whether the management panel asset is automatically updated from GitHub Releases.
	// Defaults to true. Set false to keep a manually deployed panel without it being overwritten.
	AutoUpdatePanel *bool `yaml:"auto-update-panel,omitempty"`
	// PanelGitHubRepository overrides the GitHub repository used to fetch the management panel asset.
	// Accepts either a repository URL (https://github.com/org/repo) or an API releases endpoint.
	PanelGitHubRepository string `yaml:"panel-github-repository"`
	// CPAGitHubRepository overrides the GitHub repository used for CPA backend self-update and releases listing.
	CPAGitHubRepository string `yaml:"cpa-github-repository"`
	// AutoCheckUpdate enables automatic periodic checking for new versions.
	// When false, version checks only happen on manual request. Defaults to false.
	AutoCheckUpdate bool `yaml:"auto-check-update"`
	// AutoUpdateCPA controls whether the backend binary is automatically installed
	// after a periodic version check finds a newer release. Defaults to false.
	AutoUpdateCPA bool `yaml:"auto-update-cpa,omitempty"`
	// CheckInterval is the interval (in minutes) between automatic version checks.
	// Only effective when AutoCheckUpdate is true. Default: 180 (3 hours). Minimum: 30.
	CheckInterval int `yaml:"check-interval,omitempty"`
}

// IsAutoUpdatePanel returns whether automatic panel updates are enabled (default: true).
func (rm RemoteManagement) IsAutoUpdatePanel() bool {
	if rm.DisableAutoUpdatePanel != nil {
		return !*rm.DisableAutoUpdatePanel
	}
	if rm.AutoUpdatePanel == nil {
		return true
	}
	return *rm.AutoUpdatePanel
}

type CodexConfig struct {
	IdentityConfuse      bool `yaml:"identity-confuse" json:"identity-confuse"`
	DisableCodexCloaking bool `yaml:"disable-codex-cloaking" json:"disable-codex-cloaking"`
	OptimizeMultiAgentV2 bool `yaml:"optimize-multi-agent-v2" json:"optimize-multi-agent-v2"`
	// OrphanDelegationCompatibility enables opt-in compatibility for orphan Codex delegation outputs.
	OrphanDelegationCompatibility bool `yaml:"orphan-delegation-compatibility" json:"orphan-delegation-compatibility"`
	// LiveMediaRelay terminates and relays Codex Live WebRTC media in this process.
	LiveMediaRelay CodexLiveMediaRelayConfig `yaml:"live-media-relay" json:"live-media-relay"`
}

// CodexLiveMediaRelayConfig configures the in-process Codex Live WebRTC gateway.
type CodexLiveMediaRelayConfig struct {
	Enabled                 bool                 `yaml:"enabled" json:"enabled"`
	MaxSessions             int                  `yaml:"max-sessions" json:"max-sessions"`
	DisablePrivateRemoteIPs bool                 `yaml:"disable-private-remote-ips" json:"disable-private-remote-ips"`
	PublicIP                string               `yaml:"public-ip" json:"public-ip"`
	UDPPortMin              uint16               `yaml:"udp-port-min" json:"udp-port-min"`
	UDPPortMax              uint16               `yaml:"udp-port-max" json:"udp-port-max"`
	ICEServers              []CodexLiveICEServer `yaml:"ice-servers" json:"ice-servers"`
}

// CodexLiveICEServer configures a STUN or TURN server for the media relay.
type CodexLiveICEServer struct {
	URLs       []string `yaml:"urls" json:"urls"`
	Username   string   `yaml:"username" json:"-"`
	Credential string   `yaml:"credential" json:"-"`
}

// QuotaExceeded defines the behavior when API quota limits are exceeded.
// It provides configuration options for automatic failover mechanisms.
type QuotaExceeded struct {
	// SwitchProject indicates whether to automatically switch to another project when a quota is exceeded.
	SwitchProject bool `yaml:"switch-project" json:"switch-project"`

	// SwitchPreviewModel indicates whether to automatically switch to a preview model when a quota is exceeded.
	SwitchPreviewModel bool `yaml:"switch-preview-model" json:"switch-preview-model"`

	// AntigravityCredits enables credits-based last-resort fallback for Claude models.
	AntigravityCredits bool `yaml:"antigravity-credits" json:"antigravity-credits"`
}

// QuotaRefresh configures the backend quota polling scheduler for the management panel.
type QuotaRefresh struct {
	// Enabled controls whether the backend periodically queries provider quota APIs. Default: false.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// Interval is the base polling interval in seconds. Default: 600 (10 minutes).
	Interval int `yaml:"interval" json:"interval"`

	// MaxInterval is the maximum polling interval after exponential backoff in seconds. Default: 1800 (30 minutes).
	MaxInterval int `yaml:"max-interval" json:"max-interval"`
}

// routingConfig configures how credentials are selected for requests.
type routingConfig struct {
	// Strategy selects the credential selection strategy.
	// Supported values: "round-robin" (default), "weighted-round-robin", "quota-aware", "fill-first".
	Strategy string `yaml:"strategy,omitempty" json:"strategy,omitempty"`

	// ClaudeCodeSessionAffinity is kept for compatibility with older configs.
	ClaudeCodeSessionAffinity bool `yaml:"claude-code-session-affinity,omitempty" json:"claude-code-session-affinity,omitempty"`

	// SessionAffinity enables session-sticky routing.
	SessionAffinity bool `yaml:"session-affinity,omitempty" json:"session-affinity,omitempty"`

	// SessionAffinityTTL controls how long session bindings are retained.
	SessionAffinityTTL string `yaml:"session-affinity-ttl,omitempty" json:"session-affinity-ttl,omitempty"`

	// SessionAffinitySubagents controls whether child agents inherit their parent's credential.
	// Nil preserves the default enabled behavior; false explicitly disables inheritance.
	SessionAffinitySubagents *bool `yaml:"session-affinity-subagents,omitempty" json:"session-affinity-subagents,omitempty"`
}

// OAuthModelAlias defines a model ID alias for a specific channel.
// It maps the upstream model name (Name) to the client-visible alias (Alias).
// When Fork is true, the alias is added as an additional model in listings while
// keeping the original model ID available.
type OAuthModelAlias struct {
	Name  string `yaml:"name" json:"name"`
	Alias string `yaml:"alias" json:"alias"`
	Fork  bool   `yaml:"fork,omitempty" json:"fork,omitempty"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`
}

// OAuthModelSetting overrides metadata for one OAuth model or its client-facing alias.
type OAuthModelSetting struct {
	Name             string `yaml:"name" json:"name"`
	Alias            string `yaml:"alias,omitempty" json:"alias,omitempty"`
	MaxContextLength int    `yaml:"max-context-length,omitempty" json:"max-context-length,omitempty"`
}

// ResolveOAuthModelSetting prefers an explicit alias over a model name; later rules of equal specificity win.
func ResolveOAuthModelSetting(settings []OAuthModelSetting, modelID, catalogName, modelName string) *OAuthModelSetting {
	id := strings.ToLower(strings.TrimSpace(modelID))
	catalog := strings.ToLower(strings.TrimSpace(catalogName))
	name := strings.ToLower(strings.TrimSpace(modelName))
	var aliasMatch, nameMatch *OAuthModelSetting
	for i := range settings {
		entry := &settings[i]
		entryName := strings.ToLower(strings.TrimSpace(entry.Name))
		if entryName == "" {
			continue
		}
		alias := strings.ToLower(strings.TrimSpace(entry.Alias))
		if alias != "" && alias == id {
			aliasMatch = entry
		} else if (alias == "" || alias == id) && (id == entryName || catalog == entryName || name == entryName) {
			nameMatch = entry
		}
	}
	if aliasMatch != nil {
		return aliasMatch
	}
	return nameMatch
}

// PayloadConfig defines default and override parameter rules applied to provider payloads.
type PayloadConfig struct {
	// Default defines rules that only set parameters when they are missing in the payload.
	Default []PayloadRule `yaml:"default" json:"default"`
	// DefaultRaw defines rules that set raw JSON values only when they are missing.
	DefaultRaw []PayloadRule `yaml:"default-raw" json:"default-raw"`
	// Override defines rules that always set parameters, overwriting any existing values.
	Override []PayloadRule `yaml:"override" json:"override"`
	// OverrideRaw defines rules that always set raw JSON values, overwriting any existing values.
	OverrideRaw []PayloadRule `yaml:"override-raw" json:"override-raw"`
	// Filter defines rules that remove parameters from the payload by JSON path.
	Filter []PayloadFilterRule `yaml:"filter" json:"filter"`
}

// PayloadFilterRule describes a rule to remove specific JSON paths from matching model payloads.
type PayloadFilterRule struct {
	// Models lists model entries with name pattern and protocol constraint.
	Models []PayloadModelRule `yaml:"models" json:"models"`
	// Params lists JSON paths (gjson/sjson syntax) to remove from the payload.
	Params []string `yaml:"params" json:"params"`
}

// PayloadRule describes a single rule targeting a list of models with parameter updates.
type PayloadRule struct {
	// Models lists model entries with name pattern and protocol constraint.
	Models []PayloadModelRule `yaml:"models" json:"models"`
	// Params maps JSON paths (gjson/sjson syntax) to values written into the payload.
	// For *-raw rules, values are treated as raw JSON fragments (strings are used as-is).
	Params map[string]any `yaml:"params" json:"params"`
}

// PayloadModelRule ties a model name pattern to a specific translator protocol.
type PayloadModelRule struct {
	// Name is the model name or wildcard pattern (e.g., "gpt-*", "*-5", "gemini-*-pro").
	Name string `yaml:"name" json:"name"`
	// Protocol restricts the rule to a specific translator format (e.g., "gemini", "responses").
	Protocol string `yaml:"protocol" json:"protocol"`
	// Headers restricts the rule to requests whose headers match all configured wildcard patterns.
	Headers map[string]string `yaml:"headers" json:"headers"`
	// FromProtocol restricts the rule to a specific source protocol (e.g., "gemini", "responses").
	FromProtocol string `yaml:"from-protocol" json:"from-protocol"`
	// Match requires payload JSON paths to equal the configured values.
	Match []map[string]any `yaml:"match" json:"match"`
	// NotMatch requires payload JSON paths to not equal the configured values.
	NotMatch []map[string]any `yaml:"not-match" json:"not-match"`
	// Exist requires payload JSON paths to exist and not be null.
	Exist []string `yaml:"exist" json:"exist"`
	// NotExist requires payload JSON paths to be missing or null.
	NotExist []string `yaml:"not-exist" json:"not-exist"`
}

// CloakConfig configures request cloaking for non-Claude-Code clients.
// Cloaking disguises API requests to appear as originating from the official Claude Code CLI.
type CloakConfig struct {
	// Mode controls cloaking behavior: "auto" (default), "always", or "never".
	// - "auto": cloak only when client is not Claude Code (based on User-Agent)
	// - "always": always apply cloaking regardless of client
	// - "never": never apply cloaking
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`

	// StrictMode controls how system prompts are handled when cloaking.
	// - false (default): prepend Claude Code prompt to user system messages
	// - true: strip all user system messages, keep only Claude Code prompt
	StrictMode bool `yaml:"strict-mode,omitempty" json:"strict-mode,omitempty"`

	// SensitiveWords is a list of words to obfuscate with zero-width characters.
	// This can help bypass certain content filters.
	SensitiveWords []string `yaml:"sensitive-words,omitempty" json:"sensitive-words,omitempty"`

	// CacheUserID controls whether Claude user_id values are cached per API key.
	// When false, a fresh random user_id is generated for every request.
	CacheUserID *bool `yaml:"cache-user-id,omitempty" json:"cache-user-id,omitempty"`
}

// ClaudeKey represents the configuration for a Claude API key,
// including the API key itself and an optional base URL for the API endpoint.
type ClaudeKey struct {
	// APIKey is the authentication key for accessing Claude API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Weight controls proportional selection under weighted-round-robin.
	// An omitted value defaults to 1; non-positive values exclude this credential; maximum 1,000,000.
	Weight *int `yaml:"weight,omitempty" json:"weight,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/claude-sonnet-4").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the Claude API endpoint.
	// If empty, the default Claude API URL will be used.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// Models defines upstream model names and aliases for request routing.
	Models []ClaudeModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// RebuildMidSystemMessage moves Claude messages with role "system" into the top-level system field.
	RebuildMidSystemMessage bool `yaml:"rebuild-mid-system-message,omitempty" json:"rebuild-mid-system-message,omitempty"`

	// Cloak configures request cloaking for non-Claude-Code clients.
	Cloak *CloakConfig `yaml:"cloak,omitempty" json:"cloak,omitempty"`

	// DisableCooling 覆盖全局冷却策略，nil 继承，false 明确启用冷却
	DisableCooling *bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`

	// RequestRetry 指定额外重试轮数，nil 或负值继承，零禁用额外重试
	RequestRetry *int `yaml:"request-retry,omitempty" json:"request-retry,omitempty"`

	// RequestScopedErrors configures custom classification rules for upstream errors.
	RequestScopedErrors []RequestScopedErrorRule `yaml:"request-scoped-errors,omitempty" json:"request-scoped-errors,omitempty"`

	// FingerprintProfile 选择请求身份，不改变凭证的认证权限和刷新方式
	FingerprintProfile string `yaml:"fingerprint-profile,omitempty" json:"fingerprint-profile,omitempty"`

	// ExperimentalCCHSigning 仅保留旧配置兼容，不授权请求签名
	ExperimentalCCHSigning bool `yaml:"experimental-cch-signing,omitempty" json:"experimental-cch-signing,omitempty"`
}

func (k ClaudeKey) GetAPIKey() string  { return k.APIKey }
func (k ClaudeKey) GetBaseURL() string { return k.BaseURL }

// ClaudeModel describes a mapping between an alias and the actual upstream model name.
type ClaudeModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// MaxContextLength overrides the context window advertised to Codex clients.
	MaxContextLength int `yaml:"max-context-length,omitempty" json:"max-context-length,omitempty"`

	// IsCompat preserves compatibility-only message fields for API-key models.
	IsCompat bool `yaml:"is-compat,omitempty" json:"is-compat,omitempty"`

	// Thinking overrides the upstream model's thinking capability.
	Thinking *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

func (m ClaudeModel) GetName() string          { return m.Name }
func (m ClaudeModel) GetAlias() string         { return m.Alias }
func (m ClaudeModel) GetDisplayName() string   { return m.DisplayName }
func (m ClaudeModel) GetMaxContextLength() int { return m.MaxContextLength }
func (m ClaudeModel) GetIsCompat() bool        { return m.IsCompat }
func (m ClaudeModel) GetThinking() *registry.ThinkingSupport {
	return m.Thinking
}

// CodexKey represents the configuration for a Codex API key,
// including the API key itself and an optional base URL for the API endpoint.
type CodexKey struct {
	// APIKey is the authentication key for accessing Codex API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Weight controls proportional selection under weighted-round-robin.
	// An omitted value defaults to 1; non-positive values exclude this credential; maximum 1,000,000.
	Weight *int `yaml:"weight,omitempty" json:"weight,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/gpt-5-codex").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the Codex API endpoint.
	// If empty, the default Codex API URL will be used.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// Websockets enables the Responses API websocket transport for this credential.
	Websockets bool `yaml:"websockets,omitempty" json:"websockets,omitempty"`

	// AlphaSearch allows this Codex API key to serve the Alpha Search endpoint.
	AlphaSearch bool `yaml:"alpha-search,omitempty" json:"alpha-search,omitempty"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// Models defines upstream model names and aliases for request routing.
	Models []CodexModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// DisableCooling 覆盖全局冷却策略，nil 继承，false 明确启用冷却
	DisableCooling *bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`

	// DisableCodexCloaking overrides the global Codex cloaking setting for this key.
	DisableCodexCloaking *bool `yaml:"disable-codex-cloaking,omitempty" json:"disable-codex-cloaking,omitempty"`

	// RequestRetry 指定额外重试轮数，nil 或负值继承，零禁用额外重试
	RequestRetry *int `yaml:"request-retry,omitempty" json:"request-retry,omitempty"`

	// RequestScopedErrors configures custom classification rules for upstream errors.
	RequestScopedErrors []RequestScopedErrorRule `yaml:"request-scoped-errors,omitempty" json:"request-scoped-errors,omitempty"`
}

func (k CodexKey) GetAPIKey() string  { return k.APIKey }
func (k CodexKey) GetBaseURL() string { return k.BaseURL }

// CodexModel describes a mapping between an alias and the actual upstream model name.
type CodexModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// MaxContextLength overrides the context window advertised to Codex clients.
	MaxContextLength int `yaml:"max-context-length,omitempty" json:"max-context-length,omitempty"`

	// ForceMapping rewrites upstream response model fields back to Alias.
	ForceMapping bool `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`

	// IsCompat preserves compatibility-only message fields for API-key models.
	IsCompat bool `yaml:"is-compat,omitempty" json:"is-compat,omitempty"`

	// SupportConfigurationUpdate enables configuration_update for a Codex API-key model.
	SupportConfigurationUpdate bool `yaml:"support-configuration-update,omitempty" json:"support-configuration-update,omitempty"`

	// Thinking overrides the upstream model's thinking capability.
	Thinking *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

func (m CodexModel) GetName() string          { return m.Name }
func (m CodexModel) GetAlias() string         { return m.Alias }
func (m CodexModel) GetDisplayName() string   { return m.DisplayName }
func (m CodexModel) GetMaxContextLength() int { return m.MaxContextLength }
func (m CodexModel) GetForceMapping() bool    { return m.ForceMapping }
func (m CodexModel) GetIsCompat() bool        { return m.IsCompat }
func (m CodexModel) GetThinking() *registry.ThinkingSupport {
	return m.Thinking
}

// XAIKey uses the Codex API key structure for native xAI execution.
type XAIKey = CodexKey

// XAIModel uses the Codex model mapping structure for xAI models.
type XAIModel = CodexModel

// MetaKey uses the Codex API key structure for native Meta Muse execution.
type MetaKey = CodexKey

// GeminiKey represents the configuration for a Gemini API key,
// including optional overrides for upstream base URL, proxy routing, and headers.
type GeminiKey struct {
	// APIKey is the authentication key for accessing Gemini API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Priority controls selection preference when multiple credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Weight controls proportional selection under weighted-round-robin.
	// An omitted value defaults to 1; non-positive values exclude this credential; maximum 1,000,000.
	Weight *int `yaml:"weight,omitempty" json:"weight,omitempty"`

	// Prefix optionally namespaces models for this credential (e.g., "teamA/gemini-3-pro-preview").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL optionally overrides the Gemini API endpoint.
	BaseURL string `yaml:"base-url,omitempty" json:"base-url,omitempty"`

	// ProxyURL optionally overrides the global proxy for this API key.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// Models defines upstream model names and aliases for request routing.
	Models []GeminiModel `yaml:"models,omitempty" json:"models,omitempty"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`

	// DisableCooling 覆盖全局冷却策略，nil 继承，false 明确启用冷却
	DisableCooling *bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`

	// RequestRetry 指定额外重试轮数，nil 或负值继承，零禁用额外重试
	RequestRetry *int `yaml:"request-retry,omitempty" json:"request-retry,omitempty"`

	// RequestScopedErrors configures custom classification rules for upstream errors.
	RequestScopedErrors []RequestScopedErrorRule `yaml:"request-scoped-errors,omitempty" json:"request-scoped-errors,omitempty"`
}

func (k GeminiKey) GetAPIKey() string  { return k.APIKey }
func (k GeminiKey) GetBaseURL() string { return k.BaseURL }

// GeminiModel describes a mapping between an alias and the actual upstream model name.
type GeminiModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// MaxContextLength overrides the context window advertised to Codex clients.
	MaxContextLength int `yaml:"max-context-length,omitempty" json:"max-context-length,omitempty"`

	// IsCompat preserves compatibility-only message fields for API-key models.
	IsCompat bool `yaml:"is-compat,omitempty" json:"is-compat,omitempty"`

	// Thinking overrides the upstream model's thinking capability.
	Thinking *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

func (m GeminiModel) GetName() string          { return m.Name }
func (m GeminiModel) GetAlias() string         { return m.Alias }
func (m GeminiModel) GetDisplayName() string   { return m.DisplayName }
func (m GeminiModel) GetMaxContextLength() int { return m.MaxContextLength }
func (m GeminiModel) GetIsCompat() bool        { return m.IsCompat }
func (m GeminiModel) GetThinking() *registry.ThinkingSupport {
	return m.Thinking
}

// OpenAICompatibility represents the configuration for OpenAI API compatibility
// with external providers, allowing model aliases to be routed through OpenAI API format.
type OpenAICompatibility struct {
	// Name is the identifier for this OpenAI compatibility configuration.
	Name string `yaml:"name" json:"name"`

	// Priority controls selection preference when multiple providers or credentials match.
	// Higher values are preferred; defaults to 0.
	Priority int `yaml:"priority,omitempty" json:"priority,omitempty"`

	// Disabled prevents this provider from being used for routing.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`

	// Prefix optionally namespaces model aliases for this provider (e.g., "teamA/kimi-k2").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// BaseURL is the base URL for the external OpenAI-compatible API endpoint.
	BaseURL string `yaml:"base-url" json:"base-url"`

	// APIKeyEntries defines API keys with optional per-key proxy configuration.
	APIKeyEntries []OpenAICompatibilityAPIKey `yaml:"api-key-entries,omitempty" json:"api-key-entries,omitempty"`

	// Models defines the model configurations including aliases for routing.
	Models []OpenAICompatibilityModel `yaml:"models" json:"models"`

	// Headers optionally adds extra HTTP headers for requests sent to this provider.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// SupportPromptCacheKey enables derived prompt_cache_key injection for supported requests.
	SupportPromptCacheKey bool `yaml:"support-prompt-cache-key,omitempty" json:"support-prompt-cache-key,omitempty"`

	// DisableCooling 覆盖全局冷却策略，nil 继承，false 明确启用冷却
	DisableCooling *bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`

	// RequestRetry 指定额外重试轮数，nil 或负值继承，零禁用额外重试
	RequestRetry *int `yaml:"request-retry,omitempty" json:"request-retry,omitempty"`

	// RequestScopedErrors configures custom classification rules for upstream errors.
	RequestScopedErrors []RequestScopedErrorRule `yaml:"request-scoped-errors,omitempty" json:"request-scoped-errors,omitempty"`
}

// OpenAICompatibilityAPIKey represents an API key configuration with optional proxy setting.
type OpenAICompatibilityAPIKey struct {
	// APIKey is the authentication key for accessing the external API services.
	APIKey string `yaml:"api-key" json:"api-key"`

	// Weight controls proportional selection under weighted-round-robin.
	// An omitted value defaults to 1; non-positive values exclude this credential; maximum 1,000,000.
	Weight *int `yaml:"weight,omitempty" json:"weight,omitempty"`

	// ProxyURL overrides the global proxy setting for this API key if provided.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`
}

// OpenAICompatibilityModel represents a model configuration for OpenAI compatibility,
// including the actual model name and its alias for API routing.
type OpenAICompatibilityModel struct {
	// Name is the actual model name used by the external provider.
	Name string `yaml:"name" json:"name"`

	// Alias is the model name alias that clients will use to reference this model.
	Alias string `yaml:"alias" json:"alias"`

	// DisplayName is the optional human-readable name shown in model catalogs.
	DisplayName string `yaml:"display-name,omitempty" json:"display-name,omitempty"`

	// MaxContextLength overrides the context window advertised to Codex clients.
	MaxContextLength int `yaml:"max-context-length,omitempty" json:"max-context-length,omitempty"`

	// UseMaxCompletionTokens selects max_completion_tokens for this upstream model.
	UseMaxCompletionTokens bool `yaml:"use-max-completion-tokens,omitempty" json:"use-max-completion-tokens,omitempty"`

	// Image marks this model as callable through /v1/images/generations and /v1/images/edits.
	Image bool `yaml:"image,omitempty" json:"image,omitempty"`

	// InputModalities declares chat/responses input capabilities, such as text or image.
	InputModalities []string `yaml:"input-modalities,omitempty" json:"input-modalities,omitempty"`

	// OutputModalities declares supported output modalities when known.
	OutputModalities []string `yaml:"output-modalities,omitempty" json:"output-modalities,omitempty"`

	// IsCompat preserves compatibility-only message fields for API-key models.
	IsCompat bool `yaml:"is-compat,omitempty" json:"is-compat,omitempty"`

	// Thinking overrides the upstream model's thinking capability.
	Thinking *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

func (m OpenAICompatibilityModel) GetName() string          { return m.Name }
func (m OpenAICompatibilityModel) GetAlias() string         { return m.Alias }
func (m OpenAICompatibilityModel) GetDisplayName() string   { return m.DisplayName }
func (m OpenAICompatibilityModel) GetMaxContextLength() int { return m.MaxContextLength }
func (m OpenAICompatibilityModel) GetIsCompat() bool        { return m.IsCompat }
func (m OpenAICompatibilityModel) GetUseMaxCompletionTokens() bool {
	return m.UseMaxCompletionTokens
}
func (m OpenAICompatibilityModel) GetThinking() *registry.ThinkingSupport {
	return m.Thinking
}

// NormalizePluginsConfig applies default plugin configuration values.
func (cfg *Config) NormalizePluginsConfig() {
	if cfg == nil {
		return
	}
	cfg.Plugins.Dir = strings.TrimSpace(cfg.Plugins.Dir)
	if cfg.Plugins.Dir == "" {
		cfg.Plugins.Dir = defaultPluginsDir
	}
	if len(cfg.Plugins.StoreSources) > 0 {
		sources := make([]string, 0, len(cfg.Plugins.StoreSources))
		for _, source := range cfg.Plugins.StoreSources {
			source = strings.TrimSpace(source)
			if source == "" {
				continue
			}
			sources = append(sources, source)
		}
		cfg.Plugins.StoreSources = sources
	}
	cfg.Plugins.StoreAuth = sdkpluginstore.NormalizeAuthConfigs(cfg.Plugins.StoreAuth)
	if cfg.Plugins.Configs == nil {
		cfg.Plugins.Configs = map[string]PluginInstanceConfig{}
		return
	}
	canonicalConfigs := make(map[string]PluginInstanceConfig, len(cfg.Plugins.Configs))
	for id, item := range cfg.Plugins.Configs {
		canonicalID := canonicalPluginConfigID(id)
		if _, duplicate := canonicalConfigs[canonicalID]; duplicate {
			return
		}
		canonicalConfigs[canonicalID] = item
	}
	cfg.Plugins.Configs = canonicalConfigs
}

func canonicalPluginConfigID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}
