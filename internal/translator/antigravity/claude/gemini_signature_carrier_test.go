package claude

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func geminiCarrierSignatureForTest(t *testing.T, discriminator byte) string {
	t.Helper()

	inner := protowire.AppendTag(nil, 1, protowire.BytesType)
	inner = protowire.AppendBytes(inner, []byte{0x01, 0x0c, 0x39, 0xd6, 0xc7, discriminator})
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, inner)
	return base64.StdEncoding.EncodeToString(payload)
}

func TestConvertClaudeRequestToAntigravityReattachesTrailingGeminiSignature(t *testing.T) {
	nativeSignature := geminiCarrierSignatureForTest(t, 0x34)
	input := []byte(`{"messages":[{"role":"assistant","content":[{"type":"text","text":"visible"},{"type":"thinking","thinking":"","signature":"` + nativeSignature + `"}]}]}`)

	output := convertClaudeRequestToAntigravity("gemini-3-pro-preview", input, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 1 || parts[0].Get("text").String() != "visible" {
		t.Fatalf("native text part malformed: %s", output)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != nativeSignature {
		t.Fatalf("thoughtSignature = %q, want %q; output=%s", got, nativeSignature, output)
	}
}

func TestConvertClaudeRequestToAntigravityPreservesConsecutiveGeminiCarriers(t *testing.T) {
	signature1 := geminiCarrierSignatureForTest(t, 0x34)
	signature2 := geminiCarrierSignatureForTest(t, 0x35)
	input := []byte(`{"messages":[{"role":"assistant","content":[{"type":"text","text":"first"},{"type":"text","text":"second"},{"type":"thinking","thinking":"","signature":"` + signature1 + `"},{"type":"thinking","thinking":"","signature":"` + signature2 + `"}]}]}`)

	output := convertClaudeRequestToAntigravity("gemini-3-pro-preview", input, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want two text parts and one carrier; output=%s", len(parts), output)
	}
	if got := parts[1].Get("thoughtSignature").String(); got != signature1 {
		t.Fatalf("nearest text signature = %q, want %q; output=%s", got, signature1, output)
	}
	if got := parts[2].Get("thoughtSignature").String(); got != signature2 ||
		!parts[2].Get("text").Exists() || parts[2].Get("text").String() != "" {
		t.Fatalf("detached carrier malformed: %s; output=%s", parts[2].Raw, output)
	}
}

func TestConvertClaudeRequestToAntigravityTargetsFollowingGeminiTool(t *testing.T) {
	nativeSignature := geminiCarrierSignatureForTest(t, 0x34)
	carrier := encodeGeminiClaudeCarrierSignature(
		nativeSignature,
		geminiClaudeCarrierNext,
		geminiClaudeCarrierFunction,
	)
	input := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hidden","signature":"` + carrier + `"},{"type":"tool_use","id":"tool-1","name":"read","input":{"path":"a"}}]}]}`)

	output := convertClaudeRequestToAntigravity("gemini-3-pro-preview", input, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 2 || !parts[0].Get("thought").Bool() || !parts[1].Get("functionCall").Exists() {
		t.Fatalf("thought and tool parts malformed: %s", output)
	}
	if parts[0].Get("thoughtSignature").Exists() {
		t.Fatalf("signature remained on thought part: %s", output)
	}
	if got := parts[1].Get("thoughtSignature").String(); got != nativeSignature {
		t.Fatalf("tool signature = %q, want %q; output=%s", got, nativeSignature, output)
	}
}

func TestConvertClaudeRequestToAntigravityDropsMismatchedGeminiCarrier(t *testing.T) {
	nativeSignature := geminiCarrierSignatureForTest(t, 0x34)
	carrier := encodeGeminiClaudeCarrierSignature(
		nativeSignature,
		geminiClaudeCarrierNext,
		geminiClaudeCarrierFunction,
	)
	input := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hidden","signature":"` + carrier + `"},{"type":"text","text":"visible"}]}]}`)

	output := convertClaudeRequestToAntigravity("gemini-3-pro-preview", input, true)
	if strings.Contains(string(output), nativeSignature) || strings.Contains(
		string(output), geminiClaudeCarrierPrefix,
	) {
		t.Fatalf("mismatched carrier reached Gemini wire: %s", output)
	}
}

func TestStripInvalidGeminiSignatureThinkingBlocksDropsUserCarrier(t *testing.T) {
	nativeSignature := geminiCarrierSignatureForTest(t, 0x34)
	carrier := encodeGeminiClaudeCarrierSignature(
		nativeSignature,
		geminiClaudeCarrierNext,
		geminiClaudeCarrierText,
	)
	input := []byte(`{"messages":[{"role":"user","content":[{"type":"thinking","thinking":"","signature":"` + carrier + `"},{"type":"text","text":"user text"}]}]}`)

	output := StripInvalidGeminiSignatureThinkingBlocks(input)
	if got := gjson.GetBytes(output, "messages.0.content.#").Int(); got != 1 {
		t.Fatalf("content count = %d, want one user text block; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "messages.0.content.0.text").String(); got != "user text" {
		t.Fatalf("user text = %q; output=%s", got, output)
	}
}

func TestConvertAntigravityResponseToClaudeVisibleGeminiSignatureCarrier(t *testing.T) {
	nativeSignature := geminiCarrierSignatureForTest(t, 0x34)
	request := []byte(`{"model":"gemini-3-pro-preview"}`)
	response := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"visible","thoughtSignature":"` + nativeSignature + `"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3-pro-preview","responseId":"response-1"}}`)

	var params any
	output := bytes.Join(
		convertAntigravityResponseToClaude(
			context.Background(),
			"gemini-3-pro-preview",
			request,
			request,
			response,
			&params,
		), nil,
	)
	outputText := string(output)
	carrier := encodeGeminiClaudeCarrierSignature(
		nativeSignature,
		geminiClaudeCarrierNext,
		geminiClaudeCarrierText,
	)
	carrierPosition := strings.Index(outputText, `"signature":"`+carrier+`"`)
	textPosition := strings.Index(outputText, `"text":"visible"`)
	if carrierPosition < 0 || textPosition < carrierPosition {
		t.Fatalf("carrier must precede visible text: %s", output)
	}
}

func TestConvertAntigravityResponseToClaudeGeminiToolCarrierAndStableID(t *testing.T) {
	nativeSignature := geminiCarrierSignatureForTest(t, 0x34)
	request := []byte(`{"model":"gemini-3-pro-preview"}`)
	response := []byte(`{"response":{"candidates":[{"content":{"parts":[{"thoughtSignature":"` + nativeSignature + `","functionCall":{"id":"native-call-1","name":"read","args":{"path":"a"}}}]},"finishReason":"STOP"}],"modelVersion":"gemini-3-pro-preview","responseId":"response-1"}}`)
	wantID := util.GeminiClaudeToolUseID("native-call-1", "read", `{"path":"a"}`)

	nonStream := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3-pro-preview",
		request,
		request,
		response,
		nil,
	)
	if got := gjson.GetBytes(nonStream, `content.#(type=="tool_use").id`).String(); got != wantID {
		t.Fatalf("non-stream tool ID = %q, want %q; output=%s", got, wantID, nonStream)
	}

	var params any
	stream := bytes.Join(
		convertAntigravityResponseToClaude(
			context.Background(),
			"gemini-3-pro-preview",
			request,
			request,
			response,
			&params,
		), nil,
	)
	carrier := encodeGeminiClaudeCarrierSignature(
		nativeSignature,
		geminiClaudeCarrierNext,
		geminiClaudeCarrierFunction,
	)
	streamText := string(stream)
	if !strings.Contains(streamText, `"signature":"`+carrier+`"`) ||
		!strings.Contains(streamText, `"id":"`+wantID+`"`) {
		t.Fatalf("stream tool carrier or stable ID missing: %s", stream)
	}
}

func TestConvertAntigravityResponseToClaudeTrailingFunctionCarrierRoundTrip(t *testing.T) {
	nativeSignature := geminiCarrierSignatureForTest(t, 0x34)
	request := []byte(`{"model":"gemini-3-pro-preview"}`)
	response := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"native-call-1","name":"read","args":{"path":"a"}}},{"text":"","thoughtSignature":"` + nativeSignature + `"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3-pro-preview","responseId":"response-1"}}`)

	claudeResponse := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3-pro-preview",
		request,
		request,
		response,
		nil,
	)
	content := gjson.GetBytes(claudeResponse, "content").Array()
	if len(content) != 2 || content[0].Get("type").String() != "tool_use" ||
		content[1].Get("type").String() != "thinking" {
		t.Fatalf("Claude carrier response malformed: %s", claudeResponse)
	}
	decoded, direction, targetKind, marked, valid := decodeGeminiClaudeCarrierSignature(
		content[1].Get("signature").String(),
	)
	if !marked || !valid || decoded != nativeSignature ||
		direction != geminiClaudeCarrierPrevious || targetKind != geminiClaudeCarrierFunction {
		t.Fatalf("trailing function carrier malformed: %q", content[1].Get("signature").String())
	}

	replay := []byte(`{"messages":[{"role":"assistant","content":[]}]}`)
	replay, _ = sjson.SetRawBytes(replay, "messages.0.content", []byte(gjson.GetBytes(claudeResponse, "content").Raw))
	replay = StripInvalidGeminiSignatureThinkingBlocks(replay)
	translated := convertClaudeRequestToAntigravity("gemini-3-pro-preview", replay, true)
	parts := gjson.GetBytes(translated, "request.contents.0.parts").Array()
	if len(parts) != 1 || !parts[0].Get("functionCall").Exists() {
		t.Fatalf("trailing carrier did not bind to function call: %s", translated)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != nativeSignature {
		t.Fatalf("function signature = %q, want %q; output=%s", got, nativeSignature, translated)
	}
}
