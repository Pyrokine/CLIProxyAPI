package responses

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	internalsignature "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
)

const testResponsesGeminiThoughtSignature = "EjQKMgEMOdbHO0Gd+c9Mxk4ELwPGbpCEcp2mFfYYLix2UVtBH3fL8GECc4+JITVnHF4qZDsA"

func TestReorderOpenAIResponsesDetachedReasoningDoesNotCrossUserMessage(t *testing.T) {
	items := gjson.Parse(
		`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]},
		{"id":"rs_test_detached_after_1","type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[]},
		{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{}"}
	]`,
	).Array()
	reordered := reorderOpenAIResponsesDetachedReasoning(items)
	if got := reordered[0].Get("role").String(); got != "user" {
		t.Fatalf("detached reasoning crossed user boundary: first role=%q", got)
	}
	if got := reordered[1].Get("type").String(); got != "reasoning" {
		t.Fatalf("item 1 = %q, want reasoning", got)
	}
}

func TestConvertOpenAIResponsesRequestToGeminiMatchesMissingOutputBeforeFutureExplicitID(t *testing.T) {
	inputJSON := []byte(`{
		"input":[
			{"type":"function_call","call_id":"call-a","name":"same","arguments":"{}"},
			{"type":"function_call","name":"same","arguments":"{}"},
			{"type":"function_call_output","name":"same","output":"generated-result"},
			{"type":"function_call_output","call_id":"call-a","output":"explicit-result"}
		]
	}`)

	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", inputJSON, false)
	var callIDs []string
	resultIDsByValue := make(map[string]string)
	for _, content := range gjson.GetBytes(result, "contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if call := part.Get("functionCall"); call.Exists() {
				callIDs = append(callIDs, call.Get("id").String())
			}
			if response := part.Get("functionResponse"); response.Exists() {
				resultIDsByValue[response.Get("response.result").String()] = response.Get("id").String()
			}
		}
	}
	const generated = "call_responses_0000000000000001"
	if len(callIDs) != 2 || callIDs[0] != "call-a" || callIDs[1] != generated {
		t.Fatalf("function call IDs = %#v; result: %s", callIDs, result)
	}
	if got := resultIDsByValue["generated-result"]; got != generated {
		t.Fatalf("named result ID = %q, want %q; result: %s", got, generated, result)
	}
	if got := resultIDsByValue["explicit-result"]; got != "call-a" {
		t.Fatalf("future explicit result ID = %q, want call-a; result: %s", got, result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_ReattachesReasoningAndSignatureToFunctionCall(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run"}]},
			{"type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[{"type":"summary_text","text":"hidden thought"}]},
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{\"command\":\"true\"}"},
			{"type":"function_call_output","call_id":"call-1","output":"ok"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	parts := gjson.GetBytes(result, "contents.1.parts").Array()
	if len(parts) != 2 || !parts[0].Get("thought").Bool() {
		t.Fatalf("reasoning/function parts malformed: %s", result)
	}
	if got := parts[1].Get("functionCall.name").String(); got != "run_command" {
		t.Fatalf("function name = %q; result=%s", got, result)
	}
	if got := parts[1].Get("thoughtSignature").String(); got != testResponsesGeminiThoughtSignature {
		t.Fatalf("function signature = %q, want %q; result=%s", got, testResponsesGeminiThoughtSignature, result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_SyntheticParallelCallsOnlyFirstGetsSentinel(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{\"command\":\"one\"}"},
			{"type":"function_call","call_id":"call-2","name":"run_command","arguments":"{\"command\":\"two\"}"}
		]
	}`

	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	parts := gjson.GetBytes(result, "contents.0.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2 parallel calls; result=%s", len(parts), result)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != internalsignature.GeminiSkipThoughtSignatureValidator {
		t.Fatalf("first synthetic call signature = %q, want sentinel; result=%s", got, result)
	}
	if signature := parts[1].Get("thoughtSignature"); signature.Exists() {
		t.Fatalf("second synthetic sibling should remain unsigned; result=%s", result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_NativeParallelCallsPreserveUnsignedSibling(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run twice"}]},
			{"type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[]},
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{\"command\":\"one\"}"},
			{"type":"function_call","call_id":"call-2","name":"run_command","arguments":"{\"command\":\"two\"}"}
		]
	}`

	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	var calls []gjson.Result
	for _, content := range gjson.GetBytes(result, "contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if part.Get("functionCall").Exists() {
				calls = append(calls, part)
			}
		}
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2; result=%s", len(calls), result)
	}
	if got := calls[0].Get("thoughtSignature").String(); got != testResponsesGeminiThoughtSignature {
		t.Fatalf("first call signature = %q, want native signature; result=%s", got, result)
	}
	if signature := calls[1].Get("thoughtSignature"); signature.Exists() {
		t.Fatalf("native unsigned sibling should remain unsigned; result=%s", result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_PreservesMultipleLeadingToolSignatures(t *testing.T) {
	secondRaw, errDecode := base64.StdEncoding.DecodeString(testResponsesGeminiThoughtSignature)
	if errDecode != nil {
		t.Fatal(errDecode)
	}
	secondRaw[len(secondRaw)-1] ^= 1
	secondSignature := base64.StdEncoding.EncodeToString(secondRaw)
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run twice"}]},
			{"id":"rs_before_1","type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[]},
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{\"command\":\"one\"}"},
			{"id":"rs_before_2","type":"reasoning","encrypted_content":"` + secondSignature + `","summary":[]},
			{"type":"function_call","call_id":"call-2","name":"run_command","arguments":"{\"command\":\"two\"}"},
			{"type":"function_call_output","call_id":"call-1","output":"one"},
			{"type":"function_call_output","call_id":"call-2","output":"two"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	var signatures, sequence []string
	for _, content := range gjson.GetBytes(result, "contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if part.Get("functionCall").Exists() {
				signatures = append(signatures, part.Get("thoughtSignature").String())
				sequence = append(sequence, "call:"+part.Get("functionCall.id").String())
			}
			if part.Get("functionResponse").Exists() {
				sequence = append(sequence, "output:"+part.Get("functionResponse.id").String())
			}
		}
	}
	if len(signatures) != 2 || signatures[0] != testResponsesGeminiThoughtSignature || signatures[1] != secondSignature {
		t.Fatalf("tool signatures = %v; result=%s", signatures, result)
	}
	if got := strings.Join(sequence, ","); got != "call:call-1,call:call-2,output:call-1,output:call-2" {
		t.Fatalf("parallel tool call/output sequence = %q; result=%s", got, result)
	}
	if errValidate := internalsignature.ValidateGeminiFunctionCallPairing(result); errValidate != nil {
		t.Fatalf("parallel tool history is invalid: %v; result=%s", errValidate, result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_GroupsReversedParallelToolOutputs(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{\"command\":\"one\"}"},
			{"type":"function_call","call_id":"call-2","name":"run_command","arguments":"{\"command\":\"two\"}"},
			{"type":"function_call_output","call_id":"call-2","output":"two"},
			{"type":"function_call_output","call_id":"call-1","output":"one"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	if errValidate := internalsignature.ValidateGeminiFunctionCallPairing(result); errValidate != nil {
		t.Fatalf("parallel tool history is invalid: %v; result=%s", errValidate, result)
	}
	contents := gjson.GetBytes(result, "contents").Array()
	if len(contents) != 2 || contents[0].Get("role").String() != "model" || contents[1].Get("role").String() != "user" {
		t.Fatalf("parallel tool roles malformed; result=%s", result)
	}
	responses := contents[1].Get("parts").Array()
	if len(responses) != 2 {
		t.Fatalf("function response count = %d, want 2; result=%s", len(responses), result)
	}
	if got := responses[0].Get("functionResponse.id").String(); got != "call-1" {
		t.Fatalf("first function response = %q, want call-1; result=%s", got, result)
	}
	if got := responses[0].Get("functionResponse.response.result").String(); got != "one" {
		t.Fatalf("first function result = %q, want one; result=%s", got, result)
	}
	if got := responses[1].Get("functionResponse.id").String(); got != "call-2" {
		t.Fatalf("second function response = %q, want call-2; result=%s", got, result)
	}
	if got := responses[1].Get("functionResponse.response.result").String(); got != "two" {
		t.Fatalf("second function result = %q, want two; result=%s", got, result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_GroupsNonContiguousParallelToolOutputs(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{\"command\":\"one\"}"},
			{"type":"function_call","call_id":"call-2","name":"run_command","arguments":"{\"command\":\"two\"}"},
			{"type":"function_call_output","call_id":"call-1","output":"one"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"between outputs"}]},
			{"type":"function_call_output","call_id":"call-2","output":"two"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	contents := gjson.GetBytes(result, "contents").Array()
	if len(contents) != 4 || contents[0].Get("role").String() != "model" || contents[1].Get("role").String() != "user" || contents[2].Get("role").String() != "user" || contents[3].Get("role").String() != "user" {
		t.Fatalf("non-contiguous tool output roles malformed; result=%s", result)
	}
	if got := contents[1].Get("parts.0.functionResponse.id").String(); got != "call-1" {
		t.Fatalf("first function response = %q, want call-1; result=%s", got, result)
	}
	if got := contents[2].Get("parts.0.text").String(); got != "between outputs" {
		t.Fatalf("intervening user message = %q; result=%s", got, result)
	}
	if got := contents[3].Get("parts.0.functionResponse.id").String(); got != "call-2" {
		t.Fatalf("second function response crossed user boundary: got %q; result=%s", got, result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_PreservesReasoningBeforePairedFunctionSignature(t *testing.T) {
	secondSignature := differentResponsesGeminiThoughtSignature(t)
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[{"type":"summary_text","text":"first"}]},
			{"type":"reasoning","encrypted_content":"` + secondSignature + `","summary":[{"type":"summary_text","text":"second"}]},
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{\"command\":\"true\"}"},
			{"type":"function_call_output","call_id":"call-1","output":"ok"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	var signatures []string
	for _, content := range gjson.GetBytes(result, "contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if signature := part.Get("thoughtSignature").String(); signature != "" {
				signatures = append(signatures, signature)
			}
		}
	}
	if len(signatures) != 2 || signatures[0] != testResponsesGeminiThoughtSignature || signatures[1] != secondSignature {
		t.Fatalf("reasoning/function signatures = %v; result=%s", signatures, result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_PreservesFunctionOutputOrderAcrossModelText(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{\"command\":\"one\"}"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"between"}]},
			{"type":"function_call","call_id":"call-2","name":"run_command","arguments":"{\"command\":\"two\"}"},
			{"type":"function_call_output","call_id":"call-1","output":"one"},
			{"type":"function_call_output","call_id":"call-2","output":"two"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	var sequence []string
	for _, content := range gjson.GetBytes(result, "contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if id := part.Get("functionCall.id").String(); id != "" {
				sequence = append(sequence, "call:"+id)
			}
			if id := part.Get("functionResponse.id").String(); id != "" {
				sequence = append(sequence, "output:"+id)
			}
		}
	}
	if got := strings.Join(sequence, ","); got != "call:call-1,call:call-2,output:call-1,output:call-2" {
		t.Fatalf("function output order = %q; result=%s", got, result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_ReattachesTrailingDetachedSignatureToText(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"turn one"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"visible answer"}]},
			{"id":"rs_text_detached_after_1","type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"turn two"}]}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	parts := gjson.GetBytes(result, "contents.1.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("model parts = %d, want one signed visible part; result=%s", len(parts), result)
	}
	if got := parts[0].Get("text").String(); got != "visible answer" {
		t.Fatalf("visible text = %q; result=%s", got, result)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != testResponsesGeminiThoughtSignature {
		t.Fatalf("signature = %q, want detached signature; result=%s", got, result)
	}
	if parts[0].Get("thought").Bool() {
		t.Fatalf("detached visible carrier must not emit an empty thought part; result=%s", result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_ReattachesUnmarkedTrailingSignatureToText(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.5-flash",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"turn one"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"visible answer"}]},
			{"id":"rs_client_rewritten","type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"turn two"}]}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", []byte(inputJSON), false)
	parts := gjson.GetBytes(result, "contents.1.parts").Array()
	if len(parts) != 1 {
		t.Fatalf(
			"model parts = %d, want one signed visible part after client rewrites carrier ID; result=%s", len(parts),
			result,
		)
	}
	if got := parts[0].Get("text").String(); got != "visible answer" {
		t.Fatalf("visible text = %q; result=%s", got, result)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != testResponsesGeminiThoughtSignature {
		t.Fatalf("signature = %q, want unmarked trailing signature; result=%s", got, result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_UnmarkedReasoningBeforeFunctionCallStillPairsCall(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.5-flash",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will run it."}]},
			{"type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[]},
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{\"command\":\"true\"}"},
			{"type":"function_call_output","call_id":"call-1","output":"ok"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", []byte(inputJSON), false)
	modelParts := gjson.GetBytes(result, "contents.1.parts").Array()
	if len(modelParts) != 2 {
		t.Fatalf("model parts = %d, want unsigned preamble plus signed call; result=%s", len(modelParts), result)
	}
	if signature := modelParts[0].Get("thoughtSignature"); signature.Exists() {
		t.Fatalf("function-call signature was retargeted to preamble; result=%s", result)
	}
	if got := modelParts[1].Get("thoughtSignature").String(); got != testResponsesGeminiThoughtSignature {
		t.Fatalf("function signature = %q, want unmarked reasoning signature; result=%s", got, result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_ReattachesDetachedSignatureToFunctionCall(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run"}]},
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{\"command\":\"true\"}"},
			{"id":"rs_function_detached_after_1","type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[]},
			{"type":"function_call_output","call_id":"call-1","output":"ok"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	functionParts := gjson.GetBytes(result, "contents.#(role==\"model\")#.parts").Array()
	found := false
	for _, partArray := range functionParts {
		for _, part := range partArray.Array() {
			if part.Get("functionCall.name").String() != "run_command" {
				continue
			}
			found = true
			if got := part.Get("thoughtSignature").String(); got != testResponsesGeminiThoughtSignature {
				t.Fatalf("function signature = %q, want detached signature; result=%s", got, result)
			}
		}
	}
	if !found {
		t.Fatalf("function call not found; result=%s", result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_ReattachesUnmarkedPostCallSignatureWithMatchingOutput(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{\"command\":\"true\"}"},
			{"type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[]},
			{"type":"function_call_output","call_id":"call-1","output":"ok"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	if got := gjson.GetBytes(
		result, "contents.0.parts.0.thoughtSignature",
	).String(); got != testResponsesGeminiThoughtSignature {
		t.Fatalf("unmarked post-call signature = %q, want native signature; result=%s", got, result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_ReattachesDirectionalFunctionCarriersWithoutIDs(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		direction string
		input     func(string) string
	}{
		{
			name:      "leading",
			direction: geminiResponsesCarrierNext,
			input: func(carrier string) string {
				return `[{"type":"reasoning","encrypted_content":"` + carrier + `","summary":[]},{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"ok"}]`
			},
		},
		{
			name:      "post-call",
			direction: geminiResponsesCarrierPrevious,
			input: func(carrier string) string {
				return `[{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{}"},{"type":"reasoning","encrypted_content":"` + carrier + `","summary":[]},{"type":"function_call_output","call_id":"call-1","output":"ok"}]`
			},
		},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				carrier := encodeGeminiResponsesCarrier(
					testResponsesGeminiThoughtSignature, testCase.direction, geminiResponsesCarrierFunction,
				)
				inputJSON := []byte(`{"model":"gemini-3.6-flash-high","input":` + testCase.input(carrier) + `}`)
				result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", inputJSON, false)
				if got := gjson.GetBytes(
					result, "contents.0.parts.0.thoughtSignature",
				).String(); got != testResponsesGeminiThoughtSignature {
					t.Fatalf("directional function signature = %q, want native signature; result=%s", got, result)
				}
				if strings.Contains(string(result), geminiResponsesCarrierPrefix) {
					t.Fatalf("directional function carrier leaked to Gemini wire: %s", result)
				}
			},
		)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_DoesNotRetargetExtraPreviousCarrier(t *testing.T) {
	signature2 := differentResponsesGeminiThoughtSignature(t)
	for _, testCase := range []struct {
		name       string
		targetKind string
		input      func(string, string) string
		assert     func(*testing.T, []gjson.Result)
	}{
		{
			name:       "text",
			targetKind: geminiResponsesCarrierText,
			input: func(first, extra string) string {
				return `[{"type":"reasoning","encrypted_content":"` + first + `","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"signed"}]},{"type":"reasoning","encrypted_content":"` + extra + `","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"unsigned"}]}]`
			},
			assert: func(t *testing.T, parts []gjson.Result) {
				if len(parts) != 3 || parts[0].Get("text").String() != "signed" || parts[0].Get("thoughtSignature").String() != testResponsesGeminiThoughtSignature || !parts[1].Get("text").Exists() || parts[1].Get("text").String() != "" || parts[1].Get("thoughtSignature").String() != signature2 || parts[2].Get("text").String() != "unsigned" || parts[2].Get("thoughtSignature").String() != "" {
					t.Fatalf("extra previous text carrier retargeted: %v", parts)
				}
			},
		},
		{
			name:       "function",
			targetKind: geminiResponsesCarrierFunction,
			input: func(first, extra string) string {
				return `[{"type":"reasoning","encrypted_content":"` + first + `","summary":[]},{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{}"},{"type":"reasoning","encrypted_content":"` + extra + `","summary":[]},{"type":"function_call","call_id":"call-2","name":"run_command","arguments":"{}"}]`
			},
			assert: func(t *testing.T, parts []gjson.Result) {
				if len(parts) != 3 || parts[0].Get("functionCall.id").String() != "call-1" || parts[0].Get("thoughtSignature").String() != testResponsesGeminiThoughtSignature || !parts[1].Get("text").Exists() || parts[1].Get("text").String() != "" || parts[1].Get("thoughtSignature").String() != signature2 || parts[2].Get("functionCall.id").String() != "call-2" || parts[2].Get("thoughtSignature").String() != "" {
					t.Fatalf("extra previous function carrier retargeted: %v", parts)
				}
			},
		},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				first := encodeGeminiResponsesCarrier(
					testResponsesGeminiThoughtSignature, geminiResponsesCarrierNext, testCase.targetKind,
				)
				extra := encodeGeminiResponsesCarrier(signature2, geminiResponsesCarrierPrevious, testCase.targetKind)
				request := []byte(`{"model":"gemini-3.6-flash-high","input":` + testCase.input(first, extra) + `}`)
				translated := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", request, false)
				testCase.assert(t, gjson.GetBytes(translated, "contents.0.parts").Array())
			},
		)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_DoesNotBindStandaloneFunctionCarrier(t *testing.T) {
	carrier := encodeGeminiResponsesCarrier(
		testResponsesGeminiThoughtSignature, geminiResponsesCarrierStandalone, geminiResponsesCarrierFunction,
	)
	inputJSON := []byte(`{"model":"gemini-3.6-flash-high","input":[{"type":"reasoning","encrypted_content":"` + carrier + `","summary":[]},{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{}"}]}`)
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", inputJSON, false)
	parts := gjson.GetBytes(result, "contents.0.parts").Array()
	if len(parts) != 2 || parts[0].Get("thoughtSignature").String() != testResponsesGeminiThoughtSignature || parts[1].Get("thoughtSignature").String() != geminiResponsesThoughtSignature {
		t.Fatalf("standalone carrier was bound to function call: %s", result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_ReattachesUnmarkedParallelPostCallSignature(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{\"command\":\"one\"}"},
			{"type":"function_call","call_id":"call-2","name":"run_command","arguments":"{\"command\":\"two\"}"},
			{"type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[]},
			{"type":"function_call_output","call_id":"call-1","output":"one"},
			{"type":"function_call_output","call_id":"call-2","output":"two"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	parts := gjson.GetBytes(result, "contents.0.parts").Array()
	if len(parts) != 2 || parts[0].Get("thoughtSignature").String() != geminiResponsesThoughtSignature || parts[1].Get("thoughtSignature").String() != testResponsesGeminiThoughtSignature {
		t.Fatalf("parallel post-call signature was not attached to call-2: %s", result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_ReattachesAlternatingParallelPostCallSignatures(t *testing.T) {
	signature2 := differentResponsesGeminiThoughtSignature(t)
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{\"command\":\"one\"}"},
			{"type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[]},
			{"type":"function_call","call_id":"call-2","name":"run_command","arguments":"{\"command\":\"two\"}"},
			{"type":"reasoning","encrypted_content":"` + signature2 + `","summary":[]},
			{"type":"function_call_output","call_id":"call-1","output":"one"},
			{"type":"function_call_output","call_id":"call-2","output":"two"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	parts := gjson.GetBytes(result, "contents.0.parts").Array()
	if len(parts) != 2 || parts[0].Get("functionCall.id").String() != "call-1" || parts[0].Get("thoughtSignature").String() != testResponsesGeminiThoughtSignature || parts[1].Get("functionCall.id").String() != "call-2" || parts[1].Get("thoughtSignature").String() != signature2 {
		t.Fatalf("alternating parallel post-call signatures shifted: %s", result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_PreservesExtraConsecutivePostCallCarrier(t *testing.T) {
	signature2 := differentResponsesGeminiThoughtSignature(t)
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{}"},
			{"type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[]},
			{"type":"reasoning","encrypted_content":"` + signature2 + `","summary":[]},
			{"type":"function_call_output","call_id":"call-1","output":"ok"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	parts := gjson.GetBytes(result, "contents.0.parts").Array()
	if len(parts) != 2 || parts[0].Get("functionCall.id").String() != "call-1" || parts[0].Get("thoughtSignature").String() != testResponsesGeminiThoughtSignature || parts[1].Get("thoughtSignature").String() != signature2 {
		t.Fatalf("consecutive post-call carriers malformed: %s", result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_DoesNotPairUnmarkedPostCallSignatureAcrossMismatch(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{}"},
			{"type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[]},
			{"type":"function_call_output","call_id":"other-call","output":"ok"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	if got := gjson.GetBytes(
		result, "contents.0.parts.0.thoughtSignature",
	).String(); got != geminiResponsesThoughtSignature {
		t.Fatalf("mismatched output paired signature %q; result=%s", got, result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_DoesNotPairUnmarkedPostCallSignatureAcrossUserMessage(t *testing.T) {
	inputJSON := `{
		"model":"gemini-3.6-flash-high",
		"input":[
			{"type":"function_call","call_id":"call-1","name":"run_command","arguments":"{}"},
			{"type":"reasoning","encrypted_content":"` + testResponsesGeminiThoughtSignature + `","summary":[]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"boundary"}]},
			{"type":"function_call_output","call_id":"call-1","output":"ok"}
		]
	}`
	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", []byte(inputJSON), false)
	if got := gjson.GetBytes(
		result, "contents.0.parts.0.thoughtSignature",
	).String(); got != geminiResponsesThoughtSignature {
		t.Fatalf("user-boundary carrier paired signature %q; result=%s", got, result)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_StripsTrailingAssistantPrefill(t *testing.T) {
	inputJSON := `{
		"model": "gpt-5.4",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "previous answer"}]
			}
		]
	}`

	result := ConvertOpenAIResponsesRequestToGemini("gemini-3.1-pro-high", []byte(inputJSON), false)
	resultJSON := gjson.ParseBytes(result)
	contents := resultJSON.Get("contents").Array()

	if len(contents) != 1 {
		t.Fatalf("contents length = %d, want 1. contents=%s", len(contents), resultJSON.Get("contents").Raw)
	}
	if got := contents[0].Get("role").String(); got != "user" {
		t.Fatalf("final remaining role = %q, want %q", got, "user")
	}
}

func TestConvertOpenAIResponsesRequestToGemini_TextFormatJSONSchema(t *testing.T) {
	inputJSON := `{
		"model": "gemini-flash-lite",
		"temperature": 0.2,
		"input": [
			{
				"role": "user",
				"content": [
					{
						"type": "input_text",
						"text": "Return structured JSON."
					}
				]
			}
		],
		"text": {
			"format": {
				"type": "json_schema",
				"strict": true,
				"name": "response",
				"schema": {
					"type": "object",
					"properties": {
						"cleanedContent": {
							"type": "string"
						}
					},
					"required": [
						"cleanedContent"
					],
					"additionalProperties": false
				}
			}
		}
	}`

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.1-flash-lite", []byte(inputJSON), false)
	result := gjson.ParseBytes(output)
	genConfig := result.Get("generationConfig")

	if got := genConfig.Get("responseMimeType").String(); got != "application/json" {
		t.Fatalf("responseMimeType = %q, want application/json. Output: %s", got, output)
	}
	schema := genConfig.Get("responseJsonSchema")
	if !schema.Exists() {
		t.Fatalf("responseJsonSchema missing. Output: %s", output)
	}
	if genConfig.Get("responseSchema").Exists() {
		t.Fatalf("responseSchema should not be set with responseJsonSchema. Output: %s", output)
	}
	if got := schema.Get("type").String(); got != "object" {
		t.Fatalf("schema type = %q, want object. Output: %s", got, output)
	}
	if got := schema.Get("properties.cleanedContent.type").String(); got != "string" {
		t.Fatalf("cleanedContent type = %q, want string. Output: %s", got, output)
	}
	if additionalProperties := schema.Get("additionalProperties"); !additionalProperties.Exists() || additionalProperties.Bool() {
		t.Fatalf("additionalProperties = %s, want false. Output: %s", additionalProperties.Raw, output)
	}
	if got := genConfig.Get("temperature").Float(); got != 0.2 {
		t.Fatalf("temperature = %v, want 0.2. Output: %s", got, output)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_TextFormatJSONObject(t *testing.T) {
	inputJSON := `{
		"model": "gemini-flash-lite",
		"input": "Return a JSON object.",
		"text": {
			"format": {
				"type": "json_object"
			}
		}
	}`

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.1-flash-lite", []byte(inputJSON), false)
	result := gjson.ParseBytes(output)
	genConfig := result.Get("generationConfig")

	if got := genConfig.Get("responseMimeType").String(); got != "application/json" {
		t.Fatalf("responseMimeType = %q, want application/json. Output: %s", got, output)
	}
	if genConfig.Get("responseJsonSchema").Exists() {
		t.Fatalf("responseJsonSchema should not be set for json_object. Output: %s", output)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_PreservesReasoningOnlyHistory(t *testing.T) {
	input := []byte(`{
		"model": "gpt-5",
		"input": [{
			"type": "reasoning",
			"encrypted_content": "gemini#` + testResponsesGeminiThoughtSignature + `",
			"summary": [{"type": "summary_text", "text": "reasoning summary"}]
		}]
	}`)

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", input, false)
	parts := gjson.GetBytes(output, "contents.0.parts").Array()
	if got := gjson.GetBytes(output, "contents").Array(); len(got) != 1 {
		t.Fatalf("contents length = %d, want 1. Output: %s", len(got), output)
	}
	if len(parts) != 1 {
		t.Fatalf("parts length = %d, want 1. Output: %s", len(parts), output)
	}
	if got := parts[0].Get("thought").Bool(); !got {
		t.Fatalf("parts[0] should be thought. Output: %s", output)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != testResponsesGeminiThoughtSignature {
		t.Fatalf(
			"parts[0].thoughtSignature = %q, want %q. Output: %s", got, testResponsesGeminiThoughtSignature, output,
		)
	}
	if got := parts[0].Get("text").String(); got != "reasoning summary" {
		t.Fatalf("thought text = %q, want reasoning summary. Output: %s", got, output)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_DropsEmptyUnsignedReasoningCarrier(t *testing.T) {
	input := []byte(`{
		"model":"gemini-3.6-flash-high",
		"input":[{"type":"reasoning","encrypted_content":"","summary":[]}]
	}`)

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", input, false)
	if got := gjson.GetBytes(output, "contents.#").Int(); got != 0 {
		t.Fatalf("contents = %d, want no empty unsigned model content; output=%s", got, output)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_PreservesUnboundDetachedCarrierWithoutEmptyThought(t *testing.T) {
	input := []byte(`{
		"model": "gemini-3.6-flash-high",
		"input": [{
			"id": "rs_unbound_detached_after_1",
			"type": "reasoning",
			"encrypted_content": "` + testResponsesGeminiThoughtSignature + `",
			"summary": []
		}]
	}`)

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash-high", input, false)
	parts := gjson.GetBytes(output, "contents.0.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("unbound carrier parts = %d, want one signed carrier; output=%s", len(parts), output)
	}
	if parts[0].Get("thought").Bool() || !parts[0].Get("text").Exists() || parts[0].Get("text").String() != "" {
		t.Fatalf("unbound carrier emitted an empty thought part: %s", output)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != testResponsesGeminiThoughtSignature {
		t.Fatalf("unbound carrier signature = %q, want %q; output=%s", got, testResponsesGeminiThoughtSignature, output)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_PreservesReasoningBeforeTrailingAssistantPrefill(t *testing.T) {
	inputJSON := `{
		"model": "gpt-5.4",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "hello"}]
			},
			{
				"type": "reasoning",
				"encrypted_content": "gemini#` + testResponsesGeminiThoughtSignature + `",
				"summary": [{"type": "summary_text", "text": "reasoning summary"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "previous answer"}]
			}
		]
	}`

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", []byte(inputJSON), false)
	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 2 {
		t.Fatalf("contents length = %d, want 2. Output: %s", len(contents), output)
	}
	if got := contents[0].Get("role").String(); got != "user" {
		t.Fatalf("contents[0].role = %q, want user", got)
	}
	if got := contents[1].Get("parts.1.thoughtSignature").String(); got != testResponsesGeminiThoughtSignature {
		t.Fatalf("reasoning visible thoughtSignature = %q, want preserved signature", got)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_ReasoningSignatureCompatibility(t *testing.T) {
	tests := []struct {
		name          string
		encrypted     string
		wantSignature string
	}{
		{
			name:          "GPT encrypted_content is dropped from Gemini thought",
			encrypted:     validResponsesGPTReasoningSignature(),
			wantSignature: "",
		},
		{
			name:          "Gemini encrypted_content is preserved",
			encrypted:     "gemini#" + testResponsesGeminiThoughtSignature,
			wantSignature: testResponsesGeminiThoughtSignature,
		},
		{
			name:          "Missing encrypted_content leaves Gemini thought unsigned",
			encrypted:     "",
			wantSignature: "",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				input := []byte(`{
				"model": "gpt-5",
				"input": [{
					"type": "reasoning",
					"encrypted_content": "` + tt.encrypted + `",
					"summary": [{"type": "summary_text", "text": "reasoning summary"}]
				}]
			}`)

				output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", input, false)
				parts := gjson.GetBytes(output, "contents.0.parts").Array()
				if len(parts) != 1 {
					t.Fatalf("parts length = %d, want 1. Output: %s", len(parts), output)
				}
				if got := parts[0].Get("thoughtSignature").String(); got != tt.wantSignature {
					t.Fatalf("thoughtSignature = %q, want %q. Output: %s", got, tt.wantSignature, output)
				}
				if got := parts[0].Get("text").String(); got != "reasoning summary" {
					t.Fatalf("thought text = %q, want reasoning summary. Output: %s", got, output)
				}
			},
		)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_MergesReasoningWithAssistantVisibleAnswer(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3.5-flash",
		"input": [
			{
				"type": "reasoning",
				"encrypted_content": "gemini#` + testResponsesGeminiThoughtSignature + `",
				"summary": [{"type": "summary_text", "text": "internal reasoning"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "visible answer"}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "continue"}]
			}
		]
	}`

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", []byte(inputJSON), false)
	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 2 {
		t.Fatalf("contents length = %d, want 2. Output: %s", len(contents), output)
	}
	parts := contents[0].Get("parts").Array()
	if len(parts) != 2 {
		t.Fatalf("model parts length = %d, want 2. Output: %s", len(parts), output)
	}
	if got := parts[0].Get("thought").Bool(); !got {
		t.Fatalf("parts[0] should be thought. Output: %s", output)
	}
	if got := parts[0].Get("thoughtSignature").String(); got != "" {
		t.Fatalf("parts[0].thoughtSignature = %q, want empty. Output: %s", got, output)
	}
	if got := parts[1].Get("text").String(); got != "visible answer" {
		t.Fatalf("visible text = %q, want visible answer. Output: %s", got, output)
	}
	if got := parts[1].Get("thoughtSignature").String(); got != testResponsesGeminiThoughtSignature {
		t.Fatalf("visible thoughtSignature = %q, want preserved signature", got)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_MergesReasoningWithUserRoleOutputText(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3.5-flash",
		"input": [
			{
				"type": "reasoning",
				"encrypted_content": "gemini#` + testResponsesGeminiThoughtSignature + `",
				"summary": [{"type": "summary_text", "text": "reasoning summary"}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "output_text", "text": "visible from user role"}]
			}
		]
	}`
	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", []byte(inputJSON), false)
	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 1 {
		t.Fatalf("contents length = %d, want 1. Output: %s", len(contents), output)
	}
	if got := contents[0].Get("parts.1.text").String(); got != "visible from user role" {
		t.Fatalf("visible text = %q", got)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_MergesReasoningWithAssistantStringContent(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3.5-flash",
		"input": [
			{
				"type": "reasoning",
				"encrypted_content": "gemini#` + testResponsesGeminiThoughtSignature + `",
				"summary": [{"type": "summary_text", "text": "reasoning summary"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": "string visible answer"
			}
		]
	}`
	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", []byte(inputJSON), false)
	if got := gjson.GetBytes(output, "contents.0.parts.1.text").String(); got != "string visible answer" {
		t.Fatalf("visible text = %q", got)
	}
}

func TestConvertOpenAIResponsesRequestToGemini_PreservesWhitespaceWhenMergingReasoning(t *testing.T) {
	inputJSON := `{
		"model": "gemini-3.5-flash",
		"input": [
			{
				"type": "reasoning",
				"encrypted_content": "gemini#` + testResponsesGeminiThoughtSignature + `",
				"summary": [{"type": "summary_text", "text": "reasoning summary"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "  lead trail  "}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "next"}]
			}
		]
	}`
	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", []byte(inputJSON), false)
	if got := gjson.GetBytes(output, "contents.0.parts.1.text").String(); got != "  lead trail  " {
		t.Fatalf("visible text = %q, want preserved whitespace", got)
	}
}
func TestConvertOpenAIResponsesRequestToGemini_SystemAndDeveloperRoles(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		wantText string
	}{
		{
			name:     "system role",
			role:     "system",
			wantText: "System message text",
		},
		{
			name:     "developer role",
			role:     "developer",
			wantText: "Developer message text",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				input := []byte(`{
				"instructions": "Be a helpful assistant",
				"input": [
					{
						"type": "message",
						"role": "` + tt.role + `",
						"content": [
							{
								"type": "input_text",
								"text": "` + tt.wantText + `"
							}
						]
					},
					{
						"type": "message",
						"role": "user",
						"content": [
							{
								"type": "input_text",
								"text": "Hello"
							}
						]
					}
				]
			}`)

				output := ConvertOpenAIResponsesRequestToGemini("gemini-3.5-flash", input, false)
				result := gjson.ParseBytes(output)

				systemInstruction := result.Get("systemInstruction")
				if !systemInstruction.Exists() {
					t.Fatalf("systemInstruction missing. Output: %s", output)
				}
				parts := systemInstruction.Get("parts")
				if got := parts.Get("#").Int(); got != 2 {
					t.Fatalf("systemInstruction parts = %d, want 2. Output: %s", got, output)
				}
				if got := parts.Get("0.text").String(); got != "Be a helpful assistant" {
					t.Fatalf(
						"first systemInstruction part = %q, want %q. Output: %s", got, "Be a helpful assistant", output,
					)
				}
				if got := parts.Get("1.text").String(); got != tt.wantText {
					t.Fatalf("second systemInstruction part = %q, want %q. Output: %s", got, tt.wantText, output)
				}

				result.Get("contents").ForEach(
					func(_, value gjson.Result) bool {
						if role := value.Get("role").String(); role == tt.role {
							t.Fatalf("role %q leaked into contents array. Output: %s", tt.role, output)
						}
						return true
					},
				)
			},
		)
	}
}

func TestConvertOpenAIResponsesRequestToGeminiCleansToolSchemaRequiredFields(t *testing.T) {
	inputJSON := `{
		"model": "gemini-2.0-flash",
		"input": "hi",
		"tools": [{
			"type": "function",
			"name": "search_company",
			"description": "Search",
			"parameters": {
				"type": "object",
				"title": "SearchCompany",
				"properties": {
					"country": {"type": "string"},
					"industry": {"type": "string"}
				},
				"required": ["country", "industry", "stale_field", "another_stale"]
			}
		}]
	}`

	output := ConvertOpenAIResponsesRequestToGemini("gemini-2.0-flash", []byte(inputJSON), false)
	schema := gjson.GetBytes(output, "tools.0.functionDeclarations.0.parametersJsonSchema")

	if !schema.Exists() {
		t.Fatalf("parametersJsonSchema missing. Output: %s", output)
	}
	if schema.Get("title").Exists() {
		t.Fatalf("schema title should be removed. Output: %s", output)
	}
	required := schema.Get("required").Array()
	if len(required) != 2 {
		t.Fatalf("required length = %d, want 2. Schema: %s", len(required), schema.Raw)
	}
	if got := required[0].String(); got != "country" {
		t.Fatalf("required[0] = %q, want country. Schema: %s", got, schema.Raw)
	}
	if got := required[1].String(); got != "industry" {
		t.Fatalf("required[1] = %q, want industry. Schema: %s", got, schema.Raw)
	}
}

func TestConvertOpenAIResponsesRequestToGeminiConvertsNamespaceCustomTools(t *testing.T) {
	input := []byte(`{
		"tools":[{
			"type":"namespace",
			"name":"mcp__editor",
			"tools":[{"type":"custom","name":"apply_patch","description":"Apply a patch"}]
		}],
		"tool_choice":{"type":"custom","name":"apply_patch","namespace":"mcp__editor"},
		"input":[
			{"type":"additional_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]},
			{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","namespace":"mcp__editor","input":"*** Begin Patch"},
			{"type":"custom_tool_call_output","call_id":"call-1","output":"applied"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash", input, false)
	if len(output) == 0 {
		t.Fatal("translation was rejected")
	}
	if got := gjson.GetBytes(output, "tools.0.functionDeclarations.0.name").String(); got != "mcp__editor__apply_patch" {
		t.Fatalf("custom declaration name = %q; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "tools.0.functionDeclarations.0.parametersJsonSchema.required.0").String(); got != "input" {
		t.Fatalf("custom declaration schema missing input wrapper: %s", output)
	}
	if got := gjson.GetBytes(output, "tools.0.functionDeclarations.1.name").String(); got != "lookup" {
		t.Fatalf("additional tool declaration name = %q; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "toolConfig.functionCallingConfig.allowedFunctionNames.0").String(); got != "mcp__editor__apply_patch" {
		t.Fatalf("tool choice = %q; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "contents.0.parts.0.functionCall.name").String(); got != "mcp__editor__apply_patch" {
		t.Fatalf("custom call name = %q; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "contents.0.parts.0.functionCall.args.input").String(); got != "*** Begin Patch" {
		t.Fatalf("custom call input = %q; output=%s", got, output)
	}
	if got := gjson.GetBytes(output, "contents.1.parts.0.functionResponse.name").String(); got != "mcp__editor__apply_patch" {
		t.Fatalf("custom response name = %q; output=%s", got, output)
	}
}

func TestConvertOpenAIResponsesRequestToGeminiResolvesUniqueShortToolName(t *testing.T) {
	input := []byte(`{
		"tools":[{"type":"namespace","name":"first","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}],
		"input":[{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"}]
	}`)
	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash", input, false)
	if got := gjson.GetBytes(output, "contents.0.parts.0.functionCall.name").String(); got != "first__lookup" {
		t.Fatalf("resolved name = %q, want first__lookup; output=%s", got, output)
	}
}

func TestConvertOpenAIResponsesRequestToGeminiRejectsAmbiguousShortToolName(t *testing.T) {
	input := []byte(`{
		"tools":[
			{"type":"namespace","name":"first","tools":[{"type":"function","name":"lookup"}]},
			{"type":"namespace","name":"second","tools":[{"type":"custom","name":"lookup"}]}
		],
		"input":[{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"}]
	}`)
	output, errConvert := convertOpenAIResponsesRequestToGemini("gemini-3.6-flash", input, false)
	if !errors.Is(errConvert, translatorcommon.ErrResponsesToolNameAmbiguous) {
		t.Fatalf("error = %v, want ambiguous tool name", errConvert)
	}
	if output != nil {
		t.Fatalf("ambiguous translation output = %s, want nil", output)
	}
	if publicOutput := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash", input, false); publicOutput != nil {
		t.Fatalf("public translator did not reject ambiguity: %s", publicOutput)
	}
}

func TestConvertOpenAIResponsesRequestToGeminiPreservesStructuredAndMixedToolOutputs(t *testing.T) {
	input := []byte(`{
		"input":[
			{"type":"function_call","call_id":"call-string","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call-string","output":"{\"value\":1}"},
			{"type":"function_call","call_id":"call-object","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call-object","output":{"value":1}},
			{"type":"function_call","call_id":"call-mixed","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call-mixed","output":[
				{"type":"text","text":"first"},
				{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="},
				{"type":"input_image","image_url":"data:image/jpeg;base64,d29ybGQ="},
				{"type":"text","text":"second"}
			]}
		]
	}`)
	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash", input, false)
	contents := gjson.GetBytes(output, "contents").Array()
	if got := contents[1].Get("parts.0.functionResponse.response.result"); got.Type != gjson.String || got.String() != `{"value":1}` {
		t.Fatalf("raw string result changed type: %s; output=%s", got.Raw, output)
	}
	if got := contents[3].Get("parts.0.functionResponse.response.result.value").Int(); got != 1 {
		t.Fatalf("object result = %d, want 1; output=%s", got, output)
	}
	mixed := contents[5].Get("parts.0.functionResponse")
	if got := mixed.Get("response.result").String(); got != "first\nsecond" {
		t.Fatalf("mixed text result = %q; output=%s", got, output)
	}
	if got := mixed.Get("parts.#").Int(); got != 2 {
		t.Fatalf("mixed image part count = %d, want 2; output=%s", got, output)
	}
	if got := mixed.Get("parts.0.inlineData.mimeType").String(); got != "image/png" {
		t.Fatalf("first image MIME = %q; output=%s", got, output)
	}
	if got := mixed.Get("parts.1.inlineData.mimeType").String(); got != "image/jpeg" {
		t.Fatalf("second image MIME = %q; output=%s", got, output)
	}
}

func TestConvertOpenAIResponsesRequestToGeminiDeduplicatesResultsAndKeepsImageSlots(t *testing.T) {
	input := []byte(`{
		"input":[
			{"type":"function_call","call_id":"call-1","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call-1","output":"old"},
			{"type":"function_call_output","call_id":"call-1","output":"new"},
			{"type":"message","role":"user","content":[
				{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="},
				{"type":"input_text","text":"continue"}
			]}
		]
	}`)
	output := ConvertOpenAIResponsesRequestToGemini("gemini-3.6-flash", input, false)
	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 2 {
		t.Fatalf("contents count = %d, want 2; output=%s", len(contents), output)
	}
	parts := contents[1].Get("parts").Array()
	if len(parts) != 3 || parts[0].Get("text").String() != "continue" ||
		parts[1].Get("inline_data.data").String() != "aGVsbG8=" ||
		parts[2].Get("functionResponse.response.result").String() != "new" {
		t.Fatalf("result-slot ordering or de-duplication failed: %s", contents[1].Raw)
	}
}

func validResponsesGPTReasoningSignature() string {
	raw := make([]byte, 1+8+16+16+32)
	raw[0] = 0x80
	raw[8] = 1
	for i := 9; i < len(raw); i++ {
		raw[i] = byte(i)
	}
	return base64.URLEncoding.EncodeToString(raw)
}

func TestConvertOpenAIResponsesRequestToGeminiEffortDoesNotSetSummary(t *testing.T) {
	output := ConvertOpenAIResponsesRequestToGemini(
		"gemini-3-flash",
		[]byte(`{"reasoning":{"effort":"high"},"input":"hello"}`),
		false,
	)
	if got := gjson.GetBytes(output, "generationConfig.thinkingConfig.thinkingLevel").String(); got != "high" {
		t.Fatalf("thinkingLevel = %q, want high; output=%s", got, output)
	}
	if gjson.GetBytes(output, "generationConfig.thinkingConfig.includeThoughts").Exists() {
		t.Fatalf("Responses effort alone set includeThoughts: %s", output)
	}
}

func TestConvertOpenAIResponsesRequestToGeminiTreatsDeveloperAsSystemInstruction(t *testing.T) {
	input := []byte(`{
		"input": [
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"follow policy"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToGemini("gemini-2.5-pro", input, false)

	if got := gjson.GetBytes(output, "systemInstruction.parts.0.text").String(); got != "follow policy" {
		t.Fatalf("systemInstruction.parts.0.text = %q, want follow policy; output=%s", got, output)
	}
	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 1 {
		t.Fatalf("contents length = %d, want 1; output=%s", len(contents), output)
	}
	if got := contents[0].Get("role").String(); got != "user" {
		t.Fatalf("contents.0.role = %q, want user; output=%s", got, output)
	}
}

func TestConvertOpenAIResponsesRequestToGeminiAccumulatesSystemInstructions(t *testing.T) {
	input := []byte(`{
		"instructions":"base instruction",
		"input":[
			{"type":"message","role":"system","content":[{"type":"input_text","text":"system policy"}]},
			{"type":"message","role":"developer","content":"developer policy"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToGemini("gemini-2.5-pro", input, false)

	parts := gjson.GetBytes(output, "systemInstruction.parts").Array()
	want := []string{"base instruction", "system policy", "developer policy"}
	if len(parts) != len(want) {
		t.Fatalf("systemInstruction parts = %d, want %d; output=%s", len(parts), len(want), output)
	}
	for i, text := range want {
		if got := parts[i].Get("text").String(); got != text {
			t.Fatalf("systemInstruction.parts.%d.text = %q, want %q; output=%s", i, got, text, output)
		}
	}
	contents := gjson.GetBytes(output, "contents").Array()
	if len(contents) != 1 || contents[0].Get("role").String() != "user" {
		t.Fatalf("system roles should not leak into contents: %s", output)
	}
}

func TestConvertOpenAIResponsesRequestToGeminiMapsTextFormats(t *testing.T) {
	tests := []struct {
		name       string
		format     string
		wantSchema bool
	}{
		{
			name:       "json schema prefers canonical schema",
			format:     `{"type":"json_schema","schema":{"type":"object","properties":{"canonical":{"type":"string"}}},"json_schema":{"schema":{"type":"object","properties":{"nested":{"type":"string"}}}}}`,
			wantSchema: true,
		},
		{
			name:   "json object",
			format: `{"type":"json_object"}`,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				input := []byte(`{
				"input":"return JSON",
				"max_output_tokens":256,
				"temperature":0.2,
				"top_p":0.8,
				"stop_sequences":["END"],
				"text":{"format":` + tt.format + `}
			}`)

				output := ConvertOpenAIResponsesRequestToGemini("gemini-2.5-pro", input, false)
				config := gjson.GetBytes(output, "generationConfig")
				if config.Get("maxOutputTokens").Int() != 256 ||
					config.Get("temperature").Float() != 0.2 ||
					config.Get("topP").Float() != 0.8 ||
					config.Get("stopSequences.0").String() != "END" {
					t.Fatalf("generation config fields were not preserved: %s", output)
				}
				if got := config.Get("responseMimeType").String(); got != "application/json" {
					t.Fatalf("responseMimeType = %q, want application/json: %s", got, output)
				}
				if tt.wantSchema {
					schema := config.Get("responseJsonSchema")
					if !schema.Get("properties.canonical").Exists() || schema.Get("properties.nested").Exists() {
						t.Fatalf("canonical schema was not preferred: %s", output)
					}
				} else if config.Get("responseJsonSchema").Exists() {
					t.Fatalf("json_object should not set responseJsonSchema: %s", output)
				}
			},
		)
	}
}

func TestConvertOpenAIResponsesRequestToGeminiIgnoresUnknownTextFormat(t *testing.T) {
	input := []byte(`{"input":"hello","text":{"format":{"type":"xml"}}}`)

	output := ConvertOpenAIResponsesRequestToGemini("gemini-2.5-pro", input, false)

	if gjson.GetBytes(output, "generationConfig").Exists() {
		t.Fatalf("unknown text format should not create generationConfig: %s", output)
	}
}

func TestConvertOpenAIResponsesRequestToGeminiBuildsGenerationConfigWithoutIntermediateObject(t *testing.T) {
	input := []byte(`{"input":"hello","temperature":0.5,"top_p":0.9,"stop_sequences":["done"],"text":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`)

	output := ConvertOpenAIResponsesRequestToGemini("gemini-test", input, false)

	if got := gjson.GetBytes(output, "generationConfig.temperature").Float(); got != 0.5 {
		t.Fatalf("temperature = %v, want 0.5", got)
	}
	if got := gjson.GetBytes(output, "generationConfig.topP").Float(); got != 0.9 {
		t.Fatalf("topP = %v, want 0.9", got)
	}
	if got := gjson.GetBytes(output, "generationConfig.stopSequences.0").String(); got != "done" {
		t.Fatalf("stop sequence = %q, want done", got)
	}
	if got := gjson.GetBytes(output, "generationConfig.responseMimeType").String(); got != "application/json" {
		t.Fatalf("responseMimeType = %q, want application/json", got)
	}
	if !gjson.GetBytes(output, "generationConfig.responseJsonSchema").Exists() {
		t.Fatal("responseJsonSchema should be present")
	}
	if gjson.GetBytes(output, "generationConfig.responseSchema").Exists() {
		t.Fatal("responseSchema should not be present")
	}
}
