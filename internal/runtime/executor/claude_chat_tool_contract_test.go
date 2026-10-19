package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestClaudeExecutorChatToolNameRoundTrip(t *testing.T) {
	names := []string{"mcp.server:get_time", "mcp:server.get_time", strings.Repeat("t", 64) + "a", strings.Repeat("t", 64) + "b", "clean_tool"}
	tools := make([]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": name}})
	}
	input, err := json.Marshal(map[string]any{"model": "claude-sonnet-4-5", "messages": []any{map[string]any{"role": "user", "content": "fixture"}}, "tools": tools})
	if err != nil {
		t.Fatal(err)
	}
	for _, oauth := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("oauth=%t/stream=%t", oauth, stream), func(t *testing.T) {
				requests := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					body, readErr := io.ReadAll(request.Body)
					if readErr != nil {
						http.Error(w, readErr.Error(), http.StatusBadRequest)
						return
					}
					requests <- body
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"fixture\",\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
					for index, tool := range gjson.GetBytes(body, "tools").Array() {
						name := tool.Get("name").String()
						if oauth {
							parts, _ := parseClaudeMCPAlias(name)
							name = "mcp__" + parts.server + "__" + name
						}
						_, _ = fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_%d\",\"name\":%q,\"input\":{}}}\n\n", index, index, name)
						_, _ = fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"n\\\":9007199254740993}\"}}\n\n", index)
						_, _ = fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", index)
					}
					_, _ = fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
				}))
				defer server.Close()
				key := "sk-ant-api-fixture"
				if oauth {
					key = "sk-ant-oat01-test"
				}
				auth := &cliproxyauth.Auth{
					ID: "claude-chat-tool-fixture", Provider: "claude",
					Attributes: map[string]string{"api_key": key, "base_url": server.URL},
					Metadata:   map[string]any{"account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "claude_device_ids": []string{strings.Repeat("a", 64)}},
				}
				executor := NewClaudeExecutor(&config.Config{})
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				request := cliproxyexecutor.Request{Model: "claude-sonnet-4-5", Payload: input}
				options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: input}
				var calls []gjson.Result
				if stream {
					result, executeErr := executor.ExecuteStream(ctx, auth, request, options)
					if executeErr != nil {
						t.Fatal(executeErr)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						payload := bytes.TrimSpace(chunk.Payload)
						payload = bytes.TrimSpace(bytes.TrimPrefix(payload, []byte("data:")))
						if call := gjson.GetBytes(payload, "choices.0.delta.tool_calls.0"); call.Exists() {
							calls = append(calls, call)
						}
					}
				} else {
					result, executeErr := executor.Execute(ctx, auth, request, options)
					if executeErr != nil {
						t.Fatal(executeErr)
					}
					calls = gjson.GetBytes(result.Payload, "choices.0.message.tool_calls").Array()
				}
				if len(calls) != len(names) {
					t.Fatalf("calls=%d, want %d", len(calls), len(names))
				}
				for index, call := range calls {
					if call.Get("function.name").String() != names[index] || call.Get("function.arguments").String() != `{"n":9007199254740993}` {
						t.Fatalf("client tool identity or arguments changed: %s", call.Raw)
					}
				}
				select {
				case body := <-requests:
					if !gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "tools.#").Int() != int64(len(names)) {
						t.Fatalf("upstream request changed: %s", body)
					}
					seen := make(map[string]bool)
					for _, tool := range gjson.GetBytes(body, "tools").Array() {
						name := tool.Get("name").String()
						if name == "" || len(name) > 64 || seen[name] || strings.ContainsAny(name, ".:") {
							t.Fatalf("upstream name invalid or conflicting: %q", name)
						}
						seen[name] = true
						if strings.HasPrefix(name, "mcp__") != oauth {
							t.Fatalf("OAuth alias boundary changed: %q", name)
						}
					}
				case <-ctx.Done():
					t.Fatal("upstream request was not observed")
				}
			})
		}
	}
}
