package auth

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManagerRecoverCredentialQuotaPreservesModelQuotaAndRegistryState(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	const (
		authID   = "recover-credential-quota"
		provider = "recover-credential-provider"
		model    = "quota-family-model"
	)
	next := time.Now().Add(time.Hour)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	registered, errRegister := manager.Register(
		ctx, &Auth{
			ID:               authID,
			Provider:         provider,
			Status:           StatusError,
			StatusMessage:    "credential quota",
			Unavailable:      true,
			NextRetryAfter:   next,
			LastError:        &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential quota"},
			Quota:            QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
			StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					StatusMessage:  "model quota",
					Unavailable:    true,
					NextRetryAfter: next,
					LastError:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "model quota"},
					Quota: QuotaState{
						Exceeded: true, Reason: "quota", Family: "family", NextRecoverAt: next, Generation: 5,
					},
					EffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
					EffectKey:   "family",
					QuotaEpoch:  7,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	reg.SetModelQuotaExceeded(authID, model)
	reg.SuspendClientModel(authID, model, "quota")
	if available, errAvailable := reg.GetFirstAvailableModel(provider); errAvailable == nil || available != "" {
		t.Fatalf("registry model before recovery = %q, %v; want none", available, errAvailable)
	}

	updated, errRecover := manager.RecoverCredentialQuota(ctx, authID)
	if errRecover != nil {
		t.Fatalf("RecoverCredentialQuota() error = %v", errRecover)
	}
	if updated == nil || updated.Generation == registered.Generation {
		t.Fatalf("RecoverCredentialQuota() result = %#v, want new generation", updated)
	}
	state := updated.ModelStates[model]
	if state == nil || !state.Quota.Exceeded || !state.Unavailable || state.QuotaEpoch != 7 ||
		state.Quota.Generation != 5 || state.EffectScope != cliproxyexecutor.StateEffectScopeQuotaFamily {
		t.Fatalf("model quota state changed = %#v", state)
	}
	if updated.Status != StatusError || !updated.Unavailable || updated.LastError == nil ||
		updated.LastError.Message != "model quota" {
		t.Fatalf("aggregated auth state = %#v", updated)
	}
	if available, errAvailable := reg.GetFirstAvailableModel(provider); errAvailable == nil || available != "" {
		t.Fatalf("registry model after recovery = %q, %v; want none", available, errAvailable)
	}
}

func TestManagerRecoverQuotaAfterRefreshClearsPersistedFamilyCooldownAndRestoresRouting(t *testing.T) {
	ctx := context.Background()
	const (
		authID   = "recover-family-persistence"
		provider = "codex"
		model    = "gpt-5.6-sol"
		family   = "gpt-5.6"
	)
	startedAt := time.Now().UTC()
	next := startedAt.Add(time.Hour)
	authDir := t.TempDir()
	cooldownStore := NewFileCooldownStateStoreWithAuthDir(authDir, authDir)
	manager := NewManager(nil, nil, nil)
	manager.SetCooldownStateStore(cooldownStore)
	registered, errRegister := manager.Register(
		WithSkipPersist(ctx), &Auth{
			ID:          authID,
			Provider:    provider,
			FileName:    filepath.Join(authDir, "codex.json"),
			Status:      StatusError,
			Unavailable: true,
			LastError:   &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"},
			Quota:       QuotaState{Exceeded: true, Family: family, NextRecoverAt: next, Generation: 4},
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					StatusMessage:  "quota exhausted",
					Unavailable:    true,
					NextRetryAfter: next,
					LastError:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"},
					Quota: QuotaState{
						Exceeded:       true,
						Reason:         "quota",
						Family:         family,
						NextRecoverAt:  next,
						Generation:     4,
						ResetAuthority: StateResetAuthoritySuccess,
					},
					EffectScope:    cliproxyexecutor.StateEffectScopeQuotaFamily,
					EffectKey:      family,
					ResetAuthority: StateResetAuthoritySuccess,
					Generation:     4,
					UpdatedAt:      startedAt.Add(-time.Minute),
					QuotaEpoch:     6,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	manager.PersistCooldownStates(ctx)
	if records, errLoad := cooldownStore.Load(ctx); errLoad != nil || len(records) != 2 {
		t.Fatalf("persisted cooldown before recovery = %+v, %v", records, errLoad)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })
	reg.SetModelQuotaExceeded(authID, model)
	reg.SuspendClientModel(authID, model, "quota")

	candidates := QuotaFamilyRecoveryCandidates(registered)
	recovered, errRecover := manager.RecoverQuotaAfterRefresh(ctx, authID, startedAt, candidates)
	if errRecover != nil {
		t.Fatalf("RecoverQuotaAfterRefresh() error = %v", errRecover)
	}
	state := recovered.ModelStates[model]
	if state == nil || state.Unavailable || state.Quota.Exceeded || state.QuotaEpoch != 7 {
		t.Fatalf("recovered model state = %+v", state)
	}
	if picked, errPick := manager.scheduler.pickSingle(
		ctx, provider, model, cliproxyexecutor.Options{}, nil,
	); errPick != nil || picked == nil || picked.ID != authID {
		t.Fatalf("route after recovery = picked:%#v error:%v", picked, errPick)
	}
	if records, errLoad := cooldownStore.Load(ctx); errLoad != nil || len(records) != 0 {
		t.Fatalf("persisted cooldown after recovery = %+v, %v", records, errLoad)
	}

	restarted := NewManager(nil, nil, nil)
	restarted.SetCooldownStateStore(cooldownStore)
	if _, errRegister = restarted.Register(
		WithSkipPersist(ctx), &Auth{
			ID:       authID,
			Provider: provider,
			FileName: filepath.Join(authDir, "codex.json"),
			Status:   StatusActive,
		},
	); errRegister != nil {
		t.Fatalf("restart Register() error = %v", errRegister)
	}
	if errRestore := restarted.RestoreCooldownStates(ctx); errRestore != nil {
		t.Fatalf("RestoreCooldownStates() error = %v", errRestore)
	}
	afterRestart, _ := restarted.GetByID(authID)
	if state := afterRestart.ModelStates[model]; state != nil && (state.Unavailable || state.Quota.Exceeded) {
		t.Fatalf("old cooldown returned after restart = %+v", state)
	}
	if picked, errPick := restarted.scheduler.pickSingle(
		ctx, provider, model, cliproxyexecutor.Options{}, nil,
	); errPick != nil || picked == nil || picked.ID != authID {
		t.Fatalf("route after restart = picked:%#v error:%v", picked, errPick)
	}
}

func TestManagerRecoverCredentialQuotaRestoresLegacyAuthOnly429(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	next := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(
		ctx, &Auth{
			ID:             "recover-legacy-auth-only",
			Provider:       "codex",
			Status:         StatusError,
			StatusMessage:  "rate limited",
			Unavailable:    true,
			NextRetryAfter: next,
			LastError:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	updated, errRecover := manager.RecoverCredentialQuota(ctx, registered.ID)
	if errRecover != nil {
		t.Fatalf("RecoverCredentialQuota() error = %v", errRecover)
	}
	if updated == nil || updated.Generation == registered.Generation || updated.Status != StatusActive ||
		updated.Unavailable || updated.LastError != nil || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("legacy auth-only quota was not recovered = %#v", updated)
	}
}

func TestManagerRecoverCredentialQuotaPreservesLegacyModel429(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	next := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(
		ctx, &Auth{
			ID:             "recover-legacy-model",
			Provider:       "codex",
			Status:         StatusError,
			StatusMessage:  "rate limited",
			Unavailable:    true,
			NextRetryAfter: next,
			LastError:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
			ModelStates: map[string]*ModelState{
				"legacy-model": {
					Status:         StatusError,
					StatusMessage:  "rate limited",
					Unavailable:    true,
					NextRetryAfter: next,
					LastError:      &Error{HTTPStatus: http.StatusTooManyRequests, Message: "rate limited"},
					QuotaEpoch:     3,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	updated, errRecover := manager.RecoverCredentialQuota(ctx, registered.ID)
	if errRecover != nil {
		t.Fatalf("RecoverCredentialQuota() error = %v", errRecover)
	}
	state := updated.ModelStates["legacy-model"]
	if updated.Generation != registered.Generation || state == nil || !state.Unavailable ||
		state.LastError == nil || state.QuotaEpoch != 3 {
		t.Fatalf("legacy model quota changed = %#v", updated)
	}
}

func TestManagerRecoverCredentialQuotaRejectsModelScopedAggregate(t *testing.T) {
	for _, scope := range []cliproxyexecutor.StateEffectScope{
		cliproxyexecutor.StateEffectScopeExactModel,
		cliproxyexecutor.StateEffectScopeQuotaFamily,
	} {
		t.Run(
			string(scope), func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				next := time.Now().Add(time.Hour)
				registered, errRegister := manager.Register(
					context.Background(), &Auth{
						ID:               "recover-model-scoped-" + string(scope),
						Provider:         "codex",
						Status:           StatusError,
						Unavailable:      true,
						NextRetryAfter:   next,
						LastError:        &Error{HTTPStatus: http.StatusTooManyRequests, Message: "model quota"},
						Quota:            QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next},
						StateEffectScope: scope,
					},
				)
				if errRegister != nil {
					t.Fatalf("Register() error = %v", errRegister)
				}
				updated, errRecover := manager.RecoverCredentialQuota(context.Background(), registered.ID)
				if errRecover != nil {
					t.Fatalf("RecoverCredentialQuota() error = %v", errRecover)
				}
				if updated == nil || updated.Generation != registered.Generation || !updated.Quota.Exceeded ||
					!updated.Unavailable || updated.LastError == nil {
					t.Fatalf("model-scoped aggregate changed = %#v", updated)
				}
			},
		)
	}
}

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

func TestManagerResetQuotaClearsLegacy429Cooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	const (
		authID = "reset-legacy-429"
		model  = "legacy-quota-model"
	)
	next := time.Now().Add(time.Hour)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	registered, errRegister := manager.Register(
		ctx, &Auth{
			ID:             authID,
			Provider:       "codex",
			Status:         StatusError,
			StatusMessage:  "rate limited",
			Unavailable:    true,
			NextRetryAfter: next,
			LastError:      &Error{HTTPStatus: 429, Message: "rate limited"},
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					StatusMessage:  "rate limited",
					Unavailable:    true,
					NextRetryAfter: next,
					LastError:      &Error{HTTPStatus: 429, Message: "rate limited"},
					QuotaEpoch:     7,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	reg.SuspendClientModel(authID, model, "quota")

	updated, models, err := manager.ResetQuota(ctx, authID)
	if err != nil {
		t.Fatalf("ResetQuota() error = %v", err)
	}
	if len(models) != 1 || models[0] != model {
		t.Fatalf("ResetQuota() models = %v, want [%s]", models, model)
	}
	if updated == nil || updated.Generation == registered.Generation {
		t.Fatalf("ResetQuota() result = %#v, want new generation", updated)
	}
	if updated.Status != StatusActive || updated.Unavailable || updated.LastError != nil ||
		!updated.NextRetryAfter.IsZero() {
		t.Fatalf("legacy auth cooldown was not cleared: %#v", updated)
	}
	state := updated.ModelStates[model]
	if state == nil || state.Status != StatusActive || state.Unavailable || state.LastError != nil ||
		!state.NextRetryAfter.IsZero() || state.QuotaEpoch != 8 {
		t.Fatalf("legacy model cooldown was not cleared: %#v", state)
	}
	if available, errAvailable := reg.GetFirstAvailableModel("codex"); errAvailable != nil || available != model {
		t.Fatalf("registry available model after reset = %q, %v; want %q", available, errAvailable, model)
	}
}

func TestManagerResetQuotaAuthLegacy429PreservesNonQuotaModelBlock(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	const (
		authID       = "reset-auth-legacy-429"
		activeModel  = "active-model"
		missingModel = "missing-model"
	)
	next := time.Now().Add(time.Hour)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(
		authID,
		"codex",
		[]*registry.ModelInfo{{ID: activeModel}, {ID: missingModel}},
	)
	t.Cleanup(func() { reg.UnregisterClient(authID) })

	registered, errRegister := manager.Register(
		ctx, &Auth{
			ID:             authID,
			Provider:       "codex",
			Status:         StatusError,
			StatusMessage:  "rate limited",
			Unavailable:    true,
			NextRetryAfter: next,
			LastError:      &Error{HTTPStatus: 429, Message: "rate limited"},
			ModelStates: map[string]*ModelState{
				activeModel: {Status: StatusActive},
				missingModel: {
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
	reg.SuspendClientModel(authID, activeModel, "quota")
	reg.SuspendClientModel(authID, missingModel, "not_found")

	updated, models, err := manager.ResetQuota(ctx, authID)
	if err != nil {
		t.Fatalf("ResetQuota() error = %v", err)
	}
	if len(models) != 1 || models[0] != activeModel {
		t.Fatalf("ResetQuota() models = %v, want [%s]", models, activeModel)
	}
	if updated == nil || updated.Generation == registered.Generation {
		t.Fatalf("ResetQuota() result = %#v, want new generation", updated)
	}
	missingState := updated.ModelStates[missingModel]
	if missingState == nil || missingState.Status != StatusError || !missingState.Unavailable ||
		missingState.LastError == nil || missingState.LastError.HTTPStatus != 404 {
		t.Fatalf("non-quota model state was cleared: %#v", missingState)
	}
	if updated.Status != StatusError || updated.LastError == nil || updated.LastError.HTTPStatus != 404 {
		t.Fatalf("aggregated auth error = %#v, want preserved 404", updated)
	}
}

func TestManagerResetQuotaPreservesDisabledState(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	const (
		authID = "reset-disabled-legacy-429"
		model  = "disabled-model"
	)
	next := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(
		ctx, &Auth{
			ID:             authID,
			Provider:       "codex",
			Status:         StatusDisabled,
			StatusMessage:  "rate limited",
			Disabled:       true,
			Unavailable:    true,
			NextRetryAfter: next,
			LastError:      &Error{HTTPStatus: 429, Message: "rate limited"},
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: next,
					LastError:      &Error{HTTPStatus: 429, Message: "rate limited"},
					QuotaEpoch:     1,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	updated, models, err := manager.ResetQuota(ctx, authID)
	if err != nil {
		t.Fatalf("ResetQuota() error = %v", err)
	}
	if len(models) != 1 || models[0] != model {
		t.Fatalf("ResetQuota() models = %v, want [%s]", models, model)
	}
	if updated == nil || updated.Generation == registered.Generation {
		t.Fatalf("ResetQuota() result = %#v, want new generation", updated)
	}
	if !updated.Disabled || updated.Status != StatusDisabled {
		t.Fatalf("disabled state changed: %#v", updated)
	}
	if updated.Unavailable || updated.LastError != nil || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("disabled auth retained legacy cooldown: %#v", updated)
	}
	state := updated.ModelStates[model]
	if state == nil || state.Unavailable || state.LastError != nil || state.QuotaEpoch != 2 {
		t.Fatalf("disabled model retained legacy cooldown: %#v", state)
	}
}

func TestManagerResetQuotaPreservesUnattributedCooldown(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	ctx := context.Background()
	const (
		authID = "reset-unattributed-cooldown"
		model  = "unknown-cooldown-model"
	)
	next := time.Now().Add(time.Hour)
	registered, errRegister := manager.Register(
		ctx, &Auth{
			ID:             authID,
			Provider:       "codex",
			Status:         StatusError,
			Unavailable:    true,
			NextRetryAfter: next,
			ModelStates: map[string]*ModelState{
				model: {
					Status:         StatusError,
					Unavailable:    true,
					NextRetryAfter: next,
					QuotaEpoch:     3,
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	updated, models, err := manager.ResetQuota(ctx, authID)
	if err != nil {
		t.Fatalf("ResetQuota() error = %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("ResetQuota() models = %v, want none", models)
	}
	if updated == nil || updated.Generation != registered.Generation {
		t.Fatalf("ResetQuota() changed unattributed cooldown: %#v", updated)
	}
	state := updated.ModelStates[model]
	if state == nil || !state.Unavailable || state.NextRetryAfter != next || state.QuotaEpoch != 3 {
		t.Fatalf("unattributed cooldown changed: %#v", state)
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
