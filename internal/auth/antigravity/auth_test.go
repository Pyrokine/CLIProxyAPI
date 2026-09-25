package antigravity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestUpstreamHTTPErrorPreservesStatusCode(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"error":{"code":403,"message":"permission denied"}}`,
			)),
		}, nil
	})}
	auth := NewAuth(nil, client)

	tests := map[string]func() error{
		"token exchange": func() error {
			_, err := auth.ExchangeCodeForTokens(context.Background(), "code", "http://localhost/callback")
			return err
		},
		"user info": func() error {
			_, err := auth.FetchUserInfo(context.Background(), "access-token")
			return err
		},
		"project discovery": func() error {
			_, err := auth.FetchProjectID(context.Background(), "access-token")
			return err
		},
		"onboarding": func() error {
			_, err := auth.onboardUser(context.Background(), "access-token", "free-tier")
			return err
		},
	}
	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("expected upstream HTTP error")
			}
			var statusError interface{ StatusCode() int }
			if !errors.As(err, &statusError) {
				t.Fatalf("error type = %T, want status-coded error", err)
			}
			if statusError.StatusCode() != http.StatusForbidden {
				t.Fatalf("status code = %d, want %d", statusError.StatusCode(), http.StatusForbidden)
			}
		})
	}
}
