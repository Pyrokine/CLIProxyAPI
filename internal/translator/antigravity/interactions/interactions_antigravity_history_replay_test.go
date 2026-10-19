package interactions

import (
	"context"
	"fmt"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/tidwall/gjson"
)

func TestInteractionsAntigravityParallelHistory(t *testing.T) {
	steps := `[{"type":"user_input","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]},{"type":"thought","content":[{"type":"text","text":"plan"}]},{"type":"model_output","content":[{"type":"text","text":"calling"}]},{"type":"function_call","name":"lookup","call_id":"a","id":"ignored","arguments":{"key":1}},{"type":"function_call","name":"lookup","id":"b","arguments":{"key":2}},{"type":"function_result","name":"lookup","call_id":"a","result":{"value":1}},{"type":"function_result","name":"lookup","id":"b","result":{"value":2}},{"type":"model_output","content":[{"type":"text","text":"done"}]}]`
	for _, testCase := range []struct {
		name  string
		input string
	}{
		{"flat", steps},
		{"steps", `{"steps":` + steps + `}`},
		{"wrapped", `[{"role":"user","steps":[{"type":"user_input","content":[{"text":"first"},{"text":"second"}]}]},{"role":"assistant","steps":[{"type":"thought","content":[{"text":"plan"}]},{"type":"model_output","content":[{"text":"calling"}]},{"type":"function_call","name":"lookup","call_id":"a","id":"ignored","arguments":{"key":1}},{"type":"function_call","name":"lookup","id":"b","arguments":{"key":2}}]},{"role":"user","steps":[{"type":"function_result","name":"lookup","call_id":"a","result":{"value":1}},{"type":"function_result","name":"lookup","id":"b","result":{"value":2}}]},{"role":"model","content":[{"text":"done"}]}]`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			out, err := convertInteractionsRequestToAntigravityChecked("gemini-history-test", []byte(`{"input":`+testCase.input+`}`), false)
			if err != nil {
				t.Fatal(err)
			}
			if err := signature.ValidateGeminiFunctionCallPairing(out); err != nil {
				t.Fatalf("parallel history rejected: %v; body=%s", err, out)
			}
			contents := gjson.GetBytes(out, "request.contents").Array()
			if len(contents) != 4 {
				t.Fatalf("contents = %d, want 4; body=%s", len(contents), out)
			}
			for i, role := range []string{"user", "model", "user", "model"} {
				if contents[i].Get("role").String() != role {
					t.Fatalf("content %d role = %q, want %q", i, contents[i].Get("role").String(), role)
				}
			}
			if got := contents[0].Get("parts.#").Int(); got != 2 {
				t.Fatalf("user content array split: %s", out)
			}
			if contents[1].Get("parts.0.text").String() != "plan" || !contents[1].Get("parts.0.thought").Bool() || contents[1].Get("parts.1.text").String() != "calling" {
				t.Fatalf("thinking/text order changed: %s", out)
			}
			for i, id := range []string{"a", "b"} {
				call := contents[1].Get(fmt.Sprintf("parts.%d.functionCall", i+2))
				result := contents[2].Get(fmt.Sprintf("parts.%d.functionResponse", i))
				if call.Get("id").String() != id || result.Get("id").String() != id || call.Get("name").String() != "lookup" || result.Get("name").String() != "lookup" || call.Get("args.key").Int() != int64(i+1) || result.Get("response.value").Int() != int64(i+1) {
					t.Fatalf("tool identity or payload changed: %s", out)
				}
			}
		})
	}
}

func TestInteractionsAntigravityPreservesExplicitTurnBoundaries(t *testing.T) {
	for _, input := range []string{
		`[{"role":"user","content":[{"text":"one"},{"text":"two"}]},{"role":"user","content":[{"text":"three"}]},{"role":"assistant","content":[{"text":"four"}]}]`,
		`[{"role":"user","parts":[{"text":"one"},{"text":"two"}]},{"role":"user","parts":[{"text":"three"}]},{"role":"model","parts":[{"text":"four"}]}]`,
		`[{"role":"user","steps":[{"type":"user_input","content":[{"text":"one"},{"text":"two"}]}]},{"role":"user","steps":[{"type":"user_input","content":[{"text":"three"}]}]},{"role":"model","steps":[{"type":"model_output","content":[{"text":"four"}]}]}]`,
	} {
		out := convertInteractionsRequestToAntigravity("gemini-history-test", []byte(`{"input":`+input+`}`), false)
		if gjson.GetBytes(out, "request.contents.#").Int() != 3 || gjson.GetBytes(out, "request.contents.0.parts.#").Int() != 2 || gjson.GetBytes(out, "request.contents.2.role").String() != "model" {
			t.Fatalf("explicit turn/content array boundary lost: %s", out)
		}
	}
	out := convertInteractionsRequestToAntigravity("gemini-history-test", []byte(`{"input":[{"role":"model","steps":[{"type":"function_call","name":"lookup","call_id":"a"}]},{"role":"model","steps":[{"type":"function_call","name":"lookup","call_id":"b"}]},{"type":"function_result","name":"lookup","call_id":"a","result":1},{"type":"function_result","name":"lookup","call_id":"b","result":2}]}`), false)
	if err := signature.ValidateGeminiFunctionCallPairing(out); err == nil {
		t.Fatalf("separate model turns bypassed pairing validation: %s", out)
	}
	out = convertInteractionsRequestToAntigravity("gemini-history-test", []byte(`{"input":[{"type":"function_call","name":"lookup","call_id":"a"},{"type":"user_input","content":[]},{"type":"function_call","name":"lookup","call_id":"b"},{"type":"function_result","name":"lookup","call_id":"a","result":1},{"type":"function_result","name":"lookup","call_id":"b","result":2}]}`), false)
	if err := signature.ValidateGeminiFunctionCallPairing(out); err == nil {
		t.Fatalf("parallel calls merged across an empty user turn: %s", out)
	}
	out = convertInteractionsRequestToAntigravity("gemini-history-test", []byte(`{"input":[{"type":"function_call","name":"lookup","call_id":"a"},{"type":"function_result","name":"lookup","call_id":"wrong","result":1}]}`), false)
	if err := signature.ValidateGeminiFunctionCallPairing(out); err == nil {
		t.Fatalf("mismatched call identity accepted: %s", out)
	}
}

func TestInteractionsAntigravityNativeSignatureRoundTrip(t *testing.T) {
	valid := antigravityInteractionsSignatureForTest()
	for _, thoughtPart := range []string{
		`{"thought":true,"text":"plan","thoughtSignature":"` + valid + `"}`,
		`{"thought":true,"thoughtSignature":"` + valid + `"}`,
	} {
		raw := []byte(`{"response":{"candidates":[{"content":{"parts":[` + thoughtPart + `,{"text":"calling","thoughtSignature":"` + valid + `"},{"functionCall":{"name":"lookup","id":"a","args":{"key":1}},"thoughtSignature":"` + valid + `"}]}}]}}`)
		response := convertAntigravityResponseToInteractionsNonStream(context.Background(), "gemini-history-test", nil, nil, raw, nil)
		steps := gjson.GetBytes(response, "steps").Raw
		out := convertInteractionsRequestToAntigravity("gemini-history-test", []byte(`{"input":{"role":"model","steps":`+steps+`}}`), false)
		out = signature.SanitizeGeminiRequestThoughtSignatures(out, "request.contents")
		parts := gjson.GetBytes(out, "request.contents.0.parts").Array()
		if len(parts) != 3 {
			t.Fatalf("response→request lost parts: %s; response=%s", out, response)
		}
		for i, part := range parts {
			if got := part.Get("thoughtSignature").String(); got != valid {
				t.Fatalf("part %d signature = %q, want %q; body=%s", i, got, valid, out)
			}
		}
	}
}

func TestInteractionsAntigravityContentSignatures(t *testing.T) {
	valid := antigravityInteractionsSignatureForTest()
	for _, input := range []string{
		`{"type":"thought","content":[{"text":"one","signature":"` + valid + `"},{"text":"two","signature":"` + valid + `"}]}`,
		`{"role":"model","content":[{"type":"thought","text":"one","signature":"` + valid + `"},{"type":"text","text":"two","signature":"` + valid + `"}]}`,
	} {
		out := convertInteractionsRequestToAntigravity("gemini-history-test", []byte(`{"input":`+input+`}`), false)
		parts := gjson.GetBytes(out, "request.contents.0.parts").Array()
		if len(parts) != 2 || !parts[0].Get("thought").Bool() {
			t.Fatalf("signed content array shape lost: %s", out)
		}
		for _, part := range parts {
			if part.Get("thoughtSignature").String() != valid {
				t.Fatalf("content signature lost: %s", out)
			}
		}
	}
}

func TestInteractionsAntigravityRejectsForeignAndOpaqueSignatures(t *testing.T) {
	for _, raw := range []string{"sig_turn1", "gemini#invalid", antigravityClaudeSignatureForTest()} {
		out := convertInteractionsRequestToAntigravity("gemini-history-test", []byte(`{"input":[{"type":"thought","content":[{"text":"plan"}],"signature":"`+raw+`"},{"type":"function_call","name":"lookup","call_id":"a","signature":"`+raw+`"}]}`), false)
		out = signature.SanitizeGeminiRequestThoughtSignatures(out, "request.contents")
		for _, content := range gjson.GetBytes(out, "request.contents").Array() {
			for _, part := range content.Get("parts").Array() {
				if got := part.Get("thoughtSignature").String(); got != "" && got != signature.GeminiSkipThoughtSignatureValidator {
					t.Fatalf("foreign/opaque signature forwarded: %q; body=%s", got, out)
				}
			}
		}
	}
}
