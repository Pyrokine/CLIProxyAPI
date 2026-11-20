package auth

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func originalThinkingStateAuth(now time.Time) *Auth {
	return &Auth{
		ID: "auth-thinking-states", Provider: "gemini", ModelStates: map[string]*ModelState{
			"gemini-3.1-pro-preview(high)": {
				Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour),
				Quota: QuotaState{
					Exceeded:      true,
					NextRecoverAt: now.Add(time.Hour),
					BackoffLevel:  1,
				}, UpdatedAt: now,
			},
			"gemini-3.1-pro-preview(low)": {
				Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(2 * time.Hour),
				Quota:     QuotaState{Exceeded: true, NextRecoverAt: now.Add(2 * time.Hour), BackoffLevel: 2},
				UpdatedAt: now.Add(time.Minute),
			},
		},
	}
}

func TestOriginalThinkingSuffixStatePublicationInputs(t *testing.T) {
	for _, entry := range []string{"register", "update", "load", "restore", "restore reversed"} {
		t.Run(
			entry, func(t *testing.T) {
				now := time.Now()
				credential := originalThinkingStateAuth(now)
				const model = "gemini-3.1-pro-preview"
				reg := registerProjectionTestClient(
					t, credential.ID, credential.Provider, &registry.ModelInfo{ID: model},
				)
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				manager.RegisterExecutor(&roundExecutor{provider: "gemini"})
				switch entry {
				case "register":
					if _, err := manager.Register(t.Context(), credential); err != nil {
						t.Fatal(err)
					}
				case "update":
					if _, err := manager.Update(t.Context(), credential); err != nil {
						t.Fatal(err)
					}
				case "load":
					manager.SetStore(&countingStore{list: []*Auth{credential}})
					if err := manager.Load(t.Context()); err != nil {
						t.Fatal(err)
					}
				case "restore", "restore reversed":
					if _, err := manager.Register(
						t.Context(), &Auth{ID: credential.ID, Provider: credential.Provider},
					); err != nil {
						t.Fatal(err)
					}
					routes := []string{model + "(high)", model + "(low)"}
					if entry == "restore reversed" {
						routes[0], routes[1] = routes[1], routes[0]
					}
					var records []CooldownStateRecord
					for _, route := range routes {
						state := credential.ModelStates[route]
						records = append(
							records, CooldownStateRecord{
								AuthID: credential.ID, Provider: credential.Provider,
								Model: route, NextRetryAfter: state.NextRetryAfter, UpdatedAt: state.UpdatedAt,
								Quota:       state.Quota,
								EffectScope: core.StateEffectScopeExactModel, EffectKey: route, StateGeneration: 1,
								ResetAuthority: StateResetAuthoritySuccess,
							},
						)
					}
					manager.SetCooldownStateStore(&recordingCooldownStateStore{load: records})
					if err := manager.RestoreCooldownStates(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				current, found := manager.GetByID(credential.ID)
				if !found || len(current.ModelStates) != 1 {
					t.Fatalf("published credential=%+v found=%t", current, found)
				}
				state := current.ModelStates[model]
				deadline := now.Add(2 * time.Hour)
				if state == nil || !state.Unavailable || !state.NextRetryAfter.Equal(deadline) ||
					state.Quota.BackoffLevel != 2 ||
					!state.Quota.NextRecoverAt.Equal(deadline) ||
					state.EffectScope == core.StateEffectScopeExactModel && state.EffectKey != model {
					t.Fatalf("canonical state=%+v, want latest original 2 h state and canonical key", state)
				}
				if blocked, _, _ := isAuthBlockedForModel(current, model, now.Add(90*time.Minute)); !blocked {
					t.Fatal("canonical state became selectable before the original later deadline")
				}
				manager.RefreshSchedulerEntry(credential.ID)
				if !reg.IsModelSuspendedForClient(credential.ID, model) {
					t.Fatal("canonical cooldown was not projected to the model registry")
				}
				for _, route := range []string{model, model + "(high)", model + "(low)"} {
					selected, err := manager.SelectAuth(t.Context(), credential.Provider, route, core.Options{})
					var cooldown *modelCooldownError
					if selected != nil || !errors.As(err, &cooldown) || cooldown.resetIn < 115*time.Minute {
						t.Fatalf(
							"route=%s selected=%+v error=%v, want original later cooldown",
							route,
							selected,
							err,
						)
					}
				}
			},
		)
	}
}

func TestOriginalCooldownRestorePreservesIndependentQuotaAuthority(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "quota-first", true: "error-first"}[reversed], func(t *testing.T) {
				now := time.Now()
				const model = "gemini-3.1-pro-preview"
				credential := &Auth{ID: "auth-thinking-states", Provider: "gemini"}
				reg := registerProjectionTestClient(
					t, credential.ID, credential.Provider, &registry.ModelInfo{ID: model},
				)
				manager := NewManager(nil, nil, nil)
				if _, err := manager.Register(t.Context(), credential); err != nil {
					t.Fatal(err)
				}
				records := []CooldownStateRecord{
					{
						AuthID: credential.ID, Model: model + "(high)", StateGeneration: 4, UpdatedAt: now,
						NextRetryAfter: now.Add(2 * time.Hour), EffectScope: core.StateEffectScopeExactModel,
						EffectKey:      model + "(high)",
						ResetAuthority: StateResetAuthorityOperator, Quota: QuotaState{
							Exceeded: true, Generation: 9,
							NextRecoverAt: now.Add(2 * time.Hour), Family: "independent",
							ResetAuthority: StateResetAuthorityOperator,
						},
					},
					{
						AuthID: credential.ID, Model: model + "(low)", StateGeneration: 8,
						UpdatedAt:      now.Add(time.Minute),
						NextRetryAfter: now.Add(time.Minute),
						LastError:      &Error{HTTPStatus: 503, Message: "independent model failure"},
						EffectScope:    core.StateEffectScopeExactModel, EffectKey: model + "(low)",
						ResetAuthority: StateResetAuthoritySuccess,
					},
				}
				if reversed {
					records[0], records[1] = records[1], records[0]
				}
				store := &recordingCooldownStateStore{load: records}
				manager.SetCooldownStateStore(store)
				if err := manager.RestoreCooldownStates(t.Context()); err != nil {
					t.Fatal(err)
				}
				current, _ := manager.GetByID(credential.ID)
				state := current.ModelStates[model]
				if len(current.ModelStates) != 1 || state == nil || state.Generation != 8 || state.EffectKey != model ||
					state.LastError ==
						nil ||
					state.LastError.HTTPStatus !=
						503 ||
					state.LastError.executionState().StateGeneration !=
						8 ||
					state.ResetAuthority != StateResetAuthoritySuccess || state.Quota.Generation != 9 ||
					state.Quota.Family != "independent" ||
					state.Quota.ResetAuthority != StateResetAuthorityOperator ||
					!state.Quota.NextRecoverAt.Equal(now.Add(2*time.Hour)) {
					t.Fatalf("restore lost independent state or quota authority: %+v", state)
				}
				if blocked, _, _ := isAuthBlockedForModel(current, model, now.Add(90*time.Minute)); !blocked {
					t.Fatal("shorter model error replaced the independent quota deadline")
				}
				if !reg.IsModelSuspendedForClient(credential.ID, model) {
					t.Fatal("restored authority was not projected to the model registry")
				}
				var persisted bool
				for _, record := range store.records {
					if record.Model == model {
						persisted = record.EffectKey == model && record.StateGeneration == 8 &&
							record.Quota.Generation == 9 &&
							record.Quota.ResetAuthority == StateResetAuthorityOperator
					}
				}
				if !persisted {
					t.Fatalf("canonical authority was not persisted: %+v", store.records)
				}
			},
		)
	}
}

func TestOriginalAuthUpdateDisabledAndActiveInputs(t *testing.T) {
	for _, sample := range []struct {
		id, model        string
		before, disabled bool
		backoff          int
	}{
		{id: "auth-disabled", model: "stale-model", before: true, disabled: true, backoff: 5},
		{id: "auth-a2d", model: "stale-model", disabled: true, backoff: 9},
		{id: "auth-d2a", model: "stale-model", before: true, backoff: 4},
		{id: "auth-active", model: "active-model", backoff: 3},
	} {
		t.Run(
			sample.id, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				manager.RegisterExecutor(&roundExecutor{provider: "claude"})
				status := StatusActive
				if sample.before {
					status = StatusDisabled
				}
				registerRequestScopedTestAuth(
					t, manager, &Auth{
						ID: sample.id, Provider: "claude", Disabled: sample.before, Status: status,
						ModelStates: map[string]*ModelState{
							sample.model: {Quota: QuotaState{BackoffLevel: sample.backoff}},
						},
					}, sample.model,
				)
				status = StatusActive
				if sample.disabled {
					status = StatusDisabled
				}
				if _, err := manager.Update(
					t.Context(), &Auth{ID: sample.id, Provider: "claude", Disabled: sample.disabled, Status: status},
				); err != nil {
					t.Fatal(err)
				}
				current, found := manager.GetByID(sample.id)
				if !found || len(current.ModelStates) != 1 || current.ModelStates[sample.model] == nil ||
					current.ModelStates[sample.model].Quota.BackoffLevel != sample.backoff {
					t.Fatalf("original inactive history was not retained: %+v", current)
				}
				state := current.ModelStates[sample.model]
				if state.Unavailable || !state.NextRetryAfter.IsZero() || state.Quota.Exceeded ||
					!state.Quota.NextRecoverAt.IsZero() {
					t.Fatalf("inactive model history became an active restriction: %+v", state)
				}
				selected, err := manager.SelectAuth(t.Context(), "claude", sample.model, core.Options{})
				if sample.disabled {
					if selected != nil || err == nil {
						t.Fatalf("disabled credential selected=%+v error=%v", selected, err)
					}
				} else if err != nil || selected == nil || selected.ID != sample.id {
					t.Fatalf("active credential selected=%+v error=%v", selected, err)
				}
			},
		)
	}
}

func TestOriginalAuthUpdateStrictSaveAndSnapshotInputs(t *testing.T) {
	for _, operation := range []string{"register", "update"} {
		for _, fail := range []bool{false, true} {
			t.Run(
				operation+map[bool]string{false: "/saved", true: "/save failed"}[fail], func(t *testing.T) {
					failure := errors.New("original update save failed")
					var stored *Auth
					store := &callbackAuthStore{
						save: func(snapshot *Auth) error {
							stored = snapshot
							snapshot.Metadata["store_field"] = "saved-field"
							if fail {
								return failure
							}
							return nil
						},
					}
					manager := NewManager(store, nil, nil)
					initial := &Auth{
						ID: "auth-1", Provider: "claude", Status: StatusActive, Metadata: map[string]any{"k": "v"},
						ModelStates: map[string]*ModelState{"test-model": {Quota: QuotaState{BackoffLevel: 7}}},
					}
					if _, err := manager.Register(WithSkipPersist(t.Context()), initial); err != nil {
						t.Fatal(err)
					}
					before, _ := manager.GetByID(initial.ID)
					next := before.Clone()
					next.Metadata["k"] = "v2"
					var result *Auth
					var err error
					if operation == "register" {
						result, err = manager.Register(t.Context(), next)
					} else {
						result, err = manager.Update(t.Context(), next)
					}
					current, _ := manager.GetByID(initial.ID)
					if fail {
						if result != nil || !errors.Is(err, failure) || !reflect.DeepEqual(current, before) {
							t.Fatalf("failed save result=%+v error=%v current=%+v", result, err, current)
						}
						return
					}
					if err != nil || result == nil || stored == nil || current.Metadata["k"] != "v2" ||
						current.Metadata["store_field"] !=
							"saved-field" ||
						current.ModelStates["test-model"].Quota.BackoffLevel !=
							7 {
						t.Fatalf("saved result=%+v error=%v current=%+v", result, err, current)
					}
					stored.Metadata["k"] = "mutated-store-snapshot"
					stored.ModelStates["test-model"].Quota.BackoffLevel = 99
					current, _ = manager.GetByID(initial.ID)
					if current.Metadata["k"] != "v2" || result.Metadata["k"] != "v2" ||
						current.ModelStates["test-model"].Quota.BackoffLevel !=
							7 ||
						result.ModelStates["test-model"].Quota.BackoffLevel !=
							7 {
						t.Fatal("saved storage snapshot retained a mutable runtime or return reference")
					}
				},
			)
		}
	}
}
