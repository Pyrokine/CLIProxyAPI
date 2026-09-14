package openai

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
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
	streamGate          <-chan struct{}
	streamStarted       chan<- struct{}
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
	ctx context.Context,
	_ *coreauth.Auth,
	_ coreexecutor.Request,
	_ coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 1)
	if e.streamStarted != nil {
		close(e.streamStarted)
	}
	publish := func() {
		if !e.closeWithoutPayload {
			chunks <- coreexecutor.StreamChunk{Payload: append([]byte(nil), e.payload...)}
		}
		close(chunks)
	}
	if e.streamGate == nil {
		publish()
		return &coreexecutor.StreamResult{Chunks: chunks}, nil
	}
	go func() {
		select {
		case <-e.streamGate:
			publish()
		case <-ctx.Done():
		}
	}()
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

type responsesHeartbeatRecorder struct {
	*httptest.ResponseRecorder
	heartbeat chan struct{}
	once      sync.Once
}

func newResponsesHeartbeatRecorder() *responsesHeartbeatRecorder {
	return &responsesHeartbeatRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		heartbeat:        make(chan struct{}),
	}
}

func (r *responsesHeartbeatRecorder) Write(payload []byte) (int, error) {
	if bytes.Contains(payload, []byte(": keep-alive\n\n")) {
		r.once.Do(func() { close(r.heartbeat) })
	}
	return r.ResponseRecorder.Write(payload)
}

func (r *responsesHeartbeatRecorder) Flush() {
	r.ResponseRecorder.Flush()
}

func newResponsesStreamTestRouter(
	t *testing.T,
	model string,
	executor *responsesStreamTestExecutor,
) *gin.Engine {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
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

	h := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/responses", h.Responses)
	return router
}

func newOpenAIChatStreamTestRouter(
	t *testing.T,
	model string,
	executor *responsesStreamTestExecutor,
) *gin.Engine {
	t.Helper()
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
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

	h := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/chat/completions", h.ChatCompletions)
	return router
}

func TestOpenAIChatHandlerRejectsEmptyStreamBootstrapWithoutDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	model := "openai-chat-empty-bootstrap"
	router := newOpenAIChatStreamTestRouter(t, model, &responsesStreamTestExecutor{closeWithoutPayload: true})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","stream":true}`),
	)
	request.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if strings.Contains(body, "[DONE]") || !strings.Contains(body, `"error"`) {
		t.Fatalf("body = %q, want bootstrap error without [DONE]", body)
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}

func TestReceiveInitialResponsesStreamPreservesBufferedErrorAfterDataClose(t *testing.T) {
	data := make(chan []byte)
	close(data)
	errs := make(chan *handlers.ErrorMessage, 1)
	want := &handlers.ErrorMessage{StatusCode: http.StatusTooManyRequests, Error: errors.New("quota exhausted")}
	errs <- want
	close(errs)

	chunk, errMsg, streamErr := receiveInitialResponsesStream(context.Background(), data, errs, nil, nil)
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

	_, errMsg, streamErr := receiveInitialResponsesStream(context.Background(), data, errs, nil, nil)
	if errMsg != nil {
		t.Fatalf("error message = %#v, want nil", errMsg)
	}
	if streamErr == nil || !strings.Contains(streamErr.Error(), "before response.completed") {
		t.Fatalf("stream error = %v, want incomplete stream", streamErr)
	}
}

func TestReceiveInitialResponsesStreamWritesKeepAliveBeforeFirstChunk(t *testing.T) {
	data := make(chan []byte, 1)
	errs := make(chan *handlers.ErrorMessage)
	keepAlive := make(chan time.Time, 1)
	keepAlive <- time.Now()
	want := []byte("event: response.created\ndata: {}\n\n")
	writes := 0

	chunk, errMsg, streamErr := receiveInitialResponsesStream(
		context.Background(),
		data,
		errs,
		keepAlive,
		func() error {
			writes++
			data <- want
			return nil
		},
	)
	if streamErr != nil || errMsg != nil {
		t.Fatalf("receive result = (%v, %v), want no error", errMsg, streamErr)
	}
	if writes != 1 {
		t.Fatalf("keep-alive writes = %d, want 1", writes)
	}
	if string(chunk) != string(want) {
		t.Fatalf("chunk = %q, want %q", chunk, want)
	}
}

func TestResponsesHandlerSendsKeepAliveWhileWaitingForFirstUpstreamChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	originalInterval := responsesStreamKeepAliveInterval
	responsesStreamKeepAliveInterval = 5 * time.Millisecond
	t.Cleanup(func() { responsesStreamKeepAliveInterval = originalInterval })

	gate := make(chan struct{})
	started := make(chan struct{})
	model := "responses-stream-delayed-first-chunk"
	executor := &responsesStreamTestExecutor{
		payload:       []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"),
		streamGate:    gate,
		streamStarted: started,
	}
	router := newResponsesStreamTestRouter(t, model, executor)
	recorder := newResponsesHeartbeatRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"`+model+`","stream":true}`),
	)
	request.Header.Set("Content-Type", "application/json")

	done := make(chan struct{})
	go func() {
		router.ServeHTTP(recorder, request)
		close(done)
	}()
	<-started

	select {
	case <-recorder.heartbeat:
	case <-time.After(time.Second):
		close(gate)
		<-done
		t.Fatal("keep-alive was not written while the upstream stream was waiting for its first chunk")
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Responses handler did not finish after the upstream terminal chunk")
	}

	body := recorder.Body.String()
	heartbeatIndex := strings.Index(body, ": keep-alive\n\n")
	completedIndex := strings.Index(body, "event: response.completed")
	if heartbeatIndex < 0 || completedIndex < 0 || heartbeatIndex >= completedIndex {
		t.Fatalf("stream order = %q, want keep-alive before response.completed", body)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
	}
}

func TestResponsesHandlerRecordsClientCancelBeforeFirstChunkAs499(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gate := make(chan struct{})
	started := make(chan struct{})
	model := "responses-stream-cancel-before-first-chunk"
	executor := &responsesStreamTestExecutor{
		streamGate:    gate,
		streamStarted: started,
	}
	router := newResponsesStreamTestRouter(t, model, executor)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"`+model+`","stream":true}`),
	)
	request.Header.Set("Content-Type", "application/json")
	requestContext, cancel := context.WithCancel(request.Context())
	request = request.WithContext(requestContext)

	done := make(chan struct{})
	go func() {
		router.ServeHTTP(recorder, request)
		close(done)
	}()
	<-started
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		close(gate)
		t.Fatal("Responses handler did not finish after the client canceled")
	}
	if recorder.Code != clienterror.StatusClientClosedRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, clienterror.StatusClientClosedRequest)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", recorder.Body.String())
	}
}

func TestForwardResponsesStreamExposesOnlyClientFaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name        string
		status      int
		message     string
		wantExposed bool
	}{
		{name: "bad request", status: http.StatusBadRequest, message: "bad request", wantExposed: true},
		{name: "conflict", status: http.StatusConflict, message: "conflict", wantExposed: true},
		{name: "message too big", status: http.StatusRequestEntityTooLarge, message: "too large", wantExposed: true},
		{name: "unprocessable", status: http.StatusUnprocessableEntity, message: "invalid", wantExposed: true},
		{
			name:        "cyber policy behind bad gateway",
			status:      http.StatusBadGateway,
			message:     `{"error":{"type":"invalid_request","code":"cyber_policy","message":"blocked"}}`,
			wantExposed: true,
		},
		{
			name:        "context length behind internal error",
			status:      http.StatusInternalServerError,
			message:     `{"response":{"error":{"type":"server_error","code":"context_length_exceeded"}}}`,
			wantExposed: true,
		},
		{
			name:        "invalid request type behind bad gateway",
			status:      http.StatusBadGateway,
			message:     `{"body":{"error":{"type":"invalid_request","message":"invalid"}}}`,
			wantExposed: true,
		},
		{name: "authentication", status: http.StatusUnauthorized, message: "invalid token"},
		{name: "quota", status: http.StatusTooManyRequests, message: "quota exhausted"},
		{name: "transport", status: http.StatusInternalServerError, message: "unexpected EOF"},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
				h := NewResponsesAPIHandler(base)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				flusher, ok := c.Writer.(http.Flusher)
				if !ok {
					t.Fatal("expected gin writer to implement http.Flusher")
				}

				data := make(chan []byte)
				errs := make(chan *handlers.ErrorMessage, 1)
				errs <- &handlers.ErrorMessage{StatusCode: test.status, Error: errors.New(test.message)}
				close(errs)

				h.forwardResponsesStream(c, flusher, func(error) {}, nil, data, errs)
				body := recorder.Body.String()
				exposed := strings.Contains(body, `"type":"error"`)
				if exposed != test.wantExposed {
					t.Fatalf("error exposed = %t, want %t: %q", exposed, test.wantExposed, body)
				}
				if exposed && strings.Contains(body, `"error":{`) {
					t.Fatalf("expected streaming error chunk, got HTTP error body: %q", body)
				}
			},
		)
	}
}

func TestForwardResponsesStreamUsesResponseFailedForCodex(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name   string
		header string
		value  string
	}{
		{name: "user agent", header: "User-Agent", value: "Codex Desktop/26.803.41515"},
		{name: "originator", header: "Originator", value: "codex-tui/0.145.0"},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
				h := NewResponsesAPIHandler(base)

				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				c.Request.Header.Set(tt.header, tt.value)

				flusher, ok := c.Writer.(http.Flusher)
				if !ok {
					t.Fatal("expected gin writer to implement http.Flusher")
				}

				data := make(chan []byte)
				errs := make(chan *handlers.ErrorMessage, 1)
				errs <- &handlers.ErrorMessage{
					StatusCode: http.StatusBadRequest,
					Error:      errors.New(`{"error":{"type":"invalid_request","code":"cyber_policy","message":"blocked"}}`),
				}
				close(errs)

				h.forwardResponsesStream(c, flusher, func(error) {}, nil, data, errs)
				body := recorder.Body.String()
				if !strings.Contains(body, "event: response.failed") || !strings.Contains(
					body, `"type":"response.failed"`,
				) {
					t.Fatalf("missing response.failed event: %q", body)
				}
				if strings.Contains(body, "event: error") {
					t.Fatalf("unexpected legacy error event for Codex: %q", body)
				}
				if !strings.Contains(body, `"type":"invalid_request"`) || !strings.Contains(
					body, `"code":"cyber_policy"`,
				) {
					t.Fatalf("missing nested Codex error detail: %q", body)
				}
			},
		)
	}
}

func TestResponsesStreamUsesGenericFailureForCodexServerError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Originator", "Codex Desktop")

	if errWrite := writeResponsesStreamTerminalError(
		c, &handlers.ErrorMessage{
			StatusCode: http.StatusInternalServerError,
			Error:      errors.New("upstream transport secret"),
		},
	); errWrite != nil {
		t.Fatalf("write terminal error: %v", errWrite)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: response.failed") || !strings.Contains(
		body, http.StatusText(http.StatusBadGateway),
	) {
		t.Fatalf("generic Codex failure = %q", body)
	}
	if strings.Contains(body, "upstream transport secret") {
		t.Fatalf("internal upstream error was exposed: %q", body)
	}
}

func TestResponsesSSEAccumulatorAcceptsIncompleteAndSuppressesPostTerminalFrames(t *testing.T) {
	acc := newResponsesSSEFrameAccumulator()
	frames := acc.AddChunk(
		[]byte(
			"event: response.incomplete\ndata: {\"type\":\"response.incomplete\"}\n\n" +
				"event: response.done\ndata: {\"type\":\"response.done\"}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n",
		),
	)

	if err := acc.Err(); err != nil {
		t.Fatalf("accumulator error = %v, want nil", err)
	}
	if len(frames) != 1 || !bytes.Contains(frames[0], []byte("response.incomplete")) {
		t.Fatalf("frames = %q, want only response.incomplete", frames)
	}
	if final := acc.Finalize(); len(final) != 0 {
		t.Fatalf("final frames = %q, want none", final)
	}
	if err := acc.Err(); err != nil {
		t.Fatalf("finalize error = %v, want nil", err)
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
	if strings.Contains(body, `"type":"error"`) || strings.Contains(body, "malformed data") {
		t.Fatalf("malformed upstream stream error was exposed: %q", body)
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
	if strings.Contains(body, `"type":"error"`) {
		t.Fatalf("malformed upstream stream error was exposed: %q", body)
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
	if strings.Contains(body, "late upstream failure") || strings.Contains(body, `"type":"error"`) {
		t.Fatalf("non-client terminal error was exposed: %q", body)
	}
	if cancelledWith == nil || !strings.Contains(cancelledWith.Error(), "late upstream failure") {
		t.Fatalf("cancel error = %v, want late upstream failure", cancelledWith)
	}
}

func TestResponsesHandlerHidesMalformedCleanCloseErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name                string
		payload             string
		closeWithoutPayload bool
	}{
		{name: "zero chunks", closeWithoutPayload: true},
		{name: "empty stream", payload: ""},
		{name: "comment only", payload: ": keep-alive\n\n"},
		{
			name:    "created without completion",
			payload: "event: response.created\ndata: {\"type\":\"response.created\"}\n\n",
		},
		{
			name:    "delimiterless completion",
			payload: "event: response.completed\ndata: {\"type\":\"response.completed\"}",
		},
		{name: "truncated JSON", payload: "event: response.completed\ndata: {\"type\":"},
		{name: "empty data", payload: "event: response.completed\ndata:\n\n"},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
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
				var outcomeState *internallogging.RequestOutcomeState
				router := gin.New()
				router.Use(internallogging.GinLogrusLogger())
				router.Use(
					func(c *gin.Context) {
						outcomeState = internallogging.RequestOutcomeStateFromContext(c.Request.Context())
						c.Next()
					},
				)
				router.POST("/v1/responses", h.Responses)
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(
					http.MethodPost,
					"/v1/responses",
					strings.NewReader(`{"model":"`+model+`","stream":true}`),
				)
				request.Header.Set("Content-Type", "application/json")
				router.ServeHTTP(recorder, request)

				if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(
					contentType, "text/event-stream",
				) {
					t.Fatalf("Content-Type = %q, want text/event-stream", contentType)
				}
				body := recorder.Body.String()
				if strings.Contains(body, "event: error") || strings.Contains(body, `"type":"error"`) {
					t.Fatalf("malformed upstream stream error was exposed: %q", body)
				}
				if strings.Contains(body, `{"error":{`) {
					t.Fatalf("received plain HTTP error body: %q", body)
				}
				if outcomeState == nil {
					t.Fatal("request outcome state was not attached")
				}
				outcome := outcomeState.Snapshot()
				if !outcome.Failed || outcome.Kind != internallogging.RequestOutcomeStreamError {
					t.Fatalf("request outcome = %+v, want stream failure", outcome)
				}
			},
		)
	}
}
