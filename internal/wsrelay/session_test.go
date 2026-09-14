package wsrelay

import (
	"context"
	"errors"
	"testing"
)

func TestSessionDispatchFailsOnlySlowPendingRequest(t *testing.T) {
	s := &session{closed: make(chan struct{})}
	slow := newPendingRequest(context.Background(), s.closed)
	fast := newPendingRequest(context.Background(), s.closed)
	s.pending.Store("slow", slow)
	s.pending.Store("fast", fast)
	for range cap(slow.ch) {
		slow.ch <- message{ID: "slow", Type: MessageTypeStreamChunk}
	}

	s.dispatch(message{ID: "slow", Type: MessageTypeStreamChunk})
	if _, exists := s.pending.Load("slow"); exists {
		t.Fatal("slow request remained registered after its buffer overflowed")
	}

	s.dispatch(message{ID: "fast", Type: MessageTypeStreamChunk})
	gotFast, errFast := fast.receive()
	if errFast != nil {
		t.Fatalf("receive fast request: %v", errFast)
	}
	if gotFast.ID != "fast" {
		t.Fatalf("fast message id = %q, want fast", gotFast.ID)
	}

	for range cap(slow.ch) {
		if _, errReceive := slow.receive(); errReceive != nil {
			t.Fatalf("drain slow request: %v", errReceive)
		}
	}
	if _, errReceive := slow.receive(); !errors.Is(errReceive, errPendingBufferFull) {
		t.Fatalf("slow request error = %v, want %v", errReceive, errPendingBufferFull)
	}
}

func TestPendingRequestReceivesBufferedTerminalMessageAfterSessionClose(t *testing.T) {
	sessionClosed := make(chan struct{})
	req := newPendingRequest(context.Background(), sessionClosed)
	terminal := message{ID: "request", Type: MessageTypeStreamEnd}
	if errDeliver := req.deliver(terminal); errDeliver != nil {
		t.Fatalf("deliver terminal message: %v", errDeliver)
	}
	close(sessionClosed)

	got, err := req.receive()
	if err != nil {
		t.Fatalf("receive terminal message: %v", err)
	}
	if got.Type != MessageTypeStreamEnd {
		t.Fatalf("message type = %q, want %q", got.Type, MessageTypeStreamEnd)
	}
}

func TestSessionCleanupUnblocksPendingReceive(t *testing.T) {
	s := &session{closed: make(chan struct{})}
	req := newPendingRequest(context.Background(), s.closed)
	s.pending.Store("request", req)

	s.cleanup(errors.New("test cleanup"))
	if _, errReceive := req.receive(); !errors.Is(errReceive, errClosed) {
		t.Fatalf("receive error = %v, want %v", errReceive, errClosed)
	}
	if _, exists := s.pending.Load("request"); exists {
		t.Fatal("session cleanup left a pending request registered")
	}
}
