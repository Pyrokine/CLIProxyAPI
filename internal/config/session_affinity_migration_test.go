package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSessionAffinityRetirementMigratesConfiguration(t *testing.T) {
	cases := []struct {
		name, source string
		enabled      bool
	}{
		{"legacy true", "routing: {claude-code-session-affinity: true}", true},
		{"legacy false", "routing: {claude-code-session-affinity: false}", false},
		{"quoted legacy bool", "routing: {claude-code-session-affinity: 'yes'}", true},
		{"explicit false", "routing: {claude-code-session-affinity: true, session-affinity: false}", false},
		{"explicit true", "routing: {claude-code-session-affinity: false, session-affinity: true}", true},
		{"explicit null", "routing: {claude-code-session-affinity: true, session-affinity: null}", false},
		{"unused invalid legacy", "routing: {claude-code-session-affinity: [], session-affinity: false}", false},
		{
			"routing alias", "defaults: &route {claude-code-session-affinity: true, future: retained}\nrouting: *route",
			true,
		},
		{
			"routing merge",
			"defaults: &route {claude-code-session-affinity: true, future: retained" +
				"}\nrouting: {<<: *route, session-affinity: false}",
			false,
		},
		{
			"root merge",
			"defaults: &base {routing: {claude-code-session-affinity: true, future: retained}}\n<<: *base", true,
		},
		{
			"merge sequence",
			"first: &first {claude-code-session-affinity: true}\nsecond: &second {c" +
				"laude-code-session-affinity: false}\nrouting: {<<: [*first, *second]}",
			true,
		},
		{"aliased key", "name: &name claude-code-session-affinity\nrouting: {*name: true}", true},
		{"literal merge key", "routing: {claude-code-session-affinity: true, '<<': retained}", true},
	}
	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				cfg, err := ParseConfigBytes([]byte(tc.source))
				if err != nil || cfg.Routing.SessionAffinity != tc.enabled {
					t.Fatalf("parse enabled=%v error=%v", cfg, err)
				}
				raw, err := json.Marshal(cfg.Routing)
				if err != nil || strings.Contains(string(raw), "claude-code-session-affinity") ||
					!strings.Contains(string(raw), "\"session-affinity\":") {
					t.Fatalf("runtime JSON retained the old field or omitted the new value: %s, %v", raw, err)
				}
				migrated, err := MigrateRoutingSessionAffinityYAML([]byte(tc.source))
				if err != nil {
					t.Fatal(err)
				}
				var before, after map[string]any
				if err := yaml.Unmarshal([]byte(tc.source), &before); err != nil {
					t.Fatal(err)
				}
				if err := yaml.Unmarshal(migrated, &after); err != nil {
					t.Fatal(err)
				}
				routing := after["routing"].(map[string]any)
				if _, exists := routing["claude-code-session-affinity"]; exists ||
					routing["session-affinity"] != tc.enabled {
					t.Fatalf("saved routing = %#v", routing)
				}
				for key, value := range before["routing"].(map[string]any) {
					if key != "session-affinity" && key != "claude-code-session-affinity" && routing[key] != value {
						t.Fatalf("routing extension %s changed: %#v", key, routing)
					}
				}
				if _, err := ParseConfigBytes(migrated); err != nil {
					t.Fatalf("migrated YAML cannot reload: %v", err)
				}
				if again, err := MigrateRoutingSessionAffinityYAML(migrated); err != nil ||
					string(again) != string(migrated) {
					t.Fatalf("migration is not idempotent: %v", err)
				}
			},
		)
	}
}

func TestSessionAffinityRetirementJSONAndInvalidInput(t *testing.T) {
	for _, source := range []string{
		`{"routing":{"claude-code-session-affinity":true}}`,
		`{"routing":{"claude-code-session-affinity":false,"session-affinity":true}}`,
		`{"routing":{"CLAUDE-CODE-SESSION-AFFINITY":true}}`,
	} {
		var cfg Config
		if err := json.Unmarshal([]byte(source), &cfg); err != nil || !cfg.Routing.SessionAffinity {
			t.Fatalf("JSON migration failed: %s, %v", source, err)
		}
	}
	for _, source := range []string{
		`{"routing":{"claude-code-session-affinity":true,"session-affinity":false}}`,
		`{"routing":{"claude-code-session-affinity":[],"session-affinity":false}}`,
		`{"routing":{"claude-code-session-affinity":true,"SESSION-AFFINITY":false}}`,
	} {
		var cfg Config
		if err := json.Unmarshal([]byte(source), &cfg); err != nil || cfg.Routing.SessionAffinity {
			t.Fatalf("new JSON value lost precedence: %s, %v", source, err)
		}
	}
	for _, source := range []string{
		"routing: {claude-code-session-affinity: []}",
		"routing: {claude-code-session-affinity: 'true'}",
		"routing: {claude-code-session-affinity: true, session-affinity: []}",
	} {
		if _, err := ParseConfigBytes([]byte(source)); err == nil {
			t.Fatalf("invalid YAML accepted: %s", source)
		}
	}
	var cfg Config
	if err := json.Unmarshal([]byte(`{"routing":{"session-affinity":"true"}}`), &cfg); err == nil {
		t.Fatal("JSON boolean type validation was weakened")
	}
}

func TestSessionAffinityRetirementFileSaveAndDisable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	source := "# configuration\nrouting:\n  # affinity policy\n  claude-code-session-" +
		"affinity: true\n  future: retained # extension\nfuture-root: retained" +
		"\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil || !cfg.Routing.SessionAffinity {
		t.Fatalf("load migration failed: %v", err)
	}
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Routing.SessionAffinity = false
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, retained := range []string{
		"# configuration", "# affinity policy", "# extension", "future: retained", "future-root: retained",
		"session-affinity: false",
	} {
		if !strings.Contains(string(raw), retained) {
			t.Fatalf("saved configuration lost %q: %s", retained, raw)
		}
	}
	if strings.Contains(string(raw), "claude-code-session-affinity") {
		t.Fatalf("save retained the retired key: %s", raw)
	}
	restored, err := LoadConfig(path)
	if err != nil || restored.Routing.SessionAffinity {
		t.Fatalf("saved disable did not reload: %v", err)
	}
}
