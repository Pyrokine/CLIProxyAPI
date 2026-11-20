package executor

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestKimiClaudeUsageKeepsProviderAndObservedTokens(t *testing.T) {
	for _, provider := range []string{"", "kimi", "kimi-ai"} {
		for _, mode := range []struct {
			name   string
			stream bool
			failed bool
		}{
			{name: "message"},
			{name: "stream", stream: true},
			{name: "stream read failure", stream: true, failed: true},
		} {
			t.Run(
				fmt.Sprintf("provider=%s/%s", provider, mode.name), func(t *testing.T) {
					ctx, finish := xaiOutcomeContext(t)
					ctx = util.WithRoundTripper(
						ctx, kimiTokenRoundTripper(
							func(req *http.Request) (*http.Response, error) {
								response := `{"id":"msg_test","type":"message","role":"assistant","model":"k3","con` +
									`tent":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage"` +
									`:{"input_tokens":7,"output_tokens":5}}`
								contentType := "application/json"
								if mode.stream {
									contentType = "text/event-stream"
									response = ("event: message_start\ndata: {\"type\":\"message_start\"," +
										"\"message\":" +
										"{") +
										"\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"mode" +
										"l\":\"k3\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_toke" +
										"ns\":7,\"output_tokens\":0}}}\n\n" +
										("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"s" +
											"top_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n") +
										"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
								}
								var body io.Reader = strings.NewReader(response)
								if mode.failed {
									response = strings.TrimSuffix(
										response, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
									)
									body = &terminalErrorReader{
										data: []byte(response),
										err:  io.ErrUnexpectedEOF,
									}
								}
								return &http.Response{
									StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}},
									Body: io.NopCloser(body), Request: req,
								}, nil
							},
						),
					)
					exec := NewKimiExecutor(&config.Config{})
					if provider == "kimi-ai" {
						exec = NewKimiAIExecutor(&config.Config{})
					}
					payload := []byte(
						`{"model":"kimi-k3","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`,
					)
					req := cliproxyexecutor.Request{Model: "kimi-k3", Payload: payload}
					opts := cliproxyexecutor.Options{
						SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude,
						OriginalRequest: payload, Stream: mode.stream,
					}
					auth := &cliproxyauth.Auth{
						Provider: provider, Attributes: map[string]string{},
						Metadata: map[string]any{"access_token": "test-token"},
					}
					var resultErr error
					if mode.stream {
						result, err := exec.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								resultErr = chunk.Err
							}
						}
					} else {
						_, resultErr = exec.Execute(ctx, auth, req, opts)
					}
					if mode.failed && !errors.Is(resultErr, io.ErrUnexpectedEOF) ||
						!mode.failed && resultErr != nil {
						t.Fatalf("execution error = %v, want read failure=%t", resultErr, mode.failed)
					}
					records := finish()
					if len(records) != 1 {
						t.Fatalf("usage records = %d, want 1", len(records))
					}
					record := records[0]
					if record.Provider != exec.Identifier() || record.Model != "kimi-k3" ||
						record.ResponseModel != "k3" ||
						record.Failed != mode.failed ||
						record.Detail.InputTokens != 7 ||
						record.Detail.OutputTokens != 5 ||
						record.Detail.TotalTokens != 12 {
						t.Fatalf(
							"delegated Kimi usage = %+v, want provider=%s, logical/served models "+
								"and observed 7/5/12 "+
								"tokens",
							record,
							exec.Identifier(),
						)
					}
				},
			)
		}
	}
}
