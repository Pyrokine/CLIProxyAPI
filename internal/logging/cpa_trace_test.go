package logging

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestFormatCPATraceID(t *testing.T) {
	selectedAt := time.Date(2026, time.August, 22, 12, 34, 56, 0, time.UTC)
	got := formatCPATraceID(selectedAt, "auth-index", "1234abcd")
	if got != "20260822123456-auth-index-1234abcd" {
		t.Fatalf("FormatCPATraceID() = %q", got)
	}

	for _, test := range []struct {
		name       string
		selectedAt time.Time
		authIndex  string
		requestID  string
	}{
		{name: "zero time", authIndex: "auth-index", requestID: "1234abcd"},
		{name: "empty auth index", selectedAt: selectedAt, requestID: "1234abcd"},
		{name: "empty request ID", selectedAt: selectedAt, authIndex: "auth-index"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				if gotEmpty := formatCPATraceID(test.selectedAt, test.authIndex, test.requestID); gotEmpty != "" {
					t.Fatalf("FormatCPATraceID() = %q, want empty", gotEmpty)
				}
			},
		)
	}
}

func TestSetGinCPATraceIDWritesUncommittedDirectResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/live", nil)
	setGinRequestID(c, "1234abcd")

	SetGinCPATraceID(c, "auth-index")
	got := recorder.Header().Get(CPATraceIDHeader)
	if !regexp.MustCompile(`^\d{14}-auth-index-1234abcd$`).MatchString(got) {
		t.Fatalf("trace ID = %q", got)
	}
}

func TestSetGinCPATraceIDUsesRequestContextFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	c.Request = req.WithContext(WithRequestID(req.Context(), "fallback"))

	SetGinCPATraceID(c, "auth-index")
	if got := recorder.Header().Get(CPATraceIDHeader); !strings.HasSuffix(got, "-auth-index-fallback") {
		t.Fatalf("trace ID = %q", got)
	}
}

func TestCPATraceIDMiddlewareRequiresSelectionBeforeResponseCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(CPATraceIDMiddleware())
	engine.GET(
		"/selected", func(c *gin.Context) {
			setGinRequestID(c, "1234abcd")
			callback := GinCPATraceIDCallback(c)
			callback("auth-index")
			c.Status(http.StatusOK)
		},
	)
	engine.GET(
		"/unselected", func(c *gin.Context) {
			setGinRequestID(c, "1234abcd")
			c.Status(http.StatusOK)
		},
	)
	engine.GET(
		"/committed", func(c *gin.Context) {
			setGinRequestID(c, "1234abcd")
			c.Writer.WriteHeaderNow()
			SetGinCPATraceID(c, "auth-index")
		},
	)

	t.Run(
		"writes selected auth trace", func(t *testing.T) {
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/selected", nil))
			traceID := recorder.Header().Get(CPATraceIDHeader)
			if !regexp.MustCompile(`^\d{14}-auth-index-1234abcd$`).MatchString(traceID) {
				t.Fatalf("trace ID = %q", traceID)
			}
		},
	)

	t.Run(
		"skips unselected request", func(t *testing.T) {
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/unselected", nil))
			if got := recorder.Header().Get(CPATraceIDHeader); got != "" {
				t.Fatalf("trace ID = %q, want empty", got)
			}
		},
	)

	t.Run(
		"skips committed response", func(t *testing.T) {
			recorder := httptest.NewRecorder()
			engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/committed", nil))
			if got := recorder.Header().Get(CPATraceIDHeader); got != "" {
				t.Fatalf("trace ID = %q, want empty", got)
			}
		},
	)
}

func TestSetGinCPATraceIDNoopsWithoutRequestID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	SetGinCPATraceID(nil, "auth-index")

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil).WithContext(context.Background())
	SetGinCPATraceID(c, "auth-index")
	if got := recorder.Header().Get(CPATraceIDHeader); got != "" {
		t.Fatalf("trace ID without request ID = %q", got)
	}
}

type traceFlushErrorWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w *traceFlushErrorWriter) FlushError() error { return w.err }

func TestCPATraceResponseWriterReportsFlushErrorAndSetsTraceID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	flushFailure := errors.New("transport flush failed")
	transport := &traceFlushErrorWriter{ResponseRecorder: httptest.NewRecorder(), err: flushFailure}
	c, _ := gin.CreateTestContext(transport)
	state := &cpaTraceState{}
	state.set("test-trace")
	c.Writer = &cpaTraceResponseWriter{ResponseWriter: c.Writer, state: state}
	if err := http.NewResponseController(c.Writer).Flush(); !errors.Is(err, flushFailure) {
		t.Fatalf("flush error = %v", err)
	}
	if got := transport.Result().Header.Get(CPATraceIDHeader); got != "test-trace" {
		t.Fatalf("trace header = %q", got)
	}
}

func TestCPATraceResponseWriterExposesUnderlyingWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	wrapper := &cpaTraceResponseWriter{ResponseWriter: c.Writer, state: &cpaTraceState{}}

	if wrapper.Unwrap() != c.Writer {
		t.Fatal("Unwrap() did not return the underlying Gin response writer")
	}
}

func TestCPATraceIDConcurrentSelectionAndResponseCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(CPATraceIDMiddleware())
	engine.GET(
		"/race", func(c *gin.Context) {
			setGinRequestID(c, "1234abcd")
			traceCallback := GinCPATraceIDCallback(c)
			start := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				<-start
				traceCallback("auth-index")
			}()
			close(start)
			_, _ = c.Writer.Write([]byte("\n"))
			<-done
		},
	)

	for range 100 {
		recorder := httptest.NewRecorder()
		engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/race", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
		}
	}
}
