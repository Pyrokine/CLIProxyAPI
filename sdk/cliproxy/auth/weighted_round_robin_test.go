package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestWeightedRoundRobinSelectorDistributesAndSkipsNonPositiveWeights(t *testing.T) {
	selector := &WeightedRoundRobinSelector{}
	auths := []*Auth{
		{ID: "a", Attributes: map[string]string{AttributeWeight: "5"}},
		{ID: "b", Attributes: map[string]string{AttributeWeight: "3"}},
		{ID: "c", Attributes: map[string]string{AttributeWeight: "2"}},
		{ID: "excluded", Attributes: map[string]string{AttributeWeight: "0"}},
	}

	counts := make(map[string]int)
	for index := range 100 {
		picked, err := selector.Pick(context.Background(), "gemini", "model", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatalf("Pick() #%d error = %v", index, err)
		}
		counts[picked.ID]++
	}
	for authID, want := range map[string]int{"a": 50, "b": 30, "c": 20, "excluded": 0} {
		if counts[authID] != want {
			t.Fatalf("auth %q picks = %d, want %d; all = %#v", authID, counts[authID], want, counts)
		}
	}
}

func TestSchedulerWeightedRoundRobinHonorsPriorityBeforeWeight(t *testing.T) {
	scheduler := newSchedulerForTest(
		&WeightedRoundRobinSelector{},
		&Auth{
			ID: "heavy-low", Provider: "gemini", Attributes: map[string]string{"priority": "1", AttributeWeight: "100"},
		},
		&Auth{
			ID: "light-high", Provider: "gemini", Attributes: map[string]string{"priority": "10", AttributeWeight: "1"},
		},
	)
	for index := range 20 {
		picked, err := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("pickSingle() #%d error = %v", index, err)
		}
		if picked.ID != "light-high" {
			t.Fatalf("pickSingle() #%d auth.ID = %q, want light-high", index, picked.ID)
		}
	}
}

func TestSchedulerWeightedRoundRobinSkipsZeroWeightPriorityTier(t *testing.T) {
	scheduler := newSchedulerForTest(
		&WeightedRoundRobinSelector{},
		&Auth{
			ID: "excluded", Provider: "gemini", Attributes: map[string]string{"priority": "10", AttributeWeight: "0"},
		},
		&Auth{
			ID: "available", Provider: "gemini", Attributes: map[string]string{"priority": "0", AttributeWeight: "1"},
		},
	)
	picked, err := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickSingle() error = %v", err)
	}
	if picked == nil || picked.ID != "available" {
		t.Fatalf("pickSingle() = %#v, want available", picked)
	}
}

func TestSchedulerMixedProvidersUsesCredentialWeights(t *testing.T) {
	scheduler := newSchedulerForTest(
		&WeightedRoundRobinSelector{},
		&Auth{ID: "gemini-a", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "5"}},
		&Auth{ID: "claude-b", Provider: "claude", Attributes: map[string]string{AttributeWeight: "3"}},
		&Auth{ID: "claude-c", Provider: "claude", Attributes: map[string]string{AttributeWeight: "2"}},
	)
	counts := make(map[string]int)
	providers := make(map[string]int)
	for index := range 100 {
		picked, provider, err := scheduler.pickMixed(
			context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil,
		)
		if err != nil {
			t.Fatalf("pickMixed() #%d error = %v", index, err)
		}
		counts[picked.ID]++
		providers[provider]++
	}
	for authID, want := range map[string]int{"gemini-a": 50, "claude-b": 30, "claude-c": 20} {
		if counts[authID] != want {
			t.Fatalf("auth %q picks = %d, want %d; all = %#v", authID, counts[authID], want, counts)
		}
	}
	if providers["gemini"] != 50 || providers["claude"] != 50 {
		t.Fatalf("provider picks = %#v, want gemini:claude=50:50", providers)
	}
}

func TestSchedulerWeightedRoundRobinResetsCreditsWhenWeightsChange(t *testing.T) {
	authA := &Auth{ID: "a", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1000000"}}
	authB := &Auth{ID: "b", Provider: "gemini", Attributes: map[string]string{AttributeWeight: "1"}}
	scheduler := newSchedulerForTest(&WeightedRoundRobinSelector{}, authA, authB)
	for index := range 1000 {
		if _, err := scheduler.pickSingle(
			context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil,
		); err != nil {
			t.Fatalf("warmup pickSingle() #%d error = %v", index, err)
		}
	}

	authA.Attributes[AttributeWeight] = "1"
	scheduler.upsertAuth(authA)
	counts := make(map[string]int)
	for index := range 20 {
		picked, err := scheduler.pickSingle(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if err != nil {
			t.Fatalf("pickSingle() after weight change #%d error = %v", index, err)
		}
		counts[picked.ID]++
	}
	if counts["a"] != 10 || counts["b"] != 10 {
		t.Fatalf("picks after weight change = %#v, want a:b=10:10", counts)
	}
}

func TestSessionAffinityWeightedBindingRebindsAfterWeightBecomesZero(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(
		SessionAffinityConfig{
			Fallback: &WeightedRoundRobinSelector{},
			TTL:      time.Hour,
		},
	)
	defer selector.Stop()

	authA := &Auth{ID: "auth-a", Attributes: map[string]string{AttributeWeight: "1"}}
	authB := &Auth{ID: "auth-b", Attributes: map[string]string{AttributeWeight: "1"}}
	auths := []*Auth{authA, authB}
	opts := cliproxyexecutor.Options{Headers: http.Header{"X-Session-ID": {"weighted-session"}}}
	first, err := selector.Pick(context.Background(), "claude", "model", opts, auths)
	if err != nil {
		t.Fatalf("first Pick() error = %v", err)
	}
	if first.ID != authA.ID {
		t.Fatalf("first Pick() auth.ID = %q, want %q", first.ID, authA.ID)
	}

	authA.Attributes[AttributeWeight] = "0"
	second, err := selector.Pick(context.Background(), "claude", "model", opts, auths)
	if err != nil {
		t.Fatalf("Pick() after weight update error = %v", err)
	}
	if second.ID != authB.ID {
		t.Fatalf("Pick() after weight update auth.ID = %q, want %q", second.ID, authB.ID)
	}

	authA.Attributes[AttributeWeight] = "10"
	third, err := selector.Pick(context.Background(), "claude", "model", opts, auths)
	if err != nil {
		t.Fatalf("Pick() after re-enable error = %v", err)
	}
	if third.ID != authB.ID {
		t.Fatalf("Pick() after re-enable auth.ID = %q, want sticky auth %q", third.ID, authB.ID)
	}
}

func TestApplyAuthWeightMetadataNormalizesPersistedValue(t *testing.T) {
	metadata := map[string]any{AttributeWeight: json.Number("-5")}
	auth := &Auth{Metadata: metadata}
	if err := ApplyAuthWeightMetadata(auth, metadata); err != nil {
		t.Fatalf("ApplyAuthWeightMetadata() error = %v", err)
	}
	if auth.Attributes[AttributeWeight] != "0" {
		t.Fatalf("attribute weight = %q, want 0", auth.Attributes[AttributeWeight])
	}
	if metadata[AttributeWeight] != int64(0) {
		t.Fatalf("metadata weight = %#v, want int64(0)", metadata[AttributeWeight])
	}
}
