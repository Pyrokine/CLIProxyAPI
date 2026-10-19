package clienterror

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

type statusError struct {
	status int
	body   string
}

func (e statusError) Error() string   { return e.body }
func (e statusError) StatusCode() int { return e.status }

type upstreamBodyError struct {
	statusError
	responseBody string
}

func (e upstreamBodyError) ResponseBody() []byte { return []byte(e.responseBody) }

type directBodyError struct {
	body   []byte
	direct bool
}

func (e directBodyError) Error() string        { return "synthetic diagnostic" }
func (e directBodyError) DirectResponse() bool { return e.direct }
func (e directBodyError) ResponseBody() []byte { return e.body }

func TestResponseBodyFromError(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		body   string
		direct bool
	}{
		{"nil", nil, "", false},
		{"plain", errors.New("failure"), "", false},
		{"unmarked", directBodyError{body: []byte("private diagnostic")}, "", false},
		{"marked", directBodyError{body: []byte(" denied\n"), direct: true}, " denied\n", true},
		{"wrapped", fmt.Errorf("wrapper: %w", directBodyError{body: []byte(" denied\n"), direct: true}), " denied\n", true},
		{"empty", directBodyError{direct: true}, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, direct := ResponseBodyFromError(test.err)
			if direct != test.direct || string(body) != test.body {
				t.Fatalf("body=%q direct=%t", body, direct)
			}
		})
	}
}

func TestHTTPStatusFromError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil", err: nil, want: 0},
		{name: "plain error", err: errors.New("boom"), want: 0},
		{name: "context canceled", err: context.Canceled, want: StatusClientClosedRequest},
		{name: "context deadline exceeded", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{
			name: "url error wraps canceled",
			err:  &url.Error{Op: "Post", URL: "https://example.com", Err: context.Canceled},
			want: StatusClientClosedRequest,
		},
		{
			name: "url error wraps deadline",
			err:  &url.Error{Op: "Post", URL: "https://example.com", Err: context.DeadlineExceeded},
			want: http.StatusGatewayTimeout,
		},
		{
			name: "fmt wrap canceled",
			err:  fmt.Errorf("upstream: %w", context.Canceled),
			want: StatusClientClosedRequest,
		},
		{
			name: "explicit status code wins",
			err:  statusError{status: http.StatusTooManyRequests, body: "rate limited"},
			want: http.StatusTooManyRequests,
		},
		{
			name: "explicit status wins over canceled unwrap",
			err: statusAndUnwrapError{
				status: http.StatusTooManyRequests,
				body:   "rate limited",
				cause:  context.Canceled,
			},
			want: http.StatusTooManyRequests,
		},
		{
			name: "zero status code falls through to canceled unwrap",
			err: statusAndUnwrapError{
				status: 0,
				body:   "canceled",
				cause:  context.Canceled,
			},
			want: StatusClientClosedRequest,
		},
		{
			name: "zero status code without unwrap stays unknown",
			err:  statusError{status: 0, body: context.Canceled.Error()},
			want: 0,
		},
		{
			name: "wrapped status code via errors.As",
			err:  fmt.Errorf("execute failed: %w", statusError{status: http.StatusUnauthorized, body: "unauthorized"}),
			want: http.StatusUnauthorized,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				if got := HTTPStatusFromError(test.err); got != test.want {
					t.Fatalf("HTTPStatusFromError() = %d, want %d", got, test.want)
				}
			},
		)
	}

	if got := HTTPStatusFromErrorOr(errors.New("boom"), http.StatusBadGateway); got != http.StatusBadGateway {
		t.Fatalf("HTTPStatusFromErrorOr(plain) = %d, want %d", got, http.StatusBadGateway)
	}
	if got := HTTPStatusFromErrorOr(context.Canceled, http.StatusBadGateway); got != StatusClientClosedRequest {
		t.Fatalf("HTTPStatusFromErrorOr(canceled) = %d, want %d", got, StatusClientClosedRequest)
	}
}

type statusAndUnwrapError struct {
	status int
	body   string
	cause  error
}

func (e statusAndUnwrapError) Error() string { return e.body }
func (e statusAndUnwrapError) StatusCode() int {
	return e.status
}
func (e statusAndUnwrapError) Unwrap() error { return e.cause }

func TestIsRequestFaultStructuredIdentifiers(t *testing.T) {
	for _, code := range []string{
		"cyber_policy",
		"context_length_exceeded",
		"message_too_big",
		"string_above_max_length",
		"invalid_prompt",
		"invalid_value",
		"unsupported_value",
		"invalid_request_error",
		"previous_response_not_found",
	} {
		t.Run(
			"code/"+code, func(t *testing.T) {
				err := errors.New(`{"error":{"code":"` + code + `"}}`)
				if !IsRequestFault(http.StatusBadGateway, err) {
					t.Fatalf("code %q was not classified as a request fault", code)
				}
			},
		)
	}

	for _, errType := range []string{
		"invalid_request",
		"invalid_request_error",
		"bad_request_error",
		"invalid_prompt",
	} {
		t.Run(
			"type/"+errType, func(t *testing.T) {
				err := errors.New(`{"error":{"type":"` + errType + `"}}`)
				if !IsRequestFault(http.StatusBadGateway, err) {
					t.Fatalf("type %q was not classified as a request fault", errType)
				}
			},
		)
	}
}

func TestIsRequestFault(t *testing.T) {
	tests := []struct {
		name   string
		status int
		err    error
		want   bool
	}{
		{name: "bad request status", status: http.StatusBadRequest, err: errors.New("bad request"), want: true},
		{name: "conflict status", status: http.StatusConflict, err: errors.New("conflict"), want: true},
		{
			name: "entity too large status", status: http.StatusRequestEntityTooLarge, err: errors.New("too large"),
			want: true,
		},
		{
			name: "unprocessable status", status: http.StatusUnprocessableEntity, err: errors.New("unprocessable"),
			want: true,
		},
		{
			name:   "cyber policy behind bad gateway",
			status: http.StatusBadGateway,
			err:    errors.New(`{"error":{"type":"invalid_request","code":"cyber_policy","message":"blocked"}}`),
			want:   true,
		},
		{
			name:   "context length behind internal error",
			status: http.StatusInternalServerError,
			err:    errors.New(`{"response":{"error":{"type":"server_error","code":"context_length_exceeded"}}}`),
			want:   true,
		},
		{
			name:   "invalid request type behind bad gateway",
			status: http.StatusBadGateway,
			err:    errors.New(`{"body":{"error":{"type":"invalid_request","message":"invalid"}}}`),
			want:   true,
		},
		{
			name: "status from error",
			err:  statusError{status: http.StatusConflict, body: "conflict"},
			want: true,
		},
		{name: "unauthorized", status: http.StatusUnauthorized, err: errors.New("invalid token")},
		{
			name:   "authentication error status overrides invalid request body",
			status: http.StatusUnauthorized,
			err: errors.New(
				`{"error":{"type":"authentication_error","code":"invalid_request_error","message":"invalid key"}}`,
			),
		},
		{
			name:   "payment status overrides invalid request body",
			status: http.StatusPaymentRequired,
			err:    errors.New(`{"error":{"type":"invalid_request_error","code":"invalid_request_error"}}`),
		},
		{
			name:   "rate limit status overrides invalid request body",
			status: http.StatusTooManyRequests,
			err:    errors.New(`{"error":{"type":"invalid_request_error","code":"invalid_request_error"}}`),
		},
		{
			name:   "model not found overrides invalid request type",
			status: http.StatusBadRequest,
			err: errors.New(
				`{"error":{"type":"invalid_request_error","code":"model_not_found","message":"unknown model"}}`,
			),
		},
		{
			name:   "model not found code prefix",
			status: http.StatusBadRequest,
			err:    errors.New("model_not_found: unknown model"),
		},
		{
			name: "item not persisted", status: http.StatusNotFound,
			err: errors.New("Item with id 'rs_1' not found. Items are not persisted when `store` is set to false."),
			want: true,
		},
		{
			name: "Claude stale thread", status: http.StatusNotFound,
			err: errors.New(`{"error":{"type":"not_found_error","message":"No thread state was found for the requested previous_message_id."}}`),
			want: true,
		},
		{
			name: "Claude stale thread in response body",
			err: upstreamBodyError{
				statusError: statusError{status: http.StatusNotFound, body: "upstream failure"},
				responseBody: `{"error":{"type":"not_found_error","message":"No thread state was found for previous_message_id."}}`,
			},
			want: true,
		},
		{
			name: "Claude stale thread plain text", status: http.StatusNotFound,
			err: errors.New("No thread state was found for previous_message_id."), want: true,
		},
		{
			name: "unrelated Claude 404", status: http.StatusNotFound,
			err: errors.New(`{"error":{"type":"not_found_error","message":"Not Found"}}`),
		},
		{
			name: "Claude thread server failure", status: http.StatusInternalServerError,
			err: errors.New(`{"error":{"type":"not_found_error","message":"No thread state was found for previous_message_id."}}`),
		},
		{name: "quota", status: http.StatusTooManyRequests, err: errors.New("quota")},
		{name: "transport", status: http.StatusBadGateway, err: errors.New("unexpected EOF")},
		{name: "invalid JSON body", status: http.StatusBadGateway, err: fmt.Errorf("%s", `{"error":`)},
		{name: "nil", status: 0},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				if got := IsRequestFault(test.status, test.err); got != test.want {
					t.Fatalf("IsRequestFault(%d, %v) = %t, want %t", test.status, test.err, got, test.want)
				}
			},
		)
	}
}
