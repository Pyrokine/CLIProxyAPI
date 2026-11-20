package gemini

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

func TestGeminiTerminalAuthenticationClassificationReachesStreamError(t *testing.T) {
	for _, alt := range []string{"", "json"} {
		t.Run(
			"alt="+alt, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(
					http.MethodPost, "/v1beta/models/model:streamGenerateContent", nil,
				).WithContext(t.Context())
				ctx.Writer.WriteHeaderNow()
				data := make(chan core.StreamChunk)
				errs := make(chan *handlers.ErrorMessage, 1)
				errs <- &handlers.ErrorMessage{
					StatusCode: http.StatusServiceUnavailable,
					Error: auth.NewTerminalAuthError(
						&auth.Error{Code: "auth_unavailable", HTTPStatus: http.StatusServiceUnavailable},
						&auth.Error{
							HTTPStatus: http.StatusUnauthorized,
							Message:    "refresh_token_reused api_key=SYNTHETIC_GEMINI_SECRET",
						},
					),
				}
				base := handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil)
				forwardStream(base, ctx, ctx.Writer, alt, func(error) {}, data, errs, func([]byte) error { return nil })
				body := recorder.Body.String()
				for _, field := range []string{
					`"code":"upstream_authentication_required"`, `"type":"authentication_error"`, `"retryable":false`,
					"refresh_token_reused", "[REDACTED]",
				} {
					if !strings.Contains(body, field) {
						t.Fatalf("stream error lost %s: %s", field, body)
					}
				}
				if strings.Contains(body, "SYNTHETIC_GEMINI_SECRET") {
					t.Fatalf("stream error leaked upstream credential: %s", body)
				}
			},
		)
	}
}
