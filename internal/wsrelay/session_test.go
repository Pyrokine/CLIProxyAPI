package wsrelay

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newSessionTestPair(t *testing.T) (*session, *websocket.Conn) {
	t.Helper()
	sessionCh := make(chan *session, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		sessionCh <- newSession(conn, nil, "test")
	}))
	client, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if errDial != nil {
		server.Close()
		t.Fatalf("dial websocket: %v", errDial)
	}
	s := <-sessionCh
	t.Cleanup(func() {
		s.cleanup(errClosed)
		_ = client.Close()
		server.Close()
	})
	return s, client
}

func waitForPendingRemoval(t *testing.T, s *session, id string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if _, exists := s.pending.Load(id); !exists {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending request %q was not removed", id)
		}
		time.Sleep(time.Millisecond)
	}
}

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

func TestSessionRequestCancellationRemovesPendingState(t *testing.T) {
	s, client := newSessionTestPair(t)
	ctx, cancel := context.WithCancel(context.Background())
	req, errRequest := s.request(ctx, message{ID: "cancelled", Type: messageTypeHTTPReq})
	if errRequest != nil {
		t.Fatalf("request: %v", errRequest)
	}
	if errDeadline := client.SetReadDeadline(time.Now().Add(time.Second)); errDeadline != nil {
		t.Fatalf("set read deadline: %v", errDeadline)
	}
	var sent message
	if errRead := client.ReadJSON(&sent); errRead != nil {
		t.Fatalf("read request: %v", errRead)
	}
	cancel()
	waitForPendingRemoval(t, s, "cancelled")

	if _, errReceive := req.receive(); !errors.Is(errReceive, context.Canceled) {
		t.Fatalf("receive error = %v, want context.Canceled", errReceive)
	}
	req.cancelMu.Lock()
	stopped := req.cancelStopped
	stopCancel := req.stopCancel
	req.cancelMu.Unlock()
	if !stopped || stopCancel != nil {
		t.Fatalf("cancellation watcher state = stopped:%v stop:%v", stopped, stopCancel != nil)
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

func TestSessionDispatchDeliversTerminalAfterRegularBufferIsFull(t *testing.T) {
	s := &session{closed: make(chan struct{})}
	req := newPendingRequest(context.Background(), s.closed)
	stopCalled := false
	req.setStopCancel(func() bool {
		stopCalled = true
		return true
	})
	s.pending.Store("request", req)
	for range cap(req.ch) {
		req.ch <- message{ID: "request", Type: MessageTypeStreamChunk}
	}

	s.dispatch(message{ID: "request", Type: MessageTypeStreamEnd})
	if _, exists := s.pending.Load("request"); exists {
		t.Fatal("terminal request remained registered")
	}
	if !stopCalled {
		t.Fatal("terminal response did not stop cancellation tracking")
	}
	for range cap(req.ch) {
		got, errReceive := req.receive()
		if errReceive != nil {
			t.Fatalf("drain regular message: %v", errReceive)
		}
		if got.Type != MessageTypeStreamChunk {
			t.Fatalf("regular message type = %q, want %q", got.Type, MessageTypeStreamChunk)
		}
	}
	terminal, errReceive := req.receive()
	if errReceive != nil {
		t.Fatalf("receive terminal message: %v", errReceive)
	}
	if terminal.Type != MessageTypeStreamEnd {
		t.Fatalf("terminal message type = %q, want %q", terminal.Type, MessageTypeStreamEnd)
	}
}

func TestSessionDispatchDeliversTerminalOnlyOnce(t *testing.T) {
	s := &session{closed: make(chan struct{})}
	req := newPendingRequest(context.Background(), s.closed)
	s.pending.Store("request", req)

	s.dispatch(message{ID: "request", Type: MessageTypeStreamEnd})
	s.dispatch(message{ID: "request", Type: MessageTypeError})

	terminal, errReceive := req.receive()
	if errReceive != nil {
		t.Fatalf("receive terminal message: %v", errReceive)
	}
	if terminal.Type != MessageTypeStreamEnd {
		t.Fatalf("terminal message type = %q, want %q", terminal.Type, MessageTypeStreamEnd)
	}
	select {
	case duplicate := <-req.terminal:
		t.Fatalf("duplicate terminal message = %+v", duplicate)
	default:
	}
}

func TestSessionCleanupUnblocksPendingReceive(t *testing.T) {
	s := &session{closed: make(chan struct{})}
	req := newPendingRequest(context.Background(), s.closed)
	stopCalled := false
	req.setStopCancel(func() bool {
		stopCalled = true
		return true
	})
	s.pending.Store("request", req)

	s.cleanup(errors.New("test cleanup"))
	if _, errReceive := req.receive(); !errors.Is(errReceive, errClosed) {
		t.Fatalf("receive error = %v, want %v", errReceive, errClosed)
	}
	if _, exists := s.pending.Load("request"); exists {
		t.Fatal("session cleanup left a pending request registered")
	}
	if !stopCalled {
		t.Fatal("session cleanup did not stop cancellation tracking")
	}
}
