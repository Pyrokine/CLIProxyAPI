package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
)

func TestRequestLoggingMiddlewareWritesRouteTraceAfterTimeline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &timelineCaptureLogger{enabled: true}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST(
		"/v1/responses", func(c *gin.Context) {
			callback := logging.GinRouteTraceCallback(c)
			if callback == nil {
				t.Fatal("route trace callback missing from gin context")
			}
			callback(
				cliproxyexecutor.RouteTraceEvent{
					Attempt:     1,
					Provider:    "codex",
					Model:       "gpt-5",
					AuthIndex:   "0123456789abcdef",
					Action:      "attempt.completed",
					ResultCode:  "success",
					FinalTarget: true,
				},
			)
			c.String(http.StatusOK, "ok")
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5"}`))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	output := logger.requestTimeline
	timelineIndex := bytes.Index(output, []byte("=== REQUEST TIMELINE ==="))
	routeIndex := bytes.Index(output, []byte("=== ROUTE TRACE ==="))
	if timelineIndex < 0 || routeIndex <= timelineIndex {
		t.Fatalf("request trace sections are missing or out of order: %s", string(output))
	}
	if !bytes.Contains(output, []byte(`"auth_index":"0123456789abcdef"`)) ||
		!bytes.Contains(output, []byte(`"result_code":"success"`)) {
		t.Fatalf("route trace event missing: %s", string(output))
	}
}

func TestRequestLoggingMiddlewareWaitsForStreamResultTrace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &timelineCaptureLogger{enabled: true}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST(
		"/v1/responses", func(c *gin.Context) {
			callback := logging.GinRouteTraceCallback(c)
			if callback == nil {
				t.Fatal("route trace callback missing from gin context")
			}
			callback(
				cliproxyexecutor.RouteTraceEvent{
					Attempt:     1,
					Provider:    "codex",
					Model:       "gpt-6-astra",
					Action:      "attempt.stream_started",
					ResultCode:  "stream_started",
					FinalTarget: false,
				},
			)
			go func() {
				time.Sleep(25 * time.Millisecond)
				callback(
					cliproxyexecutor.RouteTraceEvent{
						Attempt:     1,
						Provider:    "codex",
						Model:       "gpt-6-astra",
						Action:      "attempt.failed",
						Reason:      "stream_terminated",
						ResultCode:  "stream_failed",
						FinalTarget: true,
					},
				)
				callback(
					cliproxyexecutor.RouteTraceEvent{
						Attempt:     1,
						Provider:    "codex",
						Model:       "gpt-6-astra",
						Action:      "result.recorded",
						ResultCode:  "stream_failed",
						FinalTarget: true,
					},
				)
			}()
			c.Status(http.StatusOK)
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)

	if !bytes.Contains(logger.requestTimeline, []byte(`"action":"result.recorded"`)) ||
		!bytes.Contains(logger.requestTimeline, []byte(`"result_code":"stream_failed"`)) {
		t.Fatalf("stream terminal route trace missing: %s", logger.requestTimeline)
	}
}
