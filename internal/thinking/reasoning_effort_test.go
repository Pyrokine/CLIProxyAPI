package thinking

import "testing"

func TestExtractReasoningEffortUsesSuffixOverBody(t *testing.T) {
	got := ExtractReasoningEffort([]byte(`{"reasoning_effort":"low"}`), "openai", "gpt-5.4(high)")
	if got != "high" {
		t.Fatalf("ExtractReasoningEffort() = %q, want %q", got, "high")
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
				got := ExtractTranslatedReasoningEffort([]byte(test.body), "kimi")
				if got != test.want {
					t.Fatalf("ExtractTranslatedReasoningEffort() = %q, want %q", got, test.want)
				}
			},
		)
	}
}
