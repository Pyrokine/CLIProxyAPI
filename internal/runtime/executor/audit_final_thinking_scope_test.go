package executor

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestAuditClaudeAndKimiReplayUsesFinalPayloadModel(t *testing.T) {
	for _, provider := range []string{"claude", "kimi"} {
		for _, stream := range []bool{false, true} {
			for _, sameFinalModel := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("%s/stream=%t/same-final=%t", provider, stream, sameFinalModel), func(t *testing.T) {
						internalcache.ClearClaudeThinkingReplayCache()
						internalcache.ClearKimiThinkingReplayCache()
						t.Cleanup(internalcache.ClearClaudeThinkingReplayCache)
						t.Cleanup(internalcache.ClearKimiThinkingReplayCache)
						bodies := make([][]byte, 0, 2)
						ctx := util.WithRoundTripper(
							claudeReplayLocalContext(t.Context()), devinTestRoundTripper(
								func(request *http.Request) (*http.Response, error) {
									body, err := io.ReadAll(request.Body)
									if err != nil {
										return nil, err
									}
									bodies = append(bodies, body)
									contentType, response := "application/json", claudeReplaySignedResponse(
										"synthetic-first", "a", "tool-one",
									)
									if len(bodies) == 2 {
										response = claudeReplayTextResponse("synthetic-second")
									}
									if stream {
										contentType, response = "text/event-stream", claudeReplayThinkingStream(
											"synthetic-first", "a", "tool-one",
										)
										if len(bodies) == 2 {
											response = claudeReplayTextStream("synthetic-second")
										}
									}
									return &http.Response{
										StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}},
										Body: io.NopCloser(strings.NewReader(response)), Request: request,
									}, nil
								},
							),
						)
						initial, firstFinal, secondFinal := "claude-synthetic-4772", "claude-model-b", "claude-model-c"
						if provider == "kimi" {
							initial, firstFinal, secondFinal = "k3", "k3", "k2.6"
						}
						if sameFinalModel {
							secondFinal = firstFinal
						}
						credential := claudeReplayTestAuth("http://127.0.0.1:1")
						credential.Provider = provider
						credential.Metadata = map[string]any{"access_token": "synthetic-token"}
						firstPayload := []byte(`{"max_tokens":32,"messages":[{"role":"user","content":"inspect"}]}`)
						secondPayload := claudeReplayOmittedPayload(struct{ toolID, path string }{"tool-one", "a"})
						for turn, final := range []string{firstFinal, secondFinal} {
							cfg := &config.Config{
								Payload: config.PayloadConfig{
									Override: []config.PayloadRule{
										{
											Models: []config.PayloadModelRule{
												{
													Name: initial, Protocol: "claude",
												},
											}, Params: map[string]any{"model": final},
										},
									},
								},
							}
							var executor auth.ProviderExecutor = NewClaudeExecutor(cfg)
							if provider == "kimi" {
								executor = NewKimiExecutor(cfg)
							}
							payload := firstPayload
							if turn == 1 {
								payload = secondPayload
								if provider == "kimi" {
									internalcache.ClearClaudeThinkingReplayCache()
								}
							}
							req, opts := claudeReplayTestRequest(payload, t.Name(), true)
							req.Model = initial
							var err error
							if stream {
								var result *core.StreamResult
								result, err = executor.ExecuteStream(ctx, credential, req, opts)
								if err == nil && drainClaudeReplayStream(t, result) {
									t.Fatal("stream failed")
								}
							} else {
								_, err = executor.Execute(ctx, credential, req, opts)
							}
							if err != nil {
								t.Fatal(err)
							}
						}
						if len(bodies) != 2 || gjson.GetBytes(
							bodies[0], "model",
						).String() != firstFinal || gjson.GetBytes(bodies[1], "model").String() != secondFinal {
							t.Fatalf("final wire models changed: %q", bodies)
						}
						restored := gjson.GetBytes(bodies[1], "messages.1.content.0.signature").String() == "sig-a"
						if restored != sameFinalModel {
							t.Fatalf("same final=%t restored=%t body=%s", sameFinalModel, restored, bodies[1])
						}
					},
				)
			}
		}
	}
}
