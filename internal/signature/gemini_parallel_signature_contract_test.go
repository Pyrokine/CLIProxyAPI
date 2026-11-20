package signature

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeGeminiRequestThoughtSignaturesNativeParallelPreservesUnsignedSibling(t *testing.T) {
	nativeSignature := geminiField2ThoughtSignatureForTest([]byte{0x01, 0x0c, 0x39})
	input := []byte(`{"contents":[{"role":"model",` +
		`"parts":[{"functionCall":{"name":"first",` +
		`"args":{}},"thoughtSignature":"` + nativeSignature + `"},{"functionCall":{"name":"second","args":{}}}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != nativeSignature {
		t.Fatalf("first call signature = %q, want native signature; output=%s", got, out)
	}
	if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("native unsigned sibling should remain unsigned; output=%s", out)
	}
	if &out[0] != &input[0] {
		t.Fatal("already-native parallel history was copied")
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesRemovesPollutedSiblingBypass(t *testing.T) {
	nativeSignature := geminiField2ThoughtSignatureForTest([]byte{0x01, 0x0c, 0x39})
	input := []byte(`{"contents":[{"role":"model",` +
		`"parts":[{"functionCall":{"name":"first",` +
		`"args":{}},"thoughtSignature":"` + nativeSignature + `"},{"functionCall":{"name":"second","args":{}},` +
		`"thoughtSignature":"` + GeminiSkipThoughtSignatureValidator + `"}]}]}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

	if got := gjson.GetBytes(out, "contents.0.parts.0.thoughtSignature").String(); got != nativeSignature {
		t.Fatalf("first call signature = %q, want native signature; output=%s", got, out)
	}
	if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
		t.Fatalf("polluted sibling bypass should be removed; output=%s", out)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesRemovesPrefixedSiblingBypass(t *testing.T) {
	nativeSignature := geminiField2ThoughtSignatureForTest([]byte{0x01, 0x0c, 0x39})
	for _, prefix := range []string{"gemini", "google"} {
		t.Run(
			prefix, func(t *testing.T) {
				input := []byte(`{"contents":[{"role":"model",` +
					`"parts":[{"functionCall":{"name":"first",` +
					(`"args":{}},"thoughtSignature":` +
						`"`) +
					nativeSignature +
					(`"},{"functionCall":{"name":` +
						`"second","args":{}},`) +
					`"thoughtSignature":"` + prefix + `#` + GeminiSkipThoughtSignatureValidator + `"}]}]}`)

				out := SanitizeGeminiRequestThoughtSignatures(input, "contents")

				if signature := gjson.GetBytes(out, "contents.0.parts.1.thoughtSignature"); signature.Exists() {
					t.Fatalf("prefixed sibling bypass should be removed; output=%s", out)
				}
			},
		)
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesPreservesField2WrappedUUIDFunctionCall(t *testing.T) {
	sig := geminiField2ThoughtSignatureForTest([]byte("e24830a7-5cd6-42fe-998b-ee539e72b9c3"))
	input := []byte(`{"request":{"contents":[{"role":"model",` +
		`"parts":[{"functionCall":{"name":"f",` +
		`"args":{}},"thoughtSignature":"` + sig + `"}]}]}}`)

	out := SanitizeGeminiRequestThoughtSignatures(input, "request.contents")

	if got := gjson.GetBytes(out, "request.contents.0.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("thoughtSignature = %q, want wrapped UUID signature preserved. Output: %s", got, string(out))
	}
}
