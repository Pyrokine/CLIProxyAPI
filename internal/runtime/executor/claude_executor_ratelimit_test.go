package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestClaudeExecutor_RateLimit_CountTokensHonorsRateLimitReset(t *testing.T) {
	now := time.Now()
	sevenDayReset := now.Add(7 * 24 * time.Hour).Unix()

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/messages/count_tokens" {
					t.Errorf("count tokens request path = %q", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Anthropic-Ratelimit-Unified-Status", "rejected")
				w.Header().Set("Anthropic-Ratelimit-Unified-7d-Status", "rejected")
				w.Header().Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(sevenDayReset, 10))
				w.Header().Set("Retry-After", "604800")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write(
					[]byte(`{"type":"error","error":{"type":"rate_limit_error",` +
						`"message":"7-day rate limit exceeded."}}`),
				)
			},
		),
	)
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "claude-auth-1",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": server.URL,
		},
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	_, err := executor.countTokensUpstream(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
	)
	if err == nil {
		t.Fatal("expected error from CountTokens, got nil")
	}

	var dispositionErr cliproxyexecutor.ExecutionDispositionError
	if !errors.As(err, &dispositionErr) {
		t.Fatalf("CountTokens error = %T, want ExecutionDispositionError", err)
	}
	disposition := dispositionErr.ExecutionDisposition()
	if disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseResponseStarted ||
		disposition.Outcome != cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing ||
		disposition.RetryDirective != cliproxyexecutor.RetryDirectiveRetryAfter ||
		disposition.EffectScope != cliproxyexecutor.StateEffectScopeCredential {
		t.Fatalf("CountTokens disposition = %+v, want received credential-scoped rate limit", disposition)
	}

	var retryAfter interface{ RetryAfter() *time.Duration }
	if !errors.As(err, &retryAfter) || retryAfter == nil || retryAfter.RetryAfter() == nil {
		t.Fatalf("expected CountTokens rate limit error to implement RetryAfter, got %v", err)
	}
	var credentialScoped interface{ IsCredentialScoped() bool }
	if !errors.As(err, &credentialScoped) || credentialScoped == nil || !credentialScoped.IsCredentialScoped() {
		t.Fatalf("expected CountTokens rate limit error to be credential-scoped, got %v", err)
	}

	minExpected := 7*24*time.Hour - 5*time.Second
	maxExpected := 7*24*time.Hour + 35*time.Second
	if *retryAfter.RetryAfter() < minExpected || *retryAfter.RetryAfter() > maxExpected {
		t.Fatalf("RetryAfter = %v, want between %v and %v", *retryAfter.RetryAfter(), minExpected, maxExpected)
	}
}
