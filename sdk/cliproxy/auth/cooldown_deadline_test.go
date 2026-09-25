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
				if scope == cliproxyexecutor.StateEffectScopeQuotaFamily {
					auth.ModelStates = map[string]*ModelState{"model-a": {Quota: quota}}
				}
				applyResultStateLocked(auth, Result{
					Model: "model-a", RetryAfter: tc.hint,
					Error:       &Error{HTTPStatus: http.StatusTooManyRequests},
					Disposition: cliproxyexecutor.ExecutionDisposition{EffectScope: scope},
				}, now, false)
				next, recover := auth.NextRetryAfter, auth.Quota.NextRecoverAt
				if scope == cliproxyexecutor.StateEffectScopeQuotaFamily {
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
	for _, status := range []int{401, 402, 403, 404, 429, 503} {
		for _, disabled := range []bool{false, true} {
			now := time.Now()
			previous := now.Add(24 * time.Hour).Round(0)
			auth := &Auth{ID: "deadline-auth", NextRetryAfter: previous}
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
