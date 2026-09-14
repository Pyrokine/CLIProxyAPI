package access

import (
	"fmt"
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
	for range maxFailures {
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

	for range maxFailures {
		rl.RecordFailure(ip)
	}
	if !rl.IsLimited(ip) {
		t.Fatal("expected limited immediately after lockout")
	}

	// Manually expire the lockout by reaching into the entry.
	rl.ipEntries.mu.Lock()
	entry := rl.ipEntries.entries[ip]
	if entry == nil {
		rl.ipEntries.mu.Unlock()
		t.Fatal("expected entry to exist")
	}
	entry.lockedUntil = time.Now().Add(-1 * time.Second)
	rl.ipEntries.mu.Unlock()

	if rl.IsLimited(ip) {
		t.Fatal("expected not limited after lockout expiry")
	}
}

func TestLockoutEscalatesAfterRepeatFailures(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "10.0.0.2"
	for range maxFailures {
		rl.RecordFailure(ip)
	}

	rl.ipEntries.mu.Lock()
	entry := rl.ipEntries.entries[ip]
	if entry == nil {
		rl.ipEntries.mu.Unlock()
		t.Fatal("expected entry to exist")
	}
	firstUntil := entry.lockedUntil
	entry.lockedUntil = time.Now().Add(-time.Second)
	entry.firstFailure = time.Now().Add(-failureWindow - time.Second)
	rl.ipEntries.mu.Unlock()

	for range maxFailures {
		rl.RecordFailure(ip)
	}

	rl.ipEntries.mu.Lock()
	secondUntil := entry.lockedUntil
	level := entry.lockoutLevel
	rl.ipEntries.mu.Unlock()

	if level != 2 {
		t.Fatalf("lockoutLevel = %d, want 2", level)
	}
	if !secondUntil.After(firstUntil) {
		t.Fatalf("expected second lockout to last longer, firstUntil=%s secondUntil=%s", firstUntil, secondUntil)
	}
}

func TestRecordSuccess_ClearsFailures(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()

	ip := "172.16.0.1"

	// Record some failures (not enough to trigger lockout).
	for range maxFailures - 1 {
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
	for range maxFailures - 1 {
		rl.RecordFailure(ip)
	}

	// Simulate window expiry by backdating firstFailure.
	rl.ipEntries.mu.Lock()
	entry := rl.ipEntries.entries[ip]
	if entry == nil {
		rl.ipEntries.mu.Unlock()
		t.Fatal("expected entry to exist")
	}
	entry.firstFailure = time.Now().Add(-failureWindow - time.Second)
	rl.ipEntries.mu.Unlock()

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
	for range goroutines {
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

func TestRateLimitCapacityKeepsLockoutsAndNewFailures(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()
	now := time.Now()
	key := func(index int) string {
		return fmt.Sprintf("2001:db8::%x", index)
	}
	rl.ipEntries.mu.Lock()
	for i := range maxRateLimitEntriesPerAxis {
		entry := &rateLimitEntry{
			failures:     1,
			firstFailure: now.Add(-time.Minute),
			lastActivity: now.Add(-time.Hour).Add(time.Duration(i) * time.Millisecond),
		}
		if i == 0 {
			entry.lockedUntil = now.Add(lockoutDuration)
		}
		rl.ipEntries.addEntryLocked(key(i), entry)
	}
	rl.ipEntries.mu.Unlock()

	newKey := "2001:db8::ffff"
	rl.RecordFailure(newKey)
	rl.ipEntries.mu.Lock()
	defer rl.ipEntries.mu.Unlock()
	if got := len(rl.ipEntries.entries); got != maxRateLimitEntriesPerAxis {
		t.Fatalf("tracked entries = %d, want %d", got, maxRateLimitEntriesPerAxis)
	}
	if rl.ipEntries.entries[key(0)] == nil {
		t.Fatal("active lockout was discarded at capacity")
	}
	if rl.ipEntries.entries[newKey] == nil {
		t.Fatal("new failure was not retained at capacity")
	}
	discardedUnlocked := false
	for i := 1; i < maxRateLimitEntriesPerAxis; i++ {
		if rl.ipEntries.entries[key(i)] == nil {
			discardedUnlocked = true
			break
		}
	}
	if !discardedUnlocked {
		t.Fatal("no unlocked entry was discarded at capacity")
	}
}

func TestRateLimitCapacityFailsClosedWhenAllEntriesAreLocked(t *testing.T) {
	rl := NewAuthRateLimiter()
	defer rl.Stop()
	now := time.Now()
	rl.ipEntries.mu.Lock()
	for i := range maxRateLimitEntriesPerAxis {
		key := fmt.Sprintf("2001:db8::%x", i)
		rl.ipEntries.addEntryLocked(
			key, &rateLimitEntry{
				lockedUntil:  now.Add(lockoutDuration),
				lastActivity: now,
			},
		)
	}
	rl.ipEntries.mu.Unlock()

	if !rl.IsLimited("2001:db8::ffff") {
		t.Fatal("unknown client was allowed while capacity contained only active lockouts")
	}
}

func TestHashAccountKeyUsesFullSHA256(t *testing.T) {
	got := HashAccountKey(" account-secret ")
	if len(got) != 64 {
		t.Fatalf("hash length = %d, want 64", len(got))
	}
	if got != HashAccountKey("account-secret") {
		t.Fatal("account hash did not normalize surrounding whitespace")
	}
}
