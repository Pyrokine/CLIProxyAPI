package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

type chatWaitingExecutor struct {
	mediaRegressionExecutor
	phase string
	gate  <-chan struct{}
}

func (e *chatWaitingExecutor) wait(ctx context.Context) error {
	select {
	case <-e.gate:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (e *chatWaitingExecutor) ExecuteStream(
	ctx context.Context,
	_ *coreauth.Auth,
	_ coreexecutor.Request,
	_ coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	if e.phase == "setup" {
		if err := e.wait(ctx); err != nil {
			return nil, err
		}
	}
	if e.err != nil {
		return nil, e.err
	}
	chunks := make(chan coreexecutor.StreamChunk, 2)
	go func() {
		defer close(chunks)
		if e.phase == "first payload" && e.wait(ctx) != nil {
			return
		}
		chunks <- coreexecutor.StreamChunk{
			Payload: []byte((`{"id":"fixture-stream","choices":[{"index":0,` +
				`"delta":{"content":"fixture complete"},` +
				`"finish_reason":` +
				`"stop"}]}`)),
		}
		chunks <- coreexecutor.StreamChunk{Terminal: true}
	}()
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *chatWaitingExecutor) Execute(
	ctx context.Context,
	auth *coreauth.Auth,
	request coreexecutor.Request,
	options coreexecutor.Options,
) (coreexecutor.Response, error) {
	if err := e.wait(ctx); err != nil {
		return coreexecutor.Response{}, err
	}
	return e.mediaRegressionExecutor.Execute(ctx, auth, request, options)
}

type chatNonStreamingWaitWriter struct {
	gin.ResponseWriter
	finalWrite   chan struct{}
	releaseFinal <-chan struct{}
	heartbeats   atomic.Int32
}

func (writer *chatNonStreamingWaitWriter) Write(payload []byte) (int, error) {
	if string(payload) == "\n" {
		writer.heartbeats.Add(1)
	} else {
		close(writer.finalWrite)
		<-writer.releaseFinal
	}
	return writer.ResponseWriter.Write(payload)
}

func TestChatAndCompletionsNonStreamingKeepAliveStopsBeforeFinalWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"chat", "completions"} {
		for _, scenario := range []string{"success", "upstream error", "client cancellation", "disabled", "immediate"} {
			t.Run(
				endpoint+"/"+scenario, func(t *testing.T) {
					synctest.Test(
						t, func(t *testing.T) {
							const model = "fixture-nonstream-wait"
							gate := make(chan struct{})
							executor := &chatWaitingExecutor{
								provider: "fixture-chat-provider",
								payload: []byte(
									(`{"id":"fixture-nonstream","choices":[{"index":0,"message":{"role":"` +
										`assistant","content":"fixture complete"},"finish_reason":"stop"}]}`),
								),
								gate: gate,
							}
							if scenario == "upstream error" {
								executor.err = &imageHandlerStatusError{
									statusCode: http.StatusBadRequest, err: errors.New(mediaPrivateError),
								}
							}
							manager := coreauth.NewManager(nil, nil, nil)
							manager.RegisterExecutor(executor)
							auth := &coreauth.Auth{
								ID: t.Name(), Provider: executor.Identifier(), Status: coreauth.StatusActive,
							}
							if _, err := manager.Register(t.Context(), auth); err != nil {
								t.Fatal(err)
							}
							registry.GetGlobalRegistry().RegisterClient(
								auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}},
							)
							t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
							seconds := 1
							if scenario == "disabled" {
								seconds = 0
							}
							handler := NewAPIHandler(
								handlers.NewBaseAPIHandlers(
									&sdkconfig.SDKConfig{NonStreamKeepAliveInterval: seconds}, manager,
								),
							)
							recorder := httptest.NewRecorder()
							c, _ := gin.CreateTestContext(recorder)
							requestCtx, cancel := context.WithCancel(t.Context())
							defer cancel()
							path := "/v1/chat/completions"
							body := `{"model":"` + model + `","messages":[{"role":"user","content":"fixture"}]}`
							if endpoint == "completions" {
								path, body = "/v1/completions", `{"model":"`+model+`","prompt":"fixture"}`
							}
							c.Request = httptest.NewRequest(
								http.MethodPost, path, strings.NewReader(body),
							).WithContext(requestCtx)
							c.Request.Header.Set("Content-Type", "application/json")
							releaseFinal := make(chan struct{})
							writer := &chatNonStreamingWaitWriter{
								ResponseWriter: c.Writer, finalWrite: make(chan struct{}), releaseFinal: releaseFinal,
							}
							c.Writer = writer
							if scenario == "immediate" {
								close(gate)
							}
							done := make(chan struct{})
							go func() {
								defer close(done)
								if endpoint == "completions" {
									handler.Completions(c)
								} else {
									handler.ChatCompletions(c)
								}
							}()
							synctest.Wait()
							if scenario != "immediate" {
								time.Sleep(2 * time.Second)
								synctest.Wait()
								want := int32(2)
								if seconds == 0 {
									want = 0
								}
								if got := writer.heartbeats.Load(); got != want {
									t.Errorf("waiting heartbeats=%d, want %d", got, want)
								}
								if scenario == "client cancellation" {
									cancel()
								} else {
									close(gate)
								}
								synctest.Wait()
							}
							select {
							case <-writer.finalWrite:
							default:
								t.Error("handler did not reach its final response write")
							}
							beforeFinal := writer.heartbeats.Load()
							time.Sleep(3 * time.Second)
							synctest.Wait()
							if writer.heartbeats.Load() != beforeFinal {
								t.Error(
									"keepalive continued after execution returned, before final response delivery",
								)
							}
							close(releaseFinal)
							synctest.Wait()
							<-done
							response := recorder.Body.String()
							if scenario == "upstream error" {
								assertMediaErrorPrivacy(t, response)
							} else if scenario == "client cancellation" {
								if strings.Contains(response, "fixture complete") {
									t.Error("canceled execution returned a successful completion")
								}
							} else if !strings.Contains(response, "fixture complete") {
								t.Errorf("missing final completion: %s", response)
							}
							time.Sleep(3 * time.Second)
							synctest.Wait()
							if recorder.Body.String() != response || writer.heartbeats.Load() != beforeFinal {
								t.Error("keepalive wrote after the handler returned")
							}
						},
					)
				},
			)
		}
	}
}

func TestChatAndCompletionsWaitsUseConfiguredHeartbeat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"chat", "completions"} {
		for _, scenario := range []string{
			"setup", "first payload", "upstream error", "heartbeat flush failure", "disabled", "client cancellation",
		} {
			t.Run(
				endpoint+"/"+scenario, func(t *testing.T) {
					const model = "fixture-chat-wait"
					gate := make(chan struct{})
					executor := &chatWaitingExecutor{
						provider: "fixture-chat-provider",
						phase:    scenario,
						gate:     gate,
					}
					if scenario != "first payload" {
						executor.phase = "setup"
					}
					if scenario == "upstream error" {
						executor.err = &imageHandlerStatusError{
							statusCode: http.StatusBadRequest, err: errors.New(mediaPrivateError),
						}
					}
					manager := coreauth.NewManager(nil, nil, nil)
					manager.RegisterExecutor(executor)
					auth := &coreauth.Auth{
						ID:       t.Name(),
						Provider: executor.Identifier(),
						Status:   coreauth.StatusActive,
					}
					if _, err := manager.Register(t.Context(), auth); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(
						auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}},
					)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					seconds := 1
					if scenario == "disabled" {
						seconds = 0
					}
					handler := NewAPIHandler(
						handlers.NewBaseAPIHandlers(
							&sdkconfig.SDKConfig{
								Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: seconds},
							},
							manager,
						),
					)
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					outcome := internallogging.NewRequestOutcomeState()
					requestCtx, cancel := context.WithCancel(
						internallogging.WithRequestOutcomeState(
							t.Context(), outcome,
						),
					)
					defer cancel()
					path := "/v1/chat/completions"
					body := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"fixture"}]}`
					if endpoint == "completions" {
						path = "/v1/completions"
						body = `{"model":"` + model + `","stream":true,"prompt":"fixture"}`
					}
					c.Request = httptest.NewRequest(
						http.MethodPost, path, strings.NewReader(body),
					).WithContext(requestCtx)
					c.Request.Header.Set("Content-Type", "application/json")
					writer := &imageHeartbeatWriter{ResponseWriter: c.Writer, heartbeat: make(chan struct{})}
					if scenario == "heartbeat flush failure" {
						writer.flushErr = errors.New("fixture chat heartbeat flush failure")
					}
					c.Writer = writer
					done := make(chan struct{})
					go func() {
						defer close(done)
						if endpoint == "completions" {
							handler.Completions(c)
						} else {
							handler.ChatCompletions(c)
						}
					}()
					heartbeat := false
					timer := time.NewTimer(1500 * time.Millisecond)
					select {
					case <-writer.heartbeat:
						heartbeat = true
					case <-timer.C:
					}
					timer.Stop()
					if scenario == "client cancellation" {
						cancel()
					} else {
						close(gate)
					}
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						cancel()
						<-done
						t.Fatal("stream handler did not finish after executor release or cancellation")
					}
					if heartbeat != (scenario != "disabled") {
						t.Errorf("heartbeat=%t during %s", heartbeat, scenario)
					}
					response := recorder.Body.String()
					switch scenario {
					case "heartbeat flush failure", "client cancellation":
						if strings.Contains(response, "fixture complete") ||
							strings.Contains(response, "[DONE]") {
							t.Errorf("failed stream published success: %s", response)
						}
						if scenario == "heartbeat flush failure" && !outcome.Snapshot().Failed {
							t.Fatal("heartbeat flush failure was recorded as success")
						}
					case "upstream error":
						if !outcome.Snapshot().Failed || strings.Count(
							response, `"error"`,
						) != 1 || strings.Contains(response, "[DONE]") || !strings.Contains(response, "data: ") {
							t.Errorf("post-heartbeat error = %+v, body=%s", outcome.Snapshot(), response)
						}
						assertMediaErrorPrivacy(t, response)
					default:
						if outcome.Snapshot().Failed || strings.Count(
							response, "[DONE]",
						) != 1 || !strings.Contains(response, "fixture complete") {
							t.Errorf("completion after wait = %+v, body=%s", outcome.Snapshot(), response)
						}
					}
				},
			)
		}
	}
}

func TestCompletionsTracksSourceChoicesBeforeFiltering(t *testing.T) {
	for _, finishHiddenChoice := range []bool{false, true} {
		name := "unfinished role-only choice"
		if finishHiddenChoice {
			name = "all observed choices finish"
		}
		t.Run(
			name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				outcome := internallogging.NewRequestOutcomeState()
				c.Request = httptest.NewRequest(
					http.MethodPost, "/v1/completions", nil,
				).WithContext(internallogging.WithRequestOutcomeState(t.Context(), outcome))
				data := make(chan coreexecutor.StreamChunk, 4)
				data <- coreexecutor.StreamChunk{
					Payload: []byte((`{"choices":[{"index":1,"delta":{"role":` +
						`"assistant"},"finish_reason":` +
						`null}]}`)),
				}
				data <- coreexecutor.StreamChunk{
					Payload: []byte((`{"choices":[{"index":0,"delta":{"content":` +
						`"visible"},"finish_reason":` +
						`"stop"}]}`)),
				}
				if finishHiddenChoice {
					data <- coreexecutor.StreamChunk{
						Payload: []byte((`{"choices":[{"index":1,"delta":{},` +
							`"finish_reason":` +
							`"stop"}]}`)),
					}
				}
				var terminalAck error
				data <- coreexecutor.StreamChunk{Terminal: true, Acknowledge: func(err error) { terminalAck = err }}
				close(data)
				var canceledWith error
				handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
				handler.handleStreamResult(
					c, c.Writer, func(err error) { canceledWith = err }, data, nil,
					convertChatCompletionsStreamChunkToCompletions,
				)
				body := recorder.Body.String()
				if finishHiddenChoice {
					if terminalAck != nil || canceledWith != nil || outcome.Snapshot().Failed || strings.Count(
						body, "[DONE]",
					) != 1 {
						t.Fatalf("complete choices = %q, cancel=%v, ack=%v", body, canceledWith, terminalAck)
					}
					return
				}
				if terminalAck == nil || canceledWith == nil || !strings.Contains(
					canceledWith.Error(), "choice 1",
				) || !outcome.Snapshot().Failed || strings.Contains(body, "[DONE]") || !strings.Contains(
					body, `"error"`,
				) {
					t.Fatalf(
						"unfinished filtered choice = %q, cancel=%v, ack=%v, outcome=%+v", body, canceledWith,
						terminalAck, outcome.Snapshot(),
					)
				}
			},
		)
	}
}

func TestChatAndCompletionsTerminalErrorsPreservePrivacyAndCause(t *testing.T) {
	for _, endpoint := range []string{"chat", "completions"} {
		t.Run(
			endpoint, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/completions", nil)
				cause := errors.New(mediaPrivateError)
				data := make(chan coreexecutor.StreamChunk, 2)
				data <- coreexecutor.StreamChunk{
					Payload: []byte((`{"choices":[{"index":0,"delta":{"content":` +
						`"partial"}}]}`)),
				}
				var acknowledged bool
				data <- coreexecutor.StreamChunk{
					Err: cause, Acknowledge: func(err error) {
						acknowledged = true
						if err != nil {
							t.Errorf("error frame delivery failed: %v", err)
						}
					},
				}
				close(data)
				var canceledWith error
				var transform func([]byte) []byte
				if endpoint == "completions" {
					transform = convertChatCompletionsStreamChunkToCompletions
				}
				handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
				handler.handleStreamResult(
					c,
					c.Writer,
					func(err error) { canceledWith = err },
					data,
					nil,
					transform,
				)
				body := recorder.Body.String()
				assertMediaErrorPrivacy(t, body)
				if !acknowledged || !errors.Is(canceledWith, cause) || strings.Count(
					body, `"error"`,
				) != 1 || strings.Contains(body, "[DONE]") {
					t.Fatalf(
						"terminal error acknowledgment=%t, cancel=%v, body=%s",
						acknowledged,
						canceledWith,
						body,
					)
				}
			},
		)
	}
}
