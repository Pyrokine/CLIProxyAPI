package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type requestScopedHomeDispatcher struct {
	mu             sync.Mutex
	auths          []*Auth
	constraints    []home.AuthDispatchConstraints
	accounted      bool
	secondDispatch chan struct{}
	secondOnce     sync.Once
}

func (*requestScopedHomeDispatcher) HeartbeatOK() bool { return true }

func (*requestScopedHomeDispatcher) RPopAuth(
	context.Context,
	string,
	string,
	http.Header,
	int,
) ([]byte, error) {
	panic("constraints capability was bypassed")
}

func (d *requestScopedHomeDispatcher) RPopAuthWithConstraints(
	_ context.Context,
	requestedModel string,
	_ string,
	_ http.Header,
	_ int,
	constraints home.AuthDispatchConstraints,
) ([]byte, error) {
	d.mu.Lock()
	d.constraints = append(d.constraints, constraints)
	if len(d.constraints) == 2 && d.secondDispatch != nil {
		d.secondOnce.Do(func() { close(d.secondDispatch) })
	}
	var selected *Auth
	for _, candidate := range d.auths {
		if candidate == nil || requestScopedHomeAuthExcluded(candidate.ID, constraints) {
			continue
		}
		selected = candidate.Clone()
		break
	}
	accounted := d.accounted
	d.mu.Unlock()

	if selected == nil {
		return []byte(`{"error":{"type":"auth_unavailable","message":"no synthetic auth"}}`), nil
	}
	payload := map[string]any{
		"auth":       selected,
		"auth_index": selected.ID,
		"model":      requestedModel,
	}
	if accounted {
		payload["concurrency"] = homeConcurrencyTuple{
			Accounted:    true,
			CredentialID: selected.ID,
			Model:        canonicalHomeConcurrencyModelKey(requestedModel),
		}
	}
	return json.Marshal(payload)
}

func requestScopedHomeAuthExcluded(id string, constraints home.AuthDispatchConstraints) bool {
	if constraints.PinnedAuthID != "" && constraints.PinnedAuthID != id {
		return true
	}
	for _, excluded := range constraints.ExcludedAuthIDs {
		if excluded == id {
			return true
		}
	}
	return false
}

func (d *requestScopedHomeDispatcher) dispatchCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.constraints)
}

func newRequestScopedHomeManager(
	t *testing.T,
	dispatcher *requestScopedHomeDispatcher,
	executor *requestScopedTestExecutor,
	hook Hook,
) (*Manager, *executionregistry.Registry) {
	t.Helper()
	manager := NewManager(nil, nil, hook)
	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	manager.SetRetryConfig(0, 0, 2)
	manager.RegisterExecutor(executor)
	registry := executionregistry.New()
	if bundle := manager.PublishHomeDispatch(dispatcher, registry, 1); bundle == nil {
		t.Fatal("PublishHomeDispatch() returned nil")
	}
	return manager, registry
}

func requestScopedHomeAuth(id, provider string, status int, action string) *Auth {
	auth := &Auth{ID: id, Provider: provider, Status: StatusActive}
	if action != "" {
		auth.Metadata = map[string]any{
			"request_scoped_errors": []internalconfig.RequestScopedErrorRule{{
				Status: status,
				Match:  []string{"matched"},
				Action: action,
			}},
		}
	}
	return auth
}

func TestRequestScopedErrorActionsHomeExecution(t *testing.T) {
	for _, test := range []struct {
		name                  string
		action                string
		status                int
		wantSuccess           bool
		wantDispatch          int
		forceCooldown         bool
		explicitRequestScoped bool
	}{
		{name: "stop", action: RequestScopedActionStop, wantDispatch: 1},
		{name: "stop and cooldown", action: RequestScopedActionStopAndCooldown, wantDispatch: 1, forceCooldown: true},
		{name: "continue", action: RequestScopedActionContinue, wantSuccess: true, wantDispatch: 2},
		{name: "continue explicit request-scoped error", action: RequestScopedActionContinue, wantSuccess: true, wantDispatch: 2, explicitRequestScoped: true},
		{name: "continue unknown outcome", action: RequestScopedActionContinue, status: http.StatusInternalServerError, wantDispatch: 1},
		{name: "continue and cooldown", action: RequestScopedActionContinueAndCooldown, wantSuccess: true, wantDispatch: 2, forceCooldown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const provider = "request-home-provider"
			const model = "request-home-model"
			status := test.status
			if status == 0 {
				status = http.StatusBadRequest
			}
			var upstream error = &requestScopedTestError{status: status, message: "matched"}
			if test.explicitRequestScoped {
				upstream = &Error{
					Code:           ErrorCodeRequestScoped,
					Message:        "matched",
					HTTPStatus:     http.StatusBadRequest,
					DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
					Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
					RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
					EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
				}
			}
			executor := &requestScopedTestExecutor{provider: provider}
			executor.execute = func(auth *Auth) (cliproxyexecutor.Response, error) {
				if auth.ID == "home-first" {
					return cliproxyexecutor.Response{}, upstream
				}
				return cliproxyexecutor.Response{Payload: []byte("success")}, nil
			}
			dispatcher := &requestScopedHomeDispatcher{auths: []*Auth{
				requestScopedHomeAuth("home-first", provider, status, test.action),
				requestScopedHomeAuth("home-second", provider, 0, ""),
			}}
			hook := &runtimeResultHook{results: make(chan Result, 3)}
			manager, _ := newRequestScopedHomeManager(t, dispatcher, executor, hook)

			response, err := manager.Execute(
				context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{},
			)
			if test.wantSuccess {
				if err != nil || string(response.Payload) != "success" {
					t.Fatalf("Execute() response = %q, error = %v", response.Payload, err)
				}
			} else if err != upstream {
				t.Fatalf("Execute() error = %T/%v, want original", err, err)
			}
			if calls := executor.calls.Load(); calls != int32(test.wantDispatch) {
				t.Fatalf("Execute() calls = %d, want %d", calls, test.wantDispatch)
			}
			if calls := dispatcher.dispatchCount(); calls != test.wantDispatch {
				t.Fatalf("Home dispatches = %d, want %d", calls, test.wantDispatch)
			}
			select {
			case failed := <-hook.results:
				if failed.Success || test.forceCooldown != isForceCooldownResult(failed) {
					t.Fatalf("Home failure result = %#v", failed)
				}
			case <-time.After(time.Second):
				t.Fatal("Home failure result was not reported")
			}
		})
	}
}

func TestRequestScopedErrorHomeCountTokensForceCooldownOverridesNeutral404(t *testing.T) {
	const provider = "request-home-count"
	const model = "request-home-model"
	upstream := &requestScopedTestError{status: http.StatusNotFound, message: "matched"}
	executor := &requestScopedTestExecutor{provider: provider}
	executor.count = func(*Auth) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{}, upstream
	}
	auth := requestScopedHomeAuth("home-count", provider, http.StatusNotFound, RequestScopedActionStopAndCooldown)
	auth.Metadata["disable_cooling"] = true
	dispatcher := &requestScopedHomeDispatcher{auths: []*Auth{auth}}
	hook := &runtimeResultHook{results: make(chan Result, 1)}
	manager, _ := newRequestScopedHomeManager(t, dispatcher, executor, hook)

	_, err := manager.ExecuteCount(
		context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{},
	)
	if err != upstream {
		t.Fatalf("ExecuteCount() error = %T/%v, want original", err, err)
	}
	if calls := executor.countCalls.Load(); calls != 1 {
		t.Fatalf("CountTokens calls = %d, want 1", calls)
	}
	select {
	case marked := <-hook.results:
		if !isForceCooldownResult(marked) || marked.Error == nil || marked.Error.Code != ErrorCodeForceCooldown ||
			marked.Disposition.EffectScope != cliproxyexecutor.StateEffectScopeExactModel ||
			marked.Disposition.EffectKey != model || marked.Disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry {
			t.Fatalf("Home CountTokens result = %#v", marked)
		}
	case <-time.After(time.Second):
		t.Fatal("Home CountTokens result was not reported")
	}
}

func TestRequestScopedErrorHomeStreamBootstrap(t *testing.T) {
	for _, test := range []struct {
		name                  string
		action                string
		wantSuccess           bool
		wantDispatch          int
		explicitRequestScoped bool
	}{
		{name: "stop", action: RequestScopedActionStop, wantDispatch: 1},
		{name: "continue", action: RequestScopedActionContinue, wantSuccess: true, wantDispatch: 2},
		{name: "continue explicit request-scoped error", action: RequestScopedActionContinue, wantSuccess: true, wantDispatch: 2, explicitRequestScoped: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const provider = "request-home-stream"
			const model = "request-home-model"
			var upstream error = &requestScopedTestError{status: http.StatusBadRequest, message: "matched"}
			if test.explicitRequestScoped {
				upstream = &Error{
					Code:           ErrorCodeRequestScoped,
					Message:        "matched",
					HTTPStatus:     http.StatusBadRequest,
					DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
					Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
					RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
					EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
				}
			}
			executor := &requestScopedTestExecutor{provider: provider}
			executor.stream = func(auth *Auth) (*cliproxyexecutor.StreamResult, error) {
				chunks := make(chan cliproxyexecutor.StreamChunk, 1)
				if auth.ID == "home-first" {
					chunks <- cliproxyexecutor.StreamChunk{Err: upstream}
				} else {
					chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("success")}
				}
				close(chunks)
				return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Test": {"home"}}, Chunks: chunks}, nil
			}
			dispatcher := &requestScopedHomeDispatcher{auths: []*Auth{
				requestScopedHomeAuth("home-first", provider, http.StatusBadRequest, test.action),
				requestScopedHomeAuth("home-second", provider, 0, ""),
			}}
			manager, _ := newRequestScopedHomeManager(t, dispatcher, executor, nil)

			result, err := manager.ExecuteStream(
				context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true},
			)
			if err != nil || result == nil {
				t.Fatalf("ExecuteStream() result = %#v, error = %v", result, err)
			}
			if result.Headers.Get("X-Test") != "home" {
				t.Fatalf("stream headers = %#v", result.Headers)
			}
			var payload []byte
			var delivered error
			for chunk := range result.Chunks {
				payload = append(payload, chunk.Payload...)
				if chunk.Err != nil {
					delivered = chunk.Err
				}
			}
			if test.wantSuccess {
				if delivered != nil || string(payload) != "success" {
					t.Fatalf("stream payload = %q, error = %v", payload, delivered)
				}
			} else {
				var got *requestScopedTestError
				if !errors.As(delivered, &got) || got != upstream || isRequestStopError(delivered) {
					t.Fatalf("stream error = %T/%v, want original without action marker", delivered, delivered)
				}
			}
			if calls := executor.streamCalls.Load(); calls != int32(test.wantDispatch) {
				t.Fatalf("ExecuteStream() calls = %d, want %d", calls, test.wantDispatch)
			}
			if calls := dispatcher.dispatchCount(); calls != test.wantDispatch {
				t.Fatalf("Home dispatches = %d, want %d", calls, test.wantDispatch)
			}
		})
	}
}

func TestRequestScopedErrorHomeContinueWaitsForReleaseAcknowledgement(t *testing.T) {
	for _, cancelRelease := range []bool{false, true} {
		name := "acknowledged"
		if cancelRelease {
			name = "cancelled"
		}
		t.Run(name, func(t *testing.T) {
			const provider = "request-home-release"
			const model = "request-home-model"
			upstream := &requestScopedTestError{status: http.StatusBadRequest, message: "matched"}
			executor := &requestScopedTestExecutor{provider: provider}
			executor.execute = func(auth *Auth) (cliproxyexecutor.Response, error) {
				if auth.ID == "home-first" {
					return cliproxyexecutor.Response{}, upstream
				}
				return cliproxyexecutor.Response{Payload: []byte("success")}, nil
			}
			dispatcher := &requestScopedHomeDispatcher{
				auths: []*Auth{
					requestScopedHomeAuth("home-first", provider, http.StatusBadRequest, RequestScopedActionContinue),
					requestScopedHomeAuth("home-second", provider, 0, ""),
				},
				accounted:      true,
				secondDispatch: make(chan struct{}),
			}
			manager, registry := newRequestScopedHomeManager(t, dispatcher, executor, nil)
			releaseStarted := make(chan struct{})
			releaseAcknowledged := make(chan struct{})
			var releases atomic.Int32
			registry.SetReleaseSink(func(group executionregistry.ReleaseGroup, sequence int64) *executionregistry.ReleaseTicket {
				if releases.Add(1) != 1 {
					return nil
				}
				close(releaseStarted)
				return executionregistry.NewReleaseTicket(group, sequence, releaseAcknowledged)
			})

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			type executeResult struct {
				response cliproxyexecutor.Response
				err      error
			}
			done := make(chan executeResult, 1)
			go func() {
				response, err := manager.Execute(
					ctx, []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{},
				)
				done <- executeResult{response: response, err: err}
			}()

			select {
			case <-releaseStarted:
			case <-ctx.Done():
				t.Fatal("credential release was not requested")
			}
			select {
			case <-dispatcher.secondDispatch:
				t.Fatal("Home redispatched before release acknowledgement")
			case <-time.After(50 * time.Millisecond):
			}

			if cancelRelease {
				cancel()
			} else {
				close(releaseAcknowledged)
			}
			result := <-done
			if cancelRelease {
				if result.err == nil || dispatcher.dispatchCount() != 1 || executor.calls.Load() != 1 {
					t.Fatalf("cancelled release result = %#v, dispatches = %d, calls = %d", result, dispatcher.dispatchCount(), executor.calls.Load())
				}
				select {
				case <-dispatcher.secondDispatch:
					t.Fatal("cancelled release allowed a new Home dispatch")
				default:
				}
				return
			}
			if result.err != nil || string(result.response.Payload) != "success" || dispatcher.dispatchCount() != 2 || executor.calls.Load() != 2 {
				t.Fatalf("acknowledged release result = %#v, dispatches = %d, calls = %d", result, dispatcher.dispatchCount(), executor.calls.Load())
			}
			select {
			case <-dispatcher.secondDispatch:
			default:
				t.Fatal("acknowledged release did not allow a new Home dispatch")
			}
		})
	}
}
