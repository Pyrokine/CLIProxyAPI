package gemini

import (
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/tidwall/gjson"
)

func TestConvertGeminiRequestToAntigravity_DropsIncompatibleClientSignatureOnTextPart(t *testing.T) {
	validSignature := "abc123validSignature1234567890123456789012345678901234567890"
	inputJSON := []byte(fmt.Sprintf(
		`{
		"model": "gemini-3-pro-preview",
		"contents": [
			{
				"role": "model",
				"parts": [
					{"text": "previous answer", "thoughtSignature": "%s"}
				]
			}
		]
	}`, validSignature,
	))

	output := ConvertGeminiRequestToAntigravity("gemini-3-pro-preview", inputJSON, false)
	if thoughtSignature := gjson.GetBytes(
		output,
		"request.contents.0.parts.0.thoughtSignature",
	); thoughtSignature.Exists() {
		t.Fatalf("incompatible text signature should be dropped, got %s", thoughtSignature.Raw)
	}
}

func TestConvertGeminiRequestToAntigravity_LeavesUnsignedThoughtPartUnsigned(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-pro-preview",
		"contents": [
			{
				"role": "model",
				"parts": [
					{"thought": "internal reasoning"}
				]
			}
		]
	}`)

	output := ConvertGeminiRequestToAntigravity("gemini-3-pro-preview", inputJSON, false)
	if thoughtSignature := gjson.GetBytes(
		output,
		"request.contents.0.parts.0.thoughtSignature",
	); thoughtSignature.Exists() {
		t.Fatalf("unsigned thought should remain unsigned, got %s", thoughtSignature.Raw)
	}
}

func TestConvertGeminiRequestToAntigravity_ParallelFunctionCallsOnlyFirstGetsSentinel(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-pro-preview",
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "tool_one", "args": {"a": "1"}}},
					{"functionCall": {"name": "tool_two", "args": {"b": "2"}}}
				]
			}
		]
	}`)

	output := ConvertGeminiRequestToAntigravity("gemini-3-pro-preview", inputJSON, false)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("Expected 2 parts, got %d", len(parts))
	}
	if got := parts[0].Get("thoughtSignature").String(); got != signature.GeminiSkipThoughtSignatureValidator {
		t.Fatalf("first call signature = %q, want sentinel", got)
	}
	if parts[1].Get("thoughtSignature").Exists() {
		t.Fatalf("second parallel call should remain unsigned: %s", parts[1].Raw)
	}
}
