package auth

import (
	"testing"
	"time"
)

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
