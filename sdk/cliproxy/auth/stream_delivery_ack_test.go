package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type streamDeliveryResultHook struct {
	result chan Result
}

func (h *streamDeliveryResultHook) OnAuthRegistered(context.Context, *Auth) {}
func (h *streamDeliveryResultHook) OnAuthUpdated(context.Context, *Auth)    {}

func (h *streamDeliveryResultHook) OnResult(_ context.Context, result Result) {
	select {
	case h.result <- result:
	default:
	}
}

func newStreamDeliveryResultHook() *streamDeliveryResultHook {
	return &streamDeliveryResultHook{result: make(chan Result, 4)}
}

func assertNoStreamDeliveryResult(t *testing.T, hook *streamDeliveryResultHook) {
	t.Helper()
	select {
	case result := <-hook.result:
		t.Fatalf("unexpected stream result before acknowledgement: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestWrapStreamResultWaitsForPayloadAndTerminalAcknowledgements(t *testing.T) {
	hook := newStreamDeliveryResultHook()
	manager := NewManager(nil, nil, hook)
	ctx := coreexecutor.WithStreamDeliveryAcknowledgement(context.Background())
	originalAcknowledged := make(chan error, 1)
	source := make(chan coreexecutor.StreamChunk, 1)
	source <- coreexecutor.StreamChunk{
		Payload: []byte("payload"),
		Acknowledge: func(err error) {
			originalAcknowledged <- err
		},
	}
	close(source)

	stream := manager.wrapStreamResult(
		ctx,
		&Auth{ID: "runtime-stream", Provider: "codex"},
		"codex",
		"gpt-5",
		nil,
		nil,
		source,
		OAuthModelAliasResult{},
	)
	payload := <-stream.Chunks
	if string(payload.Payload) != "payload" || payload.Acknowledge == nil || payload.Terminal {
		t.Fatalf("payload chunk = %#v", payload)
	}
	assertNoStreamDeliveryResult(t, hook)
	select {
	case chunk := <-stream.Chunks:
		t.Fatalf("received next chunk before payload acknowledgement: %#v", chunk)
	case <-time.After(20 * time.Millisecond):
	}

	payload.Acknowledge(nil)
	if err := <-originalAcknowledged; err != nil {
		t.Fatalf("original acknowledgement error = %v", err)
	}
	terminal := <-stream.Chunks
	if !terminal.Terminal || terminal.Acknowledge == nil || len(terminal.Payload) != 0 {
		t.Fatalf("terminal chunk = %#v", terminal)
	}
	assertNoStreamDeliveryResult(t, hook)
	terminal.Acknowledge(nil)
	for range stream.Chunks {
	}

	select {
	case result := <-hook.result:
		if !result.Success || !result.RuntimeOnly {
			t.Fatalf("stream result = %#v, want runtime success", result)
		}
	case <-time.After(time.Second):
		t.Fatal("stream success was not recorded after terminal acknowledgement")
	}
}

func TestWrapStreamResultTreatsDeliveryFailureAsRequestScoped(t *testing.T) {
	hook := newStreamDeliveryResultHook()
	manager := NewManager(nil, nil, hook)
	auth, errRegister := manager.Register(
		context.Background(), &Auth{
			ID:       "managed-stream",
			Provider: "codex",
			Status:   StatusActive,
		},
	)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	ctx := coreexecutor.WithStreamDeliveryAcknowledgement(context.Background())
	source := make(chan coreexecutor.StreamChunk, 1)
	source <- coreexecutor.StreamChunk{Payload: []byte("payload")}
	close(source)
	stream := manager.wrapStreamResult(
		ctx, auth.Clone(), auth.Provider, "gpt-5", nil, nil, source, OAuthModelAliasResult{},
	)
	chunk := <-stream.Chunks
	writeErr := errors.New("downstream write failed")
	chunk.Acknowledge(writeErr)
	for range stream.Chunks {
	}

	select {
	case result := <-hook.result:
		if result.Success || result.Error == nil || !result.Error.IsRequestScoped() {
			t.Fatalf("delivery result = %#v, want request-scoped failure", result)
		}
		if result.Error.Message != writeErr.Error() {
			t.Fatalf("delivery result error = %q, want %q", result.Error.Message, writeErr.Error())
		}
	case <-time.After(time.Second):
		t.Fatal("delivery failure was not recorded")
	}
	current, ok := manager.GetByID(auth.ID)
	if !ok || current.Status != StatusActive || current.Unavailable {
		t.Fatalf("auth changed availability after downstream failure: %#v", current)
	}
	if len(current.ModelStates) != 0 {
		t.Fatalf("downstream failure changed model state: %#v", current.ModelStates)
	}
}

func TestWrapStreamResultWaitsForTerminalErrorDelivery(t *testing.T) {
	hook := newStreamDeliveryResultHook()
	manager := NewManager(nil, nil, hook)
	ctx := coreexecutor.WithStreamDeliveryAcknowledgement(context.Background())
	upstreamErr := &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"}
	source := make(chan coreexecutor.StreamChunk, 1)
	source <- coreexecutor.StreamChunk{Err: upstreamErr}
	close(source)
	stream := manager.wrapStreamResult(
		ctx,
		&Auth{ID: "runtime-stream", Provider: "codex"},
		"codex",
		"gpt-5",
		nil,
		nil,
		source,
		OAuthModelAliasResult{},
	)

	chunk := <-stream.Chunks
	if chunk.Err == nil || chunk.Acknowledge == nil {
		t.Fatalf("terminal error chunk = %#v", chunk)
	}
	assertNoStreamDeliveryResult(t, hook)
	chunk.Acknowledge(nil)
	for range stream.Chunks {
	}
	select {
	case result := <-hook.result:
		if result.Success || result.Error == nil || result.Error.HTTPStatus != http.StatusTooManyRequests {
			t.Fatalf("upstream failure result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream failure was not recorded after terminal error delivery")
	}
}

func TestWrapStreamResultAcknowledgesCancellationWhileWaitingForDelivery(t *testing.T) {
	hook := newStreamDeliveryResultHook()
	manager := NewManager(nil, nil, hook)
	ctx, cancel := context.WithCancel(
		coreexecutor.WithStreamDeliveryAcknowledgement(context.Background()),
	)
	originalAcknowledged := make(chan error, 1)
	source := make(chan coreexecutor.StreamChunk, 1)
	source <- coreexecutor.StreamChunk{
		Payload: []byte("payload"),
		Acknowledge: func(err error) {
			originalAcknowledged <- err
		},
	}
	close(source)
	stream := manager.wrapStreamResult(
		ctx,
		&Auth{ID: "runtime-stream", Provider: "codex"},
		"codex",
		"gpt-5",
		nil,
		nil,
		source,
		OAuthModelAliasResult{},
	)

	chunk := <-stream.Chunks
	if chunk.Acknowledge == nil {
		t.Fatal("payload acknowledgement is nil")
	}
	cancel()
	for range stream.Chunks {
	}
	if err := <-originalAcknowledged; !errors.Is(err, context.Canceled) {
		t.Fatalf("original acknowledgement error = %v, want context canceled", err)
	}
	select {
	case result := <-hook.result:
		if result.Success || result.Error == nil || !result.Error.IsRequestScoped() {
			t.Fatalf("cancellation result = %#v, want request-scoped failure", result)
		}
	case <-time.After(time.Second):
		t.Fatal("stream cancellation was not recorded")
	}
}

func TestWrapStreamResultCancellationWhileWaitingForSourceReleasesQuotaLease(t *testing.T) {
	hook := newStreamDeliveryResultHook()
	manager := NewManager(nil, &QuotaAwareSelector{}, hook)
	auth, errRegister := manager.Register(
		context.Background(),
		&Auth{ID: "stalled-stream", Index: "stalled-index", Provider: "codex", Status: StatusActive},
	)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	manager.scheduler.mu.Lock()
	lease := manager.scheduler.acquireQuotaInFlightLocked(auth, auth.Provider, time.Now())
	manager.scheduler.mu.Unlock()
	if lease == nil {
		t.Fatal("quota in-flight lease was not created")
	}
	ctx, cancel := context.WithCancel(
		coreexecutor.WithStreamDeliveryAcknowledgement(context.Background()),
	)
	ctx, transfer := withQuotaInFlightTransfer(ctx, lease)
	if transfer == nil {
		t.Fatal("quota in-flight lease transfer was not created")
	}
	source := make(chan coreexecutor.StreamChunk)
	stream := manager.wrapStreamResult(
		ctx, auth.Clone(), auth.Provider, "gpt-5", nil, nil, source, OAuthModelAliasResult{},
	)
	waitForQuotaInFlightCounts(t, manager, 1)

	cancel()
	select {
	case _, ok := <-stream.Chunks:
		if ok {
			t.Fatal("wrapper emitted a chunk after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("wrapper output did not close after source wait cancellation")
	}
	waitForQuotaInFlightCounts(t, manager, 0)

	select {
	case result := <-hook.result:
		if result.Success || result.Error == nil || !result.Error.IsRequestScoped() {
			t.Fatalf("cancellation result = %#v, want request-scoped failure", result)
		}
	case <-time.After(time.Second):
		t.Fatal("source wait cancellation was not recorded")
	}
	current, ok := manager.GetByID(auth.ID)
	if !ok || current.Status != StatusActive || current.Unavailable || len(current.ModelStates) != 0 {
		t.Fatalf("auth changed availability after source wait cancellation: %#v", current)
	}
}

func TestHomeStreamScopeEndsAfterSourceWaitCancellationOnce(t *testing.T) {
	registry := executionregistry.New()
	pending, errBegin := registry.BeginDispatch()
	if errBegin != nil {
		t.Fatalf("begin dispatch: %v", errBegin)
	}
	released := 0
	registry.SetReleaseSink(
		func(executionregistry.ReleaseGroup, int64) {
			released++
		},
	)
	scope, errInstall := registry.Install(
		pending, executionregistry.ScopeSpec{
			RequestID:    "request",
			CredentialID: "credential",
			Model:        "gpt-5",
			Accounted:    true,
		},
	)
	if errInstall != nil {
		t.Fatalf("install scope: %v", errInstall)
	}
	selection, errSelection := newHomeDispatchSelection(
		&Auth{ID: "home-auth", Provider: "codex"}, nil, "codex", scope,
	)
	if errSelection != nil {
		t.Fatalf("new selection: %v", errSelection)
	}
	manager := NewManager(nil, nil, nil)
	ctx, cancel := context.WithCancel(
		coreexecutor.WithStreamDeliveryAcknowledgement(context.Background()),
	)
	source := make(chan coreexecutor.StreamChunk)
	stream := manager.wrapStreamResult(
		ctx,
		&Auth{ID: "home-auth", Provider: "codex"},
		"codex",
		"gpt-5",
		nil,
		nil,
		source,
		OAuthModelAliasResult{},
	)
	owned := wrapHomeOwnedStream(ctx, stream, selection, func() {}, true)

	cancel()
	select {
	case _, ok := <-owned.Chunks:
		if ok {
			t.Fatal("home stream emitted a chunk after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("home stream did not close after source wait cancellation")
	}
	if selection.Active() || released != 1 {
		t.Fatalf("selection state after source wait cancellation: active=%v released=%d", selection.Active(), released)
	}
	selection.End("duplicate")
	if released != 1 {
		t.Fatalf("scope released %d times, want once", released)
	}
}

func TestHomeStreamScopeEndsAfterDeliveryFailureOnce(t *testing.T) {
	registry := executionregistry.New()
	pending, errBegin := registry.BeginDispatch()
	if errBegin != nil {
		t.Fatalf("begin dispatch: %v", errBegin)
	}
	released := 0
	registry.SetReleaseSink(
		func(executionregistry.ReleaseGroup, int64) {
			released++
		},
	)
	scope, errInstall := registry.Install(
		pending, executionregistry.ScopeSpec{
			RequestID:    "request",
			CredentialID: "credential",
			Model:        "gpt-5",
			Accounted:    true,
		},
	)
	if errInstall != nil {
		t.Fatalf("install scope: %v", errInstall)
	}
	selection, errSelection := newHomeDispatchSelection(
		&Auth{ID: "home-auth", Provider: "codex"}, nil, "codex", scope,
	)
	if errSelection != nil {
		t.Fatalf("new selection: %v", errSelection)
	}
	manager := NewManager(nil, nil, nil)
	ctx := coreexecutor.WithStreamDeliveryAcknowledgement(context.Background())
	source := make(chan coreexecutor.StreamChunk, 1)
	source <- coreexecutor.StreamChunk{Payload: []byte("payload")}
	close(source)
	stream := manager.wrapStreamResult(
		ctx,
		&Auth{ID: "home-auth", Provider: "codex"},
		"codex",
		"gpt-5",
		nil,
		nil,
		source,
		OAuthModelAliasResult{},
	)
	owned := wrapHomeOwnedStream(ctx, stream, selection, func() {}, true)

	payload := <-owned.Chunks
	payload.Acknowledge(errors.New("downstream write failed"))
	for range owned.Chunks {
	}
	if selection.Active() || released != 1 {
		t.Fatalf("selection state after delivery failure: active=%v released=%d", selection.Active(), released)
	}
	selection.End("duplicate")
	if released != 1 {
		t.Fatalf("scope released %d times, want once", released)
	}
}

func TestHomeStreamScopeEndsAfterTerminalAcknowledgementOnce(t *testing.T) {
	registry := executionregistry.New()
	pending, errBegin := registry.BeginDispatch()
	if errBegin != nil {
		t.Fatalf("begin dispatch: %v", errBegin)
	}
	released := 0
	registry.SetReleaseSink(
		func(executionregistry.ReleaseGroup, int64) {
			released++
		},
	)
	scope, errInstall := registry.Install(
		pending, executionregistry.ScopeSpec{
			RequestID:    "request",
			CredentialID: "credential",
			Model:        "gpt-5",
			Accounted:    true,
		},
	)
	if errInstall != nil {
		t.Fatalf("install scope: %v", errInstall)
	}
	selection, errSelection := newHomeDispatchSelection(
		&Auth{ID: "home-auth", Provider: "codex"}, nil, "codex", scope,
	)
	if errSelection != nil {
		t.Fatalf("new selection: %v", errSelection)
	}
	manager := NewManager(nil, nil, nil)
	ctx := coreexecutor.WithStreamDeliveryAcknowledgement(context.Background())
	source := make(chan coreexecutor.StreamChunk, 1)
	source <- coreexecutor.StreamChunk{Payload: []byte("payload")}
	close(source)
	stream := manager.wrapStreamResult(
		ctx,
		&Auth{ID: "home-auth", Provider: "codex"},
		"codex",
		"gpt-5",
		nil,
		nil,
		source,
		OAuthModelAliasResult{},
	)
	owned := wrapHomeOwnedStream(ctx, stream, selection, func() {}, true)

	payload := <-owned.Chunks
	payload.Acknowledge(nil)
	terminal := <-owned.Chunks
	if !terminal.Terminal {
		t.Fatalf("terminal chunk = %#v", terminal)
	}
	if !selection.Active() || released != 0 {
		t.Fatalf("selection ended before terminal acknowledgement: active=%v released=%d", selection.Active(), released)
	}
	terminal.Acknowledge(nil)
	for range owned.Chunks {
	}
	if selection.Active() || released != 1 {
		t.Fatalf("selection state after terminal acknowledgement: active=%v released=%d", selection.Active(), released)
	}
	selection.End("duplicate")
	if released != 1 {
		t.Fatalf("scope released %d times, want once", released)
	}
}
