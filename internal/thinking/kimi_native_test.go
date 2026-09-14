package thinking

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestValidateConfigClampsKimiLevelFallbacks(t *testing.T) {
	modelInfo := &registry.ModelInfo{
		ID: "kimi-level-only",
		Thinking: &registry.ThinkingSupport{
			ZeroAllowed: false,
			Levels:      []string{"low", "high"},
		},
	}

	auto, errAuto := validateConfig(
		Config{Mode: ModeAuto, Budget: -1}, modelInfo, "openai", "kimi", false,
	)
	if errAuto != nil {
		t.Fatalf("validate auto error: %v", errAuto)
	}
	if auto.Mode != ModeLevel || auto.Level != levelLow {
		t.Fatalf("auto config = %+v, want level low", auto)
	}

	disabled, errDisabled := validateConfig(
		Config{Mode: ModeNone}, modelInfo, "openai", "kimi", false,
	)
	if errDisabled != nil {
		t.Fatalf("validate disabled error: %v", errDisabled)
	}
	if disabled.Mode != ModeNone || disabled.Level != levelLow {
		t.Fatalf("disabled config = %+v, want ModeNone with low fallback", disabled)
	}
}
