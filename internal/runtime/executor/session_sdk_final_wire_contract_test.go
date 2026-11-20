package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestInteractionsBuiltinToolsReachFinalGeminiAndAntigravityWire(t *testing.T) {
	for _, tc := range []struct {
		name, tools string
		fields      map[string]string
	}{
		{
			"typed builtins", `[{"type":"url_context"},{"type":"code_execution"},{"type":"google_search"}]`,
			map[string]string{"urlContext": "{}", "codeExecution": "{}", "googleSearch": "{}"},
		},
		{
			"typed options",
			`[{"type":"url_context","url_context":{"limit":2}},{"type":"code_execution","codeExecution":{"` +
				`environment":"sandbox"}},{"type":"web_search","web_search":{"mode":"search"}}]`,
			map[string]string{
				"urlContext.limit": "2", "codeExecution.environment": `"sandbox"`, "googleSearch.mode": `"search"`,
			},
		},
		{
			"native aliases",
			`[{"url_context":{"limit":2},"code_execution":{"environment":"sandbox"},"web_search":{"mode":"search"}}]`,
			map[string]string{
				"urlContext.limit": "2", "codeExecution.environment": `"sandbox"`, "googleSearch.mode": `"search"`,
			},
		},
		{
			"composite native fields", `[{"googleSearch":{},"urlContext":{}}]`,
			map[string]string{"googleSearch": "{}", "urlContext": "{}"},
		},
		{
			"mixed function and builtin",
			`[{"type":"function","name":"lookup","parameters":{"type":"object"}},` +
				`{"type":"url_context"},{"type":` +
				`"code_execution"}]`,
			map[string]string{"functionDeclarations.0.name": `"lookup"`, "urlContext": "{}", "codeExecution": "{}"},
		},
	} {
		for _, provider := range []string{"gemini", "antigravity"} {
			for _, stream := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("%s/%s/stream=%t", tc.name, provider, stream), func(t *testing.T) {
						payload := []byte(`{"model":"gemini-3-flash","input":"hello","tools":` + tc.tools + `}`)
						wire := auditProtocolFinalWire(
							t, provider, "gemini-3-flash", translator.FormatInteractions, payload, stream,
						)
						prefix := ""
						if provider == "antigravity" {
							prefix = "request."
						}
						for field, want := range tc.fields {
							found := false
							for _, tool := range gjson.GetBytes(wire, prefix+"tools").Array() {
								found = found || tool.Get(field).Raw == want
							}
							if !found {
								t.Fatalf("final %s=%s missing: %s", field, want, wire)
							}
						}
						if gjson.GetBytes(wire, prefix+"generationConfig.temperature").Float() != 0.25 {
							t.Fatalf("final payload rule was not consumed: %s", wire)
						}
					},
				)
			}
		}
	}
}

func TestResponsesReasoningHistoryReachesFinalOpenAICompatWire(t *testing.T) {
	const call1 = `{"type":"function_call","call_id":"call_1","name":"exec_command","arguments":"{}"}`
	const call2 = `{"type":"function_call","call_id":"call_2","name":"exec_command","arguments":"{}"}`
	const result1 = `{"type":"function_call_output","call_id":"call_1","output":"ok"}`
	const result2 = `{"type":"function_call_output","call_id":"call_2","output":"ok"}`
	const reasoning = `{"type":"reasoning","summary":[{"type":"summary_text","text":"first plan"}]}`
	for _, tc := range []struct {
		name, effort, input string
		count               int
		fields              map[string]string
	}{
		{
			"follow-up tool", "high",
			reasoning + (`,{"type":"message","role":"assistant","content":[{"type":"output_text"` +
				`,"text":"starting"}]},{"type":"function_call","call_id":"call_1","name` +
				`":"exec_command","arguments":"{\"cmd\":\"ls\"}"},`) +
				result1 +
				(`,{"type":"function_call",` +
					`"call_id":"call_2","name":` +
					`"write_stdin","arguments":` +
					`"{\"data\":\"x\"}"},`) +
				result2 +
				(`,{"type":"reasoning","summary":[{"type":"summary_text","text":"second plan"}]},{"type":"` +
					`function_call","call_id":"call_3","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}` +
					`,{"type":"function_call_output","call_id":"call_3","output":"ok"},{"type":"message","` +
					`role":"assistant","content":[{"type":"output_text","text":"apply_patch is available"}]},` +
					`{"type":"function_call","call_id":"call_4","name":"exec_command","arguments":"{\"cmd\":\` +
					`"cat\"}"},{"type":"function_call_output","call_id":"call_4","output":"ok"}`),
			8, map[string]string{
				"0.reasoning_content": `"first plan"`, "2.reasoning_content": `"first plan"`,
				"2.tool_calls.0.id": `"call_2"`, "6.reasoning_content": `"second ` +
					`plan"`, "6.tool_calls.0.id": `"call_4"`,
			},
		},
		{
			"missing reasoning placeholder", "high", call1 + "," + result1, 2,
			map[string]string{"0.reasoning_content": `"[reasoning unavailable]"`},
		},
		{"effort none", "none", call1 + "," + result1, 2, map[string]string{"0.reasoning_content": ""}},
		{
			"custom tool follow-up", "high",
			reasoning + (`,{"type":"custom_tool_call","call_id":"custom_1","name":"do_work","input":"step1"},{` +
				`"type":"custom_tool_call_output","call_id":"custom_1","output":"done"},{"type":"` +
				`custom_tool_call","call_id":"custom_2","name":"do_work","input":"step2"},{"type":"` +
				`custom_tool_call_output","call_id":"custom_2","output":"done"}`),
			4, map[string]string{
				"0.reasoning_content": `"first plan"`, "2.reasoning_content": `"first plan"`,
				"2.tool_calls.0.id": `"custom_2"`,
			},
		},
		{
			"new user clears prior reasoning", "high",
			reasoning +
				"," +
				call1 +
				"," +
				result1 +
				`,{"type":"message","role":"user","content":"next"},` +
				call2 +
				"," +
				result2,
			5,
			map[string]string{
				"0.reasoning_content": `"first ` +
					`plan"`, "3.reasoning_content": `"[reasoning ` +
					`unavailable]"`,
			},
		},
		{
			"assistant reasoning merges once", "high",
			reasoning + (`,{"type":"message","role":"assistant","reasoning_content":"first plan","content":[{"` +
				`type":"output_text","text":"working"}]},{"type":"reasoning","summary":[{"type":"` +
				`summary_text","text":"second plan"}]},`) + call1 + "," + result1,
			2, map[string]string{
				"0.reasoning_content": `"first plan\n\nsecond plan"`, "0.content.0.text": `"working"`,
				"0.tool_calls.0.id": `"call_1"`,
			},
		},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
					var wire []byte
					calls := 0
					transport := &auditClaudeContractTransport{
						respond: func(request *http.Request) (*http.Response, error) {
							calls++
							if request.URL.Host != "payload-contract.invalid" ||
								request.URL.Path != "/v1/chat/completions" {
								return nil, fmt.Errorf("unexpected isolated route %s", request.URL)
							}
							var err error
							wire, err = io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							body := `{"id":"fixture","object":"chat.completion","choices":[{"index":0,"mess` +
								`age":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
							if stream {
								body = "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"" +
									"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\"" +
									":\"stop\"}]}\n\ndata: [DONE]\n\n"
							}
							return googlePayloadResponse(request, body), nil
						},
					}
					ctx, cancel := context.WithTimeout(
						translator.WithPluginHooks(t.Context(), nil),
						5*time.Second,
					)
					defer cancel()
					ctx = util.WithRoundTripper(ctx, transport)
					cfg := &config.Config{}
					credential := &auth.Auth{
						ID: t.Name(), Provider: "openai", Attributes: map[string]string{
							"base_url": "https://payload-contract.invalid/v1", "api_key": "synthetic",
						},
					}
					if newProxyAwareHTTPClient(ctx, cfg, credential, 0).Transport != transport {
						t.Fatal("OpenAI-compatible transport is not isolated")
					}
					payload := []byte(
						`{"model":"deepseek-v4.1-flash","reasoning":{"effort":` +
							`"` +
							tc.effort +
							`"},"input":[` +
							tc.input +
							`]}`,
					)
					original := bytes.Clone(payload)
					request := core.Request{Model: "deepseek-v4.1-flash", Payload: payload}
					opts := core.Options{
						SourceFormat: translator.FormatOpenAIResponse, ResponseFormat: translator.FormatOpenAI,
						Stream: stream,
					}
					implementation := NewOpenAICompatExecutor("openai", cfg)
					var err error
					if stream {
						var result *core.StreamResult
						result, err = implementation.ExecuteStream(ctx, credential, request, opts)
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						_, err = implementation.Execute(ctx, credential, request, opts)
					}
					if err != nil || calls != 1 || !bytes.Equal(payload, original) || len(
						gjson.GetBytes(
							wire, "messages",
						).Array(),
					) != tc.count {
						t.Fatalf("final request: error=%v calls=%d wire=%s", err, calls, wire)
					}
					for field, want := range tc.fields {
						if got := gjson.GetBytes(wire, "messages."+field).Raw; got != want {
							t.Fatalf("final messages.%s=%s, want %s: %s", field, got, want, wire)
						}
					}
				},
			)
		}
	}
}
