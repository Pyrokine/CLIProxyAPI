package auth

import (
	"context"
	"strings"
	"testing"

	devinauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/devin"
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
			name:      "callback state mismatch",
			input:     "http://127.0.0.1:1455/callback?code=fixture-code&state=wrong",
			wantError: "state mismatch",
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
