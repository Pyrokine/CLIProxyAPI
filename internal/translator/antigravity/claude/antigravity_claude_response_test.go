package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
)

// ============================================================================
// Signature Caching Tests
// ============================================================================

func TestConvertAntigravityResponseToClaude_ParamsInitialized(t *testing.T) {
	cache.ClearSignatureCache("")

	// Request with user message - should initialize params
	requestJSON := []byte(`{
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hello world"}]}
		]
	}`)

	// First response chunk with thinking
	responseJSON := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "Let me think...", "thought": true}]
				}
			}]
		}
	}`)

	var param any
	ctx := context.Background()
	convertAntigravityResponseToClaude(
		ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, responseJSON, &param,
	)

	params := param.(*params)
	if !params.HasFirstResponse {
		t.Error("HasFirstResponse should be set after first chunk")
	}
	if params.CurrentThinkingText.Len() == 0 {
		t.Error("Thinking text should be accumulated")
	}
}

func TestConvertAntigravityResponseToClaude_ThinkingTextAccumulated(t *testing.T) {
	cache.ClearSignatureCache("")

	requestJSON := []byte(`{
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Test"}]}]
	}`)

	// First thinking chunk
	chunk1 := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "First part of thinking...", "thought": true}]
				}
			}]
		}
	}`)

	// Second thinking chunk (continuation)
	chunk2 := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": " Second part of thinking...", "thought": true}]
				}
			}]
		}
	}`)

	var param any
	ctx := context.Background()

	// Process first chunk - starts new thinking block
	convertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, chunk1, &param)
	params := param.(*params)

	if params.CurrentThinkingText.Len() == 0 {
		t.Error("Thinking text should be accumulated after first chunk")
	}

	// Process second chunk - continues thinking block
	convertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, chunk2, &param)

	text := params.CurrentThinkingText.String()
	if !strings.Contains(text, "First part") || !strings.Contains(text, "Second part") {
		t.Errorf("Thinking text should accumulate both parts, got: %s", text)
	}
}

func TestConvertAntigravityResponseToClaude_SignatureCached(t *testing.T) {
	cache.ClearSignatureCache("")

	requestJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Cache test"}]}]
	}`)

	// Thinking chunk
	thinkingChunk := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "My thinking process here", "thought": true}]
				}
			}]
		}
	}`)

	// Signature chunk
	validSignature := "abc123validSignature1234567890123456789012345678901234567890"
	signatureChunk := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "", "thought": true, "thoughtSignature": "` + validSignature + `"}]
				}
			}]
		}
	}`)

	var param any
	ctx := context.Background()

	// Process thinking chunk
	convertAntigravityResponseToClaude(
		ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, thinkingChunk, &param,
	)
	params := param.(*params)
	thinkingText := params.CurrentThinkingText.String()

	if thinkingText == "" {
		t.Fatal("Thinking text should be accumulated")
	}

	// Process signature chunk - should cache the signature
	convertAntigravityResponseToClaude(
		ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, signatureChunk, &param,
	)

	// Verify signature was cached
	cachedSig := cache.GetCachedSignature("claude-sonnet-4-5-thinking", thinkingText)
	if cachedSig != validSignature {
		t.Errorf("Expected cached signature '%s', got '%s'", validSignature, cachedSig)
	}

	// Verify thinking text was reset after caching
	if params.CurrentThinkingText.Len() != 0 {
		t.Error("Thinking text should be reset after signature is cached")
	}
}

func TestConvertAntigravityResponseToClaude_MultipleThinkingBlocks(t *testing.T) {
	cache.ClearSignatureCache("")

	requestJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Multi block test"}]}]
	}`)

	validSig1 := "signature1_12345678901234567890123456789012345678901234567"
	validSig2 := "signature2_12345678901234567890123456789012345678901234567"

	// First thinking block with signature
	block1Thinking := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "First thinking block", "thought": true}]
				}
			}]
		}
	}`)
	block1Sig := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "", "thought": true, "thoughtSignature": "` + validSig1 + `"}]
				}
			}]
		}
	}`)

	// Text content (breaks thinking)
	textBlock := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "Regular text output"}]
				}
			}]
		}
	}`)

	// Second thinking block with signature
	block2Thinking := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "Second thinking block", "thought": true}]
				}
			}]
		}
	}`)
	block2Sig := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [{"text": "", "thought": true, "thoughtSignature": "` + validSig2 + `"}]
				}
			}]
		}
	}`)

	var param any
	ctx := context.Background()

	// Process first thinking block
	convertAntigravityResponseToClaude(
		ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, block1Thinking, &param,
	)
	params := param.(*params)
	firstThinkingText := params.CurrentThinkingText.String()

	convertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, block1Sig, &param)

	// Verify first signature cached
	if cache.GetCachedSignature("claude-sonnet-4-5-thinking", firstThinkingText) != validSig1 {
		t.Error("First thinking block signature should be cached")
	}

	// Process text (transitions out of thinking)
	convertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, textBlock, &param)

	// Process second thinking block
	convertAntigravityResponseToClaude(
		ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, block2Thinking, &param,
	)
	secondThinkingText := params.CurrentThinkingText.String()

	convertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, block2Sig, &param)

	// Verify second signature cached
	if cache.GetCachedSignature("claude-sonnet-4-5-thinking", secondThinkingText) != validSig2 {
		t.Error("Second thinking block signature should be cached")
	}
}

func TestConvertAntigravityResponseToClaude_EmitsSignatureOnlyChunk(t *testing.T) {
	cache.ClearSignatureCache("")
	requestJSON := []byte(`{"model":"claude-sonnet-4-5-thinking"}`)
	thinkingChunk := []byte(`{
		"response":{"candidates":[{"content":{"parts":[{"text":"private reasoning","thought":true}]}}]}
	}`)
	signatureChunk := []byte(`{
		"response":{"candidates":[{"content":{"parts":[{"thoughtSignature":"sig-only"}]}}]}
	}`)

	var param any
	ctx := context.Background()
	convertAntigravityResponseToClaude(
		ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, thinkingChunk, &param,
	)
	out := string(
		convertAntigravityResponseToClaude(
			ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, signatureChunk, &param,
		)[0],
	)

	if !strings.Contains(out, `"type":"signature_delta"`) || !strings.Contains(out, "sig-only") {
		t.Fatalf("signature_delta missing: %s", out)
	}
	if strings.Contains(out, "text_delta") {
		t.Fatalf("signature-only chunk emitted text_delta: %s", out)
	}
}

func TestConvertAntigravityResponseToClaudeNonStream_TreatsSignatureAsThinking(t *testing.T) {
	requestJSON := []byte(`{"model":"claude-sonnet-4-5-thinking"}`)
	responseJSON := []byte(`{
		"response":{
			"responseId":"r1",
			"modelVersion":"claude-sonnet-4-5-thinking",
			"candidates":[{"content":{"parts":[{"text":"signed thought","thoughtSignature":"sig-nonstream"}]}}],
			"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}
		}
	}`)

	out := convertAntigravityResponseToClaudeNonStream(
		context.Background(), "claude-sonnet-4-5-thinking", requestJSON, requestJSON, responseJSON, nil,
	)
	body := string(out)
	if !strings.Contains(body, `"type":"thinking"`) || !strings.Contains(body, "signed thought") {
		t.Fatalf("thinking block missing: %s", body)
	}
	if !strings.Contains(body, "sig-nonstream") {
		t.Fatalf("thinking signature missing: %s", body)
	}
}

func TestConvertAntigravityResponseToClaudeNonStream_WebSearchGrounding(t *testing.T) {
	requestJSON := []byte(`{
		"model": "gemini-3.1-flash-lite",
		"tools": [{"type": "web_search_20250305", "name": "web_search"}]
	}`)
	translatedRequestJSON := []byte(`{"model":"gemini-3.1-flash-lite","request":{"tools":[{"googleSearch":{}}]}}`)
	responseJSON := testAntigravityGroundingResponse()

	output := convertAntigravityResponseToClaudeNonStream(
		context.Background(), "gemini-3.1-flash-lite", requestJSON, translatedRequestJSON, responseJSON, nil,
	)

	if got := gjson.GetBytes(output, "content.0.type").String(); got != "server_tool_use" {
		t.Fatalf("first content block = %q, want server_tool_use: %s", got, output)
	}
	if got := gjson.GetBytes(output, "content.1.type").String(); got != "web_search_tool_result" {
		t.Fatalf("second content block = %q, want web_search_tool_result: %s", got, output)
	}
	if got := gjson.GetBytes(output, "usage.server_tool_use.web_search_requests").Int(); got != 1 {
		t.Fatalf("web_search_requests = %d, want 1: %s", got, output)
	}
	if got := gjson.GetBytes(output, "content.1.content.0.url").String(); got != "https://example.com/weather" {
		t.Fatalf("search result url = %q: %s", got, output)
	}
	if got := gjson.GetBytes(output, "content.2.citations.0.url").String(); got != "https://example.com/weather" {
		t.Fatalf("citation url = %q: %s", got, output)
	}
}

func TestConvertAntigravityResponseToClaudeNonStream_WebSearchGroundingRequiresNativeGoogleSearch(t *testing.T) {
	requestJSON := []byte(`{
		"model": "gemini-3-flash-agent",
		"tools": [{"type": "web_search_20250305", "name": "web_search"}]
	}`)
	translatedRequestJSON := []byte(`{"model":"gemini-3-flash-agent","request":{"contents":[]}}`)
	responseJSON := testAntigravityGroundingResponse()

	output := convertAntigravityResponseToClaudeNonStream(
		context.Background(), "gemini-3-flash-agent", requestJSON, translatedRequestJSON, responseJSON, nil,
	)

	if got := gjson.GetBytes(output, "content.0.type").String(); got == "server_tool_use" {
		t.Fatalf("non-native translated request should not synthesize server_tool_use: %s", output)
	}
	if got := gjson.GetBytes(output, "usage.server_tool_use.web_search_requests").Int(); got != 0 {
		t.Fatalf("web_search_requests = %d, want 0: %s", got, output)
	}
}

func TestConvertAntigravityResponseToClaudeStream_WebSearchGrounding(t *testing.T) {
	requestJSON := []byte(`{
		"model": "gemini-3.1-flash-lite",
		"tools": [{"type": "web_search_20250305", "name": "web_search"}]
	}`)
	translatedRequestJSON := []byte(`{"model":"gemini-3.1-flash-lite","request":{"tools":[{"googleSearch":{}}]}}`)

	var param any
	output := bytes.Join(
		convertAntigravityResponseToClaude(
			context.Background(), "gemini-3.1-flash-lite", requestJSON, translatedRequestJSON,
			testAntigravityGroundingResponse(), &param,
		),
		nil,
	)
	output = append(
		output,
		bytes.Join(
			convertAntigravityResponseToClaude(
				context.Background(), "gemini-3.1-flash-lite", requestJSON, translatedRequestJSON, []byte("[DONE]"),
				&param,
			),
			nil,
		)...,
	)
	outputText := string(output)

	for _, needle := range []string{
		`"type":"server_tool_use"`,
		`"type":"web_search_tool_result"`,
		`"web_search_requests":1`,
		`"type":"citations_delta"`,
		`event: message_stop`,
	} {
		if !strings.Contains(outputText, needle) {
			t.Fatalf("stream output missing %s:\n%s", needle, outputText)
		}
	}
}

func TestConvertAntigravityResponseToClaudeStream_WebSearchBuffersTextUntilGrounding(t *testing.T) {
	requestJSON := []byte(`{
		"model": "gemini-3.1-flash-lite",
		"tools": [{"type": "web_search_20250305", "name": "web_search"}]
	}`)
	translatedRequestJSON := []byte(`{"model":"gemini-3.1-flash-lite","request":{"tools":[{"googleSearch":{}}]}}`)

	var param any
	firstChunk := []byte(`{
		"response": {
			"modelVersion": "gemini-3.1-flash-lite",
			"responseId": "resp-web-search-stream",
			"candidates": [{"content": {"parts": [{"text": "Beijing weather "}]}}],
			"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 2, "totalTokenCount": 12}
		}
	}`)
	finalChunk := []byte(`{
		"response": {
			"modelVersion": "gemini-3.1-flash-lite",
			"responseId": "resp-web-search-stream",
			"candidates": [{
				"content": {"parts": [{"text": "is clear today."}]},
				"groundingMetadata": {
					"webSearchQueries": ["Beijing weather"],
					"groundingChunks": [{"web": {"uri": "https://example.com/weather", "title": "Beijing Weather"}}],
					"groundingSupports": [{
						"segment": {"startIndex": 0, "endIndex": 31, "text": "Beijing weather is clear today."},
						"groundingChunkIndices": [0]
					}]
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 6, "totalTokenCount": 16}
		}
	}`)

	output := bytes.Join(
		convertAntigravityResponseToClaude(
			context.Background(), "gemini-3.1-flash-lite", requestJSON, translatedRequestJSON, firstChunk, &param,
		), nil,
	)
	output = append(
		output, bytes.Join(
			convertAntigravityResponseToClaude(
				context.Background(), "gemini-3.1-flash-lite", requestJSON, translatedRequestJSON, finalChunk, &param,
			), nil,
		)...,
	)
	output = append(
		output, bytes.Join(
			convertAntigravityResponseToClaude(
				context.Background(), "gemini-3.1-flash-lite", requestJSON, translatedRequestJSON, []byte("[DONE]"),
				&param,
			), nil,
		)...,
	)
	outputText := string(output)

	textStart := strings.Index(outputText, `"content_block":{"type":"text"`)
	serverToolStart := strings.Index(outputText, `"content_block":{"type":"server_tool_use"`)
	if serverToolStart < 0 {
		t.Fatalf("stream output missing server_tool_use:\n%s", outputText)
	}
	if textStart >= 0 && textStart < serverToolStart {
		t.Fatalf("text block was emitted before server_tool_use:\n%s", outputText)
	}
	if strings.Contains(outputText, `"index":0,"content_block":{"type":"text"`) {
		t.Fatalf("index 0 must be reserved for server_tool_use:\n%s", outputText)
	}
	if !strings.Contains(outputText, `"index":0,"content_block":{"type":"server_tool_use"`) {
		t.Fatalf("server_tool_use must use index 0:\n%s", outputText)
	}
	if !strings.Contains(outputText, `"index":1,"content_block":{"type":"web_search_tool_result"`) {
		t.Fatalf("web_search_tool_result must use index 1:\n%s", outputText)
	}
	if !strings.Contains(outputText, `Beijing weather is clear today.`) {
		t.Fatalf("buffered text was not emitted after web search blocks:\n%s", outputText)
	}
}

func TestConvertAntigravityResponseToClaudeStream_WebSearchMessageStartOutputTokensZero(t *testing.T) {
	requestJSON := []byte(`{
		"model": "gemini-3.1-flash-lite",
		"tools": [{"type": "web_search_20250305", "name": "web_search"}]
	}`)
	translatedRequestJSON := []byte(`{"model":"gemini-3.1-flash-lite","request":{"tools":[{"googleSearch":{}}]}}`)
	responseJSON := []byte(`{
		"response": {
			"modelVersion": "gemini-3.1-flash-lite",
			"responseId": "resp-web-search-start",
			"candidates": [{"content": {"parts": [{"text": "Beijing weather"}]}}],
			"cpaUsageMetadata": {"promptTokenCount": 85, "candidatesTokenCount": 43}
		}
	}`)

	var param any
	output := bytes.Join(
		convertAntigravityResponseToClaude(
			context.Background(), "gemini-3.1-flash-lite", requestJSON, translatedRequestJSON, responseJSON, &param,
		),
		nil,
	)
	messageStart := sseDataForEvent(t, string(output), "message_start")

	if got := gjson.Get(messageStart, "message.usage.output_tokens").Int(); got != 0 {
		t.Fatalf("message_start output_tokens = %d, want 0: %s", got, messageStart)
	}
}

func TestConvertAntigravityResponseToClaudeNonStreamEmptyCandidateReturnsContentArray(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3-flash-agent"}`)
	output := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3-flash-agent",
		requestJSON,
		requestJSON,
		testEmptyAntigravityResponse(),
		nil,
	)

	content := gjson.GetBytes(output, "content")
	if !content.IsArray() || len(content.Array()) != 0 {
		t.Fatalf("content = %s, want empty array: %s", content.Raw, output)
	}
	if got := gjson.GetBytes(output, "stop_reason").String(); got != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn: %s", got, output)
	}
}

func TestConvertAntigravityResponseToClaudeStreamEmptyCandidateClosesMessage(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3-flash-agent"}`)
	responseJSON := testEmptyAntigravityResponse()

	var param any
	output := bytes.Join(
		convertAntigravityResponseToClaude(
			context.Background(), "gemini-3-flash-agent", requestJSON, requestJSON, responseJSON, &param,
		),
		nil,
	)
	output = append(
		output,
		bytes.Join(
			convertAntigravityResponseToClaude(
				context.Background(), "gemini-3-flash-agent", requestJSON, requestJSON, []byte("[DONE]"), &param,
			),
			nil,
		)...,
	)
	outputText := string(output)

	lastIndex := -1
	for _, eventName := range []string{
		"message_start",
		"content_block_start",
		"content_block_stop",
		"message_delta",
		"message_stop",
	} {
		index := strings.Index(outputText, "event: "+eventName+"\n")
		if index < 0 {
			t.Fatalf("event %q not found in:\n%s", eventName, outputText)
		}
		if index <= lastIndex {
			t.Fatalf("event %q is out of order in:\n%s", eventName, outputText)
		}
		lastIndex = index
	}

	contentBlockStart := sseDataForEvent(t, outputText, "content_block_start")
	if got := gjson.Get(contentBlockStart, "content_block.type").String(); got != "text" {
		t.Fatalf("empty content block type = %q, want text: %s", got, contentBlockStart)
	}
	if text := gjson.Get(contentBlockStart, "content_block.text"); !text.Exists() || text.String() != "" {
		t.Fatalf("empty content block text = %s, want empty string: %s", text.Raw, contentBlockStart)
	}

	messageDelta := sseDataForEvent(t, outputText, "message_delta")
	if got := gjson.Get(messageDelta, "delta.stop_reason").String(); got != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn: %s", got, messageDelta)
	}
	if got := gjson.Get(messageDelta, "usage.input_tokens").Int(); got != 64214 {
		t.Fatalf("input_tokens = %d, want 64214: %s", got, messageDelta)
	}
	if got := gjson.Get(messageDelta, "usage.output_tokens").Int(); got != 0 {
		t.Fatalf("output_tokens = %d, want 0: %s", got, messageDelta)
	}
}

func TestConvertAntigravityResponseToClaudeStreamRestoresDisambiguatedName(t *testing.T) {
	first := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build"
	second := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build_logs"
	original := []byte(`{"model":"gemini-3-flash","tools":[
		{"name":"` + first + `","input_schema":{"type":"object"}},
		{"name":"` + second + `","input_schema":{"type":"object"}}
	]}`)
	mapped := util.SanitizedFunctionNameMap(original)[second]
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"` + mapped + `","args":{}}}]}}]}}`)
	var param any

	output := bytes.Join(
		convertAntigravityResponseToClaude(
			context.Background(),
			"gemini-3-flash",
			original,
			original,
			responseJSON,
			&param,
		),
		nil,
	)
	contentBlockStart := sseDataForEvent(t, string(output), "content_block_start")
	if got := gjson.Get(contentBlockStart, "content_block.name").String(); got != second {
		t.Fatalf("stream tool name = %q, want %q. Output: %s", got, second, output)
	}
}

func TestConvertAntigravityResponseToClaudeNonStreamRestoresDisambiguatedName(t *testing.T) {
	first := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build"
	second := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build_logs"
	original := []byte(`{"model":"gemini-3-flash","tools":[
		{"name":"` + first + `","input_schema":{"type":"object"}},
		{"name":"` + second + `","input_schema":{"type":"object"}}
	]}`)
	mapped := util.SanitizedFunctionNameMap(original)[second]
	responseJSON := []byte(`{"response":{
		"responseId":"response-id",
		"modelVersion":"gemini-3-flash",
		"candidates":[{"content":{"parts":[{"functionCall":{"name":"` + mapped + `","args":{}}}]}}]
	}}`)

	output := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3-flash",
		original,
		original,
		responseJSON,
		nil,
	)
	if got := gjson.GetBytes(output, "content.0.name").String(); got != second {
		t.Fatalf("tool name = %q, want %q. Output: %s", got, second, output)
	}
}

func testEmptyAntigravityResponse() []byte {
	return []byte(`{
		"response": {
			"candidates": [{
				"content": {"role": "model", "parts": [{"text": ""}]},
				"finishReason": "STOP"
			}],
			"usageMetadata": {"promptTokenCount": 64214, "totalTokenCount": 64214},
			"modelVersion": "gemini-3-flash-a",
			"responseId": "eBNcat8X5evPsg_lhqyQAg"
		}
	}`)
}

func TestWebSearchResultsFromGrounding_DeduplicatesAndSkipsEmptyURLs(t *testing.T) {
	groundingMetadata := gjson.Parse(
		`{
		"groundingChunks": [
			{"web": {"uri": "https://example.com/a", "title": "A"}},
			{"web": {"uri": "https://example.com/b", "title": "B"}},
			{"web": {"uri": "https://example.com/a", "title": "A duplicate"}},
			{"web": {"uri": "", "title": "Empty"}}
		]
	}`,
	)

	results := webSearchResultsFromGrounding(groundingMetadata)

	if got := gjson.GetBytes(results, "#").Int(); got != 2 {
		t.Fatalf("result count = %d, want 2: %s", got, string(results))
	}
	if got := gjson.GetBytes(results, "0.url").String(); got != "https://example.com/a" {
		t.Fatalf("first url = %q: %s", got, string(results))
	}
	if got := gjson.GetBytes(results, "1.url").String(); got != "https://example.com/b" {
		t.Fatalf("second url = %q: %s", got, string(results))
	}
}

func TestBuildWebSearchCitedTextBlocks_TrimsOverlappingGroundingSupports(t *testing.T) {
	first := "北京今天晴"
	second := "北京今天晴，气温19到31度"
	textContent := second + "。"

	blocks := buildWebSearchCitedTextBlocks(
		textContent, []webSearchGroundingSupport{
			{
				StartIndex: 0,
				EndIndex:   int64(len([]byte(first))),
				Text:       first,
				ChunkURLs:  []string{"https://example.com/weather"},
				ChunkTitle: "Weather",
			},
			{
				StartIndex: 0,
				EndIndex:   int64(len([]byte(second))),
				Text:       second,
				ChunkURLs:  []string{"https://example.com/weather"},
				ChunkTitle: "Weather",
			},
		},
	)

	var got strings.Builder
	for _, block := range blocks {
		got.WriteString(block.Text)
	}
	if got.String() != textContent {
		t.Fatalf("joined text = %q, want %q", got.String(), textContent)
	}
	if len(blocks) < 2 || blocks[1].Text != "，气温19到31度" {
		t.Fatalf("overlap suffix block not trimmed correctly: %#v", blocks)
	}
	if gotCitation := blocks[1].Citations[0]["cited_text"]; gotCitation != blocks[1].Text {
		t.Fatalf("cited_text = %q, want emitted text %q", gotCitation, blocks[1].Text)
	}
}

func sseDataForEvent(t *testing.T, output string, eventName string) string {
	t.Helper()

	currentEvent := ""
	for line := range strings.SplitSeq(output, "\n") {
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			currentEvent = after
			continue
		}
		if currentEvent == eventName && strings.HasPrefix(line, "data: ") {
			return strings.TrimPrefix(line, "data: ")
		}
	}

	t.Fatalf("event %q not found in:\n%s", eventName, output)
	return ""
}

func testAntigravityGroundingResponse() []byte {
	resp := map[string]any{
		"response": map[string]any{
			"responseId":   "resp-web-search",
			"modelVersion": "gemini-3.1-flash-lite",
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"parts": []any{
							map[string]any{"text": "Beijing weather is clear today."},
						},
					},
					"groundingMetadata": map[string]any{
						"webSearchQueries": []any{"Beijing weather June 10 2026"},
						"groundingChunks": []any{
							map[string]any{
								"web": map[string]any{
									"uri":   "https://example.com/weather",
									"title": "Beijing Weather",
								},
							},
						},
						"groundingSupports": []any{
							map[string]any{
								"segment": map[string]any{
									"startIndex": int64(0),
									"endIndex":   int64(31),
									"text":       "Beijing weather is clear today.",
								},
								"groundingChunkIndices": []any{0},
							},
						},
					},
					"finishReason": "STOP",
				},
			},
			"usageMetadata": map[string]any{
				"promptTokenCount":     10,
				"candidatesTokenCount": 6,
				"totalTokenCount":      16,
			},
		},
	}
	raw, _ := json.Marshal(resp)
	return raw
}
