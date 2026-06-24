package signature

import (
	"encoding/base64"
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
	payload := protowire.AppendTag(nil, 1, protowire.BytesType)
	payload = protowire.AppendBytes(payload, []byte{0x01, 0x02, 0x03, 0x04})
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
				decision := DecideCompatibility(tc.provider, tc.sig, tc.kind)
				if !decision.Compatible || decision.Action != ActionPreserve || decision.NormalizedSignature == "" {
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

	if decision := DecideCompatibility(
		ProviderClaude, geminiSig, BlockKindClaudeThinking,
	); decision.Action != ActionDropBlock {
		t.Fatalf("Claude target with Gemini signature action = %s, want drop block", decision.Action)
	}
	if decision := DecideCompatibility(
		ProviderGPT, claudeSig, BlockKindGPTReasoning,
	); decision.Action != ActionDropBlock {
		t.Fatalf("GPT target with Claude signature action = %s, want drop block", decision.Action)
	}
	decision := DecideCompatibility(ProviderGemini, gptSig, BlockKindGeminiFunctionCall)
	if decision.Action != ActionReplaceWithGeminiBypass || decision.ReplacementSignature != GeminiSkipThoughtSignatureValidator {
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
	if got := gjson.GetBytes(
		out, "contents.0.parts.1.thoughtSignature",
	).String(); got != GeminiSkipThoughtSignatureValidator {
		t.Fatalf("invalid thoughtSignature = %q, want skip sentinel. Output: %s", got, string(out))
	}
	if gjson.GetBytes(out, "contents.1.parts.0.thoughtSignature").Exists() {
		t.Fatalf("functionResponse thoughtSignature should be removed. Output: %s", string(out))
	}
}
