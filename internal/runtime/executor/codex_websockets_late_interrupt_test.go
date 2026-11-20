package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gorilla/websocket"
)

func TestInterruptExecutionSessionCompletesDuringAuthCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	complete := make(chan struct{})
	interruptCount := make(chan int, 1)
	upstream := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				defer func(closeResource func() error) { _ = closeResource() }(conn.Close)
				select {
				case <-complete:
				case <-ctx.Done():
					return
				}
				terminal := `{"type":"response.completed","response":{"id":"r1","output":[]}}`
				if err = conn.WriteMessage(websocket.TextMessage, []byte(terminal)); err != nil {
					t.Errorf("write completion: %v", err)
					return
				}
				count := 0
				for {
					_, payload, errRead := conn.ReadMessage()
					if errRead != nil {
						return
					}
					if string(payload) == "barrier" {
						interruptCount <- count
						return
					}
					count++
				}
			},
		),
	)
	defer upstream.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	executor := newCodexWebsocketsExecutor(nil)
	const sessionID = "completion-during-auth-check"
	defer executor.CloseExecutionSession(sessionID)
	sess := executor.getOrCreateSession(sessionID)
	sess.conn = client
	sess.connCloser = newWebsocketConnectionCloser(client)
	sess.configureConn(client)
	readCh := make(chan codexWebsocketRead, 4)
	sess.setActive(client, readCh)
	go executor.readUpstreamLoop(sess, client)
	authContext := core.WithWebsocketAuthCheck(
		ctx, func(string) bool {
			close(complete)
			select {
			case event := <-readCh:
				if event.err != nil {
					t.Fatal(event.err)
				}
			case <-ctx.Done():
				t.Fatal("completion timed out")
			}
			sess.clearActive(client, readCh)
			return true
		},
	)
	payload := []byte(`{"type":"response.interrupt","response_id":"r1"}`)
	if err = executor.InterruptExecutionSession(authContext, sessionID, payload); err != nil {
		t.Fatalf("completed interrupt = %v", err)
	}
	// 顺序屏障确保此前发送的控制帧已被上游读取
	if err = client.WriteMessage(websocket.TextMessage, []byte("barrier")); err != nil {
		t.Fatal(err)
	}
	select {
	case count := <-interruptCount:
		if count != 0 {
			t.Fatalf("forwarded %d interrupt(s) after the response became terminal", count)
		}
	case <-ctx.Done():
		t.Fatal("upstream barrier timed out")
	}
}

func TestInterruptExecutionSessionAfterTerminal(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.done", "response.incomplete", "response.failed"} {
		t.Run(
			terminal, func(t *testing.T) {
				upstream := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
							if err != nil {
								t.Errorf("upgrade: %v", err)
								return
							}
							defer func(closeResource func() error) { _ = closeResource() }(conn.Close)
							for _, payload := range []string{
								`{"type":"response.created","response":{"id":"r1"}}`,
								`{"type":"` + terminal + `","response":{"id":"r1","output":[]}}`,
							} {
								if err = conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
									t.Errorf("write: %v", err)
									return
								}
							}
							_, payload, errRead := conn.ReadMessage()
							if errRead != nil {
								return
							}
							if string(payload) != `{"type":"response.interrupt","response_id":"r2"}` {
								t.Errorf("late interrupt contaminated next response: %s", payload)
							}
							response := `{"type":"response.incomplete","response":{"id":"r2"}}`
							if err = conn.WriteMessage(websocket.TextMessage, []byte(response)); err != nil {
								t.Errorf("write r2 terminal: %v", err)
							}
							_, _, _ = conn.ReadMessage()
						},
					),
				)
				defer upstream.Close()
				client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
				if err != nil {
					t.Fatal(err)
				}
				executor := newCodexWebsocketsExecutor(nil)
				const sessionID = "late-interrupt"
				defer executor.CloseExecutionSession(sessionID)
				sess := executor.getOrCreateSession(sessionID)
				sess.conn = client
				sess.connCloser = newWebsocketConnectionCloser(client)
				sess.configureConn(client)
				readCh := make(chan codexWebsocketRead, 4)
				sess.setActive(client, readCh)
				go executor.readUpstreamLoop(sess, client)
				read := func() codexWebsocketRead {
					t.Helper()
					select {
					case event := <-readCh:
						if event.err != nil {
							t.Fatal(event.err)
						}
						return event
					case <-time.After(3 * time.Second):
						t.Fatal("upstream event timed out")
						return codexWebsocketRead{}
					}
				}
				read()
				read()
				sess.clearActive(client, readCh)
				late := []byte(`{"type":"response.interrupt","response_id":"r1"}`)
				for i := range 2 {
					if err = executor.InterruptExecutionSession(t.Context(), sessionID, late); err != nil {
						t.Fatalf("terminal r1 interrupt %d must be idempotent, got %v", i+1, err)
					}
				}
				unknown := []byte(`{"type":"response.interrupt","response_id":"unknown"}`)
				err = executor.InterruptExecutionSession(t.Context(), sessionID, unknown)
				if !errors.Is(err, core.ErrNoActiveUpstreamWebsocket) {
					t.Fatalf("unknown idle interrupt = %v, want ErrNoActiveUpstreamWebsocket", err)
				}
				readCh = make(chan codexWebsocketRead, 4)
				sess.setActive(client, readCh)
				if err = executor.InterruptExecutionSession(t.Context(), sessionID, late); err != nil {
					t.Fatalf("late r1 during r2 = %v", err)
				}
				active := []byte(`{"type":"response.interrupt","response_id":"r2"}`)
				if err = executor.InterruptExecutionSession(t.Context(), sessionID, active); err != nil {
					t.Fatal(err)
				}
				read()
				sess.clearActive(client, readCh)
				for _, id := range []string{"r1", "r2"} {
					payload := []byte(`{"type":"response.interrupt","response_id":"` + id + `"}`)
					if err = executor.InterruptExecutionSession(t.Context(), sessionID, payload); err != nil {
						t.Fatalf("earlier terminal %s = %v", id, err)
					}
				}
				if err = client.WriteMessage(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "test disconnect"),
				); err != nil {
					t.Fatal(err)
				}
				select {
				case <-sess.upstreamDisconnectCh:
				case <-time.After(3 * time.Second):
					t.Fatal("real disconnect was not reported")
				}
				err = executor.InterruptExecutionSession(t.Context(), sessionID, active)
				if !errors.Is(err, core.ErrNoActiveUpstreamWebsocket) {
					t.Fatalf("disconnected interrupt = %v, want ErrNoActiveUpstreamWebsocket", err)
				}
			},
		)
	}
}
