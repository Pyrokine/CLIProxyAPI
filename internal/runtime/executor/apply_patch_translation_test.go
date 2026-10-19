package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/wsrelay"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const translatedPatchRequest = `{"model":"offline-model","max_output_tokens":64,"tools":[{"type":"custom","name":"apply_patch","description":"This is a FREEFORM tool, so do not wrap the patch in JSON."}],"tool_choice":{"type":"custom","name":"apply_patch"},"input":[{"role":"user","content":[{"type":"input_text","text":"offline patch"}]}]}`

func patchProtocolResponse(t *testing.T, format sdktranslator.Format, name, arguments string) (string, string) {
	t.Helper()
	set := func(body, path string, value any) string {
		t.Helper()
		out, err := sjson.Set(body, path, value)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	sse := func(body string) string { return "data: " + body + "\n\n" }
	switch format {
	case sdktranslator.FormatOpenAI:
		body := `{"id":"offline-patch","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_patch","type":"function","function":{"name":"","arguments":""}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`
		body = set(set(body, "choices.0.message.tool_calls.0.function.name", name), "choices.0.message.tool_calls.0.function.arguments", arguments)
		start := `{"id":"offline-patch","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"","arguments":""}}]},"finish_reason":null}]}`
		stream := sse(set(start, "choices.0.delta.tool_calls.0.function.name", name))
		for _, fragment := range arguments {
			stream += sse(set(`{"id":"offline-patch","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":""}}]},"finish_reason":null}]}`, "choices.0.delta.tool_calls.0.function.arguments", string(fragment)))
		}
		stream += sse(`{"id":"offline-patch","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		stream += sse(`{"id":"offline-patch","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2,"total_tokens":9}}`) + "data: [DONE]\n\n"
		return body, stream
	case sdktranslator.FormatClaude:
		stream := sse(`{"type":"message_start","message":{"id":"offline-patch","type":"message","role":"assistant","model":"claude-sonnet-4","content":[],"usage":{"input_tokens":7,"output_tokens":0}}}`)
		stream += sse(set(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_patch","name":"","input":{}}}`, "content_block.name", name))
		for _, fragment := range arguments {
			stream += sse(set(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`, "delta.partial_json", string(fragment)))
		}
		stream += sse(`{"type":"content_block_stop","index":0}`)
		stream += sse(`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":2}}`)
		stream += sse(`{"type":"message_stop"}`)
		return stream, stream
	case sdktranslator.FormatInteractions:
		body := `{"id":"offline-patch","status":"completed","model":"offline-model","steps":[{"id":"fc_patch","call_id":"call_patch","type":"function_call","name":"","arguments":{}}],"usage":{"total_input_tokens":7,"total_output_tokens":2,"total_tokens":9}}`
		body = set(body, "steps.0.name", name)
		body, err := sjson.SetRaw(body, "steps.0.arguments", arguments)
		if err != nil {
			t.Fatal(err)
		}
		stream := sse(`{"event_type":"interaction.created","interaction":{"id":"offline-patch","model":"offline-model"}}`)
		stream += sse(set(`{"event_type":"step.start","index":0,"step":{"id":"fc_patch","call_id":"call_patch","type":"function_call","name":""}}`, "step.name", name))
		for _, fragment := range arguments {
			stream += sse(set(`{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":""}}`, "delta.arguments", string(fragment)))
		}
		stream += sse(`{"event_type":"step.stop","index":0}`)
		stream += sse(`{"event_type":"interaction.completed","interaction":`+body+`}`) + "data: [DONE]\n\n"
		return body, stream
	default:
		body := `{"responseId":"offline-patch","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_patch","name":"","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}`
		body = set(body, "candidates.0.content.parts.0.functionCall.name", name)
		out, err := sjson.SetRaw(body, "candidates.0.content.parts.0.functionCall.args", arguments)
		if err != nil {
			t.Fatal(err)
		}
		if format == sdktranslator.FormatGeminiCLI || format == sdktranslator.FormatAntigravity {
			out = `{"response":` + out + `}`
		}
		return out, sse(out)
	}
}

func patchRelay(t *testing.T, ctx context.Context, authID string, stream bool, transport http.RoundTripper) *wsrelay.Manager {
	t.Helper()
	manager := wsrelay.NewManager(wsrelay.Options{ProviderFactory: func(*http.Request) (string, error) { return authID, nil }})
	server := httptest.NewServer(manager.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })
	client, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+manager.Path(), nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	done := make(chan error, 1)
	go func() {
		done <- func() error {
			var envelope json.RawMessage
			if errRead := client.ReadJSON(&envelope); errRead != nil {
				return errRead
			}
			req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, gjson.GetBytes(envelope, "payload.url").String(), strings.NewReader(gjson.GetBytes(envelope, "payload.body").String()))
			if errRequest != nil {
				return errRequest
			}
			upstream, errResponse := transport.RoundTrip(req)
			if errResponse != nil {
				return errResponse
			}
			defer upstream.Body.Close()
			body, errRead := io.ReadAll(upstream.Body)
			if errRead != nil {
				return errRead
			}
			id := gjson.GetBytes(envelope, "id").String()
			write := func(kind string, payload map[string]any) error {
				return client.WriteJSON(map[string]any{"id": id, "type": kind, "payload": payload})
			}
			if !stream {
				return write(wsrelay.MessageTypeHTTPResp, map[string]any{"status": upstream.StatusCode, "headers": upstream.Header, "body": string(body)})
			}
			if errWrite := write(wsrelay.MessageTypeStreamStart, map[string]any{"status": upstream.StatusCode, "headers": upstream.Header}); errWrite != nil {
				return errWrite
			}
			if errWrite := write(wsrelay.MessageTypeStreamChunk, map[string]any{"data": string(body)}); errWrite != nil {
				return errWrite
			}
			return write(wsrelay.MessageTypeStreamEnd, nil)
		}()
	}()
	t.Cleanup(func() {
		select {
		case errClient := <-done:
			if errClient != nil {
				t.Error(errClient)
			}
		case <-time.After(time.Second):
			t.Error("relay fixture did not complete")
		}
	})
	return manager
}

func TestExecutorsTranslateFreeformApplyPatch(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	cfg := &config.Config{}
	for _, tc := range []struct {
		name   string
		create func() coreauth.ProviderExecutor
		format sdktranslator.Format
		model  string
		schema string
	}{
		{"chat", func() coreauth.ProviderExecutor { return NewOpenAICompatExecutor("openai-compatibility", cfg) }, sdktranslator.FormatOpenAI, "gpt-4o", "tools.0.function.parameters"},
		{"qwen", func() coreauth.ProviderExecutor { return NewQwenExecutor(cfg) }, sdktranslator.FormatOpenAI, "gpt-4o", "tools.0.function.parameters"},
		{"iflow", func() coreauth.ProviderExecutor { return NewIFlowExecutor(cfg) }, sdktranslator.FormatOpenAI, "gpt-4o", "tools.0.function.parameters"},
		{"claude", func() coreauth.ProviderExecutor { return NewClaudeExecutor(cfg) }, sdktranslator.FormatClaude, "claude-sonnet-4", "tools.0.input_schema"},
		{"gemini", func() coreauth.ProviderExecutor { return NewGeminiExecutor(cfg) }, sdktranslator.FormatGemini, "gemini-3.7-flash", "tools.0.functionDeclarations.0.parametersJsonSchema"},
		{"aistudio", func() coreauth.ProviderExecutor { return NewAIStudioExecutor(cfg, "aistudio", nil) }, sdktranslator.FormatGemini, "gemini-3.7-flash", "tools.0.functionDeclarations.0.parametersJsonSchema"},
		{"gemini-interactions", func() coreauth.ProviderExecutor { return NewGeminiInteractionsExecutor(cfg) }, sdktranslator.FormatInteractions, "gemini-3.7-flash", "tools.0.parameters"},
		{"devin", func() coreauth.ProviderExecutor { return NewDevinExecutor(cfg) }, sdktranslator.FormatInteractions, "devin/swe-2", "parameters"},
		{"vertex", func() coreauth.ProviderExecutor { return NewGeminiVertexExecutor(cfg) }, sdktranslator.FormatGemini, "gemini-3.7-flash", "tools.0.functionDeclarations.0.parametersJsonSchema"},
		{"vertex-service-account", func() coreauth.ProviderExecutor { return NewGeminiVertexExecutor(cfg) }, sdktranslator.FormatGemini, "gemini-3.7-flash", "tools.0.functionDeclarations.0.parametersJsonSchema"},
		{"antigravity-claude", func() coreauth.ProviderExecutor { return NewAntigravityExecutor(cfg) }, sdktranslator.FormatAntigravity, "claude-sonnet-4", "request.tools.0.functionDeclarations.0.parameters"},
		{"gemini-cli", func() coreauth.ProviderExecutor { return NewGeminiCLIExecutor(cfg) }, sdktranslator.FormatGeminiCLI, "gemini-3.7-flash", "request.tools.0.functionDeclarations.0.parametersJsonSchema"},
		{"antigravity", func() coreauth.ProviderExecutor { return NewAntigravityExecutor(cfg) }, sdktranslator.FormatAntigravity, "gemini-3.6-flash-high", "request.tools.0.functionDeclarations.0.parameters"},
	} {
		for _, original := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, invalid := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%t/invalid=%t/original=%t", tc.name, stream, invalid, original), func(t *testing.T) {
						const input = "*** Begin Patch\n中文🧪 <>&\n*** End Patch"
						arguments, err := sjson.Set(`{}`, "input", input)
						if err != nil {
							t.Fatal(err)
						}
						if invalid {
							arguments = `{"input":42}`
						}
						body, events := patchProtocolResponse(t, tc.format, "apply_patch", arguments)
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						ctx = usage.WithRequestedModelAlias(ctx, t.Name())
						plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
						registerTestUsagePlugin(t, plugin)
						defer func() {
							select {
							case record := <-plugin.records:
								if record.Alias != t.Name() || record.Failed != invalid {
									t.Errorf("usage outcome = %#v, want failed=%t", record, invalid)
								}
								if (!stream || !invalid) && (record.Detail.InputTokens != 7 || record.Detail.OutputTokens != 2) {
									t.Errorf("observed usage changed: %#v", record.Detail)
								}
							case <-time.After(time.Second):
								t.Error("usage outcome was not published")
							}
							select {
							case record := <-plugin.records:
								t.Errorf("duplicate usage outcome: %#v", record)
							case <-time.After(20 * time.Millisecond):
							}
						}()
						var requests, tokenRequests atomic.Int32
						transport := apiKeyCompatRoundTripper(func(req *http.Request) (*http.Response, error) {
							if tc.name == "vertex-service-account" && req.URL.Host == "offline-token.invalid" && req.URL.Path == "/token" {
								tokenRequests.Add(1)
								return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"synthetic-bearer","expires_in":3600,"token_type":"Bearer"}`)), Request: req}, nil
							}
							requests.Add(1)
							if tc.name == "vertex-service-account" && (req.Header.Get("Authorization") != "Bearer synthetic-bearer" || req.Header.Get("X-Goog-Api-Key") != "") {
								return nil, fmt.Errorf("service-account credential headers were not preserved")
							}
							payload, errRead := io.ReadAll(req.Body)
							if errRead != nil {
								return nil, errRead
							}
							if tc.name == "devin" {
								_, wire, errFrame := helps.ReadConnectFrame(bytes.NewReader(payload))
								if errFrame != nil {
									return nil, errFrame
								}
								definition := firstDevinBytesField(t, wire, 10)
								payload = []byte(`{"parameters":` + string(firstDevinBytesField(t, definition, 3)) + `}`)
							}
							if gjson.GetBytes(payload, tc.schema+".properties.input.type").String() != "string" {
								return nil, fmt.Errorf("patch function schema missing at %s: %s", tc.schema, payload)
							}
							response, contentType := body, "application/json"
							if stream || tc.format == sdktranslator.FormatClaude || tc.name == "antigravity-claude" {
								response, contentType = events, "text/event-stream"
							}
							if tc.name == "devin" {
								tool := testDevinBytesField(nil, 1, []byte("call_patch"))
								tool = testDevinBytesField(tool, 2, []byte("apply_patch"))
								tool = testDevinBytesField(tool, 3, []byte(arguments))
								usageField := testDevinVarintField(nil, 2, 7)
								usageField = testDevinVarintField(usageField, 3, 2)
								response = string(testDevinConnectStream(testDevinBytesField(nil, 7, usageField), testDevinBytesField(nil, 6, tool)))
								contentType = "application/connect+proto"
							}
							return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
						})
						ctx = util.WithRoundTripper(ctx, transport)
						exec := tc.create()
						auth := &coreauth.Auth{ID: "offline-patch-" + tc.name, Provider: exec.Identifier(),
							Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://offline-patch.invalid"},
							Metadata: map[string]any{"access_token": "synthetic-token", "project_id": "synthetic-project", "expired": time.Now().Add(time.Hour).Format(time.RFC3339),
								"token": map[string]any{"access_token": "synthetic-token", "token_type": "Bearer", "expiry": time.Now().Add(time.Hour).Format(time.RFC3339)}},
						}
						if tc.name == "aistudio" {
							exec = NewAIStudioExecutor(cfg, "aistudio", patchRelay(t, ctx, auth.ID, stream, transport))
						}
						if tc.name == "vertex-service-account" {
							delete(auth.Attributes, "api_key")
							delete(auth.Metadata, "access_token")
							auth.Metadata["service_account"] = map[string]any{"type": "service_account", "project_id": "synthetic-project", "client_email": "synthetic@offline.invalid", "private_key": privateKey, "token_uri": "https://offline-token.invalid/token"}
						}
						payload := []byte(translatedPatchRequest)
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Stream: stream}
						if original {
							opts.OriginalRequest = bytes.Clone(payload)
							payload = bytes.Replace(payload, []byte("This is a FREEFORM tool, so do not wrap the patch in JSON."), []byte("Prepared provider tool"), 1)
						}
						payloadBefore, originalBefore := bytes.Clone(payload), bytes.Clone(opts.OriginalRequest)
						req := cliproxyexecutor.Request{Model: tc.model, Payload: payload}
						var output []byte
						var executionErr error
						if stream {
							result, errStream := exec.ExecuteStream(ctx, auth, req, opts)
							executionErr = errStream
							if errStream == nil {
								for chunk := range result.Chunks {
									output = append(output, chunk.Payload...)
									if chunk.Err != nil {
										executionErr = chunk.Err
									}
									if chunk.Acknowledge != nil {
										chunk.Acknowledge(nil)
									}
								}
							}
						} else {
							response, errExecute := exec.Execute(ctx, auth, req, opts)
							output, executionErr = response.Payload, errExecute
						}
						if !bytes.Equal(req.Payload, payloadBefore) || !bytes.Equal(opts.OriginalRequest, originalBefore) {
							t.Fatal("executor modified caller request bytes")
						}
						if requests.Load() != 1 {
							t.Fatalf("upstream requests = %d, want 1; error=%v", requests.Load(), executionErr)
						}
						if tc.name == "vertex-service-account" && tokenRequests.Load() != 1 {
							t.Errorf("synthetic token requests = %d, want 1", tokenRequests.Load())
						}
						if invalid {
							var status interface{ StatusCode() int }
							if !errors.As(executionErr, &status) || status.StatusCode() != http.StatusBadGateway {
								t.Fatalf("invalid patch error = %v, want 502; output=%s", executionErr, output)
							}
							if strings.Contains(string(output), `"type":"response.completed"`) || (!stream && len(output) != 0) {
								t.Fatalf("invalid patch delivered as success: %s", output)
							}
							return
						}
						if executionErr != nil {
							t.Fatal(executionErr)
						}
						terminal := output
						if stream {
							terminal = nil
							completed, inputDone := 0, 0
							var delta strings.Builder
							for _, line := range strings.Split(string(output), "\n") {
								if !strings.HasPrefix(line, "data:") {
									continue
								}
								data := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
								switch gjson.GetBytes(data, "type").String() {
								case "response.custom_tool_call_input.delta":
									delta.WriteString(gjson.GetBytes(data, "delta").String())
								case "response.custom_tool_call_input.done":
									inputDone++
									if gjson.GetBytes(data, "input").String() != input {
										t.Fatalf("patch done input changed: %s", data)
									}
								case "response.completed":
									completed++
									terminal = []byte(gjson.GetBytes(data, "response").Raw)
								}
							}
							if completed != 1 || inputDone != 1 || delta.String() != input {
								t.Fatalf("stream contract: completed=%d, inputDone=%d, delta=%q; output=%s", completed, inputDone, delta.String(), output)
							}
						}
						for _, field := range []string{"tools", "tool_choice"} {
							if echoed := gjson.GetBytes(terminal, field); echoed.Exists() && !reflect.DeepEqual(echoed.Value(), gjson.Get(translatedPatchRequest, field).Value()) {
								t.Fatalf("client %s declaration changed: %s", field, echoed.Raw)
							}
						}
						item := gjson.GetBytes(terminal, "output.0")
						if item.Get("type").String() != "custom_tool_call" || item.Get("name").String() != "apply_patch" || item.Get("input").String() != input || item.Get("call_id").String() == "" || item.Get("id").String() == "" {
							t.Fatalf("patch was not restored: %s", terminal)
						}
						if gjson.GetBytes(terminal, "usage.input_tokens").Int() != 7 || gjson.GetBytes(terminal, "usage.output_tokens").Int() != 2 {
							t.Fatalf("usage changed: %s", terminal)
						}
					})
				}
			}
		}
	}
}

func TestChatPatchStreamTermination(t *testing.T) {
	cfg := &config.Config{}
	for _, exec := range []coreauth.ProviderExecutor{
		NewOpenAICompatExecutor("openai-compatibility", cfg), NewQwenExecutor(cfg), NewIFlowExecutor(cfg),
	} {
		for _, tc := range []struct {
			name     string
			finished bool
			done     bool
			readErr  error
			cancel   bool
		}{
			{"completed", true, true, nil, false},
			{"clean EOF after finish", true, false, nil, false},
			{"truncated EOF", false, false, nil, false},
			{"premature DONE", false, true, nil, false},
			{"read failure", true, false, io.ErrUnexpectedEOF, false},
			{"cancelled delivery", true, true, nil, true},
		} {
			t.Run(exec.Identifier()+"/"+tc.name, func(t *testing.T) {
				_, events := patchProtocolResponse(t, sdktranslator.FormatOpenAI, "apply_patch", `{"input":"patch"}`)
				events = strings.TrimSuffix(events, "data: [DONE]\n\n")
				if !tc.finished {
					position := strings.Index(events, `"finish_reason":"tool_calls"`)
					events = events[:strings.LastIndex(events[:position], "data:")]
				}
				if tc.done {
					events += "data: [DONE]\n\n"
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				ctx = usage.WithRequestedModelAlias(ctx, t.Name())
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
				registerTestUsagePlugin(t, plugin)
				ctx = util.WithRoundTripper(ctx, apiKeyCompatRoundTripper(func(req *http.Request) (*http.Response, error) {
					reader := io.Reader(strings.NewReader(events))
					if tc.readErr != nil {
						reader = io.MultiReader(reader, &terminalErrorReader{done: true, err: tc.readErr})
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(reader), Request: req}, nil
				}))
				auth := &coreauth.Auth{ID: t.Name(), Provider: exec.Identifier(), Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://offline-patch.invalid"}}
				payload := []byte(translatedPatchRequest)
				result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gpt-4o", Payload: payload}, cliproxyexecutor.Options{
					SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload, Stream: true,
				})
				if err != nil {
					t.Fatal(err)
				}
				var output strings.Builder
				var streamErr error
				for chunk := range result.Chunks {
					output.Write(chunk.Payload)
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
					if tc.cancel && len(chunk.Payload) != 0 {
						cancel()
					}
				}
				failed := !tc.finished || tc.readErr != nil || tc.cancel
				if !tc.cancel && (streamErr != nil) != failed {
					t.Errorf("stream error = %v, want failed=%t; output=%s", streamErr, failed, output.String())
				}
				if tc.readErr != nil && !errors.Is(streamErr, tc.readErr) {
					t.Errorf("read failure was lost: %v", streamErr)
				}
				completed := strings.Count(output.String(), `"type":"response.completed"`)
				if (failed && completed != 0) || (!failed && completed != 1) {
					t.Errorf("completed=%d, want failed=%t; output=%s", completed, failed, output.String())
				}
				select {
				case record := <-plugin.records:
					if record.Failed != failed {
						t.Errorf("usage failed=%t, want %t", record.Failed, failed)
					}
					if tc.finished && !tc.cancel && (record.Detail.InputTokens != 7 || record.Detail.OutputTokens != 2) {
						t.Errorf("observed usage changed: %#v", record.Detail)
					}
				case <-time.After(time.Second):
					t.Error("usage was not published")
				}
				select {
				case record := <-plugin.records:
					t.Errorf("duplicate usage: %#v", record)
				case <-time.After(20 * time.Millisecond):
				}
			})
		}
	}
}

func TestOriginalRequestPayload(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  []byte
		original []byte
		want     []byte
	}{
		{"omitted", []byte("request"), nil, []byte("request")},
		{"empty", []byte("request"), []byte{}, []byte("request")},
		{"explicit", []byte("working"), []byte("client"), []byte("client")},
		{"explicit-without-payload", nil, []byte("client"), []byte("client")},
		{"empty-request", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := cliproxyexecutor.Request{Payload: tc.payload}
			opts := cliproxyexecutor.Options{OriginalRequest: tc.original}
			if got := originalRequestPayload(req, opts); !bytes.Equal(got, tc.want) {
				t.Fatalf("original request = %q, want %q", got, tc.want)
			}
		})
	}
}
