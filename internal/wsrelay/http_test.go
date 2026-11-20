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

func TestResponseFinalURLProtocol(t *testing.T) {
	for _, value := range []any{
		nil, "https://user:synthetic-secret@final.example.test/result?api_key=synthetic-secret&region=test",
		"relative/path", 42,
	} {
		payload := map[string]any{"status": float64(503), "final_url": value}
		resp := decodeResponse(payload)
		if resp.Status != 503 {
			t.Fatalf("status=%d", resp.Status)
		}
		if _, valid := value.(string); valid && value != "relative/path" {
			if resp.FinalURL != "https://final.example.test/result?api_key=%2A%2A%2A&region=test" {
				t.Fatalf("final URL=%q", resp.FinalURL)
			}
		} else if resp.FinalURL != "" {
			t.Fatalf("invented final URL=%q", resp.FinalURL)
		}
	}
	for _, kind := range []string{MessageTypeStreamStart, MessageTypeHTTPResp} {
		ctx := t.Context()
		pending := newPendingRequest(ctx, make(chan struct{}))
		if err := pending.deliver(
			message{
				Type: kind, Payload: map[string]any{"final_url": "https://final.example.test/result"},
			},
		); err != nil {
			t.Fatal(err)
		}
		if kind == MessageTypeStreamStart {
			if err := pending.deliver(message{Type: MessageTypeStreamEnd}); err != nil {
				t.Fatal(err)
			}
		}
		events := streamPendingEvents(ctx, pending)
		event, ok := <-events
		if !ok || event.FinalURL != "https://final.example.test/result" {
			t.Fatalf("event=%+v", event)
		}
		for range events {
		}
	}
}
