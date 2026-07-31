package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type requestRefreshEvaluatorExecutor struct {
	schedulerTestExecutor
	refreshCalls atomic.Int32
}

func (*requestRefreshEvaluatorExecutor) Identifier() string { return "request-refresh" }

func (*requestRefreshEvaluatorExecutor) ShouldRefresh(_ time.Time, auth *Auth) bool {
	return auth != nil && auth.Metadata["access_token"] == "old"
}

func (e *requestRefreshEvaluatorExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	e.refreshCalls.Add(1)
	updated := auth.Clone()
	updated.Metadata["access_token"] = "new"
	return updated, nil
}

func TestPrepareRequestAuthRefreshesManagedCredentialBeforeExecution(t *testing.T) {
	store := &countingStore{}
	manager := NewManager(store, nil, nil)
	executor := &requestRefreshEvaluatorExecutor{}
	manager.RegisterExecutor(executor)

	registered, errRegister := manager.Register(
		context.Background(),
		&Auth{
			ID:       "request-refresh.json",
			Provider: executor.Identifier(),
			Metadata: map[string]any{"access_token": "old", "refresh_token": "refresh"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	prepared, errPrepare := manager.prepareRequestAuth(context.Background(), executor, registered)
	if errPrepare != nil {
		t.Fatalf("prepareRequestAuth() error = %v", errPrepare)
	}
	if prepared == nil || prepared.Metadata["access_token"] != "new" {
		t.Fatalf("prepared auth = %#v", prepared)
	}
	if executor.refreshCalls.Load() != 1 {
		t.Fatalf("Refresh calls = %d, want 1", executor.refreshCalls.Load())
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current.Metadata["access_token"] != "new" {
		t.Fatalf("manager auth = %#v, found = %t", current, ok)
	}
	if store.saveCount.Load() != 2 {
		t.Fatalf("store Save calls = %d, want register and refresh", store.saveCount.Load())
	}
}
