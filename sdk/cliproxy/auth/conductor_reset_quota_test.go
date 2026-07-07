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
