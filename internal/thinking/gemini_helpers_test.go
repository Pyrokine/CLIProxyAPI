package thinking

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestApplyGeminiLevelFormatModeNoneZeroDeletesThinkingConfig(t *testing.T) {
	body := []byte(`{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","includeThoughts":true},"temperature":0.2}}`)
	out, err := ApplyGeminiLevelFormat(body, Config{Mode: ModeNone}, GeminiDirectPrefix)
	if err != nil {
		t.Fatalf("ApplyGeminiLevelFormat() error = %v", err)
	}
	if gjson.GetBytes(out, GeminiDirectPrefix).Exists() {
		t.Fatalf("thinkingConfig should be removed: %s", string(out))
	}
	if got := gjson.GetBytes(out, "generationConfig.temperature").Float(); got != 0.2 {
		t.Fatalf("temperature = %v, want 0.2", got)
	}
}

func TestApplyGeminiBudgetFormatModeNoneZeroDeletesThinkingConfig(t *testing.T) {
	body := []byte(`{"request":{"generationConfig":{"thinkingConfig":{"thinkingBudget":1024,"includeThoughts":true},"temperature":0.2}}}`)
	out, err := ApplyGeminiBudgetFormat(body, Config{Mode: ModeNone}, 0, GeminiCLIPrefix)
	if err != nil {
		t.Fatalf("ApplyGeminiBudgetFormat() error = %v", err)
	}
	if gjson.GetBytes(out, GeminiCLIPrefix).Exists() {
		t.Fatalf("thinkingConfig should be removed: %s", string(out))
	}
	if got := gjson.GetBytes(out, "request.generationConfig.temperature").Float(); got != 0.2 {
		t.Fatalf("temperature = %v, want 0.2", got)
	}
}
