package openai

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestResponsesFailedEventsAreTerminalFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, failure := range []struct{ name, frame string }{
		{
			"response failed",
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response" +
				"\":{\"status\":\"failed\",\"error\":{\"code\":\"invalid_value\",\"mess" +
				"age\":\"invalid field; Bearer fixture-private-bearer; http://10.23.4.5" +
				"/private\",\"access_token\":\"fixture-private-access\",\"password\":\"" +
				"fixture-private-password\"}}}\n\n",
		},
		{
			"typed error",
			"event: error\ndata: {\"type\":\"error\",\"code\":\"invalid_value\",\"message\":\"invalid field; " +
				"Bearer fixture-private-bearer; http://10.23.4.5/private\"}\n\n",
		},
		{"event-only error", "event: error\ndata: " + mediaPrivateError + "\n\n"},
	} {
		for _, combined := range []bool{false, true} {
			name := failure.name + "/separate chunks"
			if combined {
				name = failure.name + "/same chunk"
			}
			t.Run(
				name, func(t *testing.T) {
					outcome := internallogging.NewRequestOutcomeState()
					recorder := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(recorder)
					ctx.Request = httptest.NewRequest(
						http.MethodPost, "/v1/responses", nil,
					).WithContext(internallogging.WithRequestOutcomeState(t.Context(), outcome))
					data := make(chan coreexecutor.StreamChunk, 3)
					completed := "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
					if combined {
						data <- coreexecutor.StreamChunk{Payload: []byte(failure.frame + completed)}
					} else {
						data <- coreexecutor.StreamChunk{Payload: []byte(failure.frame)}
						data <- coreexecutor.StreamChunk{Payload: []byte(completed)}
					}
					data <- coreexecutor.StreamChunk{Terminal: true}
					close(data)
					var canceledWith error
					handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
					handler.forwardResponsesStreamChunks(
						ctx, ctx.Writer, func(err error) { canceledWith = err }, nil, data, nil,
					)
					body := recorder.Body.String()
					if strings.Contains(body, "response.completed") {
						t.Errorf("success followed protocol failure: %s", body)
					}
					if canceledWith == nil || !outcome.Snapshot().Failed {
						t.Errorf(
							"protocol failure recorded as success: cancellation=%v outcome=%#v", canceledWith,
							outcome.Snapshot(),
						)
					}
					assertMediaErrorPrivacy(t, body)
					if !recorder.Flushed {
						t.Fatal("failure frame was not flushed")
					}
				},
			)
		}
	}
}
