package responses

import (
	"encoding/base64"
	"testing"

	sigcompat "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestConvertOpenAIResponsesRequestToAntigravity_EmptyClaudeReasoningDoesNotShiftLaterSignature(t *testing.T) {
	rawSig1 := testAntigravityResponsesClaudeSignatureForModel(t, "claude-sonnet-4-6")
	rawSig2 := testAntigravityResponsesClaudeSignatureForModel(t, "claude-opus-4-6")
	expectedSig2, ok := sigcompat.CompatibleAntigravityClaudeThinkingSignature(rawSig2)
	if !ok {
		t.Fatal("second Claude signature should be compatible")
	}
	raw := []byte(`{
		"model":"claude-opus-4-6-thinking",
		"input":[
			{"type":"reasoning","encrypted_content":"` + rawSig1 + `","summary":[]},
			{"role":"user",` +
		`"content":[{"type":"input_text","text":"boundary"}]},` +
		`
			{"type":"reasoning","encrypted_content":"` + rawSig2 + `","summary":[{"type":"summary_text",` +
		`"text":"second reasoning"}]},
			{"role":"user",` +
		`"content":[{"type":"input_text","text":"continue"}]}
		]
	}`)
	out := convertOpenAIResponsesRequestToAntigravity("claude-opus-4-6-thinking", raw, false)
	var thoughts []gjson.Result
	for _, content := range gjson.GetBytes(out, "request.contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if part.Get("thought").Bool() {
				thoughts = append(thoughts, part)
			}
		}
	}
	if len(thoughts) != 1 {
		t.Fatalf("thought count = %d, want only the non-empty reasoning item. Output: %s", len(thoughts), out)
	}
	if got := thoughts[0].Get("text").String(); got != "second reasoning" {
		t.Fatalf("thought text = %q, want second reasoning. Output: %s", got, out)
	}
	if got := thoughts[0].Get("thoughtSignature").String(); got != expectedSig2 {
		t.Fatalf(
			"later thought received the wrong signature prefix/len = %q/%d, want %q/%d. Output: %s",
			firstByte(got),
			len(got),
			firstByte(expectedSig2),
			len(expectedSig2),
			out,
		)
	}
}

func TestConvertOpenAIResponsesRequestToAntigravity_EmptyClaudeReasoningBeforeFunctionDoesNotShiftLaterSignature(
	t *testing.T,
) {
	rawSig1 := testAntigravityResponsesClaudeSignatureForModel(t, "claude-sonnet-4-6")
	rawSig2 := testAntigravityResponsesClaudeSignatureForModel(t, "claude-opus-4-6")
	expectedSig2, ok := sigcompat.CompatibleAntigravityClaudeThinkingSignature(rawSig2)
	if !ok {
		t.Fatal("second Claude signature should be compatible")
	}
	raw := []byte(`{
		"model":"claude-opus-4-6-thinking",
		"input":[
			{"type":"reasoning","encrypted_content":"` + rawSig1 + `","summary":[]},
			{"type":"function_call",` +
		`"call_id":"call-1","name":"run","arguments":"{}"},` +
		`
			{"type":"function_call_output","call_id":"call-1",` +
		`"output":"ok"},
			{"type":"reasoning","encrypted_content":"` + rawSig2 + `","summary":[{"type":"summary_text",` +
		`"text":"second reasoning"}]},
			{"role":"user",` +
		`"content":[{"type":"input_text","text":"continue"}]}
		]
	}`)
	out := convertOpenAIResponsesRequestToAntigravity("claude-opus-4-6-thinking", raw, false)
	var thoughts []gjson.Result
	for _, content := range gjson.GetBytes(out, "request.contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if part.Get("thought").Bool() {
				thoughts = append(thoughts, part)
			}
		}
	}
	if len(thoughts) != 1 || thoughts[0].Get("text").String() != "second reasoning" {
		t.Fatalf("later reasoning placement malformed. Output: %s", out)
	}
	if got := thoughts[0].Get("thoughtSignature").String(); got != expectedSig2 {
		t.Fatalf(
			"later thought received the wrong signature prefix/len = %q/%d, want %q/%d. Output: %s",
			firstByte(got),
			len(got),
			firstByte(expectedSig2),
			len(expectedSig2),
			out,
		)
	}
}

func TestConvertOpenAIResponsesRequestToAntigravity_GeminiReasoningUsesNativeThoughtSignaturePlacement(t *testing.T) {
	sig := "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"
	raw := []byte(
		`{"model":"gemini-3.5-flash","input":[{"type":"reasoning",` +
			`"encrypted_content":"gemini#` + sig + `","summary":[{"type":"summary_text",` +
			`"text":"reasoning summary"}]}]}`,
	)
	out := convertOpenAIResponsesRequestToAntigravity("gemini-3-flash-agent", raw, false)
	parts := gjson.GetBytes(out, "request.contents.0.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("parts length = %d, want 1. Output: %s", len(parts), out)
	}
	if got := parts[0].Get("thought").Bool(); !got {
		t.Fatalf("parts[0] should be thought. Output: %s", out)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != sig {
		t.Fatalf("parts[0].thoughtSignature = %q, want preserved Gemini signature. Output: %s", got, out)
	}
}

func testAntigravityResponsesClaudeSignatureForModel(t *testing.T, model string) string {
	t.Helper()
	var channelBlock []byte
	channelBlock = protowire.AppendTag(channelBlock, 1, protowire.VarintType)
	channelBlock = protowire.AppendVarint(channelBlock, 12)
	channelBlock = protowire.AppendTag(channelBlock, 2, protowire.VarintType)
	channelBlock = protowire.AppendVarint(channelBlock, 2)
	channelBlock = protowire.AppendTag(channelBlock, 6, protowire.BytesType)
	channelBlock = protowire.AppendString(channelBlock, model)

	var container []byte
	container = protowire.AppendTag(container, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channelBlock)

	var payload []byte
	payload = protowire.AppendTag(payload, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	payload = protowire.AppendTag(payload, 3, protowire.VarintType)
	payload = protowire.AppendVarint(payload, 1)
	return base64.StdEncoding.EncodeToString(payload)
}

func firstByte(s string) string {
	if s == "" {
		return ""
	}
	return s[:1]
}
