package auth

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSessionCachePublicDefaultTTLKeepsThirtyMinutes(t *testing.T) {
	for _, constructor := range []struct {
		name string
		new  func(time.Duration) *SessionCache
	}{
		{name: "default capacity", new: NewSessionCache},
		{
			name: "explicit capacity",
			new:  func(ttl time.Duration) *SessionCache { return NewSessionCacheWithCapacity(ttl, 4) },
		},
	} {
		for _, ttl := range []time.Duration{0, -time.Nanosecond, 20 * time.Minute, time.Hour} {
			t.Run(
				constructor.name+"/"+ttl.String(), func(t *testing.T) {
					cache := constructor.new(ttl)
					defer cache.Stop()
					want := ttl
					if want <= 0 {
						want = 30 * time.Minute
					}
					before := time.Now()
					cache.Set("synthetic-session", "synthetic-auth")
					after := time.Now()
					cache.mu.RLock()
					entry := cache.entries["synthetic-session"]
					cache.mu.RUnlock()
					if cache.ttl != want || entry.expiresAt.Before(before.Add(want)) ||
						entry.expiresAt.After(after.Add(want)) {
						t.Fatalf("ttl=%v expiry=%v, want %v within Set interval", cache.ttl, entry.expiresAt, want)
					}
					if _, found := cache.Get("synthetic-session"); !found {
						t.Fatal("newly stored binding is unavailable")
					}
					cache.mu.Lock()
					entry.expiresAt = entry.expiresAt.Add(-31 * time.Minute)
					cache.entries["synthetic-session"] = entry
					cache.groups["synthetic-session"] = entry
					cache.mu.Unlock()
					if _, found := cache.Get("synthetic-session"); found != (want > 31*time.Minute) {
						t.Fatalf("binding availability after 31 minutes = %v, ttl=%v", found, want)
					}
				},
			)
		}
	}
}

func TestSessionAffinityInternalDefaultRemainsOneHour(t *testing.T) {
	for _, selector := range []*SessionAffinitySelector{
		NewSessionAffinitySelector(nil),
		NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{}),
	} {
		if selector.cache.ttl != time.Hour {
			selector.Stop()
			t.Fatalf("internal selector cache ttl=%v, want one hour", selector.cache.ttl)
		}
		selector.Stop()
	}
}

func TestSessionCacheSetGetAndInvalidate(t *testing.T) {
	cache := NewSessionCache(time.Hour)
	defer cache.Stop()

	cache.Set("gemini::m1::s1", "auth-a")
	cache.Set("gemini::m1::s2", "auth-b")

	if got, ok := cache.Get("gemini::m1::s1"); !ok || got != "auth-a" {
		t.Fatalf("Get() = %q, %v, want auth-a, true", got, ok)
	}
	if got, ok := cache.GetAndRefresh("gemini::m1::s2"); !ok || got != "auth-b" {
		t.Fatalf("GetAndRefresh() = %q, %v, want auth-b, true", got, ok)
	}

	cache.InvalidateAuth("auth-a")
	if _, ok := cache.Get("gemini::m1::s1"); ok {
		t.Fatal("expected auth-a binding to be invalidated")
	}
	if got, ok := cache.Get("gemini::m1::s2"); !ok || got != "auth-b" {
		t.Fatalf("expected unrelated binding to remain, got %q %v", got, ok)
	}
}

func TestSessionCacheEvictsWholeAliasGroups(t *testing.T) {
	t.Parallel()

	cache := NewSessionCacheWithCapacity(time.Hour, 4)
	defer cache.Stop()
	cache.SetAliases("auth-a", "a-primary", "a-alias")
	cache.SetAliases("auth-b", "b-primary", "b-alias")
	cache.Set("c-primary", "auth-c")

	if cache.Len() > 4 {
		t.Fatalf("Len() = %d, want <= 4", cache.Len())
	}
	if _, ok := cache.Get("a-primary"); ok {
		t.Fatal("oldest primary alias remains after group eviction")
	}
	if _, ok := cache.Get("a-alias"); ok {
		t.Fatal("oldest secondary alias remains after group eviction")
	}
	if got, ok := cache.Get("b-primary"); !ok || got != "auth-b" {
		t.Fatalf("newer group = %q, %v", got, ok)
	}
}

func TestSessionCacheTouchUpdatesAliasGroupLRU(t *testing.T) {
	t.Parallel()

	cache := NewSessionCacheWithCapacity(time.Hour, 2)
	defer cache.Stop()
	cache.Set("first", "auth-a")
	cache.Set("second", "auth-b")
	if !cache.Touch("first", "auth-a") {
		t.Fatal("Touch() did not refresh the live first binding")
	}
	cache.Set("third", "auth-c")

	if got, ok := cache.Get("first"); !ok || got != "auth-a" {
		t.Fatalf("recently touched binding = %q, %v", got, ok)
	}
	if _, ok := cache.Get("second"); ok {
		t.Fatal("least recently used alias group was not evicted")
	}
	if got, ok := cache.Get("third"); !ok || got != "auth-c" {
		t.Fatalf("new binding = %q, %v", got, ok)
	}
}

func TestSessionCacheExpectedAuthGuardsLateResults(t *testing.T) {
	t.Parallel()

	cache := NewSessionCache(time.Hour)
	defer cache.Stop()
	cache.SetAliases("auth-old", "child", "alias")
	cache.SetAliases("auth-new", "child", "alias")

	if cache.Touch("child", "auth-old") {
		t.Fatal("late success refreshed rebound binding")
	}
	if cache.CompareAndDelete("child", "auth-old") {
		t.Fatal("late failure deleted rebound binding")
	}
	if got, ok := cache.Get("child"); !ok || got != "auth-new" {
		t.Fatalf("child binding = %q, %v, want auth-new", got, ok)
	}
	if !cache.CompareAndDelete("child", "auth-new") {
		t.Fatal("current auth could not delete child alias")
	}
	if _, ok := cache.Get("child"); ok {
		t.Fatal("deleted child alias remains")
	}
	if got, ok := cache.Get("alias"); !ok || got != "auth-new" {
		t.Fatalf("surviving alias = %q, %v, want auth-new", got, ok)
	}
}

func TestSessionCacheConcurrentAccessAndStop(t *testing.T) {
	t.Parallel()

	cache := NewSessionCacheWithCapacity(time.Hour, 128)
	var workers sync.WaitGroup
	for worker := range 12 {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			authID := fmt.Sprintf("auth-%d", worker)
			for iteration := range 300 {
				key := fmt.Sprintf("session-%d-%d", worker, iteration)
				cache.Set(key, authID)
				cache.GetAndRefresh(key)
				cache.Touch(key, authID)
				if iteration%7 == 0 {
					cache.CompareAndDelete(key, authID)
				}
			}
		}(worker)
	}
	for range 8 {
		workers.Go(
			func() {
				cache.Stop()
			},
		)
	}
	workers.Wait()
	if cache.Len() > 128 {
		t.Fatalf("Len() = %d, want <= 128", cache.Len())
	}
}
