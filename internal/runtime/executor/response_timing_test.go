package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestResponseTokenEvents(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		want          bool
	}{
		{"empty", "", false},
		{"keepalive", ": keepalive", false},
		{"malformed", `data: {"type":"response.output_text.delta","delta":"hello"`, false},
		{"role", `{"choices":[{"delta":{"role":"assistant","content":""}}]}`, false},
		{"chat text", `{"choices":[{"delta":{"content":"hello"}}]}`, true},
		{"chat reasoning", `{"choices":[{"delta":{"reasoning":"thinking"}}]}`, true},
		{"chat custom tool", `{"choices":[{"delta":{"tool_calls":[{"custom":{"input":"patch"}}]}}]}`, true},
		{"chat finish", `{"choices":[{"finish_reason":"stop"}]}`, true},
		{"metadata", `{"modelVersion":"gemini-test","usageMetadata":{"promptTokenCount":5}}`, false},
		{"gemini empty", `{"candidates":[{"content":{"parts":[]}}]}`, false},
		{"gemini text", `{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`, true},
		{"gemini thoughtText", `{"candidates":[{"content":{"parts":[{"thoughtText":"thinking"}]}}]}`, true},
		{"gemini thought string", `{"candidates":[{"content":{"parts":[{"thought":"thinking"}]}}]}`, true},
		{"gemini thought flag only", `{"candidates":[{"content":{"parts":[{"thought":true}]}}]}`, false},
		{"gemini function", `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup"}}]}}]}`, true},
		{"gemini image", `{"response":{"candidates":[{"content":{"parts":[{"inlineData":{"data":"aGk="}}]}}]}}`, true},
		{"gemini finish", `{"candidates":[{"finishReason":"STOP"}]}`, true},
		{"wrapped error", `{"response":{"error":{"message":"failed"}}}`, true},
		{"claude start metadata", `{"type":"message_start","message":{"model":"claude-test"}}`, false},
		{"claude ping", `{"type":"ping"}`, false},
		{"claude empty delta", `{"type":"content_block_delta","delta":{"text":""}}`, false},
		{"claude text", `{"type":"content_block_delta","delta":{"text":"hello"}}`, true},
		{"claude thinking", `{"type":"content_block_delta","delta":{"thinking":"thinking"}}`, true},
		{"claude arguments", `{"type":"content_block_delta","delta":{"partial_json":"{}"}}`, true},
		{"claude signature", `{"type":"content_block_delta","delta":{"signature":"signature"}}`, true},
		{"claude initial text", `{"type":"content_block_start","content_block":{"text":"hello"}}`, true},
		{"claude terminal delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, true},
		{"claude stop", `{"type":"message_stop"}`, true},
		{"claude complete tool", `{"content":[{"type":"tool_use","name":"lookup"}]}`, true},
		{"responses metadata", `{"type":"response.created","response":{"model":"gpt-test"}}`, false},
		{"responses empty delta", `{"type":"response.output_text.delta","delta":""}`, false},
		{"responses text", `{"type":"response.output_text.delta","delta":"hello"}`, true},
		{"responses custom tool", `{"type":"response.custom_tool_call_input.delta","delta":"patch"}`, true},
		{"responses reasoning", `{"type":"response.reasoning_text.delta","delta":"thinking"}`, true},
		{"responses audio", `{"type":"response.audio.delta","data":"aGk="}`, true},
		{"responses image", `{"type":"response.image_generation_call.partial_image","partial_image_b64":"aGk="}`, true},
		{"responses shell", `{"type":"response.shell_call_command.added","command":"pwd"}`, true},
		{"responses text done", `{"type":"response.output_text.done","text":"hello"}`, true},
		{"responses refusal", `{"type":"response.refusal.done","refusal":"refused"}`, true},
		{"responses arguments", `{"type":"response.function_call_arguments.done","arguments":"{}"}`, true},
		{"responses custom input", `{"type":"response.custom_tool_call_input.done","input":"patch"}`, true},
		{"responses code", `{"type":"response.code_interpreter_call_code.done","code":"print(1)"}`, true},
		{"responses summary part", `{"type":"response.reasoning_summary_part.done","part":{"text":"summary"}}`, true},
		{"responses refusal part", `{"type":"response.content_part.done","part":{"refusal":"refused"}}`, true},
		{"responses tool item", `{"type":"response.output_item.done","item":{"type":"function_call","arguments":"{}"}}`, true},
		{"responses text item", `{"type":"response.output_item.done","item":{"type":"message","content":[{"text":"hello"}]}}`, true},
		{"responses unknown item", `{"type":"response.output_item.done","item":{"type":"unknown","arguments":"metadata"}}`, false},
		{"responses message arguments", `{"type":"response.output_item.done","item":{"type":"message","arguments":"metadata"}}`, false},
		{"responses empty item", `{"type":"response.output_item.done","item":{"type":"message","content":[]}}`, false},
		{"responses completed", `{"type":"response.completed"}`, true},
		{"responses failed", `{"type":"response.failed"}`, true},
		{"multiline SSE", "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\ndata: \"delta\":\"hello\"}\n\n", true},
		{"CRLF SSE", "event: response.output_text.delta\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\r\n\r\n", true},
		{"raw done", "[DONE]", true},
		{"SSE done", "data: [DONE]\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isResponseTokenEvent([]byte(tc.payload)); got != tc.want {
				t.Fatalf("isResponseTokenEvent(%q) = %t, want %t", tc.payload, got, tc.want)
			}
		})
	}
}

func TestResponseTokenObservationPreservesFirstEvent(t *testing.T) {
	//lint:ignore SA1012 验证空 context 不创建计时记录
	observeResponseTokenEvent(nil, []byte(`{"type":"response.output_text.delta","delta":"hello"}`))
	synctest.Test(t, func(t *testing.T) {
		ctx := logging.WithResponseTimingHolder(context.Background())
		start := time.Now()
		time.Sleep(250 * time.Millisecond)
		logging.MarkFirstResponseByte(ctx)
		observeResponseTokenEvent(ctx, []byte(`{"type":"response.created","response":{"model":"gpt-test"}}`))
		if !logging.GetFirstResponseTokenAt(ctx).IsZero() {
			t.Fatal("metadata recorded an output token")
		}
		time.Sleep(84 * time.Second)
		observeResponseTokenEvent(ctx, []byte(`{"type":"response.output_text.delta","delta":"hello"}`))
		first := logging.GetFirstResponseTokenAt(ctx)
		time.Sleep(2 * time.Second)
		observeResponseTokenEvent(ctx, []byte(`{"type":"response.completed"}`))
		if first.IsZero() || logging.GetFirstResponseTokenAt(ctx) != first {
			t.Fatal("first effective output was missing or replaced")
		}
		reporter := &usageReporter{requestedAt: start}
		record := reporter.finalizeRecord(ctx, usage.Record{RequestedAt: start})
		if record.TimeToFirstByteMs != 84250 {
			t.Fatalf("published effective TTFT = %d, want 84250", record.TimeToFirstByteMs)
		}
	})
}

func TestProviderEffectiveTokenTimingAtExecutors(t *testing.T) {
	cfg := &config.Config{}
	for _, tc := range []struct {
		name, model, request, metadata, token, terminal string
		format                                          sdktranslator.Format
		execute                                         func(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
	}{
		{
			name: "claude", model: "claude-sonnet-4-6", format: sdktranslator.FormatClaude,
			request:  `{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
			metadata: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-6\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			token:    "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n",
			terminal: "data: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n",
			execute:  NewClaudeExecutor(cfg).ExecuteStream,
		},
		{
			name: "codex", model: "gpt-5.4-mini", format: sdktranslator.FormatCodex,
			request:  `{"model":"gpt-5.4-mini","input":"hello"}`,
			metadata: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"model\":\"gpt-5.4-mini\"}}\n\n",
			token:    "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n",
			terminal: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"model\":\"gpt-5.4-mini\",\"output\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":1,\"total_tokens\":6}}}\n\n",
			execute:  newCodexExecutor(cfg).ExecuteStream,
		},
		{
			name: "xai", model: "grok-test", format: sdktranslator.FormatOpenAIResponse,
			request:  `{"model":"grok-test","input":"hello"}`,
			metadata: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"model\":\"grok-test\"}}\n\n",
			token:    "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n",
			terminal: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"model\":\"grok-test\",\"output\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":1,\"total_tokens\":6}}}\n\n",
			execute:  newXAIExecutor(cfg).ExecuteStream,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &captureUsagePlugin{records: make(chan usage.Record, 4)}
			registerTestUsagePlugin(t, capture)
			usage.StartDefault(context.Background())
			synctest.Test(t, func(t *testing.T) {
				transport := openAICompatTTFTRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					time.Sleep(250 * time.Millisecond)
					reader, writer := io.Pipe()
					go func() {
						defer func() { _ = writer.Close() }()
						if _, err := io.WriteString(writer, tc.metadata); err != nil {
							return
						}
						time.Sleep(84 * time.Second)
						if _, err := io.WriteString(writer, tc.token); err != nil {
							return
						}
						time.Sleep(2 * time.Second)
						_, _ = io.WriteString(writer, tc.terminal)
					}()
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader, Request: req}, nil
				})
				ctx := util.WithRoundTripper(logging.WithResponseTimingHolder(context.Background()), http.RoundTripper(transport))
				auth := &cliproxyauth.Auth{Provider: tc.name, Attributes: map[string]string{"api_key": "synthetic", "base_url": "http://example.invalid/v1", "using_api": "true"}}
				result, err := tc.execute(ctx, auth, cliproxyexecutor.Request{Model: tc.model, Payload: []byte(tc.request)},
					cliproxyexecutor.Options{SourceFormat: tc.format, ResponseFormat: tc.format, Stream: true})
				if err != nil {
					t.Fatalf("ExecuteStream: %v", err)
				}
				var delivered strings.Builder
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream: %v", chunk.Err)
					}
					delivered.Write(chunk.Payload)
				}
				if !strings.Contains(delivered.String(), "hello") {
					t.Fatalf("effective output was not delivered: %s", delivered.String())
				}
			})
			record := awaitOpenAICompatUsage(t, capture)
			if got := time.Duration(record.TimeToFirstByteMs) * time.Millisecond; got != 84*time.Second+250*time.Millisecond {
				t.Fatalf("effective-token TTFT = %v, want 84.25s", got)
			}
			if record.Failed || record.Detail.TotalTokens != 6 {
				t.Fatalf("published stream record = %#v", record)
			}
		})
	}
}
