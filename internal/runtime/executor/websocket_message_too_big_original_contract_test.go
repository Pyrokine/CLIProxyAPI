package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestSendTerminalWebsocketReadInvalidatesBeforeWaitingForCapacity(t *testing.T) {
	terminalErr := &websocket.CloseError{Code: websocket.CloseMessageTooBig}
	t.Run(
		"available channel keeps fast path ordering", func(t *testing.T) {
			ch := make(chan codexWebsocketRead, 1)
			done := make(chan struct{})
			invalidateCalls := 0
			invalidated := sendTerminalWebsocketRead(
				ch, done, codexWebsocketRead{err: terminalErr}, func() {
					invalidateCalls++
				},
			)
			if invalidated || invalidateCalls != 0 {
				t.Fatalf("available channel invalidated=%t calls=%d", invalidated, invalidateCalls)
			}
			if event := <-ch; !errors.Is(event.err, terminalErr) {
				t.Fatalf("terminal error = %v, want %v", event.err, terminalErr)
			}
		},
	)
	t.Run(
		"full channel invalidates before waiting", func(t *testing.T) {
			ch := make(chan codexWebsocketRead, 1)
			ch <- codexWebsocketRead{payload: []byte("queued")}
			done := make(chan struct{})
			defer close(done)
			invalidateCalled := make(chan struct{})
			result := make(chan bool, 1)
			go func() {
				result <- sendTerminalWebsocketRead(
					ch, done, codexWebsocketRead{err: terminalErr}, func() {
						close(invalidateCalled)
					},
				)
			}()
			select {
			case <-invalidateCalled:
			case <-time.After(time.Second):
				t.Fatal("invalidation did not happen before waiting for channel capacity")
			}
			select {
			case <-result:
				t.Fatal("terminal sender returned before capacity was released")
			default:
			}
			<-ch
			select {
			case event := <-ch:
				if !errors.Is(event.err, terminalErr) {
					t.Fatalf("terminal error = %v, want %v", event.err, terminalErr)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for terminal read")
			}
			select {
			case invalidated := <-result:
				if !invalidated {
					t.Fatal("full channel should report early invalidation")
				}
			case <-time.After(time.Second):
				t.Fatal("terminal sender did not finish")
			}
		},
	)
	t.Run(
		"full channel stops when invalidation cancels active read", func(t *testing.T) {
			ch := make(chan codexWebsocketRead, 1)
			ch <- codexWebsocketRead{payload: []byte("queued")}
			done := make(chan struct{})
			invalidated := sendTerminalWebsocketRead(
				ch, done, codexWebsocketRead{err: terminalErr}, func() {
					close(done)
				},
			)
			if !invalidated || len(ch) != 1 {
				t.Fatalf("full channel invalidated=%t length=%d, want true/1", invalidated, len(ch))
			}
		},
	)
}

func assertOriginalWebsocketWriteErrorMapping(t *testing.T) {
	t.Helper()
	networkWriteErr := errors.New("write: broken pipe")
	for _, test := range []struct {
		name       string
		closeCode  int
		writeErr   error
		wantStatus int
	}{
		{
			"close sent after message too big is request scoped", websocket.CloseMessageTooBig,
			websocket.ErrCloseSent, http.StatusRequestEntityTooLarge,
		},
		{
			"network write error after message too big is request scoped", websocket.CloseMessageTooBig,
			networkWriteErr, http.StatusRequestEntityTooLarge,
		},
		{"other close keeps stale connection retry", websocket.CloseNormalClosure, websocket.ErrCloseSent, 0},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				sess := &codexWebsocketSession{}
				conn := &websocket.Conn{}
				sess.resetUpstreamDisconnectError(conn)
				sess.setUpstreamDisconnectError(conn, &websocket.CloseError{Code: test.closeCode})
				mapped := mapCodexWebsocketWriteError(sess, conn, test.writeErr)
				if test.wantStatus == 0 {
					if !errors.Is(mapped, test.writeErr) {
						t.Fatalf("ordinary write error changed: %v", mapped)
					}
					return
				}
				status, ok := mapped.(interface{ StatusCode() int })
				if !ok || status.StatusCode() != test.wantStatus {
					t.Fatalf("mapped status = %v, want %d; err=%v", status, test.wantStatus, mapped)
				}
				var scoped core.RequestScopedError
				if !errors.As(mapped, &scoped) || !scoped.IsRequestScoped() {
					t.Fatalf("mapped error is not request scoped: %T", mapped)
				}
				var disposition core.ExecutionDispositionError
				if !errors.As(mapped, &disposition) ||
					disposition.ExecutionDisposition().RetryDirective != core.RetryDirectiveNoRetry {
					t.Fatalf("mapped write error permits retry: %v", mapped)
				}
			},
		)
	}
}

func TestMapCodexWebsocketWriteErrorStopsRetryForMessageTooBig(t *testing.T) {
	assertOriginalWebsocketWriteErrorMapping(t)
}

func TestMapXAIWebsocketWriteErrorStopsRetryForMessageTooBig(t *testing.T) {
	assertOriginalWebsocketWriteErrorMapping(t)
}

func TestMapCodexWebsocketWriteErrorDoesNotReusePriorConnectionClose(t *testing.T) {
	sess := &codexWebsocketSession{}
	priorConn, replacementConn := &websocket.Conn{}, &websocket.Conn{}
	sess.resetUpstreamDisconnectError(priorConn)
	sess.setUpstreamDisconnectError(priorConn, &websocket.CloseError{Code: websocket.CloseMessageTooBig})
	prior := mapCodexWebsocketWriteError(sess, priorConn, websocket.ErrCloseSent)
	var scoped core.RequestScopedError
	if !errors.As(prior, &scoped) || !scoped.IsRequestScoped() {
		t.Fatalf("prior 1009 error lost request scope: %v", prior)
	}
	sess.resetUpstreamDisconnectError(replacementConn)
	sess.setUpstreamDisconnectError(priorConn, &websocket.CloseError{Code: websocket.CloseMessageTooBig})
	sess.setUpstreamDisconnectError(replacementConn, &websocket.CloseError{Code: websocket.CloseNormalClosure})
	replacement := mapCodexWebsocketWriteError(sess, replacementConn, websocket.ErrCloseSent)
	if !errors.Is(replacement, websocket.ErrCloseSent) {
		t.Fatalf("replacement connection inherited prior 1009: %v", replacement)
	}
}

func assertOriginalMessageTooBigStream(t *testing.T, provider string) {
	t.Helper()
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				upgrader := websocket.Upgrader{}
				conn, err := upgrader.Upgrade(w, request, nil)
				if err != nil {
					t.Errorf("upgrade websocket: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, err := conn.ReadMessage(); err != nil {
					t.Errorf("read upstream websocket message: %v", err)
					return
				}
				closeMessage := websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "message too big")
				if err := conn.WriteControl(
					websocket.CloseMessage, closeMessage, time.Now().Add(time.Second),
				); err != nil {
					t.Errorf("write close websocket message: %v", err)
				}
			},
		),
	)
	defer server.Close()
	credential := &coreauth.Auth{
		ProxyURL:   "none",
		Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL},
	}
	model := "gpt-5-codex"
	var implementation coreauth.ProviderExecutor = newCodexWebsocketsExecutor(
		&config.Config{DisableImageGeneration: config.DisableImageGenerationAll},
	)
	if provider == "xai" {
		model, implementation = "grok-4.5", newXAIWebsocketsExecutor(&config.Config{})
		credential.Provider = "xai"
		credential.Attributes = map[string]string{"base_url": server.URL, "websockets": "true"}
		credential.Metadata = map[string]any{"access_token": "xai-token"}
	}
	request := core.Request{
		Model: model, Payload: []byte(`{"model":"` + model +
			`","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}
	opts := core.Options{SourceFormat: translator.FormatOpenAIResponse, ResponseFormat: translator.FormatOpenAIResponse}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := implementation.ExecuteStream(ctx, credential, request, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	select {
	case chunk, ok := <-result.Chunks:
		if !ok || chunk.Err == nil {
			t.Fatal("stream did not deliver a message-too-big error")
		}
		status, ok := chunk.Err.(interface{ StatusCode() int })
		if !ok || status.StatusCode() != http.StatusRequestEntityTooLarge {
			t.Fatalf("status error = %v, want 413; err=%v", status, chunk.Err)
		}
		if code := gjson.Get(chunk.Err.Error(), "error.code").String(); code != "message_too_big" {
			t.Fatalf("error code = %q, want message_too_big", code)
		}
		var scoped core.RequestScopedError
		if !errors.As(chunk.Err, &scoped) || !scoped.IsRequestScoped() {
			t.Fatalf("message-too-big error is not request scoped: %T", chunk.Err)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for error stream chunk")
	}
	for range result.Chunks {
	}
}

func TestCodexWebsocketsExecuteStreamMapsMessageTooBigClose(t *testing.T) {
	assertOriginalMessageTooBigStream(t, "codex")
}

func TestXAIWebsocketsExecuteStreamMapsMessageTooBigClose(t *testing.T) {
	assertOriginalMessageTooBigStream(t, "xai")
}

func TestCodexWebsocketsUpstreamDisconnectChanSignalsOnInvalidate(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				upgrader := websocket.Upgrader{}
				conn, err := upgrader.Upgrade(w, request, nil)
				if err != nil {
					t.Errorf("upgrade websocket: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
			},
		),
	)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	implementation := newCodexWebsocketsExecutor(&config.Config{})
	disconnect := implementation.UpstreamDisconnectChan("sess-1")
	if disconnect == nil {
		t.Fatal("expected disconnect channel")
	}
	session := implementation.getOrCreateSession("sess-1")
	if session == nil {
		t.Fatal("expected session")
	}
	session.connMu.Lock()
	session.conn, session.readerConn = conn, conn
	session.authID, session.wsURL = "auth-1", "ws://example.test/responses"
	session.connMu.Unlock()
	upstreamErr := errors.New("upstream gone")
	implementation.invalidateUpstreamConn(session, conn, "test_invalidate", upstreamErr)
	select {
	case err, ok := <-disconnect:
		if !ok || err == nil || err.Error() != upstreamErr.Error() {
			t.Fatalf("disconnect error = %v, want %v", err, upstreamErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for disconnect signal")
	}
}

func TestXAIWebsocketsEnabledForConfigAPIKey(t *testing.T) {
	credential := &coreauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"api_key": "xai-key", "websockets": "true"},
	}
	if !xaiWebsocketsEnabled(credential) {
		t.Fatal("xaiWebsocketsEnabled() = false, want true")
	}
}
