package executor

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
)

func TestCodexOriginalUpstreamDisconnectSubscription(t *testing.T) {
	execute := NewCodexAutoExecutor(&config.Config{})
	testOriginalUpstreamDisconnectSubscription(
		t, execute, execute.wsExec.getOrCreateSession, execute.wsExec.invalidateUpstreamConn,
	)
}

func TestXAIOriginalUpstreamDisconnectSubscription(t *testing.T) {
	execute := NewXAIAutoExecutor(&config.Config{})
	testOriginalUpstreamDisconnectSubscription(
		t, execute, execute.wsExec.store.getOrCreate, execute.wsExec.invalidateUpstreamConn,
	)
}

func testOriginalUpstreamDisconnectSubscription(
	t *testing.T,
	execute interface{ CloseExecutionSession(string) },
	sessionFor func(string) *codexWebsocketSession,
	invalidate func(*codexWebsocketSession, *websocket.Conn, string, error),
) {
	t.Helper()
	subscriber, ok := any(execute).(interface{ UpstreamDisconnectChan(string) <-chan error })
	if !ok {
		t.Fatal("executor lost its published upstream disconnect subscription")
	}
	const session = "original-disconnect-session"
	defer execute.CloseExecutionSession(session)
	upstream := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				peer, err := (&websocket.Upgrader{}).Upgrade(w, request, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
				for {
					if _, _, err := peer.ReadMessage(); err != nil {
						return
					}
				}
			},
		),
	)
	defer upstream.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(upstream.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func(closeResource func() error) { _ = closeResource() }(conn.Close)
	disconnected := subscriber.UpstreamDisconnectChan(session)
	if disconnected == nil || subscriber.UpstreamDisconnectChan(session) != disconnected ||
		subscriber.UpstreamDisconnectChan(" ") != nil {
		t.Fatal("disconnect subscription changed its session or empty-input contract")
	}
	selected := sessionFor(session)
	selected.connMu.Lock()
	selected.conn, selected.readerConn = conn, conn
	selected.authID, selected.wsURL = "auth-1", "ws://example.test/responses"
	selected.connMu.Unlock()
	cause := errors.New("upstream gone")
	invalidate(selected, conn, "test_invalidate", cause)
	invalidate(selected, conn, "duplicate", cause)
	select {
	case got, open := <-disconnected:
		if !open || !errors.Is(got, cause) {
			t.Fatalf("disconnect cause=%v open=%t", got, open)
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect subscription did not receive the original cause")
	}
	if _, open := <-disconnected; open {
		t.Fatal("disconnect subscription remained open or repeated its event")
	}
}

func TestOriginalDisconnectNotificationPreservesRetryPermission(t *testing.T) {
	session := &codexWebsocketSession{upstreamDisconnectCh: make(chan error, 1)}
	failure := (&auth.Error{
		Message: "retry before processing", HTTPStatus: http.StatusServiceUnavailable,
	}).WithExecutionDisposition(
		core.ExecutionDisposition{
			Outcome: core.ExecutionOutcomeRejectedBeforeProcessing, RetryDirective: core.RetryDirectiveNextAuth,
		}, 0,
	)
	session.notifyUpstreamDisconnect(failure)
	select {
	case cause := <-session.upstreamDisconnectCh:
		t.Fatalf("safe retry emitted a terminal disconnect: %v", cause)
	default:
	}
	session.notifyUpstreamDisconnect(nil)
	if cause, open := <-session.upstreamDisconnectCh; !open || cause != nil {
		t.Fatalf("session close notification=%v open=%t", cause, open)
	}
	if _, open := <-session.upstreamDisconnectCh; open {
		t.Fatal("session close retained its disconnect channel")
	}
}

func TestCodexOriginalIdleDisconnectEndsHomeOwnerBeforeRedispatch(t *testing.T) {
	testOriginalIdleDisconnectEndsHomeOwner(t, "codex", "gpt-5.4")
}

func TestXAIOriginalIdleDisconnectEndsHomeOwnerBeforeRedispatch(t *testing.T) {
	testOriginalIdleDisconnectEndsHomeOwner(t, "xai", "grok-4")
}

func testOriginalIdleDisconnectEndsHomeOwner(t *testing.T, provider, model string) {
	t.Helper()
	peers := make(chan *websocket.Conn, 2)
	upstream := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				peer, err := (&websocket.Upgrader{}).Upgrade(w, request, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
				peers <- peer
				for {
					if _, _, err := peer.ReadMessage(); err != nil {
						return
					}
					if err := peer.WriteMessage(
						websocket.TextMessage, []byte(strings.ReplaceAll(runtimeCodexCompleted, "gpt-5.4", model)),
					); err != nil {
						return
					}
				}
			},
		),
	)
	defer upstream.Close()
	cfg := &config.Config{
		Home: config.HomeConfig{Enabled: true}, DisableImageGeneration: config.DisableImageGenerationAll,
	}
	manager := auth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	var execute auth.ProviderExecutor = NewCodexAutoExecutor(cfg)
	if provider == "xai" {
		execute = NewXAIAutoExecutor(cfg)
	}
	manager.RegisterExecutor(execute)
	const session = "original-idle-session"
	defer manager.CloseExecutionSession(session)
	credential := runtimeCodexAuth(upstream.URL)
	credential.Provider = provider
	envelope, err := json.Marshal(
		map[string]any{
			"model": model, "auth_index": credential.ID, "auth": credential,
			"concurrency": map[string]any{"accounted": true, "credential_id": credential.ID, "model": model},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &auditWebsocketHomeDispatcher{payload: envelope}
	registry := executionregistry.New()
	released := make(chan executionregistry.ReleaseGroup, 4)
	registry.SetReleaseSink(func(group executionregistry.ReleaseGroup, _ int64) { released <- group })
	manager.PublishHomeDispatch(dispatcher, registry, 1)
	ctx := core.WithDownstreamWebsocket(t.Context())
	request := core.Request{
		Model: model, Payload: []byte(`{"model":"` + model + `","input":[{"role":"user","content":"synthetic"}]}`),
	}
	opts := core.Options{
		SourceFormat: translator.FormatOpenAIResponse,
		Metadata:     map[string]any{core.ExecutionSessionMetadataKey: session},
	}
	for turn := range 2 {
		result, err := manager.ExecuteStream(ctx, []string{provider}, request, opts)
		if err != nil {
			t.Fatal(err)
		}
		for chunk := range result.Chunks {
			if chunk.Acknowledge != nil {
				chunk.Acknowledge(nil)
			}
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
		}
		if turn == 0 {
			first := <-peers
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case group := <-released:
				if group.CredentialID != credential.ID || group.Model != model {
					t.Fatalf("idle release=%+v", group)
				}
			case <-time.After(time.Second):
				t.Fatal("idle upstream disconnect retained its Home owner and concurrency")
			}
		}
	}
	if dispatcher.calls.Load() != 2 || ctx.Err() != nil {
		t.Fatalf("idle retry Home dispatches=%d downstream context=%v", dispatcher.calls.Load(), ctx.Err())
	}
}
