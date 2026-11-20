package auth

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOriginalRefreshDurationInputs(t *testing.T) {
	for _, sample := range []struct {
		name  string
		value any
		want  time.Duration
	}{
		{"duration", 90 * time.Second, 90 * time.Second},
		{"int", 90, 90 * time.Second},
		{"int32", int32(90), 90 * time.Second},
		{"int64", int64(90), 90 * time.Second},
		{"uint", uint(90), 90 * time.Second},
		{"uint32", uint32(90), 90 * time.Second},
		{"uint64", uint64(90), 90 * time.Second},
		{"float32", float32(1.5), 1500 * time.Millisecond},
		{"float64", 1.5, 1500 * time.Millisecond},
		{"json integer", json.Number("90"), 90 * time.Second},
		{"json fraction", json.Number("1.5"), 1500 * time.Millisecond},
		{"duration string", " 1m30s ", 90 * time.Second},
		{"seconds string", " 1.5 ", 1500 * time.Millisecond},
		{"zero", 0, 0},
		{"negative", -1, 0},
		{"invalid string", "invalid", 0},
		{"unsupported", false, 0},
		{"nil", nil, 0},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				if got := parseDurationValue(sample.value); got != sample.want {
					t.Fatalf("duration=%s, want %s", got, sample.want)
				}
				credential := &Auth{Metadata: map[string]any{"refresh_interval_seconds": sample.value}}
				if got := authPreferredInterval(credential); got != sample.want {
					t.Fatalf("preferred interval=%s, want %s", got, sample.want)
				}
			},
		)
	}
	if got := authPreferredInterval(
		&Auth{
			Metadata:   map[string]any{"refresh_interval_seconds": 90},
			Attributes: map[string]string{"refreshInterval": "1s"},
		},
	); got != 90*time.Second {
		t.Fatalf("metadata interval priority=%s", got)
	}
	if authPreferredInterval(nil) != 0 {
		t.Fatal("nil credential has a preferred interval")
	}
}

func TestOriginalRefreshTimestampFallbacks(t *testing.T) {
	last := time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)
	manager := NewManager(nil, nil, nil)
	for _, source := range []string{"metadata", "attributes"} {
		for _, key := range []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"} {
			t.Run(
				source+"/"+key, func(t *testing.T) {
					credential := &Auth{
						Metadata:   map[string]any{"refresh_interval_seconds": "invalid"},
						Attributes: map[string]string{"refreshInterval": "2h"},
					}
					if source == "metadata" {
						credential.Metadata[key] = last.Format(time.RFC3339)
					} else {
						credential.Attributes[key] = last.Format(time.RFC3339)
					}
					if got, ok := authLastRefreshTimestamp(credential); !ok || !got.Equal(last) {
						t.Fatalf("last refresh=%v, found=%t", got, ok)
					}
					boundary := last.Add(2 * time.Hour)
					if manager.shouldRefresh(
						credential, boundary.Add(-time.Nanosecond),
					) || !manager.shouldRefresh(credential, boundary) {
						t.Fatal("metadata timestamp or attribute interval lost its refresh boundary")
					}
				},
			)
		}
	}
	if got, ok := authLastRefreshTimestamp(nil); ok || !got.IsZero() {
		t.Fatalf("nil credential timestamp=%v, found=%t", got, ok)
	}
}

func TestOriginalRefreshInvalidGrantBackoffInputs(t *testing.T) {
	for failures, minutes := range []int{1, 1, 2, 4, 8, 16, 30, 30, 30} {
		if got := invalidGrantBackoffDuration(failures); got != time.Duration(minutes)*time.Minute {
			t.Fatalf("failures=%d backoff=%s, want %d min", failures, got, minutes)
		}
	}
	for _, failures := range []int{-1, 64} {
		want := time.Minute
		if failures > 1 {
			want = 30 * time.Minute
		}
		if got := invalidGrantBackoffDuration(failures); got != want {
			t.Fatalf("failures=%d backoff=%s, want %s", failures, got, want)
		}
	}
}
