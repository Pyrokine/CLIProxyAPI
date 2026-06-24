package claude

import (
	"context"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
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
	ConvertAntigravityResponseToClaude(
		ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, responseJSON, &param,
	)

	params := param.(*Params)
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
	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, chunk1, &param)
	params := param.(*Params)

	if params.CurrentThinkingText.Len() == 0 {
		t.Error("Thinking text should be accumulated after first chunk")
	}

	// Process second chunk - continues thinking block
	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, chunk2, &param)

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
	ConvertAntigravityResponseToClaude(
		ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, thinkingChunk, &param,
	)
	params := param.(*Params)
	thinkingText := params.CurrentThinkingText.String()

	if thinkingText == "" {
		t.Fatal("Thinking text should be accumulated")
	}

	// Process signature chunk - should cache the signature
	ConvertAntigravityResponseToClaude(
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
	ConvertAntigravityResponseToClaude(
		ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, block1Thinking, &param,
	)
	params := param.(*Params)
	firstThinkingText := params.CurrentThinkingText.String()

	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, block1Sig, &param)

	// Verify first signature cached
	if cache.GetCachedSignature("claude-sonnet-4-5-thinking", firstThinkingText) != validSig1 {
		t.Error("First thinking block signature should be cached")
	}

	// Process text (transitions out of thinking)
	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, textBlock, &param)

	// Process second thinking block
	ConvertAntigravityResponseToClaude(
		ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, block2Thinking, &param,
	)
	secondThinkingText := params.CurrentThinkingText.String()

	ConvertAntigravityResponseToClaude(ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, block2Sig, &param)

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
	ConvertAntigravityResponseToClaude(
		ctx, "claude-sonnet-4-5-thinking", requestJSON, requestJSON, thinkingChunk, &param,
	)
	out := string(
		ConvertAntigravityResponseToClaude(
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

	out := ConvertAntigravityResponseToClaudeNonStream(
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
