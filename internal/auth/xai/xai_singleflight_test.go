package xai

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

type xaiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f xaiRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokensSingleflightDeduplicatesConcurrentCalls(t *testing.T) {
	var calls int32
	entered := make(chan struct{})
	release := make(chan struct{})
	auth := &Auth{
		httpClient: &http.Client{
			Transport: xaiRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					if atomic.AddInt32(&calls, 1) == 1 {
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
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestRefreshTokensSingleflightDoesNotShareDiscoveryFailureWithExplicitEndpoint(t *testing.T) {
	discoveryEntered := make(chan struct{})
	releaseDiscovery := make(chan struct{})
	var tokenCalls int32
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
					atomic.AddInt32(&tokenCalls, 1)
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
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Fatalf("token calls = %d, want 1", got)
	}
}
