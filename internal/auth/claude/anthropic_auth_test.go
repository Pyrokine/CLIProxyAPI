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

	var calls atomic.Int32
	auth := &Auth{
		httpClient: &http.Client{
			Transport: roundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
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
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 refresh attempt after 429, got %d", got)
	}

	_, err = auth.RefreshTokensWithRetry(context.Background(), "dummy_refresh_token", 3)
	if err == nil {
		t.Fatalf("expected immediate blocked refresh error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected blocked retry to avoid a second refresh call, got %d attempts", got)
	}
	if blockedUntil := claudeRefreshBlockedUntil("dummy_refresh_token"); !blockedUntil.After(time.Now()) {
		t.Fatalf("expected blocked-until timestamp to be set, got %v", blockedUntil)
	}
}

func TestRefreshTokensWithRetryDoesNotReplayAmbiguousFailure(t *testing.T) {
	for _, tt := range []struct {
		name      string
		transport roundTripFunc
	}{
		{
			name: "transport",
			transport: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("connection lost after request")
			},
		},
		{
			name: "decoding",
			transport: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"access_token":`)),
					Request:    req,
				}, nil
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetClaudeRefreshState()
			defer resetClaudeRefreshState()
			var calls int
			auth := &Auth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return tt.transport(req)
			})}}
			_, err := auth.RefreshTokensWithRetry(context.Background(), "single-use-token", 3)
			if err == nil || !strings.Contains(err.Error(), "after 1 attempts") || calls != 1 {
				t.Fatalf("RefreshTokensWithRetry() = %v, calls = %d; want one attempt", err, calls)
			}
		})
	}
	if !isClaudeRefreshRetryable(&refreshHTTPError{status: http.StatusServiceUnavailable, retryable: true}) {
		t.Fatal("explicit retryable HTTP response must remain retryable")
	}
}

func TestRefreshTokensDeduplicatesConcurrentRefresh(t *testing.T) {
	resetClaudeRefreshState()
	defer resetClaudeRefreshState()

	var tokenCalls atomic.Int32
	var profileCalls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	auth := &Auth{
		httpClient: &http.Client{
			Transport: roundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					switch req.URL.String() {
					case refreshTokenURL:
						tokenCalls.Add(1)
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
							"scope":"user:profile user:inference"
						}`,
								),
							),
							Header:  make(http.Header),
							Request: req,
						}, nil
					case profileURL:
						profileCalls.Add(1)
						return &http.Response{
							StatusCode: http.StatusOK,
							Body: io.NopCloser(
								strings.NewReader(
									`{
							"account":{"uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","email":"shared@example.com"},
							"organization":{"uuid":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","name":"Shared Org"}
						}`,
								),
							),
							Header:  make(http.Header),
							Request: req,
						}, nil
					default:
						t.Fatalf("unexpected OAuth request URL %s", req.URL)
						return nil, nil
					}
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
	if got := tokenCalls.Load(); got != 1 {
		close(release)
		t.Fatalf("expected concurrent refresh to share a single upstream call, got %d", got)
	}
	close(release)

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("expected refresh to succeed, got %v", err)
		}
		td := <-results
		if td == nil || td.AccessToken != "new-access" {
			t.Fatalf("expected refreshed access token, got %#v", td)
		}
		if td.AccountUUID != "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
			t.Fatalf("account UUID = %q, want OAuth profile account", td.AccountUUID)
		}
		if td.OrganizationUUID != "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" || td.OrganizationName != "Shared Org" {
			t.Fatalf("organization = %q/%q, want OAuth profile organization", td.OrganizationUUID, td.OrganizationName)
		}
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream refresh call, got %d", got)
	}
	if got := profileCalls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 OAuth profile call, got %d", got)
	}
}
