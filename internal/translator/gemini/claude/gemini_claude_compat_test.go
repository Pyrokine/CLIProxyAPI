package claude

import (
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/tidwall/gjson"
)

const capturedGeminiThinkingSignature = "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"

func TestConvertClaudeRequestToGeminiWithCompatPreservesEmptyThinking(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":""}]}]}`)

	withoutCompat := convertClaudeRequestToGeminiDefault("deepseek-v4", payload, false)
	if gjson.GetBytes(withoutCompat, "contents.0.parts.#").Int() != 0 {
		t.Fatalf("default translation preserved thinking: %s", withoutCompat)
	}

	withCompat := ConvertClaudeRequestToGeminiWithCompat("deepseek-v4", payload, false)
	part := gjson.GetBytes(withCompat, "contents.0.parts.0")
	if !part.Get("thought").Bool() || part.Get("text").String() != "reason" {
		t.Fatalf("compat translation missing thought part: %s", withCompat)
	}
	if !part.Get("thoughtSignature").Exists() || part.Get("thoughtSignature").String() != "" {
		t.Fatalf("compat translation did not preserve empty signature: %s", withCompat)
	}
}

func TestConvertClaudeRequestToGeminiThinkingSignaturePolicies(t *testing.T) {
	tests := []struct {
		name          string
		compatibility bool
		signature     string
		wantPart      bool
		wantSignature string
	}{
		{
			name:          "strict preserves native Gemini",
			signature:     "gemini#" + capturedGeminiThinkingSignature,
			wantPart:      true,
			wantSignature: capturedGeminiThinkingSignature,
		},
		{
			name:      "strict rejects Claude signature",
			signature: "EgQKAggM",
		},
		{
			name:      "strict rejects unknown signature",
			signature: "opaque-client-signature",
		},
		{
			name:          "compat preserves unknown non-empty signature",
			compatibility: true,
			signature:     "opaque-client-signature",
			wantPart:      true,
			wantSignature: "opaque-client-signature",
		},
		{
			name:          "compat replaces Claude signature",
			compatibility: true,
			signature:     "EgQKAggM",
			wantPart:      true,
			wantSignature: signature.GeminiSkipThoughtSignatureValidator,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"` + testCase.signature + `"}]}]}`)
			var output []byte
			if testCase.compatibility {
				output = ConvertClaudeRequestToGeminiWithCompat("gemini-3-pro", payload, false)
			} else {
				output = convertClaudeRequestToGeminiDefault("gemini-3-pro", payload, false)
			}
			part := gjson.GetBytes(output, "contents.0.parts.0")
			if part.Exists() != testCase.wantPart {
				t.Fatalf("thought part exists = %v, want %v; output: %s", part.Exists(), testCase.wantPart, output)
			}
			if testCase.wantPart && part.Get("thoughtSignature").String() != testCase.wantSignature {
				t.Fatalf(
					"thoughtSignature = %q, want %q; output: %s",
					part.Get("thoughtSignature").String(),
					testCase.wantSignature,
					output,
				)
			}
		})
	}
}

func TestConvertClaudeRequestToGeminiClientSignaturePrecedesCache(t *testing.T) {
	const modelName = "gemini-3-pro"
	const thinkingText = "cached reasoning text"
	cache.ClearSignatureCache(modelName)
	t.Cleanup(func() { cache.ClearSignatureCache(modelName) })
	cache.StoreSignature(modelName, thinkingText, capturedGeminiThinkingSignature)

	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"` + thinkingText + `","signature":"opaque-client-signature"}]}]}`)
	output := ConvertClaudeRequestToGeminiWithCompat(modelName, payload, false)
	if got := gjson.GetBytes(output, "contents.0.parts.0.thoughtSignature").String(); got != "opaque-client-signature" {
		t.Fatalf("thoughtSignature = %q, want explicit client signature; output: %s", got, output)
	}
}

func TestConvertClaudeRequestToGeminiPreservesFunctionCallSignature(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":"run"},{"role":"assistant","content":[{"type":"tool_use","id":"lookup-1","name":"lookup","input":{},"signature":"` + capturedGeminiThinkingSignature + `"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"lookup-1","content":"done"}]}]}`)
	output := convertClaudeRequestToGeminiDefault("gemini-3-pro", payload, false)
	if got := gjson.GetBytes(output, "contents.1.parts.0.thoughtSignature").String(); got != capturedGeminiThinkingSignature {
		t.Fatalf("functionCall thoughtSignature = %q, want native signature; output: %s", got, output)
	}
}
