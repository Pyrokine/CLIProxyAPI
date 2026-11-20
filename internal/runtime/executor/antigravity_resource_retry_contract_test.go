package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestAntigravityBoundedResourceRetryAtGenerationEntries(t *testing.T) {
	cache.StoreSignature(t.Name(), t.Name(), strings.Repeat("s", 64))
	t.Cleanup(func() { cache.ClearSignatureCache(t.Name()) })
	resource := func(delay, extra string) string {
		details := extra
		if delay != "" {
			details = fmt.Sprintf(`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":%q}`, delay)
			if extra != "" {
				details += "," + extra
			}
		}
		return (`{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"` +
			`RESOURCE_EXHAUSTED","details":[`) + details + `]}}`
	}
	for _, fixture := range []struct {
		name, body   string
		status       int
		failures     int
		wait         time.Duration
		cancel       bool
		disableRetry bool
	}{
		{name: "100ms", body: resource("0.1s", ""), wait: 100 * time.Millisecond},
		{name: "500ms", body: resource("0.5s", ""), wait: 500 * time.Millisecond},
		{
			name: "original structured rate limit",
			body: `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.goog` +
				`leapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"},{"@ty` +
				`pe":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.5s"}]}}`,
			wait: 500 * time.Millisecond,
		},
		{
			name: "original UI rate limit",
			body: `{"error":{"code":429,"message":"You have exhausted your capacity on this model. Your ` +
				`quota will reset after 0s.","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.` +
				`googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","domain":"cloudcode-pa.` +
				`googleapis.com","metadata":{"model":"claude-opus-4-6-thinking","quotaResetDelay":"479.` +
				`417207ms","quotaResetTimeStamp":"2026-04-20T09:19:49Z","uiMessage":"true"}},{"@type":"type.` +
				`googleapis.com/google.rpc.RetryInfo","retryDelay":"0.479417207s"}]}}`,
			wait: 479417207 * time.Nanosecond,
		},
		{name: "original quota message", body: `{"error":{"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED"}}`},
		{name: "original unstructured 429", body: `{"error":{"message":"too many requests"}}`},
		{
			name: "original structured quota",
			body: `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.goog` +
				`leapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED"}]}}`,
		},
		{name: "positive floor", body: resource("0.001s", ""), wait: 100 * time.Millisecond},
		{name: "explicit zero", body: resource("0s", ""), wait: 100 * time.Millisecond},
		{name: "bounded ramp", body: resource("0.001s", ""), failures: 6, wait: 2 * time.Second},
		{name: "too long", body: resource("0.501s", "")},
		{name: "negative", body: resource("-0.1s", "")},
		{name: "malformed", body: resource("invalid", "")},
		{name: "original missing hint", body: resource("", "")},
		{
			name: "credits reason", body: resource(
				"0.1s", `{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":`+
					`"INSUFFICIENT_G1_CREDITS_BALANCE"}`,
			),
		},
		{
			name: "credits message", body: strings.Replace(
				resource("0.1s", ""), "Resource has been exhausted (e.g. check quota).",
				"Resource has been exhausted: insufficient GOOGLE_ONE_AI credits", 1,
			),
		},
		{name: "unknown 503", body: resource("0.1s", ""), status: http.StatusServiceUnavailable},
		{
			name: "existing no capacity", body: `{"error":{"message":"No capacity available"}}`,
			status: http.StatusServiceUnavailable, wait: 250 * time.Millisecond,
		},
		{name: "canceled wait", body: resource("0.1s", ""), wait: 100 * time.Millisecond, cancel: true},
		{name: "zero retry budget", body: resource("0.1s", ""), disableRetry: true},
	} {
		for _, entry := range []string{"generate", "claude aggregate", "stream"} {
			t.Run(
				fixture.name+"/"+entry, func(t *testing.T) {
					synctest.Test(
						t, func(t *testing.T) {
							ctx, _ := xaiOutcomeContext(t)
							ctx, cancel := context.WithCancel(ctx)
							defer cancel()
							if fixture.cancel {
								timer := time.AfterFunc(50*time.Millisecond, cancel)
								defer timer.Stop()
							}
							credential := antigravityCountTokensAuth(
								"https://resource-retry.synthetic.invalid", t.Name(),
							)
							failures := max(fixture.failures, 1)
							cfg := &config.Config{RequestRetry: failures}
							if fixture.disableRetry {
								cfg.RequestRetry = 0
							}
							status := fixture.status
							if status == 0 {
								status = http.StatusTooManyRequests
							}
							calls := 0
							var previousAttempt time.Time
							transport := &auditClaudeContractTransport{
								respond: func(request *http.Request) (*http.Response, error) {
									calls++
									if fixture.failures > 1 && calls > 1 {
										wantDelay := time.Duration(min(calls-1, 5)) * 100 * time.Millisecond
										if elapsed := time.Since(previousAttempt); elapsed != wantDelay {
											t.Errorf("attempt=%d delay=%v, want %v", calls, elapsed, wantDelay)
										}
									}
									previousAttempt = time.Now()
									if request.URL.Host != "resource-retry.synthetic.invalid" ||
										calls > failures+1 {
										t.Fatalf("unexpected upstream request %d: %s", calls, request.URL)
									}
									body := fixture.body
									responseStatus := status
									headers := http.Header{"Content-Type": {"application/json"}}
									if calls == failures+1 {
										responseStatus = http.StatusOK
										body = (`{"response":{"candidates":[{"content":{"role":"model",` +
											`"parts":[{"text"`) +
											`:"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1` +
											`,"candidatesTokenCount":1,"totalTokenCount":2}}}`
										if entry != "generate" {
											headers.Set("Content-Type", "text/event-stream")
											body = "data: " + body + "\n\n"
										}
									}
									return &http.Response{
										StatusCode: responseStatus, Header: headers, Request: request,
										Body: io.NopCloser(strings.NewReader(body)),
									}, nil
								},
							}
							ctx = util.WithRoundTripper(ctx, transport)
							if newAntigravityHTTPClient(ctx, cfg, credential, 0).Transport != transport {
								t.Fatal("resource retry transport is not memory-isolated")
							}
							request := core.Request{
								Model: "gemini-2.5-flash",
								Payload: []byte(
									`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`,
								),
							}
							opts := core.Options{
								SourceFormat:   translator.FormatAntigravity,
								ResponseFormat: translator.FormatAntigravity,
							}
							if entry == "claude aggregate" {
								request.Model = "gemini-3-pro-preview"
							}
							started := time.Now()
							executor := NewAntigravityExecutor(cfg)
							var err error
							var delivered []byte
							if entry == "stream" {
								var stream *core.StreamResult
								stream, err = executor.ExecuteStream(ctx, credential, request, opts)
								if err == nil {
									for chunk := range stream.Chunks {
										delivered = append(delivered, chunk.Payload...)
										if chunk.Err != nil {
											err = chunk.Err
										}
									}
								}
							} else {
								var response core.Response
								response, err = executor.Execute(ctx, credential, request, opts)
								delivered = response.Payload
							}
							wantCalls := 1
							wantWait := fixture.wait
							switch {
							case fixture.cancel:
								wantWait = 50 * time.Millisecond
								if !errors.Is(err, context.Canceled) {
									t.Errorf("error=%v; expected cancellation during the bounded wait", err)
								}
							case fixture.wait > 0:
								wantCalls = failures + 1
								if err != nil || !strings.Contains(string(delivered), "ok") {
									t.Errorf(
										"error=%v delivered=%s; expected successful final request",
										err,
										delivered,
									)
								}
							default:
								if testExecutorStatusCode(err) != status {
									t.Errorf("error=%v; expected original status %d", err, status)
								}
							}
							if calls != wantCalls || time.Since(started) != wantWait {
								t.Fatalf(
									"calls=%d elapsed=%v; expected calls=%d elapsed=%v",
									calls,
									time.Since(started),
									wantCalls,
									wantWait,
								)
							}
						},
					)
				},
			)
		}
	}
}
