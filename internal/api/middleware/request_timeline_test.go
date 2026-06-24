package middleware

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/gin-gonic/gin"
)

type timelineCaptureLogger struct {
	enabled         bool
	requestTimeline []byte
	streamingWriter *timelineCaptureStreamingWriter
}

func (l *timelineCaptureLogger) LogRequest(
	_, _ string,
	_ map[string][]string,
	_ []byte,
	_ int,
	_ map[string][]string,
	_, _, _, _, _ []byte,
	_ []*interfaces.ErrorMessage,
	_ string,
	_, _ time.Time,
) error {
	return nil
}

func (l *timelineCaptureLogger) LogRequestWithOptionsAndTimeline(
	_, _ string,
	_ map[string][]string,
	_ []byte,
	_ int,
	_ map[string][]string,
	_, _, _, _, _, requestTimeline []byte,
	_ []*interfaces.ErrorMessage,
	_ bool,
	_ string,
	_, _ time.Time,
) error {
	l.requestTimeline = bytes.Clone(requestTimeline)
	return nil
}

func (l *timelineCaptureLogger) LogStreamingRequest(
	_, _ string,
	_ map[string][]string,
	_ []byte,
	_ string,
) (logging.StreamingLogWriter, error) {
	l.streamingWriter = &timelineCaptureStreamingWriter{}
	return l.streamingWriter, nil
}

func (l *timelineCaptureLogger) IsEnabled() bool {
	return l.enabled
}

type timelineCaptureStreamingWriter struct {
	requestTimeline []byte
}

func (w *timelineCaptureStreamingWriter) WriteChunkAsync(_ []byte) {}

func (w *timelineCaptureStreamingWriter) WriteStatus(_ int, _ map[string][]string) error {
	return nil
}

func (w *timelineCaptureStreamingWriter) WriteAPIRequest(_ []byte) error {
	return nil
}

func (w *timelineCaptureStreamingWriter) WriteAPIResponse(_ []byte) error {
	return nil
}

func (w *timelineCaptureStreamingWriter) WriteAPIWebsocketTimeline(_ []byte) error {
	return nil
}

func (w *timelineCaptureStreamingWriter) SetFirstChunkTimestamp(_ time.Time) {}

func (w *timelineCaptureStreamingWriter) WriteRequestTimeline(requestTimeline []byte) error {
	w.requestTimeline = bytes.Clone(requestTimeline)
	return nil
}

func (w *timelineCaptureStreamingWriter) Close() error {
	return nil
}

func TestRequestLoggingMiddlewareRuntimeDisabledSkipsErrorOnlyLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &timelineCaptureLogger{enabled: false}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger, RequestLoggingOptions{RuntimeEnabled: func() bool { return false }}))
	engine.POST(
		"/v1/messages", func(c *gin.Context) {
			if _, exists := c.Get(logging.RequestTimelineGinKey); exists {
				t.Fatal("request timeline should not be created when runtime gate is disabled")
			}
			c.String(http.StatusInternalServerError, "fail")
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude"}`))
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusInternalServerError)
	}
	if len(logger.requestTimeline) > 0 {
		t.Fatalf("request timeline logged while runtime gate was disabled: %q", string(logger.requestTimeline))
	}
}

func TestRequestLoggingMiddlewareWritesRequestTimeline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &timelineCaptureLogger{enabled: true}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST(
		"/v1/messages", func(c *gin.Context) {
			value, exists := c.Get(logging.RequestTimelineGinKey)
			if !exists {
				t.Fatal("request timeline missing from gin context")
			}
			timeline, ok := value.(*logging.RequestTimeline)
			if !ok || timeline == nil {
				t.Fatal("request timeline has unexpected type")
			}
			timeline.Add("handler.work", "step=test")
			c.String(http.StatusOK, "ok")
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude"}`))
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	assertTimelineContains(
		t, logger.requestTimeline, "request.received", "handler.work", "client.response.first_write",
		"request.handler.done", "request.log.start",
	)
}

func TestRequestLoggingMiddlewareWritesStreamingRequestTimeline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &timelineCaptureLogger{enabled: true}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST(
		"/v1/messages", func(c *gin.Context) {
			c.Header("Content-Type", "text/event-stream")
			c.Writer.WriteHeader(http.StatusOK)
			_, _ = c.Writer.Write([]byte("data: ok\n\n"))
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"stream":true}`))
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if logger.streamingWriter == nil {
		t.Fatal("streaming writer was not created")
	}
	assertTimelineContains(
		t, logger.streamingWriter.requestTimeline, "request.received", "client.response.headers",
		"client.response.first_write", "request.handler.done", "request.log.start",
	)
}

func TestRequestLoggingMiddlewareSkipsTimelineForSuccessfulRequestsWhenDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &timelineCaptureLogger{enabled: false}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST(
		"/v1/messages", func(c *gin.Context) {
			c.String(http.StatusOK, "ok")
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude"}`))
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if len(logger.requestTimeline) > 0 {
		t.Fatalf("timeline was logged while request logging was disabled: %s", string(logger.requestTimeline))
	}
}

func TestRequestLoggingMiddlewareWritesTimelineForForcedErrorLog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &timelineCaptureLogger{enabled: false}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST(
		"/v1/messages", func(c *gin.Context) {
			c.String(http.StatusInternalServerError, "failed")
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude"}`))
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusInternalServerError)
	}
	assertTimelineContains(
		t, logger.requestTimeline, "request.received", "client.response.first_write", "request.handler.done",
		"request.log.start",
	)
}

func TestRequestLoggingMiddlewareSkipsManagementTimeline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger := &timelineCaptureLogger{enabled: true}
	engine := gin.New()
	engine.Use(RequestLoggingMiddleware(logger))
	engine.POST(
		"/v0/management/config", func(c *gin.Context) {
			if _, exists := c.Get(logging.RequestTimelineGinKey); exists {
				t.Fatal("request timeline should not be attached to management routes")
			}
			c.String(http.StatusOK, "ok")
		},
	)

	req := httptest.NewRequest(http.MethodPost, "/v0/management/config", bytes.NewBufferString(`{"enabled":true}`))
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if len(logger.requestTimeline) > 0 {
		t.Fatalf("management route timeline was logged: %s", string(logger.requestTimeline))
	}
}

func assertTimelineContains(t *testing.T, timeline []byte, events ...string) {
	t.Helper()
	if len(timeline) == 0 {
		t.Fatal("timeline is empty")
	}
	if !bytes.Contains(timeline, []byte("=== REQUEST TIMELINE ===")) {
		t.Fatalf("timeline missing section header: %s", string(timeline))
	}
	for _, event := range events {
		if !bytes.Contains(timeline, []byte(event)) {
			t.Fatalf("timeline missing %q: %s", event, string(timeline))
		}
	}
}
