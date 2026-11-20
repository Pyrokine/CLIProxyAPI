package auth

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalTargetedPublicationScopeInputs(t *testing.T) {
	for _, sample := range []struct {
		id, provider string
		models       []string
		success      bool
		credential   bool
		empty        bool
		reuse        bool
	}{
		{id: "auth-targeted-test", provider: "custom-prov", models: []string{"model-a", "model-b", "model-c"}},
		{
			id: "auth-success-targeted-test", provider: "custom-prov-succ", models: []string{"model-1", "model-2"},
			success: true,
		},
		{
			id: "auth-cred-scope-test", provider: "custom-prov-cred", models: []string{"model-x", "model-y"},
			credential: true,
		},
		{id: "auth-empty-shard-test", provider: "custom-prov-empty", models: []string{"only-model"}, empty: true},
		{
			id: "auth-reuse-modelset-test", provider: "custom-prov-reuse", models: []string{"m1", "m2"},
			credential: true, reuse: true,
		},
	} {
		t.Run(
			sample.id, func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				var models []*registry.ModelInfo
				for _, model := range sample.models {
					models = append(models, &registry.ModelInfo{ID: model})
				}
				registerProjectionTestClient(t, sample.id, sample.provider, models...)
				registered, err := manager.Register(
					WithSkipPersist(
						t.Context(),
					),
					&Auth{ID: sample.id, Provider: sample.provider, Status: StatusActive},
				)
				if err != nil {
					t.Fatal(err)
				}
				warmSchedulerModels(t, manager, sample.provider, sample.id, sample.models...)
				if sample.empty {
					warmSchedulerModels(t, manager, sample.provider, sample.id, "")
				}
				before := map[string]schedulerShardTestState{}
				for _, model := range sample.models {
					before[model] = schedulerShardStateForTest(
						manager.scheduler,
						sample.provider,
						model,
						sample.id,
					)
				}
				if sample.reuse {
					manager.scheduler.mu.Lock()
					scheduled := manager.scheduler.providers[sample.provider].auths[sample.id]
					scheduled.supportedModelSet["reused_instance_marker"] = struct{}{}
					manager.scheduler.mu.Unlock()
				}
				failure := errorWithState(
					&Error{
						Code:       "internal_server_error",
						Message:    "500 Internal Server Error",
						HTTPStatus: 500,
					},
					errorState{
						DispatchPhase:  core.DispatchPhaseResponseStarted,
						Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
						RetryDirective: core.RetryDirectiveNoRetry, EffectScope: core.StateEffectScopeExactModel,
						EffectKey: sample.models[0],
					},
				)
				if sample.credential {
					failure.Code, failure.Message, failure.HTTPStatus = "quota_exceeded", "429 Quota "+
						"Exceeded", http.StatusTooManyRequests
					if sample.reuse {
						failure.Message = "429 Quota"
					}
					failure.executionState().EffectScope,
						failure.executionState().EffectKey = core.StateEffectScopeCredential, sample.id
				}
				if sample.success {
					failure = nil
				}
				result := executionResult(
					registered, sample.provider, sample.models[0], failure, core.DispatchPhaseResponseStarted,
				)
				manager.MarkResult(t.Context(), result)
				for index, model := range sample.models {
					after := schedulerShardStateForTest(manager.scheduler, sample.provider, model, sample.id)
					blocked := !sample.success && (sample.credential || index == 0)
					if !after.present || (after.state != scheduledStateReady) != blocked {
						t.Fatalf("model=%s state=%+v, want blocked=%t", model, after, blocked)
					}
					if !sample.credential && index > 0 &&
						(after.meta != before[model].meta || after.lastPicked != before[model].lastPicked) {
						t.Fatalf("unrelated model metadata or cursor changed: %s", model)
					}
				}
				if sample.empty {
					if state := schedulerShardStateForTest(
						manager.scheduler, sample.provider, "", sample.id,
					); !state.present || state.state == scheduledStateReady {
						t.Fatalf("empty aggregate shard=%+v", state)
					}
				}
				if sample.reuse {
					manager.scheduler.mu.Lock()
					scheduled := manager.scheduler.providers[sample.provider].auths[sample.id]
					_, retained := scheduled.supportedModelSet["reused_instance_marker"]
					manager.scheduler.mu.Unlock()
					if !retained {
						t.Fatal("credential result reloaded an unchanged model set")
					}
				}
			},
		)
	}
}

func TestOriginalTargetedPublicationRegistryEpochInputs(t *testing.T) {
	for _, existingShard := range []bool{false, true} {
		id, provider := "auth-reg-epoch-test", "custom-prov-reg-epoch"
		if existingShard {
			id, provider = "auth-1", "custom-prov-epoch-sync"
		}
		t.Run(
			id, func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				reg := registerProjectionTestClient(t, id, provider, &registry.ModelInfo{ID: "model-1"})
				registered, err := manager.Register(
					WithSkipPersist(t.Context()), &Auth{ID: id, Provider: provider, Status: StatusActive},
				)
				if err != nil {
					t.Fatal(err)
				}
				warmSchedulerModels(t, manager, provider, id, "model-1")
				if existingShard {
					registerProjectionTestClient(t, "auth-2", provider, &registry.ModelInfo{ID: "model-2"})
					if _, err := manager.Register(
						WithSkipPersist(t.Context()), &Auth{ID: "auth-2", Provider: provider, Status: StatusActive},
					); err != nil {
						t.Fatal(err)
					}
					warmSchedulerModels(t, manager, provider, "auth-2", "model-2")
				}
				reg.RegisterClient(id, provider, []*registry.ModelInfo{{ID: "model-1"}, {ID: "model-2"}})
				manager.MarkResult(
					t.Context(),
					executionResult(registered, provider, "model-1", nil, core.DispatchPhaseResponseStarted),
				)
				if _, err := manager.scheduler.pickSingle(
					t.Context(), provider, "model-2",
					core.Options{Metadata: map[string]any{core.PinnedAuthMetadataKey: id}}, nil,
				); err != nil {
					t.Fatalf("new registry model was not selectable: %v", err)
				}
				if state := schedulerShardStateForTest(
					manager.scheduler, provider, "model-2", id,
				); !state.present || state.state != scheduledStateReady {
					t.Fatalf("existing shard missed registry epoch: %+v", state)
				}
			},
		)
	}
}

func TestOriginalTargetedPublicationSnapshotInputs(t *testing.T) {
	for _, sample := range []struct {
		id, provider string
		models       []string
		credential   bool
		disabled     bool
	}{
		{id: "auth-ooo-test", provider: "custom-prov-ooo", models: []string{"model-p", "model-q"}},
		{
			id: "auth-ooo-cred-test", provider: "custom-prov-ooo-cred", models: []string{"m-ooo-1", "m-ooo-2"},
			credential: true,
		},
		{
			id: "auth-stale-disabled-test", provider: "custom-prov-stale-disabled", models: []string{"m-stale"},
			disabled: true,
		},
	} {
		t.Run(
			sample.id, func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				var models []*registry.ModelInfo
				for _, model := range sample.models {
					models = append(models, &registry.ModelInfo{ID: model})
				}
				registerProjectionTestClient(t, sample.id, sample.provider, models...)
				registered, err := manager.Register(
					WithSkipPersist(
						t.Context(),
					),
					&Auth{ID: sample.id, Provider: sample.provider, Status: StatusActive},
				)
				if err != nil {
					t.Fatal(err)
				}
				warmSchedulerModels(t, manager, sample.provider, sample.id, sample.models...)
				now := time.Now()
				latest, stale := registered.Clone(), registered.Clone()
				latest.UpdatedAt, stale.UpdatedAt = now.Add(time.Millisecond), now
				if sample.disabled {
					stale.Disabled, stale.Status = true, StatusDisabled
				} else if sample.credential {
					latest.Unavailable, latest.Status = true, StatusError
					latest.Quota = QuotaState{
						Exceeded: true, Reason: "credential_quota", NextRecoverAt: now.Add(time.Hour),
					}
					latest.StateEffectScope, latest.StateEffectKey = core.StateEffectScopeCredential, sample.id
					stale.Unavailable, stale.Quota = true, latest.Quota
					stale.StateEffectScope, stale.StateEffectKey = latest.StateEffectScope, latest.StateEffectKey
				} else {
					latest.ModelStates = map[string]*ModelState{}
					for _, model := range sample.models {
						latest.ModelStates[model] = &ModelState{
							Unavailable: true, Status: StatusError, NextRetryAfter: now.Add(10 * time.Minute),
						}
					}
					stale.ModelStates =
						map[string]*ModelState{sample.models[0]: latest.ModelStates[sample.models[0]].Clone()}
				}
				manager.scheduler.upsertAuthResult(latest, []string{sample.models[len(sample.models)-1]}, false)
				manager.scheduler.upsertAuthResult(stale, []string{sample.models[0]}, sample.credential)
				for _, model := range sample.models {
					state := schedulerShardStateForTest(manager.scheduler, sample.provider, model, sample.id)
					if !state.present || (state.state == scheduledStateReady) != sample.disabled {
						t.Fatalf("latest snapshot model=%s state=%+v", model, state)
					}
				}
			},
		)
	}
}

func TestOriginalTargetedPublicationCredentialRecoveryInputs(t *testing.T) {
	for _, outOfOrder := range []bool{false, true} {
		id, provider, modelA, modelB := "auth-global-recovery-test", "custom-prov-recovery", "m-rec-a", "m-rec-b"
		if outOfOrder {
			id, provider, modelA, modelB = "auth-global-recovery-ooo-test", "custom-prov-recovery-ooo",
				"m-ooo-a", "m-ooo-b"
		}
		t.Run(
			id, func(t *testing.T) {
				manager := NewManager(nil, &RoundRobinSelector{}, nil)
				registerProjectionTestClient(
					t, id, provider, &registry.ModelInfo{ID: modelA}, &registry.ModelInfo{ID: modelB},
				)
				if _, err := manager.Register(
					WithSkipPersist(t.Context()), &Auth{ID: id, Provider: provider, Status: StatusActive},
				); err != nil {
					t.Fatal(err)
				}
				warmSchedulerModels(t, manager, provider, id, modelA, modelB)
				markOriginalCooldownResult(
					t, manager, Result{
						AuthID: id, Provider: provider,
						Error: &Error{
							Code: "unauthorized", Message: "401 Unauthorized", HTTPStatus: http.StatusUnauthorized,
						},
					},
				)
				failed, _ := manager.GetByID(id)
				for _, model := range []string{modelA, modelB} {
					if state := schedulerShardStateForTest(
						manager.scheduler, provider, model, id,
					); !state.present || state.state == scheduledStateReady {
						t.Fatalf("credential failure missed model %s", model)
					}
				}
				markOriginalCooldownResult(
					t, manager, Result{AuthID: id, Provider: provider, Model: modelA, Success: true},
				)
				if state := schedulerShardStateForTest(
					manager.scheduler, provider, modelB, id,
				); state.state == scheduledStateReady {
					t.Fatal("model-only success cleared a credential restriction")
				}
				markOriginalCooldownResult(
					t, manager, Result{
						AuthID: id, Provider: provider, Model: modelA, Success: true,
						Disposition: core.ExecutionDisposition{
							DispatchPhase: core.DispatchPhaseResponseStarted, Outcome: core.ExecutionOutcomeSucceeded,
							RetryDirective: core.RetryDirectiveNoRetry, EffectScope: core.StateEffectScopeCredential,
							EffectKey: id,
						},
					},
				)
				if outOfOrder {
					manager.scheduler.upsertAuthResult(failed, nil, true)
				}
				for _, model := range []string{modelA, modelB} {
					if picked, err := manager.scheduler.pickSingle(
						t.Context(), provider, model, core.Options{}, nil,
					); err != nil || picked == nil || picked.ID != id {
						t.Fatalf("credential recovery model=%s auth=%+v error=%v", model, picked, err)
					}
				}
			},
		)
	}
}
