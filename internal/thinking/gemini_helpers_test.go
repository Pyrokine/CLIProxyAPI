package thinking

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestApplyGeminiFormatsDoNotInventSummaryVisibility(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		config Config
		apply  func([]byte, Config) ([]byte, error)
		path   string
	}{
		{
			name:   "level",
			body:   `{}`,
			config: Config{Mode: ModeLevel, Level: LevelHigh},
			apply: func(body []byte, config Config) ([]byte, error) {
				return ApplyGeminiLevelFormat(body, config, GeminiDirectPrefix)
			},
			path: GeminiDirectPrefix + ".includeThoughts",
		},
		{
			name:   "budget",
			body:   `{}`,
			config: Config{Mode: ModeBudget, Budget: 2048},
			apply: func(body []byte, config Config) ([]byte, error) {
				return ApplyGeminiBudgetFormat(body, config, config.Budget, GeminiCLIPrefix)
			},
			path: GeminiCLIPrefix + ".includeThoughts",
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				out, err := test.apply([]byte(test.body), test.config)
				if err != nil {
					t.Fatalf("apply error = %v", err)
				}
				if gjson.GetBytes(out, test.path).Exists() {
					t.Fatalf("thinking amount invented summary visibility: %s", out)
				}
			},
		)
	}
}

func TestApplyGeminiFormatsOnlyPreserveBooleanSummary(t *testing.T) {
	for _, value := range []string{`"true"`, `1`, `null`} {
		body := []byte(`{"generationConfig":{"thinkingConfig":{"includeThoughts":` + value + `}}}`)
		out, err := ApplyGeminiLevelFormat(body, Config{Mode: ModeLevel, Level: LevelHigh}, GeminiDirectPrefix)
		if err != nil {
			t.Fatalf("ApplyGeminiLevelFormat() error = %v", err)
		}
		if gjson.GetBytes(out, GeminiDirectPrefix+".includeThoughts").Exists() {
			t.Fatalf("non-boolean includeThoughts %s survived normalization: %s", value, out)
		}
	}
}

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
