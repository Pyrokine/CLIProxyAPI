package xai

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestApplySetsReasoningEffort(t *testing.T) {
	applier := newApplier()
	modelInfo := &registry.ModelInfo{
		ID:       "grok-4.3",
		Thinking: &registry.ThinkingSupport{ZeroAllowed: true, Levels: []string{"none", "low", "medium", "high"}},
	}

	out, err := applier.Apply(
		[]byte(`{"input":"hello"}`),
		thinking.Config{Mode: thinking.ModeLevel, Level: thinking.LevelHigh},
		modelInfo,
	)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want %q, body=%s", got, "high", out)
	}
}

func TestApplyModeNoneFallsBackToLowestLevelWhenDisableUnsupported(t *testing.T) {
	applier := newApplier()
	modelInfo := &registry.ModelInfo{
		ID:       "grok-3-mini",
		Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}},
	}

	out, err := applier.Apply([]byte(`{"input":"hello"}`), thinking.Config{Mode: thinking.ModeNone}, modelInfo)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if got := gjson.GetBytes(out, "reasoning.effort").String(); got != "low" {
		t.Fatalf("reasoning.effort = %q, want %q, body=%s", got, "low", out)
	}
}
