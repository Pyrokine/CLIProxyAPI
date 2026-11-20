package auth

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type auditContentRefreshExecutor struct {
	schedulerTestExecutor
	mutate func(context.Context, *Auth) error
}

func (e *auditContentRefreshExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	if e.mutate != nil {
		if err := e.mutate(ctx, auth); err != nil {
			return nil, err
		}
	}
	updated := auth.Clone()
	updated.Metadata["access_token"] = "synthetic-refreshed"
	updated.Metadata["expired"] = "2030-01-01T00:00:00Z"
	updated.Metadata["project_id"] = "discovered-proj-123"
	return updated, nil
}

type issue6417OriginalRefreshExecutor struct {
	auditLegacyVersionExecutor
	started chan struct{}
	release chan struct{}
}

func (e *issue6417OriginalRefreshExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	close(e.started)
	<-e.release
	auth.Metadata["access_token"] = "access-token-a-refreshed"
	auth.Metadata["refresh_token"] = "refresh-token-a-refreshed"
	auth.Metadata["expired"] = time.Now().Add(time.Hour).Format(time.RFC3339)
	return auth, nil
}

func TestOriginalIssue6417RefreshPreservesConcurrentReplacement(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			store := &auditLegacyVersionStore{}
			manager := NewManager(store, &RoundRobinSelector{}, nil)
			executor := &issue6417OriginalRefreshExecutor{
				id:      "antigravity",
				started: make(chan struct{}), release: make(chan struct{}),
			}
			manager.RegisterExecutor(executor)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(executor.release) }) }
			defer release()
			base, errRegister := manager.Register(
				context.Background(), &Auth{
					ID: "issue-6417-auth", Provider: "antigravity", Status: StatusActive,
					Metadata: map[string]any{
						"access_token": "access-token-a", "refresh_token": "refresh-token-a",
						"expired": time.Now().Add(-time.Hour).Format(time.RFC3339),
					},
				},
			)
			if errRegister != nil {
				t.Fatal(errRegister)
			}
			type result struct {
				auth *Auth
				err  error
			}
			refreshed, replaced := make(chan result, 1), make(chan result, 1)
			go func() {
				_, err := manager.ForceRefreshAuth(context.Background(), base.ID)
				auth, _ := manager.GetByID(base.ID)
				refreshed <- result{auth, err}
			}()
			<-executor.started
			current, found := manager.GetByID(base.ID)
			if !found {
				t.Fatal("credential disappeared during refresh")
			}
			current.Metadata["access_token"] = "access-token-b"
			current.Metadata["refresh_token"] = "refresh-token-b"
			current.Label = "concurrent-user-label"
			go func() {
				auth, err := manager.Update(context.Background(), current)
				replaced <- result{auth, err}
			}()
			synctest.Wait()
			replacedBeforeRefresh := len(replaced) != 0
			release()
			synctest.Wait()
			update, refresh := <-replaced, <-refreshed
			if update.err != nil || update.auth == nil || update.auth.Generation <= base.Generation {
				t.Fatalf("concurrent update = %+v, %v", update.auth, update.err)
			}
			if replacedBeforeRefresh && refresh.err == nil {
				t.Fatal("obsolete refresh lost its error")
			}
			if refresh.err != nil {
				if refresh.err.Error() != "credential refresh result is stale" || refresh.auth == nil ||
					refresh.auth.Metadata["access_token"] !=
						"access-token-b" ||
					refresh.auth.Metadata["refresh_token"] !=
						("refresh-token-"+
							"b") {
					t.Fatalf("obsolete refresh result = %+v, %v", refresh.auth, refresh.err)
				}
			} else if refresh.auth == nil {
				t.Fatal("credential snapshot disappeared after refresh")
			}
			final, found := manager.GetByID(base.ID)
			store.mu.Lock()
			persisted := store.records[base.ID].Clone()
			store.mu.Unlock()
			if !found || final.Generation != update.auth.Generation {
				t.Fatalf("final credential = %+v", final)
			}
			for _, auth := range []*Auth{final, persisted} {
				if auth == nil || auth.Metadata["access_token"] != "access-token-b" ||
					auth.Metadata["refresh_token"] != "refresh-token-b" || auth.Label != "concurrent-user-label" {
					t.Fatalf("concurrent credential content was overwritten: %+v", auth)
				}
			}
		},
	)
}

func TestAuditRefreshMergesConcurrentUserContent(t *testing.T) {
	for _, kind := range []string{"note", "proxy", "clear-proxy", "prefix-attributes", "disabled"} {
		t.Run(
			kind, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				executor := &auditContentRefreshExecutor{}
				manager.RegisterExecutor(executor)
				credential := &Auth{
					ID: "merge-auth", Provider: "test", Status: StatusActive, Prefix: "old-prefix",
					Attributes: map[string]string{"shared": "base"}, Metadata: map[string]any{
						"access_token": "synthetic-old", "refresh_token": "synthetic-owner", "note": "original-note",
					},
				}
				if kind == "clear-proxy" {
					credential.ProxyURL = "http://proxy.old:8080"
					credential.Metadata["proxy_url"] = credential.ProxyURL
				}
				registered, err := manager.Register(t.Context(), credential)
				if err != nil {
					t.Fatal(err)
				}
				executor.mutate = func(ctx context.Context, _ *Auth) error {
					current, _ := manager.GetByID(registered.ID)
					switch kind {
					case "note":
						current.Metadata["note"] = "concurrently-updated-note"
					case "proxy":
						current.ProxyURL = "http://127.0.0.1:8080"
						current.Metadata["proxy_url"] = current.ProxyURL
					case "clear-proxy":
						current.ProxyURL = ""
						delete(current.Metadata, "proxy_url")
					case "prefix-attributes":
						current.Prefix = "concurrent-prefix"
						current.Attributes["added_by_user"] = "custom"
					case "disabled":
						current.Disabled = true
						current.Status = StatusDisabled
					}
					_, err := manager.Update(ctx, current)
					return err
				}
				if _, err := manager.ForceRefreshAuth(t.Context(), registered.ID); err != nil {
					t.Fatalf("valid exchange lost to concurrent %s edit: %v", kind, err)
				}
				current, _ := manager.GetByID(registered.ID)
				if current.Metadata["access_token"] != "synthetic-refreshed" ||
					current.Metadata["project_id"] != "discovered-proj-123" {
					t.Fatalf("fresh exchange not committed: %#v", current.Metadata)
				}
				switch kind {
				case "note":
					if current.Metadata["note"] != "concurrently-updated-note" {
						t.Fatal("concurrent note was overwritten")
					}
				case "proxy":
					if current.ProxyURL != "http://127.0.0.1:8080" ||
						current.Metadata["proxy_url"] != current.ProxyURL {
						t.Fatal("concurrent proxy was overwritten")
					}
				case "clear-proxy":
					if _, exists := current.Metadata["proxy_url"]; current.ProxyURL != "" || exists {
						t.Fatal("cleared proxy was resurrected")
					}
				case "prefix-attributes":
					if current.Prefix != "concurrent-prefix" ||
						current.Attributes["added_by_user"] != "custom" {
						t.Fatal("concurrent prefix/attribute was overwritten")
					}
				case "disabled":
					if !current.Disabled || current.Status != StatusDisabled {
						t.Fatal("refresh enabled a concurrently disabled credential")
					}
				}
			},
		)
	}
}

func TestAuditReRegistrationPreservesIndependentQuota(t *testing.T) {
	for _, credentialScope := range []bool{false, true} {
		name := "model"
		if credentialScope {
			name = "credential"
		}
		t.Run(
			name, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				deadline := time.Now().Add(time.Hour)
				quota := QuotaState{
					Exceeded: true, Family: "independent", Reason: "operator quota", Generation: 7,
					ResetAuthority: StateResetAuthorityOperator, NextRecoverAt: deadline,
				}
				credential := &Auth{
					ID: "reregister-auth", Provider: "test", Status: StatusError, Unavailable: true,
					Metadata: map[string]any{
						"access_token":  "synthetic-access",
						"refresh_token": "synthetic-owner",
					},
				}
				if credentialScope {
					credential.Quota = quota
					credential.StateGeneration = 7
					credential.StateEffectScope = core.StateEffectScopeCredential
					credential.ResetAuthority = StateResetAuthorityOperator
				} else {
					credential.ModelStates = map[string]*ModelState{
						"gpt-5": {
							Status: StatusError, Unavailable: true, Generation: 7, Quota: quota,
							ResetAuthority: StateResetAuthorityOperator,
						},
					}
				}
				registered, err := manager.Register(t.Context(), credential)
				if err != nil {
					t.Fatal(err)
				}
				fresh := &Auth{
					ID: registered.ID, Provider: registered.Provider, Status: StatusActive,
					Metadata: map[string]any{
						"access_token":  "synthetic-access",
						"refresh_token": "synthetic-owner",
					},
				}
				current, err := manager.Register(t.Context(), fresh)
				if err != nil {
					t.Fatal(err)
				}
				if current.credentialGeneration == registered.credentialGeneration {
					t.Fatal("re-registration did not invalidate old execution snapshots")
				}
				var actual QuotaState
				if credentialScope {
					actual = current.Quota
				} else if state := current.ModelStates["gpt-5"]; state != nil {
					actual = state.Quota
				}
				if !actual.Exceeded || actual.Generation != 7 ||
					actual.ResetAuthority != StateResetAuthorityOperator ||
					!actual.NextRecoverAt.Equal(deadline) {
					t.Fatalf("independent quota lost across registration: %+v", actual)
				}
				if blocked, _, _ := isAuthBlockedForModel(current, "gpt-5", time.Now()); !blocked {
					t.Fatal("re-registration made independently limited credential selectable")
				}
			},
		)
	}
}
