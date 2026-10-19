package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type blockingHTTPPluginLoader struct {
	url string
}

func (loader blockingHTTPPluginLoader) Open(file pluginFile, host *Host) (pluginClient, error) {
	instance := &hostCallbackInstance{}
	host.registerHostCallbackInstance(file.ID, instance)
	caller := withHostCallbackInstance(withHostCallbackInstanceID(withHostCallbackPluginID(context.Background(), file.ID), 7), instance)
	operation, err := host.callFromPlugin(caller, "host.http.operation_open", []byte(`{}`))
	if err != nil {
		return nil, err
	}
	opened, err := decodeRPCEnvelope[rpcHostHTTPOperationOpenResponse](operation)
	if err != nil {
		return nil, err
	}
	request, err := json.Marshal(map[string]string{"operation_id": opened.OperationID, "url": loader.url})
	if err != nil {
		return nil, err
	}
	_, err = host.callFromPlugin(caller, "host.http.do_stream", request)
	return nil, err
}

func TestCancelledPluginLoadCancelsHTTPBeforeInitializationReturns(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(stopped)
	}))
	defer server.Close()

	host := newForTest(blockingHTTPPluginLoader{url: server.URL})
	cfg := pluginRuntimeConfigForTest(makePluginDir(t, "alpha"), 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- host.ApplyConfig(ctx, cfg)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("loading plugin did not begin HTTP request")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("cancelling plugin initialization did not cancel host HTTP")
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ApplyConfig error = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("plugin initialization did not return after cancellation")
	}
}

func TestUnloadDuringPluginInitializationClosesCurrentAndLateHTTPInstances(t *testing.T) {
	host := New()
	host.initializing["alpha"] = &pluginInitialization{done: make(chan struct{})}
	host.loading["alpha"] = struct{}{}
	instance := &hostCallbackInstance{}
	host.registerHostCallbackInstance("alpha", instance)
	caller := withHostCallbackInstance(withHostCallbackPluginID(context.Background(), "alpha"), instance)
	operationID := testHostHTTPOperation(t, host, caller, "")

	unloaded, err := host.UnloadPluginWithError("alpha")
	if !unloaded || !errors.Is(err, errPluginInitializationInProgress) || !instance.closed.Load() {
		t.Fatalf("unload = %t, error = %v, instance closed = %t", unloaded, err, instance.closed.Load())
	}
	host.httpOperations.mu.Lock()
	_, remains := host.httpOperations.operations[hostHTTPOperationKey{pluginID: "alpha", operationID: operationID}]
	host.httpOperations.mu.Unlock()
	if remains {
		t.Fatal("HTTP operation survived plugin unload")
	}
	if _, errOpen := host.callFromPlugin(caller, "host.http.operation_open", []byte(`{}`)); errOpen == nil {
		t.Fatal("unloaded initialization opened a new HTTP operation")
	}
	late := &hostCallbackInstance{}
	host.registerHostCallbackInstance("alpha", late)
	if !late.closed.Load() {
		t.Fatal("late plugin callback instance remained open after unload")
	}
}
