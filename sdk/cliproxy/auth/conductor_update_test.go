package auth

import (
	"context"
	"net/http"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestManager_SetConfigClearsHomeRuntimeAuthsWhenHomeDisabled(t *testing.T) {
	m := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:         "runtime-auth",
		Provider:   "codex",
		Attributes: map[string]string{"websockets": "true"},
	}

	m.rememberHomeRuntimeAuth("session-1", auth)
	if _, ok := m.GetExecutionSessionAuthByID("session-1", "runtime-auth"); !ok {
		t.Fatal("expected runtime auth to be cached")
	}

	m.SetConfig(&internalconfig.Config{})
	if _, ok := m.GetExecutionSessionAuthByID("session-1", "runtime-auth"); ok {
		t.Fatal("expected runtime auth cache to be cleared when Home is disabled")
	}
}

func TestManager_Update_PreservesModelStates(t *testing.T) {
	// The current Update method replaces the auth entry entirely (only preserving
	// Index/indexAssigned). ModelStates are not automatically preserved. This test
	// verifies the caller must include ModelStates in the update payload to retain them.
	m := NewManager(nil, nil, nil)

	model := "test-model"
	backoffLevel := 7

	if _, errRegister := m.Register(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "claude",
			Metadata: map[string]any{"k": "v"},
			ModelStates: map[string]*ModelState{
				model: {
					Quota: QuotaState{BackoffLevel: backoffLevel},
				},
			},
		},
	); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	// Update without ModelStates replaces the entry entirely
	if _, errUpdate := m.Update(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "claude",
			Metadata: map[string]any{"k": "v2"},
		},
	); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}

	// Current behavior: ModelStates are not preserved when omitted from update
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected ModelStates to be empty after update without them, got %+v", updated.ModelStates)
	}

	// Verify metadata was updated
	if v, ok := updated.Metadata["k"]; !ok || v != "v2" {
		t.Fatalf("expected metadata k=v2, got %v", updated.Metadata)
	}
}

func TestManager_SetConfigUpdatesAutoRefreshWorkers(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{AuthAutoRefreshWorkers: 3})

	m.mu.RLock()
	workers := cap(m.refreshSemaphore)
	m.mu.RUnlock()

	if workers != 3 {
		t.Fatalf("refresh semaphore capacity = %d, want 3", workers)
	}
}

func TestManager_RefreshAuthUnauthorizedStopsAutoRefreshRetry(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(unauthorizedRefreshExecutor{})
	_, err := m.Register(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "unauthorized-refresh",
			Metadata: map[string]any{"token": "old"},
		},
	)
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	m.refreshAuth(context.Background(), "auth-1")
	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatal("expected auth to be present")
	}
	if updated.LastError == nil || updated.LastError.Code != "unauthorized" {
		t.Fatalf("expected unauthorized refresh error, got %+v", updated.LastError)
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("expected unauthorized refresh to stop retries, got %s", updated.NextRefreshAfter)
	}
	if m.shouldRefresh(updated, updated.UpdatedAt) {
		t.Fatal("expected unauthorized refresh failure to disable auto-refresh retry")
	}
}

type unauthorizedRefreshExecutor struct{ schedulerTestExecutor }

func (unauthorizedRefreshExecutor) Identifier() string { return "unauthorized-refresh" }

func (unauthorizedRefreshExecutor) Refresh(_ context.Context, _ *Auth) (*Auth, error) {
	return nil, &Error{Code: "authentication_error", Message: "401 unauthorized", HTTPStatus: http.StatusUnauthorized}
}

func TestManager_Update_WithModelStatesPreservesModelStates(t *testing.T) {
	m := NewManager(nil, nil, nil)

	model := "test-model"
	backoffLevel := 7

	if _, errRegister := m.Register(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "claude",
			ModelStates: map[string]*ModelState{
				model: {
					Quota: QuotaState{BackoffLevel: backoffLevel},
				},
			},
		},
	); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	// Update with ModelStates included preserves them
	if _, errUpdate := m.Update(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "claude",
			Metadata: map[string]any{"k": "v2"},
			ModelStates: map[string]*ModelState{
				model: {
					Quota: QuotaState{BackoffLevel: backoffLevel},
				},
			},
		},
	); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	if len(updated.ModelStates) == 0 {
		t.Fatalf("expected ModelStates to be preserved")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if state.Quota.BackoffLevel != backoffLevel {
		t.Fatalf("expected BackoffLevel to be %d, got %d", backoffLevel, state.Quota.BackoffLevel)
	}
}
