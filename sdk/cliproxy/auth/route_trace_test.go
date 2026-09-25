package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type routeTraceTestError struct {
	status     int
	retryAfter time.Duration
	message    string
}

func (e routeTraceTestError) Error() string {
	return e.message
}

func (e routeTraceTestError) StatusCode() int {
	return e.status
}

func (e routeTraceTestError) RetryAfter() *time.Duration {
	if e.retryAfter <= 0 {
		return nil
	}
	retryAfter := e.retryAfter
	return &retryAfter
}

type routeTraceIsPanicError struct{}

func (routeTraceIsPanicError) Error() string {
	return "trace-only error"
}

func (routeTraceIsPanicError) Is(_ error) bool {
	panic("trace-only Is panic")
}

type routeTraceTestExecutor struct {
	provider string

	executeFn            func(*Auth) (cliproxyexecutor.Response, error)
	executeWithContextFn func(context.Context, *Auth) (cliproxyexecutor.Response, error)
	streamFn             func(*Auth) (*cliproxyexecutor.StreamResult, error)
	prepareFn            func(*Auth) (*Auth, error)

	mu           sync.Mutex
	executeCalls []string
	streamCalls  []string
	prepareCalls []string
}

func (e *routeTraceTestExecutor) Identifier() string {
	return e.provider
}

func (e *routeTraceTestExecutor) Execute(
	ctx context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeCalls = append(e.executeCalls, auth.ID)
	e.mu.Unlock()
	if e.executeWithContextFn != nil {
		return e.executeWithContextFn(ctx, auth)
	}
	if e.executeFn != nil {
		return e.executeFn(auth)
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *routeTraceTestExecutor) ExecuteStream(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.streamCalls = append(e.streamCalls, auth.ID)
	e.mu.Unlock()
	if e.streamFn != nil {
		return e.streamFn(auth)
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(auth.ID)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *routeTraceTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *routeTraceTestExecutor) CountTokens(
	_ context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("unexpected CountTokens call")
}

func (e *routeTraceTestExecutor) HttpRequest(
	_ context.Context,
	_ *Auth,
	_ *http.Request,
) (*http.Response, error) {
	return nil, errors.New("unexpected HttpRequest call")
}

func (e *routeTraceTestExecutor) ShouldPrepareRequestAuth(*Auth) bool {
	return e.prepareFn != nil
}

func (e *routeTraceTestExecutor) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	e.mu.Lock()
	e.prepareCalls = append(e.prepareCalls, auth.ID)
	e.mu.Unlock()
	return e.prepareFn(auth)
}

func (e *routeTraceTestExecutor) ExecuteCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.executeCalls...)
}

func (e *routeTraceTestExecutor) StreamCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.streamCalls...)
}

func (e *routeTraceTestExecutor) PrepareCalls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.prepareCalls...)
}

type routeTraceTestRecorder struct {
	mu     sync.Mutex
	events []cliproxyexecutor.RouteTraceEvent
}

func (r *routeTraceTestRecorder) Add(event cliproxyexecutor.RouteTraceEvent) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *routeTraceTestRecorder) Events() []cliproxyexecutor.RouteTraceEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]cliproxyexecutor.RouteTraceEvent(nil), r.events...)
}

type routeTraceMetadataSelector struct {
	metadata map[string]any
}

func (s *routeTraceMetadataSelector) Pick(
	_ context.Context,
	_, _ string,
	opts cliproxyexecutor.Options,
	auths []*Auth,
) (*Auth, error) {
	s.metadata = cloneSchedulerAnyMap(opts.Metadata)
	if len(auths) == 0 {
		return nil, nil
	}
	return auths[0], nil
}

type routeTraceMetadataScheduler struct {
	metadata map[string]any
}

func (s *routeTraceMetadataScheduler) PickAuth(
	_ context.Context,
	req pluginapi.SchedulerPickRequest,
) (pluginapi.SchedulerPickResponse, bool, error) {
	s.metadata = cloneSchedulerAnyMap(req.Options.Metadata)
	if len(req.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{}, false, nil
	}
	return pluginapi.SchedulerPickResponse{
		AuthID:  req.Candidates[0].ID,
		Handled: true,
	}, true, nil
}

type routeTraceMetadataExecutor struct {
	*routeTraceTestExecutor
	executeMetadata map[string]any
	streamMetadata  map[string]any
	countMetadata   map[string]any
}

func (e *routeTraceMetadataExecutor) Execute(
	ctx context.Context,
	auth *Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.executeMetadata = cloneSchedulerAnyMap(opts.Metadata)
	return e.routeTraceTestExecutor.Execute(ctx, auth, req, opts)
}

func (e *routeTraceMetadataExecutor) ExecuteStream(
	ctx context.Context,
	auth *Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.streamMetadata = cloneSchedulerAnyMap(opts.Metadata)
	return e.routeTraceTestExecutor.ExecuteStream(ctx, auth, req, opts)
}

func (e *routeTraceMetadataExecutor) CountTokens(
	_ context.Context,
	_ *Auth,
	_ cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.countMetadata = cloneSchedulerAnyMap(opts.Metadata)
	return cliproxyexecutor.Response{Payload: []byte("count-success")}, nil
}

func assertNoRouteTraceMetadata(t *testing.T, metadata map[string]any) {
	t.Helper()
	if got := metadata["business_key"]; got != "business_value" {
		t.Fatalf("business metadata = %#v", got)
	}
	for key := range metadata {
		if key == cliproxyexecutor.RouteTraceCallbackMetadataKey ||
			key == cliproxyexecutor.RouteTraceTruncatedCallbackMetadataKey ||
			strings.HasPrefix(key, "__route_trace_") {
			t.Fatalf("route trace metadata leaked through key %q", key)
		}
	}
}

func routeTraceTestOptions(callback func(cliproxyexecutor.RouteTraceEvent)) cliproxyexecutor.Options {
	metadata := make(map[string]any)
	if callback != nil {
		metadata[cliproxyexecutor.RouteTraceCallbackMetadataKey] = callback
	}
	return cliproxyexecutor.Options{Metadata: metadata}
}

func registerRouteTraceTestAuths(
	t *testing.T,
	manager *Manager,
	provider,
	model string,
	auths ...*Auth,
) {
	t.Helper()
	for _, auth := range auths {
		if auth.Status == "" {
			auth.Status = StatusActive
		}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(
			auth.ID,
			provider,
			[]*registry.ModelInfo{{ID: model}},
		)
		authID := auth.ID
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	}
}

func routeTraceTestAuthIndex(t *testing.T, manager *Manager, authID string) string {
	t.Helper()
	auth, ok := manager.GetByID(authID)
	if !ok || auth == nil || auth.Index == "" {
		t.Fatalf("registered auth %q has no runtime index", authID)
	}
	return auth.Index
}

func requireRouteTraceActionOrder(
	t *testing.T,
	events []cliproxyexecutor.RouteTraceEvent,
	actions ...string,
) {
	t.Helper()
	position := 0
	for _, event := range events {
		if position < len(actions) && event.Action == actions[position] {
			position++
		}
	}
	if position != len(actions) {
		got := make([]string, 0, len(events))
		for _, event := range events {
			got = append(got, event.Action)
		}
		t.Fatalf("route trace actions = %v, missing ordered suffix %v", got, actions[position:])
	}
}

func findRouteTraceEvent(
	t *testing.T,
	events []cliproxyexecutor.RouteTraceEvent,
	action string,
	attempt int,
) cliproxyexecutor.RouteTraceEvent {
	t.Helper()
	for _, event := range events {
		if event.Action == action && event.Attempt == attempt {
			return event
		}
	}
	t.Fatalf("route trace event action=%q attempt=%d not found", action, attempt)
	return cliproxyexecutor.RouteTraceEvent{}
}

func TestManagerRouteTraceMetadataIsHiddenFromExtensions(t *testing.T) {
	newMetadata := func(model string) map[string]any {
		return map[string]any{
			"business_key": "business_value",
			cliproxyexecutor.RequestedModelMetadataKey:              model,
			cliproxyexecutor.RouteTraceCallbackMetadataKey:          func(cliproxyexecutor.RouteTraceEvent) {},
			cliproxyexecutor.RouteTraceTruncatedCallbackMetadataKey: func(int) {},
			"__route_trace_private":                                 "private",
		}
	}

	t.Run(
		"selector_executor_interceptor", func(t *testing.T) {
			const (
				provider = "route-trace-extension-selector-provider"
				model    = "route-trace-extension-selector-model"
			)
			selector := &routeTraceMetadataSelector{}
			executor := &routeTraceMetadataExecutor{
				routeTraceTestExecutor: &routeTraceTestExecutor{provider: provider},
			}
			manager := NewManager(nil, selector, nil)
			manager.SetRetryConfig(0, 0, 0)
			manager.RegisterExecutor(executor)
			registerRouteTraceTestAuths(
				t,
				manager,
				provider,
				model,
				&Auth{ID: "route-trace-extension-selector-auth", Provider: provider},
			)
			var interceptorMetadata map[string]any
			opts := cliproxyexecutor.Options{
				Metadata: newMetadata(model),
				RequestAfterAuthInterceptor: func(
					_ context.Context,
					req cliproxyexecutor.RequestAfterAuthInterceptRequest,
				) cliproxyexecutor.RequestAfterAuthInterceptResponse {
					interceptorMetadata = cloneSchedulerAnyMap(req.Options.Metadata)
					return cliproxyexecutor.RequestAfterAuthInterceptResponse{}
				},
			}
			if _, errExecute := manager.Execute(
				context.Background(),
				[]string{provider},
				cliproxyexecutor.Request{Model: model},
				opts,
			); errExecute != nil {
				t.Fatalf("Execute() error = %v", errExecute)
			}
			assertNoRouteTraceMetadata(t, selector.metadata)
			assertNoRouteTraceMetadata(t, executor.executeMetadata)
			assertNoRouteTraceMetadata(t, interceptorMetadata)

			if _, errCount := manager.ExecuteCount(
				context.Background(),
				[]string{provider},
				cliproxyexecutor.Request{Model: model},
				cliproxyexecutor.Options{Metadata: newMetadata(model)},
			); errCount != nil {
				t.Fatalf("ExecuteCount() error = %v", errCount)
			}
			assertNoRouteTraceMetadata(t, executor.countMetadata)

			streamResult, errStream := manager.ExecuteStream(
				context.Background(),
				[]string{provider},
				cliproxyexecutor.Request{Model: model},
				cliproxyexecutor.Options{Stream: true, Metadata: newMetadata(model)},
			)
			if errStream != nil {
				t.Fatalf("ExecuteStream() error = %v", errStream)
			}
			if streamResult == nil {
				t.Fatal("ExecuteStream() result = nil")
			}
			for range streamResult.Chunks {
			}
			assertNoRouteTraceMetadata(t, executor.streamMetadata)
		},
	)

	t.Run(
		"plugin_scheduler", func(t *testing.T) {
			const (
				provider = "route-trace-extension-scheduler-provider"
				model    = "route-trace-extension-scheduler-model"
			)
			scheduler := &routeTraceMetadataScheduler{}
			executor := &routeTraceMetadataExecutor{
				routeTraceTestExecutor: &routeTraceTestExecutor{provider: provider},
			}
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			manager.SetRetryConfig(0, 0, 0)
			manager.SetPluginScheduler(scheduler)
			manager.RegisterExecutor(executor)
			registerRouteTraceTestAuths(
				t,
				manager,
				provider,
				model,
				&Auth{ID: "route-trace-extension-scheduler-auth", Provider: provider},
			)
			if _, errExecute := manager.Execute(
				context.Background(),
				[]string{provider},
				cliproxyexecutor.Request{Model: model},
				cliproxyexecutor.Options{Metadata: newMetadata(model)},
			); errExecute != nil {
				t.Fatalf("Execute() error = %v", errExecute)
			}
			assertNoRouteTraceMetadata(t, scheduler.metadata)
			assertNoRouteTraceMetadata(t, executor.executeMetadata)
		},
	)
}

func TestRouteTraceProducerBudgetReservesTerminalEvents(t *testing.T) {
	tests := []struct {
		name          string
		publishDetail func(cliproxyexecutor.Options)
		wantTruncated int
	}{
		{
			name: "non_terminal_budget",
			publishDetail: func(opts cliproxyexecutor.Options) {
				for range 300 {
					publishRouteTraceEvent(
						opts.Metadata, cliproxyexecutor.RouteTraceEvent{
							Action: "attempt.prepare_started",
						},
					)
				}
			},
			wantTruncated: 52,
		},
		{
			name: "candidate_exclusion_budget",
			publishDetail: func(opts cliproxyexecutor.Options) {
				for range 200 {
					publishRouteTraceEvent(
						opts.Metadata, cliproxyexecutor.RouteTraceEvent{
							Action: "candidate.excluded",
						},
					)
				}
				for range 120 {
					publishRouteTraceEvent(
						opts.Metadata, cliproxyexecutor.RouteTraceEvent{
							Action: "attempt.prepare_started",
						},
					)
				}
			},
			wantTruncated: 72,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				recorder := &routeTraceTestRecorder{}
				truncated := 0
				opts := ensureRouteTraceState(
					cliproxyexecutor.Options{
						Metadata: map[string]any{
							cliproxyexecutor.RouteTraceCallbackMetadataKey: recorder.Add,
							cliproxyexecutor.RouteTraceTruncatedCallbackMetadataKey: func(count int) {
								truncated += count
							},
						},
					},
				)
				test.publishDetail(opts)
				for range 8 {
					publishRouteTraceEvent(
						opts.Metadata, cliproxyexecutor.RouteTraceEvent{
							Action:      "attempt.completed",
							FinalTarget: true,
						},
					)
				}
				publishRouteTraceEvent(
					opts.Metadata, cliproxyexecutor.RouteTraceEvent{
						Action:      "attempt.completed",
						FinalTarget: true,
					},
				)

				events := recorder.Events()
				if len(events) != cliproxyexecutor.MaxRouteTraceEvents {
					t.Fatalf("events = %d, want %d", len(events), cliproxyexecutor.MaxRouteTraceEvents)
				}
				for _, event := range events[len(events)-8:] {
					if event.Action != "attempt.completed" || !event.FinalTarget {
						t.Fatalf("terminal reserve contains non-terminal event: %+v", event)
					}
				}
				if truncated != test.wantTruncated+1 {
					t.Fatalf("truncated = %d, want %d", truncated, test.wantTruncated+1)
				}
			},
		)
	}
}

func TestRouteTraceTruncatedCallbackPanicIsIsolated(t *testing.T) {
	opts := ensureRouteTraceState(
		cliproxyexecutor.Options{
			Metadata: map[string]any{
				cliproxyexecutor.RouteTraceCallbackMetadataKey: func(cliproxyexecutor.RouteTraceEvent) {},
				cliproxyexecutor.RouteTraceTruncatedCallbackMetadataKey: func(int) {
					panic("truncated callback panic")
				},
			},
		},
	)
	for index := 0; index <= maxRouteTraceNonTerminalEvents; index++ {
		publishRouteTraceEvent(
			opts.Metadata, cliproxyexecutor.RouteTraceEvent{
				Action: "attempt.prepare_started",
			},
		)
	}
}

func TestRouteTraceErrorInspectionPanicIsIsolated(t *testing.T) {
	code, status, retryAfterMS := routeTraceError(nil, routeTraceIsPanicError{}, "execution_failed")
	if code != "" || status != 0 || retryAfterMS != 0 {
		t.Fatalf("disabled trace error fields = %q/%d/%d", code, status, retryAfterMS)
	}

	metadata := map[string]any{
		cliproxyexecutor.RouteTraceCallbackMetadataKey: func(cliproxyexecutor.RouteTraceEvent) {},
	}
	code, status, retryAfterMS = routeTraceError(metadata, routeTraceIsPanicError{}, "execution_failed")
	if code != "execution_failed" || status != 0 || retryAfterMS != 0 {
		t.Fatalf("panic-isolated trace error fields = %q/%d/%d", code, status, retryAfterMS)
	}
}

func TestManagerRouteTraceSuccessfulExecution(t *testing.T) {
	const (
		provider = "route-trace-success-provider"
		model    = "route-trace-success-model"
	)
	executor := &routeTraceTestExecutor{provider: provider}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	registerRouteTraceTestAuths(
		t,
		manager,
		provider,
		model,
		&Auth{ID: "route-trace-success-auth", Index: "success-index", Provider: provider},
	)
	recorder := &routeTraceTestRecorder{}

	response, errExecute := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		routeTraceTestOptions(recorder.Add),
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(response.Payload); got != "route-trace-success-auth" {
		t.Fatalf("response payload = %q", got)
	}
	if got := executor.ExecuteCalls(); !reflect.DeepEqual(got, []string{"route-trace-success-auth"}) {
		t.Fatalf("execute calls = %v", got)
	}

	events := recorder.Events()
	requireRouteTraceActionOrder(
		t,
		events,
		"candidate.selected",
		"route.selected",
		"attempt.prepare_started",
		"attempt.prepare_completed",
		"attempt.execute_started",
		"attempt.completed",
		"result.recorded",
	)
	completed := findRouteTraceEvent(t, events, "attempt.completed", 1)
	if completed.PreviousAttempt != 0 || completed.Provider != provider || completed.Model != model {
		t.Fatalf("completed event = %+v", completed)
	}
	if completed.AuthIndex != routeTraceTestAuthIndex(t, manager, "route-trace-success-auth") ||
		completed.ResultCode != "success" {
		t.Fatalf("completed event = %+v", completed)
	}
	if completed.HTTPStatus != http.StatusOK || !completed.FinalTarget {
		t.Fatalf("completed event = %+v", completed)
	}
}

func TestManagerRouteTraceCancellationRecordsTerminalAttempt(t *testing.T) {
	const (
		provider = "route-trace-cancel-provider"
		model    = "route-trace-cancel-model"
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	executor := &routeTraceTestExecutor{
		provider: provider,
		executeWithContextFn: func(execCtx context.Context, _ *Auth) (cliproxyexecutor.Response, error) {
			cancel()
			<-execCtx.Done()
			return cliproxyexecutor.Response{}, execCtx.Err()
		},
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	registerRouteTraceTestAuths(
		t,
		manager,
		provider,
		model,
		&Auth{ID: "route-trace-cancel-auth", Provider: provider},
	)
	recorder := &routeTraceTestRecorder{}

	_, errExecute := manager.Execute(
		ctx,
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		routeTraceTestOptions(recorder.Add),
	)
	if !errors.Is(errExecute, context.Canceled) {
		t.Fatalf("Execute() error = %v, want context canceled", errExecute)
	}
	failed := findRouteTraceEvent(t, recorder.Events(), "attempt.failed", 1)
	if failed.Reason != "request_canceled" || failed.ResultCode != "context_canceled" || !failed.FinalTarget {
		t.Fatalf("canceled attempt event = %+v", failed)
	}
}

func TestManagerRouteTraceRetryAfterAndAttemptOrder(t *testing.T) {
	const (
		provider = "route-trace-retry-provider"
		model    = "route-trace-retry-model"
	)
	executor := &routeTraceTestExecutor{
		provider: provider,
		executeFn: func(auth *Auth) (cliproxyexecutor.Response, error) {
			if auth.ID == "route-trace-retry-a" {
				return cliproxyexecutor.Response{}, routeTraceTestError{
					status:     http.StatusTooManyRequests,
					retryAfter: 1500 * time.Millisecond,
					message:    "private upstream detail",
				}
			}
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	registerRouteTraceTestAuths(
		t,
		manager,
		provider,
		model,
		&Auth{ID: "route-trace-retry-a", Index: "retry-index-a", Provider: provider},
		&Auth{ID: "route-trace-retry-b", Index: "retry-index-b", Provider: provider},
	)
	recorder := &routeTraceTestRecorder{}

	response, errExecute := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		routeTraceTestOptions(recorder.Add),
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(response.Payload); got != "route-trace-retry-b" {
		t.Fatalf("response payload = %q", got)
	}
	if got := executor.ExecuteCalls(); !reflect.DeepEqual(
		got,
		[]string{"route-trace-retry-a", "route-trace-retry-b"},
	) {
		t.Fatalf("execute calls = %v", got)
	}

	events := recorder.Events()
	failed := findRouteTraceEvent(t, events, "attempt.failed", 1)
	if failed.AuthIndex != routeTraceTestAuthIndex(t, manager, "route-trace-retry-a") ||
		failed.PreviousAttempt != 0 {
		t.Fatalf("failed event = %+v", failed)
	}
	if failed.ResultCode != "rate_limited" || failed.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("failed event = %+v", failed)
	}
	if failed.RetryAfterMS != 1500 {
		t.Fatalf("failed retry_after_ms = %d", failed.RetryAfterMS)
	}
	completed := findRouteTraceEvent(t, events, "attempt.completed", 2)
	if completed.AuthIndex != routeTraceTestAuthIndex(t, manager, "route-trace-retry-b") ||
		completed.PreviousAttempt != 1 {
		t.Fatalf("completed event = %+v", completed)
	}
}

func TestManagerRouteTracePrepareFailureFallsBack(t *testing.T) {
	const (
		provider = "route-trace-prepare-provider"
		model    = "route-trace-prepare-model"
	)
	executor := &routeTraceTestExecutor{
		provider: provider,
		prepareFn: func(auth *Auth) (*Auth, error) {
			if auth.ID == "route-trace-prepare-a" {
				return nil, errors.New("private prepare detail")
			}
			return auth.Clone(), nil
		},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	registerRouteTraceTestAuths(
		t,
		manager,
		provider,
		model,
		&Auth{ID: "route-trace-prepare-a", Index: "prepare-index-a", Provider: provider},
		&Auth{ID: "route-trace-prepare-b", Index: "prepare-index-b", Provider: provider},
	)
	recorder := &routeTraceTestRecorder{}

	response, errExecute := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		routeTraceTestOptions(recorder.Add),
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(response.Payload); got != "route-trace-prepare-b" {
		t.Fatalf("response payload = %q", got)
	}
	if got := executor.PrepareCalls(); !reflect.DeepEqual(
		got,
		[]string{"route-trace-prepare-a", "route-trace-prepare-b"},
	) {
		t.Fatalf("prepare calls = %v", got)
	}
	if got := executor.ExecuteCalls(); !reflect.DeepEqual(got, []string{"route-trace-prepare-b"}) {
		t.Fatalf("execute calls = %v", got)
	}

	events := recorder.Events()
	prepareFailed := findRouteTraceEvent(t, events, "attempt.prepare_failed", 1)
	if prepareFailed.AuthIndex != routeTraceTestAuthIndex(t, manager, "route-trace-prepare-a") ||
		prepareFailed.ResultCode != "prepare_failed" {
		t.Fatalf("prepare failed event = %+v", prepareFailed)
	}
	completed := findRouteTraceEvent(t, events, "attempt.completed", 2)
	if completed.AuthIndex != routeTraceTestAuthIndex(t, manager, "route-trace-prepare-b") ||
		completed.PreviousAttempt != 1 {
		t.Fatalf("completed event = %+v", completed)
	}
}

type routeTraceEquivalenceResult struct {
	response               cliproxyexecutor.Response
	errorType              string
	errorText              string
	status                 int
	retryAfterMS           int64
	executeCalls           []string
	selectedAuth           any
	selectedIndex          any
	pinnedAuth             any
	selectedAuthCallbacks  []string
	selectedIndexCallbacks []string
}

func runRouteTraceCallbackEquivalenceScenario(
	t *testing.T,
	mode string,
	succeed,
	pinSelected bool,
) routeTraceEquivalenceResult {
	t.Helper()
	const (
		provider = "route-trace-equivalence-provider"
		model    = "route-trace-equivalence-model"
		authA    = "route-trace-equivalence-a"
		authB    = "route-trace-equivalence-b"
	)
	executor := &routeTraceTestExecutor{
		provider: provider,
		executeFn: func(auth *Auth) (cliproxyexecutor.Response, error) {
			if auth.ID == authA {
				return cliproxyexecutor.Response{}, routeTraceTestError{
					status:     http.StatusServiceUnavailable,
					retryAfter: 25 * time.Millisecond,
					message:    "first credential failed",
				}
			}
			if succeed {
				return cliproxyexecutor.Response{
					Payload:  []byte("equivalent-success"),
					Metadata: map[string]any{"source": "test"},
					Headers:  http.Header{"X-Test": []string{"equivalent"}},
				}, nil
			}
			return cliproxyexecutor.Response{}, routeTraceTestError{
				status:     http.StatusBadGateway,
				retryAfter: 50 * time.Millisecond,
				message:    "second credential failed",
			}
		},
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	registerRouteTraceTestAuths(
		t,
		manager,
		provider,
		model,
		&Auth{ID: authA, Provider: provider},
		&Auth{ID: authB, Provider: provider},
	)

	var selectedAuthCallbacks []string
	var selectedIndexCallbacks []string
	metadata := map[string]any{
		cliproxyexecutor.RequestedModelMetadataKey: model,
		cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
			selectedAuthCallbacks = append(selectedAuthCallbacks, authID)
		},
		cliproxyexecutor.SelectedAuthIndexCallbackMetadataKey: func(authIndex string) {
			selectedIndexCallbacks = append(selectedIndexCallbacks, authIndex)
		},
	}
	if pinSelected {
		metadata[cliproxyexecutor.PinSelectedAuthMetadataKey] = true
	}
	var saturatedTrace *internallogging.RouteTrace
	switch mode {
	case "none":
	case "noop":
		metadata[cliproxyexecutor.RouteTraceCallbackMetadataKey] = func(cliproxyexecutor.RouteTraceEvent) {}
	case "panic":
		metadata[cliproxyexecutor.RouteTraceCallbackMetadataKey] = func(cliproxyexecutor.RouteTraceEvent) {
			panic("callback panic")
		}
	case "saturated":
		saturatedTrace = internallogging.NewRouteTrace()
		for range cliproxyexecutor.MaxRouteTraceEvents {
			saturatedTrace.Add(cliproxyexecutor.RouteTraceEvent{Action: "candidate.excluded"})
		}
		metadata[cliproxyexecutor.RouteTraceCallbackMetadataKey] = saturatedTrace.Callback()
		metadata[cliproxyexecutor.RouteTraceTruncatedCallbackMetadataKey] = saturatedTrace.TruncatedCallback()
	default:
		t.Fatalf("unknown callback mode %q", mode)
	}
	opts := cliproxyexecutor.Options{Metadata: metadata}
	response, errExecute := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		opts,
	)
	expectedCalls := []string{authA}
	if got := executor.ExecuteCalls(); !reflect.DeepEqual(got, expectedCalls) {
		t.Fatalf("execute calls = %v, want %v", got, expectedCalls)
	}
	if saturatedTrace != nil && saturatedTrace.Snapshot().TruncatedEvents == 0 {
		t.Fatal("saturated collector did not record omitted events")
	}

	result := routeTraceEquivalenceResult{
		response:               response,
		executeCalls:           executor.ExecuteCalls(),
		selectedAuth:           metadata[cliproxyexecutor.SelectedAuthMetadataKey],
		selectedIndex:          metadata[cliproxyexecutor.SelectedAuthIndexMetadataKey],
		pinnedAuth:             metadata[cliproxyexecutor.PinnedAuthMetadataKey],
		selectedAuthCallbacks:  selectedAuthCallbacks,
		selectedIndexCallbacks: selectedIndexCallbacks,
	}
	if errExecute != nil {
		result.errorType = fmt.Sprintf("%T", errExecute)
		result.errorText = errExecute.Error()
		result.status = statusCodeFromError(errExecute)
		if retryAfter := retryAfterFromError(errExecute); retryAfter != nil {
			result.retryAfterMS = retryAfter.Milliseconds()
		}
	}
	return result
}

func TestManagerRouteTraceCallbackDoesNotChangeExecution(t *testing.T) {
	tests := []struct {
		name        string
		succeed     bool
		pinSelected bool
	}{
		{name: "fallback_success", succeed: true},
		{name: "fallback_failure"},
		{name: "pin_selected", succeed: true, pinSelected: true},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				var baseline routeTraceEquivalenceResult
				for index, mode := range []string{"none", "noop", "panic", "saturated"} {
					t.Run(
						mode, func(t *testing.T) {
							got := runRouteTraceCallbackEquivalenceScenario(t, mode, test.succeed, test.pinSelected)
							if index == 0 {
								baseline = got
								return
							}
							if !reflect.DeepEqual(got, baseline) {
								t.Fatalf(
									"execution differs from no-trace baseline:\n got: %#v\nwant: %#v", got, baseline,
								)
							}
						},
					)
				}
			},
		)
	}
}

type routeTraceStreamChunkSnapshot struct {
	payload      string
	errorType    string
	errorText    string
	status       int
	retryAfterMS int64
}

type routeTraceStreamEquivalenceResult struct {
	headers                http.Header
	chunks                 []routeTraceStreamChunkSnapshot
	streamErrorType        string
	streamErrorText        string
	streamCalls            []string
	selectedAuth           any
	selectedIndex          any
	selectedAuthCallbacks  []string
	selectedIndexCallbacks []string
}

type routeTraceStreamEvents struct {
	beforeConsume []cliproxyexecutor.RouteTraceEvent
	afterConsume  []cliproxyexecutor.RouteTraceEvent
}

func runRouteTraceStreamEquivalenceScenario(
	t *testing.T,
	mode string,
	lateFailure bool,
) (routeTraceStreamEquivalenceResult, routeTraceStreamEvents) {
	t.Helper()
	const (
		provider = "route-trace-stream-equivalence-provider"
		model    = "route-trace-stream-equivalence-model"
		authA    = "route-trace-stream-equivalence-a"
		authB    = "route-trace-stream-equivalence-b"
	)
	executor := &routeTraceTestExecutor{
		provider: provider,
		streamFn: func(auth *Auth) (*cliproxyexecutor.StreamResult, error) {
			chunks := make(chan cliproxyexecutor.StreamChunk, 3)
			if auth.ID != authA {
				chunks <- cliproxyexecutor.StreamChunk{Err: errors.New("unexpected credential fallback")}
			} else {
				chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("first-payload")}
				if lateFailure {
					chunks <- cliproxyexecutor.StreamChunk{
						Err: routeTraceTestError{
							status:     http.StatusBadGateway,
							retryAfter: 75 * time.Millisecond,
							message:    "late stream failed",
						},
					}
				} else {
					chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("second-payload")}
				}
			}
			close(chunks)
			return &cliproxyexecutor.StreamResult{
				Headers: http.Header{"X-Test": []string{"stream-equivalent"}},
				Chunks:  chunks,
			}, nil
		},
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	registerRouteTraceTestAuths(
		t,
		manager,
		provider,
		model,
		&Auth{ID: authA, Provider: provider},
		&Auth{ID: authB, Provider: provider},
	)

	var selectedAuthCallbacks []string
	var selectedIndexCallbacks []string
	metadata := map[string]any{
		cliproxyexecutor.RequestedModelMetadataKey: model,
		cliproxyexecutor.SelectedAuthCallbackMetadataKey: func(authID string) {
			selectedAuthCallbacks = append(selectedAuthCallbacks, authID)
		},
		cliproxyexecutor.SelectedAuthIndexCallbackMetadataKey: func(authIndex string) {
			selectedIndexCallbacks = append(selectedIndexCallbacks, authIndex)
		},
	}
	var recorder *routeTraceTestRecorder
	var saturatedTrace *internallogging.RouteTrace
	switch mode {
	case "none":
	case "noop":
		metadata[cliproxyexecutor.RouteTraceCallbackMetadataKey] = func(cliproxyexecutor.RouteTraceEvent) {}
	case "recording":
		recorder = &routeTraceTestRecorder{}
		metadata[cliproxyexecutor.RouteTraceCallbackMetadataKey] = recorder.Add
	case "panic":
		metadata[cliproxyexecutor.RouteTraceCallbackMetadataKey] = func(cliproxyexecutor.RouteTraceEvent) {
			panic("callback panic")
		}
	case "saturated":
		saturatedTrace = internallogging.NewRouteTrace()
		for range cliproxyexecutor.MaxRouteTraceEvents {
			saturatedTrace.Add(cliproxyexecutor.RouteTraceEvent{Action: "candidate.excluded"})
		}
		metadata[cliproxyexecutor.RouteTraceCallbackMetadataKey] = saturatedTrace.Callback()
		metadata[cliproxyexecutor.RouteTraceTruncatedCallbackMetadataKey] = saturatedTrace.TruncatedCallback()
	default:
		t.Fatalf("unknown callback mode %q", mode)
	}
	payload := []byte(`{"input":"stream-equivalence"}`)
	opts := cliproxyexecutor.Options{Stream: true, OriginalRequest: payload, Metadata: metadata}
	streamResult, errStream := manager.ExecuteStream(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model, Payload: payload},
		opts,
	)
	if got := executor.StreamCalls(); !reflect.DeepEqual(got, []string{authA}) {
		t.Fatalf("stream calls = %v", got)
	}
	result := routeTraceStreamEquivalenceResult{
		streamCalls:            executor.StreamCalls(),
		selectedAuth:           metadata[cliproxyexecutor.SelectedAuthMetadataKey],
		selectedIndex:          metadata[cliproxyexecutor.SelectedAuthIndexMetadataKey],
		selectedAuthCallbacks:  selectedAuthCallbacks,
		selectedIndexCallbacks: selectedIndexCallbacks,
	}
	if errStream != nil {
		result.streamErrorType = fmt.Sprintf("%T", errStream)
		result.streamErrorText = errStream.Error()
		return result, routeTraceStreamEvents{}
	}
	if streamResult == nil {
		t.Fatal("ExecuteStream() result = nil")
	}
	streamEvents := routeTraceStreamEvents{}
	if recorder != nil {
		streamEvents.beforeConsume = recorder.Events()
	}
	result.headers = streamResult.Headers.Clone()
	for chunk := range streamResult.Chunks {
		snapshot := routeTraceStreamChunkSnapshot{payload: string(chunk.Payload)}
		if chunk.Err != nil {
			snapshot.errorType = fmt.Sprintf("%T", chunk.Err)
			snapshot.errorText = chunk.Err.Error()
			snapshot.status = statusCodeFromError(chunk.Err)
			if retryAfter := retryAfterFromError(chunk.Err); retryAfter != nil {
				snapshot.retryAfterMS = retryAfter.Milliseconds()
			}
		}
		result.chunks = append(result.chunks, snapshot)
	}
	if saturatedTrace != nil && saturatedTrace.Snapshot().TruncatedEvents == 0 {
		t.Fatal("saturated collector did not record omitted stream events")
	}
	if recorder != nil {
		streamEvents.afterConsume = recorder.Events()
	}
	return result, streamEvents
}

func TestManagerRouteTraceCallbackDoesNotChangeStreamExecution(t *testing.T) {
	for _, lateFailure := range []bool{false, true} {
		scenario := "success"
		if lateFailure {
			scenario = "late_failure"
		}
		t.Run(
			scenario, func(t *testing.T) {
				var baseline routeTraceStreamEquivalenceResult
				for index, mode := range []string{"none", "noop", "panic", "saturated"} {
					t.Run(
						mode, func(t *testing.T) {
							got, _ := runRouteTraceStreamEquivalenceScenario(t, mode, lateFailure)
							if index == 0 {
								baseline = got
								return
							}
							if !reflect.DeepEqual(got, baseline) {
								t.Fatalf(
									"stream execution differs from no-trace baseline:\n got: %#v\nwant: %#v", got,
									baseline,
								)
							}
						},
					)
				}
			},
		)
	}
}

func TestManagerRouteTraceRecordsRealStreamTerminal(t *testing.T) {
	for _, lateFailure := range []bool{false, true} {
		scenario := "success"
		if lateFailure {
			scenario = "late_failure"
		}
		t.Run(
			scenario, func(t *testing.T) {
				_, streamEvents := runRouteTraceStreamEquivalenceScenario(t, "recording", lateFailure)
				started := findRouteTraceEvent(t, streamEvents.beforeConsume, "attempt.stream_started", 1)
				if started.FinalTarget || started.ResultCode != "stream_started" {
					t.Fatalf("stream started event = %+v", started)
				}
				for _, event := range streamEvents.beforeConsume {
					if event.Attempt == 1 && (event.Action == "attempt.completed" || event.Action == "attempt.failed" ||
						event.Action == "result.recorded") {
						t.Fatalf("terminal event published before stream consumption: %+v", event)
					}
				}
				events := streamEvents.afterConsume
				if lateFailure {
					failed := findRouteTraceEvent(t, events, "attempt.failed", 1)
					if failed.Reason != "stream_terminated" || failed.ResultCode != "upstream_error" ||
						failed.HTTPStatus != http.StatusBadGateway || failed.RetryAfterMS != 75 || !failed.FinalTarget {
						t.Fatalf("stream failed event = %+v", failed)
					}
				} else {
					completed := findRouteTraceEvent(t, events, "attempt.completed", 1)
					if completed.ResultCode != "success" || completed.HTTPStatus != http.StatusOK || !completed.FinalTarget {
						t.Fatalf("stream completed event = %+v", completed)
					}
				}
				resultRecorded := findRouteTraceEvent(t, events, "result.recorded", 1)
				if !resultRecorded.FinalTarget {
					t.Fatalf("stream result event = %+v", resultRecorded)
				}
			},
		)
	}
}

func TestManagerRouteTraceStateSurvivesEnrichedStreamRetries(t *testing.T) {
	const (
		provider = "route-trace-stream-retry-state-provider"
		model    = "route-trace-stream-retry-state-model"
	)
	executor := &routeTraceTestExecutor{
		provider: provider,
		streamFn: func(*Auth) (*cliproxyexecutor.StreamResult, error) {
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{
				Err: routeTraceTestError{
					status:  http.StatusBadRequest,
					message: "bootstrap failed",
				},
			}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	registerRouteTraceTestAuths(
		t,
		manager,
		provider,
		model,
		&Auth{ID: "route-trace-stream-retry-state-auth", Provider: provider},
	)
	recorder := &routeTraceTestRecorder{}
	payload := []byte(`{"input":"same-request"}`)
	metadata := map[string]any{
		cliproxyexecutor.RequestedModelMetadataKey:     model,
		cliproxyexecutor.RouteTraceCallbackMetadataKey: recorder.Add,
	}
	opts := cliproxyexecutor.Options{Stream: true, OriginalRequest: payload, Metadata: metadata}

	for call := range 2 {
		result, errStream := manager.ExecuteStream(
			context.Background(),
			[]string{provider},
			cliproxyexecutor.Request{Model: model, Payload: payload},
			opts,
		)
		if errStream != nil {
			t.Fatalf("ExecuteStream() call %d error = %v", call+1, errStream)
		}
		if result == nil {
			t.Fatalf("ExecuteStream() call %d result = nil", call+1)
		}
		var terminalErr error
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				terminalErr = chunk.Err
			}
		}
		if terminalErr == nil {
			t.Fatalf("ExecuteStream() call %d terminal error = nil", call+1)
		}
	}

	var selectedAttempts []int
	for _, event := range recorder.Events() {
		if event.Action == "route.selected" {
			selectedAttempts = append(selectedAttempts, event.Attempt)
		}
	}
	if !reflect.DeepEqual(selectedAttempts, []int{1, 2}) {
		t.Fatalf("route selected attempts = %v, want [1 2]", selectedAttempts)
	}
	if routeTraceStateFromMetadata(metadata) == nil {
		t.Fatal("route trace state was not retained in the request metadata")
	}
}

func TestManagerRouteTraceStreamBootstrapUnknownOutcomeStopsReplay(t *testing.T) {
	const (
		provider = "route-trace-stream-provider"
		model    = "route-trace-stream-model"
	)
	executor := &routeTraceTestExecutor{
		provider: provider,
		streamFn: func(auth *Auth) (*cliproxyexecutor.StreamResult, error) {
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			if auth.ID == "route-trace-stream-a" {
				chunks <- cliproxyexecutor.StreamChunk{
					Err: routeTraceTestError{
						status:     http.StatusServiceUnavailable,
						retryAfter: 1500 * time.Millisecond,
						message:    "private stream bootstrap detail",
					},
				}
			} else {
				chunks <- cliproxyexecutor.StreamChunk{Err: errors.New("unexpected credential replay")}
			}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	registerRouteTraceTestAuths(
		t,
		manager,
		provider,
		model,
		&Auth{ID: "route-trace-stream-a", Provider: provider},
		&Auth{ID: "route-trace-stream-b", Provider: provider},
	)
	recorder := &routeTraceTestRecorder{}

	opts := routeTraceTestOptions(recorder.Add)
	opts.Stream = true
	result, errStream := manager.ExecuteStream(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		opts,
	)
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	if result == nil {
		t.Fatal("ExecuteStream() result = nil")
	}
	var terminalErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			terminalErr = chunk.Err
		}
	}
	if terminalErr == nil || terminalErr.Error() != "private stream bootstrap detail" {
		t.Fatalf("stream terminal error = %v", terminalErr)
	}
	if got := executor.StreamCalls(); !reflect.DeepEqual(got, []string{"route-trace-stream-a"}) {
		t.Fatalf("stream calls = %v", got)
	}

	events := recorder.Events()
	failed := findRouteTraceEvent(t, events, "attempt.failed", 1)
	if failed.Reason != "upstream_failure" || failed.HTTPStatus != http.StatusServiceUnavailable ||
		failed.RetryAfterMS != 1500 || !failed.FinalTarget {
		t.Fatalf("failed event = %+v", failed)
	}
	if failed.DispatchPhase != cliproxyexecutor.DispatchPhaseResponseStarted ||
		failed.Outcome != cliproxyexecutor.ExecutionOutcomeUnknown ||
		failed.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry ||
		failed.EffectScope != cliproxyexecutor.StateEffectScopeProvider || failed.EffectKey != provider {
		t.Fatalf("failed disposition = %+v", failed)
	}
	recorded := findRouteTraceEvent(t, events, "result.recorded", 1)
	if recorded.DispatchPhase != failed.DispatchPhase || recorded.Outcome != failed.Outcome ||
		recorded.RetryDirective != failed.RetryDirective || recorded.EffectScope != failed.EffectScope ||
		recorded.EffectKey != failed.EffectKey || recorded.StateGeneration != failed.StateGeneration {
		t.Fatalf("recorded event = %+v, failed event = %+v", recorded, failed)
	}
	stopped := findRouteTraceEvent(t, events, "route.stopped", 1)
	if stopped.Reason != "outcome_unknown" || stopped.Outcome != cliproxyexecutor.ExecutionOutcomeUnknown ||
		stopped.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry {
		t.Fatalf("stopped event = %+v", stopped)
	}
}

func TestManagerRouteTraceCooldownWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			provider = "route-trace-cooldown-provider"
			model    = "route-trace-cooldown-model"
		)
		var executeCount int
		executor := &routeTraceTestExecutor{
			provider: provider,
			executeFn: func(auth *Auth) (cliproxyexecutor.Response, error) {
				executeCount++
				if executeCount == 1 {
					return cliproxyexecutor.Response{}, routeTraceTestError{
						status:     http.StatusTooManyRequests,
						retryAfter: 5 * time.Millisecond,
						message:    "private cooldown detail",
					}
				}
				return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
			},
		}
		manager := NewManager(nil, &RoundRobinSelector{}, nil)
		manager.SetRetryConfig(1, 2*minQuotaCooldownFloor, 0)
		manager.RegisterExecutor(executor)
		registerRouteTraceTestAuths(
			t,
			manager,
			provider,
			model,
			&Auth{ID: "route-trace-cooldown-auth", Provider: provider},
		)
		recorder := &routeTraceTestRecorder{}

		response, errExecute := manager.Execute(
			context.Background(),
			[]string{provider},
			cliproxyexecutor.Request{Model: model},
			routeTraceTestOptions(recorder.Add),
		)
		if errExecute != nil {
			t.Fatalf("Execute() error = %v", errExecute)
		}
		if got := string(response.Payload); got != "route-trace-cooldown-auth" {
			t.Fatalf("response payload = %q", got)
		}
		if got := executor.ExecuteCalls(); !reflect.DeepEqual(
			got,
			[]string{"route-trace-cooldown-auth", "route-trace-cooldown-auth"},
		) {
			t.Fatalf("execute calls = %v", got)
		}

		events := recorder.Events()
		cooldownWait := findRouteTraceEvent(t, events, "cooldown.wait", 2)
		if cooldownWait.ResultCode != "rate_limited" || cooldownWait.HTTPStatus != http.StatusTooManyRequests {
			t.Fatalf("cooldown wait event = %+v", cooldownWait)
		}
		if cooldownWait.RetryAfterMS != minQuotaCooldownFloor.Milliseconds() {
			t.Fatalf("cooldown retry_after_ms = %d", cooldownWait.RetryAfterMS)
		}
		findRouteTraceEvent(t, events, "cooldown.completed", 2)
		completed := findRouteTraceEvent(t, events, "attempt.completed", 3)
		if !completed.FinalTarget {
			t.Fatalf("completed event = %+v", completed)
		}
	})
}

func TestManagerRouteTraceAllCandidatesExcluded(t *testing.T) {
	const model = "route-trace-excluded-model"
	executor := &routeTraceTestExecutor{provider: "codex"}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	registerRouteTraceTestAuths(
		t,
		manager,
		"codex",
		model,
		&Auth{
			ID:         "route-trace-excluded-a",
			Provider:   "codex",
			Attributes: map[string]string{"plan_type": "free"},
		},
		&Auth{
			ID:         "route-trace-excluded-b",
			Provider:   "codex",
			Attributes: map[string]string{"plan_type": "free"},
		},
	)
	recorder := &routeTraceTestRecorder{}
	opts := routeTraceTestOptions(recorder.Add)
	opts.Metadata[cliproxyexecutor.DisallowFreeAuthMetadataKey] = true

	_, errExecute := manager.Execute(
		context.Background(),
		[]string{"codex"},
		cliproxyexecutor.Request{Model: model},
		opts,
	)
	if errExecute == nil {
		t.Fatal("Execute() error = nil")
	}
	if got := executor.ExecuteCalls(); len(got) != 0 {
		t.Fatalf("execute calls = %v", got)
	}

	events := recorder.Events()
	excluded := make(map[string]struct{})
	for _, event := range events {
		if event.Action == "candidate.excluded" && event.Reason == "request_filter" {
			excluded[event.AuthIndex] = struct{}{}
		}
	}
	if len(excluded) != 2 {
		t.Fatalf("request-filter exclusions = %v, events = %+v", excluded, events)
	}
	selectionFailed := findRouteTraceEvent(t, events, "route.selection_failed", 1)
	if selectionFailed.ResultCode != "selection_failed" {
		t.Fatalf("selection failed event = %+v", selectionFailed)
	}
}

func TestManagerRouteTraceMixedProviderUnknownOutcomeStopsReplay(t *testing.T) {
	const model = "route-trace-mixed-model"
	firstExecutor := &routeTraceTestExecutor{
		provider: "route-trace-mixed-a",
		executeFn: func(*Auth) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, routeTraceTestError{
				status:  http.StatusServiceUnavailable,
				message: "private first provider detail",
			}
		},
	}
	secondExecutor := &routeTraceTestExecutor{
		provider: "route-trace-mixed-b",
		executeFn: func(*Auth) (cliproxyexecutor.Response, error) {
			return cliproxyexecutor.Response{}, errors.New("unexpected provider replay")
		},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(firstExecutor)
	manager.RegisterExecutor(secondExecutor)
	registerRouteTraceTestAuths(
		t,
		manager,
		firstExecutor.provider,
		model,
		&Auth{ID: "route-trace-mixed-auth-a", Provider: firstExecutor.provider},
	)
	registerRouteTraceTestAuths(
		t,
		manager,
		secondExecutor.provider,
		model,
		&Auth{ID: "route-trace-mixed-auth-b", Provider: secondExecutor.provider},
	)
	recorder := &routeTraceTestRecorder{}

	_, errExecute := manager.Execute(
		context.Background(),
		[]string{firstExecutor.provider, secondExecutor.provider},
		cliproxyexecutor.Request{Model: model},
		routeTraceTestOptions(recorder.Add),
	)
	if errExecute == nil || errExecute.Error() != "private first provider detail" {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := firstExecutor.ExecuteCalls(); !reflect.DeepEqual(got, []string{"route-trace-mixed-auth-a"}) {
		t.Fatalf("first provider calls = %v", got)
	}
	if got := secondExecutor.ExecuteCalls(); len(got) != 0 {
		t.Fatalf("second provider calls = %v", got)
	}

	events := recorder.Events()
	selected := findRouteTraceEvent(t, events, "route.selected", 1)
	if selected.Provider != firstExecutor.provider {
		t.Fatalf("selected provider = %q", selected.Provider)
	}
	failed := findRouteTraceEvent(t, events, "attempt.failed", 1)
	if failed.Provider != firstExecutor.provider || failed.HTTPStatus != http.StatusServiceUnavailable ||
		!failed.FinalTarget || failed.DispatchPhase != cliproxyexecutor.DispatchPhaseResponseStarted ||
		failed.Outcome != cliproxyexecutor.ExecutionOutcomeUnknown ||
		failed.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry ||
		failed.EffectScope != cliproxyexecutor.StateEffectScopeProvider || failed.EffectKey != firstExecutor.provider {
		t.Fatalf("failed event = %+v", failed)
	}
	stopped := findRouteTraceEvent(t, events, "route.stopped", 1)
	if stopped.Reason != "outcome_unknown" || stopped.Provider != firstExecutor.provider {
		t.Fatalf("stopped event = %+v", stopped)
	}
}

func TestManagerRouteTraceHomeDispatch(t *testing.T) {
	executor := &routeTraceMetadataExecutor{
		routeTraceTestExecutor: &routeTraceTestExecutor{provider: "codex"},
	}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	payload, errMarshal := json.Marshal(
		homeAuthDispatchResponse{
			AuthIndex: "0123456789abcdef",
			Provider:  "codex",
			Auth: Auth{
				ID:       "0123456789abcdef",
				Provider: "codex",
			},
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal home dispatch response: %v", errMarshal)
	}
	dispatcher := &testHomeDispatcher{heartbeat: true, payload: payload}
	previous := currentHomeDispatcher
	currentHomeDispatcher = func() homeAuthDispatcher { return dispatcher }
	t.Cleanup(func() { currentHomeDispatcher = previous })
	recorder := &routeTraceTestRecorder{}

	opts := routeTraceTestOptions(recorder.Add)
	opts.Metadata["business_key"] = "business_value"
	response, errExecute := manager.Execute(
		context.Background(),
		[]string{"codex"},
		cliproxyexecutor.Request{Model: "route-trace-home-model"},
		opts,
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(response.Payload); got != "0123456789abcdef" {
		t.Fatalf("response payload = %q", got)
	}
	if dispatcher.calls != 1 {
		t.Fatalf("home dispatch calls = %d", dispatcher.calls)
	}
	assertNoRouteTraceMetadata(t, executor.executeMetadata)

	events := recorder.Events()
	selected := findRouteTraceEvent(t, events, "route.selected", 1)
	if selected.Provider != "codex" || selected.Reason != "home_dispatch" {
		t.Fatalf("selected event = %+v", selected)
	}
	expectedIndex := stableAuthIndex("id:0123456789abcdef")
	if selected.AuthIndex != expectedIndex || selected.AuthIndex == "0123456789abcdef" {
		t.Fatalf("selected auth index = %q, want trace-safe %q", selected.AuthIndex, expectedIndex)
	}
	decisionEvent := findRouteTraceEvent(t, events, "route.decision", 1)
	if decisionEvent.Decision == nil || decisionEvent.Decision.Strategy != "custom" ||
		decisionEvent.Decision.CandidateScope != "selected_only" ||
		decisionEvent.Decision.MatchedRule != "home_dispatch" || decisionEvent.Decision.Selected == nil ||
		decisionEvent.Decision.Selected.AuthIndex != expectedIndex || len(decisionEvent.Decision.Candidates) != 1 ||
		decisionEvent.Decision.Candidates[0].AuthIndex != expectedIndex {
		t.Fatalf("home decision = %+v", decisionEvent.Decision)
	}
	encodedDecision, errMarshal := json.Marshal(decisionEvent)
	if errMarshal != nil {
		t.Fatalf("marshal home decision: %v", errMarshal)
	}
	if strings.Contains(string(encodedDecision), "0123456789abcdef") {
		t.Fatalf("home decision leaked raw auth identity: %s", encodedDecision)
	}
	completed := findRouteTraceEvent(t, events, "attempt.completed", 1)
	if !completed.FinalTarget || completed.AuthIndex != expectedIndex {
		t.Fatalf("completed event = %+v", completed)
	}
}

func TestManagerRouteTraceCallbackCanMutateManagerAfterSelection(t *testing.T) {
	const (
		provider = "route-trace-reentrant-provider"
		model    = "route-trace-reentrant-model"
	)
	executor := &routeTraceTestExecutor{provider: provider}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	registerRouteTraceTestAuths(
		t,
		manager,
		provider,
		model,
		&Auth{ID: "route-trace-reentrant-auth", Provider: provider},
	)
	var (
		once        sync.Once
		callbackErr error
	)
	callback := func(event cliproxyexecutor.RouteTraceEvent) {
		if event.Action != "candidate.selected" {
			return
		}
		once.Do(
			func() {
				_, callbackErr = manager.Register(
					context.Background(),
					&Auth{ID: "route-trace-callback-added", Provider: provider, Status: StatusActive},
				)
			},
		)
	}

	response, errExecute := manager.Execute(
		context.Background(),
		[]string{provider},
		cliproxyexecutor.Request{Model: model},
		routeTraceTestOptions(callback),
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if callbackErr != nil {
		t.Fatalf("callback Register() error = %v", callbackErr)
	}
	if got := string(response.Payload); got != "route-trace-reentrant-auth" {
		t.Fatalf("response payload = %q", got)
	}
	if _, ok := manager.GetByID("route-trace-callback-added"); !ok {
		t.Fatal("callback-added auth is missing")
	}
}

func TestManagerRouteTraceAntigravityCreditsFallback(t *testing.T) {
	const model = "claude-route-trace-credits-model"
	var executeCount int
	executor := &routeTraceTestExecutor{
		provider: "antigravity",
		executeFn: func(auth *Auth) (cliproxyexecutor.Response, error) {
			executeCount++
			if executeCount == 1 {
				return cliproxyexecutor.Response{}, routeTraceTestError{
					status:  http.StatusTooManyRequests,
					message: "primary quota exhausted",
				}
			}
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.SetConfig(
		&internalconfig.Config{
			QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
		},
	)
	manager.RegisterExecutor(executor)
	registerRouteTraceTestAuths(
		t,
		manager,
		"antigravity",
		model,
		&Auth{ID: "route-trace-credits-auth", Provider: "antigravity"},
	)
	recorder := &routeTraceTestRecorder{}

	response, errExecute := manager.Execute(
		context.Background(),
		[]string{"antigravity"},
		cliproxyexecutor.Request{Model: model},
		routeTraceTestOptions(recorder.Add),
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := string(response.Payload); got != "route-trace-credits-auth" {
		t.Fatalf("response payload = %q", got)
	}
	if got := executor.ExecuteCalls(); !reflect.DeepEqual(
		got,
		[]string{"route-trace-credits-auth", "route-trace-credits-auth"},
	) {
		t.Fatalf("execute calls = %v", got)
	}

	events := recorder.Events()
	var fallbackSelected cliproxyexecutor.RouteTraceEvent
	for _, event := range events {
		if event.Action == "route.selected" && event.Reason == "antigravity_credits_fallback" {
			fallbackSelected = event
			break
		}
	}
	if fallbackSelected.Attempt == 0 {
		t.Fatalf("credits fallback selection missing: %+v", events)
	}
	completed := findRouteTraceEvent(t, events, "attempt.completed", fallbackSelected.Attempt)
	if completed.PreviousAttempt != fallbackSelected.PreviousAttempt || !completed.FinalTarget {
		t.Fatalf("credits fallback completed event = %+v", completed)
	}
	if completed.AuthIndex != routeTraceTestAuthIndex(t, manager, "route-trace-credits-auth") {
		t.Fatalf("credits fallback auth index = %q", completed.AuthIndex)
	}
}

func TestManagerRouteTraceAntigravityCreditsStreamFallback(t *testing.T) {
	const model = "claude-route-trace-credits-stream-model"
	var streamCount int
	executor := &routeTraceTestExecutor{
		provider: "antigravity",
		streamFn: func(*Auth) (*cliproxyexecutor.StreamResult, error) {
			streamCount++
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			if streamCount == 1 {
				chunks <- cliproxyexecutor.StreamChunk{
					Err: routeTraceTestError{
						status:  http.StatusTooManyRequests,
						message: "primary stream quota exhausted",
					},
				}
			} else {
				chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("credits-stream-success")}
			}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.SetConfig(
		&internalconfig.Config{
			QuotaExceeded: internalconfig.QuotaExceeded{AntigravityCredits: true},
		},
	)
	manager.RegisterExecutor(executor)
	registerRouteTraceTestAuths(
		t,
		manager,
		"antigravity",
		model,
		&Auth{ID: "route-trace-credits-stream-auth", Provider: "antigravity"},
	)
	recorder := &routeTraceTestRecorder{}

	opts := routeTraceTestOptions(recorder.Add)
	opts.Stream = true
	result, errStream := manager.ExecuteStream(
		context.Background(),
		[]string{"antigravity"},
		cliproxyexecutor.Request{Model: model},
		opts,
	)
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	if result == nil {
		t.Fatal("ExecuteStream() result = nil")
	}
	var payload []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if got := string(payload); got != "credits-stream-success" {
		t.Fatalf("stream payload = %q", got)
	}
	if got := executor.StreamCalls(); !reflect.DeepEqual(
		got,
		[]string{"route-trace-credits-stream-auth", "route-trace-credits-stream-auth"},
	) {
		t.Fatalf("stream calls = %v", got)
	}

	events := recorder.Events()
	var fallbackSelected cliproxyexecutor.RouteTraceEvent
	for _, event := range events {
		if event.Action == "route.selected" && event.Reason == "antigravity_credits_fallback" {
			fallbackSelected = event
			break
		}
	}
	if fallbackSelected.Attempt == 0 {
		t.Fatalf("credits stream fallback selection missing: %+v", events)
	}
	started := findRouteTraceEvent(t, events, "attempt.stream_started", fallbackSelected.Attempt)
	if started.ResultCode != "stream_started" || started.FinalTarget {
		t.Fatalf("credits stream fallback start event = %+v", started)
	}
	completed := findRouteTraceEvent(t, events, "attempt.completed", fallbackSelected.Attempt)
	if completed.ResultCode != "success" || !completed.FinalTarget {
		t.Fatalf("credits stream fallback completed event = %+v", completed)
	}
	resultRecorded := findRouteTraceEvent(t, events, "result.recorded", fallbackSelected.Attempt)
	if resultRecorded.ResultCode != "success" || !resultRecorded.FinalTarget {
		t.Fatalf("credits stream fallback result event = %+v", resultRecorded)
	}
}
