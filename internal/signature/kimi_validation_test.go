package signature

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func kimiThinkingSignatureForTest(t *testing.T, rawLen int) string {
	t.Helper()
	return kimiThinkingSignatureWithFirstCharForTest(t, rawLen, 'A')
}

func kimiThinkingSignatureWithFirstCharForTest(t *testing.T, rawLen int, firstChar byte) string {
	t.Helper()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	alphabetIndex := strings.IndexByte(alphabet, firstChar)
	if alphabetIndex < 0 {
		t.Fatalf("unsupported base64 first character %q", firstChar)
	}
	decoded := make([]byte, rawLen*3/4)
	for index := range decoded {
		decoded[index] = byte(index)
	}
	decoded[0] = byte(alphabetIndex << 2)
	signature := base64.RawStdEncoding.EncodeToString(decoded)
	if len(signature) != rawLen || signature[0] != firstChar {
		t.Fatalf(
			"generated signature length/first char = %d/%q, want %d/%q", len(signature), signature[0], rawLen, firstChar,
		)
	}
	return signature
}

func TestInspectKimiThinkingSignatureReportsMode(t *testing.T) {
	for _, test := range []struct {
		name   string
		rawLen int
		mode   kimiThinkingSignatureMode
	}{
		{"non-streaming", kimiThinkingSignatureNonStreamingLen, kimiThinkingSignatureModeNonStreaming},
		{"streaming", kimiThinkingSignatureStreamingLen, kimiThinkingSignatureModeStreaming},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				signature := kimiThinkingSignatureForTest(t, test.rawLen)
				info, err := inspectKimiThinkingSignature(signature)
				if err != nil {
					t.Fatalf("InspectKimiThinkingSignature() error = %v", err)
				}
				if info.RawLen != test.rawLen || info.Mode != test.mode {
					t.Fatalf("info = %+v, want raw length %d and mode %q", info, test.rawLen, test.mode)
				}
				if got := DetectProvider(signature); got != providerKimi {
					t.Fatalf("DetectProvider() = %q, want %q", got, providerKimi)
				}
			},
		)
	}
}

func TestDetectProviderKimiAfterEnvelopeCandidatePrefixes(t *testing.T) {
	for _, firstChar := range []byte{'C', 'E', 'R', 'g'} {
		t.Run(
			string(firstChar), func(t *testing.T) {
				signature := kimiThinkingSignatureWithFirstCharForTest(
					t,
					kimiThinkingSignatureStreamingLen,
					firstChar,
				)
				if got := DetectProvider(signature); got != providerKimi {
					t.Fatalf("DetectProvider(%q-prefixed signature) = %q, want %q", firstChar, got, providerKimi)
				}
			},
		)
	}
}

func TestInspectKimiThinkingSignatureRejectsMalformedInput(t *testing.T) {
	signature := kimiThinkingSignatureForTest(t, kimiThinkingSignatureStreamingLen)
	for _, test := range []struct {
		name      string
		signature string
	}{
		{"neighbouring length", signature[:len(signature)-1]},
		{"whitespace", " " + signature[1:]},
		{"padding", signature[:len(signature)-1] + "="},
		{"invalid character", "!" + signature[1:]},
		{"low entropy", strings.Repeat("A", len(signature))},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				if _, err := inspectKimiThinkingSignature(test.signature); err == nil {
					t.Fatal("InspectKimiThinkingSignature() error = nil")
				}
			},
		)
	}
}

func TestProviderFromModelNameRecognizesKimiAndGrok(t *testing.T) {
	for _, model := range []string{"kimi-k3", "moonshot-v1", "k2.5", "k3-256k"} {
		if got := ProviderFromModelName(model); got != providerKimi {
			t.Errorf("ProviderFromModelName(%q) = %q, want %q", model, got, providerKimi)
		}
	}
	for _, model := range []string{"grok-4.6", "grok-composer-2.5-fast", "grok-code-fast-1"} {
		if got := ProviderFromModelName(model); got != providerGrok {
			t.Errorf("ProviderFromModelName(%q) = %q, want %q", model, got, providerGrok)
		}
	}
}

func TestDecideCompatibilityHandlesKimiAndGrokTargets(t *testing.T) {
	kimiSignature := kimiThinkingSignatureForTest(t, kimiThinkingSignatureNonStreamingLen)
	if decision := decideCompatibility(providerKimi, kimiSignature, blockKindUnknown); !decision.Compatible ||
		decision.Action != actionPreserve || decision.NormalizedSignature != kimiSignature {
		t.Fatalf("native Kimi decision = %+v, want preserve", decision)
	}

	claudeSignature := validClaudeThinkingSignatureForTest()
	if decision := decideCompatibility(providerKimi, claudeSignature, BlockKindClaudeThinking); decision.Compatible ||
		decision.Action != actionDropSignature {
		t.Fatalf("foreign Kimi decision = %+v, want drop signature", decision)
	}
	if decision := decideCompatibility(providerGrok, claudeSignature, blockKindUnknown); decision.Compatible ||
		decision.Action != actionDropBlock {
		t.Fatalf("foreign Grok decision = %+v, want drop block", decision)
	}
}

func TestInspectGrokEncryptedContentRejectsKimiAndAllowsShortNativeShape(t *testing.T) {
	kimiSignature := kimiThinkingSignatureForTest(t, kimiThinkingSignatureStreamingLen)
	if _, err := InspectGrokEncryptedContent(kimiSignature); err == nil {
		t.Fatal("InspectGrokEncryptedContent() accepted Kimi signature")
	}

	decoded := make([]byte, 43)
	for index := range decoded {
		decoded[index] = byte(index)
	}
	grokContent := base64.RawStdEncoding.EncodeToString(decoded)
	if _, err := InspectGrokEncryptedContent(grokContent); err != nil {
		t.Fatalf("InspectGrokEncryptedContent() rejected 43-byte native shape: %v", err)
	}
	if got := DetectProvider(grokContent); got != providerUnknown {
		t.Fatalf("DetectProvider(grok content) = %q, want target-only %q", got, providerUnknown)
	}
}

func TestSanitizeClaudeMessagesAppliesKimiAndGrokPolicies(t *testing.T) {
	claudeSignature := validClaudeThinkingSignatureForTest()
	payload := []byte(`{"messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"private","signature":"` + claudeSignature + `"},` +
		`{"type":"text","text":"visible"}` +
		`]}]}`)

	kimiOutput, kimiReport := sanitizeClaudeMessagesSignaturesForModel(payload, "kimi-k3")
	if kimiReport.DroppedBlocks != 0 || kimiReport.DroppedSignatures != 1 {
		t.Fatalf("Kimi report = %+v, want one dropped signature", kimiReport)
	}
	if got := gjson.GetBytes(kimiOutput, "messages.0.content.0.type").String(); got != "thinking" {
		t.Fatalf("Kimi first content type = %q, want thinking; output=%s", got, kimiOutput)
	}
	if gjson.GetBytes(kimiOutput, "messages.0.content.0.signature").Exists() {
		t.Fatalf("Kimi thinking signature was not removed; output=%s", kimiOutput)
	}

	grokOutput, grokReport := sanitizeClaudeMessagesSignaturesForModel(payload, "grok-4.6")
	if grokReport.DroppedBlocks != 1 {
		t.Fatalf("Grok report = %+v, want one dropped block", grokReport)
	}
	if got := gjson.GetBytes(grokOutput, "messages.0.content.0.type").String(); got != "text" {
		t.Fatalf("Grok retained content type = %q, want text; output=%s", got, grokOutput)
	}
}
