package wsrelay

import (
	"bytes"
	"errors"
	"testing"
)

func TestStreamPendingEventsPropagatesPendingFailure(t *testing.T) {
	ctx := t.Context()
	pending := newPendingRequest(ctx, make(chan struct{}))
	pending.fail(errPendingBufferFull)

	event, ok := <-streamPendingEvents(ctx, pending)
	if !ok {
		t.Fatal("stream closed without an error event")
	}
	if event.Type != MessageTypeError || !errors.Is(event.Err, errPendingBufferFull) {
		t.Fatalf("stream event = %+v, want pending buffer error", event)
	}
}

func TestWriteChunkWithLimit(t *testing.T) {
	var body bytes.Buffer
	if err := writeChunkWithLimit(&body, []byte("1234"), 6); err != nil {
		t.Fatalf("write first chunk: %v", err)
	}
	if err := writeChunkWithLimit(&body, []byte("56"), 6); err != nil {
		t.Fatalf("write chunk at limit: %v", err)
	}
	if got := body.String(); got != "123456" {
		t.Fatalf("body = %q, want %q", got, "123456")
	}

	if err := writeChunkWithLimit(&body, []byte("7"), 6); err == nil {
		t.Fatal("expected response size error")
	}
	if got := body.String(); got != "123456" {
		t.Fatalf("body changed after rejected chunk: %q", got)
	}
}
