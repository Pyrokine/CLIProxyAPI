package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gorilla/websocket"
)

func TestCodexInterruptPreservesFrameAndIgnoresTerminalTurn(t *testing.T) {
	upstreamFrames := make(chan [][]byte, 1)
	keepUpstreamOpen := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade upstream: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		var received [][]byte
		for len(received) < 2 {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Errorf("read upstream frame: %v", errRead)
				return
			}
			received = append(received, bytes.Clone(payload))
			if len(received) == 1 {
				if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"r1","output":[]}}`)); errWrite != nil {
					t.Errorf("send terminal frame: %v", errWrite)
					return
				}
			}
		}
		upstreamFrames <- received
		<-keepUpstreamOpen
	}))
	defer upstream.Close()
	defer close(keepUpstreamOpen)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial upstream: %v", err)
	}
	defer func() { _ = conn.Close() }()

	executor := newCodexWebsocketsExecutor(&config.Config{})
	const sessionID = "offline-interrupt-test"
	defer executor.CloseExecutionSession(sessionID)
	sess := executor.getOrCreateSession(sessionID)
	sess.connMu.Lock()
	sess.conn = conn
	sess.authID = "auth-1"
	sess.connMu.Unlock()
	sess.configureConn(conn)
	go executor.readUpstreamLoop(sess, conn)

	first := []byte(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items","extension":42}`)
	if errIdle := executor.InterruptExecutionSession(context.Background(), sessionID, first); !errors.Is(errIdle, cliproxyexecutor.ErrNoActiveUpstreamWebsocket) {
		t.Fatalf("idle interrupt = %v", errIdle)
	}
	readCh := make(chan codexWebsocketRead, 4)
	sess.setActive(conn, readCh)
	denied := cliproxyexecutor.WithWebsocketAuthCheck(context.Background(), func(authID string) bool {
		return false
	})
	if errDenied := executor.InterruptExecutionSession(denied, sessionID, first); errDenied == nil {
		t.Fatal("disabled auth accepted the interrupt")
	}
	allowed := cliproxyexecutor.WithWebsocketAuthCheck(context.Background(), func(authID string) bool {
		return authID == "auth-1"
	})
	if errForward := executor.InterruptExecutionSession(allowed, sessionID, first); errForward != nil {
		t.Fatalf("active interrupt: %v", errForward)
	}
	select {
	case frame := <-readCh:
		if frame.err != nil || !bytes.Contains(frame.payload, []byte(`"response.completed"`)) {
			t.Fatalf("terminal frame = %+v", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("terminal frame not received")
	}
	sess.clearActive(conn, readCh)
	if errLate := executor.InterruptExecutionSession(allowed, sessionID, first); errLate != nil {
		t.Fatalf("late interrupt after completion = %v", errLate)
	}
	if errLate := executor.InterruptExecutionSession(denied, sessionID, first); errLate != nil {
		t.Fatalf("late interrupt after credential was disabled = %v", errLate)
	}
	readCh = make(chan codexWebsocketRead, 4)
	sess.setActive(conn, readCh)
	if errLate := executor.InterruptExecutionSession(allowed, sessionID, first); errLate != nil {
		t.Fatalf("late interrupt during next turn = %v", errLate)
	}
	second := []byte(`{"type":"response.interrupt","response_id":"r2"}`)
	if errSecond := executor.InterruptExecutionSession(allowed, sessionID, second); errSecond != nil {
		t.Fatalf("next-turn interrupt: %v", errSecond)
	}
	select {
	case frames := <-upstreamFrames:
		if !bytes.Equal(frames[0], first) || !bytes.Equal(frames[1], second) {
			t.Fatalf("upstream frames = %q", frames)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not receive next-turn interrupt")
	}
	completedDuringAuth := cliproxyexecutor.WithWebsocketAuthCheck(context.Background(), func(string) bool {
		sess.activeMu.Lock()
		sess.terminalResponseIDs["r2"] = struct{}{}
		sess.activeMu.Unlock()
		return false
	})
	if errLate := executor.InterruptExecutionSession(completedDuringAuth, sessionID, second); errLate != nil {
		t.Fatalf("interrupt completed during credential check = %v", errLate)
	}
	sess.clearActive(conn, readCh)
	disconnectErr := errors.New("synthetic upstream disconnect")
	sess.setUpstreamDisconnectError(conn, disconnectErr)
	if errLate := executor.InterruptExecutionSession(allowed, sessionID, second); !errors.Is(errLate, disconnectErr) {
		t.Fatalf("late interrupt after disconnect = %v, want %v", errLate, disconnectErr)
	}
}
