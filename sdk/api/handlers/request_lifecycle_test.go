package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
)

type lifecycleTestHost struct {
	interceptRequestBeforeAuth func(
		context.Context,
		pluginapi.RequestInterceptRequest,
	) pluginapi.RequestInterceptResponse
	interceptRequestAfterAuth func(
		context.Context,
		pluginapi.RequestInterceptRequest,
	) pluginapi.RequestInterceptResponse
	interceptResponse func(
		context.Context,
		pluginapi.ResponseInterceptRequest,
	) pluginapi.ResponseInterceptResponse
	interceptStreamChunk func(
		context.Context,
		pluginapi.StreamChunkInterceptRequest,
	) pluginapi.StreamChunkInterceptResponse
	completeRequest func(context.Context, pluginapi.RequestCompletion)
}

func (h *lifecycleTestHost) InterceptRequestBeforeAuth(
	ctx context.Context,
	req pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	if h != nil && h.interceptRequestBeforeAuth != nil {
		return h.interceptRequestBeforeAuth(ctx, req)
	}
	return pluginapi.RequestInterceptResponse{Headers: cloneHeader(req.Headers), Body: cloneBytes(req.Body)}
}

func (h *lifecycleTestHost) InterceptRequestAfterAuth(
	ctx context.Context,
	req pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	if h != nil && h.interceptRequestAfterAuth != nil {
		return h.interceptRequestAfterAuth(ctx, req)
	}
	return pluginapi.RequestInterceptResponse{Headers: cloneHeader(req.Headers), Body: cloneBytes(req.Body)}
}

func (h *lifecycleTestHost) InterceptResponse(
	ctx context.Context,
	req pluginapi.ResponseInterceptRequest,
) pluginapi.ResponseInterceptResponse {
	if h != nil && h.interceptResponse != nil {
		return h.interceptResponse(ctx, req)
	}
	return pluginapi.ResponseInterceptResponse{Headers: cloneHeader(req.ResponseHeaders), Body: cloneBytes(req.Body)}
}

func (h *lifecycleTestHost) InterceptStreamChunk(
	ctx context.Context,
	req pluginapi.StreamChunkInterceptRequest,
) pluginapi.StreamChunkInterceptResponse {
	if h != nil && h.interceptStreamChunk != nil {
		return h.interceptStreamChunk(ctx, req)
	}
	return pluginapi.StreamChunkInterceptResponse{Headers: cloneHeader(req.ResponseHeaders), Body: cloneBytes(req.Body)}
}

func (h *lifecycleTestHost) CompleteRequest(ctx context.Context, completion pluginapi.RequestCompletion) {
	if h != nil && h.completeRequest != nil {
		h.completeRequest(ctx, completion)
	}
}

func TestRequestInterceptorWithoutBodyChangeKeepsOriginalRequest(t *testing.T) {
	host := &lifecycleTestHost{interceptRequestAfterAuth: func(
		_ context.Context,
		req pluginapi.RequestInterceptRequest,
	) pluginapi.RequestInterceptResponse {
		if string(req.Body) != "payload" {
			t.Fatalf("request body = %q, want payload", req.Body)
		}
		return pluginapi.RequestInterceptResponse{Headers: http.Header{"X-Plugin": {"yes"}}}
	}}
	handler := &BaseAPIHandler{PluginHost: host}
	request := coreexecutor.Request{Payload: []byte("payload")}
	options := coreexecutor.Options{OriginalRequest: []byte("original")}

	response := handler.applyRequestInterceptorsAfterAuth(
		context.Background(), coreexecutor.RequestAfterAuthInterceptRequest{
			Request: request,
			Options: options,
		}, nil, "",
	)
	if string(response.Request.Payload) != "payload" || string(response.Options.OriginalRequest) != "original" ||
		response.Options.Headers.Get("X-Plugin") != "yes" {
		t.Fatalf("unexpected response: payload = %q, original = %q, headers = %#v",
			response.Request.Payload, response.Options.OriginalRequest, response.Options.Headers)
	}
}

func TestWriteModelListResponseAppliesResponseInterceptor(t *testing.T) {
	var captured pluginapi.ResponseInterceptRequest
	host := &lifecycleTestHost{interceptResponse: func(
		_ context.Context,
		req pluginapi.ResponseInterceptRequest,
	) pluginapi.ResponseInterceptResponse {
		captured = req
		return pluginapi.ResponseInterceptResponse{
			Headers: http.Header{"X-Plugin-Model-List": {"applied"}},
			Body:    []byte(`{"object":"list","data":[{"id":"rewritten"}]}`),
		}
	}}
	handler := &BaseAPIHandler{PluginHost: host}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("X-Client", "catalog")
	request = request.WithContext(logging.WithRequestID(request.Context(), "req-model-list"))
	ginContext.Request = request

	handler.WriteModelListResponse(ginContext, "openai", gin.H{
		"object": "list",
		"data":   []map[string]any{{"id": "original"}},
	})

	if captured.SourceFormat != "openai" || captured.StatusCode != http.StatusOK ||
		captured.RequestID != "req-model-list" || captured.RequestHeaders.Get("X-Client") != "catalog" ||
		!strings.Contains(string(captured.Body), `"id":"original"`) {
		t.Fatalf("captured model list response = %#v", captured)
	}
	if recorder.Header().Get("X-Plugin-Model-List") != "applied" ||
		!strings.Contains(recorder.Body.String(), `"id":"rewritten"`) {
		t.Fatalf("intercepted response headers=%v body=%s", recorder.Header(), recorder.Body.String())
	}
}

func TestWriteModelListResponseBodyPreservesInterceptor(t *testing.T) {
	var captured []byte
	host := &lifecycleTestHost{interceptResponse: func(
		_ context.Context,
		req pluginapi.ResponseInterceptRequest,
	) pluginapi.ResponseInterceptResponse {
		captured = append([]byte(nil), req.Body...)
		return pluginapi.ResponseInterceptResponse{
			Headers: http.Header{"X-Plugin-Model-List": {"applied"}},
			Body:    req.Body,
		}
	}}
	handler := &BaseAPIHandler{PluginHost: host}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.156.0", nil)
	body := []byte(`{"models":[{"display_name":"Model <tag>"}]}`)

	handler.WriteModelListResponseBody(ginContext, "openai", body)
	if string(captured) != string(body) || recorder.Body.String() != string(body) ||
		recorder.Header().Get("X-Plugin-Model-List") != "applied" {
		t.Fatalf("captured=%s response=%s headers=%v", captured, recorder.Body.String(), recorder.Header())
	}
}

type lifecycleCaptureExecutor struct {
	provider string

	mu          sync.Mutex
	lastRequest coreexecutor.Request
	execute     func(
		context.Context,
		*coreauth.Auth,
		coreexecutor.Request,
		coreexecutor.Options,
	) (coreexecutor.Response, error)
	executeCount func(
		context.Context,
		*coreauth.Auth,
		coreexecutor.Request,
		coreexecutor.Options,
	) (coreexecutor.Response, error)
	stream func(
		context.Context,
		*coreauth.Auth,
		coreexecutor.Request,
		coreexecutor.Options,
	) (*coreexecutor.StreamResult, error)
}

type lifecycleBootstrapDispositionError struct {
	message     string
	disposition coreexecutor.ExecutionDisposition
}

func (e lifecycleBootstrapDispositionError) Error() string {
	return e.message
}

func (e lifecycleBootstrapDispositionError) ExecutionDisposition() coreexecutor.ExecutionDisposition {
	return e.disposition
}

func (e *lifecycleCaptureExecutor) Identifier() string {
	if e.provider != "" {
		return e.provider
	}
	return "codex"
}

func (e *lifecycleCaptureExecutor) Execute(
	ctx context.Context,
	auth *coreauth.Auth,
	req coreexecutor.Request,
	opts coreexecutor.Options,
) (coreexecutor.Response, error) {
	e.capture(req)
	if e.execute != nil {
		return e.execute(ctx, auth, req, opts)
	}
	return coreexecutor.Response{Payload: []byte("ok")}, nil
}

func (e *lifecycleCaptureExecutor) ExecuteStream(
	ctx context.Context,
	auth *coreauth.Auth,
	req coreexecutor.Request,
	opts coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	e.capture(req)
	if e.stream != nil {
		return e.stream(ctx, auth, req, opts)
	}
	chunks := make(chan coreexecutor.StreamChunk)
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *lifecycleCaptureExecutor) Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (e *lifecycleCaptureExecutor) CountTokens(
	ctx context.Context,
	auth *coreauth.Auth,
	req coreexecutor.Request,
	opts coreexecutor.Options,
) (coreexecutor.Response, error) {
	e.capture(req)
	if e.executeCount != nil {
		return e.executeCount(ctx, auth, req, opts)
	}
	return coreexecutor.Response{Payload: []byte("0")}, nil
}

func (e *lifecycleCaptureExecutor) HttpRequest(
	context.Context,
	*coreauth.Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, &coreauth.Error{
		Code:       "not_implemented",
		Message:    "HttpRequest not implemented",
		HTTPStatus: http.StatusNotImplemented,
	}
}

func (e *lifecycleCaptureExecutor) capture(req coreexecutor.Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastRequest = coreexecutor.Request{Model: req.Model, Payload: cloneBytes(req.Payload)}
}

func (e *lifecycleCaptureExecutor) captured() coreexecutor.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastRequest
}

func newLifecycleHandler(
	t *testing.T,
	model string,
	executor *lifecycleCaptureExecutor,
	hooks ...coreauth.Hook,
) *BaseAPIHandler {
	t.Helper()
	var hook coreauth.Hook
	if len(hooks) > 0 {
		hook = hooks[0]
	}
	manager := coreauth.NewManager(nil, nil, hook)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "handler-lifecycle-" + model,
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": model + "@example.com"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("manager.Register(): %v", errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(
		auth.ID,
		auth.Provider,
		[]*registry.ModelInfo{{ID: model}},
	)
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		},
	)
	return NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
}

func TestRequestLifecycleTrackerUsesUniqueExecutionIDs(t *testing.T) {
	handler := NewBaseAPIHandlers(nil, nil)
	ctx := logging.WithRequestID(context.Background(), "trace-1")
	first := handler.newRequestLifecycleTracker(ctx, "", "openai", "model", "model", false, nil, "")
	second := handler.newRequestLifecycleTracker(ctx, "", "openai", "model", "model", false, nil, "")
	if first.requestID() == "" || second.requestID() == "" || first.requestID() == second.requestID() {
		t.Fatalf("lifecycle request IDs = %q and %q", first.requestID(), second.requestID())
	}
	if first.traceID() != "trace-1" || second.traceID() != "trace-1" {
		t.Fatalf("trace IDs = %q and %q", first.traceID(), second.traceID())
	}
}

func TestHandlerRequestInterceptorTerminatesBeforeAuth(t *testing.T) {
	model := "handler-lifecycle-terminate-before-auth"
	executor := &lifecycleCaptureExecutor{}
	handler := newLifecycleHandler(t, model, executor)
	var requestID string
	var completion pluginapi.RequestCompletion
	handler.SetPluginHost(
		&lifecycleTestHost{
			interceptRequestBeforeAuth: func(
				_ context.Context,
				req pluginapi.RequestInterceptRequest,
			) pluginapi.RequestInterceptResponse {
				requestID = req.RequestID
				return pluginapi.RequestInterceptResponse{
					Terminate:       true,
					StatusCode:      0,
					ResponseHeaders: http.Header{"Content-Type": {"application/json"}, "X-Policy": {"blocked"}},
					ResponseBody:    []byte(`{"error":"blocked"}`),
				}
			},
			completeRequest: func(_ context.Context, got pluginapi.RequestCompletion) {
				completion = got
			},
		},
	)

	body, headers, errMsg := handler.ExecuteWithAuthManager(
		context.Background(),
		"openai",
		model,
		[]byte(`{"model":"`+model+`"}`),
		"",
	)
	if body != nil || headers != nil {
		t.Fatalf("terminated response body = %q, headers = %#v", body, headers)
	}
	if errMsg == nil || !errMsg.DirectResponse || errMsg.StatusCode != http.StatusForbidden {
		t.Fatalf("termination error = %#v", errMsg)
	}
	if requestID == "" || completion.RequestID != requestID {
		t.Fatalf("request IDs = start %q, completion %q", requestID, completion.RequestID)
	}
	if completion.Outcome != pluginapi.RequestCompletionRejected || completion.StatusCode != http.StatusForbidden {
		t.Fatalf("completion = %#v", completion)
	}
	if executor.captured().Model != "" {
		t.Fatalf("executor received terminated request: %#v", executor.captured())
	}
}

func TestHandlerRequestInterceptorTerminatesAfterAuth(t *testing.T) {
	model := "handler-lifecycle-terminate-after-auth"
	executor := &lifecycleCaptureExecutor{}
	handler := newLifecycleHandler(t, model, executor)
	var beforeRequestID string
	var afterRequestID string
	var completion pluginapi.RequestCompletion
	handler.SetPluginHost(
		&lifecycleTestHost{
			interceptRequestBeforeAuth: func(
				_ context.Context,
				req pluginapi.RequestInterceptRequest,
			) pluginapi.RequestInterceptResponse {
				beforeRequestID = req.RequestID
				return pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body}
			},
			interceptRequestAfterAuth: func(
				_ context.Context,
				req pluginapi.RequestInterceptRequest,
			) pluginapi.RequestInterceptResponse {
				afterRequestID = req.RequestID
				return pluginapi.RequestInterceptResponse{
					Terminate:       true,
					StatusCode:      http.StatusTooManyRequests,
					ResponseHeaders: http.Header{"Retry-After": {"3"}},
					ResponseBody:    []byte(`{"error":"busy"}`),
				}
			},
			completeRequest: func(_ context.Context, got pluginapi.RequestCompletion) {
				completion = got
			},
		},
	)

	_, _, errMsg := handler.ExecuteWithAuthManager(
		context.Background(),
		"openai",
		model,
		[]byte(`{"model":"`+model+`"}`),
		"",
	)
	if errMsg == nil || !errMsg.DirectResponse || errMsg.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("termination error = %#v", errMsg)
	}
	if beforeRequestID == "" || afterRequestID != beforeRequestID || completion.RequestID != beforeRequestID {
		t.Fatalf(
			"request IDs = before %q, after %q, completion %q",
			beforeRequestID,
			afterRequestID,
			completion.RequestID,
		)
	}
	if completion.Outcome != pluginapi.RequestCompletionRejected {
		t.Fatalf("completion outcome = %q", completion.Outcome)
	}
	if executor.captured().Model != "" {
		t.Fatalf("executor received terminated request: %#v", executor.captured())
	}
}

func TestHandlerAfterAuthTerminationSkipsCountAndStreamExecutors(t *testing.T) {
	for _, operation := range []string{"count", "stream"} {
		t.Run(
			operation, func(t *testing.T) {
				model := "handler-lifecycle-terminate-" + operation
				executor := &lifecycleCaptureExecutor{}
				handler := newLifecycleHandler(t, model, executor)
				afterCalls := 0
				handler.SetPluginHost(
					&lifecycleTestHost{
						interceptRequestAfterAuth: func(
							_ context.Context,
							_ pluginapi.RequestInterceptRequest,
						) pluginapi.RequestInterceptResponse {
							afterCalls++
							return pluginapi.RequestInterceptResponse{
								Terminate:    true,
								StatusCode:   http.StatusForbidden,
								ResponseBody: []byte(`{"error":"blocked"}`),
							}
						},
					},
				)

				var errMsg *ErrorMessage
				if operation == "count" {
					_, _, errMsg = handler.ExecuteCountWithAuthManager(
						context.Background(),
						"openai",
						model,
						[]byte(`{"model":"`+model+`"}`),
						"",
					)
				} else {
					dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(
						context.Background(),
						"openai",
						model,
						[]byte(`{"model":"`+model+`","stream":true}`),
						"",
					)
					if dataChan != nil {
						t.Fatal("terminated stream returned a data channel")
					}
					errMsg = <-errChan
				}
				if errMsg == nil || !errMsg.DirectResponse || errMsg.StatusCode != http.StatusForbidden {
					t.Fatalf("termination error = %#v", errMsg)
				}
				if afterCalls != 1 {
					t.Fatalf("after-auth interceptor calls = %d, want 1", afterCalls)
				}
				if executor.captured().Model != "" {
					t.Fatalf("executor received terminated request: %#v", executor.captured())
				}
			},
		)
	}
}

func TestHandlerLifecycleCompletesSuccessfulAndFailedRequests(t *testing.T) {
	tests := []struct {
		name string
		exec func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (
			coreexecutor.Response,
			error,
		)
		wantOutcome pluginapi.RequestCompletionOutcome
		wantError   bool
	}{
		{
			name: "success",
			exec: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (
				coreexecutor.Response,
				error,
			) {
				return coreexecutor.Response{Payload: []byte("ok")}, nil
			},
			wantOutcome: pluginapi.RequestCompletionSucceeded,
		},
		{
			name: "failed",
			exec: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (
				coreexecutor.Response,
				error,
			) {
				return coreexecutor.Response{}, fmt.Errorf("upstream failed")
			},
			wantOutcome: pluginapi.RequestCompletionFailed,
			wantError:   true,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				model := "handler-lifecycle-" + test.name
				executor := &lifecycleCaptureExecutor{execute: test.exec}
				handler := newLifecycleHandler(t, model, executor)
				var requestID string
				var responseRequestID string
				var completions []pluginapi.RequestCompletion
				handler.SetPluginHost(
					&lifecycleTestHost{
						interceptRequestBeforeAuth: func(
							_ context.Context,
							req pluginapi.RequestInterceptRequest,
						) pluginapi.RequestInterceptResponse {
							requestID = req.RequestID
							return pluginapi.RequestInterceptResponse{Headers: req.Headers, Body: req.Body}
						},
						interceptResponse: func(
							_ context.Context,
							req pluginapi.ResponseInterceptRequest,
						) pluginapi.ResponseInterceptResponse {
							responseRequestID = req.RequestID
							return pluginapi.ResponseInterceptResponse{Headers: req.ResponseHeaders, Body: req.Body}
						},
						completeRequest: func(_ context.Context, completion pluginapi.RequestCompletion) {
							completions = append(completions, completion)
						},
					},
				)

				_, _, errMsg := handler.ExecuteWithAuthManager(
					context.Background(),
					"openai",
					model,
					[]byte(`{"model":"`+model+`"}`),
					"",
				)
				if test.wantError != (errMsg != nil) {
					t.Fatalf("error = %#v, wantError = %t", errMsg, test.wantError)
				}
				if len(completions) != 1 {
					t.Fatalf("completion count = %d, want 1", len(completions))
				}
				completion := completions[0]
				if completion.RequestID != requestID || completion.Outcome != test.wantOutcome {
					t.Fatalf("completion = %#v", completion)
				}
				if completion.StartedAt.IsZero() || completion.CompletedAt.Before(completion.StartedAt) {
					t.Fatalf("completion timestamps = %#v", completion)
				}
				if !test.wantError && responseRequestID != requestID {
					t.Fatalf("response request ID = %q, want %q", responseRequestID, requestID)
				}
			},
		)
	}
}

func TestHandlerLifecycleCompletesStreamOnce(t *testing.T) {
	tests := []struct {
		name        string
		cancel      bool
		wantOutcome pluginapi.RequestCompletionOutcome
	}{
		{name: "success", wantOutcome: pluginapi.RequestCompletionSucceeded},
		{name: "canceled", cancel: true, wantOutcome: pluginapi.RequestCompletionCanceled},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				model := "handler-lifecycle-stream-" + test.name
				chunks := make(chan coreexecutor.StreamChunk, 1)
				chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"chunk":true}`)}
				if !test.cancel {
					close(chunks)
				}
				executor := &lifecycleCaptureExecutor{
					stream: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (
						*coreexecutor.StreamResult,
						error,
					) {
						return &coreexecutor.StreamResult{Chunks: chunks}, nil
					},
				}
				handler := newLifecycleHandler(t, model, executor)
				completions := make(chan pluginapi.RequestCompletion, 2)
				var streamRequestID string
				handler.SetPluginHost(
					&lifecycleTestHost{
						interceptStreamChunk: func(
							_ context.Context,
							req pluginapi.StreamChunkInterceptRequest,
						) pluginapi.StreamChunkInterceptResponse {
							streamRequestID = req.RequestID
							return pluginapi.StreamChunkInterceptResponse{Headers: req.ResponseHeaders, Body: req.Body}
						},
						completeRequest: func(_ context.Context, completion pluginapi.RequestCompletion) {
							completions <- completion
						},
					},
				)
				ctx, cancel := context.WithCancel(context.Background())
				dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(
					ctx,
					"openai",
					model,
					[]byte(`{"model":"`+model+`","stream":true}`),
					"",
				)
				if test.cancel {
					cancel()
				} else {
					defer cancel()
				}
				for dataChan != nil || errChan != nil {
					select {
					case _, ok := <-dataChan:
						if !ok {
							dataChan = nil
						}
					case errMsg, ok := <-errChan:
						if ok && errMsg != nil {
							t.Fatalf("stream error = %#v", errMsg)
						}
						if !ok {
							errChan = nil
						}
					}
				}
				select {
				case completion := <-completions:
					if completion.Outcome != test.wantOutcome || completion.RequestID == "" {
						t.Fatalf("stream completion = %#v", completion)
					}
					if !test.cancel && streamRequestID != completion.RequestID {
						t.Fatalf("stream request ID = %q, completion ID = %q", streamRequestID, completion.RequestID)
					}
				case <-time.After(time.Second):
					t.Fatal("missing stream completion")
				}
				select {
				case duplicate := <-completions:
					t.Fatalf("duplicate stream completion = %#v", duplicate)
				default:
				}
			},
		)
	}
}

func TestHandlerLifecycleClassifiesCanceledDeliveryAcknowledgements(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		err           error
		cancelContext bool
		wantOutcome   pluginapi.RequestCompletionOutcome
		wantStatus    int
	}{
		{name: "canceled", err: context.Canceled, cancelContext: true, wantOutcome: pluginapi.RequestCompletionCanceled},
		{name: "wrapped canceled", err: fmt.Errorf("nested delivery: %w", context.Canceled), cancelContext: true, wantOutcome: pluginapi.RequestCompletionCanceled},
		{name: "write failure", err: errors.New("downstream write failed"), cancelContext: true, wantOutcome: pluginapi.RequestCompletionFailed, wantStatus: http.StatusInternalServerError},
		{name: "canceled before context", err: context.Canceled, wantOutcome: pluginapi.RequestCompletionCanceled},
		{name: "wrapped canceled before context", err: fmt.Errorf("nested delivery: %w", context.Canceled), wantOutcome: pluginapi.RequestCompletionCanceled},
		{name: "write failure before context", err: errors.New("downstream write failed"), wantOutcome: pluginapi.RequestCompletionFailed, wantStatus: http.StatusInternalServerError},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			model := "handler-lifecycle-canceled-delivery-" + strings.ReplaceAll(testCase.name, " ", "-")
			source := make(chan coreexecutor.StreamChunk, 1)
			source <- coreexecutor.StreamChunk{Payload: []byte(`{"chunk":true}`)}
			t.Cleanup(func() { close(source) })
			executor := &lifecycleCaptureExecutor{
				stream: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
					return &coreexecutor.StreamResult{Chunks: source}, nil
				},
			}
			handler := newLifecycleHandler(t, model, executor)
			completions := make(chan pluginapi.RequestCompletion, 2)
			handler.SetPluginHost(&lifecycleTestHost{
				completeRequest: func(_ context.Context, completion pluginapi.RequestCompletion) {
					completions <- completion
				},
			})
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			dataChan, _, errChan := handler.ExecuteStreamChunksWithAuthManager(
				ctx, "openai", model, []byte(`{"model":"`+model+`","stream":true}`), "",
			)
			select {
			case payload := <-dataChan:
				if payload.Acknowledge == nil {
					t.Fatal("payload acknowledgement is nil")
				}
				payload.Acknowledge(testCase.err)
			case <-time.After(time.Second):
				t.Fatal("missing initial stream payload")
			}
			if testCase.cancelContext {
				cancel()
			}
			for range dataChan {
			}
			for range errChan {
			}
			select {
			case completion := <-completions:
				if completion.Outcome != testCase.wantOutcome || completion.StatusCode != testCase.wantStatus || completion.Error != testCase.err.Error() {
					t.Fatalf("completion = %#v, want outcome %s, status %d, error %q", completion, testCase.wantOutcome, testCase.wantStatus, testCase.err.Error())
				}
			case <-time.After(time.Second):
				t.Fatal("missing stream completion")
			}
			select {
			case completion := <-completions:
				t.Fatalf("duplicate stream completion = %#v", completion)
			default:
			}
		})
	}
}

func TestHandlerLifecycleCompletesDeliveryFailuresAsFailed(t *testing.T) {
	for _, stage := range []string{"payload", "terminal"} {
		t.Run(
			stage, func(t *testing.T) {
				model := "handler-lifecycle-delivery-" + stage
				source := make(chan coreexecutor.StreamChunk, 1)
				source <- coreexecutor.StreamChunk{Payload: []byte(`{"chunk":true}`)}
				close(source)
				executor := &lifecycleCaptureExecutor{
					stream: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (
						*coreexecutor.StreamResult,
						error,
					) {
						return &coreexecutor.StreamResult{Chunks: source}, nil
					},
				}
				handler := newLifecycleHandler(t, model, executor)
				completions := make(chan pluginapi.RequestCompletion, 1)
				handler.SetPluginHost(
					&lifecycleTestHost{
						completeRequest: func(_ context.Context, completion pluginapi.RequestCompletion) {
							completions <- completion
						},
					},
				)
				dataChan, _, errChan := handler.ExecuteStreamChunksWithAuthManager(
					context.Background(),
					"openai",
					model,
					[]byte(`{"model":"`+model+`","stream":true}`),
					"",
				)
				writeErr := errors.New("downstream " + stage + " write failed")
				payload := <-dataChan
				if payload.Acknowledge == nil {
					t.Fatal("payload acknowledgement is nil")
				}
				if stage == "payload" {
					payload.Acknowledge(writeErr)
				} else {
					payload.Acknowledge(nil)
					terminal := <-dataChan
					if !terminal.Terminal || terminal.Acknowledge == nil {
						t.Fatalf("terminal chunk = %#v", terminal)
					}
					terminal.Acknowledge(writeErr)
				}
				for range dataChan {
				}
				for range errChan {
				}

				select {
				case completion := <-completions:
					if completion.Outcome != pluginapi.RequestCompletionFailed || completion.Error != writeErr.Error() {
						t.Fatalf("completion = %#v, want delivery failure", completion)
					}
				case <-time.After(time.Second):
					t.Fatal("missing delivery failure completion")
				}
			},
		)
	}
}

func TestHandlerStreamInterceptorDropAcknowledgesRequestScopedFailure(t *testing.T) {
	model := "handler-lifecycle-stream-drop"
	acknowledged := make(chan error, 1)
	chunks := make(chan coreexecutor.StreamChunk, 2)
	chunks <- coreexecutor.StreamChunk{
		Payload: []byte(`{"chunk":1}`),
		Acknowledge: func(err error) {
			acknowledged <- err
		},
	}
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"chunk":2}`)}
	close(chunks)
	executor := &lifecycleCaptureExecutor{
		stream: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (
			*coreexecutor.StreamResult,
			error,
		) {
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	handler := newLifecycleHandler(t, model, executor)
	completions := make(chan pluginapi.RequestCompletion, 1)
	handler.SetPluginHost(
		&lifecycleTestHost{
			interceptStreamChunk: func(
				_ context.Context,
				req pluginapi.StreamChunkInterceptRequest,
			) pluginapi.StreamChunkInterceptResponse {
				return pluginapi.StreamChunkInterceptResponse{
					Headers:   req.ResponseHeaders,
					Body:      req.Body,
					DropChunk: true,
				}
			},
			completeRequest: func(_ context.Context, completion pluginapi.RequestCompletion) {
				completions <- completion
			},
		},
	)
	ctx := t.Context()

	dataChan, _, errChan := handler.ExecuteStreamChunksWithAuthManager(
		ctx,
		"openai",
		model,
		[]byte(`{"model":"`+model+`","stream":true}`),
		"",
	)
	var errMsg *ErrorMessage
	select {
	case errMsg = <-errChan:
		if errMsg == nil || errMsg.Acknowledge == nil || errMsg.Error == nil {
			t.Fatalf("stream error = %#v", errMsg)
		}
	case <-time.After(time.Second):
		t.Fatal("missing stream interceptor error")
	}
	select {
	case err := <-acknowledged:
		t.Fatalf("source chunk acknowledged before terminal error delivery: %v", err)
	default:
	}

	errMsg.Acknowledge(nil)
	select {
	case err := <-acknowledged:
		if err == nil || err.Error() != "stream chunk dropped by interceptor" {
			t.Fatalf("source acknowledgement error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("source chunk was not acknowledged after terminal error delivery")
	}
	select {
	case _, ok := <-dataChan:
		if ok {
			t.Fatal("dropped stream emitted downstream data")
		}
	case <-time.After(time.Second):
		t.Fatal("dropped stream data channel did not close")
	}
	select {
	case completion := <-completions:
		if completion.Outcome != pluginapi.RequestCompletionFailed {
			t.Fatalf("stream completion = %#v, want failed", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("missing dropped stream completion")
	}
}

func TestHandlerStreamInterceptorDropPreservesDeliveryFailureCompletion(t *testing.T) {
	model := "handler-lifecycle-stream-drop-delivery"
	acknowledged := make(chan error, 1)
	chunks := make(chan coreexecutor.StreamChunk, 1)
	chunks <- coreexecutor.StreamChunk{
		Payload: []byte(`{"chunk":1}`),
		Acknowledge: func(err error) {
			acknowledged <- err
		},
	}
	close(chunks)
	executor := &lifecycleCaptureExecutor{
		stream: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (
			*coreexecutor.StreamResult,
			error,
		) {
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	handler := newLifecycleHandler(t, model, executor)
	completions := make(chan pluginapi.RequestCompletion, 1)
	handler.SetPluginHost(
		&lifecycleTestHost{
			interceptStreamChunk: func(
				_ context.Context,
				req pluginapi.StreamChunkInterceptRequest,
			) pluginapi.StreamChunkInterceptResponse {
				return pluginapi.StreamChunkInterceptResponse{
					Headers:   req.ResponseHeaders,
					Body:      req.Body,
					DropChunk: true,
				}
			},
			completeRequest: func(_ context.Context, completion pluginapi.RequestCompletion) {
				completions <- completion
			},
		},
	)

	dataChan, _, errChan := handler.ExecuteStreamChunksWithAuthManager(
		context.Background(),
		"openai",
		model,
		[]byte(`{"model":"`+model+`","stream":true}`),
		"",
	)
	errMsg := <-errChan
	if errMsg == nil || errMsg.Acknowledge == nil {
		t.Fatalf("stream error = %#v", errMsg)
	}
	writeErr := errors.New("stream error write failed")
	errMsg.Acknowledge(writeErr)
	for range dataChan {
	}
	for range errChan {
	}
	if errAck := <-acknowledged; !errors.Is(errAck, writeErr) {
		t.Fatalf("source acknowledgement error = %v, want %v", errAck, writeErr)
	}
	select {
	case completion := <-completions:
		if completion.Outcome != pluginapi.RequestCompletionFailed || completion.Error != writeErr.Error() {
			t.Fatalf("stream completion = %#v, want delivery failure", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("missing stream delivery failure completion")
	}
}

func TestHandlerStreamBootstrapReplayBoundary(t *testing.T) {
	tests := []struct {
		name      string
		firstErr  error
		wantCalls int
	}{
		{
			name: "explicit not sent",
			firstErr: lifecycleBootstrapDispositionError{
				message: "explicit not sent",
				disposition: coreexecutor.ExecutionDisposition{
					DispatchPhase:  coreexecutor.DispatchPhaseNotSent,
					Outcome:        coreexecutor.ExecutionOutcomeRejectedBeforeProcessing,
					RetryDirective: coreexecutor.RetryDirectiveNextAuth,
					EffectScope:    coreexecutor.StateEffectScopeRequest,
				},
			},
			wantCalls: 2,
		},
		{
			name:      "unclassified bootstrap outcome unknown",
			firstErr:  errors.New("unclassified bootstrap failure"),
			wantCalls: 1,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				model := "handler-bootstrap-" + strings.ReplaceAll(test.name, " ", "-")
				calls := 0
				var callsMu sync.Mutex
				executor := &lifecycleCaptureExecutor{
					stream: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (
						*coreexecutor.StreamResult,
						error,
					) {
						callsMu.Lock()
						calls++
						call := calls
						callsMu.Unlock()
						chunks := make(chan coreexecutor.StreamChunk, 1)
						if call == 1 {
							chunks <- coreexecutor.StreamChunk{Err: test.firstErr}
						} else {
							chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"chunk":1}`)}
						}
						close(chunks)
						return &coreexecutor.StreamResult{Chunks: chunks}, nil
					},
				}
				handler := newLifecycleHandler(t, model, executor)
				handler.UpdateClients(
					&sdkconfig.SDKConfig{
						Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 1},
					},
				)

				dataChan, _, errChan := handler.ExecuteStreamChunksWithAuthManager(
					context.Background(),
					"openai",
					model,
					[]byte(`{"model":"`+model+`","stream":true}`),
					"",
				)
				if test.wantCalls == 2 {
					payload := <-dataChan
					if payload.Terminal || payload.Acknowledge == nil || string(payload.Payload) != `{"chunk":1}` {
						t.Fatalf("stream payload = %#v", payload)
					}
					payload.Acknowledge(nil)
					terminal := <-dataChan
					if !terminal.Terminal || terminal.Acknowledge == nil {
						t.Fatalf("transport terminal = %#v", terminal)
					}
					terminal.Acknowledge(nil)
				} else {
					bootstrapData := dataChan
					timeout := time.After(time.Second)
				waitForError:
					for {
						select {
						case errMsg := <-errChan:
							if errMsg == nil || errMsg.Acknowledge == nil || errMsg.Error == nil {
								t.Fatalf("bootstrap error = %#v", errMsg)
							}
							errMsg.Acknowledge(nil)
							break waitForError
						case chunk, ok := <-bootstrapData:
							if !ok {
								bootstrapData = nil
								continue
							}
							if chunk.Acknowledge != nil {
								chunk.Acknowledge(nil)
							}
							for chunk = range dataChan {
								if chunk.Acknowledge != nil {
									chunk.Acknowledge(nil)
								}
							}
							t.Fatalf("unsafe bootstrap retry produced data: %#v", chunk)
						case <-timeout:
							t.Fatal("bootstrap result timed out")
						}
					}
				}
				for range dataChan {
				}
				for range errChan {
				}
				callsMu.Lock()
				gotCalls := calls
				callsMu.Unlock()
				if gotCalls != test.wantCalls {
					t.Fatalf("stream calls = %d, want %d", gotCalls, test.wantCalls)
				}
			},
		)
	}
}

func TestHandlerLifecycleCompletesFailedStream(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		status  int
		outcome pluginapi.RequestCompletionOutcome
		direct  bool
		body    string
	}{
		{"plain", errors.New("stream failed"), http.StatusInternalServerError, pluginapi.RequestCompletionFailed, false, ""},
		{"upstream", fmt.Errorf("wrapper: %w", &lifecycleUpstreamError{body: []byte(" synthetic denied\n"), direct: true}), http.StatusUnauthorized, pluginapi.RequestCompletionFailed, true, " synthetic denied\n"},
		{"upstream-empty", &lifecycleUpstreamError{direct: true}, http.StatusUnauthorized, pluginapi.RequestCompletionFailed, true, ""},
		{"plugin", &coreexecutor.RequestTerminatedError{HTTPStatus: http.StatusForbidden, Body: []byte("policy")}, http.StatusForbidden, pluginapi.RequestCompletionRejected, true, "policy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := "handler-lifecycle-stream-failed-" + test.name
			streamErr := test.err
			executor := &lifecycleCaptureExecutor{
				stream: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (
					*coreexecutor.StreamResult,
					error,
				) {
					chunks := make(chan coreexecutor.StreamChunk, 1)
					chunks <- coreexecutor.StreamChunk{Err: streamErr}
					close(chunks)
					return &coreexecutor.StreamResult{Chunks: chunks}, nil
				},
			}
			handler := newLifecycleHandler(t, model, executor)
			completions := make(chan pluginapi.RequestCompletion, 1)
			handler.SetPluginHost(
				&lifecycleTestHost{
					completeRequest: func(_ context.Context, completion pluginapi.RequestCompletion) {
						completions <- completion
					},
				},
			)

			dataChan, _, errChan := handler.ExecuteStreamWithAuthManager(
				context.Background(),
				"openai",
				model,
				[]byte(`{"model":"`+model+`","stream":true}`),
				"",
			)
			if chunk, ok := <-dataChan; ok {
				t.Fatalf("unexpected stream chunk = %q", chunk)
			}
			errMsg := <-errChan
			if errMsg == nil || !errors.Is(errMsg.Error, streamErr) || errMsg.DirectResponse != test.direct || string(errMsg.Body) != test.body {
				t.Fatalf("stream error = %#v", errMsg)
			}
			completion := <-completions
			if completion.Outcome != test.outcome ||
				completion.StatusCode != test.status ||
				completion.Error != streamErr.Error() {
				t.Fatalf("stream completion = %#v", completion)
			}
		})
	}
}

func TestWriteErrorResponseDirectResponseFiltersReservedHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Writer.Header().Set("X-Cpa-Trace-Id", "local-trace")
	ctx.Writer.Header().Set("Access-Control-Allow-Origin", "https://trusted.example")

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(
		ctx, &ErrorMessage{
			StatusCode:     http.StatusForbidden,
			DirectResponse: true,
			Body:           []byte(`{"error":"blocked"}`),
			Headers: http.Header{
				"Content-Type":                {"application/problem+json"},
				"X-Plugin-Policy":             {"blocked"},
				"X-Cpa-Trace-Id":              {"plugin-trace"},
				"Access-Control-Allow-Origin": {"https://untrusted.example"},
			},
		},
	)

	if recorder.Code != http.StatusForbidden || recorder.Body.String() != `{"error":"blocked"}` {
		t.Fatalf("response = status %d, body %q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "application/problem+json" ||
		recorder.Header().Get("X-Plugin-Policy") != "blocked" {
		t.Fatalf("plugin headers = %#v", recorder.Header())
	}
	if recorder.Header().Get("X-Cpa-Trace-Id") != "local-trace" ||
		recorder.Header().Get("Access-Control-Allow-Origin") != "https://trusted.example" {
		t.Fatalf("reserved headers were overwritten: %#v", recorder.Header())
	}
}

func TestExecutionErrorMessagePreservesTerminationResponse(t *testing.T) {
	errTerminated := &coreexecutor.RequestTerminatedError{
		HTTPStatus: http.StatusForbidden,
		Header:     http.Header{"X-Policy": {"blocked"}},
		Body:       []byte(`{"error":"blocked"}`),
	}
	msg := executionErrorMessage(errTerminated)
	if msg == nil || !msg.DirectResponse || msg.StatusCode != http.StatusForbidden {
		t.Fatalf("error message = %#v", msg)
	}
	if !errors.Is(msg.Error, errTerminated) || msg.Headers.Get("X-Policy") != "blocked" ||
		string(msg.Body) != `{"error":"blocked"}` {
		t.Fatalf("direct response = %#v", msg)
	}
}

type lifecycleUpstreamError struct {
	body   []byte
	direct bool
}

func (e *lifecycleUpstreamError) Error() string        { return "synthetic diagnostic" }
func (e *lifecycleUpstreamError) StatusCode() int      { return http.StatusUnauthorized }
func (e *lifecycleUpstreamError) DirectResponse() bool { return e.direct }
func (e *lifecycleUpstreamError) ResponseBody() []byte { return e.body }

func TestExecutionErrorMessagePreservesUpstreamResponse(t *testing.T) {
	for _, test := range []struct{ name, body, contentType string }{
		{"json", "  {\"error\":\"synthetic denied\"}\n", "application/json"},
		{"text", " synthetic denied\n", "text/plain; charset=utf-8"},
		{"empty", "", "text/plain; charset=utf-8"},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := &lifecycleUpstreamError{body: []byte(test.body), direct: true}
			wrapped := fmt.Errorf("transport wrapper: %w", original)
			msg := executionErrorMessage(wrapped)
			if !msg.DirectResponse || msg.StatusCode != http.StatusUnauthorized || string(msg.Body) != test.body || msg.Headers.Get("Content-Type") != test.contentType || !errors.Is(msg.Error, original) {
				t.Fatalf("response=%#v", msg)
			}
			if len(original.body) > 0 {
				original.body[0] = 'X'
			}
			if string(msg.Body) != test.body {
				t.Fatal("response aliases error body")
			}
			for _, loggingEnabled := range []bool{false, true} {
				recorder := httptest.NewRecorder()
				var ctx *gin.Context
				handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{RequestLog: loggingEnabled}, nil)
				engine := gin.New()
				engine.POST("/v1/messages", func(c *gin.Context) {
					ctx = c
					if err := handler.WriteErrorResponseWithError(c, msg); err != nil {
						t.Fatal(err)
					}
				})
				engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
				if recorder.Code != http.StatusUnauthorized || recorder.Body.String() != test.body || recorder.Header().Get("Content-Type") != test.contentType {
					t.Fatalf("wire status=%d body=%q headers=%v", recorder.Code, recorder.Body.String(), recorder.Header())
				}
				wantLog := ""
				if loggingEnabled {
					wantLog = test.body
				}
				if got := string(requestLogContextBytes(ctx, "API_RESPONSE")); got != wantLog {
					t.Fatalf("logged body=%q want=%q", got, wantLog)
				}
			}
			calls := 0
			tracker := &requestLifecycleTracker{ctx: context.Background(), host: &lifecycleTestHost{completeRequest: func(_ context.Context, completion pluginapi.RequestCompletion) {
				calls++
				if completion.Outcome != pluginapi.RequestCompletionFailed || completion.StatusCode != http.StatusUnauthorized {
					t.Fatalf("completion=%#v", completion)
				}
			}}}
			tracker.completeError(context.Background(), msg)
			tracker.completeError(context.Background(), msg)
			if calls != 1 {
				t.Fatalf("completion calls=%d", calls)
			}
		})
	}
	msg := executionErrorMessage(&lifecycleUpstreamError{body: []byte("private control message")})
	if msg.DirectResponse || len(msg.Body) != 0 {
		t.Fatalf("unmarked response=%#v", msg)
	}
}

func TestExecutionErrorMessageMapsContextStatuses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "canceled", err: context.Canceled, want: clienterror.StatusClientClosedRequest},
		{name: "deadline", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{
			name: "url error wraps canceled",
			err:  &url.Error{Op: "Post", URL: "https://example.com", Err: context.Canceled},
			want: clienterror.StatusClientClosedRequest,
		},
		{name: "plain error defaults to 500", err: errors.New("boom"), want: http.StatusInternalServerError},
		{
			name: "explicit status wins",
			err: &coreauth.Error{
				Code:       "rate_limited",
				Message:    "slow down",
				HTTPStatus: http.StatusTooManyRequests,
			},
			want: http.StatusTooManyRequests,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				msg := executionErrorMessage(test.err)
				if msg == nil {
					t.Fatal("executionErrorMessage() returned nil")
				}
				if msg.StatusCode != test.want {
					t.Fatalf("StatusCode = %d, want %d", msg.StatusCode, test.want)
				}
				// The constructor must retain the exact error value, not only an equivalent unwrap chain.
				// noinspection GoDirectComparisonOfErrors
				if msg.Error != test.err {
					t.Fatalf("Error = %v, want original %v", msg.Error, test.err)
				}
			},
		)
	}
}

func TestStatusFromErrorMapsContextStatuses(t *testing.T) {
	if got := statusFromError(context.Canceled); got != clienterror.StatusClientClosedRequest {
		t.Fatalf("statusFromError(canceled) = %d, want %d", got, clienterror.StatusClientClosedRequest)
	}
	if got := statusFromError(context.DeadlineExceeded); got != http.StatusGatewayTimeout {
		t.Fatalf("statusFromError(deadline) = %d, want %d", got, http.StatusGatewayTimeout)
	}
	if got := statusFromError(
		&url.Error{
			Op: "Post", URL: "https://example.com", Err: context.Canceled,
		},
	); got != clienterror.StatusClientClosedRequest {
		t.Fatalf("statusFromError(url canceled) = %d, want %d", got, clienterror.StatusClientClosedRequest)
	}
	if got := statusFromError(errors.New("boom")); got != 0 {
		t.Fatalf("statusFromError(plain) = %d, want 0", got)
	}
}

func TestWriteErrorResponseMapsContextStatuses(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "canceled", err: context.Canceled, want: clienterror.StatusClientClosedRequest},
		{name: "deadline", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

				handler := NewBaseAPIHandlers(nil, nil)
				handler.WriteErrorResponse(ctx, executionErrorMessage(test.err))

				if recorder.Code != test.want {
					t.Fatalf("status = %d, want %d", recorder.Code, test.want)
				}
			},
		)
	}
}

func TestPublicAuthSelectionErrorDoesNotDiscloseCause(t *testing.T) {
	for _, test := range []struct{ name, input, forbiddenRaw string }{
		{"multi word password", "password = correct horse battery staple", "battery staple"},
		{"multi word credentials", "credentials: alice secret", "alice secret"},
		{"unix customer path", "open /mnt/secrets/alice: permission denied", "alice"},
		{"cookie value", "Cookie: sessionid=COOKIESECRET", "COOKIESECRET"},
	} {
		t.Run(test.name, func(t *testing.T) {
			model := "isolated-selection-error-model"
			executor := &lifecycleCaptureExecutor{
				execute: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
					return coreexecutor.Response{}, coreauth.WithCause(
						&coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusServiceUnavailable},
						errors.New(test.input),
					)
				},
			}
			handler := newLifecycleHandler(t, model, executor)
			handler.AuthManager.SetRetryConfig(0, 0, 1)
			router := gin.New()
			router.POST("/v1/chat/completions", func(c *gin.Context) {
				_, _, errMsg := handler.ExecuteWithAuthManager(c.Request.Context(), "openai", model, []byte(fmt.Sprintf(`{"model":%q}`, model)), "")
				if errMsg == nil {
					t.Fatal("expected an exhausted selection error")
				}
				handler.WriteErrorResponse(c, errMsg)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
			if response.Code != http.StatusServiceUnavailable {
				t.Errorf("status=%d want 503", response.Code)
			}
			if strings.Contains(response.Body.String(), test.forbiddenRaw) {
				t.Errorf("public response disclosed synthetic sensitive text %q: %s", test.forbiddenRaw, response.Body.String())
			}
		})
	}
}

func TestAuthSelectionWithoutCredentialsReturns503(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			const model = "claude-sonnet-4-6"
			manager := coreauth.NewManager(nil, nil, nil)
			manager.SetRetryConfig(0, 0, 1)
			manager.RegisterExecutor(&lifecycleCaptureExecutor{provider: "claude"})
			registry.GetGlobalRegistry().RegisterClient("isolated-no-auth", "claude", []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("isolated-no-auth") })
			handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
			router := gin.New()
			router.POST("/v1/chat/completions", func(c *gin.Context) {
				var errMsg *ErrorMessage
				if streaming {
					data, _, errs := handler.ExecuteStreamWithAuthManager(c.Request.Context(), "openai", model, []byte(`{"model":"claude-sonnet-4-6","stream":true}`), "")
					if data != nil {
						for range data {
						}
					}
					for item := range errs {
						if item != nil {
							errMsg = item
						}
					}
				} else {
					_, _, errMsg = handler.ExecuteWithAuthManager(c.Request.Context(), "openai", model, []byte(`{"model":"claude-sonnet-4-6"}`), "")
				}
				if errMsg == nil {
					t.Fatal("expected no-auth error")
				}
				handler.WriteErrorResponse(c, errMsg)
			})
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
			if response.Code != http.StatusServiceUnavailable {
				t.Errorf("status=%d want 503; body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestAfterAuthPatchPreservesOriginalSessionInputs(t *testing.T) {
	for _, name := range []string{"clear-session", "original-request", "lcp-hierarchy", "path-body"} {
		t.Run(name, func(t *testing.T) {
			request := coreexecutor.Request{Model: "gpt-5.6-luna"}
			options := coreexecutor.Options{Headers: make(http.Header), Metadata: make(map[string]any)}
			var patch pluginapi.RequestInterceptResponse
			switch name {
			case "clear-session":
				options.Headers.Set("X-Session-ID", "initial-session")
				options.Metadata[coreexecutor.CanonicalSessionIDMetadataKey] = "session:initial-session"
				options.Metadata[coreexecutor.ParentSessionIDMetadataKey] = "session:initial-parent"
				patch.ClearHeaders = []string{"X-Session-ID"}
			case "original-request":
				options.OriginalRequest = []byte(`{"session_id":"important-session"}`)
				options.Metadata[coreexecutor.CanonicalSessionIDMetadataKey] = "session:important-session"
				patch.Headers = http.Header{"X-Trace-ID": {"trace-abc"}}
			case "lcp-hierarchy":
				request.Payload = []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
				options.Metadata[coreexecutor.LCPAffinitySessionIDMetadataKey] = "lcp:v1:child-fork-123"
				options.Metadata[coreexecutor.CanonicalSessionIDMetadataKey] = "lcp:v1:child-fork-123"
				options.Metadata[coreexecutor.ParentSessionIDMetadataKey] = "lcp:v1:parent-trunk-000"
				patch.Headers = http.Header{"X-Trace-ID": {"trace-xyz"}}
			case "path-body":
				request.Model = "gpt-image-2.5"
				request.Payload = []byte(`{"model":"gpt-image-2.5","prompt":"edit this"}`)
				options.Metadata[coreexecutor.RequestPathMetadataKey] = "/v1/images/edits"
				patch.Path = "/v1/images/generations"
				patch.Body = []byte(`{"model":"gpt-image-2.5","prompt":"rewritten for generations"}`)
			}
			calls := 0
			handler := &BaseAPIHandler{PluginHost: &lifecycleTestHost{
				interceptRequestAfterAuth: func(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
					calls++
					return patch
				},
			}}
			result := handler.applyRequestInterceptorsAfterAuth(context.Background(), coreexecutor.RequestAfterAuthInterceptRequest{
				Request: request, Options: options,
			}, nil, "")
			if calls != 1 || !result.Handled || result.Terminate {
				t.Fatalf("patch consumer not reached: calls=%d, result=%+v", calls, result)
			}
			switch name {
			case "clear-session":
				if len(result.Options.Headers) != 0 {
					t.Fatalf("headers not cleared: %v", result.Options.Headers)
				}
				if _, exists := result.Options.Metadata[coreexecutor.CanonicalSessionIDMetadataKey]; exists {
					t.Error("canonical session was not cleared from metadata after interceptor cleared headers")
				}
				if _, exists := result.Options.Metadata[coreexecutor.ParentSessionIDMetadataKey]; exists {
					t.Error("parent session was not cleared from metadata after interceptor cleared headers")
				}
			case "original-request":
				if result.Options.Metadata[coreexecutor.CanonicalSessionIDMetadataKey] != "session:important-session" {
					t.Fatalf("original request session changed: %+v", result.Options.Metadata)
				}
			case "lcp-hierarchy":
				if result.Options.Metadata[coreexecutor.CanonicalSessionIDMetadataKey] != "lcp:v1:child-fork-123" || result.Options.Metadata[coreexecutor.ParentSessionIDMetadataKey] != "lcp:v1:parent-trunk-000" {
					t.Fatalf("LCP hierarchy changed: %+v", result.Options.Metadata)
				}
			case "path-body":
				if string(result.Request.Payload) != `{"model":"gpt-image-2.5","prompt":"rewritten for generations"}` || result.Options.Metadata[coreexecutor.RequestPathMetadataKey] != "/v1/images/generations" {
					t.Fatalf("path/body rewrite lost: request=%+v, metadata=%+v", result.Request, result.Options.Metadata)
				}
			}
		})
	}
}

func TestAfterAuthSessionClearingReachesExecutor(t *testing.T) {
	for _, operation := range []string{"execute", "count", "stream"} {
		t.Run(operation, func(t *testing.T) {
			model := "audit-post-auth-clearing-" + operation
			called := 0
			capture := func(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, options coreexecutor.Options) (coreexecutor.Response, error) {
				called++
				metadata := logging.GetClientRequestMetadata(ctx)
				if options.Headers.Get("X-Session-ID") != "" {
					t.Error("cleared header reached executor")
				}
				if metadata.SessionID != "" || metadata.ParentSessionID != "" || options.Metadata[coreexecutor.CanonicalSessionIDMetadataKey] != nil {
					t.Errorf("cleared session still reached executor: session=%q parent=%q canonical=%v utility=%q", metadata.SessionID, metadata.ParentSessionID, options.Metadata[coreexecutor.CanonicalSessionIDMetadataKey], util.SessionIDFromContext(ctx))
				}
				return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
			}
			executor := &lifecycleCaptureExecutor{execute: capture, executeCount: capture,
				stream: func(ctx context.Context, auth *coreauth.Auth, request coreexecutor.Request, options coreexecutor.Options) (*coreexecutor.StreamResult, error) {
					_, _ = capture(ctx, auth, request, options)
					chunks := make(chan coreexecutor.StreamChunk, 1)
					chunks <- coreexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")}
					close(chunks)
					return &coreexecutor.StreamResult{Chunks: chunks}, nil
				},
			}
			handler := newLifecycleHandler(t, model, executor)
			handler.PluginHost = &lifecycleTestHost{interceptRequestAfterAuth: func(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
				return pluginapi.RequestInterceptResponse{ClearHeaders: []string{"X-Session-ID"}}
			}}
			ctx := context.Background()
			request := ModelExecutionRequest{EntryProtocol: "openai", Model: model, Body: []byte(`{"model":"` + model + `"}`), Headers: http.Header{"X-Session-ID": {"initial-session"}}}
			if operation == "stream" {
				request.Stream = true
				result, err := handler.ExecuteModelStream(ctx, request)
				if err != nil {
					t.Fatal(err)
				}
				for range result.Chunks {
				}
			} else if operation == "count" {
				if _, _, err := handler.executeNonStream(ctx, "openai", "openai", model, request.Body, "", false, false, request.Headers, nil, "", handler.AuthManager.ExecuteCount); err != nil {
					t.Fatal(err)
				}
			} else if _, err := handler.ExecuteModel(ctx, request); err != nil {
				t.Fatal(err)
			}
			if called != 1 {
				t.Fatalf("executor calls=%d, want 1", called)
			}
		})
	}
}

func TestAfterAuthSessionClearingPreservesCallerAndExecutionSessions(t *testing.T) {
	for _, operation := range []string{"execute", "count", "stream"} {
		for _, session := range []string{"inferred", "caller", "caller-empty", "execution"} {
			t.Run(operation+"/"+session, func(t *testing.T) {
				ctx := context.Background()
				wantUtility, wantCanonical := "", ""
				switch session {
				case "caller":
					ctx = util.WithSessionID(ctx, "caller-session")
					wantUtility = "caller-session"
				case "caller-empty":
					ctx = util.WithSessionID(ctx, "")
				case "execution":
					ctx = WithExecutionSessionID(ctx, "active-execution")
					wantUtility, wantCanonical = "execution:active-execution", "execution:active-execution"
				}
				calls := 0
				capture := func(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
					calls++
					metadata := logging.GetClientRequestMetadata(ctx)
					if metadata.SessionID != wantCanonical || metadata.ParentSessionID != "" || util.SessionIDFromContext(ctx) != wantUtility {
						t.Errorf("session=%q parent=%q utility=%q, want canonical=%q utility=%q", metadata.SessionID, metadata.ParentSessionID, util.SessionIDFromContext(ctx), wantCanonical, wantUtility)
					}
					if opts.Headers.Get("X-Session-ID") != "" {
						t.Error("cleared session header reached executor")
					}
					return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
				}
				executor := &lifecycleCaptureExecutor{execute: capture, executeCount: capture, stream: func(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (*coreexecutor.StreamResult, error) {
					_, _ = capture(ctx, auth, req, opts)
					chunks := make(chan coreexecutor.StreamChunk, 1)
					chunks <- coreexecutor.StreamChunk{Payload: []byte("data: [DONE]\n\n")}
					close(chunks)
					return &coreexecutor.StreamResult{Chunks: chunks}, nil
				}}
				model := "postauth-session-" + operation + "-" + session
				handler := newLifecycleHandler(t, model, executor)
				handler.PluginHost = &lifecycleTestHost{interceptRequestAfterAuth: func(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
					return pluginapi.RequestInterceptResponse{ClearHeaders: []string{"X-Session-ID"}}
				}}
				req := ModelExecutionRequest{EntryProtocol: "openai", Model: model, Body: []byte(`{"model":"` + model + `"}`), Headers: http.Header{"X-Session-ID": {"initial-session"}}}
				if operation == "stream" {
					req.Stream = true
					result, err := handler.ExecuteModelStream(ctx, req)
					if err != nil {
						t.Fatal(err)
					}
					for range result.Chunks {
					}
				} else if operation == "count" {
					if _, _, err := handler.executeNonStream(ctx, "openai", "openai", model, req.Body, "", false, false, req.Headers, nil, "", handler.AuthManager.ExecuteCount); err != nil {
						t.Fatal(err)
					}
				} else if _, err := handler.ExecuteModel(ctx, req); err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatalf("executor calls=%d, want 1", calls)
				}
			})
		}
	}
}

func TestAfterAuthBodyChangeRefreshesSessionMetadata(t *testing.T) {
	handler := &BaseAPIHandler{PluginHost: &lifecycleTestHost{interceptRequestAfterAuth: func(context.Context, pluginapi.RequestInterceptRequest) pluginapi.RequestInterceptResponse {
		return pluginapi.RequestInterceptResponse{Body: []byte(`{"session_id":"replacement","messages":[{"role":"user","content":"new request"}]}`)}
	}}}
	metadata := map[string]any{
		coreexecutor.CanonicalSessionIDMetadataKey:   "lcp:v1:old",
		coreexecutor.ParentSessionIDMetadataKey:      "lcp:v1:parent",
		coreexecutor.LCPAffinitySessionIDMetadataKey: "lcp:v1:old",
		coreexecutor.LCPFingerprintMetadataKey:       []string{"old"},
		coreexecutor.IsForkMetadataKey:               true,
		coreexecutor.IsCompactionMetadataKey:         true,
		coreexecutor.NodeKindMetadataKey:             "fork",
	}
	result := handler.applyRequestInterceptorsAfterAuth(context.Background(), coreexecutor.RequestAfterAuthInterceptRequest{
		Request: coreexecutor.Request{Payload: []byte(`{"messages":[{"role":"user","content":"old request"}]}`), Metadata: metadata},
		Options: coreexecutor.Options{Metadata: metadata},
	}, nil, "")
	for _, got := range []map[string]any{result.Request.Metadata, result.Options.Metadata} {
		if got[coreexecutor.CanonicalSessionIDMetadataKey] != "session:replacement" {
			t.Fatalf("replacement session missing: %+v", got)
		}
		for _, key := range []string{coreexecutor.ParentSessionIDMetadataKey, coreexecutor.LCPAffinitySessionIDMetadataKey, coreexecutor.LCPFingerprintMetadataKey, coreexecutor.IsForkMetadataKey, coreexecutor.IsCompactionMetadataKey, coreexecutor.NodeKindMetadataKey} {
			if _, exists := got[key]; exists {
				t.Errorf("stale metadata %q retained: %+v", key, got)
			}
		}
	}
	if metadata[coreexecutor.CanonicalSessionIDMetadataKey] != "lcp:v1:old" {
		t.Fatal("caller metadata changed")
	}
}

func TestGetContextWithCancelPreservesFailureCause(t *testing.T) {
	for _, failure := range []error{io.ErrClosedPipe, io.ErrShortWrite, context.Canceled, nil} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
			ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			ctx, cancel := handler.GetContextWithCancel(nil, ginCtx, context.Background())
			cancel(failure)
			want := failure
			if want == nil {
				want = context.Canceled
			}
			if ctx.Err() != context.Canceled || !errors.Is(context.Cause(ctx), want) {
				t.Fatalf("cancellation = %v, cause = %v, want %v", ctx.Err(), context.Cause(ctx), want)
			}
			cancel()
			if !errors.Is(context.Cause(ctx), want) {
				t.Fatalf("cleanup changed cause to %v", context.Cause(ctx))
			}
		})
	}
}

func TestHandlerLifecyclePreservesUpstreamFailureAfterCleanup(t *testing.T) {
	upstreamFailure := errors.New("upstream stream read failed")
	source := make(chan coreexecutor.StreamChunk, 2)
	source <- coreexecutor.StreamChunk{Payload: []byte(`{"chunk":true}`)}
	source <- coreexecutor.StreamChunk{Err: upstreamFailure}
	t.Cleanup(func() { close(source) })
	executor := &lifecycleCaptureExecutor{
		stream: func(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
			return &coreexecutor.StreamResult{Chunks: source}, nil
		},
	}
	handler := newLifecycleHandler(t, "handler-upstream-error-cleanup", executor)
	completions := make(chan pluginapi.RequestCompletion, 2)
	handler.SetPluginHost(&lifecycleTestHost{
		completeRequest: func(_ context.Context, completion pluginapi.RequestCompletion) {
			completions <- completion
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	data, _, errs := handler.ExecuteStreamChunksWithAuthManager(ctx, "openai", "handler-upstream-error-cleanup", []byte(`{"stream":true}`), "")
	select {
	case chunk := <-data:
		if chunk.Acknowledge == nil {
			t.Fatal("missing payload acknowledgement")
		}
		chunk.Acknowledge(nil)
	case <-time.After(time.Second):
		t.Fatal("missing initial payload")
	}
	select {
	case failure := <-errs:
		if failure == nil || !errors.Is(failure.Error, upstreamFailure) {
			t.Fatalf("upstream failure = %#v", failure)
		}
		cancel()
		if failure.Acknowledge != nil {
			failure.Acknowledge(nil)
		}
	case <-time.After(time.Second):
		t.Fatal("missing upstream failure")
	}
	select {
	case completion := <-completions:
		if completion.Outcome != pluginapi.RequestCompletionFailed || completion.Error != upstreamFailure.Error() || completion.StatusCode != http.StatusInternalServerError {
			t.Fatalf("cleanup replaced upstream failure: %#v", completion)
		}
	case <-time.After(time.Second):
		t.Fatal("missing failure completion")
	}
	select {
	case completion := <-completions:
		t.Fatalf("duplicate completion: %#v", completion)
	default:
	}
}
