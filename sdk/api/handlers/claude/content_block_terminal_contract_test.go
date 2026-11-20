package claude

import (
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

func TestClaudeSDKChunksCloseBlocksBeforeTerminalFrames(t *testing.T) {
	const prefix = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{" +
		"\"id\":\"fixture\",\"model\":\"claude-test\"}}\n\n" +
		("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"" +
			"index\":3,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n") +
		("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"" +
			"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	const delta = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"s" +
		"top_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"
	const stop = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	const failure = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":" +
		"\"api_error\",\"message\":\"synthetic " +
		"failure\"}}\n\n"
	for _, ending := range []string{"delta-stop", "delta-eof", "error"} {
		for _, mode := range []string{"whole", "bytes", "split-frames"} {
			t.Run(
				ending+"/"+mode, func(t *testing.T) {
					wire := prefix + delta
					if ending == "delta-stop" {
						wire += stop
					} else if ending == "error" {
						wire = prefix + failure
					}
					stride := len(wire)
					if mode == "bytes" {
						stride = 1
					} else if mode == "split-frames" {
						stride = 17
					}
					data := make(chan coreexecutor.StreamChunk, len(wire)+1)
					for offset := 0; offset < len(wire); offset += stride {
						data <- coreexecutor.StreamChunk{Payload: []byte(wire[offset:min(offset+stride, len(wire))])}
					}
					close(data)
					errs := make(chan *handlers.ErrorMessage)
					close(errs)
					gin.SetMode(gin.TestMode)
					recorder := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(recorder)
					ctx.Request = httptest.NewRequest("POST", "/v1/messages", nil)
					handler := NewCodeAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
					lifecycle := newClaudeStreamLifecycle()
					handler.forwardClaudeStreamChunksWithLifecycle(
						ctx, recorder, func(error) {}, data, errs, lifecycle, nil, 0,
					)
					events := parseClaudeHandlerSSEEvents(t, recorder.Body.String())
					assertClaudeHandlerSSELifecycle(t, events)
					var names []string
					var closed []int64
					for _, event := range events {
						names = append(names, event.name)
						if event.name == "content_block_stop" {
							closed = append(closed, event.data.Get("index").Int())
						}
					}
					want := []string{
						"message_start", "content_block_start", "content_block_start", "content_block_stop",
						"content_block_stop",
					}
					if ending == "error" {
						want = append(want, "error")
					} else {
						want = append(want, "message_delta", "message_stop")
					}
					if !slices.Equal(names, want) || !slices.Equal(
						closed, []int64{3, 1},
					) || len(lifecycle.openBlocks) != 0 {
						t.Fatalf(
							"terminal sequence=%v want=%v closed=%v open=%v",
							names,
							want,
							closed,
							lifecycle.openBlocks,
						)
					}
					if ending != "error" && !strings.Contains(recorder.Body.String(), `"output_tokens":2`) {
						t.Fatal("terminal usage was lost")
					}
				},
			)
		}
	}
}
