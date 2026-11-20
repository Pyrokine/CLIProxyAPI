package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
)

type rejectSecondBindLifecycle struct {
	binds atomic.Int32
}

func (l *rejectSecondBindLifecycle) Bind(func() error) error {
	if l.binds.Add(1) > 1 {
		return fmt.Errorf("retry lifecycle bind rejected")
	}
	return nil
}

func (*rejectSecondBindLifecycle) End(string) {}

func TestCodexWebsocketSessionActiveChannelBelongsToConnection(t *testing.T) {
	sess := &codexWebsocketSession{}
	oldConn := &websocket.Conn{}
	newConn := &websocket.Conn{}
	oldCh := make(chan codexWebsocketRead, 1)
	newCh := make(chan codexWebsocketRead, 1)

	sess.setActive(oldConn, oldCh)
	if ch, _, _ := sess.activeForConn(oldConn); ch != oldCh {
		t.Fatal("old connection did not own its active channel")
	}

	sess.setActive(newConn, newCh)
	if sess.clearActive(oldConn, oldCh) {
		t.Fatal("old connection cleared the new active channel")
	}
	if ch, _, _ := sess.activeForConn(oldConn); ch != nil {
		t.Fatal("old connection retained access to an active channel")
	}
	if ch, _, _ := sess.activeForConn(newConn); ch != newCh {
		t.Fatal("new connection lost its active channel")
	}
	if !sess.clearActive(newConn, newCh) {
		t.Fatal("new connection could not clear its active channel")
	}

	closedOldCh := make(chan codexWebsocketRead, 4096)
	sess.setActive(oldConn, closedOldCh)
	if !sess.clearActive(oldConn, closedOldCh) {
		t.Fatal("old connection could not clear its active channel before retry")
	}
	close(closedOldCh)
	retryCh := make(chan codexWebsocketRead, 4096)
	sess.setActive(newConn, retryCh)
	if retryCh == closedOldCh {
		t.Fatal("retry reused the old connection's read channel")
	}
	select {
	case retryCh <- codexWebsocketRead{conn: newConn}:
	default:
		t.Fatal("retry read channel was not writable")
	}
}

type trackedWebsocketLifecycle struct {
	mu    sync.Mutex
	close func() error
	once  sync.Once
	ends  atomic.Int32
}

type drainDuringBindWebsocketLifecycle struct{}

func (drainDuringBindWebsocketLifecycle) Bind(closeFn func() error) error {
	if errClose := closeFn(); errClose != nil {
		return errClose
	}
	return fmt.Errorf("execution lifecycle drained during Bind")
}

func (drainDuringBindWebsocketLifecycle) End(string) {}

func (l *trackedWebsocketLifecycle) Bind(closeFn func() error) error {
	l.mu.Lock()
	l.close = closeFn
	l.mu.Unlock()
	return nil
}

func (l *trackedWebsocketLifecycle) End(string) {
	l.once.Do(
		func() {
			l.ends.Add(1)
			l.mu.Lock()
			closeFn := l.close
			l.mu.Unlock()
			if closeFn != nil {
				_ = closeFn()
			}
		},
	)
}

func TestClearRetryActiveStateClearsOriginalConnection(t *testing.T) {
	sess := &codexWebsocketSession{}
	originalConn := &websocket.Conn{}
	originalCh := make(chan codexWebsocketRead, 4096)
	sess.setActive(originalConn, originalCh)
	if !sess.clearActive(originalConn, originalCh) {
		t.Fatal("clearRetryActiveState() = false, want true")
	}
	if ch, done, _ := sess.activeForConn(originalConn); ch != nil || done != nil {
		t.Fatalf("original active state = %v/%v, want nil", ch, done)
	}
}

func TestWebsocketRetryBindFailureClearsActiveSessionState(t *testing.T) {
	tests := []struct {
		name string
		run  func(t *testing.T, baseURL string) (func(cliproxyexecutor.Options) error, *codexWebsocketSession)
	}{
		{
			name: "Codex nonstream",
			run: func(t *testing.T, baseURL string) (func(cliproxyexecutor.Options) error, *codexWebsocketSession) {
				executor := newCodexWebsocketsExecutor(
					&config.Config{DisableImageGeneration: config.DisableImageGenerationAll},
				)
				executor.sessions = make(map[string]*codexWebsocketSession)
				auth := &cliproxyauth.Auth{
					ID: "retry-bind-codex", Provider: "codex",
					Attributes: map[string]string{"api_key": "test-key", "base_url": baseURL},
				}
				req := cliproxyexecutor.Request{
					Model: "gpt-5-codex",
					Payload: []byte(
						`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`,
					),
				}
				primed := false
				return func(runOpts cliproxyexecutor.Options) error {
					if !primed {
						wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/responses"
						conn, _, _, errEnsure := executor.ensureUpstreamConn(
							context.Background(),
							auth,
							executor.getOrCreateSession("retry-bind"),
							auth.ID,
							wsURL,
							http.Header{},
						)
						if errEnsure != nil {
							return errEnsure
						}
						if errDeadline := conn.SetWriteDeadline(time.Now().Add(-time.Second)); errDeadline != nil {
							return errDeadline
						}
						primed = true
					}
					_, errExecute := executor.Execute(context.Background(), auth, req, runOpts)
					return errExecute
				}, executor.getOrCreateSession("retry-bind")
			},
		},
		{
			name: "Codex stream",
			run: func(t *testing.T, baseURL string) (func(cliproxyexecutor.Options) error, *codexWebsocketSession) {
				executor := newCodexWebsocketsExecutor(
					&config.Config{DisableImageGeneration: config.DisableImageGenerationAll},
				)
				executor.sessions = make(map[string]*codexWebsocketSession)
				auth := &cliproxyauth.Auth{
					ID: "retry-bind-codex", Provider: "codex",
					Attributes: map[string]string{"api_key": "test-key", "base_url": baseURL},
				}
				req := cliproxyexecutor.Request{
					Model: "gpt-5-codex",
					Payload: []byte(
						`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`,
					),
				}
				primed := false
				return func(runOpts cliproxyexecutor.Options) error {
					if !primed {
						wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/responses"
						conn, _, _, errEnsure := executor.ensureUpstreamConn(
							context.Background(),
							auth,
							executor.getOrCreateSession("retry-bind"),
							auth.ID,
							wsURL,
							http.Header{},
						)
						if errEnsure != nil {
							return errEnsure
						}
						if errDeadline := conn.SetWriteDeadline(time.Now().Add(-time.Second)); errDeadline != nil {
							return errDeadline
						}
						primed = true
					}
					result, errExecute := executor.ExecuteStream(context.Background(), auth, req, runOpts)
					if errExecute != nil {
						return errExecute
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							return chunk.Err
						}
					}
					return nil
				}, executor.getOrCreateSession("retry-bind")
			},
		},
		{
			name: "xAI stream",
			run: func(t *testing.T, baseURL string) (func(cliproxyexecutor.Options) error, *codexWebsocketSession) {
				executor := newXAIWebsocketsExecutor(&config.Config{})
				executor.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				auth := &cliproxyauth.Auth{
					ID: "retry-bind-xai", Provider: "xai",
					Attributes: map[string]string{"base_url": baseURL, "websockets": "true"},
					Metadata:   map[string]any{"access_token": "test-token"},
				}
				req := cliproxyexecutor.Request{
					Model: "grok-4",
					Payload: []byte(
						`{"model":"grok-4","input":[{"type":"message","role":"user","content":"hello"}]}`,
					),
				}
				primed := false
				return func(runOpts cliproxyexecutor.Options) error {
					if !primed {
						wsURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/responses"
						conn, _, _, errEnsure := executor.ensureUpstreamConn(
							context.Background(),
							auth,
							executor.store.getOrCreate("retry-bind"),
							auth.ID,
							wsURL,
							http.Header{},
						)
						if errEnsure != nil {
							return errEnsure
						}
						if errDeadline := conn.SetWriteDeadline(time.Now().Add(-time.Second)); errDeadline != nil {
							return errDeadline
						}
						primed = true
					}
					result, errExecute := executor.ExecuteStream(context.Background(), auth, req, runOpts)
					if errExecute != nil {
						return errExecute
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							return chunk.Err
						}
					}
					return nil
				}, executor.store.getOrCreate("retry-bind")
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				var connections atomic.Int32
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							conn, errUpgrade := upgrader.Upgrade(w, r, nil)
							if errUpgrade != nil {
								t.Errorf("upgrade websocket: %v", errUpgrade)
								return
							}
							connection := connections.Add(1)
							defer func() { _ = conn.Close() }()
							if connection == 1 {
								_, _, _ = conn.ReadMessage()
								return
							}
							if connection == 2 {
								_, _, _ = conn.ReadMessage()
								return
							}
							if _, _, errRead := conn.ReadMessage(); errRead != nil {
								return
							}
							completed := []byte(
								(`{"type":"response.completed","response":{"id":"response-1","output":[]` +
									`,"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`),
							)
							if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
								t.Errorf("write websocket completion: %v", errWrite)
							}
						},
					),
				)
				defer server.Close()

				lifecycle := &rejectSecondBindLifecycle{}
				opts := cliproxyexecutor.Options{
					SourceFormat:   sdktranslator.FormatOpenAIResponse,
					ResponseFormat: sdktranslator.FormatOpenAIResponse, ExecutionLifecycle: lifecycle,
					Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "retry-bind"},
				}
				run, sess := test.run(t, server.URL)
				if errRun := run(opts); errRun == nil {
					t.Fatal("first request error = nil, want the original write failure")
				}
				if got := lifecycle.binds.Load(); got != 1 {
					t.Fatalf("write failure automatically rebound the lifecycle %d times, want 1", got)
				}
				if errRun := run(opts); errRun == nil || !strings.Contains(
					errRun.Error(), "retry lifecycle bind rejected",
				) {
					t.Fatalf("explicit retry error = %v, want lifecycle bind rejection", errRun)
				}
				if got := lifecycle.binds.Load(); got != 2 {
					t.Fatalf("lifecycle binds = %d, want 2", got)
				}
				sess.activeMu.Lock()
				active := sess.activeConn != nil || sess.activeCh != nil || sess.activeDone != nil ||
					sess.activeCancel != nil
				sess.activeMu.Unlock()
				if active {
					t.Fatal("retry bind failure left the old active websocket state")
				}

				opts.ExecutionLifecycle = nil
				if errRun := run(opts); errRun != nil {
					t.Fatalf("second request error = %v", errRun)
				}
				if got := connections.Load(); got != 3 {
					t.Fatalf("websocket connections = %d, want 3 after retry bind failure", got)
				}
			},
		)
	}
}

func TestWebsocketSessionCloseEndsRetainedLifecycleOnce(t *testing.T) {
	exec := newCodexWebsocketsExecutor(&config.Config{})
	exec.sessions = make(map[string]*codexWebsocketSession)
	server, closed := newWebsocketTargetServer(t)
	defer server.Close()

	sess := exec.getOrCreateSession("retained-lifecycle")
	if sess == nil {
		t.Fatal("retained lifecycle session was not created")
	}
	auth := &cliproxyauth.Auth{ID: "auth-a"}
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn := ensureWebsocketTargetConn(t, exec.ensureUpstreamConn, auth, sess, auth.ID, wsURL)
	lifecycle := &trackedWebsocketLifecycle{}
	if errBind := sess.bindExecutionLifecycle(
		context.Background(), cliproxyexecutor.Options{ExecutionLifecycle: lifecycle}, conn, sess.connCloser,
	); errBind != nil {
		t.Fatalf("bind execution lifecycle: %v", errBind)
	}

	exec.CloseExecutionSession("retained-lifecycle")
	lifecycle.End("duplicate_close")
	if got := lifecycle.ends.Load(); got != 1 {
		t.Fatalf("lifecycle End calls = %d, want 1", got)
	}
	if got := <-closed; got != auth.ID {
		t.Fatalf("closed server auth = %q, want %q", got, auth.ID)
	}
}

type closeCountingNetConn struct {
	net.Conn
	closes atomic.Int32
}

func (c *closeCountingNetConn) Close() error {
	c.closes.Add(1)
	return c.Conn.Close()
}

func newCloseCountingWebsocketConn(t *testing.T, rawURL string) (*websocket.Conn, *closeCountingNetConn) {
	t.Helper()
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil {
		t.Fatalf("parse websocket URL: %v", errParse)
	}
	conn, errDial := net.Dial("tcp", parsed.Host)
	if errDial != nil {
		t.Fatalf("dial websocket: %v", errDial)
	}
	counting := &closeCountingNetConn{Conn: conn}
	dialer := websocket.Dialer{
		NetDialContext: func(context.Context, string, string) (net.Conn, error) { return counting, nil },
		ReadBufferSize: 1024, WriteBufferSize: 1024,
	}
	wsConn, _, errClient := dialer.DialContext(t.Context(), rawURL, nil)
	if errClient != nil {
		_ = counting.Close()
		t.Fatalf("create websocket client: %v", errClient)
	}
	return wsConn, counting
}

func TestSessionlessWebsocketSelectionEndAndDirectCloseRaceClosesOnce(t *testing.T) {
	server, _ := newWebsocketTargetServer(t)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, physical := newCloseCountingWebsocketConn(t, wsURL)
	closer := newWebsocketConnectionCloser(conn)
	lifecycle := &trackedWebsocketLifecycle{}
	if errBind := (*codexWebsocketSession)(nil).bindExecutionLifecycle(
		context.Background(), cliproxyexecutor.Options{ExecutionLifecycle: lifecycle}, conn, closer,
	); errBind != nil {
		t.Fatalf("bind sessionless lifecycle: %v", errBind)
	}

	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		lifecycle.End("selection_ended")
	}()
	go func() {
		defer wait.Done()
		if errClose := closer.Close(); errClose != nil {
			t.Errorf("direct close: %v", errClose)
		}
	}()
	wait.Wait()

	if got := physical.closes.Load(); got != 1 {
		t.Fatalf("physical websocket closes = %d, want 1", got)
	}
}

func TestWebsocketLifecycleReplacementKeepsNewOwnersConnection(t *testing.T) {
	server, _ := newWebsocketTargetServer(t)
	defer server.Close()
	conn, physical := newCloseCountingWebsocketConn(t, "ws"+strings.TrimPrefix(server.URL, "http"))
	closer := newWebsocketConnectionCloser(conn)
	defer func(closeResource func() error) {
		if err := closeResource(); err != nil {
			t.Errorf("close replaced websocket owner: %v", err)
		}
	}(closer.Close)
	sess := &codexWebsocketSession{conn: conn, connCloser: closer}
	previous := &trackedWebsocketLifecycle{}
	current := &trackedWebsocketLifecycle{}
	for _, lifecycle := range []*trackedWebsocketLifecycle{previous, current} {
		if err := sess.bindExecutionLifecycle(
			context.Background(), cliproxyexecutor.Options{ExecutionLifecycle: lifecycle}, conn, closer,
		); err != nil {
			t.Fatal(err)
		}
	}
	if previous.ends.Load() != 1 || current.ends.Load() != 0 || physical.closes.Load() != 0 {
		t.Fatalf(
			"ownership transfer closed the current connection: previous=%d current=%d closes=%d",
			previous.ends.Load(),
			current.ends.Load(),
			physical.closes.Load(),
		)
	}
	current.End("selection_ended")
	previous.End("duplicate_close")
	if got := physical.closes.Load(); got != 1 {
		t.Fatalf("physical websocket closes = %d, want 1", got)
	}
}

func TestWebsocketDrainDuringBindClosesOwnedConnectionOnce(t *testing.T) {
	server, _ := newWebsocketTargetServer(t)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, physical := newCloseCountingWebsocketConn(t, wsURL)
	closer := newWebsocketConnectionCloser(conn)
	sess := &codexWebsocketSession{
		conn:       conn,
		connCloser: closer,
		wsURL:      wsURL,
		authID:     "auth-a",
		readerConn: conn,
	}

	errBind := sess.bindExecutionLifecycle(
		context.Background(),
		cliproxyexecutor.Options{ExecutionLifecycle: drainDuringBindWebsocketLifecycle{}},
		conn,
		closer,
	)
	if errBind == nil {
		t.Fatal("bind execution lifecycle error = nil, want drain error")
	}
	closeWebsocketAfterBindFailure(sess, conn, closer)

	if got := physical.closes.Load(); got != 1 {
		t.Fatalf("physical websocket closes = %d, want 1", got)
	}
	sess.connMu.Lock()
	defer sess.connMu.Unlock()
	if sess.conn != nil || sess.connCloser != nil || sess.lifecycle != nil {
		t.Fatalf(
			"drained session state = conn:%v closer:%v lifecycle:%v, want detached", sess.conn, sess.connCloser,
			sess.lifecycle,
		)
	}
}

func TestWebsocketTargetReplacementPhysicallyClosesOwnedConnectionOnce(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "Codex"},
		{name: "xAI"},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				serverA, _ := newWebsocketTargetServer(t)
				defer serverA.Close()
				serverB, _ := newWebsocketTargetServer(t)
				defer serverB.Close()

				var ensure func(
					context.Context,
					*cliproxyauth.Auth,
					*codexWebsocketSession,
					string,
					string,
					http.Header,
				) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error)
				var closeSession func(string)
				var sess *codexWebsocketSession
				switch test.name {
				case "Codex":
					exec := newCodexWebsocketsExecutor(&config.Config{})
					exec.sessions = make(map[string]*codexWebsocketSession)
					ensure = exec.ensureUpstreamConn
					closeSession = exec.CloseExecutionSession
					sess = exec.getOrCreateSession("counted-target-change")
				case "xAI":
					exec := newXAIWebsocketsExecutor(&config.Config{})
					exec.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
					ensure = exec.ensureUpstreamConn
					closeSession = exec.CloseExecutionSession
					sess = exec.store.getOrCreate("counted-target-change")
				}
				defer closeSession("counted-target-change")

				//goland:noinspection SpellCheckingInspection
				wsURLA := "ws" + strings.TrimPrefix(serverA.URL, "http")
				//goland:noinspection SpellCheckingInspection
				wsURLB := "ws" + strings.TrimPrefix(serverB.URL, "http")
				connA, physical := newCloseCountingWebsocketConn(t, wsURLA)
				sess.connMu.Lock()
				sess.conn = connA
				sess.connCloser = newWebsocketConnectionCloser(connA)
				sess.wsURL = wsURLA
				sess.authID = "auth-a"
				sess.readerConn = connA
				sess.connMu.Unlock()
				lifecycle := &trackedWebsocketLifecycle{}
				if errBind := sess.bindExecutionLifecycle(
					context.Background(), cliproxyexecutor.Options{ExecutionLifecycle: lifecycle}, connA,
					sess.connCloser,
				); errBind != nil {
					t.Fatalf("bind execution lifecycle: %v", errBind)
				}

				if _, _, _, errEnsure := ensure(
					context.Background(), &cliproxyauth.Auth{ID: "auth-b"}, sess, "auth-b", wsURLB, nil,
				); errEnsure != nil {
					t.Fatalf("replace websocket target: %v", errEnsure)
				}
				if got := physical.closes.Load(); got != 1 {
					t.Fatalf("physical websocket closes = %d, want 1", got)
				}
			},
		)
	}
}

func TestWebsocketLifecycleEndThenInvalidateAndCloseAllPhysicallyClosesOnce(t *testing.T) {
	tests := []struct {
		name string
		run  func(*codexWebsocketSession, *websocket.Conn, *trackedWebsocketLifecycle)
	}{
		{
			name: "Codex",
			run: func(sess *codexWebsocketSession, conn *websocket.Conn, lifecycle *trackedWebsocketLifecycle) {
				exec := newCodexWebsocketsExecutor(&config.Config{})
				exec.sessions = map[string]*codexWebsocketSession{sess.sessionID: sess}
				lifecycle.End("lifecycle_ended")
				exec.invalidateUpstreamConn(sess, conn, "invalidated", nil)
				exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
			},
		},
		{
			name: "xAI",
			run: func(sess *codexWebsocketSession, conn *websocket.Conn, lifecycle *trackedWebsocketLifecycle) {
				exec := newXAIWebsocketsExecutor(&config.Config{})
				exec.store = &xaiWebsocketSessionStore{
					sessions: map[string]*codexWebsocketSession{sess.sessionID: sess},
				}
				lifecycle.End("lifecycle_ended")
				exec.invalidateUpstreamConn(sess, conn, "invalidated", nil)
				exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				server, _ := newWebsocketTargetServer(t)
				defer server.Close()
				wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
				conn, physical := newCloseCountingWebsocketConn(t, wsURL)
				sess := &codexWebsocketSession{
					sessionID: "counted-lifecycle", conn: conn, connCloser: newWebsocketConnectionCloser(conn),
					wsURL: wsURL, authID: "auth-a", readerConn: conn,
				}
				lifecycle := &trackedWebsocketLifecycle{}
				if errBind := sess.bindExecutionLifecycle(
					context.Background(), cliproxyexecutor.Options{ExecutionLifecycle: lifecycle}, conn,
					sess.connCloser,
				); errBind != nil {
					t.Fatalf("bind execution lifecycle: %v", errBind)
				}

				test.run(sess, conn, lifecycle)
				if got := physical.closes.Load(); got != 1 {
					t.Fatalf("physical websocket closes = %d, want 1", got)
				}
			},
		)
	}
}

func TestWebsocketExecutorsReconnectWhenSessionTargetChanges(t *testing.T) {
	t.Run(
		"Codex", func(t *testing.T) {
			exec := newCodexWebsocketsExecutor(&config.Config{})
			exec.sessions = make(map[string]*codexWebsocketSession)
			testWebsocketExecutorReconnectsWhenSessionTargetChanges(
				t,
				exec.UpstreamDisconnectChan,
				exec.getOrCreateSession,
				exec.ensureUpstreamConn,
				exec.CloseExecutionSession,
			)
		},
	)

	t.Run(
		"xAI", func(t *testing.T) {
			exec := newXAIWebsocketsExecutor(&config.Config{})
			exec.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			testWebsocketExecutorReconnectsWhenSessionTargetChanges(
				t,
				exec.UpstreamDisconnectChan,
				exec.store.getOrCreate,
				exec.ensureUpstreamConn,
				exec.CloseExecutionSession,
			)
		},
	)
}

func testWebsocketExecutorReconnectsWhenSessionTargetChanges(
	t *testing.T,
	disconnectChan func(string) <-chan error,
	getSession func(string) *codexWebsocketSession,
	ensureConn func(
		context.Context,
		*cliproxyauth.Auth,
		*codexWebsocketSession,
		string,
		string,
		http.Header,
	) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error),
	closeSession func(string),
) {
	t.Helper()

	serverA, closedA := newWebsocketTargetServer(t)
	defer serverA.Close()
	serverB, closedB := newWebsocketTargetServer(t)
	defer serverB.Close()

	sessionID := "target-switch-session"
	disconnectCh := disconnectChan(sessionID)
	sess := getSession(sessionID)
	if sess == nil {
		t.Fatal("expected websocket session")
	}
	defer closeSession(sessionID)

	authA := &cliproxyauth.Auth{ID: "auth-a"}
	authB := &cliproxyauth.Auth{ID: "auth-b"}
	//goland:noinspection SpellCheckingInspection
	wsURLA := "ws" + strings.TrimPrefix(serverA.URL, "http")
	//goland:noinspection SpellCheckingInspection
	wsURLB := "ws" + strings.TrimPrefix(serverB.URL, "http")

	connA := ensureWebsocketTargetConn(t, ensureConn, authA, sess, authA.ID, wsURLA)
	connAReused := ensureWebsocketTargetConn(t, ensureConn, authA, sess, authA.ID, wsURLA)
	if connAReused != connA {
		t.Fatal("matching websocket target did not reuse the existing connection")
	}

	connURLB := ensureWebsocketTargetConn(t, ensureConn, authA, sess, authA.ID, wsURLB)
	if connURLB == connA {
		t.Fatal("websocket URL change reused the existing connection")
	}
	if got := <-closedA; got != authA.ID {
		t.Fatalf("closed server A auth = %q, want %q", got, authA.ID)
	}

	connAuthB := ensureWebsocketTargetConn(t, ensureConn, authB, sess, authB.ID, wsURLB)
	if connAuthB == connURLB {
		t.Fatal("websocket auth change reused the existing connection")
	}
	if got := <-closedB; got != authA.ID {
		t.Fatalf("first closed server B auth = %q, want %q", got, authA.ID)
	}

	sess.connMu.Lock()
	gotAuthID := sess.authID
	gotURL := sess.wsURL
	sess.connMu.Unlock()
	if gotAuthID != authB.ID || gotURL != wsURLB {
		t.Fatalf("session target = {%q %q}, want {%q %q}", gotAuthID, gotURL, authB.ID, wsURLB)
	}

	select {
	case errDisconnect := <-disconnectCh:
		t.Fatalf("controlled websocket target switch notified downstream: %v", errDisconnect)
	default:
	}
}

func ensureWebsocketTargetConn(
	t *testing.T,
	ensureConn func(
		context.Context,
		*cliproxyauth.Auth,
		*codexWebsocketSession,
		string,
		string,
		http.Header,
	) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error),
	auth *cliproxyauth.Auth,
	sess *codexWebsocketSession,
	authID string,
	wsURL string,
) *websocket.Conn {
	t.Helper()
	headers := http.Header{"X-Test-Auth": []string{authID}}
	conn, _, resp, errEnsure := ensureConn(context.Background(), auth, sess, authID, wsURL, headers)
	if resp != nil && resp.Body != nil {
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				t.Errorf("close handshake response body: %v", errClose)
			}
		}()
	}
	if errEnsure != nil {
		t.Fatalf("ensure websocket connection: %v", errEnsure)
	}
	if conn == nil {
		t.Fatal("ensure websocket connection returned nil")
	}
	return conn
}

func newWebsocketTargetServer(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	closed := make(chan string, 4)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				authID := r.Header.Get("X-Test-Auth")
				conn, errUpgrade := upgrader.Upgrade(w, r, nil)
				if errUpgrade != nil {
					t.Errorf("upgrade websocket: %v", errUpgrade)
					return
				}
				defer func() {
					if errClose := conn.Close(); errClose != nil {
						t.Errorf("close upstream websocket: %v", errClose)
					}
					closed <- authID
				}()
				for {
					if _, _, errRead := conn.ReadMessage(); errRead != nil {
						return
					}
				}
			},
		),
	)
	return server, closed
}

type registryDrainWebsocketLifecycle struct {
	scope *executionregistry.Scope
	ends  atomic.Int32
}

func (l *registryDrainWebsocketLifecycle) Bind(closeFn func() error) error {
	return l.scope.Bind(closeFn)
}

func (l *registryDrainWebsocketLifecycle) End(string) {
	l.ends.Add(1)
	l.scope.End("websocket_closed")
}

func (l *registryDrainWebsocketLifecycle) Retain() {}

type websocketHomeDispatcher struct {
	provider string
}

func (d websocketHomeDispatcher) HeartbeatOK() bool { return true }

func (d websocketHomeDispatcher) RPopAuth(context.Context, string, string, http.Header, int) ([]byte, error) {
	return json.Marshal(
		map[string]any{
			"auth": map[string]any{
				"id":       "home-websocket-auth",
				"provider": d.provider,
				"status":   "active",
				"attributes": map[string]string{
					"api_key": "home-key",
				},
			},
		},
	)
}

func (websocketHomeDispatcher) AbortAmbiguousDispatch() {}

type accountedWebsocketHomeDispatcher struct {
	provider string
	baseURL  string
	calls    atomic.Int32
	releases atomic.Int32
	before   atomic.Bool
}

func (*accountedWebsocketHomeDispatcher) HeartbeatOK() bool { return true }

func (d *accountedWebsocketHomeDispatcher) RPopAuth(
	_ context.Context,
	model string,
	_ string,
	_ http.Header,
	_ int,
) ([]byte, error) {
	call := d.calls.Add(1)
	if call > 1 && d.releases.Load() != call-1 {
		d.before.Store(false)
	} else if call > 1 {
		d.before.Store(true)
	}
	upstreamModel := "model-a"
	if strings.Contains(strings.ToLower(model), "(custom)") {
		upstreamModel = "model-a(custom)"
	}
	return json.Marshal(
		map[string]any{
			"model":      upstreamModel,
			"auth_index": "accounted-websocket-auth",
			"auth": map[string]any{
				"id":       "accounted-websocket-auth",
				"provider": d.provider,
				"status":   "active",
				"attributes": map[string]string{
					"api_key":    "test-key",
					"base_url":   d.baseURL,
					"websockets": "true",
				},
			},
			"concurrency": map[string]any{
				"accounted":     true,
				"credential_id": "accounted-websocket-auth",
				"model":         upstreamModel,
			},
		},
	)
}

func (*accountedWebsocketHomeDispatcher) AbortAmbiguousDispatch() {}

func TestAuditAccountedCodexXAIReconnectReuseAndTargetChange(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		newExecutor func() cliproxyauth.ProviderExecutor
	}{
		{
			name:     "Codex",
			provider: "codex",
			newExecutor: func() cliproxyauth.ProviderExecutor {
				executor := newCodexWebsocketsExecutor(
					&config.Config{DisableImageGeneration: config.DisableImageGenerationAll},
				)
				executor.sessions = make(map[string]*codexWebsocketSession)
				return executor
			},
		},
		{
			name:     "xAI",
			provider: "xai",
			newExecutor: func() cliproxyauth.ProviderExecutor {
				executor := newXAIWebsocketsExecutor(&config.Config{})
				executor.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				executor.idStore = &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
				return executor
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				var connections atomic.Int32
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							conn, errUpgrade := upgrader.Upgrade(w, r, nil)
							if errUpgrade != nil {
								t.Errorf("upgrade websocket: %v", errUpgrade)
								return
							}
							connections.Add(1)
							defer func() { _ = conn.Close() }()
							for {
								if _, _, errRead := conn.ReadMessage(); errRead != nil {
									return
								}
								completed := []byte(
									(`{"type":"response.completed","response":{"id":"response-1","output":[]` +
										`,"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`),
								)
								if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
									return
								}
							}
						},
					),
				)
				defer server.Close()

				registry := executionregistry.New()
				dispatcher := &accountedWebsocketHomeDispatcher{provider: test.provider, baseURL: server.URL}
				var releaseGroups []executionregistry.ReleaseGroup
				registry.SetReleaseSink(
					func(group executionregistry.ReleaseGroup, _ int64) {
						dispatcher.releases.Add(1)
						releaseGroups = append(releaseGroups, group)
					},
				)
				manager := cliproxyauth.NewManager(nil, nil, nil)
				manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
				manager.PublishHomeDispatch(dispatcher, registry, 1)
				manager.RegisterExecutor(test.newExecutor())
				t.Cleanup(func() { manager.CloseExecutionSession("accounted-websocket-session") })

				ctx, cancel := context.WithTimeout(
					cliproxyexecutor.WithDownstreamWebsocket(t.Context()),
					5*time.Second,
				)
				defer cancel()
				opts := cliproxyexecutor.Options{
					Stream:         true,
					SourceFormat:   sdktranslator.FormatOpenAIResponse,
					ResponseFormat: sdktranslator.FormatOpenAIResponse,
					Metadata: map[string]any{
						cliproxyexecutor.ExecutionSessionMetadataKey: "accounted-websocket-session",
						cliproxyexecutor.PinnedAuthMetadataKey:       "accounted-websocket-auth",
					},
				}
				execute := func(model string) {
					t.Helper()
					result, errExecute := manager.ExecuteStream(
						ctx,
						[]string{test.provider},
						cliproxyexecutor.Request{
							Model:   model,
							Payload: []byte(`{"model":"model-a","input":[]}`),
						},
						opts,
					)
					if errExecute != nil {
						t.Fatalf("ExecuteStream(%q) error = %v", model, errExecute)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatalf("ExecuteStream(%q) chunk error = %v", model, chunk.Err)
						}
						if chunk.Acknowledge != nil {
							t.Fatal("ordinary stream consumer unexpectedly required delivery acknowledgement")
						}
					}
				}

				execute(" MODEL-A(HIGH) ")
				execute("model-a")
				if got := dispatcher.calls.Load(); got != 1 {
					t.Fatalf("Home RPOP calls = %d, want 1 for canonical retained reuse", got)
				}
				manager.CloseExecutionSession("accounted-websocket-session")
				execute("model-a")
				execute("model-a(custom)")
				if got := dispatcher.calls.Load(); got != 3 {
					t.Fatalf("Home RPOP calls = %d, want 3 after reconnect and target change", got)
				}
				if !dispatcher.before.Load() {
					t.Fatal("previous accounted selection was not released before redispatch")
				}
				manager.CloseExecutionSession("accounted-websocket-session")
				wantGroups := []executionregistry.ReleaseGroup{
					{CredentialID: "accounted-websocket-auth", Model: "model-a"},
					{CredentialID: "accounted-websocket-auth", Model: "model-a"},
					{CredentialID: "accounted-websocket-auth", Model: "model-a(custom)"},
				}
				if !reflect.DeepEqual(releaseGroups, wantGroups) {
					t.Fatalf("release groups = %#v, want %#v", releaseGroups, wantGroups)
				}
				if got := connections.Load(); got != 3 {
					t.Fatalf("upstream websocket connections = %d, want 3", got)
				}
			},
		)
	}
}

func TestHomeSelectionRegistryDrainClosesRealWebsocketSessions(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		newExecutor func() (
			cliproxyauth.ProviderExecutor,
			func(string) *codexWebsocketSession,
			func(
				context.Context,
				*cliproxyauth.Auth,
				*codexWebsocketSession,
				string,
				string,
				http.Header,
			) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error),
		)
	}{
		{
			name:     "Codex",
			provider: "codex",
			newExecutor: func() (
				cliproxyauth.ProviderExecutor,
				func(string) *codexWebsocketSession,
				func(
					context.Context,
					*cliproxyauth.Auth,
					*codexWebsocketSession,
					string,
					string,
					http.Header,
				) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error),
			) {
				executor := newCodexWebsocketsExecutor(&config.Config{})
				executor.sessions = make(map[string]*codexWebsocketSession)
				return executor, executor.getOrCreateSession, executor.ensureUpstreamConn
			},
		},
		{
			name:     "xAI",
			provider: "xai",
			newExecutor: func() (
				cliproxyauth.ProviderExecutor,
				func(string) *codexWebsocketSession,
				func(
					context.Context,
					*cliproxyauth.Auth,
					*codexWebsocketSession,
					string,
					string,
					http.Header,
				) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error),
			) {
				executor := newXAIWebsocketsExecutor(&config.Config{})
				executor.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				return executor, executor.store.getOrCreate, executor.ensureUpstreamConn
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				server, closed := newWebsocketTargetServer(t)
				defer server.Close()

				executor, getSession, ensureConn := test.newExecutor()
				registry := executionregistry.New()
				manager := cliproxyauth.NewManager(nil, nil, nil)
				manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
				manager.PublishHomeDispatch(websocketHomeDispatcher{provider: test.provider}, registry, 1)
				manager.RegisterExecutor(executor)
				selection, errSelect := manager.SelectHomeAuthByKind(
					context.Background(), test.provider, "model-a", cliproxyauth.AuthKindAPIKey,
					cliproxyexecutor.Options{},
				)
				if errSelect != nil {
					t.Fatalf("SelectHomeAuthByKind() error = %v", errSelect)
				}
				auth := selection.CloneAuth()
				sess := getSession("real-home-drain")
				wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
				conn := ensureWebsocketTargetConn(t, ensureConn, auth, sess, auth.ID, wsURL)
				if errBind := sess.bindExecutionLifecycle(
					context.Background(), cliproxyexecutor.Options{ExecutionLifecycle: selection}, conn,
					sess.connCloser,
				); errBind != nil {
					t.Fatalf("bind execution lifecycle: %v", errBind)
				}

				drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
				defer cancelDrain()
				if errDrain := registry.Drain(drainCtx); errDrain != nil {
					t.Fatalf("Drain() error = %v", errDrain)
				}
				if selection.Active() {
					t.Fatal("registry drain did not end the Home dispatch selection")
				}
				if got := <-closed; got != auth.ID {
					t.Fatalf("closed server auth = %q, want %q", got, auth.ID)
				}
			},
		)
	}
}

type codex426RetryDispatcher struct {
	calls                    atomic.Int32
	baseURLs                 []string
	websockets               []bool
	releases                 atomic.Int32
	releasedBeforeSecondRPop atomic.Bool
}

func (d *codex426RetryDispatcher) HeartbeatOK() bool { return true }

func (d *codex426RetryDispatcher) RPopAuth(_ context.Context, model string, _ string, _ http.Header, _ int) (
	[]byte,
	error,
) {
	call := int(d.calls.Add(1))
	if call > len(d.baseURLs) {
		return nil, fmt.Errorf("unexpected Home dispatch %d", call)
	}
	if call == 2 {
		d.releasedBeforeSecondRPop.Store(d.releases.Load() == 1)
	}
	credentialID := "codex-home-" + strconv.Itoa(call)
	attributes := map[string]string{
		"api_key":  "home-key",
		"base_url": d.baseURLs[call-1],
	}
	if call <= len(d.websockets) && d.websockets[call-1] {
		attributes["websockets"] = "true"
	}
	return json.Marshal(
		map[string]any{
			"model":      model,
			"auth_index": credentialID,
			"auth": map[string]any{
				"id":         credentialID,
				"provider":   "codex",
				"status":     "active",
				"attributes": attributes,
			},
			"concurrency": map[string]any{
				"accounted":     true,
				"credential_id": credentialID,
				"model":         model,
			},
		},
	)
}

func (*codex426RetryDispatcher) AbortAmbiguousDispatch() {}

func TestAuditHomeCodex426WebsocketToHTTPFreshSelection(t *testing.T) {
	upgradeRequired := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
			},
		),
	)
	defer upgradeRequired.Close()

	var httpFallbackCalls atomic.Int32
	httpFallback := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/responses" {
					http.Error(w, "unexpected fallback request", http.StatusBadRequest)
					return
				}
				httpFallbackCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(
					[]byte(("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"response" +
						"-1\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0," +
						"\"total_tokens\":0}}}\n\n")),
				)
			},
		),
	)
	defer httpFallback.Close()

	executor := NewCodexAutoExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
	executor.wsExec.sessions = make(map[string]*codexWebsocketSession)
	dispatcher := &codex426RetryDispatcher{
		baseURLs:   []string{upgradeRequired.URL, httpFallback.URL},
		websockets: []bool{true, false},
	}
	registry := executionregistry.New()
	var releaseGroups []executionregistry.ReleaseGroup
	var releaseGroupsMu sync.Mutex
	registry.SetReleaseSink(
		func(group executionregistry.ReleaseGroup, sequence int64) *executionregistry.ReleaseTicket {
			dispatcher.releases.Add(1)
			releaseGroupsMu.Lock()
			releaseGroups = append(releaseGroups, group)
			releaseGroupsMu.Unlock()
			done := make(chan struct{})
			close(done)
			return executionregistry.NewReleaseTicket(group, sequence, done)
		},
	)
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	manager.PublishHomeDispatch(dispatcher, registry, 1)
	manager.RegisterExecutor(executor)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	request := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{
		Stream: true, SourceFormat: sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "home-426"},
	}
	_, firstError := manager.ExecuteStream(ctx, []string{"codex"}, request, opts)
	if statusErrCode(firstError) != http.StatusUpgradeRequired || dispatcher.calls.Load() != 1 ||
		httpFallbackCalls.Load() != 0 {
		t.Fatalf(
			"upgrade refusal was automatically replayed: err=%v selections=%d HTTP calls=%d", firstError,
			dispatcher.calls.Load(), httpFallbackCalls.Load(),
		)
	}
	result, errExecute := manager.ExecuteStream(ctx, []string{"codex"}, request, opts)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	if !dispatcher.releasedBeforeSecondRPop.Load() {
		t.Fatal("first accounted selection was not released before the 426 retry RPOP")
	}
	if got := dispatcher.releases.Load(); got != 1 {
		t.Fatalf("accounted releases before response completion = %d, want 1", got)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}
	if got := dispatcher.calls.Load(); got != 2 {
		t.Fatalf("Home RPOP calls = %d, want 2 after 426", got)
	}
	if got := httpFallbackCalls.Load(); got != 1 {
		t.Fatalf("HTTP fallback calls = %d, want 1 on the fresh Home selection", got)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for dispatcher.releases.Load() != 2 {
		select {
		case <-deadline.C:
			t.Fatalf("accounted releases after response completion = %d, want 2", dispatcher.releases.Load())
		case <-time.After(time.Millisecond):
		}
	}
	releaseGroupsMu.Lock()
	gotReleaseGroups := append([]executionregistry.ReleaseGroup(nil), releaseGroups...)
	releaseGroupsMu.Unlock()
	wantReleaseGroups := []executionregistry.ReleaseGroup{
		{CredentialID: "codex-home-1", Model: "gpt-5-codex"},
		{CredentialID: "codex-home-2", Model: "gpt-5-codex"},
	}
	if !reflect.DeepEqual(gotReleaseGroups, wantReleaseGroups) {
		t.Fatalf("accounted release groups = %#v, want %#v", gotReleaseGroups, wantReleaseGroups)
	}
}

func TestWebsocketRegistryDrainClosesAndEndsRetainedSession(t *testing.T) {
	tests := []struct {
		name       string
		getSession func(string) *codexWebsocketSession
		ensureConn func(
			context.Context,
			*cliproxyauth.Auth,
			*codexWebsocketSession,
			string,
			string,
			http.Header,
		) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error)
	}{
		{
			name: "Codex",
			getSession: func(sessionID string) *codexWebsocketSession {
				executor := newCodexWebsocketsExecutor(&config.Config{})
				executor.sessions = make(map[string]*codexWebsocketSession)
				return executor.getOrCreateSession(sessionID)
			},
			ensureConn: func(
				ctx context.Context,
				auth *cliproxyauth.Auth,
				sess *codexWebsocketSession,
				authID, wsURL string,
				headers http.Header,
			) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error) {
				executor := newCodexWebsocketsExecutor(&config.Config{})
				return executor.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, headers)
			},
		},
		{
			name: "xAI shared session",
			getSession: func(sessionID string) *codexWebsocketSession {
				executor := newXAIWebsocketsExecutor(&config.Config{})
				executor.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
				return executor.store.getOrCreate(sessionID)
			},
			ensureConn: func(
				ctx context.Context,
				auth *cliproxyauth.Auth,
				sess *codexWebsocketSession,
				authID, wsURL string,
				headers http.Header,
			) (*websocket.Conn, *websocketConnectionCloser, *http.Response, error) {
				executor := newXAIWebsocketsExecutor(&config.Config{})
				return executor.ensureUpstreamConn(ctx, auth, sess, authID, wsURL, headers)
			},
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				server, closed := newWebsocketTargetServer(t)
				defer server.Close()

				registry := executionregistry.New()
				pending, errBegin := registry.BeginDispatch()
				if errBegin != nil {
					t.Fatalf("BeginDispatch() error = %v", errBegin)
				}
				scope, errInstall := registry.Install(pending, executionregistry.ScopeSpec{Kind: "websocket"})
				if errInstall != nil {
					t.Fatalf("Install() error = %v", errInstall)
				}
				lifecycle := &registryDrainWebsocketLifecycle{scope: scope}
				auth := &cliproxyauth.Auth{ID: "auth-a"}
				wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
				sess := test.getSession("drain-retained-session")
				conn := ensureWebsocketTargetConn(t, test.ensureConn, auth, sess, auth.ID, wsURL)
				if errBind := sess.bindExecutionLifecycle(
					context.Background(), cliproxyexecutor.Options{ExecutionLifecycle: lifecycle}, conn,
					sess.connCloser,
				); errBind != nil {
					t.Fatalf("bind execution lifecycle: %v", errBind)
				}

				drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
				defer cancelDrain()
				if errDrain := registry.Drain(drainCtx); errDrain != nil {
					t.Fatalf("Drain() error = %v", errDrain)
				}
				ended := time.After(time.Second)
				for lifecycle.ends.Load() == 0 {
					select {
					case <-ended:
						t.Fatal("registry drain did not end the connection lifecycle")
					case <-time.After(time.Millisecond):
					}
				}
				if got := lifecycle.ends.Load(); got != 1 {
					t.Fatalf("lifecycle End calls = %d, want 1", got)
				}
				if got := <-closed; got != auth.ID {
					t.Fatalf("closed server auth = %q, want %q", got, auth.ID)
				}
			},
		)
	}
}
