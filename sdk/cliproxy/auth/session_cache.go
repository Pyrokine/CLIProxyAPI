package auth

import (
	"strings"
	"sync"
	"time"
)

type sessionBinding struct {
	authID    string
	expiresAt time.Time
	updatedAt time.Time
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
	interval := c.ttl / 2
	if interval < time.Minute {
		interval = time.Minute
	}
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
	for key, binding := range c.entries {
		if !binding.expiresAt.After(now) {
			delete(c.entries, key)
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
			delete(c.entries, key)
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
			delete(c.entries, key)
		}
		return "", false
	}
	binding.updatedAt = now
	binding.expiresAt = now.Add(c.ttl)
	c.entries[key] = binding
	return binding.authID, true
}

func (c *SessionCache) Set(key, authID string) {
	if c == nil {
		return
	}
	key = strings.TrimSpace(key)
	authID = strings.TrimSpace(authID)
	if key == "" || authID == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	c.entries[key] = sessionBinding{authID: authID, updatedAt: now, expiresAt: now.Add(c.ttl)}
	c.mu.Unlock()
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
