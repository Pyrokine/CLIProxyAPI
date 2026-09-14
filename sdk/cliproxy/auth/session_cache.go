package auth

import (
	"strings"
	"sync"
	"time"
)

const maxStableSessionAliases = 64

type sessionBinding struct {
	authID    string
	expiresAt time.Time
	updatedAt time.Time
	aliases   []string
}

// SessionCache keeps session-to-auth bindings in memory with TTL cleanup.
type SessionCache struct {
	ttl     time.Duration
	mu      sync.RWMutex
	entries map[string]sessionBinding
	stopCh  chan struct{}
	once    sync.Once
}

func NewSessionCache(ttl time.Duration) *SessionCache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	cache := &SessionCache{
		ttl:     ttl,
		entries: make(map[string]sessionBinding),
		stopCh:  make(chan struct{}),
	}
	go cache.cleanupLoop()
	return cache
}

func (c *SessionCache) cleanupLoop() {
	interval := max(c.ttl/2, time.Minute)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.cleanupExpired()
		case <-c.stopCh:
			return
		}
	}
}

func (c *SessionCache) cleanupExpired() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, binding := range c.entries {
		if !binding.expiresAt.After(now) {
			c.removeAliasGroupLocked(binding)
		}
	}
}

func (c *SessionCache) Stop() {
	if c == nil {
		return
	}
	c.once.Do(
		func() {
			close(c.stopCh)
		},
	)
}

func (c *SessionCache) Get(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}
	now := time.Now()
	c.mu.RLock()
	binding, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || !binding.expiresAt.After(now) {
		if ok {
			c.mu.Lock()
			if current, exists := c.entries[key]; exists && !current.expiresAt.After(time.Now()) {
				c.removeAliasGroupLocked(current)
			}
			c.mu.Unlock()
		}
		return "", false
	}
	return binding.authID, true
}

func (c *SessionCache) GetAndRefresh(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", false
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	binding, ok := c.entries[key]
	if !ok || !binding.expiresAt.After(now) {
		if ok {
			c.removeAliasGroupLocked(binding)
		}
		return "", false
	}
	aliases := compactSessionAliases(mergeSessionAliases([]string{key}, binding.aliases...))
	c.replaceAliasGroupsLocked(binding.authID, now, now.Add(c.ttl), aliases, binding)
	return binding.authID, true
}

func (c *SessionCache) Set(key, authID string) {
	c.SetAliases(authID, key)
}

func (c *SessionCache) SetAliases(authID string, keys ...string) {
	if c == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()

	aliases := mergeSessionAliases(nil, keys...)
	previous := make([]sessionBinding, 0, len(aliases))
	for _, key := range aliases {
		binding, ok := c.entries[key]
		if !ok {
			continue
		}
		if !binding.expiresAt.After(now) {
			c.removeAliasGroupLocked(binding)
			continue
		}
		previous = append(previous, binding)
		aliases = mergeSessionAliases(aliases, binding.aliases...)
	}
	aliases = compactSessionAliases(aliases)
	if len(aliases) == 0 {
		return
	}
	c.replaceAliasGroupsLocked(authID, now, now.Add(c.ttl), aliases, previous...)
}

func (c *SessionCache) replaceAliasGroupsLocked(
	authID string,
	updatedAt,
	expiresAt time.Time,
	aliases []string,
	previous ...sessionBinding,
) {
	for _, binding := range previous {
		c.removeAliasGroupLocked(binding)
	}
	binding := sessionBinding{authID: authID, updatedAt: updatedAt, expiresAt: expiresAt, aliases: aliases}
	for _, alias := range aliases {
		c.entries[alias] = binding
	}
}

func (c *SessionCache) removeAliasGroupLocked(binding sessionBinding) {
	for _, alias := range binding.aliases {
		current, ok := c.entries[alias]
		if !ok || current.authID != binding.authID || !current.expiresAt.Equal(binding.expiresAt) ||
			!equalSessionAliases(current.aliases, binding.aliases) {
			continue
		}
		delete(c.entries, alias)
	}
}

func mergeSessionAliases(existing []string, candidates ...string) []string {
	aliases := make([]string, 0, len(existing)+len(candidates))
	seen := make(map[string]struct{}, len(existing)+len(candidates))
	add := func(alias string) {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			return
		}
		if _, ok := seen[alias]; ok {
			return
		}
		seen[alias] = struct{}{}
		aliases = append(aliases, alias)
	}
	for _, alias := range existing {
		add(alias)
	}
	for _, alias := range candidates {
		add(alias)
	}
	return aliases
}

func compactSessionAliases(aliases []string) []string {
	compacted := make([]string, 0, len(aliases))
	hasPromptCacheKey := false
	stableAliases := 0
	for _, alias := range aliases {
		if strings.Contains(alias, "::pck:") || strings.HasPrefix(alias, "pck:") {
			if hasPromptCacheKey {
				continue
			}
			hasPromptCacheKey = true
		} else {
			if stableAliases >= maxStableSessionAliases {
				continue
			}
			stableAliases++
		}
		compacted = append(compacted, alias)
	}
	return compacted
}

func compactHomeSessionAliases(aliases []string) []string {
	compacted := make([]string, 0, len(aliases))
	hasPromptCacheKey := false
	stableAliases := 0
	for _, alias := range aliases {
		if strings.HasPrefix(alias, "pck:") {
			if hasPromptCacheKey {
				continue
			}
			hasPromptCacheKey = true
		} else {
			if stableAliases >= maxStableSessionAliases {
				continue
			}
			stableAliases++
		}
		compacted = append(compacted, alias)
	}
	return compacted
}

func equalSessionAliases(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (c *SessionCache) InvalidateAuth(authID string) {
	if c == nil {
		return
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, binding := range c.entries {
		if binding.authID == authID {
			delete(c.entries, key)
		}
	}
}
