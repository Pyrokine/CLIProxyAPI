package auth

import (
	"context"
	"testing"
)

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
