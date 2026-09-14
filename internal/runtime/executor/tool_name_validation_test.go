package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestValidateCodexResponsesToolNames(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		requireDeclared bool
		wantError       string
	}{
		{
			name: "declared history call",
			body: `{
				"tools":[{"type":"function","name":"ops__run"}],
				"input":[{"type":"function_call","name":"ops__run","call_id":"call-1","arguments":"{}"}],
				"tool_choice":{"type":"function","name":"ops__run"}
			}`,
			requireDeclared: true,
		},
		{
			name:            "invalid history syntax",
			body:            `{"tools":[{"type":"function","name":"run"}],"input":[{"type":"function_call","name":"bad.name"}]}`,
			requireDeclared: true,
			wantError:       "invalid tool name at input[0].name",
		},
		{
			name:            "history call may reference a previous tool set",
			body:            `{"tools":[{"type":"function","name":"run"}],"input":[{"type":"function_call","name":"mcp__history__get"}]}`,
			requireDeclared: true,
		},
		{
			name:            "history call may exist without current declarations",
			body:            `{"input":[{"type":"function_call","name":"run"}]}`,
			requireDeclared: true,
		},
		{
			name:            "undeclared current tool choice",
			body:            `{"tools":[{"type":"function","name":"run"}],"tool_choice":{"type":"function","name":"other"}}`,
			requireDeclared: true,
			wantError:       "undeclared tool name at tool_choice.name",
		},
		{
			name:            "direct Responses history may omit current declarations",
			body:            `{"input":[{"type":"function_call","name":"run"}]}`,
			requireDeclared: false,
		},
		{
			name:            "invalid declaration",
			body:            `{"tools":[{"type":"function","name":"bad/name"}]}`,
			requireDeclared: false,
			wantError:       "invalid tool name at tools[0].name",
		},
		{
			name:            "overlong name",
			body:            `{"tools":[{"type":"function","name":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`,
			requireDeclared: false,
			wantError:       "expected 1-64 ASCII characters",
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				err := validateCodexResponsesToolNames([]byte(test.body), test.requireDeclared)
				if test.wantError == "" {
					if err != nil {
						t.Fatalf("validateCodexResponsesToolNames() error = %v", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("validateCodexResponsesToolNames() error = %v, want substring %q", err, test.wantError)
				}
				if strings.Contains(err.Error(), "bad.name") || strings.Contains(err.Error(), "bad/name") {
					t.Fatalf("validation error leaked rejected tool name: %v", err)
				}
			},
		)
	}
}

func TestCodexResponsesBodyAllowsValidUndeclaredClaudeHistoryToolName(t *testing.T) {
	const historicalToolName = "mcp__mcp-claude-history__history_get"
	payload := []byte(`{
		"model":"gpt-5.6-sol",
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_history","name":"mcp__mcp-claude-history__history_get","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_history","content":"done"}]},
			{"role":"user","content":"continue"}
		],
		"tools":[{"name":"Read","input_schema":{"type":"object"}}]
	}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: payload}

	prepared, err := codexResponsesBody(
		context.Background(),
		&config.Config{},
		nil,
		"gpt-5.6-sol",
		req.Model,
		"codex",
		sdktranslator.FormatClaude,
		cliproxyexecutor.Options{},
		req,
		true,
	)
	if err != nil {
		t.Fatalf("codexResponsesBody() error = %v", err)
	}

	found := false
	for _, item := range gjson.GetBytes(prepared.Body, "input").Array() {
		if item.Get("type").String() == "function_call" && item.Get("name").String() == historicalToolName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("historical tool call %q was not preserved: %s", historicalToolName, prepared.Body)
	}
}

func TestCodexResponsesBodyRejectsInvalidClaudeHistoryToolName(t *testing.T) {
	messages := make([]map[string]any, 0, 121)
	for range 120 {
		messages = append(messages, map[string]any{"role": "user", "content": "history"})
	}
	messages = append(
		messages, map[string]any{
			"role": "assistant",
			"content": []map[string]any{
				{
					"type":  "tool_use",
					"id":    "toolu_1",
					"name":  "python -c bad",
					"input": map[string]any{},
				},
			},
		},
	)
	payload, errMarshal := json.Marshal(
		map[string]any{
			"model":    "gpt-5.6-sol",
			"messages": messages,
			"tools": []map[string]any{
				{
					"name":         "Bash",
					"input_schema": map[string]any{"type": "object"},
				},
			},
		},
	)
	if errMarshal != nil {
		t.Fatalf("json.Marshal() error = %v", errMarshal)
	}
	req := cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: payload}

	_, err := codexResponsesBody(
		context.Background(),
		&config.Config{},
		nil,
		"gpt-5.6-sol",
		req.Model,
		"codex",
		sdktranslator.FormatClaude,
		cliproxyexecutor.Options{},
		req,
		true,
	)
	if err == nil {
		t.Fatal("codexResponsesBody() error = nil, want local validation error")
	}
	if statusErrCode(err) != 400 {
		t.Fatalf("status code = %d, want 400; error=%v", statusErrCode(err), err)
	}
	if !strings.Contains(err.Error(), "invalid tool name at input[120].name") {
		t.Fatalf("error = %v, want input[120].name", err)
	}
	if strings.Contains(err.Error(), "python -c bad") {
		t.Fatalf("error leaked rejected tool name: %v", err)
	}
}

func TestGrokToCodexInvalidToolNameReplayStopsAtBothBoundaries(t *testing.T) {
	const rejectedName = "python -c 'print(1)'"

	var xaiRequests atomic.Int32
	xaiServer := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				xaiRequests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"name\":\"" + rejectedName + "\",\"call_id\":\"call-1\",\"arguments\":\"{}\"}}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"))
			},
		),
	)
	defer xaiServer.Close()

	xaiExecutor := newXAIExecutor(&config.Config{})
	xaiResult, err := xaiExecutor.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": xaiServer.URL}},
		cliproxyexecutor.Request{
			Model: "grok-4.3",
			Payload: []byte(`{
				"model":"grok-4.3",
				"messages":[{"role":"user","content":"run the declared tool"}],
				"tools":[{"name":"Bash","input_schema":{"type":"object"}}]
			}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Stream: true},
	)
	if err != nil {
		t.Fatalf("xAI ExecuteStream() error = %v", err)
	}

	for {
		select {
		case chunk, ok := <-xaiResult.Chunks:
			if !ok {
				t.Fatal("xAI stream closed before terminal error")
			}
			if chunk.Err == nil {
				continue
			}
			if xaiRequests.Load() != 1 {
				t.Fatalf("xAI request count = %d, want 1", xaiRequests.Load())
			}
			if statusErrCode(chunk.Err) != http.StatusBadGateway ||
				!strings.Contains(chunk.Err.Error(), "invalid_tool_name") {
				t.Fatalf("xAI terminal error = %v", chunk.Err)
			}
			if strings.Contains(chunk.Err.Error(), rejectedName) {
				t.Fatalf("xAI validation error leaked rejected tool name: %v", chunk.Err)
			}
			var dispositionErr cliproxyexecutor.ExecutionDispositionError
			if !errors.As(chunk.Err, &dispositionErr) || dispositionErr == nil ||
				dispositionErr.ExecutionDisposition().DispatchPhase != cliproxyexecutor.DispatchPhaseResponseStarted {
				t.Fatalf("xAI terminal error disposition = %T %v", chunk.Err, chunk.Err)
			}
			goto replay
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for xAI stream to terminate")
		}
	}

replay:
	var codexRequests atomic.Int32
	codexServer := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				codexRequests.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			},
		),
	)
	defer codexServer.Close()

	codexPayload, errMarshal := json.Marshal(
		map[string]any{
			"model": "gpt-5.6-sol",
			"messages": []map[string]any{
				{"role": "user", "content": "run the tool"},
				{
					"role": "assistant",
					"content": []map[string]any{
						{
							"type":  "tool_use",
							"id":    "toolu_1",
							"name":  rejectedName,
							"input": map[string]any{},
						},
					},
				},
			},
			"tools": []map[string]any{
				{
					"name":         "Bash",
					"input_schema": map[string]any{"type": "object"},
				},
			},
		},
	)
	if errMarshal != nil {
		t.Fatalf("json.Marshal() error = %v", errMarshal)
	}

	_, err = newCodexExecutor(&config.Config{}).Execute(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": codexServer.URL}},
		cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: codexPayload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
	)
	if err == nil || statusErrCode(err) != http.StatusBadRequest {
		t.Fatalf("Codex Execute() error = %v, want local 400", err)
	}
	if codexRequests.Load() != 0 {
		t.Fatalf("Codex upstream request count = %d, want 0", codexRequests.Load())
	}
	if strings.Contains(err.Error(), rejectedName) {
		t.Fatalf("Codex validation error leaked rejected tool name: %v", err)
	}
}

func TestXAIToolIdentityRejectsInvalidAndUndeclaredToolCalls(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"Bash"}]}`)

	t.Run(
		"invalid command-shaped name", func(t *testing.T) {
			normalizer := newXAIToolIdentityNormalizer(body).newResponseNormalizer(false)
			rejectedName := "python -c 'print(1)'"
			event := []byte(`{"type":"response.output_item.added","item":{"type":"function_call","name":"` + rejectedName + `","call_id":"call-1"}}`)
			got := normalizer.normalize(event)
			if eventType := gjson.GetBytes(got, "type").String(); eventType != "response.failed" {
				t.Fatalf("event type = %q, want response.failed; event=%s", eventType, got)
			}
			if code := gjson.GetBytes(got, "response.error.code").String(); code != "invalid_tool_name" {
				t.Fatalf("error code = %q, want invalid_tool_name; event=%s", code, got)
			}
			if strings.Contains(string(got), rejectedName) {
				t.Fatalf("error event leaked rejected tool name: %s", got)
			}
			if next := normalizer.normalize([]byte(`{"type":"response.completed","response":{"output":[]}}`)); next != nil {
				t.Fatalf("event after terminal failure was preserved: %s", next)
			}
		},
	)

	t.Run(
		"valid but undeclared name", func(t *testing.T) {
			normalizer := newXAIToolIdentityNormalizer(body).newResponseNormalizer(false)
			got := normalizer.normalize([]byte(`{"type":"response.output_item.done","item":{"type":"function_call","name":"Read","call_id":"call-2"}}`))
			if message := gjson.GetBytes(got, "response.error.message").String(); !strings.Contains(
				message, "undeclared tool call",
			) {
				t.Fatalf("error message = %q, want undeclared tool call; event=%s", message, got)
			}
		},
	)

	t.Run(
		"declared name", func(t *testing.T) {
			normalizer := newXAIToolIdentityNormalizer(body).newResponseNormalizer(false)
			event := []byte(`{"type":"response.output_item.done","item":{"type":"function_call","name":"Bash","call_id":"call-3"}}`)
			if got := normalizer.normalize(event); string(got) != string(event) {
				t.Fatalf("declared event changed: got=%s want=%s", got, event)
			}
		},
	)

	t.Run(
		"invalid completed output", func(t *testing.T) {
			normalizer := newXAIToolIdentityNormalizer(body).newResponseNormalizer(false)
			got := normalizer.normalize([]byte(`{"type":"response.completed","response":{"output":[{"type":"function_call","name":"bad/name","call_id":"call-4"}]}}`))
			if eventType := gjson.GetBytes(got, "type").String(); eventType != "response.failed" {
				t.Fatalf("event type = %q, want response.failed; event=%s", eventType, got)
			}
		},
	)
}
