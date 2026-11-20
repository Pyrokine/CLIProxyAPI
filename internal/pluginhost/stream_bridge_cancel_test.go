package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestStreamBridgeCancelDropsUnreadAcceptedQueue(t *testing.T) {
	for _, mode := range []string{"cleanup", "abort", "context"} {
		t.Run(
			mode, func(t *testing.T) {
				bridge := newStreamBridge()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var calls atomic.Int32
				done := make(chan struct{})
				id, chunks, cleanup := bridge.open(
					ctx, func() {
						calls.Add(1)
						close(done)
					},
				)
				defer cleanup()
				for index := range 17 {
					if err := bridge.emit(
						context.Background(), id, pluginapi.ExecutorStreamChunk{Payload: []byte{byte(index)}},
					); err != nil {
						t.Fatalf("emit %d: %v", index, err)
					}
				}
				emitDone := make(chan error, 1)
				go func() {
					emitDone <- bridge.emit(
						context.Background(), id, pluginapi.ExecutorStreamChunk{Payload: []byte("blocked")},
					)
				}()
				select {
				case err := <-emitDone:
					t.Fatalf("full queue accepted another chunk: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
				switch mode {
				case "cleanup":
					cleanup()
				case "abort":
					bridge.abort(id)
				case "context":
					cancel()
				}
				waitStreamBridgeSignal(t, done)
				select {
				case err := <-emitDone:
					if err == nil {
						t.Fatal("canceled emitter returned success")
					}
				case <-time.After(time.Second):
					t.Fatal("cancel did not unblock emitter")
				}
				assertStreamBridgeClosed(t, chunks)
				cleanup()
				bridge.abort(id)
				bridge.close(id, "late")
				bridge.mu.Lock()
				remaining := len(bridge.streams)
				bridge.mu.Unlock()
				if remaining != 0 || calls.Load() != 1 {
					t.Fatalf("remaining streams = %d, cleanup calls = %d", remaining, calls.Load())
				}
			},
		)
	}
}

func TestStreamBridgeCancelAfterCloseDiscardsPendingTerminalError(t *testing.T) {
	for _, count := range []int{0, 1, 17} {
		t.Run(
			fmt.Sprint(count), func(t *testing.T) {
				bridge := newStreamBridge()
				id, chunks, cleanup := bridge.open(context.Background())
				defer cleanup()
				for index := range count {
					if err := bridge.emit(
						context.Background(), id, pluginapi.ExecutorStreamChunk{Payload: []byte{byte(index)}},
					); err != nil {
						t.Fatal(err)
					}
				}
				bridge.close(id, "terminal")
				cleanup()
				assertStreamBridgeClosed(t, chunks)
			},
		)
	}
}

func TestStreamBridgeConcurrentCloseAndCancel(t *testing.T) {
	bridge := newStreamBridge()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	id, chunks, cleanup := bridge.open(ctx, func() { calls.Add(1) })
	defer cleanup()
	for index := range 17 {
		if err := bridge.emit(ctx, id, pluginapi.ExecutorStreamChunk{Payload: []byte{byte(index)}}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for index := range 32 {
		wg.Go(
			func() {
				switch index % 4 {
				case 0:
					bridge.close(id, "terminal")
				case 1:
					cleanup()
				case 2:
					bridge.abort(id)
				case 3:
					cancel()
				}
			},
		)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitStreamBridgeSignal(t, done)
	assertStreamBridgeClosed(t, chunks)
	if calls.Load() != 1 {
		t.Fatalf("cleanup calls = %d, want 1", calls.Load())
	}
}

func TestRPCPluginStreamBridgeContract(t *testing.T) {
	for _, mode := range []string{"cancel", "close", "close-error"} {
		t.Run(
			mode, func(t *testing.T) {
				host, rpc, request := newStreamBridgeRPCFixture()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				response, err := rpc.ExecuteStream(ctx, pluginapi.ExecutorRequest{})
				if err != nil {
					t.Fatal(err)
				}
				if response.Headers.Get("X-Fixture") != "stream" {
					t.Fatalf("headers = %v", response.Headers)
				}
				done := make(chan struct{})
				if _, ok := host.addCallbackCleanupHandle(request.HostCallbackID, func() { close(done) }); !ok {
					t.Fatal("stream callback scope was already closed")
				}
				for index := range 17 {
					emitStreamBridgeHostChunk(t, host, request.StreamID, index)
				}
				if mode == "cancel" {
					cancel()
					waitStreamBridgeSignal(t, done)
					assertStreamBridgeClosed(t, response.Chunks)
				} else {
					terminal := ""
					if mode == "close-error" {
						terminal = "terminal"
					}
					closeStreamBridgeHost(t, host, request.StreamID, terminal)
					for index := range 17 {
						select {
						case chunk, ok := <-response.Chunks:
							if !ok || chunk.Err != nil || string(chunk.Payload) != string([]byte{byte(index)}) {
								t.Fatalf("chunk %d = %#v, open = %v", index, chunk, ok)
							}
						case <-time.After(time.Second):
							t.Fatal("normal close lost accepted data")
						}
					}
					if terminal != "" {
						select {
						case chunk, ok := <-response.Chunks:
							if !ok || chunk.Err == nil || chunk.Err.Error() != terminal || len(chunk.Payload) != 0 {
								t.Fatalf("terminal = %#v, open = %v", chunk, ok)
							}
						case <-time.After(time.Second):
							t.Fatal("missing terminal error")
						}
					}
					assertStreamBridgeClosed(t, response.Chunks)
					waitStreamBridgeSignal(t, done)
				}
				if _, _, ok := host.activeCallbackContext(request.HostCallbackID); ok {
					t.Fatal("stream callback scope remains open")
				}
				if err := host.streams.emit(
					context.Background(), request.StreamID, pluginapi.ExecutorStreamChunk{},
				); err == nil {
					t.Fatal("completed stream still accepts chunks")
				}
			},
		)
	}
}

func TestExecutorAdapterStreamBridgeCancelAndTerminalCleanup(t *testing.T) {
	for _, mode := range []string{"cancel", "terminal"} {
		t.Run(
			mode, func(t *testing.T) {
				host, rpc, request := newStreamBridgeRPCFixture()
				format := sdktranslator.FormatOpenAIResponse
				adapter := &executorAdapter{
					host: host, pluginID: rpc.id, provider: "fixture-provider",
					executor: rpcProviderExecutor{rpc}, inputFormats: []sdktranslator.Format{format},
					outputFormats: []sdktranslator.Format{format},
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result, err := adapter.ExecuteStream(
					ctx, nil, coreexecutor.Request{
						Model: "fixture-model", Format: format, Payload: []byte(`{"model":"fixture-model"}`),
					}, coreexecutor.Options{Stream: true},
				)
				if err != nil {
					t.Fatal(err)
				}
				upstream, _, ok := host.activeCallbackContext(request.HostCallbackID)
				if !ok {
					t.Fatal("missing upstream callback context")
				}
				cleanupDone := make(chan struct{})
				if _, ok := host.addCallbackCleanupHandle(request.HostCallbackID, func() { close(cleanupDone) }); !ok {
					t.Fatal("missing upstream cleanup")
				}
				for index := range 17 {
					emitStreamBridgeHostChunk(t, host, request.StreamID, index)
				}
				if mode == "cancel" {
					cancel()
				} else {
					closeStreamBridgeHost(t, host, request.StreamID, "terminal")
				}
				payloads, terminalErrors := 0, 0
				timeout := time.After(time.Second)
				for {
					select {
					case chunk, open := <-result.Chunks:
						if !open {
							goto finished
						}
						if mode == "terminal" {
							if chunk.Err != nil {
								terminalErrors++
								if payloads != 17 || chunk.Err.Error() != "terminal" {
									t.Fatalf("terminal after %d chunks: %v", payloads, chunk.Err)
								}
							} else {
								if terminalErrors != 0 || string(chunk.Payload) != string([]byte{byte(payloads)}) {
									t.Fatalf("payload %d = %q after %d errors", payloads, chunk.Payload, terminalErrors)
								}
								payloads++
							}
						}
					case <-timeout:
						t.Fatal("SDK consumer did not finish")
					}
				}
			finished:
				if mode == "terminal" && (payloads != 17 || terminalErrors != 1) {
					t.Fatalf("payloads = %d, terminal errors = %d", payloads, terminalErrors)
				}
				waitStreamBridgeSignal(t, upstream.Done())
				waitStreamBridgeSignal(t, cleanupDone)
				if _, _, ok := host.activeCallbackContext(request.HostCallbackID); ok {
					t.Fatal("SDK completion retained upstream callback scope")
				}
			},
		)
	}
}

func TestHostStreamEmitRetainsAcceptedResultAfterCallCancellation(t *testing.T) {
	host := New()
	raw, err := json.Marshal(rpcStreamEmitRequest{StreamID: "accepted-rpc", Payload: []byte("accepted")})
	if err != nil {
		t.Fatal(err)
	}
	for range 1000 {
		entry := &streamEntry{input: make(chan streamInput), done: make(chan struct{}), abort: make(chan struct{})}
		host.streams.mu.Lock()
		host.streams.streams["accepted-rpc"] = entry
		host.streams.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		published := make(chan struct{})
		go func() {
			request := <-entry.input
			if string(request.chunk.Payload) != "accepted" {
				t.Errorf("RPC payload=%q", request.chunk.Payload)
			}
			cancel()
			request.accepted <- true
			close(published)
		}()
		_, err := host.callFromPlugin(ctx, pluginabi.MethodHostStreamEmit, raw)
		<-published
		host.streams.remove("accepted-rpc", entry)
		if err != nil {
			t.Fatalf("accepted host.stream.emit returned cancellation: %v", err)
		}
	}
}

func newStreamBridgeRPCFixture() (*Host, *rpcPluginAdapter, *rpcExecutorRequest) {
	host := New()
	request := &rpcExecutorRequest{}
	client := &testSymbolLookup{
		methodHandlers: map[string]func(context.Context, []byte) ([]byte, error){
			pluginabi.MethodExecutorExecuteStream: func(_ context.Context, raw []byte) ([]byte, error) {
				if err := json.Unmarshal(raw, request); err != nil {
					return nil, err
				}
				return marshalRPCResult(rpcExecutorStreamResponse{Headers: http.Header{"X-Fixture": {"stream"}}})
			},
		},
	}
	return host, &rpcPluginAdapter{id: "stream-fixture", host: host, client: client}, request
}

func emitStreamBridgeHostChunk(t *testing.T, host *Host, id string, index int) {
	t.Helper()
	raw, err := json.Marshal(rpcStreamEmitRequest{StreamID: id, Payload: []byte{byte(index)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.callFromPlugin(context.Background(), pluginabi.MethodHostStreamEmit, raw); err != nil {
		t.Fatalf("host emit %d: %v", index, err)
	}
}

func closeStreamBridgeHost(t *testing.T, host *Host, id, terminal string) {
	t.Helper()
	raw, err := json.Marshal(rpcStreamCloseRequest{StreamID: id, Error: terminal})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.callFromPlugin(context.Background(), pluginabi.MethodHostStreamClose, raw); err != nil {
		t.Fatal(err)
	}
}

func waitStreamBridgeSignal(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream cleanup did not finish")
	}
}

func assertStreamBridgeClosed(t *testing.T, chunks <-chan pluginapi.ExecutorStreamChunk) {
	t.Helper()
	select {
	case chunk, open := <-chunks:
		if open {
			t.Fatalf("canceled or completed stream delivered unread chunk: %#v", chunk)
		}
	case <-time.After(time.Second):
		t.Fatal("stream channel did not close")
	}
}
