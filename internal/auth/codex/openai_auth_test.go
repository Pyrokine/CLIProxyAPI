package codex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokensDetachedRequestHasTimeout(t *testing.T) {
	originalTimeout := codexRefreshTimeout
	codexRefreshTimeout = 20 * time.Millisecond
	defer func() { codexRefreshTimeout = originalTimeout }()
	codexRefreshGroup = singleflight.Group{}
	defer func() { codexRefreshGroup = singleflight.Group{} }()
	auth := &Auth{
		httpClient: &http.Client{
			Transport: roundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					<-req.Context().Done()
					return nil, req.Context().Err()
				},
			),
		},
	}
	started := time.Now()
	_, errRefresh := auth.refreshTokens(context.Background(), "timeout-refresh-token")
	if !errors.Is(errRefresh, context.DeadlineExceeded) {
		t.Fatalf("RefreshTokens() error = %v", errRefresh)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("RefreshTokens() elapsed = %s", elapsed)
	}
}

func TestRefreshTokensWithRetry_NonRetryableOnlyAttemptsOnce(t *testing.T) {
	var calls atomic.Int32
	auth := &Auth{
		httpClient: &http.Client{
			Transport: roundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
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
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 refresh attempt, got %d", got)
	}
}

func TestRefreshTokensSingleflightDeduplicatesConcurrentCalls(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	auth := &Auth{
		httpClient: &http.Client{
			Transport: roundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					if calls.Add(1) == 1 {
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
			_, err := auth.refreshTokens(context.Background(), "shared_codex_refresh_token")
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
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}
