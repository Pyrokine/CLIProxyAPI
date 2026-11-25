package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestAuditPublicStreamsRetainObservedUsageOnReadFailure(t *testing.T) {
	for _, provider := range []string{"gemini", "vertex-key", "vertex-service-account", "kimi", "kimi-ai"} {
		for _, readErr := range []error{io.EOF, io.ErrUnexpectedEOF} {
			t.Run(
				provider+"/"+readErr.Error(), func(t *testing.T) {
					plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
					registerTestUsagePlugin(t, plugin)
					cfg := &config.Config{}
					credential := &auth.Auth{
						ID: t.Name(), Provider: provider,
						Attributes: map[string]string{
							"api_key":  "synthetic-key",
							"base_url": "http://127.0.0.1:1",
						},
					}
					request := core.Request{
						Model: "gemini-2.5-flash",
						Payload: []byte(
							`{"model":"gemini-2.5-flash","contents":[{"role":"user","parts":[{"text":"synthetic"}]}]}`,
						),
					}
					opts := core.Options{
						SourceFormat:   translator.FormatGemini,
						ResponseFormat: translator.FormatGemini,
					}
					wire := "data: {\"modelVersion\":\"synthetic-model\",\"candidates\":[{\"index\"" +
						":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"synthetic\"}" +
						"]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\"" +
						":7,\"candidatesTokenCount\":5,\"totalTokenCount\":12}}\n\n"
					var execute interface {
						ExecuteStream(context.Context, *auth.Auth, core.Request, core.Options) (
							*core.StreamResult,
							error,
						)
					} = NewGeminiExecutor(cfg)
					switch provider {
					case "vertex-key":
						execute = NewGeminiVertexExecutor(cfg)
					case "vertex-service-account":
						execute = NewGeminiVertexExecutor(cfg)
						credential.Attributes = nil
						credential.Metadata = map[string]any{
							"project_id": "synthetic-project", "location": "global",
							"service_account": googleImagenServiceAccount(t),
						}
					case "kimi", "kimi-ai":
						execute = NewKimiExecutor(cfg)
						if provider == "kimi-ai" {
							execute = NewKimiAIExecutor(cfg)
						}
						request = core.Request{
							Model: "kimi-k2.5",
							Payload: []byte(
								`{"model":"kimi-k2.5","messages":[{"role":"user","content":"synthetic"}]}`,
							),
						}
						opts = core.Options{
							SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAI,
						}
						wire = "data: {\"id\":\"synthetic\",\"model\":\"synthetic-model\",\"choices\":" +
							"[{\"index\":0,\"delta\":{\"content\":\"synthetic\"},\"finish_reason\":" +
							"\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":5,\"to" +
							"tal_tokens\":12}}\n\n"
					}
					body := &auditImageBody{
						Reader: &auditImageFinalRead{
							payload: []byte(wire),
							err:     readErr,
						}, closed: make(chan struct{}),
					}
					ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
					defer cancel()
					ctx = util.WithRoundTripper(
						ctx, devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								if request.URL.String() == testVertexTokenURL {
									return &http.Response{
										StatusCode: http.StatusOK,
										Header:     http.Header{"Content-Type": {"application/json"}},
										Body: io.NopCloser(
											strings.NewReader(
												`{"access_token":"synthetic-access","token_type":"Bearer",` +
													`"expires_in":` +
													`3600}`,
											),
										),
										Request: request,
									}, nil
								}
								return &http.Response{
									StatusCode: http.StatusOK,
									Header:     http.Header{"Content-Type": {"text/event-stream"}}, Body: body,
									Request: request,
								}, nil
							},
						),
					)
					stream, err := execute.ExecuteStream(ctx, credential, request, opts)
					if err != nil {
						t.Fatal(err)
					}
					var streamErr error
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							streamErr = chunk.Err
						}
					}
					failed := readErr != io.EOF
					if (streamErr != nil) != failed {
						t.Fatalf("read=%v stream=%v", readErr, streamErr)
					}
					record := testWaitForDevinUsageRecord(t, plugin.records)
					if record.Failed != failed || record.Detail.InputTokens != 7 ||
						record.Detail.OutputTokens != 5 ||
						record.Detail.TotalTokens != 12 ||
						record.ResponseModel != "synthetic-model" {
						t.Fatalf("record=%+v", record)
					}
					select {
					case <-body.closed:
					default:
						t.Fatal("stream body remains open")
					}
				},
			)
		}
	}
}

func TestAuditGeminiCLIPartialUsagePreservesPresence(t *testing.T) {
	for _, prompt := range []string{"", `,"promptTokenCount":0`, `,"promptTokenCount":7`} {
		t.Run(
			fmt.Sprintf("prompt=%s", prompt), func(t *testing.T) {
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
				registerTestUsagePlugin(t, plugin)
				wire := "data: {\"response\":{\"usageMetadata\":{\"promptTokenCount\":7}}}\n\n" + fmt.Sprintf(
					"data: {\"response\":{\"candidates\":[{\"index\":0,\"content\":{\"role"+
						"\":\"model\",\"parts\":[{\"text\":\"synthetic\"}]},\"finishReason\":\""+
						"STOP\"}],\"usageMetadata\":{\"candidatesTokenCount\":5,\"totalTokenCou"+
						"nt\":12%s}}}\n\n",
					prompt,
				)
				ctx := util.WithRoundTripper(
					t.Context(), devinTestRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							return &http.Response{
								StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
								Body: io.NopCloser(strings.NewReader(wire)), Request: request,
							}, nil
						},
					),
				)
				credential := &auth.Auth{
					ID: t.Name(), Provider: "gemini-cli", Metadata: map[string]any{
						"project_id": "synthetic-project", "access_token": "synthetic-access", "token": map[string]any{
							"access_token": "synthetic-access", "token_type": "Bearer",
							"expiry": time.Now().Add(time.Hour).Format(time.RFC3339),
						},
					},
				}
				stream, err := NewGeminiCLIExecutor(&config.Config{}).ExecuteStream(
					ctx, credential, core.Request{
						Model:   "gemini-2.5-flash",
						Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"synthetic"}]}]}`),
					}, core.Options{SourceFormat: translator.FormatGemini, ResponseFormat: translator.FormatGemini},
				)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
				record := testWaitForDevinUsageRecord(t, plugin.records)
				wantInput := int64(7)
				if strings.Contains(prompt, ":0") {
					wantInput = 0
				}
				if record.Failed || record.Detail.InputTokens != wantInput || record.Detail.OutputTokens != 5 ||
					record.Detail.TotalTokens != 12 {
					t.Fatalf("prompt=%s record=%+v", prompt, record)
				}
			},
		)
	}
}

func TestAuditGeminiNativeInteractionsFailureIsVisible(t *testing.T) {
	for _, mode := range []string{"completed", "error-object", "failed-status"} {
		t.Run(
			mode, func(t *testing.T) {
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
				registerTestUsagePlugin(t, plugin)
				event := `{"event_type":"interaction.completed","interaction":{"status":"completed"}}`
				if mode == "error-object" {
					event = `{"event_type":"error","error":{"message":"synthetic failure"}}`
				}
				if mode == "failed-status" {
					event = `{"event_type":"interaction.failed","interaction":{"status":"failed"}}`
				}
				wire := "data: {\"event_type\":\"interaction.delta\",\"usage\":{\"input_tokens" +
					"\":7,\"output_tokens\":5,\"total_tokens\":12}}\n\ndata: " +
					event +
					"\n\nevent: done\ndata: [DONE]\n\n"
				body := &auditImageBody{Reader: strings.NewReader(wire), closed: make(chan struct{})}
				ctx := util.WithRoundTripper(
					t.Context(), devinTestRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							return &http.Response{
								StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
								Body: body, Request: request,
							}, nil
						},
					),
				)
				credential := &auth.Auth{
					ID: t.Name(), Provider: "gemini-interactions",
					Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "http://127.0.0.1:1"},
				}
				stream, err := NewGeminiInteractionsExecutor(&config.Config{}).ExecuteStream(
					ctx, credential, core.Request{
						Model: "gemini-2.5-flash", Payload: []byte(`{"model":"gemini-2.5-flash","input":"synthetic"}`),
					}, core.Options{
						SourceFormat: translator.FormatInteractions, ResponseFormat: translator.FormatInteractions,
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
				failed := mode != "completed"
				if (streamErr != nil) != failed || !strings.Contains(output.String(), event) {
					t.Fatalf("error=%v output=%s", streamErr, output.Bytes())
				}
				record := testWaitForDevinUsageRecord(t, plugin.records)
				if record.Failed != failed || record.Detail.InputTokens != 7 ||
					record.Detail.OutputTokens != 5 ||
					record.Detail.TotalTokens != 12 {
					t.Fatalf("record=%+v", record)
				}
				select {
				case <-body.closed:
				default:
					t.Fatal("native stream body remains open")
				}
			},
		)
	}
}
