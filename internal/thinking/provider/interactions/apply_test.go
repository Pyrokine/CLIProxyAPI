package interactions

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestApplyModeNoneRemovesSummaryVisibilityAliases(t *testing.T) {
	body := []byte(`{
		"generation_config":{
			"thinking_summaries":"auto",
			"thinkingSummaries":"none",
			"thinking_config":{"include_thoughts":true},
			"thinkingConfig":{"includeThoughts":false},
			"temperature":0.2
		},
		"generationConfig":{
			"thinking_summaries":"auto",
			"thinkingSummaries":"none",
			"thinkingConfig":{"includeThoughts":true}
		}
	}`)
	out, err := newApplier().Apply(
		body,
		thinking.Config{Mode: thinking.ModeNone},
		&registry.ModelInfo{Thinking: &registry.ThinkingSupport{ZeroAllowed: true}},
	)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	for _, path := range []string{
		"generation_config.thinking_summaries",
		"generation_config.thinkingSummaries",
		"generation_config.thinking_config",
		"generation_config.thinkingConfig",
		"generationConfig.thinking_summaries",
		"generationConfig.thinkingSummaries",
		"generationConfig.thinkingConfig",
	} {
		if gjson.GetBytes(out, path).Exists() {
			t.Errorf("%s survived fully disabled thinking: %s", path, out)
		}
	}
	if got := gjson.GetBytes(out, "generation_config.temperature").Float(); got != 0.2 {
		t.Fatalf("generation_config.temperature = %v, want 0.2", got)
	}
}

func TestApplyModeNoneFallbackLevelPreservesExplicitSummary(t *testing.T) {
	body := []byte(`{"generation_config":{"thinking_summaries":"auto"}}`)
	out, err := newApplier().Apply(
		body,
		thinking.Config{Mode: thinking.ModeNone, Level: "low"},
		&registry.ModelInfo{Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}}},
	)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := gjson.GetBytes(out, "generation_config.thinking_level").String(); got != "low" {
		t.Fatalf("thinking_level = %q, want low; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "generation_config.thinking_summaries").String(); got != "auto" {
		t.Fatalf("thinking_summaries = %q, want auto; body=%s", got, out)
	}
}
