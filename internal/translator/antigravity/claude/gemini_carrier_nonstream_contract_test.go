package claude

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestConvertAntigravityResponseToClaudeNonStream_SignedThoughtBeforeUnsignedTextKeepsTarget(t *testing.T) {
	signature := testGeminiEPrefixSignature(t)
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high"}`)
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hid` +
		`den","thought":true,` +
		`"thoughtSignature":"` + signature + `"},{"text":"visible"}]},"finishReason":"STOP"}],` +
		`"modelVersion":"gemini-3.6-flash",` +
		`"responseId":"signed-thought-unsigned-text"}}`)

	output := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3.6-flash-high",
		requestJSON,
		requestJSON,
		responseJSON,
		nil,
	)
	replayRequest := []byte(`{"model":"gemini-3.6-flash-high",` +
		`"messages":[{"role":"assistant","content":[]}]}`)
	replayRequest, _ = sjson.SetRawBytes(
		replayRequest,
		"messages.0.content",
		[]byte(gjson.GetBytes(output, "content").Raw),
	)
	replayRequest = StripInvalidGeminiSignatureThinkingBlocks(replayRequest)
	translated := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", replayRequest, false)
	parts := gjson.GetBytes(translated, "request.contents.0.parts").Array()
	if len(parts) != 2 || parts[0].Get("text").String() != "hidden" || !parts[0].Get("thought").Bool() ||
		parts[0].Get("thoughtSignature").String() != signature ||
		parts[1].Get("text").String() != "visible" ||
		parts[1].Get("thoughtSignature").String() != "" {
		t.Fatalf("signed thought target changed: output=%s translated=%s", output, translated)
	}
}

func TestConvertAntigravityResponseToClaudeNonStream_PreviousCarrierDoesNotCrossFollowingText(t *testing.T) {
	signature1 := testGeminiEPrefixSignature(t)
	signature2 := geminiCarrierSignatureForTest(t, 0x35)
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high"}`)
	responseJSON := []byte(
		`{"response":{"candidates":[{"content":{"parts":[{"text":"A",` +
			`"thoughtSignature":"` +
			signature1 +
			`"},{"text":"","thoughtSignature":"` +
			signature2 +
			`"},{"text":"B"}]},"finishReason":"STOP"}],` +
			`"modelVersion":"gemini-3.6-flash",` +
			`"responseId":"previous-carrier-boundary"}}`,
	)

	output := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3.6-flash-high",
		requestJSON,
		requestJSON,
		responseJSON,
		nil,
	)
	replayRequest := []byte(`{"model":"gemini-3.6-flash-high",` +
		`"messages":[{"role":"assistant","content":[]}]}`)
	replayRequest, _ = sjson.SetRawBytes(
		replayRequest,
		"messages.0.content",
		[]byte(gjson.GetBytes(output, "content").Raw),
	)
	replayRequest = StripInvalidGeminiSignatureThinkingBlocks(replayRequest)
	translated := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", replayRequest, false)
	parts := gjson.GetBytes(translated, "request.contents.0.parts").Array()
	if len(parts) != 3 || parts[0].Get("text").String() != "A" ||
		parts[0].Get("thoughtSignature").String() != signature1 ||
		!parts[1].Get("text").Exists() ||
		parts[1].Get("text").String() != "" ||
		parts[1].Get("thoughtSignature").String() != signature2 ||
		parts[2].Get("text").String() != "B" ||
		parts[2].Get("thoughtSignature").String() != "" {
		t.Fatalf("previous carrier crossed following text: output=%s translated=%s", output, translated)
	}
}

func TestConvertAntigravityResponseToClaudeNonStream_PreservesDistinctThoughtAndTextSignatures(t *testing.T) {
	sig1 := testGeminiEPrefixSignature(t)
	sig2 := geminiCarrierSignatureForTest(t, 0x35)
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high"}`)
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hid` +
		`den","thought":true,` +
		`"thoughtSignature":"` +
		sig1 +
		`"},{"text":"visible","thoughtSignature":"` +
		sig2 +
		`"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.6-flash",` +
		`"responseId":"resp-distinct-signatures"}}`)

	output := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3.6-flash-high",
		requestJSON,
		requestJSON,
		responseJSON,
		nil,
	)
	content := gjson.GetBytes(output, "content").Array()
	if len(content) != 3 {
		t.Fatalf("content blocks = %d, want signed thought + carrier + text; output=%s", len(content), output)
	}
	if got := content[0].Get("thinking").String(); got != "hidden" {
		t.Fatalf("thought text = %q; output=%s", got, output)
	}
	wantThoughtCarrier := encodeGeminiClaudeCarrierSignature(
		sig1, geminiClaudeCarrierStandalone, geminiClaudeCarrierText,
	)
	if got := content[0].Get("signature").String(); got != wantThoughtCarrier {
		t.Fatalf("thought signature = %q, want standalone carrier %q; output=%s", got, wantThoughtCarrier, output)
	}
	wantCarrier := encodeGeminiClaudeCarrierSignature(sig2, geminiClaudeCarrierNext, geminiClaudeCarrierText)
	if got := content[1].Get("signature").String(); got != wantCarrier || content[1].Get("thinking").String() != "" {
		t.Fatalf("visible carrier malformed: %s; output=%s", content[1].Raw, output)
	}
	if got := content[2].Get("text").String(); got != "visible" {
		t.Fatalf("visible text = %q; output=%s", got, output)
	}

	replayRequest := []byte(`{"model":"gemini-3.6-flash-high",` +
		`"messages":[{"role":"assistant","content":[]},` +
		`{"role":"user","content":[{"type":"text","text":"continue"}]}]}`)
	replayRequest, _ = sjson.SetRawBytes(
		replayRequest,
		"messages.0.content",
		[]byte(gjson.GetBytes(output, "content").Raw),
	)
	replayRequest = StripInvalidGeminiSignatureThinkingBlocks(replayRequest)
	translated := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", replayRequest, false)
	parts := gjson.GetBytes(translated, "request.contents.0.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("replayed parts = %d, want thought + text; translated=%s", len(parts), translated)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != sig1 {
		t.Fatalf("replayed thought signature = %q, want %q; translated=%s", got, sig1, translated)
	}
	if got := parts[1].Get("thoughtSignature").String(); got != sig2 {
		t.Fatalf("replayed text signature = %q, want %q; translated=%s", got, sig2, translated)
	}
}

func TestConvertAntigravityResponseToClaudeNonStream_ThoughtBeforeSignedToolRoundTrips(t *testing.T) {
	validSignature := testGeminiEPrefixSignature(t)
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high"}`)
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hid` +
		`den analysis",` +
		`"thought":true},{"thoughtSignature":"` +
		validSignature +
		`","functionCall":{"id":"native-id","name":"run_command",` +
		`"args":{"command":"true"}}}]},"finishReason":"STOP"}],` +
		`"modelVersion":"gemini-3.6-flash",` +
		`"responseId":"resp-thought-tool"}}`)

	output := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3.6-flash-high",
		requestJSON,
		requestJSON,
		responseJSON,
		nil,
	)
	if got := gjson.GetBytes(output, "content.#").Int(); got != 2 {
		t.Fatalf("content blocks = %d, want thinking + tool_use; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "content.0.thinking").String(); got != "hidden analysis" {
		t.Fatalf("thinking text = %q; output=%s", got, output)
	}
	wantCarrier := encodeGeminiClaudeCarrierSignature(
		validSignature, geminiClaudeCarrierNext, geminiClaudeCarrierFunction,
	)
	if got := gjson.GetBytes(output, "content.0.signature").String(); got != wantCarrier {
		t.Fatalf("thinking carrier signature = %q, want %q; output=%s", got, wantCarrier, output)
	}

	replayRequest := []byte(`{"model":"gemini-3.6-flash-high",` +
		`"messages":[{"role":"assistant","content":[]},` +
		`{"role":"user","content":[{"type":"text","text":"continue"}]}]}`)
	replayRequest, _ = sjson.SetRawBytes(
		replayRequest,
		"messages.0.content",
		[]byte(gjson.GetBytes(output, "content").Raw),
	)
	replayRequest = StripInvalidGeminiSignatureThinkingBlocks(replayRequest)
	translated := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", replayRequest, false)
	if got := gjson.GetBytes(translated, "request.contents.0.parts.0.text").String(); got != "hidden analysis" {
		t.Fatalf("replayed thought text = %q; translated=%s", got, translated)
	}
	if gjson.GetBytes(translated, "request.contents.0.parts.0.thoughtSignature").Exists() {
		t.Fatalf("thought part must remain unsigned; translated=%s", translated)
	}
	if got := gjson.GetBytes(
		translated, "request.contents.0.parts.1.thoughtSignature",
	).String(); got != validSignature {
		t.Fatalf("tool signature = %q, want %q; translated=%s", got, validSignature, translated)
	}
}

func TestConvertAntigravityResponseToClaudeNonStream_DetachedGeminiSignatureAfterVisibleText(t *testing.T) {
	validSignature := testGeminiEPrefixSignature(t)
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high"}`)
	//goland:noinspection SpellCheckingInspection
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"vis` +
		`ible answer"},` +
		`{"text":"","thoughtSignature":"` + validSignature + `"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":10,` +
		`"candidatesTokenCount":2,"thoughtsTokenCount":3,` +
		`"totalTokenCount":15},` +
		`"modelVersion":"gemini-3.6-flash","responseId":"resp-detached"}}`)
	output := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3.6-flash-high",
		requestJSON,
		requestJSON,
		responseJSON,
		nil,
	)
	if got := gjson.GetBytes(output, "content.#").Int(); got != 2 {
		t.Fatalf("content blocks = %d, want text + detached thinking; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "content.0.text").String(); got != "visible answer" {
		t.Fatalf("visible text = %q; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "content.1.type").String(); got != "thinking" {
		t.Fatalf("detached block type = %q; output=%s", got, output)
	}
	wantCarrier := encodeGeminiClaudeCarrierSignature(
		validSignature, geminiClaudeCarrierPrevious, geminiClaudeCarrierText,
	)
	if got := gjson.GetBytes(output, "content.1.signature").String(); got != wantCarrier {
		t.Fatalf("detached signature = %q, want %q; output=%s", got, wantCarrier, output)
	}
}

func TestConvertAntigravityResponseToClaudeNonStream_TextWithThoughtSignatureStaysText(t *testing.T) {

	requestJSON := []byte(`{"model":"gemini-3.1-pro-low"}`)
	translatedRequestJSON := []byte(`{"model":"gemini-3.1-pro-low"}`)
	responseJSON := []byte(`{
		"response": {
			"candidates": [{
				"content": {
					"parts": [
						{"text": "I need to multiply 17 by 24.", "thought": true},
						{"text": "408", "thoughtSignature": "sig-final-answer"}
					]
				},
				"finishReason": "STOP"
			}],
			"usageMetadata": {
				"promptTokenCount": 16,
				"candidatesTokenCount": 3,
				"thoughtsTokenCount": 42,
				"totalTokenCount": 61
			},
			"modelVersion": "gemini-3.1-pro-low",
			"responseId": "resp-text-sig"
		}
	}`)

	output := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3.1-pro-low",
		requestJSON,
		translatedRequestJSON,
		responseJSON,
		nil,
	)
	if got := gjson.GetBytes(output, "content.#").Int(); got != 2 {
		t.Fatalf("content block count = %d, want 2. Output: %s", got, output)
	}
	if got := gjson.GetBytes(output, "content.0.type").String(); got != "thinking" {
		t.Fatalf("content.0.type = %q, want thinking. Output: %s", got, output)
	}
	if got := gjson.GetBytes(output, "content.0.thinking").String(); got != "I need to multiply 17 by 24." {
		t.Fatalf("thinking = %q, want thought text. Output: %s", got, output)
	}
	wantCarrier := encodeGeminiClaudeCarrierSignature(
		"sig-final-answer", geminiClaudeCarrierNext, geminiClaudeCarrierText,
	)
	if got := gjson.GetBytes(output, "content.0.signature").String(); got != wantCarrier {
		t.Fatalf("signature = %q, want %q. Output: %s", got, wantCarrier, output)
	}
	if got := gjson.GetBytes(output, "content.1.type").String(); got != "text" {
		t.Fatalf("content.1.type = %q, want text. Output: %s", got, output)
	}
	if got := gjson.GetBytes(output, "content.1.text").String(); got != "408" {
		t.Fatalf("text = %q, want final answer. Output: %s", got, output)
	}
}
