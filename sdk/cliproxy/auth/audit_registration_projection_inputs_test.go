package auth

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestAuditRegistrationDropsRemovedRoutesWithoutClearingIndependentAuthority(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	deadline := time.Now().Add(time.Hour)
	credential := &Auth{
		ID: "projection-transition", Provider: "test", Status: StatusError, Unavailable: true,
		ModelStates: map[string]*ModelState{
			"removed-model": {
				Status: StatusError, Unavailable: true, NextRetryAfter: deadline, Generation: 7,
				Quota: QuotaState{
					Exceeded: true, Family: "removed-family", Generation: 7, NextRecoverAt: deadline,
					ResetAuthority: StateResetAuthorityOperator,
				},
			},
			"retained-model": {
				Status: StatusError, Unavailable: true, NextRetryAfter: deadline, Generation: 8,
				Quota: QuotaState{
					Exceeded: true, Family: "retained-family", Generation: 8, NextRecoverAt: deadline,
					ResetAuthority: StateResetAuthorityOperator,
				},
			},
		},
	}
	registered, err := manager.Register(t.Context(), credential)
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(registered.ID, "test", []*registry.ModelInfo{{ID: "removed-model"}, {ID: "retained-model"}})
	t.Cleanup(func() { reg.UnregisterClient(registered.ID) })
	manager.RefreshSchedulerEntry(registered.ID)
	_, oldEpoch := reg.GetModelsAndEpochForClient(registered.ID)
	reg.UnregisterClient(registered.ID)
	reg.RegisterClient(registered.ID, "test", []*registry.ModelInfo{{ID: "fresh-model"}, {ID: "retained-model"}})
	manager.RefreshSchedulerEntry(registered.ID)
	models, epoch := reg.GetModelsAndEpochForClient(registered.ID)
	if epoch <= oldEpoch || len(models) != 2 || reg.IsModelSuspendedForClient(
		registered.ID, "fresh-model",
	) || !reg.IsModelQuotaExceededForClient(registered.ID, "retained-model") {
		t.Fatalf("current registration/projection lost authority: epoch=%d old=%d models=%#v", epoch, oldEpoch, models)
	}
	for _, model := range models {
		if model.ID == "removed-model" {
			t.Fatal("removed route remains published")
		}
	}
	if selected, err := manager.scheduler.pickSingle(
		t.Context(), "test", "fresh-model", core.Options{}, nil,
	); err != nil || selected == nil || selected.ID != registered.ID {
		t.Fatalf("removed-model state blocked fresh route: selected=%#v error=%v", selected, err)
	}
	if selected, err := manager.scheduler.pickSingle(
		t.Context(), "test", "removed-model", core.Options{}, nil,
	); err == nil || selected != nil {
		t.Fatalf("removed route remains selectable: selected=%#v error=%v", selected, err)
	}
	if reg.ApplyClientModelProjections(
		registered.ID, oldEpoch, ^uint64(0), []registry.ClientModelProjection{{ModelID: "retained-model"}},
	) {
		t.Fatal("old registration epoch cleared current authority")
	}
	if !reg.IsModelQuotaExceededForClient(registered.ID, "retained-model") {
		t.Fatal("retained independent authority was cleared")
	}
}

func TestRegistryReRegistrationPreservesDeadlinesAndResetAuthority(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(12*time.Hour)
	for _, tc := range []struct {
		name                    string
		state                   *ModelState
		blocked, quota          bool
		complete, needsOperator bool
	}{
		{name: "missing state"},
		{name: "clean state", state: &ModelState{Status: StatusActive}},
		{
			name: "active cooldown", blocked: true, state: &ModelState{
				Status: StatusError, StatusMessage: "not_found", Unavailable: true, NextRetryAfter: future,
				LastError: &Error{HTTPStatus: http.StatusNotFound},
			},
		},
		{
			name: "expired cooldown", state: &ModelState{
				Status: StatusError, Unavailable: true, NextRetryAfter: past,
				LastError: &Error{HTTPStatus: http.StatusNotFound},
			},
		},
		{
			name: "active quota", blocked: true, quota: true, state: &ModelState{
				Status: StatusError, Unavailable: true, NextRetryAfter: past,
				Quota: QuotaState{Exceeded: true, NextRecoverAt: future},
			},
		},
		{
			name: "expired quota", state: &ModelState{
				Status: StatusError, Unavailable: true, NextRetryAfter: past,
				Quota: QuotaState{Exceeded: true, NextRecoverAt: past},
			},
		},
		{name: "disabled model", blocked: true, state: &ModelState{Status: StatusDisabled}},
		{
			name: "no deadline requires matching success", blocked: true, complete: true, state: &ModelState{
				Status: StatusError, Unavailable: true, Generation: 7, ResetAuthority: StateResetAuthoritySuccess,
				LastError: &Error{HTTPStatus: http.StatusNotFound},
			},
		},
		{
			name: "no deadline requires operator", blocked: true, quota: true, complete: true, needsOperator: true,
			state: &ModelState{
				Status: StatusError, Unavailable: true, Generation: 7, ResetAuthority: StateResetAuthorityOperator,
				Quota: QuotaState{Exceeded: true, Generation: 8, ResetAuthority: StateResetAuthorityOperator},
			},
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				const model, provider = "gpt-5.4", "codex"
				manager := NewManager(nil, &FillFirstSelector{}, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				credential := &Auth{ID: t.Name(), Provider: provider, Status: StatusActive}
				if tc.state != nil {
					credential.ModelStates = map[string]*ModelState{model: tc.state}
				}
				registered, err := manager.Register(t.Context(), credential)
				if err != nil {
					t.Fatal(err)
				}
				reg := registerProjectionTestClient(t, registered.ID, provider, &registry.ModelInfo{ID: model})
				manager.RefreshSchedulerEntry(registered.ID)
				reg.UnregisterClient(registered.ID)
				reg.RegisterClient(registered.ID, provider, []*registry.ModelInfo{{ID: model}})
				manager.RefreshSchedulerEntry(registered.ID)
				current, ok := manager.GetByID(registered.ID)
				if !ok || current.Generation != registered.Generation || !reflect.DeepEqual(
					current.ModelStates, registered.ModelStates,
				) {
					t.Fatalf("registration changed credential state: before=%#v after=%#v", registered, current)
				}
				checkRouting := func(blocked, quota bool) {
					t.Helper()
					if reg.IsModelSuspendedForClient(registered.ID, model) != blocked ||
						reg.IsModelQuotaExceededForClient(registered.ID, model) != quota {
						t.Fatalf("registry block/quota differs from %t/%t", blocked, quota)
					}
					selected, err := manager.scheduler.pickSingle(t.Context(), provider, model, core.Options{}, nil)
					if blocked && (err == nil || selected != nil) ||
						!blocked && (err != nil || selected == nil || selected.ID != registered.ID) {
						t.Fatalf("scheduler blocked=%t selected=%#v error=%v", blocked, selected, err)
					}
				}
				checkRouting(tc.blocked, tc.quota)
				if !tc.complete {
					return
				}
				staleFailure := executionResult(
					current, provider, model,
					&Error{HTTPStatus: http.StatusTooManyRequests}, core.DispatchPhaseResponseStarted,
				)
				manager.MarkResult(
					t.Context(), executionResult(current, provider, model, nil, core.DispatchPhaseResponseStarted),
				)
				checkRouting(tc.needsOperator, tc.needsOperator)
				if tc.needsOperator {
					_, affected, err := manager.ResetCooldown(t.Context(), registered.ID)
					if err != nil || len(affected) != 1 || affected[0] != model {
						t.Fatalf("local operator recovery affected=%v error=%v", affected, err)
					}
					checkRouting(false, false)
				}
				manager.MarkResult(t.Context(), staleFailure)
				checkRouting(false, false)
			},
		)
	}
}

func TestRegistryRemovalPreservesIndependentModelFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		empty    bool
		status   int
		message  string
		deadline time.Duration
	}{
		{"unsupported model", false, http.StatusPaymentRequired, "payment_required", 30 * time.Minute},
		{"empty registry", true, http.StatusNotFound, "not_found", 12 * time.Hour},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				state := &ModelState{
					Status: StatusError, StatusMessage: tc.message, Unavailable: true,
					NextRetryAfter: time.Now().Add(tc.deadline),
					EffectScope:    core.StateEffectScopeExactModel, EffectKey: "gpt-5.4", Generation: 7,
					ResetAuthority: StateResetAuthoritySuccess,
				}
				if tc.empty {
					state.LastError = &Error{HTTPStatus: tc.status, Message: tc.message}
				}
				registered, err := manager.Register(
					t.Context(), &Auth{
						ID: t.Name(), Provider: "codex", Status: StatusError, Unavailable: true,
						StatusMessage: tc.message, LastError: &Error{HTTPStatus: tc.status, Message: tc.message},
						ModelStates: map[string]*ModelState{"gpt-5.4": state},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				reg := registerProjectionTestClient(t, registered.ID, "codex", &registry.ModelInfo{ID: "gpt-5.4"})
				manager.RefreshSchedulerEntry(registered.ID)
				_, previousEpoch := reg.GetModelsAndEpochForClient(registered.ID)
				reg.UnregisterClient(registered.ID)
				if !tc.empty {
					reg.RegisterClient(registered.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.5"}})
				}
				manager.RefreshSchedulerEntry(registered.ID)
				current, ok := manager.GetByID(registered.ID)
				if !ok || !reflect.DeepEqual(current, registered) {
					t.Fatalf(
						"removing a route changed independent credential state: before=%#v after=%#v", registered,
						current,
					)
				}
				models := reg.GetModelsForClient(registered.ID)
				if tc.empty && len(models) != 0 || !tc.empty && (len(models) != 1 || models[0].ID != "gpt-5.5") {
					t.Fatalf("removed route remains published: %#v", models)
				}
				if selected, err := manager.scheduler.pickSingle(
					t.Context(), "codex", "gpt-5.4", core.Options{}, nil,
				); err == nil || selected != nil {
					t.Fatalf("removed route remains selectable: auth=%#v error=%v", selected, err)
				}
				if !tc.empty {
					selected, err := manager.scheduler.pickSingle(t.Context(), "codex", "gpt-5.5", core.Options{}, nil)
					if err != nil || selected == nil || selected.ID != registered.ID {
						t.Fatalf("old model failure blocks new route: auth=%#v error=%v", selected, err)
					}
				}
				if reg.ApplyClientModelProjections(
					registered.ID, previousEpoch, ^uint64(0),
					[]registry.ClientModelProjection{{ModelID: "gpt-5.4"}},
				) {
					t.Fatal("old registration projection was accepted after removal")
				}
			},
		)
	}
}
