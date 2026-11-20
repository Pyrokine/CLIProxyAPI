package executor

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditContext1MFinalModelReachesEveryJSONConsumer(t *testing.T) {
	for _, tc := range []struct {
		provider string
		model    string
		format   translator.Format
		wire     string
		build    func(*config.Config) auth.ProviderExecutor
	}{
		{
			"codex", "gpt-5.4", translator.FormatCodex, "responses",
			func(cfg *config.Config) auth.ProviderExecutor { return newCodexExecutor(cfg) },
		},
		{
			"xai", "grok-4.6", translator.FormatCodex, "responses",
			func(cfg *config.Config) auth.ProviderExecutor { return newXAIExecutor(cfg) },
		},
		{
			"claude", "claude-sonnet-4-5", translator.FormatClaude, "claude",
			func(cfg *config.Config) auth.ProviderExecutor { return NewClaudeExecutor(cfg) },
		},
		{
			"qwen", "qwen3-coder-plus", translator.FormatOpenAI, "chat",
			func(cfg *config.Config) auth.ProviderExecutor { return NewQwenExecutor(cfg) },
		},
		{
			"kimi", "kimi-k2.5", translator.FormatOpenAI, "chat",
			func(cfg *config.Config) auth.ProviderExecutor { return NewKimiExecutor(cfg) },
		},
		{
			"kimi", "kimi-k2.5", translator.FormatOpenAIResponse, "responses",
			func(cfg *config.Config) auth.ProviderExecutor { return NewKimiExecutor(cfg) },
		},
		{
			"iflow", "qwen3-coder-plus", translator.FormatOpenAI, "chat",
			func(cfg *config.Config) auth.ProviderExecutor { return NewIFlowExecutor(cfg) },
		},
		{
			"gemini-interactions", "gemini-2.5-pro", translator.FormatInteractions, "interactions",
			func(cfg *config.Config) auth.ProviderExecutor { return NewGeminiInteractionsExecutor(cfg) },
		},
		{
			"vertex", "gemini-2.5-pro", translator.FormatInteractions, "interactions",
			func(cfg *config.Config) auth.ProviderExecutor { return NewGeminiVertexExecutor(cfg) },
		},
	} {
		for _, target := range []string{"same", "small", "other-capable"} {
			for _, operation := range []string{"execute", "stream", "count"} {
				if tc.wire == "interactions" && operation == "count" {
					continue
				}
				t.Run(
					tc.provider+"/"+tc.wire+"/"+target+"/"+operation, func(t *testing.T) {
						cfg := &config.Config{}
						final := tc.model
						if target != "same" {
							final += "-" + target
						}
						cfg.Payload.Override = []config.PayloadRule{
							{
								Models: []config.PayloadModelRule{{Name: tc.model}},
								Params: map[string]any{"model": final},
							},
						}
						manager := auth.NewManager(nil, nil, nil)
						manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
						manager.SetConfig(cfg)
						manager.RegisterExecutor(tc.build(cfg))
						credential, err := manager.Register(
							t.Context(), &auth.Auth{
								ID: t.Name(), Provider: tc.provider, Attributes: map[string]string{
									"api_key": "synthetic-key", "base_url": "https://context.synthetic.invalid",
									"auth_kind": "apikey", "interactions": "true",
								},
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						reg := registry.GetGlobalRegistry()
						reg.RegisterClient(
							credential.ID, tc.provider, []*registry.ModelInfo{
								{ID: tc.model, ContextLength: 1_000_000},
								{ID: tc.model + "-small", ContextLength: 128_000},
								{ID: tc.model + "-other-capable", MaxContextLength: 1_000_000},
							},
						)
						t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
						manager.RefreshSchedulerEntry(credential.ID)
						calls := 0
						ctx := util.WithRoundTripper(
							t.Context(), devinTestRoundTripper(
								func(request *http.Request) (*http.Response, error) {
									calls++
									body, readErr := io.ReadAll(request.Body)
									if readErr != nil || gjson.GetBytes(body, "model").String() != final {
										t.Errorf("body=%s error=%v", body, readErr)
									}
									wire := `{"id":"synthetic","model":"` +
										final +
										(`","choices":[{"index":0,` +
											`"message":{"role":` +
											`"assistant","content":` +
											`"synthetic"},` +
											`"finish_reason":` +
											`"stop"}]}`)
									header := make(http.Header)
									if operation == "count" {
										wire = `{"input_tokens":3}`
									} else if tc.wire == "responses" {
										wire = "data: {\"type\":\"response.completed\",\"response\":{\"id\":" +
											"\"synthetic\",\"status\":\"completed\",\"model\":" +
											"\"" +
											final +
											"\",\"output\":[]}}\n\n"
										header.Set("Content-Type", "text/event-stream")
									} else if tc.wire == "claude" {
										wire = `{"id":"synthetic","type":"message","role":"assistant","model":"` +
											final +
											(`","content":[{"type":"text","text":"synthetic"}],"stop_reason":"end_tu` +
												`rn","usage":{"input_tokens":1,"output_tokens":1}}`)
										if operation == "stream" {
											wire = ("event: message_start\ndata: {\"type\":\"message_start\"," +
												"\"message\":" +
												"{") +
												("\"id\":\"synthetic\",\"type\":\"message\",\"role\":" +
													"\"assistant\"," +
													"\"mod") +
												("el\":" +
													"\"") +
												final +
												("\",\"content\":[]}}\n\nevent: " +
													"message_stop\ndata: {\"type\":" +
													"\"message_stop\"}\n\n")
										}
									} else if tc.wire == "interactions" {
										wire = `{"interaction":{"id":"synthetic","status":"completed","steps":[]}}`
										if operation == "stream" {
											wire = ("data: {\"event_type\":\"interaction.completed\"," +
												"\"interaction\":" +
												"{\"id") +
												"\":\"synthetic\",\"status\":\"completed\"}}\n\ndata: [DONE]\n\n"
										}
									} else if operation == "stream" {
										wire = "data: {\"id\":\"synthetic\",\"model\":\"" +
											final +
											("\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" +
												"synthetic\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n" +
												"\n")
									}
									return &http.Response{
										StatusCode: http.StatusOK, Header: header,
										Body: io.NopCloser(strings.NewReader(wire)), Request: request,
									}, nil
								},
							),
						)
						model := tc.model + "[1m]"
						payload := `{"model":"` +
							model +
							`","messages":[{"role":"user","content":"synthetic"}],"max_tokens":16}`
						if tc.wire == "responses" || tc.wire == "interactions" {
							payload = `{"model":"` + model + `","input":"synthetic"}`
						}
						request := core.Request{Model: model, Payload: []byte(payload)}
						opts := core.Options{SourceFormat: tc.format, ResponseFormat: tc.format}
						switch operation {
						case "execute":
							_, err = manager.Execute(ctx, []string{tc.provider}, request, opts)
						case "count":
							_, err = manager.ExecuteCount(ctx, []string{tc.provider}, request, opts)
						case "stream":
							var result *core.StreamResult
							result, err = manager.ExecuteStream(ctx, []string{tc.provider}, request, opts)
							if err == nil {
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										err = chunk.Err
									}
								}
							}
						}
						if target == "small" {
							status, ok := err.(interface{ StatusCode() int })
							if !ok || status.StatusCode() != http.StatusBadRequest || calls != 0 {
								t.Fatalf("unsupported target error=%v calls=%d", err, calls)
							}
						} else if err != nil || (operation != "count" && calls != 1) {
							t.Fatalf("valid target error=%v calls=%d", err, calls)
						}
					},
				)
			}
		}
	}
}
