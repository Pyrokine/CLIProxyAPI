package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type executionOutcomeStatusError struct {
	status     int
	retryAfter *time.Duration
}

func (e executionOutcomeStatusError) Error() string {
	return http.StatusText(e.status)
}

func (e executionOutcomeStatusError) StatusCode() int {
	return e.status
}

func (e executionOutcomeStatusError) RetryAfter() *time.Duration {
	return e.retryAfter
}

func TestExecutionDispositionForAttemptClassifiesReplayAndScope(t *testing.T) {
	retryAfter := time.Minute
	auth := &Auth{ID: "auth-1", Provider: "provider-a"}
	cases := []struct {
		name        string
		err         error
		phase       cliproxyexecutor.DispatchPhase
		wantPhase   cliproxyexecutor.DispatchPhase
		wantOutcome cliproxyexecutor.ExecutionOutcome
		wantRetry   cliproxyexecutor.RetryDirective
		wantScope   cliproxyexecutor.StateEffectScope
		wantKey     string
	}{
		{
			name: "prepare failure", err: context.Canceled,
			phase:       cliproxyexecutor.DispatchPhaseNotSent,
			wantPhase:   cliproxyexecutor.DispatchPhaseNotSent,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeUnknown,
			wantRetry:   cliproxyexecutor.RetryDirectiveNoRetry,
			wantScope:   cliproxyexecutor.StateEffectScopeRequest,
		},
		{
			name: "transport outcome unknown", err: context.DeadlineExceeded,
			phase:       cliproxyexecutor.DispatchPhaseMaybeSent,
			wantPhase:   cliproxyexecutor.DispatchPhaseMaybeSent,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeUnknown,
			wantRetry:   cliproxyexecutor.RetryDirectiveNoRetry,
			wantScope:   cliproxyexecutor.StateEffectScopeRequest,
		},
		{
			name: "credential rejected", err: executionOutcomeStatusError{status: http.StatusUnauthorized},
			phase:       cliproxyexecutor.DispatchPhaseMaybeSent,
			wantPhase:   cliproxyexecutor.DispatchPhaseResponseStarted,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveSameAuthAfterRefresh,
			wantScope:   cliproxyexecutor.StateEffectScopeCredential,
			wantKey:     auth.ID,
		},
		{
			name: "quota family rejected", err: executionOutcomeStatusError{
				status: http.StatusTooManyRequests, retryAfter: &retryAfter,
			},
			phase:       cliproxyexecutor.DispatchPhaseMaybeSent,
			wantPhase:   cliproxyexecutor.DispatchPhaseResponseStarted,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveRetryAfter,
			wantScope:   cliproxyexecutor.StateEffectScopeQuotaFamily,
			wantKey:     "model-a",
		},
		{
			name: "provider outcome unknown", err: executionOutcomeStatusError{status: http.StatusServiceUnavailable},
			phase:       cliproxyexecutor.DispatchPhaseMaybeSent,
			wantPhase:   cliproxyexecutor.DispatchPhaseResponseStarted,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeUnknown,
			wantRetry:   cliproxyexecutor.RetryDirectiveNoRetry,
			wantScope:   cliproxyexecutor.StateEffectScopeProvider,
			wantKey:     auth.Provider,
		},
	}

	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				disposition := executionDispositionForAttempt(tc.err, tc.phase, auth, auth.Provider, "model-a")
				if disposition.DispatchPhase != tc.wantPhase || disposition.Outcome != tc.wantOutcome ||
					disposition.RetryDirective != tc.wantRetry || disposition.EffectScope != tc.wantScope ||
					disposition.EffectKey != tc.wantKey {
					t.Fatalf("execution disposition = %+v", disposition)
				}
			},
		)
	}
}

func TestManagerMarkResultRejectsStaleCredentialGeneration(t *testing.T) {
	manager, before := newExecutionOutcomeTestManager(
		t, &Auth{
			ID: "credential-auth", Provider: "provider-a", Status: StatusActive,
		},
	)
	failure := executionResult(
		before,
		before.Provider,
		"model-a",
		executionOutcomeStatusError{status: http.StatusUnauthorized},
		cliproxyexecutor.DispatchPhaseMaybeSent,
	)
	manager.MarkResult(context.Background(), failure)

	afterFailure := getExecutionOutcomeTestAuth(t, manager, before.ID)
	if !afterFailure.Unavailable || afterFailure.StateGeneration != failure.StateGeneration+1 ||
		afterFailure.StateEffectScope != cliproxyexecutor.StateEffectScopeCredential {
		t.Fatalf("credential state after failure = %+v", afterFailure)
	}

	staleSuccess := Result{
		AuthID: before.ID, AuthGeneration: before.Generation, Provider: before.Provider, Model: "model-a",
		Success: true,
		Disposition: cliproxyexecutor.ExecutionDisposition{
			DispatchPhase:  cliproxyexecutor.DispatchPhaseDownstreamCommitted,
			Outcome:        cliproxyexecutor.ExecutionOutcomeSucceeded,
			RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
			EffectScope:    cliproxyexecutor.StateEffectScopeCredential,
			EffectKey:      before.ID,
		},
		StateGeneration: failure.StateGeneration,
	}
	manager.MarkResult(context.Background(), staleSuccess)
	stillFailed := getExecutionOutcomeTestAuth(t, manager, before.ID)
	if !stillFailed.Unavailable || stillFailed.StateGeneration != afterFailure.StateGeneration {
		t.Fatalf("stale success changed credential state: %+v", stillFailed)
	}

	freshSuccess := executionResult(
		afterFailure, afterFailure.Provider, "model-a", nil,
		cliproxyexecutor.DispatchPhaseDownstreamCommitted,
	)
	manager.MarkResult(context.Background(), freshSuccess)
	cleared := getExecutionOutcomeTestAuth(t, manager, before.ID)
	if cleared.Unavailable || cleared.LastError != nil ||
		cleared.StateGeneration != afterFailure.StateGeneration+1 {
		t.Fatalf("fresh success did not clear credential state: %+v", cleared)
	}
}

func TestManagerMarkResultKeepsInFlightQuotaAfterLabelUpdate(t *testing.T) {
	manager, selected := newExecutionOutcomeTestManager(t, &Auth{
		ID: "in-flight-label", Provider: "provider-a", Status: StatusActive,
		Metadata: map[string]any{"access_token": "test-token"},
	})
	retryAfter := time.Hour
	failure := executionResult(
		selected, selected.Provider, "model-a",
		executionOutcomeStatusError{status: http.StatusTooManyRequests, retryAfter: &retryAfter},
		cliproxyexecutor.DispatchPhaseMaybeSent,
	)

	renamed := selected.Clone()
	renamed.Label = "new label"
	updated, err := manager.Update(WithSkipPersist(context.Background()), renamed)
	if err != nil || updated == nil || updated.Generation == selected.Generation ||
		updated.credentialGeneration != selected.credentialGeneration {
		t.Fatalf("Update(label) = (%#v, %v)", updated, err)
	}
	manager.SetResultPolicy(ResultPolicyFunc(func(_ context.Context, result Result) Result {
		result.AuthGeneration = 0
		result.generationSnapshot = resultGenerationSnapshot{}
		return result
	}))
	manager.MarkResult(context.Background(), failure)

	current := getExecutionOutcomeTestAuth(t, manager, selected.ID)
	state := current.ModelStates["model-a"]
	if current.Label != "new label" || current.Failed != 1 || state == nil ||
		!state.Quota.Exceeded || !state.Unavailable {
		t.Fatalf("in-flight 429 after label update = %#v", current)
	}
}

func TestManagerMarkResultCountsInFlightRequestFaultAfterLabelUpdate(t *testing.T) {
	manager, selected := newExecutionOutcomeTestManager(t, &Auth{
		ID: "in-flight-neutral-label", Provider: "provider-a", Status: StatusActive,
		Metadata: map[string]any{"access_token": "test-token"},
	})
	result := executionResult(
		selected, selected.Provider, "model-a", context.Canceled,
		cliproxyexecutor.DispatchPhaseMaybeSent,
	)
	renamed := selected.Clone()
	renamed.Label = "new label"
	if _, err := manager.Update(WithSkipPersist(context.Background()), renamed); err != nil {
		t.Fatalf("Update(label) error = %v", err)
	}
	manager.MarkResult(context.Background(), result)

	current := getExecutionOutcomeTestAuth(t, manager, selected.ID)
	if current.Failed != 1 || current.Unavailable || len(current.ModelStates) != 0 {
		t.Fatalf("request fault after label update = %#v", current)
	}
}

func TestManagerMarkResultRejectsOldResultAfterSameTokenRecreation(t *testing.T) {
	manager, selected := newExecutionOutcomeTestManager(t, &Auth{
		ID: "same-token-recreation", Provider: "provider-a", Status: StatusActive,
		Metadata: map[string]any{"access_token": "test-token"},
	})
	failure := executionResult(
		selected, selected.Provider, "model-a",
		executionOutcomeStatusError{status: http.StatusTooManyRequests},
		cliproxyexecutor.DispatchPhaseMaybeSent,
	)
	if err := manager.Remove(WithSkipPersist(context.Background()), selected.ID); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	recreated, err := manager.Register(WithAllowAuthRecreate(context.Background()), &Auth{
		ID: selected.ID, Provider: selected.Provider, Status: StatusActive,
		Metadata: map[string]any{"access_token": "test-token"},
	})
	if err != nil || recreated == nil || recreated.credentialGeneration == selected.credentialGeneration {
		t.Fatalf("Register(recreate) = (%#v, %v)", recreated, err)
	}
	manager.MarkResult(context.Background(), failure)

	current := getExecutionOutcomeTestAuth(t, manager, selected.ID)
	if current.Failed != 0 || len(current.ModelStates) != 0 {
		t.Fatalf("old result changed recreated credential: %#v", current)
	}
}

func TestManagerMarkResultRejectsStaleExactModelGeneration(t *testing.T) {
	manager, before := newExecutionOutcomeTestManager(
		t, &Auth{
			ID: "model-auth", Provider: "provider-a", Status: StatusActive,
		},
	)
	failure := executionResult(
		before,
		before.Provider,
		"model-a",
		executionOutcomeStatusError{status: http.StatusNotFound},
		cliproxyexecutor.DispatchPhaseMaybeSent,
	)
	manager.MarkResult(context.Background(), failure)

	afterFailure := getExecutionOutcomeTestAuth(t, manager, before.ID)
	state := afterFailure.ModelStates["model-a"]
	if state == nil || !state.Unavailable || state.Generation != failure.StateGeneration+1 ||
		state.EffectScope != cliproxyexecutor.StateEffectScopeExactModel {
		t.Fatalf("model state after failure = %+v", state)
	}

	staleSuccess := Result{
		AuthID: before.ID, AuthGeneration: before.Generation, Provider: before.Provider, Model: "model-a",
		Success:         true,
		ModelQuotaEpoch: failure.ModelQuotaEpoch,
		Disposition: cliproxyexecutor.ExecutionDisposition{
			DispatchPhase:  cliproxyexecutor.DispatchPhaseDownstreamCommitted,
			Outcome:        cliproxyexecutor.ExecutionOutcomeSucceeded,
			RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
			EffectScope:    cliproxyexecutor.StateEffectScopeExactModel,
			EffectKey:      "model-a",
		},
		StateGeneration: failure.StateGeneration,
	}
	manager.MarkResult(context.Background(), staleSuccess)
	stillFailed := getExecutionOutcomeTestAuth(t, manager, before.ID).ModelStates["model-a"]
	if stillFailed == nil || !stillFailed.Unavailable || stillFailed.Generation != state.Generation {
		t.Fatalf("stale success changed model state: %+v", stillFailed)
	}

	current := getExecutionOutcomeTestAuth(t, manager, before.ID)
	manager.MarkResult(
		context.Background(), executionResult(
			current, current.Provider, "model-a", nil,
			cliproxyexecutor.DispatchPhaseDownstreamCommitted,
		),
	)
	cleared := getExecutionOutcomeTestAuth(t, manager, before.ID).ModelStates["model-a"]
	if cleared == nil || cleared.Unavailable || cleared.LastError != nil || cleared.Generation != state.Generation+1 {
		t.Fatalf("fresh success did not clear model state: %+v", cleared)
	}
}

func TestManagerMarkResultKeepsQuotaFamilyAtomicAndDecaysBackoff(t *testing.T) {
	const family = "shared-family"
	manager, before := newExecutionOutcomeTestManager(
		t, &Auth{
			ID: "quota-auth", Provider: "provider-a", Status: StatusActive,
			ModelStates: map[string]*ModelState{
				"model-a": {Status: StatusActive, Generation: 1, Quota: QuotaState{Family: family, Generation: 1}},
				"model-b": {Status: StatusActive, Generation: 1, Quota: QuotaState{Family: family, Generation: 1}},
			},
		},
	)
	retryAfter := time.Minute
	firstFailure := executionResult(
		before,
		before.Provider,
		"model-a",
		executionOutcomeStatusError{status: http.StatusTooManyRequests, retryAfter: &retryAfter},
		cliproxyexecutor.DispatchPhaseMaybeSent,
	)
	manager.MarkResult(context.Background(), firstFailure)

	afterFirstFailure := getExecutionOutcomeTestAuth(t, manager, before.ID)
	assertExecutionOutcomeQuotaFamily(t, afterFirstFailure, family, true, 1, 2)

	staleSuccess := Result{
		AuthID: before.ID, AuthGeneration: before.Generation, Provider: before.Provider, Model: "model-a",
		Success:         true,
		ModelQuotaEpoch: firstFailure.ModelQuotaEpoch,
		Disposition: cliproxyexecutor.ExecutionDisposition{
			DispatchPhase:  cliproxyexecutor.DispatchPhaseDownstreamCommitted,
			Outcome:        cliproxyexecutor.ExecutionOutcomeSucceeded,
			RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
			EffectScope:    cliproxyexecutor.StateEffectScopeQuotaFamily,
			EffectKey:      family,
		},
		StateGeneration: firstFailure.StateGeneration,
	}
	manager.MarkResult(context.Background(), staleSuccess)
	stillFailed := getExecutionOutcomeTestAuth(t, manager, before.ID)
	assertExecutionOutcomeQuotaFamily(t, stillFailed, family, true, 1, 2)

	manager.mu.Lock()
	for _, state := range manager.auths[before.ID].ModelStates {
		state.NextRetryAfter = time.Time{}
		state.Quota.NextRecoverAt = time.Time{}
	}
	manager.mu.Unlock()
	stillFailed = getExecutionOutcomeTestAuth(t, manager, before.ID)

	secondFailure := executionResult(
		stillFailed,
		stillFailed.Provider,
		"model-b",
		executionOutcomeStatusError{status: http.StatusTooManyRequests},
		cliproxyexecutor.DispatchPhaseMaybeSent,
	)
	manager.MarkResult(context.Background(), secondFailure)
	afterSecondFailure := getExecutionOutcomeTestAuth(t, manager, before.ID)
	assertExecutionOutcomeQuotaFamily(t, afterSecondFailure, family, true, 2, 3)

	freshSuccess := executionResult(
		afterSecondFailure, afterSecondFailure.Provider, "model-a", nil,
		cliproxyexecutor.DispatchPhaseDownstreamCommitted,
	)
	manager.MarkResult(context.Background(), freshSuccess)
	cleared := getExecutionOutcomeTestAuth(t, manager, before.ID)
	assertExecutionOutcomeQuotaFamily(t, cleared, family, false, 1, 4)
	if cleared.Quota.Exceeded {
		t.Fatalf("aggregate quota remained exceeded: %+v", cleared.Quota)
	}
}

func TestQuotaRefreshRecoveryAdvancesEpochAndRejectsOlder429(t *testing.T) {
	const family = "shared-family"
	manager, before := newExecutionOutcomeTestManager(
		t, &Auth{
			ID: "quota-refresh-auth", Provider: "codex", Status: StatusError, Unavailable: true,
			ModelStates: map[string]*ModelState{
				"model-a": {
					Status: StatusError, Unavailable: true,
					LastError: &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"},
					Quota: QuotaState{
						Exceeded: true, Family: family, Generation: 4,
						ResetAuthority: StateResetAuthoritySuccess,
					},
					EffectScope:    cliproxyexecutor.StateEffectScopeQuotaFamily,
					EffectKey:      family,
					ResetAuthority: StateResetAuthoritySuccess,
					Generation:     4,
					UpdatedAt:      time.Now().Add(-time.Minute),
					QuotaEpoch:     7,
				},
			},
		},
	)
	staleFailure := executionResult(
		before,
		before.Provider,
		"model-a",
		executionOutcomeStatusError{status: http.StatusTooManyRequests},
		cliproxyexecutor.DispatchPhaseMaybeSent,
	)
	candidates := QuotaFamilyRecoveryCandidates(before)
	if len(candidates) != 1 || candidates[0].ExpectedGeneration != 4 {
		t.Fatalf("recovery candidates = %#v", candidates)
	}
	startedAt := time.Now().UTC()
	recovered, errRecover := manager.RecoverQuotaAfterRefresh(
		context.Background(), before.ID, startedAt, candidates,
	)
	if errRecover != nil {
		t.Fatalf("RecoverQuotaAfterRefresh() error = %v", errRecover)
	}
	state := recovered.ModelStates["model-a"]
	if state == nil || state.Quota.Exceeded || state.Unavailable || state.QuotaEpoch != 8 ||
		state.Quota.Generation != 5 {
		t.Fatalf("recovered state = %#v", state)
	}

	manager.MarkResult(context.Background(), staleFailure)
	afterStale := getExecutionOutcomeTestAuth(t, manager, before.ID).ModelStates["model-a"]
	if afterStale == nil || afterStale.Quota.Exceeded || afterStale.Unavailable || afterStale.QuotaEpoch != 8 {
		t.Fatalf("older 429 restored recovered cooldown = %#v", afterStale)
	}
}

func TestManagerMarkResultKeepsRequestGroupAndProviderScopesAvailabilityNeutral(t *testing.T) {
	manager, current := newExecutionOutcomeTestManager(
		t, &Auth{
			ID: "neutral-auth", Provider: "provider-a", Status: StatusActive,
		},
	)
	scopes := []cliproxyexecutor.StateEffectScope{
		cliproxyexecutor.StateEffectScopeRequest,
		cliproxyexecutor.StateEffectScopeGroup,
		cliproxyexecutor.StateEffectScopeProvider,
	}
	for _, scope := range scopes {
		manager.MarkResult(
			context.Background(), Result{
				AuthID: current.ID, AuthGeneration: current.Generation, Provider: current.Provider, Model: "model-a",
				Disposition: cliproxyexecutor.ExecutionDisposition{
					DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
					Outcome:        cliproxyexecutor.ExecutionOutcomeUnknown,
					RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
					EffectScope:    scope,
					EffectKey:      "scope-key",
				},
				Error: &Error{Message: "availability-neutral failure"},
			},
		)
	}

	updated := getExecutionOutcomeTestAuth(t, manager, current.ID)
	if updated.Unavailable || updated.LastError != nil || updated.StateEffectScope != "" ||
		len(updated.ModelStates) != 0 || updated.Failed != int64(len(scopes)) {
		t.Fatalf("availability-neutral result changed runtime availability: %+v", updated)
	}
}

func TestStateResetAuthorityControlsClearing(t *testing.T) {
	now := time.Now()
	state := &ModelState{
		Status: StatusError, Unavailable: true,
		ResetAuthority: StateResetAuthorityReset, Generation: 4,
		Quota: QuotaState{
			Exceeded: true, Family: "family-a", Generation: 6,
			ResetAuthority: StateResetAuthorityReset,
		},
	}
	if clearModelStateOnSuccess(state, now) {
		t.Fatal("same-generation success cleared reset-authoritative state")
	}
	if !clearModelState(state, now, StateResetAuthorityReset) {
		t.Fatal("reset did not clear reset-authoritative state")
	}
	if state.Unavailable || state.Quota.Exceeded || state.Generation != 5 || state.Quota.Generation != 7 {
		t.Fatalf("state after reset = %+v", state)
	}

	state.Unavailable = true
	state.Status = StatusError
	state.ResetAuthority = StateResetAuthorityOperator
	state.Quota.Exceeded = true
	state.Quota.ResetAuthority = StateResetAuthorityOperator
	if clearModelState(state, now, StateResetAuthorityReset) {
		t.Fatal("reset cleared operator-authoritative state")
	}
	if !clearModelState(state, now, StateResetAuthorityOperator) {
		t.Fatal("operator action did not clear operator-authoritative state")
	}
}

func TestUpdateAggregatedAvailabilityProjectsMultipleQuotaFamilies(t *testing.T) {
	now := time.Now()
	earlier := now.Add(time.Minute)
	later := now.Add(2 * time.Minute)
	auth := &Auth{
		ModelStates: map[string]*ModelState{
			"model-a": {
				Status: StatusError, Unavailable: true, NextRetryAfter: later,
				Quota: QuotaState{
					Exceeded: true, Family: "family-a", NextRecoverAt: later,
					BackoffLevel: 1, Generation: 3, ResetAuthority: StateResetAuthoritySuccess,
				},
			},
			"model-b": {
				Status: StatusError, Unavailable: true, NextRetryAfter: earlier,
				Quota: QuotaState{
					Exceeded: true, Family: "family-b", NextRecoverAt: earlier,
					BackoffLevel: 2, Generation: 5, ResetAuthority: StateResetAuthorityOperator,
				},
			},
		},
	}

	updateAggregatedAvailability(auth, now)
	if !auth.Quota.Exceeded || auth.Quota.Family != "" || auth.Quota.Generation != 5 ||
		auth.Quota.BackoffLevel != 2 || auth.Quota.ResetAuthority != StateResetAuthorityOperator ||
		!auth.Quota.NextRecoverAt.Equal(earlier) {
		t.Fatalf("aggregate quota = %+v", auth.Quota)
	}
}

func newExecutionOutcomeTestManager(t *testing.T, auth *Auth) (*Manager, *Auth) {
	t.Helper()
	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	return manager, getExecutionOutcomeTestAuth(t, manager, auth.ID)
}

func getExecutionOutcomeTestAuth(t *testing.T, manager *Manager, authID string) *Auth {
	t.Helper()
	auth, ok := manager.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("auth %q not found", authID)
	}
	return auth
}

func assertExecutionOutcomeQuotaFamily(
	t *testing.T,
	auth *Auth,
	family string,
	exceeded bool,
	backoffLevel int,
	generation uint64,
) {
	t.Helper()
	for _, model := range []string{"model-a", "model-b"} {
		state := auth.ModelStates[model]
		if state == nil || state.Quota.Exceeded != exceeded || state.Quota.Family != family ||
			state.Quota.BackoffLevel != backoffLevel || state.Quota.Generation != generation {
			t.Fatalf("quota state for %s = %+v", model, state)
		}
		if state.Unavailable != exceeded {
			t.Fatalf("model state for %s unavailable = %t, want %t", model, state.Unavailable, exceeded)
		}
	}
}
