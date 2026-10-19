package auth

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestUnversionedResultCannotCoolDownReplacementCredential(t *testing.T) {
	manager, original := newExecutionOutcomeTestManager(t, &Auth{
		ID: "unversioned-rotation", Provider: "provider-a", Status: StatusActive,
		Metadata: map[string]any{"access_token": "first"},
	})
	updatedAuth := original.Clone()
	updatedAuth.Metadata = map[string]any{"access_token": "second"}
	updated, err := manager.Update(WithSkipPersist(context.Background()), updatedAuth)
	if err != nil || updated == nil || updated.credentialGeneration == original.credentialGeneration {
		t.Fatalf("credential update = (%#v, %v)", updated, err)
	}
	stale := Result{AuthID: original.ID, Provider: original.Provider, Model: "model-a",
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "stale cooldown"}}
	manager.MarkResult(context.Background(), stale)
	current := getExecutionOutcomeTestAuth(t, manager, original.ID)
	if current.Failed != 0 || len(current.ModelStates) != 0 {
		t.Fatalf("unversioned result changed replacement: failed=%d models=%+v", current.Failed, current.ModelStates)
	}

	stale.AuthGeneration = updated.Generation
	manager.MarkResult(context.Background(), stale)
	current = getExecutionOutcomeTestAuth(t, manager, original.ID)
	if current.Failed != 1 || current.ModelStates["model-a"] == nil || !current.ModelStates["model-a"].Quota.Exceeded {
		t.Fatalf("versioned current result not applied: failed=%d models=%+v", current.Failed, current.ModelStates)
	}
}

func TestUnversionedResultCannotChangeRecreatedCredential(t *testing.T) {
	manager, original := newExecutionOutcomeTestManager(t, &Auth{
		ID: "unversioned-recreation", Provider: "provider-a", Status: StatusActive,
		Metadata: map[string]any{"access_token": "same-token"},
	})
	if err := manager.Remove(WithSkipPersist(context.Background()), original.ID); err != nil {
		t.Fatal(err)
	}
	recreated, err := manager.Register(WithAllowAuthRecreate(context.Background()), &Auth{
		ID: original.ID, Provider: original.Provider, Status: StatusActive,
		Metadata: map[string]any{"access_token": "same-token"},
	})
	if err != nil || recreated == nil {
		t.Fatalf("re-register = (%#v, %v)", recreated, err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID: original.ID, Provider: original.Provider, Model: "model-a",
		Error: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "stale cooldown"},
	})
	current := getExecutionOutcomeTestAuth(t, manager, original.ID)
	if current.Failed != 0 || len(current.ModelStates) != 0 {
		t.Fatalf("unversioned result changed recreated credential: %+v", current)
	}
}

func TestUnversionedNeutralResultCannotCountAgainstReplacement(t *testing.T) {
	manager, original := newExecutionOutcomeTestManager(t, &Auth{
		ID: "unversioned-neutral", Provider: "provider-a", Status: StatusActive,
		Metadata: map[string]any{"access_token": "first"},
	})
	updatedAuth := original.Clone()
	updatedAuth.Metadata = map[string]any{"access_token": "second"}
	if _, err := manager.Update(WithSkipPersist(context.Background()), updatedAuth); err != nil {
		t.Fatal(err)
	}
	manager.MarkResult(context.Background(), Result{
		AuthID: original.ID, Provider: original.Provider, Model: "model-a",
		Error: &Error{Message: "request stopped"},
		Disposition: cliproxyexecutor.ExecutionDisposition{
			DispatchPhase: cliproxyexecutor.DispatchPhaseNotSent,
			EffectScope:   cliproxyexecutor.StateEffectScopeRequest,
		},
	})
	if current := getExecutionOutcomeTestAuth(t, manager, original.ID); current.Failed != 0 {
		t.Fatalf("stale neutral result counted against new credential: %+v", current)
	}
}

func TestStaleAccessTokenResultCannotDisableRotatedToken(t *testing.T) {
	manager, original := newExecutionOutcomeTestManager(t, &Auth{
		ID: "rotated-access-token", Provider: "provider-a", Status: StatusActive,
		Metadata: map[string]any{
			"access_token": "access-a", "refresh_token": "refresh-owner",
		},
	})
	stale := executionResult(original, original.Provider, "model-a",
		executionOutcomeStatusError{status: http.StatusUnauthorized},
		cliproxyexecutor.DispatchPhaseMaybeSent)
	updatedAuth := original.Clone()
	updatedAuth.Metadata["access_token"] = "access-b"
	updated, err := manager.Update(WithSkipPersist(context.Background()), updatedAuth)
	if err != nil || updated == nil {
		t.Fatalf("Update() = (%#v, %v)", updated, err)
	}
	if updated.credentialGeneration != original.credentialGeneration {
		t.Fatal("token rotation unexpectedly changed the credential owner")
	}

	manager.MarkResult(context.Background(), stale)
	current := getExecutionOutcomeTestAuth(t, manager, original.ID)
	if current.Failed != 0 || current.Unavailable || current.LastError != nil {
		t.Fatalf("old token response changed the current credential: %+v", current)
	}
	manager.MarkResult(context.Background(), executionResult(updated, updated.Provider, "model-a",
		executionOutcomeStatusError{status: http.StatusUnauthorized},
		cliproxyexecutor.DispatchPhaseMaybeSent))
	if current = getExecutionOutcomeTestAuth(t, manager, original.ID); current.Failed != 1 {
		t.Fatalf("current token response was ignored: %+v", current)
	}
}

func TestLoadPreservesInFlightResultForSameCredential(t *testing.T) {
	store := &countingStore{list: []*Auth{{
		ID: "reload-same-binding", Provider: "provider-a", Status: StatusActive,
		Metadata: map[string]any{"access_token": "same-token"},
	}}}
	manager := NewManager(store, nil, nil)
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	original := getExecutionOutcomeTestAuth(t, manager, "reload-same-binding")
	if err := manager.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager.MarkResult(context.Background(), executionResult(
		original, original.Provider, "model-a",
		executionOutcomeStatusError{status: http.StatusTooManyRequests},
		cliproxyexecutor.DispatchPhaseMaybeSent,
	))
	if current := getExecutionOutcomeTestAuth(t, manager, original.ID); current.Failed != 1 {
		t.Fatalf("same-credential result was rejected after Load: %+v", current)
	}
}
