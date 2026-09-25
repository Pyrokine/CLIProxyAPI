package auth

import (
	"context"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestCredentialCoolingOverridePresence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		value    bool
		present  bool
	}{
		{name: "missing"},
		{name: "false", metadata: map[string]any{"disable_cooling": false}, present: true},
		{name: "true", metadata: map[string]any{"disable_cooling": true}, value: true, present: true},
		{name: "legacy_false", metadata: map[string]any{"disable-cooling": false}, present: true},
		{name: "string_false", metadata: map[string]any{"disable_cooling": "false"}, present: true},
		{name: "canonical_false", metadata: map[string]any{"disable_cooling": false, "disable-cooling": true}, present: true},
		{name: "invalid", metadata: map[string]any{"disable_cooling": "invalid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{Metadata: tc.metadata}
			value, present := auth.DisableCoolingOverride()
			if value != tc.value || present != tc.present {
				t.Fatalf("override=(%t,%t), want (%t,%t)", value, present, tc.value, tc.present)
			}
		})
	}
}

func TestCredentialRetryOverrideInheritance(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]any
		value    int
		present  bool
	}{
		{name: "missing"},
		{name: "negative", metadata: map[string]any{"request_retry": -1}},
		{name: "zero", metadata: map[string]any{"request_retry": 0}, present: true},
		{name: "positive", metadata: map[string]any{"request_retry": 2}, value: 2, present: true},
		{name: "legacy_negative", metadata: map[string]any{"request-retry": -1}},
		{name: "canonical_inherit", metadata: map[string]any{"request_retry": -1, "request-retry": 2}},
		{name: "string_inherit", metadata: map[string]any{"request_retry": "-1"}},
		{name: "canonical_zero", metadata: map[string]any{"request_retry": 0, "request-retry": 2}, present: true},
		{name: "invalid", metadata: map[string]any{"request_retry": "invalid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := &Auth{Metadata: tc.metadata}
			value, present := auth.RequestRetryOverride()
			if value != tc.value || present != tc.present {
				t.Fatalf("override=(%d,%t), want (%d,%t)", value, present, tc.value, tc.present)
			}
		})
	}
}

func TestCredentialRetryOverrideReachesCooldownBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   int
		attempt int
		allowed bool
	}{
		{name: "inherit_second_round", value: -1, attempt: 1, allowed: true},
		{name: "inherit_exhausted", value: -1, attempt: 2},
		{name: "explicit_zero", value: 0},
		{name: "explicit_one", value: 1, allowed: true},
		{name: "explicit_one_exhausted", value: 1, attempt: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, _ := newExecutionOutcomeTestManager(t, &Auth{
				ID: "retry-override", Provider: "test", Status: StatusError,
				Unavailable: true, NextRetryAfter: time.Now().Add(time.Minute), Quota: QuotaState{Exceeded: true},
				Metadata: map[string]any{"request_retry": tc.value},
			})
			manager.RegisterExecutor(schedulerTestExecutor{})
			registerReservationContextModels(t, "test", "model-a", "retry-override")
			manager.SetRetryConfig(2, time.Minute, 0)
			wait, allowed := manager.closestCooldownWait(context.Background(), cliproxyexecutor.Options{}, []string{"test"}, "model-a", tc.attempt, 0)
			if allowed != tc.allowed || (allowed && wait <= 0) {
				t.Fatalf("wait=%v allowed=%t, want %t", wait, allowed, tc.allowed)
			}
		})
	}
}

func TestCredentialCoolingOverrideReachesMarkResult(t *testing.T) {
	for _, global := range []bool{false, true} {
		for _, override := range []*bool{nil, new(false), new(true)} {
			metadata := map[string]any{}
			if override != nil {
				metadata["disable_cooling"] = *override
			}
			manager, current := newExecutionOutcomeTestManager(t, &Auth{
				ID: "cooling-override", Provider: "provider-a", Status: StatusActive, Metadata: metadata,
			})
			manager.SetConfig(&internalconfig.Config{DisableCooling: global})
			manager.MarkResult(context.Background(), executionResult(current, current.Provider, "model-a", executionOutcomeStatusError{status: 401}, cliproxyexecutor.DispatchPhaseMaybeSent))
			after := getExecutionOutcomeTestAuth(t, manager, current.ID)
			disabled := global
			if override != nil {
				disabled = *override
			}
			if after.Unavailable == disabled || after.NextRetryAfter.IsZero() != disabled {
				t.Fatalf("global=%t metadata=%v unavailable=%t deadline=%v", global, metadata, after.Unavailable, after.NextRetryAfter)
			}
		}
	}
}
