package helps

import (
	"net/http"
	"testing"
)

func TestNewTransportCacheDefaultsCapacity(t *testing.T) {
	for _, capacity := range []int{0, -1} {
		cache := NewTransportCache[cacheKey](capacity)
		if cache.capacity != DefaultTransportCacheCapacity {
			t.Fatalf("capacity=%d, want 64 for input=%d", cache.capacity, capacity)
		}
	}
}

func TestTransportCachePurgeAndNilSafety(t *testing.T) {
	cache := NewTransportCache[cacheKey](4)
	build := func() (*http.Transport, error) { return &http.Transport{}, nil }
	for _, key := range []cacheKey{{"a", ""}, {"b", ""}} {
		if _, err := cache.Get(key, build); err != nil {
			t.Fatal(err)
		}
	}
	if cache.Len() != 2 {
		t.Fatalf("cache length=%d, want 2", cache.Len())
	}
	cache.Purge()
	if cache.Len() != 0 {
		t.Fatalf("cache length after purge=%d", cache.Len())
	}
	if transport, err := cache.Get(cacheKey{"a", ""}, build); err != nil || transport == nil {
		t.Fatalf("retry after purge=%p, %v", transport, err)
	}
	var nilCache *TransportCache[cacheKey]
	if _, err := nilCache.Get(cacheKey{}, build); err == nil {
		t.Fatal("nil cache accepted Get")
	}
	if nilCache.Len() != 0 {
		t.Fatal("nil cache length changed")
	}
	nilCache.Purge()
	if _, err := cache.Get(cacheKey{"nil-build", ""}, nil); err == nil {
		t.Fatal("nil builder accepted")
	}
}
