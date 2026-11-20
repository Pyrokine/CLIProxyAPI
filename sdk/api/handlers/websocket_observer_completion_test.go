package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type completionWebSocketHost struct {
	*handlerWebSocketObserverHost
	completed chan struct{}
}

func (h *completionWebSocketHost) CompleteRequest(context.Context, pluginapi.RequestCompletion) {
	close(h.completed)
}

func TestWebSocketObserverRejectsEventsAfterExecutionCompletes(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		stream bool
		failed bool
	}{
		{name: "nonstream success"},
		{name: "nonstream failure", failed: true},
		{name: "stream success", stream: true},
		{name: "stream failure", stream: true, failed: true},
	} {
		t.Run(
			scenario.name, func(t *testing.T) {
				host := &completionWebSocketHost{
					handlerWebSocketObserverHost: &handlerWebSocketObserverHost{}, completed: make(chan struct{}),
				}
				var observer coreexecutor.WebSocketResponseObserver
				observe := func(options coreexecutor.Options) {
					observer = options.WebSocketResponseObserver
					if observer == nil {
						t.Fatal("observer was not installed")
					}
					observer(context.Background(), coreexecutor.WebSocketResponseEvent{EventType: "response.created"})
				}
				executor := &lifecycleCaptureExecutor{provider: "codex"}
				executor.execute = func(
					_ context.Context,
					_ *coreauth.Auth,
					_ coreexecutor.Request,
					options coreexecutor.Options,
				) (coreexecutor.Response, error) {
					observe(options)
					if scenario.failed {
						return coreexecutor.Response{}, errors.New("fixture execution failure")
					}
					return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
				}
				executor.stream = func(
					_ context.Context,
					_ *coreauth.Auth,
					_ coreexecutor.Request,
					options coreexecutor.Options,
				) (*coreexecutor.StreamResult, error) {
					observe(options)
					chunks := make(chan coreexecutor.StreamChunk, 2)
					if scenario.failed {
						chunks <- coreexecutor.StreamChunk{Err: errors.New("fixture stream failure")}
					} else {
						chunks <- coreexecutor.StreamChunk{Payload: []byte(`data: {"ok":true}` + "\n\n")}
						chunks <- coreexecutor.StreamChunk{Terminal: true}
					}
					close(chunks)
					return &coreexecutor.StreamResult{Chunks: chunks}, nil
				}
				const model = "fixture-ws-observer-completion"
				handler := newLifecycleHandler(t, model, executor)
				handler.SetPluginHost(host)
				if scenario.stream {
					data, _, failures := handler.ExecuteStreamWithAuthManager(
						t.Context(), "openai", model, []byte(`{"model":"fixture-ws-observer-completion"}`), "",
					)
					if data != nil {
						for range data {
						}
					}
					failed := false
					if failures != nil {
						for message := range failures {
							failed = failed || message != nil
						}
					}
					if failed != scenario.failed {
						t.Fatalf("stream failure = %t, want %t", failed, scenario.failed)
					}
				} else {
					_, _, message := handler.ExecuteWithAuthManager(
						t.Context(), "openai", model, []byte(`{"model":"fixture-ws-observer-completion"}`), "",
					)
					if (message != nil) != scenario.failed {
						t.Fatalf("execution error = %+v, failed = %t", message, scenario.failed)
					}
				}
				select {
				case <-host.completed:
				case <-time.After(5 * time.Second):
					t.Fatal("execution did not complete")
				}
				observer(context.Background(), coreexecutor.WebSocketResponseEvent{EventType: "late.event"})
				events, _ := host.snapshot()
				if len(events) != 1 || events[0].EventType != "response.created" {
					t.Fatalf("events after execution completion = %+v", events)
				}
				select {
				case <-host.closed:
				case <-time.After(5 * time.Second):
					t.Fatal("completed execution retained an open observer callback")
				}
			},
		)
	}
}
