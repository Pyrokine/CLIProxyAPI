package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func registerProjectionTestClient(
	t *testing.T,
	authID,
	provider string,
	models ...*registry.ModelInfo,
) *registry.ModelRegistry {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, provider, models)
	t.Cleanup(func() {
		reg.UnregisterClient(authID)
	})
	return reg
}

func TestClientModelProjectionUsesRegisteredRouteIdentity(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	auth := &Auth{
		ID:       "projection-route-identity-auth",
		Provider: "codex",
		Prefix:   "team",
		ModelStates: map[string]*ModelState{
			"upstream-model": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: future,
			},
			"team/route-alias": {
				Status:         StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: future,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: future,
				},
			},
		},
	}

	base := clientModelProjectionForAuth(auth, "route-alias", now)
	if base.Suspended || base.QuotaExceeded {
		t.Fatalf("base route projection = %#v, want independent clean state", base)
	}
	prefixed := clientModelProjectionForAuth(auth, "team/route-alias", now)
	if !prefixed.Suspended || !prefixed.QuotaExceeded || prefixed.SuspendReason != "quota" {
		t.Fatalf("prefixed route projection = %#v, want route-key quota state", prefixed)
	}

	expiredCredentialQuota := clientModelProjectionForAuth(&Auth{
		ID:               "projection-expired-credential-quota-auth",
		Status:           StatusError,
		Unavailable:      true,
		NextRetryAfter:   now.Add(-time.Minute),
		Quota:            QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(-time.Minute)},
		StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
	}, "route-alias", now)
	if expiredCredentialQuota.Suspended || expiredCredentialQuota.QuotaExceeded {
		t.Fatalf("expired credential quota projection = %#v, want current routing eligibility", expiredCredentialQuota)
	}

	cooldownDisabled := clientModelProjectionForAuth(&Auth{
		ID: "projection-cooling-disabled-auth",
		ModelStates: map[string]*ModelState{
			"route-alias": {
				Status: StatusError,
				Quota:  QuotaState{Exceeded: true, Reason: "quota"},
			},
		},
	}, "route-alias", now)
	if cooldownDisabled.Suspended || cooldownDisabled.QuotaExceeded {
		t.Fatalf("cooldown-disabled projection = %#v, want quota state without routing block", cooldownDisabled)
	}
}

func TestManagerMarkResultPublishesWholeClientProjection(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	const (
		authID      = "mark-result-whole-projection-auth"
		provider    = "mark-result-whole-projection-provider"
		failedModel = "mark-result-failed-model"
		cleanModel  = "mark-result-clean-model"
	)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Provider: provider, Status: StatusActive,
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	reg := registerProjectionTestClient(
		t,
		authID,
		provider,
		&registry.ModelInfo{ID: failedModel},
		&registry.ModelInfo{ID: cleanModel},
	)
	reg.SuspendClientModel(authID, cleanModel, "stale")
	reg.SetModelQuotaExceeded(authID, cleanModel)

	failure := executionResult(
		registered,
		provider,
		failedModel,
		&Error{HTTPStatus: http.StatusNotFound, Code: "model_not_found", Message: "model not found"},
		cliproxyexecutor.DispatchPhaseResponseStarted,
	)
	manager.MarkResult(context.Background(), failure)

	if !reg.IsModelSuspendedForClient(authID, failedModel) {
		t.Fatal("failed route model was not suspended")
	}
	if reg.IsModelQuotaExceededForClient(authID, failedModel) {
		t.Fatal("exact-model failure was published as quota exhaustion")
	}
	if reg.IsModelSuspendedForClient(authID, cleanModel) ||
		reg.IsModelQuotaExceededForClient(authID, cleanModel) {
		t.Fatal("whole-client projection retained stale state on an unaffected model")
	}
	current, ok := manager.GetByID(authID)
	if !ok || current == nil {
		t.Fatal("auth missing after MarkResult()")
	}
	if current.Generation != registered.Generation {
		t.Fatalf("MarkResult() changed auth generation from %d to %d", registered.Generation, current.Generation)
	}
	manager.mu.RLock()
	projectionGeneration := manager.authGenerationSeq
	manager.mu.RUnlock()
	if projectionGeneration <= registered.Generation {
		t.Fatalf(
			"registry projection generation = %d, want above auth generation %d",
			projectionGeneration,
			registered.Generation,
		)
	}

	_, epoch := reg.GetModelsAndEpochForClient(authID)
	if reg.ApplyClientModelProjections(authID, epoch, registered.Generation, []registry.ClientModelProjection{
		{ModelID: failedModel},
		{ModelID: cleanModel},
	}) {
		t.Fatal("registry accepted a stale clean projection")
	}
	if !reg.IsModelSuspendedForClient(authID, failedModel) {
		t.Fatal("stale clean projection resumed the failed model")
	}

	success := executionResult(
		current,
		provider,
		failedModel,
		nil,
		cliproxyexecutor.DispatchPhaseResponseStarted,
	)
	manager.MarkResult(context.Background(), success)
	if reg.IsModelSuspendedForClient(authID, failedModel) ||
		reg.IsModelQuotaExceededForClient(authID, failedModel) {
		t.Fatal("serialized success did not clear the exact-model projection")
	}
	current, ok = manager.GetByID(authID)
	if !ok || current == nil || current.Generation != registered.Generation {
		t.Fatalf("serialized success changed auth identity: %#v", current)
	}
	manager.mu.RLock()
	successProjectionGeneration := manager.authGenerationSeq
	manager.mu.RUnlock()
	if successProjectionGeneration <= projectionGeneration {
		t.Fatalf(
			"success projection generation = %d, want above failure generation %d",
			successProjectionGeneration,
			projectionGeneration,
		)
	}
}

func TestManagerMarkResultPublishesWholeQuotaFamily(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	const (
		authID   = "mark-result-family-projection-auth"
		provider = "mark-result-family-projection-provider"
		modelA   = "mark-result-family-model-a"
		modelB   = "mark-result-family-model-b"
		family   = "mark-result-family"
	)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Provider: provider, Status: StatusActive,
		ModelStates: map[string]*ModelState{
			modelA: {Status: StatusActive, Quota: QuotaState{Family: family, Generation: 1}},
			modelB: {Status: StatusActive, Quota: QuotaState{Family: family, Generation: 1}},
		},
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	reg := registerProjectionTestClient(
		t,
		authID,
		provider,
		&registry.ModelInfo{ID: modelA},
		&registry.ModelInfo{ID: modelB},
	)

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
		if !reg.IsModelSuspendedForClient(authID, model) ||
			!reg.IsModelQuotaExceededForClient(authID, model) {
			t.Fatalf("quota-family model %q did not receive the complete projection", model)
		}
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
		if reg.IsModelSuspendedForClient(authID, model) ||
			reg.IsModelQuotaExceededForClient(authID, model) {
			t.Fatalf("quota-family success did not clear model %q", model)
		}
	}
}

func TestManagerMarkResultPublishesCredentialQuotaAcrossRegisteredModels(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	const (
		authID   = "mark-result-credential-projection-auth"
		provider = "mark-result-credential-projection-provider"
		modelA   = "mark-result-credential-model-a"
		modelB   = "mark-result-credential-model-b"
	)
	registered, errRegister := manager.Register(context.Background(), &Auth{
		ID: authID, Provider: provider, Status: StatusActive,
	})
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	reg := registerProjectionTestClient(
		t,
		authID,
		provider,
		&registry.ModelInfo{ID: modelA},
		&registry.ModelInfo{ID: modelB},
	)
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
	for _, model := range []string{modelA, modelB} {
		if !reg.IsModelSuspendedForClient(authID, model) {
			t.Fatalf("credential quota did not suspend registered model %q", model)
		}
		if reg.IsModelQuotaExceededForClient(authID, model) {
			t.Fatalf("credential quota was published as model quota for %q", model)
		}
	}

	failed, ok := manager.GetByID(authID)
	if !ok || failed == nil || !hasCredentialQuotaCooldownState(failed) {
		t.Fatalf("credential quota state = %#v", failed)
	}
	recovered, errRecover := manager.RecoverCredentialQuota(context.Background(), authID)
	if errRecover != nil {
		t.Fatalf("RecoverCredentialQuota() error = %v", errRecover)
	}
	if recovered == nil || recovered.Generation <= failed.Generation {
		t.Fatalf("recovered auth = %#v, want newer generation", recovered)
	}
	for _, model := range []string{modelA, modelB} {
		if reg.IsModelSuspendedForClient(authID, model) ||
			reg.IsModelQuotaExceededForClient(authID, model) {
			t.Fatalf("credential quota recovery did not restore model %q", model)
		}
	}
}

func TestClientModelProjectionMatchesRoutingBlocks(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Minute)
	for _, test := range []struct {
		name string
		auth *Auth
	}{
		{name: "credential non-quota", auth: &Auth{
			Status: StatusError, Unavailable: true, NextRetryAfter: future,
			StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
			LastError:        &Error{HTTPStatus: http.StatusUnauthorized},
			ModelStates:      map[string]*ModelState{"other-model": {Status: StatusActive}},
		}},
		{name: "error credential scope", auth: &Auth{
			Status: StatusError, Unavailable: true, NextRetryAfter: future,
			LastError:   &Error{HTTPStatus: http.StatusUnauthorized, EffectScope: cliproxyexecutor.StateEffectScopeCredential},
			ModelStates: map[string]*ModelState{"other-model": {Status: StatusActive}},
		}},
		{name: "expired credential", auth: &Auth{
			Status: StatusError, Unavailable: true, NextRetryAfter: past,
			StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
		}},
		{name: "active exact model", auth: &Auth{ModelStates: map[string]*ModelState{
			"route-model": {Status: StatusError, Unavailable: true, NextRetryAfter: future},
		}}},
		{name: "expired exact model", auth: &Auth{ModelStates: map[string]*ModelState{
			"route-model": {Status: StatusError, Unavailable: true, NextRetryAfter: past},
		}}},
		{name: "disabled credential", auth: &Auth{Disabled: true}},
		{name: "cooling disabled", auth: &Auth{ModelStates: map[string]*ModelState{
			"route-model": {Status: StatusError, Quota: QuotaState{Exceeded: true, Reason: "quota"}},
		}}},
		{name: "clean credential", auth: &Auth{Status: StatusActive}},
	} {
		t.Run(test.name, func(t *testing.T) {
			blocked, _, _ := isAuthBlockedForModel(test.auth, "route-model", now)
			projection := clientModelProjectionForAuth(test.auth, "route-model", now)
			if projection.Suspended != blocked {
				t.Fatalf("projection suspended=%t, routing blocked=%t", projection.Suspended, blocked)
			}
			if projection.QuotaExceeded {
				t.Fatalf("non-quota routing block became quota exhaustion: %+v", projection)
			}
		})
	}
}

func TestManagerMarkResultPublishesCredentialFailureAcrossRegisteredModels(t *testing.T) {
	for _, existingModelState := range []bool{false, true} {
		name := "without model state"
		if existingModelState {
			name = "with unrelated model state"
		}
		t.Run(name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			const authID = "credential-failure-projection-auth"
			const provider = "credential-failure-projection-provider"
			const modelA, modelB = "credential-failure-model-a", "credential-failure-model-b"
			auth := &Auth{ID: authID, Provider: provider, Status: StatusActive}
			if existingModelState {
				auth.ModelStates = map[string]*ModelState{modelB: {Status: StatusActive}}
			}
			registered, errRegister := manager.Register(context.Background(), auth)
			if errRegister != nil {
				t.Fatalf("Register: %v", errRegister)
			}
			reg := registerProjectionTestClient(t, authID, provider,
				&registry.ModelInfo{ID: modelA}, &registry.ModelInfo{ID: modelB})
			failure := executionResult(registered, provider, modelA,
				&Error{HTTPStatus: http.StatusUnauthorized, Message: "temporary unauthorized"},
				cliproxyexecutor.DispatchPhaseResponseStarted)
			manager.MarkResult(context.Background(), failure)
			current, ok := manager.GetByID(authID)
			if !ok || current.StateEffectScope != cliproxyexecutor.StateEffectScopeCredential ||
				!current.NextRetryAfter.After(time.Now()) {
				t.Fatalf("credential failure state missing: %+v", current)
			}
			for _, model := range []string{modelA, modelB} {
				blocked, _, _ := isAuthBlockedForModel(current, model, time.Now())
				if !blocked || !reg.IsModelSuspendedForClient(authID, model) {
					t.Fatalf("model %s: routing blocked=%t, registry suspended=%t", model,
						blocked, reg.IsModelSuspendedForClient(authID, model))
				}
				if reg.IsModelQuotaExceededForClient(authID, model) {
					t.Fatalf("credential authentication failure became quota exhaustion for %s", model)
				}
			}
			manager.mu.Lock()
			generation := manager.nextAuthGenerationLocked()
			manager.mu.Unlock()
			manager.publishClientModelProjections(current, generation, current.NextRetryAfter.Add(time.Second))
			for _, model := range []string{modelA, modelB} {
				if reg.IsModelSuspendedForClient(authID, model) {
					t.Fatalf("expired credential cooldown still suspended %s", model)
				}
			}
		})
	}
}
