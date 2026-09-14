package auth

import (
	"errors"
	"net/http"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestAntigravityCreditsFallbackRequiresReplaySafeOutcome(t *testing.T) {
	manager := &Manager{}
	manager.runtimeConfig.Store(
		&internalconfig.Config{
			QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
		},
	)
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "quota rejected before processing",
			err:  executionOutcomeStatusError{status: http.StatusTooManyRequests},
			want: true,
		},
		{
			name: "provider response outcome unknown",
			err:  executionOutcomeStatusError{status: http.StatusServiceUnavailable},
		},
		{
			name: "transport outcome unknown",
			err:  errors.New("connection reset after request write"),
		},
		{
			name: "no normal candidate attempted",
			err:  &Error{Code: "auth_not_found", Message: "no auth available"},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				if got := shouldAttemptAntigravityCreditsFallback(manager, tt.err); got != tt.want {
					t.Fatalf("shouldAttemptAntigravityCreditsFallback() = %t, want %t", got, tt.want)
				}
			},
		)
	}
}
