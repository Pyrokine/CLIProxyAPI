package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type schedulerShardTestState struct {
	meta       *scheduledAuthMeta
	state      scheduledState
	lastPicked string
	present    bool
}

func schedulerShardStateForTest(
	scheduler *authScheduler,
	provider,
	model,
	authID string,
) schedulerShardTestState {
	if scheduler == nil {
		return schedulerShardTestState{}
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	providerState := scheduler.providers[provider]
	if providerState == nil {
		return schedulerShardTestState{}
	}
	shard := providerState.modelShards[canonicalModelKey(model)]
	if shard == nil {
		return schedulerShardTestState{}
	}
	entry := shard.entries[authID]
	if entry == nil {
		return schedulerShardTestState{}
	}
	lastPicked := ""
	if bucket := shard.readyByPriority[entry.meta.priority]; bucket != nil {
		lastPicked = bucket.all.lastPickedAuth
	}
	return schedulerShardTestState{
		meta:       entry.meta,
		state:      entry.state,
		lastPicked: lastPicked,
		present:    true,
	}
}

func warmSchedulerModels(
	t *testing.T,
	manager *Manager,
	provider,
	authID string,
	models ...string,
) {
	t.Helper()
	for _, model := range models {
		picked, errPick := manager.scheduler.pickSingle(
			context.Background(), provider, model, cliproxyexecutor.Options{}, nil,
		)
		if errPick != nil || picked == nil || picked.ID != authID {
			t.Fatalf("pickSingle(%q) = (%#v, %v), want auth %q", model, picked, errPick, authID)
		}
	}
}

func TestManagerMarkResultTargetsSchedulerModelShards(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	const (
		authID   = "targeted-scheduler-auth"
		provider = "targeted-scheduler-provider"
		modelA   = "targeted-scheduler-model-a"
		modelB   = "targeted-scheduler-model-b"
		modelC   = "targeted-scheduler-model-c"
		modelD   = "targeted-scheduler-model-d"
	)
	registerProjectionTestClient(
		t,
		authID,
		provider,
		&registry.ModelInfo{ID: modelA},
		&registry.ModelInfo{ID: modelB},
		&registry.ModelInfo{ID: modelC},
		&registry.ModelInfo{ID: modelD},
	)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Provider: provider, Status: StatusActive,
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	warmSchedulerModels(t, manager, provider, authID, modelA, modelB, modelC)
	beforeB := schedulerShardStateForTest(manager.scheduler, provider, modelB, authID)
	beforeC := schedulerShardStateForTest(manager.scheduler, provider, modelC, authID)

	failure := executionResult(
		registered,
		provider,
		modelA,
		&Error{HTTPStatus: http.StatusNotFound, Code: "model_not_found", Message: "model not found"},
		cliproxyexecutor.DispatchPhaseResponseStarted,
	)
	manager.MarkResult(context.Background(), failure)

	afterA := schedulerShardStateForTest(manager.scheduler, provider, modelA, authID)
	afterB := schedulerShardStateForTest(manager.scheduler, provider, modelB, authID)
	afterC := schedulerShardStateForTest(manager.scheduler, provider, modelC, authID)
	if !afterA.present || afterA.state == scheduledStateReady {
		t.Fatalf("target model shard = %#v, want blocked state", afterA)
	}
	if afterB.meta != beforeB.meta || afterC.meta != beforeC.meta {
		t.Fatal("exact-model result replaced metadata in an unrelated shard")
	}
	if afterB.state != scheduledStateReady || afterC.state != scheduledStateReady {
		t.Fatalf("unrelated shard states = (%v, %v), want ready", afterB.state, afterC.state)
	}
	if afterB.lastPicked != beforeB.lastPicked || afterC.lastPicked != beforeC.lastPicked {
		t.Fatal("exact-model result changed an unrelated shard cursor")
	}

	current, ok := manager.GetByID(authID)
	if !ok || current == nil {
		t.Fatal("auth missing after MarkResult()")
	}
	if blocked, _, _ := isAuthBlockedForModel(current, modelB, time.Now()); blocked {
		t.Fatal("aggregate exact-model state blocked a clean sibling model")
	}
	selectable, _ := manager.currentSelectableAuth(
		authID,
		modelB,
		map[string]struct{}{provider: {}},
		nil,
		nil,
	)
	if selectable == nil {
		t.Fatal("runtime revalidation rejected the clean sibling model")
	}
	picked, errPick := manager.scheduler.pickSingle(
		context.Background(), provider, modelD, cliproxyexecutor.Options{}, nil,
	)
	if errPick != nil || picked == nil || picked.ID != authID {
		t.Fatalf("lazy clean sibling shard = (%#v, %v), want auth %q", picked, errPick, authID)
	}

	success := executionResult(
		current,
		provider,
		modelA,
		nil,
		cliproxyexecutor.DispatchPhaseResponseStarted,
	)
	manager.MarkResult(context.Background(), success)
	afterSuccessA := schedulerShardStateForTest(manager.scheduler, provider, modelA, authID)
	afterSuccessB := schedulerShardStateForTest(manager.scheduler, provider, modelB, authID)
	afterSuccessC := schedulerShardStateForTest(manager.scheduler, provider, modelC, authID)
	if !afterSuccessA.present || afterSuccessA.state != scheduledStateReady {
		t.Fatalf("target model success left shard in state %#v", afterSuccessA)
	}
	if afterSuccessB.meta != beforeB.meta || afterSuccessC.meta != beforeC.meta {
		t.Fatal("exact-model success replaced metadata in an unrelated shard")
	}
}

func TestManagerMarkResultTargetsWholeQuotaFamily(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	const (
		authID   = "targeted-family-auth"
		provider = "targeted-family-provider"
		modelA   = "targeted-family-model-a"
		modelB   = "targeted-family-model-b"
		modelC   = "targeted-family-model-c"
		family   = "targeted-family"
	)
	registerProjectionTestClient(
		t,
		authID,
		provider,
		&registry.ModelInfo{ID: modelA},
		&registry.ModelInfo{ID: modelB},
		&registry.ModelInfo{ID: modelC},
	)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Provider: provider, Status: StatusActive,
		ModelStates: map[string]*ModelState{
			modelA: {Status: StatusActive, Quota: QuotaState{Family: family, Generation: 1}},
			modelB: {Status: StatusActive, Quota: QuotaState{Family: family, Generation: 1}},
			modelC: {Status: StatusActive},
		},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	warmSchedulerModels(t, manager, provider, authID, modelA, modelB, modelC)
	beforeC := schedulerShardStateForTest(manager.scheduler, provider, modelC, authID)

	retryAfter := time.Hour
	failure := executionResult(
		registered,
		provider,
		modelA,
		&Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"},
		cliproxyexecutor.DispatchPhaseResponseStarted,
	)
	failure.RetryAfter = &retryAfter
	manager.MarkResult(context.Background(), failure)

	for _, model := range []string{modelA, modelB} {
		state := schedulerShardStateForTest(manager.scheduler, provider, model, authID)
		if !state.present || state.state == scheduledStateReady {
			t.Fatalf("quota-family shard %q = %#v, want blocked", model, state)
		}
	}
	afterFailureC := schedulerShardStateForTest(manager.scheduler, provider, modelC, authID)
	if afterFailureC.meta != beforeC.meta || afterFailureC.state != scheduledStateReady {
		t.Fatalf("unrelated quota-family shard changed: before=%#v after=%#v", beforeC, afterFailureC)
	}

	current, ok := manager.GetByID(authID)
	if !ok || current == nil {
		t.Fatal("auth missing after quota-family failure")
	}
	success := executionResult(
		current,
		provider,
		modelA,
		nil,
		cliproxyexecutor.DispatchPhaseResponseStarted,
	)
	manager.MarkResult(context.Background(), success)
	for _, model := range []string{modelA, modelB} {
		state := schedulerShardStateForTest(manager.scheduler, provider, model, authID)
		if !state.present || state.state != scheduledStateReady {
			t.Fatalf("quota-family success left shard %q in state %#v", model, state)
		}
	}
	afterSuccessC := schedulerShardStateForTest(manager.scheduler, provider, modelC, authID)
	if afterSuccessC.meta != beforeC.meta {
		t.Fatal("quota-family success replaced unrelated shard metadata")
	}
}

func TestManagerCredentialResultUpdatesEverySchedulerShard(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	const (
		authID   = "targeted-credential-auth"
		provider = "targeted-credential-provider"
		modelA   = "targeted-credential-model-a"
		modelB   = "targeted-credential-model-b"
	)
	registerProjectionTestClient(
		t,
		authID,
		provider,
		&registry.ModelInfo{ID: modelA},
		&registry.ModelInfo{ID: modelB},
	)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Provider: provider, Status: StatusActive,
		ModelStates: map[string]*ModelState{
			modelA: {Status: StatusActive},
			modelB: {Status: StatusActive},
		},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	warmSchedulerModels(t, manager, provider, authID, modelA, modelB)
	beforeA := schedulerShardStateForTest(manager.scheduler, provider, modelA, authID)
	beforeB := schedulerShardStateForTest(manager.scheduler, provider, modelB, authID)

	disposition := cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
		Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: cliproxyexecutor.RetryDirectiveRetryAfter,
		EffectScope:    cliproxyexecutor.StateEffectScopeCredential,
		EffectKey:      authID,
	}
	retryAfter := time.Hour
	manager.MarkResult(context.Background(), Result{
		AuthID:          authID,
		AuthGeneration:  registered.Generation,
		Provider:        provider,
		Model:           modelA,
		Error:           &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential quota"},
		RetryAfter:      &retryAfter,
		Disposition:     disposition,
		StateGeneration: stateGenerationForDisposition(registered, modelA, disposition),
	})

	afterA := schedulerShardStateForTest(manager.scheduler, provider, modelA, authID)
	afterB := schedulerShardStateForTest(manager.scheduler, provider, modelB, authID)
	if afterA.meta == beforeA.meta || afterB.meta == beforeB.meta {
		t.Fatal("credential-scoped result did not refresh every shard")
	}
	if afterA.state == scheduledStateReady || afterB.state == scheduledStateReady {
		t.Fatalf("credential-scoped shard states = (%v, %v), want blocked", afterA.state, afterB.state)
	}
}

func TestSchedulerTargetedResultUsesLatestSnapshotWhenIncomingIsStale(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	const (
		authID   = "targeted-stale-auth"
		provider = "targeted-stale-provider"
		modelA   = "targeted-stale-model-a"
		modelB   = "targeted-stale-model-b"
	)
	registerProjectionTestClient(
		t,
		authID,
		provider,
		&registry.ModelInfo{ID: modelA},
		&registry.ModelInfo{ID: modelB},
	)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Provider: provider, Status: StatusActive,
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	warmSchedulerModels(t, manager, provider, authID, modelA, modelB)
	baseTime := time.Now()
	future := baseTime.Add(time.Hour)

	latest := registered.Clone()
	latest.UpdatedAt = baseTime.Add(time.Second)
	latest.ModelStates = map[string]*ModelState{
		modelA: {Status: StatusActive, UpdatedAt: latest.UpdatedAt},
		modelB: {
			Status: StatusError, Unavailable: true, NextRetryAfter: future, UpdatedAt: latest.UpdatedAt,
		},
	}
	manager.scheduler.upsertAuthResult(latest, []string{modelB}, false)

	stale := registered.Clone()
	stale.UpdatedAt = baseTime
	stale.ModelStates = map[string]*ModelState{
		modelA: {Status: StatusError, Unavailable: true, NextRetryAfter: future, UpdatedAt: stale.UpdatedAt},
	}
	manager.scheduler.upsertAuthResult(stale, []string{modelA}, false)

	stateA := schedulerShardStateForTest(manager.scheduler, provider, modelA, authID)
	stateB := schedulerShardStateForTest(manager.scheduler, provider, modelB, authID)
	if stateA.state != scheduledStateReady {
		t.Fatalf("stale model result regressed model A to state %v", stateA.state)
	}
	if stateB.state == scheduledStateReady {
		t.Fatalf(
			"latest model B failure was lost: shard=%#v auth=%#v model=%#v",
			stateB,
			stateB.meta.auth,
			stateB.meta.auth.ModelStates[modelB],
		)
	}

	stale.Disabled = true
	stale.Status = StatusDisabled
	manager.scheduler.upsertAuthResult(stale, []string{modelA}, false)
	if picked, errPick := manager.scheduler.pickSingle(
		context.Background(), provider, modelA, cliproxyexecutor.Options{}, nil,
	); errPick != nil || picked == nil || picked.ID != authID {
		t.Fatalf("stale disabled result removed current auth: picked=%#v error=%v", picked, errPick)
	}
}

func TestSchedulerTargetedResultReusesRegisteredModelSet(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	const (
		authID   = "targeted-model-set-auth"
		provider = "targeted-model-set-provider"
		modelA   = "targeted-model-set-a"
		modelB   = "targeted-model-set-b"
		sentinel = "targeted-model-set-sentinel"
	)
	registerProjectionTestClient(
		t,
		authID,
		provider,
		&registry.ModelInfo{ID: modelA},
		&registry.ModelInfo{ID: modelB},
	)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Provider: provider, Status: StatusActive,
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	warmSchedulerModels(t, manager, provider, authID, modelA)
	manager.scheduler.mu.Lock()
	manager.scheduler.providers[provider].auths[authID].supportedModelSet[sentinel] = struct{}{}
	manager.scheduler.mu.Unlock()

	latest := registered.Clone()
	latest.UpdatedAt = time.Now()
	manager.scheduler.upsertAuthResult(latest, []string{modelA}, false)
	manager.scheduler.mu.Lock()
	_, reused := manager.scheduler.providers[provider].auths[authID].supportedModelSet[sentinel]
	manager.scheduler.mu.Unlock()
	if !reused {
		t.Fatal("targeted result reloaded an unchanged registered model set")
	}
}

func TestSchedulerRegistryEpochRefreshesExistingShardsFromLatestAuth(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	const (
		authID   = "targeted-epoch-auth"
		otherID  = "targeted-epoch-other-auth"
		provider = "targeted-epoch-provider"
		modelA   = "targeted-epoch-model-a"
		modelB   = "targeted-epoch-model-b"
	)
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: modelA}})
	reg.RegisterClient(otherID, provider, []*registry.ModelInfo{{ID: modelB}})
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
		reg.UnregisterClient(otherID)
	})
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Provider: provider, Status: StatusActive,
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	if _, errOther := manager.Register(context.Background(), &Auth{
		ID: otherID, Provider: provider, Status: StatusActive,
	}); errOther != nil {
		t.Fatalf("Register(other) error = %v", errOther)
	}
	warmSchedulerModels(t, manager, provider, authID, modelA)
	warmSchedulerModels(t, manager, provider, otherID, modelB)
	if before := schedulerShardStateForTest(manager.scheduler, provider, modelB, authID); before.present {
		t.Fatal("new model shard already contained auth before registry update")
	}

	latest := registered.Clone()
	latest.UpdatedAt = time.Now().Add(time.Hour)
	manager.scheduler.upsertAuthResult(latest, []string{modelA}, false)
	reg.RegisterClient(authID, provider, []*registry.ModelInfo{{ID: modelA}, {ID: modelB}})
	_, currentEpoch := reg.GetModelsAndEpochForClient(authID)

	manager.scheduler.upsertAuth(registered)
	after := schedulerShardStateForTest(manager.scheduler, provider, modelB, authID)
	if !after.present || after.state != scheduledStateReady {
		t.Fatalf("registry epoch refresh did not add auth to existing model B shard: %#v", after)
	}
	manager.scheduler.mu.Lock()
	meta := manager.scheduler.providers[provider].auths[authID]
	manager.scheduler.mu.Unlock()
	if meta == nil || meta.registryEpoch != currentEpoch || !meta.auth.UpdatedAt.Equal(latest.UpdatedAt) {
		t.Fatalf("refreshed scheduler metadata = %#v, want epoch %d and latest auth", meta, currentEpoch)
	}
}

func TestSchedulerTargetedResultUpdatesEmptyModelShard(t *testing.T) {
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	const (
		authID   = "targeted-empty-auth"
		provider = "targeted-empty-provider"
		model    = "targeted-empty-model"
	)
	registerProjectionTestClient(t, authID, provider, &registry.ModelInfo{ID: model})
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Provider: provider, Status: StatusActive,
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	warmSchedulerModels(t, manager, provider, authID, "", model)
	before := schedulerShardStateForTest(manager.scheduler, provider, "", authID)

	failure := executionResult(
		registered,
		provider,
		model,
		&Error{HTTPStatus: http.StatusNotFound, Code: "model_not_found", Message: "model not found"},
		cliproxyexecutor.DispatchPhaseResponseStarted,
	)
	manager.MarkResult(context.Background(), failure)
	after := schedulerShardStateForTest(manager.scheduler, provider, "", authID)
	if after.meta == before.meta {
		t.Fatal("targeted result did not refresh the empty-model shard")
	}
	if after.state == scheduledStateReady {
		t.Fatalf("empty-model shard state = %v, want aggregate blocked state", after.state)
	}
}
