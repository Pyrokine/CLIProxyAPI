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

	for index := range 17 {
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

func TestStreamBridgeEmitUsesAcceptedPumpResultAfterContextCancellation(t *testing.T) {
	for range 1000 {
		ctx, cancel := context.WithCancel(context.Background())
		entry := &streamEntry{input: make(chan streamInput), done: make(chan struct{}), abort: make(chan struct{})}
		published := make(chan struct{})
		go func() {
			request := <-entry.input
			cancel()
			request.accepted <- true
			close(published)
		}()
		err := entry.emit(ctx, pluginapi.ExecutorStreamChunk{Payload: []byte("accepted")})
		<-published
		if err != nil {
			t.Fatalf("accepted emit returned error: %v", err)
		}
	}
}

func TestStreamEntryAcceptedResultSurvivesStopSignals(t *testing.T) {
	for _, mode := range []string{"close", "abort"} {
		t.Run(
			mode, func(t *testing.T) {
				for range 1000 {
					entry := &streamEntry{
						input: make(chan streamInput), done: make(chan struct{}), abort: make(chan struct{}),
					}
					published := make(chan struct{})
					go func() {
						request := <-entry.input
						request.accepted <- true
						if mode == "close" {
							close(entry.done)
						} else {
							close(entry.abort)
						}
						close(published)
					}()
					err := entry.emit(context.Background(), pluginapi.ExecutorStreamChunk{Payload: []byte("accepted")})
					<-published
					if err != nil {
						t.Fatalf("accepted emit returned %s error: %v", mode, err)
					}
				}
			},
		)
	}
}

func TestStreamEntryPumpRejectionRemainsVisible(t *testing.T) {
	for _, mode := range []string{"cancel", "close", "abort"} {
		t.Run(
			mode, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				entry := &streamEntry{
					input: make(chan streamInput), done: make(chan struct{}), abort: make(chan struct{}),
				}
				published := make(chan struct{})
				go func() {
					request := <-entry.input
					request.accepted <- false
					switch mode {
					case "cancel":
						cancel()
					case "close":
						close(entry.done)
					case "abort":
						close(entry.abort)
					}
					close(published)
				}()
				err := entry.emit(ctx, pluginapi.ExecutorStreamChunk{})
				<-published
				if err == nil {
					t.Fatal("rejected emit returned success")
				}
			},
		)
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
