package quota

import (
	"math"
	"testing"
	"time"
)

func TestParseSchedulingWindowNormalizesProviders(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		credType     CredentialType
		model        string
		data         string
		wantHeadroom float64
		wantReset    time.Time
	}{
		{
			name:     "claude strictest generic and model window",
			credType: TypeClaude,
			model:    "claude-sonnet-4-5",
			data: `{"usage":{
				"five_hour":{"utilization":20,"resets_at":"2026-09-08T13:00:00Z"},
				"seven_day":{"utilization":35,"resets_at":"2026-09-15T12:00:00Z"},
				"seven_day_sonnet":{"utilization":70,"resets_at":"2026-09-14T12:00:00Z"},
				"seven_day_opus":{"utilization":95,"resets_at":"2026-09-13T12:00:00Z"}
			}}`,
			wantHeadroom: 0.30,
			wantReset:    time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC),
		},
		{
			name:     "codex strictest window",
			credType: TypeCodex,
			model:    "gpt-5-codex",
			data: `{"rate_limit":{"allowed":true,
				"primary_window":{"used_percent":25,"reset_after_seconds":1800},
				"secondary_window":{"used_percent":60,"reset_at":"2026-09-15T12:00:00Z"}
			}}`,
			wantHeadroom: 0.40,
			wantReset:    time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC),
		},
		{
			name:     "gemini model scoped bucket",
			credType: TypeGeminiCli,
			model:    "gemini-2.5-pro",
			data: `{"quota":{"buckets":[
				{"modelId":"gemini-2.5-flash","remainingFraction":0.9},
				{"modelId":"gemini-2.5-pro","remainingFraction":0.6,"resetTime":"2026-09-09T12:00:00Z"},
				{"remainingFraction":0.8}
			]}}`,
			wantHeadroom: 0.60,
			wantReset:    time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC),
		},
		{
			name:         "kimi amount window",
			credType:     TypeKimi,
			data:         `{"limits":[{"detail":{"limit":100,"used":25,"reset_in":3600}},{"detail":{"limit":10,"remaining":2}}]}`,
			wantHeadroom: 0.20,
		},
		{
			name:     "xai normalized quota",
			credType: TypeXAI,
			model:    "grok-4",
			data: `{"normalized":{"usage_percent":42.5,
				"usage_period":{"end":"2026-10-01T00:00:00Z"},
				"product_usage":[{"product":"grok-4","usage_percent":80}]
			}}`,
			wantHeadroom: 0.20,
			wantReset:    time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				window, err := parseSchedulingWindow(tt.credType, []byte(tt.data), tt.model, now)
				if err != nil {
					t.Fatalf("parseSchedulingWindow() error = %v", err)
				}
				if math.Abs(window.headroom-tt.wantHeadroom) > 1e-9 {
					t.Fatalf("headroom = %v, want %v", window.headroom, tt.wantHeadroom)
				}
				if tt.wantReset.IsZero() {
					return
				}
				if window.resetAt == nil || !window.resetAt.Equal(tt.wantReset) {
					t.Fatalf("resetAt = %v, want %v", window.resetAt, tt.wantReset)
				}
			},
		)
	}
}

func TestParseSchedulingWindowRejectsUnsafeModelScope(t *testing.T) {
	_, err := parseSchedulingWindow(
		TypeGeminiCli,
		[]byte(`{"quota":{"buckets":[{"modelId":"gemini-2.5-flash","remainingFraction":0.8}]}}`),
		"gemini-2.5-pro",
		time.Now(),
	)
	if err == nil {
		t.Fatal("model-mismatched quota bucket was accepted")
	}
}

func TestParseSchedulingWindowRejectsTrailingNestedJSON(t *testing.T) {
	_, errParse := parseSchedulingWindow(
		TypeGeminiCli,
		[]byte(`{"quota":"{\"buckets\":[{\"remainingFraction\":0.8}]} {\"extra\":true}"}`),
		"gemini-2.5-pro",
		time.Now(),
	)
	if errParse == nil {
		t.Fatal("nested quota with trailing JSON was accepted")
	}
}

func TestSchedulerSchedulingSnapshotsFreshStaleUnknown(t *testing.T) {
	scheduler := NewScheduler(DefaultConfig(), nil)
	scheduler.Register("fresh.json", TypeCodex, "fresh-index")
	scheduler.Register("stale.json", TypeCodex, "stale-index")

	freshKey := EntryKey(TypeCodex, "fresh.json")
	staleKey := EntryKey(TypeCodex, "stale.json")
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	scheduler.mu.Lock()
	fresh := scheduler.entries[freshKey]
	fresh.mu.Lock()
	fresh.Status = StatusSuccess
	fresh.Freshness = FreshnessFresh
	fresh.Data = []byte(`{"rate_limit":{"primary_window":{"used_percent":30,"reset_after_seconds":60}}}`)
	fresh.LastRefresh = new(now)
	fresh.ObservedAt = new(now)
	fresh.mu.Unlock()
	stale := scheduler.entries[staleKey]
	stale.mu.Lock()
	stale.Status = StatusError
	stale.Freshness = freshnessStale
	stale.Data = []byte(`{"rate_limit":{"primary_window":{"used_percent":10}}}`)
	stale.LastRefresh = new(now)
	stale.ObservedAt = new(now)
	stale.mu.Unlock()
	scheduler.mu.Unlock()

	snapshots := scheduler.SchedulingSnapshots(
		TypeCodex,
		[]string{"fresh-index", "stale-index", "missing-index"},
		"gpt-5-codex",
		now,
	)
	freshSnapshot := snapshots["fresh-index"]
	if freshSnapshot.Status != SchedulingStatusFresh || math.Abs(freshSnapshot.Headroom-0.7) > 1e-9 {
		t.Fatalf("fresh snapshot = %#v", freshSnapshot)
	}
	if freshSnapshot.ResetAt == nil || !freshSnapshot.ResetAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("fresh reset = %v", freshSnapshot.ResetAt)
	}
	if freshSnapshot.ObservedAt == nil || !freshSnapshot.ObservedAt.Equal(now) {
		t.Fatalf("fresh observedAt = %v", freshSnapshot.ObservedAt)
	}
	if snapshots["stale-index"].Status != SchedulingStatusStale {
		t.Fatalf("stale snapshot = %#v", snapshots["stale-index"])
	}
	if snapshots["missing-index"].Status != SchedulingStatusUnknown {
		t.Fatalf("unknown snapshot = %#v", snapshots["missing-index"])
	}
}

func TestSchedulerSchedulingSnapshotsDoesNotPublishRetainedDataAfterFailure(t *testing.T) {
	scheduler := NewScheduler(DefaultConfig(), nil)
	scheduler.Register("claude.json", TypeClaude, "claude-index")
	key := EntryKey(TypeClaude, "claude.json")
	scheduler.mu.Lock()
	entry := scheduler.entries[key]
	entry.mu.Lock()
	entry.Status = statusLoading
	entry.Data = []byte(`{"usage":{"five_hour":{"utilization":5}}}`)
	entry.mu.Unlock()
	scheduler.mu.Unlock()

	snapshot := scheduler.SchedulingSnapshots(
		TypeClaude, []string{"claude-index"}, "claude-sonnet-4", time.Now(),
	)["claude-index"]
	if snapshot.Status != SchedulingStatusStale || snapshot.Headroom != 0 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}
