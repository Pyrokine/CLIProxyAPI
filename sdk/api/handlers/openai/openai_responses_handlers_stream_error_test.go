package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/interfaces"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestForwardResponsesStreamTerminalErrorUsesResponsesErrorChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New("unexpected EOF")}
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, nil, data, errs)
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("expected responses error chunk, got: %q", body)
	}
	if strings.Contains(body, `"error":{`) {
		t.Fatalf("expected streaming error chunk (top-level type), got HTTP error body: %q", body)
	}
}

func TestForwardResponsesStreamBuffersMalformedSSEFrameUntilComplete(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte, 2)
	data <- []byte("event: response.created\ndata: {\"type\":")
	data <- []byte("\"response.created\"}\n\n")
	close(data)
	errs := make(chan *interfaces.ErrorMessage)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, nil, data, errs)
	body := recorder.Body.String()
	if strings.Contains(body, "data: {\"type\":\n\n") {
		t.Fatalf("malformed partial SSE frame was forwarded: %q", body)
	}
	if !strings.Contains(body, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n") {
		t.Fatalf("expected completed SSE frame, got: %q", body)
	}
}

func TestForwardResponsesStreamPreservesFragmentedSSEFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte, 2)
	data <- []byte("event: response.created\n")
	data <- []byte("data: {\"type\":\"response.created\"}\n\n")
	close(data)
	errs := make(chan *interfaces.ErrorMessage)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, nil, data, errs)
	body := recorder.Body.String()
	if strings.Contains(body, "event: response.created\n\ndata:") {
		t.Fatalf("fragmented event and data fields were split into separate SSE frames: %q", body)
	}
	if !strings.Contains(body, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n") {
		t.Fatalf("expected complete SSE frame, got: %q", body)
	}
}
