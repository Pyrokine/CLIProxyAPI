package logging

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestRouteDecisionSanitizesAndDeepCopiesNestedFields(t *testing.T) {
	native := true
	converted := false
	quotaHeadroom := math.NaN()
	inFlight := int64(-1)
	matchedRules := []string{"highest_priority", "round_robin_group", "private-rule"}
	excludedReasons := []string{"disabled", "password=secret"}
	decision := &cliproxyexecutor.RouteDecisionSnapshot{
		Strategy:       "round-robin",
		RequestedModel: "model-a",
		RoutedModel:    "model-a",
		Providers:      []string{"gemini", "bearer secret"},
		Affinity:       "invalid-affinity",
		MatchedRule:    "round_robin",
		CandidateScope: "complete",
		Candidates: []cliproxyexecutor.RouteDecisionCandidate{
			{
				Provider:        "token-secret",
				Model:           "https://proxy.example/model",
				TargetModel:     "model-a",
				AuthIndex:       "0123456789abcdef",
				Priority:        3,
				EffectiveWeight: -5,
				Native:          &native,
				Converted:       &converted,
				Affinity:        "pinned",
				Cooldowns: cliproxyexecutor.RouteDecisionCooldowns{
					ExactModel: cliproxyexecutor.RouteDecisionCooldown{
						Known:           true,
						Active:          true,
						RetryAfterMS:    -1,
						Reason:          "password",
						EffectKey:       "token-secret",
						StateGeneration: 7,
						ResetAuthority:  "private-reset",
					},
				},
				QuotaHeadroom:   &quotaHeadroom,
				InFlight:        &inFlight,
				MatchedRules:    matchedRules,
				ExcludedReasons: excludedReasons,
				Selected:        true,
			},
		},
		Selected: &cliproxyexecutor.RouteDecisionSelection{
			Provider:    "gemini",
			Model:       "model-a",
			TargetModel: "model-a",
			AuthIndex:   "0123456789abcdef",
			MatchedRule: "round_robin",
		},
	}
	trace := NewRouteTrace()
	trace.Add(
		cliproxyexecutor.RouteTraceEvent{
			Provider: "mixed",
			Model:    "model-a",
			Action:   "route.decision",
			Reason:   "round_robin",
			Decision: decision,
		},
	)

	decision.Providers[0] = "mutated-provider"
	decision.Candidates[0].TargetModel = "mutated-model"
	matchedRules[0] = "mutated-rule"
	excludedReasons[0] = "mutated-reason"
	native = false

	snapshot := trace.Snapshot()
	if len(snapshot.Events) != 1 || snapshot.Events[0].Decision == nil {
		t.Fatalf("snapshot events = %+v", snapshot.Events)
	}
	clean := snapshot.Events[0].Decision
	if len(clean.Providers) != 1 || clean.Providers[0] != "gemini" || clean.Affinity != "" ||
		clean.CandidateScope != "complete" {
		t.Fatalf("clean providers/affinity/scope = %+v", clean)
	}
	if len(clean.Candidates) != 1 {
		t.Fatalf("clean candidates = %+v", clean.Candidates)
	}
	candidate := clean.Candidates[0]
	if candidate.Provider != "" || candidate.Model != "" || candidate.TargetModel != "model-a" ||
		candidate.EffectiveWeight != 0 || candidate.Native == nil || !*candidate.Native ||
		candidate.QuotaHeadroom != nil || candidate.InFlight != nil {
		t.Fatalf("clean candidate = %+v", candidate)
	}
	if len(candidate.MatchedRules) != 2 || candidate.MatchedRules[0] != "highest_priority" ||
		candidate.MatchedRules[1] != "round_robin_group" || len(candidate.ExcludedReasons) != 1 ||
		candidate.ExcludedReasons[0] != "disabled" {
		t.Fatalf("clean candidate rules = %+v/%+v", candidate.MatchedRules, candidate.ExcludedReasons)
	}
	cooldown := candidate.Cooldowns.ExactModel
	if !cooldown.Known || !cooldown.Active || cooldown.RetryAfterMS != 0 || cooldown.Reason != "" ||
		cooldown.EffectKey != "" || cooldown.ResetAuthority != "" || cooldown.StateGeneration != 7 {
		t.Fatalf("clean cooldown = %+v", cooldown)
	}

	clean.Providers[0] = "snapshot-mutated"
	clean.Candidates[0].MatchedRules[0] = "snapshot-rule-mutated"
	clean.Candidates[0].Native = new(false)
	second := trace.Snapshot().Events[0].Decision
	if second.Providers[0] != "gemini" || second.Candidates[0].MatchedRules[0] != "highest_priority" ||
		second.Candidates[0].Native == nil || !*second.Candidates[0].Native {
		t.Fatalf("stored decision changed through snapshot: %+v", second)
	}

	encoded, errMarshal := json.Marshal(second)
	if errMarshal != nil {
		t.Fatalf("Marshal() error = %v", errMarshal)
	}
	for _, secret := range []string{"bearer secret", "token-secret", "password=secret", "private-reset"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("sanitized decision retained %q: %s", secret, encoded)
		}
	}
}

func TestRouteTraceReplacesCredentialEffectKeysWithAuthIndex(t *testing.T) {
	const (
		authIndex = "0123456789abcdef"
		rawAuthID = "credential-secret.json"
	)
	trace := NewRouteTrace()
	trace.Add(
		cliproxyexecutor.RouteTraceEvent{
			AuthIndex:      authIndex,
			Action:         "route.decision",
			DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
			Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			RetryDirective: cliproxyexecutor.RetryDirectiveNextAuth,
			EffectScope:    cliproxyexecutor.StateEffectScopeCredential,
			EffectKey:      rawAuthID,
			Decision: &cliproxyexecutor.RouteDecisionSnapshot{
				Strategy:       "round-robin",
				CandidateScope: "complete",
				Candidates: []cliproxyexecutor.RouteDecisionCandidate{
					{
						AuthIndex: authIndex,
						Cooldowns: cliproxyexecutor.RouteDecisionCooldowns{
							Credential: cliproxyexecutor.RouteDecisionCooldown{
								Known:     true,
								EffectKey: rawAuthID,
							},
						},
					},
				},
			},
		},
	)

	snapshot := trace.Snapshot()
	if len(snapshot.Events) != 1 || snapshot.Events[0].Decision == nil {
		t.Fatalf("snapshot events = %+v", snapshot.Events)
	}
	event := snapshot.Events[0]
	if event.EffectKey != authIndex {
		t.Fatalf("event effect key = %q, want auth index", event.EffectKey)
	}
	candidate := event.Decision.Candidates[0]
	if candidate.Cooldowns.Credential.EffectKey != authIndex {
		t.Fatalf("credential cooldown effect key = %q, want auth index", candidate.Cooldowns.Credential.EffectKey)
	}
	encoded, errMarshal := json.Marshal(event)
	if errMarshal != nil {
		t.Fatalf("Marshal() error = %v", errMarshal)
	}
	if strings.Contains(string(encoded), rawAuthID) {
		t.Fatalf("route trace retained raw auth ID: %s", encoded)
	}
}

func TestRouteDecisionDropsInvalidSelectedIdentity(t *testing.T) {
	trace := NewRouteTrace()
	trace.Add(
		cliproxyexecutor.RouteTraceEvent{
			Action: "route.decision",
			Decision: &cliproxyexecutor.RouteDecisionSnapshot{
				Strategy: "custom",
				Selected: &cliproxyexecutor.RouteDecisionSelection{
					AuthIndex: "credential.json",
				},
			},
		},
	)
	snapshot := trace.Snapshot()
	if len(snapshot.Events) != 1 || snapshot.Events[0].Decision == nil {
		t.Fatalf("snapshot events = %+v", snapshot.Events)
	}
	if snapshot.Events[0].Decision.Selected != nil {
		t.Fatalf("invalid selected identity retained: %+v", snapshot.Events[0].Decision.Selected)
	}
}

func TestRouteDecisionSanitizesQuotaAwareSignals(t *testing.T) {
	freshHeadroom := 0.6
	freshReset := int64(1200)
	freshInFlight := int64(2)
	staleHeadroom := 0.4
	staleReset := int64(300)
	negative := int64(-1)
	trace := NewRouteTrace()
	trace.Add(
		cliproxyexecutor.RouteTraceEvent{
			Action: "route.decision",
			Decision: &cliproxyexecutor.RouteDecisionSnapshot{
				Strategy:       "quota-aware",
				MatchedRule:    "quota_aware",
				CandidateScope: "complete",
				Candidates: []cliproxyexecutor.RouteDecisionCandidate{
					{
						AuthIndex:         "0123456789abcdef",
						QuotaStatus:       "fresh",
						QuotaHeadroom:     &freshHeadroom,
						QuotaResetAfterMS: &freshReset,
						InFlight:          &freshInFlight,
						MatchedRules: []string{
							"quota_available_score",
							"quota_reset_proximity",
							"quota_pool_cursor",
						},
					},
					{
						AuthIndex:         "1123456789abcdef",
						QuotaStatus:       "stale",
						QuotaHeadroom:     &staleHeadroom,
						QuotaResetAfterMS: &staleReset,
						InFlight:          &freshInFlight,
						MatchedRules:      []string{"quota_weighted_fallback"},
					},
					{
						AuthIndex:         "2123456789abcdef",
						QuotaStatus:       "invalid",
						QuotaHeadroom:     &staleHeadroom,
						QuotaResetAfterMS: &negative,
						InFlight:          &negative,
					},
				},
			},
		},
	)

	freshHeadroom = 0.1
	freshReset = 1
	freshInFlight = 99
	decision := trace.Snapshot().Events[0].Decision
	if decision == nil || decision.Strategy != "quota-aware" || decision.MatchedRule != "quota_aware" {
		t.Fatalf("quota-aware decision = %+v", decision)
	}
	fresh := decision.Candidates[0]
	if fresh.QuotaStatus != "fresh" || fresh.QuotaHeadroom == nil || *fresh.QuotaHeadroom != 0.6 ||
		fresh.QuotaResetAfterMS == nil || *fresh.QuotaResetAfterMS != 1200 ||
		fresh.InFlight == nil || *fresh.InFlight != 2 || len(fresh.MatchedRules) != 3 {
		t.Fatalf("fresh quota signals = %+v", fresh)
	}
	stale := decision.Candidates[1]
	if stale.QuotaStatus != "stale" || stale.QuotaHeadroom != nil || stale.QuotaResetAfterMS != nil ||
		stale.InFlight == nil || *stale.InFlight != 2 || len(stale.MatchedRules) != 1 {
		t.Fatalf("stale quota signals = %+v", stale)
	}
	invalid := decision.Candidates[2]
	if invalid.QuotaStatus != "" || invalid.QuotaHeadroom != nil || invalid.QuotaResetAfterMS != nil ||
		invalid.InFlight != nil {
		t.Fatalf("invalid quota signals = %+v", invalid)
	}
}
