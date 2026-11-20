package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
)

type auditWebsocketHomeDispatcher struct {
	homeOpenAICompatDispatcher
	calls atomic.Int32
}

func (d *auditWebsocketHomeDispatcher) RPopAuth(
	ctx context.Context,
	model, session string,
	headers http.Header,
	count int,
) ([]byte, error) {
	d.calls.Add(1)
	return d.homeOpenAICompatDispatcher.RPopAuth(ctx, model, session, headers, count)
}

type auditWebsocketLifecycleExecutor struct {
	auth.ProviderExecutor
	owner      *auth.HomeDispatchSelection
	closeCount atomic.Int32
	closed     chan struct{}
}

func (e *auditWebsocketLifecycleExecutor) capture(opts core.Options) error {
	owner, ok := opts.ExecutionLifecycle.(*auth.HomeDispatchSelection)
	if !ok {
		return fmt.Errorf("real Manager Home lifecycle was not propagated")
	}
	if e.owner != owner {
		e.owner = owner
		return owner.Bind(
			func() error {
				if e.closeCount.Add(1) == 1 {
					close(e.closed)
				}
				return nil
			},
		)
	}
	return nil
}

func (e *auditWebsocketLifecycleExecutor) Execute(
	ctx context.Context,
	credential *auth.Auth,
	req core.Request,
	opts core.Options,
) (core.Response, error) {
	if err := e.capture(opts); err != nil {
		return core.Response{}, err
	}
	return e.ProviderExecutor.Execute(ctx, credential, req, opts)
}

func (e *auditWebsocketLifecycleExecutor) ExecuteStream(
	ctx context.Context,
	credential *auth.Auth,
	req core.Request,
	opts core.Options,
) (*core.StreamResult, error) {
	if err := e.capture(opts); err != nil {
		return nil, err
	}
	return e.ProviderExecutor.ExecuteStream(ctx, credential, req, opts)
}

func TestAuditHomeWebsocketLifecycleOwnsPersistentTransport(t *testing.T) {
	for _, mode := range []string{"codex-execute", "codex-stream", "xai-stream"} {
		for _, raw := range []bool{false, true} {
			for _, ending := range []string{
				"drain", "manager-close", "executor-close", "auth-close", "ack-error", "cancel",
			} {
				if mode == "codex-execute" && (ending == "ack-error" || ending == "cancel") {
					continue
				}
				t.Run(
					fmt.Sprintf("%s/raw=%t/%s", mode, raw, ending), func(t *testing.T) {
						provider, model := "codex", "gpt-5.4"
						if mode == "xai-stream" {
							provider, model = "xai", "grok-4.3"
						}
						var handshakes atomic.Int32
						peerClosed := make(chan struct{}, 4)
						server := httptest.NewServer(
							http.HandlerFunc(
								func(w http.ResponseWriter, r *http.Request) {
									peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
									if err != nil {
										t.Error(err)
										return
									}
									handshakes.Add(1)
									defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
									defer func() { peerClosed <- struct{}{} }()
									for turn := 1; ; turn++ {
										if _, _, err = peer.ReadMessage(); err != nil {
											return
										}
										completed := fmt.Sprintf(
											`{"type":"response.completed","response":{"id":"synthetic-%d","`+
												`status":"completed","model":%q,"output":[],"usage":{"`+
												`input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
											turn,
											model,
										)
										if err = peer.WriteMessage(
											websocket.TextMessage, []byte(completed),
										); err != nil {
											return
										}
									}
								},
							),
						)
						defer server.Close()
						cfg := &config.Config{
							Home:                   config.HomeConfig{Enabled: true},
							DisableImageGeneration: config.DisableImageGenerationAll,
						}
						var base auth.ProviderExecutor = newCodexWebsocketsExecutor(cfg)
						var session *codexWebsocketSession
						sessionID := t.Name()
						if provider == "xai" {
							base = newXAIWebsocketsExecutor(cfg)
							session = base.(*XAIWebsocketsExecutor).store.getOrCreate(sessionID)
						} else {
							session = base.(*codexWebsocketsExecutor).getOrCreateSession(sessionID)
						}
						closeSession := base.(interface{ CloseExecutionSession(string) }).CloseExecutionSession
						defer closeSession(sessionID)
						spy := &auditWebsocketLifecycleExecutor{
							ProviderExecutor: base,
							closed:           make(chan struct{}),
						}
						manager := auth.NewManager(nil, nil, nil)
						manager.SetConfig(cfg)
						manager.RegisterExecutor(spy)
						credential := &auth.Auth{
							ID: t.Name(), Provider: provider, Attributes: map[string]string{
								"api_key": "synthetic-key", "base_url": server.URL, "websockets": "true",
								"auth_kind": "apikey",
							},
						}
						payload, err := json.Marshal(
							map[string]any{
								"model": model, "model_info": map[string]any{"id": model, "context_length": 32768},
								"auth": credential,
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						dispatcher := &auditWebsocketHomeDispatcher{payload: payload}
						reg := executionregistry.New()
						manager.PublishHomeDispatch(dispatcher, reg, 1)
						ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
						defer cancel()
						ctx = core.WithStreamDeliveryAcknowledgement(ctx)
						if raw {
							ctx = core.WithDownstreamWebsocket(ctx)
						}
						failure := ending == "ack-error" || ending == "cancel"
						sawAcknowledgement := false
						turns := 1
						if raw && !failure {
							turns = 2
						}
						for turn := 0; turn < turns; turn++ {
							opts := core.Options{
								SourceFormat:   translator.FormatOpenAIResponse,
								ResponseFormat: translator.FormatOpenAIResponse, Stream: mode != "codex-execute",
								Metadata: map[string]any{core.ExecutionSessionMetadataKey: sessionID},
							}
							req := core.Request{
								Model: model, Payload: []byte(`{"model":"` + model + `","input":"synthetic"}`),
							}
							if mode == "codex-execute" {
								_, err = manager.Execute(ctx, []string{provider}, req, opts)
							} else {
								var result *core.StreamResult
								result, err = manager.ExecuteStream(ctx, []string{provider}, req, opts)
								if err == nil {
									for chunk := range result.Chunks {
										if chunk.Acknowledge != nil {
											if spy.closeCount.Load() != 0 {
												t.Fatal("Home owner ended before terminal delivery acknowledgement")
											}
											sawAcknowledgement = true
											switch ending {
											case "ack-error":
												chunk.Acknowledge(fmt.Errorf("synthetic downstream write failed"))
											case "cancel":
												cancel()
												chunk.Acknowledge(context.Canceled)
											default:
												chunk.Acknowledge(nil)
											}
										}
										if chunk.Err != nil {
											err = chunk.Err
										}
									}
								}
							}
							if err != nil && (ending != "cancel" || !errors.Is(err, context.Canceled)) {
								t.Fatal(err)
							}
						}
						if failure && !sawAcknowledgement {
							t.Fatal("terminal acknowledgement path was not exercised")
						}
						if failure {
							select {
							case <-spy.closed:
							case <-time.After(time.Second):
								t.Fatal("failed delivery retained its Home owner until Registry drain")
							}
						}
						if raw && !failure &&
							(dispatcher.calls.Load() != 1 || handshakes.Load() != 1 || spy.closeCount.Load() != 0) {
							t.Fatalf(
								"same session was not retained: dispatches=%d handshakes=%d closes=%d",
								dispatcher.calls.Load(), handshakes.Load(), spy.closeCount.Load(),
							)
						}
						session.connMu.Lock()
						conn := session.conn
						session.connMu.Unlock()
						switch ending {
						case "manager-close":
							manager.CloseExecutionSession(sessionID)
						case "executor-close":
							closeSession(sessionID)
						case "auth-close":
							base.(interface{ CloseExecutionSessionsForAuth(string) }).CloseExecutionSessionsForAuth(
								credential.ID,
							)
						}
						drainCtx, cancelDrain := context.WithTimeout(t.Context(), 3*time.Second)
						defer cancelDrain()
						if err = reg.Drain(drainCtx); err != nil {
							t.Fatalf("drain retained connection: %v", err)
						}
						if spy.closeCount.Load() != 1 {
							t.Fatalf("Home owner closed %d times", spy.closeCount.Load())
						}
						if conn != nil && conn.NetConn().SetWriteDeadline(time.Now()) == nil {
							t.Fatal("Registry drain left persistent upstream connection open")
						}
						select {
						case <-peerClosed:
						case <-drainCtx.Done():
							t.Fatal("synthetic peer did not observe transport closure")
						}
						manager.CloseExecutionSession(sessionID)
						closeSession(sessionID)
						if spy.closeCount.Load() != 1 {
							t.Fatal("repeated session closure released owner again")
						}
					},
				)
			}
		}
	}
}
