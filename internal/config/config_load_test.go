package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestApplyHomeRuntimeConfig(t *testing.T) {
	cfg := &Config{
		APIKeys:                []string{"local-key"},
		UsageStatisticsEnabled: false,
		DisableCooling:         false,
		WebsocketAuth:          true,
		RemoteManagement: RemoteManagement{
			AllowRemote:         true,
			DisableControlPanel: false,
		},
	}

	ApplyHomeRuntimeConfig(cfg)

	if cfg.APIKeys != nil {
		t.Fatalf("APIKeys = %v, want nil", cfg.APIKeys)
	}
	if !cfg.UsageStatisticsEnabled {
		t.Fatal("expected usage statistics to be enabled")
	}
	if !cfg.DisableCooling {
		t.Fatal("expected quota cooldown to be disabled")
	}
	if cfg.WebsocketAuth {
		t.Fatal("expected WebSocket auth to be disabled")
	}
	if cfg.RemoteManagement.AllowRemote {
		t.Fatal("expected remote management to be disabled")
	}
	if !cfg.RemoteManagement.DisableControlPanel {
		t.Fatal("expected control panel to be disabled")
	}
}

func TestParseConfigBytes_HostDefaultsToLoopback(t *testing.T) {
	cfg, err := ParseConfigBytes([]byte("port: 8317\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}
	if cfg.Host != "127.0.0.1" {
		t.Fatalf("Host = %q, want 127.0.0.1", cfg.Host)
	}
}

func TestParseConfigBytesImageArtifactCacheDefaultsMatchLoadConfig(t *testing.T) {
	parsed, err := ParseConfigBytes([]byte("port: 8317\n"))
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err = os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	loaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if parsed.ImageArtifactCache != loaded.ImageArtifactCache {
		t.Fatalf(
			"ParseConfigBytes image artifact cache = %#v, want LoadConfig value %#v", parsed.ImageArtifactCache,
			loaded.ImageArtifactCache,
		)
	}
}

func TestLoadConfigRejectsAliasedManagementSecret(t *testing.T) {
	tests := map[string]string{
		"remote management alias": `shared: &management
  secret-key: plain-secret
remote-management: *management
`,
		"secret alias": `shared-secret: &secret plain-secret
remote-management:
  secret-key: *secret
`,
		"merged secret": `shared: &management
  secret-key: plain-secret
remote-management:
  <<: *management
`,
		"remote management key alias": `remote-name: &remote-name remote-management
*remote-name:
  secret-key: plain-secret
`,
		"secret key alias": `secret-name: &secret-name secret-key
remote-management:
  *secret-name: plain-secret
`,
	}
	for name, content := range tests {
		t.Run(
			name, func(t *testing.T) {
				configPath := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
					t.Fatalf("write config: %v", err)
				}
				if _, err := LoadConfig(configPath); err == nil || !strings.Contains(
					err.Error(),
					errManagementSecretYAMLAlias.Error(),
				) {
					t.Fatalf("LoadConfig error = %v, want alias rejection", err)
				}
				persisted, errRead := os.ReadFile(configPath)
				if errRead != nil {
					t.Fatalf("read config: %v", errRead)
				}
				if string(persisted) != content {
					t.Fatalf("rejected config changed:\n%s", persisted)
				}
			},
		)
	}
}

func TestParseConfigBytesRejectsAliasedManagementSecret(t *testing.T) {
	for name, content := range map[string]string{
		"mapping alias": `shared: &management
  secret-key: plain-secret
remote-management: *management
`,
		"mapping key alias": `secret-name: &secret-name secret-key
remote-management:
  *secret-name: plain-secret
`,
	} {
		t.Run(
			name, func(t *testing.T) {
				if _, err := ParseConfigBytes([]byte(content)); err == nil || !strings.Contains(
					err.Error(),
					errManagementSecretYAMLAlias.Error(),
				) {
					t.Fatalf("ParseConfigBytes error = %v, want alias rejection", err)
				}
			},
		)
	}
}

func TestLoadConfigRejectsNegativeImageArtifactCache(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := "image-artifact-cache:\n  retention-days: -1\n  max-total-size-mb: 1\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := LoadConfig(configPath); err == nil || !strings.Contains(
		err.Error(), "image-artifact-cache.retention-days",
	) {
		t.Fatalf("LoadConfig err = %v, want retention-days error", err)
	}
}

func TestParseConfigBytesRejectsNegativeImageArtifactCache(t *testing.T) {
	content := "image-artifact-cache:\n  retention-days: 1\n  max-total-size-mb: -1\n"
	if _, err := ParseConfigBytes([]byte(content)); err == nil || !strings.Contains(
		err.Error(), "image-artifact-cache.max-total-size-mb",
	) {
		t.Fatalf("ParseConfigBytes err = %v, want max-total-size-mb error", err)
	}
}

func TestParseConfigBytes_PluginStoreSources(t *testing.T) {
	cfg, err := ParseConfigBytes(
		[]byte(`
plugins:
  store-sources:
    - " https://example.com/plugins.json "
    - ""
    - "https://mirror.example.com/store.json"
`),
	)
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}

	want := []string{"https://example.com/plugins.json", "https://mirror.example.com/store.json"}
	if len(cfg.Plugins.StoreSources) != len(want) {
		t.Fatalf("StoreSources = %#v, want %#v", cfg.Plugins.StoreSources, want)
	}
	for i, item := range want {
		if cfg.Plugins.StoreSources[i] != item {
			t.Fatalf("StoreSources[%d] = %q, want %q", i, cfg.Plugins.StoreSources[i], item)
		}
	}
}

func TestParseConfigBytesInteractionsAPIKeys(t *testing.T) {
	cfg, err := ParseConfigBytes(
		[]byte(`
interactions-api-key:
  - api-key: " interactions-key-1 "
    base-url: " https://interactions.example.com "
    proxy-url: " http://proxy.local:8080 "
    prefix: " /team-a/ "
    headers:
      X-Custom: " value "
      " ": "drop"
    excluded-models:
      - " Gemini-2.5-Pro "
      - "gemini-2.5-pro"
      - ""
    models:
      - name: gemini-2.5-pro
        alias: g25p
  - api-key: "interactions-key-1"
    base-url: "https://interactions.example.com"
    prefix: duplicate
  - api-key: "interactions-key-1"
    base-url: "https://other-interactions.example.com"
  - api-key: "   "
`),
	)
	if err != nil {
		t.Fatalf("ParseConfigBytes: %v", err)
	}

	assertSanitizedInteractionsKeys(t, cfg)
}

func TestLoadConfigInteractionsAPIKeys(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := []byte(`
interactions-api-key:
  - api-key: " interactions-key-1 "
    base-url: " https://interactions.example.com "
    proxy-url: " http://proxy.local:8080 "
    prefix: " /team-a/ "
    headers:
      X-Custom: " value "
    excluded-models:
      - " Gemini-2.5-Pro "
      - "gemini-2.5-pro"
    models:
      - name: gemini-2.5-pro
        alias: g25p
  - api-key: "interactions-key-1"
    base-url: "https://interactions.example.com"
  - api-key: "interactions-key-1"
    base-url: "https://other-interactions.example.com"
`)
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	assertSanitizedInteractionsKeys(t, cfg)
}

func assertSanitizedInteractionsKeys(t *testing.T, cfg *Config) {
	t.Helper()
	if len(cfg.InteractionsKey) != 2 {
		t.Fatalf("InteractionsKey length = %d, want 2", len(cfg.InteractionsKey))
	}
	first := cfg.InteractionsKey[0]
	if first.APIKey != "interactions-key-1" {
		t.Fatalf("api key = %q, want interactions-key-1", first.APIKey)
	}
	if first.BaseURL != "https://interactions.example.com" {
		t.Fatalf("base url = %q, want https://interactions.example.com", first.BaseURL)
	}
	if first.ProxyURL != "http://proxy.local:8080" {
		t.Fatalf("proxy url = %q, want http://proxy.local:8080", first.ProxyURL)
	}
	if first.Prefix != "team-a" {
		t.Fatalf("prefix = %q, want team-a", first.Prefix)
	}
	if got := first.Headers["X-Custom"]; got != "value" {
		t.Fatalf("header X-Custom = %q, want value", got)
	}
	if len(first.ExcludedModels) != 1 || first.ExcludedModels[0] != "gemini-2.5-pro" {
		t.Fatalf("excluded models = %#v, want [gemini-2.5-pro]", first.ExcludedModels)
	}
	if len(first.Models) != 1 || first.Models[0].Name != "gemini-2.5-pro" || first.Models[0].Alias != "g25p" {
		t.Fatalf("models = %#v, want gemini-2.5-pro alias g25p", first.Models)
	}
	if cfg.InteractionsKey[1].BaseURL != "https://other-interactions.example.com" {
		t.Fatalf("second base url = %q, want https://other-interactions.example.com", cfg.InteractionsKey[1].BaseURL)
	}
}

func TestPluginInstanceConfigMarshalYAMLPreservesStoreMetadata(t *testing.T) {
	var cfg struct {
		Plugin PluginInstanceConfig `yaml:"plugin"`
	}
	if err := yaml.Unmarshal(
		[]byte(`
plugin:
  enabled: true
  priority: 7
  store:
    id: sample
    version: v1.2.3
    source_url: https://example.com/plugins.json
  custom:
    nested: value
`), &cfg,
	); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	text := string(out)
	for _, want := range []string{
		"store:", "version: v1.2.3", "source_url: https://example.com/plugins.json", "custom:", "nested: value",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("marshaled config missing %q:\n%s", want, text)
		}
	}
}

func TestLoadConfig_WebsocketAuth_DefaultTrue(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	// Minimal config without ws-auth field.
	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if !cfg.WebsocketAuth {
		t.Fatal("expected WebsocketAuth to default to true")
	}
}

func TestLoadConfig_WebsocketAuth_ExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	content := "port: 8317\nws-auth: false\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.WebsocketAuth {
		t.Fatal("expected WebsocketAuth to be false when explicitly set")
	}
}

func TestLoadConfig_WebsocketAuth_ExplicitTrue(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	content := "port: 8317\nws-auth: true\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if !cfg.WebsocketAuth {
		t.Fatal("expected WebsocketAuth to be true when explicitly set")
	}
}

func TestRemoteManagementDisableAutoUpdatePanel(t *testing.T) {
	disabled := true
	rm := RemoteManagement{DisableAutoUpdatePanel: &disabled}
	if rm.IsAutoUpdatePanel() {
		t.Fatal("expected disable-auto-update-panel to disable panel updates")
	}

	disabled = false
	if !rm.IsAutoUpdatePanel() {
		t.Fatal("expected false disable-auto-update-panel to keep panel updates enabled")
	}

	rm = RemoteManagement{AutoUpdatePanel: new(bool)}
	if rm.IsAutoUpdatePanel() {
		t.Fatal("expected legacy auto-update-panel=false to disable panel updates")
	}
}

func TestLoadConfig_RemoteManagementDisableAutoUpdatePanel(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	content := "port: 8317\nremote-management:\n  disable-auto-update-panel: true\n"
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RemoteManagement.IsAutoUpdatePanel() {
		t.Fatal("expected loaded disable-auto-update-panel to disable panel updates")
	}
}

func TestLoadConfig_RemoteManagementDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	if err := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.RemoteManagement.PanelGitHubRepository != DefaultPanelGitHubRepository {
		t.Fatalf("panel repo = %q, want %q", cfg.RemoteManagement.PanelGitHubRepository, DefaultPanelGitHubRepository)
	}
	if cfg.RemoteManagement.CPAGitHubRepository != defaultCPAGitHubRepository {
		t.Fatalf("cpa repo = %q, want %q", cfg.RemoteManagement.CPAGitHubRepository, defaultCPAGitHubRepository)
	}
	if cfg.RemoteManagement.AutoUpdateCPA {
		t.Fatal("expected auto-update-cpa to default to false")
	}
}

func TestValidateConfigCoversPanelManagedFields(t *testing.T) {
	cfg := &Config{
		APIKeys:              []string{"valid-api-key"},
		Port:                 8317,
		AutoRefreshInterval:  -1,
		ModelRefreshInterval: -1,
		CORSAllowedOrigins:   []string{"not-url", ""},
		QuotaRefresh: QuotaRefresh{
			Enabled:     true,
			Interval:    10,
			MaxInterval: 5,
		},
		Routing: routingConfig{
			Strategy:           "bad",
			SessionAffinityTTL: "wat",
		},
		Plugins: PluginsConfig{
			Configs: map[string]PluginInstanceConfig{
				"bad id": {},
			},
		},
	}

	warnings := ValidateConfig(cfg)
	for _, want := range []string{
		"auto-refresh-interval -1 is negative",
		"model-refresh-interval -1 is negative",
		"cors-allowed-origins[0]",
		"cors-allowed-origins[1] is empty",
		"quota-refresh.interval 10 is below minimum 60 seconds",
		"quota-refresh.max-interval 5 is below minimum 300 seconds",
		"quota-refresh.max-interval is smaller than quota-refresh.interval",
		"routing.strategy \"bad\" is invalid",
		"routing.session-affinity-ttl \"wat\" is not a positive duration",
		"plugins.configs contains invalid plugin id \"bad id\"",
	} {
		if !hasWarningContaining(warnings, want) {
			t.Fatalf("warnings missing %q: %#v", want, warnings)
		}
	}
}

func TestValidateConfigAcceptsQuotaAwareRouting(t *testing.T) {
	warnings := ValidateConfig(&Config{Routing: routingConfig{Strategy: "quota-aware"}})
	if hasWarningContaining(warnings, "routing.strategy") {
		t.Fatalf("quota-aware routing warnings = %#v", warnings)
	}
}

func hasWarningContaining(warnings []string, want string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, want) {
			return true
		}
	}
	return false
}

func TestSaveLastGoodReplacesBackupAtomically(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	lastGoodPath := LastGoodPath(configPath)
	if err := os.WriteFile(configPath, []byte("port: 9090\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lastGoodPath, []byte("port: 8317\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(lastGoodPath)
	if err != nil {
		t.Fatal(err)
	}

	if err = SaveLastGood(configPath); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(lastGoodPath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("last-good backup was updated in place")
	}
	if after.Mode().Perm() != 0o600 {
		t.Fatalf("last-good mode = %o, want 600", after.Mode().Perm())
	}
	written, err := os.ReadFile(lastGoodPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != "port: 9090\n" {
		t.Fatalf("last-good content = %q, want current config", written)
	}
}
