package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

type openAIStreamDeliveryWriter struct {
	gin.ResponseWriter
	events     *[]string
	writeErr   error
	shortWrite bool
	flushErr   error
}

func (w *openAIStreamDeliveryWriter) Write(payload []byte) (int, error) {
	*w.events = append(*w.events, "write")
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.shortWrite && len(payload) > 0 {
		return len(payload) - 1, nil
	}
	return w.ResponseWriter.Write(payload)
}

func (w *openAIStreamDeliveryWriter) FlushError() error {
	*w.events = append(*w.events, "flush")
	return w.flushErr
}

type responsesImagesDeliveryExecutor struct {
	*imageCaptureExecutor
	chunks []coreexecutor.StreamChunk
}

func (e *responsesImagesDeliveryExecutor) ExecuteStream(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, len(e.chunks))
	for _, chunk := range e.chunks {
		chunks <- chunk
	}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

type responsesImagesDeliveryHook struct {
	coreauth.NoopHook
	results chan coreauth.Result
}

func (h *responsesImagesDeliveryHook) OnResult(_ context.Context, result coreauth.Result) {
	h.results <- result
}

func TestForwardRawImageStreamChunksAcknowledgesDeliveredFrames(t *testing.T) {
	handler, ctx, _ := newImageStreamTestHandler(t)
	events := make([]string, 0, 8)
	ctx.Writer = &openAIStreamDeliveryWriter{ResponseWriter: ctx.Writer, events: &events}
	data := make(chan coreexecutor.StreamChunk, 3)
	data <- coreexecutor.StreamChunk{
		Payload: []byte("data: {\"type\":\"image_generation.partial\",\"value\":1}\n\n"),
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("payload acknowledgement error = %v", err)
			}
			events = append(events, "payload_ack")
		},
	}
	data <- coreexecutor.StreamChunk{
		Payload: []byte("data: {\"type\":\"response.completed\"}\n\n"),
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("completion acknowledgement error = %v", err)
			}
			events = append(events, "completion_ack")
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

	err := handler.forwardRawImageStreamChunks(context.Background(), ctx, data, nil)
	if err != nil {
		t.Fatalf("forwardRawImageStreamChunks error = %v", err)
	}
	want := []string{
		"write", "flush", "payload_ack",
		"write", "flush", "completion_ack",
		"flush", "terminal_ack",
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestForwardRawImageStreamChunksAcknowledgesTerminalErrorAfterFlush(t *testing.T) {
	handler, ctx, recorder := newImageStreamTestHandler(t)
	events := make([]string, 0, 5)
	ctx.Writer = &openAIStreamDeliveryWriter{ResponseWriter: ctx.Writer, events: &events}
	data := make(chan coreexecutor.StreamChunk)
	close(data)
	upstreamErr := errors.New("image upstream failed")
	errs := make(chan *handlers.ErrorMessage, 1)
	errs <- &handlers.ErrorMessage{
		StatusCode: http.StatusBadGateway,
		Error:      upstreamErr,
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("terminal error acknowledgement = %v", err)
			}
			events = append(events, "error_ack")
		},
	}
	close(errs)

	err := handler.forwardRawImageStreamChunks(context.Background(), ctx, data, errs)
	if !errors.Is(err, upstreamErr) {
		t.Fatalf("forwardRawImageStreamChunks error = %v, want %v", err, upstreamErr)
	}
	want := []string{"write", "write", "write", "flush", "error_ack"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if body := recorder.Body.String(); body == "" {
		t.Fatal("terminal error response was not written")
	}
}

func TestDeliverCollectedImagesResponseAcknowledgesFinalDelivery(t *testing.T) {
	writeFailure := errors.New("write failed")
	flushFailure := errors.New("flush failed")
	tests := []struct {
		name       string
		writeErr   error
		shortWrite bool
		flushErr   error
		wantErr    error
		wantEvents []string
	}{
		{
			name:       "success",
			wantEvents: []string{"write", "flush", "terminal_ack"},
		},
		{
			name:       "write failure",
			writeErr:   writeFailure,
			wantErr:    writeFailure,
			wantEvents: []string{"write", "terminal_ack"},
		},
		{
			name:       "short write",
			shortWrite: true,
			wantErr:    io.ErrShortWrite,
			wantEvents: []string{"write", "terminal_ack"},
		},
		{
			name:       "flush failure",
			flushErr:   flushFailure,
			wantErr:    flushFailure,
			wantEvents: []string{"write", "flush", "terminal_ack"},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				events := make([]string, 0, 3)
				writer := &openAIStreamDeliveryWriter{
					ResponseWriter: c.Writer,
					events:         &events,
					writeErr:       test.writeErr,
					shortWrite:     test.shortWrite,
					flushErr:       test.flushErr,
				}
				var acknowledged error
				err := deliverCollectedImagesResponse(
					writer,
					writer,
					[]byte(`{"created":1,"data":[]}`),
					func(err error) {
						acknowledged = err
						events = append(events, "terminal_ack")
					},
				)
				if !errors.Is(err, test.wantErr) || !errors.Is(acknowledged, test.wantErr) {
					t.Fatalf("delivery/ack errors = %v/%v, want %v", err, acknowledged, test.wantErr)
				}
				if !reflect.DeepEqual(events, test.wantEvents) {
					t.Fatalf("events = %v, want %v", events, test.wantEvents)
				}
			},
		)
	}
}

func TestCollectImagesFromResponsesChunksDefersTerminalAcknowledgement(t *testing.T) {
	handler, c, _ := newImageStreamTestHandler(t)
	events := make([]string, 0, 4)
	data := make(chan coreexecutor.StreamChunk, 2)
	data <- coreexecutor.StreamChunk{
		Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"created_at\":123,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"" + testPNGBase64 + "\",\"output_format\":\"png\"}]}}\n\n"),
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("payload acknowledgement = %v", err)
			}
			events = append(events, "payload_ack")
		},
	}
	data <- coreexecutor.StreamChunk{
		Terminal: true,
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("terminal acknowledgement = %v", err)
			}
			events = append(events, "terminal_ack")
		},
	}
	close(data)

	out, terminalAcknowledge, errMsg := handler.collectImagesFromResponsesChunks(
		context.Background(), c, data, nil, "url",
	)
	if errMsg != nil {
		t.Fatalf("collectImagesFromResponsesChunks error = %#v", errMsg)
	}
	if terminalAcknowledge == nil || !reflect.DeepEqual(events, []string{"payload_ack"}) {
		t.Fatalf("collector events = %v, terminal acknowledgement present = %t", events, terminalAcknowledge != nil)
	}
	if !strings.Contains(string(out), `"url":"https://cpa.test/v1/artifacts/img_`) {
		t.Fatalf("collected response = %s", out)
	}

	writer := &openAIStreamDeliveryWriter{ResponseWriter: c.Writer, events: &events}
	if err := deliverCollectedImagesResponse(writer, writer, out, terminalAcknowledge); err != nil {
		t.Fatalf("deliverCollectedImagesResponse error = %v", err)
	}
	want := []string{"payload_ack", "write", "flush", "terminal_ack"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestCollectImagesFromResponsesUsesDeliveryAwareStream(t *testing.T) {
	const model = "responses-image-delivery-model"
	payloadAcknowledged := make(chan error, 1)
	executor := &responsesImagesDeliveryExecutor{
		imageCaptureExecutor: &imageCaptureExecutor{},
		chunks: []coreexecutor.StreamChunk{
			{
				Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"created_at\":123,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"" + testPNGBase64 + "\",\"output_format\":\"png\"}]}}\n\n"),
				Acknowledge: func(err error) {
					payloadAcknowledged <- err
				},
			},
		},
	}
	hook := &responsesImagesDeliveryHook{results: make(chan coreauth.Result, 1)}
	manager := coreauth.NewManager(nil, nil, hook)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "responses-image-delivery-auth",
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
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(auth.ID)
		},
	)

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	setTestImageArtifactDeps(t, base)
	handler := NewAPIHandler(base)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

	handler.collectImagesFromResponses(c, []byte(`{"model":"`+model+`"}`), "url")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"url":"https://cpa.test/v1/artifacts/img_`) {
		t.Fatalf("response = %s", recorder.Body.String())
	}
	select {
	case err := <-payloadAcknowledged:
		if err != nil {
			t.Fatalf("payload acknowledgement = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("payload was not acknowledged")
	}
	select {
	case result := <-hook.results:
		if !result.Success {
			t.Fatalf("stream result = %#v, want success", result)
		}
	case <-time.After(time.Second):
		t.Fatal("stream result was not recorded after response delivery")
	}
}

func TestCollectImagesFromResponsesChunksAcknowledgesProtocolErrorAfterResponse(t *testing.T) {
	writeFailure := errors.New("error response write failed")
	tests := []struct {
		name       string
		writeErr   error
		wantWrite  error
		wantEvents []string
	}{
		{
			name:       "error response delivered",
			wantEvents: []string{"payload_ack", "write", "flush", "terminal_ack"},
		},
		{
			name:       "error response write failure",
			writeErr:   writeFailure,
			wantWrite:  writeFailure,
			wantEvents: []string{"payload_ack", "write", "terminal_ack"},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				handler, c, _ := newImageStreamTestHandler(t)
				events := make([]string, 0, 4)
				var terminalAckErr error
				data := make(chan coreexecutor.StreamChunk, 2)
				data <- coreexecutor.StreamChunk{
					Payload: []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_missing\"}}\n\n"),
					Acknowledge: func(err error) {
						if err != nil {
							t.Errorf("payload acknowledgement = %v", err)
						}
						events = append(events, "payload_ack")
					},
				}
				data <- coreexecutor.StreamChunk{
					Terminal: true,
					Acknowledge: func(err error) {
						terminalAckErr = err
						events = append(events, "terminal_ack")
					},
				}
				close(data)

				out, terminalAcknowledge, errMsg := handler.collectImagesFromResponsesChunks(
					context.Background(), c, data, nil, "b64_json",
				)
				if out != nil || terminalAcknowledge != nil || errMsg == nil || errMsg.Error == nil ||
					!strings.Contains(errMsg.Error.Error(), "before completion") {
					t.Fatalf("collector result = (%s, terminal:%t, %#v)", out, terminalAcknowledge != nil, errMsg)
				}
				if !reflect.DeepEqual(events, []string{"payload_ack"}) {
					t.Fatalf("events before error response = %v", events)
				}

				writer := &openAIStreamDeliveryWriter{
					ResponseWriter: c.Writer,
					events:         &events,
					writeErr:       test.writeErr,
				}
				c.Writer = writer
				errWrite := handler.WriteAcknowledgedStreamError(c, writer, errMsg)
				if !errors.Is(errWrite, test.wantWrite) {
					t.Fatalf("error response write = %v, want %v", errWrite, test.wantWrite)
				}
				if terminalAckErr == nil || !strings.Contains(terminalAckErr.Error(), "before completion") ||
					test.wantWrite != nil && !errors.Is(terminalAckErr, test.wantWrite) {
					t.Fatalf("terminal acknowledgement = %v", terminalAckErr)
				}
				if !reflect.DeepEqual(events, test.wantEvents) {
					t.Fatalf("events = %v, want %v", events, test.wantEvents)
				}
			},
		)
	}
}

func TestCollectImagesFromResponsesChunksLateErrorWins(t *testing.T) {
	handler, c, _ := newImageStreamTestHandler(t)
	events := make([]string, 0, 4)
	payloadAcknowledged := make(chan struct{})
	data := make(chan coreexecutor.StreamChunk)
	errs := make(chan *handlers.ErrorMessage)
	lateErr := errors.New("late responses failure")
	go func() {
		data <- coreexecutor.StreamChunk{
			Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"created_at\":123,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"" + testPNGBase64 + "\",\"output_format\":\"png\"}]}}\n\n"),
			Acknowledge: func(err error) {
				if err != nil {
					t.Errorf("payload acknowledgement = %v", err)
				}
				events = append(events, "payload_ack")
				close(payloadAcknowledged)
			},
		}
		<-payloadAcknowledged
		errs <- &handlers.ErrorMessage{
			StatusCode: http.StatusBadGateway,
			Error:      lateErr,
			Acknowledge: func(err error) {
				if err != nil {
					t.Errorf("late error acknowledgement = %v", err)
				}
				events = append(events, "error_ack")
			},
		}
		close(errs)
		close(data)
	}()

	out, terminalAcknowledge, errMsg := handler.collectImagesFromResponsesChunks(
		context.Background(), c, data, errs, "url",
	)
	if out != nil || terminalAcknowledge != nil || errMsg == nil || !errors.Is(errMsg.Error, lateErr) {
		t.Fatalf("collector result = (%s, terminal:%t, %#v)", out, terminalAcknowledge != nil, errMsg)
	}
	writer := &openAIStreamDeliveryWriter{ResponseWriter: c.Writer, events: &events}
	c.Writer = writer
	if errWrite := handler.WriteAcknowledgedStreamError(c, writer, errMsg); errWrite != nil {
		t.Fatalf("error response write = %v", errWrite)
	}
	want := []string{"payload_ack", "write", "flush", "error_ack"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestCollectImagesFromResponsesChunksRequiresTransportTerminal(t *testing.T) {
	handler, c, _ := newImageStreamTestHandler(t)
	acknowledged := false
	data := make(chan coreexecutor.StreamChunk, 1)
	data <- coreexecutor.StreamChunk{
		Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"created_at\":123,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"" + testPNGBase64 + "\",\"output_format\":\"png\"}]}}\n\n"),
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("payload acknowledgement = %v", err)
			}
			acknowledged = true
		},
	}
	close(data)

	out, terminalAcknowledge, errMsg := handler.collectImagesFromResponsesChunks(
		context.Background(), c, data, nil, "url",
	)
	if !acknowledged || out != nil || terminalAcknowledge != nil || errMsg == nil || errMsg.Error == nil ||
		!strings.Contains(errMsg.Error.Error(), "before transport completion") {
		t.Fatalf(
			"collector result = acknowledged:%t output:%s terminal:%t error:%#v", acknowledged, out,
			terminalAcknowledge != nil, errMsg,
		)
	}
}
