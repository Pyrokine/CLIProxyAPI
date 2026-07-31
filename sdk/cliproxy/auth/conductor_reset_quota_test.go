package auth

import (
	"context"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
)

func TestManagerResetQuotaClearsRuntimeAndRegistryState(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	authID := "reset-quota-auth"
	model := "reset-quota-model"
	next := time.Now().Add(time.Hour)

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "claude", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(
		func() {
			reg.UnregisterClient(authID)
		},
	)

	if _, err := manager.Register(
		ctx, &Auth{
			ID:             authID,
			Provider:       "claude",
			Status:         StatusError,
			StatusMessage:  "quota exhausted",
			Unavailable:    true,
			NextRetryAfter: next,
			Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					StatusMessage:  "quota exhausted",
					Unavailable:    true,
					NextRetryAfter: next,
					Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
					UpdatedAt:      next,
				},
			},
		},
	); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	reg.SetModelQuotaExceeded(authID, model)
	reg.SuspendClientModel(authID, model, "quota")
	if available, err := reg.GetFirstAvailableModel("claude"); err == nil || available != "" {
		t.Fatalf("registry available model before reset = %q, %v; want none", available, err)
	}

	updated, models, err := manager.ResetQuota(ctx, authID)
	if err != nil {
		t.Fatalf("ResetQuota() error = %v", err)
	}
	if updated == nil {
		t.Fatalf("ResetQuota() updated auth is nil")
	}
	if len(models) != 1 || models[0] != model {
		t.Fatalf("ResetQuota() models = %v, want [%s]", models, model)
	}
	if updated.Status != StatusActive || updated.StatusMessage != "" || updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf(
			"updated auth state = status %q message %q unavailable %v next %v",
			updated.Status,
			updated.StatusMessage,
			updated.Unavailable,
			updated.NextRetryAfter,
		)
	}
	if updated.Quota.Exceeded || updated.Quota.Reason != "" || !updated.Quota.NextRecoverAt.IsZero() || updated.Quota.BackoffLevel != 0 {
		t.Fatalf("updated auth quota = %+v, want cleared", updated.Quota)
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("updated model state missing")
	}
	if state.Status != StatusActive || state.StatusMessage != "" || state.Unavailable || !state.NextRetryAfter.IsZero() {
		t.Fatalf(
			"updated model state = status %q message %q unavailable %v next %v",
			state.Status,
			state.StatusMessage,
			state.Unavailable,
			state.NextRetryAfter,
		)
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
		t.Fatalf("updated model quota = %+v, want cleared", state.Quota)
	}
	if available, err := reg.GetFirstAvailableModel("claude"); err != nil || available != model {
		t.Fatalf("registry available model after reset = %q, %v; want %q", available, err, model)
	}
}

func TestManagerResetQuotaRejectsInFlightPreResetResult(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	const (
		authID = "reset-quota-in-flight"
		model  = "quota-model"
	)
	next := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(
		ctx, &Auth{
			ID:             authID,
			Provider:       "claude",
			Status:         StatusError,
			Unavailable:    true,
			NextRetryAfter: next,
			Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: next,
					Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
					QuotaEpoch:     2,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	updated, _, errReset := manager.ResetQuota(ctx, authID)
	if errReset != nil {
		t.Fatalf("ResetQuota() error = %v", errReset)
	}
	if updated == nil || updated.Generation == registered.Generation {
		t.Fatalf("ResetQuota() result = %#v, want new generation", updated)
	}

	retryAfter := time.Hour
	manager.MarkResult(
		ctx, Result{
			AuthID:          authID,
			AuthGeneration:  registered.Generation,
			Model:           model,
			ModelQuotaEpoch: 2,
			Error:           &Error{HTTPStatus: 429, Message: "stale quota exhausted"},
			RetryAfter:      &retryAfter,
		},
	)
	current, ok := manager.GetByID(authID)
	if !ok {
		t.Fatal("auth missing after stale pre-reset result")
	}
	state := current.ModelStates[model]
	if state == nil || state.Quota.Exceeded || state.Unavailable || state.QuotaEpoch != 3 {
		t.Fatalf("stale pre-reset result changed recovered model: %#v", state)
	}

	manager.MarkResult(
		ctx, Result{
			AuthID:          authID,
			AuthGeneration:  current.Generation,
			Model:           model,
			ModelQuotaEpoch: state.QuotaEpoch,
			Error:           &Error{HTTPStatus: 429, Message: "new quota exhausted"},
			RetryAfter:      &retryAfter,
		},
	)
	current, ok = manager.GetByID(authID)
	if !ok {
		t.Fatal("auth missing after current result")
	}
	state = current.ModelStates[model]
	if state == nil || !state.Quota.Exceeded || !state.Unavailable || state.QuotaEpoch != 4 {
		t.Fatalf("current post-reset result was not applied: %#v", state)
	}
}

func TestManagerResetQuotaPreservesNonQuotaModelError(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	const (
		authID     = "reset-mixed-auth"
		quotaModel = "quota-model"
		errorModel = "missing-model"
	)
	next := time.Now().Add(time.Hour)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(
		authID,
		"claude",
		[]*registry.ModelInfo{{ID: quotaModel}, {ID: errorModel}},
	)
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	registered, errRegister := manager.Register(
		ctx, &Auth{
			ID:             authID,
			Provider:       "claude",
			Status:         StatusError,
			StatusMessage:  "quota exhausted",
			Unavailable:    true,
			NextRetryAfter: next,
			Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
			ModelStates: map[string]*ModelState{
				quotaModel: {
					Status:         StatusError,
					StatusMessage:  "quota exhausted",
					Unavailable:    true,
					NextRetryAfter: next,
					LastError:      &Error{HTTPStatus: 429, Message: "quota exhausted"},
					Quota:          QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
					UpdatedAt:      next.Add(-time.Minute),
					QuotaEpoch:     3,
				},
				errorModel: {
					Status:         StatusError,
					StatusMessage:  "model not found",
					Unavailable:    true,
					NextRetryAfter: next,
					LastError:      &Error{HTTPStatus: 404, Message: "model not found"},
					UpdatedAt:      next,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	reg.SetModelQuotaExceeded(authID, quotaModel)
	reg.SuspendClientModel(authID, quotaModel, "quota")
	reg.SuspendClientModel(authID, errorModel, "not_found")

	updated, models, err := manager.ResetQuota(ctx, authID)
	if err != nil {
		t.Fatalf("ResetQuota() error = %v", err)
	}
	if len(models) != 1 || models[0] != quotaModel {
		t.Fatalf("ResetQuota() models = %v, want [%s]", models, quotaModel)
	}
	quotaState := updated.ModelStates[quotaModel]
	if quotaState == nil || quotaState.Quota.Exceeded || quotaState.Unavailable || quotaState.QuotaEpoch != 4 {
		t.Fatalf("quota model state = %#v, want recovered epoch 4", quotaState)
	}
	errorState := updated.ModelStates[errorModel]
	if errorState == nil || errorState.Status != StatusError || !errorState.Unavailable || errorState.LastError == nil {
		t.Fatalf("non-quota model state was cleared: %#v", errorState)
	}
	if updated.Status != StatusError || updated.StatusMessage != "model not found" || updated.LastError == nil {
		t.Fatalf("aggregated auth error = %#v", updated)
	}
	if current, ok := manager.GetByID(authID); !ok || current.Generation == registered.Generation {
		t.Fatalf("ResetQuota() did not advance auth generation: %#v", current)
	}
}
