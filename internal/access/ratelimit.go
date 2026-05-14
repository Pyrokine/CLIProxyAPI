package access

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

const (
	maxFailures      = 10
	failureWindow    = time.Minute
	lockoutDuration  = 60 * time.Second
	staleEntryMaxAge = 5 * time.Minute
	cleanupInterval  = time.Minute
)

type rateLimitEntry struct {
	mu           sync.Mutex
	failures     int
	firstFailure time.Time
	lockedUntil  time.Time
}

// AuthRateLimiter tracks authentication failures and enforces rate limits on two
// independent axes:
//   - per client IP (mitigates a single host brute-forcing)
//   - per candidate account identifier (mitigates attackers rotating IPs against one account)
//
// After maxFailures within failureWindow on either axis, subsequent requests are
// rejected with 429 until lockoutDuration elapses.
type AuthRateLimiter struct {
	ipEntries      sync.Map // ip string → *rateLimitEntry
	accountEntries sync.Map // accountHash string → *rateLimitEntry
	stopCh         chan struct{}
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
	return isLimited(&rl.ipEntries, ip)
}

// RecordFailure records an authentication failure for the given IP.
// If the failure count reaches the threshold within the window, the IP is locked out.
func (rl *AuthRateLimiter) RecordFailure(ip string) {
	recordFailure(&rl.ipEntries, ip)
}

// RecordSuccess clears all failure state for the given IP.
func (rl *AuthRateLimiter) RecordSuccess(ip string) {
	rl.ipEntries.Delete(ip)
}

// IsAccountLimited returns true if the given account identifier is currently locked out.
// Use HashAccountKey to derive accountHash from a raw credential — never pass raw secrets.
func (rl *AuthRateLimiter) IsAccountLimited(accountHash string) bool {
	if accountHash == "" {
		return false
	}
	return isLimited(&rl.accountEntries, accountHash)
}

// RecordAccountFailure records an authentication failure for the given account hash.
func (rl *AuthRateLimiter) RecordAccountFailure(accountHash string) {
	if accountHash == "" {
		return
	}
	recordFailure(&rl.accountEntries, accountHash)
}

// RecordAccountSuccess clears all failure state for the given account hash.
func (rl *AuthRateLimiter) RecordAccountSuccess(accountHash string) {
	if accountHash == "" {
		return
	}
	rl.accountEntries.Delete(accountHash)
}

// HashAccountKey derives a non-reversible identifier for a credential candidate.
// Returns empty string for empty input so callers can short-circuit safely.
func HashAccountKey(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:8])
}

func isLimited(entries *sync.Map, key string) bool {
	val, ok := entries.Load(key)
	if !ok {
		return false
	}
	entry := val.(*rateLimitEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return time.Now().Before(entry.lockedUntil)
}

func recordFailure(entries *sync.Map, key string) {
	now := time.Now()
	val, _ := entries.LoadOrStore(key, &rateLimitEntry{})
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
	cleanupMap := func(m *sync.Map) {
		m.Range(
			func(key, value any) bool {
				entry := value.(*rateLimitEntry)
				entry.mu.Lock()
				stale := now.Sub(entry.firstFailure) > staleEntryMaxAge && now.After(entry.lockedUntil)
				entry.mu.Unlock()
				if stale {
					m.Delete(key)
				}
				return true
			},
		)
	}
	cleanupMap(&rl.ipEntries)
	cleanupMap(&rl.accountEntries)
}
