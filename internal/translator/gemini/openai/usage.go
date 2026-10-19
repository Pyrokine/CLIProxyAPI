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
	HasCachedTokens bool
	Seen            bool
}

// Update 保留各项有效计数，返回本帧是否提供完整且一致的输入、输出和总量
func (usage *GeminiUsage) Update(metadata gjson.Result) bool {
	if usage == nil || !metadata.IsObject() {
		return false
	}
	usage.Seen = true
	hasPrompt := updateGeminiTokenCount(metadata.Get("promptTokenCount"), &usage.PromptTokens)
	hasCandidates := updateGeminiTokenCount(metadata.Get("candidatesTokenCount"), &usage.CandidateTokens)
	updateGeminiTokenCount(metadata.Get("thoughtsTokenCount"), &usage.ThoughtTokens)
	hasTotal := updateGeminiTokenCount(metadata.Get("totalTokenCount"), &usage.TotalTokens)
	if updateGeminiTokenCount(metadata.Get("cachedContentTokenCount"), &usage.CachedTokens) {
		usage.HasCachedTokens = true
	}
	if !hasPrompt || !hasCandidates || !hasTotal || usage.CandidateTokens > math.MaxInt64-usage.ThoughtTokens {
		return false
	}
	output := usage.OutputTokens()
	return usage.PromptTokens <= math.MaxInt64-output && usage.PromptTokens+output == usage.TotalTokens
}

// OutputTokens includes visible candidate tokens and hidden thought tokens.
func (usage GeminiUsage) OutputTokens() int64 {
	if usage.CandidateTokens > math.MaxInt64-usage.ThoughtTokens {
		return 0
	}
	return usage.CandidateTokens + usage.ThoughtTokens
}

func updateGeminiTokenCount(value gjson.Result, target *int64) bool {
	if target == nil || value.Type != gjson.Number {
		return false
	}
	raw := strings.TrimSpace(value.Raw)
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed < 0 {
		return false
	}
	*target = parsed
	return true
}
