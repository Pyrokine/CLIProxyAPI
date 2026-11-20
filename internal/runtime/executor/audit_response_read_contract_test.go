package executor

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestAuditDevinFailureRetainsObservedSnapshot(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, outcome := range []string{"completed", "unavailable", "missing-eos"} {
			t.Run(
				fmt.Sprintf("stream=%t/%s", streaming, outcome), func(t *testing.T) {
					plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
					registerTestUsagePlugin(t, plugin)
					metrics := testDevinVarintField(nil, 2, 7)
					metrics = testDevinVarintField(metrics, 3, 3)
					metrics = testDevinVarintField(metrics, 4, 11)
					metrics = testDevinVarintField(metrics, 5, 2)
					metrics = testDevinBytesField(metrics, 9, []byte("devin/swe-3"))
					frame := testDevinBytesField(nil, 3, []byte("synthetic answer"))
					frame = testDevinBytesField(frame, 7, metrics)
					wire := helps.WrapConnectEnvelope(frame)
					if outcome != "missing-eos" {
						trailer := `{}`
						if outcome == "unavailable" {
							trailer = `{"error":{"code":"unavailable","message":"synthetic failure"}}`
						}
						wire = append(
							wire,
							helps.WrapConnectEnvelopeWithFlag(helps.ConnectFlagEndStream, []byte(trailer))...,
						)
					}
					body := &auditImageBody{Reader: bytes.NewReader(wire), closed: make(chan struct{})}
					ctx := util.WithRoundTripper(
						t.Context(), devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								return &http.Response{
									StatusCode: http.StatusOK,
									Header: http.Header{
										"Content-Type": {"application/connect+proto"},
									}, Body: body,
									Request: request,
								}, nil
							},
						),
					)
					credential := &auth.Auth{
						ID: t.Name(), Provider: "devin", Attributes: map[string]string{
							"api_key": "synthetic-session", "base_url": "https://devin.synthetic.invalid",
						},
					}
					request := core.Request{
						Model: "devin/" +
							"swe-2", Payload: []byte(`{"input":[{"type":"user_input","content":` +
							`"synthetic"}]}`),
					}
					opts := core.Options{
						SourceFormat: translator.FormatInteractions, ResponseFormat: translator.FormatInteractions,
					}
					execute := NewDevinExecutor(&config.Config{})
					var err error
					if streaming {
						var stream *core.StreamResult
						stream, err = execute.ExecuteStream(ctx, credential, request, opts)
						if err == nil {
							for chunk := range stream.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						_, err = execute.Execute(ctx, credential, request, opts)
					}
					failed := outcome != "completed"
					if (err != nil) != failed {
						t.Fatalf("outcome=%s error=%v", outcome, err)
					}
					if outcome == "unavailable" &&
						testExecutorStatusCode(err) != http.StatusServiceUnavailable {
						t.Fatalf("error status=%v", err)
					}
					record := testWaitForDevinUsageRecord(t, plugin.records)
					if record.Failed != failed || record.ResponseModel != "devin/swe-3" ||
						record.Detail.InputTokens != 9 ||
						record.Detail.OutputTokens != 3 ||
						record.Detail.CachedTokens != 2 ||
						record.Detail.CacheCreationTokens != 11 ||
						record.Detail.TotalTokens != 12 {
						t.Fatalf("record=%+v", record)
					}
					select {
					case <-body.closed:
					default:
						t.Fatal("Connect body remains open")
					}
				},
			)
		}
	}
}

func TestAuditQwenDecodesDeclaredContentEncoding(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, encoding := range []string{"identity", "gzip", "deflate", "bad-gzip", "gzip-error"} {
			t.Run(
				fmt.Sprintf("stream=%t/%s", streaming, encoding), func(t *testing.T) {
					plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
					registerTestUsagePlugin(t, plugin)
					wire := []byte((`{"id":"synthetic","object":"chat.completion","model":"qwen-returned","` +
						`choices":[{"index":0,"message":{"role":"assistant","content":"synthetic answer"},"` +
						`finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":5,"` +
						`total_tokens":12}}`))
					if streaming {
						wire = []byte(
							"data: {\"id\":\"synthetic\",\"object\":\"chat.completion.chunk\",\"mod" +
								"el\":\"qwen-returned\",\"choices\":[{\"index\":0,\"delta\":{\"content" +
								"\":\"synthetic answer\"},\"finish_reason\":\"stop\"}],\"usage\":{\"pro" +
								"mpt_tokens\":7,\"completion_tokens\":5,\"total_tokens\":12}}\n\ndata: " +
								"[DONE]\n\n",
						)
					}
					if encoding == "gzip-error" {
						wire = []byte(`{"error":{"message":"synthetic upstream failure"}}`)
					}
					var encoded bytes.Buffer
					headerEncoding := encoding
					switch encoding {
					case "gzip", "gzip-error":
						writer := gzip.NewWriter(&encoded)
						if _, err := writer.Write(wire); err != nil {
							t.Fatal(err)
						}
						if err := writer.Close(); err != nil {
							t.Fatal(err)
						}
						headerEncoding = "gzip"
					case "deflate":
						writer := zlib.NewWriter(&encoded)
						if _, err := writer.Write(wire); err != nil {
							t.Fatal(err)
						}
						if err := writer.Close(); err != nil {
							t.Fatal(err)
						}
					case "bad-gzip":
						encoded.WriteString("synthetic invalid gzip")
						headerEncoding = "gzip"
					default:
						encoded.Write(wire)
					}
					server := httptest.NewServer(
						http.HandlerFunc(
							func(w http.ResponseWriter, r *http.Request) {
								if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
									t.Error("Qwen did not advertise gzip")
								}
								w.Header().Set("Content-Encoding", headerEncoding)
								w.Header().Set("Content-Type", "application/json")
								if streaming {
									w.Header().Set("Content-Type", "text/event-stream")
								}
								if encoding == "gzip-error" {
									w.WriteHeader(http.StatusServiceUnavailable)
								}
								_, _ = w.Write(encoded.Bytes())
							},
						),
					)
					defer server.Close()
					transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
					defer transport.CloseIdleConnections()
					closed := make(chan struct{})
					ctx := util.WithRoundTripper(
						t.Context(), devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								response, err := transport.RoundTrip(request)
								if err == nil {
									response.Body = &auditImageBody{Reader: response.Body, closed: closed}
								}
								return response, err
							},
						),
					)
					credential := &auth.Auth{
						ID: t.Name(), Provider: "qwen",
						Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL},
					}
					request := core.Request{
						Model: "qwen3-coder-plus",
						Payload: []byte(
							`{"model":"qwen3-coder-plus","messages":[{"role":"user","content":"synthetic"}]}`,
						),
					}
					opts := core.Options{
						SourceFormat:   translator.FormatOpenAI,
						ResponseFormat: translator.FormatOpenAI,
					}
					execute := NewQwenExecutor(&config.Config{})
					var output bytes.Buffer
					var err error
					if streaming {
						var stream *core.StreamResult
						stream, err = execute.ExecuteStream(ctx, credential, request, opts)
						if err == nil {
							for chunk := range stream.Chunks {
								output.Write(chunk.Payload)
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						var response core.Response
						response, err = execute.Execute(ctx, credential, request, opts)
						output.Write(response.Payload)
					}
					failed := encoding == "bad-gzip" || encoding == "gzip-error"
					if (err != nil) != failed {
						t.Fatalf("error=%v output=%q", err, output.Bytes())
					}
					if encoding == "gzip-error" &&
						(testExecutorStatusCode(err) != http.StatusServiceUnavailable || !strings.Contains(
							err.Error(), "synthetic upstream failure",
						)) {
						t.Fatalf("compressed error=%v", err)
					}
					record := testWaitForDevinUsageRecord(t, plugin.records)
					if record.Failed != failed {
						t.Fatalf("record=%+v", record)
					}
					if !failed && (!strings.Contains(
						output.String(), "synthetic answer",
					) ||
						record.ResponseModel !=
							"qwen-returned" ||
						record.Detail.InputTokens !=
							7 ||
						record.Detail.OutputTokens !=
							5 ||
						record.Detail.TotalTokens !=
							12) {
						t.Fatalf("output=%s record=%+v", output.Bytes(), record)
					}
					select {
					case <-closed:
					default:
						t.Fatal("compressed response body remains open")
					}
				},
			)
		}
	}
}

func TestAuditMetaTerminalReadAndJSONWhitespace(t *testing.T) {
	const response = `{"id":"synthetic","object":"response","status":"completed","model":"meta-returned","` +
		`output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic ` +
		`answer"}]}],"usage":{"input_tokens":7,"output_tokens":5,"total_tokens":12}}`
	const completed = `{"type":"response.completed","response":` + response + `}`
	for _, streaming := range []bool{false, true} {
		//goland:noinspection SpellCheckingInspection
		for _, mode := range []string{
			"sse-eof", "sse-same-read-error", "sse-later-error", "sse-open-pipe", "preterminal-error", "json",
			"json-lf", "json-lflf", "json-crlfcrlf",
		} {
			if streaming && strings.HasPrefix(mode, "json") {
				continue
			}
			t.Run(
				fmt.Sprintf("stream=%t/%s", streaming, mode), func(t *testing.T) {
					plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
					registerTestUsagePlugin(t, plugin)
					wire := "data: " + completed + "\n\n"
					if mode == "preterminal-error" {
						wire = "data: {\"type\":\"response.in_progress\",\"response\":{\"model\":\"met" +
							"a-returned\",\"usage\":{\"input_tokens\":7,\"output_tokens\":5,\"total" +
							"_tokens\":12}}}\n\n"
					}
					//goland:noinspection SpellCheckingInspection
					if strings.HasPrefix(mode, "json") {
						wire = response
						switch mode {
						case "json-lf":
							wire += "\n"
						case "json-lflf":
							wire += "\n\n"
						case "json-crlfcrlf":
							wire += "\r\n\r\n"
						}
					}
					var reader io.Reader = strings.NewReader(wire)
					switch mode {
					case "sse-same-read-error", "preterminal-error":
						reader = &auditImageFinalRead{payload: []byte(wire), err: io.ErrUnexpectedEOF}
					case "sse-later-error":
						reader = io.MultiReader(
							strings.NewReader(wire),
							&auditImageFinalRead{err: io.ErrUnexpectedEOF},
						)
					case "sse-open-pipe":
						pipe, writer := io.Pipe()
						reader = pipe
						t.Cleanup(func() { _ = pipe.Close(); _ = writer.Close() })
						go func() { _, _ = io.WriteString(writer, wire) }()
					}
					body := &auditImageBody{Reader: reader, closed: make(chan struct{})}
					ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
					defer cancel()
					stop := context.AfterFunc(ctx, func() { _ = body.Close() })
					defer stop()
					contentType := "text/event-stream"
					if strings.HasPrefix(mode, "json") {
						contentType = "application/json"
					}
					ctx = util.WithRoundTripper(
						ctx, devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								return &http.Response{
									StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}},
									Body: body, Request: request,
								}, nil
							},
						),
					)
					credential := &auth.Auth{
						ID: t.Name(), Provider: "meta", Attributes: map[string]string{
							"api_key": "synthetic-key", "base_url": "https://meta.synthetic.invalid",
						},
					}
					request := core.Request{
						Model: "meta-model", Payload: []byte(`{"model":"meta-model","input":"synthetic"}`),
					}
					opts := core.Options{
						SourceFormat: translator.FormatCodex, ResponseFormat: translator.FormatOpenAIResponse,
					}
					execute := NewMetaExecutor(&config.Config{})
					var output bytes.Buffer
					var err error
					if streaming {
						var stream *core.StreamResult
						stream, err = execute.ExecuteStream(ctx, credential, request, opts)
						if err == nil {
							for chunk := range stream.Chunks {
								output.Write(chunk.Payload)
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					} else {
						var result core.Response
						result, err = execute.Execute(ctx, credential, request, opts)
						output.Write(result.Payload)
					}
					failed := mode == "preterminal-error"
					if (err != nil) != failed ||
						(!failed && !strings.Contains(output.String(), "synthetic answer")) {
						t.Fatalf("error=%v output=%s", err, output.Bytes())
					}
					if failed && !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("read error=%v", err)
					}
					record := testWaitForDevinUsageRecord(t, plugin.records)
					if record.Failed != failed || record.ResponseModel != "meta-returned" ||
						record.Detail.InputTokens != 7 ||
						record.Detail.OutputTokens != 5 ||
						record.Detail.TotalTokens != 12 {
						t.Fatalf("record=%+v", record)
					}
					select {
					case <-body.closed:
					default:
						t.Fatal("Meta response body remains open")
					}
				},
			)
		}
	}
}

func TestAuditCompatibleImagesReadFailureRetainsBytesAndUsage(t *testing.T) {
	const wire = "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"AA==\",\"model\":\"synthetic-" +
		"image-model\",\"usage\":{\"input_tokens\":7,\"output_tokens\":5,\"total_tokens\":12}}\n\n"
	for _, mode := range []string{"eof", "same-read-error", "later-error"} {
		t.Run(
			mode, func(t *testing.T) {
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
				registerTestUsagePlugin(t, plugin)
				var reader io.Reader = strings.NewReader(wire)
				if mode == "same-read-error" {
					reader = &auditImageFinalRead{payload: []byte(wire), err: io.ErrUnexpectedEOF}
				}
				if mode == "later-error" {
					reader = io.MultiReader(
						strings.NewReader(wire),
						&auditImageFinalRead{err: io.ErrUnexpectedEOF},
					)
				}
				body := &auditImageBody{Reader: reader, closed: make(chan struct{})}
				ctx := util.WithRoundTripper(
					t.Context(), devinTestRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							if request.URL.Path != openAICompatImagesGenerationsPath {
								t.Errorf("image path=%s", request.URL.Path)
							}
							return &http.Response{
								StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
								Body: body, Request: request,
							}, nil
						},
					),
				)
				credential := &auth.Auth{
					ID: t.Name(), Provider: "synthetic-images", Attributes: map[string]string{
						"api_key": "synthetic-key", "base_url": "https://images.synthetic.invalid",
					},
				}
				stream, err := NewOpenAICompatExecutor("synthetic-images", &config.Config{}).ExecuteStream(
					ctx, credential,
					core.Request{
						Model:   "gpt-image-1",
						Payload: []byte(`{"model":"gpt-image-1","prompt":"synthetic"}`),
					},
					core.Options{
						SourceFormat:   translator.FromString(openAICompatImageHandlerType),
						ResponseFormat: translator.FormatOpenAI,
						Metadata:       map[string]any{core.RequestPathMetadataKey: "/v1/images/generations"},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				var streamErr error
				for chunk := range stream.Chunks {
					output.Write(chunk.Payload)
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
				}
				failed := mode != "eof"
				if (streamErr != nil) != failed || output.String() != wire {
					t.Fatalf("error=%v output=%s", streamErr, output.Bytes())
				}
				record := testWaitForDevinUsageRecord(t, plugin.records)
				if record.Failed != failed || record.ResponseModel != "synthetic-image-model" ||
					record.Detail.InputTokens != 7 ||
					record.Detail.OutputTokens != 5 ||
					record.Detail.TotalTokens != 12 {
					t.Fatalf("record=%+v", record)
				}
				select {
				case <-body.closed:
				default:
					t.Fatal("image stream body remains open")
				}
			},
		)
	}
}
