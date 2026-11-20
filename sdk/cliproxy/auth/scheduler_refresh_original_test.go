package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalSchedulerRefreshFailureInputs(t *testing.T) {
	for _, sample := range []struct {
		id, email, token, message string
		expiry                    time.Duration
		unavailable               bool
		transient                 bool
	}{
		{id: "unauthorized-refresh", email: "x@example.com"},
		{
			id: "unauthorized-refresh-valid-token", email: "active@example.com", token: "valid-future-access-token",
			expiry: 48 * time.Hour,
		},
		{
			id: "unauthorized-refresh-preexisting-unavailable", email: "quota@example.com",
			token: "valid-future-access-token", expiry: 48 * time.Hour,
			unavailable: true, message: "quota_exceeded",
		},
		{
			id: "transient-refresh-expired-token", email: "expired@example.com", token: "expired-access-token",
			expiry: -24 * time.Hour, transient: true,
		},
	} {
		t.Run(
			sample.id, func(t *testing.T) {
				failure := errors.New("token refresh failed with status 401: invalid_grant")
				if sample.transient {
					failure = errors.New("upstream 503 service unavailable")
				}
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				manager.RegisterExecutor(&routingRefreshTestExecutor{provider: "codex", refreshErr: failure})
				credential := &Auth{
					ID: sample.id, Provider: "codex", Status: StatusActive,
					Unavailable: sample.unavailable, StatusMessage: sample.message,
					Metadata: map[string]any{"email": sample.email},
				}
				if sample.token != "" {
					credential.Metadata["access_token"] = sample.token
					credential.Metadata["expired"] = time.Now().Add(sample.expiry).Format(time.RFC3339)
				}
				if sample.unavailable {
					credential.Status = StatusError
				}
				registered, err := manager.Register(t.Context(), credential)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := manager.ForceRefreshAuth(t.Context(), registered.ID); !errors.Is(err, failure) {
					t.Fatalf("refresh error=%v, want original", err)
				}
				current, found := manager.GetByID(registered.ID)
				if !found || current.LastError == nil || current.LastError.Message != failure.Error() {
					t.Fatalf("refresh failure state=%+v", current)
				}
				valid := sample.expiry > 0
				if current.Unavailable != (sample.unavailable || !valid) ||
					(current.Status == StatusError) != (sample.unavailable || !valid) ||
					!current.NextRefreshAfter.IsZero() != (valid || sample.transient) {
					t.Fatalf(
						"availability=%t status=%s retry=%v", current.Unavailable, current.Status,
						current.NextRefreshAfter,
					)
				}
				if sample.message != "" && current.StatusMessage != sample.message {
					t.Fatalf("status message=%q, want %q", current.StatusMessage, sample.message)
				}
				if !valid && !sample.transient &&
					(current.LastError.StatusCode() !=
						401 ||
						current.LastError.Code !=
							"unauthorized" ||
						manager.shouldRefresh(
							current, time.Now(),
						)) {
					t.Fatalf("unauthorized refresh remained eligible: %+v", current)
				}
			},
		)
	}
}

func TestOriginalSchedulerTerminalSelectionInputs(t *testing.T) {
	for _, sample := range []struct {
		name     string
		selector Selector
	}{
		{name: "fast_path_round_robin", selector: &RoundRobinSelector{}},
		{name: "legacy_path_nil_selector"},
	} {
		for _, staleModel := range []bool{false, true} {
			name := sample.name + "/plain"
			prefix := "unauthorized-terminal-"
			if staleModel {
				name, prefix = sample.name+"/historical_model_error", "unauthorized-stale-model-"
			}
			t.Run(
				name, func(t *testing.T) {
					manager := NewManager(nil, sample.selector, nil)
					manager.SetHomeClientProvider(func() HomeClient { return nil })
					manager.RegisterExecutor(
						&routingRefreshTestExecutor{
							provider:   "codex",
							refreshErr: errors.New("token refresh failed with status 401: invalid_grant"),
						},
					)
					credential := &Auth{
						ID: prefix + sample.name, Provider: "codex", Metadata: map[string]any{"email": "x@example.com"},
					}
					if staleModel {
						credential.ModelStates = map[string]*ModelState{
							"any-model": {
								LastError: &Error{
									Code: "rate_limit_exceeded", Message: "historical rate limit", HTTPStatus: 429,
								},
								UpdatedAt: time.Now().Add(-10 * time.Minute),
							},
						}
					}
					registerRequestScopedTestAuth(t, manager, credential, "any-model")
					if _, err := manager.ForceRefreshAuth(t.Context(), credential.ID); err == nil {
						t.Fatal("missing original unauthorized refresh error")
					}
					_, err := manager.SelectAuth(t.Context(), "codex", "any-model", core.Options{})
					if err == nil || !IsTerminalAuthError(err) ||
						statusCodeFromError(err) != http.StatusServiceUnavailable {
						t.Fatalf("terminal selection=%T/%v", err, err)
					}
					var classified *Error
					if !errors.As(err, &classified) || classified.Retryable || strings.Contains(
						err.Error(), "historical rate limit",
					) {
						t.Fatalf("terminal classification=%+v error=%v", classified, err)
					}
					if !staleModel {
						healthy := &Auth{ID: "healthy-" + sample.name, Provider: "codex", Status: StatusActive}
						registerRequestScopedTestAuth(t, manager, healthy, "any-model")
						manager.RefreshSchedulerEntry(healthy.ID)
						selected, err := manager.SelectAuth(t.Context(), "codex", "any-model", core.Options{})
						if err != nil || selected == nil || selected.ID != healthy.ID {
							t.Fatalf("healthy selection=%+v error=%v", selected, err)
						}
					}
				},
			)
		}
	}
	t.Run(
		"quota-cooldown-auth", func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.SetHomeClientProvider(func() HomeClient { return nil })
			manager.RegisterExecutor(&routingRefreshTestExecutor{provider: "codex"})
			credential := &Auth{
				ID: "quota-cooldown-auth", Provider: "codex", Unavailable: true,
				Quota: QuotaState{
					Exceeded: true, Reason: "credential_quota", NextRecoverAt: time.Now().Add(time.Minute),
				},
			}
			registerRequestScopedTestAuth(t, manager, credential, "model-cooldown")
			if _, err := manager.SelectAuth(
				t.Context(), "codex", "model-cooldown", core.Options{},
			); err == nil || IsTerminalAuthError(err) {
				t.Fatalf("quota selection error=%T/%v", err, err)
			}
		},
	)
}

func TestOriginalSchedulerRefreshModelRegistrationInputs(t *testing.T) {
	for _, operation := range []string{"register", "update"} {
		t.Run(
			operation, func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				credential := &Auth{ID: "refresh-entry-" + operation, Provider: "gemini"}
				registered, err := manager.Register(t.Context(), credential)
				if err != nil {
					t.Fatal(err)
				}
				if operation == "update" {
					registered.Metadata = map[string]any{"updated": true}
					if _, err := manager.Update(t.Context(), registered); err != nil {
						t.Fatal(err)
					}
				}
				registerReservationContextModels(t, "gemini", "scheduler-refresh-model", credential.ID)
				manager.RefreshSchedulerEntry(credential.ID)
				selected, err := manager.scheduler.pickSingle(
					t.Context(), "gemini", "scheduler-refresh-model", core.Options{}, nil,
				)
				if err != nil || selected == nil || selected.ID != credential.ID {
					t.Fatalf("registered model selection=%+v error=%v", selected, err)
				}
				registry.GetGlobalRegistry().UnregisterClient(credential.ID)
				manager.RefreshSchedulerEntry(credential.ID)
				if selected, err := manager.scheduler.pickSingle(
					t.Context(), "gemini", "scheduler-refresh-model", core.Options{}, nil,
				); err == nil || selected != nil {
					t.Fatalf("removed model stayed selectable: auth=%+v error=%v", selected, err)
				}
			},
		)
	}
	t.Run(
		"scheduler-cooldown-rebuild-model", func(t *testing.T) {
			const model = "scheduler-cooldown-rebuild-model"
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.SetHomeClientProvider(func() HomeClient { return nil })
			manager.RegisterExecutor(&routingRefreshTestExecutor{provider: "gemini"})
			old, err := manager.Register(t.Context(), &Auth{ID: "cooldown-stale-old", Provider: "gemini"})
			if err != nil {
				t.Fatal(err)
			}
			registerReservationContextModels(t, "gemini", model, old.ID)
			manager.MarkResult(
				t.Context(), Result{
					AuthID: old.ID, AuthGeneration: old.Generation,
					Provider: "gemini", Model: model, Error: &Error{HTTPStatus: 429, Message: "quota"},
				},
			)
			current, err := manager.Register(t.Context(), &Auth{ID: "cooldown-stale-new", Provider: "gemini"})
			if err != nil {
				t.Fatal(err)
			}
			registerReservationContextModels(t, "gemini", model, current.ID)
			selected, err := manager.SelectAuth(t.Context(), "gemini", model, core.Options{})
			if err != nil || selected == nil || selected.ID != current.ID {
				t.Fatalf("fresh model registration=%+v error=%v", selected, err)
			}
		},
	)
}

func TestOriginalSchedulerAccessTokenExpirationInputs(t *testing.T) {
	t.Run(
		"expired-token-blocked", func(t *testing.T) {
			credential := &Auth{
				ID: "expired-token-blocked", Provider: "codex", Status: StatusActive,
				Metadata: map[string]any{
					"email": "user@example.com", "access_token": "expired-token",
					"expired": time.Now().Add(-time.Hour).Format(time.RFC3339),
				},
			}
			if blocked, _, _ := isAuthBlockedForModel(credential, "gpt-5", time.Now()); !blocked {
				t.Fatal("expired original token remained selectable")
			}
		},
	)
	t.Run(
		"short-lived-auth", func(t *testing.T) {
			synctest.Test(
				t, func(t *testing.T) {
					manager := NewManager(nil, &RoundRobinSelector{}, nil)
					manager.RegisterExecutor(&routingRefreshTestExecutor{provider: "codex"})
					credential := &Auth{
						ID: "short-lived-auth", Provider: "codex", Status: StatusActive,
						Metadata: map[string]any{
							"email": "short@example.com", "access_token": "short-lived-access-token",
							"expired": time.Now().Add(10 * time.Minute).Format(time.RFC3339),
						},
					}
					registerRequestScopedTestAuth(t, manager, credential, "gpt-5-short-test")
					selected, err := manager.scheduler.pickSingle(
						t.Context(), "codex", "gpt-5-short-test", core.Options{}, nil,
					)
					if err != nil || selected == nil || selected.ID != credential.ID {
						t.Fatalf("unexpired selection=%+v error=%v", selected, err)
					}
					time.Sleep(11 * time.Minute)
					selected, err = manager.scheduler.pickSingle(
						t.Context(), "codex", "gpt-5-short-test", core.Options{}, nil,
					)
					if err == nil || selected != nil {
						t.Fatalf("expired immutable snapshot selected=%+v error=%v", selected, err)
					}
				},
			)
		},
	)
}

func TestOriginalSchedulerSkipPersistenceRejectsOlderGeneration(t *testing.T) {
	store := &auditLegacyUpdateStore{}
	manager := NewManager(store, &RoundRobinSelector{}, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	base, err := manager.Register(
		t.Context(), &Auth{
			ID: "watermark-auth", Provider: "antigravity", Status: StatusActive,
			Metadata: map[string]any{
				"type": "antigravity", "access_token": "gen1-token", "proxy_url": "http://initial.proxy",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	stale := base.Clone()
	stale.Metadata["stale_sentinel"] = "must_not_persist"
	watcher := stale.Clone()
	delete(watcher.Metadata, "stale_sentinel")
	watcher.ProxyURL = "http://user-disk-edit.proxy"
	watcher.Metadata["proxy_url"] = watcher.ProxyURL
	current, err := manager.Update(WithSkipPersist(t.Context()), watcher)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	result, err := manager.MutatePersistentAuth(
		WithExpectedAuthGeneration(t.Context(), stale.Generation), stale.ID,
		func(context.Context) (*Auth, error) {
			called = true
			return stale, nil
		},
	)
	if called || result != nil || !errors.Is(err, ErrAuthGenerationConflict) {
		t.Fatalf("old generation reached storage=%t result=%+v error=%v", called, result, err)
	}
	if store.saved == nil || store.saved.Metadata["stale_sentinel"] != nil {
		t.Fatalf("old sentinel was saved: %+v", store.saved)
	}
	current.Metadata["access_token"] = "gen3-token"
	if _, err := manager.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if store.saved.Metadata["access_token"] != "gen3-token" ||
		store.saved.Metadata["proxy_url"] != "http://user-disk-edit.proxy" {
		t.Fatalf("new version did not preserve watcher fields: %+v", store.saved)
	}
}
