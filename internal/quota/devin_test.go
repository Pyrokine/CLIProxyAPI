package quota

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestDevinBindingsUseAuthScopedEntryKeys(t *testing.T) {
	fetches := make(chan string, 2)
	scheduler := NewSchedulerWithResult(DefaultConfig(), func(_ context.Context, entry *Entry) (FetchResult, error) {
		fetches <- entry.AuthIndex
		return FetchResult{
			Data:         []byte(`{"daily_quota_present":true,"daily_quota_remaining_percent":50}`),
			Source:       "devin_user_status",
			ObservedAt:   time.Now().UTC(),
			Completeness: FetchComplete,
		}, nil
	})
	scheduler.RegisterBinding("devin-shared.json", TypeDevin, "auth-a", "revision-a")
	scheduler.RegisterBinding("devin-shared.json", TypeDevin, "auth-b", "revision-b")
	scheduler.SetBindingDisabled("devin-shared.json", TypeDevin, "auth-b", true)

	keyA := BindingEntryKey(TypeDevin, "devin-shared.json", "auth-a")
	keyB := BindingEntryKey(TypeDevin, "devin-shared.json", "auth-b")
	status := scheduler.GetStatus()
	if len(status.Credentials) != 2 || status.Credentials[keyA] == nil || status.Credentials[keyB] == nil {
		t.Fatalf("Devin credentials = %#v", status.Credentials)
	}
	if status.Credentials[keyA].Disabled || !status.Credentials[keyB].Disabled {
		t.Fatalf("disabled state leaked between identities: a=%t b=%t",
			status.Credentials[keyA].Disabled, status.Credentials[keyB].Disabled)
	}

	targets, unresolved := scheduler.ResolveRefreshTargets([]string{keyA})
	if len(unresolved) != 0 || len(targets) != 1 || targets[0].AuthIndex != "auth-a" {
		t.Fatalf("resolved targets = %#v unresolved=%#v", targets, unresolved)
	}
	result := scheduler.RefreshTarget(context.Background(), targets[0])
	if result.Outcome != RefreshOutcomeSucceeded {
		t.Fatalf("refresh result = %#v", result)
	}
	if got := <-fetches; got != "auth-a" {
		t.Fatalf("refreshed auth index = %q, want auth-a", got)
	}
	status = scheduler.GetStatus()
	if status.Credentials[keyA].Status != StatusSuccess || status.Credentials[keyB].Status != StatusIdle {
		t.Fatalf("single identity refresh changed sibling: a=%s b=%s",
			status.Credentials[keyA].Status, status.Credentials[keyB].Status)
	}

	scheduler.UnregisterBinding("devin-shared.json", TypeDevin, "auth-a")
	status = scheduler.GetStatus()
	if status.Credentials[keyA] != nil || status.Credentials[keyB] == nil {
		t.Fatalf("single identity removal changed wrong bindings: %#v", status.Credentials)
	}
	scheduler.Unregister("devin-shared.json")
	if got := scheduler.GetStatus().Credentials; len(got) != 0 {
		t.Fatalf("file removal retained Devin bindings: %#v", got)
	}
}

func TestParseDevinSchedulingWindowUsesStrictestPresentQuota(t *testing.T) {
	weeklyReset := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	data, err := json.Marshal(map[string]any{
		"daily_quota_present":            true,
		"daily_quota_remaining_percent":  80,
		"daily_quota_reset_at":           "2026-09-17T12:00:00Z",
		"weekly_quota_present":           true,
		"weekly_quota_remaining_percent": 35,
		"weekly_quota_reset_at":          weeklyReset.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("marshal quota fixture: %v", err)
	}
	window, err := parseSchedulingWindow(TypeDevin, data, "devin/swe-2", time.Now())
	if err != nil {
		t.Fatalf("parseSchedulingWindow() error = %v", err)
	}
	if math.Abs(window.headroom-0.35) > 1e-9 {
		t.Fatalf("headroom = %f, want 0.35", window.headroom)
	}
	if window.resetAt == nil || !window.resetAt.Equal(weeklyReset) {
		t.Fatalf("resetAt = %v, want %v", window.resetAt, weeklyReset)
	}

	zero, err := parseSchedulingWindow(TypeDevin, []byte(
		`{"daily_quota_present":true,"daily_quota_remaining_percent":0,"weekly_quota_present":false}`,
	), "", time.Now())
	if err != nil || zero.headroom != 0 {
		t.Fatalf("explicit zero window = %#v, error = %v", zero, err)
	}
	if _, err = parseSchedulingWindow(TypeDevin, []byte(
		`{"daily_quota_present":false,"weekly_quota_present":false}`,
	), "", time.Now()); err == nil {
		t.Fatal("absent Devin quota produced a scheduling window")
	}
}
