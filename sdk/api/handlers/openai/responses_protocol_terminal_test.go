package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestResponsesProtocolTerminalControlsForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := responsesStreamKeepAliveInterval
	responsesStreamKeepAliveInterval = time.Millisecond
	t.Cleanup(func() { responsesStreamKeepAliveInterval = previous })
	for _, scenario := range []string{"delayed transport terminal", "late payload", "late error"} {
		t.Run(
			scenario, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(t.Context())
				data := make(chan coreexecutor.StreamChunk, 3)
				firstAcknowledged := make(chan struct{})
				data <- coreexecutor.StreamChunk{
					Payload: []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"),
					Acknowledge: func(err error) {
						if err != nil {
							t.Errorf("protocol terminal acknowledgement = %v", err)
						}
						close(firstAcknowledged)
					},
				}
				var lateAcknowledgement error
				lateError := errors.New("fixture late upstream failure")
				switch scenario {
				case "late payload", "late error":
					chunk := coreexecutor.StreamChunk{Acknowledge: func(err error) { lateAcknowledgement = err }}
					if scenario == "late payload" {
						chunk.Payload = []byte(
							("event: response.output_text.delta\ndata: {\"type\":\"response.output_t" +
								"ext.delta\",\"delta\":\"fixture-late-payload\"}\n\n"),
						)
					} else {
						chunk.Err = lateError
					}
					data <- chunk
					data <- coreexecutor.StreamChunk{Terminal: true}
					close(data)
				case "delayed transport terminal":
					go func() {
						select {
						case <-firstAcknowledged:
						case <-t.Context().Done():
							return
						}
						timer := time.NewTimer(15 * time.Millisecond)
						defer timer.Stop()
						select {
						case <-timer.C:
							data <- coreexecutor.StreamChunk{Terminal: true}
							close(data)
						case <-t.Context().Done():
						}
					}()
				}
				var canceledWith error
				handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
				handler.forwardResponsesStreamChunks(
					ctx, ctx.Writer, func(err error) { canceledWith = err }, nil, data, nil,
				)
				body := recorder.Body.String()
				terminal := strings.Index(body, "event: response.completed")
				if terminal < 0 || strings.Count(
					body, "event: response.completed",
				) != 1 || strings.Contains(body[terminal:], "keep-alive") || strings.Contains(
					body, "fixture-late-payload",
				) || strings.Contains(body, "event: error") || strings.Contains(body, "event: response.failed") {
					t.Fatalf("unexpected post-terminal output: %q", body)
				}
				if scenario == "delayed transport terminal" {
					if canceledWith != nil {
						t.Fatalf("successful terminal canceled with %v", canceledWith)
					}
				} else if lateAcknowledgement == nil || canceledWith == nil {
					t.Fatalf("late chunk acknowledgement/cancellation = %v/%v", lateAcknowledgement, canceledWith)
				}
			},
		)
	}
}
