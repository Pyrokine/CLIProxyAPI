package executor

import (
	"github.com/gorilla/websocket"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCodexPongDoesNotWaitForApplicationWrite(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverConnCh := make(chan *websocket.Conn, 1)
	pongReceived := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		conn.SetPongHandler(func(appData string) error {
			pongReceived <- appData
			return nil
		})
		serverConnCh <- conn
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, errDial := websocket.DefaultDialer.Dial(wsURL, nil)
	if errDial != nil {
		t.Fatalf("dial websocket failed: %v", errDial)
	}
	defer func() { _ = clientConn.Close() }()

	serverConn := <-serverConnCh
	defer func() { _ = serverConn.Close() }()

	sess := &codexWebsocketSession{sessionID: "test-keepalive"}
	sess.configureConn(clientConn)

	// Start client read loop so it processes control frames.
	go func() {
		for {
			if _, _, errRead := clientConn.ReadMessage(); errRead != nil {
				return
			}
		}
	}()

	// Simulate an active application message write holding writeMu.
	sess.writeMu.Lock()
	defer sess.writeMu.Unlock()

	// Upstream sends a keepalive ping while writeMu is held.
	errPing := serverConn.WriteControl(websocket.PingMessage, []byte("keepalive-ping"), time.Now().Add(time.Second))
	if errPing != nil {
		t.Fatalf("failed to send ping: %v", errPing)
	}

	// Pong must be received promptly without being starved by writeMu.
	select {
	case got := <-pongReceived:
		if got != "keepalive-ping" {
			t.Fatalf("unexpected pong payload: got %q, want keepalive-ping", got)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("pong response was blocked/starved while writeMu was held")
	}
}
