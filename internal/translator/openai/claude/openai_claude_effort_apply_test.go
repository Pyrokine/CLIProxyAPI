package claude

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking/provider/openai"
	"github.com/tidwall/gjson"
)

func TestClaudeOpenAIEnabledEffortFinalPayload(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		thinking string
		effort   string
		model    string
		want     string
	}{
		{"enabled high", `{"type":"enabled"}`, `"high"`, "custom-effort-test", "high"},
		{"enabled normalized", `{"type":"enabled"}`, `" HIGH "`, "custom-effort-test", "high"},
		{"enabled low", `{"type":"enabled"}`, `"low"`, "custom-effort-test", "low"},
		{"enabled auto", `{"type":"enabled"}`, `"auto"`, "custom-effort-test", "auto"},
		{"enabled missing effort", `{"type":"enabled"}`, `null`, "custom-effort-test", "auto"},
		{"enabled empty effort", `{"type":"enabled"}`, `" "`, "custom-effort-test", "auto"},
		{"enabled non-string effort", `{"type":"enabled"}`, `123`, "custom-effort-test", "auto"},
		{"explicit budget wins", `{"type":"enabled","budget_tokens":1024}`, `"high"`, "custom-effort-test", "low"},
		{"explicit auto budget wins", `{"type":"enabled","budget_tokens":-1}`, `"high"`, "custom-effort-test", "auto"},
		{"explicit zero budget wins", `{"type":"enabled","budget_tokens":0}`, `"high"`, "custom-effort-test", "none"},
		{"disabled wins", `{"type":"disabled"}`, `"high"`, "custom-effort-test", "none"},
		{"adaptive explicit effort", `{"type":"adaptive"}`, `"high"`, "custom-effort-test", "high"},
		{"adaptive default", `{"type":"adaptive"}`, `null`, "custom-effort-test", "xhigh"},
		{"suffix wins", `{"type":"enabled"}`, `"high"`, "custom-effort-test(low)", "low"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			source := []byte(`{"messages":[{"role":"user","content":"hi"}],"thinking":` + testCase.thinking + `,"output_config":{"effort":` + testCase.effort + `}}`)
			for _, bound := range []bool{false, true} {
				body, err := convertClaudeRequestToOpenAIChecked(testCase.model, source, false, false)
				if err != nil {
					t.Fatal(err)
				}
				var modelInfo *registry.ModelInfo
				if bound {
					modelInfo = &registry.ModelInfo{ID: "custom-effort-test", UserDefined: true}
				}
				final, err := thinking.ApplyThinkingWithModelInfoAndSummary(body, source, testCase.model, "claude", "openai", "offline-compat", modelInfo, thinking.SummaryConfig{})
				if err != nil {
					t.Fatal(err)
				}
				if got := gjson.GetBytes(final, "reasoning_effort").String(); got != testCase.want {
					t.Fatalf("bound=%t final reasoning_effort=%q, want %q; translated=%s final=%s", bound, got, testCase.want, body, final)
				}
			}
		})
	}
}
