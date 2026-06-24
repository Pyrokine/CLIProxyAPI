package codex

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokensWithRetry_NonRetryableOnlyAttemptsOnce(t *testing.T) {
	var calls int32
	auth := &Auth{
		httpClient: &http.Client{
			Transport: roundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					atomic.AddInt32(&calls, 1)
					return &http.Response{
						StatusCode: http.StatusBadRequest,
						Body: io.NopCloser(
							strings.NewReader(`{"error":"invalid_grant","code":"refresh_token_reused"}`),
						),
						Header:  make(http.Header),
						Request: req,
					}, nil
				},
			),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected error for non-retryable refresh failure")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "refresh_token_reused") {
		t.Fatalf("expected refresh_token_reused in error, got: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 refresh attempt, got %d", got)
	}
}

func TestRefreshTokensSingleflightDeduplicatesConcurrentCalls(t *testing.T) {
	var calls int32
	entered := make(chan struct{})
	release := make(chan struct{})
	auth := &Auth{
		httpClient: &http.Client{
			Transport: roundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					if atomic.AddInt32(&calls, 1) == 1 {
						close(entered)
					}
					<-release
					return &http.Response{
						StatusCode: http.StatusOK,
						Body: io.NopCloser(
							strings.NewReader(`{"access_token":"access","refresh_token":"refresh-next","expires_in":3600}`),
						),
						Header:  make(http.Header),
						Request: req,
					}, nil
				},
			),
		},
	}

	const workers = 8
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			<-start
			_, err := auth.RefreshTokens(context.Background(), "shared_codex_refresh_token")
			errs <- err
		}()
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("refresh request did not start")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("refresh failed: %v", err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}
