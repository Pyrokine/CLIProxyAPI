package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestMarkResultQuotaFailureDoesNotEraseSiblingObservation(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth, err := manager.Register(
		t.Context(), &Auth{
			ID: "quota-sibling-auth", Provider: "codex",
			ModelStates: map[string]*ModelState{
				"gpt-5.3-codex": {
					Status: StatusActive,
					Quota: QuotaState{
						ObservedAt: time.Unix(10, 0),
						Signals:    map[string]string{"X-Codex-Primary-Used-Percent": "40"},
					},
				},
				"gpt-5.4": {
					Status: StatusActive,
					Quota: QuotaState{
						ObservedAt: time.Unix(11, 0),
						Signals:    map[string]string{"X-Codex-Primary-Used-Percent": "41"},
					},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := newUpstreamAttemptContext(t.Context(), false)
	RecordUpstreamResponseHeaders(
		ctx, http.Header{
			"Retry-After": {"120"}, "X-Codex-Primary-Used-Percent": {"99"},
		},
	)
	result := executionResult(
		auth, "codex", "gpt-5.3-codex", errorWithState(
			&Error{
				HTTPStatus: http.StatusTooManyRequests, Message: "quota",
			}, errorState{EffectScope: cliproxyexecutor.StateEffectScopeCredential},
		), cliproxyexecutor.DispatchPhaseResponseStarted,
	)
	manager.MarkResult(ctx, result)

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("auth not found after MarkResult")
	}
	for _, model := range []string{"gpt-5.3-codex", "gpt-5.4"} {
		if blocked, _, _ := isAuthBlockedForModel(updated, model, time.Now()); !blocked {
			t.Fatalf("credential quota did not block model %s: %+v", model, updated)
		}
	}
	current := updated.ModelStates["gpt-5.3-codex"]
	if current == nil || current.Quota.Signals["X-Codex-Primary-Used-Percent"] != "99" ||
		current.Quota.Signals["Retry-After"] != "120" {
		t.Fatalf("current model observation was not refreshed: %#v", current)
	}
	sibling := updated.ModelStates["gpt-5.4"]
	if sibling == nil || sibling.Quota.Signals["X-Codex-Primary-Used-Percent"] != "41" ||
		!sibling.Quota.ObservedAt.Equal(
			time.Unix(
				11, 0,
			),
		) {
		t.Fatalf("credential quota erased sibling observation: %#v", sibling)
	}
	if _, exists := sibling.Quota.Signals["Retry-After"]; exists {
		t.Fatalf("sibling inherited the current response: %#v", sibling.Quota.Signals)
	}
}

func TestMarkResultRetainedCredentialQuotaStillObservesModel(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	recoverAt := time.Now().Add(time.Hour)
	auth, err := manager.Register(
		t.Context(), &Auth{
			ID: "quota-retain-auth", Provider: "codex", Status: StatusError, Unavailable: true,
			NextRetryAfter: recoverAt, StateEffectScope: cliproxyexecutor.StateEffectScopeCredential,
			StateEffectKey: "quota-retain-auth", ResetAuthority: StateResetAuthorityReset,
			Quota: QuotaState{
				Exceeded: true, Reason: "credential_quota", NextRecoverAt: recoverAt,
				ObservedAt: time.Unix(10, 0), Signals: map[string]string{"X-Codex-Primary-Used-Percent": "10"},
				ResetAuthority: StateResetAuthorityReset,
			},
			ModelStates: map[string]*ModelState{
				"gpt-5.3-codex": {
					Status: StatusError, Unavailable: true, NextRetryAfter: recoverAt,
					EffectScope: cliproxyexecutor.StateEffectScopeExactModel,
					EffectKey:   "gpt-5.3-codex", ResetAuthority: StateResetAuthorityReset,
					Quota: QuotaState{
						Exceeded: true, Reason: "credential_quota", NextRecoverAt: recoverAt,
						ObservedAt: time.Unix(10, 0), Signals: map[string]string{"X-Codex-Primary-Used-Percent": "10"},
						ResetAuthority: StateResetAuthorityReset,
					},
				},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := newUpstreamAttemptContext(t.Context(), false)
	RecordUpstreamResponseHeaders(ctx, http.Header{"X-Codex-Primary-Used-Percent": {"20"}})
	manager.MarkResult(
		ctx, executionResult(auth, "codex", "gpt-5.3-codex", nil, cliproxyexecutor.DispatchPhaseResponseStarted),
	)

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("auth not found after MarkResult")
	}
	if !updated.Quota.Exceeded || updated.Quota.Reason != "credential_quota" ||
		!updated.Quota.NextRecoverAt.Equal(recoverAt) {
		t.Fatalf("retained credential cooldown was disturbed: %#v", updated.Quota)
	}
	if updated.Quota.Signals["X-Codex-Primary-Used-Percent"] != "20" {
		t.Fatalf("auth observation was not refreshed: %#v", updated.Quota.Signals)
	}
	state := updated.ModelStates["gpt-5.3-codex"]
	if state == nil || !state.Quota.Exceeded || state.Quota.Reason != "credential_quota" ||
		!state.Quota.NextRecoverAt.Equal(recoverAt) {
		t.Fatalf("model cooldown was disturbed: %#v", state)
	}
	if state.Quota.Signals["X-Codex-Primary-Used-Percent"] != "20" {
		t.Fatalf("retained cooldown prevented model observation: %#v", state.Quota.Signals)
	}
}

func TestMergeModelStateKeepsNewestObservationSnapshot(t *testing.T) {
	target := &ModelState{
		UpdatedAt: time.Unix(20, 0), Quota: QuotaState{
			ObservedAt: time.Unix(20, 0), Signals: map[string]string{"X-Codex-Plan-Type": "pro"},
		},
	}
	//goland:noinspection SpellCheckingInspection
	source := &ModelState{
		UpdatedAt: time.Unix(30, 0), Quota: QuotaState{
			ObservedAt: time.Unix(30, 0), Signals: map[string]string{"X-Codex-Active-Limit": "codex_bengalfox"},
		},
	}
	manager := NewManager(nil, nil, nil)
	registered, err := manager.Register(
		context.Background(), &Auth{
			ID: "quota-merge-auth", Provider: "codex",
			ModelStates: map[string]*ModelState{"gpt-5.3-codex": target, "gpt-5.3-codex(high)": source},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	merged := registered.ModelStates["gpt-5.3-codex"]
	if len(registered.ModelStates) != 1 || merged == nil || !merged.Quota.ObservedAt.Equal(time.Unix(30, 0)) {
		t.Fatalf("registered merged state = %+v", registered.ModelStates)
	}
	//goland:noinspection SpellCheckingInspection
	if merged.Quota.Signals["X-Codex-Active-Limit"] != "codex_bengalfox" {
		t.Fatalf("newest snapshot was lost: %#v", merged.Quota.Signals)
	}
	if _, exists := merged.Quota.Signals["X-Codex-Plan-Type"]; exists {
		t.Fatalf("stale snapshot key survived the merge: %#v", merged.Quota.Signals)
	}
}
