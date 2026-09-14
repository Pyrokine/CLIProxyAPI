package signature

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func validClaudeThinkingSignatureForTest() string {
	channel := protowire.AppendTag(nil, 1, protowire.VarintType)
	channel = protowire.AppendVarint(channel, 12)
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channel)
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func validGeminiThoughtSignatureForTest() string {
	return geminiField2ThoughtSignatureForTest([]byte{0x01, 0x02, 0x03, 0x04})
}

func geminiField2ThoughtSignatureForTest(value []byte) string {
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, value)
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func validGPTReasoningSignatureForTest() string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	return base64.RawURLEncoding.EncodeToString(payload)
}

func TestDecideCompatibilityPreservesNativeSignatures(t *testing.T) {
	cases := []struct {
		name     string
		provider Provider
		kind     BlockKind
		sig      string
	}{
		{"claude", ProviderClaude, BlockKindClaudeThinking, validClaudeThinkingSignatureForTest()},
		{"gemini", ProviderGemini, BlockKindGeminiModelPart, validGeminiThoughtSignatureForTest()},
		{"gpt", ProviderGPT, BlockKindGPTReasoning, validGPTReasoningSignatureForTest()},
	}

	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				decision := decideCompatibility(tc.provider, tc.sig, tc.kind)
				if !decision.Compatible || decision.Action != actionPreserve || decision.NormalizedSignature == "" {
					t.Fatalf("decision = %+v, want native preserve", decision)
				}
			},
		)
	}
}

func TestDecideCompatibilityCrossProviderActions(t *testing.T) {
	claudeSig := validClaudeThinkingSignatureForTest()
	geminiSig := validGeminiThoughtSignatureForTest()
	gptSig := validGPTReasoningSignatureForTest()

	if decision := decideCompatibility(
		ProviderClaude, geminiSig, BlockKindClaudeThinking,
	); decision.Action != actionDropBlock {
		t.Fatalf("Claude target with Gemini signature action = %s, want drop block", decision.Action)
	}
	if decision := decideCompatibility(
		ProviderGPT, claudeSig, BlockKindGPTReasoning,
	); decision.Action != actionDropBlock {
		t.Fatalf("GPT target with Claude signature action = %s, want drop block", decision.Action)
	}
	decision := decideCompatibility(ProviderGemini, gptSig, BlockKindGeminiFunctionCall)
	if decision.Action != actionReplaceWithGeminiBypass || decision.ReplacementSignature != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("Gemini target with GPT signature decision = %+v, want skip sentinel replacement", decision)
	}
}

func TestSanitizeClaudeMessagesForClaudeUpstreamDropsIncompatibleBlocksAndToolSignatures(t *testing.T) {
	payload := []byte(`{
		"messages":[{
			"role":"assistant",
			"content":[
				{"type":"thinking","thinking":"x","signature":"skip_thought_signature_validator"},
				{"type":"tool_use","id":"toolu_1","name":"Read","input":{},"signature":"skip_thought_signature_validator","extra_content":{"google":{"thought_signature":"skip_thought_signature_validator"}},"model":"gemini-3-pro"},
				{"type":"text","text":"visible"}
			]
		}]
	}`)

	out, report := SanitizeClaudeMessagesForClaudeUpstream(payload, "claude-opus-4-7")
	if report.DroppedBlocks != 1 || report.DroppedSignatures == 0 {
		t.Fatalf("report = %+v, want one dropped block and dropped tool signatures", report)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.type").String(); got != "tool_use" {
		t.Fatalf("first retained type = %q, want tool_use. Output: %s", got, string(out))
	}
	for _, path := range []string{
		"messages.0.content.0.signature",
		"messages.0.content.0.extra_content.google.thought_signature",
		"messages.0.content.0.extra_content",
		"messages.0.content.0.model",
	} {
		if gjson.GetBytes(out, path).Exists() {
			t.Fatalf("%s should be removed. Output: %s", path, string(out))
		}
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesPreservesNativeAndReplacesInvalid(t *testing.T) {
	validSig := validGeminiThoughtSignatureForTest()
	payload := []byte(`{
		"contents":[{
			"role":"model",
			"parts":[
				{"functionCall":{"name":"A","args":{}},"thoughtSignature":"` + validSig + `"},
				{"functionCall":{"name":"B","args":{}},"thoughtSignature":"not-native"}
			]
		},{
			"role":"user",
			"parts":[{"functionResponse":{"name":"A","response":{}},"thoughtSignature":"` + validSig + `"}]
		}]
	}`)

	out := SanitizeGeminiRequestThoughtSignatures(payload, "contents")
	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != validSig {
		t.Fatalf("valid thoughtSignature = %q, want %q. Output: %s", got, validSig, string(out))
	}
	if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("invalid sibling thoughtSignature should be removed. Output: %s", string(out))
	}
	if gjson.GetBytes(out, "contents.1.parts.0.thoughtSignature").Exists() {
		t.Fatalf("functionResponse thoughtSignature should be removed. Output: %s", string(out))
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesParallelSyntheticOnlyFirstGetsBypass(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"first","args":{}}},{"functionCall":{"name":"second","args":{}}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(
		out, "contents.0.parts.0.thoughtSignature",
	).String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("first call signature = %q, want bypass sentinel; output=%s", got, out)
	}
	if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("second parallel call should remain unsigned; output=%s", out)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesLeavesUnsignedThoughtUnsigned(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"text":"hidden","thought":true}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if signature := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature"); signature.Exists() {
		t.Fatalf("unsigned thought should remain unsigned; output=%s", out)
	}
	if &out[0] != &input[0] {
		t.Fatal("unsigned thought payload was copied")
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesNormalizesDuplicateCanonicalField(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `","thoughtSignature":"bad","thoughtSignature":"worse"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(
		out, "contents.0.parts.0.thoughtSignature",
	).String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("thoughtSignature = %q, want bypass sentinel. Output: %s", got, out)
	}
	if count := strings.Count(string(out), `"thoughtSignature"`); count != 1 {
		t.Fatalf("thoughtSignature field count = %d, want 1. Output: %s", count, out)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesReusesUnsignedFunctionResponsePayload(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"ok"}}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if &out[0] != &input[0] {
		t.Fatal("unsigned function response payload was copied")
	}
	if string(out) != string(input) {
		t.Fatalf("payload changed:\n got: %s\nwant: %s", out, input)
	}
}

func TestGeminiField2WrappedUUIDIsRecognized(t *testing.T) {
	wrapped := geminiField2ThoughtSignatureForTest([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
	if !isRecognizedGeminiSignature(wrapped) {
		t.Fatal("field-2 wrapped UUID should be recognized")
	}

	bare := base64.StdEncoding.EncodeToString([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
	if isRecognizedGeminiSignature(bare) {
		t.Fatal("bare base64 UUID should not be recognized")
	}
}

func TestGeminiField1EnvelopeIsRecognized(t *testing.T) {
	payload := protowire.AppendTag(nil, 1, protowire.BytesType)
	payload = protowire.AppendBytes(payload, []byte{0x01, 0x02, 0x03})
	payload = protowire.AppendTag(payload, 1, protowire.BytesType)
	payload = protowire.AppendBytes(payload, []byte{0x01, 0x04, 0x05})
	if !isRecognizedGeminiSignature(base64.StdEncoding.EncodeToString(payload)) {
		t.Fatal("repeated field-1 envelope should be recognized")
	}
}

func TestValidateGeminiThoughtSignaturesAllowsUnsignedParallelSibling(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"read_file","args":{}},"thoughtSignature":"skip_thought_signature_validator"},{"functionCall":{"id":"call-2","name":"read_file","args":{}}}]}]}`)
	if err := validateGeminiThoughtSignatures(input, true); err != nil {
		t.Fatalf("unsigned parallel sibling should be valid: %v", err)
	}
}

func TestValidateGeminiFunctionCallPairingValidParallelGroup(t *testing.T) {
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"weather","args":{}}},{"functionCall":{"id":"call-2","name":"weather","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"id":"call-1","name":"weather","response":{}}},{"functionResponse":{"id":"call-2","name":"weather","response":{}}}]}]}`)
	if err := ValidateGeminiFunctionCallPairing(input); err != nil {
		t.Fatalf("valid pairing failed: %v", err)
	}
}

// noinspection GoUnusedGlobalVariable
var benchmarkSanitizedGeminiRequest []byte

func BenchmarkSanitizeGeminiRequestThoughtSignaturesNormalizedHistory(b *testing.B) {
	payload := strings.Repeat("x", 8<<20)
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"value":"` + payload + `"}},"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"ok"}}}]}]}`)
	output := SanitizeGeminiRequestThoughtSignatures(input, "contents")
	if &output[0] != &input[0] {
		b.Fatal("normalized payload was copied")
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for b.Loop() {
		benchmarkSanitizedGeminiRequest = SanitizeGeminiRequestThoughtSignatures(input, "contents")
	}
}
