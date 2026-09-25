package synthesizer

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestCodexCoolingIdentityCompatibility(t *testing.T) {
	for _, provider := range []string{"codex", "xai"} {
		for _, priority := range []int{0, 4} {
			for _, cooling := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/priority_%d/cooling_%t", provider, priority, cooling), func(t *testing.T) {
					fields := ""
					if cooling {
						fields = `,"disable-cooling":true`
					}
					var cfg config.Config
					if err := json.Unmarshal([]byte(fmt.Sprintf(`{"%s-api-key":[{"api-key":"synthetic-key","base-url":"https://example.test","priority":%d%s}]}`, provider, priority, fields)), &cfg); err != nil {
						t.Fatal(err)
					}
					auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{Config: &cfg, Now: time.Unix(100, 0), IDGenerator: NewStableIDGenerator()})
					if err != nil || len(auths) != 1 {
						t.Fatalf("synthesize: auths=%v err=%v", auths, err)
					}
					parts := []string{"synthetic-key", "https://example.test"}
					if priority != 0 || cooling {
						parts = append(parts, fmt.Sprintf(`[%d,null,"","","",null,"",false,%t]`, priority, cooling))
					}
					want, _ := NewStableIDGenerator().next(provider+":apikey", parts...)
					if auths[0].ID != want {
						t.Fatalf("id=%s, want legacy id=%s", auths[0].ID, want)
					}
				})
			}
		}
	}
}

func TestCredentialOverrideIdentitySurvivesReorder(t *testing.T) {
	for _, provider := range []string{"claude-api-key", "codex-api-key", "xai-api-key", "gemini-api-key", "interactions-api-key", "vertex-api-key", "openai-compatibility"} {
		t.Run(provider, func(t *testing.T) {
			entries := []string{
				`{"name":"example","api-key":"synthetic-key"}`,
				`{"name":"example","api-key":"synthetic-key","disable-cooling":false,"request-retry":0}`,
				`{"name":"example","api-key":"synthetic-key","disable-cooling":true,"request-retry":2}`,
				`{"name":"example","api-key":"synthetic-key","request-retry":3}`,
			}
			synthesize := func(order []int) map[string]string {
				t.Helper()
				raw := make([]json.RawMessage, len(order))
				for i, index := range order {
					raw[i] = json.RawMessage(entries[index])
				}
				data, err := json.Marshal(map[string]any{provider: raw})
				if err != nil {
					t.Fatal(err)
				}
				var cfg config.Config
				if err := json.Unmarshal(data, &cfg); err != nil {
					t.Fatal(err)
				}
				auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{Config: &cfg, Now: time.Unix(100, 0), IDGenerator: NewStableIDGenerator()})
				if err != nil || len(auths) != len(order) {
					t.Fatalf("synthesize: count=%d err=%v", len(auths), err)
				}
				ids := map[string]string{}
				seen := map[string]bool{}
				for _, auth := range auths {
					cooling, hasCooling := auth.DisableCoolingOverride()
					retry, hasRetry := auth.RequestRetryOverride()
					key := fmt.Sprintf("%t:%t/%d:%t", cooling, hasCooling, retry, hasRetry)
					ids[key] = auth.ID
					if seen[auth.ID] {
						t.Fatal("distinct overrides share an auth ID")
					}
					seen[auth.ID] = true
				}
				return ids
			}
			original, reordered := synthesize([]int{0, 1, 2, 3}), synthesize([]int{3, 2, 1, 0})
			for key, id := range original {
				if reordered[key] != id {
					t.Fatalf("reorder changed %s: %s -> %s", key, id, reordered[key])
				}
			}
		})
	}
}

func TestConfigCredentialOverrideProjection(t *testing.T) {
	for _, provider := range []string{"claude-api-key", "codex-api-key", "xai-api-key", "gemini-api-key", "interactions-api-key", "vertex-api-key", "openai-compatibility"} {
		for _, tc := range []struct {
			name, fields string
			cooling      bool
			hasCooling   bool
			retry        int
			hasRetry     bool
		}{
			{name: "inherit"},
			{name: "false_zero", fields: `,"disable-cooling":false,"request-retry":0`, hasCooling: true, hasRetry: true},
			{name: "true_negative", fields: `,"disable-cooling":true,"request-retry":-1`, cooling: true, hasCooling: true},
			{name: "false_positive", fields: `,"disable-cooling":false,"request-retry":2`, hasCooling: true, retry: 2, hasRetry: true},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				var cfg config.Config
				input := fmt.Sprintf(`{"%s":[{"name":"example","api-key":"synthetic-key"%s}]}`, provider, tc.fields)
				if err := json.Unmarshal([]byte(input), &cfg); err != nil {
					t.Fatal(err)
				}
				// OpenAI-compatible 同时验证有 key 和无 key 的投影
				if provider == "openai-compatibility" {
					withKey := cfg.OpenAICompatibility[0]
					withKey.Name = "with-key"
					withKey.APIKeyEntries = []config.OpenAICompatibilityAPIKey{{APIKey: "synthetic-key"}}
					cfg.OpenAICompatibility = append(cfg.OpenAICompatibility, withKey)
				}
				auths, err := NewConfigSynthesizer().Synthesize(&SynthesisContext{Config: &cfg, Now: time.Unix(100, 0), IDGenerator: NewStableIDGenerator()})
				if err != nil || len(auths) == 0 {
					t.Fatalf("synthesize: auths=%v err=%v", auths, err)
				}
				for _, auth := range auths {
					cooling, hasCooling := auth.DisableCoolingOverride()
					retry, hasRetry := auth.RequestRetryOverride()
					if cooling != tc.cooling || hasCooling != tc.hasCooling || retry != tc.retry || hasRetry != tc.hasRetry {
						t.Fatalf("cooling=(%t,%t) retry=(%d,%t), want (%t,%t) (%d,%t)", cooling, hasCooling, retry, hasRetry, tc.cooling, tc.hasCooling, tc.retry, tc.hasRetry)
					}
				}
			})
		}
	}
}
