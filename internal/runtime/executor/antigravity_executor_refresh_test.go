package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type antigravityRoundTripFunc func(*http.Request) (*http.Response, error)

func (f antigravityRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestAntigravityRefreshTokenSingleflightDeduplicatesConcurrentCalls(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	roundTripper := antigravityRoundTripFunc(
		func(req *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-release
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(
					strings.NewReader(`{"access_token":"access","refresh_token":"refresh-next","expires_in":3600,"token_type":"Bearer"}`),
				),
				Header:  make(http.Header),
				Request: req,
			}, nil
		},
	)
	ctx := util.WithRoundTripper(context.Background(), roundTripper)
	executor := &AntigravityExecutor{}
	auth := &cliproxyauth.Auth{
		ID: "antigravity-auth",
		Metadata: map[string]any{
			"refresh_token": "shared_antigravity_refresh_token",
			"project_id":    "project-id",
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
			updated, err := executor.refreshToken(ctx, auth.Clone())
			if err == nil && metaStringValue(updated.Metadata, "access_token") != "access" {
				err = errUnexpectedAccessToken
			}
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

func TestFetchAntigravityProjectIDPreservesExactCancellation(t *testing.T) {
	started := make(chan struct{})
	roundTripper := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(util.WithRoundTripper(context.Background(), roundTripper))
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := (&AntigravityExecutor{}).fetchAntigravityProjectID(
			ctx,
			&cliproxyauth.Auth{Metadata: map[string]any{"access_token": "token"}},
			"token",
		)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("project ID request did not start")
	}
	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("fetchAntigravityProjectID error = %#v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled project ID request did not return")
	}
}

var errUnexpectedAccessToken = statusErr{code: http.StatusInternalServerError, msg: "unexpected access token"}
