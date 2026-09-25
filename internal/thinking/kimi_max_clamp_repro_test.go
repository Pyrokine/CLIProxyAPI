package thinking_test

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/claude"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/kimi"
	"github.com/tidwall/gjson"
)

func TestKimiK28ClaudeMessagesMaxPreservesMax(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-kimi-k28-max-" + t.Name()
	reg.RegisterClient(clientID, "kimi", registry.GetKimiModels())
	t.Cleanup(func() { reg.UnregisterClient(clientID) })

	models := []string{
		"kimi-k2.8",
		"k2.8",
		"kimi-k2.8-preview",
		"k2.8-preview",
		"kimi-k2.8-code",
		"k2.8-code",
	}
	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			body := []byte(
				`{"model":"` + model +
					`","messages":[{"role":"user","content":"hi"}],` +
					`"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`,
			)
			out, err := thinking.ApplyThinking(body, model, "claude", "claude", "claude")
			if err != nil {
				t.Fatalf("ApplyThinking() error = %v", err)
			}
			if got := gjson.GetBytes(out, "output_config.effort").String(); got != "max" {
				t.Fatalf("output_config.effort = %q, want max; body=%s", got, out)
			}
		})
	}
}

func TestKimiK25ClaudeMessagesMaxClampsToHigh(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-kimi-k25-max-" + t.Name()
	reg.RegisterClient(clientID, "kimi", registry.GetKimiModels())
	t.Cleanup(func() { reg.UnregisterClient(clientID) })

	body := []byte(
		`{"model":"kimi-k2.5","messages":[{"role":"user","content":"hi"}],` +
			`"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`,
	)
	out, err := thinking.ApplyThinking(body, "kimi-k2.5", "claude", "claude", "claude")
	if err != nil {
		t.Fatalf("ApplyThinking() error = %v", err)
	}
	if got := gjson.GetBytes(out, "thinking.type").String(); got != "adaptive" {
		t.Fatalf("thinking.type = %q, want adaptive; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "output_config.effort").String(); got != "high" {
		t.Fatalf("output_config.effort = %q, want high; body=%s", got, out)
	}
}

func TestUnknownCompatibleModelDoesNotInferMaxThinking(t *testing.T) {
	body := []byte(`{"model":"unknown-compatible","thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`)
	out, err := thinking.ApplyThinkingWithModelInfoAndSummary(
		body,
		nil,
		"unknown-compatible",
		"claude",
		"claude",
		"claude",
		&registry.ModelInfo{ID: "unknown-compatible", IsCompat: true},
		thinking.SummaryConfig{},
	)
	if err != nil {
		t.Fatalf("ApplyThinkingWithModelInfoAndSummary() error = %v", err)
	}
	if gjson.GetBytes(out, "thinking").Exists() || gjson.GetBytes(out, "output_config.effort").Exists() {
		t.Fatalf("unknown compatible model retained inferred thinking metadata: %s", out)
	}
}
