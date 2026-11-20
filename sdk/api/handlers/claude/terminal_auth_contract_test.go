package claude

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

type terminalClaudeBodyError struct{}

func (terminalClaudeBodyError) Error() string { return "refresh_token_reused" }
func (terminalClaudeBodyError) ResponseBody() []byte {
	return []byte((`{"error":{"type":"authentication_error","message":"api_key=SYNTHETIC_C` +
		`LAUDE_SECRET","provider_debug":{"token":"SYNTHETIC_CLAUDE_SECRET"}}}`))
}

func TestClaudeTerminalAuthenticationErrorPreservesNativeShapeAndPrivacy(t *testing.T) {
	terminal := auth.NewTerminalAuthError(
		&auth.Error{
			Code: "auth_unavailable", HTTPStatus: http.StatusServiceUnavailable,
		}, terminalClaudeBodyError{},
	)
	api := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	if err := api.WriteErrorResponseWithError(
		ctx, &handlers.ErrorMessage{StatusCode: http.StatusServiceUnavailable, Error: terminal},
	); err != nil {
		t.Fatal(err)
	}
	var response claudeErrorResponse
	if err := json.Unmarshal(
		recorder.Body.Bytes(), &response,
	); err !=
		nil ||
		recorder.Code !=
			http.StatusServiceUnavailable ||
		response.Type !=
			"error" ||
		response.Error.Type !=
			("authentication"+
				"_error") {
		t.Fatalf("native terminal HTTP=%d body=%s error=%v", recorder.Code, recorder.Body.String(), err)
	}
	if strings.Contains(recorder.Body.String(), "SYNTHETIC_CLAUDE_SECRET") || !strings.Contains(
		response.Error.Message, "refresh_token_reused",
	) || !strings.Contains(response.Error.Message, "[REDACTED]") {
		t.Fatalf("native terminal error reason/privacy=%s", recorder.Body.String())
	}
}
