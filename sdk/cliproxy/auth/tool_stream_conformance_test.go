package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const (
	toolStreamFixtureProvider = "tool-stream-provider"
	toolStreamFixtureModel    = "gpt-5-tool"
	toolStreamFixturePayload  = `{"type":"response.output_item.done","item":{"type":"function_call","id":"call_weather","call_id":"call_weather","name":"weather","arguments":"{\"city\":\"北京\"}"}}`
)

type toolStreamFixtureAttempt struct {
	payload      []byte
	err          error
	acknowledged chan error
}

type toolStreamFixtureExecutor struct {
	mu       sync.Mutex
	attempts []toolStreamFixtureAttempt
	calls    []string
}

func (*toolStreamFixtureExecutor) Identifier() string {
	return toolStreamFixtureProvider
}

func (*toolStreamFixtureExecutor) Execute(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("unexpected non-stream execution")
}

func (e *toolStreamFixtureExecutor) ExecuteStream(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	call := len(e.calls)
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	e.calls = append(e.calls, authID)
	if call >= len(e.attempts) {
		e.mu.Unlock()
		return nil, fmt.Errorf("unexpected stream attempt %d", call+1)
	}
	attempt := e.attempts[call]
	e.mu.Unlock()

	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunk := cliproxyexecutor.StreamChunk{
		Payload: append([]byte(nil), attempt.payload...),
		Err:     attempt.err,
	}
	if attempt.acknowledged != nil {
		chunk.Acknowledge = func(err error) {
			attempt.acknowledged <- err
		}
	}
	chunks <- chunk
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (*toolStreamFixtureExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (*toolStreamFixtureExecutor) CountTokens(
	context.Context,
	*Auth,
	cliproxyexecutor.Request,
	cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("unexpected count-tokens execution")
}

func (*toolStreamFixtureExecutor) HttpRequest(
	context.Context,
	*Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, errors.New("unexpected HTTP execution")
}

func (e *toolStreamFixtureExecutor) Calls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

type toolStreamFixtureDispositionError struct {
	message     string
	disposition cliproxyexecutor.ExecutionDisposition
}

func (e toolStreamFixtureDispositionError) Error() string {
	return e.message
}

func (e toolStreamFixtureDispositionError) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	return e.disposition
}

type toolStreamFixtureDispatcher struct {
	mu       sync.Mutex
	payloads [][]byte
	calls    int
}

func (*toolStreamFixtureDispatcher) HeartbeatOK() bool {
	return true
}

func (d *toolStreamFixtureDispatcher) RPopAuth(
	context.Context,
	string,
	string,
	http.Header,
	int,
) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.calls >= len(d.payloads) {
		d.calls++
		return nil, errors.New("unexpected Home dispatch")
	}
	payload := append([]byte(nil), d.payloads[d.calls]...)
	d.calls++
	return payload, nil
}

func (d *toolStreamFixtureDispatcher) Calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

type toolStreamFixtureRelease struct {
	group    executionregistry.ReleaseGroup
	sequence int64
}

type toolStreamFixtureReleaseTracker struct {
	mu       sync.Mutex
	releases []toolStreamFixtureRelease
}

func (r *toolStreamFixtureReleaseTracker) Record(
	group executionregistry.ReleaseGroup,
	sequence int64,
) {
	r.mu.Lock()
	r.releases = append(r.releases, toolStreamFixtureRelease{group: group, sequence: sequence})
	r.mu.Unlock()
}

func (r *toolStreamFixtureReleaseTracker) Snapshot() []toolStreamFixtureRelease {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]toolStreamFixtureRelease(nil), r.releases...)
}

type toolStreamFixtureHarness struct {
	home       bool
	manager    *Manager
	executor   *toolStreamFixtureExecutor
	dispatcher *toolStreamFixtureDispatcher
	hook       *streamDeliveryResultHook
	releases   *toolStreamFixtureReleaseTracker
	ctx        context.Context
	cancel     context.CancelFunc
}

func newToolStreamFixtureHarness(
	t *testing.T,
	home bool,
	authIDs []string,
	attempts []toolStreamFixtureAttempt,
) *toolStreamFixtureHarness {
	t.Helper()
	hook := newStreamDeliveryResultHook()
	manager := NewManager(nil, nil, hook)
	manager.SetRetryConfig(0, 0, len(authIDs))
	executor := &toolStreamFixtureExecutor{attempts: attempts}
	manager.RegisterExecutor(executor)
	baseCtx := cliproxyexecutor.WithStreamDeliveryAcknowledgement(context.Background())
	ctx, cancel := context.WithCancel(baseCtx)
	t.Cleanup(cancel)

	harness := &toolStreamFixtureHarness{
		home:     home,
		manager:  manager,
		executor: executor,
		hook:     hook,
		ctx:      ctx,
		cancel:   cancel,
	}
	if !home {
		auths := make([]*Auth, 0, len(authIDs))
		for _, authID := range authIDs {
			auths = append(auths, &Auth{ID: authID, Provider: toolStreamFixtureProvider, Status: StatusActive})
		}
		registerRouteTraceTestAuths(
			t,
			manager,
			toolStreamFixtureProvider,
			toolStreamFixtureModel,
			auths...,
		)
		return harness
	}

	manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	registry := executionregistry.New()
	releases := &toolStreamFixtureReleaseTracker{}
	registry.SetReleaseSink(
		func(group executionregistry.ReleaseGroup, sequence int64) {
			releases.Record(group, sequence)
		},
	)
	payloads := make([][]byte, 0, len(authIDs))
	for _, authID := range authIDs {
		payload, errMarshal := json.Marshal(
			struct {
				Model       string               `json:"model"`
				AuthIndex   string               `json:"auth_index"`
				Auth        Auth                 `json:"auth"`
				Concurrency homeConcurrencyTuple `json:"concurrency"`
			}{
				Model:     toolStreamFixtureModel,
				AuthIndex: authID,
				Auth: Auth{
					ID:       authID,
					Index:    authID,
					Provider: toolStreamFixtureProvider,
				},
				Concurrency: homeConcurrencyTuple{
					Accounted:    true,
					CredentialID: authID,
					Model:        toolStreamFixtureModel,
				},
			},
		)
		if errMarshal != nil {
			t.Fatalf("marshal Home fixture: %v", errMarshal)
		}
		payloads = append(payloads, payload)
	}
	dispatcher := &toolStreamFixtureDispatcher{payloads: payloads}
	if bundle := manager.PublishHomeDispatch(dispatcher, registry, 1); bundle == nil {
		t.Fatal("PublishHomeDispatch() returned nil")
	}
	harness.dispatcher = dispatcher
	harness.releases = releases
	return harness
}

func (h *toolStreamFixtureHarness) ExecuteStream(t *testing.T) *cliproxyexecutor.StreamResult {
	t.Helper()
	result, errExecute := h.manager.ExecuteStream(
		h.ctx,
		[]string{toolStreamFixtureProvider},
		cliproxyexecutor.Request{Model: toolStreamFixtureModel},
		cliproxyexecutor.Options{Stream: true},
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	if result == nil || result.Chunks == nil {
		t.Fatalf("ExecuteStream() result = %#v", result)
	}
	return result
}

func (h *toolStreamFixtureHarness) AssertAttemptsAndReleases(t *testing.T, want int) {
	t.Helper()
	calls := h.executor.Calls()
	if len(calls) != want {
		t.Fatalf("executor calls = %v, want %d", calls, want)
	}
	if !h.home {
		return
	}
	if calls := h.dispatcher.Calls(); calls != want {
		t.Fatalf("Home dispatch calls = %d, want %d", calls, want)
	}
	releases := h.releases.Snapshot()
	if len(releases) != want {
		t.Fatalf("Home releases = %+v, want %d", releases, want)
	}
	seen := make(map[executionregistry.ReleaseGroup]struct{}, len(releases))
	for _, release := range releases {
		if release.sequence != 1 {
			t.Fatalf("Home release = %+v, want first sequence", release)
		}
		if _, duplicate := seen[release.group]; duplicate {
			t.Fatalf("Home scope released more than once: %+v", release.group)
		}
		seen[release.group] = struct{}{}
	}
}

func readToolStreamFixtureResult(t *testing.T, hook *streamDeliveryResultHook) Result {
	t.Helper()
	select {
	case result := <-hook.result:
		return result
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream result")
		return Result{}
	}
}

func assertNoMoreToolStreamFixtureResults(t *testing.T, hook *streamDeliveryResultHook) {
	t.Helper()
	select {
	case result := <-hook.result:
		t.Fatalf("unexpected additional stream result: %#v", result)
	default:
	}
}

func assertToolStreamFixturePayload(t *testing.T, chunk cliproxyexecutor.StreamChunk, authIDs ...string) {
	t.Helper()
	if chunk.Terminal || chunk.Err != nil || chunk.Acknowledge == nil || string(chunk.Payload) != toolStreamFixturePayload {
		t.Fatalf("tool stream payload = %#v", chunk)
	}
	for _, authID := range authIDs {
		if strings.Contains(string(chunk.Payload), authID) {
			t.Fatalf("tool stream payload leaked Home auth ID %q", authID)
		}
	}
}

func assertToolStreamFixtureFailure(t *testing.T, result Result, message string) {
	t.Helper()
	if result.Success || result.Error == nil || result.Error.Message != message ||
		result.Disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseResponseStarted ||
		result.Disposition.Outcome != cliproxyexecutor.ExecutionOutcomeUnknown ||
		result.Disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry ||
		result.Disposition.EffectScope != cliproxyexecutor.StateEffectScopeRequest ||
		!result.Error.IsRequestScoped() {
		t.Fatalf("delivery failure result = %#v", result)
	}
}

func TestToolStreamExecutionPathConformance(t *testing.T) {
	paths := []struct {
		name string
		home bool
	}{
		{name: "normal"},
		{name: "Home", home: true},
	}
	for _, path := range paths {
		t.Run(
			path.name, func(t *testing.T) {
				t.Run(
					"success waits for terminal acknowledgement", func(t *testing.T) {
						acknowledged := make(chan error, 1)
						harness := newToolStreamFixtureHarness(
							t, path.home, []string{"success-auth"}, []toolStreamFixtureAttempt{
								{
									payload:      []byte(toolStreamFixturePayload),
									acknowledged: acknowledged,
								},
							},
						)
						stream := harness.ExecuteStream(t)
						payload := <-stream.Chunks
						assertToolStreamFixturePayload(t, payload, "success-auth")
						assertNoStreamDeliveryResult(t, harness.hook)
						payload.Acknowledge(nil)
						if errAck := <-acknowledged; errAck != nil {
							t.Fatalf("upstream payload acknowledgement = %v", errAck)
						}
						terminal := <-stream.Chunks
						if !terminal.Terminal || terminal.Acknowledge == nil || len(terminal.Payload) != 0 || terminal.Err != nil {
							t.Fatalf("transport terminal = %#v", terminal)
						}
						assertNoStreamDeliveryResult(t, harness.hook)
						terminal.Acknowledge(nil)
						terminal.Acknowledge(nil)
						for range stream.Chunks {
						}

						result := readToolStreamFixtureResult(t, harness.hook)
						if !result.Success || result.RuntimeOnly != path.home ||
							result.Disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseDownstreamCommitted ||
							result.Disposition.Outcome != cliproxyexecutor.ExecutionOutcomeSucceeded {
							t.Fatalf("successful stream result = %#v", result)
						}
						assertNoMoreToolStreamFixtureResults(t, harness.hook)
						harness.AssertAttemptsAndReleases(t, 1)
					},
				)

				t.Run(
					"write failure is request scoped", func(t *testing.T) {
						acknowledged := make(chan error, 1)
						harness := newToolStreamFixtureHarness(
							t, path.home, []string{"write-auth"}, []toolStreamFixtureAttempt{
								{
									payload:      []byte(toolStreamFixturePayload),
									acknowledged: acknowledged,
								},
							},
						)
						stream := harness.ExecuteStream(t)
						payload := <-stream.Chunks
						assertToolStreamFixturePayload(t, payload, "write-auth")
						writeErr := errors.New("downstream write failed")
						payload.Acknowledge(writeErr)
						payload.Acknowledge(nil)
						for range stream.Chunks {
						}
						if errAck := <-acknowledged; !errors.Is(errAck, writeErr) {
							t.Fatalf("upstream payload acknowledgement = %v, want %v", errAck, writeErr)
						}
						assertToolStreamFixtureFailure(
							t,
							readToolStreamFixtureResult(t, harness.hook),
							writeErr.Error(),
						)
						assertNoMoreToolStreamFixtureResults(t, harness.hook)
						harness.AssertAttemptsAndReleases(t, 1)
					},
				)

				t.Run(
					"client cancel is request scoped", func(t *testing.T) {
						acknowledged := make(chan error, 1)
						harness := newToolStreamFixtureHarness(
							t, path.home, []string{"cancel-auth"}, []toolStreamFixtureAttempt{
								{
									payload:      []byte(toolStreamFixturePayload),
									acknowledged: acknowledged,
								},
							},
						)
						stream := harness.ExecuteStream(t)
						payload := <-stream.Chunks
						assertToolStreamFixturePayload(t, payload, "cancel-auth")
						harness.cancel()
						for range stream.Chunks {
						}
						if errAck := <-acknowledged; !errors.Is(errAck, context.Canceled) {
							t.Fatalf("upstream payload acknowledgement = %v, want context canceled", errAck)
						}
						assertToolStreamFixtureFailure(
							t,
							readToolStreamFixtureResult(t, harness.hook),
							context.Canceled.Error(),
						)
						assertNoMoreToolStreamFixtureResults(t, harness.hook)
						harness.AssertAttemptsAndReleases(t, 1)
					},
				)

				t.Run(
					"ambiguous bootstrap is not replayed", func(t *testing.T) {
						ambiguousErr := toolStreamFixtureDispositionError{
							message: "ambiguous bootstrap",
							disposition: cliproxyexecutor.ExecutionDisposition{
								DispatchPhase:  cliproxyexecutor.DispatchPhaseMaybeSent,
								Outcome:        cliproxyexecutor.ExecutionOutcomeUnknown,
								RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
								EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
							},
						}
						harness := newToolStreamFixtureHarness(
							t,
							path.home,
							[]string{"ambiguous-auth", "unused-auth"},
							[]toolStreamFixtureAttempt{{err: ambiguousErr}},
						)
						stream := harness.ExecuteStream(t)
						chunk, ok := <-stream.Chunks
						if !ok || chunk.Err == nil || chunk.Err.Error() != ambiguousErr.Error() {
							t.Fatalf("bootstrap error chunk = (%#v, %t)", chunk, ok)
						}
						var dispositionErr cliproxyexecutor.ExecutionDispositionError
						if !errors.As(chunk.Err, &dispositionErr) || dispositionErr == nil {
							t.Fatalf("bootstrap error lost execution disposition: %T", chunk.Err)
						}
						disposition := dispositionErr.ExecutionDisposition()
						if disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseMaybeSent ||
							disposition.Outcome != cliproxyexecutor.ExecutionOutcomeUnknown ||
							disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry ||
							disposition.EffectScope != cliproxyexecutor.StateEffectScopeRequest {
							t.Fatalf("bootstrap stream disposition = %#v", disposition)
						}
						if _, open := <-stream.Chunks; open {
							t.Fatal("bootstrap error stream remained open")
						}
						result := readToolStreamFixtureResult(t, harness.hook)
						if result.Success || result.Disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseMaybeSent ||
							result.Disposition.Outcome != cliproxyexecutor.ExecutionOutcomeUnknown ||
							result.Disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry ||
							result.Disposition.EffectScope != cliproxyexecutor.StateEffectScopeRequest {
							t.Fatalf("ambiguous bootstrap result = %#v", result)
						}
						assertNoMoreToolStreamFixtureResults(t, harness.hook)
						harness.AssertAttemptsAndReleases(t, 1)
					},
				)

				t.Run(
					"explicit not sent may use next auth", func(t *testing.T) {
						notSentErr := toolStreamFixtureDispositionError{
							message: "rejected before dispatch",
							disposition: cliproxyexecutor.ExecutionDisposition{
								DispatchPhase:  cliproxyexecutor.DispatchPhaseNotSent,
								Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
								RetryDirective: cliproxyexecutor.RetryDirectiveNextAuth,
								EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
							},
						}
						harness := newToolStreamFixtureHarness(
							t,
							path.home,
							[]string{"first-auth", "second-auth"},
							[]toolStreamFixtureAttempt{
								{err: notSentErr},
								{payload: []byte(toolStreamFixturePayload)},
							},
						)
						stream := harness.ExecuteStream(t)
						payload := <-stream.Chunks
						assertToolStreamFixturePayload(t, payload, "first-auth", "second-auth")
						payload.Acknowledge(nil)
						terminal := <-stream.Chunks
						if !terminal.Terminal || terminal.Acknowledge == nil {
							t.Fatalf("transport terminal = %#v", terminal)
						}
						terminal.Acknowledge(nil)
						for range stream.Chunks {
						}

						failure := readToolStreamFixtureResult(t, harness.hook)
						if failure.Success || failure.Disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseNotSent ||
							failure.Disposition.Outcome != cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing ||
							failure.Disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNextAuth {
							t.Fatalf("not-sent bootstrap result = %#v", failure)
						}
						success := readToolStreamFixtureResult(t, harness.hook)
						if !success.Success || success.Disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseDownstreamCommitted ||
							success.Disposition.Outcome != cliproxyexecutor.ExecutionOutcomeSucceeded {
							t.Fatalf("retried stream result = %#v", success)
						}
						assertNoMoreToolStreamFixtureResults(t, harness.hook)
						harness.AssertAttemptsAndReleases(t, 2)
					},
				)
			},
		)
	}
}
