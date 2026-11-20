package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexTerminalOriginalErrorFieldsAndStatus(t *testing.T) {
	for _, test := range []struct {
		name, event, wantType, wantCode string
		status                          int
		retry                           time.Duration
	}{
		{
			name: "context response failed", status: http.StatusBadRequest,
			event: `{"type":"response.failed","response":{"id":"resp_1","status":"failed",` +
				`"error":{"code":"context_length_exceeded","message":"Your input exceeds the context window ` +
				`of this model. Please adjust your input and try again."}}}`,
			wantType: "invalid_request_error", wantCode: "context_too_large",
		},
		{
			name: "context top-level error", status: http.StatusBadRequest,
			event: `{"type":"error","code":"context_length_exceeded","message":` +
				`"Your input exceeds the context window of this model. Please adjust your input and try again.",` +
				`"sequence_number":2}`,
			wantType: "invalid_request_error", wantCode: "context_too_large",
		},
		{
			name: "invalid request", status: http.StatusBadRequest,
			event: `{"type":"error","error":{"type":"invalid_request_error",` +
				`"code":"invalid_value","message":"Invalid input."}}`,
			wantType: "invalid_request_error", wantCode: "invalid_value",
		},
		{
			name: "cyber policy", status: http.StatusBadRequest,
			event: `{"type":"error","error":{"type":"invalid_request","code":"cyber_policy",` +
				`"message":"This content was flagged for possible cybersecurity risk."}}`,
			wantType: "invalid_request", wantCode: "cyber_policy",
		},
		{
			name: "authentication", status: http.StatusUnauthorized,
			event: `{"type":"response.failed","response":{"error":{"type":"authentication_error",` +
				`"code":"invalid_api_key","message":"Invalid token."}}}`,
			wantType: "authentication_error", wantCode: "auth_unavailable",
		},
		{
			name: "rate limit", status: http.StatusTooManyRequests,
			event: `{"type":"error","error":{"type":"rate_limit_error",` +
				`"code":"rate_limit_exceeded","message":"Rate limit reached."}}`,
			wantType: "rate_limit_error", wantCode: "rate_limit_exceeded",
		},
		{
			name: "unknown upstream failure", status: http.StatusBadGateway,
			event: `{"type":"response.failed","response":{"error":{"type":"upstream_error",` +
				`"code":"unknown","message":"Upstream failed."}}}`,
			wantType: "upstream_error", wantCode: "unknown",
		},
		{
			name: "overload without buffering", status: http.StatusBadGateway,
			event: `{"type":"error","error":{"type":"service_unavailable_error",` +
				(`"code":"server_is_overloaded","message":"Our servers are currently ` +
					`overloaded. Please try again ` +
					`later."}}`),
			wantType: "service_unavailable_error", wantCode: "server_is_overloaded",
		},
		{
			name: "model not found with invalid request type", status: http.StatusNotFound,
			event: `{"type":"error","error":{"type":"invalid_request_error","code":"model_not_found",` +
				`"message":"The model gpt-5.5 does not exist or you do not have access to it."}}`,
			wantType: "invalid_request_error", wantCode: "model_not_found",
		},
		{
			name: "usage limit error event", status: http.StatusTooManyRequests, retry: 300 * time.Second,
			event: `{"type":"error","error":{"type":"usage_limit_reached",` +
				`"message":"You've hit your usage limit.","resets_in_seconds":300}}`,
			wantType: "usage_limit_reached",
		},
		{
			name: "usage limit response failed", status: http.StatusTooManyRequests, retry: 60 * time.Second,
			event: `{"type":"response.failed","response":{"error":{"type":"usage_limit_reached",` +
				`"message":"usage limit reached","resets_in_seconds":60}}}`,
			wantType: "usage_limit_reached",
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				err, handled := parseCodexEventError([]byte(test.event))
				if !handled || err == nil || statusErrCode(err) != test.status {
					t.Fatalf(
						"handled=%t status=%d error=%v, want %d",
						handled,
						statusErrCode(err),
						err,
						test.status,
					)
				}
				if gjson.Get(err.Error(), "error.type").String() != test.wantType ||
					gjson.Get(err.Error(), "error.code").String() != test.wantCode {
					t.Errorf("terminal error lost its original compatible fields: %v", err)
				}
				if sequence := gjson.Get(test.event, "sequence_number"); sequence.Exists() &&
					gjson.Get(err.Error(), "sequence_number").Raw != sequence.Raw {
					t.Errorf("terminal error lost sequence_number: %v", err)
				}
				if test.retry != 0 {
					retry, ok := err.(interface{ RetryAfter() *time.Duration })
					if !ok || retry.RetryAfter() == nil || *retry.RetryAfter() != test.retry {
						t.Errorf("terminal retry = %v, want %v", err, test.retry)
					}
				}
			},
		)
	}
	for _, event := range []string{
		`{"type":"response.completed","response":{"status":"completed"}}`,
		`{"type":"response.incomplete","response":{"status":"incomplete"}}`,
	} {
		if err, handled := parseCodexEventError([]byte(event)); handled || err != nil {
			t.Errorf("successful terminal was classified as an error: %s, %v", event, err)
		}
	}
}

func TestCodexTransportFailureBeforeAndAfterTerminal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, completed := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("stream=%t/completed=%t", stream, completed), func(t *testing.T) {
					capture := newCodexContractUsageCapture(t)
					auth := runtimeCodexAuth("https://codex-terminal.invalid")
					auth.ID = capture.authID
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					t.Cleanup(cancel)
					calls := 0
					ctx = util.WithRoundTripper(
						ctx, devinTestRoundTripper(
							func(r *http.Request) (*http.Response, error) {
								calls++
								frame := ""
								if completed {
									frame = `data: {"type":"response.completed","response":{"id":"resp_partial",` +
										`"model":"gpt-5.5","output":[],"usage":{"input_tokens":1,"output_tokens":2,` +
										`"total_tokens":3}}}` + "\n\n"
								}
								body := io.MultiReader(
									strings.NewReader(frame),
									iotest.ErrReader(io.ErrUnexpectedEOF),
								)
								return &http.Response{
									StatusCode: http.StatusOK, Request: r,
									Header: http.Header{
										"Content-Type": {"text/event-stream"},
									}, Body: io.NopCloser(body),
								}, nil
							},
						),
					)
					executor := newCodexExecutor(&config.Config{})
					req := core.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hi"}`)}
					opts := core.Options{SourceFormat: translator.FormatOpenAIResponse, Stream: stream}
					var executeErr error
					if stream {
						result, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								executeErr = chunk.Err
							}
						}
					} else {
						_, executeErr = executor.Execute(ctx, auth, req, opts)
					}
					if calls != 1 || (executeErr == nil) != completed {
						t.Fatalf("calls=%d completed=%t error=%v", calls, completed, executeErr)
					}
					if !completed {
						var scoped core.RequestScopedError
						if statusErrCode(executeErr) != http.StatusRequestTimeout ||
							!errors.As(executeErr, &scoped) || !scoped.IsRequestScoped() {
							t.Fatalf(
								"pre-terminal transport error lost its request-scoped HTTP408: %v",
								executeErr,
							)
						}
					}
					record := capture.next(t)
					if record.Failed == completed {
						t.Fatalf(
							"terminal outcome and usage disagree: completed=%t record=%+v",
							completed,
							record,
						)
					}
				},
			)
		}
	}
}

func TestCodexOriginalContextErrorReachesExecuteAndStream(t *testing.T) {
	const message = "Your input exceeds the context window of this model. Please adjust your input and try again."
	const event = `{"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded",` +
		`"message":"` + message + `","param":"input"},"sequence_number":2}`
	for _, stream := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				t.Cleanup(cancel)
				calls := 0
				ctx = util.WithRoundTripper(
					ctx, devinTestRoundTripper(
						func(r *http.Request) (*http.Response, error) {
							calls++
							body := "event: response.created\n" +
								(`data: {"type":"response.created","response":{"id":"resp_1","model":` +
									`"gpt-5.5"}}`) +
								"\n\n" +
								"event: error\ndata: " + event + "\n\n" +
								`event: response.failed` + "\n" +
								`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed",` +
								`"error":{"code":"context_length_exceeded","message":"` + message + `"}}}` + "\n\n"
							return &http.Response{
								StatusCode: http.StatusOK, Request: r,
								Header: http.Header{"Content-Type": {"text/event-stream"}},
								Body:   io.NopCloser(strings.NewReader(body)),
							}, nil
						},
					),
				)
				err := codexConsumerError(
					t, ctx, runtimeCodexAuth("https://codex-terminal.invalid"), core.Request{
						Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
					}, core.Options{SourceFormat: translator.FormatOpenAIResponse, Stream: stream}, false, stream,
				)
				if calls != 1 || statusErrCode(err) != http.StatusBadRequest {
					t.Fatalf("calls=%d error=%v, want one request and HTTP 400", calls, err)
				}
				if gjson.Get(err.Error(), "error.type").String() != "invalid_request_error" ||
					gjson.Get(err.Error(), "error.code").String() != "context_too_large" ||
					gjson.Get(err.Error(), "error.message").String() != message ||
					gjson.Get(err.Error(), "error.param").String() != "input" ||
					gjson.Get(err.Error(), "sequence_number").Int() != 2 {
					t.Fatalf("public executor lost original compatible context error fields: %v", err)
				}
			},
		)
	}
}
