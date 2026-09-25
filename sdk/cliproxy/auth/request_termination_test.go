package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestRequestTerminatedErrorSkipsRetryAndCreditsFallback(t *testing.T) {
	errTerminated := &cliproxyexecutor.RequestTerminatedError{HTTPStatus: http.StatusTooManyRequests}
	if !isRequestScopedError(errTerminated) {
		t.Fatal("isRequestScopedError() = false")
	}
	manager := &Manager{}
	manager.runtimeConfig.Store(
		&internalconfig.Config{
			QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
		},
	)
	if shouldAttemptAntigravityCreditsFallback(manager, errTerminated) {
		t.Fatal("terminated request must not use Antigravity credits fallback")
	}
	if _, retry := manager.shouldRetryAfterError(
		context.Background(), cliproxyexecutor.Options{}, errTerminated,
		0,
		[]string{"antigravity"},
		"claude-test",
		1,
	); retry {
		t.Fatal("terminated request must not wait for cooldown retry")
	}
}

func TestApplyRequestAfterAuthInterceptorReturnsTerminationBeforeHandledRequest(t *testing.T) {
	originalReq := cliproxyexecutor.Request{Model: "model", Payload: []byte(`{"original":true}`)}
	originalOpts := cliproxyexecutor.Options{
		Headers: http.Header{"X-Original": {"one"}},
		RequestAfterAuthInterceptor: func(
			context.Context,
			cliproxyexecutor.RequestAfterAuthInterceptRequest,
		) cliproxyexecutor.RequestAfterAuthInterceptResponse {
			return cliproxyexecutor.RequestAfterAuthInterceptResponse{
				Handled:         true,
				Request:         cliproxyexecutor.Request{Model: "changed"},
				Terminate:       true,
				StatusCode:      http.StatusForbidden,
				ResponseHeaders: http.Header{"X-Policy": {"blocked"}},
				ResponseBody:    []byte(`{"error":"blocked"}`),
			}
		},
	}

	gotReq, gotOpts, errIntercept := applyRequestAfterAuthInterceptor(
		context.Background(),
		nil,
		"codex",
		nil,
		originalReq,
		originalOpts,
		"model",
	)
	var terminated *cliproxyexecutor.RequestTerminatedError
	if !errors.As(errIntercept, &terminated) || terminated == nil {
		t.Fatalf("interceptor error = %v, want RequestTerminatedError", errIntercept)
	}
	if gotReq.Model != originalReq.Model || string(gotReq.Payload) != string(originalReq.Payload) {
		t.Fatalf("terminated request = %#v, want original %#v", gotReq, originalReq)
	}
	if gotOpts.Headers.Get("X-Original") != "one" {
		t.Fatalf("terminated options = %#v", gotOpts)
	}
	if terminated.StatusCode() != http.StatusForbidden ||
		terminated.ResponseHeaders().Get("X-Policy") != "blocked" ||
		string(terminated.ResponseBody()) != `{"error":"blocked"}` {
		t.Fatalf("termination response = %#v", terminated)
	}
}
