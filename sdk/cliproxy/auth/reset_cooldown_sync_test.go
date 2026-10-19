package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestResetCooldownClearsTimedNonQuotaRoutingStateWithoutChangingQuotaReset(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	auth := &Auth{
		ID: "temporary-provider-failure", Provider: "codex", Status: StatusError,
		Unavailable: true, NextRetryAfter: next,
		StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
		LastError:        &Error{HTTPStatus: http.StatusServiceUnavailable},
		ModelStates: map[string]*ModelState{
			"model-a": {
				Status: StatusError, Unavailable: true, NextRetryAfter: next,
				LastError: &Error{HTTPStatus: http.StatusServiceUnavailable},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	if views := CooldownSnapshotForAuth(auth, time.Now()); len(views) != 2 {
		t.Fatalf("visible cooldowns = %+v, want credential and model", views)
	}
	if _, models, err := manager.ResetQuota(context.Background(), auth.ID); err != nil || len(models) != 0 {
		t.Fatalf("quota-only reset = %v, %v", models, err)
	}
	if unchanged, ok := manager.GetByID(auth.ID); !ok || !unchanged.Unavailable || !unchanged.ModelStates["model-a"].Unavailable {
		t.Fatalf("quota-only reset changed non-quota state: %+v", unchanged)
	}
	updated, models, err := manager.ResetCooldown(context.Background(), auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated == nil || len(models) != 1 || models[0] != "model-a" {
		t.Fatalf("reset result = %+v, %v", updated, models)
	}
	if updated.Unavailable || updated.Status != StatusActive || !updated.NextRetryAfter.IsZero() ||
		updated.ModelStates["model-a"].Unavailable || updated.ModelStates["model-a"].Status != StatusActive ||
		!updated.ModelStates["model-a"].NextRetryAfter.IsZero() {
		t.Fatalf("temporary cooldown remains: %+v", updated)
	}
	if views := CooldownSnapshotForAuth(updated, time.Now()); len(views) != 0 {
		t.Fatalf("visible cooldowns after reset = %+v", views)
	}
}

func TestResetCooldownClearsCredentialTimerWithPermanentModelFailure(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	auth := &Auth{
		ID: "credential-and-permanent-model", Provider: "codex", Status: StatusError,
		Unavailable: true, NextRetryAfter: next,
		StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
		LastError:        &Error{HTTPStatus: http.StatusServiceUnavailable},
		ModelStates: map[string]*ModelState{
			"permanent": {
				Status: StatusError, Unavailable: true,
				LastError: &Error{HTTPStatus: http.StatusUnauthorized},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	updated, _, err := manager.ResetCooldown(context.Background(), auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.NextRetryAfter.After(time.Now()) || updated.StateEffectScope == cliproxyexecutor.StateEffectScopeCredential {
		t.Fatalf("credential retry timer remains: %+v", updated)
	}
	if !updated.ModelStates["permanent"].Unavailable || updated.ModelStates["permanent"].LastError == nil {
		t.Fatalf("permanent model failure was cleared: %+v", updated.ModelStates["permanent"])
	}
}

func TestResetCooldownDoesNotEnableDisabledCredentialOrModel(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	auth := &Auth{
		ID: "disabled-cooldown", Provider: "codex", Status: StatusDisabled,
		Disabled: true, Unavailable: true, NextRetryAfter: next,
		StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
		ModelStates: map[string]*ModelState{
			"disabled-model": {Status: StatusDisabled, Unavailable: true, NextRetryAfter: next},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	updated, models, err := manager.ResetCooldown(context.Background(), auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Disabled || updated.Status != StatusDisabled || updated.ModelStates["disabled-model"].Status != StatusDisabled ||
		len(models) != 0 {
		t.Fatalf("disabled routing state changed: %+v, %v", updated, models)
	}
}

func TestResetCooldownPreservesPermanentModelFailure(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	auth := &Auth{
		ID: "mixed-blocks", Provider: "codex", Status: StatusError,
		ModelStates: map[string]*ModelState{
			"timed": {
				Status: StatusError, Unavailable: true, NextRetryAfter: next,
				LastError: &Error{HTTPStatus: http.StatusServiceUnavailable},
			},
			"permanent": {
				Status: StatusError, Unavailable: true,
				LastError: &Error{HTTPStatus: http.StatusUnauthorized},
			},
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	updated, models, err := manager.ResetCooldown(context.Background(), auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0] != "timed" || updated.ModelStates["timed"].Unavailable ||
		!updated.ModelStates["permanent"].Unavailable || updated.ModelStates["permanent"].LastError == nil {
		t.Fatalf("reset changed a permanent failure: %+v, %v", updated, models)
	}
}
