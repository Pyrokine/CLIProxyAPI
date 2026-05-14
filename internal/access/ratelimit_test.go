package access

import (
	"sync"
	"testing"
	"time"
)

func TestRecordFailure_TriggersLockout(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "192.168.1.100"

	// Before any failures, should not be limited.
	if rl.IsLimited(ip) {
		t.Fatal("expected not limited before any failures")
	}

	// Record maxFailures failures.
	for i := 0; i < maxFailures; i++ {
		rl.RecordFailure(ip)
	}

	// Should now be limited.
	if !rl.IsLimited(ip) {
		t.Fatal("expected limited after maxFailures failures")
	}
}

func TestLockoutExpires(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.1"

	for i := 0; i < maxFailures; i++ {
		rl.RecordFailure(ip)
	}
	if !rl.IsLimited(ip) {
		t.Fatal("expected limited immediately after lockout")
	}

	// Manually expire the lockout by reaching into the entry.
	val, ok := rl.ipEntries.Load(ip)
	if !ok {
		t.Fatal("expected entry to exist")
	}
	entry := val.(*rateLimitEntry)
	entry.mu.Lock()
	entry.lockedUntil = time.Now().Add(-1 * time.Second)
	entry.mu.Unlock()

	if rl.IsLimited(ip) {
		t.Fatal("expected not limited after lockout expiry")
	}
}

func TestRecordSuccess_ClearsFailures(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "172.16.0.1"

	// Record some failures (not enough to trigger lockout).
	for i := 0; i < maxFailures-1; i++ {
		rl.RecordFailure(ip)
	}

	// Record success should clear the entry.
	rl.RecordSuccess(ip)

	// Now even after recording one more failure, should not be limited.
	rl.RecordFailure(ip)
	if rl.IsLimited(ip) {
		t.Fatal("expected not limited after success cleared failures")
	}
}

func TestWindowReset_FailuresOutsideWindowDontCount(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.10.10.10"

	// Record some failures.
	for i := 0; i < maxFailures-1; i++ {
		rl.RecordFailure(ip)
	}

	// Simulate window expiry by backdating firstFailure.
	val, ok := rl.ipEntries.Load(ip)
	if !ok {
		t.Fatal("expected entry to exist")
	}
	entry := val.(*rateLimitEntry)
	entry.mu.Lock()
	entry.firstFailure = time.Now().Add(-failureWindow - time.Second)
	entry.mu.Unlock()

	// Next failure resets the window, so only 1 failure in new window.
	rl.RecordFailure(ip)
	if rl.IsLimited(ip) {
		t.Fatal("expected not limited: window should have reset")
	}
}

func TestStop_Idempotent(t *testing.T) {
	rl := NewAuthRateLimiter()

	// Calling Stop twice should not panic.
	rl.Stop()
	rl.Stop()
}

func TestConcurrentAccess(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "concurrent-ip"
	var wg sync.WaitGroup
	goroutines := 50

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			rl.RecordFailure(ip)
			rl.IsLimited(ip)
		}()
	}
	wg.Wait()

	// After 50 failures, should definitely be limited.
	if !rl.IsLimited(ip) {
		t.Fatal("expected limited after concurrent failures exceeding threshold")
	}
}

func TestIsLimited_UnknownIP_ReturnsFalse(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	if rl.IsLimited("never-seen") {
		t.Fatal("expected false for unknown IP")
	}
}
