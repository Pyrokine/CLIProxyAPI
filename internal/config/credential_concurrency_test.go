package config

import (
	"math"
	"testing"
	"time"
)

func TestCredentialConcurrencyLimiterConfig(t *testing.T) {
	got := (CredentialConcurrencyConfig{}).WithDefaults()
	if got.LifecycleConfigRevision != 0 || got.ObservationBarrierRevision != 0 {
		t.Fatalf("default revisions = %d, %d, want 0, 0", got.LifecycleConfigRevision, got.ObservationBarrierRevision)
	}
	if got.CPAHeartbeatTimeout != 3*time.Second || got.CPACancelBound != 5*time.Second ||
		got.ReclaimGrace != 5*time.Second || got.CleanupInterval != 5*time.Second {
		t.Fatalf("default lifecycle config = %#v", got)
	}
	if got.ReleaseFlushInterval != 250*time.Millisecond || got.ReleaseMaxBackoff != 2*time.Second ||
		got.BusyRetryMin != 250*time.Millisecond || got.BusyRetryMax != time.Second || got.MaxLimit != 1_000_000 {
		t.Fatalf("default limiter config = %#v", got)
	}
	if err := ValidateCredentialConcurrency(got); err != nil {
		t.Fatalf("ValidateCredentialConcurrency() error = %v", err)
	}
}

func TestCredentialConcurrencyAllowsHomeAuthoritativeHeartbeat(t *testing.T) {
	cfg := (CredentialConcurrencyConfig{}).WithDefaults()
	cfg.CPAHeartbeatTimeout = 20 * time.Second
	if err := ValidateCredentialConcurrency(cfg); err != nil {
		t.Fatalf("ValidateCredentialConcurrency() error = %v", err)
	}
}

func TestCredentialConcurrencyConfigDefaultsOnlyMissingFields(t *testing.T) {
	for _, test := range []struct {
		name, payload string
	}{
		{
			name: "explicit zero revision",
			payload: "credential-concurrency:\n" +
				"  lifecycle-config-revision: 0\n" +
				"  cpa-heartbeat-timeout: 3s\n" +
				"  cpa-cancel-bound: 5s\n" +
				"  reclaim-grace: 5s\n" +
				"  cleanup-interval: 5s\n",
		},
		{
			name: "explicit zero duration",
			payload: "credential-concurrency:\n" +
				"  lifecycle-config-revision: 1\n" +
				"  cpa-heartbeat-timeout: 0s\n" +
				"  cpa-cancel-bound: 5s\n" +
				"  reclaim-grace: 5s\n" +
				"  cleanup-interval: 5s\n",
		},
		{
			name: "explicit null duration",
			payload: "credential-concurrency:\n" +
				"  lifecycle-config-revision: 1\n" +
				"  cpa-heartbeat-timeout: null\n" +
				"  cpa-cancel-bound: 5s\n" +
				"  reclaim-grace: 5s\n" +
				"  cleanup-interval: 5s\n",
		},
		{
			name: "negative observation barrier",
			payload: "credential-concurrency:\n" +
				"  lifecycle-config-revision: 1\n" +
				"  observation-barrier-revision: -1\n" +
				"  cpa-heartbeat-timeout: 3s\n" +
				"  cpa-cancel-bound: 5s\n" +
				"  reclaim-grace: 5s\n" +
				"  cleanup-interval: 5s\n",
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				parsed, err := ParseConfigBytes([]byte(test.payload))
				if err != nil {
					t.Fatalf("ParseConfigBytes() error = %v", err)
				}
				if err := ValidateCredentialConcurrency(parsed.CredentialConcurrency.WithDefaults()); err == nil {
					t.Fatal("explicit invalid lifecycle value was accepted")
				}
			},
		)
	}
}

func TestCredentialConcurrencyConfigRejectsInvalidLimiter(t *testing.T) {
	for _, cfg := range []CredentialConcurrencyConfig{
		{
			ReleaseFlushInterval: time.Second, ReleaseMaxBackoff: 500 * time.Millisecond,
			BusyRetryMin: time.Millisecond, BusyRetryMax: time.Millisecond, MaxLimit: 1,
		},
		{
			ReleaseFlushInterval: time.Millisecond, ReleaseMaxBackoff: time.Millisecond,
			BusyRetryMin: 1500 * time.Microsecond, BusyRetryMax: 2 * time.Millisecond, MaxLimit: 1,
		},
		{
			ReleaseFlushInterval: time.Millisecond, ReleaseMaxBackoff: time.Millisecond,
			BusyRetryMin: time.Millisecond, BusyRetryMax: time.Millisecond, MaxLimit: 1_000_001,
		},
	} {
		cfg.CPAHeartbeatTimeout = 3 * time.Second
		cfg.CPACancelBound = 5 * time.Second
		cfg.ReclaimGrace = 5 * time.Second
		cfg.CleanupInterval = 5 * time.Second
		if err := ValidateCredentialConcurrency(cfg); err == nil {
			t.Fatalf("ValidateCredentialConcurrency(%#v) error = nil", cfg)
		}
	}
}

func TestCredentialConcurrencyRejectsMaxIntLimit(t *testing.T) {
	cfg := (CredentialConcurrencyConfig{}).WithDefaults()
	cfg.MaxLimit = math.MaxInt64
	if err := ValidateCredentialConcurrency(cfg); err == nil {
		t.Fatal("maximum int64 concurrency limit was accepted")
	}
}
