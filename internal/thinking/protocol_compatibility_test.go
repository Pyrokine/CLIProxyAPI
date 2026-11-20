package thinking_test

import (
	"bytes"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/claude"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/openai"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestThinkingMaxSuffixOverridesEffortAndUsesStandardBudget(t *testing.T) {
	body := []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`)
	info := &registry.ModelInfo{
		ID: "synthetic-known-claude", Type: "claude", MaxCompletionTokens: 64000, Thinking: &registry.ThinkingSupport{
			Levels: []string{"low", "medium", "high", "max"}, ZeroAllowed: true, DynamicAllowed: true,
		},
	}
	out, err := thinking.ApplyThinkingWithModelInfoAndSummary(
		body, body, info.ID+"(max)", "claude", "claude", "claude", info, thinking.SummaryConfig{},
	)
	if err != nil || gjson.GetBytes(out, "output_config.effort").String() != "max" {
		t.Fatalf("max suffix did not override low effort: %s, %v", out, err)
	}
	if budget, ok := thinking.ConvertLevelToBudget("max"); !ok || budget != 128000 {
		t.Fatalf("max standard budget = %d, %t; want 128000", budget, ok)
	}
	for _, limit := range []int{200000, 64000, 16384} {
		budgetInfo := &registry.ModelInfo{
			ID: "synthetic-budget-model", Type: "claude", MaxCompletionTokens: 300000,
			Thinking: &registry.ThinkingSupport{Min: 1, Max: limit, ZeroAllowed: true, DynamicAllowed: true},
		}
		out, err := thinking.ApplyThinkingWithModelInfoAndSummary(
			[]byte(
				`{"max_tokens":300000}`,
			),
			nil,
			budgetInfo.ID+"(max)",
			"openai",
			"claude",
			"claude",
			budgetInfo,
			thinking.SummaryConfig{},
		)
		want := min(128000, limit)
		if err != nil || gjson.GetBytes(out, "thinking.budget_tokens").Int() != int64(want) {
			t.Fatalf("max did not clamp to model limit %d: %s, %v", limit, out, err)
		}
	}
}

func TestThinkingRawClaudeEnabledInputsRetainPublishedContract(t *testing.T) {
	for _, testCase := range []struct{ name, thinking, effort, want string }{
		{"explicit effort", `{"type":"enabled"}`, `,"output_config":{"effort":"high"}`, "high"},
		{
			"legacy budget wins", `{"type":"enabled","budget_tokens":8192}`, `,"output_config":{"effort":"high"}`,
			"medium",
		},
		{"no budget or effort", `{"type":"enabled"}`, "", "auto"},
		{"empty effort", `{"type":"enabled"}`, `,"output_config":{"effort":""}`, "auto"},
		{"whitespace effort", `{"type":"enabled"}`, `,"output_config":{"effort":"   "}`, "auto"},
		{"non-string effort", `{"type":"enabled"}`, `,"output_config":{"effort":123}`, "auto"},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				body := []byte(
					(`{"model":"custom-openai","messages":[{"role":"user","content":"hi"}],` +
						`"thinking":`) +
						testCase.thinking +
						testCase.effort +
						`}`,
				)
				original := bytes.Clone(body)
				out, err := thinking.ApplyThinking(body, "custom-openai", "claude", "openai", "openai")
				if err != nil || gjson.GetBytes(out, "reasoning_effort").String() != testCase.want {
					t.Fatalf(
						"Raw effort = %q, want %q: %s, %v", gjson.GetBytes(out, "reasoning_effort").String(),
						testCase.want, out, err,
					)
				}
				if !bytes.Equal(body, original) || gjson.GetBytes(
					out, "model",
				).String() != "custom-openai" || gjson.GetBytes(out, "messages.0.content").String() != "hi" {
					t.Fatal("Raw thinking changed its source buffer, logical model, or user content")
				}
			},
		)
	}
}

func TestThinkingConvertedTargetFieldsRemainAuthoritative(t *testing.T) {
	ctx := sdktranslator.WithPluginHooks(t.Context(), nil)
	raw := []byte((`{"model":"custom-openai","messages":[{"role":"user","content":"hi"}],"` +
		`thinking":{"type":"enabled"},"output_config":{"effort":"high"}}`))
	translated, err := sdktranslator.TranslateRequestChecked(
		ctx, sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, "custom-openai", raw, false,
	)
	if err != nil || gjson.GetBytes(translated, "reasoning_effort").String() != "high" {
		t.Fatalf("registered SDK converter failed: %s, %v", translated, err)
	}
	out, err := thinking.ApplyThinking(translated, "custom-openai", "claude", "openai", "openai")
	if err != nil || gjson.GetBytes(out, "reasoning_effort").String() != "high" {
		t.Fatalf("SDK conversion → thinking changed effort: %s, %v", out, err)
	}
	mixed := []byte(
		(`{"reasoning_effort":"low","thinking":{"type":"enabled",` +
			`"budget_tokens":8192},"output_config":{"effort":` +
			`"high"}}`),
	)
	out, err = thinking.ApplyThinking(mixed, "custom-openai", "claude", "openai", "openai")
	if err != nil || gjson.GetBytes(out, "reasoning_effort").String() != "low" {
		t.Fatalf("source fallback replaced an existing target setting: %s, %v", out, err)
	}
	reg := sdktranslator.NewRegistry()
	envelope := reg.TranslateRequestEnvelope(
		ctx, "absent-source", "absent-target",
		sdktranslator.RequestEnvelope{Model: "opaque(max)", Body: raw},
	)
	if envelope.Err != nil || envelope.Model != "opaque(max)" || !bytes.Equal(envelope.Body, raw) {
		t.Fatalf("absent converter changed model/body: %+v", envelope)
	}
}

func TestThinkingKimiDottedAliasesPreserveNativePrecedence(t *testing.T) {
	for _, provider := range []string{"kimi", "kimi-ai", "kimi.ai", "kimi.com"} {
		t.Run(
			provider, func(t *testing.T) {
				for _, testCase := range []struct{ body, want string }{
					{`{"thinking":{"type":"enabled","effort":"high"},"reasoning_effort":"low"}`, "high"},
					{`{"thinking":{"type":"disabled","effort":"high"},"reasoning_effort":"low"}`, "none"},
					{`{"thinking":{"type":"enabled"},"reasoning_effort":"low"}`, ""},
				} {
					if got := thinking.ExtractTranslatedReasoningEffort(
						[]byte(testCase.body), provider,
					); got != testCase.want {
						t.Fatalf("%s effort = %q, want %q", provider, got, testCase.want)
					}
				}
			},
		)
	}
}
