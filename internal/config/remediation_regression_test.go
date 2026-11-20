package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestImageArtifactCacheRejectsOverflowOnLoad(t *testing.T) {
	for _, setting := range []string{
		"retention-days: 106752", "retention-days: 213504",
		fmt.Sprintf("max-total-size-mb: %d", MaxImageArtifactCacheSizeMB+1),
	} {
		t.Run(
			setting, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(
					path, []byte("port: 8317\nimage-artifact-cache:\n  "+setting+"\n"), 0600,
				); err != nil {
					t.Fatal(err)
				}
				if _, err := LoadConfig(path); err == nil {
					t.Fatal("overflow configuration was accepted")
				}
			},
		)
	}
}

func TestLoadWithFallbackRejectsInvalidPortWithoutOverwritingLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	good := []byte("port: 8317\n")
	if err := os.WriteFile(LastGoodPath(path), good, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("port: 65536\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, fallback, err := LoadWithFallback(path)
	if err != nil || !fallback || cfg.Port != 8317 {
		t.Fatalf("fallback=%v cfg=%+v err=%v", fallback, cfg, err)
	}
	data, err := os.ReadFile(LastGoodPath(path))
	if err != nil || string(data) != string(good) {
		t.Fatalf("last-good=%s err=%v", data, err)
	}
}

func TestDisableImageGenerationLegacyAliasesAndFourModes(t *testing.T) {
	for input, expected := range map[string]DisableImageGenerationMode{
		"on": DisableImageGenerationAll, "yes": DisableImageGenerationAll, "no": DisableImageGenerationOff,
		"false": DisableImageGenerationOff, "true": DisableImageGenerationAll, "off": DisableImageGenerationOff,
		"all": DisableImageGenerationAll, "chat": DisableImageGenerationChat,
		"passthrough": DisableImageGenerationPassthrough,
	} {
		t.Run(
			input, func(t *testing.T) {
				var fromJSON, fromYAML DisableImageGenerationMode
				encoded, _ := json.Marshal(input)
				if err := json.Unmarshal(encoded, &fromJSON); err != nil {
					t.Fatal(err)
				}
				if err := yaml.Unmarshal([]byte(input), &fromYAML); err != nil {
					t.Fatal(err)
				}
				if fromJSON != expected || fromYAML != expected {
					t.Fatalf("JSON=%v YAML=%v want=%v", fromJSON, fromYAML, expected)
				}
				wantJSON := fmt.Sprintf("%q", expected.String())
				if expected == DisableImageGenerationOff {
					wantJSON = "false"
				} else if expected == DisableImageGenerationAll {
					wantJSON = "true"
				}
				result, err := json.Marshal(fromJSON)
				if err != nil || string(result) != wantJSON {
					t.Fatalf("output=%s want=%s err=%v", result, wantJSON, err)
				}
			},
		)
	}
}

func TestPluginConfigDefaultDisabledAndTypedMarshal(t *testing.T) {
	for _, input := range []string{"{}", "enabled: false", "enabled: true"} {
		var cfg PluginInstanceConfig
		if err := yaml.Unmarshal([]byte(input), &cfg); err != nil {
			t.Fatal(err)
		}
		want := strings.Contains(input, "true")
		if cfg.Enabled == nil || *cfg.Enabled != want {
			t.Fatalf("input=%q enabled=%v", input, cfg.Enabled)
		}
		enabled := !want
		cfg.Enabled = &enabled
		result, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var readback PluginInstanceConfig
		if err := yaml.Unmarshal(
			result, &readback,
		); err != nil || readback.Enabled == nil || *readback.Enabled != enabled {
			t.Fatalf("readback=%s err=%v", result, err)
		}
	}
}

func TestSaveConfigMappingAliasPreservesInheritedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	initial := []byte(("defaults: &defaults\n  user-agent: original-agent\n  beta-features: original-beta\n" +
		"codex-header-defaults: *defaults\nport: 8317\n"))
	if err := os.WriteFile(path, initial, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.CodexHeaderDefaults.UserAgent = "changed-agent"
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	readback, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if readback.CodexHeaderDefaults.UserAgent != "changed-agent" ||
		readback.CodexHeaderDefaults.BetaFeatures != "original-beta" {
		t.Fatalf("headers=%+v", readback.CodexHeaderDefaults)
	}
	var document map[string]any
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document["defaults"].(map[string]any)["user-agent"] != "original-agent" {
		t.Fatalf("anchor target changed: %s", data)
	}
}

func TestCodexHeaderDefaultsTrim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(
		path, []byte("port: 8317\ncodex-header-defaults:\n  user-agent: '  agent  '\n  beta-features: '  feature  '\n"),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CodexHeaderDefaults.UserAgent != "agent" || cfg.CodexHeaderDefaults.BetaFeatures != "feature" {
		t.Fatalf("headers=%+v", cfg.CodexHeaderDefaults)
	}
}

func TestVertexForceMappingYAMLRoundTrip(t *testing.T) {
	var model VertexCompatModel
	if err := yaml.Unmarshal([]byte("name: upstream\nalias: public\nforce-mapping: true\n"), &model); err != nil {
		t.Fatal(err)
	}
	if !model.GetForceMapping() {
		t.Fatal("force mapping missing from optional resolver interface")
	}
	data, err := yaml.Marshal(model)
	if err != nil || !strings.Contains(string(data), "force-mapping: true") {
		t.Fatalf("output=%s err=%v", data, err)
	}
}
