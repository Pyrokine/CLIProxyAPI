package claude

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestConvertGeminiResponseToClaude_EmitsSignatureOnlyChunk(t *testing.T) {
	var param any
	thinking := []byte(`{
		"candidates":[{"content":{"parts":[{"text":"private reasoning","thought":true}]}}]
	}`)
	validSignature := geminiClaudeSignatureForTest()
	signature := []byte(`{"candidates":[{"content":{"parts":[{"thoughtSignature":"` + validSignature + `"}]}}]}`)

	convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, thinking, &param)
	out := string(convertGeminiResponseToClaude(context.Background(), "gemini-2.5-pro", nil, nil, signature, &param)[0])

	if !strings.Contains(out, `"type":"signature_delta"`) || !strings.Contains(out, validSignature) {
		t.Fatalf("signature_delta missing: %s", out)
	}
	if strings.Contains(out, "text_delta") {
		t.Fatalf("signature-only chunk emitted text_delta: %s", out)
	}
}

func geminiClaudeSignatureForTest() string {
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, []byte{0x01, 0x02, 0x03, 0x04})
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func TestConvertGeminiResponseToClaude_PreservesStandaloneSignaturePart(t *testing.T) {
	var param any
	validSignature := geminiClaudeSignatureForTest()
	chunks := convertGeminiResponseToClaude(
		context.Background(),
		"gemini-2.5-pro",
		nil,
		nil,
		[]byte(`{"candidates":[{"content":{"parts":[{"thoughtSignature":"`+validSignature+`"}]}}]}`),
		&param,
	)
	if len(chunks) != 1 {
		t.Fatalf("chunk count = %d, want 1", len(chunks))
	}
	output := string(chunks[0])
	if !strings.Contains(output, `"content_block":{"type":"thinking"`) {
		t.Fatalf("standalone signature did not open a thinking block: %s", output)
	}
	if !strings.Contains(output, `"type":"signature_delta"`) || !strings.Contains(output, validSignature) {
		t.Fatalf("standalone signature delta missing: %s", output)
	}
}

func TestConvertGeminiResponseToClaude_StrictlyDropsInvalidSignatures(t *testing.T) {
	for _, invalidSignature := range []string{"opaque-client-signature", "EgQKAggM"} {
		t.Run(invalidSignature, func(t *testing.T) {
			var param any
			chunks := convertGeminiResponseToClaude(
				context.Background(),
				"gemini-2.5-pro",
				nil,
				nil,
				[]byte(`{"candidates":[{"content":{"parts":[{"text":"private","thought":true,"thoughtSignature":"`+invalidSignature+`"}]}}]}`),
				&param,
			)
			output := string(chunks[0])
			if strings.Contains(output, "signature_delta") || strings.Contains(output, invalidSignature) {
				t.Fatalf("invalid signature was emitted: %s", output)
			}
			if !strings.Contains(output, `"type":"thinking_delta"`) || strings.Contains(output, `"type":"text_delta"`) {
				t.Fatalf("hidden thought isolation changed: %s", output)
			}
		})
	}
}

func TestConvertGeminiResponseToClaudeNonStreamPreservesThoughtSignature(t *testing.T) {
	validSignature := geminiClaudeSignatureForTest()
	output := convertGeminiResponseToClaudeNonStream(
		context.Background(),
		"gemini-2.5-pro",
		nil,
		nil,
		[]byte(`{"candidates":[{"content":{"parts":[{"text":"private","thought":true},{"thoughtSignature":"`+validSignature+`"},{"text":"visible"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2}}`),
		nil,
	)
	thinking := gjson.GetBytes(output, "content.0")
	if thinking.Get("type").String() != "thinking" || thinking.Get("thinking").String() != "private" {
		t.Fatalf("thinking block = %s", thinking.Raw)
	}
	if got := thinking.Get("signature").String(); got != validSignature {
		t.Fatalf("thinking signature = %q, want %q", got, validSignature)
	}
	if got := gjson.GetBytes(output, "content.1.text").String(); got != "visible" {
		t.Fatalf("visible text = %q, want visible; output=%s", got, output)
	}
}

func TestConvertGeminiResponseToClaudePreservesToolEnvelope(t *testing.T) {
	validSignature := geminiClaudeSignatureForTest()
	part := `{"functionCall":{"id":"native-call-id","name":"lookup","args":{"query":"status"}},"thoughtSignature":"` + validSignature + `"}`

	var param any
	chunks := convertGeminiResponseToClaude(
		context.Background(), "gemini-2.5-pro", nil, nil,
		[]byte(`{"candidates":[{"content":{"parts":[`+part+`]}}]}`),
		&param,
	)
	streamOutput := string(chunks[0])
	if !strings.Contains(streamOutput, `"id":"native-call-id"`) ||
		!strings.Contains(streamOutput, `"signature":"`+validSignature+`"`) {
		t.Fatalf("stream tool envelope changed: %s", streamOutput)
	}

	nonStreamOutput := convertGeminiResponseToClaudeNonStream(
		context.Background(), "gemini-2.5-pro", nil, nil,
		[]byte(`{"candidates":[{"content":{"parts":[`+part+`]},"finishReason":"STOP"}]}`),
		nil,
	)
	tool := gjson.GetBytes(nonStreamOutput, "content.0")
	if tool.Get("id").String() != "native-call-id" || tool.Get("signature").String() != validSignature {
		t.Fatalf("non-stream tool envelope changed: %s", tool.Raw)
	}
}

func TestConvertGeminiResponseToClaudeSubtractsCacheReadUsage(t *testing.T) {
	var param any
	convertGeminiResponseToClaude(
		context.Background(), "gemini-2.5-pro", nil, nil,
		[]byte(`{"candidates":[{"content":{"parts":[{"text":"answer"}]}}]}`),
		&param,
	)
	stream := convertGeminiResponseToClaude(
		context.Background(), "gemini-2.5-pro", nil, nil,
		[]byte(`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"cachedContentTokenCount":4,"candidatesTokenCount":2}}`),
		&param,
	)
	streamOutput := string(stream[0])
	if !strings.Contains(streamOutput, `"input_tokens":6`) ||
		!strings.Contains(streamOutput, `"cache_read_input_tokens":4`) {
		t.Fatalf("stream cache usage mismatch: %s", streamOutput)
	}

	nonStreamOutput := convertGeminiResponseToClaudeNonStream(
		context.Background(), "gemini-2.5-pro", nil, nil,
		[]byte(`{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"cachedContentTokenCount":5,"candidatesTokenCount":2}}`),
		nil,
	)
	if got := gjson.GetBytes(nonStreamOutput, "usage.input_tokens").Int(); got != 0 {
		t.Fatalf("non-stream input_tokens = %d, want 0; output=%s", got, nonStreamOutput)
	}
	if got := gjson.GetBytes(nonStreamOutput, "usage.cache_read_input_tokens").Int(); got != 5 {
		t.Fatalf("non-stream cache_read_input_tokens = %d, want 5; output=%s", got, nonStreamOutput)
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
