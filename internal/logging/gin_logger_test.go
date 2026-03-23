package logging

import (
	"net/http"
	"net/http/httptest"
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
