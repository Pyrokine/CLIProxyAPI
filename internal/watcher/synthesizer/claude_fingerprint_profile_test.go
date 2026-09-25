package synthesizer

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestClaudeFingerprintConfigIdentity(t *testing.T) {
	entries := []config.ClaudeKey{
		{APIKey: "synthetic", BaseURL: "https://example.test"},
		{APIKey: "synthetic", BaseURL: "https://example.test", FingerprintProfile: "oauth-cli"},
	}
	synthesize := func(keys []config.ClaudeKey) map[string]string {
		t.Helper()
		auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{
			Config: &config.Config{ClaudeKey: keys}, Now: time.Unix(100, 0), IDGenerator: NewStableIDGenerator(),
		})
		if err != nil {
			t.Fatal(err)
		}
		out := make(map[string]string)
		for _, auth := range auths {
			profile, exists := auth.Attributes[coreauth.AttributeClaudeFingerprintProfile]
			if !exists {
				t.Fatal("default profile must be explicitly projected")
			}
			out[profile] = auth.ID
		}
		return out
	}
	ids := synthesize(entries)
	if len(ids) != 2 || ids[""] == ids["claude-code-cli"] {
		t.Fatalf("profiles share an identity: %v", ids)
	}
	legacyID, _ := NewStableIDGenerator().next("claude:apikey", "synthetic", "https://example.test")
	if ids[""] != legacyID {
		t.Fatal("default profile changed the existing stable identity")
	}
	entries[1].FingerprintProfile = "claude-code-cli"
	reordered := synthesize([]config.ClaudeKey{entries[1], entries[0]})
	if ids[""] != reordered[""] || ids["claude-code-cli"] != reordered["claude-code-cli"] {
		t.Fatal("reordering or normalizing changed identity")
	}
}

func TestClaudeFingerprintFileReload(t *testing.T) {
	ctx := &SynthesisContext{AuthDir: t.TempDir(), Config: &config.Config{}, Now: time.Unix(100, 0)}
	path := filepath.Join(ctx.AuthDir, "synthetic.json")
	for _, tc := range []struct {
		json, want string
		invalid    bool
	}{
		{`{"type":"claude","fingerprint-profile":"oauth-cli"}`, "claude-code-cli", false},
		{`{"type":"kimi","fingerprint-profile":"oauth-cli"}`, "claude-code-cli", false},
		{`{"type":"kimi","fingerprint_profile":"","fingerprint-profile":"oauth-cli"}`, "", false},
		{`{"type":"claude","fingerprint_profile":null}`, "", true},
		{`{"type":"claude","fingerprint_profile":"unknown"}`, "", true},
	} {
		auths, err := SynthesizeAuthFile(ctx, path, []byte(tc.json))
		if tc.invalid {
			if err == nil {
				t.Fatalf("accepted invalid profile: %s", tc.json)
			}
			continue
		}
		if err != nil || len(auths) != 1 {
			t.Fatalf("synthesis failed: %v", err)
		}
		auth := auths[0]
		if auth.Attributes[coreauth.AttributeClaudeFingerprintProfile] != tc.want || auth.Metadata[coreauth.AttributeClaudeFingerprintProfile] != tc.want {
			t.Fatalf("metadata and attributes disagree: %v, %v", auth.Metadata, auth.Attributes)
		}
		if _, exists := auth.Metadata["fingerprint-profile"]; exists {
			t.Fatal("legacy key remains after reload")
		}
	}
}
