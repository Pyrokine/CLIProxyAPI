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
	"github.com/tidwall/gjson"
)

func auditFinalUsageCall(
	ctx context.Context,
	execute auth.ProviderExecutor,
	credential *auth.Auth,
	request core.Request,
	opts core.Options,
	streaming bool,
) error {
	if !streaming {
		_, err := execute.Execute(ctx, credential, request, opts)
		return err
	}
	result, err := execute.ExecuteStream(ctx, credential, request, opts)
	if err != nil {
		return err
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			err = chunk.Err
		}
	}
	return err
}

func TestAuditQwenOAuthResourceURLReachesTransport(t *testing.T) {
	for _, resource := range []string{
		"portal.qwen.ai", "https://portal.qwen.ai", "https://portal.qwen.ai/v1", "https://portal.qwen.ai/v1/",
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/stream=%t", resource, streaming), func(t *testing.T) {
					calls := 0
					wire := `{"id":"synthetic","model":"qwen3-coder-plus","choices":[{"index":0,"message":{"` +
						`role":"assistant","content":"synthetic"},"finish_reason":"stop"}]}`
					if streaming {
						wire = "data: {\"id\":\"synthetic\",\"model\":\"qwen3-coder-plus\",\"choices\"" +
							":[{\"index\":0,\"delta\":{\"content\":\"synthetic\"},\"finish_reason\"" +
							":\"stop\"}]}\n\ndata: [DONE]\n\n"
					}
					ctx := util.WithRoundTripper(
						t.Context(), devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								calls++
								if request.URL.Scheme != "https" || request.URL.Host != "portal.qwen.ai" ||
									request.URL.Path != "/v1/chat/completions" ||
									request.Header.Get("Authorization") != "Bearer synthetic-access" {
									t.Errorf(
										"upstream URL=%s authorization=%q", request.URL,
										request.Header.Get("Authorization"),
									)
								}
								return &http.Response{
									StatusCode: http.StatusOK, Header: make(http.Header),
									Body: io.NopCloser(strings.NewReader(wire)), Request: request,
								}, nil
							},
						),
					)
					credential := &auth.Auth{
						ID: t.Name(), Provider: "qwen",
						Metadata: map[string]any{"access_token": "synthetic-access", "resource_url": resource},
					}
					err := auditFinalUsageCall(
						ctx, NewQwenExecutor(&config.Config{}), credential, core.Request{
							Model:   "qwen3-coder-plus",
							Payload: []byte(`{"messages":[{"role":"user","content":"synthetic"}]}`),
						}, core.Options{SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAI},
						streaming,
					)
					if err != nil || calls != 1 {
						t.Fatalf("error=%v calls=%d", err, calls)
					}
				},
			)
		}
	}
}

func TestAuditGeminiCLIEnvelopeUsageReachesPublicRecords(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		for _, field := range []string{"usageMetadata", "usage_metadata"} {
			for _, streaming := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("wrapped=%t/%s/stream=%t", wrapped, field, streaming), func(t *testing.T) {
						plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
						registerTestUsagePlugin(t, plugin)
						body := fmt.Sprintf(
							`{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"synthetic"`+
								`}]},"finishReason":"STOP"}],"%s":{"promptTokenCount":11,"`+
								`candidatesTokenCount":7,"thoughtsTokenCount":3,"totalTokenCount":21,"`+
								`cachedContentTokenCount":5}}`,
							field,
						)
						if wrapped {
							body = `{"response":` + body + `}`
						}
						wire := body
						if streaming {
							wire = "data: " + body + "\n\n"
						}
						ctx := util.WithRoundTripper(
							t.Context(), devinTestRoundTripper(
								func(request *http.Request) (*http.Response, error) {
									return &http.Response{
										StatusCode: http.StatusOK, Header: make(http.Header),
										Body: io.NopCloser(strings.NewReader(wire)), Request: request,
									}, nil
								},
							),
						)
						credential := &auth.Auth{
							ID: t.Name(), Provider: "gemini-cli", Metadata: map[string]any{
								"project_id": "synthetic-project", "access_token": "synthetic-access",
								"token": map[string]any{
									"access_token": "synthetic-access", "token_type": "Bearer",
									"expiry": time.Now().Add(time.Hour).Format(time.RFC3339),
								},
							},
						}
						err := auditFinalUsageCall(
							ctx, NewGeminiCLIExecutor(&config.Config{}), credential, core.Request{
								Model: "gemini-2.5-flash",
								Payload: []byte(
									`{"contents":[{"role":"user","parts":[{"text":"synthetic"}]}]}`,
								),
							}, core.Options{
								SourceFormat: translator.FormatGemini, ResponseFormat: translator.FormatOpenAIResponse,
							}, streaming,
						)
						if err != nil {
							t.Fatal(err)
						}
						r := testWaitForDevinUsageRecord(t, plugin.records)
						if r.Failed || r.Detail.InputTokens != 11 || r.Detail.OutputTokens != 7 ||
							r.Detail.ReasoningTokens != 3 ||
							r.Detail.TotalTokens != 21 ||
							r.Detail.CachedTokens != 5 {
							t.Fatalf("record=%+v", r)
						}
						traffic := `{"` + field + `":{"trafficType":"ON_DEMAND"}}`
						if wrapped {
							traffic = `{"response":` + traffic + `}`
						}
						if detail := parseGeminiCLIUsage([]byte(traffic)); detail != (usage.Detail{}) {
							t.Fatalf("traffic-only detail=%+v", detail)
						}
						if detail, ok := parseGeminiCLIStreamUsage([]byte("data: " + traffic)); ok ||
							detail != (usage.Detail{}) {
							t.Fatalf("traffic-only stream detail=%+v valid=%t", detail, ok)
						}
					},
				)
			}
		}
	}
}

func TestAuditResponsesTierOnlyKeepsObservedTokens(t *testing.T) {
	for _, provider := range []string{"meta", "xai"} {
		for _, streaming := range []bool{false, true} {
			for _, failed := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("%s/stream=%t/failed=%t", provider, streaming, failed), func(t *testing.T) {
						plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
						registerTestUsagePlugin(t, plugin)
						wire := "data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"synthetic\"," +
							"\"model\":\"grok-4.6\",\"usage\":{\"input_tokens\":7,\"output_tokens\":5,\"" +
							"total_tokens\":12}}}\n\ndata: {\"type\":\"response.in_progress\",\"response\":{" +
							"\"id\":\"synthetic\",\"service_tier\":\"default\"}}\n\n"
						terminal := `{"type":"response.completed","response":{"id":"synthetic","object":"` +
							`response","status":"completed","model":"grok-4.6","output":[]}}`
						if failed {
							terminal = `{"type":"response.failed","response":{"id":"synthetic","error":` +
								`{"type":"server_error","message":"synthetic ` +
								`failure"}}}`
						}
						wire += "data: " + terminal + "\n\n"
						ctx := util.WithRoundTripper(
							t.Context(), devinTestRoundTripper(
								func(request *http.Request) (*http.Response, error) {
									return &http.Response{
										StatusCode: http.StatusOK,
										Header:     http.Header{"Content-Type": {"text/event-stream"}},
										Body:       io.NopCloser(strings.NewReader(wire)), Request: request,
									}, nil
								},
							),
						)
						credential := &auth.Auth{
							ID: t.Name(), Provider: provider, Attributes: map[string]string{
								"api_key": "synthetic-key", "base_url": "https://upstream.synthetic.invalid",
							},
						}
						var execute auth.ProviderExecutor = NewMetaExecutor(&config.Config{})
						if provider == "xai" {
							execute = &XAIExecutor{cfg: &config.Config{}}
						}
						err := auditFinalUsageCall(
							ctx, execute, credential, core.Request{
								Model: "grok-4.6", Payload: []byte(`{"model":"grok-4.6","input":"synthetic"}`),
							}, core.Options{
								SourceFormat: translator.FormatCodex, ResponseFormat: translator.FormatOpenAIResponse,
							}, streaming,
						)
						if (err != nil) != failed {
							t.Fatalf("failed=%t error=%v", failed, err)
						}
						r := testWaitForDevinUsageRecord(t, plugin.records)
						if r.Failed != failed || r.Detail.InputTokens != 7 || r.Detail.OutputTokens != 5 ||
							r.Detail.TotalTokens != 12 ||
							r.Detail.ResponseServiceTier != "default" {
							t.Fatalf("record=%+v", r)
						}
					},
				)
			}
		}
	}
}

func TestAuditXAIImageSupportUsesFinalPayloadModel(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, final := range []string{"grok-4.5", "grok-4.6"} {
			t.Run(
				fmt.Sprintf("stream=%t/model=%s", streaming, final), func(t *testing.T) {
					calls := 0
					cfg := &config.Config{}
					cfg.Payload.Override = []config.PayloadRule{
						{
							Models: []config.PayloadModelRule{{Name: "grok-4.5"}},
							Params: map[string]any{"model": final},
						},
					}
					ctx := util.WithRoundTripper(
						t.Context(), devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								calls++
								body, err := io.ReadAll(request.Body)
								if err != nil || gjson.GetBytes(
									body, "model",
								).String() != final || !bytes.Contains(body, []byte(`"type":"image_generation"`)) {
									t.Errorf("body=%s error=%v", body, err)
								}
								wire := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"" +
									"synthetic\",\"object\":\"response\",\"status\":\"completed\",\"model\":" +
									"\"grok-4.6\",\"output\":[]}}\n\n"
								return &http.Response{
									StatusCode: http.StatusOK,
									Header:     http.Header{"Content-Type": {"text/event-stream"}},
									Body:       io.NopCloser(strings.NewReader(wire)), Request: request,
								}, nil
							},
						),
					)
					credential := &auth.Auth{
						ID: t.Name(), Provider: "xai", Attributes: map[string]string{
							"api_key": "synthetic-key", "base_url": "https://xai.synthetic.invalid",
						},
					}
					err := auditFinalUsageCall(
						ctx, &XAIExecutor{cfg: cfg}, credential, core.Request{
							Model: "grok-4.5",
							Payload: []byte(
								`{"model":"grok-4.5","input":"synthetic","tools":[{"type":"image_generation"}]}`,
							),
						}, core.Options{
							SourceFormat: translator.FormatCodex, ResponseFormat: translator.FormatOpenAIResponse,
						}, streaming,
					)
					if final == "grok-4.6" {
						if err != nil || calls != 1 {
							t.Fatalf("error=%v calls=%d", err, calls)
						}
					} else if testExecutorStatusCode(err) != http.StatusBadRequest || calls != 0 {
						t.Fatalf("unsupported final model error=%v calls=%d", err, calls)
					}
				},
			)
		}
	}
}

func TestAuditCodexContextErrorKeepsStructuredCode(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			for _, explicitStatus := range []int{0, http.StatusTooManyRequests} {
				t.Run(
					fmt.Sprintf("nested=%t/stream=%t/status=%d", nested, streaming, explicitStatus),
					func(t *testing.T) {
						payload := `{"type":"error","code":"context_length_exceeded","message":"Your ` +
							`input exceeds the context window","param":` +
							`"input"}`
						if nested {
							payload = `{"type":"response.failed","response":{"error":{"code":"` +
								`context_length_exceeded","message":"Your input exceeds the context window","` +
								`param":"input"}}}`
						}
						if explicitStatus != 0 {
							payload = fmt.Sprintf(`{"status":%d,`, explicitStatus) + payload[1:]
						}
						wire := "data: " + payload + "\n\n"
						ctx := util.WithRoundTripper(
							t.Context(), devinTestRoundTripper(
								func(request *http.Request) (*http.Response, error) {
									return &http.Response{
										StatusCode: http.StatusOK,
										Header:     http.Header{"Content-Type": {"text/event-stream"}},
										Body:       io.NopCloser(strings.NewReader(wire)), Request: request,
									}, nil
								},
							),
						)
						credential := &auth.Auth{
							ID: t.Name(), Provider: "codex", Attributes: map[string]string{
								"api_key": "synthetic-key", "base_url": "https://codex.synthetic.invalid",
							},
						}
						err := auditFinalUsageCall(
							ctx,
							&codexExecutor{cfg: &config.Config{}},
							credential,
							core.Request{
								Model:   "gpt-5.4",
								Payload: []byte(`{"model":"gpt-5.4","input":"synthetic"}`),
							},
							core.Options{
								SourceFormat: translator.FormatCodex, ResponseFormat: translator.FormatOpenAIResponse,
							},
							streaming,
						)
						want := http.StatusBadRequest
						if explicitStatus != 0 {
							want = explicitStatus
						}
						if testExecutorStatusCode(err) != want || !strings.Contains(
							err.Error(), `"code":"context_length_exceeded"`,
						) || !strings.Contains(err.Error(), `"param":"input"`) {
							t.Fatalf("error=%v want status=%d and context identity", err, want)
						}
					},
				)
			}
		}
	}
}
