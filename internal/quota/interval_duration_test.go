package quota

import (
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"
)

func TestQuotaRefreshBackoffDoesNotOverflowBeforeCapping(t *testing.T) {
	now := time.Date(2026, time.November, 11, 10, 0, 0, 0, time.UTC)
	cfg := Config{Interval: 1 << 30, MaxInterval: 1 << 30}
	entry := &Entry{FailureCount: 4}
	applyRefreshFailureLocked(entry, errors.New("synthetic transport failure"), now, cfg)
	want := time.Duration(cfg.MaxInterval) * time.Second
	if entry.NextRefresh == nil || !entry.NextRefresh.After(now) || entry.NextRefresh.Sub(now) != want {
		t.Fatalf("retry=%v, want=%v", entry.NextRefresh, now.Add(want))
	}
}

func TestQuotaRefreshIntervalCannotScheduleInThePast(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("the oversized interval requires a 64-bit int")
	}
	huge := int64(1) << 34
	cfg := Config{Interval: int(huge), MaxInterval: int(huge)}
	scheduler := NewScheduler(cfg, nil)
	if got := scheduler.GetConfig(); int64(got.Interval) != MaxRefreshIntervalSeconds ||
		int64(got.MaxInterval) != MaxRefreshIntervalSeconds {
		t.Fatalf("oversized config was not limited: %+v", got)
	}
	scheduler.UpdateConfig(cfg)
	if got := scheduler.GetConfig(); int64(got.Interval) != MaxRefreshIntervalSeconds ||
		int64(got.MaxInterval) != MaxRefreshIntervalSeconds {
		t.Fatalf("updated oversized config was not limited: %+v", got)
	}
	now := time.Date(2026, time.November, 11, 10, 0, 0, 0, time.UTC)
	entry := &Entry{}
	applyRefreshSuccessLocked(
		entry, FetchResult{Data: []byte(`{"quota":"ok"}`), ObservedAt: now, FetchedAt: now}, now, scheduler.GetConfig(),
	)
	if entry.NextRefresh == nil || !entry.NextRefresh.After(now) {
		t.Fatalf("next refresh=%v must follow %v", entry.NextRefresh, now)
	}
}

func TestQuotaNonPositiveIntervalsRetainDefaults(t *testing.T) {
	values := []int{0, -1}
	if strconv.IntSize == 64 {
		large := -int64(1) << 34
		values = append(values, int(large))
	}
	now := time.Date(2026, time.November, 11, 10, 0, 0, 0, time.UTC)
	for _, value := range values {
		cfg := Config{Interval: value, MaxInterval: value}
		scheduler := NewScheduler(cfg, nil)
		if scheduler.GetConfig() != cfg {
			t.Fatalf("legacy default configuration changed: %+v", scheduler.GetConfig())
		}
		success := &Entry{}
		applyRefreshSuccessLocked(success, FetchResult{Data: []byte(`{}`), ObservedAt: now, FetchedAt: now}, now, cfg)
		if success.NextRefresh == nil || !success.NextRefresh.Equal(now.Add(600*time.Second)) {
			t.Fatalf("default next refresh=%v", success.NextRefresh)
		}
		failed := &Entry{FailureCount: 4}
		applyRefreshFailureLocked(failed, errors.New("synthetic transport failure"), now, cfg)
		if failed.RetryAt == nil || !failed.RetryAt.Equal(now.Add(1800*time.Second)) {
			t.Fatalf("default retry=%v", failed.RetryAt)
		}
	}
}

func TestQuotaAbsoluteResetDoesNotOverflow(t *testing.T) {
	for _, value := range []any{1e300, "1e300", float64(253402300800), float64(253402300800000)} {
		if reset := quotaAbsoluteTime(value); reset != nil {
			t.Errorf("unsupported absolute reset %v created deadline %v", value, reset)
		}
	}
	for _, seconds := range []int64{1, 1794391200, 253402300799} {
		if reset := quotaAbsoluteTime(seconds); reset == nil || reset.Unix() != seconds {
			t.Errorf("absolute reset %v must preserve Unix seconds %v", reset, seconds)
		}
	}
	for _, milliseconds := range []int64{1794391200123, 253402300799999} {
		reset := quotaAbsoluteTime(milliseconds)
		if reset == nil || reset.UnixMilli() != milliseconds {
			t.Fatalf("absolute reset %v must preserve Unix milliseconds %v", reset, milliseconds)
		}
		if _, err := reset.MarshalJSON(); err != nil {
			t.Errorf("absolute reset %v cannot be serialized: %v", reset, err)
		}
	}
}

func TestQuotaRelativeResetDoesNotOverflow(t *testing.T) {
	observed := time.Date(2026, time.November, 11, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		kind CredentialType
		body string
	}{
		{
			name: "codex", kind: TypeCodex,
			body: `{"rate_limit":{"primary_window":{"used_percent":25,"reset_after_seconds":%s}}}`,
		},
		{name: "kimi", kind: TypeKimi, body: `{"limits":[{"detail":{"limit":100,"used":25,"reset_in":%s}}]}`},
	} {
		for _, seconds := range []string{"0", "60", "17179869184", "1e300"} {
			t.Run(
				test.name+"/"+seconds, func(t *testing.T) {
					window, err := parseSchedulingWindow(
						test.kind, []byte(fmt.Sprintf(test.body, seconds)), "model", observed,
					)
					if err != nil {
						t.Fatalf("parse: %v", err)
					}
					if seconds == "0" || seconds == "60" {
						value, _ := strconv.Atoi(seconds)
						if window.resetAt == nil ||
							!window.resetAt.Equal(observed.Add(time.Duration(value)*time.Second)) {
							t.Fatalf("reset=%v for %s seconds", window.resetAt, seconds)
						}
					} else if window.resetAt != nil {
						t.Fatalf("unsupported relative reset %s created deadline %v", seconds, window.resetAt)
					}
				},
			)
		}
	}
}
