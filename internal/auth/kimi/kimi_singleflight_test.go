package kimi

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

type kimiRoundTripFunc func(*http.Request) (*http.Response, error)

func (f kimiRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRefreshTokenSingleflightDeduplicatesConcurrentCalls(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	client := &DeviceFlowClient{
		httpClient: &http.Client{
			Transport: kimiRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					if calls.Add(1) == 1 {
						close(entered)
					}
					<-release
					return &http.Response{
						StatusCode: http.StatusOK,
						Body: io.NopCloser(
							strings.NewReader(`{"access_token":"access","refresh_token":"refresh-next","token_type":"Bearer","expires_in":3600}`),
						),
						Header:  make(http.Header),
						Request: req,
					}, nil
				},
			),
		},
		deviceID: "device-id",
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
			_, err := client.RefreshToken(context.Background(), " shared_kimi_refresh_token ")
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
