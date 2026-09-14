package logging

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestGinLogrusRecoveryHandlesErrAbortHandler verifies that gin v1.12.0 handles
// http.ErrAbortHandler internally without calling our custom recovery handler.
func TestGinLogrusRecoveryHandlesErrAbortHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET(
		"/abort", func(c *gin.Context) {
			panic(http.ErrAbortHandler)
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/abort", nil)
	recorder := httptest.NewRecorder()

	// gin v1.12.0 treats ErrAbortHandler as a broken pipe and silently aborts
	// the context. No panic propagates to the caller.
	engine.ServeHTTP(recorder, req)
}

func TestGinLogrusRecoveryHandlesRegularPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusRecovery())
	engine.GET(
		"/panic", func(c *gin.Context) {
			panic("boom")
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", recorder.Code)
	}
}

func TestIsAIAPIPathIncludesPublicAPIGroups(t *testing.T) {
	for _, path := range []string{
		"/v1",
		"/v1/models",
		"/v1/alpha/search",
		"/v1beta/interactions",
		"/openai/v1/videos",
		"/backend-api/codex/responses",
		"/api/provider/openai/v1/models",
	} {
		if !isAIAPIPath(path) {
			t.Fatalf("expected %s to be treated as AI API path", path)
		}
	}
	for _, path := range []string{
		"/v0/management/config",
		"/v10/models",
		"/openai/v10/videos",
		"/backend-api/codex-status",
		"/api/providers",
	} {
		if isAIAPIPath(path) {
			t.Fatalf("expected %s not to be treated as AI API path", path)
		}
	}
}

func TestIsAIAPIPathIncludesCodexLive(t *testing.T) {
	for _, path := range []string{
		"/v1/live",
		"/v1/live/call-1",
		"/v1/realtime",
		"/v1/realtime/calls",
		"/v1/realtime/calls/call-1",
	} {
		if !isAIAPIPath(path) {
			t.Fatalf("expected %s to be treated as AI API path", path)
		}
	}
}

func TestGinLogrusLoggerAddsRequestIDForCodexLive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(GinLogrusLogger())

	var requestIDFromContext string
	var requestIDFromGin string
	engine.POST(
		"/v1/live", func(c *gin.Context) {
			requestIDFromContext = GetRequestID(c.Request.Context())
			requestIDFromGin = GetGinRequestID(c)
			c.Status(http.StatusOK)
		},
	)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/live", nil))
	if requestIDFromContext == "" || requestIDFromGin != requestIDFromContext {
		t.Fatalf("context request ID = %q, Gin request ID = %q", requestIDFromContext, requestIDFromGin)
	}
}

func TestGinLogrusLoggerAddsRequestIDForCodexBackend(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusLogger())

	var requestIDFromContext string
	var requestIDFromGin string
	engine.POST(
		"/backend-api/codex/responses", func(c *gin.Context) {
			requestIDFromContext = GetRequestID(c.Request.Context())
			requestIDFromGin = GetGinRequestID(c)
			c.Status(http.StatusOK)
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", nil)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	if requestIDFromContext == "" {
		t.Fatalf("expected request ID in request context")
	}
	if requestIDFromGin != requestIDFromContext {
		t.Fatalf("expected Gin request ID %q to match context request ID %q", requestIDFromGin, requestIDFromContext)
	}
}

func TestGinLogrusLoggerAddsRequestStateForRootResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.Use(GinLogrusLogger())

	var requestID string
	var outcomeState *RequestOutcomeState
	engine.POST(
		"/responses", func(c *gin.Context) {
			requestID = GetRequestID(c.Request.Context())
			outcomeState = RequestOutcomeStateFromContext(c.Request.Context())
			c.Status(http.StatusOK)
		},
	)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/responses", nil))
	if len(requestID) != 32 {
		t.Fatalf("request ID length = %d, want 32: %q", len(requestID), requestID)
	}
	if outcomeState == nil {
		t.Fatal("request outcome state was not attached")
	}
}

func TestGenerateRequestIDFallbackRemainsUnique(t *testing.T) {
	previous := readRequestIDRandom
	readRequestIDRandom = func([]byte) (int, error) {
		return 0, errors.New("entropy unavailable")
	}
	t.Cleanup(
		func() {
			readRequestIDRandom = previous
		},
	)

	first := generateRequestID()
	second := generateRequestID()
	if len(first) != 32 || len(second) != 32 {
		t.Fatalf("fallback IDs = %q, %q, want 32 hex characters", first, second)
	}
	if first == second {
		t.Fatalf("fallback request IDs collided: %q", first)
	}
	if strings.Trim(first, "0") == "" || strings.Trim(second, "0") == "" {
		t.Fatalf("fallback request ID must not be all zero: %q, %q", first, second)
	}
}
