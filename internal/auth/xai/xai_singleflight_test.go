package xai

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type xaiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f xaiRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokensSingleflightDeduplicatesConcurrentCalls(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	auth := &Auth{
		httpClient: &http.Client{
			Transport: xaiRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					if calls.Add(1) == 1 {
						close(entered)
					}
					<-release
					return &http.Response{
						StatusCode: http.StatusOK,
						Body: io.NopCloser(
							strings.NewReader(`{"access_token":"access","refresh_token":"refresh-next","id_token":"","token_type":"Bearer","expires_in":3600}`),
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
			_, err := auth.RefreshTokens(context.Background(), " shared_xai_refresh_token ", "https://x.ai/oauth/token")
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

func TestRefreshTokensRejectsUntrustedExplicitEndpoint(t *testing.T) {
	var calls atomic.Int32
	auth := &Auth{
		httpClient: &http.Client{
			Transport: xaiRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
					return nil, fmt.Errorf("unexpected request to %s", req.URL)
				},
			),
		},
	}

	_, err := auth.RefreshTokens(
		context.Background(),
		"refresh-token",
		"http://127.0.0.1/oauth/token",
	)
	if err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("RefreshTokens() error = %v, want trusted endpoint validation error", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("refresh requests = %d, want 0", got)
	}
}

func TestRefreshTokensSingleflightSeparatesTokenEndpoints(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	auth := &Auth{
		httpClient: &http.Client{
			Transport: xaiRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					entered <- req.URL.Host
					<-release
					return &http.Response{
						StatusCode: http.StatusOK,
						Body: io.NopCloser(
							strings.NewReader(`{"access_token":"` + req.URL.Host + `","expires_in":3600}`),
						),
						Header:  make(http.Header),
						Request: req,
					}, nil
				},
			),
		},
	}

	endpoints := []string{
		"https://x.ai/oauth/token",
		"https://auth.x.ai/oauth/token",
	}
	results := make(chan string, len(endpoints))
	errs := make(chan error, len(endpoints))
	for _, endpoint := range endpoints {
		go func() {
			tokenData, err := auth.RefreshTokens(context.Background(), "shared-refresh-token", endpoint)
			if err != nil {
				errs <- err
				return
			}
			results <- tokenData.AccessToken
		}()
	}

	seenHosts := make(map[string]struct{}, len(endpoints))
	for range endpoints {
		select {
		case host := <-entered:
			seenHosts[host] = struct{}{}
		case <-time.After(time.Second):
			close(release)
			t.Fatal("refreshes for distinct token endpoints were coalesced")
		}
	}
	close(release)
	if len(seenHosts) != len(endpoints) {
		t.Fatalf("refresh hosts = %#v", seenHosts)
	}
	for range endpoints {
		select {
		case err := <-errs:
			t.Fatalf("RefreshTokens() error = %v", err)
		case accessToken := <-results:
			if _, ok := seenHosts[accessToken]; !ok {
				t.Fatalf("access token = %q, want one of %#v", accessToken, seenHosts)
			}
		case <-time.After(time.Second):
			t.Fatal("refresh result timed out")
		}
	}
}

func TestRefreshTokensSingleflightCanonicalizesEquivalentEndpoints(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	auth := &Auth{
		httpClient: &http.Client{
			Transport: xaiRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					if calls.Add(1) == 1 {
						close(entered)
					}
					<-release
					return &http.Response{
						StatusCode: http.StatusOK,
						Body: io.NopCloser(
							strings.NewReader(`{"access_token":"access","expires_in":3600}`),
						),
						Header:  make(http.Header),
						Request: req,
					}, nil
				},
			),
		},
	}

	endpoints := []string{"https://x.ai/oauth/token", "HTTPS://X.AI:443/oauth/token#ignored"}
	errs := make(chan error, len(endpoints))
	for _, endpoint := range endpoints {
		go func() {
			_, err := auth.RefreshTokens(context.Background(), "shared-token", endpoint)
			errs <- err
		}()
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("refresh request did not start")
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range endpoints {
		if err := <-errs; err != nil {
			t.Fatalf("RefreshTokens() error = %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestRefreshTokensSingleflightIsScopedToAuthClient(t *testing.T) {
	entered := make(chan string, 2)
	release := make(chan struct{})
	newAuth := func(label string) *Auth {
		return &Auth{
			httpClient: &http.Client{
				Transport: xaiRoundTripFunc(
					func(req *http.Request) (*http.Response, error) {
						entered <- label
						<-release
						return &http.Response{
							StatusCode: http.StatusOK,
							Body: io.NopCloser(
								strings.NewReader(`{"access_token":"` + label + `","expires_in":3600}`),
							),
							Header:  make(http.Header),
							Request: req,
						}, nil
					},
				),
			},
		}
	}
	errs := make(chan error, 2)
	for _, auth := range []*Auth{newAuth("first"), newAuth("second")} {
		go func() {
			_, err := auth.RefreshTokens(context.Background(), "shared-token", "https://x.ai/oauth/token")
			errs <- err
		}()
	}
	seen := make(map[string]struct{}, 2)
	for range 2 {
		select {
		case label := <-entered:
			seen[label] = struct{}{}
		case <-time.After(time.Second):
			close(release)
			t.Fatal("refreshes on separate clients were coalesced")
		}
	}
	close(release)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("RefreshTokens() error = %v", err)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("refresh clients = %#v", seen)
	}
}

func TestRefreshTokensCompletesRotationAfterRequestCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	auth := &Auth{
		httpClient: &http.Client{
			Transport: xaiRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					close(entered)
					<-release
					return &http.Response{
						StatusCode: http.StatusOK,
						Body: io.NopCloser(
							strings.NewReader(`{"access_token":"access","expires_in":3600}`),
						),
						Header:  make(http.Header),
						Request: req,
					}, nil
				},
			),
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := auth.RefreshTokens(ctx, "refresh-token", "https://x.ai/oauth/token")
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		close(release)
		t.Fatalf("RefreshTokens() returned before rotation completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RefreshTokens() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("detached refresh did not finish")
	}
}

func TestRefreshTokensRejectsRedirectOutsideXAI(t *testing.T) {
	var calls atomic.Int32
	auth := NewXAIAuth(nil)
	auth.httpClient.Transport = xaiRoundTripFunc(
		func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			response := &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
				Request:    req,
			}
			response.Header.Set("Location", "https://example.com/oauth/token")
			return response, nil
		},
	)
	_, err := auth.RefreshTokens(context.Background(), "refresh-token", "https://x.ai/oauth/token")
	if err == nil || !strings.Contains(err.Error(), "not on x.ai") {
		t.Fatalf("RefreshTokens() error = %v, want redirect validation error", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("transport calls = %d, want 1", got)
	}
}

func TestRefreshTokensSingleflightDoesNotShareDiscoveryFailureWithExplicitEndpoint(t *testing.T) {
	discoveryEntered := make(chan struct{})
	releaseDiscovery := make(chan struct{})
	var tokenCalls atomic.Int32
	auth := &Auth{
		httpClient: &http.Client{
			Transport: xaiRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodGet {
						close(discoveryEntered)
						<-releaseDiscovery
						return &http.Response{
							StatusCode: http.StatusInternalServerError,
							Body:       io.NopCloser(strings.NewReader(`discovery failed`)),
							Header:     make(http.Header),
							Request:    req,
						}, nil
					}
					tokenCalls.Add(1)
					return &http.Response{
						StatusCode: http.StatusOK,
						Body: io.NopCloser(
							strings.NewReader(`{"access_token":"access","refresh_token":"refresh-next","id_token":"","token_type":"Bearer","expires_in":3600}`),
						),
						Header:  make(http.Header),
						Request: req,
					}, nil
				},
			),
		},
	}

	missingEndpointErr := make(chan error, 1)
	go func() {
		_, err := auth.RefreshTokens(context.Background(), " shared_xai_refresh_token_endpoint ", "")
		missingEndpointErr <- err
	}()
	select {
	case <-discoveryEntered:
	case <-time.After(time.Second):
		close(releaseDiscovery)
		t.Fatal("discovery request did not start")
	}

	explicitAccess := make(chan string, 1)
	explicitErr := make(chan error, 1)
	go func() {
		tokenData, err := auth.RefreshTokens(
			context.Background(), " shared_xai_refresh_token_endpoint ", "https://x.ai/oauth/token",
		)
		if err != nil {
			explicitErr <- err
			return
		}
		if tokenData == nil {
			explicitAccess <- ""
			return
		}
		explicitAccess <- tokenData.AccessToken
	}()

	select {
	case err := <-explicitErr:
		close(releaseDiscovery)
		t.Fatalf("explicit endpoint refresh failed: %v", err)
	case accessToken := <-explicitAccess:
		if accessToken != "access" {
			close(releaseDiscovery)
			t.Fatalf("access token = %q, want access", accessToken)
		}
	case <-time.After(time.Second):
		close(releaseDiscovery)
		t.Fatal("explicit endpoint refresh waited for discovery failure")
	}

	close(releaseDiscovery)
	if err := <-missingEndpointErr; err == nil {
		t.Fatal("missing endpoint refresh succeeded, want discovery error")
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("token calls = %d, want 1", got)
	}
}
