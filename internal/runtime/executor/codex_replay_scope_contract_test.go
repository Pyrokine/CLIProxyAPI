package executor

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestCodexOriginalReplayCallerAndCredentialIsolation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, boundary := range []string{
			"same", "refreshed-token", "caller", "metadata-caller", "credential", "credential-whitespace", "service",
			"endpoint",
		} {
			t.Run(
				fmt.Sprintf("stream=%t/%s", stream, boundary), func(t *testing.T) {
					internalcache.ClearCodexReasoningReplayCache()
					t.Cleanup(internalcache.ClearCodexReasoningReplayCache)
					encrypted := validCodexReplayEncryptedContentForTest(7)
					var bodies [][]byte
					transport := devinTestRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							body, err := io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							bodies = append(bodies, body)
							payload := (`data: {"type":"response.completed","response":{"id":"response-` +
								`fixture","status":"completed","output":[{"type":"reasoning","summary":[],"` +
								`encrypted_content":"`) + encrypted + `"}]}}` + "\n\n"
							return &http.Response{
								StatusCode: 200, Request: request,
								Header: http.Header{"Content-Type": {"text/event-stream"}},
								Body:   io.NopCloser(strings.NewReader(payload)),
							}, nil
						},
					)
					executor := newCodexExecutor(&config.Config{})
					for turn := range 2 {
						caller, credential, service := "synthetic-client-a", "synthetic-credential-a", t.Name()
						if turn == 1 {
							switch boundary {
							case "caller", "metadata-caller":
								caller = "synthetic-client-b"
							case "credential":
								credential = "synthetic-credential-b"
							case "credential-whitespace":
								credential = " " + credential + " "
							case "service":
								service += "-other"
							}
						}
						ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
						ginContext.Set("apiKey", caller)
						ctx := util.WithGinContext(t.Context(), ginContext)
						ctx = home.WithClientScope(ctx, home.ClientScope{ID: service, Enabled: false})
						ctx = util.WithRoundTripper(ctx, transport)
						auth := &cliproxyauth.Auth{
							ID: credential, Attributes: map[string]string{
								"api_key": "synthetic-upstream-key", "base_url": "https://replay-fixture.invalid",
							},
						}
						if turn == 1 {
							switch boundary {
							case "refreshed-token":
								auth.Attributes["api_key"] = "synthetic-refreshed-key"
							case "endpoint":
								auth.Attributes["base_url"] = "https://other-replay-fixture.invalid"
							}
						}
						request := core.Request{
							Model: "gpt-5.4", Payload: []byte(fmt.Sprintf(
								`{"metadata":{"user_id":"{\"session_id\":\"shared-session\"}"},`+
									`"messages":[{"role":"user","content":`+
									`"turn-%d"}]}`,
								turn,
							)),
						}
						options := core.Options{SourceFormat: translator.FormatClaude}
						if boundary == "metadata-caller" {
							ginContext.Set("apiKey", "")
							options.Metadata = map[string]any{core.CallerScopeMetadataKey: caller}
						}
						if stream {
							result, err := executor.ExecuteStream(ctx, auth, request, options)
							if err != nil {
								t.Fatal(err)
							}
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
								if chunk.Acknowledge != nil {
									chunk.Acknowledge(nil)
								}
							}
						} else if _, err := executor.Execute(ctx, auth, request, options); err != nil {
							t.Fatal(err)
						}
					}
					if len(bodies) != 2 {
						t.Fatalf("upstream requests = %d, want 2", len(bodies))
					}
					replayed := strings.Contains(string(bodies[1]), encrypted)
					if replayed != (boundary == "same" || boundary == "refreshed-token") {
						t.Fatalf(
							"%s replayed=%t, final input=%s",
							boundary,
							replayed,
							gjson.GetBytes(bodies[1], "input").Raw,
						)
					}
				},
			)
		}
	}
}
