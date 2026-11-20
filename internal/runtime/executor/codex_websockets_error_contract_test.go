package executor

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestParseCodexWebsocketErrorMarksConnectionLimitRetryable(t *testing.T) {
	payload := `{"type":"error","status":429,"error":{"code":"websocket_connection_limit_reached",` +
		`"message":"too many websockets"},"headers":{"retry-after":"1"}}`
	err, ok := parseCodexWebsocketError([]byte(payload))
	if !ok {
		t.Fatal("expected websocket error")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %#v, want 429", err)
	}
	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatal("expected retryable websocket connection limit error")
	}
	if got := *retryable.RetryAfter(); got != 0 {
		t.Fatalf("retryAfter = %v, want connection-limit fallback 0", got)
	}
	withHeaders, ok := err.(interface{ Headers() http.Header })
	if !ok || withHeaders.Headers().Get("retry-after") != "1" {
		t.Fatalf("headers = %#v, want retry-after", err)
	}
}

func TestParseCodexWebsocketErrorUsesUsageLimitRetryMetadata(t *testing.T) {
	payload := `{"type":"error","status":429,"body":{"error":{"type":"usage_limit_reached",` +
		`"message":"usage limit reached","resets_in_seconds":7}}}`
	err, ok := parseCodexWebsocketError([]byte(payload))
	if !ok {
		t.Fatal("expected websocket error")
	}
	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatal("expected retryable usage limit websocket error")
	}
	if got := *retryable.RetryAfter(); got != 7*time.Second {
		t.Fatalf("retryAfter = %v, want 7s", got)
	}
}

func TestParseCodexWebsocketErrorPreservesWrappedBodyAndHeaders(t *testing.T) {
	payload := `{"type":"error","status":429,"body":{"error":{"code":"websocket_connection_limit_reached",` +
		`"type":"server_error","message":"too many websocket connections"}},"headers":{"x-request-id":"req-1"}}`
	err, ok := parseCodexWebsocketError([]byte(payload))
	if !ok {
		t.Fatal("expected websocket error")
	}
	parsed := gjson.Parse(err.Error())
	if got := parsed.Get("status").Int(); got != http.StatusTooManyRequests {
		t.Fatalf("wrapped status = %d, want 429; payload=%s", got, err.Error())
	}
	if got := parsed.Get("body.error.code").String(); got != "websocket_connection_limit_reached" {
		t.Fatalf(
			"wrapped body error code = %s, want websocket_connection_limit_reached; payload=%s",
			got, err.Error(),
		)
	}
	if got := parsed.Get("error.code").String(); got != "websocket_connection_limit_reached" {
		t.Fatalf(
			"surface error code = %s, want websocket_connection_limit_reached; payload=%s",
			got, err.Error(),
		)
	}
	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatal("expected body.error.code websocket connection limit to be retryable")
	}
	withHeaders, ok := err.(interface{ Headers() http.Header })
	if !ok || withHeaders.Headers().Get("x-request-id") != "req-1" {
		t.Fatalf("headers = %#v, want x-request-id", err)
	}
}

// noinspection GoTypeAssertionOnErrors
func TestCodexWebsocketConnectionLimitOriginalLocations(t *testing.T) {
	for index, fragment := range []string{
		`"error":{"code":"websocket_connection_limit_reached"}`,
		`"error":{"type":"websocket_connection_limit_reached"}`,
		`"body":{"error":{"code":"websocket_connection_limit_reached"}}`,
		`"body":{"error":{"type":"websocket_connection_limit_reached"}}`,
		`"code":"websocket_connection_limit_reached"`,
		`"error":"websocket_connection_limit_reached"`,
	} {
		t.Run(
			fmt.Sprint(index), func(t *testing.T) {
				payload := `{"type":"error","status":429,` + fragment + `,"headers":{"Retry-After":"1"}}`
				err, handled := parseCodexWebsocketError([]byte(payload))
				if !handled || err == nil {
					t.Fatalf("connection limit was not recognized: %v", err)
				}
				retryable, ok := err.(interface{ RetryAfter() *time.Duration })
				if !ok || retryable.RetryAfter() == nil || *retryable.RetryAfter() != 0 {
					t.Fatalf("connection limit lost zero retry hint: %#v", err)
				}
				typed, ok := err.(core.ExecutionDispositionError)
				want := core.HTTPStatusDisposition(http.StatusTooManyRequests, true)
				want.RetryDirective = core.RetryDirectiveNoRetry
				want.EffectScope = core.StateEffectScopeRequest
				if !ok || typed.ExecutionDisposition() != want {
					t.Fatalf("connection limit lost its request-level no-retry rejection: %#v", err)
				}
			},
		)
	}
}

func TestCodexWebsocketOriginalErrorEnvelopeFallbacks(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		status  int
		message string
	}{
		{
			name: "status_code",
			payload: `{"type":"error","status_code":403,"body":{"error":{"type":"permission_error",` +
				`"message":"original permission error"}},"sequence_number":27}`,
			status: http.StatusForbidden, message: "original permission error",
		},
		{
			name: "wrapped error priority",
			payload: `{"type":"error","status":400,"error":{"message":"outer"},` +
				`"body":{"error":{"type":"invalid_request_error","message":"inner","param":"input"}}}`,
			status: http.StatusBadRequest, message: "inner",
		},
		{
			name:    "missing details",
			payload: `{"type":"error","status":503,"headers":{"x-request-id":"req-2"}}`,
			status:  http.StatusServiceUnavailable, message: http.StatusText(http.StatusServiceUnavailable),
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				err, handled := parseCodexWebsocketError([]byte(test.payload))
				if !handled || statusErrCode(err) != test.status {
					t.Fatalf("error=%v, want status=%d", err, test.status)
				}
				parsed := gjson.Parse(err.Error())
				if parsed.Get("status").Int() != int64(test.status) ||
					parsed.Get("error.message").String() != test.message {
					t.Fatalf("original error status or message changed: %s", err.Error())
				}
				original := gjson.Parse(test.payload)
				for _, field := range []string{"body", "sequence_number", "headers"} {
					if parsed.Get(field).Raw != original.Get(field).Raw {
						t.Errorf("original %s changed: %s", field, err.Error())
					}
				}
			},
		)
	}
}

// noinspection GoTypeAssertionOnErrors
func TestCodexWebsocketErrorRetryMetadataPriority(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		status  int
		delay   time.Duration
	}{
		{
			name: "quota reset wins", status: http.StatusTooManyRequests, delay: 7 * time.Second,
			payload: `{"type":"error","status":429,"error":{"type":"usage_limit_reached",` +
				`"code":"websocket_connection_limit_reached","resets_in_seconds":7},"headers":{"Retry-After":"99"}}`,
		},
		{
			name: "ordinary retry header", status: http.StatusTooManyRequests, delay: time.Second,
			payload: `{"type":"error","status":429,"error":{"type":"rate_limit_error"},` +
				`"headers":{"Retry-After":"1"}}`,
		},
		{
			name: "unknown timeout", status: http.StatusRequestTimeout, delay: time.Second,
			payload: `{"type":"error","status":408,"error":{"message":"unknown execution"},` +
				`"headers":{"Retry-After":"1"}}`,
		},
		{
			name: "unknown connection outcome", status: http.StatusBadGateway,
			payload: `{"type":"error","status":502,"error":{"code":"websocket_connection_limit_reached"},` +
				`"headers":{"Retry-After":"1"}}`,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				err, handled := parseCodexWebsocketError([]byte(test.payload))
				if !handled || statusErrCode(err) != test.status {
					t.Fatalf("error=%v, want status=%d", err, test.status)
				}
				retryable, ok := err.(interface{ RetryAfter() *time.Duration })
				if !ok || retryable.RetryAfter() == nil || *retryable.RetryAfter() != test.delay {
					t.Fatalf("retry metadata priority changed: %#v, want %v", err, test.delay)
				}
				typed, ok := err.(core.ExecutionDispositionError)
				if !ok || typed.ExecutionDisposition() != core.HTTPStatusDisposition(test.status, true) {
					t.Fatalf("retry hint changed typed processing outcome: %#v", err)
				}
			},
		)
	}
}
