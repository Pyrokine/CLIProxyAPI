package auth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOriginalSubsecondCooldownPublicationInputs(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	for _, sample := range []struct {
		id, name string
		delay    time.Duration
		direct   bool
	}{
		{id: "auth-gemini-subsecond", name: "model floor", delay: 708 * time.Millisecond},
		{id: "auth-gemini-longer", name: "longer hint", delay: 5 * time.Minute},
		{id: "auth-level-subsecond", name: "credential floor", delay: 708 * time.Millisecond, direct: true},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				const model = "gemini-3.1-flash-image"
				credential := &Auth{ID: sample.id}
				failure := &Error{HTTPStatus: http.StatusTooManyRequests, Message: "RESOURCE_EXHAUSTED"}
				before := time.Now()
				if sample.direct {
					applyAuthFailureState(credential, failure, &sample.delay, before, false)
				} else {
					credential.Provider = "google"
					manager := NewManager(nil, nil, nil)
					registerRequestScopedTestAuth(t, manager, credential, model)
					before = time.Now()
					markOriginalCooldownResult(
						t, manager, Result{
							AuthID: sample.id, Provider: "google", Model: model,
							RetryAfter: &sample.delay, Error: failure,
							Disposition: core.ExecutionDisposition{
								DispatchPhase:  core.DispatchPhaseResponseStarted,
								Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
								RetryDirective: core.RetryDirectiveRetryAfter,
								EffectScope:    core.StateEffectScopeExactModel, EffectKey: model,
							},
						},
					)
					var found bool
					credential, found = manager.GetByID(sample.id)
					if !found {
						t.Fatal("quota credential was not published")
					}
				}
				retry, recoverAt := credential.NextRetryAfter, credential.Quota.NextRecoverAt
				if !sample.direct {
					state := credential.ModelStates[model]
					if state == nil || state.EffectScope != core.StateEffectScopeExactModel ||
						state.ResetAuthority != StateResetAuthoritySuccess {
						t.Fatalf("model quota state=%+v", state)
					}
					retry, recoverAt = state.NextRetryAfter, state.Quota.NextRecoverAt
				}
				want := max(sample.delay, 10*time.Second)
				if retry.Before(before.Add(want)) || retry.After(time.Now().Add(want)) || !retry.Equal(recoverAt) {
					t.Fatalf("retry=%v recover=%v, want %s from publication", retry, recoverAt, want)
				}
			},
		)
	}
}

func TestOriginalSubsecondCooldownPoolInputs(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	for _, slow := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "short hint", true: "expired during round"}[slow], func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				manager.SetRetryConfig(5, 5*time.Second, 6)
				const model = "gemini-3.1-flash-image"
				ids := []string{"auth-storm-1", "auth-storm-2"}
				if slow {
					ids = []string{"auth-slow-1", "auth-slow-2"}
				}
				for _, id := range ids {
					registerRequestScopedTestAuth(
						t, manager, &Auth{ID: id, Provider: "google", Status: StatusActive}, model,
					)
				}
				delay := 708 * time.Millisecond
				failure := &requestScopedDispositionError{
					requestScopedTestError: &requestScopedTestError{
						status: http.StatusTooManyRequests, message: "quota exhausted", retryAfter: &delay,
					},
					disposition: core.ExecutionDisposition{
						DispatchPhase:  core.DispatchPhaseResponseStarted,
						Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
						RetryDirective: core.RetryDirectiveRetryAfter,
						EffectScope:    core.StateEffectScopeExactModel, EffectKey: model,
					},
				}
				var calls []string
				provider := &requestScopedTestExecutor{
					provider: "google", execute: func(selected *Auth) (core.Response, error) {
						calls = append(calls, selected.ID)
						if slow && selected.ID == ids[1] {
							manager.mu.Lock()
							first := manager.auths[ids[0]]
							if first == nil || first.ModelStates[model] == nil {
								manager.mu.Unlock()
								t.Fatal("first credential did not publish its quota state")
							}
							expired := time.Now().Add(-5 * time.Second)
							first.ModelStates[model].NextRetryAfter = expired
							first.ModelStates[model].Quota.NextRecoverAt = expired
							manager.mu.Unlock()
						}
						return core.Response{}, failure
					},
				}
				manager.RegisterExecutor(provider)
				_, err := manager.Execute(t.Context(), []string{"google"}, core.Request{Model: model}, core.Options{})
				if !errors.Is(
					err, failure,
				) || statusCodeFromError(err) != http.StatusTooManyRequests || !reflect.DeepEqual(calls, ids) {
					t.Fatalf("error=%v calls=%v, want original 429 and initial round %v", err, calls, ids)
				}
				if slow {
					current, found := manager.GetByID(ids[0])
					if !found || current.ModelStates[model] == nil ||
						!current.ModelStates[model].NextRetryAfter.Before(time.Now()) {
						t.Fatalf("slow pool did not preserve the expired first credential: %+v", current)
					}
				}
			},
		)
	}
}

func TestOriginalSubsecondCooldownRetryInputs(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	for _, sample := range []struct {
		name, id, model string
		compat          bool
	}{
		{name: "expired attempted credential", id: "auth-attempted-expired", model: "gemini-3.1-flash-image"},
		{name: "large maximum wait", id: "auth-large-maxwait", model: "gemini-3.1-flash-image"},
		{name: "provider cooling override", id: "auth-compat-override", model: "compat-model-override", compat: true},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				expired := time.Now().Add(-5 * time.Second)
				credential := &Auth{
					ID: sample.id, Provider: "google", ModelStates: map[string]*ModelState{
						sample.model: {
							Status: StatusError, Unavailable: true, NextRetryAfter: expired,
							Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: expired},
						},
					},
				}
				provider := "google"
				if sample.compat {
					credential.Provider = "openai-compatibility"
					credential.Attributes = map[string]string{"provider_key": "custom-llm"}
					provider = credential.Provider
				}
				registerRequestScopedTestAuth(t, manager, credential, sample.model)
				manager.RegisterExecutor(&roundExecutor{provider: provider})
				state := &retryRoundState{defaultRetry: 5, attempted: make(map[string]struct{})}
				ctx := context.WithValue(t.Context(), retryRoundContextKey{}, state)
				wait, found := manager.closestCooldownWait(
					ctx, core.Options{}, []string{provider}, sample.model, 0, http.StatusTooManyRequests,
				)
				if !found || wait != 0 {
					t.Fatalf("untried expired credential wait=%v found=%t, want ready", wait, found)
				}
				state.attempted[sample.id] = struct{}{}
				wait, found = manager.closestCooldownWait(
					ctx, core.Options{}, []string{provider}, sample.model, 0, http.StatusTooManyRequests,
				)
				if !found || wait < 10*time.Second {
					t.Fatalf("attempted 429 wait=%v found=%t, want at least 10 s", wait, found)
				}
				failure := errorWithState(
					&Error{HTTPStatus: http.StatusTooManyRequests, Message: "RESOURCE_EXHAUSTED"}, errorState{
						DispatchPhase:  core.DispatchPhaseResponseStarted,
						Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
						RetryDirective: core.RetryDirectiveRetryAfter, EffectScope: core.StateEffectScopeExactModel,
						EffectKey: sample.model,
					},
				)
				if wait, retry := manager.shouldRetryAfterError(
					ctx, core.Options{}, failure, 0, []string{provider}, sample.model, 30*time.Second,
				); !retry || wait < 10*time.Second {
					t.Fatalf("large maximum wait=%v retry=%t, want positive 429 cooldown", wait, retry)
				}
				if sample.compat {
					manager.SetConfig(
						&config.Config{
							OpenAICompatibility: []config.OpenAICompatibility{
								{
									Name: "custom-llm", DisableCooling: new(true),
								},
							},
						},
					)
					wait, found = manager.closestCooldownWait(
						ctx, core.Options{}, []string{provider}, sample.model, 0, http.StatusTooManyRequests,
					)
					if !found || wait != 0 {
						t.Fatalf("provider cooling override wait=%v found=%t, want immediate retry", wait, found)
					}
				}
			},
		)
	}
}
