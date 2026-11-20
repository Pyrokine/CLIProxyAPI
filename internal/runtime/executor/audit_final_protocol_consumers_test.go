package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestAuditCodexFinalStreamOptionsConsumers(t *testing.T) {
	for _, source := range []translator.Format{translator.FormatCodex, translator.FormatOpenAIResponse} {
		for _, override := range []bool{false, true} {
			for _, allowed := range []bool{false, true} {
				for _, transport := range []string{"http", "http-stream", "ws", "ws-stream", "count"} {
					t.Run(
						fmt.Sprintf("%s/override=%t/allowed=%t/%s", source, override, allowed, transport),
						func(t *testing.T) {
							received := make(chan []byte, 1)
							server := httptest.NewServer(
								http.HandlerFunc(
									func(w http.ResponseWriter, r *http.Request) {
										if websocket.IsWebSocketUpgrade(r) {
											peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
											if err != nil {
												t.Error(err)
												return
											}
											defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
											_, body, err := peer.ReadMessage()
											if err != nil {
												t.Error(err)
												return
											}
											received <- body
											if err = peer.WriteMessage(
												websocket.TextMessage, []byte(runtimeCodexCompleted),
											); err != nil {
												t.Error(err)
											}
											return
										}
										body, err := io.ReadAll(r.Body)
										if err != nil {
											t.Error(err)
											return
										}
										received <- body
										w.Header().Set("Content-Type", "text/event-stream")
										_, _ = io.WriteString(w, "data: "+runtimeCodexCompleted+"\n\n")
									},
								),
							)
							defer server.Close()
							options := map[string]any{"include_usage": true, "unknown": "synthetic"}
							optionJSON := `{"include_usage":true,"unknown":"synthetic"}`
							if allowed {
								options["reasoning_summary_delivery"] = "concise"
								optionJSON = `{"include_usage":true,"unknown":"synthetic",` +
									`"reasoning_summary_delivery":` +
									`"concise"}`
							}
							cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
							if override {
								cfg.Payload.Override = []config.PayloadRule{
									{
										Models: []config.PayloadModelRule{
											{
												Name: "gpt-5.4", Protocol: "codex",
											},
										}, Params: map[string]any{
											"stream_options": options, "service_tier": "operator-tier",
										},
									},
								}
							}
							request := core.Request{
								Model: "gpt-5.4",
								Payload: []byte(
									`{"model":"gpt-5.4","input":"synthetic","stream_options":` + optionJSON + `}`,
								),
							}
							opts := core.Options{
								SourceFormat:   source,
								ResponseFormat: translator.FormatOpenAIResponse,
							}
							ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
							defer cancel()
							if strings.HasPrefix(transport, "ws") {
								ctx = core.WithDownstreamWebsocket(ctx)
							}
							execute := NewCodexAutoExecutor(cfg)
							credential := runtimeCodexAuth(server.URL)
							var body []byte
							var err error
							if transport == "count" {
								prepared, prepareErr := codexResponsesBody(
									ctx,
									cfg,
									credential,
									request.Model,
									request.Model,
									"codex",
									source,
									opts,
									request,
									false,
									false,
								)
								if prepareErr != nil {
									t.Fatal(prepareErr)
								}
								body = prepared.Body
								_, err = execute.CountTokens(ctx, credential, request, opts)
								if len(received) != 0 {
									t.Fatal("count dispatched an upstream request")
								}
							} else {
								if strings.HasSuffix(transport, "stream") {
									var result *core.StreamResult
									result, err = execute.ExecuteStream(ctx, credential, request, opts)
									if err == nil {
										for chunk := range result.Chunks {
											if chunk.Err != nil {
												err = chunk.Err
											}
										}
									}
								} else {
									_, err = execute.Execute(ctx, credential, request, opts)
								}
								if err == nil {
									body = <-received
								}
							}
							if err != nil {
								t.Fatal(err)
							}
							streamOptions := gjson.GetBytes(body, "stream_options")
							if allowed {
								if streamOptions.Raw != `{"reasoning_summary_delivery":"concise"}` {
									t.Fatalf("final options=%s body=%s", streamOptions.Raw, body)
								}
							} else if streamOptions.Exists() {
								t.Fatalf("unsupported options survived: %s", body)
							}
							if override && gjson.GetBytes(body, "service_tier").String() != "operator-tier" {
								t.Fatalf("operator rule changed: %s", body)
							}
						},
					)
				}
			}
		}
	}
}

func TestAuditOpenAICompatSourceTerminalAndErrors(t *testing.T) {
	const content = "data: {\"id\":\"synthetic-terminal\",\"object\":\"chat.completion.chun" +
		"k\",\"model\":\"synthetic-model\",\"choices\":[{\"index\":0,\"delta\":" +
		"{\"content\":\"synthetic\"},\"finish_reason\":null}]}\n\n"
	const usageFrame = "data: {\"id\":\"synthetic-terminal\",\"object\":\"chat.completion.chunk\",\"model\":" +
		"\"synthetic-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{" +
		"\"prompt_tokens\":7,\"completion_tokens\":5,\"total_tokens\":12}}\n\n"
	for _, format := range []translator.Format{translator.FormatOpenAI, translator.FormatOpenAIResponse} {
		for _, mode := range []string{
			"done-read-error", "done-open-pipe", "done-followed-error", "preterminal-read-error", "error-json",
			"error-event", "invalid-json",
		} {
			t.Run(
				format.String()+"/"+mode, func(t *testing.T) {
					plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
					registerTestUsagePlugin(t, plugin)
					wire := content + usageFrame
					success := strings.HasPrefix(mode, "done-")
					switch mode {
					case "done-read-error", "done-open-pipe":
						wire += "data: [DONE]\n\n"
					case "done-followed-error":
						wire += "data: [DONE]\n\ndata: {\"error\":{\"message\":\"after terminal\"}}\n\n"
					case "error-json":
						wire += "data: {\"error\":{\"message\":\"synthetic failure\"}}\n\ndata: [DONE]\n\n"
					case "error-event":
						wire += "event: error\ndata: {\"code\":\"synthetic\",\"message\":\"synthetic " +
							"failure\"}\n\ndata: " +
							"[DONE]\n\n"
					case "invalid-json":
						wire += "data: {broken\n\ndata: [DONE]\n\n"
					}
					var reader io.Reader = &auditImageFinalRead{payload: []byte(wire), err: io.ErrUnexpectedEOF}
					if mode == "done-open-pipe" {
						pipe, writer := io.Pipe()
						reader = pipe
						t.Cleanup(func() { _ = pipe.Close(); _ = writer.Close() })
						go func() { _, _ = io.WriteString(writer, wire) }()
					}
					body := &auditImageBody{Reader: reader, closed: make(chan struct{})}
					ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
					defer cancel()
					ctx = util.WithRoundTripper(
						ctx, devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								return &http.Response{
									StatusCode: http.StatusOK,
									Header:     http.Header{"Content-Type": {"text/event-stream"}}, Body: body,
									Request: request,
								}, nil
							},
						),
					)
					credential := &auth.Auth{
						ID: t.Name(), Provider: "synthetic",
						Attributes: map[string]string{
							"api_key":  "synthetic-key",
							"base_url": "http://127.0.0.1:1",
						},
					}
					result, err := NewOpenAICompatExecutor("synthetic", &config.Config{}).ExecuteStream(
						ctx, credential, core.Request{
							Model: "synthetic",
							Payload: []byte(
								`{"model":"synthetic","messages":[{"role":"user","content":"synthetic"}]}`,
							),
						}, core.Options{SourceFormat: translator.FormatOpenAI, ResponseFormat: format},
					)
					if err != nil {
						t.Fatal(err)
					}
					var output bytes.Buffer
					var streamErr error
					for chunk := range result.Chunks {
						output.Write(chunk.Payload)
						if chunk.Err != nil {
							streamErr = chunk.Err
						}
					}
					if (streamErr == nil) != success {
						t.Fatalf("success=%t error=%v output=%s", success, streamErr, output.Bytes())
					}
					if !success && bytes.Contains(output.Bytes(), []byte(`"response.completed"`)) {
						t.Fatalf("error stream completed: %s", output.Bytes())
					}
					record := testWaitForDevinUsageRecord(t, plugin.records)
					if record.Failed == success || record.Detail.InputTokens != 7 ||
						record.Detail.OutputTokens != 5 ||
						record.Detail.TotalTokens != 12 ||
						record.ResponseModel != "synthetic-model" {
						t.Fatalf("usage=%+v", record)
					}
					select {
					case <-body.closed:
					default:
						t.Fatal("response body remains open")
					}
				},
			)
		}
	}
}

func TestAuditOpenAICompatInteractionsTerminalDropsLateText(t *testing.T) {
	const first = "data: {\"id\":\"synthetic-terminal\",\"model\":\"synthetic-model\",\"choices\":[{\"index\"" +
		":0,\"delta\":{\"content\":\"first\"},\"finish_reason\":null}]}\n\n"
	const completed = "data: {\"id\":\"synthetic-terminal\",\"model\":\"synthetic-model\",\"choices\":[],\"" +
		"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5,\"total_tokens\":12}}\n\n"
	const late = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":" +
		"\"late-content\"},\"finish_reason\":" +
		"null}]}\n\n"
	body := &auditImageBody{
		Reader: strings.NewReader(
			first + completed + late + "data: [DONE]\n\n" + late,
		), closed: make(chan struct{}),
	}
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	calls := 0
	ctx = util.WithRoundTripper(
		ctx, devinTestRoundTripper(
			func(request *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body,
					Request: request,
				}, nil
			},
		),
	)
	credential := &auth.Auth{
		ID: t.Name(), Provider: "synthetic",
		Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "http://synthetic.invalid/v1"},
	}
	payload := []byte(`{"model":"synthetic-model","messages":[{"role":"user","content":"synthetic"}]}`)
	result, err := NewOpenAICompatExecutor("synthetic", &config.Config{}).ExecuteStream(
		ctx, credential,
		core.Request{Model: "synthetic-model", Payload: payload},
		core.Options{
			SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatInteractions,
			OriginalRequest: payload,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if calls != 1 || !bytes.Contains(output.Bytes(), []byte("first")) || bytes.Contains(
		output.Bytes(), []byte("late-content"),
	) {
		t.Fatalf("calls=%d output=%s", calls, output.Bytes())
	}
	if bytes.Count(output.Bytes(), []byte(`"event_type":"interaction.completed"`)) != 1 || bytes.Count(
		output.Bytes(), []byte("data: [DONE]"),
	) != 1 {
		t.Fatalf("terminal events changed: %s", output.Bytes())
	}
	record := testWaitForDevinUsageRecord(t, plugin.records)
	if record.Failed || !record.Completed || record.Detail.InputTokens != 7 ||
		record.Detail.OutputTokens != 5 ||
		record.Detail.TotalTokens != 12 {
		t.Fatalf("usage=%+v", record)
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("response body remains open")
	}
}
