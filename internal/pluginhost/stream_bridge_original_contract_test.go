package pluginhost

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type streamBridgeNotifyContext struct {
	context.Context
	ready chan struct{}
	once  sync.Once
}

func (c *streamBridgeNotifyContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.ready) })
	return c.Context.Done()
}

func TestStreamBridgeCloseUnblocksPendingEmit(t *testing.T) {
	bridge := newStreamBridge()
	id, chunks, cleanup := bridge.open(context.Background())
	defer cleanup()
	for range 17 {
		if err := bridge.emit(
			context.Background(), id, pluginapi.ExecutorStreamChunk{Payload: []byte("buffered")},
		); err != nil {
			t.Fatal(err)
		}
	}
	ctx := &streamBridgeNotifyContext{Context: context.Background(), ready: make(chan struct{})}
	emitted := make(chan error, 1)
	go func() {
		emitted <- bridge.emit(ctx, id, pluginapi.ExecutorStreamChunk{Payload: []byte("blocked")})
	}()
	select {
	case <-ctx.ready:
	case <-time.After(time.Second):
		t.Fatal("emit did not reach the blocked send")
	}
	select {
	case err := <-emitted:
		t.Fatalf("emit returned while saturated: %v", err)
	default:
	}
	bridge.close(id, "")
	select {
	case err := <-emitted:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("pending emit = %v, want closed error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not unblock the pending emit")
	}
	count := 0
	for chunk := range chunks {
		if chunk.Err != nil || string(chunk.Payload) != "buffered" {
			t.Fatalf("rejected emit or error reached the consumer: %+v", chunk)
		}
		count++
	}
	if count != 17 {
		t.Fatalf("delivered chunks = %d, want 17 accepted chunks", count)
	}
}

func TestStreamBridgeAbortClosesSaturatedStreamWithoutConsumer(t *testing.T) {
	bridge := newStreamBridge()
	id, chunks, cleanup := bridge.open(context.Background())
	defer cleanup()
	bridge.mu.Lock()
	entry := bridge.streams[id]
	bridge.mu.Unlock()
	for range 17 {
		if err := bridge.emit(
			context.Background(), id, pluginapi.ExecutorStreamChunk{Payload: []byte("buffered")},
		); err != nil {
			t.Fatal(err)
		}
	}
	cleanup()
	select {
	case <-entry.finished:
	case <-time.After(time.Second):
		t.Fatal("abort left the saturated stream pump running")
	}
	if _, ok := <-chunks; ok {
		t.Fatal("aborted stream retained buffered chunks")
	}
}

func TestStreamBridgeCleanupAbortsPendingGracefulClose(t *testing.T) {
	bridge := newStreamBridge()
	id, chunks, cleanup := bridge.open(context.Background())
	defer cleanup()
	bridge.mu.Lock()
	entry := bridge.streams[id]
	bridge.mu.Unlock()
	for range 17 {
		if err := bridge.emit(
			context.Background(), id, pluginapi.ExecutorStreamChunk{Payload: []byte("buffered")},
		); err != nil {
			t.Fatal(err)
		}
	}
	bridge.close(id, "plugin stream failed")
	cleanup()
	select {
	case <-entry.finished:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not abort the pending graceful close")
	}
	if _, ok := <-chunks; ok {
		t.Fatal("cleanup retained queued chunks after aborting graceful close")
	}
}

func TestStreamBridgeCloseDeliversTerminalError(t *testing.T) {
	bridge := newStreamBridge()
	id, chunks, cleanup := bridge.open(context.Background())
	defer cleanup()
	bridge.close(id, "plugin stream failed")
	chunk, ok := <-chunks
	if !ok || chunk.Err == nil || chunk.Err.Error() != "plugin stream failed" {
		t.Fatalf("terminal chunk = %+v, open = %t", chunk, ok)
	}
	if _, ok := <-chunks; ok {
		t.Fatal("stream remains open after terminal error")
	}
}

func TestStreamBridgeClosePreservesTerminalErrorWhenBufferIsFull(t *testing.T) {
	bridge := newStreamBridge()
	id, chunks, cleanup := bridge.open(context.Background())
	defer cleanup()
	for range 17 {
		if err := bridge.emit(
			context.Background(), id, pluginapi.ExecutorStreamChunk{Payload: []byte("buffered")},
		); err != nil {
			t.Fatal(err)
		}
	}
	closed := make(chan struct{})
	go func() {
		bridge.close(id, "plugin stream failed")
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close blocked on the saturated stream")
	}
	count, errorsCount := 0, 0
	for chunk := range chunks {
		count++
		if chunk.Err != nil {
			errorsCount++
			if chunk.Err.Error() != "plugin stream failed" || count != 18 {
				t.Fatalf("terminal error or ordering = %+v at chunk %d", chunk, count)
			}
		} else if string(chunk.Payload) != "buffered" {
			t.Fatalf("buffered payload changed: %+v", chunk)
		}
	}
	if count != 18 || errorsCount != 1 {
		t.Fatalf("chunks/errors = %d/%d, want 17 buffered chunks and one terminal error", count, errorsCount)
	}
}
