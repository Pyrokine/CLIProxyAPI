package openai

import (
	"math"
	"testing"

	"github.com/tidwall/gjson"
)

func TestGeminiUsageValidatesAndAddsOutputTokens(t *testing.T) {
	var usage GeminiUsage
	usage.Update(gjson.Parse(`{
		"promptTokenCount":12,
		"candidatesTokenCount":7,
		"thoughtsTokenCount":5,
		"totalTokenCount":24,
		"cachedContentTokenCount":3
	}`))

	if !usage.Seen || usage.PromptTokens != 12 || usage.OutputTokens() != 12 ||
		usage.ThoughtTokens != 5 || usage.TotalTokens != 24 || usage.CachedTokens != 3 {
		t.Fatalf("unexpected usage: %#v", usage)
	}
}

func TestGeminiUsageRejectsInvalidCountsAndOverflow(t *testing.T) {
	var usage GeminiUsage
	usage.Update(gjson.Parse(`{
		"promptTokenCount":-1,
		"candidatesTokenCount":1.5,
		"thoughtsTokenCount":"4",
		"totalTokenCount":null
	}`))
	if usage.PromptTokens != 0 || usage.CandidateTokens != 0 || usage.ThoughtTokens != 0 ||
		usage.TotalTokens != 0 || usage.OutputTokens() != 0 {
		t.Fatalf("invalid counters were accepted: %#v", usage)
	}

	usage.CandidateTokens = math.MaxInt64
	usage.ThoughtTokens = 1
	if got := usage.OutputTokens(); got != 0 {
		t.Fatalf("overflowing output total = %d, want 0", got)
	}
}

func TestGeminiUsageAccumulatesAcrossChunks(t *testing.T) {
	var usage GeminiUsage
	usage.Update(gjson.Parse(`{"promptTokenCount":9,"candidatesTokenCount":2}`))
	usage.Update(gjson.Parse(`{"thoughtsTokenCount":4,"candidatesTokenCount":-1}`))

	if usage.PromptTokens != 9 || usage.CandidateTokens != 2 || usage.ThoughtTokens != 4 ||
		usage.OutputTokens() != 6 {
		t.Fatalf("usage was not accumulated: %#v", usage)
	}
}
