package helps

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type idleTransportFixture struct {
	transport *http.Transport
	closed    <-chan struct{}
	cleanup   func()
}

func newIdleTransportFixture(t *testing.T) idleTransportFixture {
	t.Helper()

	idle := make(chan struct{}, 1)
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateIdle:
			select {
			case idle <- struct{}{}:
			default:
			}
		case http.StateClosed:
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Transport: transport}
	response, err := client.Get(server.URL)
	if err != nil {
		server.Close()
		t.Fatalf("GET idle transport fixture: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	select {
	case <-idle:
	case <-time.After(2 * time.Second):
		transport.CloseIdleConnections()
		server.Close()
		t.Fatal("timed out waiting for idle connection")
	}

	return idleTransportFixture{
		transport: transport,
		closed:    closed,
		cleanup: func() {
			transport.CloseIdleConnections()
			server.Close()
		},
	}
}

func waitForIdleTransportClose(t *testing.T, closed <-chan struct{}) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for idle connection to close")
	}
}

func TestTransportCacheBoundedLRUClosesEvictedTransport(t *testing.T) {
	first := newIdleTransportFixture(t)
	defer first.cleanup()
	second := newIdleTransportFixture(t)
	defer second.cleanup()
	third := newIdleTransportFixture(t)
	defer third.cleanup()

	cache := NewTransportCache[string](2)
	if _, err := cache.Get("first", func() (*http.Transport, error) { return first.transport, nil }); err != nil {
		t.Fatalf("cache first transport: %v", err)
	}
	if _, err := cache.Get("second", func() (*http.Transport, error) { return second.transport, nil }); err != nil {
		t.Fatalf("cache second transport: %v", err)
	}
	if _, err := cache.Get("first", func() (*http.Transport, error) {
		t.Fatal("cache hit rebuilt first transport")
		return nil, nil
	}); err != nil {
		t.Fatalf("reuse first transport: %v", err)
	}
	if _, err := cache.Get("third", func() (*http.Transport, error) { return third.transport, nil }); err != nil {
		t.Fatalf("cache third transport: %v", err)
	}

	if cache.Len() != 2 {
		t.Fatalf("cache length = %d, want 2", cache.Len())
	}
	if !cache.Contains("first") || !cache.Contains("third") || cache.Contains("second") {
		t.Fatalf("cache keys after LRU eviction: first=%t second=%t third=%t", cache.Contains("first"), cache.Contains("second"), cache.Contains("third"))
	}
	waitForIdleTransportClose(t, second.closed)
	select {
	case <-first.closed:
		t.Fatal("most recently used transport was closed")
	default:
	}
}

func TestTransportCacheConcurrentSameKeyReuse(t *testing.T) {
	cache := NewTransportCache[string](4)
	want := &http.Transport{}
	var builds atomic.Int32

	const callers = 32
	start := make(chan struct{})
	results := make(chan *http.Transport, callers)
	errorsCh := make(chan error, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			<-start
			transport, err := cache.Get("shared", func() (*http.Transport, error) {
				builds.Add(1)
				time.Sleep(time.Millisecond)
				return want, nil
			})
			results <- transport
			errorsCh <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errorsCh)

	for err := range errorsCh {
		if err != nil {
			t.Fatalf("Get error = %v", err)
		}
	}
	for transport := range results {
		if transport != want {
			t.Fatalf("transport = %p, want %p", transport, want)
		}
	}
	if builds.Load() != 1 {
		t.Fatalf("build count = %d, want 1", builds.Load())
	}
}

func TestTransportCacheDoesNotCacheBuildFailures(t *testing.T) {
	cache := NewTransportCache[string](2)
	wantErr := errors.New("build failed")
	if transport, err := cache.Get("error", func() (*http.Transport, error) { return nil, wantErr }); transport != nil || !errors.Is(err, wantErr) {
		t.Fatalf("error build = (%v, %v), want (nil, %v)", transport, err, wantErr)
	}
	if cache.Contains("error") {
		t.Fatal("build error was cached")
	}
	if transport, err := cache.Get("nil", func() (*http.Transport, error) { return nil, nil }); transport != nil || err == nil {
		t.Fatalf("nil build = (%v, %v), want (nil, error)", transport, err)
	}
	if cache.Contains("nil") || cache.Len() != 0 {
		t.Fatalf("failed builds changed cache: len=%d", cache.Len())
	}
}

func TestTransportCacheCloseOperations(t *testing.T) {
	closeKey := newIdleTransportFixture(t)
	defer closeKey.cleanup()
	closeMatch := newIdleTransportFixture(t)
	defer closeMatch.cleanup()
	keep := newIdleTransportFixture(t)
	defer keep.cleanup()
	purge := newIdleTransportFixture(t)
	defer purge.cleanup()

	cache := NewTransportCache[string](8)
	fixtures := map[string]idleTransportFixture{
		"close-key":   closeKey,
		"close-match": closeMatch,
		"keep":        keep,
		"purge":       purge,
	}
	for key, fixture := range fixtures {
		fixture := fixture
		if _, err := cache.Get(key, func() (*http.Transport, error) { return fixture.transport, nil }); err != nil {
			t.Fatalf("cache %q: %v", key, err)
		}
	}

	if !cache.CloseKey("close-key") || cache.CloseKey("missing") {
		t.Fatal("CloseKey return values did not identify removed key")
	}
	waitForIdleTransportClose(t, closeKey.closed)

	if removed := cache.CloseMatching(func(key string) bool { return key == "close-match" }); removed != 1 {
		t.Fatalf("CloseMatching removed %d entries, want 1", removed)
	}
	waitForIdleTransportClose(t, closeMatch.closed)
	if !cache.Contains("keep") || !cache.Contains("purge") {
		t.Fatal("CloseMatching removed non-matching entries")
	}

	cache.Purge()
	if cache.Len() != 0 {
		t.Fatalf("cache length after Purge = %d, want 0", cache.Len())
	}
	waitForIdleTransportClose(t, keep.closed)
	waitForIdleTransportClose(t, purge.closed)
}
