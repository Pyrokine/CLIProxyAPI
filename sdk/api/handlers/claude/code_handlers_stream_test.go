package claude

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type claudeHandlerSSEEvent struct {
	name string
	data gjson.Result
}

type claudeEmptyStreamExecutor struct{}

func (*claudeEmptyStreamExecutor) Identifier() string { return "claude-empty-stream-test" }

func (*claudeEmptyStreamExecutor) Execute(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (*claudeEmptyStreamExecutor) ExecuteStream(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk)
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (*claudeEmptyStreamExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (*claudeEmptyStreamExecutor) CountTokens(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (*claudeEmptyStreamExecutor) HttpRequest(
	context.Context,
	*coreauth.Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestClaudeHandlerRejectsEmptyStreamBootstrapWithoutMessageStop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	model := "claude-empty-stream-bootstrap"
	executor := &claudeEmptyStreamExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       model + "-auth",
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
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
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/messages", handler.ClaudeMessages)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"`+model+`","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Content-Type", "application/json")

	router.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if strings.Contains(body, "event: message_stop") || !strings.Contains(body, `"error"`) {
		t.Fatalf("body = %q, want bootstrap error without message_stop", body)
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}

func runClaudeForwardStream(t *testing.T, chunk string) []claudeHandlerSSEEvent {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	data := make(chan []byte)
	errs := make(chan *handlers.ErrorMessage)
	go func() {
		data <- []byte(chunk)
		errs <- &handlers.ErrorMessage{StatusCode: 500, Error: errors.New("transport failed")}
	}()

	handler.forwardClaudeStream(ctx, recorder, func(error) {}, data, errs)
	return parseClaudeHandlerSSEEvents(t, recorder.Body.String())
}

func parseClaudeHandlerSSEEvents(t *testing.T, stream string) []claudeHandlerSSEEvent {
	t.Helper()
	blocks := strings.Split(strings.TrimSpace(stream), "\n\n")
	events := make([]claudeHandlerSSEEvent, 0, len(blocks))
	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			t.Fatalf("invalid SSE block %q", block)
		}
		data := strings.TrimPrefix(lines[1], "data: ")
		if !gjson.Valid(data) {
			t.Fatalf("invalid SSE JSON %q", data)
		}
		events = append(
			events, claudeHandlerSSEEvent{
				name: strings.TrimPrefix(lines[0], "event: "),
				data: gjson.Parse(data),
			},
		)
	}
	return events
}

func assertClaudeHandlerSSELifecycle(t *testing.T, events []claudeHandlerSSEEvent) {
	t.Helper()
	open := make(map[int]bool)
	messageStarted := false
	messageDeltaSeen := false
	terminalSeen := false
	for eventIndex, event := range events {
		if terminalSeen {
			t.Fatalf("event %d (%s) arrived after terminal event", eventIndex, event.name)
		}
		if event.data.Get("type").String() != event.name {
			t.Fatalf(
				"event %d name %q does not match payload type %q", eventIndex, event.name,
				event.data.Get("type").String(),
			)
		}
		switch event.name {
		case "message_start":
			if messageStarted {
				t.Fatalf("event %d emitted duplicate message_start", eventIndex)
			}
			messageStarted = true
		case "content_block_start":
			if !messageStarted {
				t.Fatalf("event %d started content before message_start", eventIndex)
			}
			index := int(event.data.Get("index").Int())
			if open[index] {
				t.Fatalf("event %d started block %d twice", eventIndex, index)
			}
			open[index] = true
		case "content_block_stop":
			index := int(event.data.Get("index").Int())
			if !open[index] {
				t.Fatalf("event %d stopped block %d without an open start", eventIndex, index)
			}
			open[index] = false
		case "message_delta":
			messageDeltaSeen = true
		case "message_stop":
			if !messageDeltaSeen {
				t.Fatalf("event %d emitted message_stop without message_delta", eventIndex)
			}
			terminalSeen = true
		case "error":
			terminalSeen = true
		}
	}
	for index, isOpen := range open {
		if isOpen {
			t.Fatalf("block %d remained open at end of stream", index)
		}
	}
}

func TestForwardClaudeStream_TransportErrorClosesOpenBlocks(t *testing.T) {
	events := runClaudeForwardStream(
		t,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":3,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
	)

	assertClaudeHandlerSSELifecycle(t, events)
	if len(events) != 4 || events[2].name != "content_block_stop" || events[2].data.Get("index").Int() != 3 || events[3].name != "error" {
		t.Fatalf("terminal events = %#v, want block 3 stop followed by error", events)
	}
}

func TestForwardClaudeStream_TransportErrorAfterMessageDeltaFinishesNormally(t *testing.T) {
	events := runClaudeForwardStream(
		t,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{}}\n\n",
	)

	assertClaudeHandlerSSELifecycle(t, events)
	if len(events) != 3 || events[2].name != "message_stop" {
		t.Fatalf("terminal events = %#v, want message_stop without error", events)
	}
}

func TestWriteClaudeStreamKeepAliveWritesAnthropicPing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	timeline := internallogging.NewRequestTimeline()
	ctx.Set(internallogging.RequestTimelineGinKey, timeline)
	telemetry := &claudeHeartbeatTelemetry{}

	result := writeClaudeStreamKeepAlive(ctx, telemetry)
	if result.Err != nil || !result.Written {
		t.Fatalf("heartbeat result = %+v, want successful write", result)
	}
	want := "event: ping\ndata: {\"type\":\"ping\"}\n\n"
	if recorder.Body.String() != want {
		t.Fatalf("heartbeat = %q, want %q", recorder.Body.String(), want)
	}
	telemetry.summary(ctx)
	var timelineBody bytes.Buffer
	if err := internallogging.WriteRequestTimeline(&timelineBody, timeline.Snapshot()); err != nil {
		t.Fatalf("write timeline: %v", err)
	}
	if !strings.Contains(timelineBody.String(), "client.stream.heartbeat.write") ||
		!strings.Contains(timelineBody.String(), "client.stream.heartbeat.summary") {
		t.Fatalf("timeline = %q, want heartbeat write and summary", timelineBody.String())
	}
}

func TestForwardClaudeStream_TransportErrorAfterMessageStopIsSuppressed(t *testing.T) {
	events := runClaudeForwardStream(
		t,
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	)

	assertClaudeHandlerSSELifecycle(t, events)
	if len(events) != 3 {
		t.Fatalf("events = %#v, want transport error suppressed after message_stop", events)
	}
}

type claudeStreamDeliveryWriter struct {
	gin.ResponseWriter
	events *[]string
}

func (w *claudeStreamDeliveryWriter) Write(payload []byte) (int, error) {
	*w.events = append(*w.events, "write")
	return w.ResponseWriter.Write(payload)
}

func (w *claudeStreamDeliveryWriter) FlushError() error {
	*w.events = append(*w.events, "flush")
	return nil
}

func TestForwardClaudeStreamChunksValidatesProtocolTerminal(t *testing.T) {
	tests := []struct {
		name             string
		payload          string
		wantAckError     string
		wantTerminalBody string
	}{
		{
			name:             "missing protocol terminal",
			payload:          "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n",
			wantAckError:     "before message_stop",
			wantTerminalBody: "event: error\n",
		},
		{
			name: "complete message delta synthesizes message stop",
			payload: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{}}\n\n",
			wantTerminalBody: "event: message_stop\n",
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				recorder := httptest.NewRecorder()
				ginCtx, _ := gin.CreateTestContext(recorder)
				ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				data := make(chan coreexecutor.StreamChunk, 2)
				var payloadAckErr error
				data <- coreexecutor.StreamChunk{
					Payload: []byte(test.payload),
					Acknowledge: func(err error) {
						payloadAckErr = err
					},
				}
				var terminalAckErr error
				terminalAckCalled := false
				data <- coreexecutor.StreamChunk{
					Terminal: true,
					Acknowledge: func(err error) {
						terminalAckCalled = true
						terminalAckErr = err
					},
				}
				close(data)
				var canceledWith error
				handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))

				handler.forwardClaudeStreamChunksWithLifecycle(
					ginCtx,
					ginCtx.Writer,
					func(err error) { canceledWith = err },
					data,
					nil,
					newClaudeStreamLifecycle(),
					&claudeHeartbeatTelemetry{},
					0,
				)

				if payloadAckErr != nil {
					t.Fatalf("payload acknowledgement error = %v", payloadAckErr)
				}
				if !terminalAckCalled {
					t.Fatal("terminal acknowledgement was not called")
				}
				if test.wantAckError == "" {
					if terminalAckErr != nil || canceledWith != nil {
						t.Fatalf(
							"terminal acknowledgement/cancel errors = %v/%v, want nil", terminalAckErr, canceledWith,
						)
					}
				} else {
					if terminalAckErr == nil || !strings.Contains(terminalAckErr.Error(), test.wantAckError) {
						t.Fatalf("terminal acknowledgement error = %v, want %q", terminalAckErr, test.wantAckError)
					}
					if canceledWith == nil || !strings.Contains(canceledWith.Error(), test.wantAckError) {
						t.Fatalf("cancel error = %v, want %q", canceledWith, test.wantAckError)
					}
				}
				if !strings.Contains(recorder.Body.String(), test.wantTerminalBody) {
					t.Fatalf("response body = %q, want %q", recorder.Body.String(), test.wantTerminalBody)
				}
			},
		)
	}
}

func TestForwardClaudeStreamChunksRejectsMissingTransportTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	data := make(chan coreexecutor.StreamChunk)
	close(data)
	var canceledWith error
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))

	handler.forwardClaudeStreamChunksWithLifecycle(
		ginCtx,
		ginCtx.Writer,
		func(err error) { canceledWith = err },
		data,
		nil,
		newClaudeStreamLifecycle(),
		&claudeHeartbeatTelemetry{},
		0,
	)

	if canceledWith == nil || !strings.Contains(canceledWith.Error(), "before transport terminal") {
		t.Fatalf("cancel error = %v, want missing transport terminal", canceledWith)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "event: message_stop") || !strings.Contains(body, "event: error") {
		t.Fatalf("body = %q, want Claude error without message_stop", body)
	}
}

func TestForwardClaudeStreamChunksDoesNotAppendErrorAfterMessageStop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	data := make(chan coreexecutor.StreamChunk, 1)
	data <- coreexecutor.StreamChunk{
		Payload: []byte(
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		),
	}
	close(data)
	var canceledWith error
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))

	handler.forwardClaudeStreamChunksWithLifecycle(
		ginCtx,
		ginCtx.Writer,
		func(err error) { canceledWith = err },
		data,
		nil,
		newClaudeStreamLifecycle(),
		&claudeHeartbeatTelemetry{},
		0,
	)

	if !strings.Contains(recorder.Body.String(), "event: message_stop") ||
		strings.Contains(recorder.Body.String(), "event: error") {
		t.Fatalf("body = %q, want message_stop without an appended error", recorder.Body.String())
	}
	if canceledWith == nil || !strings.Contains(canceledWith.Error(), "before transport terminal") {
		t.Fatalf("cancel error = %v, want missing transport terminal", canceledWith)
	}
}

func TestForwardClaudeStreamChunksPropagatesCancelAndWriteFailure(t *testing.T) {
	t.Run(
		"client cancel", func(t *testing.T) {
			requestContext, cancelRequest := context.WithCancel(context.Background())
			cancelRequest()
			recorder := httptest.NewRecorder()
			ginContext, _ := gin.CreateTestContext(recorder)
			ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(requestContext)
			var canceledWith error
			handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))

			handler.forwardClaudeStreamChunksWithLifecycle(
				ginContext,
				ginContext.Writer,
				func(err error) { canceledWith = err },
				make(chan coreexecutor.StreamChunk),
				nil,
				newClaudeStreamLifecycle(),
				&claudeHeartbeatTelemetry{},
				0,
			)

			if !errors.Is(canceledWith, context.Canceled) {
				t.Fatalf("cancel error = %v, want context canceled", canceledWith)
			}
			if strings.Contains(recorder.Body.String(), "event: message_stop") {
				t.Fatalf("body = %q, want no message_stop", recorder.Body.String())
			}
		},
	)

	t.Run(
		"write failure", func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ginContext, _ := gin.CreateTestContext(recorder)
			ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			writeErr := errors.New("claude stream write failed")
			writer := &claudeCountingResponseWriter{ResponseWriter: ginContext.Writer, err: writeErr}
			ginContext.Writer = writer
			data := make(chan coreexecutor.StreamChunk, 2)
			var acknowledgedWith error
			data <- coreexecutor.StreamChunk{
				Payload: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n"),
				Acknowledge: func(err error) {
					acknowledgedWith = err
				},
			}
			data <- coreexecutor.StreamChunk{Terminal: true}
			close(data)
			var canceledWith error
			handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))

			handler.forwardClaudeStreamChunksWithLifecycle(
				ginContext,
				writer,
				func(err error) { canceledWith = err },
				data,
				nil,
				newClaudeStreamLifecycle(),
				&claudeHeartbeatTelemetry{},
				0,
			)

			if !errors.Is(acknowledgedWith, writeErr) || !errors.Is(canceledWith, writeErr) {
				t.Fatalf("acknowledgement/cancel errors = %v/%v, want %v", acknowledgedWith, canceledWith, writeErr)
			}
		},
	)
}

func TestForwardClaudeStreamChunksAcknowledgesProtocolWrites(t *testing.T) {
	gin.SetMode(gin.TestMode)
	events := make([]string, 0, 5)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Writer = &claudeStreamDeliveryWriter{ResponseWriter: ginCtx.Writer, events: &events}
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	data := make(chan coreexecutor.StreamChunk, 2)
	data <- coreexecutor.StreamChunk{
		Payload: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("payload acknowledgement error = %v", err)
			}
			events = append(events, "payload_ack")
		},
	}
	data <- coreexecutor.StreamChunk{
		Terminal: true,
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("terminal acknowledgement error = %v", err)
			}
			events = append(events, "terminal_ack")
		},
	}
	close(data)
	var canceledWith error
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))

	handler.forwardClaudeStreamChunksWithLifecycle(
		ginCtx,
		ginCtx.Writer,
		func(err error) { canceledWith = err },
		data,
		nil,
		newClaudeStreamLifecycle(),
		&claudeHeartbeatTelemetry{},
		0,
	)

	if canceledWith != nil {
		t.Fatalf("cancel error = %v, want nil", canceledWith)
	}
	want := []string{"write", "flush", "payload_ack", "flush", "terminal_ack"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

type claudeCountingResponseWriter struct {
	gin.ResponseWriter
	err     error
	writes  int
	flushes int
}

func (w *claudeCountingResponseWriter) Write(payload []byte) (int, error) {
	w.writes++
	if w.err != nil {
		return 0, w.err
	}
	return w.ResponseWriter.Write(payload)
}

func (w *claudeCountingResponseWriter) Flush() {
	w.flushes++
	w.ResponseWriter.Flush()
}

func newClaudeHeartbeatTestContext(
	t *testing.T,
) (*gin.Context, *internallogging.RequestTimeline, context.CancelFunc, *httptest.ResponseRecorder) {
	t.Helper()
	requestContext, cancel := context.WithCancel(context.Background())
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("POST", "/v1/messages", nil).WithContext(requestContext)
	timeline := internallogging.NewRequestTimeline()
	ctx.Set(internallogging.RequestTimelineGinKey, timeline)
	return ctx, timeline, cancel, recorder
}

func waitForClaudeStreamingResponse(
	t *testing.T,
	ctx *gin.Context,
	start <-chan claudeStreamStart,
	interval time.Duration,
	cancel context.CancelFunc,
) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	go func() {
		handler.forwardClaudeStreamingResponse(ctx, ctx.Writer, func(...any) {}, start, interval)
		close(done)
	}()
	t.Cleanup(
		func() {
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("Claude streaming response did not stop")
			}
		},
	)
	return done
}

func waitForClaudeStreamStop(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Claude streaming response did not stop")
	}
}

func countClaudeTimelineEvents(events []internallogging.RequestTimelineEvent, name string) int {
	count := 0
	for _, event := range events {
		if event.Name == name {
			count++
		}
	}
	return count
}

func TestClaudeStreamingHeartbeatRunsBeforeUpstreamStarts(t *testing.T) {
	ctx, timeline, cancel, recorder := newClaudeHeartbeatTestContext(t)
	start := make(chan claudeStreamStart)
	done := waitForClaudeStreamingResponse(t, ctx, start, time.Millisecond, cancel)
	time.Sleep(15 * time.Millisecond)
	cancel()
	waitForClaudeStreamStop(t, done)

	if err := waitForClaudeTimelineEvent(t, timeline, "client.stream.heartbeat.write"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.Body.String(), "event: ping\ndata: {\"type\":\"ping\"}") {
		t.Fatalf("body = %q, want Anthropic ping before upstream starts", recorder.Body.String())
	}
}

func TestClaudeStreamingHeartbeatRunsAfterUpstreamStartsBeforeFirstChunk(t *testing.T) {
	ctx, timeline, cancel, recorder := newClaudeHeartbeatTestContext(t)
	start := make(chan claudeStreamStart, 1)
	start <- claudeStreamStart{data: make(chan coreexecutor.StreamChunk), errs: make(chan *handlers.ErrorMessage)}
	done := waitForClaudeStreamingResponse(t, ctx, start, time.Millisecond, cancel)
	time.Sleep(15 * time.Millisecond)
	cancel()
	waitForClaudeStreamStop(t, done)

	if err := waitForClaudeTimelineEvent(t, timeline, "client.stream.heartbeat.write"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.Body.String(), "event: ping\ndata: {\"type\":\"ping\"}") {
		t.Fatalf("body = %q, want Anthropic ping after upstream starts", recorder.Body.String())
	}
}

func TestClaudeStreamingHeartbeatRunsAfterFirstChunk(t *testing.T) {
	ctx, timeline, cancel, recorder := newClaudeHeartbeatTestContext(t)
	start := make(chan claudeStreamStart, 1)
	data := make(chan coreexecutor.StreamChunk, 1)
	data <- coreexecutor.StreamChunk{
		Payload: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n"),
	}
	start <- claudeStreamStart{data: data}
	done := waitForClaudeStreamingResponse(t, ctx, start, time.Millisecond, cancel)
	time.Sleep(15 * time.Millisecond)
	cancel()
	waitForClaudeStreamStop(t, done)

	if err := waitForClaudeTimelineEvent(t, timeline, "client.stream.heartbeat.write"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.Body.String(), "event: ping\ndata: {\"type\":\"ping\"}") {
		t.Fatalf("body = %q, want Anthropic ping after first chunk", recorder.Body.String())
	}
}

func readClaudeNetworkSSEBlock(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var block strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE block: %v", err)
		}
		block.WriteString(line)
		if line == "\n" {
			return block.String()
		}
	}
}

func TestClaudeStreamingHeartbeatFlushesAcrossAllWaitingPhases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	start := make(chan claudeStreamStart, 1)
	data := make(chan coreexecutor.StreamChunk, 1)
	errs := make(chan *handlers.ErrorMessage)
	handlerDone := make(chan struct{})
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	router := gin.New()
	router.GET(
		"/v1/messages", func(c *gin.Context) {
			defer close(handlerDone)
			handler.forwardClaudeStreamingResponse(
				c, c.Writer, func(...any) {}, start, 10*time.Millisecond,
			)
		},
	)
	server := httptest.NewServer(router)
	defer server.Close()

	requestContext, cancelRequest := context.WithCancel(context.Background())
	request, errRequest := http.NewRequestWithContext(
		requestContext, http.MethodGet, server.URL+"/v1/messages", nil,
	)
	if errRequest != nil {
		t.Fatalf("create request: %v", errRequest)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, errResponse := client.Do(request)
	if errResponse != nil {
		t.Fatalf("start stream: %v", errResponse)
	}
	defer response.Body.Close()
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-cache, no-transform" {
		t.Fatalf("Cache-Control = %q, want no-cache, no-transform", got)
	}
	if got := response.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}
	reader := bufio.NewReader(response.Body)
	if block := readClaudeNetworkSSEBlock(t, reader); !strings.Contains(block, "event: ping") {
		t.Fatalf("setup wait block = %q, want ping", block)
	}

	start <- claudeStreamStart{data: data, errs: errs}
	if block := readClaudeNetworkSSEBlock(t, reader); !strings.Contains(block, "event: ping") {
		t.Fatalf("first-chunk wait block = %q, want ping", block)
	}

	data <- coreexecutor.StreamChunk{
		Payload: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n"),
	}
	for {
		block := readClaudeNetworkSSEBlock(t, reader)
		if strings.Contains(block, "event: message_start") {
			break
		}
		if !strings.Contains(block, "event: ping") {
			t.Fatalf("unexpected pre-chunk SSE block %q", block)
		}
	}
	if block := readClaudeNetworkSSEBlock(t, reader); !strings.Contains(block, "event: ping") {
		t.Fatalf("ordinary stream wait block = %q, want ping", block)
	}

	cancelRequest()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("stream handler did not stop after client cancellation")
	}
}

func TestClaudeStreamingHeartbeatSuppressedAfterTerminalEvent(t *testing.T) {
	ctx, timeline, cancel, recorder := newClaudeHeartbeatTestContext(t)
	data := make(chan []byte, 1)
	data <- []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	telemetry := &claudeHeartbeatTelemetry{}
	defer telemetry.summary(ctx)
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	done := make(chan struct{})
	go func() {
		handler.forwardClaudeStreamWithLifecycle(
			ctx, ctx.Writer, func(error) {}, data, nil, newClaudeStreamLifecycle(), telemetry, time.Millisecond,
		)
		close(done)
	}()
	time.Sleep(15 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Claude stream did not stop after terminal event")
	}
	telemetry.summary(ctx)

	if strings.Contains(recorder.Body.String(), "event: ping") {
		t.Fatalf("body = %q, want no ping after terminal event", recorder.Body.String())
	}
	var summary string
	for _, event := range timeline.Snapshot() {
		if event.Name == "client.stream.heartbeat.summary" {
			summary = event.Detail
		}
	}
	if !strings.Contains(summary, "suppressed=") || !strings.Contains(summary, "flushes=0") {
		t.Fatalf("summary = %q, want suppressed heartbeat and no heartbeat flush", summary)
	}
}

func TestClaudeHeartbeatZeroWriteDoesNotFlush(t *testing.T) {
	ctx, _, cancel, _ := newClaudeHeartbeatTestContext(t)
	defer cancel()
	writer := &claudeCountingResponseWriter{ResponseWriter: ctx.Writer}
	ctx.Writer = writer
	if errFlush := flushClaudeStreamKeepAlive(
		writer, ctx, &claudeHeartbeatTelemetry{}, handlers.KeepAliveWriteResult{},
	); errFlush != nil {
		t.Fatalf("flush heartbeat: %v", errFlush)
	}
	if writer.flushes != 0 {
		t.Fatalf("flushes = %d, want zero for an empty heartbeat write", writer.flushes)
	}
}

func TestClaudeHeartbeatTimelineIsBoundedAndSummaryCountsAttempts(t *testing.T) {
	ctx, timeline, cancel, recorder := newClaudeHeartbeatTestContext(t)
	telemetry := &claudeHeartbeatTelemetry{}
	for range 12 {
		result := writeClaudeStreamKeepAlive(ctx, telemetry)
		if errFlush := flushClaudeStreamKeepAlive(recorder, ctx, telemetry, result); errFlush != nil {
			t.Fatalf("flush heartbeat: %v", errFlush)
		}
	}
	telemetry.summary(ctx)

	events := timeline.Snapshot()
	if count := countClaudeTimelineEvents(events, "client.stream.heartbeat.write"); count > claudeHeartbeatDetailLimit {
		t.Fatalf("heartbeat write details = %d, want at most %d", count, claudeHeartbeatDetailLimit)
	}
	if count := countClaudeTimelineEvents(events, "client.stream.heartbeat.flush"); count > claudeHeartbeatDetailLimit {
		t.Fatalf("heartbeat flush details = %d, want at most %d", count, claudeHeartbeatDetailLimit)
	}
	if count := countClaudeTimelineEvents(events, "client.stream.heartbeat.summary"); count != 1 {
		t.Fatalf("heartbeat summaries = %d, want one", count)
	}
	if !strings.Contains(events[len(events)-1].Detail, "attempts=12") ||
		!strings.Contains(events[len(events)-1].Detail, "written=12") ||
		!strings.Contains(events[len(events)-1].Detail, "flushes=12") {
		t.Fatalf("summary = %q, want all 12 attempts, writes, and flushes", events[len(events)-1].Detail)
	}
	cancel()
}

func TestClaudeHeartbeatWriteFailureCancelsWithoutFlush(t *testing.T) {
	ctx, timeline, cancel, _ := newClaudeHeartbeatTestContext(t)
	writer := &claudeCountingResponseWriter{
		ResponseWriter: ctx.Writer, err: errors.New("claude heartbeat write failed"),
	}
	ctx.Writer = writer
	defer cancel()
	state := internallogging.NewRequestOutcomeState()
	ctx.Request = ctx.Request.WithContext(internallogging.WithRequestOutcomeState(ctx.Request.Context(), state))
	var canceledWith error
	telemetry := &claudeHeartbeatTelemetry{}
	handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	handler.forwardClaudeStreamWithLifecycle(
		ctx,
		writer,
		func(err error) {
			canceledWith = err
		},
		make(chan []byte),
		nil,
		newClaudeStreamLifecycle(),
		telemetry,
		time.Millisecond,
	)
	telemetry.summary(ctx)

	if !errors.Is(canceledWith, writer.err) {
		t.Fatalf("cancel error = %v, want %v", canceledWith, writer.err)
	}
	if writer.writes != 1 || writer.flushes != 0 {
		t.Fatalf("writer writes=%d flushes=%d, want one write and no flush", writer.writes, writer.flushes)
	}
	outcome := state.Snapshot()
	if !outcome.Failed || outcome.Kind != internallogging.RequestOutcomeStreamError || outcome.Reason != writer.err.Error() {
		t.Fatalf("request outcome = %+v, want Claude heartbeat failure", outcome)
	}
	if countClaudeTimelineEvents(timeline.Snapshot(), "client.stream.heartbeat.summary") != 1 {
		t.Fatal("heartbeat summary was not recorded")
	}
}

func waitForClaudeTimelineEvent(t *testing.T, timeline *internallogging.RequestTimeline, name string) error {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if countClaudeTimelineEvents(timeline.Snapshot(), name) > 0 {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return errors.New("timed out waiting for " + name)
}
