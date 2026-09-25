package openai

import (
	"math"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// GeminiUsage stores validated Gemini token counters across response chunks.
type GeminiUsage struct {
	PromptTokens    int64
	CandidateTokens int64
	ThoughtTokens   int64
	TotalTokens     int64
	CachedTokens    int64
	Seen            bool
}

// Update retains the latest valid non-negative integer value for each counter.
func (usage *GeminiUsage) Update(metadata gjson.Result) {
	if usage == nil || !metadata.IsObject() {
		return
	}
	usage.Seen = true
	updateGeminiTokenCount(metadata.Get("promptTokenCount"), &usage.PromptTokens)
	updateGeminiTokenCount(metadata.Get("candidatesTokenCount"), &usage.CandidateTokens)
	updateGeminiTokenCount(metadata.Get("thoughtsTokenCount"), &usage.ThoughtTokens)
	updateGeminiTokenCount(metadata.Get("totalTokenCount"), &usage.TotalTokens)
	updateGeminiTokenCount(metadata.Get("cachedContentTokenCount"), &usage.CachedTokens)
}

// OutputTokens includes visible candidate tokens and hidden thought tokens.
func (usage GeminiUsage) OutputTokens() int64 {
	if usage.CandidateTokens > math.MaxInt64-usage.ThoughtTokens {
		return 0
	}
	return usage.CandidateTokens + usage.ThoughtTokens
}

func updateGeminiTokenCount(value gjson.Result, target *int64) {
	if target == nil || value.Type != gjson.Number {
		return
	}
	raw := strings.TrimSpace(value.Raw)
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed < 0 {
		return
	}
	*target = parsed
}
