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

func (p *captureUsagePlugin) HandleUsage(_ context.Context, record usage.Record) {
	select {
	case p.records <- record:
	default:
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

func TestUsageReporterIncludesReasoningEffort(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	usage.RegisterPlugin(plugin)

	ctx := usage.WithReasoningEffort(context.Background(), "medium")
	reporter := newUsageReporter(ctx, "openai", "gpt-5.4", nil)
	reporter.publish(ctx, usage.Detail{TotalTokens: 1})

	select {
	case got := <-plugin.records:
		if got.ReasoningEffort != "medium" {
			t.Fatalf("reasoning effort = %q, want %q", got.ReasoningEffort, "medium")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage record")
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
