package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
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

	_, gotReq, gotOpts, errIntercept := applyRequestAfterAuthInterceptor(
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

func TestRequestAfterAuthInterceptorOwnsControlledContainers(t *testing.T) {
	for _, handled := range []bool{false, true} {
		name := "unhandled"
		if handled {
			name = "handled"
		}
		t.Run(
			name, func(t *testing.T) {
				opaque := &struct{ value string }{value: "caller-owned"}
				request := cliproxyexecutor.Request{
					Model: "model", Payload: []byte(`{"value":"original"}`),
					Metadata: map[string]any{"request": "original", "opaque": opaque},
				}
				options := cliproxyexecutor.Options{
					Headers: http.Header{"X-Original": {"one"}}, Query: url.Values{"q": {"one"}},
					OriginalRequest: []byte(`{"value":"baseline"}`),
					Metadata:        map[string]any{"option": "original", "opaque": opaque},
				}
				var captured cliproxyexecutor.RequestAfterAuthInterceptRequest
				options.RequestAfterAuthInterceptor = func(
					_ context.Context,
					in cliproxyexecutor.RequestAfterAuthInterceptRequest,
				) cliproxyexecutor.RequestAfterAuthInterceptResponse {
					captured = in
					if in.Request.Metadata["opaque"] != opaque || in.Options.Metadata["opaque"] != opaque {
						t.Error("opaque metadata ownership changed")
					}
					in.Request.Payload[10] = 'x'
					in.Options.OriginalRequest[10] = 'y'
					in.Request.Metadata["request"] = "changed"
					in.Options.Metadata["option"] = "changed"
					in.Options.Headers["X-Original"][0] = "changed"
					in.Options.Query["q"][0] = "changed"
					return cliproxyexecutor.RequestAfterAuthInterceptResponse{
						Handled: handled, Request: in.Request, Options: in.Options,
					}
				}
				_, got, opts, err := applyRequestAfterAuthInterceptor(
					t.Context(), nil, "codex", nil, request, options, "model",
				)
				if err != nil {
					t.Fatal(err)
				}
				if string(request.Payload) !=
					`{"value":"original"}` ||
					string(options.OriginalRequest) !=
						`{"value":"baseline"}` ||
					request.Metadata["request"] != "original" || options.Metadata["option"] != "original" ||
					options.Headers.Get("X-Original") != "one" ||
					options.Query.Get("q") != "one" {
					t.Fatal("callback changed caller-owned request containers")
				}
				captured.Request.Payload[10] = 'z'
				captured.Options.OriginalRequest[10] = 'z'
				captured.Request.Metadata["request"] = "late"
				captured.Options.Metadata["option"] = "late"
				captured.Options.Headers["X-Original"][0] = "late"
				captured.Options.Query["q"][0] = "late"
				wantRequest, wantOriginal, want := `{"value":"original"}`, `{"value":"baseline"}`, "original"
				wantHeader := "one"
				if handled {
					//goland:noinspection SpellCheckingInspection
					wantRequest, wantOriginal, want, wantHeader = `{"value":"xriginal"}`, `{"value":"yaseline`+
						`"}`, "changed", "changed"
				}
				if string(got.Payload) != wantRequest || string(opts.OriginalRequest) != wantOriginal ||
					got.Metadata["request"] != want || opts.Metadata["option"] != want ||
					opts.Headers.Get("X-Original") != wantHeader || opts.Query.Get("q") != wantHeader ||
					got.Metadata["opaque"] != opaque || opts.Metadata["opaque"] != opaque {
					t.Fatal("late callback mutation changed the selected execution containers")
				}
			},
		)
	}
}
