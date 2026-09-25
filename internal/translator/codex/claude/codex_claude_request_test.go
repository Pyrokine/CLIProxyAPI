package claude

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToCodex_SystemMessageScenarios(t *testing.T) {
	tests := []struct {
		name             string
		inputJSON        string
		wantHasDeveloper bool
		wantTexts        []string
	}{
		{
			name: "No system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasDeveloper: false,
		},
		{
			name: "Empty string system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": "",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasDeveloper: false,
		},
		{
			name: "String system field",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": "Be helpful",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasDeveloper: true,
			wantTexts:        []string{"Be helpful"},
		},
		{
			name: "System role in messages",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [
					{"role": "system", "content": "Follow the project instructions"},
					{"role": "user", "content": "hello"}
				]
			}`,
			wantHasDeveloper: false,
			wantTexts:        []string{"<system-reminder>\nFollow the project instructions\n</system-reminder>"},
		},
		{
			name: "Array system field with filtered billing header",
			inputJSON: `{
				"model": "claude-3-opus",
				"system": [
					{"type": "text", "text": "x-anthropic-billing-header: tenant-123"},
					{"type": "text", "text": "Block 1"},
					{"type": "text", "text": "Block 2"}
				],
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantHasDeveloper: true,
			wantTexts:        []string{"Block 1", "Block 2"},
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				result := ConvertClaudeRequestToCodex("test-model", []byte(tt.inputJSON), false)
				resultJSON := gjson.ParseBytes(result)
				inputs := resultJSON.Get("input").Array()

				hasDeveloper := len(inputs) > 0 && inputs[0].Get("role").String() == "developer"
				if hasDeveloper != tt.wantHasDeveloper {
					t.Fatalf(
						"got hasDeveloper = %v, want %v. Output: %s", hasDeveloper, tt.wantHasDeveloper,
						resultJSON.Get("input").Raw,
					)
				}

				if !tt.wantHasDeveloper && len(tt.wantTexts) == 0 {
					return
				}

				content := inputs[0].Get("content").Array()
				if len(content) != len(tt.wantTexts) {
					t.Fatalf(
						"got %d system content items, want %d. Content: %s", len(content), len(tt.wantTexts),
						inputs[0].Get("content").Raw,
					)
				}

				for i, wantText := range tt.wantTexts {
					if gotType := content[i].Get("type").String(); gotType != "input_text" {
						t.Fatalf("content[%d] type = %q, want %q", i, gotType, "input_text")
					}
					if gotText := content[i].Get("text").String(); gotText != wantText {
						t.Fatalf("content[%d] text = %q, want %q", i, gotText, wantText)
					}
				}
			},
		)
	}
}

func TestConvertClaudeRequestToCodex_AssistantThinkingSignatureBecomesReasoningInput(t *testing.T) {
	const signature = "gAAAAAAAAAAAERERERERERERERERERERESIiIiIiIiIiIiIiIiIiIiIzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMw"
	inputJSON := `{
		"model": "gpt-5.5",
		"messages": [{
			"role": "assistant",
			"content": [
				{"type": "thinking", "thinking": "previous reasoning", "signature": "` + signature + `"},
				{"type": "text", "text": "answer"}
			]
		}]
	}`

	result := ConvertClaudeRequestToCodex("gpt-5.5", []byte(inputJSON), false)
	items := gjson.GetBytes(result, "input").Array()
	if len(items) != 2 {
		t.Fatalf("got %d input items, want 2: %s", len(items), result)
	}
	if got := items[0].Get("type").String(); got != "reasoning" {
		t.Fatalf("input[0].type = %q, want reasoning: %s", got, result)
	}
	if got := items[0].Get("encrypted_content").String(); got != signature {
		t.Fatalf("encrypted_content = %q, want %q: %s", got, signature, result)
	}
	if got := items[1].Get("content.0.type").String(); got != "output_text" {
		t.Fatalf("assistant text type = %q, want output_text: %s", got, result)
	}
}

func TestConvertClaudeRequestToCodex_DropsInvalidThinkingSignature(t *testing.T) {
	inputJSON := `{
		"model": "gpt-5.5",
		"messages": [{
			"role": "assistant",
			"content": [
				{"type": "thinking", "thinking": "bad reasoning", "signature": ""},
				{"type": "text", "text": "answer"}
			]
		}]
	}`

	result := ConvertClaudeRequestToCodex("gpt-5.5", []byte(inputJSON), false)
	items := gjson.GetBytes(result, "input").Array()
	if len(items) != 1 {
		t.Fatalf("got %d input items, want 1: %s", len(items), result)
	}
	if got := items[0].Get("type").String(); got == "reasoning" {
		t.Fatalf("invalid thinking signature produced reasoning input: %s", result)
	}
}

func TestConvertClaudeRequestToCodex_ParallelToolCalls(t *testing.T) {
	tests := []struct {
		name                  string
		inputJSON             string
		wantParallelToolCalls bool
	}{
		{
			name: "Default to true when tool_choice.disable_parallel_tool_use is absent",
			inputJSON: `{
				"model": "claude-3-opus",
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantParallelToolCalls: true,
		},
		{
			name: "Disable parallel tool calls when client opts out",
			inputJSON: `{
				"model": "claude-3-opus",
				"tool_choice": {"disable_parallel_tool_use": true},
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantParallelToolCalls: false,
		},
		{
			name: "Keep parallel tool calls enabled when client explicitly allows them",
			inputJSON: `{
				"model": "claude-3-opus",
				"tool_choice": {"disable_parallel_tool_use": false},
				"messages": [{"role": "user", "content": "hello"}]
			}`,
			wantParallelToolCalls: true,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				result := ConvertClaudeRequestToCodex("test-model", []byte(tt.inputJSON), false)
				resultJSON := gjson.ParseBytes(result)

				if got := resultJSON.Get("parallel_tool_calls").Bool(); got != tt.wantParallelToolCalls {
					t.Fatalf(
						"parallel_tool_calls = %v, want %v. Output: %s", got, tt.wantParallelToolCalls, string(result),
					)
				}
			},
		)
	}
}

func TestConvertClaudeRequestToCodex_ServiceTier(t *testing.T) {
	tests := []struct {
		name        string
		serviceTier any
		speed       any
		want        string
		wantExists  bool
	}{
		{name: "priority passes through", serviceTier: "priority", want: "priority", wantExists: true},
		{name: "fast tier normalizes to priority", serviceTier: "fast", want: "priority", wantExists: true},
		{name: "unsupported tier is omitted", serviceTier: "default"},
		{name: "non-string tier is omitted", serviceTier: true},
		{name: "fast speed maps to priority", speed: "fast", want: "priority", wantExists: true},
		{name: "standard speed is omitted", speed: "standard"},
		{name: "non-string speed is omitted", speed: true},
		{
			name:        "fast speed overrides unsupported Anthropic tier",
			serviceTier: "auto",
			speed:       "fast",
			want:        "priority",
			wantExists:  true,
		},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				request := map[string]any{
					"model":    "gpt-5.4",
					"messages": []map[string]any{{"role": "user", "content": "Reply with OK"}},
				}
				if test.serviceTier != nil {
					request["service_tier"] = test.serviceTier
				}
				if test.speed != nil {
					request["speed"] = test.speed
				}
				inputJSON, errMarshal := json.Marshal(request)
				if errMarshal != nil {
					t.Fatalf("json.Marshal() error = %v", errMarshal)
				}

				result := ConvertClaudeRequestToCodex("gpt-5.4", inputJSON, false)
				serviceTierResult := gjson.GetBytes(result, "service_tier")
				if serviceTierResult.Exists() != test.wantExists {
					t.Fatalf(
						"service_tier exists = %v, want %v; output=%s", serviceTierResult.Exists(), test.wantExists,
						result,
					)
				}
				if test.wantExists && serviceTierResult.String() != test.want {
					t.Fatalf("service_tier = %q, want %q; output=%s", serviceTierResult.String(), test.want, result)
				}
			},
		)
	}
}

func TestConvertClaudeRequestToCodex_EscapesOutputStringsAsJSON(t *testing.T) {
	const modelName = "gpt-\x01-model"
	const inputJSON = "{\"messages\":[],\"thinking\":{\"type\":\"adaptive\"}," +
		"\"output_config\":{\"effort\":\"high\\u0001low\"}}"

	result := ConvertClaudeRequestToCodex(modelName, []byte(inputJSON), false)
	if !gjson.ValidBytes(result) {
		t.Fatalf("result is invalid JSON: %q", result)
	}
	if got := gjson.GetBytes(result, "model").String(); got != modelName {
		t.Fatalf("model = %q, want %q", got, modelName)
	}
	if got, want := gjson.GetBytes(result, "reasoning.effort").String(), "high\x01low"; got != want {
		t.Fatalf("reasoning.effort = %q, want %q", got, want)
	}
}

func TestConvertClaudeRequestToCodex_PreservesMixedRequestSemantics(t *testing.T) {
	const signature = "gAAAAAAAAAAAERERERERERERERERERERESIiIiIiIiIiIiIiIiIiIiIzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMw"
	inputJSON := `{
		"system": [
			{"type": "text", "text": "x-anthropic-billing-header: tenant-123"},
			{"type": "text", "text": "developer instructions"}
		],
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "hello"},
				{"type": "image", "source": {"media_type": "image/png", "data": "aW1hZ2U="}}
			]},
			{"role": "assistant", "content": [
				{"type": "text", "text": "before reasoning"},
				{"type": "thinking", "thinking": "hidden", "signature": "` + signature + `"},
				{"type": "tool_use", "id": "call-1", "name": "lookup", "input": {"query": "weather"}},
				{"type": "text", "text": "after tool"}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "call-1", "content": [
					{"type": "text", "text": "sunny"},
					{"type": "image", "source": {"mime_type": "image/jpeg", "base64": "cmVzdWx0"}}
				]},
				{"type": "text", "text": "thanks"}
			]},
			{"role": "system", "content": "runtime rule"}
		],
		"tools": [
			{
				"name": "lookup",
				"description": "Look up data",
				"input_schema": {
					"$schema": "https://json-schema.org/draft/2020-12/schema",
					"type": "object",
					"properties": {"query": {"type": "string"}}
				},
				"cache_control": {"type": "ephemeral"},
				"defer_loading": true
			},
			{"type": "web_search_20250305", "name": "web_search"}
		],
		"tool_choice": {"disable_parallel_tool_use": true},
		"thinking": {"type": "adaptive"},
		"output_config": {"effort": "HIGH"}
	}`

	result := ConvertClaudeRequestToCodex("gpt-5.5", []byte(inputJSON), false)
	if !gjson.ValidBytes(result) {
		t.Fatalf("result is invalid JSON: %s", result)
	}

	resultJSON := gjson.ParseBytes(result)
	if got := resultJSON.Get("model").String(); got != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5", got)
	}
	if got := resultJSON.Get("parallel_tool_calls").Bool(); got {
		t.Fatal("parallel_tool_calls = true, want false")
	}
	if got := resultJSON.Get("reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want high", got)
	}

	items := resultJSON.Get("input").Array()
	if len(items) != 9 {
		t.Fatalf("got %d input items, want 9: %s", len(items), result)
	}
	checks := []struct {
		path string
		want string
	}{
		{"0.role", "developer"},
		{"0.content.0.text", "developer instructions"},
		{"1.content.0.text", "hello"},
		{"1.content.1.image_url", "data:image/png;base64,aW1hZ2U="},
		{"2.content.0.text", "before reasoning"},
		{"3.type", "reasoning"},
		{"3.encrypted_content", signature},
		{"4.type", "function_call"},
		{"4.name", "lookup"},
		{"4.arguments", `{"query": "weather"}`},
		{"5.content.0.text", "after tool"},
		{"6.type", "function_call_output"},
		{"6.output.0.text", "sunny"},
		{"6.output.1.image_url", "data:image/jpeg;base64,cmVzdWx0"},
		{"7.content.0.text", "thanks"},
		{"8.content.0.text", "<system-reminder>\nruntime rule\n</system-reminder>"},
	}
	for _, check := range checks {
		if got := resultJSON.Get("input." + check.path).String(); got != check.want {
			t.Fatalf("input.%s = %q, want %q: %s", check.path, got, check.want, result)
		}
	}

	tools := resultJSON.Get("tools").Array()
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2: %s", len(tools), result)
	}
	if got := tools[0].Get("type").String(); got != "function" {
		t.Fatalf("tools[0].type = %q, want function", got)
	}
	if got := tools[0].Get("parameters.properties.query.type").String(); got != "string" {
		t.Fatalf("tool parameter type = %q, want string", got)
	}
	for _, path := range []string{"input_schema", "parameters.$schema", "cache_control", "defer_loading"} {
		if tools[0].Get(path).Exists() {
			t.Fatalf("tools[0].%s should be removed: %s", path, tools[0].Raw)
		}
	}
	if got := tools[1].Get("type").String(); got != "web_search" {
		t.Fatalf("tools[1].type = %q, want web_search", got)
	}
}

func TestConvertClaudeRequestToCodexOutputConfigFormat(t *testing.T) {
	tests := []struct {
		name       string
		format     string
		wantFormat bool
		wantName   string
		wantStrict bool
	}{
		{
			name: "complete nested required schema remains strict",
			format: `{"type":"json_schema","name":"answer","schema":{"$schema":"draft","$id":"root","type":"object","properties":{` +
				`"answer":{"$id":"field","type":"string","pattern":"^\\p{L}+$"},` +
				`"meta":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}` +
				`},"required":["answer","meta"]}}`,
			wantFormat: true,
			wantName:   "answer",
			wantStrict: true,
		},
		{
			name: "nested missing required downgrades default strict",
			format: `{"type":"json_schema","schema":{"type":"object","properties":{` +
				`"answer":{"type":"string"},` +
				`"meta":{"type":"object","properties":{"id":{"type":"string"}}}` +
				`},"required":["answer","meta"]}}`,
			wantFormat: true,
			wantName:   "cli_proxy_structured_output",
			wantStrict: false,
		},
		{
			name:       "missing required downgrades default strict",
			format:     `{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"string"}}}}`,
			wantFormat: true,
			wantName:   "cli_proxy_structured_output",
			wantStrict: false,
		},
		{
			name:       "explicit non-strict remains false",
			format:     `{"type":"json_schema","strict":false,"schema":{"type":"object","properties":{}}}`,
			wantFormat: true,
			wantName:   "cli_proxy_structured_output",
			wantStrict: false,
		},
		{name: "missing schema is ignored", format: `{"type":"json_schema","name":"answer"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := []byte(`{"messages":[],"output_config":{"format":` + test.format + `}}`)
			output := ConvertClaudeRequestToCodex("gpt-5.6", input, false)
			format := gjson.GetBytes(output, "text.format")
			if format.Exists() != test.wantFormat {
				t.Fatalf("text.format exists = %v, want %v; output=%s", format.Exists(), test.wantFormat, output)
			}
			if !test.wantFormat {
				return
			}
			if got := format.Get("name").String(); got != test.wantName {
				t.Fatalf("name = %q, want %q; output=%s", got, test.wantName, output)
			}
			if strict := format.Get("strict"); !strict.Exists() || strict.Bool() != test.wantStrict {
				t.Fatalf("strict = %s, want %v; output=%s", strict.Raw, test.wantStrict, output)
			}
			for _, path := range []string{
				"schema.$schema",
				"schema.$id",
				"schema.properties.answer.$id",
				"schema.properties.answer.pattern",
			} {
				if format.Get(path).Exists() {
					t.Fatalf("%s should be removed: %s", path, output)
				}
			}
		})
	}
}

func TestConvertClaudeRequestToCodexDoesNotInferReasoningSummary(t *testing.T) {
	input := []byte(`{"messages":[],"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`)
	output := ConvertClaudeRequestToCodex("gpt-5.6", input, false)
	if got := gjson.GetBytes(output, "reasoning.effort").String(); got != "high" {
		t.Fatalf("reasoning.effort = %q, want high; output=%s", got, output)
	}
	if summary := gjson.GetBytes(output, "reasoning.summary"); summary.Exists() {
		t.Fatalf("reasoning.summary was inferred from effort: %s", output)
	}
}

func TestConvertClaudeRequestToCodexNormalizesToolInputSchemas(t *testing.T) {
	input := []byte(`{
		"messages":[],
		"tools":[
			{"name":"default_schema"},
			{"name":"normalized_schema","input_schema":{
				"$schema":"draft-07",
				"type":"object",
				"properties":{"query":{"$id":"query","type":"string","pattern":"^\\P{N}+$"}}
			}}
		]
	}`)

	output := ConvertClaudeRequestToCodex("gpt-5.6", input, false)
	parameters := gjson.GetBytes(output, "tools.0.parameters")
	if parameters.Get("type").String() != "object" || !parameters.Get("properties").IsObject() {
		t.Fatalf("default input_schema = %s, want object with properties; output=%s", parameters.Raw, output)
	}
	for _, path := range []string{
		"tools.1.parameters.$schema",
		"tools.1.parameters.properties.query.$id",
		"tools.1.parameters.properties.query.pattern",
	} {
		if gjson.GetBytes(output, path).Exists() {
			t.Fatalf("%s should be removed: %s", path, output)
		}
	}
	for index := range 2 {
		if strict := gjson.GetBytes(output, fmt.Sprintf("tools.%d.strict", index)); strict.Type != gjson.False {
			t.Fatalf("tools.%d.strict = %s, want false; output=%s", index, strict.Raw, output)
		}
	}
}

func TestConvertClaudeRequestToCodexPreservesBase64PDFDocumentContent(t *testing.T) {
	inputJSON := `{
		"messages":[{
			"role":"user",
			"content":[
				{"type":"text","text":"before"},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQK"}},
				{"type":"text","text":"after"}
			]
		}]
	}`

	result := ConvertClaudeRequestToCodex("gpt-5.6-sol", []byte(inputJSON), false)
	content := gjson.GetBytes(result, "input.0.content").Array()
	if len(content) != 3 {
		t.Fatalf("got %d content items, want 3. Output: %s", len(content), result)
	}

	wantTypes := []string{"input_text", "input_file", "input_text"}
	for i, wantType := range wantTypes {
		if got := content[i].Get("type").String(); got != wantType {
			t.Fatalf("content[%d].type = %q, want %q. Output: %s", i, got, wantType, result)
		}
	}
	if got := content[0].Get("text").String(); got != "before" {
		t.Fatalf("content[0].text = %q, want before", got)
	}
	if got := content[1].Get("file_data").String(); got != "data:application/pdf;base64,JVBERi0xLjQK" {
		t.Fatalf("content[1].file_data = %q, want PDF data URL", got)
	}
	if got := content[1].Get("filename").String(); got != "document.pdf" {
		t.Fatalf("content[1].filename = %q, want document.pdf", got)
	}
	if got := content[2].Get("text").String(); got != "after" {
		t.Fatalf("content[2].text = %q, want after", got)
	}
}

func TestConvertClaudeRequestToCodexIgnoresUnsupportedDocumentContent(t *testing.T) {
	inputJSON := `{
		"messages":[{
			"role":"user",
			"content":[
				{"type":"text","text":"keep"},
				{"type":"document","source":{"type":"url","media_type":"application/pdf","data":"ignored"}},
				{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"ignored"}}
			]
		}]
	}`

	result := ConvertClaudeRequestToCodex("gpt-5.6-sol", []byte(inputJSON), false)
	content := gjson.GetBytes(result, "input.0.content").Array()
	if len(content) != 1 || content[0].Get("text").String() != "keep" {
		t.Fatalf("unsupported documents should be omitted: %s", result)
	}
}

func TestConvertClaudeRequestToCodex_LargeRequestKeepsToolNamesConsistent(t *testing.T) {
	const toolCount = 32
	const messagePairs = 64
	request := buildLargeClaudeRequest(messagePairs, toolCount)
	result := ConvertClaudeRequestToCodex("gpt-5.5", request, false)
	if !gjson.ValidBytes(result) {
		t.Fatal("result is invalid JSON")
	}

	tools := gjson.GetBytes(result, "tools").Array()
	if len(tools) != toolCount {
		t.Fatalf("got %d tools, want %d", len(tools), toolCount)
	}
	shortNames := make([]string, toolCount)
	seen := make(map[string]struct{}, toolCount)
	for i, tool := range tools {
		shortNames[i] = tool.Get("name").String()
		if len(shortNames[i]) > 64 {
			t.Fatalf("tools[%d].name length = %d, want <= 64", i, len(shortNames[i]))
		}
		if _, ok := seen[shortNames[i]]; ok {
			t.Fatalf("duplicate shortened tool name %q", shortNames[i])
		}
		seen[shortNames[i]] = struct{}{}
	}

	items := gjson.GetBytes(result, "input").Array()
	if want := 1 + messagePairs*3; len(items) != want {
		t.Fatalf("got %d input items, want %d", len(items), want)
	}
	for i := range messagePairs {
		call := items[2+i*3]
		if got, want := call.Get("name").String(), shortNames[i%toolCount]; got != want {
			t.Fatalf("function call %d name = %q, want %q", i, got, want)
		}
		if got, want := call.Get("call_id").String(), fmt.Sprintf("call-%d", i); got != want {
			t.Fatalf("function call %d call_id = %q, want %q", i, got, want)
		}
	}
}

// noinspection GoUnusedGlobalVariable
var benchmarkClaudeRequestOutput []byte

func BenchmarkConvertClaudeRequestToCodex_LargeRequest(b *testing.B) {
	for _, messagePairs := range []int{100, 500} {
		request := buildLargeClaudeRequest(messagePairs, 64)
		b.Run(
			fmt.Sprintf("message_pairs=%d", messagePairs), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(request)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					benchmarkClaudeRequestOutput = ConvertClaudeRequestToCodex("gpt-5.5", request, false)
				}
			},
		)
	}
}

func buildLargeClaudeRequest(messagePairs, toolCount int) []byte {
	toolNames := make([]string, toolCount)
	tools := make([]map[string]any, 0, toolCount)
	for i := range toolCount {
		name := fmt.Sprintf("mcp__server_%03d__%s", i, strings.Repeat("shared_tool_name_", 5))
		toolNames[i] = name
		tools = append(
			tools, map[string]any{
				"name":        name,
				"description": "A tool used by the large request benchmark",
				"input_schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"value": map[string]any{"type": "string"},
					},
				},
			},
		)
	}

	messages := make([]map[string]any, 0, messagePairs*2)
	text := strings.Repeat("request payload ", 32)
	for i := range messagePairs {
		toolName := toolNames[i%toolCount]
		messages = append(
			messages,
			map[string]any{
				"role": "assistant",
				"content": []map[string]any{
					{"type": "text", "text": text},
					{
						"type":  "tool_use",
						"id":    fmt.Sprintf("call-%d", i),
						"name":  toolName,
						"input": map[string]any{"value": fmt.Sprintf("value-%d", i)},
					},
				},
			},
			map[string]any{
				"role": "user",
				"content": []map[string]any{
					{
						"type":        "tool_result",
						"tool_use_id": fmt.Sprintf("call-%d", i),
						"content":     text,
					},
				},
			},
		)
	}

	request := map[string]any{
		"system":   "large request benchmark",
		"messages": messages,
		"tools":    tools,
	}
	data, err := json.Marshal(request)
	if err != nil {
		panic(err)
	}
	return data
}
