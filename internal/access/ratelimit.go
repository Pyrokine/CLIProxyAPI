package access

import (
	"sync"
	"time"
)

const (
	maxFailures       = 10
	failureWindow     = time.Minute
	lockoutDuration   = 60 * time.Second
	staleEntryMaxAge  = 5 * time.Minute
	cleanupInterval   = time.Minute
)

type rateLimitEntry struct {
	mu           sync.Mutex
	failures     int
	firstFailure time.Time
	lockedUntil  time.Time
}

// AuthRateLimiter tracks per-IP authentication failures and enforces
// rate limits: after maxFailures failures within failureWindow, the IP
// is locked out for lockoutDuration.
type AuthRateLimiter struct {
	entries sync.Map // ip string → *rateLimitEntry
	stopCh  chan struct{}
}

// NewAuthRateLimiter creates a rate limiter and starts background cleanup.
func NewAuthRateLimiter() *AuthRateLimiter {
	rl := &AuthRateLimiter{
		stopCh: make(chan struct{}),
	}
	go rl.cleanupLoop()
	return rl
}

// Stop terminates the background cleanup goroutine. Safe to call multiple times.
func (rl *AuthRateLimiter) Stop() {
	select {
	case <-rl.stopCh:
		// already closed
	default:
		close(rl.stopCh)
	}
}

// IsLimited returns true if the given IP is currently locked out.
func (rl *AuthRateLimiter) IsLimited(ip string) bool {
	val, ok := rl.entries.Load(ip)
	if !ok {
		return false
	}
	entry := val.(*rateLimitEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return time.Now().Before(entry.lockedUntil)
}

// RecordFailure records an authentication failure for the given IP.
// If the failure count reaches the threshold within the window, the IP is locked out.
func (rl *AuthRateLimiter) RecordFailure(ip string) {
	now := time.Now()
	val, _ := rl.entries.LoadOrStore(ip, &rateLimitEntry{})
	entry := val.(*rateLimitEntry)

	entry.mu.Lock()
	defer entry.mu.Unlock()

	// Reset window if expired
	if now.Sub(entry.firstFailure) > failureWindow {
		entry.failures = 0
		entry.firstFailure = now
	}

	if entry.failures == 0 {
		entry.firstFailure = now
	}
	entry.failures++

	if entry.failures >= maxFailures {
		entry.lockedUntil = now.Add(lockoutDuration)
	}
}

// RecordSuccess clears all failure state for the given IP.
func (rl *AuthRateLimiter) RecordSuccess(ip string) {
	rl.entries.Delete(ip)
}

func (rl *AuthRateLimiter) cleanupLoop() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case <-ticker.C:
			rl.cleanup()
		}
	}
}

func (rl *AuthRateLimiter) cleanup() {
	now := time.Now()
	rl.entries.Range(func(key, value any) bool {
		entry := value.(*rateLimitEntry)
		entry.mu.Lock()
		stale := now.Sub(entry.firstFailure) > staleEntryMaxAge && now.After(entry.lockedUntil)
		entry.mu.Unlock()
		if stale {
			rl.entries.Delete(key)
		}
		return true
	})
}
