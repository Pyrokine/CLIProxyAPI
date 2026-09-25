package auth

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type resultPolicyRecordingHook struct {
	NoopHook
	last atomic.Pointer[Result]
}

func (hook *resultPolicyRecordingHook) OnResult(_ context.Context, result Result) {
	hook.last.Store(&result)
}

type resultPolicyRecordingStore struct {
	mu    sync.Mutex
	saved []*Auth
}

func (*resultPolicyRecordingStore) List(context.Context) ([]*Auth, error) {
	return nil, nil
}

func (store *resultPolicyRecordingStore) Save(_ context.Context, auth *Auth) (string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.saved = append(store.saved, auth.Clone())
	return "", nil
}

func (*resultPolicyRecordingStore) Delete(context.Context, string) error {
	return nil
}

func (store *resultPolicyRecordingStore) snapshot() []*Auth {
	store.mu.Lock()
	defer store.mu.Unlock()
	result := make([]*Auth, 0, len(store.saved))
	for _, auth := range store.saved {
		result = append(result, auth.Clone())
	}
	return result
}

func TestManagerMarkResultResultPolicyUsesObservedGenerationBeforeMutationPersistenceAndPublication(t *testing.T) {
	const (
		authID   = "result-policy-auth"
		provider = "result-policy-provider"
		model    = "result-policy-model"
		sibling  = "result-policy-sibling"
	)
	store := &resultPolicyRecordingStore{}
	hook := &resultPolicyRecordingHook{}
	manager := NewManager(store, nil, hook)
	registered, errRegister := manager.Register(
		WithSkipPersist(context.Background()),
		&Auth{
			ID:              authID,
			Provider:        provider,
			Status:          StatusActive,
			StateGeneration: 4,
			Metadata:        map[string]any{"token": "test"},
			ModelStates: map[string]*ModelState{
				model:   {Status: StatusActive, Generation: 7},
				sibling: {Status: StatusActive, Generation: 3},
			},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	cooldownStore := &recordingCooldownStateStore{}
	manager.SetCooldownStateStore(cooldownStore)
	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(
		authID,
		provider,
		[]*registry.ModelInfo{{ID: model}, {ID: sibling}},
	)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	var policyInvoked atomic.Bool
	var stateUnchanged atomic.Bool
	manager.SetResultPolicy(
		ResultPolicyFunc(func(ctx context.Context, result Result) Result {
			policyInvoked.Store(ctx != nil)
			current, exists := manager.GetByID(authID)
			if exists && current != nil && !current.Unavailable &&
				current.ModelStates[model] != nil && !current.ModelStates[model].Unavailable &&
				current.ModelStates[model].Generation == 7 {
				stateUnchanged.Store(true)
			}

			result.AuthID = "different-auth"
			result.AuthGeneration = 0
			result.RuntimeOnly = true
			result.Provider = "different-provider"
			result.Model = sibling
			result.ModelQuotaEpoch = 0
			result.StateGeneration = 0
			result.Disposition.EffectScope = cliproxyexecutor.StateEffectScopeExactModel
			result.Disposition.EffectKey = sibling
			return result
		}),
	)

	first := executionResult(
		registered,
		provider,
		model,
		executionOutcomeStatusError{status: http.StatusForbidden},
		cliproxyexecutor.DispatchPhaseMaybeSent,
	)
	stale := executionResult(
		registered,
		provider,
		model,
		executionOutcomeStatusError{status: http.StatusForbidden},
		cliproxyexecutor.DispatchPhaseMaybeSent,
	)
	if first.StateGeneration != 4 || first.generationSnapshot.exactModelStateGeneration != 7 {
		t.Fatalf(
			"result generations before policy = selected:%d exact-model:%d",
			first.StateGeneration,
			first.generationSnapshot.exactModelStateGeneration,
		)
	}

	//lint:ignore SA1012 verifies MarkResult normalizes a nil context before policy execution
	manager.MarkResult(nil, first)
	if !policyInvoked.Load() || !stateUnchanged.Load() {
		t.Fatal("ResultPolicy did not run with a non-nil context before runtime mutation")
	}

	updated := getExecutionOutcomeTestAuth(t, manager, authID)
	targetState := updated.ModelStates[model]
	if targetState == nil || !targetState.Unavailable || targetState.Generation != 8 ||
		targetState.EffectScope != cliproxyexecutor.StateEffectScopeExactModel ||
		targetState.EffectKey != model {
		t.Fatalf("target model state after policy = %+v", targetState)
	}
	if siblingState := updated.ModelStates[sibling]; siblingState == nil ||
		siblingState.Unavailable || siblingState.Generation != 3 {
		t.Fatalf("sibling model state after policy = %+v", siblingState)
	}
	if updated.Unavailable || updated.StateGeneration != 4 || updated.Failed != 1 {
		t.Fatalf("credential state after policy = %+v", updated)
	}

	if picked, errPick := manager.scheduler.pickSingle(
		context.Background(), provider, sibling, cliproxyexecutor.Options{}, nil,
	); errPick != nil || picked == nil || picked.ID != authID {
		t.Fatalf("scheduler sibling selection = picked:%#v error:%v", picked, errPick)
	}
	if picked, errPick := manager.scheduler.pickSingle(
		context.Background(), provider, model, cliproxyexecutor.Options{}, nil,
	); picked != nil || errPick == nil {
		t.Fatalf("scheduler selected policy-blocked model: picked=%#v error=%v", picked, errPick)
	}
	if available, errAvailable := modelRegistry.GetFirstAvailableModel(provider); errAvailable != nil ||
		available != sibling {
		t.Fatalf("registry available model after policy = %q, %v", available, errAvailable)
	}

	saved := store.snapshot()
	if len(saved) != 1 || saved[0].ID != authID || saved[0].ModelStates[model] == nil ||
		!saved[0].ModelStates[model].Unavailable || saved[0].ModelStates[sibling].Unavailable {
		t.Fatalf("persisted auth after policy = %+v", saved)
	}
	cooldownStore.mu.Lock()
	cooldownRecords := cloneCooldownStateRecords(cooldownStore.records)
	cooldownSaves := cooldownStore.saveCount.Load()
	cooldownStore.mu.Unlock()
	if cooldownSaves != 1 || len(cooldownRecords) != 1 || cooldownRecords[0].Model != model ||
		cooldownRecords[0].EffectScope != cliproxyexecutor.StateEffectScopeExactModel ||
		cooldownRecords[0].StateGeneration != 8 {
		t.Fatalf("persisted cooldown records after policy = saves:%d records:%+v", cooldownSaves, cooldownRecords)
	}

	received := hook.last.Load()
	if received == nil || received.AuthID != authID || received.AuthGeneration != registered.Generation ||
		received.RuntimeOnly || received.Provider != provider || received.Model != model ||
		received.StateGeneration != 7 || received.Disposition.EffectScope != cliproxyexecutor.StateEffectScopeExactModel ||
		received.Disposition.EffectKey != model || received.Error == nil || received.Error.StateGeneration != 7 {
		t.Fatalf("hook result after policy = %+v", received)
	}

	manager.MarkResult(context.Background(), stale)
	stillUpdated := getExecutionOutcomeTestAuth(t, manager, authID)
	if stillUpdated.ModelStates[model].Generation != 8 || stillUpdated.Failed != 1 ||
		len(store.snapshot()) != 1 || cooldownStore.saveCount.Load() != 1 {
		t.Fatalf(
			"stale policy result changed state: auth=%+v saves=%d cooldown-saves=%d",
			stillUpdated,
			len(store.snapshot()),
			cooldownStore.saveCount.Load(),
		)
	}
}

func TestManagerMarkResultResultPolicyPreservesQuotaFamilyGenerationAndEpoch(t *testing.T) {
	const family = "result-policy-family"
	manager, registered := newExecutionOutcomeTestManager(
		t,
		&Auth{
			ID:              "result-policy-family-auth",
			Provider:        "provider-a",
			Status:          StatusActive,
			StateGeneration: 4,
			ModelStates: map[string]*ModelState{
				"model-a": {
					Status:     StatusActive,
					Generation: 5,
					QuotaEpoch: 7,
					Quota:      QuotaState{Family: family, Generation: 9},
				},
				"model-b": {
					Status:     StatusActive,
					Generation: 6,
					QuotaEpoch: 7,
					Quota:      QuotaState{Family: family, Generation: 9},
				},
			},
		},
	)
	manager.SetResultPolicy(
		ResultPolicyFunc(func(_ context.Context, result Result) Result {
			result.ModelQuotaEpoch = 0
			result.StateGeneration = 0
			result.Disposition.EffectScope = cliproxyexecutor.StateEffectScopeQuotaFamily
			result.Disposition.EffectKey = "different-family"
			result.Error.HTTPStatus = http.StatusTooManyRequests
			return result
		}),
	)

	first := executionResult(
		registered,
		registered.Provider,
		"model-a",
		executionOutcomeStatusError{status: http.StatusForbidden},
		cliproxyexecutor.DispatchPhaseMaybeSent,
	)
	stale := executionResult(
		registered,
		registered.Provider,
		"model-a",
		executionOutcomeStatusError{status: http.StatusForbidden},
		cliproxyexecutor.DispatchPhaseMaybeSent,
	)
	if first.generationSnapshot.quotaFamily != family ||
		first.generationSnapshot.quotaFamilyGeneration != 9 || first.ModelQuotaEpoch != 7 {
		t.Fatalf("captured family generation = %+v epoch=%d", first.generationSnapshot, first.ModelQuotaEpoch)
	}

	manager.MarkResult(context.Background(), first)
	updated := getExecutionOutcomeTestAuth(t, manager, registered.ID)
	for _, model := range []string{"model-a", "model-b"} {
		state := updated.ModelStates[model]
		if state == nil || !state.Unavailable || !state.Quota.Exceeded || state.Quota.Family != family ||
			state.Quota.Generation != 10 || state.QuotaEpoch != 8 ||
			state.EffectScope != cliproxyexecutor.StateEffectScopeQuotaFamily || state.EffectKey != family {
			t.Fatalf("quota family state for %s = %+v", model, state)
		}
	}
	if updated.Failed != 1 {
		t.Fatalf("failure count after family policy = %d, want 1", updated.Failed)
	}

	manager.MarkResult(context.Background(), stale)
	stillUpdated := getExecutionOutcomeTestAuth(t, manager, registered.ID)
	for _, model := range []string{"model-a", "model-b"} {
		state := stillUpdated.ModelStates[model]
		if state.Quota.Generation != 10 || state.QuotaEpoch != 8 {
			t.Fatalf("stale family result changed %s = %+v", model, state)
		}
	}
	if stillUpdated.Failed != 1 {
		t.Fatalf("stale family result failure count = %d, want 1", stillUpdated.Failed)
	}
}

func TestManagerMarkResultResultPolicyDropAndClear(t *testing.T) {
	store := &resultPolicyRecordingStore{}
	hook := &resultPolicyRecordingHook{}
	manager := NewManager(store, nil, hook)
	registered, errRegister := manager.Register(
		WithSkipPersist(context.Background()),
		&Auth{
			ID:       "result-policy-drop",
			Provider: "provider-a",
			Status:   StatusActive,
			Metadata: map[string]any{"token": "test"},
		},
	)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}

	var calls atomic.Int32
	manager.SetResultPolicy(
		ResultPolicyFunc(func(_ context.Context, result Result) Result {
			calls.Add(1)
			result.AuthID = ""
			return result
		}),
	)
	manager.MarkResult(context.Background(), Result{})
	if calls.Load() != 0 {
		t.Fatalf("policy calls for initially empty AuthID = %d", calls.Load())
	}
	manager.MarkResult(
		context.Background(),
		executionResult(
			registered,
			registered.Provider,
			"model-a",
			executionOutcomeStatusError{status: http.StatusForbidden},
			cliproxyexecutor.DispatchPhaseMaybeSent,
		),
	)
	if calls.Load() != 1 || hook.last.Load() != nil || len(store.snapshot()) != 0 {
		t.Fatalf(
			"dropped result side effects = calls:%d hook:%+v saves:%d",
			calls.Load(),
			hook.last.Load(),
			len(store.snapshot()),
		)
	}
	if current := getExecutionOutcomeTestAuth(t, manager, registered.ID); current.Failed != 0 || current.Unavailable {
		t.Fatalf("dropped result changed auth = %+v", current)
	}

	manager.SetResultPolicy(nil)
	if manager.ResultPolicy() != nil {
		t.Fatal("ResultPolicy() remained set after SetResultPolicy(nil)")
	}
	manager.MarkResult(
		context.Background(),
		executionResult(
			registered,
			registered.Provider,
			"model-a",
			executionOutcomeStatusError{status: http.StatusForbidden},
			cliproxyexecutor.DispatchPhaseMaybeSent,
		),
	)
	if current := getExecutionOutcomeTestAuth(t, manager, registered.ID); current.Failed != 1 || !current.Unavailable {
		t.Fatalf("result after clearing policy = %+v", current)
	}
}

func TestManagerMarkResultResultPolicyCannotAuthorizeUnsafeReplay(t *testing.T) {
	cases := []struct {
		name        string
		disposition cliproxyexecutor.ExecutionDisposition
		wantPhase   cliproxyexecutor.DispatchPhase
	}{
		{
			name: "unknown outcome",
			disposition: cliproxyexecutor.ExecutionDisposition{
				DispatchPhase:  cliproxyexecutor.DispatchPhaseMaybeSent,
				Outcome:        cliproxyexecutor.ExecutionOutcomeUnknown,
				RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
				EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
			},
			wantPhase: cliproxyexecutor.DispatchPhaseMaybeSent,
		},
		{
			name: "failed outcome",
			disposition: cliproxyexecutor.ExecutionDisposition{
				DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
				Outcome:        cliproxyexecutor.ExecutionOutcomeFailed,
				RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
				EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
			},
			wantPhase: cliproxyexecutor.DispatchPhaseResponseStarted,
		},
		{
			name: "downstream committed",
			disposition: cliproxyexecutor.ExecutionDisposition{
				DispatchPhase:  cliproxyexecutor.DispatchPhaseDownstreamCommitted,
				Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
				RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
				EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
			},
			wantPhase: cliproxyexecutor.DispatchPhaseDownstreamCommitted,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			hook := &resultPolicyRecordingHook{}
			manager := NewManager(nil, nil, hook)
			registered, errRegister := manager.Register(
				WithSkipPersist(context.Background()),
				&Auth{ID: "result-policy-replay-" + testCase.name, Provider: "provider-a", Status: StatusActive},
			)
			if errRegister != nil {
				t.Fatalf("Register() error = %v", errRegister)
			}
			manager.SetResultPolicy(
				ResultPolicyFunc(func(_ context.Context, result Result) Result {
					result.Disposition.DispatchPhase = cliproxyexecutor.DispatchPhaseNotSent
					result.Disposition.Outcome = cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing
					result.Disposition.RetryDirective = cliproxyexecutor.RetryDirectiveNextAuth
					return result
				}),
			)
			manager.MarkResult(
				context.Background(),
				Result{
					AuthID:         registered.ID,
					AuthGeneration: registered.Generation,
					Provider:       registered.Provider,
					Model:          "model-a",
					Error:          &Error{Message: "request failed"},
					Disposition:    testCase.disposition,
				},
			)
			received := hook.last.Load()
			if received == nil || received.Disposition.DispatchPhase != testCase.wantPhase ||
				received.Disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry {
				t.Fatalf("policy replay disposition = %+v", received)
			}
		})
	}
}

func TestManagerResultPolicyConcurrentSwap(t *testing.T) {
	manager, registered := newExecutionOutcomeTestManager(
		t,
		&Auth{ID: "result-policy-concurrent", Provider: "provider-a", Status: StatusActive},
	)
	var calls atomic.Int64
	policy := ResultPolicyFunc(func(_ context.Context, result Result) Result {
		calls.Add(1)
		return result
	})
	manager.SetResultPolicy(policy)
	manager.MarkResult(
		context.Background(),
		Result{AuthID: registered.ID, Provider: registered.Provider, Model: "model-a", Success: true},
	)
	if calls.Load() != 1 {
		t.Fatalf("initial ResultPolicy calls = %d, want 1", calls.Load())
	}

	const workers = 30
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < 20; worker++ {
		go func() {
			defer wait.Done()
			for attempt := 0; attempt < 10; attempt++ {
				manager.MarkResult(
					context.Background(),
					Result{AuthID: registered.ID, Provider: registered.Provider, Model: "model-a", Success: true},
				)
			}
		}()
	}
	for worker := 0; worker < 10; worker++ {
		worker := worker
		go func() {
			defer wait.Done()
			for attempt := 0; attempt < 10; attempt++ {
				if (worker+attempt)%2 == 0 {
					manager.SetResultPolicy(policy)
				} else {
					manager.SetResultPolicy(nil)
				}
				_ = manager.ResultPolicy()
			}
		}()
	}
	wait.Wait()
	if calls.Load() == 0 {
		t.Fatal("ResultPolicy was never invoked during concurrent MarkResult calls")
	}
}
