package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToCodexWithCompatPreservesEmptyThinking(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":""}]}]}`)

	withoutCompat := ConvertClaudeRequestToCodex("deepseek-v4", payload, false)
	if gjson.GetBytes(withoutCompat, "input.#").Int() != 0 {
		t.Fatalf("default translation preserved empty-signature thinking: %s", withoutCompat)
	}

	withCompat := ConvertClaudeRequestToCodexWithCompat("deepseek-v4", payload, false)
	if gjson.GetBytes(withCompat, "input.0.type").String() != "reasoning" {
		t.Fatalf("compat translation missing reasoning item: %s", withCompat)
	}
	if !gjson.GetBytes(withCompat, "input.0.encrypted_content").Exists() {
		t.Fatalf("compat translation missing empty encrypted_content: %s", withCompat)
	}
}

func TestConvertClaudeRequestToCodexWithCompatPreservesOnlyUnknownOpaqueSignatures(t *testing.T) {
	tests := []struct {
		name          string
		signature     string
		wantCompat    bool
		wantEncrypted string
	}{
		{
			name:          "unknown opaque",
			signature:     "opaque-provider-signature",
			wantCompat:    true,
			wantEncrypted: "opaque-provider-signature",
		},
		{name: "known foreign", signature: "skip_thought_signature_validator"},
		{name: "invalid known prefix", signature: "gpt#opaque-provider-signature"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(
				`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"` +
					test.signature + `"}]}]}`,
			)
			strict := ConvertClaudeRequestToCodex("gpt-5.6", payload, false)
			if count := gjson.GetBytes(strict, "input.#").Int(); count != 0 {
				t.Fatalf("strict translation preserved signature %q: %s", test.signature, strict)
			}

			compat := ConvertClaudeRequestToCodexWithCompat("compat-model", payload, false)
			if !test.wantCompat {
				if count := gjson.GetBytes(compat, "input.#").Int(); count != 0 {
					t.Fatalf("compat translation preserved rejected signature %q: %s", test.signature, compat)
				}
				return
			}
			if got := gjson.GetBytes(compat, "input.0.encrypted_content").String(); got != test.wantEncrypted {
				t.Fatalf("encrypted_content = %q, want %q; output=%s", got, test.wantEncrypted, compat)
			}
		})
	}

	const escapedSignature = "opaque:\"token\"\\path-一"
	escapedPayload := []byte(
		`{"messages":[{"role":"assistant","content":[` +
			`{"type":"thinking","thinking":"reason","signature":"opaque:\"token\"\\path-\u4e00"}` +
			`]}]}`,
	)
	escapedOutput := ConvertClaudeRequestToCodexWithCompat("compat-model", escapedPayload, false)
	if got := gjson.GetBytes(escapedOutput, "input.0.encrypted_content").String(); got != escapedSignature {
		t.Fatalf("escaped encrypted_content = %q, want %q; output=%s", got, escapedSignature, escapedOutput)
	}

	nonString := []byte(
		`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":42}]}]}`,
	)
	output := ConvertClaudeRequestToCodexWithCompat("compat-model", nonString, false)
	if gjson.GetBytes(output, "input.#").Int() != 0 {
		t.Fatalf("compat translation preserved non-string signature: %s", output)
	}
}
