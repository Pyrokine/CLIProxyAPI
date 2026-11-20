package executor

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditClaudeExplicitCachePayloadRulesReachFinalTransport(t *testing.T) {
	for _, remove := range []bool{false, true} {
		for _, operation := range []string{"messages", "stream", "count"} {
			t.Run(
				fmt.Sprintf("remove=%t/%s", remove, operation), func(t *testing.T) {
					cfg := &config.Config{
						ClaudeKey: []config.ClaudeKey{
							{
								APIKey: "synthetic-cache-key", BaseURL: "https://api.anthropic.com",
								Cloak: &config.CloakConfig{Mode: "never"},
							},
						},
					}
					models := []config.PayloadModelRule{{Name: "claude-opus-5", Protocol: "claude"}}
					if remove {
						cfg.Payload.Filter = []config.PayloadFilterRule{
							{
								Models: models, Params: []string{"system.0.cache_control"},
							},
						}
					} else {
						cfg.Payload.Override = []config.PayloadRule{
							{
								Models: models, Params: map[string]any{
									"system.0.cache_control": map[string]any{
										"type": "ephemeral", "ttl": "5m",
									},
								},
							},
						}
					}
					calls := 0
					ctx := util.WithRoundTripper(
						t.Context(), claudeContractTransport(
							func(request *http.Request) (*http.Response, error) {
								calls++
								body, err := io.ReadAll(request.Body)
								if err != nil {
									return nil, err
								}
								marker := gjson.GetBytes(body, "system.0.cache_control")
								if remove && countCacheControls(body) != 0 {
									t.Errorf("filtered caller cache control was recreated: %s", body)
								}
								if !remove &&
									(countCacheControls(body) !=
										1 ||
										marker.Get("ttl").String() !=
											"5m" ||
										marker.Get("type").String() !=
											"ephemeral") {
									t.Errorf("final payload rule lost cache authority: %s", body)
								}
								if gjson.GetBytes(body, "prompt_cache_options").Exists() {
									t.Errorf("proxy cache mode reached upstream: %s", body)
								}
								return claudeContractResponse(request, body), nil
							},
						),
					)
					credential := &auth.Auth{
						Provider: "claude", Attributes: map[string]string{
							"api_key": "synthetic-cache-key", "base_url": "https://api.anthropic.com",
						},
					}
					request := core.Request{
						Model: "claude-opus-5",
						Payload: []byte(
							(`{"model":"claude-opus-5","max_tokens":64,"prompt_cache_options":{"mode` +
								`":"explicit"},"system":[{"type":"text","text":"caller guidance","cache` +
								`_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user"` +
								`,"content":"synthetic request"}]}`),
						),
					}
					executeClaudeContractRequest(
						t, NewClaudeExecutor(cfg), ctx, credential, request,
						core.Options{SourceFormat: translator.FormatClaude}, operation,
					)
					if calls != 1 {
						t.Fatalf("upstream calls=%d", calls)
					}
				},
			)
		}
	}
}

func TestAuditResponsesZeroReasoningDetailsAtExecutorOutput(t *testing.T) {
	assertUsage := func(t *testing.T, payload []byte, path string) {
		t.Helper()
		usage := gjson.GetBytes(payload, path)
		details := usage.Get("output_tokens_details")
		reasoning := details.Get("reasoning_tokens")
		cached := usage.Get("input_tokens_details.cached_tokens")
		if !details.IsObject() || !reasoning.Exists() || reasoning.Int() != 0 || !cached.Exists() ||
			cached.Int() != 0 ||
			usage.Get("input_tokens").Int() != 5 ||
			usage.Get("output_tokens").Int() != 2 {
			t.Fatalf("zero reasoning schema or tokens changed: %s", payload)
		}
	}
	assertTerminal := func(t *testing.T, frames []byte) {
		t.Helper()
		found := false
		for line := range bytes.SplitSeq(frames, []byte("\n")) {
			line = bytes.TrimPrefix(line, []byte("data: "))
			if gjson.GetBytes(line, "type").String() == "response.completed" {
				assertUsage(t, line, "response.usage")
				found = true
			}
		}
		if !found {
			t.Fatalf("completed response missing: %s", frames)
		}
	}
	for _, details := range []string{"", `,"completion_tokens_details":{"reasoning_tokens":0}`} {
		for _, streaming := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("explicit=%t/stream=%t", details != "", streaming), func(t *testing.T) {
					usage := `"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7` + details + `}`
					wire := (`{"id":"synthetic-zero","model":"qwen3-coder-plus","choices":[{"index":` +
						`0,"message":{"role":"assistant","content":"synthetic"},"finish_reason"` +
						`:"stop"}],`) + usage + `}`
					if streaming {
						wire = "data: " + (`{"id":"synthetic-zero","model":"qwen3-coder-plus","choices":[{"index":` +
							`0,"delta":{"content":"synthetic"},"finish_reason":"stop"}],`) +
							usage +
							`}` +
							("\n\ndata: " +
								"[DONE]\n\n")
					}
					calls := 0
					ctx := util.WithRoundTripper(
						t.Context(), devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								calls++
								return &http.Response{
									StatusCode: http.StatusOK, Header: make(http.Header),
									Body: io.NopCloser(strings.NewReader(wire)), Request: request,
								}, nil
							},
						),
					)
					credential := &auth.Auth{
						ID: t.Name(), Provider: "qwen",
						Metadata: map[string]any{
							"access_token": "synthetic-access",
							"resource_url": "portal.qwen.ai",
						},
					}
					request := core.Request{
						Model:   "qwen3-coder-plus",
						Payload: []byte(`{"messages":[{"role":"user","content":"synthetic"}]}`),
					}
					opts := core.Options{
						SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAIResponse,
					}
					execute := NewQwenExecutor(&config.Config{})
					if streaming {
						result, err := execute.ExecuteStream(ctx, credential, request, opts)
						if err != nil {
							t.Fatal(err)
						}
						var frames []byte
						for chunk := range result.Chunks {
							if chunk.Acknowledge != nil {
								chunk.Acknowledge(chunk.Err)
							}
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							frames = append(frames, chunk.Payload...)
						}
						assertTerminal(t, frames)
					} else {
						result, err := execute.Execute(ctx, credential, request, opts)
						if err != nil {
							t.Fatal(err)
						}
						assertUsage(t, result.Payload, "usage")
					}
					if calls != 1 {
						t.Fatalf("upstream calls=%d", calls)
					}
				},
			)
		}
	}
	t.Run(
		"compaction-json", func(t *testing.T) {
			assertUsage(
				t,
				helps.BuildAntigravityCompactionResponse("synthetic-model", "synthetic-capsule", 5, 2, 7),
				"usage",
			)
		},
	)
	t.Run(
		"compaction-sse", func(t *testing.T) {
			assertTerminal(
				t, bytes.Join(
					helps.BuildAntigravityCompactionStreamChunks(
						"synthetic-model", "synthetic-capsule", 5, 2, 7,
					), nil,
				),
			)
		},
	)
}
