package openai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestOpenAIMissingFinishRequiresSSEError(t *testing.T) {
	for _, tc := range []struct {
		name        string
		payload     string
		completions bool
	}{
		{"chat absent", `{"choices":[{"index":0,"delta":{"content":"partial"}}]}`, false},
		{"chat null", `{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`, false},
		{"chat empty", `{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":""}]}`, false},
		{"legacy absent", `{"choices":[{"index":0,"delta":{"content":"partial"}}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			data := make(chan coreexecutor.StreamChunk, 2)
			data <- coreexecutor.StreamChunk{Payload: []byte(tc.payload)}
			var terminalAck error
			data <- coreexecutor.StreamChunk{Terminal: true, Acknowledge: func(err error) { terminalAck = err }}
			close(data)
			var canceledWith error
			handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
			var transform func([]byte) []byte
			if tc.completions {
				transform = convertChatCompletionsStreamChunkToCompletions
			}
			handler.handleStreamResult(c, c.Writer, func(err error) { canceledWith = err }, data, nil, transform)
			t.Logf("body=%q cancel=%v ack=%v status=%d", recorder.Body.String(), canceledWith, terminalAck, recorder.Code)
			if canceledWith == nil || terminalAck == nil || !strings.Contains(canceledWith.Error(), "finish_reason") {
				t.Fatalf("current lifecycle did not detect missing finish_reason: cancel=%v ack=%v", canceledWith, terminalAck)
			}
			if strings.Contains(recorder.Body.String(), "[DONE]") {
				t.Fatal("truncated stream emitted success terminal")
			}
			if !strings.Contains(recorder.Body.String(), `"error"`) {
				t.Fatal("missing finish_reason was only acknowledged internally; no structured SSE error reached client")
			}
		})
	}
}

func TestOpenAICompleteStillEmitsDone(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	data := make(chan coreexecutor.StreamChunk, 2)
	data <- coreexecutor.StreamChunk{Payload: []byte(`{"choices":[{"index":0,"delta":{"content":"complete"},"finish_reason":"stop"}]}`)}
	data <- coreexecutor.StreamChunk{Terminal: true}
	close(data)
	var canceledWith error
	handler := NewAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	handler.handleStreamResult(c, c.Writer, func(err error) { canceledWith = err }, data, nil, nil)
	if canceledWith != nil || !strings.Contains(recorder.Body.String(), "data: [DONE]") {
		t.Fatalf("complete stream result body=%q cancel=%v", recorder.Body.String(), canceledWith)
	}
}
