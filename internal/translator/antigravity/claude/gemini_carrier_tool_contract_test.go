package claude

import (
	"encoding/base64"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToAntigravity_DirectToolSignatureWinsOverLeadingCarrier(t *testing.T) {
	prefixSig := testGeminiEPrefixSignature(t)
	directSig := geminiCarrierSignatureForTest(t, 0x35)
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"` + prefixSig + `"},
			{"type":"tool_use","id":"tool-1","name":"run_command",` +
		`"input":{"command":"true"},"signature":"` + directSig + `"}
		]}]
	}`)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want carrier + directly signed tool; output=%s", len(parts), output)
	}
	if parts[0].Get("thoughtSignature").String() != prefixSig || !parts[0].Get("text").Exists() ||
		parts[0].Get("text").String() != "" {
		t.Fatalf("prefix carrier malformed: %s; output=%s", parts[0].Raw, output)
	}
	if !parts[1].Get("functionCall").Exists() || parts[1].Get("thoughtSignature").String() != directSig {
		t.Fatalf("direct tool signature was overwritten: %s; output=%s", parts[1].Raw, output)
	}
}

func TestConvertClaudeRequestToAntigravity_PreservesCarrierBetweenDirectlySignedParallelTools(t *testing.T) {
	sig1 := testGeminiEPrefixSignature(t)
	sig2 := geminiCarrierSignatureForTest(t, 0x35)
	rawSig3, errDecode := base64.StdEncoding.DecodeString(sig1)
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	rawSig3[len(rawSig3)-1] ^= 2
	sig3 := base64.StdEncoding.EncodeToString(rawSig3)
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"assistant",` +
		`"content":[
			{"type":"tool_use","id":"tool-1","name":"run_command",` +
		`"input":{"command":"one"},"signature":"` + sig1 + `"},
			{"type":"thinking","thinking":"","signature":"` + sig2 + `"},
			{"type":"tool_use","id":"tool-2","name":"run_command",` +
		`"input":{"command":"two"},"signature":"` + sig3 + `"}
		]}]
	}`)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want tool + carrier + tool; output=%s", len(parts), output)
	}
	if parts[0].Get("functionCall.id").String() != "tool-1" || parts[0].Get("thoughtSignature").String() != sig1 {
		t.Fatalf("first tool malformed: %s; output=%s", parts[0].Raw, output)
	}
	if parts[1].Get("thoughtSignature").String() != sig2 || !parts[1].Get("text").Exists() ||
		parts[1].Get("text").String() != "" {
		t.Fatalf("middle carrier malformed: %s; output=%s", parts[1].Raw, output)
	}
	if parts[2].Get("functionCall.id").String() != "tool-2" || parts[2].Get("thoughtSignature").String() != sig3 {
		t.Fatalf("second tool malformed: %s; output=%s", parts[2].Raw, output)
	}
}

func TestConvertClaudeRequestToAntigravity_PreservesTrailingCarrierAfterSignedTool(t *testing.T) {
	sig1 := testGeminiEPrefixSignature(t)
	sig2 := geminiCarrierSignatureForTest(t, 0x35)
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"` + sig1 + `"},
			{"type":"tool_use","id":"tool-1",` +
		`"name":"run_command","input":{"command":"true"}},` +
		`
			{"type":"thinking","thinking":"","signature":"` + sig2 + `"}
		]}]
	}`)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want signed tool + trailing carrier; output=%s", len(parts), output)
	}
	if !parts[0].Get("functionCall").Exists() || parts[0].Get("thoughtSignature").String() != sig1 {
		t.Fatalf("signed tool was reordered: %s; output=%s", parts[0].Raw, output)
	}
	if parts[1].Get("thoughtSignature").String() != sig2 || !parts[1].Get("text").Exists() ||
		parts[1].Get("text").String() != "" {
		t.Fatalf("trailing carrier malformed: %s; output=%s", parts[1].Raw, output)
	}
}

func TestConvertClaudeRequestToAntigravity_DetachedToolCarrierTargetsFollowingTool(t *testing.T) {
	geminiSig := testGeminiEPrefixSignature(t)
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"assistant","content":[
			{"type":"text","text":"preface"},
			{"type":"thinking","thinking":"","signature":"` + geminiSig + `"},
			{"type":"tool_use","id":"claude-id","name":"run_command",` +
		`"input":{"command":"true"}}
		]}]
	}`)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	if got := gjson.GetBytes(output, "request.contents.0.parts.0.thoughtSignature").String(); got != "" {
		t.Fatalf("detached tool signature attached backward to text: %q; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "request.contents.0.parts.1.thoughtSignature").String(); got != geminiSig {
		t.Fatalf("tool signature = %q, want %q; output=%s", got, geminiSig, output)
	}
}

func TestConvertClaudeRequestToAntigravity_GeminiThinkingSignatureTargetsFollowingTool(t *testing.T) {
	geminiSig := testGeminiEPrefixSignature(t)
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"hidden thought","signature":"` + geminiSig + `"},
			{"type":"tool_use","id":"claude-id","name":"run_command",` +
		`"input":{"command":"true"}}
		]}]
	}`)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 2 || !parts[0].Get("thought").Bool() {
		t.Fatalf("thought/tool parts malformed: %s", output)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != "" {
		t.Fatalf("signature remained on thought part: %q; output=%s", got, output)
	}
	if got := parts[1].Get("thoughtSignature").String(); got != geminiSig {
		t.Fatalf("tool signature = %q, want %q; output=%s", got, geminiSig, output)
	}
}

func TestConvertClaudeRequestToAntigravity_PreservesGeminiToolSignature(t *testing.T) {
	geminiSig := testGeminiEPrefixSignature(t)
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"` + geminiSig + `"},
			{"type":"tool_use","id":"claude-id","name":"run_command",` +
		`"input":{"command":"true"}
		]}]
	}`)
	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	if got := gjson.GetBytes(output, "request.contents.0.parts.0.thoughtSignature").String(); got != geminiSig {
		t.Fatalf("tool signature = %q, want %q; output=%s", got, geminiSig, output)
	}
}

func TestConvertClaudeRequestToAntigravity_NativeParallelToolLeavesUnsignedSibling(t *testing.T) {
	geminiSig := testGeminiEPrefixSignature(t)
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"` + geminiSig + `"},
			{"type":"tool_use","id":"call-1","name":"Read",` +
		`"input":{"file_path":"/tmp/a"}},
			{"type":"tool_use",` +
		`"id":"call-2","name":"Read",` +
		`"input":{"file_path":"/tmp/b"}}
		]}]
	}`)

	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 function calls; output=%s", len(parts), output)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != geminiSig {
		t.Fatalf("first call signature = %q, want native signature; output=%s", got, output)
	}
	if signature := parts[1].Get("thoughtSignature"); signature.Exists() {
		t.Fatalf("native unsigned sibling should remain unsigned; output=%s", output)
	}
}

func TestConvertClaudeRequestToAntigravity_SyntheticParallelToolOnlyFirstGetsSentinel(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gemini-3.6-flash-high",` +
		`
		"messages":[{"role":"assistant",` +
		`"content":[
			{"type":"tool_use","id":"call-1",` +
		`"name":"Read","input":{"file_path":"/tmp/a"}},` +
		`
			{"type":"tool_use","id":"call-2","name":"Read",` +
		`"input":{"file_path":"/tmp/b"}}
		]}]
	}`)

	output := convertClaudeRequestToAntigravity("gemini-3.6-flash-high", inputJSON, true)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 function calls; output=%s", len(parts), output)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != "skip_thought_signature_validator" {
		t.Fatalf("first synthetic call signature = %q, want sentinel; output=%s", got, output)
	}
	if signature := parts[1].Get("thoughtSignature"); signature.Exists() {
		t.Fatalf("second synthetic sibling should remain unsigned; output=%s", output)
	}
}

func TestConvertClaudeRequestToAntigravity_BypassMode_DropsWrappedRedactedThinking(t *testing.T) {
	cache.ClearSignatureCache("")
	t.Cleanup(
		func() {
			cache.ClearSignatureCache("")
		},
	)

	_, validSignature := testAntigravityClaudeSignature(t)

	inputJSON := []byte(`{
		"model": "claude-sonnet-4-6",
		"messages": [
			{
				"role": "user",` +
		`
				"content": [{"type": "text", "text": "Test user message"}]
			},` +
		`
			{
				"role": "assistant",
				"content": [
					{"type": "thinking",` +
		` "thinking": {"cache_control": {"type": "ephemeral"}},` +
		` "signature": "` + validSignature + `"},
					{"type": "text", "text": "Answer"}
				]
			},
			{
				"role": "user",
				"content": [{"type": "text", "text": "Follow up"}]
			}
		],
		"thinking": {"type": "enabled", "budget_tokens": 8000}
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-6", inputJSON, false)

	assistantParts := gjson.GetBytes(output, "request.contents.1.parts").Array()
	if len(assistantParts) != 1 {
		t.Fatalf(
			"Expected 1 part (wrapped redacted thinking dropped), got %d: %s",
			len(assistantParts), gjson.GetBytes(output, "request.contents.1.parts").Raw,
		)
	}
	if assistantParts[0].Get("text").String() != "Answer" {
		t.Fatalf("Expected text part preserved, got: %s", assistantParts[0].Raw)
	}
	if assistantParts[0].Get("thoughtSignature").Exists() {
		t.Fatalf("Wrapped redacted Claude signature must not move to text: %s", assistantParts[0].Raw)
	}
}

func TestConvertClaudeRequestToAntigravity_BypassMode_DropsWrappedRedactedThinkingBeforeTool(t *testing.T) {
	cache.ClearSignatureCache("")
	t.Cleanup(
		func() {
			cache.ClearSignatureCache("")
		},
	)

	_, validSignature := testAntigravityClaudeSignature(t)
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-6",
		"messages": [{
			"role": "assistant",
			"content": [
				{"type": "thinking", "thinking": "", "signature": "` + validSignature + `"},
				{"type": "tool_use", "id": "tool-1", "name": "run",` +
		` "input": {"command": "true"}}
			]
		}]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-6", inputJSON, false)
	toolPart := gjson.GetBytes(output, "request.contents.0.parts.0")
	if !toolPart.Get("functionCall").Exists() {
		t.Fatalf("Expected tool part preserved: %s", output)
	}
	if toolPart.Get("thoughtSignature").Exists() {
		t.Fatalf("Wrapped redacted Claude signature must not move to tool: %s", toolPart.Raw)
	}
}
