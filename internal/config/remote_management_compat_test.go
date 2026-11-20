package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRemoteManagementPanelFlagPresenceRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		enabled      bool
	}{
		{name: "default", source: `{}`, enabled: true},
		{name: "legacy true", source: `auto-update-panel: true`, enabled: true},
		{name: "legacy false", source: `auto-update-panel: false`, enabled: false},
		{name: "disable true", source: `disable-auto-update-panel: true`, enabled: false},
		{name: "disable false", source: `disable-auto-update-panel: false`, enabled: true},
		{
			name:   "explicit false overrides legacy false",
			source: "auto-update-panel: false\ndisable-auto-update-panel: false", enabled: true,
		},
		{
			name:   "explicit true overrides legacy true",
			source: "auto-update-panel: true\ndisable-auto-update-panel: true", enabled: false,
		},
		{
			name: "null retains legacy", source: "auto-update-panel: false\ndisable-auto-update-panel: null",
			enabled: false,
		},
		{
			name:    "merged false overrides legacy",
			source:  "defaults: &panel {disable-auto-update-panel: false}\n<<: *panel\nauto-update-panel: false",
			enabled: true,
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				var value RemoteManagement
				if err := yaml.Unmarshal([]byte(tc.source), &value); err != nil {
					t.Fatal(err)
				}
				if got := value.IsAutoUpdatePanel(); got != tc.enabled {
					t.Fatalf("decoded enabled=%t, want %t", got, tc.enabled)
				}
				encoded, err := yaml.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var restored RemoteManagement
				if err := yaml.Unmarshal(encoded, &restored); err != nil ||
					restored.IsAutoUpdatePanel() != tc.enabled {
					t.Fatalf(
						"round-trip enabled=%t, want %t, error=%v",
						restored.IsAutoUpdatePanel(),
						tc.enabled,
						err,
					)
				}
				cloned, err := Clone(&Config{RemoteManagement: value})
				if err != nil || cloned == nil || cloned.RemoteManagement.IsAutoUpdatePanel() != tc.enabled {
					t.Fatalf("clone changed panel flag: error=%v", err)
				}
			},
		)
	}
}

func TestRemoteManagementPanelFlagSavePreservesPriority(t *testing.T) {
	for _, explicitFalse := range []bool{false, true} {
		name := "legacy-only"
		if explicitFalse {
			name = "explicit-false"
		}
		t.Run(
			name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				content := "port: 8317\nremote-management:\n  # panel fixture\n  " +
					"auto-update-panel: false\n  extension: " +
					"preserved\n"
				if explicitFalse {
					content += "  disable-auto-update-panel: false\n"
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				cfg, err := LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				if cfg.RemoteManagement.IsAutoUpdatePanel() != explicitFalse ||
					cfg.RemoteManagement.PanelGitHubRepository != DefaultPanelGitHubRepository ||
					cfg.RemoteManagement.CPAGitHubRepository != defaultCPAGitHubRepository {
					t.Fatal("loading changed panel priority or repository defaults")
				}
				cloned, err := Clone(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := SaveConfigPreserveComments(path, cloned); err != nil {
					t.Fatal(err)
				}
				restored, err := LoadConfig(path)
				if err != nil || restored == nil ||
					restored.RemoteManagement.IsAutoUpdatePanel() != explicitFalse {
					t.Fatalf("saved panel priority changed: error=%v", err)
				}
				body, err := os.ReadFile(path)
				if err != nil || !strings.Contains(string(body), "# panel fixture") || !strings.Contains(
					string(body), "extension: preserved",
				) {
					t.Fatalf("save lost the existing comment or extension: error=%v", err)
				}
			},
		)
	}
}

func TestRemoteManagementPanelFlagReusePreservesAbsentAndNull(t *testing.T) {
	legacy := false
	value := RemoteManagement{DisableAutoUpdatePanel: true, AutoUpdatePanel: &legacy}
	if err := yaml.Unmarshal([]byte(`{}`), &value); err != nil || value.IsAutoUpdatePanel() {
		t.Fatalf("absent field changed existing configuration: error=%v", err)
	}
	if err := yaml.Unmarshal(
		[]byte(`disable-auto-update-panel: null`), &value,
	); err != nil || value.DisableAutoUpdatePanel || value.IsAutoUpdatePanel() {
		t.Fatalf("null did not restore legacy policy: error=%v", err)
	}
	if err := yaml.Unmarshal(
		[]byte(`disable-auto-update-panel: false`), &value,
	); err != nil || !value.IsAutoUpdatePanel() {
		t.Fatalf("explicit false did not override legacy policy: error=%v", err)
	}
	if err := yaml.Unmarshal(
		[]byte(`disable-auto-update-panel: invalid`), &value,
	); err == nil || !value.IsAutoUpdatePanel() {
		t.Fatalf("invalid boolean changed accepted configuration: error=%v", err)
	}
}
