package auth

import (
	"testing"
	"time"
)

func TestTransientCooldownCannotOverflowDuration(t *testing.T) {
	previous := transientErrorCooldownSeconds.Load()
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(previous) })
	const maximum = int64((1<<63 - 1) / time.Second)
	seconds := int(^uint(0) >> 1)
	SetTransientErrorCooldownSeconds(seconds)
	want := min(int64(seconds), maximum)
	if got := transientErrorCooldownSeconds.Load(); got != want {
		t.Fatalf("stored cooldown = %d, want %d", got, want)
	}
	now := time.Unix(0, 0)
	if got := recoverableFailureRetryAfterWithHint(now, nil, false).Sub(now); got != time.Duration(want)*time.Second {
		t.Fatalf("cooldown = %v, want %v", got, time.Duration(want)*time.Second)
	}
	SetTransientErrorCooldownSeconds(-1)
	if next := recoverableFailureRetryAfterWithHint(now, nil, false); !next.IsZero() {
		t.Fatalf("negative cooldown produced a retry time: %v", next)
	}
}
