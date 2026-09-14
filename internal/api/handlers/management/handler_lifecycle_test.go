package management

import (
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestHandlerBatchCloseStopsAttemptCleanup(t *testing.T) {
	const handlerCount = 32
	handlers := make([]*Handler, 0, handlerCount)
	for range handlerCount {
		handlers = append(handlers, NewHandler(&config.Config{}, "", nil, withEnvSecret("")))
	}
	done := make(chan error, handlerCount)
	for _, handler := range handlers {
		go func(handler *Handler) {
			done <- handler.Close()
		}(handler)
	}
	for range handlerCount {
		select {
		case errClose := <-done:
			if errClose != nil {
				t.Fatalf("Close() error = %v", errClose)
			}
		case <-time.After(time.Second):
			t.Fatal("batch Close() timed out")
		}
	}
}

func TestHandlerCloseStopsAttemptCleanup(t *testing.T) {
	handler := NewHandler(&config.Config{}, "", nil, withEnvSecret(""))
	if handler.attemptCleanupCancel == nil {
		t.Fatal("attempt cleanup cancel was not initialized")
	}

	done := make(chan error, 1)
	go func() {
		done <- handler.Close()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not wait for attempt cleanup to exit")
	}

	if err := handler.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}
