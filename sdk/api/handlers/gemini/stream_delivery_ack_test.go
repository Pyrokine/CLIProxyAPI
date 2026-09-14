package gemini

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gin-gonic/gin"
)

type geminiDeliveryWriter struct {
	gin.ResponseWriter
	events *[]string
}

func (w *geminiDeliveryWriter) Write(payload []byte) (int, error) {
	*w.events = append(*w.events, "write")
	return w.ResponseWriter.Write(payload)
}

func (w *geminiDeliveryWriter) FlushError() error {
	*w.events = append(*w.events, "flush")
	return nil
}

func newGeminiDeliveryContext(events *[]string) *gin.Context {
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Writer = &geminiDeliveryWriter{ResponseWriter: context.Writer, events: events}
	context.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini:streamGenerateContent", nil)
	return context
}

func TestForwardGeminiStreamAcknowledgesFinalWrites(t *testing.T) {
	events := make([]string, 0, 7)
	context := newGeminiDeliveryContext(&events)
	data := make(chan coreexecutor.StreamChunk, 2)
	data <- coreexecutor.StreamChunk{
		Payload: []byte(`{"candidates":[]}`),
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

	handler.forwardGeminiStream(
		context, context.Writer, "", func(err error) { canceledWith = err }, data, nil,
	)

	if canceledWith != nil {
		t.Fatalf("cancel error = %v, want nil", canceledWith)
	}
	want := []string{"write", "write", "write", "flush", "payload_ack", "flush", "terminal_ack"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestForwardGeminiCLIStreamAcknowledgesSuppressedDoneChunk(t *testing.T) {
	events := make([]string, 0, 4)
	context := newGeminiDeliveryContext(&events)
	data := make(chan coreexecutor.StreamChunk, 2)
	data <- coreexecutor.StreamChunk{
		Payload: []byte("[DONE]"),
		Acknowledge: func(err error) {
			if err != nil {
				t.Errorf("done acknowledgement error = %v", err)
			}
			events = append(events, "done_ack")
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
	handler := NewCLIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

	handler.forwardCLIStream(
		context, context.Writer, "", func(err error) { canceledWith = err }, data, nil,
	)

	if canceledWith != nil {
		t.Fatalf("cancel error = %v, want nil", canceledWith)
	}
	want := []string{"flush", "done_ack", "flush", "terminal_ack"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}
