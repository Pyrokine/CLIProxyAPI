package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

type responsesStreamTestExecutor struct {
	payload             []byte
	closeWithoutPayload bool
}

func (e *responsesStreamTestExecutor) Identifier() string { return "responses-stream-test" }

func (e *responsesStreamTestExecutor) Execute(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *responsesStreamTestExecutor) ExecuteStream(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 1)
	if !e.closeWithoutPayload {
		chunks <- coreexecutor.StreamChunk{Payload: append([]byte(nil), e.payload...)}
	}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *responsesStreamTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *responsesStreamTestExecutor) CountTokens(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *responsesStreamTestExecutor) HttpRequest(
	context.Context,
	*coreauth.Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestReceiveInitialResponsesStreamPreservesBufferedErrorAfterDataClose(t *testing.T) {
	data := make(chan []byte)
	close(data)
	errs := make(chan *handlers.ErrorMessage, 1)
	want := &handlers.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New("quota exhausted")}
	errs <- want
	close(errs)

	chunk, errMsg, streamErr := receiveInitialResponsesStream(context.Background(), data, errs)
	if streamErr != nil {
		t.Fatalf("stream error = %v, want nil", streamErr)
	}
	if len(chunk) != 0 {
		t.Fatalf("chunk = %q, want empty", chunk)
	}
	if errMsg != want {
		t.Fatalf("error message = %#v, want %#v", errMsg, want)
	}
}

func TestReceiveInitialResponsesStreamReportsCleanEmptyStream(t *testing.T) {
	data := make(chan []byte)
	errs := make(chan *handlers.ErrorMessage)
	close(data)
	close(errs)

	_, errMsg, streamErr := receiveInitialResponsesStream(context.Background(), data, errs)
	if errMsg != nil {
		t.Fatalf("error message = %#v, want nil", errMsg)
	}
	if streamErr == nil || !strings.Contains(streamErr.Error(), "before response.completed") {
		t.Fatalf("stream error = %v, want incomplete stream", streamErr)
	}
}

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
	errs := make(chan *handlers.ErrorMessage, 1)
	errs <- &handlers.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New("unexpected EOF")}
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
	data <- []byte("event: response.completed\ndata: {\"type\":")
	data <- []byte("\"response.completed\"}\n\n")
	close(data)
	errs := make(chan *handlers.ErrorMessage)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, nil, data, errs)
	body := recorder.Body.String()
	if strings.Contains(body, "data: {\"type\":\n\n") {
		t.Fatalf("malformed partial SSE frame was forwarded: %q", body)
	}
	if !strings.Contains(body, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n") {
		t.Fatalf("expected completed SSE frame, got: %q", body)
	}
}

func TestForwardResponsesStreamRejectsMalformedSSEAtCleanClose(t *testing.T) {
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

	data := make(chan []byte, 1)
	data <- []byte("event: response.completed\ndata: {\"type\":")
	close(data)
	errs := make(chan *handlers.ErrorMessage)
	close(errs)

	var cancelledWith error
	h.forwardResponsesStream(c, flusher, func(err error) { cancelledWith = err }, nil, data, errs)
	body := recorder.Body.String()
	if strings.Contains(body, "data: {\"type\":\n\n") {
		t.Fatalf("malformed terminal SSE frame was forwarded: %q", body)
	}
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "malformed data") {
		t.Fatalf("expected malformed stream error chunk, got: %q", body)
	}
	if cancelledWith == nil || !strings.Contains(cancelledWith.Error(), "malformed data") {
		t.Fatalf("cancel error = %v, want malformed data", cancelledWith)
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
	data <- []byte("event: response.completed\n")
	data <- []byte("data: {\"type\":\"response.completed\"}\n\n")
	close(data)
	errs := make(chan *handlers.ErrorMessage)
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, nil, data, errs)
	body := recorder.Body.String()
	if strings.Contains(body, "event: response.completed\n\ndata:") {
		t.Fatalf("fragmented event and data fields were split into separate SSE frames: %q", body)
	}
	if !strings.Contains(body, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n") {
		t.Fatalf("expected complete SSE frame, got: %q", body)
	}
}

func TestForwardResponsesStreamAcceptsMultilineDataJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewResponsesAPIHandler(base)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatal("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte, 1)
	data <- []byte("event: response.completed\ndata: {\"type\":\ndata: \"response.completed\"}\n\n")
	close(data)
	errs := make(chan *handlers.ErrorMessage)
	close(errs)
	var cancelledWith error
	h.forwardResponsesStream(c, flusher, func(err error) { cancelledWith = err }, nil, data, errs)

	if cancelledWith != nil {
		t.Fatalf("cancel error = %v, want nil", cancelledWith)
	}
	if !strings.Contains(recorder.Body.String(), "data: {\"type\":\ndata: \"response.completed\"}") {
		t.Fatalf("multiline SSE frame was not preserved: %q", recorder.Body.String())
	}
}

func TestForwardResponsesStreamRejectsInvalidCombinedMultilineData(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewResponsesAPIHandler(base)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatal("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte, 1)
	data <- []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\ndata: {}\n\n")
	close(data)
	errs := make(chan *handlers.ErrorMessage)
	close(errs)
	var cancelledWith error
	h.forwardResponsesStream(c, flusher, func(err error) { cancelledWith = err }, nil, data, errs)

	if cancelledWith == nil || !strings.Contains(cancelledWith.Error(), "malformed data") {
		t.Fatalf("cancel error = %v, want malformed data", cancelledWith)
	}
	if strings.Contains(recorder.Body.String(), "data: {\"type\":\"response.completed\"}\ndata: {}") {
		t.Fatalf("invalid multiline SSE frame was forwarded: %q", recorder.Body.String())
	}
}

func TestForwardResponsesStreamRejectsMultilineDataSplitAcrossChunks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewResponsesAPIHandler(base)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatal("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte, 2)
	data <- []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}")
	data <- []byte("\ndata: {}\n\n")
	close(data)
	errs := make(chan *handlers.ErrorMessage)
	close(errs)
	var cancelledWith error
	h.forwardResponsesStream(c, flusher, func(err error) { cancelledWith = err }, nil, data, errs)

	if cancelledWith == nil || !strings.Contains(cancelledWith.Error(), "malformed data") {
		t.Fatalf("cancel error = %v, want malformed data", cancelledWith)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "event: response.completed") {
		t.Fatalf("split multiline SSE event was forwarded before validation: %q", body)
	}
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("expected Responses SSE error, got %q", body)
	}
}

func TestForwardResponsesStreamPreservesChunkStartingWithDataLiteral(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewResponsesAPIHandler(base)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatal("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte, 2)
	data <- []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output_text\":\"")
	data <- []byte("data:literal\"}}\n\n")
	close(data)
	errs := make(chan *handlers.ErrorMessage)
	close(errs)
	var cancelledWith error
	h.forwardResponsesStream(c, flusher, func(err error) { cancelledWith = err }, nil, data, errs)

	if cancelledWith != nil {
		t.Fatalf("cancel error = %v, want nil", cancelledWith)
	}
	if !strings.Contains(recorder.Body.String(), `"output_text":"data:literal"`) {
		t.Fatalf("chunk boundary changed JSON payload: %q", recorder.Body.String())
	}
}

func TestForwardResponsesStreamLateErrorSuppressesTerminalSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewResponsesAPIHandler(base)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatal("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte, 1)
	data <- []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
	close(data)
	errs := make(chan *handlers.ErrorMessage, 1)
	errs <- &handlers.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New("late upstream failure")}
	close(errs)

	var cancelledWith error
	h.forwardResponsesStream(c, flusher, func(err error) { cancelledWith = err }, nil, data, errs)
	body := recorder.Body.String()
	if strings.Contains(body, "response.completed") {
		t.Fatalf("terminal success was emitted before late error: %q", body)
	}
	if !strings.Contains(body, "late upstream failure") || !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("expected terminal Responses error, got %q", body)
	}
	if cancelledWith == nil || !strings.Contains(cancelledWith.Error(), "late upstream failure") {
		t.Fatalf("cancel error = %v, want late upstream failure", cancelledWith)
	}
}

func TestResponsesHandlerReturnsSSEErrorForMalformedCleanClose(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name                string
		payload             string
		closeWithoutPayload bool
	}{
		{name: "zero chunks", closeWithoutPayload: true},
		{name: "empty stream", payload: ""},
		{name: "comment only", payload: ": keep-alive\n\n"},
		{name: "created without completion", payload: "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"},
		{name: "delimiterless completion", payload: "event: response.completed\ndata: {\"type\":\"response.completed\"}"},
		{name: "truncated JSON", payload: "event: response.completed\ndata: {\"type\":"},
		{name: "empty data", payload: "event: response.completed\ndata:\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &responsesStreamTestExecutor{
				payload:             []byte(tt.payload),
				closeWithoutPayload: tt.closeWithoutPayload,
			}
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(executor)
			model := "responses-stream-" + strings.ReplaceAll(tt.name, " ", "-")
			auth := &coreauth.Auth{
				ID:       model + "-auth",
				Provider: executor.Identifier(),
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{"type": "test"},
			}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatalf("register auth: %v", err)
			}
			registry.GetGlobalRegistry().RegisterClient(
				auth.ID,
				auth.Provider,
				[]*registry.ModelInfo{{ID: model}},
			)
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })

			base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
			h := NewResponsesAPIHandler(base)
			router := gin.New()
			router.POST("/v1/responses", h.Responses)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/responses",
				strings.NewReader(`{"model":"`+model+`","stream":true}`),
			)
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)

			if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
				t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
			}
			body := recorder.Body.String()
			if !strings.Contains(body, "event: error") || !strings.Contains(body, `"type":"error"`) {
				t.Fatalf("expected terminal Responses SSE error, got %q", body)
			}
			if strings.Contains(body, `{"error":{`) {
				t.Fatalf("received plain HTTP error body instead of Responses SSE error: %q", body)
			}
		})
	}
}
