package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexRecoveryRequiresEachObservedOutputToFinish(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		want   bool
	}{
		{"unfinished text", []string{`{"type":"response.output_text.delta","delta":"partial"}`}, false},
		{"complete text", []string{`{"type":"response.output_text.delta","delta":"complete"}`, `{"type":"response.content_part.done"}`}, true},
		{"other output done", []string{`{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`, `{"type":"response.content_part.done","output_index":1}`}, false},
		{"other content done", []string{`{"type":"response.output_text.delta","content_index":0,"delta":"partial"}`, `{"type":"response.content_part.done","content_index":1}`}, false},
		{"pending second content", []string{`{"type":"response.output_text.delta","content_index":0,"delta":"one"}`, `{"type":"response.output_text.delta","content_index":1,"delta":"two"}`, `{"type":"response.content_part.done","content_index":0}`}, false},
		{"complete contents", []string{`{"type":"response.output_text.delta","content_index":0,"delta":"one"}`, `{"type":"response.output_text.delta","content_index":1,"delta":"two"}`, `{"type":"response.content_part.done","content_index":0}`, `{"type":"response.content_part.done","content_index":1}`}, true},
		{"unfinished reasoning", []string{`{"type":"response.reasoning_summary_text.delta","summary_index":0,"delta":"partial"}`, `{"type":"response.content_part.done"}`}, false},
		{"complete reasoning", []string{`{"type":"response.reasoning_summary_text.delta","delta":"complete"}`, `{"type":"response.reasoning_summary_part.done"}`}, true},
		{"tool without done", []string{`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"edit","arguments":"{}"}}`}, false},
		{"tool partial arguments", []string{`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"edit"}}`, `{"type":"response.function_call_arguments.delta","delta":"{\"path\":"}`}, false},
		{"tool complete arguments", []string{`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call_1","name":"edit"}}`, `{"type":"response.function_call_arguments.delta","delta":"{}"}`, `{"type":"response.function_call_arguments.done","arguments":"{}"}`}, true},
		{"tool malformed done", []string{`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_1","name":"edit","arguments":"{"}}`}, false},
		{"tool missing call id", []string{`{"type":"response.output_item.done","item":{"type":"function_call","name":"edit","arguments":"{}"}}`}, false},
		{"tool incomplete status", []string{`{"type":"response.output_item.done","item":{"type":"function_call","call_id":"call_1","name":"edit","arguments":"{}","status":"incomplete"}}`}, false},
		{"message incomplete status", []string{`{"type":"response.output_item.done","item":{"type":"message","status":"incomplete","content":[{"type":"output_text","text":"partial"}]}}`}, false},
		{"empty pending message", []string{`{"type":"response.output_text.delta","output_index":0,"delta":"one"}`, `{"type":"response.content_part.done","output_index":0}`, `{"type":"response.output_item.added","output_index":1,"item":{"type":"message","content":[]}}`}, false},
		{"delta after done", []string{`{"type":"response.output_text.delta","delta":"one"}`, `{"type":"response.content_part.done"}`, `{"type":"response.output_text.delta","delta":"two"}`}, false},
		{"authoritative message done", []string{`{"type":"response.output_text.delta","delta":"complete"}`, `{"type":"response.output_item.done","item":{"type":"message","status":"completed","content":[{"type":"output_text","text":"complete"}]}}`}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recovery := newCodexNonStreamRecovery("gpt-5.5")
			for _, event := range tc.events {
				recovery.addEvent([]byte(event))
			}
			payload, ok := recovery.completedPayload()
			if ok != tc.want || (!ok && len(payload) != 0) {
				t.Fatalf("recovered=%v, want %v, payload=%s", ok, tc.want, payload)
			}
		})
	}
}

func TestCodexRecoveryKeepsInterleavedToolArgumentsByOutput(t *testing.T) {
	recovery := newCodexNonStreamRecovery("gpt-5.5")
	for _, event := range []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"item_0","type":"function_call","call_id":"call_0","name":"first"}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"second"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"item_0","delta":"{\"first\":1}"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"{\"second\":2}"}`,
		`{"type":"response.function_call_arguments.done","item_id":"item_1","arguments":"{\"second\":2}"}`,
	} {
		recovery.addEvent([]byte(event))
	}
	if payload, ok := recovery.completedPayload(); ok {
		t.Fatalf("unfinished first tool recovered as completed: %s", payload)
	}
	recovery.addEvent([]byte(`{"type":"response.function_call_arguments.done","item_id":"item_0","arguments":"{\"first\":1}"}`))
	payload, ok := recovery.completedPayload()
	if !ok || gjson.GetBytes(payload, "response.output.0.arguments").String() != `{"first":1}` ||
		gjson.GetBytes(payload, "response.output.1.arguments").String() != `{"second":2}` {
		t.Fatalf("interleaved tool arguments were mixed: recovered=%v, payload=%s", ok, payload)
	}
}

func TestCodexUnfinishedOutputFailsAtHTTPConsumer(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, unfinished := range []string{
			`{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call_1","name":"edit","arguments":"{}"}}`,
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, gjson.Get(unfinished, "type").String()), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+unfinished+"\n\n")
				}))
				defer server.Close()
				executor := newCodexExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
				req := core.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"synthetic"}`)}
				opts := core.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				if !stream {
					response, err := executor.Execute(context.Background(), runtimeCodexAuth(server.URL), req, opts)
					if err == nil || len(response.Payload) != 0 {
						t.Fatalf("unfinished response: payload=%s, error=%v", response.Payload, err)
					}
					return
				}
				result, err := executor.ExecuteStream(context.Background(), runtimeCodexAuth(server.URL), req, opts)
				if err != nil {
					t.Fatal(err)
				}
				var payload strings.Builder
				var terminalErr error
				for chunk := range result.Chunks {
					payload.Write(chunk.Payload)
					if chunk.Err != nil {
						terminalErr = chunk.Err
					}
				}
				if terminalErr == nil || strings.Contains(payload.String(), `"type":"response.completed"`) {
					t.Fatalf("unfinished stream: payload=%s, error=%v", payload.String(), terminalErr)
				}
			})
		}
	}
}
