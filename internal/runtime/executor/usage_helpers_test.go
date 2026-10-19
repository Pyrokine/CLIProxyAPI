package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coresession "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/session"
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
	t.Cleanup(
		func() {
			usage.UnregisterNamedPlugin(name).Wait()
		},
	)
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

func TestUsageReporterPreservesHTTPAndExecutionIDs(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	ctx := logging.WithRequestID(context.Background(), "http-request-id")
	ctx = usage.WithExecutionID(ctx, "model-execution-id")
	reporter := newUsageReporter(ctx, testUsageExecutor{}, "gpt-test", nil)
	reporter.publish(ctx, usage.Detail{})

	select {
	case got := <-plugin.records:
		if got.RequestID != "http-request-id" || got.ExecutionID != "model-execution-id" {
			t.Fatalf("usage IDs = (%q, %q), want separate HTTP and execution IDs", got.RequestID, got.ExecutionID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}

func TestUsageReporterIncludesRuntimeMetadata(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	ctx := usage.WithReasoningEffort(context.Background(), "medium")
	ctx = usage.WithServiceTier(ctx, "flex")
	ctx = usage.WithStream(ctx, true)
	ctx = logging.WithClientRequestMetadata(ctx, logging.ClientRequestMetadata{
		SessionID:       "slot:worker",
		ParentSessionID: "slot:root",
	})
	reporter := newUsageReporter(ctx, testUsageExecutor{}, "gpt-5.4", &cliproxyauth.Auth{
		Attributes: map[string]string{"base_url": " https://selected.example/v1 "},
	})
	reporter.publish(ctx, usage.Detail{TotalTokens: 1, ResponseServiceTier: "default"})

	select {
	case got := <-plugin.records:
		if got.Provider != "test-provider" {
			t.Fatalf("provider = %q, want %q", got.Provider, "test-provider")
		}
		if got.BaseURL != "https://selected.example/v1" || !got.Stream {
			t.Fatalf("target metadata = baseURL:%q stream:%t", got.BaseURL, got.Stream)
		}
		if got.SessionID != coresession.NormalizeToCanonicalUUID("slot:worker") ||
			got.ParentSessionID != coresession.NormalizeToCanonicalUUID("slot:root") {
			t.Fatalf("session hierarchy = (%q, %q)", got.SessionID, got.ParentSessionID)
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

	reporter.setTranslatedReasoningEffort([]byte(`{"reasoning_effort":"high"}`), "openai")

	if reporter.reasoningEffort != "high" {
		t.Fatalf("reasoning effort = %q, want high", reporter.reasoningEffort)
	}
}

func TestUsageReporterSetTranslatedReasoningEffortPreservesContextServiceTier(t *testing.T) {
	reporter := &usageReporter{reasoningEffort: "medium", serviceTier: "flex"}

	reporter.setTranslatedReasoningEffort([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "openai")

	if reporter.reasoningEffort != "" {
		t.Fatalf("reasoning effort = %q, want empty", reporter.reasoningEffort)
	}
	if reporter.serviceTier != "flex" {
		t.Fatalf("service tier = %q, want flex", reporter.serviceTier)
	}
}

func TestUsageReporterSetTranslatedReasoningEffortExtractsServiceTier(t *testing.T) {
	reporter := &usageReporter{serviceTier: usage.DefaultServiceTier}

	reporter.setTranslatedReasoningEffort(
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

func TestParsePluginExecutorResponseUsageResponsesShapes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol string
		payload  string
		input    int64
		output   int64
		total    int64
		tier     string
	}{
		{
			name: "top-level usage with tier", protocol: "openai-response",
			payload: `{"service_tier":"default","usage":{"input_tokens":34,"output_tokens":499,"total_tokens":533}}`,
			input:   34, output: 499, total: 533, tier: "default",
		},
		{
			name: "Codex protocol top-level usage", protocol: "codex",
			payload: `{"response":{"service_tier":"priority"},"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`,
			input:   3, output: 4, total: 7, tier: "priority",
		},
		{
			name: "nested response usage wins", protocol: "openai-response",
			payload: `{"service_tier":"priority","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"response":{"usage":{"input_tokens":18,"output_tokens":22,"total_tokens":40}}}`,
			input:   18, output: 22, total: 40, tier: "priority",
		},
		{
			name: "tier-only response", protocol: "openai-response",
			payload: `{"service_tier":"default"}`, tier: "default",
		},
		{
			name: "Claude message usage", protocol: "claude",
			payload: `{"message":{"usage":{"input_tokens":6,"output_tokens":8}}}`,
			input:   6, output: 8, total: 14,
		},
		{
			name: "Gemini usage", protocol: "gemini",
			payload: `{"usageMetadata":{"promptTokenCount":6,"candidatesTokenCount":8}}`,
			input:   6, output: 8, total: 14,
		},
		{
			name: "Interactions usage", protocol: "interactions-response",
			payload: `{"interaction":{"usage":{"input_tokens":6,"output_tokens":8}}}`,
			input:   6, output: 8, total: 14,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParsePluginExecutorResponseUsage(tc.protocol, []byte(tc.payload))
			if got.InputTokens != tc.input || got.OutputTokens != tc.output || got.TotalTokens != tc.total || got.ResponseServiceTier != tc.tier {
				t.Fatalf("usage = %+v, want input=%d output=%d total=%d tier=%q", got, tc.input, tc.output, tc.total, tc.tier)
			}
		})
	}
}

func TestPluginExecutorStreamUsageKeepsTopLevelResponsesTokens(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		input int64
		tier  string
	}{
		{
			name:  "tier and top-level usage in same event",
			lines: []string{`data: {"type":"response.completed","service_tier":"default","usage":{"input_tokens":34,"output_tokens":499,"total_tokens":533}}`},
			input: 34, tier: "default",
		},
		{
			name:  "tier before top-level usage",
			lines: []string{`data: {"service_tier":"priority"}`, `data: {"type":"response.completed","usage":{"input_tokens":34,"output_tokens":499,"total_tokens":533}}`},
			input: 34, tier: "priority",
		},
		{
			name:  "nested zero does not hide top-level usage",
			lines: []string{`data: {"response":{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}},"service_tier":"flex","usage":{"input_tokens":34,"output_tokens":499,"total_tokens":533}}`},
			input: 34, tier: "flex",
		},
		{
			name:  "nested nonzero usage wins",
			lines: []string{`data: {"service_tier":"priority","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"response":{"usage":{"input_tokens":34,"output_tokens":499,"total_tokens":533}}}`},
			input: 34, tier: "priority",
		},
		{
			name:  "later tier-only event preserves tokens",
			lines: []string{`data: {"usage":{"input_tokens":34,"output_tokens":499,"total_tokens":533}}`, `data: {"service_tier":"default"}`},
			input: 34, tier: "default",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buffer := NewPluginExecutorStreamUsage("openai-response")
			for _, line := range tc.lines {
				buffer.ObserveLine([]byte(line))
			}
			got, ok := buffer.Detail()
			if !ok || got.InputTokens != tc.input || got.OutputTokens != 499 || got.TotalTokens != 533 || got.ResponseServiceTier != tc.tier {
				t.Fatalf("detail = %+v ok=%v, want input=%d output=499 total=533 tier=%q", got, ok, tc.input, tc.tier)
			}
		})
	}
}

func TestPluginExecutorUsageReporterAttributesSelectedAuthOnce(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	ctx := usage.WithStream(context.Background(), false)
	auth := &cliproxyauth.Auth{
		ID: "selected-auth-1", Provider: "test-provider", FileName: "selected-auth-1.json",
		Attributes: map[string]string{"base_url": "https://selected.test/v1", "type": "oauth"},
	}
	reporter := NewPluginExecutorUsageReporter(ctx, testUsageExecutor{}, "gpt-test", auth)
	reporter.SetTranslatedReasoningEffort([]byte(`{"model":"gpt-test","service_tier":"priority"}`), "openai-response")
	reporter.Publish(ctx, ParsePluginExecutorResponseUsage("openai-response", []byte(`{"service_tier":"default","usage":{"input_tokens":34,"output_tokens":499,"total_tokens":533}}`)))
	reporter.EnsurePublished(ctx)
	reporter.PublishFailure(ctx, errors.New("late failure"))
	select {
	case got := <-plugin.records:
		if got.AuthID != auth.ID || got.AuthIndex != auth.EnsureIndex() || got.ExecutorType != "testUsageExecutor" || got.BaseURL != "https://selected.test/v1" || got.Failed || got.Stream {
			t.Fatalf("selected auth record = %+v", got)
		}
		if got.Detail.InputTokens != 34 || got.Detail.OutputTokens != 499 || got.Detail.TotalTokens != 533 || got.ServiceTier != "priority" || got.ResponseServiceTier != "default" {
			t.Fatalf("usage detail = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for plugin executor usage record")
	}
	select {
	case got := <-plugin.records:
		t.Fatalf("duplicate plugin executor usage record: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestPluginExecutorUsageReporterPreservesPartialStreamUsageOnFailure(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	ctx := usage.WithStream(context.Background(), false)
	auth := &cliproxyauth.Auth{ID: "selected-auth-2", Provider: "test-provider"}
	reporter := NewPluginExecutorUsageReporter(ctx, testUsageExecutor{}, "gpt-test", auth)
	reporter.MarkStream()
	reporter.PublishFailureWithDetail(ctx, usage.Detail{InputTokens: 34, OutputTokens: 3, TotalTokens: 37, ResponseServiceTier: "flex"}, errors.New("stream interrupted"))
	reporter.EnsurePublished(ctx)
	select {
	case got := <-plugin.records:
		if got.AuthID != auth.ID || !got.Failed || !got.Stream || got.Detail.InputTokens != 34 || got.Detail.OutputTokens != 3 || got.Detail.TotalTokens != 37 || got.ResponseServiceTier != "flex" {
			t.Fatalf("failed stream usage record = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for failed stream usage record")
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

func TestStreamUsageBuffersMarkReporterWithoutUsage(t *testing.T) {
	for _, test := range []struct {
		name    string
		publish func(*usageReporter) bool
	}{
		{
			name: "generic",
			publish: func(reporter *usageReporter) bool {
				var buffer streamUsageBuffer
				return buffer.publish(context.Background(), reporter)
			},
		},
		{
			name: "claude",
			publish: func(reporter *usageReporter) bool {
				var buffer claudeStreamUsageBuffer
				return buffer.publish(context.Background(), reporter)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reporter := &usageReporter{}
			if test.publish(reporter) {
				t.Fatal("empty stream buffer reported usage")
			}
			if !reporter.stream {
				t.Fatal("stream reporter was not marked")
			}
		})
	}
}

func TestParseGeminiStreamUsageSkipsZeroPlaceholders(t *testing.T) {
	tests := []struct {
		name       string
		lines      [][]byte
		want       usage.Detail
		wantAccept int
	}{
		{
			name: "non-zero then zero",
			lines: [][]byte{
				[]byte(`data: {"usageMetadata":{"promptTokenCount":17984,"candidatesTokenCount":2668,"thoughtsTokenCount":1028,"totalTokenCount":21680}}`),
				[]byte(`data: {"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"thoughtsTokenCount":0,"totalTokenCount":0}}`),
			},
			want: usage.Detail{
				InputTokens: 17984, OutputTokens: 2668, ReasoningTokens: 1028, TotalTokens: 21680,
			},
			wantAccept: 1,
		},
		{
			name: "zero then non-zero",
			lines: [][]byte{
				[]byte(`data: {"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"thoughtsTokenCount":0,"totalTokenCount":0}}`),
				[]byte(`data: {"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":7}}`),
			},
			want:       usage.Detail{InputTokens: 5, OutputTokens: 7, TotalTokens: 12},
			wantAccept: 1,
		},
		{
			name:       "cache only",
			lines:      [][]byte{[]byte(`data: {"usageMetadata":{"cachedContentTokenCount":9}}`)},
			want:       usage.Detail{CachedTokens: 9, CacheReadTokens: 9},
			wantAccept: 1,
		},
		{
			name:       "thought only",
			lines:      [][]byte{[]byte(`data: {"usageMetadata":{"thoughtsTokenCount":4}}`)},
			want:       usage.Detail{ReasoningTokens: 4, TotalTokens: 4},
			wantAccept: 1,
		},
		{
			name:       "missing usage",
			lines:      [][]byte{[]byte(`data: {"candidates":[]}`)},
			wantAccept: 0,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var accepted int
			var buffered streamUsageBuffer
			for _, line := range testCase.lines {
				detail, ok := parseGeminiStreamUsage(line)
				if !ok {
					continue
				}
				accepted++
				buffered.observe(detail, true, false)
			}
			if accepted != testCase.wantAccept {
				t.Fatalf("accepted usage count = %d, want %d", accepted, testCase.wantAccept)
			}
			got, ok := buffered.detailValue()
			if testCase.wantAccept == 0 {
				if ok {
					t.Fatalf("usage detail = %+v, want no usage", got)
				}
				return
			}
			if !ok {
				t.Fatal("usage detail missing")
			}
			if got.InputTokens != testCase.want.InputTokens ||
				got.OutputTokens != testCase.want.OutputTokens ||
				got.ReasoningTokens != testCase.want.ReasoningTokens ||
				got.CachedTokens != testCase.want.CachedTokens ||
				got.CacheReadTokens != testCase.want.CacheReadTokens ||
				got.TotalTokens != testCase.want.TotalTokens {
				t.Fatalf("usage detail = %+v, want %+v", got, testCase.want)
			}
		})
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
		t.Run(
			tt.name, func(t *testing.T) {
				var buffer streamUsageBuffer
				for _, chunk := range tt.chunks {
					buffer.observeOpenAIStream(chunk)
				}
				got, ok := buffer.detailValue()
				if !ok {
					t.Fatalf("detail = %+v ok=false, want %+v", got, tt.want)
				}
				assertUsageDetailWithBreakdown(t, got, tt.want)
			},
		)
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

func TestStreamUsageBufferObservesSplitResponseModels(t *testing.T) {
	reporter := &usageReporter{provider: "openai-compat", model: "requested-model"}
	buffer := streamUsageBuffer{modelReporter: reporter}
	buffer.observeOpenAIStreamChunk([]byte("data: {\"object\":\"chat.completion.chunk\",\"model\":\"served-"))
	buffer.observeOpenAIStreamChunk([]byte("v1\"}\ndata: {\"object\":\"chat.completion.chunk\",\"model\":\"served-v2\",\"choices\":[{\"finish_reason\":\"stop\"}]}"))
	buffer.flushOpenAIStream()
	buffer.observeOpenAIStreamChunk([]byte("\ndata: {\"model\":\"ignored\"}\n"))

	served, _ := reporter.responseModelSnapshot()
	if served != "served-v2" {
		t.Fatalf("chunked response model = %q, want served-v2", served)
	}
}

func TestStreamUsageBufferObservesMultilineResponseModel(t *testing.T) {
	reporter := &usageReporter{provider: "openai-compat", model: "requested-model"}
	buffer := streamUsageBuffer{modelReporter: reporter}
	buffer.observeOpenAIStreamChunk([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\n"))
	buffer.observeOpenAIStreamChunk([]byte("data: \"response\":{\"model\":\"served-multiline\"}}\n\n"))

	served, _ := reporter.responseModelSnapshot()
	if served != "served-multiline" {
		t.Fatalf("multiline response model = %q, want served-multiline", served)
	}
}

func TestStreamUsageBufferSkipsOversizedModelEvent(t *testing.T) {
	reporter := &usageReporter{provider: "openai-compat", model: "requested-model"}
	buffer := streamUsageBuffer{modelReporter: reporter}
	buffer.observeOpenAIStreamChunk(append([]byte("data: "), bytes.Repeat([]byte("x"), maxStreamModelEventBytes)...))
	buffer.observeOpenAIStreamChunk([]byte("\n\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"served-after-overflow\"}}\n\n"))
	buffer.flushOpenAIStream()

	served, _ := reporter.responseModelSnapshot()
	if served != "served-after-overflow" || len(buffer.modelLines) != 0 || buffer.modelOverflow {
		t.Fatalf("response model after oversized event = %q, lines=%d overflow=%t", served, len(buffer.modelLines), buffer.modelOverflow)
	}
}

func TestStreamUsageBufferDropsOversizedLineAndRecovers(t *testing.T) {
	var buffer streamUsageBuffer
	buffer.observeOpenAIStreamChunk(bytes.Repeat([]byte("x"), maxStreamUsageLineBytes+1))
	if !buffer.pendingOverflow || len(buffer.pending) != 0 {
		t.Fatalf(
			"overflow state = %v pending=%d, want overflow with empty pending", buffer.pendingOverflow,
			len(buffer.pending),
		)
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
	if !ok {
		t.Fatalf("detail = %+v ok=false, want %+v", detail, want)
	}
	assertUsageDetailWithBreakdown(t, detail, want)
}

func TestStreamUsageBufferParsesEscapedUsageAndServiceTierKeys(t *testing.T) {
	var buffer streamUsageBuffer
	buffer.observeOpenAIStream(
		[]byte("data: {\"\\u0075sage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5},\"service\\u005ftier\":\"priority\"}"),
	)

	detail, ok := buffer.detailValue()
	want := usage.Detail{InputTokens: 2, OutputTokens: 3, TotalTokens: 5, ResponseServiceTier: "priority"}
	if !ok {
		t.Fatalf("detail = %+v ok=false, want %+v", detail, want)
	}
	assertUsageDetailWithBreakdown(t, detail, want)
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
	if detail.InputTokens != 5 || detail.OutputTokens != 7 || detail.TotalTokens != 12 || detail.CachedTokens != 3 {
		t.Fatalf("detail = %+v", detail)
	}
	if !detail.TokenBreakdown.Valid() || detail.TokenBreakdown.Quality != usage.TokenAccountingQualityComplete {
		t.Fatalf("token breakdown = %+v, want valid complete", detail.TokenBreakdown)
	}
}

func TestStreamUsageBufferKeepsValidUsageAfterNegativeUpdate(t *testing.T) {
	var buffer streamUsageBuffer
	buffer.observeOpenAIStream([]byte(`data: {"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}`))
	buffer.observeOpenAIStream([]byte(`data: {"usage":{"input_tokens":-1,"output_tokens":-2,"total_tokens":-3}}`))

	detail, ok := buffer.detailValue()
	want := usage.Detail{InputTokens: 5, OutputTokens: 7, TotalTokens: 12}
	if !ok {
		t.Fatalf("detail = %+v ok=false, want %+v", detail, want)
	}
	assertUsageDetailWithBreakdown(t, detail, want)
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
		assertUsageDetailWithBreakdown(t, got.Detail, usage.Detail{ResponseServiceTier: "priority"})
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for zero additional-model usage record")
	}
}

func TestUsageReporterPreservesServiceTierForNonStringPayloadValue(t *testing.T) {
	reporter := &usageReporter{serviceTier: "priority"}
	reporter.setTranslatedReasoningEffort([]byte(`{"service_tier":123}`), "openai-response")
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
		assertUsageDetailWithBreakdown(t, got.Detail, usage.Detail{})
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
}

func TestUsageReporterCancellationAfterFinalizerCheckWinsOverSuccess(t *testing.T) {
	tests := []struct {
		name    string
		publish func(context.Context, *usageReporter)
	}{
		{
			name: "buffered usage",
			publish: func(ctx context.Context, reporter *usageReporter) {
				var buffer streamUsageBuffer
				buffer.observeOpenAIStream(
					[]byte(`data: {"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5},"service_tier":"default"}`),
				)
				buffer.publish(ctx, reporter)
			},
		},
		{
			name: "zero usage",
			publish: func(ctx context.Context, reporter *usageReporter) {
				reporter.ensurePublished(ctx)
			},
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
			registerTestUsagePlugin(t, plugin)

			ctx, cancel := context.WithCancel(context.Background())
			reporter := &usageReporter{provider: "openai", model: "gpt-test"}
			if errContext := scannerOrContextError(ctx, nil); errContext != nil {
				t.Fatalf("pre-publication context error = %v, want nil", errContext)
			}
			cancel()

			testCase.publish(ctx, reporter)
			reporter.publish(ctx, usage.Detail{TotalTokens: 99})

			select {
			case got := <-plugin.records:
				if !got.Failed {
					t.Fatalf("Failed = false, want true: %#v", got)
				}
				if got.Fail.StatusCode != clienterror.StatusClientClosedRequest ||
					!strings.Contains(got.Fail.Body, context.Canceled.Error()) {
					t.Fatalf("failure = %#v, want canceled request", got.Fail)
				}
				if got.Detail.InputTokens != 0 || got.Detail.OutputTokens != 0 || got.Detail.TotalTokens != 0 {
					t.Fatalf("detail = %#v, want canceled success usage discarded", got.Detail)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for canceled usage record")
			}
			select {
			case duplicate := <-plugin.records:
				t.Fatalf("duplicate usage record = %#v", duplicate)
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}

func assertUsageDetailWithBreakdown(t *testing.T, got, want usage.Detail) {
	t.Helper()
	breakdown := got.TokenBreakdown
	got.TokenBreakdown = usage.TokenBreakdown{}
	if got != want {
		t.Fatalf("detail = %+v, want %+v", got, want)
	}
	if !breakdown.Valid() || breakdown.TotalTokens != got.TotalTokens {
		t.Fatalf("token breakdown = %+v, want valid total %d", breakdown, got.TotalTokens)
	}
}

func TestFailFromErrorsMapsContextStatuses(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "canceled", err: context.Canceled, want: clienterror.StatusClientClosedRequest},
		{name: "deadline", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{
			name: "url error wraps canceled",
			err:  &url.Error{Op: "Post", URL: "https://example.com", Err: context.Canceled},
			want: clienterror.StatusClientClosedRequest,
		},
		{name: "plain error", err: errors.New("boom"), want: 0},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				fail := failFromErrors(test.err)
				if fail.StatusCode != test.want {
					t.Fatalf("StatusCode = %d, want %d; body=%q", fail.StatusCode, test.want, fail.Body)
				}
				if strings.TrimSpace(fail.Body) == "" {
					t.Fatal("expected non-empty failure body")
				}
			},
		)
	}

	if fail := failFromErrors(nil, nil); fail.StatusCode != 0 || fail.Body != "" {
		t.Fatalf("failFromErrors(nil) = %+v, want empty failure", fail)
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
		t.Run(
			tt.name, func(t *testing.T) {
				detail := parseOpenAIUsage([]byte(tt.usage))
				if detail.CachedTokens != tt.read || detail.CacheReadTokens != tt.read {
					t.Fatalf("cache read detail = %#v, want cached/read %d", detail, tt.read)
				}
				if detail.CacheCreationTokens != tt.created {
					t.Fatalf("cache creation tokens = %d, want %d", detail.CacheCreationTokens, tt.created)
				}
			},
		)
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

func TestParseClaudeUsageCapturesCacheCreationTTLBreakdown(t *testing.T) {
	data := []byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":15,"cache_creation":{"ephemeral_5m_input_tokens":6,"ephemeral_1h_input_tokens":7},"output_tokens_details":{"thinking_tokens":2}}}`)
	detail := parseClaudeUsage(data)
	if detail.CacheCreationTokens != 15 || detail.CacheCreation5mInputTokens != 6 ||
		detail.CacheCreation1hInputTokens != 7 {
		t.Fatalf("cache creation detail = %#v, want aggregate/5m/1h 15/6/7", detail)
	}
	if detail.OutputTokens != 3 || detail.ReasoningTokens != 2 || detail.TotalTokens != 33 ||
		!detail.TokenBreakdown.Valid() {
		t.Fatalf("Claude usage detail = %#v", detail)
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

func TestClaudeStreamUsageBufferMergesCumulativeCacheCreationTTLBreakdown(t *testing.T) {
	var buffer claudeStreamUsageBuffer
	buffer.observe(
		[]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":3,"cache_creation_input_tokens":10,"cache_creation":{"ephemeral_5m_input_tokens":4,"ephemeral_1h_input_tokens":6}}}}`),
	)
	buffer.observe(
		[]byte(`data: {"type":"message_delta","usage":{"output_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":5}}}`),
	)
	detail, ok := buffer.detailValue()
	if !ok {
		t.Fatal("detailValue() ok = false, want true")
	}
	if detail.CacheCreationTokens != 11 || detail.CacheCreation5mInputTokens != 5 ||
		detail.CacheCreation1hInputTokens != 6 || detail.TotalTokens != 29 {
		t.Fatalf("first merged cache creation detail = %#v", detail)
	}

	buffer.observe(
		[]byte(`data: {"type":"message_delta","usage":{"output_tokens":7,"cache_creation_input_tokens":12}}`),
	)
	detail, ok = buffer.detailValue()
	if !ok {
		t.Fatal("detailValue() after aggregate delta ok = false, want true")
	}
	if detail.CacheCreationTokens != 12 || detail.CacheCreation5mInputTokens != 5 ||
		detail.CacheCreation1hInputTokens != 6 || detail.TotalTokens != 32 ||
		!detail.TokenBreakdown.Valid() {
		t.Fatalf("second merged cache creation detail = %#v", detail)
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
		t.Run(
			tt.name, func(t *testing.T) {
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
			},
		)
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

func TestResolveUsageSourceRedactsAPIKeyAccountInfo(t *testing.T) {
	raw := "sk-pyro-abc123def456"
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": raw}}

	got, kind, redacted := resolveUsageSource(auth, "")
	if want := util.HideAPIKey(raw); got != want || kind != usage.CredentialSourceKindAPIKey || !redacted {
		t.Fatalf("resolveUsageSource API key = %q/%q/%v, want %q/api_key/true", got, kind, redacted, want)
	}
}

func TestResolveUsageSourcePreservesNonSecretAccountInfo(t *testing.T) {
	oauth := &cliproxyauth.Auth{Metadata: map[string]any{"email": "user@example.com"}}
	if got, kind, redacted := resolveUsageSource(
		oauth, "",
	); got != "user@example.com" || kind != usage.CredentialSourceKindIdentity || redacted {
		t.Fatalf("resolveUsageSource OAuth = %q/%q/%v, want user@example.com/identity/false", got, kind, redacted)
	}

	vertex := &cliproxyauth.Auth{
		Provider:   "vertex",
		Metadata:   map[string]any{"project_id": "project-123"},
		Attributes: map[string]string{"api_key": "vertex-secret-value"},
	}
	if got, kind, redacted := resolveUsageSource(
		vertex, "",
	); got != "project-123" || kind != usage.CredentialSourceKindIdentity || redacted {
		t.Fatalf("resolveUsageSource Vertex = %q/%q/%v, want project-123/identity/false", got, kind, redacted)
	}
}
