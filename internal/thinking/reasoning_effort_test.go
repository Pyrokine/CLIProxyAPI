package thinking

import "testing"

func TestExtractReasoningEffortUsesSuffixOverBody(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"reasoning_effort":"low"}`), "openai", "gpt-5.4(high)")
	if got != "high" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "high")
	}
}

func TestExtractReasoningEffortUsesLastValidResponsesUpdate(t *testing.T) {
	for _, provider := range []string{"codex", "xai", "openai-response"} {
		for _, tc := range []struct{ update, want string }{
			{`{"reasoning":{"effort":" HIGH "}}`, "high"},
			{`{"reasoning":{"effort":"none"}}`, "none"},
			{`{"reasoning":{"effort":"auto"}}`, "auto"},
			{`{"reasoning":{"effort":"max"}}`, "max"},
			{`{"reasoning":{"effort":null}}`, "medium"},
			{`{"reasoning":{"effort":1}}`, "medium"},
			{`{"reasoning":{"effort":""}}`, "medium"},
			{`{"reasoning":{"effort":"invalid"}}`, "medium"},
			{`{"tools":[]}`, "medium"},
		} {
			body := []byte(`{"reasoning":{"effort":"xhigh"},"input":[{"type":"configuration_update","reasoning":{"effort":"medium"}},{"type":"configuration_update",` + tc.update[1:len(tc.update)-1] + `}]}`)
			if got := ExtractReasoningEffort(body, provider, "fixture-model(low)"); got != tc.want {
				t.Fatalf("provider=%s update=%s requested effort=%q, want %q", provider, tc.update, got, tc.want)
			}
			if got := ExtractTranslatedReasoningEffort(body, provider); got != tc.want {
				t.Fatalf("provider=%s translated effort=%q, want %q", provider, got, tc.want)
			}
		}
		body := []byte(`{"reasoning":{"effort":"high"},"input":[{"type":"configuration_update","tools":[]}]}`)
		if got := ExtractReasoningEffort(body, provider, "fixture-model(low)"); got != "low" {
			t.Fatalf("update without effort displaced suffix: %q", got)
		}
	}
}

func TestResponsesEffortExtractionRejectsMalformedJSON(t *testing.T) {
	body := []byte(`{"reasoning":{"effort":"xhigh"},"input":`)
	for _, provider := range []string{"codex", "xai", "openai-response"} {
		if got := ExtractReasoningEffort(body, provider, "fixture-model"); got != "" {
			t.Fatalf("provider=%s malformed requested effort=%q", provider, got)
		}
		if got := ExtractTranslatedReasoningEffort(body, provider); got != "" {
			t.Fatalf("provider=%s malformed translated effort=%q", provider, got)
		}
	}
}

func TestClaudeEnabledWithoutBudgetPreservesEffort(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"thinking":{"type":"enabled"},"output_config":{"effort":"low"}}`, "low"},
		{`{"thinking":{"type":"enabled"},"output_config":{"effort":" MAX "}}`, "max"},
		{`{"thinking":{"type":"enabled"},"output_config":{"effort":"none"}}`, "none"},
		{`{"thinking":{"type":"enabled"},"output_config":{"effort":"auto"}}`, "auto"},
		{`{"thinking":{"type":"enabled"},"output_config":{"effort":""}}`, "auto"},
		{`{"thinking":{"type":"enabled"}}`, "auto"},
		{`{"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`, "none"},
		{`{"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"low"}}`, "medium"},
	} {
		if got := ExtractTranslatedReasoningEffort([]byte(tc.body), "claude"); got != tc.want {
			t.Fatalf("body=%s effort=%q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestExtractReasoningEffortConvertsBudgetToLevel(t *testing.T) {
	got := ExtractReasoningEffort(
		[]byte(`{"thinking":{"type":"enabled","budget_tokens":8192}}`), "claude", "claude-sonnet-4-5",
	)
	if got != "medium" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "medium")
	}
}

func TestExtractReasoningEffortSupportsOpenAIResponses(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"reasoning":{"effort":"medium"}}`), "openai-response", "gpt-5.4")
	if got != "medium" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "medium")
	}
}

func TestExtractReasoningEffortMissingConfigIsEmpty(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "openai", "gpt-5.4")
	if got != "" {
		t.Fatalf("ExtractReasoningEffort() = %q, want empty", got)
	}
}

func TestExtractTranslatedReasoningEffortSupportsOpenAIResponses(t *testing.T) {
	got := ExtractTranslatedReasoningEffort([]byte(`{"reasoning":{"effort":"high"}}`), "openai-response")
	if got != "high" {
		t.Fatalf("ExtractTranslatedReasoningEffort() = %q, want %q", got, "high")
	}
}

func TestExtractTranslatedReasoningEffortSupportsKimiNativeThinking(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "native effort wins",
			body: `{"thinking":{"type":"enabled","effort":"high"},"reasoning_effort":"low"}`, want: "high",
		},
		{name: "disabled wins", body: `{"thinking":{"type":"disabled"},"reasoning_effort":"high"}`, want: "none"},
		{
			name: "enabled default stays unspecified",
			body: `{"thinking":{"type":"enabled"},"reasoning_effort":"high"}`, want: "",
		},
		{name: "native auto", body: `{"thinking":{"type":"enabled","effort":"auto"}}`, want: "auto"},
		{name: "legacy fallback", body: `{"reasoning_effort":"high"}`, want: "high"},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				for _, provider := range []string{"kimi", "kimi-ai", " KIMI-AI "} {
					got := ExtractTranslatedReasoningEffort([]byte(test.body), provider)
					if got != test.want {
						t.Fatalf("ExtractTranslatedReasoningEffort(%q) = %q, want %q", provider, got, test.want)
					}
				}
			},
		)
	}
}
