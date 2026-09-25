package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClaudeFingerprintProfileDecode(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"", ""}, {"claude-code-cli", "claude-code-cli"}, {"oauth-cli", "claude-code-cli"},
		{"  OAUTH-CLI  ", "claude-code-cli"}, {" CLAUDE-CODE-CLI ", "claude-code-cli"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			encoded, _ := json.Marshal(tc.raw)
			for _, format := range []string{"json", "yaml"} {
				var key ClaudeKey
				var err error
				if format == "json" {
					err = json.Unmarshal([]byte(`{"api-key":"synthetic","fingerprint-profile":`+string(encoded)+`}`), &key)
				} else {
					err = yaml.Unmarshal([]byte("api-key: synthetic\nfingerprint-profile: "+string(encoded)+"\n"), &key)
				}
				if err != nil {
					t.Fatalf("%s: %v", format, err)
				}
				result, _ := json.Marshal(key)
				var fields map[string]any
				_ = json.Unmarshal(result, &fields)
				if got, _ := fields["fingerprint-profile"].(string); got != tc.want {
					t.Fatalf("%s profile = %q, want %q", format, got, tc.want)
				}
			}
		})
	}
}

func TestClaudeFingerprintProfileRejectsInvalidConfig(t *testing.T) {
	for _, raw := range []string{`"typo"`, `"oauth"`, `true`, `42`, `null`, `[]`, `{}`} {
		t.Run(raw, func(t *testing.T) {
			data := []byte("claude-api-key:\n  - api-key: synthetic\n    fingerprint-profile: " + raw + "\n")
			if _, err := ParseConfigBytes(data); err == nil || !strings.Contains(err.Error(), "fingerprint-profile") {
				t.Fatalf("ParseConfigBytes accepted %s: %v", raw, err)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfigOptional(path, false); err == nil || !strings.Contains(err.Error(), "fingerprint-profile") {
				t.Fatalf("LoadConfigOptional accepted %s: %v", raw, err)
			}
			var key ClaudeKey
			if err := json.Unmarshal([]byte(`{"api-key":"synthetic","fingerprint-profile":`+raw+`}`), &key); err == nil {
				t.Fatalf("JSON accepted %s", raw)
			}
		})
	}
}

func TestClaudeFingerprintProfileRejectsJSONNullCaseVariants(t *testing.T) {
	for _, field := range []string{"fingerprint-profile", "Fingerprint-Profile", "FINGERPRINT-PROFILE"} {
		key := ClaudeKey{APIKey: "synthetic", FingerprintProfile: "claude-code-cli"}
		if err := json.Unmarshal([]byte(`{"`+field+`":null}`), &key); err == nil {
			t.Errorf("null field %s was accepted", field)
		}
		if key.FingerprintProfile != "claude-code-cli" || key.APIKey != "synthetic" {
			t.Fatal("failed decode changed the existing entry")
		}
	}
}

func TestClaudeFingerprintProfileYAMLAliasAndClone(t *testing.T) {
	data := []byte("defaults: &defaults\n  fingerprint-profile: oauth-cli\nclaude-api-key:\n  - <<: *defaults\n    api-key: synthetic\n")
	cfg, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	clone, err := Clone(cfg)
	if err != nil {
		t.Fatal(err)
	}
	serialized, _ := json.Marshal(clone)
	if !strings.Contains(string(serialized), `"fingerprint-profile":"claude-code-cli"`) {
		t.Fatal("clone lost canonical profile")
	}
	if _, err := ParseConfigBytes([]byte(strings.ReplaceAll(string(data), "oauth-cli", "null"))); err == nil {
		t.Fatal("merged invalid profile was accepted")
	}
}
