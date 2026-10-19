package pluginhost

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func openModelStreamForTest(t *testing.T, host *Host, ctx context.Context, callbackID string) string {
	t.Helper()
	request, err := json.Marshal(rpcHostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{Stream: true, Model: "synthetic-model"},
		HostCallbackID:            callbackID,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := host.callFromPlugin(ctx, pluginabi.MethodHostModelExecuteStream, request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := decodeRPCEnvelope[pluginapi.HostModelStreamResponse](result)
	if err != nil || response.StreamID == "" {
		t.Fatalf("stream response = %#v, error = %v", response, err)
	}
	return response.StreamID
}

func TestHostModelStreamRejectsForeignOwner(t *testing.T) {
	for _, method := range []string{pluginabi.MethodHostModelStreamRead, pluginabi.MethodHostModelStreamClose} {
		for _, other := range []string{"plugin", "instance"} {
			t.Run(method+"/"+other, func(t *testing.T) {
				host := New()
				instance := &hostCallbackInstance{id: 101}
				owner := withHostCallbackInstance(withHostCallbackInstanceID(withHostCallbackPluginID(context.Background(), "first"), instance.id), instance)
				callbackID, closeCallback := host.openCallbackContextForPluginInstance(owner, "first", instance)
				t.Cleanup(closeCallback)
				chunks := make(chan handlers.ModelExecutionChunk, 1)
				chunks <- handlers.ModelExecutionChunk{Payload: []byte("owner-payload")}
				var streamContext context.Context
				host.SetModelExecutor(&fakeHostModelExecutor{executeModelStream: func(ctx context.Context, _ handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *handlers.ErrorMessage) {
					streamContext = ctx
					return handlers.ModelExecutionStream{Chunks: chunks}, nil
				}})
				id := openModelStreamForTest(t, host, owner, callbackID)
				foreign := withHostCallbackInstanceID(withHostCallbackPluginID(context.Background(), "second"), instance.id)
				if other == "instance" {
					foreign = withHostCallbackInstanceID(withHostCallbackPluginID(context.Background(), "first"), 102)
				}
				request, err := json.Marshal(pluginapi.HostModelStreamReadRequest{StreamID: id})
				if err != nil {
					t.Fatal(err)
				}
				if result, err := host.callFromPlugin(foreign, method, request); err == nil {
					t.Fatalf("foreign owner accessed the model stream: result=%s", result)
				}
				if streamContext.Err() != nil {
					t.Fatal("foreign close cancelled the owner stream")
				}
				result, err := host.callFromPlugin(owner, pluginabi.MethodHostModelStreamRead, request)
				if err != nil {
					t.Fatal(err)
				}
				response, err := decodeRPCEnvelope[pluginapi.HostModelStreamReadResponse](result)
				if err != nil || string(response.Payload) != "owner-payload" {
					t.Fatalf("owner response = %#v, error = %v", response, err)
				}
			})
		}
	}
}

func TestHostModelStreamReleasesOnParentCancelAndInstanceClose(t *testing.T) {
	for _, reason := range []string{"parent", "instance"} {
		t.Run(reason, func(t *testing.T) {
			host := New()
			parent, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			instance := &hostCallbackInstance{id: 101}
			owner := withHostCallbackInstance(withHostCallbackInstanceID(withHostCallbackPluginID(parent, "first"), instance.id), instance)
			var streamContext context.Context
			host.SetModelExecutor(&fakeHostModelExecutor{executeModelStream: func(ctx context.Context, _ handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *handlers.ErrorMessage) {
				streamContext = ctx
				return handlers.ModelExecutionStream{Chunks: make(chan handlers.ModelExecutionChunk)}, nil
			}})
			openModelStreamForTest(t, host, owner, "")
			if reason == "parent" {
				cancel()
			} else {
				instance.closed.Store(true)
				host.closeHostCallbackInstances("first", []*hostCallbackInstance{instance})
			}
			deadline := time.After(time.Second)
			for hostModelStreamCountForTest(t, host) != 0 {
				select {
				case <-deadline:
					t.Fatal("cancelled model stream remains registered")
				case <-time.After(time.Millisecond):
				}
			}
			if streamContext.Err() == nil {
				t.Fatal("released stream context was not cancelled")
			}
		})
	}
}

func TestHostModelStreamCompletionRemovesCallbackCleanup(t *testing.T) {
	host := New()
	callbackID, closeCallback := host.openCallbackContext(context.Background())
	t.Cleanup(closeCallback)
	host.SetModelExecutor(&fakeHostModelExecutor{executeModelStream: func(_ context.Context, _ handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *handlers.ErrorMessage) {
		chunks := make(chan handlers.ModelExecutionChunk)
		close(chunks)
		return handlers.ModelExecutionStream{Chunks: chunks}, nil
	}})
	for range 20 {
		id := openModelStreamForTest(t, host, context.Background(), callbackID)
		request, err := json.Marshal(pluginapi.HostModelStreamReadRequest{StreamID: id})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelStreamRead, request); err != nil {
			t.Fatal(err)
		}
	}
	host.callbackContexts.mu.RLock()
	count := len(host.callbackContexts.contexts[callbackID].cleanup)
	host.callbackContexts.mu.RUnlock()
	if count != 0 {
		t.Fatalf("completed model streams left %d callback cleanups", count)
	}
}

func TestHostModelStreamRejectsLateOpenAfterInstanceClose(t *testing.T) {
	host := New()
	instance := &hostCallbackInstance{id: 101}
	owner := withHostCallbackInstance(withHostCallbackPluginID(context.Background(), "first"), instance)
	var streamContext context.Context
	host.SetModelExecutor(&fakeHostModelExecutor{executeModelStream: func(ctx context.Context, _ handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *handlers.ErrorMessage) {
		streamContext = ctx
		instance.closed.Store(true)
		host.closeHostCallbackInstances("first", []*hostCallbackInstance{instance})
		return handlers.ModelExecutionStream{Chunks: make(chan handlers.ModelExecutionChunk)}, nil
	}})
	request, err := json.Marshal(pluginapi.HostModelExecutionRequest{Stream: true, Model: "synthetic-model"})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := host.callFromPlugin(owner, pluginabi.MethodHostModelExecuteStream, request); err == nil {
		t.Fatalf("closed instance registered a late model stream: %s", result)
	}
	if hostModelStreamCountForTest(t, host) != 0 || streamContext.Err() == nil {
		t.Fatal("late model stream retained resources after instance close")
	}
}

func TestHostModelStreamCloseReleasesBlockedRead(t *testing.T) {
	host := New()
	owner := withHostCallbackPluginID(context.Background(), "first")
	callbackID, closeCallback := host.openCallbackContextForPlugin(owner, "first")
	t.Cleanup(closeCallback)
	host.SetModelExecutor(&fakeHostModelExecutor{executeModelStream: func(_ context.Context, _ handlers.ModelExecutionRequest) (handlers.ModelExecutionStream, *handlers.ErrorMessage) {
		return handlers.ModelExecutionStream{Chunks: make(chan handlers.ModelExecutionChunk)}, nil
	}})
	id := openModelStreamForTest(t, host, owner, callbackID)
	request, err := json.Marshal(pluginapi.HostModelStreamReadRequest{StreamID: id})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		close(started)
		_, err := host.callFromPlugin(owner, pluginabi.MethodHostModelStreamRead, request)
		finished <- err
	}()
	<-started
	if _, err := host.callFromPlugin(owner, pluginabi.MethodHostModelStreamClose, request); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("closed model stream read returned success")
		}
	case <-time.After(time.Second):
		t.Fatal("closed model stream left a blocked reader")
	}
}

func TestHostModelCallbackPreservesSignatureAndOpaqueFieldsAcrossTurns(t *testing.T) {
	host := New()
	owner := withHostCallbackPluginID(context.Background(), "first")
	callbackID, closeCallback := host.openCallbackContextForPlugin(owner, "first")
	t.Cleanup(closeCallback)
	responseBody := []byte(`{"role":"assistant","content":[{"type":"thinking","thinking":"synthetic","signature":"claude#opaque-signature=="},{"type":"tool_use","id":"call-a","name":"lookup","input":{"value":1}}],"opaque":{"raw":"\\u0041","source_index":7,"auth_index":"fixture-index"}}`)
	requests := make([]handlers.ModelExecutionRequest, 0, 2)
	host.SetModelExecutor(&fakeHostModelExecutor{executeModel: func(_ context.Context, request handlers.ModelExecutionRequest) (handlers.ModelExecutionResponse, *handlers.ErrorMessage) {
		requests = append(requests, request)
		return handlers.ModelExecutionResponse{StatusCode: 200, Body: responseBody}, nil
	}})
	body := []byte(`{"model":"synthetic-model","messages":[{"role":"user","content":"synthetic query"}]}`)
	for turn := range 2 {
		request, err := json.Marshal(rpcHostModelExecutionRequest{
			HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
				EntryProtocol: "claude", ExitProtocol: "claude", Model: "synthetic-model",
				ForcedProvider: "claude", AuthID: "fixture-auth", Body: body,
			},
			HostCallbackID: callbackID,
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := host.callFromPlugin(owner, pluginabi.MethodHostModelExecute, request)
		if err != nil {
			t.Fatal(err)
		}
		response, err := decodeRPCEnvelope[pluginapi.HostModelExecutionResponse](result)
		if err != nil || string(response.Body) != string(responseBody) {
			t.Fatalf("turn %d changed the raw provider response: %s, error=%v", turn, response.Body, err)
		}
		if turn == 0 {
			body = append([]byte(`{"model":"synthetic-model","messages":[`), response.Body...)
			body = append(body, []byte(`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-a","content":"synthetic result"}]}]}`)...)
		}
	}
	if len(requests) != 2 || string(requests[1].Body) != string(body) {
		t.Fatal("second turn lost opaque response fields or tool history")
	}
	for _, request := range requests {
		if request.AuthID != "fixture-auth" || request.ForcedProvider != "claude" || request.SkipInterceptorPluginID != "first" {
			t.Fatalf("nested execution changed credential or caller identity: %#v", request)
		}
	}
}
