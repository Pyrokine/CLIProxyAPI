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

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
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

type antigravityDeadlineBody struct {
	ctx context.Context
}

func (b antigravityDeadlineBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (antigravityDeadlineBody) Close() error { return nil }

func TestAntigravityCredentialAcquisitionHasDeadline(t *testing.T) {
	previous := antigravityCredentialAcquisitionTimeout
	antigravityCredentialAcquisitionTimeout = 40 * time.Millisecond
	t.Cleanup(func() { antigravityCredentialAcquisitionTimeout = previous })
	for _, operation := range []string{"oauth", "project"} {
		for _, blocked := range []string{"headers", "body"} {
			t.Run(operation+"/"+blocked, func(t *testing.T) {
				roundTripper := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					deadline, ok := req.Context().Deadline()
					if !ok || time.Until(deadline) > antigravityCredentialAcquisitionTimeout {
						t.Fatal("credential acquisition request has no bounded deadline")
					}
					if blocked == "headers" {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: antigravityDeadlineBody{req.Context()}, Request: req}, nil
				})
				ctx := util.WithRoundTripper(context.Background(), roundTripper)
				exec := &AntigravityExecutor{}
				auth := &cliproxyauth.Auth{ID: t.Name(), Metadata: map[string]any{"refresh_token": t.Name(), "project_id": "fixture-project"}}
				result := make(chan error, 1)
				go func() {
					var err error
					if operation == "oauth" {
						_, err = exec.refreshToken(ctx, auth)
					} else {
						_, err = exec.fetchAntigravityProjectID(ctx, auth, "fixture-token")
					}
					result <- err
				}()
				select {
				case err := <-result:
					if err != context.DeadlineExceeded {
						t.Fatalf("acquisition error = %v, want context.DeadlineExceeded", err)
					}
				case <-time.After(time.Second):
					t.Fatal("credential acquisition did not finish within its budget")
				}
			})
		}
	}
}

func TestAntigravitySharedRefreshSurvivesCallerCancellationWithinDeadline(t *testing.T) {
	previous := antigravityCredentialAcquisitionTimeout
	antigravityCredentialAcquisitionTimeout = 150 * time.Millisecond
	t.Cleanup(func() { antigravityCredentialAcquisitionTimeout = previous })
	started := make(chan struct{})
	var calls atomic.Int32
	roundTripper := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(util.WithRoundTripper(context.Background(), roundTripper))
	defer cancel()
	auth := &cliproxyauth.Auth{ID: t.Name(), Metadata: map[string]any{"refresh_token": t.Name(), "project_id": "fixture-project"}}
	exec := &AntigravityExecutor{}
	results := make(chan error, 2)
	go func() { _, err := exec.refreshToken(ctx, auth.Clone()); results <- err }()
	<-started
	cancel()
	go func() { _, err := exec.refreshToken(context.WithoutCancel(ctx), auth.Clone()); results <- err }()
	for range 2 {
		select {
		case err := <-results:
			if err != context.DeadlineExceeded {
				t.Fatalf("shared refresh error = %v, want internal deadline", err)
			}
		case <-time.After(time.Second):
			t.Fatal("shared refresh did not finish")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("shared refresh calls = %d, want 1", calls.Load())
	}
}

func TestAntigravityRefreshReturnsBeforeCreditsQuery(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)
	started := make(chan struct{})
	release := make(chan struct{})
	var creditsCalls atomic.Int32
	roundTripper := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"access_token":"fixture-access","refresh_token":"fixture-next","expires_in":3600}`
		if req.URL.Path == antigravityLoadCodeAssistPath {
			if creditsCalls.Add(1) == 1 {
				close(started)
			}
			deadline, ok := req.Context().Deadline()
			if !ok || time.Until(deadline) > antigravityCreditsHintRefreshTimeout {
				t.Error("credits query has no bounded deadline")
			}
			select {
			case <-release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			body = `{"paidTier":{"availableCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":"200","minimumCreditAmountForUsage":"50"}]}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	auth := &cliproxyauth.Auth{ID: t.Name(), Metadata: map[string]any{"refresh_token": t.Name(), "project_id": "fixture-project"}}
	cliproxyauth.SetAntigravityCreditsHint(auth.ID, cliproxyauth.AntigravityCreditsHint{Known: true, CreditAmount: 100})
	exec := NewAntigravityExecutor(&config.Config{QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true}})
	ctx := util.WithRoundTripper(context.Background(), roundTripper)
	result := make(chan error, 1)
	go func() {
		updated, err := exec.Refresh(ctx, auth)
		if err == nil && metaStringValue(updated.Metadata, "access_token") != "fixture-access" {
			err = errUnexpectedAccessToken
		}
		result <- err
	}()
	t.Cleanup(func() {
		close(release)
		if value, ok := antigravityCreditsHintRefreshByID.Load(auth.ID); ok {
			state := value.(*antigravityCreditsHintRefreshState)
			state.mu.Lock()
			//lint:ignore SA2001 等待异步额度查询释放互斥锁，避免影响后续测试
			state.mu.Unlock()
		}
	})
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Refresh failed: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("credits query blocked refreshed credentials")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("known credits were not queued for refresh")
	}
	exec.maybeRefreshAntigravityCreditsHint(ctx, auth, "fixture-access", true)
	if creditsCalls.Load() != 1 {
		t.Fatalf("credits queries = %d, want one in-flight query", creditsCalls.Load())
	}
}
