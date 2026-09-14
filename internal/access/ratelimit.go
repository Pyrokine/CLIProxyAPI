package access

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	maxFailures                      = 10
	failureWindow                    = time.Minute
	lockoutDuration                  = 60 * time.Second
	maxLockoutDuration               = 30 * time.Minute
	staleEntryMaxAge                 = 5 * time.Minute
	cleanupInterval                  = time.Minute
	maxRateLimitEntriesPerAxis       = 4096
	rateLimitCapacityWarningInterval = time.Minute
)

type rateLimitEntry struct {
	key          string
	element      *list.Element
	failures     int
	firstFailure time.Time
	lockedUntil  time.Time
	lockoutLevel int
	lastActivity time.Time
}

type rateLimitAxis struct {
	mu                  sync.Mutex
	name                string
	entries             map[string]*rateLimitEntry
	order               *list.List
	saturatedUntil      time.Time
	lastCapacityWarning time.Time
}

func newRateLimitAxis(name string) *rateLimitAxis {
	return &rateLimitAxis{
		name:    name,
		entries: make(map[string]*rateLimitEntry),
		order:   list.New(),
	}
}

func (a *rateLimitAxis) addEntryLocked(key string, entry *rateLimitEntry) {
	if a.entries == nil {
		a.entries = make(map[string]*rateLimitEntry)
	}
	if a.order == nil {
		a.order = list.New()
	}
	entry.key = key
	entry.element = a.order.PushFront(entry)
	a.entries[key] = entry
}

func (a *rateLimitAxis) removeEntryLocked(entry *rateLimitEntry) {
	if entry == nil {
		return
	}
	delete(a.entries, entry.key)
	if a.order != nil && entry.element != nil {
		a.order.Remove(entry.element)
	}
	entry.element = nil
}

func (a *rateLimitAxis) isLimited(key string, now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if entry := a.entries[key]; entry != nil {
		return now.Before(entry.lockedUntil)
	}
	return a.capacityLimitedLocked(now)
}

func (a *rateLimitAxis) recordFailure(key string, now time.Time) {
	if a == nil {
		return
	}
	var (
		warnCapacity bool
		evicted      bool
	)
	a.mu.Lock()
	entry := a.entries[key]
	if entry == nil {
		if len(a.entries) >= maxRateLimitEntriesPerAxis {
			if !a.evictOneLocked(now) {
				warnCapacity = a.capacityWarningDueLocked(now)
				a.mu.Unlock()
				if warnCapacity {
					a.logCapacityWarning(false)
				}
				return
			}
			evicted = true
			warnCapacity = a.capacityWarningDueLocked(now)
		}
		entry = &rateLimitEntry{}
		a.addEntryLocked(key, entry)
	}

	if a.order != nil && entry.element != nil {
		a.order.MoveToFront(entry.element)
	}
	entry.lastActivity = now
	if now.Before(entry.lockedUntil) {
		a.mu.Unlock()
		if warnCapacity {
			a.logCapacityWarning(evicted)
		}
		return
	}
	if now.Sub(entry.firstFailure) > failureWindow {
		entry.failures = 0
		entry.firstFailure = now
	}
	if entry.failures == 0 {
		entry.firstFailure = now
	}
	entry.failures++
	if entry.failures >= maxFailures {
		entry.lockoutLevel++
		entry.lockedUntil = now.Add(lockoutDurationForLevel(entry.lockoutLevel))
		entry.failures = 0
		entry.firstFailure = now
	}
	a.mu.Unlock()
	if warnCapacity {
		a.logCapacityWarning(evicted)
	}
}

func (a *rateLimitAxis) delete(key string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.removeEntryLocked(a.entries[key])
	if len(a.entries) < maxRateLimitEntriesPerAxis {
		a.saturatedUntil = time.Time{}
	}
	a.mu.Unlock()
}

func (a *rateLimitAxis) evictOneLocked(now time.Time) bool {
	var earliestExpiry time.Time
	for element := a.order.Back(); element != nil; element = element.Prev() {
		entry, _ := element.Value.(*rateLimitEntry)
		if entry == nil || !now.Before(entry.lockedUntil) {
			a.removeEntryLocked(entry)
			a.saturatedUntil = time.Time{}
			return true
		}
		if earliestExpiry.IsZero() || entry.lockedUntil.Before(earliestExpiry) {
			earliestExpiry = entry.lockedUntil
		}
	}
	a.saturatedUntil = earliestExpiry
	return false
}

func (a *rateLimitAxis) capacityLimitedLocked(now time.Time) bool {
	if now.Before(a.saturatedUntil) {
		return true
	}
	if len(a.entries) < maxRateLimitEntriesPerAxis {
		a.saturatedUntil = time.Time{}
		return false
	}
	var earliestExpiry time.Time
	for _, entry := range a.entries {
		if entry == nil || !now.Before(entry.lockedUntil) {
			a.saturatedUntil = time.Time{}
			return false
		}
		if earliestExpiry.IsZero() || entry.lockedUntil.Before(earliestExpiry) {
			earliestExpiry = entry.lockedUntil
		}
	}
	a.saturatedUntil = earliestExpiry
	return true
}

func (a *rateLimitAxis) cleanup(now time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	for _, entry := range a.entries {
		if entry == nil ||
			(now.Sub(entry.lastActivity) > staleEntryMaxAge && now.After(entry.lockedUntil)) {
			a.removeEntryLocked(entry)
		}
	}
	if len(a.entries) < maxRateLimitEntriesPerAxis {
		a.saturatedUntil = time.Time{}
	}
	a.mu.Unlock()
}

func (a *rateLimitAxis) capacityWarningDueLocked(now time.Time) bool {
	if !a.lastCapacityWarning.IsZero() &&
		!now.Before(a.lastCapacityWarning) &&
		now.Sub(a.lastCapacityWarning) < rateLimitCapacityWarningInterval {
		return false
	}
	a.lastCapacityWarning = now
	return true
}

func (a *rateLimitAxis) logCapacityWarning(evicted bool) {
	entry := log.WithFields(
		log.Fields{
			"axis":     a.name,
			"capacity": maxRateLimitEntriesPerAxis,
		},
	)
	if evicted {
		entry.Warn("authentication rate limit tracking reached capacity; discarded unlocked state")
		return
	}
	entry.Warn("authentication rate limit tracking reached capacity; new state was not retained")
}

// AuthRateLimiter tracks authentication failures and enforces rate limits on two
// independent axes:
//   - per client IP (mitigates a single host brute-forcing)
//   - per candidate account identifier (mitigates attackers rotating IPs against one account)
//
// After maxFailures within failureWindow on either axis, subsequent requests are
// rejected with 429 until lockoutDuration elapses.
type AuthRateLimiter struct {
	ipEntries      *rateLimitAxis
	accountEntries *rateLimitAxis
	stopCh         chan struct{}
	stopOnce       sync.Once
}

// NewAuthRateLimiter creates a rate limiter and starts background cleanup.
func NewAuthRateLimiter() *AuthRateLimiter {
	rl := &AuthRateLimiter{
		ipEntries:      newRateLimitAxis("client_ip"),
		accountEntries: newRateLimitAxis("account"),
		stopCh:         make(chan struct{}),
	}
	go rl.cleanupLoop()
	return rl
}

// Stop terminates the background cleanup goroutine. Safe to call multiple times.
func (rl *AuthRateLimiter) Stop() {
	if rl == nil {
		return
	}
	rl.stopOnce.Do(func() { close(rl.stopCh) })
}

// IsLimited returns true if the given IP is currently locked out.
func (rl *AuthRateLimiter) IsLimited(ip string) bool {
	return rl != nil && rl.ipEntries.isLimited(ip, time.Now())
}

// RecordFailure records an authentication failure for the given IP.
// If the failure count reaches the threshold within the window, the IP is locked out.
func (rl *AuthRateLimiter) RecordFailure(ip string) {
	if rl != nil {
		rl.ipEntries.recordFailure(ip, time.Now())
	}
}

// RecordSuccess clears all failure state for the given IP.
func (rl *AuthRateLimiter) RecordSuccess(ip string) {
	if rl != nil {
		rl.ipEntries.delete(ip)
	}
}

// IsAccountLimited returns true if the given account identifier is currently locked out.
// Use HashAccountKey to derive accountHash from a raw credential — never pass raw secrets.
func (rl *AuthRateLimiter) IsAccountLimited(accountHash string) bool {
	return rl != nil && accountHash != "" && rl.accountEntries.isLimited(accountHash, time.Now())
}

// RecordAccountFailure records an authentication failure for the given account hash.
func (rl *AuthRateLimiter) RecordAccountFailure(accountHash string) {
	if rl != nil && accountHash != "" {
		rl.accountEntries.recordFailure(accountHash, time.Now())
	}
}

// RecordAccountSuccess clears all failure state for the given account hash.
func (rl *AuthRateLimiter) RecordAccountSuccess(accountHash string) {
	if rl != nil && accountHash != "" {
		rl.accountEntries.delete(accountHash)
	}
}

// HashAccountKey derives a non-reversible identifier for a credential candidate.
// Returns empty string for empty input so callers can short-circuit safely.
func HashAccountKey(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:])
}

func lockoutDurationForLevel(level int) time.Duration {
	if level <= 1 {
		return lockoutDuration
	}
	duration := lockoutDuration
	for i := 1; i < level; i++ {
		duration *= 2
		if duration >= maxLockoutDuration {
			return maxLockoutDuration
		}
	}
	return duration
}

func (rl *AuthRateLimiter) cleanupLoop() {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case now := <-ticker.C:
			rl.ipEntries.cleanup(now)
			rl.accountEntries.cleanup(now)
		}
	}
}
