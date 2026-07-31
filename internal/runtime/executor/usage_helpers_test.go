package executor

import (
	"bytes"
	"context"
	"errors"
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

func registerTestUsagePlugin(t *testing.T, plugin usage.Plugin) {
	t.Helper()
	name := t.Name()
	usage.RegisterNamedPlugin(name, plugin)
	t.Cleanup(func() {
		usage.UnregisterNamedPlugin(name).Wait()
	})
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
	registerTestUsagePlugin(t, plugin)

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
	registerTestUsagePlugin(t, plugin)

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
	registerTestUsagePlugin(t, plugin)

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

func TestUsageReporterFallbackTotalDoesNotDoubleCountCache(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	reporter := &usageReporter{provider: "openai", model: "gpt-cache", alias: "gpt-cache"}
	reporter.publish(
		context.Background(),
		usage.Detail{InputTokens: 8, OutputTokens: 2, CachedTokens: 4, CacheReadTokens: 4},
	)

	select {
	case got := <-plugin.records:
		if got.Detail.TotalTokens != 10 {
			t.Fatalf("total tokens = %d, want 10: %#v", got.Detail.TotalTokens, got.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}

func TestUsageReporterAdditionalModelFallbackTotalDoesNotDoubleCountCache(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	reporter := &usageReporter{provider: "openai", model: "gpt-main", alias: "gpt-main"}
	reporter.publishAdditionalModel(
		context.Background(),
		"gpt-image-2",
		usage.Detail{InputTokens: 8, OutputTokens: 2, CachedTokens: 4, CacheReadTokens: 4},
	)

	select {
	case got := <-plugin.records:
		if got.Model != "gpt-image-2" {
			t.Fatalf("model = %q, want gpt-image-2", got.Model)
		}
		if got.Detail.TotalTokens != 10 {
			t.Fatalf("total tokens = %d, want 10: %#v", got.Detail.TotalTokens, got.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for additional-model usage record")
	}
}

func TestInteractionsUsageReporterDoesNotDoubleCountCacheAliases(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	detail := parseInteractionsUsage(
		[]byte(`{"usage":{"input_tokens":8,"output_tokens":2,"cached_tokens":4,"cacheReadTokens":4,"cacheWriteTokens":3}}`),
	)
	reporter := &usageReporter{provider: "gemini-interactions", model: "gemini-test", alias: "gemini-test"}
	reporter.publish(context.Background(), detail)

	select {
	case got := <-plugin.records:
		if got.Detail.TotalTokens != 10 {
			t.Fatalf("total tokens = %d, want 10: %#v", got.Detail.TotalTokens, got.Detail)
		}
		if got.Detail.CachedTokens != 4 || got.Detail.CacheReadTokens != 4 || got.Detail.CacheCreationTokens != 3 {
			t.Fatalf("cache detail = %#v, want cached/read/creation 4/4/3", got.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for interactions usage record")
	}
}

func TestUsageReporterIncludesRuntimeMetadata(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	ctx := usage.WithReasoningEffort(context.Background(), "medium")
	ctx = usage.WithServiceTier(ctx, "flex")
	reporter := newUsageReporter(ctx, testUsageExecutor{}, "gpt-5.4", nil)
	reporter.publish(ctx, usage.Detail{TotalTokens: 1, ResponseServiceTier: "default"})

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
		if got.RequestServiceTier != "flex" {
			t.Fatalf("request service tier = %q, want flex", got.RequestServiceTier)
		}
		if got.ResponseServiceTier != "default" {
			t.Fatalf("response service tier = %q, want default", got.ResponseServiceTier)
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

func TestUsageReporterSetTranslatedReasoningEffortPreservesContextServiceTier(t *testing.T) {
	reporter := &usageReporter{reasoningEffort: "medium", serviceTier: "flex"}

	reporter.SetTranslatedReasoningEffort([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "openai")

	if reporter.reasoningEffort != "" {
		t.Fatalf("reasoning effort = %q, want empty", reporter.reasoningEffort)
	}
	if reporter.serviceTier != "flex" {
		t.Fatalf("service tier = %q, want flex", reporter.serviceTier)
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

func TestParseServiceTierFields(t *testing.T) {
	claudeDetail := parseClaudeUsage(
		[]byte(`{"usage":{"input_tokens":1,"output_tokens":2,"service_tier":"priority"}}`),
	)
	if claudeDetail.ResponseServiceTier != "priority" {
		t.Fatalf("Claude response service tier = %q, want priority", claudeDetail.ResponseServiceTier)
	}

	openAIDetail := parseOpenAIUsage(
		[]byte(`{"service_tier":"default","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`),
	)
	if openAIDetail.ResponseServiceTier != "default" {
		t.Fatalf("OpenAI response service tier = %q, want default", openAIDetail.ResponseServiceTier)
	}

	codexDetail, ok := parseCodexUsage(
		[]byte(`{"response":{"service_tier":"priority","usage":{"input_tokens":4,"output_tokens":5,"total_tokens":9}}}`),
	)
	if !ok || codexDetail.ResponseServiceTier != "priority" {
		t.Fatalf("parseCodexUsage = (%+v, %v), want priority tier", codexDetail, ok)
	}

	interactionDetail := parseInteractionsUsage(
		[]byte(`{"interaction":{"service_tier":"flex","usage":{"input_tokens":6,"output_tokens":7,"total_tokens":13}}}`),
	)
	if interactionDetail.ResponseServiceTier != "flex" {
		t.Fatalf("Interactions response service tier = %q, want flex", interactionDetail.ResponseServiceTier)
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

func TestStreamUsageBufferMergesFinalUsageAndTier(t *testing.T) {
	tests := []struct {
		name   string
		chunks [][]byte
		want   usage.Detail
	}{
		{
			name: "tier then usage",
			chunks: [][]byte{
				[]byte(`data: {"service_tier":"default"}`),
				[]byte(`data: {"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`),
			},
			want: usage.Detail{InputTokens: 2, OutputTokens: 3, TotalTokens: 5, ResponseServiceTier: "default"},
		},
		{
			name: "usage then final tier",
			chunks: [][]byte{
				[]byte(`data: {"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`),
				[]byte(`data: {"service_tier":"priority"}`),
			},
			want: usage.Detail{InputTokens: 2, OutputTokens: 3, TotalTokens: 5, ResponseServiceTier: "priority"},
		},
		{
			name: "zero usage replaces previous usage",
			chunks: [][]byte{
				[]byte(`data: {"usage":{"input_tokens":9,"output_tokens":8,"total_tokens":17},"service_tier":"flex"}`),
				[]byte(`data: {"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`),
			},
			want: usage.Detail{ResponseServiceTier: "flex"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buffer streamUsageBuffer
			for _, chunk := range tt.chunks {
				buffer.observeOpenAIStream(chunk)
			}
			got, ok := buffer.detailValue()
			if !ok || got != tt.want {
				t.Fatalf("detail = %+v ok=%v, want %+v", got, ok, tt.want)
			}
		})
	}
}

func TestStreamUsageBufferHandlesSplitSSELines(t *testing.T) {
	var buffer streamUsageBuffer
	buffer.observeOpenAIStreamChunk(
		[]byte("data: {\"service_tier\":\"default\"}\ndata: {\"usage\":{\"input_tokens\":"),
	)
	buffer.observeOpenAIStreamChunk([]byte("4,\"output_tokens\":5,\"total_tokens\":9}}"))
	buffer.flushOpenAIStream()

	detail, ok := buffer.detailValue()
	if !ok || detail.InputTokens != 4 || detail.OutputTokens != 5 || detail.ResponseServiceTier != "default" {
		t.Fatalf("detail = %+v ok=%v", detail, ok)
	}
}

func TestStreamUsageBufferDropsOversizedLineAndRecovers(t *testing.T) {
	var buffer streamUsageBuffer
	buffer.observeOpenAIStreamChunk(bytes.Repeat([]byte("x"), maxStreamUsageLineBytes+1))
	if !buffer.pendingOverflow || len(buffer.pending) != 0 {
		t.Fatalf("overflow state = %v pending=%d, want overflow with empty pending", buffer.pendingOverflow, len(buffer.pending))
	}

	buffer.observeOpenAIStreamChunk(
		[]byte("\ndata: {\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5},\"service_tier\":\"priority\"}\n"),
	)
	detail, ok := buffer.detailValue()
	if !ok || detail.InputTokens != 2 || detail.OutputTokens != 3 || detail.ResponseServiceTier != "priority" {
		t.Fatalf("detail = %+v ok=%v", detail, ok)
	}
}

func TestStreamUsageBufferIgnoresInvalidUsageAndTierTypes(t *testing.T) {
	var buffer streamUsageBuffer
	buffer.observeOpenAIStream(
		[]byte(`data: {"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12},"service_tier":"default"}`),
	)
	buffer.observeOpenAIStream(
		[]byte(`data: {"usage":{"input_tokens":null,"output_tokens":"7","total_tokens":null},"service_tier":123}`),
	)

	detail, ok := buffer.detailValue()
	want := usage.Detail{InputTokens: 5, OutputTokens: 7, TotalTokens: 12, ResponseServiceTier: "default"}
	if !ok || detail != want {
		t.Fatalf("detail = %+v ok=%v, want %+v", detail, ok, want)
	}
}

func TestStreamUsageBufferParsesEscapedUsageAndServiceTierKeys(t *testing.T) {
	var buffer streamUsageBuffer
	buffer.observeOpenAIStream(
		[]byte("data: {\"\\u0075sage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5},\"service\\u005ftier\":\"priority\"}"),
	)

	detail, ok := buffer.detailValue()
	want := usage.Detail{InputTokens: 2, OutputTokens: 3, TotalTokens: 5, ResponseServiceTier: "priority"}
	if !ok || detail != want {
		t.Fatalf("detail = %+v ok=%v, want %+v", detail, ok, want)
	}
}

func TestStreamUsageBufferParsesEscapedServiceTierWithoutUsage(t *testing.T) {
	var buffer streamUsageBuffer
	buffer.observeOpenAIStream([]byte("data: {\"service\\u005ftier\":\"flex\",\"type\":\"response.created\"}"))

	detail, ok := buffer.detailValue()
	want := usage.Detail{ResponseServiceTier: "flex"}
	if !ok || detail != want {
		t.Fatalf("detail = %+v ok=%v, want %+v", detail, ok, want)
	}
}

func TestParseOpenAIUsageSkipsInvalidTokenAliases(t *testing.T) {
	detail := parseOpenAIUsage(
		[]byte(`{"usage":{"prompt_tokens":null,"input_tokens":5,"completion_tokens":"bad","output_tokens":7,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":null},"input_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":null},"output_tokens_details":{"reasoning_tokens":2}},"service_tier":123}`),
	)
	if detail.InputTokens != 5 || detail.OutputTokens != 7 || detail.TotalTokens != 12 ||
		detail.CachedTokens != 3 || detail.ReasoningTokens != 2 {
		t.Fatalf("detail = %+v", detail)
	}
	if detail.ResponseServiceTier != "" {
		t.Fatalf("response tier = %q, want empty", detail.ResponseServiceTier)
	}
}

func TestParseOpenAIUsageRejectsInvalidTokenNumbers(t *testing.T) {
	detail := parseOpenAIUsage(
		[]byte(`{"usage":{"prompt_tokens":-1,"input_tokens":5,"completion_tokens":1.5,"output_tokens":7,"total_tokens":9223372036854775808,"prompt_tokens_details":{"cached_tokens":-2},"input_tokens_details":{"cached_tokens":3}}}`),
	)
	if detail.InputTokens != 5 || detail.OutputTokens != 7 || detail.TotalTokens != 0 || detail.CachedTokens != 3 {
		t.Fatalf("detail = %+v", detail)
	}
}

func TestStreamUsageBufferKeepsValidUsageAfterNegativeUpdate(t *testing.T) {
	var buffer streamUsageBuffer
	buffer.observeOpenAIStream([]byte(`data: {"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}`))
	buffer.observeOpenAIStream([]byte(`data: {"usage":{"input_tokens":-1,"output_tokens":-2,"total_tokens":-3}}`))

	detail, ok := buffer.detailValue()
	want := usage.Detail{InputTokens: 5, OutputTokens: 7, TotalTokens: 12}
	if !ok || detail != want {
		t.Fatalf("detail = %+v ok=%v, want %+v", detail, ok, want)
	}
}

func TestUsageReporterAdditionalModelPublishesZeroUsage(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	reporter := &usageReporter{provider: "codex", model: "gpt-main", alias: "gpt-main"}
	reporter.publishAdditionalModel(
		context.Background(),
		"gpt-image-2",
		usage.Detail{ResponseServiceTier: "priority"},
	)

	select {
	case got := <-plugin.records:
		if got.Model != "gpt-image-2" || got.ResponseServiceTier != "priority" {
			t.Fatalf("record = %#v", got)
		}
		if got.Detail != (usage.Detail{ResponseServiceTier: "priority"}) {
			t.Fatalf("detail = %#v", got.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for zero additional-model usage record")
	}
}

func TestUsageReporterPreservesServiceTierForNonStringPayloadValue(t *testing.T) {
	reporter := &usageReporter{serviceTier: "priority"}
	reporter.SetTranslatedReasoningEffort([]byte(`{"service_tier":123}`), "openai-response")
	if reporter.serviceTier != "priority" {
		t.Fatalf("service tier = %q, want priority", reporter.serviceTier)
	}
}

func TestStreamUsageBufferIgnoresIrrelevantAndInvalidChunks(t *testing.T) {
	var buffer streamUsageBuffer
	buffer.observeOpenAIStream([]byte(`data: {"content":"usage and service_tier"}`))
	buffer.observeOpenAIStream([]byte(`data: {"usage":`))
	buffer.observeOpenAIStream([]byte(`data: {"usage":null}`))
	if detail, ok := buffer.detailValue(); ok {
		t.Fatalf("detail = %+v ok=true, want empty", detail)
	}
}

func TestStreamUsageFailureWinsBeforeDeferredSuccess(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	ctx := context.Background()
	reporter := &usageReporter{provider: "openai", model: "gpt-test"}
	var buffer streamUsageBuffer
	buffer.observeOpenAIStream(
		[]byte(`data: {"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5},"service_tier":"default"}`),
	)

	reporter.publishFailure(ctx, errors.New("stream read failed"))
	buffer.publish(ctx, reporter)
	reporter.ensurePublished(ctx)

	select {
	case got := <-plugin.records:
		if !got.Failed {
			t.Fatalf("Failed = false, want true: %#v", got)
		}
		if got.Detail != (usage.Detail{}) {
			t.Fatalf("Detail = %#v, want zero detail", got.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
}

func TestParseOpenAIUsageNormalizesCacheTokenAliases(t *testing.T) {
	tests := []struct {
		name    string
		usage   string
		read    int64
		created int64
	}{
		{
			name:    "prompt details write alias",
			usage:   `{"usage":{"prompt_tokens":10,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":5}}}`,
			read:    3,
			created: 5,
		},
		{
			name:    "input details creation alias",
			usage:   `{"usage":{"input_tokens":8,"output_tokens":2,"input_tokens_details":{"cached_tokens":4,"cache_creation_tokens":6}}}`,
			read:    4,
			created: 6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detail := parseOpenAIUsage([]byte(tt.usage))
			if detail.CachedTokens != tt.read || detail.CacheReadTokens != tt.read {
				t.Fatalf("cache read detail = %#v, want cached/read %d", detail, tt.read)
			}
			if detail.CacheCreationTokens != tt.created {
				t.Fatalf("cache creation tokens = %d, want %d", detail.CacheCreationTokens, tt.created)
			}
		})
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

func TestClaudeStreamUsageBufferMergesMessageStartAndDelta(t *testing.T) {
	var buffer claudeStreamUsageBuffer
	buffer.observe(
		[]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":3,"cache_creation_input_tokens":4,"service_tier":"priority"}}}`),
	)
	buffer.observe([]byte(`data: {"type":"message_delta","usage":{"output_tokens":5,"service_tier":"priority"}}`))

	detail, ok := buffer.detailValue()
	if !ok {
		t.Fatal("detailValue() ok = false, want true")
	}
	if detail.InputTokens != 10 || detail.OutputTokens != 5 {
		t.Fatalf("input/output tokens = %d/%d, want 10/5", detail.InputTokens, detail.OutputTokens)
	}
	if detail.CacheReadTokens != 3 || detail.CacheCreationTokens != 4 || detail.CachedTokens != 3 {
		t.Fatalf("cache detail = %#v, want read/creation/cached 3/4/3", detail)
	}
	if detail.TotalTokens != 22 {
		t.Fatalf("total tokens = %d, want 22", detail.TotalTokens)
	}
	if detail.ResponseServiceTier != "priority" {
		t.Fatalf("response service tier = %q, want priority", detail.ResponseServiceTier)
	}
}

func TestClaudeStreamUsageBufferPreservesValidStateAfterMalformedAndOverflowingEvents(t *testing.T) {
	var buffer claudeStreamUsageBuffer
	buffer.observe(
		[]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":9223372036854775807}}}`),
	)
	buffer.observe([]byte(`data: {malformed`))
	buffer.observe([]byte(`data: {"type":"message_delta","usage":{"output_tokens":1}}`))

	detail, ok := buffer.detailValue()
	if !ok {
		t.Fatal("detailValue() ok = false, want true")
	}
	if detail.InputTokens != int64(^uint64(0)>>1) || detail.OutputTokens != 0 || detail.TotalTokens != int64(^uint64(0)>>1) {
		t.Fatalf("detail = %#v, want previous valid MaxInt64 state", detail)
	}
}

func TestClaudeStreamUsageBufferFailurePublishesAccumulatedDetailOnce(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)

	reporter := &usageReporter{provider: "claude", model: "claude-test", alias: "claude-test"}
	var buffer claudeStreamUsageBuffer
	buffer.observe(
		[]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":3}}}`),
	)
	buffer.observe([]byte(`data: {"type":"message_delta","usage":{"output_tokens":5}}`))
	buffer.publishFailure(context.Background(), reporter, errors.New("stream failed"))
	buffer.publish(context.Background(), reporter)

	select {
	case got := <-plugin.records:
		if !got.Failed || got.Completed || got.Fail.Body != "stream failed" {
			t.Fatalf("record outcome = %#v, want failed stream", got)
		}
		if got.Detail.InputTokens != 10 || got.Detail.OutputTokens != 5 || got.Detail.CacheReadTokens != 3 || got.Detail.TotalTokens != 18 {
			t.Fatalf("record detail = %#v, want accumulated 10/5/3/18", got.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
	select {
	case got := <-plugin.records:
		t.Fatalf("received duplicate usage record: %#v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestParseInteractionsUsageOpenAIStyle(t *testing.T) {
	detail := parseInteractionsUsage(
		[]byte(`{"usage":{"input_tokens":3,"output_tokens":4,"reasoning_tokens":5,"total_tokens":12,"cached_tokens":2,"cache_read_tokens":7,"cache_creation_tokens":8}}`),
	)
	if detail.InputTokens != 3 || detail.OutputTokens != 4 || detail.ReasoningTokens != 5 || detail.TotalTokens != 12 {
		t.Fatalf("detail = %#v, want input/output/reasoning/total 3/4/5/12", detail)
	}
	if detail.CachedTokens != 2 || detail.CacheReadTokens != 7 || detail.CacheCreationTokens != 8 {
		t.Fatalf("cache detail = %#v, want cached/read/creation 2/7/8", detail)
	}
}

func TestParseInteractionsUsageNestedMetadata(t *testing.T) {
	detail := parseInteractionsUsage(
		[]byte(`{"interaction":{"metadata":{"usage":{"prompt_tokens":10,"completion_tokens":20,"total_cached_tokens":6}}}}`),
	)
	if detail.InputTokens != 10 {
		t.Fatalf("input tokens = %d, want 10", detail.InputTokens)
	}
	if detail.OutputTokens != 20 {
		t.Fatalf("output tokens = %d, want 20", detail.OutputTokens)
	}
	if detail.CachedTokens != 6 || detail.CacheReadTokens != 6 {
		t.Fatalf("cache read detail = %#v, want cached/read 6", detail)
	}
	if detail.TotalTokens != 30 {
		t.Fatalf("total tokens = %d, want 30", detail.TotalTokens)
	}
}

func TestParseInteractionsUsageSkipsInvalidEarlierUsageNode(t *testing.T) {
	detail := parseInteractionsUsage(
		[]byte(`{"interaction":{"usage":"invalid"},"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`),
	)
	if detail.InputTokens != 3 || detail.OutputTokens != 4 || detail.TotalTokens != 7 {
		t.Fatalf("detail = %#v, want fallback usage 3/4/7", detail)
	}
}

func TestParseInteractionsUsageGeminiStyle(t *testing.T) {
	detail := parseInteractionsUsage(
		[]byte(`{"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":13,"thoughtsTokenCount":17,"totalTokenCount":41,"cachedContentTokenCount":19}}`),
	)
	if detail.InputTokens != 11 {
		t.Fatalf("input tokens = %d, want 11", detail.InputTokens)
	}
	if detail.OutputTokens != 13 {
		t.Fatalf("output tokens = %d, want 13", detail.OutputTokens)
	}
	if detail.ReasoningTokens != 17 {
		t.Fatalf("reasoning tokens = %d, want 17", detail.ReasoningTokens)
	}
	if detail.TotalTokens != 41 {
		t.Fatalf("total tokens = %d, want 41", detail.TotalTokens)
	}
	if detail.CachedTokens != 19 || detail.CacheReadTokens != 19 {
		t.Fatalf("cache read detail = %#v, want cached/read 19", detail)
	}
}

func TestParseInteractionsUsageCacheWriteAliasesAndTotal(t *testing.T) {
	tests := []struct {
		name      string
		usage     string
		wantTotal int64
	}{
		{
			name:      "snake case without explicit read",
			usage:     `{"usage":{"input_tokens":10,"output_tokens":4,"reasoning_tokens":2,"cached_tokens":3,"cache_write_tokens":5}}`,
			wantTotal: 16,
		},
		{
			name:      "camel case with explicit read",
			usage:     `{"usage":{"input_tokens":10,"output_tokens":4,"reasoning_tokens":2,"cached_tokens":3,"cacheReadTokens":7,"cacheWriteTokens":5}}`,
			wantTotal: 16,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			detail := parseInteractionsUsage([]byte(tt.usage))
			if detail.CachedTokens != 3 || detail.CacheCreationTokens != 5 {
				t.Fatalf("cache detail = %#v, want cached 3 and creation 5", detail)
			}
			if detail.CacheReadTokens == 0 {
				t.Fatalf("cache read tokens were not normalized: %#v", detail)
			}
			if detail.TotalTokens != tt.wantTotal {
				t.Fatalf("total tokens = %d, want %d: %#v", detail.TotalTokens, tt.wantTotal, detail)
			}
		})
	}
}

func TestParseInteractionsStreamUsageDataFrame(t *testing.T) {
	line := []byte(`data: {"metadata":{"total_usage":{"input_tokens":5,"output_tokens":6}}}`)
	detail, ok := parseInteractionsStreamUsage(line)
	if !ok {
		t.Fatal("parseInteractionsStreamUsage ok = false, want true")
	}
	if detail.InputTokens != 5 || detail.OutputTokens != 6 || detail.TotalTokens != 11 {
		t.Fatalf("detail = %#v, want input/output/total 5/6/11", detail)
	}
}

func TestParseInteractionsStreamUsagePreservesZeroUsageAndTier(t *testing.T) {
	detail, hasUsage, hasTier := parseInteractionsStreamUsageState(
		[]byte(`data: {"interaction":{"service_tier":"default","usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`),
	)
	if !hasUsage || !hasTier {
		t.Fatalf("state = (%+v, %v, %v), want usage and tier", detail, hasUsage, hasTier)
	}
	if detail.ResponseServiceTier != "default" {
		t.Fatalf("response tier = %q, want default", detail.ResponseServiceTier)
	}
}

func TestParseInteractionsStreamUsageIgnoresNonUsage(t *testing.T) {
	if detail, ok := parseInteractionsStreamUsage([]byte(`data: {"delta":"ok"}`)); ok {
		t.Fatalf("parseInteractionsStreamUsage = (%#v, true), want false", detail)
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
