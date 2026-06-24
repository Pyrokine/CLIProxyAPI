package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyHomeRuntimeConfig(t *testing.T) {
	cfg := &Config{
		SDKConfig:              SDKConfig{APIKeys: []string{"local-key"}},
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
		SDKConfig:            SDKConfig{APIKeys: []string{"valid-api-key"}},
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

func hasWarningContaining(warnings []string, want string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, want) {
			return true
		}
	}
	return false
}
