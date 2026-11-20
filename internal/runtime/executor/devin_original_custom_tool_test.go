package executor

import (
	"bytes"
	"context"
	"encoding/json"
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
	"google.golang.org/protobuf/encoding/protowire"
)

func TestRegressionIssue5910_CustomToolCallInvalidJSONStr(t *testing.T) {
	// Upstream Devin sends raw arguments for custom tool calls in field 4 (invalid_json_str),
	// parse error in field 5 (invalid_json_err), and custom flag in field 6 (is_custom_tool_call).
	var tc []byte
	tc = protowire.AppendTag(tc, 1, protowire.BytesType)
	tc = protowire.AppendString(tc, "call_custom_1")
	tc = protowire.AppendTag(tc, 2, protowire.BytesType)
	tc = protowire.AppendString(tc, "bash")
	// Field 4: invalid_json_str = "ls -la"
	tc = protowire.AppendTag(tc, 4, protowire.BytesType)
	tc = protowire.AppendString(tc, "ls -la")
	// Field 5: invalid_json_err = "not valid json"
	tc = protowire.AppendTag(tc, 5, protowire.BytesType)
	tc = protowire.AppendString(tc, "not valid json")
	// Field 6: is_custom_tool_call = true
	tc = protowire.AppendTag(tc, 6, protowire.VarintType)
	tc = protowire.AppendVarint(tc, 1)

	var f1 []byte
	f1 = protowire.AppendTag(f1, 6, protowire.BytesType)
	f1 = protowire.AppendBytes(f1, tc)

	var buf bytes.Buffer
	buf.Write(helps.WrapConnectEnvelope(f1))
	buf.Write(helps.WrapConnectEnvelopeWithFlag(helps.ConnectFlagEndStream, []byte(`{}`)))

	interactionsJSON, responseLogJSON, _, err := consumeDevinFrames(&buf, "devin/swe-2")
	if err != nil {
		t.Fatalf("consumeDevinFramesToInteractions failed: %v", err)
	}

	var respLog devinResponseLog
	if err := json.Unmarshal(
		bytes.TrimPrefix(responseLogJSON, []byte("DEVIN CONNECT RESPONSE\n")), &respLog,
	); err != nil {
		t.Fatalf("decode response log: %v", err)
	}

	if len(respLog.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(respLog.ToolCalls))
	}
	if respLog.ToolCalls[0].Arguments != "ls -la" {
		t.Errorf(
			"expected Arguments %q from invalid_json_str, got %q",
			"ls -la",
			respLog.ToolCalls[0].Arguments,
		)
	}

	steps := gjson.GetBytes(interactionsJSON, "steps").Array()
	if len(steps) != 1 {
		t.Fatalf("expected 1 step in interactions JSON, got %d", len(steps))
	}
	if steps[0].Get("name").String() != "bash" || steps[0].Get("id").String() != "call_custom_1" {
		t.Errorf("unexpected step 0: %s", steps[0].Raw)
	}
	if steps[0].Get("arguments").String() != "ls -la" {
		t.Errorf("expected step 0 arguments %q, got %q", "ls -la", steps[0].Get("arguments").String())
	}

	// Also verify streaming receives raw arguments for custom tool call
	var streamBuf bytes.Buffer
	streamBuf.Write(helps.WrapConnectEnvelope(f1))
	streamBuf.Write(helps.WrapConnectEnvelopeWithFlag(helps.ConnectFlagEndStream, []byte(`{}`)))

	exec := NewDevinExecutor(&config.Config{})
	out := make(chan cliproxyexecutor.StreamChunk, 50)
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatInteractions,
	}

	go func() {
		defer close(out)
		exec.streamDevinFrames(
			context.Background(),
			&streamBuf,
			cliproxyexecutor.Request{Model: "devin/swe-2"},
			opts,
			nil,
			newUsageReporter(context.Background(), exec, "devin/swe-2", nil),
			out,
		)
	}()

	var streamArgs strings.Builder
	for chunk := range out {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream error: %v", chunk.Err)
		}
		lines := strings.SplitSeq(string(chunk.Payload), "\n")
		for line := range lines {
			if after, ok := strings.CutPrefix(line, "data: "); ok {
				data := after
				if gjson.Get(data, "event_type").String() == "step.delta" && gjson.Get(
					data, "delta.type",
				).String() == "arguments_delta" {
					streamArgs.WriteString(gjson.Get(data, "delta.arguments").String())
				}
			}
		}
	}

	if streamArgs.String() != "ls -la" {
		t.Errorf("expected stream delta arguments %q, got %q", "ls -la", streamArgs.String())
	}
}

func TestDevinCustomToolArgumentsAcrossNativeAndResponses(t *testing.T) {
	for _, format := range []sdktranslator.Format{
		sdktranslator.FormatInteractions, sdktranslator.FormatOpenAIResponse,
	} {
		for _, streaming := range []bool{false, true} {
			for _, fragments := range [][]string{{"ls -", "la"}, {""}, {"printf \"你好\"", " <data>\n"}} {
				t.Run(
					fmt.Sprintf("%s/stream=%t/%q", format, streaming, fragments), func(t *testing.T) {
						var frames [][]byte
						for index, fragment := range fragments {
							var tool []byte
							if index == 0 {
								tool = testDevinBytesField(tool, 1, []byte("call_custom_1"))
								tool = testDevinBytesField(tool, 2, []byte("bash"))
							}
							tool = testDevinBytesField(tool, 4, []byte(fragment))
							tool = testDevinVarintField(tool, 6, 1)
							frames = append(frames, testDevinBytesField(nil, 6, tool))
						}
						wire := testDevinConnectStream(frames...)
						calls := 0
						ctx := util.WithRoundTripper(
							t.Context(), devinTestRoundTripper(
								func(request *http.Request) (*http.Response, error) {
									calls++
									return &http.Response{
										StatusCode: http.StatusOK,
										Header: http.Header{
											"Content-Type": []string{"application/connect+proto"},
										},
										Body: io.NopCloser(bytes.NewReader(wire)), Request: request,
									}, nil
								},
							),
						)
						executor := NewDevinExecutor(&config.Config{})
						auth := &cliproxyauth.Auth{
							ID: "synthetic-devin-custom", Provider: "devin", Attributes: map[string]string{
								"api_key": "synthetic-token", "base_url": "https://synthetic.devin.invalid",
							},
						}
						payload := []byte(`{"input":[{"type":"user_input","content":"hello"}]}`)
						if format == sdktranslator.FormatOpenAIResponse {
							payload = []byte(
								`{"input":"hello","tools":[{"type":"custom","name":"bash","format":{"type":"text"}}]}`,
							)
						}
						req := cliproxyexecutor.Request{Model: "devin/swe-2", Payload: payload}
						opts := cliproxyexecutor.Options{SourceFormat: format, ResponseFormat: format}
						want := strings.Join(fragments, "")
						if !streaming {
							response, err := executor.Execute(ctx, auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							path := "steps.0.arguments"
							if format == sdktranslator.FormatOpenAIResponse {
								path = "output.0.input"
								if gjson.GetBytes(response.Payload, "output.0.type").
									String() !=
									"custom_tool_call" {
									t.Fatalf("custom declaration lost response kind: %s", response.Payload)
								}
							}
							if got := gjson.GetBytes(
								response.Payload, path,
							); !got.Exists() || got.Type != gjson.String || got.String() != want {
								t.Fatalf(
									"custom arguments=%s expected=%q; response=%s",
									got.Raw,
									want,
									response.Payload,
								)
							}
						} else {
							result, err := executor.ExecuteStream(ctx, auth, req, opts)
							if err != nil {
								t.Fatal(err)
							}
							var arguments strings.Builder
							completed := false
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								for _, event := range devinContractEvents(t, []cliproxyexecutor.StreamChunk{chunk}) {
									if format == sdktranslator.FormatInteractions {
										if event.Get("delta.type").String() == "arguments_delta" {
											arguments.WriteString(event.Get("delta.arguments").String())
										}
										completed = completed ||
											event.Get("event_type").String() ==
												"interaction.completed"
									} else {
										if event.Get("type").String() == "response.custom_tool_call_input.delta" {
											arguments.WriteString(event.Get("delta").String())
										}
										completed = completed || event.Get("type").String() == "response.completed"
									}
								}
								if chunk.Acknowledge != nil {
									chunk.Acknowledge(nil)
								}
							}
							if arguments.String() != want || !completed {
								t.Fatalf(
									"stream custom arguments=%q completed=%t expected=%q", arguments.String(),
									completed, want,
								)
							}
						}
						if calls != 1 {
							t.Fatalf("synthetic upstream calls=%d expected=1", calls)
						}
					},
				)
			}
		}
	}
}
