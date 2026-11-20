package signature

import (
	"strings"
	"testing"
)

func TestGeminiWrappedUUIDEnvelopeAndFunctionCallCompatibility(t *testing.T) {
	const providerUUID = "e24830a7-5cd6-42fe-998b-ee539e72b9c3"
	sig := geminiField2ThoughtSignatureForTest([]byte(providerUUID))
	decoded, err := decodeGeminiThoughtSignature(sig)
	if err != nil {
		t.Fatal(err)
	}
	payload, validEnvelope := consumeGeminiField2Field1Value(decoded)
	if !validEnvelope || string(payload) != providerUUID || DetectProvider(sig) != ProviderGemini {
		t.Fatal("wrapped UUID envelope, opaque payload or detected provider changed")
	}
	for _, kind := range []BlockKind{BlockKindGeminiFunctionCall, BlockKindGeminiModelPart} {
		decision := decideCompatibility(ProviderGemini, sig, kind)
		if !decision.Compatible || decision.DetectedProvider != ProviderGemini ||
			decision.Action != actionPreserve || decision.NormalizedSignature != sig {
			t.Fatalf("wrapped UUID compatibility for %s: %+v", kind, decision)
		}
	}
	if normalized, compatible := CompatibleSignatureForProvider(
		ProviderGemini, sig, BlockKindGeminiFunctionCall,
	); !compatible ||
		normalized != sig {
		t.Fatalf("wrapped UUID function signature=%q compatible=%t", normalized, compatible)
	}
}

func TestValidateGeminiThoughtSignatures_FirstFunctionCallRequiresSignature(t *testing.T) {
	input := []byte(`{
		"contents": [{
			"role": "model",` +
		`
			"parts": [
				{"functionCall": {"id": "call-1",` +
		` "name": "read_file", "args": {}}}
			]
		}]
	}`)

	err := validateGeminiThoughtSignatures(input, false)
	if err == nil {
		t.Fatal("missing first functionCall thoughtSignature should fail")
	}
	if !strings.Contains(err.Error(), "missing thoughtSignature on first functionCall") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateGeminiThoughtSignatures_AllowsUnsignedParallelSibling(t *testing.T) {
	input := []byte(`{
		"contents": [{
			"role": "model",` +
		`
			"parts": [
				{
					"functionCall": {"id": "call-1",` +
		` "name": "read_file", "args": {}},` +
		`
					"thoughtSignature": "skip_thought_signature_validator"` +
		`
				},
				{"functionCall": {"id": "call-2",` +
		` "name": "read_file", "args": {}}}
			]
		}]
	}`)

	if err := validateGeminiThoughtSignatures(input, true); err != nil {
		t.Fatalf("unsigned parallel sibling should be valid: %v", err)
	}
}

func TestValidateGeminiThoughtSignatures_RejectsSentinelOutsideFirstFunctionCall(t *testing.T) {
	tests := []struct {
		name  string
		parts string
	}{
		{
			name: "parallel sibling",
			parts: `[
				{"functionCall":{"name":"first","args":{}},` +
				`"thoughtSignature":"skip_thought_signature_validator"},` +
				`
				{"functionCall":{"name":"second","args":{}},` +
				`"thoughtSignature":"skip_thought_signature_validator"}
			]`,
		},
		{
			name: "thought part",
			parts: `[{"text":"hidden","thought":true,` +
				`"thoughtSignature":"skip_thought_signature_validator"}]`,
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				input := []byte(`{"contents":[{"role":"model","parts":` + tt.parts + `}]}`)
				err := validateGeminiThoughtSignatures(input, true)
				if err == nil ||
					!strings.Contains(err.Error(), "allowed only on the first model functionCall") {
					t.Fatalf("unexpected error: %v", err)
				}
			},
		)
	}
}

func TestValidateGeminiThoughtSignatures_RejectsNonCanonicalNestedSignature(t *testing.T) {
	signature := geminiField2ThoughtSignatureForTest([]byte{0x01, 0x0c, 0x39})
	input := []byte(`{"contents":[{"role":"model",` +
		`"parts":[{"functionCall":{"name":"first","args":{},` +
		`"thoughtSignature":"` + signature + `"}}]}]}`)

	err := validateGeminiThoughtSignatures(input, false)
	if err == nil || !strings.Contains(err.Error(), "canonical top-level field") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateGeminiFunctionCallPairingPreservesUserBoundaries(t *testing.T) {
	for _, boundary := range []string{
		`{"role":"user","parts":[{"text":"boundary"}]}`,
		`{"role":"user","parts":[]}`,
		`{"role":"user"}`,
		`{"role":"user","parts":null}`,
	} {
		for _, role := range []string{"user", "model"} {
			currentBoundary := strings.Replace(boundary, `"role":"user"`, `"role":"`+role+`"`, 1)
			payload := `{"contents":[{"role":"model",` +
				`"parts":[{"functionCall":{"id":"call-1",` +
				`"name":"run","args":{}}}]},` +
				currentBoundary +
				(`,{"role":"model","parts":` +
					`[{"functionResponse":{"id":` +
					`"call-1"`) +
				`,"name":"run",` +
				`"response":{"result":"ok"}}}]}]}`
			for _, wrapped := range []bool{false, true} {
				request := payload
				if wrapped {
					request = `{"request":` + payload + `}`
				}
				err := ValidateGeminiFunctionCallPairing([]byte(request))
				if (err == nil) != (role == "user") {
					t.Fatalf("boundary=%s wrapped=%t error=%v", currentBoundary, wrapped, err)
				}
			}
		}
	}
}
