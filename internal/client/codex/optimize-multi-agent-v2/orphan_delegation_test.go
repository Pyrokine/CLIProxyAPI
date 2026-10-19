package multiagentv2

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestRewriteCodexOrphanDelegationInput(t *testing.T) {
	headers := http.Header{codexOpenAISubagentHeader: {codexCollabSpawnSubagent}}
	tests := []struct {
		name    string
		input   string
		enabled bool
		assert  func(*testing.T, []byte)
	}{
		{
			name:    "orphan create_thread",
			input:   `[{"type":"function_call_output","namespace":"codex_app","name":"create_thread","output":"created"}]`,
			enabled: true,
			assert: func(t *testing.T, output []byte) {
				item := gjson.GetBytes(output, "input.0")
				if item.Get("type").String() != "message" || item.Get("role").String() != "user" {
					t.Fatalf("orphan output was not rewritten: %s", output)
				}
				if got := item.Get("content.0.text").String(); got != "Tool output from codex_app__create_thread:\ncreated" {
					t.Fatalf("message text = %q", got)
				}
			},
		},
		{
			name: "paired call stays paired",
			input: `[` +
				`{"type":"function_call","call_id":"call-1","name":"codex_app__send_message_to_thread"},` +
				`{"type":"function_call_output","call_id":"call-1","namespace":"codex_app","name":"send_message_to_thread","output":{"ok":true}}` +
				`]`,
			enabled: true,
			assert: func(t *testing.T, output []byte) {
				if got := gjson.GetBytes(output, "input.1.type").String(); got != "function_call_output" {
					t.Fatalf("paired output type = %q, want function_call_output: %s", got, output)
				}
			},
		},
		{
			name:    "disabled",
			input:   `[{"type":"function_call_output","namespace":"codex_app","name":"create_thread","output":"created"}]`,
			enabled: false,
			assert: func(t *testing.T, output []byte) {
				if got := gjson.GetBytes(output, "input.0.type").String(); got != "function_call_output" {
					t.Fatalf("disabled rewrite changed type to %q: %s", got, output)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"model":"gpt-5.5","input":` + test.input + `}`)
			output := RewriteCodexOrphanDelegationInput(context.Background(), headers, payload, test.enabled)
			test.assert(t, output)
		})
	}
}

func TestRewriteCodexOrphanDelegationConsumesEachCallOnce(t *testing.T) {
	payload := []byte(`{"input":[` +
		`{"type":"function_call","call_id":"call-1","name":"codex_app__create_thread"},` +
		`{"type":"function_call_output","call_id":"call-1","namespace":"codex_app","name":"create_thread","output":"paired"},` +
		`{"type":"function_call_output","call_id":"call-1","namespace":"codex_app","name":"create_thread","output":"orphan"}` +
		`]}`)
	output := RewriteCodexOrphanDelegationInput(
		context.Background(),
		http.Header{codexOpenAISubagentHeader: {codexCollabSpawnSubagent}},
		payload,
		true,
	)
	if got := gjson.GetBytes(output, "input.1.type").String(); got != "function_call_output" {
		t.Fatalf("first output type = %q, want function_call_output", got)
	}
	if got := gjson.GetBytes(output, "input.2.type").String(); got != "message" {
		t.Fatalf("second output type = %q, want message: %s", got, output)
	}
}

func TestOrphanDelegationCompatibilityIsIndependentFromOptimization(t *testing.T) {
	payload := []byte(`{"model":"gpt-5.5","input":[{"type":"function_call_output","namespace":"codex_app","name":"send_message_to_thread","output":"sent"}]}`)
	cfg := &config.Config{Codex: config.CodexConfig{OrphanDelegationCompatibility: true}}
	output := TranslateRequestWithCodexMultiAgentV2(
		context.Background(),
		http.Header{codexOpenAISubagentHeader: {codexCollabSpawnSubagent}},
		cfg,
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatOpenAIResponse,
		"gpt-5.5",
		payload,
		false,
	)
	if got := gjson.GetBytes(output, "input.0.type").String(); got != "message" {
		t.Fatalf("orphan compatibility followed disabled optimization: %s", output)
	}
}

func TestRestoreCodexMultiAgentV2ResponseSupportsDottedNames(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"output":[` +
		`{"type":"function_call","name":"collaboration-optimize.send_message"},` +
		`{"type":"message","name":"collaboration-optimize.plain"}` +
		`]}}`)
	output := RestoreCodexMultiAgentV2Response(payload, true)
	if got := gjson.GetBytes(output, "response.output.0.name").String(); got != "send_message" {
		t.Fatalf("tool call name = %q, want send_message", got)
	}
	if got := gjson.GetBytes(output, "response.output.0.namespace").String(); got != codexCollaborationNamespace {
		t.Fatalf("tool call namespace = %q, want collaboration", got)
	}
	if got := gjson.GetBytes(output, "response.output.1.name").String(); got != "collaboration-optimize.plain" {
		t.Fatalf("ordinary message name changed to %q", got)
	}
}

func TestDottedCollaborationIdentityReachesResponses(t *testing.T) {
	request := []byte(`{"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"send_message","parameters":{"type":"object"}},{"type":"custom","name":"list_agents","format":{"type":"text"}}]}]}`)
	identity := translatorcommon.NewResponsesToolIdentity(request)
	for _, call := range []struct {
		name string
		body string
	}{
		{name: "send_message", body: `{"type":"function_call","name":"collaboration-optimize.send_message","namespace":null,"call_id":"call-1","arguments":"{\"name\":\"collaboration-optimize.opaque\"}"}`},
		{name: "list_agents", body: `{"type":"custom_tool_call","name":"collaboration-optimize.list_agents","call_id":"call-2","input":"collaboration-optimize.opaque"}`},
	} {
		for _, delivery := range []string{"non-stream", "stream"} {
			t.Run(call.name+"/"+delivery, func(t *testing.T) {
				var state any
				var item gjson.Result
				if delivery == "stream" {
					payload := RestoreCodexMultiAgentV2Response([]byte(`{"type":"response.output_item.added","item":`+call.body+`}`), true)
					frames := sdktranslator.Default().TranslateStream(context.Background(), sdktranslator.FormatCodex,
						sdktranslator.FormatOpenAIResponse, "gpt-5.5", request, request, payload, &state)
					if len(frames) != 1 {
						t.Fatalf("translated frames = %d, want 1", len(frames))
					}
					item = gjson.GetBytes(frames[0], "item")
				} else {
					payload := RestoreCodexMultiAgentV2Response([]byte(`{"type":"response.completed","response":{"output":[`+call.body+`]}}`), true)
					output := sdktranslator.Default().TranslateNonStream(context.Background(), sdktranslator.FormatCodex,
						sdktranslator.FormatOpenAIResponse, "gpt-5.5", request, request, payload, &state)
					item = gjson.GetBytes(output, "output.0")
				}
				if item.Get("name").String() != call.name || item.Get("namespace").String() != codexCollaborationNamespace {
					t.Fatalf("translated tool identity = %s, want collaboration/%s", item.Raw, call.name)
				}
				declaration, err := identity.ResolveDeclaration(item.Get("name").String(), item.Get("namespace").String())
				if err != nil || declaration.Name != call.name || declaration.Namespace != codexCollaborationNamespace {
					t.Fatalf("tool declaration = %#v, error = %v", declaration, err)
				}
				if item.Get("arguments").Exists() && item.Get("arguments").String() != `{"name":"collaboration-optimize.opaque"}` ||
					item.Get("input").Exists() && item.Get("input").String() != "collaboration-optimize.opaque" {
					t.Fatalf("opaque tool data changed: %s", item.Raw)
				}
			})
		}
	}
}

func TestRewriteCodexOrphanDelegationReadsGinHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginContext := &gin.Context{Request: &http.Request{Header: http.Header{codexOpenAISubagentHeader: {codexCollabSpawnSubagent}}}}
	ctx := util.WithGinContext(context.Background(), ginContext)
	payload := []byte(`{"input":[{"type":"function_call_output","namespace":"codex_app","name":"create_thread","output":"created"}]}`)
	output := RewriteCodexOrphanDelegationInput(ctx, nil, payload, true)
	if got := gjson.GetBytes(output, "input.0.type").String(); got != "message" {
		t.Fatalf("Gin header was not applied: %s", output)
	}
}
