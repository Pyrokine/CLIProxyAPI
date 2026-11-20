package auth

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestTerminalAuthenticationSelectionPreservesCredentialCauseAcrossPaths(t *testing.T) {
	const model = "terminal-auth-contract-model"
	credentialCause := &Error{
		Code: "unauthorized", Message: "refresh_token_reused", HTTPStatus: http.StatusUnauthorized,
	}
	modelCause := &Error{
		Code: "historical_rate_limit", Message: "historical rate limit", HTTPStatus: http.StatusTooManyRequests,
	}
	failed := &Auth{
		ID: "terminal-auth-contract-failed", Provider: "codex", Status: StatusError, Unavailable: true,
		LastError: credentialCause, UpdatedAt: time.Now(),
		ModelStates: map[string]*ModelState{model: {LastError: modelCause, UpdatedAt: time.Now().Add(-time.Hour)}},
	}
	registerSchedulerModels(t, "codex", model, failed.ID)
	scheduler := newSchedulerForTest(&RoundRobinSelector{}, failed)
	manager := NewManager(nil, nil, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	registered, err := manager.Register(t.Context(), failed)
	if err != nil {
		t.Fatal(err)
	}
	defer func(manager *Manager) {
		//lint:ignore SA1012 验证 nil context 使用独立的 5 s 停止期限
		if err := manager.StopAutoRefresh(nil); err != nil {
			t.Errorf("stop auto refresh: %v", err)
		}
	}(manager)
	for _, tc := range []struct {
		name string
		pick func() error
	}{
		{
			"selector", func() error {
				_, err := getAvailableAuths([]*Auth{failed}, "codex", model, core.Options{}, time.Now())
				return err
			},
		},
		{
			"scheduler", func() error {
				_, err := scheduler.pickSingle(t.Context(), "codex", model, core.Options{}, nil)
				return err
			},
		},
		{
			"mixed scheduler", func() error {
				scheduler.mu.Lock()
				defer scheduler.mu.Unlock()
				return scheduler.mixedUnavailableErrorLocked([]string{"codex"}, model, nil)
			},
		},
		{
			"manager snapshot",
			func() error { return manager.unavailableSelectionError([]*Auth{registered}, "codex", model) },
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				err := tc.pick()
				var base *Error
				if !IsTerminalAuthError(err) || !errors.As(
					err, &base,
				) || base.Retryable || base.HTTPStatus != http.StatusServiceUnavailable {
					t.Fatalf("terminal error=%T %v, base=%+v", err, err, base)
				}
				cause := errors.Unwrap(err)
				var upstream *Error
				if !errors.As(
					cause, &upstream,
				) || upstream.HTTPStatus != http.StatusUnauthorized || upstream.Code != "unauthorized" {
					t.Fatalf("terminal credential cause lost HTTP 401: %+v", upstream)
				}
				if cause == nil || !strings.Contains(
					cause.Error(), "refresh_token_reused",
				) || strings.Contains(cause.Error(), "historical rate limit") {
					t.Fatalf("terminal cause=%v", cause)
				}
			},
		)
	}
}

func TestRetryRoundKeepsTerminalAuthenticationClassificationAndObservedCause(t *testing.T) {
	upstream := &Error{HTTPStatus: http.StatusBadGateway, Message: "observed upstream failure"}
	terminal := NewTerminalAuthError(
		&Error{Code: "auth_unavailable", HTTPStatus: http.StatusServiceUnavailable},
		&Error{Code: "unauthorized", HTTPStatus: http.StatusUnauthorized, Message: "refresh_token_reused"},
	)
	state := &retryRoundState{upstreamErr: upstream}
	got := state.preferredError(terminal)
	if !IsTerminalAuthError(got) || !errors.Is(
		got, upstream,
	) || statusCodeFromError(got) != http.StatusServiceUnavailable {
		t.Fatalf("terminal retry-round result=%T %v", got, got)
	}
}

func TestTerminalAuthenticationClassificationExcludesCooldownAndHealthyCandidates(t *testing.T) {
	now := time.Now()
	failed := &Auth{
		ID: "failed", Provider: "codex", Status: StatusError, Unavailable: true,
		LastError: &Error{Code: "unauthorized", HTTPStatus: http.StatusUnauthorized, Message: "refresh_token_reused"},
	}
	healthy := &Auth{ID: "healthy", Provider: "codex", Status: StatusActive}
	available, err := getAvailableAuths([]*Auth{failed, healthy}, "codex", "model", core.Options{}, now)
	if err != nil || len(available) != 1 || available[0].ID != healthy.ID {
		t.Fatalf("healthy selection=%+v, error=%v", available, err)
	}
	cooling := &Auth{
		ID: "cooling", Provider: "codex", Status: StatusError, Unavailable: true,
		Quota:     QuotaState{Exceeded: true, NextRecoverAt: now.Add(time.Minute)},
		LastError: &Error{HTTPStatus: http.StatusTooManyRequests},
	}
	_, err = getAvailableAuths([]*Auth{cooling}, "codex", "model", core.Options{}, now)
	if err == nil || IsTerminalAuthError(err) || statusCodeFromError(err) != http.StatusTooManyRequests {
		t.Fatalf("quota cooldown classification=%v", err)
	}
	_, err = getAvailableAuths([]*Auth{failed, cooling}, "codex", "model", core.Options{}, now)
	if err == nil || IsTerminalAuthError(err) || statusCodeFromError(err) != http.StatusServiceUnavailable {
		t.Fatalf("mixed temporary/permanent classification=%v", err)
	}
}
