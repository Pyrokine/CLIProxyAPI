package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalSchedulerRefreshConcurrentInputs(t *testing.T) {
	for _, sample := range []struct {
		name, id, token, refreshed, proxy, userProxy, executorProxy string
		inPlace, replacement, recover, modelQuota, failure          bool
		typedRecovery                                               bool
	}{
		{
			name: "proxy and metadata", id: "test-antigravity", token: "old-access-token",
			refreshed: "refreshed-access-token", userProxy: "http://127.0.0.1:7890",
		},
		{
			name: "in-place metadata", id: "inplace-auth", token: "old-token", refreshed: "new-access-token",
			userProxy: "http://10.0.0.1:8888", inPlace: true,
		},
		{
			name: "cleared proxy", id: "clear-proxy-auth", token: "old-token", refreshed: "refreshed-access-token",
			proxy: "http://initial-proxy.local:8080",
		},
		{
			name: "stale registration", id: "re-register-auth", token: "original-token",
			refreshed: "refreshed-access-token", replacement: true,
		},
		{
			name: "error recovery", id: "error-status-auth", token: "expired-token",
			refreshed: "refreshed-access-token", recover: true,
		},
		{
			name: "proxy conflict", id: "conflict-proxy-auth", token: "old-token", refreshed: "refreshed-token",
			proxy:     "http://base.proxy:8080",
			userProxy: "http://user-preferred.proxy:9090", executorProxy: "http://executor-returned.proxy:8080",
			recover: true,
		},
		{
			name: "authorized error recovery", id: "error-status-auth", token: "expired-token",
			refreshed: "refreshed-access-token", recover: true, typedRecovery: true,
		},
		{
			name: "authorized proxy conflict", id: "conflict-proxy-auth", token: "old-token",
			refreshed: "refreshed-token", proxy: "http://base.proxy:8080",
			userProxy: "http://user-preferred.proxy:9090", executorProxy: "http://executor-returned.proxy:8080",
			recover: true, typedRecovery: true,
		},
		{
			name: "model quota", id: "model-cooldown-auth", token: "old-token", refreshed: "refreshed-access-token",
			modelQuota: true,
		},
		{
			name: "concurrent failure cooldown", id: "concurrent-refresh-auth", token: "valid-future-access-token",
			failure: true,
		},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				store := &auditLegacyVersionStore{}
				manager := NewManager(store, &RoundRobinSelector{}, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				provider := "antigravity"
				if sample.failure {
					provider = "codex"
				}
				started, proceed := make(chan struct{}), make(chan struct{})
				release := sync.OnceFunc(func() { close(proceed) })
				defer release()
				failure := errors.New("token refresh failed with status 401: invalid_grant")
				executor := &originalLifecycleExecutor{
					id: provider,
					mutate: func(_ context.Context, selected *Auth) (*Auth, error) {
						close(started)
						<-proceed
						if sample.failure {
							return nil, failure
						}
						selected.Metadata["access_token"] = sample.refreshed
						selected.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
						if sample.inPlace {
							selected.Metadata["account_id"] = "acc-456"
							selected.Metadata["project_id"] = "proj-789"
						}
						if sample.executorProxy != "" {
							selected.ProxyURL = sample.executorProxy
							selected.Metadata["proxy_url"] = sample.executorProxy
							selected.Status = ""
						}
						return selected, nil
					},
				}
				manager.RegisterExecutor(executor)
				credential := &Auth{
					ID: sample.id, Provider: provider, Status: StatusActive, ProxyURL: sample.proxy,
					Metadata: map[string]any{
						"type": provider, "access_token": sample.token,
						"expired": time.Now().Add(-time.Hour).Format(time.RFC3339),
					},
				}
				if sample.proxy != "" {
					credential.Metadata["proxy_url"] = sample.proxy
				}
				if sample.failure {
					credential.Metadata["email"] = "active@example.com"
					credential.Metadata["expired"] = time.Now().Add(48 * time.Hour).Format(time.RFC3339)
				}
				if sample.recover {
					credential.Status = StatusError
					if sample.id == "error-status-auth" {
						credential.Unavailable = true
						credential.StatusMessage = "token expired"
						credential.LastError = &Error{Message: "token expired"}
					}
				}
				if sample.typedRecovery {
					credential.LastError = errorWithState(
						&Error{
							HTTPStatus: http.StatusUnauthorized, Message: "token expired",
						}, errorState{EffectScope: core.StateEffectScopeCredential, EffectKey: credential.ID},
					)
					credential.StateEffectScope = core.StateEffectScopeCredential
					credential.StateEffectKey = credential.ID
					credential.ResetAuthority = StateResetAuthoritySuccess
				}
				base, err := manager.Register(t.Context(), credential)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				var refreshErr error
				go func() {
					defer close(done)
					_, refreshErr = manager.ForceRefreshAuth(t.Context(), base.ID)
				}()
				defer func() { release(); <-done }()
				select {
				case <-started:
				case <-done:
					t.Fatalf("refresh stopped before exchange: %v", refreshErr)
				case <-time.After(5 * time.Second):
					t.Fatal("original refresh did not start")
				}
				var concurrentCooldown *Auth
				quotaWait := 10 * time.Minute
				switch {
				case sample.replacement:
					if err := manager.Remove(t.Context(), base.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := manager.Register(
						WithAllowAuthRecreate(t.Context()), &Auth{
							ID: base.ID, Provider: provider, Status: StatusActive,
							Metadata: map[string]any{
								"type": provider, "access_token": "freshly-registered-different-token",
								"expired": time.Now().Add(48 * time.Hour).Format(time.RFC3339),
							},
						},
					); err != nil {
						t.Fatal(err)
					}
				case sample.failure:
					markOriginalCooldownResult(
						t, manager, Result{
							AuthID: base.ID, Provider: provider,
							Error: errorWithState(
								&Error{HTTPStatus: 503, Message: "cooling_503"}, errorState{
									DispatchPhase:  core.DispatchPhaseNotSent,
									Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
									RetryDirective: core.RetryDirectiveNoRetry,
									EffectScope:    core.StateEffectScopeCredential, EffectKey: base.ID,
								},
							),
						},
					)
					concurrentCooldown, _ = manager.GetByID(base.ID)
				case sample.modelQuota:
					markOriginalCooldownResult(
						t, manager, Result{
							AuthID: base.ID, Provider: provider, Model: "gemini-pro", RetryAfter: &quotaWait,
							Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exceeded"},
						},
					)
				case !sample.recover:
					current, _ := manager.GetByID(base.ID)
					current.ProxyURL = sample.userProxy
					if sample.userProxy == "" {
						delete(current.Metadata, "proxy_url")
					} else {
						current.Metadata["proxy_url"] = sample.userProxy
					}
					if sample.id == "test-antigravity" {
						current.Metadata["custom_field"] = "custom_value"
					}
					if _, err := manager.Update(t.Context(), current); err != nil {
						t.Fatal(err)
					}
				case sample.executorProxy != "":
					current, _ := manager.GetByID(base.ID)
					current.ProxyURL = sample.userProxy
					current.Metadata["proxy_url"] = sample.userProxy
					if _, err := manager.Update(t.Context(), current); err != nil {
						t.Fatal(err)
					}
				}
				release()
				<-done
				err = refreshErr
				current, found := manager.GetByID(base.ID)
				if !found {
					t.Fatal("credential disappeared after original refresh")
				}
				if sample.replacement {
					if err == nil || current.Metadata["access_token"] != "freshly-registered-different-token" ||
						current.Generation <= base.Generation {
						t.Fatalf("stale registration was accepted: current=%+v error=%v", current, err)
					}
					return
				}
				if sample.failure {
					if !errors.Is(err, failure) || concurrentCooldown == nil || !current.Unavailable ||
						current.StatusMessage !=
							concurrentCooldown.StatusMessage ||
						!current.NextRetryAfter.Equal(concurrentCooldown.NextRetryAfter) ||
						current.StateEffectScope !=
							concurrentCooldown.StateEffectScope ||
						current.ResetAuthority !=
							concurrentCooldown.ResetAuthority {
						t.Fatalf("concurrent credential cooldown changed: current=%+v error=%v", current, err)
					}
					return
				}
				if err != nil || current.Metadata["access_token"] != sample.refreshed {
					t.Fatalf("original refreshed token=%v error=%v", current.Metadata["access_token"], err)
				}
				if sample.modelQuota {
					state := current.ModelStates["gemini-pro"]
					if state == nil || !state.Unavailable || state.LastError == nil ||
						state.LastError.Message != "quota exceeded" ||
						!state.Quota.Exceeded ||
						time.Until(state.NextRetryAfter) < 9*time.Minute {
						t.Fatalf("independent model quota=%+v", state)
					}
				} else if current.ProxyURL !=
					sample.userProxy ||
					(sample.userProxy !=
						"" &&
						current.Metadata["proxy_url"] !=
							sample.userProxy) ||
					(sample.userProxy == "" && current.Metadata["proxy_url"] != nil) {
					t.Fatalf(
						"concurrent proxy=%q metadata=%v, want %q", current.ProxyURL, current.Metadata["proxy_url"],
						sample.userProxy,
					)
				}
				if sample.inPlace &&
					(current.Metadata["account_id"] != "acc-456" || current.Metadata["project_id"] != "proj-789") {
					t.Fatalf("in-place provider fields=%v", current.Metadata)
				}
				if sample.id == "test-antigravity" && current.Metadata["custom_field"] != "custom_value" {
					t.Fatal("original concurrent custom field was lost")
				}
				if sample.recover {
					wantStatus := StatusError
					if sample.typedRecovery {
						wantStatus = StatusActive
					}
					if current.Status != wantStatus || current.LastRefreshedAt.IsZero() ||
						(sample.typedRecovery && (current.Unavailable || current.LastError != nil)) {
						t.Fatalf("refresh recovery authority changed: %+v", current)
					}
					if !sample.typedRecovery && sample.id == "error-status-auth" &&
						(!current.Unavailable ||
							current.LastError ==
								nil ||
							current.LastError.Message !=
								"token expired") {
						t.Fatalf("refresh cleared an unclassified legacy restriction: %+v", current)
					}
				}
				store.mu.Lock()
				persisted := store.records[base.ID].Clone()
				store.mu.Unlock()
				if persisted == nil || persisted.Metadata["access_token"] != sample.refreshed ||
					persisted.ProxyURL != current.ProxyURL ||
					persisted.Metadata["proxy_url"] != current.Metadata["proxy_url"] {
					t.Fatalf("saved original provider fields=%+v", persisted)
				}
			},
		)
	}
}

func TestOriginalSchedulerPrepareKeepsConcurrentToken(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	started, proceed := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(proceed) })
	defer release()
	var calls atomic.Int32
	preparer := &originalLifecycleExecutor{
		id: "antigravity",
		mutate: func(_ context.Context, selected *Auth) (*Auth, error) {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-proceed
			selected.Metadata["project_id"] = "discovered-project-id"
			return selected, nil
		},
	}
	manager.RegisterExecutor(preparer)
	base, err := manager.Register(
		t.Context(), &Auth{
			ID: "prepare-auth", Provider: "antigravity", Status: StatusActive,
			Metadata: map[string]any{
				"type": "antigravity", "access_token": "original-token",
				"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var prepareErr error
	go func() {
		defer close(done)
		_, prepareErr = manager.PrepareRequestAuth(t.Context(), preparer, base)
	}()
	defer func() { release(); <-done }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("original preparation did not start")
	}
	current, _ := manager.GetByID(base.ID)
	current.Metadata["access_token"] = "brand-new-refreshed-token"
	if _, err := manager.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	release()
	<-done
	err = prepareErr
	current, _ = manager.GetByID(base.ID)
	if err != nil || current.Metadata["access_token"] != "brand-new-refreshed-token" ||
		current.Metadata["project_id"] != "discovered-project-id" ||
		calls.Load() != 2 {
		t.Fatalf("prepared current=%+v error=%v calls=%d", current, err, calls.Load())
	}
}
