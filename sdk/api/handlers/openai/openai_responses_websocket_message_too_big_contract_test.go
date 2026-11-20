package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestWriteWebsocketCloseForUpstreamErrorMirrorsMessageTooBig(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		reason string
	}{
		{
			"raw close error", &websocket.CloseError{Code: websocket.CloseMessageTooBig, Text: "message too big"},
			"message too big",
		},
		{
			"mapped stream error", &responsesWebsocketPayloadError{
				status:  http.StatusRequestEntityTooLarge,
				payload: []byte(`{"error":{"message":"upstream websocket message too big","code":"message_too_big"}}`),
			},
			"upstream websocket message too big",
		},
		{
			"multibyte reason stays valid", &websocket.CloseError{
				Code: websocket.CloseMessageTooBig,
				Text: strings.Repeat("🙂", 31),
			}, strings.Repeat("🙂", 30),
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				serverErr := make(chan error, 1)
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, request *http.Request) {
							conn, err := responsesWebsocketUpgrader.Upgrade(w, request, nil)
							if err != nil {
								serverErr <- err
								return
							}
							matched, wrote, errWrite := newResponsesWebsocketWriter(
								conn,
							).closeForUpstreamError(test.err)
							if (!matched || !wrote) && errWrite == nil {
								errWrite = errors.New("message-too-big error did not send a close frame")
							}
							serverErr <- errWrite
						},
					),
				)
				defer server.Close()
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
				if err != nil {
					t.Fatalf("dial websocket: %v", err)
				}
				defer func() { _ = conn.Close() }()
				if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				_, _, err = conn.ReadMessage()
				var closeErr *websocket.CloseError
				if !errors.As(
					err, &closeErr,
				) || closeErr.Code != websocket.CloseMessageTooBig || closeErr.Text != test.reason {
					t.Fatalf("downstream close = %v, want 1009/%q", err, test.reason)
				}
				if err := <-serverErr; err != nil {
					t.Fatalf("close server websocket: %v", err)
				}
			},
		)
	}
}

func TestResponsesWebsocketWriterCloseDoesNotWaitForActiveDataWriter(t *testing.T) {
	serverErr := make(chan error, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				conn, err := responsesWebsocketUpgrader.Upgrade(w, request, nil)
				if err != nil {
					serverErr <- err
					return
				}
				defer func() { _ = conn.Close() }()
				writer := newResponsesWebsocketWriter(conn)
				writer.writeMu.Lock()
				closed := make(chan error, 1)
				go func() {
					matched, wrote, errClose := writer.closeForUpstreamError(
						&websocket.CloseError{
							Code: websocket.CloseMessageTooBig, Text: "message too big",
						},
					)
					if (!matched || wrote) && errClose == nil {
						errClose = errors.New("blocked writer did not close the transport directly")
					}
					closed <- errClose
				}()
				select {
				case err := <-closed:
					writer.writeMu.Unlock()
					serverErr <- err
				case <-time.After(time.Second):
					writer.writeMu.Unlock()
					serverErr <- errors.New("close waited behind active data writer")
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
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("client read succeeded, want connection closure")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestTruncateWebsocketCloseReason(t *testing.T) {
	for _, test := range []struct {
		name     string
		reason   string
		maxBytes int
		want     string
	}{
		{"non-positive limit", "message too big", 0, ""},
		{"short valid reason unchanged", "message too big", wsCloseReasonMaxBytes, "message too big"},
		{
			"long ascii reason", strings.Repeat("x", 1<<20), wsCloseReasonMaxBytes,
			strings.Repeat("x", wsCloseReasonMaxBytes),
		},
		{
			"long invalid reason", strings.Repeat("\xff", 1<<20), wsCloseReasonMaxBytes,
			strings.Repeat("�", wsCloseReasonMaxBytes/utf8.RuneLen(utf8.RuneError)),
		},
		{"multibyte rune does not fit", "ab🙂cd", 5, "ab"},
		{"invalid bytes become replacement runes", string([]byte{'a', 0xff, 0xfe, 'b'}), 8, "a��b"},
		{"invalid replacement does not cross limit", string([]byte{'a', 'b', 0xff, 'c'}), 4, "ab"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				got := truncateWebsocketCloseReason(test.reason, test.maxBytes)
				if got != test.want || !utf8.ValidString(got) ||
					(test.maxBytes > 0 && len(got) > test.maxBytes) {
					t.Fatalf("close reason = %q, want %q within %d bytes", got, test.want, test.maxBytes)
				}
			},
		)
	}
}

func TestForwardResponsesWebsocketMirrorsMappedMessageTooBig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	serverErr := make(chan error, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				conn, err := responsesWebsocketUpgrader.Upgrade(w, request, nil)
				if err != nil {
					serverErr <- err
					return
				}
				defer func() { _ = conn.Close() }()
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = request
				data := make(chan coreexecutor.StreamChunk)
				errs := make(chan *handlers.ErrorMessage, 1)
				errs <- &handlers.ErrorMessage{
					StatusCode: http.StatusRequestEntityTooLarge,
					Error: &responsesWebsocketPayloadError{
						status: http.StatusRequestEntityTooLarge,
						payload: []byte((`{"error":{"message":"upstream websocket message too big","code":` +
							`"message_too_big"}}`)),
					},
				}
				handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
				_, _, _, errMsg, errForward := handler.forwardResponsesWebsocket(
					ctx, newResponsesWebsocketWriter(conn), func(...any) {}, data, errs,
					newWebsocketTimelineRecorder(ctx), "session-1", "downstream-1",
				)
				if errMsg == nil || errMsg.StatusCode != http.StatusRequestEntityTooLarge {
					serverErr <- fmt.Errorf("forward error message = %#v, want status 413", errMsg)
					return
				}
				if !errors.Is(errForward, websocket.ErrCloseSent) {
					serverErr <- fmt.Errorf("forward error = %v, want %v", errForward, websocket.ErrCloseSent)
					return
				}
				serverErr <- nil
			},
		),
	)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("downstream close = %v, want 1009", err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

type websocketUpstreamDisconnectExecutor struct {
	prewarmCaptureExecutor
	provider   string
	mu         sync.Mutex
	sessions   map[string]chan error
	subscribed chan string
	closed     chan string
}

func (e *websocketUpstreamDisconnectExecutor) Identifier() string { return e.provider }

func (e *websocketUpstreamDisconnectExecutor) UpstreamDisconnectChan(sessionID string) <-chan error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sessions == nil {
		e.sessions = make(map[string]chan error)
	}
	ch := e.sessions[sessionID]
	if ch == nil {
		ch = make(chan error, 1)
		e.sessions[sessionID] = ch
	}
	select {
	case e.subscribed <- sessionID:
	default:
	}
	return ch
}

func (e *websocketUpstreamDisconnectExecutor) TriggerDisconnect(sessionID string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ch := e.sessions[sessionID]; ch != nil {
		delete(e.sessions, sessionID)
		ch <- err
		close(ch)
	}
}

func (e *websocketUpstreamDisconnectExecutor) CloseExecutionSession(sessionID string) {
	e.TriggerDisconnect(sessionID, nil)
	select {
	case e.closed <- sessionID:
	default:
	}
}

func TestResponsesWebsocketMirrorsUpstreamMessageTooBigDisconnect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, provider := range []string{"codex", "xai"} {
		t.Run(
			provider, func(t *testing.T) {
				executor := &websocketUpstreamDisconnectExecutor{
					provider:   provider,
					subscribed: make(chan string, 1), closed: make(chan string, 1),
				}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.RegisterExecutor(executor)
				handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
				router := gin.New()
				router.GET("/v1/responses/ws", handler.ResponsesWebsocket)
				server := httptest.NewServer(router)
				defer server.Close()
				conn, _, err := websocket.DefaultDialer.Dial(
					"ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws", nil,
				)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conn.Close() }()
				var sessionID string
				select {
				case sessionID = <-executor.subscribed:
				case <-time.After(5 * time.Second):
					t.Fatal("timed out waiting for upstream disconnect subscription")
				}
				executor.TriggerDisconnect(
					sessionID, &websocket.CloseError{
						Code: websocket.CloseMessageTooBig, Text: "message too big",
					},
				)
				if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
					t.Fatal(err)
				}
				_, _, err = conn.ReadMessage()
				var closeErr *websocket.CloseError
				if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig ||
					closeErr.Text != "message too big" {
					t.Fatalf("downstream close = %v, want 1009/message too big", err)
				}
				select {
				case closed := <-executor.closed:
					if closed != sessionID {
						t.Fatalf("closed session = %q, want %q", closed, sessionID)
					}
				case <-time.After(time.Second):
					t.Fatal("upstream execution session was not released")
				}
			},
		)
	}
}

type prewarmWriteCompletionListener struct {
	net.Listener
	paused  chan struct{}
	release <-chan struct{}
}

func (l *prewarmWriteCompletionListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &prewarmWriteCompletionConn{Conn: conn, paused: l.paused, release: l.release}, nil
}

type prewarmWriteCompletionConn struct {
	net.Conn
	paused  chan struct{}
	release <-chan struct{}
}

func (c *prewarmWriteCompletionConn) Write(payload []byte) (int, error) {
	written, err := c.Conn.Write(payload)
	if err == nil && bytes.Contains(payload, []byte(`"type":"response.done"`)) {
		// Pause after the frame reaches the client, before WriteMessage releases writeMu.
		close(c.paused)
		<-c.release
	}
	return written, err
}

func TestResponsesWebsocketMessageTooBigAfterPrewarmWriteCompletion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, provider := range []string{"codex", "xai"} {
		t.Run(
			provider, func(t *testing.T) {
				executor := &websocketUpstreamDisconnectExecutor{
					provider: provider, subscribed: make(chan string, 2),
				}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.RegisterExecutor(executor)
				const model = "message-too-big-prewarm-model"
				auth := &coreauth.Auth{
					ID: t.Name(), Provider: provider, Status: coreauth.StatusActive,
					Attributes: map[string]string{"api_key": "synthetic-token", "websockets": "true"},
				}
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
				router := gin.New()
				router.GET("/v1/responses/ws", handler.ResponsesWebsocket)
				paused := make(chan struct{})
				release := make(chan struct{})
				releaseWrite := sync.OnceFunc(func() { close(release) })
				server := httptest.NewUnstartedServer(router)
				server.Listener = &prewarmWriteCompletionListener{
					Listener: server.Listener, paused: paused, release: release,
				}
				server.Start()
				defer func() {
					releaseWrite()
					server.Close()
				}()
				client, _, err := websocket.DefaultDialer.Dial(
					"ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws",
					http.Header{"Session-Id": {t.Name()}},
				)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = client.Close() }()
				if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				var sessionID string
				select {
				case sessionID = <-executor.subscribed:
				case <-time.After(time.Second):
					t.Fatal("initial upstream disconnect subscription is missing")
				}
				read := func(want string) {
					t.Helper()
					_, payload, err := client.ReadMessage()
					if err != nil || gjson.GetBytes(payload, "type").String() != want {
						t.Fatalf("event = %s, error = %v, want %s", payload, err, want)
					}
				}
				if err := client.WriteMessage(
					websocket.TextMessage, []byte(
						`{"type":"response.create","model":"`+model+
							`","input":[{"type":"message","role":"user","content":"hello"}]}`,
					),
				); err != nil {
					t.Fatal(err)
				}
				read(wsEventTypeCompleted)
				updated, found := manager.GetByID(auth.ID)
				if !found {
					t.Fatal("registered credential is missing")
				}
				updated.Attributes["websockets"] = "false"
				if _, err := manager.Update(context.Background(), updated); err != nil {
					t.Fatal(err)
				}
				if err := client.WriteMessage(
					websocket.TextMessage, []byte(
						`{"type":"response.create","model":"`+model+
							`","generate":false,"input":[{"type":"message","role":"user","content":"prewarm"}]}`,
					),
				); err != nil {
					t.Fatal(err)
				}
				read("response.created")
				read(wsEventTypeDone)
				select {
				case <-paused:
				case <-time.After(time.Second):
					t.Fatal("prewarm write completion was not reached")
				}
				var currentID string
				select {
				case currentID = <-executor.subscribed:
				default:
					releaseWrite()
					select {
					case currentID = <-executor.subscribed:
					case <-time.After(time.Second):
						t.Fatal("replacement upstream disconnect subscription is missing")
					}
				}
				if currentID != sessionID {
					t.Fatalf("new execution session = %q, want %q", currentID, sessionID)
				}
				executor.TriggerDisconnect(
					sessionID, &websocket.CloseError{
						Code: websocket.CloseMessageTooBig, Text: "message too big",
					},
				)
				_, _, err = client.ReadMessage()
				var closeErr *websocket.CloseError
				if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig ||
					closeErr.Text != "message too big" {
					t.Fatalf("replacement downstream close = %v, want 1009/message too big", err)
				}
			},
		)
	}
}

func TestResponsesWebsocketMessageTooBigSubscriptionFollowsPrewarmReset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, provider := range []string{"codex", "xai"} {
		t.Run(
			provider, func(t *testing.T) {
				executor := &websocketUpstreamDisconnectExecutor{
					provider:   provider,
					subscribed: make(chan string, 2),
				}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.RegisterExecutor(executor)
				const model = "message-too-big-prewarm-model"
				auth := &coreauth.Auth{
					ID: t.Name(), Provider: provider, Status: coreauth.StatusActive,
					Attributes: map[string]string{"api_key": "synthetic-token", "websockets": "true"},
				}
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().
					RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
				router := gin.New()
				router.GET("/v1/responses/ws", handler.ResponsesWebsocket)
				server := httptest.NewServer(router)
				defer server.Close()
				client, _, err := websocket.DefaultDialer.Dial(
					"ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws",
					http.Header{"Session-Id": {t.Name()}},
				)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = client.Close() }()
				if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				var sessionID string
				select {
				case sessionID = <-executor.subscribed:
				case <-time.After(time.Second):
					t.Fatal("initial upstream disconnect subscription is missing")
				}
				executor.mu.Lock()
				prior := executor.sessions[sessionID]
				executor.mu.Unlock()
				read := func(want string) {
					t.Helper()
					_, payload, err := client.ReadMessage()
					if err != nil || gjson.GetBytes(payload, "type").String() != want {
						t.Fatalf("event = %s, error = %v, want %s", payload, err, want)
					}
				}
				if err := client.WriteMessage(
					websocket.TextMessage, []byte(`{"type":"response.create","model":"`+model+
						`","input":[{"type":"message","role":"user","content":"hello"}]}`),
				); err != nil {
					t.Fatal(err)
				}
				read(wsEventTypeCompleted)
				updated, found := manager.GetByID(auth.ID)
				if !found {
					t.Fatal("registered credential is missing")
				}
				updated.Attributes["websockets"] = "false"
				if _, err := manager.Update(context.Background(), updated); err != nil {
					t.Fatal(err)
				}
				if err := client.WriteMessage(
					websocket.TextMessage, []byte(`{"type":"response.create","model":"`+model+
						`","generate":false,"input":[{"type":"message","role":"user","content":"prewarm"}]}`),
				); err != nil {
					t.Fatal(err)
				}
				read("response.created")
				read(wsEventTypeDone)
				select {
				case currentID := <-executor.subscribed:
					if currentID != sessionID {
						t.Fatalf("new execution session = %q, want %q", currentID, sessionID)
					}
				case <-time.After(time.Second):
					t.Fatal("replacement upstream disconnect subscription is missing")
				}
				executor.mu.Lock()
				current := executor.sessions[sessionID]
				executor.mu.Unlock()
				if current == nil || current == prior {
					t.Fatal("prewarm retained the retired upstream disconnect channel")
				}
				executor.TriggerDisconnect(
					sessionID, &websocket.CloseError{
						Code: websocket.CloseMessageTooBig, Text: "message too big",
					},
				)
				_, _, err = client.ReadMessage()
				var closeErr *websocket.CloseError
				if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig ||
					closeErr.Text != "message too big" {
					t.Fatalf("replacement downstream close = %v, want 1009/message too big", err)
				}
			},
		)
	}
}
