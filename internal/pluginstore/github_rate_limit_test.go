package pluginstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func githubTestHeaders(values map[string]string) http.Header {
	headers := make(http.Header)
	for name, value := range values {
		headers.Set(name, value)
	}
	return headers
}

func TestGitHubRateLimitResponses(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	reset := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	tests := []struct {
		name    string
		status  int
		headers map[string]string
		body    string
		wait    time.Duration
	}{
		{
			name: "primary forbidden", status: http.StatusForbidden,
			headers: map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset},
			wait:    time.Hour,
		},
		{
			name: "retry seconds", status: http.StatusTooManyRequests,
			headers: map[string]string{"Retry-After": "120"}, wait: 2 * time.Minute,
		},
		{
			name: "retry date", status: http.StatusTooManyRequests,
			headers: map[string]string{"Retry-After": now.Add(3 * time.Minute).Format(http.TimeFormat)},
			wait:    3 * time.Minute,
		},
		{
			name: "reset wins", status: http.StatusForbidden,
			headers: map[string]string{
				"Retry-After": "120", "X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset,
			},
			wait: time.Hour,
		},
		{
			name: "secondary message", status: http.StatusForbidden,
			body: `{"message":"You have exceeded a secondary rate limit."}`, wait: time.Minute,
		},
		{name: "headerless 429", status: http.StatusTooManyRequests, wait: time.Minute},
		{
			name: "permission denied", status: http.StatusForbidden,
			body: `{"message":"Resource not accessible by integration"}`,
		},
		{name: "not found", status: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			limiter := &GitHubRateLimiter{nowFunc: func() time.Time { return now }}
			errLimit := limiter.observe("quota", tt.status, githubTestHeaders(tt.headers), []byte(tt.body))
			if tt.wait == 0 {
				if errLimit != nil || limiter.check("quota") != nil {
					t.Fatalf("non-rate-limit response started cooldown: %v", errLimit)
				}
				return
			}
			var rateLimit *RateLimitError
			if !errors.As(errLimit, &rateLimit) || rateLimit.StatusCode != tt.status ||
				!rateLimit.RetryAt.Equal(now.Add(tt.wait)) {
				t.Fatalf("error=%v, want status=%d retry=%s", errLimit, tt.status, now.Add(tt.wait))
			}
			if errCheck := limiter.check("quota"); !errors.As(errCheck, &rateLimit) {
				t.Fatalf("check=%v, want cooldown", errCheck)
			}
		})
	}
}

func TestGitHubRateLimitKeyCanonicalHost(t *testing.T) {
	t.Parallel()
	want := githubRateLimitKey("https://api.github.com/repos/owner/repo", "", nil, false)
	for _, requestURL := range []string{
		"https://api.github.com:443/repos/other/repo",
		"https://API.GITHUB.COM/releases",
	} {
		if got := githubRateLimitKey(requestURL, "", nil, false); got != want || got == "" {
			t.Fatalf("key for %s = %q, want %q", requestURL, got, want)
		}
	}
	for _, requestURL := range []string{
		"https://api.github.com:444/repos/owner/repo",
		"http://api.github.com/",
		"https://api.github.com.example/",
		"https://raw.githubusercontent.com/",
	} {
		if got := githubRateLimitKey(requestURL, "", nil, false); got != "" {
			t.Fatalf("unrelated URL %s has GitHub quota key %q", requestURL, got)
		}
	}
}

func TestGitHubRateLimitBackoffRecoveryAndLateSuccess(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := &GitHubRateLimiter{nowFunc: func() time.Time { return now }}
	for _, delay := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		errLimit := limiter.observe("quota", http.StatusTooManyRequests, nil, nil)
		var rateLimit *RateLimitError
		if !errors.As(errLimit, &rateLimit) || !rateLimit.RetryAt.Equal(now.Add(delay)) {
			t.Fatalf("backoff=%v, want %s", errLimit, delay)
		}
		now = now.Add(delay)
	}
	_ = limiter.observe("quota", http.StatusOK, nil, nil)
	reset := now.Add(time.Hour)
	headers := githubTestHeaders(map[string]string{
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset":     strconv.FormatInt(reset.Unix(), 10),
	})
	if errObserve := limiter.observe("quota", http.StatusOK, headers, nil); errObserve != nil {
		t.Fatalf("final successful request failed: %v", errObserve)
	}
	_ = limiter.observe("quota", http.StatusOK, nil, nil)
	_ = limiter.observe(
		"quota",
		http.StatusTooManyRequests,
		githubTestHeaders(map[string]string{"Retry-After": "60"}),
		nil,
	)
	var rateLimit *RateLimitError
	if errCheck := limiter.check("quota"); !errors.As(errCheck, &rateLimit) ||
		!rateLimit.RetryAt.Equal(reset) {
		t.Fatalf("late response shortened cooldown: %v", errCheck)
	}
}

func TestGitHubCooldownSharedAcrossClientsRepositoriesAndAssets(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	limiter := &GitHubRateLimiter{nowFunc: func() time.Time { return now }}
	calls := 0
	doer := pluginIdentityDoerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     githubTestHeaders(map[string]string{"Retry-After": "60"}),
				Body:       io.NopCloser(strings.NewReader("limited")),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"tag_name":"v1.0.0"}`)),
		}, nil
	})
	first := Client{HTTPClient: doer, RateLimiter: limiter}
	second := Client{HTTPClient: doer, RateLimiter: limiter}
	ctx := context.Background()
	_, errFirst := first.FetchLatestRelease(ctx, Plugin{Repository: "https://github.com/owner/first"})
	_, errSecond := second.FetchReleaseByTag(ctx, Plugin{Repository: "https://github.com/other/second"}, "v1.0.0")
	_, errAsset := second.downloadAsset(
		ctx,
		ReleaseAsset{APIURL: "https://api.github.com:443/repos/other/second/releases/assets/1"},
	)
	for _, errRequest := range []error{errFirst, errSecond, errAsset} {
		var rateLimit *RateLimitError
		if !errors.As(errRequest, &rateLimit) {
			t.Fatalf("request error=%v, want rate limit", errRequest)
		}
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
	if _, errRegistry := second.get(
		ctx,
		DefaultRegistryURL,
		"application/json",
		RequestKindRegistry,
		maxPluginStoreRegistryBytes,
	); errRegistry != nil {
		t.Fatalf("API cooldown blocked registry host: %v", errRegistry)
	}
	now = now.Add(time.Minute)
	if _, errRecovered := second.FetchLatestRelease(
		ctx,
		Plugin{Repository: "https://github.com/other/second"},
	); errRecovered != nil || calls != 3 {
		t.Fatalf("recovery error=%v calls=%d", errRecovered, calls)
	}
}

func TestGitHubCooldownSeparatesCredentialsAndNetworkScope(t *testing.T) {
	limiter := &GitHubRateLimiter{}
	calls := 0
	doer := pluginIdentityDoerFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     githubTestHeaders(map[string]string{"Retry-After": "3600"}),
			Body:       io.NopCloser(strings.NewReader("limited")),
		}, nil
	})
	plugin := Plugin{Repository: "https://github.com/owner/repo"}
	unauthenticated := Client{HTTPClient: doer, RateLimiter: limiter}
	_, _ = unauthenticated.FetchLatestRelease(context.Background(), plugin)
	authenticated := Client{
		HTTPClient:  doer,
		RateLimiter: limiter,
		Auth: []AuthConfig{{
			Match: "https://api.github.com/", Type: AuthTypeGitHubToken,
			TokenEnv: "PLUGIN_STORE_RATE_LIMIT_TOKEN",
		}},
	}
	for _, token := range []string{"token-a", "token-b", "token-a"} {
		t.Setenv("PLUGIN_STORE_RATE_LIMIT_TOKEN", token)
		_, _ = authenticated.FetchLatestRelease(context.Background(), plugin)
	}
	_, _ = unauthenticated.FetchLatestRelease(context.Background(), plugin)
	if calls != 3 {
		t.Fatalf("credential identities made %d calls, want 3", calls)
	}
	authenticated.NetworkScope = "other-egress"
	t.Setenv("PLUGIN_STORE_RATE_LIMIT_TOKEN", "token-a")
	_, _ = authenticated.FetchLatestRelease(context.Background(), plugin)
	if calls != 4 {
		t.Fatalf("network scopes made %d calls, want 4", calls)
	}
}

type githubHeaderOnlyBody struct {
	reads  int
	closed bool
}

func (body *githubHeaderOnlyBody) Read([]byte) (int, error) {
	body.reads++
	return 0, errors.New("body must not be read")
}

func (body *githubHeaderOnlyBody) Close() error {
	body.closed = true
	return nil
}

func TestGitHubHeaderCooldownClosesBodyWithoutReading(t *testing.T) {
	t.Parallel()
	body := &githubHeaderOnlyBody{}
	client := Client{
		RateLimiter: &GitHubRateLimiter{},
		HTTPClient: pluginIdentityDoerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     githubTestHeaders(map[string]string{"Retry-After": "60"}),
				Body:       body,
			}, nil
		}),
	}
	_, errRelease := client.FetchLatestRelease(
		context.Background(),
		Plugin{Repository: "https://github.com/owner/repo"},
	)
	var rateLimit *RateLimitError
	if !errors.As(errRelease, &rateLimit) || body.reads != 0 || !body.closed {
		t.Fatalf("error=%v reads=%d closed=%v", errRelease, body.reads, body.closed)
	}
}

func TestGitHubDefaultCooldownBlocksNewClients(t *testing.T) {
	var calls atomic.Int32
	client := Client{
		NetworkScope: t.Name(),
		HTTPClient: pluginIdentityDoerFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     githubTestHeaders(map[string]string{"Retry-After": "3600"}),
				Body:       io.NopCloser(strings.NewReader("limited")),
			}, nil
		}),
	}
	key := githubRateLimitKey("https://api.github.com/", client.NetworkScope, nil, false)
	t.Cleanup(func() {
		defaultGitHubRateLimiter.mu.Lock()
		delete(defaultGitHubRateLimiter.entries, key)
		defaultGitHubRateLimiter.mu.Unlock()
	})
	_, _ = client.FetchLatestRelease(context.Background(), Plugin{Repository: "https://github.com/owner/first"})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copyClient := Client{NetworkScope: client.NetworkScope, HTTPClient: client.HTTPClient}
			_, errRelease := copyClient.FetchLatestRelease(
				context.Background(),
				Plugin{Repository: "https://github.com/other/second"},
			)
			var rateLimit *RateLimitError
			if !errors.As(errRelease, &rateLimit) {
				t.Errorf("request error = %v", errRelease)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("calls=%d, want 1", calls.Load())
	}
}

func TestRateLimitRetryAfterRoundsUp(t *testing.T) {
	t.Parallel()
	now := time.Unix(100, 0)
	for _, tt := range []struct {
		delay time.Duration
		want  int64
	}{
		{delay: -time.Second, want: 0},
		{delay: 0, want: 0},
		{delay: time.Nanosecond, want: 1},
		{delay: time.Second, want: 1},
		{delay: time.Second + time.Nanosecond, want: 2},
	} {
		errLimit := &RateLimitError{RetryAt: now.Add(tt.delay)}
		if got := errLimit.RetryAfterSeconds(now); got != tt.want {
			t.Fatalf("RetryAfterSeconds(%s)=%d, want %d", tt.delay, got, tt.want)
		}
	}
}
