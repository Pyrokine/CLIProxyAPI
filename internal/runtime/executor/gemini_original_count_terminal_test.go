package executor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiExecutorCountTokensPrependsLeadingUser(t *testing.T) {
	for _, method := range []string{"CountTokens", "Execute(countTokens)"} {
		t.Run(
			method, func(t *testing.T) {
				calls := 0
				var body []byte
				ctx := util.WithRoundTripper(
					t.Context(), devinTestRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							calls++
							if !strings.HasSuffix(request.URL.Path, "/gemini-3.7-flash:countTokens") {
								t.Errorf("countTokens endpoint=%s", request.URL)
							}
							var err error
							body, err = io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							return &http.Response{
								StatusCode: http.StatusOK, Request: request, Header: http.Header{},
								Body: io.NopCloser(strings.NewReader(`{"totalTokens":7}`)),
							}, nil
						},
					),
				)
				request := core.Request{
					Model:   "gemini-3.7-flash",
					Payload: []byte(`{"contents":[{"role":"model","parts":[{"text":"prior output"}]}]}`),
				}
				credential := &auth.Auth{
					ID: t.Name(), Provider: "gemini", Attributes: map[string]string{
						"api_key": "test-key", "base_url": "https://original-count.invalid",
					},
				}
				execute := NewGeminiExecutor(&config.Config{})
				opts := core.Options{SourceFormat: translator.FormatGemini}
				var err error
				if method == "CountTokens" {
					_, err = execute.CountTokens(ctx, credential, request, opts)
				} else {
					request.Metadata = map[string]any{"action": "countTokens"}
					_, err = execute.Execute(ctx, credential, request, opts)
				}
				if err != nil || calls != 1 {
					t.Fatalf("upstream calls=%d, error=%v", calls, err)
				}
				contents := gjson.GetBytes(body, "contents").Array()
				if len(contents) != 2 || contents[0].Get("role").String() != "user" ||
					contents[1].Get("role").String() != "model" {
					t.Fatalf("countTokens roles changed: %s", body)
				}
				if text := contents[0].Get("parts.0.text"); !text.Exists() || text.String() != "" {
					t.Fatalf("synthetic leading user missing: %s", body)
				}
				if contents[1].Get("parts.0.text").String() != "prior output" {
					t.Fatalf("trailing model content changed: %s", body)
				}
			},
		)
	}
}

func TestGeminiOriginalSparseTerminalsKeepStrictAcceptance(t *testing.T) {
	chatPayload := `{
		"model":"gemini-3.1-flash-lite","stream":true,
		"messages":[{"role":"user","content":"今天北京的天气怎么样？"}],
		"tools":[{"type":"function","function":{"name":"get_weather",
			"parameters":{"type":"object","properties":{"location":{"type":"string"}}}}}],
		"tool_choice":"auto"
	}`
	chatWire := "event: interaction.created\ndata: " +
		`{"event_type":"interaction.created","interaction":{"id":"i1","model":"gemini-3.1-flash-lite"}}` + "\n\n" +
		"event: step.start\ndata: " +
		`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"call_1",` +
		`"name":"get_weather","arguments":{}}}` + "\n\n" +
		"event: step.delta\ndata: " +
		`{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta",` +
		`"arguments":"{\"location\":\"北京\"}"}}` + "\n\n" +
		"event: step.stop\ndata: " + `{"event_type":"step.stop","index":0}` + "\n\n" +
		"event: interaction.completed\ndata: " +
		`{"event_type":"interaction.completed","interaction":{"id":"i1","status":"requires_action",` +
		`"usage":{"total_input_tokens":2,"total_output_tokens":3,"total_tokens":5}}}` + "\n\n"
	passthroughPayload := `{"model":"gemini-3.6-flash","input":[
		{"type":"user_input","id":"msg_1","content":[{"type":"text","id":"txt_1","text":"hi"}]},
		{"type":"function_call","id":"toolu_1","call_id":"toolu_1","name":"lookup","arguments":{"q":"x"}},
		{"type":"function_result","id":"toolu_1","call_id":"toolu_1","result":"ok"}
	]}`
	passthroughWire := "event: interaction.completed\ndata: " +
		`{"event_type":"interaction.completed","interaction":{"id":"i1","status":"completed"}}` + "\n\n"
	usageWire := "event: interaction.created\ndata: " +
		`{"event_type":"interaction.created","interaction":{"id":"i1"}}` + "\n\n" +
		"event: interaction.completed\ndata: " +
		`{"event_type":"interaction.completed","interaction":{"id":"i1","status":"completed",` +
		`"usage":{"total_input_tokens":2,"total_output_tokens":3,"total_tokens":5}}}` + "\n\n"

	for _, test := range []struct {
		name, model, payload, wire string
		format                     translator.Format
		input, output, total       int64
	}{
		{
			"TestGeminiExecutorNativeInteractionsTranslatesOpenAIChatRequest", "gemini-3.1-flash-lite",
			chatPayload, chatWire, translator.FormatOpenAI, 2, 3, 5,
		},
		{
			"TestGeminiExecutorNativeInteractionsStripsPassthroughInputIDsBeforeUpstream", "gemini-3.6-flash",
			passthroughPayload, passthroughWire, translator.FormatInteractions, 0, 0, 0,
		},
		{
			"TestGeminiExecutorNativeInteractionsStreamParsesUsage", "gemini-3.5-flash",
			`{"model":"gemini-3.5-flash","input":"hi","stream":` +
				`true}`, usageWire, translator.FormatInteractions, 2, 3, 5,
		},
	} {
		for _, complete := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/steps=%t", test.name, complete), func(t *testing.T) {
					plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
					registerTestUsagePlugin(t, plugin)
					wire := test.wire
					if complete {
						wire = strings.Replace(
							wire,
							`"status":"completed"`,
							`"status":"completed","steps":[]`,
							1,
						)
						wire = strings.Replace(
							wire, `"status":"requires_action"`, `"status":"requires_action","steps":[]`, 1,
						)
					}
					calls := 0
					var upstreamBody []byte
					ctx := util.WithRoundTripper(
						t.Context(),
						devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								calls++
								if request.URL.Path != "/v1beta/interactions" {
									t.Errorf("native endpoint=%s", request.URL.Path)
								}
								var err error
								upstreamBody, err = io.ReadAll(request.Body)
								if err != nil {
									return nil, err
								}
								return &http.Response{
									StatusCode: http.StatusOK, Request: request,
									Header: http.Header{"Content-Type": {"text/event-stream"}},
									Body:   io.NopCloser(strings.NewReader(wire)),
								}, nil
							},
						),
					)
					credential := &auth.Auth{
						ID: t.Name(), Provider: "gemini-interactions", Attributes: map[string]string{
							"api_key": "test-key", "base_url": "https://original-interactions.invalid",
						},
					}
					stream, err := NewGeminiInteractionsExecutor(&config.Config{}).ExecuteStream(
						ctx, credential,
						core.Request{Model: test.model, Payload: []byte(test.payload)},
						core.Options{SourceFormat: test.format, ResponseFormat: test.format},
					)
					if err != nil {
						t.Fatal(err)
					}
					var output bytes.Buffer
					var toolStart []byte
					for chunk := range stream.Chunks {
						output.Write(chunk.Payload)
						if chunk.Err != nil {
							err = chunk.Err
						}
						if gjson.GetBytes(
							chunk.Payload, "choices.0.delta.tool_calls.0.function.name",
						).String() == "get_weather" {
							toolStart = chunk.Payload
						}
					}
					var status interface{ StatusCode() int }
					if calls != 1 || (complete && err != nil) ||
						(!complete && (!errors.As(err, &status) || status.StatusCode() != http.StatusBadGateway)) {
						t.Fatalf("complete=%t upstream calls=%d, error=%v", complete, calls, err)
					}
					if test.format == translator.FormatOpenAI {
						if gjson.GetBytes(upstreamBody, "input.0.content.0.text").String() != "今天北京的天气怎么样？" ||
							gjson.GetBytes(upstreamBody, "messages").Exists() ||
							gjson.GetBytes(upstreamBody, "tools.0.type").String() != "function" ||
							gjson.GetBytes(upstreamBody, "generation_config.tool_choice").String() != "auto" ||
							gjson.GetBytes(toolStart, "choices.0.delta.tool_calls.0.id").String() != "call_1" {
							t.Fatalf(
								"original Chat request or tool identity changed: body=%s tool=%s", upstreamBody,
								toolStart,
							)
						}
					} else if test.model == "gemini-3.6-flash" {
						inputs := gjson.GetBytes(upstreamBody, "input").Array()
						if len(inputs) != 3 || inputs[0].Get("content.0.text").String() != "hi" ||
							inputs[1].Get("id").String() != "toolu_1" || inputs[1].Get("call_id").Exists() ||
							inputs[2].Get("call_id").String() != "toolu_1" {
							t.Fatalf("original passthrough input changed: %s", upstreamBody)
						}
						for _, item := range inputs {
							if item.Get("type").String() != "function_call" && item.Get("id").Exists() {
								t.Fatalf("unsupported input ID reached upstream: %s", upstreamBody)
							}
							for _, part := range item.Get("content").Array() {
								if part.Get("id").Exists() {
									t.Fatalf("unsupported part ID reached upstream: %s", upstreamBody)
								}
							}
						}
					}
					if test.format == translator.FormatInteractions && output.String() != wire {
						t.Fatalf("native frames changed: got=%q want=%q", output.String(), wire)
					}
					record := testWaitForDevinUsageRecord(t, plugin.records)
					if record.Failed == complete || record.Detail.InputTokens != test.input ||
						record.Detail.OutputTokens != test.output || record.Detail.TotalTokens != test.total {
						t.Fatalf("complete=%t observed usage=%+v", complete, record)
					}
				},
			)
		}
	}
}
