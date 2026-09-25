package handlers

import (
	"context"
	"sync"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type handlerWebSocketObserverHost struct {
	mu       sync.Mutex
	events   []pluginapi.WebSocketResponseEvent
	skipID   string
	closed   chan struct{}
	closeOne sync.Once
}

func (*handlerWebSocketObserverHost) InterceptRequestBeforeAuth(
	context.Context,
	pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{}
}

func (*handlerWebSocketObserverHost) InterceptRequestAfterAuth(
	context.Context,
	pluginapi.RequestInterceptRequest,
) pluginapi.RequestInterceptResponse {
	return pluginapi.RequestInterceptResponse{}
}

func (*handlerWebSocketObserverHost) InterceptResponse(
	context.Context,
	pluginapi.ResponseInterceptRequest,
) pluginapi.ResponseInterceptResponse {
	return pluginapi.ResponseInterceptResponse{}
}

func (*handlerWebSocketObserverHost) InterceptStreamChunk(
	context.Context,
	pluginapi.StreamChunkInterceptRequest,
) pluginapi.StreamChunkInterceptResponse {
	return pluginapi.StreamChunkInterceptResponse{}
}

func (*handlerWebSocketObserverHost) HasRequestInterceptors() bool {
	return false
}

func (*handlerWebSocketObserverHost) HasStreamInterceptors() bool {
	return false
}

func (*handlerWebSocketObserverHost) HasWebSocketResponseObservers() bool {
	return true
}

func (h *handlerWebSocketObserverHost) OpenWebSocketResponseCallback(
	ctx context.Context,
	skipPluginID string,
) (pluginapi.WebSocketResponseCallback, func()) {
	h.mu.Lock()
	h.skipID = skipPluginID
	h.mu.Unlock()
	if h.closed == nil {
		h.closed = make(chan struct{})
	}
	var activeMu sync.RWMutex
	active := true
	closeCallback := func() {
		activeMu.Lock()
		active = false
		activeMu.Unlock()
		h.closeOne.Do(func() { close(h.closed) })
	}
	context.AfterFunc(ctx, closeCallback)
	return func(event pluginapi.WebSocketResponseEvent) {
		activeMu.RLock()
		isActive := active
		activeMu.RUnlock()
		if !isActive {
			return
		}
		h.mu.Lock()
		h.events = append(h.events, event)
		h.mu.Unlock()
	}, closeCallback
}

func (h *handlerWebSocketObserverHost) snapshot() ([]pluginapi.WebSocketResponseEvent, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]pluginapi.WebSocketResponseEvent(nil), h.events...), h.skipID
}

func TestExecuteWithAuthManagerWiresWebSocketResponseObserver(t *testing.T) {
	host := &handlerWebSocketObserverHost{}
	executor := &lifecycleCaptureExecutor{provider: "codex"}
	executor.execute = func(
		ctx context.Context,
		_ *coreauth.Auth,
		_ coreexecutor.Request,
		opts coreexecutor.Options,
	) (coreexecutor.Response, error) {
		if opts.WebSocketResponseObserver == nil {
			t.Fatal("WebSocketResponseObserver = nil")
		}
		opts.WebSocketResponseObserver(ctx, coreexecutor.WebSocketResponseEvent{
			Provider:  "codex",
			EventType: "response.created",
			Payload:   []byte(`{"type":"response.created"}`),
		})
		return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
	}
	handler := newLifecycleHandler(t, "handler-websocket-observer", executor)
	handler.SetPluginHost(host)

	if _, _, errMsg := handler.ExecuteWithAuthManager(
		context.Background(),
		"openai",
		"handler-websocket-observer",
		[]byte(`{"model":"handler-websocket-observer"}`),
		"",
	); errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager() error = %v", errMsg)
	}
	events, _ := host.snapshot()
	if len(events) != 1 || events[0].RequestID == "" || events[0].Provider != "codex" ||
		events[0].EventType != "response.created" {
		t.Fatalf("events = %+v", events)
	}
}

func TestWebSocketResponseObserverCallbackExpiresAndSkipsInitiatingPlugin(t *testing.T) {
	host := &handlerWebSocketObserverHost{}
	handler := NewBaseAPIHandlers(nil, nil)
	handler.SetPluginHost(host)
	observer := handler.webSocketResponseObserver(nil, "origin-plugin")
	if observer == nil {
		t.Fatal("webSocketResponseObserver() = nil")
	}
	ctx, cancel := context.WithCancel(context.Background())
	observer(ctx, coreexecutor.WebSocketResponseEvent{Payload: []byte(`{"type":"first"}`)})
	cancel()
	select {
	case <-host.closed:
	case <-time.After(time.Second):
		t.Fatal("callback did not close after request cancellation")
	}
	observer(ctx, coreexecutor.WebSocketResponseEvent{Payload: []byte(`{"type":"late"}`)})

	events, skipID := host.snapshot()
	if skipID != "origin-plugin" {
		t.Fatalf("skip plugin ID = %q", skipID)
	}
	if len(events) != 1 || string(events[0].Payload) != `{"type":"first"}` {
		t.Fatalf("events after cancellation = %+v", events)
	}
}
