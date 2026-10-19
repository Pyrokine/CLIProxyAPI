package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func devinContractExecute(t *testing.T, payload []byte, opts cliproxyexecutor.Options, stream bool, response []byte) ([]cliproxyexecutor.StreamChunk, [][]byte, error) {
	t.Helper()
	var requests [][]byte
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = util.WithRoundTripper(ctx, devinTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "devin-fixture.invalid" {
			return nil, fmt.Errorf("unexpected synthetic destination: %s", req.URL.Host)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		requests = append(requests, body)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(response)), Request: req}, nil
	}))
	executor := NewDevinExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "devin-contract-fixture", Attributes: map[string]string{
		"api_key": "synthetic-session", "base_url": "https://devin-fixture.invalid", "device_seed": "synthetic-device",
	}}
	req := cliproxyexecutor.Request{Model: "devin/swe-2", Payload: payload}
	if len(opts.OriginalRequest) == 0 {
		opts.OriginalRequest = payload
	}
	if !stream {
		result, err := executor.Execute(ctx, auth, req, opts)
		if err != nil {
			return nil, requests, err
		}
		return []cliproxyexecutor.StreamChunk{{Payload: result.Payload}}, requests, nil
	}
	result, err := executor.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		return nil, requests, err
	}
	var chunks []cliproxyexecutor.StreamChunk
	for chunk := range result.Chunks {
		chunks = append(chunks, chunk)
	}
	return chunks, requests, nil
}

func devinContractEvents(t *testing.T, chunks []cliproxyexecutor.StreamChunk) []gjson.Result {
	t.Helper()
	var events []gjson.Result
	for _, chunk := range chunks {
		if len(chunk.Payload) == 0 {
			continue
		}
		if gjson.ValidBytes(chunk.Payload) {
			events = append(events, gjson.ParseBytes(chunk.Payload))
			continue
		}
		for _, line := range bytes.Split(chunk.Payload, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(data, []byte("[DONE]")) {
				continue
			}
			if !gjson.ValidBytes(data) {
				t.Fatalf("invalid SSE JSON: %s", data)
			}
			events = append(events, gjson.ParseBytes(data))
		}
	}
	return events
}

func devinContractWirePrompts(t *testing.T, requests [][]byte) [][]byte {
	t.Helper()
	if len(requests) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(requests))
	}
	flag, body, err := helps.ReadConnectFrame(bytes.NewReader(requests[0]))
	if err != nil || flag != helps.ConnectFlagData {
		t.Fatalf("Connect request = flag %d, error %v", flag, err)
	}
	return allDevinBytesFields(t, body, 3)
}

func devinContractPNG(t *testing.T, red uint8) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: red, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(encoded.Bytes())
}

func TestDevinStopReasonsReachResponseConsumers(t *testing.T) {
	payload := []byte(`{"input":[{"type":"user_input","content":"question"}]}`)
	for _, tc := range []struct {
		reason uint64
		finish string
		detail string
	}{
		{reason: 0}, {reason: 2},
		{reason: 1, finish: "length", detail: "max_output_tokens"},
		{reason: 3, finish: "length", detail: "max_output_tokens"},
		{reason: 11, finish: "content_filter", detail: "content_filter"},
	} {
		for _, withTool := range []bool{false, true} {
			for _, format := range []sdktranslator.Format{sdktranslator.FormatInteractions, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("reason_%d/tool_%t/%s/stream_%t", tc.reason, withTool, format, stream), func(t *testing.T) {
						frame := testDevinBytesField(nil, 3, []byte("partial answer"))
						if withTool {
							tool := testDevinBytesField(nil, 1, []byte("call_1"))
							tool = testDevinBytesField(tool, 2, []byte("lookup"))
							tool = testDevinBytesField(tool, 3, []byte(`{"q":"value"}`))
							frame = testDevinBytesField(frame, 6, tool)
						}
						frame = testDevinVarintField(frame, 5, tc.reason)
						chunks, requests, err := devinContractExecute(t, payload, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: format}, stream, testDevinConnectStream(frame, nil))
						if err != nil || len(requests) != 1 {
							t.Fatalf("request error=%v dispatches=%d", err, len(requests))
						}
						for _, chunk := range chunks {
							if chunk.Err != nil {
								t.Fatalf("stream error: %v", chunk.Err)
							}
						}
						var terminal gjson.Result
						for _, event := range devinContractEvents(t, chunks) {
							if !stream || event.Get("event_type").String() == "interaction.completed" || event.Get("type").String() == "response.completed" || event.Get("type").String() == "response.incomplete" || (event.Get("choices.0.finish_reason").Exists() && event.Get("choices.0.finish_reason").Type != gjson.Null) {
								terminal = event
							}
						}
						if !terminal.Exists() {
							t.Fatal("missing terminal response")
						}
						switch format {
						case sdktranslator.FormatOpenAI:
							want := tc.finish
							if want == "" {
								want = "stop"
								if withTool {
									want = "tool_calls"
								}
							}
							if got := terminal.Get("choices.0.finish_reason").String(); got != want {
								t.Fatalf("finish reason = %q, want %q: %s", got, want, terminal.Raw)
							}
						default:
							value := terminal
							if stream {
								if format == sdktranslator.FormatInteractions {
									value = terminal.Get("interaction")
								} else {
									value = terminal.Get("response")
								}
							}
							wantStatus := "completed"
							if tc.finish != "" {
								wantStatus = "incomplete"
							}
							if value.Get("status").String() != wantStatus || value.Get("incomplete_details.reason").String() != tc.detail {
								t.Fatalf("terminal = %s, want %s/%s", terminal.Raw, wantStatus, tc.detail)
							}
							if format == sdktranslator.FormatInteractions && value.Get("finish_reason").String() != tc.finish {
								t.Fatalf("Interactions finish reason = %s", terminal.Raw)
							}
						}
					})
				}
			}
		}
	}
}

func TestDevinUnsupportedUserTurnsFailBeforeDispatch(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		source        sdktranslator.Format
	}{
		{"remote_image", `{"input":[{"type":"user_input","content":[{"type":"image","uri":"https://media.invalid/image.png"}]}]}`, sdktranslator.FormatInteractions},
		{"audio", `{"input":[{"type":"user_input","content":[{"type":"audio","mime_type":"audio/wav","data":"AA=="}]}]}`, sdktranslator.FormatInteractions},
		{"document", `{"input":[{"type":"user_input","content":[{"type":"document","mime_type":"application/pdf","data":"AA=="}]}]}`, sdktranslator.FormatInteractions},
		{"whitespace_media", `{"input":[{"type":"user_input","content":[{"type":"text","text":"  \n"},{"type":"image","uri":"https://media.invalid/image.png"}]}]}`, sdktranslator.FormatInteractions},
		{"empty_history_turn", `{"input":[{"type":"user_input","content":[{"type":"audio","data":"AA=="}]},{"type":"model_output","text":"history"},{"type":"user_input","content":"continue"}]}`, sdktranslator.FormatInteractions},
		{"messages_audio", `{"messages":[{"role":"user","content":[{"type":"audio","data":"AA=="}]}]}`, sdktranslator.FormatInteractions},
		{"openai_remote_image", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://media.invalid/image.png"}}]}]}`, sdktranslator.FormatOpenAI},
		{"responses_remote_image", `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://media.invalid/image.png"}]}]}`, sdktranslator.FormatOpenAIResponse},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", tc.name, stream), func(t *testing.T) {
				_, requests, err := devinContractExecute(t, []byte(tc.payload), cliproxyexecutor.Options{SourceFormat: tc.source, ResponseFormat: sdktranslator.FormatInteractions}, stream, testDevinConnectStream())
				var status statusErr
				if !errors.As(err, &status) || status.code != http.StatusBadRequest || !status.requestScoped || status.dispatchPhase != cliproxyexecutor.DispatchPhaseNotSent || len(requests) != 0 {
					t.Fatalf("error=%v dispatches=%d, want request-scoped 400/NotSent before dispatch", err, len(requests))
				}
			})
		}
	}
	for _, stream := range []bool{false, true} {
		chunks, requests, err := devinContractExecute(t, []byte(`{"input":[{"type":"user_input","content":[{"type":"text","text":"keep text"},{"type":"audio","data":"AA=="}]}]}`), cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions}, stream, testDevinConnectStream())
		if err != nil || len(chunks) == 0 {
			t.Fatalf("sendable text error=%v", err)
		}
		prompts := devinContractWirePrompts(t, requests)
		if len(prompts) != 1 || string(firstDevinBytesField(t, prompts[0], 3)) != "keep text" {
			t.Fatalf("sendable text lost: %x", prompts)
		}
	}
}

func TestDevinUserTextMayBeEmpty(t *testing.T) {
	for _, text := range []string{"", "  \n"} {
		for _, stream := range []bool{false, true} {
			payload := []byte(fmt.Sprintf(`{"input":[{"type":"user_input","content":%q}]}`, text))
			_, requests, err := devinContractExecute(t, payload, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions}, stream, testDevinConnectStream())
			if err != nil {
				t.Fatalf("empty text request rejected: %v", err)
			}
			prompts := devinContractWirePrompts(t, requests)
			if len(prompts) != 1 || string(firstDevinBytesField(t, prompts[0], 3)) != text {
				t.Fatalf("empty text history changed: %x", prompts)
			}
		}
	}
}

func TestDevinInlineImagesReachWireAttachments(t *testing.T) {
	encoded := devinContractPNG(t, 1)
	for _, field := range []string{"data", "source.data", "inline_data.data", "url", "image_url", "image_url.url", "uri", "file_uri", "fileUri"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", field, stream), func(t *testing.T) {
				part := map[string]any{"type": "image"}
				value := "data:image/png;base64," + encoded
				switch field {
				case "data":
					part["data"], part["mime_type"] = encoded, "image/png"
				case "source.data":
					part["source"] = map[string]string{"type": "base64", "data": encoded, "media_type": "image/png"}
				case "inline_data.data":
					part["inline_data"] = map[string]string{"data": encoded, "mime_type": "image/png"}
				case "image_url.url":
					part["image_url"] = map[string]string{"url": value}
				default:
					part[field] = value
				}
				payload, err := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "user_input", "content": []any{part}}}})
				if err != nil {
					t.Fatal(err)
				}
				_, requests, err := devinContractExecute(t, payload, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions}, stream, testDevinConnectStream())
				if err != nil {
					t.Fatal(err)
				}
				prompts := devinContractWirePrompts(t, requests)
				if len(prompts) != 1 {
					t.Fatalf("prompts=%d", len(prompts))
				}
				images := allDevinBytesFields(t, prompts[0], 10)
				if len(images) != 1 || string(firstDevinBytesField(t, images[0], 1)) != encoded || string(firstDevinBytesField(t, images[0], 2)) != "image/png" || string(firstDevinBytesField(t, prompts[0], 3)) != "[Image 1: pasted_image_1.png]" {
					t.Fatalf("wire image fields = %x", prompts[0])
				}
			})
		}
	}
}

func TestDevinStructuredToolImagesReachWireAttachments(t *testing.T) {
	encoded := devinContractPNG(t, 2)
	business := `{"text":"failed","exit_code":1,"retryable":true}`
	parts := `[{"type":"text","text":"screen captured"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + encoded + `"}},` + business + `]`
	for _, tc := range []struct {
		name, payload string
		source        sdktranslator.Format
	}{
		{"interactions", `{"input":[{"type":"function_call","call_id":"call_img","name":"screenshot","arguments":{}},{"type":"function_result","call_id":"call_img","result":` + parts + `}]}`, sdktranslator.FormatInteractions},
		{"messages", `{"messages":[{"role":"assistant","tool_calls":[{"id":"call_img","function":{"name":"screenshot","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_img","content":` + parts + `}]}`, sdktranslator.FormatInteractions},
		{"claude", `{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_img","name":"screenshot","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_img","content":` + parts + `}]}]}`, sdktranslator.FormatClaude},
		{"responses", `{"input":[{"type":"function_call","call_id":"call_img","name":"screenshot","arguments":"{}"},{"type":"function_call_output","call_id":"call_img","output":` + parts + `}]}`, sdktranslator.FormatOpenAIResponse},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", tc.name, stream), func(t *testing.T) {
				_, requests, err := devinContractExecute(t, []byte(tc.payload), cliproxyexecutor.Options{SourceFormat: tc.source, ResponseFormat: sdktranslator.FormatInteractions}, stream, testDevinConnectStream())
				if err != nil {
					t.Fatal(err)
				}
				prompts := devinContractWirePrompts(t, requests)
				if len(prompts) != 2 {
					t.Fatalf("prompts=%d, want 2", len(prompts))
				}
				result := prompts[1]
				text := string(firstDevinBytesField(t, result, 3))
				images := allDevinBytesFields(t, result, 10)
				if firstDevinVarintField(t, result, 2) != 4 || string(firstDevinBytesField(t, result, 7)) != "call_img" || len(images) != 1 || string(firstDevinBytesField(t, images[0], 1)) != encoded || string(firstDevinBytesField(t, images[0], 2)) != "image/png" {
					t.Fatalf("tool attachment wire=%x", result)
				}
				if text != "[Image 1: pasted_image_1.png]\n\nscreen captured\n"+business {
					t.Fatalf("tool text=%q", text)
				}
			})
		}
	}
	for _, result := range []string{`{"text":"business","exit_code":0}`, `[{"type":"text","text":"business","exit_code":0}]`, `{"content":"business","metadata":{"text":"keep"}}`, `""`, `[]`} {
		t.Run("business_"+result, func(t *testing.T) {
			payload := []byte(`{"input":[{"type":"function_call","id":"call_1","name":"run","arguments":{}},{"type":"function_result","call_id":"call_1","result":` + result + `}]}`)
			_, requests, err := devinContractExecute(t, payload, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions}, false, testDevinConnectStream())
			if err != nil {
				t.Fatal(err)
			}
			prompts := devinContractWirePrompts(t, requests)
			want := result
			if result == `""` {
				want = ""
			}
			if len(prompts) != 2 || string(firstDevinBytesField(t, prompts[1], 3)) != want || len(allDevinBytesFields(t, prompts[1], 10)) != 0 {
				t.Fatalf("business result wire=%x want text %q", prompts, want)
			}
		})
	}
}

func TestDevinToolResultsMatchPendingWireCalls(t *testing.T) {
	encoded := devinContractPNG(t, 3)
	original := []byte(`{"messages":[{"role":"user","content":[{"type":"image","data":"` + encoded + `","mime_type":"image/png"}]}]}`)
	for _, payload := range []string{
		`{"input":[{"type":"function_result","call_id":"trimmed","result":"business output"},{"type":"function_call","id":"call_A","name":"first","arguments":{}},{"type":"function_call","id":"call_B","name":"second","arguments":{}},{"type":"function_result","call_id":"call_A","result":"A"},{"type":"function_result","call_id":"call_A","result":"duplicate"},{"type":"function_result","result":"B"},{"type":"function_result","result":"no call"},{"type":"user_input","content":"look"}]}`,
		`{"messages":[{"role":"tool","tool_call_id":"trimmed","content":"business output"},{"role":"assistant","tool_calls":[{"id":"call_A","function":{"name":"first","arguments":"{}"}},{"id":"call_B","function":{"name":"second","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_A","content":"A"},{"role":"tool","tool_call_id":"call_A","content":"duplicate"},{"role":"tool","content":"B"},{"role":"tool","content":"no call"},{"role":"user","content":"look"}]}`,
	} {
		for _, stream := range []bool{false, true} {
			_, requests, err := devinContractExecute(t, []byte(payload), cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions, OriginalRequest: original}, stream, testDevinConnectStream())
			if err != nil {
				t.Fatal(err)
			}
			prompts := devinContractWirePrompts(t, requests)
			wantSources := []uint64{1, 2, 4, 1, 4, 1, 1}
			wantIDs := []string{"", "", "call_A", "", "call_B", "", ""}
			wantTexts := []string{"business output", "", "A", "duplicate", "B", "no call", "[Image 1: pasted_image_1.png]\n\nlook"}
			if len(prompts) != len(wantSources) {
				t.Fatalf("prompts=%d", len(prompts))
			}
			for index, prompt := range prompts {
				ids := allDevinBytesFields(t, prompt, 7)
				if wantIDs[index] == "" {
					if len(ids) != 0 {
						t.Fatalf("prompt %d retained a tool ID: %x", index, prompt)
					}
				} else if len(ids) != 1 || string(ids[0]) != wantIDs[index] {
					t.Fatalf("prompt %d tool ID = %q, want %q", index, ids, wantIDs[index])
				}
				if firstDevinVarintField(t, prompt, 2) != wantSources[index] || string(firstDevinBytesField(t, prompt, 3)) != wantTexts[index] {
					t.Fatalf("prompt %d wire=%x", index, prompt)
				}
				images := allDevinBytesFields(t, prompt, 10)
				if index == 6 {
					if len(images) != 1 || string(firstDevinBytesField(t, images[0], 1)) != encoded {
						t.Fatalf("user image wire=%x", prompt)
					}
				} else if len(images) != 0 {
					t.Fatalf("image assigned to prompt %d", index)
				}
			}
		}
	}
}

func TestDevinTrailerFailureDeliversQueuedOutput(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatInteractions, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
		for _, scenario := range []string{"queued", "started_tool", "invalid_arguments", "custom_tool"} {
			t.Run(string(format)+"/"+scenario, func(t *testing.T) {
				tool := testDevinBytesField(nil, 1, []byte("call_1"))
				tool = testDevinBytesField(tool, 2, []byte("bash"))
				arguments := `{"command":"echo value"}`
				if scenario == "invalid_arguments" {
					arguments = `{"command":`
				}
				tool = testDevinBytesField(tool, 3, []byte(arguments))
				if scenario == "custom_tool" {
					tool = testDevinVarintField(tool, 6, 1)
				}
				var wire []byte
				if scenario != "started_tool" {
					wire = helps.WrapConnectEnvelope(testDevinBytesField(nil, 9, []byte("reason")))
				}
				wire = append(wire, helps.WrapConnectEnvelope(testDevinBytesField(nil, 6, tool))...)
				wire = append(wire, helps.WrapConnectEnvelope(testDevinBytesField(nil, 3, []byte("partial business answer")))...)
				wire = append(wire, helps.WrapConnectEnvelope(testDevinBytesField(nil, 10, []byte("late-signature")))...)
				wire = append(wire, helps.WrapConnectEnvelopeWithFlag(helps.ConnectFlagEndStream, []byte(`{"error":{"code":"unavailable","message":"synthetic failure"}}`))...)
				chunks, requests, err := devinContractExecute(t, []byte(`{"input":[{"type":"user_input","content":"question"}]}`), cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: format}, true, wire)
				if err != nil || len(requests) != 1 {
					t.Fatalf("error=%v dispatches=%d", err, len(requests))
				}
				errorCount := 0
				for index, chunk := range chunks {
					if bytes.Contains(chunk.Payload, []byte("[DONE]")) || chunk.Terminal {
						t.Fatalf("success terminal on failure: %s", chunk.Payload)
					}
					if chunk.Err != nil {
						errorCount++
						if index != len(chunks)-1 {
							t.Fatal("data followed stream error")
						}
						want := http.StatusServiceUnavailable
						if scenario == "custom_tool" {
							want = http.StatusBadGateway
						}
						if testExecutorStatusCode(chunk.Err) != want {
							t.Fatalf("stream error=%v want HTTP %d", chunk.Err, want)
						}
					}
				}
				if errorCount != 1 {
					t.Fatalf("stream errors=%d", errorCount)
				}
				var text, args string
				failurePosition, toolDonePosition, textPosition := -1, -1, -1
				toolIndex := int64(-1)
				failedEvents := 0
				for index, event := range devinContractEvents(t, chunks) {
					kind := event.Get("event_type").String()
					if kind == "" {
						kind = event.Get("type").String()
					}
					switch kind {
					case "interaction.completed", "response.completed", "response.incomplete":
						t.Fatalf("success terminal: %s", event.Raw)
					case "response.failed", "interaction.failed":
						failedEvents++
						failurePosition = index
					case "step.start":
						if event.Get("step.type").String() == "function_call" {
							toolIndex = event.Get("index").Int()
						}
					case "step.stop":
						if toolIndex >= 0 && event.Get("index").Int() == toolIndex {
							toolDonePosition = index
						}
					case "step.delta":
						if event.Get("delta.type").String() == "text" {
							text += event.Get("delta.text").String()
							textPosition = index
						}
						if event.Get("delta.type").String() == "arguments_delta" {
							args += event.Get("delta.arguments").String()
						}
					case "response.output_text.delta":
						text += event.Get("delta").String()
						textPosition = index
					case "response.function_call_arguments.delta":
						args += event.Get("delta").String()
					case "response.output_item.done":
						if event.Get("item.type").String() == "function_call" {
							toolDonePosition = index
						}
					default:
						if event.Get("error").Exists() {
							failedEvents++
							failurePosition = index
						}
						text += event.Get("choices.0.delta.content").String()
						if event.Get("choices.0.delta.content").String() != "" {
							textPosition = index
						}
						for _, call := range event.Get("choices.0.delta.tool_calls").Array() {
							args += call.Get("function.arguments").String()
						}
						if event.Get("choices.0.finish_reason").Exists() && event.Get("choices.0.finish_reason").Type != gjson.Null {
							t.Fatalf("chat success terminal: %s", event.Raw)
						}
					}
				}
				if text != "partial business answer" || failedEvents != 1 || textPosition < 0 || failurePosition <= textPosition {
					t.Fatalf("text=%q failure events=%d positions text=%d failure=%d", text, failedEvents, textPosition, failurePosition)
				}
				if scenario == "queued" || scenario == "started_tool" {
					if args != arguments || (format != sdktranslator.FormatOpenAI && (toolDonePosition < 0 || toolDonePosition >= textPosition)) {
						t.Fatalf("args=%q tool done=%d text=%d", args, toolDonePosition, textPosition)
					}
				} else if toolDonePosition >= 0 {
					t.Fatal("invalid tool was reported complete")
				}
			})
		}
	}
}

func TestDevinFailureCleanupHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	executor := NewDevinExecutor(&config.Config{})
	out := make(chan cliproxyexecutor.StreamChunk)
	state := &devinStreamState{executor: executor, ctx: ctx, responseFormat: sdktranslator.FormatInteractions, out: out, response: newDevinStreamingResponseAccumulator(false), reporter: newUsageReporter(ctx, executor, "devin/swe-2", nil), activeTextType: "thought", activeTextIndex: 0, pendingActions: []devinPendingAction{{text: "queued text"}}}
	finished := make(chan struct{})
	go func() { state.fail(devinProtocolError("synthetic failure"), "trailer_error"); close(finished) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("failure cleanup blocked after cancellation")
	}
	if !state.failed {
		t.Fatal("failure state was not recorded")
	}
}

func TestDevinAdjacentUserPartsRetainSendableText(t *testing.T) {
	payload := []byte(`{"input":[{"type":"user_input","content":[{"type":"text","text":"keep me"}]},{"type":"user_input","content":[{"type":"image","uri":"https://synthetic.invalid/image.png"}]}]}`)
	for _, stream := range []bool{false, true} {
		chunks, requests, err := devinContractExecute(t, payload, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions}, stream, testDevinConnectStream())
		if err != nil {
			t.Fatalf("sendable adjacent user content was rejected: %v", err)
		}
		if len(chunks) == 0 {
			t.Fatal("successful execution returned no payload")
		}
		prompts := devinContractWirePrompts(t, requests)
		if len(prompts) != 1 || firstDevinVarintField(t, prompts[0], 2) != 1 || string(firstDevinBytesField(t, prompts[0], 3)) != "keep me" {
			t.Fatalf("sendable adjacent user content or dropped-media omission changed: %x", prompts)
		}
	}
}

func TestDevinUserRunPreservesContentAndOmitsDroppedMedia(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		wantError     bool
		wantPrompts   int
	}{
		{"adjacent-text", `{"input":[{"type":"user_input","content":[{"type":"audio","data":"AA=="}]},{"type":"user_input","content":"keep text"}]}`, false, 1},
		{"tool-result", `{"input":[{"type":"function_call","call_id":"call_keep","name":"lookup","arguments":{}},{"type":"function_result","call_id":"call_keep","result":"keep result"},{"type":"user_input","content":[{"type":"audio","data":"AA=="}]}]}`, false, 2},
		{"media-only", `{"input":[{"type":"user_input","content":[{"type":"audio","data":"AA=="}]}]}`, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, requests, err := devinContractExecute(t, []byte(tc.payload), cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions}, false, testDevinConnectStream())
			if tc.wantError {
				if testExecutorStatusCode(err) != 400 || len(requests) != 0 {
					t.Fatalf("error=%v requests=%d, want local 400 before dispatch", err, len(requests))
				}
				return
			}
			if err != nil {
				t.Fatalf("sendable user run rejected: %v", err)
			}
			prompts := devinContractWirePrompts(t, requests)
			if len(prompts) != tc.wantPrompts {
				t.Fatalf("prompts=%d, want %d without an emptied media prompt", len(prompts), tc.wantPrompts)
			}
			for _, prompt := range prompts {
				if firstDevinVarintField(t, prompt, 2) == 1 && len(firstDevinBytesField(t, prompt, 3)) == 0 {
					t.Fatal("an emptied media-only user prompt reached the wire")
				}
			}
		})
	}
}
