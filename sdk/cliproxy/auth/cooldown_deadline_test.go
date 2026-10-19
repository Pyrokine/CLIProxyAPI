package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestQuotaCooldownDeadlineHintAndClock(t *testing.T) {
	for _, scope := range []cliproxyexecutor.StateEffectScope{
		cliproxyexecutor.StateEffectScopeCredential,
		cliproxyexecutor.StateEffectScopeQuotaFamily,
		cliproxyexecutor.StateEffectScopeExactModel,
	} {
		for _, tc := range []struct {
			name     string
			hint     *time.Duration
			want     time.Duration
			existing time.Duration
		}{
			{name: "negative", hint: new(-time.Second), want: 10 * time.Second},
			{name: "zero", hint: new(time.Duration(0)), want: 10 * time.Second},
			{name: "subsecond", hint: new(time.Millisecond), want: 10 * time.Second},
			{name: "long", hint: new(time.Minute), want: time.Minute},
			{name: "default_backoff", want: time.Second},
			{name: "active_window", want: time.Minute, existing: time.Minute},
			{name: "shorter_hint", hint: new(time.Second), want: time.Minute, existing: time.Minute},
		} {
			t.Run(string(scope)+"/"+tc.name, func(t *testing.T) {
				now := time.Now()
				quota := QuotaState{}
				if tc.existing > 0 {
					quota.Exceeded = true
					quota.NextRecoverAt = now.Add(tc.existing)
				}
				auth := &Auth{ID: "deadline-auth", Provider: "provider-a", Status: StatusActive, Quota: quota}
				if scope != cliproxyexecutor.StateEffectScopeCredential {
					auth.ModelStates = map[string]*ModelState{"model-a": {Quota: quota}}
				}
				applyResultStateLocked(auth, Result{
					Model: "model-a", RetryAfter: tc.hint,
					Error:       &Error{HTTPStatus: http.StatusTooManyRequests},
					Disposition: cliproxyexecutor.ExecutionDisposition{EffectScope: scope},
				}, now, false)
				next, recover := auth.NextRetryAfter, auth.Quota.NextRecoverAt
				if scope != cliproxyexecutor.StateEffectScopeCredential {
					state := auth.ModelStates["model-a"]
					next, recover = state.NextRetryAfter, state.Quota.NextRecoverAt
				}
				want := now.Add(tc.want).Round(0)
				if next != want || recover != want {
					t.Fatalf("retry=%v recover=%v, want wall-clock deadline %v", next, recover, want)
				}
			})
		}
	}
}

func TestFailurePreservesActiveRetryDeadline(t *testing.T) {
	for _, withModels := range []bool{false, true} {
		for _, status := range []int{401, 402, 403, 404, 429, 503} {
			for _, disabled := range []bool{false, true} {
				now := time.Now()
				previous := now.Add(24 * time.Hour).Round(0)
				auth := &Auth{ID: "deadline-auth", NextRetryAfter: previous}
				if withModels {
					auth.ModelStates = map[string]*ModelState{"model-a": {Status: StatusActive}}
				}
				applyAuthFailureState(auth, &Error{HTTPStatus: status}, new(time.Second), now, disabled)
				if disabled {
					if !auth.NextRetryAfter.IsZero() || auth.Unavailable {
						t.Fatalf("status=%d disabled cooling retained deadline: %+v", status, auth)
					}
				} else if auth.NextRetryAfter != previous {
					t.Fatalf("status=%d shortened deadline to %v, want %v", status, auth.NextRetryAfter, previous)
				}
				if status == 429 && !disabled && auth.Quota.NextRecoverAt != now.Add(10*time.Second).Round(0) {
					t.Fatalf("credential retry deadline became quota recovery: %+v", auth.Quota)
				}
			}
		}
	}
	for _, disabled := range []bool{false, true} {
		now := time.Now()
		previous := now.Add(24 * time.Hour).Round(0)
		state := &ModelState{NextRetryAfter: previous}
		applyExactModelFailure(state, "model-a", Result{Error: &Error{HTTPStatus: 404}}, now, disabled)
		if disabled {
			if !state.NextRetryAfter.IsZero() || state.Unavailable {
				t.Fatalf("disabled model cooling retained deadline: %+v", state)
			}
		} else if state.NextRetryAfter != previous {
			t.Fatalf("model deadline=%v, want %v", state.NextRetryAfter, previous)
		}
	}
}

func TestQuotaFamilyKeepsRetryAndRecoveryDeadlinesSeparate(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		now := time.Now()
		longRetry := now.Add(24 * time.Hour).Round(0)
		longQuota := now.Add(2 * time.Hour).Round(0)
		auth := &Auth{ID: "deadline-auth", Provider: "provider-a", ModelStates: map[string]*ModelState{
			"model-a": {NextRetryAfter: longRetry, Quota: QuotaState{Family: "shared", Exceeded: true, NextRecoverAt: now.Add(time.Minute), BackoffLevel: 4}},
			"model-b": {Quota: QuotaState{Family: "shared", Exceeded: true, NextRecoverAt: longQuota, BackoffLevel: 1}},
		}}
		applyResultStateLocked(auth, Result{
			Model: "model-a", RetryAfter: new(time.Second),
			Error:       &Error{HTTPStatus: http.StatusTooManyRequests},
			Disposition: cliproxyexecutor.ExecutionDisposition{EffectScope: cliproxyexecutor.StateEffectScopeQuotaFamily, EffectKey: "shared"},
		}, now, disabled)
		for model, state := range auth.ModelStates {
			if disabled {
				if state.Unavailable || !state.NextRetryAfter.IsZero() || !state.Quota.NextRecoverAt.IsZero() {
					t.Fatalf("disabled family retained deadline: %+v", state)
				}
				continue
			}
			wantRetry := longQuota
			if model == "model-a" {
				wantRetry = longRetry
			}
			if state.Quota.NextRecoverAt != longQuota || state.NextRetryAfter != wantRetry || state.Quota.BackoffLevel != 4 {
				t.Fatalf("%s retry=%v quota=%+v", model, state.NextRetryAfter, state.Quota)
			}
		}
	}
}

func TestBlockedCredentialUsesLaterActiveDeadline(t *testing.T) {
	for _, modelScoped := range []bool{false, true} {
		for _, exceeded := range []bool{false, true} {
			now := time.Now()
			retry := now.Add(time.Hour)
			quota := QuotaState{Exceeded: exceeded, NextRecoverAt: now.Add(time.Minute)}
			auth := &Auth{Unavailable: true, NextRetryAfter: retry, Quota: quota}
			if modelScoped {
				auth = &Auth{ModelStates: map[string]*ModelState{"model-a": {Unavailable: true, NextRetryAfter: retry, Quota: quota}}}
			}
			blocked, reason, next := isAuthBlockedForModel(auth, "model-a", now)
			wantReason := blockReasonOther
			if exceeded {
				wantReason = blockReasonCooldown
			}
			if !blocked || reason != wantReason || !next.Equal(retry) {
				t.Fatalf("modelScoped=%t exceeded=%t blocked=%t reason=%v next=%v, want %v", modelScoped, exceeded, blocked, reason, next, retry)
			}
			later := now.Add(2 * time.Hour)
			if modelScoped {
				auth.ModelStates["model-a"].Quota.NextRecoverAt = later
			} else {
				auth.Quota.NextRecoverAt = later
			}
			_, _, next = isAuthBlockedForModel(auth, "model-a", now)
			want := retry
			if exceeded {
				want = later
			}
			if !next.Equal(want) {
				t.Fatalf("modelScoped=%t exceeded=%t later quota deadline=%v, want %v", modelScoped, exceeded, next, want)
			}
		}
	}
}

func TestMarkResultPreservesQuotaDeadlineAndEpoch(t *testing.T) {
	manager, current := newExecutionOutcomeTestManager(t, &Auth{
		ID: "deadline-auth", Provider: "provider-a", Status: StatusActive,
		ModelStates: map[string]*ModelState{
			"model-a": {Status: StatusActive, Quota: QuotaState{Family: "shared"}},
			"model-b": {Status: StatusActive, Quota: QuotaState{Family: "shared"}},
		},
	})
	first := executionResult(current, current.Provider, "model-a", executionOutcomeStatusError{status: 429, retryAfter: new(time.Hour)}, cliproxyexecutor.DispatchPhaseMaybeSent)
	first.RetryAfter = new(time.Hour)
	manager.MarkResult(context.Background(), first)
	before := getExecutionOutcomeTestAuth(t, manager, current.ID)
	if len(before.ModelStates) != 2 || before.ModelStates["model-a"] == nil || !before.ModelStates["model-a"].Quota.Exceeded {
		t.Fatalf("first 429 did not cool both family models: %+v", before)
	}
	second := executionResult(before, before.Provider, "model-a", executionOutcomeStatusError{status: 429, retryAfter: new(time.Millisecond)}, cliproxyexecutor.DispatchPhaseMaybeSent)
	second.RetryAfter = new(time.Millisecond)
	manager.MarkResult(context.Background(), second)
	after := getExecutionOutcomeTestAuth(t, manager, current.ID)
	for model, state := range after.ModelStates {
		old := before.ModelStates[model]
		if state.NextRetryAfter != old.NextRetryAfter || state.Quota.NextRecoverAt != old.Quota.NextRecoverAt ||
			state.QuotaEpoch != old.QuotaEpoch+1 || state.Quota.Generation != old.Quota.Generation+1 {
			t.Fatalf("%s quota transition before=%+v after=%+v", model, old, state)
		}
	}
}

func TestCredentialCooldownDoesNotInheritModelState(t *testing.T) {
	longModelRetry := 8 * 24 * time.Hour
	credentialRetry := 3 * time.Hour
	shortRetry := 10 * time.Second
	for _, test := range []struct {
		name            string
		credentialModel string
		singleModel     bool
		modelHint       *time.Duration
		credentialHint  *time.Duration
		wantDuration    time.Duration
		wantBackoff     int
	}{
		{name: "long same model", credentialModel: "model-a", modelHint: &longModelRetry,
			credentialHint: &credentialRetry, wantDuration: credentialRetry},
		{name: "long cross model", credentialModel: "model-b", modelHint: &longModelRetry,
			credentialHint: &credentialRetry, wantDuration: credentialRetry},
		{name: "long single model", credentialModel: "model-a", singleModel: true, modelHint: &longModelRetry,
			credentialHint: &credentialRetry, wantDuration: credentialRetry},
		{name: "automatic same model", credentialModel: "model-a", wantDuration: quotaBackoffBase, wantBackoff: 1},
		{name: "automatic cross model", credentialModel: "model-b", wantDuration: quotaBackoffBase, wantBackoff: 1},
		{name: "hint same model", credentialModel: "model-a", credentialHint: &shortRetry, wantDuration: shortRetry},
		{name: "hint cross model", credentialModel: "model-b", credentialHint: &shortRetry, wantDuration: shortRetry},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			auth := &Auth{ID: "credential-state-isolation-auth", Provider: "credential-state-provider", Status: StatusActive,
				ModelStates: map[string]*ModelState{"model-a": {Status: StatusActive, Quota: QuotaState{Family: "model-a-only"}}}}
			if !test.singleModel {
				auth.ModelStates["model-b"] = &ModelState{Status: StatusActive, Quota: QuotaState{Family: "model-b-only"}}
			}
			registered, errRegister := manager.Register(context.Background(), auth)
			if errRegister != nil {
				t.Fatalf("Register: %v", errRegister)
			}
			modelFailure := executionResult(registered, registered.Provider, "model-a",
				&Error{HTTPStatus: http.StatusTooManyRequests, Message: "model quota"},
				cliproxyexecutor.DispatchPhaseResponseStarted)
			modelFailure.RetryAfter = test.modelHint
			modelFailure.Disposition.EffectScope = cliproxyexecutor.StateEffectScopeQuotaFamily
			modelFailure.Disposition.EffectKey = "model-a-only"
			modelFailure.StateGeneration = stateGenerationForDisposition(registered, "model-a", modelFailure.Disposition)
			manager.MarkResult(context.Background(), modelFailure)
			before, ok := manager.GetByID(auth.ID)
			if !ok || before.ModelStates["model-a"] == nil || !before.ModelStates["model-a"].Quota.Exceeded {
				t.Fatalf("model quota state missing: %+v", before)
			}
			modelBefore := *before.ModelStates["model-a"]
			quotaGeneration := before.Quota.Generation
			credentialFailure := executionResult(before, before.Provider, test.credentialModel,
				&Error{HTTPStatus: http.StatusTooManyRequests, Message: "credential quota"},
				cliproxyexecutor.DispatchPhaseResponseStarted)
			credentialFailure.RetryAfter = test.credentialHint
			credentialFailure.Disposition.EffectScope = cliproxyexecutor.StateEffectScopeCredential
			credentialFailure.Disposition.EffectKey = auth.ID
			credentialFailure.StateGeneration = stateGenerationForDisposition(before, test.credentialModel, credentialFailure.Disposition)
			started := time.Now()
			manager.MarkResult(context.Background(), credentialFailure)
			finished := time.Now()
			current, ok := manager.GetByID(auth.ID)
			if !ok || current.StateEffectScope != cliproxyexecutor.StateEffectScopeCredential || !current.Quota.Exceeded {
				t.Fatalf("credential quota state missing: %+v", current)
			}
			if current.Quota.BackoffLevel != test.wantBackoff || current.Quota.Reason != "quota" {
				t.Fatalf("credential quota inherited model state: %+v, want backoff %d", current.Quota, test.wantBackoff)
			}
			minDeadline, maxDeadline := started.Add(test.wantDuration), finished.Add(test.wantDuration)
			if current.NextRetryAfter.Before(minDeadline) || current.NextRetryAfter.After(maxDeadline) ||
				!current.NextRetryAfter.Equal(current.Quota.NextRecoverAt) {
				t.Fatalf("credential retry=%v recover=%v, want between %v and %v", current.NextRetryAfter,
					current.Quota.NextRecoverAt, minDeadline, maxDeadline)
			}
			if current.Quota.Generation <= quotaGeneration {
				t.Fatalf("quota generation did not advance: got %d, previous %d", current.Quota.Generation, quotaGeneration)
			}
			modelAfter := current.ModelStates["model-a"]
			if !modelAfter.NextRetryAfter.Equal(modelBefore.NextRetryAfter) ||
				!modelAfter.Quota.NextRecoverAt.Equal(modelBefore.Quota.NextRecoverAt) ||
				modelAfter.Quota.BackoffLevel != modelBefore.Quota.BackoffLevel {
				t.Fatalf("credential failure changed model state: before=%+v after=%+v", modelBefore, modelAfter)
			}
			afterCredentialWindow := current.NextRetryAfter.Add(time.Second)
			if !test.singleModel {
				if blocked, _, _ := isAuthBlockedForModel(current, "model-b", afterCredentialWindow); blocked {
					t.Fatal("unrelated model remained blocked beyond credential cooldown")
				}
			}
			if test.modelHint != nil {
				if blocked, _, next := isAuthBlockedForModel(current, "model-a", afterCredentialWindow); !blocked ||
					!next.Equal(modelBefore.NextRetryAfter) {
					t.Fatalf("model cooldown was lost: blocked=%t next=%v", blocked, next)
				}
				shorter := 30 * time.Minute
				nextFailure := executionResult(current, current.Provider, test.credentialModel,
					&Error{HTTPStatus: http.StatusTooManyRequests, Message: "shorter credential quota"},
					cliproxyexecutor.DispatchPhaseResponseStarted)
				nextFailure.RetryAfter = &shorter
				nextFailure.Disposition.EffectScope = cliproxyexecutor.StateEffectScopeCredential
				nextFailure.Disposition.EffectKey = auth.ID
				nextFailure.StateGeneration = stateGenerationForDisposition(current, test.credentialModel, nextFailure.Disposition)
				manager.MarkResult(context.Background(), nextFailure)
				updated, _ := manager.GetByID(auth.ID)
				if !updated.NextRetryAfter.Equal(current.NextRetryAfter) ||
					!updated.Quota.NextRecoverAt.Equal(current.Quota.NextRecoverAt) {
					t.Fatalf("shorter credential failure changed the active window: before=%+v after=%+v", current.Quota, updated.Quota)
				}
			}
		})
	}
}

func TestActiveQuotaDeadlineGovernsRoutingAvailability(t *testing.T) {
	now := time.Now()
	quotaDeadline := now.Add(time.Hour)
	for _, scope := range []string{"credential", "model"} {
		for _, test := range []struct {
			name        string
			unavailable bool
			quota       bool
			retry       time.Time
			recover     time.Time
			blocked     bool
			deadline    time.Time
		}{
			{"quota-only", false, true, time.Time{}, quotaDeadline, true, quotaDeadline},
			{"available-flag-with-retry", false, true, now.Add(time.Minute), quotaDeadline, true, quotaDeadline},
			{"expired-retry-active-quota", true, true, now.Add(-time.Minute), quotaDeadline, true, quotaDeadline},
			{"missing-retry-active-quota", true, true, time.Time{}, quotaDeadline, true, quotaDeadline},
			{"later-retry-active-quota", true, true, now.Add(2 * time.Hour), quotaDeadline, true, now.Add(2 * time.Hour)},
			{"quota-without-deadline-cooling-disabled", false, true, time.Time{}, time.Time{}, false, time.Time{}},
			{"unavailable-quota-without-deadline", true, true, time.Time{}, time.Time{}, true, time.Time{}},
			{"expired-quota", false, true, time.Time{}, now.Add(-time.Minute), false, time.Time{}},
			{"healthy", false, false, time.Time{}, time.Time{}, false, time.Time{}},
			{"nonquota-unbounded-error", true, false, time.Time{}, time.Time{}, true, time.Time{}},
		} {
			t.Run(scope+"/"+test.name, func(t *testing.T) {
				state := &ModelState{
					Unavailable: test.unavailable, NextRetryAfter: test.retry,
					Quota: QuotaState{Exceeded: test.quota, NextRecoverAt: test.recover},
				}
				auth := &Auth{Status: StatusActive}
				if scope == "credential" {
					auth.Unavailable = state.Unavailable
					auth.NextRetryAfter = state.NextRetryAfter
					auth.Quota = state.Quota
					auth.StateEffectScope = cliproxyexecutor.StateEffectScopeCredential
				} else {
					auth.ModelStates = map[string]*ModelState{"model-a": state}
				}
				blocked, reason, deadline := isAuthBlockedForModel(auth, "model-a", now)
				if blocked != test.blocked || !deadline.Equal(test.deadline) {
					t.Errorf("availability blocked=%t deadline=%v, want blocked=%t deadline=%v", blocked, deadline, test.blocked, test.deadline)
				}
				if test.blocked && test.quota && !test.deadline.IsZero() && reason != blockReasonCooldown {
					t.Errorf("active quota reason=%v, want cooldown", reason)
				}
				if !test.deadline.IsZero() {
					if blocked, _, _ := isAuthBlockedForModel(auth, "model-a", test.deadline.Add(time.Nanosecond)); blocked {
						t.Error("expired quota deadline left the credential unavailable")
					}
				}
			})
		}
	}
}

func TestExactModelQuotaRecoversOnlyRejectedModel(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "cooling enabled", true: "cooling disabled"}[disabled], func(t *testing.T) {
			now := time.Now()
			auth := &Auth{ID: "exact-model-quota", Provider: "claude", Status: StatusActive,
				ModelStates: map[string]*ModelState{"model-a": {Status: StatusActive}, "model-b": {Status: StatusActive}}}
			applyResultStateLocked(auth, Result{
				Model: "model-a", Error: &Error{HTTPStatus: http.StatusTooManyRequests},
				Disposition: cliproxyexecutor.ExecutionDisposition{EffectScope: cliproxyexecutor.StateEffectScopeExactModel},
			}, now, disabled)
			state := auth.ModelStates["model-a"]
			if state.EffectScope != cliproxyexecutor.StateEffectScopeExactModel || !state.Quota.Exceeded {
				t.Fatalf("exact-model quota state = %+v", state)
			}
			if blocked, _, _ := isAuthBlockedForModel(auth, "model-b", now); blocked {
				t.Fatal("model quota blocked an unrelated model")
			}
			if disabled {
				if state.Unavailable || !state.NextRetryAfter.IsZero() || !state.Quota.NextRecoverAt.IsZero() {
					t.Fatalf("disabled cooling retained a model deadline: %+v", state)
				}
				if blocked, _, _ := isAuthBlockedForModel(auth, "model-a", now); blocked {
					t.Fatal("disabled cooling blocked the rejected model")
				}
				return
			}
			want := now.Add(quotaBackoffBase).Round(0)
			if state.NextRetryAfter != want || state.Quota.NextRecoverAt != want {
				t.Fatalf("retry=%v recovery=%v, want %v", state.NextRetryAfter, state.Quota.NextRecoverAt, want)
			}
			if blocked, _, next := isAuthBlockedForModel(auth, "model-a", now); !blocked || next != want {
				t.Fatalf("model quota before deadline: blocked=%t next=%v", blocked, next)
			}
			if blocked, _, _ := isAuthBlockedForModel(auth, "model-a", want.Add(time.Nanosecond)); blocked {
				t.Fatal("model remained blocked after its recovery deadline")
			}
		})
	}
}
