package signature

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestSanitizeGeminiRequestThoughtSignaturesPreservesGeminiSignature(t *testing.T) {
	sig := geminiField2ThoughtSignatureForTest([]byte{0x01, 0x0c, 0x39})
	input := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}},` +
		`"thoughtSignature":"` + sig + `"}]}]}`)
	output := SanitizeGeminiRequestThoughtSignatures(input, "contents")
	if got := gjson.GetBytes(output, "contents.0.parts.0.thoughtSignature").String(); got != sig {
		t.Fatalf("thoughtSignature = %q, want %q; output=%s", got, sig, output)
	}
	if &output[0] != &input[0] {
		t.Fatal("compatible canonical signature payload was copied")
	}
}

func TestSanitizeGeminiRequestThoughtSignaturesRemovesFunctionResponseSignature(t *testing.T) {
	input := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f",` +
		`"response":{"result":"ok"},"thoughtSignature":"bad","thoughtSignature":"worse"},` +
		`"thoughtSignature":"bad"}]}]}`)
	output := SanitizeGeminiRequestThoughtSignatures(input, "contents")
	if gjson.GetBytes(output, "contents.0.parts.0.thoughtSignature").Exists() {
		t.Fatalf("functionResponse top-level thoughtSignature was retained: %s", output)
	}
	if gjson.GetBytes(output, "contents.0.parts.0.functionResponse.thoughtSignature").Exists() {
		t.Fatalf("functionResponse nested thoughtSignature was retained: %s", output)
	}
}
