package pluginhost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testHostHTTPOperation(t *testing.T, host *Host, caller context.Context, callbackID string) string {
	t.Helper()
	request, errMarshal := json.Marshal(map[string]string{"host_callback_id": callbackID})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	response, errOpen := host.callFromPlugin(caller, "host.http.operation_open", request)
	if errOpen != nil {
		t.Fatalf("open HTTP operation: %v", errOpen)
	}
	result, errDecode := decodeRPCEnvelope[rpcHostHTTPOperationOpenResponse](response)
	if errDecode != nil || result.OperationID == "" {
		t.Fatalf("operation response = %+v, error = %v", result, errDecode)
	}
	return result.OperationID
}

func testHostHTTPCancel(t *testing.T, host *Host, caller context.Context, operationID string) {
	t.Helper()
	request, errMarshal := json.Marshal(map[string]string{"operation_id": operationID})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if _, errCancel := host.callFromPlugin(caller, "host.http.cancel", request); errCancel != nil {
		t.Fatalf("cancel HTTP operation: %v", errCancel)
	}
}

func TestHostHTTPCancelBeforeStreamHeaders(t *testing.T) {
	started := make(chan struct{})
	upstreamDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(upstreamDone)
	}))
	defer server.Close()

	host := New()
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	instance := &hostCallbackInstance{}
	caller := withHostCallbackInstance(withHostCallbackInstanceID(withHostCallbackPluginID(context.Background(), "first"), 7), instance)
	callbackID, closeCallback := host.openCallbackContextForPluginInstance(parent, "first", instance)
	defer closeCallback()
	operationID := testHostHTTPOperation(t, host, caller, callbackID)

	request, errMarshal := json.Marshal(map[string]string{
		"host_callback_id": callbackID, "operation_id": operationID, "url": server.URL,
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	done := make(chan error, 1)
	go func() {
		_, errCall := host.callFromPlugin(caller, "host.http.do_stream", request)
		done <- errCall
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upstream HTTP request did not start")
	}

	testHostHTTPCancel(t, host, caller, operationID)
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop request before headers")
	}
	select {
	case errCall := <-done:
		if errCall == nil {
			t.Fatal("canceled request completed")
		}
	case <-time.After(time.Second):
		t.Fatal("host callback did not return after cancel")
	}
	if _, errCall := host.callFromPlugin(caller, "host.http.do_stream", request); errCall == nil {
		t.Fatal("canceled operation was reclaimed")
	}
}

func TestHostHTTPOperationRejectsForeignOwnerAndClosesWithCallback(t *testing.T) {
	host := New()
	instance := &hostCallbackInstance{}
	caller := withHostCallbackInstance(withHostCallbackInstanceID(withHostCallbackPluginID(context.Background(), "first"), 7), instance)
	callbackID, closeCallback := host.openCallbackContextForPluginInstance(context.Background(), "first", instance)
	defer closeCallback()
	operationID := testHostHTTPOperation(t, host, caller, callbackID)
	foreignPlugin := withHostCallbackInstance(withHostCallbackInstanceID(withHostCallbackPluginID(context.Background(), "second"), 7), instance)
	foreignInstance := withHostCallbackInstance(withHostCallbackInstanceID(withHostCallbackPluginID(context.Background(), "first"), 8), &hostCallbackInstance{})
	for _, other := range []context.Context{foreignPlugin, foreignInstance} {
		testHostHTTPCancel(t, host, other, operationID)
	}
	host.httpOperations.mu.Lock()
	operation := host.httpOperations.operations[hostHTTPOperationKey{pluginID: "first", operationID: operationID}]
	host.httpOperations.mu.Unlock()
	if operation == nil || operation.ctx.Err() != nil {
		t.Fatal("foreign owner canceled the operation")
	}
	request, errMarshal := json.Marshal(map[string]string{"host_callback_id": callbackID, "operation_id": operationID})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	for _, other := range []context.Context{foreignPlugin, foreignInstance} {
		if _, errCall := host.callFromPlugin(other, "host.http.do", request); errCall == nil {
			t.Fatal("foreign owner claimed HTTP operation")
		}
	}
	closeCallback()
	if _, errCall := host.callFromPlugin(caller, "host.http.do", request); errCall == nil {
		t.Fatal("operation survived its callback")
	}
}

func TestHostHTTPOperationCloseInstanceRejectsLateOpen(t *testing.T) {
	host := New()
	instance := &hostCallbackInstance{}
	caller := withHostCallbackInstance(withHostCallbackInstanceID(withHostCallbackPluginID(context.Background(), "first"), 7), instance)
	firstID := testHostHTTPOperation(t, host, caller, "")
	host.httpOperations.closeInstance("first", instance)
	request, errMarshal := json.Marshal(map[string]string{"operation_id": firstID})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if _, errCall := host.callFromPlugin(caller, "host.http.do", request); errCall == nil {
		t.Fatal("closed instance claimed an HTTP operation")
	}
	if _, errCall := host.callFromPlugin(caller, "host.http.operation_open", []byte(`{}`)); errCall == nil ||
		!strings.Contains(errCall.Error(), "closed") {
		t.Fatalf("late operation open error = %v, want closed instance", errCall)
	}
	fresh := withHostCallbackInstance(withHostCallbackInstanceID(withHostCallbackPluginID(context.Background(), "first"), 8), &hostCallbackInstance{})
	freshID := testHostHTTPOperation(t, host, fresh, "")
	testHostHTTPCancel(t, host, fresh, freshID)
}

func TestHostHTTPCancelWhileReadingBody(t *testing.T) {
	started := make(chan struct{})
	upstreamDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		close(started)
		<-request.Context().Done()
		close(upstreamDone)
	}))
	defer server.Close()

	host := New()
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	instance := &hostCallbackInstance{}
	caller := withHostCallbackInstance(withHostCallbackPluginID(context.Background(), "first"), instance)
	callbackID, closeCallback := host.openCallbackContextForPluginInstance(parent, "first", instance)
	defer closeCallback()
	operationID := testHostHTTPOperation(t, host, caller, callbackID)
	request, errMarshal := json.Marshal(map[string]string{
		"host_callback_id": callbackID, "operation_id": operationID, "url": server.URL,
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	done := make(chan error, 1)
	go func() {
		_, errCall := host.callFromPlugin(caller, "host.http.do", request)
		done <- errCall
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("HTTP body did not start")
	}
	testHostHTTPCancel(t, host, caller, operationID)
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Fatal("cancel did not stop body read")
	}
	select {
	case errCall := <-done:
		if errCall == nil {
			t.Fatal("canceled body read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("host callback did not return after cancel")
	}
}

func TestHostHTTPCancelAfterStreamHeadersReleasesResources(t *testing.T) {
	upstreamDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("ready"))
		w.(http.Flusher).Flush()
		<-request.Context().Done()
		close(upstreamDone)
	}))
	defer server.Close()

	host := New()
	instance := &hostCallbackInstance{}
	caller := withHostCallbackInstance(withHostCallbackInstanceID(withHostCallbackPluginID(context.Background(), "first"), 7), instance)
	callbackID, closeCallback := host.openCallbackContextForPluginInstance(context.Background(), "first", instance)
	defer closeCallback()
	operationID := testHostHTTPOperation(t, host, caller, callbackID)
	request, errMarshal := json.Marshal(map[string]string{
		"host_callback_id": callbackID, "operation_id": operationID, "url": server.URL,
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	response, errCall := host.callFromPlugin(caller, "host.http.do_stream", request)
	if errCall != nil {
		t.Fatalf("open HTTP stream: %v", errCall)
	}
	stream, errDecode := decodeRPCEnvelope[rpcHostHTTPStreamResponse](response)
	if errDecode != nil || stream.StreamID == "" {
		t.Fatalf("stream response = %+v, error = %v", stream, errDecode)
	}
	testHostHTTPCancel(t, host, caller, operationID)
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Fatal("cancel did not close active stream")
	}
	if hasHostHTTPStreams(host.httpStreams) {
		t.Fatal("stream was not removed after cancel")
	}
	host.httpOperations.mu.Lock()
	remaining := len(host.httpOperations.operations)
	host.httpOperations.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d canceled operations remain", remaining)
	}
	readRequest, errMarshal := json.Marshal(rpcHostHTTPStreamReadRequest{StreamID: stream.StreamID})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if _, errRead := host.callFromPlugin(caller, "host.http.stream_read", readRequest); errRead == nil {
		t.Fatal("canceled stream is still readable")
	}
}

func TestHostHTTPOperationFinishesWithoutAccumulatingCallbackCleanups(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	host := New()
	instance := &hostCallbackInstance{}
	caller := withHostCallbackInstance(withHostCallbackPluginID(context.Background(), "first"), instance)
	callbackID, closeCallback := host.openCallbackContextForPluginInstance(context.Background(), "first", instance)
	defer closeCallback()
	seen := make(map[string]struct{})
	for range 25 {
		operationID := testHostHTTPOperation(t, host, caller, callbackID)
		if _, reused := seen[operationID]; reused {
			t.Fatalf("reused completed operation ID %q", operationID)
		}
		seen[operationID] = struct{}{}
		request, errMarshal := json.Marshal(map[string]string{
			"host_callback_id": callbackID, "operation_id": operationID, "url": server.URL,
		})
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		if _, errCall := host.callFromPlugin(caller, "host.http.do", request); errCall != nil {
			t.Fatalf("complete HTTP operation: %v", errCall)
		}
		host.httpOperations.mu.Lock()
		remaining := len(host.httpOperations.operations)
		host.httpOperations.mu.Unlock()
		host.callbackContexts.mu.RLock()
		cleanupCount := len(host.callbackContexts.contexts[callbackID].cleanup)
		host.callbackContexts.mu.RUnlock()
		if remaining != 0 || cleanupCount != 0 {
			t.Fatalf("completed operation left %d entries and %d callback cleanups", remaining, cleanupCount)
		}
	}
}

func TestHostHTTPOperationParentCancellationRemovesCallbackCleanup(t *testing.T) {
	host := New()
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	instance := &hostCallbackInstance{}
	caller := withHostCallbackInstance(withHostCallbackPluginID(context.Background(), "first"), instance)
	callbackID, closeCallback := host.openCallbackContextForPluginInstance(parent, "first", instance)
	defer closeCallback()
	testHostHTTPOperation(t, host, caller, callbackID)
	cancelParent()
	deadline := time.After(time.Second)
	for {
		host.httpOperations.mu.Lock()
		remaining := len(host.httpOperations.operations)
		host.httpOperations.mu.Unlock()
		host.callbackContexts.mu.RLock()
		cleanupCount := len(host.callbackContexts.contexts[callbackID].cleanup)
		host.callbackContexts.mu.RUnlock()
		if remaining == 0 && cleanupCount == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("canceled operation remains: %d entries, %d callback cleanups", remaining, cleanupCount)
		case <-time.After(time.Millisecond):
		}
	}
}
