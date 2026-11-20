package executor

import (
	"context"
	"io"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCurrentClaudeModelsPrepareValidRequests(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key": "synthetic-model-refresh", "base_url": "https://example.com",
		},
	}
	for _, test := range []struct{ model, options, wantThinking, wantEffort string }{
		{"claude-fable-5-2", `"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}`, "adaptive", "high"},
		{"claude-haiku-5-5", `"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}`, "adaptive", "max"},
		{"claude-haiku-5-5", `"thinking":{"type":"disabled"},"output_config":{"effort":"high"}`, "disabled", "high"},
		{"claude-haiku-5-5", `"thinking":{"type":"adaptive"},"tool_choice":{"type":"any"}`, "", ""},
	} {
		for _, stream := range []bool{false, true} {
			body := []byte(`{"model":"` + test.model + `","max_tokens":64,"tools":[{"name":"lookup","description":"Lookup fixture","input_schema":{"type":"object","properties":{}}}],"messages":[{"role":"user","content":"hello"}],` + test.options + `,"temperature":1,"top_p":1,"top_k":20}`)
			request, err := executor.prepareClaudeRequest(
				context.Background(), auth,
				cliproxyexecutor.Request{Model: test.model, Payload: body},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, stream,
			)
			if err != nil {
				t.Fatalf("%s stream=%t: %v", test.model, stream, err)
			}
			preparedBody, err := io.ReadAll(request.httpReq.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(preparedBody, "thinking.type").String(); got != test.wantThinking {
				t.Errorf("%s thinking = %q, want %q", test.model, got, test.wantThinking)
			}
			if got := gjson.GetBytes(preparedBody, "output_config.effort").String(); got != test.wantEffort {
				t.Errorf("%s effort = %q, want %q", test.model, got, test.wantEffort)
			}
			for _, key := range []string{"temperature", "top_p", "top_k"} {
				if gjson.GetBytes(preparedBody, key).Exists() {
					t.Errorf("%s retains unsupported %s", test.model, key)
				}
			}
		}
	}
}

func TestCurrentClaudeModelsRejectUnsupportedNativeSettings(t *testing.T) {
	for _, body := range []string{
		`{"model":"claude-fable-5-2","thinking":{"type":"disabled"}}`,
		`{"model":"claude-fable-5-2","thinking":{"type":"enabled","budget_tokens":1024}}`,
		`{"model":"claude-fable-5-2","tool_choice":{"type":"any"}}`,
		`{"model":"claude-fable-5-2","tool_choice":{"type":"tool","name":"search"}}`,
		`{"model":"claude-haiku-5-5","thinking":{"type":"enabled","budget_tokens":1024}}`,
		`{"model":"claude-haiku-5-5","thinking":{"type":"disabled"},"output_config":{"effort":"xhigh"}}`,
		`{"model":"claude-haiku-5-5","thinking":{"type":"disabled"},"output_config":{"effort":"max"}}`,
	} {
		if _, err := normalizeClaudeThinkingRequest([]byte(body), false); err == nil || statusErrCode(err) != 400 {
			t.Errorf("unsupported request %s: %v", body, err)
		}
	}
	if !claudeModelUsesProgressDisplay("claude-fable-5-2") || claudeModelUsesProgressDisplay("claude-haiku-5-5") {
		t.Error("progress updates must distinguish Fable and Haiku")
	}
	if !claudeModelHasPerTurnEffort("claude-fable-5-2") || !claudeModelHasPerTurnEffort("claude-haiku-5-5") {
		t.Error("new models lost per-message effort capability")
	}
}

func TestCurrentGeminiThinkingReachesGenerateContent(t *testing.T) {
	for _, test := range []struct{ model, level string }{
		{"gemini-4-carbon", "high"}, {"gemini-4-argon", "low"}, {"gemini-4-hydrogen", "xhigh"},
	} {
		out, err := thinking.ApplyThinking(
			[]byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
			test.model+"("+test.level+")", "gemini", "gemini", "gemini",
		)
		if err != nil || gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingLevel").String() != test.level ||
			gjson.GetBytes(out, "generationConfig.thinkingConfig.thinkingBudget").Exists() {
			t.Errorf("%s GenerateContent request = %s, error=%v", test.model, out, err)
		}
	}
}

func TestCurrentClaudeNativeLimitsPreserveCompatibilityOptOut(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5-2","thinking":{"type":"disabled"},"temperature":1,"top_k":20}`)
	out, err := normalizeClaudeThinkingRequest(body, true)
	if err != nil || string(out) != string(body) {
		t.Fatalf("compatibility request changed: %s, error=%v", out, err)
	}
	out = normalizeClaudeSamplingForUpstream(body, true, true)
	if string(out) != string(body) {
		t.Fatalf("compatibility sampling changed: %s", out)
	}
}
