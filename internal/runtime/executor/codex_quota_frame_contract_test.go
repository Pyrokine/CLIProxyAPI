package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
)

type quotaFrameScopeExecutor struct {
	*codexWebsocketsExecutor
	frameProvider string
}

func (e *quotaFrameScopeExecutor) Execute(
	ctx context.Context, auth *coreauth.Auth, request core.Request, opts core.Options,
) (core.Response, error) {
	coreauth.RecordUpstreamResponseHeaders(
		ctx, http.Header{
			"X-Codex-Primary-Used-Percent": {"11"}, "X-Codex-Secondary-Used-Percent": {"17"},
		},
	)
	helps.EmitWebSocketResponseEvent(
		ctx, opts, auth, e.frameProvider, request.Model,
		[]byte((`{"type":"error","headers":{"X-Ratelimit-Remaining-Requests":"0","Retry-After":"60","X-Codex-` +
			`Primary-Used-Percent":"75"}}`)),
	)
	return core.Response{Payload: []byte(`{"output":[]}`)}, nil
}

func TestGenericWebsocketEntryPointDoesNotObserveCodexQuota(t *testing.T) {
	for _, provider := range []string{"codex", " CODEX ", "xai", "claude", ""} {
		t.Run(
			provider, func(t *testing.T) {
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
				manager.RegisterExecutor(
					&quotaFrameScopeExecutor{
						codexWebsocketsExecutor: newCodexWebsocketsExecutor(
							&config.Config{},
						), frameProvider: provider,
					},
				)
				credential, err := manager.Register(
					coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{
						ID: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(
					credential.ID, "codex", []*registry.ModelInfo{{ID: "quota-model"}},
				)
				manager.RefreshSchedulerEntry(credential.ID)
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(credential.ID) })
				_, err = manager.Execute(
					context.Background(), []string{"codex"}, core.Request{Model: "quota-model"}, core.Options{},
				)
				if err != nil {
					t.Fatal(err)
				}
				current, _ := manager.GetByID(credential.ID)
				want := "11"
				if provider == "codex" || provider == " CODEX " {
					want = "75"
					if current.Quota.Signals["Retry-After"] != "60" ||
						current.Quota.Signals["X-Ratelimit-Remaining-Requests"] != "0" {
						t.Fatal(current.Quota)
					}
				} else if current.Quota.Signals["Retry-After"] != "" ||
					current.Quota.Signals["X-Ratelimit-Remaining-Requests"] != "" {
					t.Fatal(current.Quota)
				}
				if current.Quota.Signals["X-Codex-Primary-Used-Percent"] != want ||
					current.Quota.Signals["X-Codex-Secondary-Used-Percent"] != "17" {
					t.Fatalf("provider=%q quota=%+v", provider, current.Quota)
				}
			},
		)
	}
}

func TestCodexQuotaFramesReachCurrentManagerAttempt(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, observer := range []bool{false, true} {
			for _, eventType := range []string{"codex.rate_limits", "rate_limits.updated", "error"} {
				t.Run(
					fmt.Sprintf("stream=%t/observer=%t/%s", stream, observer, eventType), func(t *testing.T) {
						frame := fmt.Sprintf(
							`{"type":%q,"rate_limits":{"primary":{"used_percent":75,`+
								`"window_minutes":300,"reset_after_seconds":123}},"plan_type":"synthetic-plan",`+
								`"credits":{"has_credits":false,"unlimited":false,"balance":"0"}}`, eventType,
						)
						frames := []string{frame, runtimeCodexCompleted}
						if eventType == "error" {
							frames = []string{
								`{"type":"error","status":429,"error":{"type":"rate_limit_error",` +
									(`"message":"synthetic quota rejection"},"headers":` +
										`{"X-Codex-Primary-Used-Percent":"75",`) +
									(`"X-Codex-Primary-Window-Minutes":"300","X-Codex-Credits-Balance":"0",` +
										`"Retry-After":"1",`) +
									`"Authorization":"synthetic-not-a-credential"}}`,
							}
						}
						var requests atomic.Int32
						server := httptest.NewServer(
							http.HandlerFunc(
								func(w http.ResponseWriter, request *http.Request) {
									peer, err := (&websocket.Upgrader{}).Upgrade(
										w, request, http.Header{
											"X-Codex-Secondary-Used-Percent": {"17"},
											"X-Request-Id":                   {"synthetic-handshake"},
										},
									)
									if err != nil {
										t.Error(err)
										return
									}
									defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
									if _, _, err := peer.ReadMessage(); err != nil {
										t.Error(err)
										return
									}
									requests.Add(1)
									for _, payload := range frames {
										if err := peer.WriteMessage(
											websocket.TextMessage, []byte(payload),
										); err != nil {
											t.Error(err)
											return
										}
									}
								},
							),
						)
						defer server.Close()
						ctx, finishUsage := xaiOutcomeContext(t)
						ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
						defer cancel()
						cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
						manager := coreauth.NewManager(nil, nil, nil)
						manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
						manager.SetConfig(cfg)
						manager.SetRetryConfig(0, 0, 1)
						manager.RegisterExecutor(newCodexWebsocketsExecutor(cfg))
						credential := runtimeCodexAuth(server.URL)
						credential.ID = t.Name()
						previousObservation := time.Unix(10, 0)
						credential.Quota = coreauth.QuotaState{
							ObservedAt: previousObservation,
							Signals:    map[string]string{"X-Codex-Primary-Used-Percent": "11"},
						}
						registered, err := manager.Register(coreauth.WithSkipPersist(ctx), credential)
						if err != nil {
							t.Fatal(err)
						}
						store := coreauth.NewFileCooldownStateStore(filepath.Join(t.TempDir(), "cds"))
						manager.SetCooldownStateStore(store)
						registry.GetGlobalRegistry().RegisterClient(
							registered.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.4"}},
						)
						manager.RefreshSchedulerEntry(registered.ID)
						t.Cleanup(
							func() {
								registry.GetGlobalRegistry().UnregisterClient(registered.ID)
								_ = manager.StopDeferredAuthRecreates(context.Background())
							},
						)
						var observed atomic.Int32
						opts := core.Options{
							SourceFormat: translator.FormatCodex, ResponseFormat: translator.FormatCodex,
							Stream: stream,
						}
						if observer {
							opts.WebSocketResponseObserver = func(
								_ context.Context,
								event core.WebSocketResponseEvent,
							) {
								observed.Add(1)
								if len(event.Payload) > 0 {
									event.Payload[0] = 'x'
								}
							}
						}
						request := core.Request{
							Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"synthetic"}`),
						}
						if stream {
							var result *core.StreamResult
							result, err = manager.ExecuteStream(ctx, []string{"codex"}, request, opts)
							if err == nil {
								for chunk := range result.Chunks {
									if chunk.Err != nil {
										err = chunk.Err
									}
								}
							}
						} else {
							_, err = manager.Execute(ctx, []string{"codex"}, request, opts)
						}
						if status, hasStatus := errors.AsType[interface {
							error
							StatusCode() int
						}](err); (eventType ==
							"error" &&
							(!hasStatus ||
								status.StatusCode() !=
									http.StatusTooManyRequests)) ||
							(eventType != "error" && err != nil) || requests.Load() != 1 {
							t.Fatalf(
								"execution error=%v requests=%d; expected one real local WebSocket attempt",
								err,
								requests.Load(),
							)
						}
						usageRecords := finishUsage()
						if len(usageRecords) != 1 {
							t.Fatalf("usage records=%d, want one actual plugin callback", len(usageRecords))
						}
						if record := usageRecords[0]; record.AuthID != registered.ID ||
							record.Failed != (eventType == "error") {
							t.Errorf("quota usage callback has wrong request identity or result: %+v", record)
						}
						usageHeaders := usageRecords[0].ResponseHeaders
						if usageHeaders.Get("X-Codex-Primary-Used-Percent") != "75" ||
							usageHeaders.Get("X-Codex-Primary-Window-Minutes") != "300" ||
							usageHeaders.Get("X-Codex-Credits-Balance") != "0" ||
							usageHeaders.Get("X-Codex-Secondary-Used-Percent") != "17" ||
							usageHeaders.Get(
								"X-Request-Id",
							) != "synthetic-handshake" || usageHeaders.Get("Authorization") != "" {
							t.Errorf("quota frame did not reach usage plugin headers: %#v", usageHeaders)
						}
						current, exists := manager.GetByID(registered.ID)
						if !exists || current.Quota.Signals["X-Codex-Primary-Used-Percent"] != "75" ||
							current.Quota.Signals["X-Codex-Primary-Window-Minutes"] != "300" ||
							current.Quota.Signals["X-Codex-Credits-Balance"] != "0" ||
							current.Quota.Signals["X-Codex-Secondary-Used-Percent"] != "17" ||
							current.Quota.Signals["Authorization"] !=
								"" ||
							current.Quota.Signals["X-Request-Id"] !=
								"" ||
							!current.Quota.ObservedAt.After(previousObservation) {
							t.Fatalf("manager did not consume current quota frame: auth=%+v", current)
						}
						wantObserved := int32(0)
						if observer {
							wantObserved = int32(len(frames))
						}
						if observed.Load() != wantObserved ||
							(eventType != "error" && (current.Quota.Exceeded || current.Unavailable)) {
							t.Fatalf(
								"observer=%d quota=%+v unavailable=%t", observed.Load(), current.Quota,
								current.Unavailable,
							)
						}
						if eventType == "error" {
							records, errLoad := store.Load(ctx)
							if errLoad != nil || len(records) == 0 {
								t.Fatalf(
									"quota rejection did not reach CDS: records=%+v error=%v",
									records,
									errLoad,
								)
							}
							for _, record := range records {
								if record.Quota.Signals["X-Codex-Primary-Used-Percent"] != "75" ||
									!record.Quota.ObservedAt.Equal(
										current.Quota.ObservedAt,
									) || record.Quota.Signals["Authorization"] != "" {
									t.Fatalf("CDS lost or exposed frame observation: %+v", record)
								}
							}
						}
					},
				)
			}
		}
	}
}
