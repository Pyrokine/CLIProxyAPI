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

func TestForwardResponsesStreamFiltersPrivateEventsByClient(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name              string
		originator        string
		wantCodexMetadata bool
	}{
		{name: "standard"},
		{name: "codex", originator: "codex-tui", wantCodexMetadata: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("Originator", tt.originator)
			chunks := make(chan coreexecutor.StreamChunk, 5)
			for _, frame := range []string{
				"event: responsesapi.timing\ndata: malformed-json\n\n",
				"event: codex.rate_limits\ndata: {\"type\":\"codex.rate_limits\"}\n\n",
				"event: codex.response.metadata\ndata: {\"type\":\"codex.response.metadata\"}\n\n",
				"event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
			} {
				chunks <- coreexecutor.StreamChunk{Payload: []byte(frame)}
			}
			chunks <- coreexecutor.StreamChunk{Terminal: true}
			close(chunks)

			var canceledWith error
			h := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
			h.forwardResponsesStreamChunks(c, c.Writer, func(err error) { canceledWith = err }, nil, chunks, nil)
			if canceledWith != nil {
				t.Fatalf("stream error = %v", canceledWith)
			}
			body := recorder.Body.String()
			if strings.Contains(body, "responsesapi.timing") || strings.Contains(body, "codex.rate_limits") {
				t.Fatalf("private events reached client: %q", body)
			}
			if got := strings.Contains(body, "codex.response.metadata"); got != tt.wantCodexMetadata {
				t.Fatalf("metadata present = %v, want %v; body=%q", got, tt.wantCodexMetadata, body)
			}
			if !strings.Contains(body, "response.completed") {
				t.Fatalf("missing public terminal frame: %q", body)
			}
		})
	}
}
