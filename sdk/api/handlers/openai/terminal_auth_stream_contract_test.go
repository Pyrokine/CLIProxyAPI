package openai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestOpenAITerminalAuthenticationClassificationReachesStreamingConsumers(t *testing.T) {
	for _, mode := range []string{"chat SSE", "image SSE", "Responses websocket"} {
		t.Run(
			mode, func(t *testing.T) {
				msg := &handlers.ErrorMessage{
					StatusCode: http.StatusServiceUnavailable,
					Error: auth.NewTerminalAuthError(
						&auth.Error{Code: "auth_unavailable", HTTPStatus: http.StatusServiceUnavailable},
						&auth.Error{
							HTTPStatus: http.StatusUnauthorized,
							Message:    "refresh_token_reused api_key=SYNTHETIC_STREAM_SECRET",
						},
					),
				}
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(t.Context())
				base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil)
				api := &APIHandler{BaseAPIHandler: base}
				var body string
				if mode == "Responses websocket" {
					payload, err := buildResponsesWebsocketErrorPayload(msg)
					if err != nil {
						t.Fatal(err)
					}
					body = string(payload)
				} else {
					ctx.Writer.WriteHeaderNow()
					if mode == "image SSE" {
						if err := api.writeInitialStreamError(ctx, ctx.Writer, msg); err != nil {
							t.Fatal(err)
						}
					} else {
						data := make(chan core.StreamChunk)
						errs := make(chan *handlers.ErrorMessage, 1)
						errs <- msg
						api.handleStreamResult(ctx, ctx.Writer, func(error) {}, data, errs, nil)
					}
					body = recorder.Body.String()
				}
				for _, field := range []string{
					`"code":"upstream_authentication_required"`, `"type":"authentication_error"`, `"retryable":false`,
					"refresh_token_reused", "[REDACTED]",
				} {
					if !strings.Contains(body, field) {
						t.Fatalf("%s lost %s: %s", mode, field, body)
					}
				}
				if strings.Contains(body, "SYNTHETIC_STREAM_SECRET") || strings.Contains(
					body, `"code":"invalid_api_key"`,
				) {
					t.Fatalf("%s leaked or misclassified upstream credentials: %s", mode, body)
				}
			},
		)
	}
}
