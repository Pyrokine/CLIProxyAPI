package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeToolChangesShareWireNames(t *testing.T) {
	const tools = `[{"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{}}},{"name":"lookup_notes","description":"Look up notes","input_schema":{"type":"object","properties":{}},"defer_loading":true},{"type":"web_search_20250305","name":"web_search"},{"name":"mcp__context7__query-docs","input_schema":{"type":"object"}}]`
	for _, test := range []struct {
		name, messages, path, declaration string
	}{
		{"addition", `[{"role":"user","content":"Reply with exactly: ok"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_reference","name":"lookup_notes"}}]}]`, "messages.1.content.0.tool.name", "tools.1.name"},
		{"removal", `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"read_file"}}]}]`, "messages.1.content.0.tool.name", "tools.0.name"},
		{"inline replacement", `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"lookup_notes","description":"v2","input_schema":{"type":"object"}}}}]}]`, "messages.1.content.0.tool.definition.name", "tools.1.name"},
		{"server control", `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_addition","tool":{"type":"tool_reference","name":"web_search"}}]}]`, "messages.1.content.0.tool.name", "tools.2.name"},
		{"caller MCP control", `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"tool_removal","tool":{"type":"tool_reference","name":"mcp__context7__query-docs"}}]}]`, "messages.1.content.0.tool.name", "tools.3.name"},
	} {
		for _, operation := range []string{"messages", "stream", "count"} {
			t.Run(test.name+"/"+operation, func(t *testing.T) {
				payload := []byte(`{"model":"claude-opus-5-5","max_tokens":64,"tools":` + tools + `,"messages":` + test.messages + `}`)
				var sent []byte
				var headers http.Header
				calls := 0
				ctx := util.WithRoundTripper(context.Background(), claudeContractTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.URL.Host != "api.anthropic.com" {
						return nil, fmt.Errorf("unexpected synthetic URL: %s", req.URL)
					}
					var err error
					sent, err = io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					headers = req.Header.Clone()
					return claudeContractResponse(req, sent), nil
				}))
				auth := &cliproxyauth.Auth{
					ID:         "final-tool-change-" + test.name + "-" + operation,
					Provider:   "claude",
					Attributes: map[string]string{"api_key": "sk-ant-oat-final-tool-change-synthetic", "cloak_mode": "always", "base_url": "https://api.anthropic.com"},
					Metadata:   map[string]any{"account_uuid": "11111111-2222-4333-8444-555555555555", "claude_device_ids": []string{"00000000000000000000000000000000000000000000000000000000000000d1"}},
				}
				executeClaudeContractRequest(t, NewClaudeExecutor(&config.Config{}), ctx, auth,
					cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: payload},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, operation)
				if calls != 1 {
					t.Fatalf("transport calls=%d, want 1", calls)
				}
				declaration := gjson.GetBytes(sent, test.declaration).String()
				reference := gjson.GetBytes(sent, test.path).String()
				if declaration == "" || reference == "" {
					t.Fatalf("missing wire tool fields: %s", sent)
				}
				if operation != "count" && !strings.Contains(helps.HeaderValueCaseInsensitive(headers, "Anthropic-Beta"), "mid-conversation-tool-changes-2026-07-01") {
					t.Fatalf("tool-change beta missing: %v", headers)
				}
				if reference != declaration {
					t.Errorf("wire symbol mismatch: declaration=%q, reference=%q, operation=%s", declaration, reference, operation)
				}
			})
		}
	}
}
