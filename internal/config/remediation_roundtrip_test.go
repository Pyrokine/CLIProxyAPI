package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigSavePreservesExplicitCloakAndUnknownMappings(t *testing.T) {
	for _, cloak := range []string{"{}", "null"} {
		t.Run(
			cloak, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				initial := ("port: 8317\ncustom-root:\n  enabled: false\nclaude-api-key:\n  - " +
					"api-key: fixture-key\n    cloak: ") +
					cloak +
					"\n    custom-extension:\n      empty: {}\n      values: [one, two]\n"
				if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
					t.Fatal(err)
				}
				cfg, err := LoadConfig(path)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Debug = true
				if err := SaveConfigPreserveComments(path, cfg); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var result map[string]any
				if err := yaml.Unmarshal(data, &result); err != nil {
					t.Fatal(err)
				}
				entry := result["claude-api-key"].([]any)[0].(map[string]any)
				if _, ok := entry["custom-extension"]; !ok {
					t.Fatalf("extension removed: %s", data)
				}
				if _, ok := entry["cloak"]; !ok {
					t.Fatalf("explicit cloak removed: %s", data)
				}
				if _, ok := result["custom-root"]; !ok {
					t.Fatalf("root mapping removed: %s", data)
				}
			},
		)
	}
}
func TestEnsureDefaultsCompletesStartupWithoutExpandingOrdinarySave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("port: 8317\n# retain this comment\ncustom: {value: 3}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "panel-github-repository:") || strings.Contains(
		string(data), "error-logs-max-files:",
	) {
		t.Fatalf("ordinary save expanded known defaults: %s", data)
	}
	EnsureDefaultsInFile(path, cfg)
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"panel-github-repository: " + DefaultPanelGitHubRepository,
		"cpa-github-repository: " + defaultCPAGitHubRepository, "addr: " + DefaultPprofAddr, "error-logs-max-files: 10",
		"debug: false", "# retain this comment", "custom:",
	} {
		if !strings.Contains(string(data), value) {
			t.Fatalf("missing %q in %s", value, data)
		}
	}
}
func TestNestedScalarSavePreservesAliasInheritance(t *testing.T) {
	for _, path := range [][]string{{"one", "value"}, {"outer", "one", "value"}} {
		filename := filepath.Join(t.TempDir(), "config.yaml")
		initial := "base: &base {value: before, inherited: retained}\none: *base\nouter: {one: *base}\n"
		if err := os.WriteFile(filename, []byte(initial), 0600); err != nil {
			t.Fatal(err)
		}
		if err := SaveConfigPreserveCommentsUpdateNestedScalar(filename, path, "after"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := yaml.Unmarshal(data, &result); err != nil {
			t.Fatal(err)
		}
		target := result
		for _, key := range path[:len(path)-1] {
			target = target[key].(map[string]any)
		}
		if target["value"] != "after" || target["inherited"] != "retained" {
			t.Fatalf("target=%v", target)
		}
		if result["base"].(map[string]any)["value"] != "before" {
			t.Fatal("anchor changed")
		}
	}
}
func TestXAIAndMetaWeightBoundaryFixtures(t *testing.T) {
	for _, family := range []string{"xai-api-key", "meta-api-key"} {
		for _, weight := range []string{"0", "-1", "1000001", "1.5", "9223372036854775808"} {
			path := filepath.Join(t.TempDir(), "config.yaml")
			raw := ("port: " +
				"8317\n") +
				family +
				(":\n  - api-key: synthetic-key\n    base-url: https://" +
					"example.test\n    weight: ") +
				weight +
				"\n"
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(path)
			if weight == "0" || weight == "-1" {
				if err != nil {
					t.Fatalf("%s explicit zero: %v", family, err)
				}
				if weight == "0" && family == "xai-api-key" &&
					(cfg.XAIKey[0].Weight == nil || *cfg.XAIKey[0].Weight != 0) {
					t.Fatal("XAI zero missing")
				}
				if weight == "0" && family == "meta-api-key" &&
					(cfg.MetaKey[0].Weight == nil || *cfg.MetaKey[0].Weight != 0) {
					t.Fatal("Meta zero missing")
				}
			} else if err == nil {
				t.Fatalf("%s accepted weight %s", family, weight)
			}
		}
	}
}
