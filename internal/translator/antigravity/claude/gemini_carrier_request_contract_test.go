package claude

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestConvertClaudeRequestToAntigravity_ReattachesDetachedGeminiSignature(t *testing.T) {
	geminiSig := testGeminiEPrefixSignature(t)
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"assistant","content":[
			{"type":"text","text":"visible answer"},
			{"type":"thinking","thinking":"","signature":"` + geminiSig + `"}
		]}]
	}`)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want one native text part; output=%s", len(parts), output)
	}
	if got := parts[0].Get("text").String(); got != "visible answer" {
		t.Fatalf("text = %q; output=%s", got, output)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != geminiSig {
		t.Fatalf("signature = %q, want detached Gemini signature; output=%s", got, output)
	}
}

func TestConvertClaudeRequestToAntigravity_ReattachesLeadingDetachedGeminiSignature(t *testing.T) {
	geminiSig := testGeminiEPrefixSignature(t)
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"` + geminiSig + `"},
			{"type":"text","text":"visible answer"}
		]}]
	}`)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	if got := gjson.GetBytes(output, "request.contents.0.parts.0.thoughtSignature").String(); got != geminiSig {
		t.Fatalf("leading detached signature = %q, want %q; output=%s", got, geminiSig, output)
	}
}

func TestConvertClaudeRequestToAntigravity_DropsLegacyRawCarrierFromUserMessage(t *testing.T) {
	geminiSig := testGeminiEPrefixSignature(t)
	inputJSON := []byte(`{"model":"gemini-3.6-flash-high","messages":[{"role":"user",` +
		(`"content":[{"type":"thinking","thinking":"","signature":` +
			`"`) +
		geminiSig +
		(`"},{"type":"text","text":` +
			`"user text"}]}]}`))
	filtered := StripInvalidGeminiSignatureThinkingBlocks(inputJSON)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", filtered, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 1 || parts[0].Get("text").String() != "user text" ||
		parts[0].Get("thoughtSignature").Exists() {
		t.Fatalf("user legacy carrier reached Gemini after filtering: %s", output)
	}

	directOutput := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	directParts := gjson.GetBytes(directOutput, "request.contents.0.parts").Array()
	if len(directParts) != 1 || directParts[0].Get("text").String() != "user text" ||
		directParts[0].Get("thoughtSignature").Exists() {
		t.Fatalf("user legacy carrier reached Gemini without prefilter: %s", directOutput)
	}
}

func TestConvertClaudeRequestToAntigravity_DistributesConsecutiveTrailingGeminiCarriers(t *testing.T) {
	sig1 := testGeminiEPrefixSignature(t)
	sig2 := geminiCarrierSignatureForTest(t, 0x35)
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"assistant","content":[
			{"type":"text","text":"first"},
			{"type":"text","text":"second"},
			{"type":"thinking","thinking":"","signature":"` + sig1 + `"},
			{"type":"thinking","thinking":"","signature":"` + sig2 + `"}
		]}]
	}`)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want two text parts + detached carrier; output=%s", len(parts), output)
	}
	if got := parts[1].Get("thoughtSignature").String(); got != sig1 {
		t.Fatalf("latest semantic text signature = %q, want %q; output=%s", got, sig1, output)
	}
	if got := parts[2].Get("thoughtSignature").String(); got != sig2 || !parts[2].Get("text").Exists() ||
		parts[2].Get("text").String() != "" {
		t.Fatalf("second carrier malformed: %s; output=%s", parts[2].Raw, output)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != "" {
		t.Fatalf(
			"second carrier must not search past the nearest semantic part, got %q; output=%s",
			got,
			output,
		)
	}
}

func TestConvertClaudeRequestToAntigravity_PreservesConsecutiveLeadingGeminiCarriers(t *testing.T) {
	sig1 := testGeminiEPrefixSignature(t)
	sig2 := geminiCarrierSignatureForTest(t, 0x35)
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"` + sig1 + `"},
			{"type":"thinking","thinking":"","signature":"` + sig2 + `"},
			{"type":"tool_use","id":"tool-1",` +
		`"name":"run_command","input":{"command":"true"}}
		]}]
	}`)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want carrier + signed tool; output=%s", len(parts), output)
	}
	if parts[0].Get("thoughtSignature").String() != sig1 || !parts[0].Get("text").Exists() ||
		parts[0].Get("text").String() != "" {
		t.Fatalf("leading carrier malformed: %s; output=%s", parts[0].Raw, output)
	}
	if !parts[1].Get("functionCall").Exists() || parts[1].Get("thoughtSignature").String() != sig2 {
		t.Fatalf("signed tool malformed: %s; output=%s", parts[1].Raw, output)
	}
}

func TestConvertClaudeRequestToAntigravity_PreservesCarrierOnlyAssistantMessage(t *testing.T) {
	sig1 := testGeminiEPrefixSignature(t)
	sig2 := geminiCarrierSignatureForTest(t, 0x35)
	for _, tc := range []struct {
		name       string
		content    string
		signatures []string
	}{
		{
			name: "single", content: `[{"type":"thinking","thinking":"","signature":"` +
				sig1 +
				`"}]`, signatures: []string{sig1},
		},
		{
			name: "multiple", content: `[{"type":"thinking","thinking":"","signature":"` +
				sig1 +
				`"},{"type":"thinking","thinking":"","signature":"` +
				sig2 +
				`"}]`, signatures: []string{sig1, sig2},
		},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				inputJSON := []byte(
					`{"model":"gemini-3.6-flash-high",` +
						`"messages":[{"role":"assistant","content":` + tc.content + `}]}`,
				)
				output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
				parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
				if len(parts) != len(tc.signatures) {
					t.Fatalf("parts = %d, want %d carriers; output=%s", len(parts), len(tc.signatures), output)
				}
				for i, signature := range tc.signatures {
					if parts[i].Get("thoughtSignature").String() != signature ||
						!parts[i].Get("text").Exists() ||
						parts[i].Get("text").String() != "" {
						t.Fatalf("carrier %d malformed: %s; output=%s", i, parts[i].Raw, output)
					}
				}
			},
		)
	}
}

func TestConvertClaudeRequestToAntigravity_PreservesConsecutiveLeadingGeminiCarriersBeforeText(t *testing.T) {
	sig1 := testGeminiEPrefixSignature(t)
	sig2 := geminiCarrierSignatureForTest(t, 0x35)
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"` + sig1 + `"},
			{"type":"thinking","thinking":"","signature":"` + sig2 + `"},
			{"type":"text","text":"visible"}
		]}]
	}`)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want carrier + signed text; output=%s", len(parts), output)
	}
	if parts[0].Get("thoughtSignature").String() != sig1 || !parts[0].Get("text").Exists() ||
		parts[0].Get("text").String() != "" {
		t.Fatalf("leading carrier order malformed: %s; output=%s", parts[0].Raw, output)
	}
	if parts[1].Get("text").String() != "visible" || parts[1].Get("thoughtSignature").String() != sig2 {
		t.Fatalf("signed text malformed: %s; output=%s", parts[1].Raw, output)
	}
}

func TestConvertClaudeRequestToAntigravity_GeminiThinkingSignatureTargetsFollowingText(t *testing.T) {
	geminiSig := testGeminiEPrefixSignature(t)
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"hidden thought","signature":"` + geminiSig + `"},
			{"type":"text","text":"visible answer"}
		]}]
	}`)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	if got := gjson.GetBytes(output, "request.contents.0.parts.0.thoughtSignature").String(); got != "" {
		t.Fatalf("Gemini signature remained on thought part: %q; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "request.contents.0.parts.1.thoughtSignature").String(); got != geminiSig {
		t.Fatalf("visible signature = %q, want %q; output=%s", got, geminiSig, output)
	}
}

func TestConvertClaudeRequestToAntigravity_LeadingCarrierDoesNotCrossSignedThinking(t *testing.T) {
	signature1 := testGeminiEPrefixSignature(t)
	signature2 := geminiCarrierSignatureForTest(t, 0x35)
	leading := encodeGeminiClaudeCarrierSignature(signature1, geminiClaudeCarrierNext, geminiClaudeCarrierAny)
	signedThought := encodeGeminiClaudeCarrierSignature(
		signature2, geminiClaudeCarrierStandalone, geminiClaudeCarrierText,
	)
	inputJSON := []byte(`{"model":"gemini-3.6-flash-high","messages":[{"role":"assistant",` +
		`"content":[{"type":"thinking","thinking":"","signature":"` +
		leading +
		`"},{"type":"thinking","thinking":"reason","signature":"` +
		signedThought +
		`"},{"type":"text","text":"answer"}]}]}`)
	inputJSON = StripInvalidGeminiSignatureThinkingBlocks(inputJSON)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 3 || parts[0].Get("text").String() != "reason" || !parts[0].Get("thought").Bool() ||
		parts[0].Get("thoughtSignature").String() != signature2 ||
		!parts[1].Get("text").Exists() ||
		parts[1].Get("text").String() != "" ||
		parts[1].Get("thoughtSignature").String() != signature1 ||
		parts[2].Get("text").String() != "answer" ||
		parts[2].Get("thoughtSignature").String() != "" {
		t.Fatalf("leading carrier crossed signed thinking: %s", output)
	}
}

func TestConvertClaudeRequestToAntigravity_DropsMismatchedMarkedNonEmptyCarrier(t *testing.T) {
	geminiSig := testGeminiEPrefixSignature(t)
	for _, content := range []string{
		`[{"type":"thinking","thinking":"hidden","signature":"` +
			encodeGeminiClaudeCarrierSignature(geminiSig, geminiClaudeCarrierNext, geminiClaudeCarrierFunction) +
			`"},{"type":"text","text":"visible"}]`,
		`[{"type":"thinking","thinking":"hidden","signature":"` +
			encodeGeminiClaudeCarrierSignature(geminiSig, geminiClaudeCarrierStandalone, geminiClaudeCarrierFunction) +
			`"}]`,
	} {
		inputJSON := []byte(`{"model":"gemini-3.6-flash-high",` +
			`"messages":[{"role":"assistant","content":` + content + `}]}`)
		output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
		if strings.Contains(string(output), geminiSig) ||
			strings.Contains(string(output), geminiClaudeCarrierPrefix) {
			t.Fatalf("mismatched marked carrier reached Gemini wire: %s", output)
		}
	}
}

func buildClaudeSignaturePayload(
	t *testing.T,
	channelID uint64,
	field2 *uint64,
	modelText string,
	includeField7 bool,
) []byte {
	t.Helper()

	var channelBlock []byte
	channelBlock = protowire.AppendTag(channelBlock, 1, protowire.VarintType)
	channelBlock = protowire.AppendVarint(channelBlock, channelID)
	if field2 != nil {
		channelBlock = protowire.AppendTag(channelBlock, 2, protowire.VarintType)
		channelBlock = protowire.AppendVarint(channelBlock, *field2)
	}
	if modelText != "" {
		channelBlock = protowire.AppendTag(channelBlock, 6, protowire.BytesType)
		channelBlock = protowire.AppendString(channelBlock, modelText)
	}
	if includeField7 {
		channelBlock = protowire.AppendTag(channelBlock, 7, protowire.VarintType)
		channelBlock = protowire.AppendVarint(channelBlock, 0)
	}

	var container []byte
	container = protowire.AppendTag(container, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channelBlock)
	container = protowire.AppendTag(container, 2, protowire.BytesType)
	container = protowire.AppendBytes(container, bytes.Repeat([]byte{0x11}, 12))
	container = protowire.AppendTag(container, 3, protowire.BytesType)
	container = protowire.AppendBytes(container, bytes.Repeat([]byte{0x22}, 12))
	container = protowire.AppendTag(container, 4, protowire.BytesType)
	container = protowire.AppendBytes(container, bytes.Repeat([]byte{0x33}, 48))

	var payload []byte
	payload = protowire.AppendTag(payload, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	payload = protowire.AppendTag(payload, 3, protowire.VarintType)
	payload = protowire.AppendVarint(payload, 1)
	return payload
}

func testAntigravityClaudeSignature(t *testing.T) (string, string) {
	t.Helper()
	native := base64.StdEncoding.EncodeToString(
		buildClaudeSignaturePayload(t, 12, new(uint64(2)), "claude-sonnet-4-6", true),
	)
	return native, base64.StdEncoding.EncodeToString([]byte(native))
}
