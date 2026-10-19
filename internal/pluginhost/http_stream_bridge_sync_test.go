package pluginhost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func hasHostHTTPStreams(bridge *hostHTTPStreamBridge) bool {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return len(bridge.streams) > 0
}

func TestHostHTTPStreamBridgeScopesStreamsToPluginInstance(t *testing.T) {
	bridge := newHostHTTPStreamBridge()
	ctx, cancel := context.WithCancel(context.Background())
	streamID := bridge.open(ctx, "first", 7, make(chan pluginapi.HTTPStreamChunk), cancel, nil)
	if streamID == "" {
		t.Fatal("stream was not opened")
	}
	for _, owner := range []struct {
		pluginID   string
		instanceID uintptr
	}{
		{pluginID: "second", instanceID: 7},
		{pluginID: "first", instanceID: 8},
	} {
		if _, _, errRead := bridge.read(ctx, owner.pluginID, owner.instanceID, streamID); errRead == nil {
			t.Fatalf("stream %q was readable by %q/%d", streamID, owner.pluginID, owner.instanceID)
		}
		bridge.close(owner.pluginID, owner.instanceID, streamID)
	}
	if !hasHostHTTPStreams(bridge) {
		t.Fatal("a different owner closed the stream")
	}
	bridge.closeInstance("first", 7)
	if hasHostHTTPStreams(bridge) || ctx.Err() == nil {
		t.Fatal("closing the owner did not cancel its stream")
	}
}

func TestHostHTTPStreamBridgeCancelsOnParentAndPluginShutdown(t *testing.T) {
	bridge := newHostHTTPStreamBridge()
	ctx, cancel := context.WithCancel(context.Background())
	streamID := bridge.open(ctx, "first", 7, make(chan pluginapi.HTTPStreamChunk), cancel, nil)
	cancel()
	deadline := time.After(time.Second)
	for {
		bridge.mu.Lock()
		_, open := bridge.streams[hostHTTPStreamKey{pluginID: "first", instanceID: 7, streamID: streamID}]
		bridge.mu.Unlock()
		if !open {
			break
		}
		select {
		case <-deadline:
			t.Fatal("stream remained registered after parent cancellation")
		case <-time.After(time.Millisecond):
		}
	}
	ctx, cancel = context.WithCancel(context.Background())
	bridge.open(ctx, "first", 8, make(chan pluginapi.HTTPStreamChunk), cancel, nil)
	bridge.closePlugin("first")
	if hasHostHTTPStreams(bridge) || ctx.Err() == nil {
		t.Fatal("plugin shutdown did not cancel its stream")
	}
}

func TestHostHTTPCallbacksRejectForeignAndClosedContext(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	host := New()
	callbackID, closeCallback := host.openCallbackContextForPlugin(context.Background(), "first")
	defer closeCallback()
	request, errMarshal := json.Marshal(rpcHostHTTPRequest{HostCallbackID: callbackID, URL: server.URL})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	foreign := withHostCallbackPluginID(context.Background(), "second")
	for _, method := range []string{pluginabi.MethodHostHTTPDo, pluginabi.MethodHostHTTPDoStream} {
		if _, errCall := host.callFromPlugin(foreign, method, request); errCall == nil ||
			!strings.Contains(errCall.Error(), "does not belong") {
			t.Fatalf("foreign %s callback error = %v", method, errCall)
		}
	}
	closeCallback()
	if _, errCall := host.callFromPlugin(withHostCallbackPluginID(context.Background(), "first"),
		pluginabi.MethodHostHTTPDo, request); errCall == nil || !strings.Contains(errCall.Error(), "is not open") {
		t.Fatalf("closed callback error = %v", errCall)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("sent %d upstream requests using invalid callback IDs", got)
	}
}

func TestHostHTTPDoCancelsWhenCallbackEnds(t *testing.T) {
	received := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(received)
		<-r.Context().Done()
	}))
	defer server.Close()

	host := New()
	callbackID, closeCallback := host.openCallbackContextForPlugin(context.Background(), "first")
	defer closeCallback()
	request, errMarshal := json.Marshal(rpcHostHTTPRequest{HostCallbackID: callbackID, URL: server.URL})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	done := make(chan error, 1)
	go func() {
		_, errCall := host.callFromPlugin(withHostCallbackPluginID(context.Background(), "first"), pluginabi.MethodHostHTTPDo, request)
		done <- errCall
	}()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("upstream HTTP request did not begin")
	}
	closeCallback()
	select {
	case errCall := <-done:
		if errCall == nil {
			t.Fatal("in-flight HTTP request completed after its callback closed")
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight HTTP request did not stop when callback closed")
	}
}

func TestHostHTTPStreamClosesWhenCallbackEnds(t *testing.T) {
	upstreamDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("ready"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	host := New()
	callbackID, closeCallback := host.openCallbackContextForPlugin(context.Background(), "first")
	defer closeCallback()
	caller := withHostCallbackPluginID(context.Background(), "first")
	caller = withHostCallbackInstanceID(caller, 7)
	request, errMarshal := json.Marshal(rpcHostHTTPRequest{HostCallbackID: callbackID, URL: server.URL})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	response, errCall := host.callFromPlugin(caller, pluginabi.MethodHostHTTPDoStream, request)
	if errCall != nil {
		t.Fatalf("open stream: %v", errCall)
	}
	opened, errDecode := decodeRPCEnvelope[rpcHostHTTPStreamResponse](response)
	if errDecode != nil || opened.StreamID == "" {
		t.Fatalf("stream response = %#v, error = %v", opened, errDecode)
	}
	readRequest, errMarshal := json.Marshal(rpcHostHTTPStreamReadRequest{StreamID: opened.StreamID})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	otherInstance := withHostCallbackInstanceID(withHostCallbackPluginID(context.Background(), "first"), 8)
	if _, errRead := host.callFromPlugin(otherInstance, pluginabi.MethodHostHTTPStreamRead, readRequest); errRead == nil {
		t.Fatal("another instance read a stream it did not open")
	}
	if _, errRead := host.callFromPlugin(caller, pluginabi.MethodHostHTTPStreamRead, readRequest); errRead != nil {
		t.Fatalf("stream owner could not read: %v", errRead)
	}
	closeCallback()
	select {
	case <-upstreamDone:
	case <-time.After(time.Second):
		t.Fatal("upstream request did not end when callback closed")
	}
	if _, errRead := host.callFromPlugin(caller, pluginabi.MethodHostHTTPStreamRead, readRequest); errRead == nil {
		t.Fatal("read remained available after callback closed")
	}
}
