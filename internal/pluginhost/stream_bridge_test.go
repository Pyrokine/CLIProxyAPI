package pluginhost

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestStreamBridgeCloseUnblocksEmitterWithoutPanic(t *testing.T) {
	bridge := newStreamBridge()
	id, chunks, cleanup := bridge.open(context.Background())
	defer cleanup()

	for index := 0; index < 17; index++ {
		if err := bridge.emit(
			context.Background(),
			id,
			pluginapi.ExecutorStreamChunk{Payload: []byte{byte(index)}},
		); err != nil {
			t.Fatalf("emit %d: %v", index, err)
		}
	}

	emitDone := make(chan error, 1)
	go func() {
		emitDone <- bridge.emit(
			context.Background(),
			id,
			pluginapi.ExecutorStreamChunk{Payload: []byte("blocked")},
		)
	}()

	select {
	case err := <-emitDone:
		t.Fatalf("blocked emit returned before close: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	bridge.close(id, "canceled")
	select {
	case err := <-emitDone:
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("blocked emit error = %v, want closed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked emit did not return after close")
	}

	payloads := make([]string, 0, 17)
	errorCount := 0
	for chunk := range chunks {
		if chunk.Err != nil {
			errorCount++
			if chunk.Err.Error() != "canceled" {
				t.Fatalf("terminal error = %v", chunk.Err)
			}
			continue
		}
		payloads = append(payloads, string(chunk.Payload))
	}
	if len(payloads) != 17 || errorCount != 1 {
		t.Fatalf("delivered payloads = %d, terminal errors = %d", len(payloads), errorCount)
	}
	for index, payload := range payloads {
		if payload != string([]byte{byte(index)}) {
			t.Fatalf("payload %d = %q", index, payload)
		}
	}
}

func TestStreamBridgeConcurrentCloseIsIdempotent(t *testing.T) {
	bridge := newStreamBridge()
	id, chunks, _ := bridge.open(context.Background())

	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			bridge.close(id, "closed")
			done <- struct{}{}
		}()
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("concurrent close blocked")
		}
	}
	for range chunks {
	}
}
