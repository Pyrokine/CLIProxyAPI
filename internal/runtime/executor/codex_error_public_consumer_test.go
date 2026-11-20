package executor_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	runtimeexecutor "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers/openai"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
)

type codexPublicTransport struct {
	body     string
	attempts atomic.Int32
}

func (t *codexPublicTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.attempts.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(t.body)), Request: request,
	}, nil
}

func (t *codexPublicTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper { return t }

func TestCodexPublicErrorEnvelopeConsumer(t *testing.T) {
	const payload = `{"type":"response.failed","sequence_number":51,"extensions":{"access_token":"synthetic-` +
		`extension-secret"},"headers":{"Authorization":"Bearer synthetic-header-secret"},"response":{"error":` +
		`{"type":"invalid_request_error","message":"Bearer synthetic-message-secret https://127.0.0.1/` +
		`internal","cause":{"api_key":"synthetic-cause-secret"}}}}`
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(
			name, func(t *testing.T) {
				cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
				transport := &codexPublicTransport{body: "data: " + payload + "\n\n"}
				ctx := home.WithClientScope(
					translator.WithPluginHooks(t.Context(), nil), home.ClientScope{ID: t.Name(), Enabled: false},
				)
				manager := coreauth.NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() coreauth.HomeClient { return nil })
				manager.SetConfig(cfg)
				manager.SetRetryConfig(0, 0, 1)
				manager.SetRoundTripperProvider(transport)
				manager.RegisterExecutor(runtimeexecutor.NewCodexAutoExecutor(cfg))
				auth := &coreauth.Auth{
					Provider: "codex", Attributes: map[string]string{
						"api_key": "synthetic-public-key", "base_url": "https://codex-public-consumer.invalid/v1",
					},
				}
				auth.ID, auth.Status = "synthetic-public-"+t.Name(), coreauth.StatusActive
				registered, err := manager.Register(coreauth.WithSkipPersist(ctx), auth)
				if err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(
					registered.ID, "codex", []*registry.ModelInfo{{ID: "gpt-5.5"}},
				)
				manager.RefreshSchedulerEntry(registered.ID)
				t.Cleanup(
					func() {
						registry.GetGlobalRegistry().UnregisterClient(registered.ID)
						_ = manager.StopDeferredAuthRecreates(context.Background())
					},
				)
				api := openai.NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(nil, manager))
				router := gin.New()
				router.POST("/v1/responses", api.Responses)
				body := `{"model":"gpt-5.5","input":"synthetic","stream":false}`
				if stream {
					body = strings.Replace(body, "false", "true", 1)
				}
				request := httptest.NewRequest(
					http.MethodPost, "/v1/responses", strings.NewReader(body),
				).WithContext(ctx)
				request.Header.Set("Content-Type", "application/json")
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusBadRequest || transport.attempts.Load() != 1 {
					t.Fatalf(
						"public status=%d, attempts=%d, response=%s", recorder.Code, transport.attempts.Load(),
						recorder.Body.String(),
					)
				}
				for _, secret := range []string{
					"synthetic-extension-secret", "synthetic-header-secret", "synthetic-message-secret",
					"synthetic-cause-secret", "127.0.0.1",
				} {
					if strings.Contains(recorder.Body.String(), secret) {
						t.Errorf("public response disclosed %q: %s", secret, recorder.Body.String())
					}
				}
				var response map[string]any
				if json.Unmarshal(
					recorder.Body.Bytes(), &response,
				) != nil || response["sequence_number"] != float64(51) {
					t.Fatalf("public error lost sequence_number: %s", recorder.Body.String())
				}
			},
		)
	}
}
