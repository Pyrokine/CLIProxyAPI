package gemini

import (
	"encoding/base64"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func validGeminiThoughtSignature() string {
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, []byte{0x01, 0x02, 0x03, 0x04})
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func TestConvertGeminiRequestToAntigravity_PreserveValidSignature(t *testing.T) {
	validSignature := validGeminiThoughtSignature()
	inputJSON := []byte(fmt.Sprintf(
		`{
		"model": "gemini-3-pro-preview",
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "test_tool", "args": {}}, "thoughtSignature": "%s"}
				]
			}
		]
	}`, validSignature,
	))

	output := ConvertGeminiRequestToAntigravity("gemini-3-pro-preview", inputJSON, false)
	outputStr := string(output)

	// Check that valid thoughtSignature is preserved
	parts := gjson.Get(outputStr, "request.contents.0.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("Expected 1 part, got %d", len(parts))
	}

	sig := parts[0].Get("thoughtSignature").String()
	if sig != validSignature {
		t.Errorf("Expected thoughtSignature '%s', got '%s'", validSignature, sig)
	}
}

func TestConvertGeminiRequestToAntigravity_RemovesGeminiSignatureForClaudeTarget(t *testing.T) {
	validSignature := validGeminiThoughtSignature()
	inputJSON := []byte(fmt.Sprintf(
		`{
			"contents": [{
				"role": "model",
				"parts": [
					{"thought": true, "text": "hidden", "thoughtSignature": "%s"},
					{"functionCall": {"name": "test_tool", "args": {}}, "thoughtSignature": "%s"}
				]
			}]
		}`, validSignature, validSignature,
	))

	output := ConvertGeminiRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("parts len = %d, want 1; output: %s", len(parts), output)
	}
	if !parts[0].Get("functionCall").Exists() {
		t.Fatalf("expected functionCall to remain; output: %s", output)
	}
	if parts[0].Get("thoughtSignature").Exists() {
		t.Fatalf("thoughtSignature should be removed for Claude target; output: %s", output)
	}
	if signature.DetectProvider(validSignature) != signature.ProviderGemini {
		t.Fatalf("test signature should be detected as Gemini")
	}
}

func TestConvertGeminiRequestToAntigravity_DropsSignatureOnlyPartForClaudeTarget(t *testing.T) {
	validSignature := validGeminiThoughtSignature()
	inputJSON := []byte(fmt.Sprintf(
		`{
			"contents": [{
				"role": "model",
				"parts": [
					{"thoughtSignature": "%s"},
					{"extra_content": {"google": {"thought_signature": "%s"}}},
					{"functionCall": {"name": "test_tool", "args": {}}, "thoughtSignature": "%s"}
				]
			}]
		}`, validSignature, validSignature, validSignature,
	))

	output := ConvertGeminiRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	parts := gjson.GetBytes(output, "request.contents.0.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("parts len = %d, want 1; output: %s", len(parts), output)
	}
	if !parts[0].Get("functionCall").Exists() {
		t.Fatalf("expected functionCall to remain; output: %s", output)
	}
	if parts[0].Get("thoughtSignature").Exists() {
		t.Fatalf("thoughtSignature should be removed for Claude target; output: %s", output)
	}
}

func TestConvertGeminiRequestToAntigravity_AddSkipSentinelToFunctionCall(t *testing.T) {
	// functionCall without signature should get skip_thought_signature_validator
	inputJSON := []byte(`{
		"model": "gemini-3-pro-preview",
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "test_tool", "args": {}}}
				]
			}
		]
	}`)

	output := ConvertGeminiRequestToAntigravity("gemini-3-pro-preview", inputJSON, false)
	outputStr := string(output)

	// Check that skip_thought_signature_validator is added to functionCall
	sig := gjson.Get(outputStr, "request.contents.0.parts.0.thoughtSignature").String()
	expectedSig := "skip_thought_signature_validator"
	if sig != expectedSig {
		t.Errorf("Expected skip sentinel '%s', got '%s'", expectedSig, sig)
	}
}

func TestConvertGeminiRequestToAntigravity_ParallelFunctionCalls(t *testing.T) {
	// Only the first functionCall receives the synthetic bypass signature
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
	outputStr := string(output)

	parts := gjson.Get(outputStr, "request.contents.0.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("Expected 2 parts, got %d", len(parts))
	}

	if got := parts[0].Get("thoughtSignature").String(); got != "skip_thought_signature_validator" {
		t.Errorf("first part signature = %q, want skip sentinel", got)
	}
	if got := parts[1].Get("thoughtSignature").String(); got != "" {
		t.Errorf("parallel sibling signature = %q, want unsigned", got)
	}
}

func TestRewriteGeminiFunctionNamesReusesNormalizedPayload(t *testing.T) {
	input := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"ok"}}}]}],"toolConfig":{"functionCallingConfig":{"allowedFunctionNames":["lookup"]}}}}`)

	output := rewriteGeminiFunctionNames(input, nil)

	if &output[0] != &input[0] {
		t.Fatal("normalized function names caused a payload copy")
	}
}

func TestRemoveEmptyGeminiFunctionToolsReusesNormalizedPayload(t *testing.T) {
	input := []byte(`{"request":{"tools":[{"functionDeclarations":[{"name":"lookup"}]}]}}`)

	output := removeEmptyGeminiFunctionTools(input)

	if &output[0] != &input[0] {
		t.Fatal("non-empty tools caused a payload copy")
	}
}

func TestRemoveEmptyGeminiFunctionToolsDeletesEmptyArray(t *testing.T) {
	input := []byte(`{"request":{"tools":[]}}`)

	output := removeEmptyGeminiFunctionTools(input)

	if gjson.GetBytes(output, "request.tools").Exists() {
		t.Fatalf("empty tools should be removed: %s", output)
	}
}

func TestRewriteGeminiFunctionNamesNormalizesNonStringNames(t *testing.T) {
	input := []byte(`{"request":{"contents":[{"role":"model","parts":[{"functionCall":{"name":true,"args":{}}}]}],"toolConfig":{"functionCallingConfig":{"allowedFunctionNames":[true]}}}}`)

	output := rewriteGeminiFunctionNames(input, nil)

	if name := gjson.GetBytes(
		output, "request.contents.0.parts.0.functionCall.name",
	); name.Type != gjson.String || name.String() != "true" {
		t.Fatalf("functionCall.name = %s, want string true", name.Raw)
	}
	if name := gjson.GetBytes(
		output, "request.toolConfig.functionCallingConfig.allowedFunctionNames.0",
	); name.Type != gjson.String || name.String() != "true" {
		t.Fatalf("allowedFunctionNames.0 = %s, want string true", name.Raw)
	}
}

func TestConvertGeminiRequestToAntigravityUsesRequestScopedFunctionNames(t *testing.T) {
	input := []byte(`{
		"tools":[{"functionDeclarations":[
			{"name":"foo bar"},
			{"name":"foo-bar"},
			{"name":"foo bar"}
		]}],
		"contents":[{"role":"model","parts":[
			{"functionCall":{"name":"foo bar","args":{}}},
			{"functionCall":{"name":"foo-bar","args":{}}}
		]}],
		"toolConfig":{"functionCallingConfig":{"allowedFunctionNames":["foo bar","foo-bar"]}}
	}`)

	output := ConvertGeminiRequestToAntigravity("gemini-3-pro-preview", input, false)
	declarations := gjson.GetBytes(output, "request.tools.0.functionDeclarations").Array()
	if len(declarations) != 2 {
		t.Fatalf("function declarations len = %d, want 2; output: %s", len(declarations), output)
	}
	firstName := declarations[0].Get("name").String()
	secondName := declarations[1].Get("name").String()
	if firstName == "" || secondName == "" || firstName == secondName {
		t.Fatalf("mapped names = %q, %q, want distinct non-empty names", firstName, secondName)
	}
	if got := gjson.GetBytes(output, "request.contents.0.parts.0.functionCall.name").String(); got != firstName {
		t.Fatalf("first functionCall name = %q, want %q", got, firstName)
	}
	if got := gjson.GetBytes(output, "request.contents.0.parts.1.functionCall.name").String(); got != secondName {
		t.Fatalf("second functionCall name = %q, want %q", got, secondName)
	}
	allowedNames := gjson.GetBytes(output, "request.toolConfig.functionCallingConfig.allowedFunctionNames").Array()
	if len(allowedNames) != 2 || allowedNames[0].String() != firstName || allowedNames[1].String() != secondName {
		t.Fatalf("allowed function names do not match declarations: %s", output)
	}
}

func TestFixCLIToolResponseReusesHistoryWithoutFunctionResponses(t *testing.T) {
	input := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]},{"role":"model","parts":[{"text":"world"}]}]}}`)

	output, errFix := fixCLIToolResponse(input)
	if errFix != nil {
		t.Fatalf("fixCLIToolResponse returned an error: %v", errFix)
	}
	if string(output) != string(input) {
		t.Fatalf("history changed:\n got: %s\nwant: %s", output, input)
	}
	if &output[0] != &input[0] {
		t.Fatal("history without function responses caused a payload copy")
	}
}

func TestFixCLIToolResponsePreservesObjectNormalization(t *testing.T) {
	input := []byte(`{"request":{"contents":{"first":{"role":"user","parts":[{"text":"hello"}]}}}}`)

	output, errFix := fixCLIToolResponse(input)
	if errFix != nil {
		t.Fatalf("fixCLIToolResponse returned an error: %v", errFix)
	}
	if !gjson.GetBytes(output, "request.contents").IsArray() {
		t.Fatalf("contents should be normalized to an array: %s", output)
	}
}

func TestConvertGeminiRequestToAntigravityBoundsLargePayloadCopies(t *testing.T) {
	const inlineDataSize = 4 << 20
	input := []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"image/png","data":"` +
		strings.Repeat("A", inlineDataSize) + `"}},{"text":"describe"}]}]}`)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	output := ConvertGeminiRequestToAntigravity("gemini-3-flash", input, false)
	runtime.ReadMemStats(&after)

	if got := gjson.GetBytes(
		output, "request.contents.0.parts.0.inlineData.data",
	).String(); len(got) != inlineDataSize {
		t.Fatalf("inline data length = %d, want %d", len(got), inlineDataSize)
	}
	if got := gjson.GetBytes(output, "model").String(); got != "gemini-3-flash" {
		t.Fatalf("model = %q, want gemini-3-flash", got)
	}
	if got := gjson.GetBytes(output, "request.safetySettings"); !got.IsArray() {
		t.Fatalf("request.safetySettings = %s, want array", got.Raw)
	}

	const allowedCopies = 3
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > allowedCopies*inlineDataSize {
		t.Fatalf(
			"conversion allocated %d bytes for a %d byte payload, want at most %d",
			allocated,
			inlineDataSize,
			allowedCopies*inlineDataSize,
		)
	}
}

func TestFixCLIToolResponse_PreservesFunctionResponseParts(t *testing.T) {
	// When functionResponse contains a "parts" field with inlineData (from Claude
	// translator's image embedding), fixCLIToolResponse should preserve it as-is.
	// parseFunctionResponseRaw returns response.Raw for valid JSON objects,
	// so extra fields like "parts" survive the pipeline.
	input := `{
		"model": "claude-opus-4-6-thinking",
		"request": {
			"contents": [
				{
					"role": "model",
					"parts": [
						{
							"functionCall": {"name": "screenshot", "args": {}}
						}
					]
				},
				{
					"role": "function",
					"parts": [
						{
							"functionResponse": {
								"id": "tool-001",
								"name": "screenshot",
								"response": {"result": "Screenshot taken"},
								"parts": [
									{"inlineData": {"mimeType": "image/png", "data": "iVBOR"}}
								]
							}
						}
					]
				}
			]
		}
	}`

	result, err := fixCLIToolResponse([]byte(input))
	if err != nil {
		t.Fatalf("fixCLIToolResponse failed: %v", err)
	}

	// Find the function response content (role=function)
	contents := gjson.GetBytes(result, "request.contents").Array()
	var funcContent gjson.Result
	for _, c := range contents {
		if c.Get("role").String() == "function" {
			funcContent = c
			break
		}
	}
	if !funcContent.Exists() {
		t.Fatal("function role content should exist in output")
	}

	// The functionResponse should be preserved with its parts field
	funcResp := funcContent.Get("parts.0.functionResponse")
	if !funcResp.Exists() {
		t.Fatal("functionResponse should exist in output")
	}

	// Verify the parts field with inlineData is preserved
	inlineParts := funcResp.Get("parts").Array()
	if len(inlineParts) != 1 {
		t.Fatalf("Expected 1 inlineData part in functionResponse.parts, got %d", len(inlineParts))
	}
	if inlineParts[0].Get("inlineData.mimeType").String() != "image/png" {
		t.Errorf("Expected mimeType 'image/png', got '%s'", inlineParts[0].Get("inlineData.mimeType").String())
	}
	if inlineParts[0].Get("inlineData.data").String() != "iVBOR" {
		t.Errorf("Expected data 'iVBOR', got '%s'", inlineParts[0].Get("inlineData.data").String())
	}

	// Verify response.result is also preserved
	if funcResp.Get("response.result").String() != "Screenshot taken" {
		t.Errorf("Expected response.result 'Screenshot taken', got '%s'", funcResp.Get("response.result").String())
	}
}

func TestFixCLIToolResponse_BackfillsEmptyFunctionResponseName(t *testing.T) {
	// When the Amp client sends functionResponse with an empty name,
	// fixCLIToolResponse should backfill it from the corresponding functionCall.
	input := `{
		"model": "gemini-3-pro-preview",
		"request": {
			"contents": [
				{
					"role": "model",
					"parts": [
						{"functionCall": {"name": "Bash", "args": {"cmd": "ls"}}}
					]
				},
				{
					"role": "function",
					"parts": [
						{"functionResponse": {"name": "", "response": {"output": "file1.txt"}}}
					]
				}
			]
		}
	}`

	result, err := fixCLIToolResponse([]byte(input))
	if err != nil {
		t.Fatalf("fixCLIToolResponse failed: %v", err)
	}

	contents := gjson.GetBytes(result, "request.contents").Array()
	var funcContent gjson.Result
	for _, c := range contents {
		if c.Get("role").String() == "function" {
			funcContent = c
			break
		}
	}
	if !funcContent.Exists() {
		t.Fatal("function role content should exist in output")
	}

	name := funcContent.Get("parts.0.functionResponse.name").String()
	if name != "Bash" {
		t.Errorf("Expected backfilled name 'Bash', got '%s'", name)
	}
}

func TestFixCLIToolResponse_BackfillsMultipleEmptyNames(t *testing.T) {
	// Parallel function calls: both responses have empty names.
	input := `{
		"model": "gemini-3-pro-preview",
		"request": {
			"contents": [
				{
					"role": "model",
					"parts": [
						{"functionCall": {"name": "Read", "args": {"path": "/a"}}},
						{"functionCall": {"name": "Grep", "args": {"pattern": "x"}}}
					]
				},
				{
					"role": "function",
					"parts": [
						{"functionResponse": {"name": "", "response": {"result": "content a"}}},
						{"functionResponse": {"name": "", "response": {"result": "match x"}}}
					]
				}
			]
		}
	}`

	result, err := fixCLIToolResponse([]byte(input))
	if err != nil {
		t.Fatalf("fixCLIToolResponse failed: %v", err)
	}

	contents := gjson.GetBytes(result, "request.contents").Array()
	var funcContent gjson.Result
	for _, c := range contents {
		if c.Get("role").String() == "function" {
			funcContent = c
			break
		}
	}
	if !funcContent.Exists() {
		t.Fatal("function role content should exist in output")
	}

	parts := funcContent.Get("parts").Array()
	if len(parts) != 2 {
		t.Fatalf("Expected 2 function response parts, got %d", len(parts))
	}

	name0 := parts[0].Get("functionResponse.name").String()
	name1 := parts[1].Get("functionResponse.name").String()
	if name0 != "Read" {
		t.Errorf("Expected first response name 'Read', got '%s'", name0)
	}
	if name1 != "Grep" {
		t.Errorf("Expected second response name 'Grep', got '%s'", name1)
	}
}

func TestFixCLIToolResponse_PreservesExistingName(t *testing.T) {
	// When functionResponse already has a valid name, it should be preserved.
	input := `{
		"model": "gemini-3-pro-preview",
		"request": {
			"contents": [
				{
					"role": "model",
					"parts": [
						{"functionCall": {"name": "Bash", "args": {}}}
					]
				},
				{
					"role": "function",
					"parts": [
						{"functionResponse": {"name": "Bash", "response": {"result": "ok"}}}
					]
				}
			]
		}
	}`

	result, err := fixCLIToolResponse([]byte(input))
	if err != nil {
		t.Fatalf("fixCLIToolResponse failed: %v", err)
	}

	contents := gjson.GetBytes(result, "request.contents").Array()
	var funcContent gjson.Result
	for _, c := range contents {
		if c.Get("role").String() == "function" {
			funcContent = c
			break
		}
	}
	if !funcContent.Exists() {
		t.Fatal("function role content should exist in output")
	}

	name := funcContent.Get("parts.0.functionResponse.name").String()
	if name != "Bash" {
		t.Errorf("Expected preserved name 'Bash', got '%s'", name)
	}
}

func TestFixCLIToolResponse_MoreResponsesThanCalls(t *testing.T) {
	// If there are more function responses than calls, unmatched extras are discarded by grouping.
	input := `{
		"model": "gemini-3-pro-preview",
		"request": {
			"contents": [
				{
					"role": "model",
					"parts": [
						{"functionCall": {"name": "Bash", "args": {}}}
					]
				},
				{
					"role": "function",
					"parts": [
						{"functionResponse": {"name": "", "response": {"result": "ok"}}},
						{"functionResponse": {"name": "", "response": {"result": "extra"}}}
					]
				}
			]
		}
	}`

	result, err := fixCLIToolResponse([]byte(input))
	if err != nil {
		t.Fatalf("fixCLIToolResponse failed: %v", err)
	}

	contents := gjson.GetBytes(result, "request.contents").Array()
	var funcContent gjson.Result
	for _, c := range contents {
		if c.Get("role").String() == "function" {
			funcContent = c
			break
		}
	}
	if !funcContent.Exists() {
		t.Fatal("function role content should exist in output")
	}

	// First response should be backfilled from the call
	name0 := funcContent.Get("parts.0.functionResponse.name").String()
	if name0 != "Bash" {
		t.Errorf("Expected first response name 'Bash', got '%s'", name0)
	}
}

func TestFixCLIToolResponse_MultipleGroupsFIFO(t *testing.T) {
	// Two sequential function call groups should be matched FIFO.
	input := `{
		"model": "gemini-3-pro-preview",
		"request": {
			"contents": [
				{
					"role": "model",
					"parts": [
						{"functionCall": {"name": "Read", "args": {}}}
					]
				},
				{
					"role": "function",
					"parts": [
						{"functionResponse": {"name": "", "response": {"result": "file content"}}}
					]
				},
				{
					"role": "model",
					"parts": [
						{"functionCall": {"name": "Grep", "args": {}}}
					]
				},
				{
					"role": "function",
					"parts": [
						{"functionResponse": {"name": "", "response": {"result": "match"}}}
					]
				}
			]
		}
	}`

	result, err := fixCLIToolResponse([]byte(input))
	if err != nil {
		t.Fatalf("fixCLIToolResponse failed: %v", err)
	}

	contents := gjson.GetBytes(result, "request.contents").Array()
	var funcContents []gjson.Result
	for _, c := range contents {
		if c.Get("role").String() == "function" {
			funcContents = append(funcContents, c)
		}
	}
	if len(funcContents) != 2 {
		t.Fatalf("Expected 2 function contents, got %d", len(funcContents))
	}

	name0 := funcContents[0].Get("parts.0.functionResponse.name").String()
	name1 := funcContents[1].Get("parts.0.functionResponse.name").String()
	if name0 != "Read" {
		t.Errorf("Expected first group name 'Read', got '%s'", name0)
	}
	if name1 != "Grep" {
		t.Errorf("Expected second group name 'Grep', got '%s'", name1)
	}
}

func TestConvertGeminiRequestToAntigravity_ClaudeModelNormalizesStrictClaudeThoughtSignature(t *testing.T) {
	nativeSig := testAntigravityGeminiClaudeSignature(t)
	expectedSig, ok := signature.CompatibleAntigravityClaudeThinkingSignature(nativeSig)
	if !ok {
		t.Fatal("test Claude signature should be compatible with Antigravity Claude")
	}

	inputJSON := []byte(`{
		"model": "claude-opus-4-6-thinking",
		"contents": [
			{
				"role": "model",
				"parts": [
					{"text": "internal reasoning", "thought": true, "thoughtSignature": "` + nativeSig + `"},
					{"text": "visible answer"}
				]
			},
			{
				"role": "user",
				"parts": [{"text": "continue"}]
			}
		]
	}`)

	output := ConvertGeminiRequestToAntigravity("claude-opus-4-6-thinking", inputJSON, false)

	part := gjson.GetBytes(output, "request.contents.0.parts.0")
	if !part.Get("thought").Bool() {
		t.Fatalf("first part should remain thought. Output: %s", output)
	}
	if got := part.Get("thoughtSignature").String(); got != expectedSig {
		t.Fatalf("thoughtSignature = %q, want %q. Output: %s", got, expectedSig, output)
	}
}

func TestConvertGeminiRequestToAntigravity_ClaudeModelDropsNonStrictEPrefixThoughtSignature(t *testing.T) {
	looseEPrefix := base64.StdEncoding.EncodeToString([]byte{0x12, 0x01, 0x02})
	if looseEPrefix[0] != 'E' {
		t.Fatalf("test signature should start with E, got %q", looseEPrefix[:1])
	}

	inputJSON := []byte(`{
		"model": "claude-opus-4-6-thinking",
		"contents": [
			{
				"role": "model",
				"parts": [
					{"text": "must not reach Claude", "thought": true, "thoughtSignature": "` + looseEPrefix + `"},
					{"text": "visible answer"}
				]
			},
			{
				"role": "user",
				"parts": [{"text": "continue"}]
			}
		]
	}`)

	output := ConvertGeminiRequestToAntigravity("claude-opus-4-6-thinking", inputJSON, false)

	if gjson.GetBytes(output, `request.contents.#.parts.#(thought=true)#`).Int() != 0 {
		t.Fatalf("non-strict E-prefix thought block should be dropped. Output: %s", output)
	}
	if got := gjson.GetBytes(output, "request.contents.0.parts.0.text").String(); got != "visible answer" {
		t.Fatalf("visible text = %q, want visible answer. Output: %s", got, output)
	}
}

func TestConvertGeminiRequestToAntigravity_ClaudeModelDropsEmptyThoughtText(t *testing.T) {
	nativeSig := testAntigravityGeminiClaudeSignature(t)
	inputJSON := []byte(`{
		"model": "claude-opus-4-6-thinking",
		"contents": [
			{
				"role": "model",
				"parts": [
					{"text": "", "thought": true, "thoughtSignature": "` + nativeSig + `"},
					{"text": "visible answer"}
				]
			},
			{
				"role": "user",
				"parts": [{"text": "continue"}]
			}
		]
	}`)

	output := ConvertGeminiRequestToAntigravity("claude-opus-4-6-thinking", inputJSON, false)

	if gjson.GetBytes(output, `request.contents.#.parts.#(thought=true)#`).Int() != 0 {
		t.Fatalf("empty-text thought block should be dropped for Antigravity Claude. Output: %s", output)
	}
	if got := gjson.GetBytes(output, "request.contents.0.parts.0.text").String(); got != "visible answer" {
		t.Fatalf("visible text = %q, want visible answer. Output: %s", got, output)
	}
}

func TestConvertGeminiRequestToAntigravity_ClaudeModelStripsUnneededFunctionCallSignature(t *testing.T) {
	nativeSig := testAntigravityGeminiClaudeSignature(t)
	inputJSON := []byte(`{
		"model": "claude-opus-4-6-thinking",
		"contents": [
			{
				"role": "model",
				"parts": [
					{"functionCall": {"name": "test_tool", "args": {}}, "thoughtSignature": "` + nativeSig + `"}
				]
			}
		]
	}`)

	output := ConvertGeminiRequestToAntigravity("claude-opus-4-6-thinking", inputJSON, false)

	part := gjson.GetBytes(output, "request.contents.0.parts.0")
	if !part.Get("functionCall").Exists() {
		t.Fatalf("functionCall should be preserved. Output: %s", output)
	}
	if part.Get("thoughtSignature").Exists() {
		t.Fatalf("functionCall thoughtSignature should be stripped for Claude target. Output: %s", output)
	}
}

func testAntigravityGeminiClaudeSignature(t *testing.T) string {
	t.Helper()
	var channelBlock []byte
	channelBlock = protowire.AppendTag(channelBlock, 1, protowire.VarintType)
	channelBlock = protowire.AppendVarint(channelBlock, 12)
	channelBlock = protowire.AppendTag(channelBlock, 2, protowire.VarintType)
	channelBlock = protowire.AppendVarint(channelBlock, 2)
	channelBlock = protowire.AppendTag(channelBlock, 6, protowire.BytesType)
	channelBlock = protowire.AppendString(channelBlock, "claude-sonnet-4-6")

	var container []byte
	container = protowire.AppendTag(container, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channelBlock)

	var payload []byte
	payload = protowire.AppendTag(payload, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	payload = protowire.AppendTag(payload, 3, protowire.VarintType)
	payload = protowire.AppendVarint(payload, 1)
	return base64.StdEncoding.EncodeToString(payload)
}

func TestSanitizeAntigravityClaudeGeminiRequestSignatures_PreservesNumberPrecision(t *testing.T) {
	inputJSON := []byte(`{
		"project": "",
		"model": "claude-sonnet-4-6",
		"request": {
			"contents": [
				{
					"role": "model",
					"parts": [
						{
							"text": "thinking",
							"thought": true,
							"thoughtSignature": "invalid"
						},
						{
							"functionCall": {
								"name": "calc",
								"args": {
									"n": 12345678901234567890,
									"big": 9007199254740993
								}
							}
						}
					]
				}
			]
		}
	}`)

	output := SanitizeAntigravityClaudeGeminiRequestSignatures("claude-sonnet-4-6", inputJSON)
	outputStr := string(output)

	bigVal := gjson.Get(outputStr, "request.contents.0.parts.0.functionCall.args.big").Raw
	nVal := gjson.Get(outputStr, "request.contents.0.parts.0.functionCall.args.n").Raw

	if bigVal != "9007199254740993" {
		t.Errorf("Precision lost for big: got %s, want 9007199254740993", bigVal)
	}
	if nVal != "12345678901234567890" {
		t.Errorf("Precision lost for n: got %s, want 12345678901234567890", nVal)
	}
}

func TestSanitizeAntigravityClaudeGeminiRequestSignatures_StripsFunctionCallSignatureForClaudeModel(t *testing.T) {
	inputJSON := []byte(`{
		"project": "",
		"model": "claude-sonnet-4-6",
		"request": {
			"contents": [
				{
					"role": "model",
					"parts": [
						{
							"functionCall": {
								"name": "calc",
								"args": {}
							},
							"thoughtSignature": "skip_thought_signature_validator"
						}
					]
				}
			]
		}
	}`)

	output := SanitizeAntigravityClaudeGeminiRequestSignatures("claude-sonnet-4-6", inputJSON)
	outputStr := string(output)

	sig := gjson.Get(outputStr, "request.contents.0.parts.0.thoughtSignature")
	if sig.Exists() {
		t.Fatalf("expected functionCall thoughtSignature to be stripped for Claude target model, got %s", sig.Raw)
	}
}
