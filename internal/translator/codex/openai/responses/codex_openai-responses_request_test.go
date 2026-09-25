package responses

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// noinspection GoUnusedGlobalVariable
var benchmarkConvertNormalizedOutput []byte

// TestConvertSystemRoleToDeveloper_BasicConversion tests the basic system -> developer role conversion
func TestConvertSystemRoleToDeveloper_BasicConversion(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are a pirate."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Say hello."}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that system role was converted to developer
	firstItemRole := gjson.Get(outputStr, "input.0.role")
	if firstItemRole.String() != "developer" {
		t.Errorf("Expected role 'developer', got '%s'", firstItemRole.String())
	}

	// Check that user role remains unchanged
	secondItemRole := gjson.Get(outputStr, "input.1.role")
	if secondItemRole.String() != "user" {
		t.Errorf("Expected role 'user', got '%s'", secondItemRole.String())
	}

	// Check content is preserved
	firstItemContent := gjson.Get(outputStr, "input.0.content.0.text")
	if firstItemContent.String() != "You are a pirate." {
		t.Errorf("Expected content 'You are a pirate.', got '%s'", firstItemContent.String())
	}
}

// TestConvertSystemRoleToDeveloper_MultipleSystemMessages tests conversion with multiple system messages
func TestConvertSystemRoleToDeveloper_MultipleSystemMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are helpful."}]
			},
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "Be concise."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that both system roles were converted
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected first role 'developer', got '%s'", firstRole.String())
	}

	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "developer" {
		t.Errorf("Expected second role 'developer', got '%s'", secondRole.String())
	}

	// Check that user role is unchanged
	thirdRole := gjson.Get(outputStr, "input.2.role")
	if thirdRole.String() != "user" {
		t.Errorf("Expected third role 'user', got '%s'", thirdRole.String())
	}
}

// TestConvertSystemRoleToDeveloper_NoSystemMessages tests that requests without system messages are unchanged
func TestConvertSystemRoleToDeveloper_NoSystemMessages(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hi there!"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that user and assistant roles are unchanged
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "user" {
		t.Errorf("Expected role 'user', got '%s'", firstRole.String())
	}

	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "assistant" {
		t.Errorf("Expected role 'assistant', got '%s'", secondRole.String())
	}
}

// TestConvertSystemRoleToDeveloper_EmptyInput tests that empty input arrays are handled correctly
func TestConvertSystemRoleToDeveloper_EmptyInput(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": []
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that input is still an empty array
	inputArray := gjson.Get(outputStr, "input")
	if !inputArray.IsArray() {
		t.Error("Input should still be an array")
	}
	if len(inputArray.Array()) != 0 {
		t.Errorf("Expected empty array, got %d items", len(inputArray.Array()))
	}
}

// TestConvertSystemRoleToDeveloper_NoInputField tests that requests without input field are unchanged
func TestConvertSystemRoleToDeveloper_NoInputField(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"stream": false
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check that other fields are still set correctly
	stream := gjson.Get(outputStr, "stream")
	if !stream.Bool() {
		t.Error("Stream should be set to true by conversion")
	}

	store := gjson.Get(outputStr, "store")
	if store.Bool() {
		t.Error("Store should be set to false by conversion")
	}
}

// TestConvertOpenAIResponsesRequestToCodex_OriginalIssue tests the exact issue reported by the user
func TestConvertOpenAIResponsesRequestToCodex_OriginalIssue(t *testing.T) {
	// This is the exact input that was failing with "System messages are not allowed"
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": "You are a pirate. Always respond in pirate speak."
			},
			{
				"type": "message",
				"role": "user",
				"content": "Say hello."
			}
		],
		"stream": false
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Verify system role was converted to developer
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected role 'developer', got '%s'", firstRole.String())
	}

	// Verify stream was set to true (as required by Codex)
	stream := gjson.Get(outputStr, "stream")
	if !stream.Bool() {
		t.Error("Stream should be set to true")
	}

	// Verify other required fields for Codex
	store := gjson.Get(outputStr, "store")
	if store.Bool() {
		t.Error("Store should be false")
	}

	parallelCalls := gjson.Get(outputStr, "parallel_tool_calls")
	if !parallelCalls.Bool() {
		t.Error("parallel_tool_calls should be true")
	}

	include := gjson.Get(outputStr, "include")
	if !include.IsArray() || len(include.Array()) != 1 {
		t.Error("include should be an array with one element")
	} else if include.Array()[0].String() != "reasoning.encrypted_content" {
		t.Errorf("Expected include[0] to be 'reasoning.encrypted_content', got '%s'", include.Array()[0].String())
	}
}

// TestConvertSystemRoleToDeveloper_AssistantRole tests that assistant role is preserved
func TestConvertSystemRoleToDeveloper_AssistantRole(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"input": [
			{
				"type": "message",
				"role": "system",
				"content": [{"type": "input_text", "text": "You are helpful."}]
			},
			{
				"type": "message",
				"role": "user",
				"content": [{"type": "input_text", "text": "Hello"}]
			},
			{
				"type": "message",
				"role": "assistant",
				"content": [{"type": "output_text", "text": "Hi!"}]
			}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Check system -> developer
	firstRole := gjson.Get(outputStr, "input.0.role")
	if firstRole.String() != "developer" {
		t.Errorf("Expected first role 'developer', got '%s'", firstRole.String())
	}

	// Check user unchanged
	secondRole := gjson.Get(outputStr, "input.1.role")
	if secondRole.String() != "user" {
		t.Errorf("Expected second role 'user', got '%s'", secondRole.String())
	}

	// Check assistant unchanged
	thirdRole := gjson.Get(outputStr, "input.2.role")
	if thirdRole.String() != "assistant" {
		t.Errorf("Expected third role 'assistant', got '%s'", thirdRole.String())
	}
}

func TestUserFieldDeletion(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"user": "test-user",
		"input": [{"role": "user", "content": "Hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	// Verify user field is deleted
	userField := gjson.Get(outputStr, "user")
	if userField.Exists() {
		t.Errorf("user field should be deleted, but it was found with value: %s", userField.Raw)
	}
}

func TestContextManagementCompactionCompatibility(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"context_management": [
			{
				"type": "compaction",
				"compact_threshold": 12000
			}
		],
		"input": [{"role":"user","content":"hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	if gjson.Get(outputStr, "context_management").Exists() {
		t.Fatalf("context_management should be removed for Codex compatibility")
	}
	if gjson.Get(outputStr, "truncation").Exists() {
		t.Fatalf("truncation should be removed for Codex compatibility")
	}
}

func TestTruncationRemovedForCodexCompatibility(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"truncation": "disabled",
		"input": [{"role":"user","content":"hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	if gjson.Get(outputStr, "truncation").Exists() {
		t.Fatalf("truncation should be removed for Codex compatibility")
	}
}

func TestStreamOptionsRemovedForCodexCompatibility(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"stream_options": {"include_usage": true},
		"input": [{"role":"user","content":"hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	if gjson.Get(outputStr, "stream_options").Exists() {
		t.Fatalf("stream_options should be removed for Codex compatibility")
	}
}

func TestConvertOpenAIResponsesRequestToCodexStripsCacheFields(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.6",
		"prompt_cache_options":{"mode":"implicit"},
		"prompt_cache_retention":"24h",
		"input":[{
			"type":"message",
			"role":"user",
			"content":[
				{"type":"input_text","text":"hello","prompt_cache_breakpoint":true},
				{"type":"input_image","image_url":"data:image/png;base64,AA=="}
			]
		}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.6", inputJSON, true)
	for _, path := range []string{
		"prompt_cache_options",
		"prompt_cache_retention",
		"input.0.content.0.prompt_cache_breakpoint",
	} {
		if gjson.GetBytes(output, path).Exists() {
			t.Fatalf("%s should be removed: %s", path, output)
		}
	}
	if got := gjson.GetBytes(output, "input.0.content.0.text").String(); got != "hello" {
		t.Fatalf("input text = %q, want hello", got)
	}
	if got := gjson.GetBytes(output, "input.0.content.1.type").String(); got != "input_image" {
		t.Fatalf("second content type = %q, want input_image", got)
	}
}

func TestNormalizeCodexBuiltinTools(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gpt-5.2",
		"tools": [
			{"type":"web_search_preview"},
			{"type":"web_search_preview_2025_03_11"},
			{"type":"function","name":"do_work"}
		],
		"tool_choice": {
			"type": "web_search_preview",
			"tools": [{"type":"web_search_preview_2025_03_11"}]
		},
		"input": [{"role":"user","content":"hello"}]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.2", inputJSON, false)
	outputStr := string(output)

	for _, path := range []string{"tools.0.type", "tools.1.type", "tool_choice.type", "tool_choice.tools.0.type"} {
		if got := gjson.Get(outputStr, path).String(); got != "web_search" {
			t.Fatalf("%s = %q, want web_search; output: %s", path, got, outputStr)
		}
	}
	if got := gjson.Get(outputStr, "tools.2.type").String(); got != "function" {
		t.Fatalf("function tool type changed to %q", got)
	}
}

func TestConvertOpenAIResponsesRequestToCodexReusesNormalizedPayload(t *testing.T) {
	inputJSON := []byte(`{"model":"gpt-5.6","stream":true,"store":false,"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"service_tier":"priority","tools":[{"type":"function","name":"do_work"}],"tool_choice":{"type":"allowed_tools","tools":[{"type":"function","name":"do_work"}]},"input":[{"type":"message","role":"user","content":"hello"},{"type":"message","role":"assistant","content":"hi"}]}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.6", inputJSON, true)

	if len(inputJSON) == 0 || len(output) == 0 {
		t.Fatal("reuse fixture and output must be non-empty")
	}
	if !bytes.Equal(output, inputJSON) {
		t.Fatalf("normalized request changed:\n got: %s\nwant: %s", output, inputJSON)
	}
	if &output[0] != &inputJSON[0] {
		t.Fatal("returned payload does not reuse the input backing array")
	}
}

func TestConvertOpenAIResponsesRequestToCodexNormalizesRequiredFields(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.6",
		"stream":"true",
		"store":true,
		"parallel_tool_calls":false,
		"include":["file_search_call.results","reasoning.encrypted_content"],
		"max_output_tokens":4096,
		"max_completion_tokens":4096,
		"temperature":0.2,
		"top_p":0.9,
		"stream_options":{"include_usage":true,"reasoning_summary_delivery":"sequential_cutoff"},
		"service_tier":"standard",
		"truncation":"auto",
		"context_management":[{"type":"compaction","compact_threshold":12000}],
		"user":"request-owner",
		"tools":[
			{"type":"web_search_preview","search_context_size":"high"},
			{"type":"web_search_preview_2025_03_11","user_location":{"type":"approximate","country":"US"}},
			{"type":"function","name":"do_work","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"allowed_tools","tools":[
			{"type":"web_search_preview"},
			{"type":"web_search_preview_2025_03_11"}
		]},
		"input":[
			{"type":"message","role":"system","content":"first system"},
			{"type":"message","role":"user","content":"hello"},
			{"type":"message","role":"system","content":"second system"},
			{"type":"message","role":"assistant","content":"hi"}
		],
		"metadata":{"sentinel":"keep"}
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.6", inputJSON, true)

	if stream := gjson.GetBytes(output, "stream"); stream.Type != gjson.True {
		t.Fatalf("stream = %s, want true", stream.Raw)
	}
	if store := gjson.GetBytes(output, "store"); store.Type != gjson.False {
		t.Fatalf("store = %s, want false", store.Raw)
	}
	if parallel := gjson.GetBytes(output, "parallel_tool_calls"); parallel.Type != gjson.True {
		t.Fatalf("parallel_tool_calls = %s, want true", parallel.Raw)
	}
	include := gjson.GetBytes(output, "include").Array()
	if len(include) != 1 || include[0].Type != gjson.String ||
		include[0].String() != "reasoning.encrypted_content" {
		t.Fatalf(
			"include = %s, want reasoning.encrypted_content only",
			gjson.GetBytes(output, "include").Raw,
		)
	}
	wantRoles := []string{"developer", "user", "developer", "assistant"}
	wantContent := []string{"first system", "hello", "second system", "hi"}
	for index := range wantRoles {
		rolePath := fmt.Sprintf("input.%d.role", index)
		if got := gjson.GetBytes(output, rolePath).String(); got != wantRoles[index] {
			t.Fatalf("%s = %q, want %q", rolePath, got, wantRoles[index])
		}

		contentPath := fmt.Sprintf("input.%d.content", index)
		if got := gjson.GetBytes(output, contentPath).String(); got != wantContent[index] {
			t.Fatalf("%s = %q, want %q", contentPath, got, wantContent[index])
		}
	}

	for _, path := range []string{
		"tools.0.type",
		"tools.1.type",
		"tool_choice.tools.0.type",
		"tool_choice.tools.1.type",
	} {
		if got := gjson.GetBytes(output, path).String(); got != "web_search" {
			t.Fatalf("%s = %q, want web_search", path, got)
		}
	}
	if got := gjson.GetBytes(output, "tool_choice.type").String(); got != "allowed_tools" {
		t.Fatalf("tool_choice.type = %q, want allowed_tools", got)
	}
	if got := gjson.GetBytes(output, "tools.2.type").String(); got != "function" {
		t.Fatalf("tools.2.type = %q, want function", got)
	}
	if got := gjson.GetBytes(output, "tools.2.name").String(); got != "do_work" {
		t.Fatalf("tools.2.name = %q, want do_work", got)
	}
	if got := gjson.GetBytes(output, "tools.0.search_context_size").String(); got != "high" {
		t.Fatalf("tools.0.search_context_size = %q, want high", got)
	}
	if got := gjson.GetBytes(output, "tools.1.user_location.country").String(); got != "US" {
		t.Fatalf("tools.1.user_location.country = %q, want US", got)
	}
	if got := gjson.GetBytes(output, "metadata.sentinel").String(); got != "keep" {
		t.Fatalf("metadata.sentinel = %q, want keep", got)
	}

	for _, path := range []string{
		"max_output_tokens",
		"max_completion_tokens",
		"temperature",
		"top_p",
		"stream_options",
		"service_tier",
		"truncation",
		"context_management",
		"user",
	} {
		if gjson.GetBytes(output, path).Exists() {
			t.Fatalf("%s should be removed: %s", path, output)
		}
	}
}

func TestConvertOpenAIResponsesRequestToCodexNormalizesTopLevelToolChoiceAlias(t *testing.T) {
	inputJSON := []byte(`{"model":"gpt-5.6","stream":true,"store":false,"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"input":[{"type":"message","role":"user","content":"hello"}],"tool_choice":{"type":"web_search_preview_2025_03_11"}}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.6", inputJSON, true)

	if got := gjson.GetBytes(output, "tool_choice.type").String(); got != "web_search" {
		t.Fatalf("tool_choice.type = %q, want web_search: %s", got, output)
	}
}

func TestConvertOpenAIResponsesRequestToCodexPreservesConfigurationUpdates(t *testing.T) {
	input := []byte(`{
		"model":"gpt-5.6",
		"reasoning":{"effort":"xhigh"},
		"input":[
			{"type":"configuration_update","reasoning":{"effort":"low"}},
			{"type":"message","role":"system","content":"instructions"},
			{"type":"configuration_update","reasoning":{"effort":"medium"}},
			{"type":"configuration_update","tools":[]},
			{"type":"message","role":"user","content":"hello"}
		]
	}`)

	output := ConvertOpenAIResponsesRequestToCodex("gpt-5.6", input, true)
	if got := gjson.GetBytes(output, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("top-level reasoning.effort = %q, want xhigh; output=%s", got, output)
	}
	items := gjson.GetBytes(output, "input").Array()
	if len(items) != 5 {
		t.Fatalf("input items = %d, want 5; output=%s", len(items), output)
	}
	if got := items[0].Get("reasoning.effort").String(); got != "low" {
		t.Fatalf("first configuration_update effort = %q, want low", got)
	}
	if got := items[1].Get("role").String(); got != "developer" {
		t.Fatalf("system role = %q, want developer", got)
	}
	if got := items[2].Get("reasoning.effort").String(); got != "medium" {
		t.Fatalf("last valid configuration_update effort = %q, want medium", got)
	}
	if items[3].Get("type").String() != "configuration_update" || items[3].Get("reasoning.effort").Exists() {
		t.Fatalf("trailing configuration_update changed: %s", items[3].Raw)
	}
}

func BenchmarkConvertOpenAIResponsesRequestToCodexNormalizedPayload(b *testing.B) {
	cases := []struct {
		name      string
		inputJSON []byte
	}{
		{name: "1KiB", inputJSON: makeNormalizedResponsesRequestForBenchmark(1 << 10)},
		{name: "1MiB", inputJSON: makeNormalizedResponsesRequestForBenchmark(1 << 20)},
		{name: "8MiB", inputJSON: makeNormalizedResponsesRequestForBenchmark(8 << 20)},
	}

	for _, testCase := range cases {
		b.Run(
			testCase.name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(testCase.inputJSON)))
				b.ResetTimer()

				var output []byte
				for b.Loop() {
					output = ConvertOpenAIResponsesRequestToCodex(
						"gpt-5.6",
						testCase.inputJSON,
						true,
					)
				}
				benchmarkConvertNormalizedOutput = output
			},
		)
	}
}

func makeNormalizedResponsesRequestForBenchmark(contentBytes int) []byte {
	var builder strings.Builder
	builder.Grow(contentBytes + 256)
	builder.WriteString(`{"model":"gpt-5.6","stream":true,"store":false,"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"input":[{"type":"message","role":"user","content":"`)
	builder.WriteString(strings.Repeat("x", contentBytes))
	builder.WriteString(`"}]}`)
	return []byte(builder.String())
}
