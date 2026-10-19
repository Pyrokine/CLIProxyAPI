package claude

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type claudeResponseBodyError struct{ body []byte }

func (e claudeResponseBodyError) Error() string        { return "wrapped upstream error" }
func (e claudeResponseBodyError) ResponseBody() []byte { return e.body }

func TestClaudeErrorResponseTimeoutAndUpstreamJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	for _, tc := range []struct {
		status  int
		message string
		kind    string
		want    string
	}{
		{http.StatusRequestTimeout, "request timed out", "timeout_error", "request timed out"},
		{http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"context too large"}}`, "invalid_request_error", "context too large"},
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		handler.WriteErrorResponse(ctx, &handlers.ErrorMessage{StatusCode: tc.status, Error: errors.New(tc.message)})
		if recorder.Code != tc.status || gjson.GetBytes(recorder.Body.Bytes(), "type").String() != "error" ||
			gjson.GetBytes(recorder.Body.Bytes(), "error.type").String() != tc.kind ||
			gjson.GetBytes(recorder.Body.Bytes(), "error.message").String() != tc.want {
			t.Fatalf("status=%d body=%s, want status=%d type=%s message=%q", recorder.Code, recorder.Body.Bytes(), tc.status, tc.kind, tc.want)
		}
	}
}

func TestClaudeMissingThreadErrorSignalsReplayWithoutChangingOtherDirectResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	const missing = `{"type":"error","error":{"type":"not_found_error","message":"No thread state was found for the requested previous_message_id."}}`
	for _, tc := range []struct {
		name string
		msg  *handlers.ErrorMessage
	}{
		{"upstream body", &handlers.ErrorMessage{StatusCode: http.StatusNotFound, Error: claudeResponseBodyError{body: []byte(missing)}}},
		{"direct response", &handlers.ErrorMessage{StatusCode: http.StatusNotFound, DirectResponse: true, Body: []byte(missing)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			handler.WriteErrorResponse(ctx, tc.msg)
			if recorder.Code != http.StatusNotFound ||
				gjson.GetBytes(recorder.Body.Bytes(), "error.details.error_code").String() != "thread_not_found" {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.Bytes())
			}
		})
	}

	plain := `{"error":"blocked"}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	handler.WriteErrorResponse(ctx, &handlers.ErrorMessage{
		StatusCode: http.StatusForbidden, DirectResponse: true, Body: []byte(plain),
		Headers: http.Header{"Content-Type": {"application/problem+json"}},
	})
	if recorder.Body.String() != plain || recorder.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("direct response changed: status=%d body=%s headers=%v", recorder.Code, recorder.Body.Bytes(), recorder.Header())
	}
}

func TestClaudeStreamErrorIncludesThreadNotFoundDetails(t *testing.T) {
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	msg := &handlers.ErrorMessage{
		StatusCode: http.StatusNotFound,
		Error:      errors.New("No thread state was found for the requested previous_message_id."),
	}
	blocks := handler.claudeStreamErrorEvents(newClaudeStreamLifecycle(), msg)
	if !strings.HasPrefix(string(blocks), "event: error\ndata: ") || strings.Count(string(blocks), "\ndata: ") != 1 ||
		gjson.GetBytes(blocks[len("event: error\ndata: "):], "error.details.error_code").String() != "thread_not_found" {
		t.Fatalf("invalid stream error: %q", blocks)
	}
}

func TestClaudeInitialStreamErrorAcknowledgesClaudeEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	acknowledged := false
	msg := &handlers.ErrorMessage{
		StatusCode: http.StatusRequestTimeout,
		Error:      errors.New("upstream request timed out"),
		Acknowledge: func(err error) {
			acknowledged = true
			if err != nil {
				t.Errorf("delivery failed: %v", err)
			}
		},
	}
	if err := handler.WriteAcknowledgedStreamError(ctx, recorder, msg); err != nil {
		t.Fatal(err)
	}
	if !acknowledged || recorder.Code != http.StatusRequestTimeout ||
		gjson.GetBytes(recorder.Body.Bytes(), "error.type").String() != "timeout_error" {
		t.Fatalf("stream initialization error: acknowledged=%t status=%d body=%s", acknowledged, recorder.Code, recorder.Body.Bytes())
	}
}

func TestClaudeCooldownResponseRetainsRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	handler.WriteErrorResponse(ctx, &handlers.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      coreauth.NewModelCooldownError("claude-test", "claude", 20*time.Second),
	})
	if got := recorder.Header().Get("Retry-After"); got != "20" {
		t.Fatalf("Retry-After = %q, want 20", got)
	}
}
