package claude_test

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	backendconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	claudehandlers "github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/claude"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestClaudeHTTPRejectsOversizedAntigravitySignature(t *testing.T) {
	gin.SetMode(gin.TestMode)
	value := "cpa-gemini-carrier-v1:next:text:" + base64.RawStdEncoding.EncodeToString(
		[]byte(strings.Repeat(
			"R", 32*1024*1024+1,
		)),
	)
	content := (`"messages":[{"role":"user","content":"hello"},{"role":"assistant","con` +
		`tent":[{"type":"thinking","thinking":"","signature":"`) + value + `"},{"type":"text","text":"visible"}]}]`
	for _, entry := range []string{"generate", "claude-aggregate", "stream", "count"} {
		t.Run(
			entry, func(t *testing.T) {
				model, path, streaming := "gemini-2.5-flash", "/v1/messages", "false"
				if entry == "claude-aggregate" {
					model = "claude-sonnet-4-6"
				}
				if entry == "stream" {
					streaming = "true"
				}
				if entry == "count" {
					path += "/count_tokens"
				}
				cfg := &backendconfig.Config{}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetConfig(cfg)
				manager.RegisterExecutor(runtimeexecutor.NewAntigravityExecutor(cfg))
				auth := &coreauth.Auth{
					ID: t.Name(), Provider: "antigravity", Status: coreauth.StatusActive,
					Attributes: map[string]string{"base_url": "https://signature-fixture.invalid"},
					Metadata: map[string]any{
						"access_token": "synthetic-fixture", "project_id": "test-project",
						"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
					},
				}
				if _, err := manager.Register(t.Context(), auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				var calls atomic.Int32
				manager.SetRoundTripperProvider(
					clineHandlerTransport{
						clineHandlerRoundTripFunc(
							func(*http.Request) (*http.Response, error) {
								calls.Add(1)
								return nil, errors.New("offline transport reached after oversized signature")
							},
						),
					},
				)
				handler := claudehandlers.NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))
				router := gin.New()
				router.POST("/v1/messages", handler.ClaudeMessages)
				router.POST("/v1/messages/count_tokens", handler.ClaudeCountTokens)
				request := httptest.NewRequest(
					http.MethodPost, path,
					strings.NewReader(`{"model":"`+model+`","max_tokens":8,"stream":`+streaming+`,`+content+`}`),
				)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Anthropic-Version", "2023-06-01")
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				body := recorder.Body.Bytes()
				message := gjson.GetBytes(body, "error.message").String()
				if recorder.Code != http.StatusBadRequest || calls.Load() != 0 || !strings.Contains(
					message, "33554432",
				) || strings.Contains(message, "RRRR") {
					t.Fatalf("oversized input: HTTP=%d calls=%d body=%s", recorder.Code, calls.Load(), body)
				}
				current, found := manager.GetByID(auth.ID)
				if !found || current.Status != coreauth.StatusActive || current.Unavailable ||
					!current.NextRetryAfter.IsZero() {
					t.Fatalf("invalid signature changed credential availability: found=%v", found)
				}
			},
		)
	}
}
