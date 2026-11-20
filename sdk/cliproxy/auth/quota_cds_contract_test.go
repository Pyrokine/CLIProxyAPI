package auth

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestQuotaObservationCDSRoundTripKeepsLatestFilteredSnapshot(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "devin"} {
		for _, model := range []string{"", "quota-model"} {
			for _, runtimeAge := range []string{"absent", "older", "equal", "newer"} {
				t.Run(
					fmt.Sprintf("%s/model=%s/runtime=%s", provider, model, runtimeAge), func(t *testing.T) {
						ctx := WithSkipPersist(context.Background())
						now := time.Now().UTC()
						observedAt := now.Add(-time.Hour)
						name := "X-Codex-Primary-Used-Percent"
						savedValue, runtimeValue := "75", "11"
						if provider == "claude" {
							name = "Anthropic-Ratelimit-Unified-5h-Utilization"
							savedValue, runtimeValue = "0.75", "0.11"
						}
						if provider == "devin" {
							name = "daily_quota_remaining_percent"
							savedValue, runtimeValue = "75%", "11%"
						}
						manager := NewManager(nil, nil, nil)
						savedQuota := QuotaState{
							ObservedAt: observedAt,
							Signals: map[string]string{
								name: savedValue, "Authorization": "synthetic-not-a-credential",
								"X-Codex-Turn-State":      "synthetic-opaque",
								"X-Codex-Credits-Balance": strings.Repeat("x", 513),
							},
						}
						cooling := &Auth{
							ID: t.Name(), Provider: provider, Status: StatusError, Unavailable: true,
							NextRetryAfter: now.Add(time.Hour), Quota: savedQuota,
						}
						if model != "" {
							cooling.Status, cooling.Unavailable, cooling.NextRetryAfter = StatusActive,
								false, time.Time{}
							cooling.Quota = QuotaState{}
							cooling.ModelStates = map[string]*ModelState{
								model: {
									Status: StatusError, Unavailable: true, NextRetryAfter: now.Add(time.Hour),
									Quota: savedQuota,
								},
							}
						}
						registered, err := manager.Register(ctx, cooling)
						if err != nil {
							t.Fatal(err)
						}
						store := NewFileCooldownStateStore(filepath.Join(t.TempDir(), "cds"))
						manager.SetCooldownStateStore(store)
						manager.PersistCooldownStates(ctx)
						records, err := store.Load(ctx)
						if err != nil || len(records) != 1 {
							t.Fatalf("CDS load records=%+v error=%v", records, err)
						}
						wantSaved := map[string]string{name: savedValue}
						if !records[0].Quota.ObservedAt.Equal(observedAt) || !reflect.DeepEqual(
							records[0].Quota.Signals, wantSaved,
						) {
							t.Errorf("CDS did not preserve only the filtered snapshot: %+v", records[0].Quota)
						}
						var runtimeQuota QuotaState
						if runtimeAge != "absent" {
							runtimeQuota = QuotaState{
								ObservedAt: observedAt, Signals: map[string]string{name: runtimeValue},
							}
							switch runtimeAge {
							case "older":
								runtimeQuota.ObservedAt = observedAt.Add(-time.Minute)
							case "newer":
								runtimeQuota.ObservedAt = observedAt.Add(time.Minute)
							}
						}
						restored := NewManager(nil, nil, nil)
						current := &Auth{
							ID: registered.ID, Provider: provider, Status: StatusActive, Quota: runtimeQuota,
						}
						if model != "" {
							current.Quota = QuotaState{}
							current.ModelStates = map[string]*ModelState{
								model: {
									Status: StatusActive, Quota: runtimeQuota,
								},
							}
						}
						if _, err := restored.Register(ctx, current); err != nil {
							t.Fatal(err)
						}
						restored.SetCooldownStateStore(store)
						if err := restored.RestoreCooldownStates(ctx); err != nil {
							t.Fatal(err)
						}
						actual, _ := restored.GetByID(registered.ID)
						quota, unavailable, deadline := actual.Quota, actual.Unavailable, actual.NextRetryAfter
						if model != "" {
							state := actual.ModelStates[model]
							quota, unavailable, deadline = state.Quota, state.Unavailable, state.NextRetryAfter
						}
						wantValue, wantTime := savedValue, observedAt
						if runtimeAge == "equal" || runtimeAge == "newer" {
							wantValue, wantTime = runtimeValue, runtimeQuota.ObservedAt
						}
						if quota.Signals[name] != wantValue || !quota.ObservedAt.Equal(wantTime) ||
							!unavailable ||
							!deadline.Equal(now.Add(time.Hour)) {
							t.Fatalf(
								"restored quota=%+v unavailable=%t deadline=%v; want value=%s time=%v "+
									"and original "+
									"cooldown",
								quota,
								unavailable,
								deadline,
								wantValue,
								wantTime,
							)
						}
					},
				)
			}
		}
	}
}

func TestCooldownQuotaSnapshotDoesNotShareObservationStorage(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	credential, err := manager.Register(
		WithSkipPersist(context.Background()), &Auth{
			ID: t.Name(), Provider: "codex", Status: StatusError, Unavailable: true,
			NextRetryAfter: time.Now().Add(time.Hour),
			Quota: QuotaState{
				ObservedAt: time.Unix(10, 0), Signals: map[string]string{"X-Codex-Primary-Used-Percent": "75"},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	records := manager.cooldownStateRecordsSnapshot()
	if len(records) != 1 {
		t.Fatal(records)
	}
	records[0].Quota.Signals["X-Codex-Primary-Used-Percent"] = "mutated-store-snapshot"
	current, _ := manager.GetByID(credential.ID)
	if current.Quota.Signals["X-Codex-Primary-Used-Percent"] != "75" {
		t.Fatal("CDS snapshot mutation reached runtime quota")
	}
}

func TestQuotaCDSRestoreFiltersUntrustedSavedSignals(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	credential, err := manager.Register(
		WithSkipPersist(context.Background()), &Auth{ID: t.Name(), Provider: "codex", Status: StatusActive},
	)
	if err != nil {
		t.Fatal(err)
	}
	store := &recordingCooldownStateStore{
		load: []CooldownStateRecord{
			{
				AuthID: credential.ID, Provider: "codex", NextRetryAfter: time.Now().Add(time.Hour),
				Quota: QuotaState{
					ObservedAt: time.Unix(10, 0), Signals: map[string]string{
						"X-Codex-Primary-Used-Percent": "75", "Authorization": "synthetic",
						"X-Codex-Plan-Type": "bad\u0085value", "X-Codex-Credits-Balance": strings.Repeat("x", 513),
					},
				},
			},
		},
	}
	manager.SetCooldownStateStore(store)
	if err := manager.RestoreCooldownStates(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, _ := manager.GetByID(credential.ID)
	if !reflect.DeepEqual(
		current.Quota.Signals, map[string]string{"X-Codex-Primary-Used-Percent": "75"},
	) || !current.Quota.ObservedAt.Equal(time.Unix(10, 0)) {
		t.Fatalf("untrusted CDS observation=%+v", current.Quota)
	}
	if !current.Unavailable || current.Status != StatusError {
		t.Fatalf("cooldown state was lost: %+v", current)
	}
}
