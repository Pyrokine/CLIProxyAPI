package auth

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestParseDevinManualLoginInput(t *testing.T) {
	const state = "expected-state"
	tests := []struct {
		name      string
		input     string
		wantCode  string
		wantToken string
		accepted  bool
		wantError string
	}{
		{name: "empty", input: "  "},
		{
			name:      "prefixed session token",
			input:     "  devin-session-token$fixture  ",
			wantToken: "devin-session-token$fixture",
			accepted:  true,
		},
		{
			name:      "quoted session token",
			input:     " 'devin-session-token$fixture' ",
			wantToken: "devin-session-token$fixture",
			accepted:  true,
		},
		{
			name:      "JWT session token",
			input:     "eyJhbGciOiJub25lIn0.fixture.signature",
			wantToken: "eyJhbGciOiJub25lIn0.fixture.signature",
			accepted:  true,
		},
		{
			name:     "callback URL",
			input:    "http://127.0.0.1:1455/callback?code=fixture-code&state=" + state,
			wantCode: "fixture-code",
			accepted: true,
		},
		{
			name:     "headless URL without state",
			input:    "https://example.test/callback?code=fixture-code",
			wantCode: "fixture-code",
			accepted: true,
		},
		{
			name:      "callback state mismatch",
			input:     "http://127.0.0.1:1455/callback?code=fixture-code&state=wrong",
			wantError: "state mismatch",
		},
		{
			name:      "authorization denied",
			input:     "https://example.test/callback?error=access_denied",
			wantError: "OAuth error: access_denied",
		},
		{
			name:      "authorization denied with description",
			input:     "https://example.test/callback?error=access_denied&error_description=user%20denied",
			wantError: "OAuth error: access_denied: user denied",
		},
		{
			name:      "fragment authorization error",
			input:     "https://example.test/callback#error=access_denied&error_description=user%20denied",
			wantError: "OAuth error: access_denied: user denied",
		},
		{name: "raw code", input: "fixture-code", wantCode: "fixture-code", accepted: true},
		{name: "unrecognized structured input", input: "not a valid callback URL"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, token, accepted, err := parseDevinManualLoginInput(test.input, state)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("parseDevinManualLoginInput() error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDevinManualLoginInput() error = %v", err)
			}
			if code != test.wantCode || token != test.wantToken || accepted != test.accepted {
				t.Fatalf(
					"parseDevinManualLoginInput() = %q/%q/%t, want %q/%q/%t",
					code,
					token,
					accepted,
					test.wantCode,
					test.wantToken,
					test.accepted,
				)
			}
		})
	}
}

func TestDevinNoBrowserDoesNotBindCallbackPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	promptError := errors.New("fixture prompt failure")
	authenticator := &DevinAuthenticator{CallbackPort: listener.Addr().(*net.TCPAddr).Port}
	_, err = authenticator.Login(context.Background(), &config.Config{}, &LoginOptions{
		NoBrowser: true,
		Prompt: func(string) (string, error) {
			return "", promptError
		},
	})
	if !errors.Is(err, promptError) {
		t.Fatalf("headless Login error = %v, want prompt error without binding callback port", err)
	}
}

func TestDevinNoBrowserPreservesOAuthErrorDescription(t *testing.T) {
	_, err := NewDevinAuthenticator().Login(context.Background(), &config.Config{}, &LoginOptions{
		NoBrowser: true,
		Prompt: func(string) (string, error) {
			return "https://example.test/callback?error=access_denied&error_description=user%20denied", nil
		},
	})
	if err == nil || err.Error() != "devin OAuth error: access_denied: user denied" {
		t.Fatalf("Login() error = %v, want complete OAuth rejection", err)
	}
}

func TestWaitForDevinLoginResultPreservesCallbackErrorDescription(t *testing.T) {
	results := make(chan *devinauth.OAuthResult, 1)
	results <- &devinauth.OAuthResult{Error: "access_denied: user denied"}
	_, _, err := waitForDevinLoginResult(context.Background(), "state", nil, results, make(chan error))
	if err == nil || err.Error() != "devin OAuth error: access_denied: user denied" {
		t.Fatalf("waitForDevinLoginResult() error = %v, want complete OAuth rejection", err)
	}
}

func TestWaitForDevinLoginResultHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := waitForDevinLoginResult(
		ctx,
		"state",
		nil,
		make(chan *devinauth.OAuthResult),
		make(chan error),
	)
	if err == nil || err != context.Canceled {
		t.Fatalf("waitForDevinLoginResult() error = %v, want context canceled", err)
	}
}
