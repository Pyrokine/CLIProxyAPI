package executor

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestDevinToolCallIDRoundTripThroughClaude(t *testing.T) {
	for _, nativeID := range []string{"0:read/file", "é:工具", "cpa_tid_v1_MDpmb28", "call_1"} {
		for _, stream := range []bool{false, true} {
			name := nativeID + "/non-stream"
			if stream {
				name = nativeID + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				tool := testDevinBytesField(nil, 1, []byte(nativeID))
				tool = testDevinBytesField(tool, 2, []byte("read_file"))
				tool = testDevinBytesField(tool, 3, []byte(`{"path":"test.txt"}`))
				tool = testDevinVarintField(tool, 4, 0)
				frames := testDevinConnectStream(testDevinBytesField(nil, 6, tool))
				executor := NewDevinExecutor(&config.Config{})
				ctx := context.Background()
				model := "devin/swe-2"

				var clientID string
				if !stream {
					interaction, _, _, err := consumeDevinFrames(bytes.NewReader(frames), model)
					if err != nil {
						t.Fatalf("consumeDevinFrames: %v", err)
					}
					response := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatInteractions, sdktranslator.FormatClaude, model, nil, nil, interaction, nil)
					clientID = gjson.GetBytes(response, "content.0.id").String()
				} else {
					out := make(chan cliproxyexecutor.StreamChunk, 32)
					go func() {
						defer close(out)
						executor.streamDevinFrames(ctx, bytes.NewReader(frames),
							cliproxyexecutor.Request{Model: model},
							cliproxyexecutor.Options{ResponseFormat: sdktranslator.FormatClaude}, nil,
							newUsageReporter(ctx, executor, model, nil), out)
					}()
					for chunk := range out {
						if chunk.Err != nil {
							t.Fatalf("streamDevinFrames: %v", chunk.Err)
						}
						index := bytes.Index(chunk.Payload, []byte("data: "))
						if index < 0 {
							continue
						}
						payload := bytes.TrimSpace(chunk.Payload[index+len("data: "):])
						if gjson.GetBytes(payload, "type").String() == "content_block_start" &&
							gjson.GetBytes(payload, "content_block.type").String() == "tool_use" {
							clientID = gjson.GetBytes(payload, "content_block.id").String()
						}
					}
				}
				if clientID == "" || clientID != helps.EncodeDevinToolCallID(nativeID) {
					t.Fatalf("Claude tool ID = %q, want encoding of %q", clientID, nativeID)
				}

				request := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"read_file","input":{"path":"test.txt"}}]},{"role":"user","content":[{"type":"tool_result","content":"done"}]}]}`)
				request, _ = sjson.SetBytes(request, "messages.0.content.0.id", clientID)
				request, _ = sjson.SetBytes(request, "messages.1.content.0.tool_use_id", clientID)
				if !gjson.ValidBytes(request) {
					t.Fatalf("Claude request is invalid: %s", request)
				}
				prepared, err := executor.prepareDevinRequest(ctx,
					&cliproxyauth.Auth{ID: "offline-devin-id-test", Attributes: map[string]string{
						"api_key": "synthetic-session-token", "device_seed": "offline-device",
					}},
					cliproxyexecutor.Request{Model: model, Payload: request},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, false)
				if err != nil {
					t.Fatalf("prepareDevinRequest: %v", err)
				}
				framed, err := io.ReadAll(prepared.httpReq.Body)
				if err != nil {
					t.Fatalf("read prepared body: %v", err)
				}
				_, wire, err := helps.ReadConnectFrame(bytes.NewReader(framed))
				if err != nil {
					t.Fatalf("read Connect frame: %v", err)
				}
				prompts := allDevinBytesFields(t, wire, 3)
				if len(prompts) != 2 {
					t.Fatalf("wire prompt count = %d, want 2", len(prompts))
				}
				call := firstDevinBytesField(t, prompts[0], 6)
				if id := string(firstDevinBytesField(t, call, 1)); id != nativeID {
					t.Fatalf("wire tool call ID = %q, want %q", id, nativeID)
				}
				if id := string(firstDevinBytesField(t, prompts[1], 7)); id != nativeID {
					t.Fatalf("wire tool result ID = %q, want %q", id, nativeID)
				}
				if !strings.Contains(string(firstDevinBytesField(t, prompts[1], 3)), "done") {
					t.Fatal("wire tool result content was lost")
				}
			})
		}
	}
}
