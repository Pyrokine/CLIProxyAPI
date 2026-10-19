package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func routingAuthTestJWT(expiry time.Time, padded bool) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payloadJSON := []byte(fmt.Sprintf(`{"exp":%d}`, expiry.Unix()))
	payload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	if padded {
		payload = base64.URLEncoding.EncodeToString(payloadJSON)
	}
	return header + "." + payload + ".sig"
}

func TestAccessTokenExpirationSources(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(2 * time.Hour)
	past := now.Add(-time.Hour)

	for _, padded := range []bool{false, true} {
		t.Run(fmt.Sprintf("jwt-padded-%t", padded), func(t *testing.T) {
			auth := &Auth{Metadata: map[string]any{
				"access_token": routingAuthTestJWT(future, padded),
				"expired":      past.Format(time.RFC3339),
			}}
			expiresAt, ok := auth.AccessTokenExpirationTime()
			if !ok || expiresAt.Unix() != future.Unix() {
				t.Fatalf("AccessTokenExpirationTime() = (%v, %t), want %v", expiresAt, ok, future)
			}
			if !auth.HasValidAccessToken(now) {
				t.Fatal("future JWT access token reported invalid")
			}
		})
	}

	for _, test := range []struct {
		name string
		meta map[string]any
		want time.Time
	}{
		{
			name: "absolute metadata fallback",
			meta: map[string]any{"access_token": "opaque", "expires_at": future.Format(time.RFC3339)},
			want: future,
		},
		{
			name: "relative timestamp",
			meta: map[string]any{"access_token": "opaque", "expires_in": 3600, "timestamp": now.Unix()},
			want: now.Add(time.Hour),
		},
		{
			name: "relative nested token",
			meta: map[string]any{
				"access_token": "opaque",
				"token":        map[string]any{"expiresIn": "7200", "issuedAt": now.Format(time.RFC3339)},
			},
			want: now.Add(2 * time.Hour),
		},
		{
			name: "malformed JWT falls back",
			meta: map[string]any{"access_token": "bad.payload.token", "expired": future.UnixMilli()},
			want: future,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := &Auth{Metadata: test.meta}
			expiresAt, ok := auth.AccessTokenExpirationTime()
			if !ok || expiresAt.Unix() != test.want.Unix() {
				t.Fatalf("AccessTokenExpirationTime() = (%v, %t), want %v", expiresAt, ok, test.want)
			}
		})
	}

	expired := &Auth{Metadata: map[string]any{"access_token": routingAuthTestJWT(past, false)}}
	if expired.HasValidAccessToken(now) {
		t.Fatal("expired JWT access token reported valid")
	}
	missing := &Auth{Metadata: map[string]any{"expired": future.Format(time.RFC3339)}}
	if missing.HasValidAccessToken(now) {
		t.Fatal("missing access token reported valid")
	}
}

type routingRefreshTestExecutor struct {
	schedulerTestExecutor
	provider   string
	refreshErr error
}

func (e *routingRefreshTestExecutor) Identifier() string { return e.provider }

func (e *routingRefreshTestExecutor) Refresh(_ context.Context, _ *Auth) (*Auth, error) {
	return nil, e.refreshErr
}

func TestRefreshFailureKeepsValidTokenAndRemovesExpiredToken(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name               string
		expiry             time.Time
		refreshErr         error
		initialStatus      Status
		initialMessage     string
		initialUnavailable bool
		wantUnavailable    bool
		wantStatus         Status
		wantMessage        string
		wantRetry          bool
	}{
		{
			name:          "valid unauthorized token remains selectable",
			expiry:        now.Add(2 * time.Hour),
			refreshErr:    &Error{Code: "authentication_error", HTTPStatus: http.StatusUnauthorized, Message: "invalid_grant"},
			initialStatus: StatusActive,
			wantStatus:    StatusActive,
			wantRetry:     true,
		},
		{
			name:               "valid token preserves existing cooldown",
			expiry:             now.Add(2 * time.Hour),
			refreshErr:         errors.New("refresh service unavailable"),
			initialStatus:      StatusError,
			initialMessage:     "quota_exceeded",
			initialUnavailable: true,
			wantUnavailable:    true,
			wantStatus:         StatusError,
			wantMessage:        "quota_exceeded",
			wantRetry:          true,
		},
		{
			name:            "expired unauthorized token removed",
			expiry:          now.Add(-time.Hour),
			refreshErr:      &Error{Code: "authentication_error", HTTPStatus: http.StatusUnauthorized, Message: "invalid_grant"},
			initialStatus:   StatusActive,
			wantUnavailable: true,
			wantStatus:      StatusError,
			wantMessage:     "unauthorized",
		},
		{
			name:            "expired transient token removed with retry",
			expiry:          now.Add(-time.Hour),
			refreshErr:      errors.New("refresh service unavailable"),
			initialStatus:   StatusActive,
			wantUnavailable: true,
			wantStatus:      StatusError,
			wantMessage:     "token expired",
			wantRetry:       true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := "routing-refresh-" + fmt.Sprint(test.expiry.UnixNano()) + "-" + test.name
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			executor := &routingRefreshTestExecutor{provider: provider, refreshErr: test.refreshErr}
			manager.RegisterExecutor(executor)
			registered, errRegister := manager.Register(context.Background(), &Auth{
				ID:            "auth-" + test.name,
				Provider:      provider,
				Status:        test.initialStatus,
				StatusMessage: test.initialMessage,
				Unavailable:   test.initialUnavailable,
				Metadata: map[string]any{
					"access_token": "opaque-access-token",
					"expired":      test.expiry.Format(time.RFC3339Nano),
				},
			})
			if errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}

			manager.refreshAuth(context.Background(), registered.ID)
			updated, ok := manager.GetByID(registered.ID)
			if !ok || updated == nil {
				t.Fatal("auth disappeared after refresh failure")
			}
			if updated.Unavailable != test.wantUnavailable || updated.Status != test.wantStatus ||
				updated.StatusMessage != test.wantMessage {
				t.Fatalf("refresh failure state = unavailable:%t status:%q message:%q", updated.Unavailable, updated.Status, updated.StatusMessage)
			}
			if updated.LastError == nil {
				t.Fatal("refresh failure did not retain LastError")
			}
			if test.wantRetry != !updated.NextRefreshAfter.IsZero() {
				t.Fatalf("NextRefreshAfter = %v, want retry=%t", updated.NextRefreshAfter, test.wantRetry)
			}
			if test.expiry.After(now) && updated.NextRefreshAfter.After(test.expiry) {
				t.Fatalf("NextRefreshAfter %v exceeds access token expiry %v", updated.NextRefreshAfter, test.expiry)
			}
		})
	}
}

type rejectingRefreshGuard struct {
	calls int
}

func (*rejectingRefreshGuard) Current() bool { return true }

func (g *rejectingRefreshGuard) CommitRefresh(func() bool) bool {
	g.calls++
	return false
}

func TestRefreshFailurePublicationGuardsGenerationExecutorAndOwner(t *testing.T) {
	provider := "routing-refresh-publication"
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	executor := &routingRefreshTestExecutor{provider: provider}
	manager.RegisterExecutor(executor)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID:       "guarded-refresh-auth",
		Provider: provider,
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token": "valid-token",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	failure := errors.New("refresh failed")

	staleGeneration := registered.Clone()
	updatedInput := registered.Clone()
	updatedInput.Metadata["note"] = "new generation"
	updated, errUpdate := manager.Update(context.Background(), updatedInput)
	if errUpdate != nil {
		t.Fatalf("Update() error = %v", errUpdate)
	}
	if manager.publishRefreshError(staleGeneration, executor, nil, failure, time.Now(), false) {
		t.Fatal("stale generation published refresh failure")
	}
	current, _ := manager.GetByID(registered.ID)
	if current.LastError != nil {
		t.Fatalf("stale generation changed auth: %#v", current.LastError)
	}

	ownerExpected := updated.Clone()
	replacement := &routingRefreshTestExecutor{provider: provider}
	manager.RegisterExecutor(replacement)
	if manager.publishRefreshError(ownerExpected, executor, nil, failure, time.Now(), false) {
		t.Fatal("replaced executor owner published refresh failure")
	}
	current, _ = manager.GetByID(registered.ID)
	if current.LastError != nil {
		t.Fatalf("replaced executor changed auth: %#v", current.LastError)
	}

	guard := &rejectingRefreshGuard{}
	if manager.publishRefreshError(current.Clone(), replacement, guard, failure, time.Now(), false) {
		t.Fatal("rejected RefreshCommitGuard published refresh failure")
	}
	if guard.calls != 1 {
		t.Fatalf("RefreshCommitGuard calls = %d, want 1", guard.calls)
	}
	afterGuard, _ := manager.GetByID(registered.ID)
	if afterGuard.LastError != nil || afterGuard.Unavailable != current.Unavailable ||
		afterGuard.Status != current.Status {
		t.Fatalf("rejected guard changed auth: before=%#v after=%#v", current, afterGuard)
	}
}

func TestSchedulerDemotesExpiredAccessTokenAndRecoversAfterUpdate(t *testing.T) {
	const (
		provider = "routing-expiry-scheduler"
		model    = "routing-expiry-model"
		authID   = "routing-expiry-auth"
	)
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(&routingRefreshTestExecutor{provider: provider})
	registerSchedulerModels(t, provider, model, authID)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID:       authID,
		Provider: provider,
		Status:   StatusActive,
		Metadata: map[string]any{
			"access_token": "short-lived-token",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	picked, errPick := manager.scheduler.pickSingle(
		context.Background(), provider, model, cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != authID {
		t.Fatalf("pick before expiry = (%#v, %v)", picked, errPick)
	}

	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	manager.mu.Lock()
	manager.auths[authID].Metadata["expired"] = past
	manager.scheduler.mu.Lock()
	if providerState := manager.scheduler.providers[provider]; providerState != nil {
		if meta := providerState.auths[authID]; meta != nil && meta.auth != nil {
			meta.auth.Metadata["expired"] = past
		}
		for _, shard := range providerState.modelShards {
			if entry := shard.entries[authID]; entry != nil && entry.auth != nil {
				entry.auth.Metadata["expired"] = past
			}
		}
	}
	manager.scheduler.mu.Unlock()
	manager.mu.Unlock()

	if pickedExpired, errExpired := manager.scheduler.pickSingle(
		context.Background(), provider, model, cliproxyexecutor.Options{}, nil,
	); errExpired == nil || pickedExpired != nil {
		t.Fatalf("pick after expiry = (%#v, %v), want blocked", pickedExpired, errExpired)
	}

	refreshed := registered.Clone()
	refreshed.Metadata["access_token"] = "refreshed-token"
	refreshed.Metadata["expired"] = time.Now().Add(2 * time.Hour).Format(time.RFC3339)
	if _, errUpdate := manager.Update(context.Background(), refreshed); errUpdate != nil {
		t.Fatalf("Update() after refresh error = %v", errUpdate)
	}
	pickedAgain, errAgain := manager.scheduler.pickSingle(
		context.Background(), provider, model, cliproxyexecutor.Options{}, nil,
	)
	if errAgain != nil || pickedAgain == nil || pickedAgain.ID != authID {
		t.Fatalf("pick after token update = (%#v, %v)", pickedAgain, errAgain)
	}
}

func TestAutoRefreshCheckWaitIsCapped(t *testing.T) {
	if MaxAutoRefreshCheckInterval > 30*time.Second {
		t.Fatalf("MaxAutoRefreshCheckInterval = %v, want at most 30s", MaxAutoRefreshCheckInterval)
	}
}
