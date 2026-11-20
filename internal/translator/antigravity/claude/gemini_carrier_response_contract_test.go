package claude

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"

	sigcompat "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestConvertAntigravityResponseToClaude_PreservesClaudeThoughtAndToolSignatures(t *testing.T) {

	_, upstreamSig1 := testAntigravityClaudeSignature(t)
	nativePayload2 := buildClaudeSignaturePayload(t, 13, new(uint64(2)), "claude-opus-4-6", true)
	nativeSig2 := base64.StdEncoding.EncodeToString(nativePayload2)
	upstreamSig2 := base64.StdEncoding.EncodeToString([]byte(nativeSig2))
	requestJSON := []byte(`{"model":"claude-sonnet-4-6"}`)
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hid` +
		`den","thought":true,` +
		`"thoughtSignature":"` + upstreamSig1 + `"},{"functionCall":{"id":"native-id","name":"run_command",` +
		`"args":{"command":"true"}},"thoughtSignature":"` + upstreamSig2 + `"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":1,` +
		`"candidatesTokenCount":1,"thoughtsTokenCount":1,` +
		`"totalTokenCount":3},` +
		`"modelVersion":"claude-sonnet-4-6-thinking",` +
		`"responseId":"resp-claude-thought-tool"}}`)

	nonStream := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"claude-sonnet-4-6",
		requestJSON,
		requestJSON,
		responseJSON,
		nil,
	)
	content := gjson.GetBytes(nonStream, "content").Array()
	if len(content) != 2 {
		t.Fatalf("content blocks = %d, want thinking + tool; output=%s", len(content), nonStream)
	}
	thinkingCarrierSig := content[0].Get("signature").String()
	toolCarrierSig := content[1].Get("signature").String()
	if thinkingCarrierSig == "" || toolCarrierSig == "" || thinkingCarrierSig == toolCarrierSig {
		t.Fatalf("Claude signatures were not kept on distinct native blocks: %s", nonStream)
	}

	replayRequest := []byte(`{"model":"claude-sonnet-4-6",` +
		`"messages":[{"role":"assistant","content":[]},` +
		`{"role":"user","content":[{"type":"text","text":"continue"}]}]}`)
	replayRequest, _ = sjson.SetRawBytes(
		replayRequest,
		"messages.0.content",
		[]byte(gjson.GetBytes(nonStream, "content").Raw),
	)
	replayRequest = StripEmptySignatureThinkingBlocks(replayRequest)
	translated := convertClaudeRequestToAntigravity("claude-sonnet-4-6", replayRequest, false)
	parts := gjson.GetBytes(translated, "request.contents.0.parts").Array()
	if len(parts) != 2 || parts[0].Get("thoughtSignature").String() != upstreamSig1 ||
		parts[1].Get("thoughtSignature").String() != upstreamSig2 {
		t.Fatalf("Claude thought/tool signatures did not round-trip: %s", translated)
	}

	var param any
	stream := bytes.Join(
		convertAntigravityResponseToClaude(
			context.Background(),
			"claude-sonnet-4-6",
			requestJSON,
			requestJSON,
			responseJSON,
			&param,
		),
		nil,
	)
	streamText := string(stream)
	if got := strings.Count(streamText, `"content_block":{"type":"thinking"`); got != 1 {
		t.Fatalf("stream thinking block count = %d, want 1; output=%s", got, stream)
	}
	if !strings.Contains(streamText, `"content_block":{"type":"tool_use"`) ||
		!strings.Contains(streamText, `"signature":"`+toolCarrierSig+`"`) {
		t.Fatalf("stream tool signature missing: %s", stream)
	}
}

func TestConvertAntigravityResponseToClaude_PreservesConsecutiveDetachedCarriers(t *testing.T) {
	sig1 := testGeminiEPrefixSignature(t)
	sig2 := geminiCarrierSignatureForTest(t, 0x35)
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high"}`)
	//goland:noinspection SpellCheckingInspection
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"vis` +
		`ible"},{"text":"",` +
		`"thoughtSignature":"` + sig1 + `"},{"text":"","thoughtSignature":"` + sig2 + `"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":1,` +
		`"candidatesTokenCount":1,"totalTokenCount":2},` +
		`"modelVersion":"gemini-3.6-flash",` +
		`"responseId":"resp-consecutive-carriers"}}`)

	nonStream := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3.6-flash-high",
		requestJSON,
		requestJSON,
		responseJSON,
		nil,
	)
	content := gjson.GetBytes(nonStream, "content").Array()
	wantCarrier1 := encodeGeminiClaudeCarrierSignature(sig1, geminiClaudeCarrierPrevious, geminiClaudeCarrierText)
	wantCarrier2 := encodeGeminiClaudeCarrierSignature(sig2, geminiClaudeCarrierPrevious, geminiClaudeCarrierText)
	if len(content) != 3 || content[1].Get("signature").String() != wantCarrier1 ||
		content[2].Get("signature").String() != wantCarrier2 {
		t.Fatalf("non-stream carriers were merged: %s", nonStream)
	}
	replayRequest := []byte(`{"model":"gemini-3.6-flash-high",` +
		`"messages":[{"role":"assistant","content":[]},` +
		`{"role":"user","content":[{"type":"text","text":"continue"}]}]}`)
	replayRequest, _ = sjson.SetRawBytes(
		replayRequest,
		"messages.0.content",
		[]byte(gjson.GetBytes(nonStream, "content").Raw),
	)
	replayRequest = StripInvalidGeminiSignatureThinkingBlocks(replayRequest)
	translated := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", replayRequest, false)
	parts := gjson.GetBytes(translated, "request.contents.0.parts").Array()
	if len(parts) != 2 || parts[0].Get("thoughtSignature").String() != sig1 ||
		parts[1].Get("thoughtSignature").String() != sig2 {
		t.Fatalf("consecutive carriers did not round-trip in order: %s", translated)
	}
	if parts[0].Get("text").String() != "visible" || !parts[1].Get("text").Exists() ||
		parts[1].Get("text").String() != "" {
		t.Fatalf("consecutive carrier targets malformed: %s", translated)
	}

	var param any
	stream := bytes.Join(
		convertAntigravityResponseToClaude(
			context.Background(),
			"gemini-3.6-flash-high",
			requestJSON,
			requestJSON,
			responseJSON,
			&param,
		),
		nil,
	)
	streamText := string(stream)
	if got := strings.Count(streamText, `"content_block":{"type":"thinking"`); got != 2 {
		t.Fatalf("stream thinking carrier count = %d, want 2; output=%s", got, stream)
	}
	if got := strings.Count(streamText, `"type":"signature_delta"`); got != 2 {
		t.Fatalf("stream signature count = %d, want 2; output=%s", got, stream)
	}
}

func TestConvertAntigravityResponseToClaude_TrailingFunctionCarrierRoundTrip(t *testing.T) {
	nativeSignature := testGeminiEPrefixSignature(t)
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high","tools":[{"name":"run_command",` +
		`"input_schema":{"type":"object",` +
		`"properties":{"command":{"type":"string"}}}}]}`)
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCa` +
		`ll":{"id":"native-ca` +
		`ll-1","name":"run_command","args":{"command":"true"}}},` +
		`{"text":"",` +
		`"thoughtSignature":"` + nativeSignature + `"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.6-flash",` +
		`"responseId":"tool-trailing-carrier"}}`)

	claudeResponse := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3.6-flash-high",
		requestJSON,
		requestJSON,
		responseJSON,
		nil,
	)
	content := gjson.GetBytes(claudeResponse, "content").Array()
	if len(content) != 2 || content[0].Get("type").String() != "tool_use" ||
		content[1].Get("type").String() != "thinking" {
		t.Fatalf("Claude response did not emit tool_use followed by carrier: %s", claudeResponse)
	}
	carrierSignature, direction, targetKind, marked, okCarrier := decodeGeminiClaudeCarrierSignature(
		content[1].Get("signature").String(),
	)
	if !marked || !okCarrier || carrierSignature != nativeSignature || direction != geminiClaudeCarrierPrevious ||
		targetKind != geminiClaudeCarrierFunction {
		t.Fatalf("trailing function carrier malformed: %q", content[1].Get("signature").String())
	}

	replayRequest := []byte(`{"model":"gemini-3.6-flash-high",` +
		`"messages":[{"role":"assistant","content":[]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"",` +
		`"content":"ok"}]}],"tools":[{"name":"run_command",` +
		`"input_schema":{"type":"object",` +
		`"properties":{"command":{"type":"string"}}}}]}`)
	replayRequest, _ = sjson.SetRawBytes(
		replayRequest,
		"messages.0.content",
		[]byte(gjson.GetBytes(claudeResponse, "content").Raw),
	)
	replayRequest, _ = sjson.SetBytes(replayRequest, "messages.1.content.0.tool_use_id", content[0].Get("id").String())
	translated := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", replayRequest, true)
	parts := gjson.GetBytes(translated, "request.contents.0.parts").Array()
	if len(parts) != 1 || !parts[0].Get("functionCall").Exists() {
		t.Fatalf("trailing carrier was not rebound to the function call: %s", translated)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != nativeSignature {
		t.Fatalf("function signature = %q, want native signature; translated=%s", got, translated)
	}
	if strings.Contains(string(translated), sigcompat.GeminiSkipThoughtSignatureValidator) {
		t.Fatalf("synthetic fallback remained after native carrier replay: %s", translated)
	}
}

func TestConvertAntigravityResponseToClaude_DirectionalTextCarriersRoundTrip(t *testing.T) {
	signature := testGeminiEPrefixSignature(t)
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high"}`)
	testCases := []struct {
		name                string
		parts               string
		wantFirstSignature  string
		wantSecondSignature string
		wantDirection       string
		carrierIndex        int
	}{
		{
			name:               "signed first part",
			parts:              `[{"text":"A","thoughtSignature":"` + signature + `"},{"text":"B"}]`,
			wantFirstSignature: signature,
			wantDirection:      geminiClaudeCarrierNext,
		},
		{
			name:               "trailing carrier before next part",
			parts:              `[{"text":"A"},{"text":"","thoughtSignature":"` + signature + `"},{"text":"B"}]`,
			wantFirstSignature: signature,
			wantDirection:      geminiClaudeCarrierPrevious,
			carrierIndex:       1,
		},
		{
			name:                "signed second part",
			parts:               `[{"text":"A"},{"text":"B","thoughtSignature":"` + signature + `"}]`,
			wantSecondSignature: signature,
			wantDirection:       geminiClaudeCarrierNext,
			carrierIndex:        1,
		},
	}
	for _, testCase := range testCases {
		t.Run(
			testCase.name, func(t *testing.T) {
				responseJSON := []byte(
					`{"response":{"candidates":[{"content":{"parts":` +
						testCase.parts +
						`},"finishReason":"STOP"}],"modelVersion":"gemini-3.6-flash",` +
						`"responseId":"directional-text"}}`,
				)
				nonStream := convertAntigravityResponseToClaudeNonStream(
					context.Background(),
					"gemini-3.6-flash-high",
					requestJSON,
					requestJSON,
					responseJSON,
					nil,
				)
				content := gjson.GetBytes(nonStream, "content").Array()
				if len(content) != 3 {
					t.Fatalf(
						"Claude content count = %d, want carrier + two text blocks; output=%s", len(content), nonStream,
					)
				}
				carrierSignature := content[testCase.carrierIndex].Get("signature").String()
				_, direction, targetKind, marked, okCarrier := decodeGeminiClaudeCarrierSignature(carrierSignature)
				if !marked || !okCarrier || direction != testCase.wantDirection ||
					targetKind != geminiClaudeCarrierText {
					t.Fatalf("directional carrier malformed: %q", carrierSignature)
				}

				replayRequest := []byte(`{"model":"gemini-3.6-flash-high",` +
					`"messages":[{"role":"assistant","content":[]},` +
					`{"role":"user","content":[{"type":"text","text":"continue"}]}]}`)
				replayRequest, _ = sjson.SetRawBytes(
					replayRequest,
					"messages.0.content",
					[]byte(gjson.GetBytes(nonStream, "content").Raw),
				)
				replayRequest = StripInvalidGeminiSignatureThinkingBlocks(replayRequest)
				translated := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", replayRequest, false)
				parts := gjson.GetBytes(translated, "request.contents.0.parts").Array()
				if len(parts) != 2 || parts[0].Get("text").String() != "A" || parts[1].Get("text").String() != "B" {
					t.Fatalf("text boundaries changed: %s", translated)
				}
				if got := parts[0].Get("thoughtSignature").String(); got != testCase.wantFirstSignature {
					t.Fatalf(
						"first signature = %q, want %q; translated=%s", got, testCase.wantFirstSignature, translated,
					)
				}
				if got := parts[1].Get("thoughtSignature").String(); got != testCase.wantSecondSignature {
					t.Fatalf(
						"second signature = %q, want %q; translated=%s", got, testCase.wantSecondSignature, translated,
					)
				}
				if strings.Contains(string(translated), geminiClaudeCarrierPrefix) {
					t.Fatalf("carrier envelope leaked to Gemini wire: %s", translated)
				}

				var param any
				stream := bytes.Join(
					convertAntigravityResponseToClaude(
						context.Background(),
						"gemini-3.6-flash-high",
						requestJSON,
						requestJSON,
						responseJSON,
						&param,
					),
					nil,
				)
				if got := strings.Count(string(stream), `"content_block":{"type":"text"`); got != 2 {
					t.Fatalf("stream text block count = %d, want 2; output=%s", got, stream)
				}
				if !strings.Contains(
					string(stream),
					geminiClaudeCarrierPrefix+testCase.wantDirection+":"+geminiClaudeCarrierText+":",
				) {
					t.Fatalf("stream carrier direction missing: %s", stream)
				}
			},
		)
	}
}

func TestConvertAntigravityResponseToClaude_LeadingCarrierTargetsFollowingThought(t *testing.T) {
	signature := testGeminiEPrefixSignature(t)
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high"}`)
	responseJSON := []byte(
		`{"response":{"candidates":[{"content":{"parts":[{"text":"",` +
			`"thoughtSignature":"` + signature + `"},{"text":"reason","thought":true}]},"finishReason":"STOP"}],` +
			`"modelVersion":"gemini-3.6-flash",` +
			`"responseId":"leading-thought"}}`,
	)
	nonStream := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3.6-flash-high",
		requestJSON,
		requestJSON,
		responseJSON,
		nil,
	)
	content := gjson.GetBytes(nonStream, "content").Array()
	if len(content) != 2 || content[0].Get("thinking").String() != "" ||
		content[1].Get("thinking").String() != "reason" {
		t.Fatalf("leading thought carrier response malformed: %s", nonStream)
	}
	replayRequest := []byte(`{"model":"gemini-3.6-flash-high",` +
		`"messages":[{"role":"assistant","content":[]}]}`)
	replayRequest, _ = sjson.SetRawBytes(
		replayRequest,
		"messages.0.content",
		[]byte(gjson.GetBytes(nonStream, "content").Raw),
	)
	replayRequest = StripInvalidGeminiSignatureThinkingBlocks(replayRequest)
	if got := gjson.GetBytes(replayRequest, "messages.0.content.#").Int(); got != 2 {
		t.Fatalf("prevalidation dropped unsigned target thought: %s", replayRequest)
	}
	translated := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", replayRequest, false)
	part := gjson.GetBytes(translated, "request.contents.0.parts.0")
	if part.Get("text").String() != "reason" || !part.Get("thought").Bool() ||
		part.Get("thoughtSignature").String() != signature {
		t.Fatalf("leading thought carrier did not round-trip: %s", translated)
	}
}

func TestConvertAntigravityResponseToClaudeUsesStableGeminiToolProvenanceID(t *testing.T) {
	requestJSON := []byte(`{"model":"gemini-3.6-flash-high"}`)
	responseJSON := []byte(`{"response":{"candidates":[{"content":{"parts":[{"thoughtSig` +
		`nature":"sig-native"` +
		`,"functionCall":{"id":"native-call-1","name":"Edit",` +
		`"args":{"file_path":"/tmp/a","old_string":"x",` +
		`"new_string":"y"}}}]},` +
		`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,` +
		`"candidatesTokenCount":1,"totalTokenCount":2},` +
		`"modelVersion":"gemini-3.6-flash",` +
		`"responseId":"resp-stable-tool-id"}}`)
	wantID := util.GeminiClaudeToolUseID(
		"native-call-1",
		"Edit",
		`{"file_path":"/tmp/a","old_string":"x","new_string":"y"}`,
	)
	if wantID == "" {
		t.Fatal("stable tool provenance ID is empty")
	}

	nonStream := convertAntigravityResponseToClaudeNonStream(
		context.Background(),
		"gemini-3.6-flash-high",
		requestJSON,
		requestJSON,
		responseJSON,
		nil,
	)
	if got := gjson.GetBytes(nonStream, "content.#(type==\"tool_use\").id").String(); got != wantID {
		t.Fatalf("non-stream tool_use.id = %q, want %q; output=%s", got, wantID, nonStream)
	}

	var param any
	stream := bytes.Join(
		convertAntigravityResponseToClaude(
			context.Background(),
			"gemini-3.6-flash-high",
			requestJSON,
			requestJSON,
			responseJSON,
			&param,
		),
		nil,
	)
	if !strings.Contains(string(stream), `"content_block":{"type":"tool_use","id":"`+wantID+`"`) {
		t.Fatalf("stream tool_use.id is not stable: %s", stream)
	}
}
