package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

const reproOAuthMessage = `token refresh failed with status 401: {"error":{"message":"Refresh credential has ` +
	`already been consumed; sign in again.","type":"invalid_request_error","code":"refresh_token_reused"}}`
const reproCompletedResponse = `{"id":"resp-fixture","object":"response","status":"completed","output":[]}`

type reproOAuthExecutor struct {
	calls        atomic.Int32
	refreshCalls atomic.Int32
}

func (*reproOAuthExecutor) Identifier() string { return "codex" }
func (e *reproOAuthExecutor) Execute(_ context.Context, _ *auth.Auth, _ core.Request, _ core.Options) (
	core.Response,
	error,
) {
	e.calls.Add(1)
	return core.Response{Payload: []byte(reproCompletedResponse)}, nil
}
func (e *reproOAuthExecutor) ExecuteStream(
	_ context.Context,
	_ *auth.Auth,
	_ core.Request,
	_ core.Options,
) (*core.StreamResult, error) {
	e.calls.Add(1)
	chunks := make(chan core.StreamChunk, 1)
	chunks <- core.StreamChunk{
		Payload: []byte("event: response.completed\ndata: {\"type\":" +
			"\"response.completed\",\"response\":" +
			reproCompletedResponse +
			"}\n\n"),
	}
	close(chunks)
	return &core.StreamResult{Chunks: chunks}, nil
}
func (e *reproOAuthExecutor) Refresh(_ context.Context, _ *auth.Auth) (*auth.Auth, error) {
	e.refreshCalls.Add(1)
	return nil, errors.New(reproOAuthMessage)
}
func (*reproOAuthExecutor) CountTokens(_ context.Context, _ *auth.Auth, _ core.Request, _ core.Options) (
	core.Response,
	error,
) {
	return core.Response{}, errors.New("count is not used by this fixture")
}

func (*reproOAuthExecutor) HttpRequest(
	_ context.Context,
	_ *auth.Auth,
	_ *http.Request,
) (*http.Response, error) {
	return nil, errors.New("network is not used by this fixture")
}

func TestReproPermanentOAuthFailureClassification(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
				model := fmt.Sprintf("repro-permanent-oauth-%t", stream)
				failedID, healthyID := "failed-"+model, "healthy-"+model
				implementation := &reproOAuthExecutor{}
				manager := auth.NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() auth.HomeClient { return nil })
				manager.RegisterExecutor(implementation)
				defer func(manager *auth.Manager) {
					//lint:ignore SA1012 验证 nil context 使用独立的 5 s 停止期限
					if err := manager.StopAutoRefresh(nil); err != nil {
						t.Errorf("stop auto refresh: %v", err)
					}
				}(manager)
				failed := &auth.Auth{
					ID: failedID, Provider: "codex", Status: auth.StatusError,
					Unavailable: true, StatusMessage: "unauthorized",
					LastError: &auth.Error{
						Code: "unauthorized", Message: reproOAuthMessage, HTTPStatus: http.StatusUnauthorized,
					},
					Metadata: map[string]any{
						"access_token": "expired-fixture", "expired": time.Now().Add(-time.Hour).Format(time.RFC3339),
					},
				}
				if _, err := manager.Register(t.Context(), failed); err != nil {
					t.Fatal(err)
				}
				catalog := registry.GetGlobalRegistry()
				catalog.RegisterClient(failedID, "codex", []*registry.ModelInfo{{ID: model}})
				manager.RefreshSchedulerEntry(failedID)
				t.Cleanup(func() { catalog.UnregisterClient(failedID); catalog.UnregisterClient(healthyID) })
				base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager)
				router := gin.New()
				router.POST("/v1/responses", NewResponsesAPIHandler(base).Responses)
				request := func() *httptest.ResponseRecorder {
					body := fmt.Sprintf(`{"model":%q,"input":"fixture","stream":%t}`, model, stream)
					req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("User-Agent", "codex_cli_rs/0.153.4")
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, req)
					return recorder
				}
				for attempt := range 3 {
					got := request()
					var payload struct {
						Error struct {
							Code, Type, Message string
							Retryable           *bool `json:"retryable"`
						} `json:"error"`
					}
					if err := json.Unmarshal(got.Body.Bytes(), &payload); err != nil {
						t.Fatalf("decode HTTP %d body %q: %v", got.Code, got.Body.String(), err)
					}
					if got.Code != http.StatusServiceUnavailable ||
						payload.Error.Type != "authentication_error" ||
						payload.Error.Code != "upstream_authentication_required" ||
						payload.Error.Retryable == nil ||
						*payload.Error.Retryable {
						t.Errorf("attempt=%d HTTP=%d body=%s", attempt, got.Code, got.Body.String())
					}
					if !strings.Contains(
						payload.Error.Message, "refresh_token_reused",
					) || !strings.Contains(
						payload.Error.Message, "[REDACTED]",
					) || strings.Contains(payload.Error.Message, "expired-fixture") {
						t.Errorf("terminal auth reason or privacy differs: %s", got.Body.String())
					}
				}
				if implementation.calls.Load() != 0 || implementation.refreshCalls.Load() != 0 {
					t.Fatalf(
						"terminal auth dispatch=%d refresh=%d", implementation.calls.Load(),
						implementation.refreshCalls.Load(),
					)
				}
				if _, err := manager.Register(
					t.Context(), &auth.Auth{ID: healthyID, Provider: "codex", Status: auth.StatusActive},
				); err != nil {
					t.Fatal(err)
				}
				catalog.RegisterClient(healthyID, "codex", []*registry.ModelInfo{{ID: model}})
				manager.RefreshSchedulerEntry(healthyID)
				if got := request(); got.Code != http.StatusOK || implementation.calls.Load() != 1 {
					t.Fatalf(
						"published healthy control HTTP=%d calls=%d body=%s",
						got.Code,
						implementation.calls.Load(),
						got.Body.String(),
					)
				}
			},
		)
	}
}
