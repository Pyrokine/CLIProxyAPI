package executor

import (
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestQwenQuotaSignalsReachBothExecutionEntries(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     int
		body       string
		header     string
		wantStatus int
		wantDelay  time.Duration
		daily      bool
		readError  bool
	}{
		{
			name: "daily delta header", status: 403, body: `{"error":{"code":"insufficient_quota"}}`, header: "7",
			wantStatus: 429, wantDelay: 7 * time.Second,
		},
		{
			name: "daily date header", status: 429, body: `{"error":{"code":"quota_exceeded"}}`, header: "http-date",
			wantStatus: 429, wantDelay: 11 * time.Second,
		},
		{
			name: "daily without deadline", status: 403, body: `{"error":{"code":"insufficient_quota"}}`,
			wantStatus: 429, daily: true,
		},
		{
			name: "daily invalid header", status: 403, body: `{"error":{"code":"insufficient_quota"}}`,
			header: "invalid", wantStatus: 429, daily: true,
		},
		{
			name: "daily negative header", status: 403, body: `{"error":{"code":"insufficient_quota"}}`, header: "-1",
			wantStatus: 429, daily: true,
		},
		{
			name: "daily explicit zero", status: 403, body: `{"error":{"code":"insufficient_quota"}}`, header: "0",
			wantStatus: 429,
		},
		{
			name: "daily bounded header", status: 403, body: `{"error":{"code":"insufficient_quota"}}`,
			header: "9223372036854775807", wantStatus: 429,
			wantDelay: time.Duration(math.MaxInt64/int64(time.Second)) * time.Second,
		},
		{
			name: "rate without deadline", status: 429,
			body: `{"error":{"code":"rate_limit_exceeded","message":"quota exceeded"}}`, wantStatus: 429,
		},
		{
			name: "rate type without deadline", status: 429,
			body: `{"error":{"type":"rate_limit_exceeded","message":"quota exceeded"}}`, wantStatus: 429,
		},
		{
			name: "rate delta header", status: 429, body: `{"error":{"code":"rate_limit_exceeded"}}`, header: "9",
			wantStatus: 429, wantDelay: 9 * time.Second,
		},
		{name: "ordinary forbidden", status: 403, body: `{"error":{"code":"access_denied"}}`, wantStatus: 403},
		{
			name: "quota text outside quota status", status: 500, body: `{"error":{"code":"insufficient_quota"}}`,
			wantStatus: 500,
		},
		{
			name: "rate body read failure", status: 429, body: `{"error":{"code":"rate_limit_exceeded"}}`, header: "13",
			wantStatus: 429, wantDelay: 13 * time.Second, readError: true,
		},
	} {
		for _, entry := range []string{"ordinary", "stream", "manager ordinary", "manager stream"} {
			t.Run(
				test.name+"/"+entry, func(t *testing.T) {
					synctest.Test(
						t, func(t *testing.T) {
							calls := 0
							readCause := errors.New("synthetic response body read failure")
							transport := &auditClaudeContractTransport{
								respond: func(request *http.Request) (*http.Response, error) {
									calls++
									if request.URL.Host != "qwen-quota.synthetic.invalid" {
										t.Fatalf("unexpected request host: %s", request.URL.Host)
									}
									header := make(http.Header)
									value := test.header
									if value == "http-date" {
										value = time.Now().Add(test.wantDelay).UTC().Format(http.TimeFormat)
									}
									if value != "" {
										header.Set("Retry-After", value)
									}
									var body io.Reader = strings.NewReader(test.body)
									if test.readError {
										body = io.MultiReader(body, iotest.ErrReader(readCause))
									}
									return &http.Response{
										StatusCode: test.status, Header: header, Body: io.NopCloser(body),
										Request: request,
									}, nil
								},
							}
							ctx, records := xaiOutcomeContext(t)
							ctx = util.WithRoundTripper(ctx, transport)
							t.Cleanup(
								func() {
									if captured := records(); len(captured) != 1 || !captured[0].Failed ||
										captured[0].Fail.StatusCode != test.wantStatus {
										t.Errorf("unexpected usage failure: %+v", captured)
									}
								},
							)
							credential := &coreauth.Auth{
								ID: t.Name(), Provider: "qwen", Metadata: map[string]any{
									"access_token": "synthetic-token",
									"resource_url": "https://qwen-quota.synthetic.invalid/v1",
								},
							}
							t.Cleanup(
								func() {
									qwenRateLimiter.Lock()
									defer qwenRateLimiter.Unlock()
									delete(qwenRateLimiter.requests, credential.ID)
								},
							)
							cfg := &config.Config{}
							if newProxyAwareHTTPClient(ctx, cfg, credential, 0).Transport != transport {
								t.Fatal("Qwen quota transport is not memory-isolated")
							}
							executor := NewQwenExecutor(cfg)
							req := core.Request{
								Model: "qwen3-coder-plus",
								Payload: []byte(
									`{"model":"qwen3-coder-plus","messages":[{"role":"user","content":"synthetic"}]}`,
								),
							}
							opts := core.Options{
								SourceFormat: translator.FormatOpenAI, ResponseFormat: translator.FormatOpenAI,
							}
							var manager *coreauth.Manager
							if strings.HasPrefix(entry, "manager") {
								manager = coreauth.NewManager(nil, nil, nil)
								manager.SetConfig(cfg)
								manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
								manager.SetRetryConfig(0, 0, 1)
								manager.RegisterExecutor(executor)
								registered, err := manager.Register(ctx, credential)
								if err != nil {
									t.Fatal(err)
								}
								credential = registered
								reg := registry.GetGlobalRegistry()
								reg.RegisterClient(credential.ID, "qwen", []*registry.ModelInfo{{ID: req.Model}})
								t.Cleanup(func() { reg.UnregisterClient(credential.ID) })
								manager.RefreshSchedulerEntry(credential.ID)
							}
							var err error
							switch {
							case entry == "manager ordinary":
								_, err = manager.Execute(ctx, []string{"qwen"}, req, opts)
							case entry == "manager stream":
								_, err = manager.ExecuteStream(ctx, []string{"qwen"}, req, opts)
							case entry == "stream":
								_, err = executor.ExecuteStream(ctx, credential, req, opts)
							default:
								_, err = executor.Execute(ctx, credential, req, opts)
							}
							var status statusErr
							if !errors.As(err, &status) || status.StatusCode() != test.wantStatus || calls != 1 {
								t.Fatalf(
									"status=%d expected=%d calls=%d error=%v", status.StatusCode(), test.wantStatus,
									calls, err,
								)
							}
							delay := status.RetryAfter()
							if test.daily {
								if delay == nil || *delay != timeUntilNextDay() {
									t.Fatalf("daily quota delay=%v, expected=%s", delay, timeUntilNextDay())
								}
							} else if test.header == "" {
								if delay != nil {
									t.Fatalf("non-daily response acquired an inferred daily deadline: %s", *delay)
								}
							} else if delay == nil || *delay != test.wantDelay {
								t.Fatalf("header deadline=%v, expected=%s", delay, test.wantDelay)
							}
							if test.readError && !errors.Is(err, readCause) {
								t.Fatalf("body read cause was lost: %v", err)
							}
							var disposition core.ExecutionDispositionError
							wantDisposition := core.HTTPStatusDisposition(test.wantStatus, delay != nil)
							if !errors.As(err, &disposition) || disposition.ExecutionDisposition() != wantDisposition {
								t.Fatalf("response authority was lost: error=%v expected=%+v", err, wantDisposition)
							}
							if manager != nil && test.wantStatus == http.StatusTooManyRequests {
								current, ok := manager.GetByID(credential.ID)
								if !ok {
									t.Fatal("selected credential is missing")
								}
								wantCooldown := time.Second
								if delay != nil {
									wantCooldown = max(*delay, 10*time.Second)
								}
								wantDeadline := time.Now().Add(wantCooldown).Round(0)
								state := current.ModelStates[req.Model]
								if state == nil || !state.Quota.Exceeded || state.NextRetryAfter != wantDeadline ||
									state.Quota.NextRecoverAt != wantDeadline {
									t.Fatalf("Manager quota=%+v expected deadline=%v", state, wantDeadline)
								}
							}
						},
					)
				},
			)
		}
	}
}
