package claude

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestConvertAntigravityResponseToClaude_VisibleGeminiSignatureUsesLeadingCarrier(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high"}`)
	validSignature := testGeminiEPrefixSignature(t)
	//goland:noinspection SpellCheckingInspection
	chunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"vis` +
		`ible answer",` +
		`"thoughtSignature":"` + validSignature + `"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.6-flash",` +
		`"responseId":"resp-visible-sig"}}`)
	var param any
	output := bytes.Join(
		convertAntigravityResponseToClaude(
			context.Background(),
			"gemini-3.6-flash-high",
			requestJSON,
			requestJSON,
			chunk,
			&param,
		),
		nil,
	)
	outputText := string(output)
	carrierPos := strings.Index(outputText, `"content_block":{"type":"thinking","thinking":""}`)
	carrierSignature := encodeGeminiClaudeCarrierSignature(
		validSignature,
		geminiClaudeCarrierNext,
		geminiClaudeCarrierText,
	)
	signaturePos := strings.Index(outputText, `"type":"signature_delta","signature":"`+carrierSignature+`"`)
	textPos := strings.Index(outputText, `"type":"text_delta","text":"visible answer"`)
	if carrierPos < 0 || signaturePos < carrierPos || textPos < signaturePos {
		t.Fatalf("visible signature carrier must precede text: %s", output)
	}
}

func TestConvertAntigravityResponseToClaude_ThoughtThenSignedFunctionUsesOneThinkingBlock(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high"}`)
	validSignature := testGeminiEPrefixSignature(t)
	chunks := [][]byte{
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hid` +
			`den thought",` +
			`"thought":true}]}}],"modelVersion":"gemini-3.6-flash",` +
			`"responseId":"resp-thought-tool"}}`),
		[]byte(
			`{"response":{"candidates":[{"content":{"parts":[{"thoughtSig` +
				`nature":"` + validSignature + `","functionCall":{"name":"run_command",` +
				`"args":{"command":"true"}}}]},` +
				`"finishReason":"STOP"}],"modelVersion":"gemini-3.6-flash",` +
				`"responseId":"resp-thought-tool"}}`,
		),
	}
	var param any
	var output []byte
	for _, chunk := range chunks {
		output = append(
			output,
			bytes.Join(
				convertAntigravityResponseToClaude(
					context.Background(),
					"gemini-3.6-flash-high",
					requestJSON,
					requestJSON,
					chunk,
					&param,
				),
				nil,
			)...,
		)
	}
	outputText := string(output)
	if got := strings.Count(outputText, `"content_block":{"type":"thinking"`); got != 1 {
		t.Fatalf("thinking block count = %d, want one signed thought block: %s", got, output)
	}
	carrierSignature := encodeGeminiClaudeCarrierSignature(
		validSignature,
		geminiClaudeCarrierNext,
		geminiClaudeCarrierFunction,
	)
	signaturePos := strings.Index(outputText, `"type":"signature_delta","signature":"`+carrierSignature+`"`)
	toolPos := strings.Index(outputText, `"content_block":{"type":"tool_use"`)
	if signaturePos < 0 || toolPos < signaturePos {
		t.Fatalf("signed thinking block must precede tool: %s", output)
	}
}

func TestConvertAntigravityResponseToClaude_DetachedGeminiSignatureAfterVisibleText(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high","messages":[{"role":"user",` +
		`"content":[{"type":"text","text":"Test"}]}]}`)
	validSignature := testGeminiEPrefixSignature(t)
	//goland:noinspection SpellCheckingInspection
	chunks := [][]byte{
		[]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"vis` +
			`ible answer"}]}}],` +
			`"modelVersion":"gemini-3.6-flash","responseId":"resp-detached"}}`),
		[]byte(
			`{"response":{"candidates":[{"content":{"parts":[{"text":"",` +
				`"thoughtSignature":"` + validSignature + `"}]},"finishReason":"STOP"}],` +
				`"usageMetadata":{"promptTokenCount":10,` +
				`"candidatesTokenCount":2,"thoughtsTokenCount":3,` +
				`"totalTokenCount":15},` +
				`"modelVersion":"gemini-3.6-flash","responseId":"resp-detached"}}`,
		),
	}

	var param any
	var output []byte
	for _, chunk := range chunks {
		output = append(
			output,
			bytes.Join(
				convertAntigravityResponseToClaude(
					context.Background(),
					"gemini-3.6-flash-high",
					requestJSON,
					requestJSON,
					chunk,
					&param,
				),
				nil,
			)...,
		)
	}
	output = append(
		output,
		bytes.Join(
			convertAntigravityResponseToClaude(
				context.Background(),
				"gemini-3.6-flash-high",
				requestJSON,
				requestJSON,
				[]byte("[DONE]"),
				&param,
			),
			nil,
		)...,
	)
	outputText := string(output)

	if !strings.Contains(outputText, `"content_block":{"type":"text","text":""}`) {
		t.Fatalf("missing visible text block: %s", outputText)
	}
	if !strings.Contains(outputText, `"content_block":{"type":"thinking","thinking":""}`) {
		t.Fatalf("missing detached thinking carrier: %s", outputText)
	}
	carrierSignature := encodeGeminiClaudeCarrierSignature(
		validSignature,
		geminiClaudeCarrierPrevious,
		geminiClaudeCarrierText,
	)
	if !strings.Contains(outputText, `"type":"signature_delta","signature":"`+carrierSignature+`"`) {
		t.Fatalf("missing detached Gemini signature: %s", outputText)
	}
	if got := strings.Count(outputText, `"type":"content_block_stop"`); got != 2 {
		t.Fatalf("content block stops = %d, want text + detached thinking; output=%s", got, outputText)
	}
}

func TestConvertAntigravityResponseToClaude_GeminiToolSignature(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high","messages":[{"role":"user",` +
		`"content":[{"type":"text","text":"Test"}]}]}`)
	validSignature := testGeminiEPrefixSignature(t)
	chunk := []byte(
		`{"response":{"candidates":[{"content":{"parts":[{"thoughtSig` +
			`nature":"` + validSignature + `","functionCall":{"id":"native-id","name":"run_command",` +
			`"args":{"command":"true"}}}]},"finishReason":"STOP"}],` +
			`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,` +
			`"thoughtsTokenCount":3,"totalTokenCount":15},` +
			`"modelVersion":"gemini-3.6-flash",` +
			`"responseId":"resp-tool"}}`,
	)

	var param any
	output := bytes.Join(
		convertAntigravityResponseToClaude(
			context.Background(),
			"gemini-3.6-flash-high",
			requestJSON,
			requestJSON,
			chunk,
			&param,
		),
		nil,
	)
	outputText := string(output)
	carrierPos := strings.Index(outputText, `"content_block":{"type":"thinking","thinking":""}`)
	carrierSignature := encodeGeminiClaudeCarrierSignature(
		validSignature,
		geminiClaudeCarrierNext,
		geminiClaudeCarrierFunction,
	)
	signaturePos := strings.Index(outputText, `"type":"signature_delta","signature":"`+carrierSignature+`"`)
	toolPos := strings.Index(outputText, `"content_block":{"type":"tool_use"`)
	if carrierPos < 0 || signaturePos < carrierPos || toolPos < signaturePos {
		t.Fatalf("tool signature carrier must precede tool_use: %s", output)
	}
}

func TestConvertAntigravityResponseToClaudeStream_PreservesDistinctThoughtAndTextSignatures(t *testing.T) {
	sig1 := testGeminiEPrefixSignature(t)
	sig2 := geminiCarrierSignatureForTest(t, 0x35)
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high"}`)
	chunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hid` +
		`den","thought":true,` +
		(`"thoughtSignature":` +
			`"`) +
		sig1 +
		(`"},{"text":"visible","thoughtSignature":` +
			`"`) +
		sig2 +
		(`"}]},"finishRea` +
			`son":"STOP"}],`) +
		`"usageMetadata":{"promptTokenCount":1,` +
		`"candidatesTokenCount":1,"thoughtsTokenCount":1,` +
		`"totalTokenCount":3},` +
		`"modelVersion":"gemini-3.6-flash",` +
		`"responseId":"resp-distinct-signatures"}}`)
	var param any
	output := bytes.Join(
		convertAntigravityResponseToClaude(
			context.Background(),
			"gemini-3.6-flash-high",
			requestJSON,
			requestJSON,
			chunk,
			&param,
		),
		nil,
	)
	outputText := string(output)
	if got := strings.Count(outputText, `"content_block":{"type":"thinking"`); got != 2 {
		t.Fatalf("thinking block count = %d, want 2; output=%s", got, output)
	}
	if got := strings.Count(outputText, `"type":"signature_delta"`); got != 2 {
		t.Fatalf("signature delta count = %d, want 2; output=%s", got, output)
	}
	firstCarrier := encodeGeminiClaudeCarrierSignature(
		sig1,
		geminiClaudeCarrierStandalone,
		geminiClaudeCarrierText,
	)
	firstSignature := strings.Index(outputText, `"signature":"`+firstCarrier+`"`)
	secondCarrier := encodeGeminiClaudeCarrierSignature(sig2, geminiClaudeCarrierNext, geminiClaudeCarrierText)
	secondSignature := strings.Index(outputText, `"signature":"`+secondCarrier+`"`)
	visibleText := strings.Index(outputText, `"text":"visible"`)
	if firstSignature < 0 || secondSignature < firstSignature || visibleText < secondSignature {
		t.Fatalf("signature/text order is wrong; output=%s", output)
	}
}

func TestConvertAntigravityResponseToClaudeStream_TextWithThoughtSignatureStaysText(t *testing.T) {

	requestJSON := []byte(`{"model":"gemini-3.1-pro-low"}`)
	translatedRequestJSON := []byte(`{"model":"gemini-3.1-pro-low"}`)
	//goland:noinspection SpellCheckingInspection
	thoughtChunk := []byte(`{
		"response": {
			"candidates": [{"content": {"parts": [{"text": "I need to m` +
		`ultiply 17 by 24.", "thought": true}]}}],` +
		`
			"modelVersion": "gemini-3.1-pro-low",` +
		`
			"responseId": "resp-text-sig"
		}
	}`)
	textChunk := []byte(`{
		"response": {
			"candidates": [{"content": {"parts": [{"text": "408",` +
		` "thoughtSignature": "sig-final-answer"}]}}],` +
		`
			"modelVersion": "gemini-3.1-pro-low",` +
		`
			"responseId": "resp-text-sig"
		}
	}`)
	finishChunk := []byte(`{
		"response": {
			"candidates": [{"finishReason": "STOP"}],` +
		`
			"usageMetadata": {"promptTokenCount": 16,` +
		` "candidatesTokenCount": 3,` +
		` "thoughtsTokenCount": 42, "totalTokenCount": 61},` +
		`
			"modelVersion": "gemini-3.1-pro-low",` +
		`
			"responseId": "resp-text-sig"
		}
	}`)

	var param any
	ctx := context.Background()
	output := bytes.Join(
		convertAntigravityResponseToClaude(
			ctx,
			"gemini-3.1-pro-low",
			requestJSON,
			translatedRequestJSON,
			thoughtChunk,
			&param,
		),
		nil,
	)
	output = append(
		output,
		bytes.Join(
			convertAntigravityResponseToClaude(
				ctx, "gemini-3.1-pro-low", requestJSON, translatedRequestJSON, textChunk, &param,
			),
			nil,
		)...,
	)
	output = append(
		output,
		bytes.Join(
			convertAntigravityResponseToClaude(
				ctx,
				"gemini-3.1-pro-low",
				requestJSON,
				translatedRequestJSON,
				finishChunk,
				&param,
			),
			nil,
		)...,
	)
	output = append(
		output,
		bytes.Join(
			convertAntigravityResponseToClaude(
				ctx,
				"gemini-3.1-pro-low",
				requestJSON,
				translatedRequestJSON,
				[]byte("[DONE]"),
				&param,
			),
			nil,
		)...,
	)
	outputText := string(output)

	wantCarrier := encodeGeminiClaudeCarrierSignature(
		"sig-final-answer", geminiClaudeCarrierNext, geminiClaudeCarrierText,
	)
	if !strings.Contains(outputText, `"delta":{"type":"signature_delta","signature":"`+wantCarrier+`"}`) {
		t.Fatalf("expected signature delta for thinking block: %s", outputText)
	}
	if !strings.Contains(outputText, `"content_block":{"type":"text","text":""}`) {
		t.Fatalf("expected text content block after thinking: %s", outputText)
	}
	if !strings.Contains(outputText, `"delta":{"type":"text_delta","text":"408"}`) {
		t.Fatalf("expected final answer as text delta: %s", outputText)
	}
	if strings.Contains(outputText, `"delta":{"type":"thinking_delta","thinking":"408"}`) {
		t.Fatalf("final answer must not be emitted as thinking delta: %s", outputText)
	}
}
