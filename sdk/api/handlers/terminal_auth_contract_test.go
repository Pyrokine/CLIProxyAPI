package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

type terminalDirectBodyError struct{}

func (terminalDirectBodyError) Error() string        { return "refresh_token_reused" }
func (terminalDirectBodyError) StatusCode() int      { return http.StatusUnauthorized }
func (terminalDirectBodyError) DirectResponse() bool { return true }
func (terminalDirectBodyError) ResponseBody() []byte {
	return []byte(`{"error":{"message":"api_key=SYNTHETIC_DIRECT_SECRET","code":"invalid_api_key"}}`)
}

func TestTerminalAuthenticationErrorOverridesRawUpstreamBody(t *testing.T) {
	cause := terminalDirectBodyError{}
	terminal := auth.NewTerminalAuthError(&auth.Error{HTTPStatus: http.StatusServiceUnavailable}, cause)
	msg := executionErrorMessage(terminal)
	if msg == nil || msg.StatusCode != http.StatusServiceUnavailable || msg.DirectResponse ||
		len(msg.Body) != 0 ||
		!auth.IsTerminalAuthError(msg.Error) {
		t.Fatalf("terminal execution message=%+v", msg)
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	NewBaseAPIHandlers(&config.SDKConfig{}, nil).WriteErrorResponse(ctx, msg)
	var response ErrorResponse
	if err := json.Unmarshal(
		recorder.Body.Bytes(), &response,
	); err !=
		nil ||
		recorder.Code !=
			http.StatusServiceUnavailable ||
		response.Error.Code !=
			"upstream_authentication_required" ||
		response.Error.Retryable ==
			nil ||
		*response.Error.Retryable {
		t.Fatalf("terminal HTTP=%d body=%s error=%v", recorder.Code, recorder.Body.String(), err)
	}
	if strings.Contains(recorder.Body.String(), "SYNTHETIC_DIRECT_SECRET") || !strings.Contains(
		response.Error.Message, "refresh_token_reused",
	) {
		t.Fatalf("terminal body reason/privacy=%s", recorder.Body.String())
	}
	ordinary := executionErrorMessage(cause)
	if ordinary == nil || !ordinary.DirectResponse || string(ordinary.Body) != string(cause.ResponseBody()) {
		t.Fatalf("ordinary direct-response contract changed: %+v", ordinary)
	}
}
