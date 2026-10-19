package responses

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToAntigravityPreservesTypedToolOutputs(t *testing.T) {
	input := []byte(`{
		"input":[
			{"type":"function_call","call_id":"call_string","name":"string_tool","arguments":"{}"},
			{"type":"function_call","call_id":"call_object","name":"object_tool","arguments":"{}"},
			{"type":"function_call","call_id":"call_array","name":"array_tool","arguments":"{}"},
			{"type":"function_call","call_id":"call_number","name":"number_tool","arguments":"{}"},
			{"type":"function_call","call_id":"call_bool","name":"bool_tool","arguments":"{}"},
			{"type":"function_call","call_id":"call_null","name":"null_tool","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_string","output":"{\"shape\":\"string\"}"},
			{"type":"function_call_output","call_id":"call_object","output":{"shape":"object"}},
			{"type":"function_call_output","call_id":"call_array","output":[1,{"shape":"array"}]},
			{"type":"function_call_output","call_id":"call_number","output":42},
			{"type":"function_call_output","call_id":"call_bool","output":true},
			{"type":"function_call_output","call_id":"call_null","output":null}
		]
	}`)

	out := convertOpenAIResponsesRequestToAntigravity("gemini-3-flash", input, false)
	responses := antigravityResponsesByCallID(out)
	if len(responses) != 6 {
		t.Fatalf("function response count = %d, want 6; output=%s", len(responses), out)
	}
	stringResult := responses["call_string"].Get("response.result")
	if stringResult.Type != gjson.String || stringResult.String() != `{"shape":"string"}` {
		t.Fatalf("JSON-shaped string changed: %s; output=%s", stringResult.Raw, out)
	}
	if got := responses["call_object"].Get("response.result.shape").String(); got != "object" {
		t.Fatalf("object result = %q; output=%s", got, out)
	}
	arrayResult := responses["call_array"].Get("response.result")
	if !arrayResult.IsArray() || len(arrayResult.Array()) != 2 || arrayResult.Get("1.shape").String() != "array" {
		t.Fatalf("array result changed: %s; output=%s", arrayResult.Raw, out)
	}
	if numberResult := responses["call_number"].Get("response.result"); numberResult.Type != gjson.Number ||
		numberResult.Int() != 42 {
		t.Fatalf("number result = %s, want numeric 42; output=%s", numberResult.Raw, out)
	}
	if boolResult := responses["call_bool"].Get("response.result"); boolResult.Type != gjson.True {
		t.Fatalf("bool result = %s, want true; output=%s", boolResult.Raw, out)
	}
	if nullResult := responses["call_null"].Get("response.result"); nullResult.Raw != "null" {
		t.Fatalf("null result = %s, want null; output=%s", nullResult.Raw, out)
	}
}

func TestReorderOpenAIResponsesToolOutputSlotsPreservesOtherSlots(t *testing.T) {
	items := gjson.Parse(`[
		{"type":"function_call","call_id":"call_a","name":"tool_a","arguments":"{}"},
		{"type":"function_call","call_id":"call_b","name":"tool_b","arguments":"{}"},
		{"type":"message","role":"user","content":"keep-first"},
		{"type":"function_call_output","call_id":"call_b","output":"B"},
		{"type":"input_image","image_url":"data:image/png;base64,SU1BR0U="},
		{"type":"function_call_output","call_id":"unmatched","output":"X"},
		{"type":"function_call_output","call_id":"call_a","output":"A"}
	]`).Array()

	ordered := reorderOpenAIResponsesToolOutputSlots(items)
	if got := ordered[2].Get("content").String(); got != "keep-first" {
		t.Fatalf("message slot moved or changed: %q", got)
	}
	if got := ordered[3].Get("call_id").String(); got != "call_a" {
		t.Fatalf("first matched output = %q, want call_a", got)
	}
	if got := ordered[4].Get("image_url").String(); got != "data:image/png;base64,SU1BR0U=" {
		t.Fatalf("image slot moved or changed: %q", got)
	}
	if got := ordered[5].Get("call_id").String(); got != "unmatched" {
		t.Fatalf("unmatched output slot moved or changed: %q", got)
	}
	if got := ordered[6].Get("call_id").String(); got != "call_b" {
		t.Fatalf("second matched output = %q, want call_b", got)
	}
}

func TestRestoreOpenAIResponsesToolOutputsPreservesUserBoundaries(t *testing.T) {
	request := []byte(`{"contents":[
		{"role":"user","parts":[{"functionResponse":{"id":"call_a","name":"tool_a","response":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"id":"call_b","name":"tool_b","response":{}}}]}
	]}`)
	outputs := map[string]gjson.Result{
		"call_a": gjson.Parse(`"A"`),
		"call_b": gjson.Parse(`"B"`),
	}

	out := restoreOpenAIResponsesToolOutputs(request, outputs)
	contents := gjson.GetBytes(out, "contents").Array()
	if len(contents) != 2 {
		t.Fatalf("user content boundaries = %d, want 2; output=%s", len(contents), out)
	}
	if got := contents[0].Get("parts.0.functionResponse.response.result").String(); got != "A" {
		t.Fatalf("first result = %q, want A; output=%s", got, out)
	}
	if got := contents[1].Get("parts.0.functionResponse.response.result").String(); got != "B" {
		t.Fatalf("second result = %q, want B; output=%s", got, out)
	}
}

func TestConvertOpenAIResponsesRequestToAntigravityKeepsParallelToolImagesBoundAndOrdered(t *testing.T) {
	input := []byte(`{
		"input":[
			{"type":"function_call","call_id":"call_a","name":"read_a","arguments":"{}"},
			{"type":"function_call","call_id":"call_b","name":"read_b","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_b","output":[
				{"type":"input_text","text":"result B"},
				{"type":"input_image","image_url":"data:image/jpeg;base64,QkJC"}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"keep between results"}]},
			{"type":"function_call_output","call_id":"call_a","output":[
				{"type":"input_text","text":"result A"},
				{"type":"input_image","image_url":"data:image/png;base64,QUFB"}
			]}
		]
	}`)

	out := convertOpenAIResponsesRequestToAntigravity("gemini-3-flash", input, false)
	parts := antigravityResponsesUserParts(out)
	responseIDs := make([]string, 0, 2)
	var textIndex, imageAIndex, imageBIndex int = -1, -1, -1
	for index, part := range parts {
		if got := part.Get("text").String(); got == "keep between results" {
			textIndex = index
		}
		response := part.Get("functionResponse")
		if !response.Exists() {
			continue
		}
		responseIDs = append(responseIDs, response.Get("id").String())
		switch response.Get("id").String() {
		case "call_a":
			if response.Get("response.result").String() != "result A" {
				t.Fatalf("call_a result changed; output=%s", out)
			}
			if response.Get("parts.0.inlineData.data").String() != "QUFB" {
				t.Fatalf("call_a image missing; output=%s", out)
			}
			imageAIndex = index
		case "call_b":
			if response.Get("response.result").String() != "result B" {
				t.Fatalf("call_b result changed; output=%s", out)
			}
			if response.Get("parts.0.inlineData.data").String() != "QkJC" {
				t.Fatalf("call_b image missing; output=%s", out)
			}
			imageBIndex = index
		}
	}
	if len(responseIDs) != 2 || responseIDs[0] != "call_a" || responseIDs[1] != "call_b" {
		t.Fatalf("parallel response order = %v, want [call_a call_b]; output=%s", responseIDs, out)
	}
	if textIndex < 0 || imageAIndex < 0 || imageBIndex < 0 {
		t.Fatalf("text or image-bearing response missing; output=%s", out)
	}
}

func TestConvertOpenAIResponsesRequestToAntigravityAcceptsClaudeStyleToolImage(t *testing.T) {
	input := []byte(`{
		"input":[
			{"type":"function_call","call_id":"call_image","name":"read_image","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_image","output":{
				"type":"image","source":{"type":"base64","media_type":"image/webp","data":"Q0M="}
			}}
		]
	}`)

	out := convertOpenAIResponsesRequestToAntigravity("gemini-3-flash", input, false)
	response := antigravityResponsesByCallID(out)["call_image"]
	if got := response.Get("parts.0.inlineData.mimeType").String(); got != "image/webp" {
		t.Fatalf("Claude-style image MIME = %q, want image/webp; output=%s", got, out)
	}
	if got := response.Get("parts.0.inlineData.data").String(); got != "Q0M=" {
		t.Fatalf("Claude-style image data = %q; output=%s", got, out)
	}
}

func TestConvertOpenAIResponsesRequestToAntigravityRejectsInvalidToolImageDataURLs(t *testing.T) {
	input := []byte(`{
		"input":[
			{"type":"function_call","call_id":"call_wrong_mime","name":"wrong_mime","arguments":"{}"},
			{"type":"function_call","call_id":"call_not_base64","name":"not_base64","arguments":"{}"},
			{"type":"function_call","call_id":"call_remote","name":"remote_image","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_wrong_mime","output":[
				{"type":"input_text","text":"keep"},
				{"type":"input_image","image_url":"data:video/mp4;base64,VklERU8="}
			]},
			{"type":"function_call_output","call_id":"call_not_base64","output":[
				{"type":"input_text","text":"keep"},
				{"type":"input_image","image_url":"data:image/png,SU1BR0U="}
			]},
			{"type":"function_call_output","call_id":"call_remote","output":[
				{"type":"input_text","text":"keep"},
				{"type":"input_image","image_url":"https://example.com/image.png"}
			]}
		]
	}`)

	out := convertOpenAIResponsesRequestToAntigravity("gemini-3-flash", input, false)
	responses := antigravityResponsesByCallID(out)
	for _, callID := range []string{"call_wrong_mime", "call_not_base64", "call_remote"} {
		response := responses[callID]
		if response.Get("parts").Exists() {
			t.Fatalf("invalid image for %s became functionResponse.parts; output=%s", callID, out)
		}
		if result := response.Get("response.result"); !result.IsArray() || len(result.Array()) != 2 {
			t.Fatalf("invalid image for %s was not preserved as structured output: %s; output=%s", callID, result.Raw, out)
		}
	}
}

func TestConvertOpenAIResponsesRequestToAntigravityPreservesMidSessionDeveloperAndDelayedResults(t *testing.T) {
	input := []byte(`{
		"instructions":"global instruction",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"run tools"}]},
			{"type":"function_call","call_id":"call_a","name":"tool_a","arguments":"{}"},
			{"type":"function_call","call_id":"call_b","name":"tool_b","arguments":"{}"},
			{"type":"message","role":"developer","content":"permission notice"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"keep user text"}]},
			{"type":"function_call_output","call_id":"call_b","output":"B"},
			{"type":"function_call_output","call_id":"call_a","output":"A"}
		]
	}`)

	out := convertOpenAIResponsesRequestToAntigravity("gemini-3-flash", input, false)
	if got := gjson.GetBytes(out, "request.systemInstruction.parts.0.text").String(); got != "global instruction" {
		t.Fatalf("system instruction = %q; output=%s", got, out)
	}
	if strings.Contains(gjson.GetBytes(out, "request.systemInstruction").Raw, "permission notice") {
		t.Fatalf("mid-session developer message was promoted globally: %s", out)
	}

	parts := antigravityResponsesUserParts(out)
	var texts []string
	var responseIDs []string
	for _, part := range parts {
		if text := part.Get("text"); text.Exists() {
			texts = append(texts, text.String())
		}
		if response := part.Get("functionResponse"); response.Exists() {
			responseIDs = append(responseIDs, response.Get("id").String())
		}
	}
	if len(texts) < 3 || texts[len(texts)-2] != "<system-reminder>\npermission notice\n</system-reminder>" || texts[len(texts)-1] != "keep user text" {
		t.Fatalf("developer/user text order changed: %v; output=%s", texts, out)
	}
	if len(responseIDs) != 2 || responseIDs[0] != "call_a" || responseIDs[1] != "call_b" {
		t.Fatalf("delayed result order = %v, want [call_a call_b]; output=%s", responseIDs, out)
	}
}

func TestConvertOpenAIResponsesRequestToAntigravityKeepsLastDuplicateResultAtFirstSlot(t *testing.T) {
	input := []byte(`{
		"input":[
			{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"old"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"between"}]},
			{"type":"function_call_output","call_id":"call_1","output":"new"}
		]
	}`)

	out := convertOpenAIResponsesRequestToAntigravity("gemini-3-flash", input, false)
	responses := antigravityResponsesByCallID(out)
	if got := responses["call_1"].Get("response.result").String(); got != "new" {
		t.Fatalf("deduplicated result = %q, want new; output=%s", got, out)
	}
	responseCount := 0
	for _, content := range gjson.GetBytes(out, "request.contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if part.Get("functionResponse.id").String() == "call_1" {
				responseCount++
			}
		}
	}
	if responseCount != 1 {
		t.Fatalf("functionResponse count = %d, want 1; output=%s", responseCount, out)
	}
}

func TestConvertOpenAIResponsesRequestToAntigravityPreservesExplicitSummaryIntent(t *testing.T) {
	tests := []struct {
		name       string
		reasoning  string
		wantExists bool
		want       bool
	}{
		{name: "effort alone does not enable summary", reasoning: `{"effort":"high"}`},
		{name: "explicit disabled", reasoning: `{"effort":"high","summary":"none"}`, wantExists: true},
		{name: "explicit enabled", reasoning: `{"summary":"auto"}`, wantExists: true, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := []byte(`{"input":"hello","reasoning":` + test.reasoning + `}`)
			out := convertOpenAIResponsesRequestToAntigravity("gemini-3-flash", input, false)
			summary := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts")
			if summary.Exists() != test.wantExists {
				t.Fatalf("includeThoughts exists = %v, want %v; output=%s", summary.Exists(), test.wantExists, out)
			}
			if test.wantExists && summary.Bool() != test.want {
				t.Fatalf("includeThoughts = %s, want %v; output=%s", summary.Raw, test.want, out)
			}
		})
	}
}

func TestConvertOpenAIResponsesRequestToAntigravityMapsExternalToolNameChain(t *testing.T) {
	input := []byte(`{
		"tools":[{"type":"function","name":"external_external_read_file","parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","name":"external_external_read_file"},
		"input":[
			{"type":"function_call","call_id":"call_1","name":"external_external_read_file","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`)

	out := convertOpenAIResponsesRequestToAntigravity("gemini-3-flash", input, false)
	const expected = "external_external_external_read_file"
	for _, path := range []string{
		"request.tools.0.functionDeclarations.0.name",
		"request.toolConfig.functionCallingConfig.allowedFunctionNames.0",
		"request.contents.0.parts.0.functionCall.name",
		"request.contents.1.parts.0.functionResponse.name",
	} {
		if got := gjson.GetBytes(out, path).String(); got != expected {
			t.Fatalf("%s = %q, want %q; output=%s", path, got, expected, out)
		}
	}
}

func antigravityResponsesByCallID(output []byte) map[string]gjson.Result {
	responses := make(map[string]gjson.Result)
	for _, content := range gjson.GetBytes(output, "request.contents").Array() {
		for _, part := range content.Get("parts").Array() {
			if response := part.Get("functionResponse"); response.Exists() {
				responses[response.Get("id").String()] = response
			}
		}
	}
	return responses
}

func antigravityResponsesUserParts(output []byte) []gjson.Result {
	var parts []gjson.Result
	for _, content := range gjson.GetBytes(output, "request.contents").Array() {
		if content.Get("role").String() != "user" {
			continue
		}
		parts = append(parts, content.Get("parts").Array()...)
	}
	return parts
}
