package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAntigravityEnsureAccessTokenUsesFiveMinuteSafetyWindow(t *testing.T) {
	t.Parallel()

	executor := &AntigravityExecutor{}
	now := time.Now()

	t.Run(
		"uses token outside safety window", func(t *testing.T) {
			auth := &cliproxyauth.Auth{
				Metadata: map[string]any{
					"access_token": "still-valid-access",
					"expired":      now.Add(5*time.Minute + time.Minute).Format(time.RFC3339),
				},
			}

			token, updated, errToken := executor.ensureAccessToken(context.Background(), auth)
			if errToken != nil {
				t.Fatalf("ensureAccessToken() error = %v", errToken)
			}
			if token != "still-valid-access" || updated != nil {
				t.Fatalf("ensureAccessToken() = %q, %#v, want existing token and nil update", token, updated)
			}
		},
	)

	t.Run(
		"uses relative expiry with issued_at seconds", func(t *testing.T) {
			issuedAt := now.Add(-10 * time.Minute).Truncate(time.Second)
			auth := &cliproxyauth.Auth{
				Metadata: map[string]any{
					"access_token": "relative-expiry-access",
					"expires_in":   3600,
					"issued_at":    issuedAt.Unix(),
				},
			}

			token, updated, errToken := executor.ensureAccessToken(context.Background(), auth)
			if errToken != nil {
				t.Fatalf("ensureAccessToken() error = %v", errToken)
			}
			if token != "relative-expiry-access" || updated != nil {
				t.Fatalf("ensureAccessToken() = %q, %#v, want existing token and nil update", token, updated)
			}
		},
	)

	t.Run(
		"refreshes token inside safety window", func(t *testing.T) {
			auth := &cliproxyauth.Auth{
				Metadata: map[string]any{
					"access_token": "expiring-access",
					"expired":      now.Add(5*time.Minute - time.Minute).Format(time.RFC3339),
				},
			}

			token, updated, errToken := executor.ensureAccessToken(context.Background(), auth)
			if errToken == nil || !strings.Contains(errToken.Error(), "missing refresh token") {
				t.Fatalf("ensureAccessToken() error = %v, want refresh attempt", errToken)
			}
			if token != "" || updated != nil {
				t.Fatalf("ensureAccessToken() = %q, %#v, want empty result after failed refresh", token, updated)
			}
		},
	)
}
