package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
)

type originalRuleStreamExecutor struct {
	reproOAuthExecutor
	failure error
	headers http.Header
}

func (e *originalRuleStreamExecutor) ExecuteStream(
	_ context.Context,
	_ *auth.Auth,
	_ core.Request,
	_ core.Options,
) (*core.StreamResult, error) {
	e.calls.Add(1)
	chunks := make(chan core.StreamChunk, 1)
	chunks <- core.StreamChunk{Err: e.failure}
	close(chunks)
	return &core.StreamResult{Headers: e.headers, Chunks: chunks}, nil
}

func originalStreamProtocolRouter(base *handlers.BaseAPIHandler, endpoint string) *gin.Engine {
	router := gin.New()
	var handler gin.HandlerFunc
	switch endpoint {
	case "/v1/responses":
		handler = NewResponsesAPIHandler(base).Responses
	case "/v1/chat/completions":
		handler = NewAPIHandler(base).ChatCompletions
	case "/v1/completions":
		handler = NewAPIHandler(base).Completions
	case "/v1/messages":
		handler = claude.NewCodeAPIHandler(base).ClaudeMessages
	}
	router.POST(endpoint, handler)
	return router
}

func TestOriginalStreamQuotaBootstrapHTTPStatusBodyAndHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions", "/v1/completions", "/v1/messages"} {
		for _, credential := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/credential_scope=%t", endpoint, credential), func(t *testing.T) {
					id, model := "stream-quota-high-"+t.Name(), "stream-quota-model"
					body := `{"error":{"type":"rate_limit_error","message":"Model rate limit exceeded"}}`
					scope, key := core.StateEffectScopeExactModel, model
					if credential {
						body = `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`
						scope, key = core.StateEffectScopeCredential, id
					}
					provider := &originalRuleStreamExecutor{
						failure: (&auth.Error{
							HTTPStatus: http.StatusTooManyRequests, Message: body,
						}).WithExecutionDisposition(
							core.ExecutionDisposition{
								DispatchPhase:  core.DispatchPhaseResponseStarted,
								Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
								RetryDirective: core.RetryDirectiveNoRetry, EffectScope: scope, EffectKey: key,
							}, 0,
						),
						headers: http.Header{"Content-Type": {"text/event-stream"}, "Retry-After": {"3600"}},
					}
					manager := auth.NewManager(nil, &auth.FillFirstSelector{}, nil)
					manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
					manager.SetRetryConfig(3, 30*time.Second, 0)
					manager.RegisterExecutor(provider)
					if _, err := manager.Register(
						t.Context(), &auth.Auth{ID: id, Provider: "codex", Status: auth.StatusActive},
					); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
					base := handlers.NewBaseAPIHandlers(&config.SDKConfig{PassthroughHeaders: true}, manager)
					router := originalStreamProtocolRouter(base, endpoint)
					requestBody := fmt.Sprintf(
						`{"model":%q,"input":"fixture","messages":[{"role":"user","content":`+
							`"fixture"}],"prompt":"fixture","stream":`+
							`true}`,
						model,
					)
					request := httptest.NewRequestWithContext(
						t.Context(), http.MethodPost, endpoint, strings.NewReader(requestBody),
					)
					request.Header.Set("Content-Type", "application/json")
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, request)
					var actual, expected any
					if err := json.Unmarshal(recorder.Body.Bytes(), &actual); err != nil {
						t.Fatalf("HTTP=%d body=%q error=%v", recorder.Code, recorder.Body.String(), err)
					}
					if err := json.Unmarshal([]byte(body), &expected); err != nil {
						t.Fatal(err)
					}
					if endpoint == "/v1/messages" {
						payload := expected.(map[string]any)
						payload["type"] = "error"
						detail := payload["error"].(map[string]any)
						if _, exists := detail["message"]; !exists {
							detail["message"] = body
						}
					}
					if recorder.Code != http.StatusTooManyRequests || !reflect.DeepEqual(actual, expected) ||
						recorder.Header().Get("Retry-After") != "3600" ||
						!strings.HasPrefix(recorder.Header().Get("Content-Type"), "application/json") {
						t.Fatalf(
							"HTTP=%d headers=%v body=%s",
							recorder.Code,
							recorder.Header(),
							recorder.Body.String(),
						)
					}
					current, found := manager.GetByID(id)
					if !found || current.Failed != 1 || provider.calls.Load() != 1 {
						t.Fatalf(
							"credential=%+v found=%t provider calls=%d",
							current,
							found,
							provider.calls.Load(),
						)
					}
				},
			)
		}
	}
}

func TestOriginalRequestRuleBootstrapHTTPStatusAndHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions", "/v1/completions", "/v1/messages"} {
		for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
			for _, action := range []string{"stop", "stop-and-cooldown"} {
				t.Run(
					fmt.Sprintf("%s/%d/%s", endpoint, status, action), func(t *testing.T) {
						const body = `{"error":{"message":"bootstrap_chunk_error","type":"server_error"}}`
						executor := &originalRuleStreamExecutor{
							failure: &auth.Error{HTTPStatus: status, Message: body},
							headers: http.Header{"Content-Type": {"text/event-stream"}, "Retry-After": {"17"}},
						}
						manager := auth.NewManager(nil, &auth.FillFirstSelector{}, nil)
						manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
						manager.SetRetryConfig(3, 5*time.Second, 5)
						manager.RegisterExecutor(executor)
						defer func(manager *auth.Manager) {
							//lint:ignore SA1012 验证 nil context 使用独立的 5 s 停止期限
							if err := manager.StopAutoRefresh(nil); err != nil {
								t.Errorf("stop auto refresh: %v", err)
							}
						}(manager)
						id, model := "original-rule-auth-"+t.Name(), "original-rule-model-"+t.Name()
						credential := &auth.Auth{
							ID: id, Provider: executor.Identifier(), Status: auth.StatusActive,
							Metadata: map[string]any{
								"request_scoped_errors": []config.RequestScopedErrorRule{
									{
										Status: status, Match: []string{"bootstrap_chunk_error"}, Action: action,
									},
								},
							},
						}
						if _, err := manager.Register(t.Context(), credential); err != nil {
							t.Fatal(err)
						}
						registry.GetGlobalRegistry().RegisterClient(
							id, executor.Identifier(), []*registry.ModelInfo{{ID: model}},
						)
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
						base := handlers.NewBaseAPIHandlers(
							&config.SDKConfig{PassthroughHeaders: true},
							manager,
						)
						router := originalStreamProtocolRouter(base, endpoint)
						requestBody := fmt.Sprintf(
							`{"model":%q,"input":"fixture","messages":[{"role":"user","content":`+
								`"fixture"}],"prompt":"fixture","stream":`+
								`true}`,
							model,
						)
						request := httptest.NewRequest(
							http.MethodPost,
							endpoint,
							strings.NewReader(requestBody),
						)
						request.Header.Set("Content-Type", "application/json")
						recorder := httptest.NewRecorder()
						router.ServeHTTP(recorder, request)
						response := recorder.Result()
						defer func(closeResource func() error) {
							if err := closeResource(); err != nil {
								t.Errorf("close recorded bootstrap response: %v", err)
							}
						}(response.Body.Close)
						var payload struct {
							Error struct{ Message, Type string }
						}
						if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
							t.Fatalf(
								"HTTP=%d headers=%v body=%q error=%v", response.StatusCode, response.Header,
								recorder.Body.String(), err,
							)
						}
						if response.StatusCode != status || payload.Error.Message != "bootstrap_chunk_error" ||
							payload.Error.Type != "server_error" {
							t.Fatalf("HTTP=%d body=%s", response.StatusCode, recorder.Body.String())
						}
						if response.Header.Get("Retry-After") != "17" || !strings.HasPrefix(
							response.Header.Get("Content-Type"), "application/json",
						) {
							t.Fatalf("HTTP headers=%v", response.Header)
						}
						current, _ := manager.GetByID(id)
						wantCooldown := action == "stop-and-cooldown"
						if executor.calls.Load() != 1 || current.Failed != 1 ||
							current.Unavailable != wantCooldown ||
							current.NextRetryAfter.IsZero() == wantCooldown {
							t.Fatalf(
								"calls=%d failures=%d unavailable=%t retry=%v",
								executor.calls.Load(),
								current.Failed,
								current.Unavailable,
								current.NextRetryAfter,
							)
						}
					},
				)
			}
		}
	}
}
