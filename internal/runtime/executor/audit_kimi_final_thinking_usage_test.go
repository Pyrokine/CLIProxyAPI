package executor

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditKimiFinalNativeThinkingPublishesUsage(t *testing.T) {
	for _, provider := range []string{"kimi", "kimi-ai"} {
		for _, responses := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, tc := range []struct{ name, native, effort, want string }{
					{"native-precedence", `{"type":"enabled","effort":"medium"}`, "high", "medium"},
					{"disabled", `{"type":"disabled","effort":"high"}`, "high", "none"},
					{"enabled-without-effort", `{"type":"enabled"}`, "high", ""},
					{"max", `{"type":"enabled","effort":"max"}`, "high", "max"},
					{"legacy", "", "high", "high"},
				} {
					t.Run(
						fmt.Sprintf("%s/responses=%t/stream=%t/%s", provider, responses, stream, tc.name),
						func(t *testing.T) {
							plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
							registerTestUsagePlugin(t, plugin)
							alias := t.Name()
							cfg := &config.Config{
								Payload: config.PayloadConfig{
									OverrideRaw: []config.PayloadRule{
										{
											Models: []config.PayloadModelRule{
												{Name: "*"},
											}, Params: map[string]any{
												"reasoning_effort": fmt.Sprintf(
													"%q", tc.effort,
												),
											},
										},
									},
								},
							}
							if tc.native != "" {
								cfg.Payload.OverrideRaw[0].Params["thinking"] = tc.native
							} else {
								cfg.Payload.Filter = []config.PayloadFilterRule{
									{
										Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"thinking"},
									},
								}
							}
							var wire []byte
							calls := 0
							ctx := usage.WithRequestedModelAlias(t.Context(), alias)
							ctx = util.WithRoundTripper(
								ctx, kimiTokenRoundTripper(
									func(req *http.Request) (*http.Response, error) {
										calls++
										var err error
										wire, err = io.ReadAll(req.Body)
										if err != nil {
											return nil, err
										}
										if !strings.HasSuffix(
											req.URL.Path, "/responses",
										) && !strings.HasSuffix(req.URL.Path, "/chat/completions") {
											return nil, fmt.Errorf("unexpected route %s", req.URL.Path)
										}
										body := `{"id":"synthetic-kimi","object":"chat.completion","model":"` +
											`kimi-k3","choices":[{"index":0,"message":{"role":"assistant","` +
											`content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens"` +
											`:3,"completion_tokens":2,"total_tokens":5}}`
										if responses {
											body = `{"id":"synthetic-kimi","object":"response","status":"` +
												`completed","model":"kimi-k3","output":[],"usage":{"` +
												`input_tokens":3,"output_tokens":2,"total_tokens":5}}`
										}
										if stream {
											if responses {
												body = "event: response.completed\ndata: {\"type\":" +
													"\"response.completed\",\"response\":" +
													body +
													"}\n\n"
											} else {
												body = ("data: {\"id\":\"synthetic-kimi\",\"object\":" +
													"\"chat.completion.chunk\",") +
													("\"model\":\"kimi-k3\",\"choices\":[{\"index\":0," +
														"\"delta\":{\"content\"") +
													(":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":" +
														"{\"prompt_tokens\":3,") +
													"\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n"
											}
										}
										return googlePayloadResponse(req, body), nil
									},
								),
							)
							exec := NewKimiExecutor(cfg)
							if provider == "kimi-ai" {
								exec = NewKimiAIExecutor(cfg)
							}
							req := cliproxyexecutor.Request{
								Model: "kimi-k3",
								Payload: []byte(
									`{"model":"kimi-k3","messages":[{"role":"user","content":"hello"}],` +
										`"reasoning_effort":` +
										`"low"}`,
								),
							}
							opts := cliproxyexecutor.Options{
								SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI,
								Stream: stream,
							}
							if responses {
								req.Payload = []byte(
									`{"model":"kimi-k3","input":[{"role":"user","content":"hello"}],` +
										`"reasoning_effort":` +
										`"low"}`,
								)
								opts.SourceFormat = sdktranslator.FormatOpenAIResponse
								opts.ResponseFormat = sdktranslator.FormatOpenAIResponse
							}
							auth := &cliproxyauth.Auth{
								ID: "synthetic-kimi", Provider: provider,
								Metadata: map[string]any{"access_token": "synthetic-token"},
							}
							if stream {
								result, err := exec.ExecuteStream(ctx, auth, req, opts)
								if err != nil {
									t.Fatal(err)
								}
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										t.Fatal(chunk.Err)
									}
								}
							} else if _, err := exec.Execute(ctx, auth, req, opts); err != nil {
								t.Fatal(err)
							}
							if calls != 1 || gjson.GetBytes(wire, "reasoning_effort").String() != tc.effort {
								t.Fatalf("calls=%d final wire=%s", calls, wire)
							}
							if tc.native != "" && gjson.GetBytes(wire, "thinking").Raw != tc.native {
								t.Fatalf("final native thinking changed: %s", wire)
							}
							record := waitForKimiUsageRecord(t, plugin.records, alias)
							if record.Provider != provider || record.ReasoningEffort != tc.want ||
								record.Detail.TotalTokens != 5 ||
								record.Failed {
								t.Fatalf(
									"wire=%s usage=%+v, want provider=%s effort=%q total=5",
									wire,
									record,
									provider,
									tc.want,
								)
							}
						},
					)
				}
			}
		}
	}
}
