package executor

import (
	"strings"
	"testing"
)

func TestResponseModelEvent(t *testing.T) {
	tests := []struct {
		name, provider, payload, want string
		terminal                      bool
	}{
		{"codex created", "codex", `data: {"type":"response.created","response":{"model":"gpt-6-sol"}}`, "gpt-6-sol", false},
		{"codex completed", "codex", `{"type":"response.completed","response":{"model":"gpt-6-luna"}}`, "gpt-6-luna", true},
		{"codex delta ignored", "codex", `{"type":"response.output_text.delta","response":{"model":"wrong"}}`, "", false},
		{"claude start", "claude", `data: {"type":"message_start","message":{"model":"claude-opus-5-5"}}`, "claude-opus-5-5", false},
		{"claude stop", "claude", `{"type":"message_stop"}`, "", true},
		{"gemini", "gemini", `{"candidates":[{"finishReason":"STOP"}],"modelVersion":"gemini-3.5-flash"}`, "gemini-3.5-flash", true},
		{"gemini interactions", "gemini", `{"event_type":"interaction.completed","interaction":{"model":"gemini-3.5-flash","status":"completed"}}`, "gemini-3.5-flash", true},
		{"interactions without model", "gemini", `{"type":"interaction.failed"}`, "", true},
		{"meta", "meta", `{"type":"response.completed","response":{"model":"meta-llama"}}`, "meta-llama", true},
		{"generic interactions", "devin", `{"event_type":"interaction.completed","interaction":{"model":"devin-2","status":"completed"}}`, "devin-2", true},
		{"generic chat completion chunk", "kimi", `{"object":"chat.completion.chunk","model":"kimi-k3","choices":[{"finish_reason":"stop"}]}`, "kimi-k3", true},
		{"chat completion", "kimi", `{"object":"chat.completion","model":"kimi-k3"}`, "kimi-k3", true},
		{"nonstring", "meta", `{"type":"response.completed","response":{"model":7}}`, "", true},
		{"oversized", "meta", `{"type":"response.completed","response":{"model":"` + strings.Repeat("x", 129) + `"}}`, "", true},
		{"control character", "meta", `{"type":"response.completed","response":{"model":"bad\nname"}}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, terminal := responseModelEvent([]byte(tt.payload), tt.provider)
			if model != tt.want || terminal != tt.terminal {
				t.Fatalf("responseModelEvent() = (%q, %t), want (%q, %t)", model, terminal, tt.want, tt.terminal)
			}
		})
	}
}

func TestUsageReporterResponseModelFinal(t *testing.T) {
	reporter := &usageReporter{provider: "codex", model: "gpt-6-sol"}
	reporter.observeResponseModel([]byte(`data: {"type":"response.created","response":{"model":"gpt-6-sol"}}`))
	reporter.observeResponseModel([]byte(`data: {"type":"response.completed","response":{"model":"gpt-6-luna"}}`))
	reporter.observeResponseModel([]byte(`data: {"type":"response.created","response":{"model":"ignored"}}`))
	served, _ := reporter.responseModelSnapshot()
	if served != "gpt-6-luna" {
		t.Fatalf("response model = %q, want gpt-6-luna", served)
	}
}

func TestModelIsSubstituted(t *testing.T) {
	tests := []struct {
		expected, served string
		want             bool
	}{
		{"gpt-6-sol", "gpt-6-sol-20260929", false},
		{"gpt-6-sol-latest", "gpt-6-sol", false},
		{"provider/gpt-6-sol", "gpt-6-sol", false},
		{"gpt-6-sol(8192)", "gpt-6-luna", true},
		{"gpt-6-sol", "gpt-6-luna", true},
		{"", "gpt-6-luna", false},
	}
	for _, tt := range tests {
		if got := modelIsSubstituted(tt.expected, tt.served); got != tt.want {
			t.Errorf("modelIsSubstituted(%q, %q) = %t, want %t", tt.expected, tt.served, got, tt.want)
		}
	}
}

func TestUsageReporterGenericTerminalStatus(t *testing.T) {
	for _, provider := range []string{"openai-compatibility", "meta", "kimi"} {
		for _, tt := range []struct {
			status string
			final  bool
		}{
			{status: "completed", final: true},
			{status: "incomplete", final: true},
			{status: "in_progress"},
			{},
		} {
			t.Run(provider+"/"+tt.status, func(t *testing.T) {
				reporter := &usageReporter{provider: provider, model: "requested-model"}
				reporter.observeResponseModel([]byte(`{"model":"served-final","status":"` + tt.status + `"}`))
				reporter.observeResponseModel([]byte(`{"model":"late-metadata"}`))
				served, _ := reporter.responseModelSnapshot()
				want := "late-metadata"
				if tt.final {
					want = "served-final"
				}
				if served != want || reporter.modelFinal != tt.final {
					t.Fatalf("model = %q final = %v, want %q/%v", served, reporter.modelFinal, want, tt.final)
				}
			})
		}
	}
}
