package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestUsageToolInputAccounting(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		protocol                               string
		payload                                string
		input, output, reasoning, total, cache int64
	}{
		{"gemini", "gemini", `{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"thoughtsTokenCount":3,"toolUsePromptTokenCount":5,"totalTokenCount":20}}`, 15, 2, 3, 20, 0},
		{"gemini_cached", "gemini", `{"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":4,"candidatesTokenCount":2,"thoughtsTokenCount":3,"toolUsePromptTokenCount":5,"totalTokenCount":20}}`, 15, 2, 3, 20, 4},
		{"interactions", "interactions", `{"usage":{"total_input_tokens":2,"total_output_tokens":6,"total_thought_tokens":3,"total_tool_use_tokens":4,"total_tokens":15}}`, 6, 6, 3, 15, 0},
		{"interactions_alias", "interactions", `{"usage":{"input_tokens":2,"output_tokens":6,"reasoning_tokens":3,"tool_use_tokens":4,"total_tokens":15}}`, 6, 6, 3, 15, 0},
		{"interactions_camel_alias", "interactions", `{"usage":{"input_tokens":2,"output_tokens":6,"reasoning_tokens":3,"toolUsePromptTokenCount":4,"total_tokens":15}}`, 6, 6, 3, 15, 0},
		{"gemini_tool_only", "gemini", `{"usageMetadata":{"toolUsePromptTokenCount":5}}`, 5, 0, 0, 5, 0},
		{"interactions_tool_only", "interactions", `{"usage":{"total_tool_use_tokens":4}}`, 4, 0, 0, 4, 0},
		{"gemini_missing_tool", "gemini", `{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"thoughtsTokenCount":3,"totalTokenCount":15}}`, 10, 2, 3, 15, 0},
		{"gemini_zero_tool", "gemini", `{"usageMetadata":{"promptTokenCount":10,"toolUsePromptTokenCount":0,"candidatesTokenCount":2,"thoughtsTokenCount":3,"totalTokenCount":15}}`, 10, 2, 3, 15, 0},
		{"gemini_fallback_total", "gemini", `{"usageMetadata":{"promptTokenCount":10,"toolUsePromptTokenCount":5,"candidatesTokenCount":2,"thoughtsTokenCount":3}}`, 15, 2, 3, 20, 0},
		{"interactions_fallback_total", "interactions", `{"usage":{"total_input_tokens":2,"total_tool_use_tokens":4,"total_output_tokens":6,"total_thought_tokens":3}}`, 6, 6, 3, 15, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
					capture := &captureUsagePlugin{records: make(chan usage.Record, 1)}
					registerTestUsagePlugin(t, capture)
					var detail usage.Detail
					if stream {
						buffer := NewPluginExecutorStreamUsage(tc.protocol)
						buffer.ObserveLine([]byte("data: " + tc.payload))
						var ok bool
						detail, ok = buffer.Detail()
						if !ok {
							t.Fatal("tool input usage was not recognized")
						}
					} else {
						detail = ParsePluginExecutorResponseUsage(tc.protocol, []byte(tc.payload))
					}
					reporter := &usageReporter{provider: tc.protocol, model: "fixture", requestedAt: time.Now()}
					reporter.publish(context.Background(), detail)
					select {
					case record := <-capture.records:
						got := record.Detail
						if got.InputTokens != tc.input || got.OutputTokens != tc.output || got.ReasoningTokens != tc.reasoning || got.TotalTokens != tc.total || got.CacheReadTokens != tc.cache {
							t.Fatalf("published counts = %+v, want input/output/reasoning/total/cache = %d/%d/%d/%d/%d", got, tc.input, tc.output, tc.reasoning, tc.total, tc.cache)
						}
						breakdown := got.TokenBreakdown
						if !breakdown.Valid() || breakdown.Quality != usage.TokenAccountingQualityComplete || breakdown.TotalTokens != tc.total || breakdown.Input.TotalTokens != tc.input || breakdown.Input.UncachedTokens != tc.input-tc.cache || breakdown.Input.CacheReadTokens != tc.cache || breakdown.Output.NonReasoningTokens != tc.output || breakdown.Output.ReasoningTokens != tc.reasoning || breakdown.UnclassifiedTokens != 0 {
							t.Fatalf("published breakdown = %+v", breakdown)
						}
					case <-time.After(time.Second):
						t.Fatal("usage record was not published")
					}
				})
			}
		})
	}
}

func TestGeminiStreamToolInputSnapshots(t *testing.T) {
	var buffer streamUsageBuffer
	for i, tc := range []struct {
		payload      string
		input, total int64
	}{
		{`{"usageMetadata":{"promptTokenCount":10,"toolUsePromptTokenCount":5,"candidatesTokenCount":2,"thoughtsTokenCount":3,"totalTokenCount":20}}`, 15, 20},
		{`{"usageMetadata":{"candidatesTokenCount":4,"totalTokenCount":22}}`, 15, 22},
		{`{"usageMetadata":{"promptTokenCount":12,"totalTokenCount":24}}`, 17, 24},
		{`{"usageMetadata":{"toolUsePromptTokenCount":6,"totalTokenCount":25}}`, 18, 25},
		{`{"usageMetadata":{"toolUsePromptTokenCount":6,"totalTokenCount":25}}`, 18, 25},
		{`{"usageMetadata":{"toolUsePromptTokenCount":0,"totalTokenCount":19}}`, 12, 19},
		{`{"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0,"thoughtsTokenCount":0,"totalTokenCount":0}}`, 12, 19},
		{`{"usageMetadata":{"toolUsePromptTokenCount":9223372036854775807,"totalTokenCount":19}}`, 12, 19},
	} {
		buffer.observeGeminiStream([]byte("data: " + tc.payload))
		detail, ok := buffer.detailValue()
		if !ok || detail.InputTokens != tc.input || detail.TotalTokens != tc.total {
			t.Fatalf("snapshot %d: detail=%+v, present=%t", i, detail, ok)
		}
		got := normalizeUsageDetailTotal(detail, "gemini", "GeminiExecutor").TokenBreakdown
		if !got.Valid() || got.Quality != usage.TokenAccountingQualityComplete || got.Input.TotalTokens != tc.input {
			t.Fatalf("snapshot %d: breakdown=%+v", i, got)
		}
	}
}

func TestUsageToolInputValidation(t *testing.T) {
	for _, protocol := range []string{"gemini", "interactions"} {
		for _, value := range []string{"-1", "1.5", `"4"`, "null", "9223372036854775807", "9223372036854775808"} {
			t.Run(protocol+"/"+value, func(t *testing.T) {
				payload := fmt.Sprintf(`{"usageMetadata":{"promptTokenCount":10,"toolUsePromptTokenCount":%s,"candidatesTokenCount":2,"thoughtsTokenCount":3,"totalTokenCount":20}}`, value)
				if protocol == "interactions" {
					payload = fmt.Sprintf(`{"usage":{"total_input_tokens":10,"total_tool_use_tokens":%s,"total_output_tokens":2,"total_thought_tokens":3,"total_tokens":20}}`, value)
				}
				detail := ParsePluginExecutorResponseUsage(protocol, []byte(payload))
				detail = normalizeUsageDetailTotal(detail, protocol, "")
				if detail.TotalTokens != 20 || detail.InputTokens < 0 || !detail.TokenBreakdown.Valid() || detail.TokenBreakdown.Quality == usage.TokenAccountingQualityComplete {
					t.Fatalf("invalid tool input was accepted or total changed: %+v", detail)
				}
			})
		}
	}
	payload := []byte(`{"usageMetadata":{"promptTokenCount":10,"toolUsePromptTokenCount":5,"candidatesTokenCount":2,"thoughtsTokenCount":3,"totalTokenCount":20}}`)
	got := normalizeUsageDetailTotal(parseGeminiUsage(payload), "custom", "CustomExecutor")
	if got.TotalTokens != 20 || got.InputTokens != 15 || got.TokenBreakdown.Quality != usage.TokenAccountingQualityUnclassified || got.TokenBreakdown.UnclassifiedTokens != 20 {
		t.Fatalf("unknown provider classification changed: %+v", got)
	}
	mismatched := []byte(strings.Replace(string(payload), `"totalTokenCount":20`, `"totalTokenCount":21`, 1))
	got = normalizeUsageDetailTotal(parseGeminiUsage(mismatched), "gemini", "")
	if got.TotalTokens != 21 || got.InputTokens != 15 || got.TokenBreakdown.Quality != usage.TokenAccountingQualityInconsistent {
		t.Fatalf("authoritative mismatch was hidden: %+v", got)
	}
}

func TestInvalidToolInputCountPreservesAccountingFailure(t *testing.T) {
	for _, field := range []struct{ protocol, path string }{
		{"gemini", "toolUsePromptTokenCount"},
		{"interactions", "total_tool_use_tokens"},
		{"interactions", "tool_use_tokens"},
		{"interactions", "toolUsePromptTokenCount"},
	} {
		for _, value := range []string{"-1", "1.5", `"4"`, "null", "9223372036854775807", "9223372036854775808"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%s/stream_%t", field.protocol, field.path, value, stream), func(t *testing.T) {
					payload := fmt.Sprintf(`{"usageMetadata":{"promptTokenCount":10,"%s":%s,"totalTokenCount":10}}`, field.path, value)
					if field.protocol == "interactions" {
						payload = fmt.Sprintf(`{"usage":{"total_input_tokens":10,"%s":%s,"total_tokens":10}}`, field.path, value)
					}
					var detail usage.Detail
					if stream {
						buffer := NewPluginExecutorStreamUsage(field.protocol)
						buffer.ObserveLine([]byte("data: " + payload))
						var present bool
						detail, present = buffer.Detail()
						if !present {
							t.Fatal("reported total was discarded")
						}
					} else {
						detail = ParsePluginExecutorResponseUsage(field.protocol, []byte(payload))
					}
					capture := &captureUsagePlugin{records: make(chan usage.Record, 1)}
					registerTestUsagePlugin(t, capture)
					reporter := newUsageReporter(context.Background(), field.protocol, "synthetic-model", nil)
					reporter.publish(context.Background(), detail)
					select {
					case record := <-capture.records:
						accounting := record.Detail.TokenBreakdown
						if record.Detail.TotalTokens != 10 || !accounting.Valid() || accounting.Quality != usage.TokenAccountingQualityInconsistent {
							t.Fatalf("invalid tool count was reclassified: %+v", record.Detail)
						}
					case <-time.After(time.Second):
						t.Fatal("usage record was not delivered")
					}
				})
			}
		}
	}
}
