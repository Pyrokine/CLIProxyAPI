package middleware

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/gin-gonic/gin"
)

func TestExtractRequestBodyPrefersOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &responseWriterWrapper{
		requestInfo: &requestInfo{Body: []byte("original-body")},
	}

	body := wrapper.extractRequestBody(c)
	if string(body) != "original-body" {
		t.Fatalf("request body = %q, want %q", string(body), "original-body")
	}

	c.Set(requestBodyOverrideContextKey, []byte("override-body"))
	body = wrapper.extractRequestBody(c)
	if string(body) != "override-body" {
		t.Fatalf("request body = %q, want %q", string(body), "override-body")
	}
}

func TestExtractRequestBodySupportsStringOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	wrapper := &responseWriterWrapper{}
	c.Set(requestBodyOverrideContextKey, "override-as-string")

	body := wrapper.extractRequestBody(c)
	if string(body) != "override-as-string" {
		t.Fatalf("request body = %q, want %q", string(body), "override-as-string")
	}
}

type responseFlushErrorWriter struct {
	*httptest.ResponseRecorder
	err error
}

func (w *responseFlushErrorWriter) FlushError() error { return w.err }

func TestResponseWriterWrapperReportsFlushError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	flushFailure := errors.New("transport flush failed")
	transport := &responseFlushErrorWriter{ResponseRecorder: httptest.NewRecorder(), err: flushFailure}
	c, _ := gin.CreateTestContext(transport)
	c.Writer = newResponseWriterWrapper(c.Writer, nil, &requestInfo{}, nil, false)
	if err := http.NewResponseController(c.Writer).Flush(); !errors.Is(err, flushFailure) {
		t.Fatalf("flush error = %v", err)
	}
	if c.Writer.Status() != http.StatusOK || !c.Writer.Written() {
		t.Fatalf("Gin response was not committed: status=%d written=%t", c.Writer.Status(), c.Writer.Written())
	}
}

func TestResponseWriterWrapperExposesUnderlyingWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	wrapper := &responseWriterWrapper{ResponseWriter: c.Writer}

	if wrapper.Unwrap() != c.Writer {
		t.Fatal("Unwrap() did not return the underlying Gin response writer")
	}
}

func TestResponseWriterWrappersPreserveReadDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(logging.CPATraceIDMiddleware())
	engine.Use(
		func(c *gin.Context) {
			c.Writer = newResponseWriterWrapper(c.Writer, nil, &requestInfo{}, nil, false)
			c.Next()
		},
	)
	engine.POST(
		"/v1/messages", func(c *gin.Context) {
			controller := http.NewResponseController(c.Writer)
			if errDeadline := controller.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); errDeadline != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": errDeadline.Error()})
				return
			}
			if _, errRead := io.ReadAll(c.Request.Body); errRead == nil {
				c.Status(http.StatusNoContent)
				return
			}
			c.Request.Close = true
			c.Header("Connection", "close")
			c.JSON(http.StatusRequestTimeout, gin.H{"status": http.StatusRequestTimeout})
		},
	)
	server := httptest.NewServer(engine)
	defer server.Close()

	conn, errDial := net.Dial("tcp", server.Listener.Addr().String())
	if errDial != nil {
		t.Fatalf("dial test server: %v", errDial)
	}
	defer func() { _ = conn.Close() }()
	if _, errWrite := fmt.Fprintf(
		conn,
		"POST /v1/messages HTTP/1.1\r\nHost: test\r\nContent-Type: application/json\r\nContent-Length: 1024\r\n\r\n{",
	); errWrite != nil {
		t.Fatalf("write partial request: %v", errWrite)
	}
	if errDeadline := conn.SetReadDeadline(time.Now().Add(time.Second)); errDeadline != nil {
		t.Fatalf("set client read deadline: %v", errDeadline)
	}

	response, errResponse := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if errResponse != nil {
		t.Fatalf("read timeout response: %v", errResponse)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("response status = %d, want %d", response.StatusCode, http.StatusRequestTimeout)
	}
}
