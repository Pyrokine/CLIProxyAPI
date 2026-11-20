package auth

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func newOriginalCooldownMonotonicManager(t *testing.T, models ...string) (*Manager, *Auth) {
	t.Helper()
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-monotonic-" + models[0], Provider: "claude"}
	infos := make([]*registry.ModelInfo, 0, len(models))
	now := time.Now().Unix()
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model, Created: now})
	}
	registerProjectionTestClient(t, auth.ID, auth.Provider, infos...)
	registered, err := m.Register(context.Background(), auth)
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	return m, registered
}

func originalCredentialQuotaDisposition(authID string) cliproxyexecutor.ExecutionDisposition {
	return cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
		Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: cliproxyexecutor.RetryDirectiveRetryAfter,
		EffectScope:    cliproxyexecutor.StateEffectScopeCredential,
		EffectKey:      authID,
	}
}

func markOriginalCooldownResult(t *testing.T, m *Manager, result Result) {
	t.Helper()
	current := getExecutionOutcomeTestAuth(t, m, result.AuthID)
	result.AuthGeneration = current.Generation
	result.ModelQuotaEpoch = modelQuotaEpoch(current, result.Model)
	result.Disposition = normalizeResultDisposition(result)
	result.StateGeneration = stateGenerationForDisposition(current, result.Model, result.Disposition)
	m.MarkResult(context.Background(), result)
}

func assertOriginalCooldownRouting(t *testing.T, auth *Auth, model string, at time.Time, wantBlocked bool) {
	t.Helper()
	blocked, _, _ := isAuthBlockedForModel(auth, model, at)
	projection := clientModelProjectionForAuth(auth, model, at)
	if blocked != wantBlocked || projection.Suspended != wantBlocked {
		t.Fatalf(
			"%s at %v: blocked=%t projection=%+v, want blocked=%t",
			model,
			at,
			blocked,
			projection,
			wantBlocked,
		)
	}
}

func TestOriginalManager_MarkResult_CredentialScopeKeepsLongerSiblingDeadline(t *testing.T) {
	m, auth := newOriginalCooldownMonotonicManager(t, "model-a", "model-b")
	markOriginalCooldownResult(
		t, m, Result{
			AuthID: auth.ID, Provider: auth.Provider, Model: "model-b",
			Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: "long 401"},
		},
	)
	before := time.Now()
	first := getExecutionOutcomeTestAuth(t, m, auth.ID)
	if first.StateEffectScope != cliproxyexecutor.StateEffectScopeCredential ||
		first.NextRetryAfter.Before(before.Add(25*time.Minute)) || len(first.ModelStates) != 0 {
		t.Fatalf("401 credential deadline missing: %+v", first)
	}
	short := 5 * time.Minute
	markOriginalCooldownResult(
		t, m, Result{
			AuthID: auth.ID, Provider: auth.Provider, Model: "model-a", RetryAfter: &short,
			Disposition: originalCredentialQuotaDisposition(auth.ID),
			Error:       &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential 429"},
		},
	)
	updated := getExecutionOutcomeTestAuth(t, m, auth.ID)
	if !updated.NextRetryAfter.Equal(first.NextRetryAfter) || updated.StateGeneration != first.StateGeneration+1 ||
		!updated.Quota.Exceeded || updated.Quota.NextRecoverAt.After(before.Add(10*time.Minute)) ||
		updated.Quota.NextRecoverAt.Before(before.Add(short)) ||
		len(updated.ModelStates) != 0 {
		t.Fatalf("short quota changed credential deadline or created model mirrors: %+v", updated)
	}
	for _, model := range []string{"model-a", "model-b"} {
		assertOriginalCooldownRouting(t, updated, model, before.Add(6*time.Minute), true)
		assertOriginalCooldownRouting(t, updated, model, before.Add(31*time.Minute), false)
		if !registry.GetGlobalRegistry().IsModelSuspendedForClient(auth.ID, model) {
			t.Fatalf("401 credential cooldown was not published for %s", model)
		}
	}
}

func TestOriginalManager_MarkResult_CredentialScope_DoesNotPromoteSiblingDeadlineToQuota(t *testing.T) {
	m, auth := newOriginalCooldownMonotonicManager(t, "model-a", "model-b")
	markOriginalCooldownResult(
		t, m, Result{
			AuthID: auth.ID, Provider: auth.Provider, Model: "model-b",
			Error: &Error{HTTPStatus: http.StatusNotFound, Message: "model not found"},
		},
	)
	before := time.Now()
	initial := getExecutionOutcomeTestAuth(t, m, auth.ID)
	bState := initial.ModelStates["model-b"]
	if bState == nil || bState.NextRetryAfter.Before(before.Add(11*time.Hour)) {
		t.Fatalf("model-b 12h deadline missing: %+v", bState)
	}
	short := 5 * time.Minute
	for _, model := range []string{"model-a", "model-b"} {
		markOriginalCooldownResult(
			t, m, Result{
				AuthID: auth.ID, Provider: auth.Provider, Model: model, RetryAfter: &short,
				Disposition: originalCredentialQuotaDisposition(auth.ID),
				Error:       &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential 429"},
			},
		)
		updated := getExecutionOutcomeTestAuth(t, m, auth.ID)
		after := updated.ModelStates["model-b"]
		if !reflect.DeepEqual(after, bState) || updated.Quota.NextRecoverAt.After(before.Add(10*time.Minute)) ||
			updated.StateEffectScope != cliproxyexecutor.StateEffectScopeCredential ||
			updated.ResetAuthority !=
				StateResetAuthoritySuccess ||
			updated.Quota.ResetAuthority !=
				StateResetAuthoritySuccess {
			t.Fatalf("credential failure promoted or changed model-b state: auth=%+v model=%+v", updated, after)
		}
		if updated.ModelStates["model-a"] != nil {
			t.Fatal("credential quota created a model-a mirror")
		}
		assertOriginalCooldownRouting(t, updated, "model-a", before.Add(6*time.Minute), false)
		assertOriginalCooldownRouting(t, updated, "model-b", before.Add(6*time.Minute), true)
	}
}

func TestOriginalManager_MarkResult_LaterShorterFailureKeepsLongerModelDeadline(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   *Error
		retry *time.Duration
	}{
		{
			"401_then_short_429", &Error{HTTPStatus: http.StatusTooManyRequests, Message: "short 429"},
			new(2 * time.Minute),
		},
		{"401_then_transient_500", &Error{HTTPStatus: http.StatusInternalServerError, Message: "transient 500"}, nil},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				m, auth := newOriginalCooldownMonotonicManager(t, "model-a")
				markOriginalCooldownResult(
					t, m, Result{
						AuthID: auth.ID, Provider: auth.Provider, Model: "model-a",
						Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: "long 401"},
					},
				)
				before := time.Now()
				initial := getExecutionOutcomeTestAuth(t, m, auth.ID)
				if initial.NextRetryAfter.Before(before.Add(25*time.Minute)) || len(initial.ModelStates) != 0 {
					t.Fatalf("401 credential deadline missing: %+v", initial)
				}
				markOriginalCooldownResult(
					t, m, Result{
						AuthID: auth.ID, Provider: auth.Provider, Model: "model-a", Error: tc.err, RetryAfter: tc.retry,
					},
				)
				updated := getExecutionOutcomeTestAuth(t, m, auth.ID)
				if !updated.NextRetryAfter.Equal(initial.NextRetryAfter) ||
					updated.StateGeneration != initial.StateGeneration ||
					updated.StateEffectScope != cliproxyexecutor.StateEffectScopeCredential {
					t.Fatalf("later failure changed the independent credential deadline: %+v", updated)
				}
				if tc.retry != nil {
					state := updated.ModelStates["model-a"]
					if state == nil || state.EffectScope != cliproxyexecutor.StateEffectScopeQuotaFamily ||
						state.NextRetryAfter.Before(
							before.Add(*tc.retry),
						) || state.NextRetryAfter.After(before.Add(3*time.Minute)) {
						t.Fatalf("independent two-minute quota state missing: %+v", state)
					}
				} else if len(updated.ModelStates) != 0 || updated.Failed != initial.Failed+1 {
					t.Fatalf("unknown 500 changed availability or lost failure statistics: %+v", updated)
				}
				assertOriginalCooldownRouting(t, updated, "model-a", before.Add(6*time.Minute), true)
			},
		)
	}
}

func TestOriginalManager_ClientModelProjection_ReflectsAuthLevelCooldown(t *testing.T) {
	m, auth := newOriginalCooldownMonotonicManager(t, "model-a")
	markOriginalCooldownResult(
		t, m, Result{
			AuthID: auth.ID, Provider: auth.Provider, Model: "",
			Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: "credential-wide 401"},
		},
	)
	snap := getExecutionOutcomeTestAuth(t, m, auth.ID)
	if !snap.Unavailable || snap.NextRetryAfter.Before(time.Now().Add(25*time.Minute)) {
		t.Fatalf("expected live auth-level cooldown: %+v", snap)
	}
	assertOriginalCooldownRouting(t, snap, "model-a", time.Now(), true)
	if !registry.GetGlobalRegistry().IsModelSuspendedForClient(auth.ID, "model-a") {
		t.Fatal("credential cooldown was not published to the registered model")
	}
}

func TestOriginalManager_ApplyAuthFailureState_PreservesLongerCredentialDeadline(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second *Error
	}{
		{"404_then_invalid_grant", &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_grant"}},
		{
			"404_then_cloudflare",
			&Error{HTTPStatus: http.StatusForbidden, Message: "just a moment... cloudflare challenge"},
		},
		{
			"404_then_transient_500",
			&Error{HTTPStatus: http.StatusInternalServerError, Message: "internal server error"},
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				m, auth := newOriginalCooldownMonotonicManager(t, "model-a")
				markOriginalCooldownResult(
					t, m, Result{
						AuthID: auth.ID, Provider: auth.Provider, Model: "",
						Error: &Error{HTTPStatus: http.StatusNotFound, Message: "credential not found"},
					},
				)
				initial := getExecutionOutcomeTestAuth(t, m, auth.ID)
				if initial.Unavailable || !initial.NextRetryAfter.IsZero() || len(initial.ModelStates) != 0 {
					t.Fatalf("unbound exact-model 404 changed credential availability: %+v", initial)
				}
				markOriginalCooldownResult(
					t, m, Result{
						AuthID: auth.ID, Provider: auth.Provider, Model: "", Error: tc.second,
					},
				)
				updated := getExecutionOutcomeTestAuth(t, m, auth.ID)
				if tc.second.Message == "invalid_grant" {
					if !updated.Unavailable || !updated.NextRetryAfter.IsZero() ||
						updated.StateEffectScope != cliproxyexecutor.StateEffectScopeCredential ||
						updated.ResetAuthority !=
							StateResetAuthorityReset ||
						updated.StateGeneration !=
							normalizedStateGeneration(initial.StateGeneration)+
								1 {
						t.Fatalf("invalid_grant did not preserve authoritative recovery: %+v", updated)
					}
					assertOriginalCooldownRouting(t, updated, "model-a", time.Now().Add(13*time.Hour), true)
				} else if updated.Unavailable || !updated.NextRetryAfter.IsZero() || len(updated.ModelStates) != 0 ||
					updated.StateGeneration != initial.StateGeneration || updated.Failed != initial.Failed+1 {
					t.Fatalf("Provider-scoped failure changed credential availability: %+v", updated)
				}
			},
		)
	}
}

func TestOriginalManager_MarkResult_CredentialScopeDoesNotInheritModelQuotaDeadline(t *testing.T) {
	for _, tc := range []struct {
		name            string
		credentialModel string
	}{
		{"cross_model", "claude-sonnet-5"},
		{"same_model", "claude-fable-5-1"},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				m, auth := newOriginalCooldownMonotonicManager(
					t,
					"claude-fable-5-1", "claude-sonnet-5", "claude-opus-5-5", "claude-sonnet-4",
				)
				for _, model := range []string{"claude-fable-5-1", "claude-sonnet-5", "claude-opus-5-5"} {
					markOriginalCooldownResult(
						t, m, Result{AuthID: auth.ID, Provider: auth.Provider, Model: model, Success: true},
					)
				}
				before := time.Now()
				modelRetry := 8 * 24 * time.Hour
				markOriginalCooldownResult(
					t, m, Result{
						AuthID: auth.ID, Provider: auth.Provider, Model: "claude-fable-5-1", RetryAfter: &modelRetry,
						Error: &Error{
							HTTPStatus: http.StatusTooManyRequests,
							Message:    "Usage credits are required for this model.",
						},
					},
				)
				modelOnly := getExecutionOutcomeTestAuth(t, m, auth.ID)
				modelState := modelOnly.ModelStates["claude-fable-5-1"]
				if modelOnly.Quota.Reason != "quota" ||
					!modelOnly.Quota.NextRecoverAt.After(before.Add(7*24*time.Hour)) {
					t.Fatalf("model quota deadline was not aggregated: %+v", modelOnly.Quota)
				}
				assertOriginalCooldownRouting(t, modelOnly, "claude-opus-5-5", time.Now(), false)
				credentialRetry := 3 * time.Hour
				markOriginalCooldownResult(
					t, m, Result{
						AuthID: auth.ID, Provider: auth.Provider, Model: tc.credentialModel,
						RetryAfter:  &credentialRetry,
						Disposition: originalCredentialQuotaDisposition(auth.ID),
						Error: &Error{
							HTTPStatus: http.StatusTooManyRequests,
							Message:    "shared window rejected",
						},
					},
				)
				updated := getExecutionOutcomeTestAuth(t, m, auth.ID)
				if updated.StateEffectScope !=
					cliproxyexecutor.StateEffectScopeCredential ||
					updated.Quota.Reason !=
						"quota" ||
					updated.Quota.NextRecoverAt.Before(before.Add(credentialRetry-time.Hour)) ||
					updated.Quota.NextRecoverAt.After(before.Add(credentialRetry+time.Hour)) ||
					updated.StateGeneration != normalizedStateGeneration(modelOnly.StateGeneration)+1 ||
					updated.ResetAuthority != StateResetAuthoritySuccess {
					t.Fatalf("credential cooldown inherited a model-only deadline: %+v", updated)
				}
				if !reflect.DeepEqual(updated.ModelStates["claude-fable-5-1"], modelState) {
					t.Fatal("credential cooldown changed the independent eight-day model state")
				}
				assertOriginalCooldownRouting(
					t, updated, "claude-opus-5-5", before.Add(credentialRetry+time.Minute), false,
				)
				assertOriginalCooldownRouting(
					t, updated, "claude-fable-5-1", before.Add(credentialRetry+time.Minute), true,
				)
				_, _, next := isAuthBlockedForModel(
					updated, "claude-fable-5-1", before.Add(credentialRetry+time.Minute),
				)
				if !next.After(before.Add(7 * 24 * time.Hour)) {
					t.Fatalf("eight-day Fable deadline was not retained: %v", next)
				}
				deadline := updated.Quota.NextRecoverAt
				shorter := 30 * time.Minute
				markOriginalCooldownResult(
					t, m, Result{
						AuthID: auth.ID, Provider: auth.Provider, Model: "claude-sonnet-4", RetryAfter: &shorter,
						Disposition: originalCredentialQuotaDisposition(auth.ID),
						Error: &Error{
							HTTPStatus: http.StatusTooManyRequests, Message: "shorter shared window rejection",
						},
					},
				)
				final := getExecutionOutcomeTestAuth(t, m, auth.ID)
				if !final.Quota.NextRecoverAt.Equal(deadline) ||
					final.StateGeneration != updated.StateGeneration+1 {
					t.Fatalf("later credential failure shortened its active deadline: %+v", final)
				}
			},
		)
	}
}

func TestOriginalManager_MarkResult_CredentialScopeBackoffPersistsAcrossWindows(t *testing.T) {
	m, auth := newOriginalCooldownMonotonicManager(t, "model-a", "model-b")
	markOriginalCooldownResult(
		t, m, Result{
			AuthID: auth.ID, Provider: auth.Provider, Model: "model-a",
			Disposition: originalCredentialQuotaDisposition(auth.ID),
			Error:       &Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential quota exceeded"},
		},
	)
	first := getExecutionOutcomeTestAuth(t, m, auth.ID)
	if first.Quota.BackoffLevel != 1 {
		t.Fatalf("first credential quota backoff = %+v", first.Quota)
	}
	expired := time.Now().Add(-time.Minute)
	m.mu.Lock()
	stored := m.auths[auth.ID]
	stored.Quota.NextRecoverAt = expired
	stored.NextRetryAfter = expired
	for _, state := range stored.ModelStates {
		if state != nil {
			state.Quota.NextRecoverAt = expired
			state.NextRetryAfter = expired
		}
	}
	m.mu.Unlock()
	secondAt := time.Now()
	markOriginalCooldownResult(
		t, m, Result{
			AuthID: auth.ID, Provider: auth.Provider, Model: "model-b",
			Disposition: originalCredentialQuotaDisposition(auth.ID),
			Error: &Error{
				HTTPStatus: http.StatusTooManyRequests,
				Message:    "credential quota exceeded again",
			},
		},
	)
	updated := getExecutionOutcomeTestAuth(t, m, auth.ID)
	if updated.Quota.BackoffLevel != 2 || updated.Quota.NextRecoverAt.Before(secondAt.Add(2*quotaBackoffBase)) ||
		updated.StateGeneration != first.StateGeneration+1 ||
		updated.Quota.Generation != first.Quota.Generation+1 ||
		len(updated.ModelStates) != 0 {
		t.Fatalf("second credential quota backoff inherited model state or lost its generation: %+v", updated)
	}
}

func TestOriginalManager_MarkResult_CredentialScopeDoesNotInheritModelBackoffLevel(t *testing.T) {
	retryAfter := 10 * time.Second
	for _, tc := range []struct {
		name     string
		retry    *time.Duration
		backoff  int
		duration time.Duration
	}{
		{"without_retry_hint", nil, 1, quotaBackoffBase},
		{"with_retry_hint", &retryAfter, 0, retryAfter},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				m, auth := newOriginalCooldownMonotonicManager(t, "model-a", "model-b")
				markOriginalCooldownResult(
					t, m, Result{
						AuthID: auth.ID, Provider: auth.Provider, Model: "model-a",
						Error: &Error{
							HTTPStatus: http.StatusTooManyRequests,
							Message:    "model-only quota exceeded",
						},
					},
				)
				modelOnly := getExecutionOutcomeTestAuth(t, m, auth.ID)
				modelState := modelOnly.ModelStates["model-a"]
				if modelOnly.Quota.Reason != "quota" || modelState == nil ||
					modelState.Quota.BackoffLevel != 1 {
					t.Fatalf(
						"model-only quota/backoff missing: auth=%+v model=%+v",
						modelOnly.Quota,
						modelState,
					)
				}
				started := time.Now()
				markOriginalCooldownResult(
					t, m, Result{
						AuthID: auth.ID, Provider: auth.Provider, Model: "model-a", RetryAfter: tc.retry,
						Disposition: originalCredentialQuotaDisposition(auth.ID),
						Error: &Error{
							HTTPStatus: http.StatusTooManyRequests, Message: "credential quota exceeded",
						},
					},
				)
				completed := time.Now()
				updated := getExecutionOutcomeTestAuth(t, m, auth.ID)
				if updated.Quota.BackoffLevel != tc.backoff ||
					updated.StateEffectScope != cliproxyexecutor.StateEffectScopeCredential ||
					updated.StateGeneration != normalizedStateGeneration(modelOnly.StateGeneration)+1 ||
					!reflect.DeepEqual(updated.ModelStates["model-a"], modelState) {
					t.Fatalf("credential quota inherited model backoff: %+v", updated)
				}
				if updated.Quota.NextRecoverAt.Before(started.Add(tc.duration)) ||
					updated.Quota.NextRecoverAt.After(completed.Add(tc.duration)) {
					t.Fatalf(
						"credential deadline=%v, want between %v and %v", updated.Quota.NextRecoverAt,
						started.Add(tc.duration), completed.Add(tc.duration),
					)
				}
			},
		)
	}
}

func TestCooldownMonotonicModelQuotaAfterCredentialFailureStaysLocal(t *testing.T) {
	m, auth := newOriginalCooldownMonotonicManager(
		t,
		"claude-fable-5-1", "claude-sonnet-5", "claude-opus-5-5", "claude-sonnet-4",
	)
	markOriginalCooldownResult(
		t, m, Result{
			AuthID: auth.ID, Provider: auth.Provider, Model: "claude-sonnet-5",
			Error: &Error{HTTPStatus: http.StatusUnauthorized, Message: "long 401"},
		},
	)
	credential := getExecutionOutcomeTestAuth(t, m, auth.ID)
	modelRetry := 8 * 24 * time.Hour
	markOriginalCooldownResult(
		t, m, Result{
			AuthID: auth.ID, Provider: auth.Provider, Model: "claude-fable-5-1", RetryAfter: &modelRetry,
			Error: &Error{
				HTTPStatus: http.StatusTooManyRequests, Message: "Usage credits are required for this model.",
			},
		},
	)
	updated := getExecutionOutcomeTestAuth(t, m, auth.ID)
	if !updated.NextRetryAfter.Equal(credential.NextRetryAfter) || updated.Quota.Exceeded ||
		!updated.Quota.NextRecoverAt.IsZero() || updated.StateGeneration != credential.StateGeneration {
		t.Fatalf(
			"model quota became a credential-wide restriction: before=%+v after=%+v",
			credential.Quota,
			updated.Quota,
		)
	}
	afterCredential := credential.NextRetryAfter.Add(time.Minute)
	assertOriginalCooldownRouting(t, updated, "claude-opus-5-5", afterCredential, false)
	assertOriginalCooldownRouting(t, updated, "claude-fable-5-1", afterCredential, true)
}

func TestCooldownMonotonicExplicitCredentialDeadlineRetainsOriginalWriters(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *Error
	}{
		{"404_then_invalid_grant", &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid_grant"}},
		{
			"404_then_cloudflare",
			&Error{HTTPStatus: http.StatusForbidden, Message: "just a moment... cloudflare challenge"},
		},
		{
			"404_then_transient_500",
			&Error{HTTPStatus: http.StatusInternalServerError, Message: "internal server error"},
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				m, auth := newOriginalCooldownMonotonicManager(t, "model-a")
				markOriginalCooldownResult(
					t, m, Result{
						AuthID: auth.ID, Provider: auth.Provider, Model: "",
						Error: &Error{HTTPStatus: http.StatusNotFound, Message: "credential not found"},
						Disposition: cliproxyexecutor.ExecutionDisposition{
							DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
							Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
							RetryDirective: cliproxyexecutor.RetryDirectiveNextAuth,
							EffectScope:    cliproxyexecutor.StateEffectScopeCredential,
							EffectKey:      auth.ID,
						},
					},
				)
				before := getExecutionOutcomeTestAuth(t, m, auth.ID)
				if !before.Unavailable || before.NextRetryAfter.Before(time.Now().Add(11*time.Hour)) {
					t.Fatalf("explicit credential 404 deadline missing: %+v", before)
				}
				markOriginalCooldownResult(
					t, m, Result{
						AuthID: auth.ID, Provider: auth.Provider, Model: "", Error: tc.err,
					},
				)
				after := getExecutionOutcomeTestAuth(t, m, auth.ID)
				if tc.err.Message == "invalid_grant" {
					if !after.NextRetryAfter.IsZero() || after.ResetAuthority != StateResetAuthorityReset ||
						after.StateGeneration != before.StateGeneration+1 {
						t.Fatalf("invalid_grant did not replace the timer with reset authority: %+v", after)
					}
					assertOriginalCooldownRouting(t, after, "model-a", time.Now().Add(13*time.Hour), true)
				} else if !after.NextRetryAfter.Equal(before.NextRetryAfter) ||
					after.StateGeneration !=
						before.StateGeneration ||
					after.Failed != before.Failed+1 {
					t.Fatalf("Provider failure changed the explicit credential deadline: %+v", after)
				}
			},
		)
	}
}

func TestCooldownMonotonicQuotaOnlyCredentialStateRetainsItsWindow(t *testing.T) {
	now := time.Now()
	credentialQuota := QuotaState{
		Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(3 * time.Hour),
		BackoffLevel: 1, Generation: 2, ResetAuthority: StateResetAuthoritySuccess,
	}
	m, auth := newExecutionOutcomeTestManager(
		t, &Auth{
			ID: "quota-only-credential", Provider: "claude", Status: StatusActive,
			StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
			Quota:            credentialQuota,
		},
	)
	modelRetry := 8 * 24 * time.Hour
	markOriginalCooldownResult(
		t, m, Result{
			AuthID: auth.ID, Provider: auth.Provider, Model: "claude-fable-5-1", RetryAfter: &modelRetry,
			Error: &Error{
				HTTPStatus: http.StatusTooManyRequests, Message: "Usage credits are required for this model.",
			},
		},
	)
	after := getExecutionOutcomeTestAuth(t, m, auth.ID)
	if !reflect.DeepEqual(after.Quota, credentialQuota) || after.Unavailable || !after.NextRetryAfter.IsZero() {
		t.Fatalf("model quota changed the independent credential quota: %+v", after)
	}
	assertOriginalCooldownRouting(t, after, "claude-opus-5-5", now.Add(3*time.Hour+time.Minute), false)
	assertOriginalCooldownRouting(t, after, "claude-fable-5-1", now.Add(3*time.Hour+time.Minute), true)
}
