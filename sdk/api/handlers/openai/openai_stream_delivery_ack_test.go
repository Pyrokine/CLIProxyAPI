package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
)

func newOpenAIStreamDeliveryContext(events *[]string) *gin.Context {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Writer = &openAIStreamDeliveryWriter{ResponseWriter: ginCtx.Writer, events: events}
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return ginCtx
}

func TestForwardOpenAIStreamAcknowledgesProtocolWrites(t *testing.T) {
	events := make([]string, 0, 9)
	ginCtx := newOpenAIStreamDeliveryContext(&events)
	data := make(chan coreexecutor.StreamChunk, 2)
	data <- coreexecutor.StreamChunk{
		Payload: []byte(`{"id":"chatcmpl-test","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
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
	handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

	handler.handleStreamResult(
		ginCtx, ginCtx.Writer, func(err error) { canceledWith = err }, data, nil, nil,
	)

	if canceledWith != nil {
		t.Fatalf("cancel error = %v, want nil", canceledWith)
	}
	want := []string{
		"write", "write", "write", "flush", "payload_ack",
		"write", "flush", "terminal_ack",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestForwardOpenAIStreamRequiresFinishReasonBeforeDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	data := make(chan coreexecutor.StreamChunk, 2)
	data <- coreexecutor.StreamChunk{
		Payload: []byte(`{"id":"chatcmpl-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_weather","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":null}]}`),
	}
	var terminalAck error
	data <- coreexecutor.StreamChunk{
		Terminal: true,
		Acknowledge: func(err error) {
			terminalAck = err
		},
	}
	close(data)
	var canceledWith error
	handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

	handler.handleStreamResult(
		ginCtx, ginCtx.Writer, func(err error) { canceledWith = err }, data, nil, nil,
	)

	if terminalAck == nil || !strings.Contains(terminalAck.Error(), "finish_reason") {
		t.Fatalf("terminal acknowledgement error = %v, want missing finish_reason", terminalAck)
	}
	if canceledWith == nil || !strings.Contains(canceledWith.Error(), "finish_reason") {
		t.Fatalf("cancel error = %v, want missing finish_reason", canceledWith)
	}
	if strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("body = %q, want no [DONE] without finish_reason", recorder.Body.String())
	}
}

func TestForwardOpenAIStreamRequiresFinishReasonForEveryChoice(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	data := make(chan coreexecutor.StreamChunk, 2)
	data <- coreexecutor.StreamChunk{
		Payload: []byte(`{"id":"chatcmpl-test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"},{"index":1,"delta":{"content":"pending"},"finish_reason":null}]}`),
	}
	var terminalAck error
	data <- coreexecutor.StreamChunk{
		Terminal: true,
		Acknowledge: func(err error) {
			terminalAck = err
		},
	}
	close(data)
	var canceledWith error
	handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

	handler.handleStreamResult(
		ginCtx, ginCtx.Writer, func(err error) { canceledWith = err }, data, nil, nil,
	)

	if terminalAck == nil || !strings.Contains(terminalAck.Error(), "choice 1") {
		t.Fatalf("terminal acknowledgement error = %v, want unfinished choice 1", terminalAck)
	}
	if canceledWith == nil || !strings.Contains(canceledWith.Error(), "choice 1") {
		t.Fatalf("cancel error = %v, want unfinished choice 1", canceledWith)
	}
	if strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("body = %q, want no [DONE] while choice 1 is unfinished", recorder.Body.String())
	}
}

func TestForwardOpenAIStreamRejectsDuplicateFinishReason(t *testing.T) {
	ginCtx := newOpenAIStreamDeliveryContext(&[]string{})
	data := make(chan coreexecutor.StreamChunk, 3)
	data <- coreexecutor.StreamChunk{
		Payload: []byte(`{"id":"chatcmpl-test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
	}
	var duplicateAck error
	data <- coreexecutor.StreamChunk{
		Payload: []byte(`{"id":"chatcmpl-test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`),
		Acknowledge: func(err error) {
			duplicateAck = err
		},
	}
	data <- coreexecutor.StreamChunk{Terminal: true}
	close(data)
	var canceledWith error
	handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

	handler.handleStreamResult(
		ginCtx, ginCtx.Writer, func(err error) { canceledWith = err }, data, nil, nil,
	)

	if duplicateAck == nil || !strings.Contains(duplicateAck.Error(), "duplicate finish_reason") {
		t.Fatalf("duplicate acknowledgement error = %v", duplicateAck)
	}
	if canceledWith == nil || !strings.Contains(canceledWith.Error(), "duplicate finish_reason") {
		t.Fatalf("cancel error = %v", canceledWith)
	}
}

func TestForwardOpenAIStreamRejectsMissingTransportTerminal(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	data := make(chan coreexecutor.StreamChunk)
	close(data)
	var canceledWith error
	handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

	handler.handleStreamResult(
		ginCtx, ginCtx.Writer, func(err error) { canceledWith = err }, data, nil, nil,
	)

	if canceledWith == nil || !strings.Contains(canceledWith.Error(), "before transport terminal") {
		t.Fatalf("cancel error = %v, want missing transport terminal", canceledWith)
	}
	if body := recorder.Body.String(); strings.Contains(body, "[DONE]") || !strings.Contains(body, `"error"`) {
		t.Fatalf("body = %q, want stream error without [DONE]", body)
	}
}

func TestForwardOpenAIStreamPropagatesCancelAndWriteFailure(t *testing.T) {
	t.Run(
		"client cancel", func(t *testing.T) {
			requestContext, cancelRequest := context.WithCancel(context.Background())
			cancelRequest()
			recorder := httptest.NewRecorder()
			ginContext, _ := gin.CreateTestContext(recorder)
			ginContext.Request = httptest.NewRequest(
				http.MethodPost, "/v1/chat/completions", nil,
			).WithContext(requestContext)
			var canceledWith error
			handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

			handler.handleStreamResult(
				ginContext,
				ginContext.Writer,
				func(err error) { canceledWith = err },
				make(chan coreexecutor.StreamChunk),
				nil,
				nil,
			)

			if !errors.Is(canceledWith, context.Canceled) {
				t.Fatalf("cancel error = %v, want context canceled", canceledWith)
			}
			if strings.Contains(recorder.Body.String(), "[DONE]") {
				t.Fatalf("body = %q, want no success terminal", recorder.Body.String())
			}
		},
	)

	t.Run(
		"write failure", func(t *testing.T) {
			events := make([]string, 0, 2)
			ginContext := newOpenAIStreamDeliveryContext(&events)
			writeErr := errors.New("OpenAI stream write failed")
			ginContext.Writer.(*openAIStreamDeliveryWriter).writeErr = writeErr
			data := make(chan coreexecutor.StreamChunk, 2)
			var acknowledgedWith error
			data <- coreexecutor.StreamChunk{
				Payload: []byte(`{"id":"chatcmpl-test","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`),
				Acknowledge: func(err error) {
					acknowledgedWith = err
				},
			}
			data <- coreexecutor.StreamChunk{Terminal: true}
			close(data)
			var canceledWith error
			handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

			handler.handleStreamResult(
				ginContext, ginContext.Writer, func(err error) { canceledWith = err }, data, nil, nil,
			)

			if !errors.Is(acknowledgedWith, writeErr) || !errors.Is(canceledWith, writeErr) {
				t.Fatalf("acknowledgement/cancel errors = %v/%v, want %v", acknowledgedWith, canceledWith, writeErr)
			}
		},
	)
}

func TestForwardResponsesStreamRejectsMissingTransportTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("Originator", "codex-tui")
	data := make(chan coreexecutor.StreamChunk)
	close(data)
	var canceledWith error
	handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

	handler.forwardResponsesStreamChunks(
		ginCtx,
		ginCtx.Writer,
		func(err error) { canceledWith = err },
		newResponsesSSEFrameAccumulator(),
		data,
		nil,
	)

	if canceledWith == nil || !strings.Contains(canceledWith.Error(), "before transport terminal") {
		t.Fatalf("cancel error = %v, want missing transport terminal", canceledWith)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "response.completed") || !strings.Contains(body, "response.failed") {
		t.Fatalf("body = %q, want response.failed without completion", body)
	}
}

func TestForwardResponsesStreamPropagatesCancelAndWriteFailure(t *testing.T) {
	t.Run(
		"client cancel", func(t *testing.T) {
			requestContext, cancelRequest := context.WithCancel(context.Background())
			cancelRequest()
			recorder := httptest.NewRecorder()
			ginContext, _ := gin.CreateTestContext(recorder)
			ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)
			var canceledWith error
			handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

			handler.forwardResponsesStreamChunks(
				ginContext,
				ginContext.Writer,
				func(err error) { canceledWith = err },
				newResponsesSSEFrameAccumulator(),
				make(chan coreexecutor.StreamChunk),
				nil,
			)

			if !errors.Is(canceledWith, context.Canceled) {
				t.Fatalf("cancel error = %v, want context canceled", canceledWith)
			}
			if strings.Contains(recorder.Body.String(), "response.completed") {
				t.Fatalf("body = %q, want no completion", recorder.Body.String())
			}
		},
	)

	t.Run(
		"write failure", func(t *testing.T) {
			events := make([]string, 0, 2)
			ginContext := newOpenAIStreamDeliveryContext(&events)
			writeErr := errors.New("responses stream write failed")
			ginContext.Writer.(*openAIStreamDeliveryWriter).writeErr = writeErr
			data := make(chan coreexecutor.StreamChunk, 2)
			var acknowledgedWith error
			data <- coreexecutor.StreamChunk{
				Payload: []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"),
				Acknowledge: func(err error) {
					acknowledgedWith = err
				},
			}
			data <- coreexecutor.StreamChunk{Terminal: true}
			close(data)
			var canceledWith error
			handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

			handler.forwardResponsesStreamChunks(
				ginContext,
				ginContext.Writer,
				func(err error) { canceledWith = err },
				newResponsesSSEFrameAccumulator(),
				data,
				nil,
			)

			if !errors.Is(acknowledgedWith, writeErr) || !errors.Is(canceledWith, writeErr) {
				t.Fatalf("acknowledgement/cancel errors = %v/%v, want %v", acknowledgedWith, canceledWith, writeErr)
			}
		},
	)
}

func TestForwardResponsesStreamAcknowledgesProtocolWrites(t *testing.T) {
	events := make([]string, 0, 9)
	ginCtx := newOpenAIStreamDeliveryContext(&events)
	data := make(chan coreexecutor.StreamChunk, 3)
	data <- coreexecutor.StreamChunk{
		Payload: []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"),
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("created acknowledgement error = %v", err)
			}
			events = append(events, "created_ack")
		},
	}
	data <- coreexecutor.StreamChunk{
		Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"),
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("completed acknowledgement error = %v", err)
			}
			events = append(events, "completed_ack")
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
	handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

	handler.forwardResponsesStreamChunks(
		ginCtx,
		ginCtx.Writer,
		func(err error) { canceledWith = err },
		newResponsesSSEFrameAccumulator(),
		data,
		nil,
	)

	if canceledWith != nil {
		t.Fatalf("cancel error = %v, want nil", canceledWith)
	}
	want := []string{
		"write", "flush", "created_ack",
		"write", "flush", "completed_ack",
		"write", "flush", "terminal_ack",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}
