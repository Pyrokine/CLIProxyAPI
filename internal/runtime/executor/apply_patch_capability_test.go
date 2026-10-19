package executor

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestNativeApplyPatchCapability(t *testing.T) {
	for _, tc := range []struct {
		name     string
		executor coreauth.ProviderExecutor
		want     bool
	}{
		{name: "codex HTTP", executor: newCodexExecutor(&config.Config{}), want: true},
		{name: "codex auto", executor: NewCodexAutoExecutor(&config.Config{}), want: true},
		{name: "codex nil", executor: (*codexExecutor)(nil)},
		{name: "codex incomplete auto", executor: &CodexAutoExecutor{}},
		{name: "xai HTTP", executor: newXAIExecutor(&config.Config{}), want: true},
		{name: "xai auto", executor: NewXAIAutoExecutor(&config.Config{}), want: true},
		{name: "xai nil", executor: (*XAIExecutor)(nil)},
		{name: "xai incomplete auto", executor: &XAIAutoExecutor{}},
		{name: "xai incomplete websocket", executor: &XAIAutoExecutor{httpExec: newXAIExecutor(&config.Config{}), wsExec: &XAIWebsocketsExecutor{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			support, declared := tc.executor.(coreauth.ApplyPatchSupport)
			if got := declared && support.SupportsApplyPatch(); got != tc.want {
				t.Fatalf("capability = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestTranslatedApplyPatchCapability(t *testing.T) {
	cfg := &config.Config{}
	for _, tc := range []struct {
		name     string
		executor coreauth.ProviderExecutor
		want     bool
	}{
		{"claude", NewClaudeExecutor(cfg), true},
		{"claude nil", (*ClaudeExecutor)(nil), false},
		{"gemini", NewGeminiExecutor(cfg), true},
		{"gemini interactions", NewGeminiInteractionsExecutor(cfg), true},
		{"gemini nil", (*GeminiExecutor)(nil), false},
		{"gemini-cli", NewGeminiCLIExecutor(cfg), true},
		{"gemini-cli nil", (*GeminiCLIExecutor)(nil), false},
		{"vertex", NewGeminiVertexExecutor(cfg), true},
		{"vertex nil", (*GeminiVertexExecutor)(nil), false},
		{"antigravity", NewAntigravityExecutor(cfg), true},
		{"antigravity nil", (*AntigravityExecutor)(nil), false},
		{"aistudio", NewAIStudioExecutor(cfg, "aistudio", nil), true},
		{"aistudio nil", (*AIStudioExecutor)(nil), false},
		{"chat", NewOpenAICompatExecutor("offline-chat", cfg), true},
		{"chat nil", (*OpenAICompatExecutor)(nil), false},
		{"qwen", NewQwenExecutor(cfg), true},
		{"qwen nil", (*QwenExecutor)(nil), false},
		{"iflow", NewIFlowExecutor(cfg), true},
		{"iflow nil", (*IFlowExecutor)(nil), false},
		{"devin", NewDevinExecutor(cfg), true},
		{"devin nil", (*DevinExecutor)(nil), false},
		{"kimi native responses", NewKimiExecutor(cfg), false},
		{"kimi-ai native responses", NewKimiAIExecutor(cfg), false},
		{"kimi nil", (*KimiExecutor)(nil), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			support, declared := tc.executor.(coreauth.ApplyPatchSupport)
			if got := declared && support.SupportsApplyPatch(); got != tc.want {
				t.Fatalf("capability = %t, want %t", got, tc.want)
			}
			if tc.want {
				manager := coreauth.NewManager(nil, nil, nil)
				manager.RegisterExecutor(tc.executor)
				if !manager.SupportsApplyPatchForProviders([]string{tc.executor.Identifier()}) {
					t.Fatal("manager did not consume executor capability")
				}
			}
		})
	}
}
