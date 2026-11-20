package executor

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestClaudeExecutionMetadataUsesFirstNonEmptySource(t *testing.T) {
	for _, test := range []struct{ name, option, want string }{
		{"empty option", "", "request-session"},
		{"whitespace option", "   ", "request-session"},
		{"nonempty option wins", "option-session", "option-session"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				credential := fingerprintAuth("claude-code-cli", "https://api.anthropic.com")
				credential.ID = t.Name()
				request := fingerprintRequest(fingerprintPayload)
				request.Metadata = map[string]any{core.ExecutionSessionMetadataKey: "request-session"}
				opts := fingerprintOptions()
				opts.Metadata = map[string]any{core.ExecutionSessionMetadataKey: test.option}
				wantSession := helps.ClaudeAgentSessionUUIDForRequest(
					nil, request.Payload, request.Payload,
					false, map[string]any{core.ExecutionSessionMetadataKey: test.want},
				)
				var headers []http.Header
				var bodies [][]byte
				messageCalls := 0
				ctx := util.WithRoundTripper(
					t.Context(), devinTestRoundTripper(
						func(req *http.Request) (*http.Response, error) {
							body, err := io.ReadAll(req.Body)
							if err != nil {
								return nil, err
							}
							headers = append(headers, req.Header.Clone())
							bodies = append(bodies, body)
							response := `{"input_tokens":42}`
							if !strings.HasSuffix(req.URL.Path, "/count_tokens") {
								messageCalls++
								response = fmt.Sprintf(
									`{"id":"msg_metadata_%d","type":"message","role":"assistant",`+
										`"model":"claude-opus-4-7","content":[{"type":"text","text":"ok"}],`+
										`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
									messageCalls,
								)
							}
							return &http.Response{
								StatusCode: http.StatusOK, Request: req,
								Header: http.Header{"Content-Type": {"application/json"}},
								Body:   io.NopCloser(strings.NewReader(response)),
							}, nil
						},
					),
				)
				execute := NewClaudeExecutor(&config.Config{})
				for range 2 {
					if _, err := execute.Execute(ctx, credential, request, opts); err != nil {
						t.Fatal(err)
					}
				}
				count, err := execute.CountTokens(ctx, credential, request, opts)
				if err != nil {
					t.Fatal(err)
				}
				if len(headers) != 3 || messageCalls != 2 || gjson.GetBytes(count.Payload, "input_tokens").Int() != 42 {
					t.Fatalf("requests=%d message calls=%d count=%s", len(headers), messageCalls, count.Payload)
				}
				for i, header := range headers {
					if got := headerValueCaseInsensitive(header, "X-Claude-Code-Session-Id"); got != wantSession {
						t.Errorf("request %d session=%q, want %q", i, got, wantSession)
					}
					if i < 2 {
						identity := gjson.Parse(gjson.GetBytes(bodies[i], "metadata.user_id").String())
						if identity.Get("session_id").String() != wantSession {
							t.Errorf("request %d metadata identity=%s, want %q", i, identity.Raw, wantSession)
						}
					}
				}
				if gjson.GetBytes(bodies[1], "diagnostics.previous_message_id").String() != "msg_metadata_1" {
					t.Errorf("lower-priority execution session lost continuity: %s", bodies[1])
				}
				if request.Metadata[core.ExecutionSessionMetadataKey] != "request-session" ||
					opts.Metadata[core.ExecutionSessionMetadataKey] != test.option {
					t.Fatal("session resolution mutated caller metadata")
				}
			},
		)
	}
}
