package httpwire

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

type flushErrorWriter struct {
	*httptest.ResponseRecorder
	legacyFlushes int
	flushes       int
}

func (w *flushErrorWriter) Flush() {
	w.legacyFlushes++
	w.ResponseRecorder.Flush()
}

func (w *flushErrorWriter) FlushError() error {
	w.flushes++
	return io.ErrClosedPipe
}

type flushMiddlewareWriter struct {
	http.ResponseWriter
	flushes int
}

func (w *flushMiddlewareWriter) FlushError() error {
	w.flushes++
	w.Header().Set("X-Middleware-Flush", "preserved")
	return FlushResponse(w.ResponseWriter)
}

func TestFlushResponsePreservesMiddlewareAndGinStatus(t *testing.T) {
	transport := &flushErrorWriter{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(transport)
	c.Writer.WriteHeader(http.StatusAccepted)
	wrapper := &flushMiddlewareWriter{ResponseWriter: c.Writer}
	if err := FlushResponse(wrapper); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("flush error = %v", err)
	}
	if wrapper.flushes != 1 || transport.flushes != 1 || transport.legacyFlushes != 0 {
		t.Fatalf("flush counts = %d/%d/%d", wrapper.flushes, transport.flushes, transport.legacyFlushes)
	}
	if !c.Writer.Written() || c.Writer.Status() != http.StatusAccepted || transport.Code != http.StatusAccepted ||
		transport.Header().Get("X-Middleware-Flush") != "preserved" {
		t.Fatalf("status or middleware header lost: status=%d transport=%d headers=%v", c.Writer.Status(), transport.Code, transport.Header())
	}
}

type bufferedFlushWriter struct {
	http.ResponseWriter
	buffer  *bufio.Writer
	flushes int
}

func (w *bufferedFlushWriter) Write(data []byte) (int, error) { return w.buffer.Write(data) }
func (w *bufferedFlushWriter) Unwrap() http.ResponseWriter    { return w.ResponseWriter }
func (w *bufferedFlushWriter) Flush() {
	w.flushes++
	_ = w.buffer.Flush()
	w.ResponseWriter.(http.Flusher).Flush()
}

func TestFlushResponsePreservesBufferedLegacyMiddleware(t *testing.T) {
	transport := httptest.NewRecorder()
	buffered := &bufferedFlushWriter{ResponseWriter: transport, buffer: bufio.NewWriter(transport)}
	if _, err := buffered.Write([]byte("data: buffered\n\n")); err != nil {
		t.Fatal(err)
	}
	if err := FlushResponse(buffered); err != nil {
		t.Fatal(err)
	}
	if buffered.flushes != 1 || transport.Body.String() != "data: buffered\n\n" || !transport.Flushed {
		t.Fatalf("legacy middleware flush not preserved: flushes=%d body=%q", buffered.flushes, transport.Body.String())
	}
}
