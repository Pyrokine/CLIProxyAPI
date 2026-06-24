package executor

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

type captureUsagePlugin struct {
	records chan usage.Record
}

type testUsageExecutor struct{}

func (testUsageExecutor) Identifier() string { return "test-provider" }

func (p *captureUsagePlugin) HandleUsage(_ context.Context, record usage.Record) {
	select {
	case p.records <- record:
	default:
	}
}

func TestResponseTimingStartsAtBodyChunk(t *testing.T) {
	ctx := logging.WithResponseTimingHolder(context.Background())
	recordAPIResponseMetadata(ctx, nil, http.StatusOK, http.Header{"X-Test": []string{"1"}})
	if firstByte := logging.GetFirstResponseByteAt(ctx); !firstByte.IsZero() {
		t.Fatalf("first response byte after headers = %s, want zero", firstByte)
	}

	appendAPIResponseChunk(ctx, nil, []byte("data"))
	if firstByte := logging.GetFirstResponseByteAt(ctx); firstByte.IsZero() {
		t.Fatal("first response byte should be marked after body chunk")
	}
}

func TestUsageReporterAttachesResponseHeaders(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	usage.RegisterPlugin(plugin)

	reporter := &usageReporter{provider: "openai", model: "gpt-test"}
	ctx := logging.WithResponseHeadersHolder(context.Background())
	logging.SetResponseHeaders(ctx, http.Header{"X-Request-Id": []string{"req-1"}})
	reporter.publishRecord(ctx, usage.Record{Detail: usage.Detail{TotalTokens: 1}})

	select {
	case got := <-plugin.records:
		if header := got.ResponseHeaders.Get("X-Request-Id"); header != "req-1" {
			t.Fatalf("published X-Request-Id = %q, want req-1", header)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}

func TestUsageReporterMarksFailedRecordIncomplete(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	usage.RegisterPlugin(plugin)

	reporter := &usageReporter{
		provider: "openai", model: "gpt-test", requestedAt: time.Now().Add(-100 * time.Millisecond),
	}
	ctx := logging.WithResponseTimingHolder(context.Background())
	logging.MarkFirstResponseByte(ctx)
	reporter.publishRecord(
		ctx, usage.Record{RequestedAt: reporter.requestedAt, Failed: true, Detail: usage.Detail{TotalTokens: 1}},
	)

	select {
	case got := <-plugin.records:
		if got.Completed {
			t.Fatal("failed usage record was marked completed")
		}
		if got.TimeToFirstByteMs <= 0 {
			t.Fatalf("TimeToFirstByteMs = %d, want > 0", got.TimeToFirstByteMs)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}

func TestUsageReporterRecordsZeroUsage(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	usage.RegisterPlugin(plugin)

	reporter := &usageReporter{provider: "openai", model: "gpt-zero", alias: "gpt-zero"}
	reporter.publish(context.Background(), usage.Detail{})

	select {
	case got := <-plugin.records:
		if got.Model != "gpt-zero" {
			t.Fatalf("model = %q, want gpt-zero", got.Model)
		}
		if got.Failed {
			t.Fatal("zero usage record was marked failed")
		}
		if got.Detail.TotalTokens != 0 || got.Detail.InputTokens != 0 || got.Detail.OutputTokens != 0 {
			t.Fatalf("detail = %#v, want all token counts to remain zero", got.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for zero usage record")
	}
}

func TestUsageReporterIncludesRuntimeMetadata(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	usage.RegisterPlugin(plugin)

	ctx := usage.WithReasoningEffort(context.Background(), "medium")
	ctx = usage.WithServiceTier(ctx, "flex")
	reporter := newUsageReporter(ctx, testUsageExecutor{}, "gpt-5.4", nil)
	reporter.publish(ctx, usage.Detail{TotalTokens: 1})

	select {
	case got := <-plugin.records:
		if got.Provider != "test-provider" {
			t.Fatalf("provider = %q, want %q", got.Provider, "test-provider")
		}
		if got.ExecutorType != "testUsageExecutor" {
			t.Fatalf("executor type = %q, want %q", got.ExecutorType, "testUsageExecutor")
		}
		if got.ReasoningEffort != "medium" {
			t.Fatalf("reasoning effort = %q, want %q", got.ReasoningEffort, "medium")
		}
		if got.ServiceTier != "flex" {
			t.Fatalf("service tier = %q, want %q", got.ServiceTier, "flex")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}

func TestUsageReporterSetTranslatedReasoningEffortOverridesContext(t *testing.T) {
	reporter := &usageReporter{reasoningEffort: "medium"}

	reporter.SetTranslatedReasoningEffort([]byte(`{"reasoning_effort":"high"}`), "openai")

	if reporter.reasoningEffort != "high" {
		t.Fatalf("reasoning effort = %q, want high", reporter.reasoningEffort)
	}
}

func TestUsageReporterSetTranslatedReasoningEffortUsesTranslatedPayload(t *testing.T) {
	reporter := &usageReporter{reasoningEffort: "medium", serviceTier: "flex"}

	reporter.SetTranslatedReasoningEffort([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "openai")

	if reporter.reasoningEffort != "" {
		t.Fatalf("reasoning effort = %q, want empty", reporter.reasoningEffort)
	}
	if reporter.serviceTier != usage.DefaultServiceTier {
		t.Fatalf("service tier = %q, want %q", reporter.serviceTier, usage.DefaultServiceTier)
	}
}

func TestUsageReporterSetTranslatedReasoningEffortExtractsServiceTier(t *testing.T) {
	reporter := &usageReporter{serviceTier: usage.DefaultServiceTier}

	reporter.SetTranslatedReasoningEffort(
		[]byte(`{"reasoning":{"effort":"high"},"service_tier":"priority"}`), "openai-response",
	)

	if reporter.reasoningEffort != "high" {
		t.Fatalf("reasoning effort = %q, want high", reporter.reasoningEffort)
	}
	if reporter.serviceTier != "priority" {
		t.Fatalf("service tier = %q, want priority", reporter.serviceTier)
	}
}

func TestParseOpenAIUsageChatCompletions(t *testing.T) {
	data := []byte(
		`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":5}}}`,
	)
	detail := parseOpenAIUsage(data)
	if detail.InputTokens != 1 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 1)
	}
	if detail.OutputTokens != 2 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 2)
	}
	if detail.TotalTokens != 3 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 3)
	}
	if detail.CachedTokens != 4 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 4)
	}
	if detail.ReasoningTokens != 5 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 5)
	}
}

func TestParseOpenAIUsageResponses(t *testing.T) {
	data := []byte(
		`{"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_tokens_details":{"cached_tokens":7},"output_tokens_details":{"reasoning_tokens":9}}}`,
	)
	detail := parseOpenAIUsage(data)
	if detail.InputTokens != 10 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 10)
	}
	if detail.OutputTokens != 20 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 20)
	}
	if detail.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 30)
	}
	if detail.CachedTokens != 7 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7)
	}
	if detail.ReasoningTokens != 9 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 9)
	}
}

func TestParseOpenAIUsageIgnoresNullUsage(t *testing.T) {
	detail := parseOpenAIUsage([]byte(`{"usage":null}`))
	if detail != (usage.Detail{}) {
		t.Fatalf("detail = %+v, want zero detail", detail)
	}
}

func TestParseOpenAIStreamUsageIgnoresNullUsage(t *testing.T) {
	line := []byte(`data: {"id":"chunk_1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}],"usage":null}`)
	if detail, ok := parseOpenAIStreamUsage(line); ok {
		t.Fatalf("parseOpenAIStreamUsage = (%+v, true), want false for null usage", detail)
	}
}

func TestParseOpenAIStreamUsageResponsesFields(t *testing.T) {
	line := []byte(`data: {"id":"chunk_1","object":"chat.completion.chunk","choices":[],"usage":{"input_tokens":8,"output_tokens":5,"total_tokens":13,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`)
	detail, ok := parseOpenAIStreamUsage(line)
	if !ok {
		t.Fatal("parseOpenAIStreamUsage ok = false, want true")
	}
	if detail.InputTokens != 8 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 8)
	}
	if detail.OutputTokens != 5 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 5)
	}
	if detail.TotalTokens != 13 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 13)
	}
	if detail.CachedTokens != 3 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 3)
	}
	if detail.ReasoningTokens != 2 {
		t.Fatalf("reasoning tokens = %d, want %d", detail.ReasoningTokens, 2)
	}
}

func TestParseClaudeUsageIncludesCacheTokensInTotal(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":3085,"output_tokens":253,"cache_read_input_tokens":7,"cache_creation_input_tokens":19514}}`)
	detail := parseClaudeUsage(data)
	if detail.InputTokens != 3085 {
		t.Fatalf("input tokens = %d, want %d", detail.InputTokens, 3085)
	}
	if detail.OutputTokens != 253 {
		t.Fatalf("output tokens = %d, want %d", detail.OutputTokens, 253)
	}
	if detail.CacheReadTokens != 7 {
		t.Fatalf("cache read tokens = %d, want %d", detail.CacheReadTokens, 7)
	}
	if detail.CacheCreationTokens != 19514 {
		t.Fatalf("cache creation tokens = %d, want %d", detail.CacheCreationTokens, 19514)
	}
	if detail.CachedTokens != 7 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 7)
	}
	if detail.TotalTokens != 22859 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 22859)
	}
}

func TestParseClaudeUsageFallsBackCachedTokensToCacheCreation(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":3085,"output_tokens":253,"cache_creation_input_tokens":19514}}`)
	detail := parseClaudeUsage(data)
	if detail.CachedTokens != 19514 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 19514)
	}
	if detail.TotalTokens != 22852 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 22852)
	}
}

func TestParseClaudeStreamUsageIncludesCacheTokensInTotal(t *testing.T) {
	line := []byte(`data: {"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}`)
	detail, ok := parseClaudeStreamUsage(line)
	if !ok {
		t.Fatal("parseClaudeStreamUsage() ok = false, want true")
	}
	if detail.TotalTokens != 22 {
		t.Fatalf("total tokens = %d, want %d", detail.TotalTokens, 22)
	}
	if detail.CachedTokens != 3 {
		t.Fatalf("cached tokens = %d, want %d", detail.CachedTokens, 3)
	}
}

func TestMaskAPIKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{
			name: "normal key with dashes",
			key:  "sk-pyro-abc123def456",
			want: "sk-pyro-...f456",
		},
		{
			name: "short key 3 chars",
			key:  "abc",
			want: "***",
		},
		{
			name: "empty string",
			key:  "",
			want: "***",
		},
		{
			name: "exactly 8 chars",
			key:  "abcdefgh",
			want: "***",
		},
		{
			name: "exactly 9 chars no dash",
			key:  "abcdefghi",
			want: "abcd...fghi",
		},
		{
			name: "key with no dash long",
			key:  "abcdefghij1234",
			want: "abcd...1234",
		},
		{
			name: "dash near end",
			key:  "sk-pyro-xy-ab",
			want: "sk-p...y-ab",
		},
		{
			name: "9 chars with dash at position 2",
			key:  "sk-abcdef",
			want: "sk-...cdef",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				got := maskAPIKey(tt.key)
				if got != tt.want {
					t.Fatalf("maskAPIKey(%q) = %q, want %q", tt.key, got, tt.want)
				}
			},
		)
	}
}
