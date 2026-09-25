package logging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"testing"
)

func TestSafeDiagnosticForLogIdempotent(t *testing.T) {
	for _, input := range []string{"Authorization: Bearer hidden-value", "cookie=hidden-value", "https://name:hidden-value@example.test?code=hidden-value"} {
		once := SafeDiagnosticForLog(input)
		if twice := SafeDiagnosticForLog(once); twice != once {
			t.Fatalf("first diagnostic=%q, second=%q", once, twice)
		}
	}
}

func TestSafeErrorDiagnosticAllowlist(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"wrapped EOF", fmt.Errorf("hidden-value: %w", io.EOF), "EOF"},
		{"unexpected EOF", io.ErrUnexpectedEOF, "unexpected_EOF"},
		{"canceled", fmt.Errorf("hidden-value: %w", context.Canceled), "canceled"},
		{"timeout", fmt.Errorf("hidden-value: %w", context.DeadlineExceeded), "timeout"},
		{"proxy auth", errors.New("socks authentication failed with hidden-value"), "proxy_authentication_failed proxy=socks"},
		{"refused", errors.New("socks connect with hidden-value: connection refused"), "proxy=socks connection_refused"},
		{"network", errors.New("dial hidden-value: no route to host"), "dial_failed network_unreachable"},
		{"JSON", errors.New("invalid character hidden-value"), "invalid_response_json"},
		{"OAuth", errors.New("hidden-value invalid_grant status 400"), "oauth_error=invalid_grant status=400"},
		{"unknown", errors.New("hidden-value"), "error_type=*errors.errorString"},
		{"URL", &url.Error{Op: "Post", URL: "https://hidden-value@example.test/token?code=hidden-value&error=invalid_grant&status=401", Err: io.EOF}, "EOF"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := SafeErrorDiagnostic(test.err)
			if got != test.want || strings.Contains(got, "hidden-value") {
				t.Fatalf("diagnostic=%q, want %q", got, test.want)
			}
		})
	}
}
