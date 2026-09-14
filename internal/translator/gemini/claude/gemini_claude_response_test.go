package claude

import (
	"context"
	"strings"
	"testing"
)

func TestConvertGeminiResponseToClaude_EmitsSignatureOnlyChunk(t *testing.T) {
	var param any
	thinking := []byte(`{
		"candidates":[{"content":{"parts":[{"text":"private reasoning","thought":true}]}}]
	}`)
	signature := []byte(`{
		"candidates":[{"content":{"parts":[{"thoughtSignature":"sig-123"}]}}]
	}`)

	convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, thinking, &param)
	out := string(convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, signature, &param)[0])

	if !strings.Contains(out, `"type":"signature_delta"`) || !strings.Contains(out, "sig-123") {
		t.Fatalf("signature_delta missing: %s", out)
	}
	if strings.Contains(out, "text_delta") {
		t.Fatalf("signature-only chunk emitted text_delta: %s", out)
	}
}

func TestConvertGeminiResponseToClaude_FinalEventsOnlyOnce(t *testing.T) {
	var param any
	thinking := []byte(`{
		"candidates":[{"content":{"parts":[{"text":"answer"}]}}]
	}`)
	final := []byte(`{
		"candidates":[{"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}
	}`)

	convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, thinking, &param)
	first := string(convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, final, &param)[0])
	second := string(convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, final, &param)[0])

	if !strings.Contains(first, "message_delta") {
		t.Fatalf("first final chunk missing message_delta: %s", first)
	}
	if strings.Contains(second, "message_delta") {
		t.Fatalf("second final chunk repeated message_delta: %s", second)
	}
}
