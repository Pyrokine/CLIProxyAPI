package claude

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
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokensDetachedRequestHasTimeout(t *testing.T) {
	originalTimeout := claudeRefreshTimeout
	claudeRefreshTimeout = 20 * time.Millisecond
	defer func() { claudeRefreshTimeout = originalTimeout }()
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()
	auth := &Auth{httpClient: &http.Client{Transport: roundTripFunc(
		func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		},
	)}}
	started := time.Now()
	_, errRefresh := auth.RefreshTokens(context.Background(), "timeout-refresh-token")
	if !errors.Is(errRefresh, context.DeadlineExceeded) {
		t.Fatalf("RefreshTokens() error = %v", errRefresh)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("RefreshTokens() elapsed = %s", elapsed)
	}
}

func TestRefreshTokensWithRetry429BlocksImmediateReplay(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	auth := &Auth{
		httpClient: &http.Client{
			Transport: roundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					atomic.AddInt32(&calls, 1)
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Body:       io.NopCloser(strings.NewReader(`{"error":"rate_limited"}`)),
						Header:     http.Header{"Retry-After": []string{"60"}},
						Request:    req,
					}, nil
				},
			),
		},
	}

	_, err := auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected 429 refresh error")
	}
	if !strings.Contains(err.Error(), "status 429") {
		t.Fatalf("expected status 429 in error, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected 1 refresh attempt after 429, got %d", got)
	}

	_, err = auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected immediate blocked refresh error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected blocked retry to avoid a second refresh call, got %d attempts", got)
	}
	if blockedUntil := claudeRefreshBlockedUntil("dummy_refresh_token"); !blockedUntil.After(time.Now()) {
		t.Fatalf("expected blocked-until timestamp to be set, got %v", blockedUntil)
	}
}

func TestRefreshTokensDeduplicatesConcurrentRefresh(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var calls int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	auth := &Auth{
		httpClient: &http.Client{
			Transport: roundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					atomic.AddInt32(&calls, 1)
					once.Do(func() { close(started) })
					<-release
					return &http.Response{
						StatusCode: http.StatusOK,
						Body: io.NopCloser(
							strings.NewReader(
								`{
						"access_token":"new-access",
						"refresh_token":"new-refresh",
						"token_type":"Bearer",
						"expires_in":3600,
						"account":{"email_address":"shared@example.com"}
					}`,
							),
						),
						Header:  make(http.Header),
						Request: req,
					}, nil
				},
			),
		},
	}

	results := make(chan *TokenData, 2)
	errs := make(chan error, 2)
	runRefresh := func() {
		td, err := auth.RefreshTokens(context.Background(), "shared-refresh-token")
		results <- td
		errs <- err
	}

	go runRefresh()
	go runRefresh()

	select {
	case <-started:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("refresh request did not start")
	}
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		close(release)
		t.Fatalf("expected concurrent refresh to share a single upstream call, got %d", got)
	}
	close(release)

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("expected refresh to succeed, got %v", err)
		}
		td := <-results
		if td == nil || td.AccessToken != "new-access" {
			t.Fatalf("expected refreshed access token, got %#v", td)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected exactly 1 upstream refresh call, got %d", got)
	}
}
