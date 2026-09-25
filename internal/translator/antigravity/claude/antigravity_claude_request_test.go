package claude

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

func validClaudeThinkingSignatureForAntigravityTest() string {
	return claudeThinkingSignatureForAntigravityTest(12)
}

func claudeThinkingSignatureForAntigravityTest(channelID uint64) string {
	channel := protowire.AppendTag(nil, 1, protowire.VarintType)
	channel = protowire.AppendVarint(channel, channelID)
	channel = protowire.AppendTag(channel, 6, protowire.BytesType)
	channel = protowire.AppendBytes(channel, []byte("claude-sonnet-4-5-thinking-test-signature-payload"))
	container := protowire.AppendTag(nil, 1, protowire.BytesType)
	container = protowire.AppendBytes(container, channel)
	payload := protowire.AppendTag(nil, 2, protowire.BytesType)
	payload = protowire.AppendBytes(payload, container)
	return base64.StdEncoding.EncodeToString(payload)
}

func TestConvertClaudeRequestToAntigravity_BasicStructure(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "Hello"}
				]
			}
		],
		"system": [
			{"type": "text", "text": "You are helpful"}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	// Check model
	if gjson.Get(outputStr, "model").String() != "claude-sonnet-4-5" {
		t.Errorf("Expected model 'claude-sonnet-4-5', got '%s'", gjson.Get(outputStr, "model").String())
	}

	// Check contents exist
	contents := gjson.Get(outputStr, "request.contents")
	if !contents.Exists() || !contents.IsArray() {
		t.Error("request.contents should exist and be an array")
	}

	// Check role mapping (assistant -> model)
	firstContent := gjson.Get(outputStr, "request.contents.0")
	if firstContent.Get("role").String() != "user" {
		t.Errorf("Expected role 'user', got '%s'", firstContent.Get("role").String())
	}

	// Check systemInstruction
	sysInstruction := gjson.Get(outputStr, "request.systemInstruction")
	if !sysInstruction.Exists() {
		t.Error("systemInstruction should exist")
	}
	if sysInstruction.Get("parts.0.text").String() != "You are helpful" {
		t.Error("systemInstruction text mismatch")
	}
}

func TestConvertClaudeRequestToAntigravity_RoleMapping(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hi"}]},
			{"role": "assistant", "content": [{"type": "text", "text": "Hello"}]}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	// assistant should be mapped to model
	secondContent := gjson.Get(outputStr, "request.contents.1")
	if secondContent.Get("role").String() != "model" {
		t.Errorf("Expected role 'model' (mapped from 'assistant'), got '%s'", secondContent.Get("role").String())
	}
}

func TestConvertClaudeRequestToAntigravity_ThinkingBlocks(t *testing.T) {
	cache.ClearSignatureCache("")

	validSignature := validClaudeThinkingSignatureForAntigravityTest()
	thinkingText := "Let me think..."

	// Pre-cache the signature (simulating a previous response for the same thinking text)
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [
			{
				"role": "user",
				"content": [{"type": "text", "text": "Test user message"}]
			},
			{
				"role": "assistant",
				"content": [
					{"type": "thinking", "thinking": "` + thinkingText + `", "signature": "` + validSignature + `"},
					{"type": "text", "text": "Answer"}
				]
			}
		]
	}`)

	cache.StoreSignature("claude-sonnet-4-5-thinking", thinkingText, validSignature)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5-thinking", inputJSON, false)
	outputStr := string(output)

	// Check thinking block conversion (now in contents.1 due to user message)
	firstPart := gjson.Get(outputStr, "request.contents.1.parts.0")
	if !firstPart.Get("thought").Bool() {
		t.Error("thinking block should have thought: true")
	}
	if firstPart.Get("text").String() != thinkingText {
		t.Error("thinking text mismatch")
	}
	expectedSignature, compatible := signature.CompatibleAntigravityClaudeThinkingSignature(validSignature)
	if !compatible {
		t.Fatal("test signature should be valid for Antigravity Claude replay")
	}
	if firstPart.Get("thoughtSignature").String() != expectedSignature {
		t.Errorf(
			"Expected thoughtSignature '%s', got '%s'", expectedSignature, firstPart.Get("thoughtSignature").String(),
		)
	}
}

func TestConvertClaudeRequestToAntigravity_ReplacesClaudeSignatureForGeminiTarget(t *testing.T) {
	cache.ClearSignatureCache("")

	claudeSignature := validClaudeThinkingSignatureForAntigravityTest()
	inputJSON := []byte(`{
		"messages": [{
			"role": "assistant",
			"content": [
				{"type": "thinking", "thinking": "thinking text", "signature": "` + claudeSignature + `"},
				{"type": "tool_use", "id": "toolu_1", "name": "read", "input": {}}
			]
		}]
	}`)

	output := convertClaudeRequestToAntigravity("gemini-3-pro-preview", inputJSON, false)
	if got := gjson.GetBytes(
		output, "request.contents.0.parts.0.thoughtSignature",
	).String(); got != "" {
		t.Fatalf("thinking thoughtSignature = %q, want unsigned thought; output: %s", got, output)
	}
	if got := gjson.GetBytes(
		output, "request.contents.0.parts.1.thoughtSignature",
	).String(); got != signature.GeminiSkipThoughtSignatureValidator {
		t.Fatalf("tool thoughtSignature = %q, want Gemini skip sentinel; output: %s", got, output)
	}
}

func TestConvertClaudeRequestToAntigravity_DoesNotUseGeminiSentinelForClaudeTarget(t *testing.T) {
	cache.ClearSignatureCache("")

	inputJSON := []byte(`{
		"messages": [{
			"role": "assistant",
			"content": [
				{"type": "tool_use", "id": "toolu_1", "name": "read", "input": {}}
			]
		}]
	}`)

	output := convertClaudeRequestToAntigravity("claude-opus-4-7", inputJSON, false)
	if gjson.GetBytes(output, "request.contents.0.parts.0.thoughtSignature").Exists() {
		t.Fatalf("Claude target tool call should not receive Gemini sentinel; output: %s", output)
	}
}

func TestConvertClaudeRequestToAntigravity_ThinkingBlockWithoutSignature(t *testing.T) {
	cache.ClearSignatureCache("")

	// Unsigned thinking blocks should be removed entirely (not converted to text)
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "thinking", "thinking": "Let me think..."},
					{"type": "text", "text": "Answer"}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5-thinking", inputJSON, false)
	outputStr := string(output)

	// Without signature, thinking block should be removed (not converted to text)
	parts := gjson.Get(outputStr, "request.contents.0.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("Expected 1 part (thinking removed), got %d", len(parts))
	}

	// Only text part should remain
	if parts[0].Get("thought").Bool() {
		t.Error("Thinking block should be removed, not preserved")
	}
	if parts[0].Get("text").String() != "Answer" {
		t.Errorf("Expected text 'Answer', got '%s'", parts[0].Get("text").String())
	}
}

func TestConvertClaudeRequestToAntigravity_ToolDeclarations(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [],
		"tools": [
			{
				"name": "test_tool",
				"description": "A test tool",
				"input_schema": {
					"type": "object",
					"properties": {
						"name": {"type": "string"}
					},
					"required": ["name"]
				}
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("gemini-1.5-pro", inputJSON, false)
	outputStr := string(output)

	// Check tools structure
	tools := gjson.Get(outputStr, "request.tools")
	if !tools.Exists() {
		t.Error("Tools should exist in output")
	}

	funcDecl := gjson.Get(outputStr, "request.tools.0.functionDeclarations.0")
	if funcDecl.Get("name").String() != "test_tool" {
		t.Errorf("Expected tool name 'test_tool', got '%s'", funcDecl.Get("name").String())
	}

	// Check input_schema renamed to parametersJsonSchema
	if funcDecl.Get("parametersJsonSchema").Exists() {
		t.Log("parametersJsonSchema exists (expected)")
	}
	if funcDecl.Get("input_schema").Exists() {
		t.Error("input_schema should be removed")
	}
}

func TestConvertClaudeRequestToAntigravity_DeduplicatesAndDisambiguatesTools(t *testing.T) {
	first := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build"
	second := "mcp__plugin_cloudflare_cloudflare-builds__workers_builds_get_build_logs"
	inputJSON := []byte(`{
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"` + second + `","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}
		],
		"tools":[
			{"name":"lookup","input_schema":{"type":"object"}},
			{"name":"lookup","description":"duplicate","input_schema":{"type":"object"}},
			{"name":"` + first + `","input_schema":{"type":"object"}},
			{"name":"` + second + `","input_schema":{"type":"object"}}
		],
		"tool_choice":{"type":"tool","name":"` + second + `"}
	}`)

	out := convertClaudeRequestToAntigravity("gemini-3-flash", inputJSON, false)
	declarations := gjson.GetBytes(out, "request.tools.0.functionDeclarations").Array()
	if len(declarations) != 3 {
		t.Fatalf("declaration count = %d, want 3. Output: %s", len(declarations), out)
	}
	firstMapped := declarations[1].Get("name").String()
	secondMapped := declarations[2].Get("name").String()
	if firstMapped == secondMapped || len(secondMapped) > 64 {
		t.Fatalf("collision names = %q and %q, want distinct names <= 64 chars", firstMapped, secondMapped)
	}
	if got := gjson.GetBytes(out, "request.contents.0.parts.0.functionCall.name").String(); got != secondMapped {
		t.Fatalf("functionCall.name = %q, want %q. Output: %s", got, secondMapped, out)
	}
	if got := gjson.GetBytes(out, "request.contents.1.parts.0.functionResponse.name").String(); got != secondMapped {
		t.Fatalf("functionResponse.name = %q, want %q. Output: %s", got, secondMapped, out)
	}
	if got := gjson.GetBytes(
		out, "request.toolConfig.functionCallingConfig.allowedFunctionNames.0",
	).String(); got != secondMapped {
		t.Fatalf("allowedFunctionNames.0 = %q, want %q. Output: %s", got, secondMapped, out)
	}
}

func TestConvertClaudeRequestToAntigravity_MapsToolResultNameOnce(t *testing.T) {
	inputJSON := []byte(`{
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read/file","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}
		],
		"tools":[
			{"name":"read/file","input_schema":{"type":"object"}},
			{"name":"read_file","input_schema":{"type":"object"}}
		]
	}`)

	out := convertClaudeRequestToAntigravity("gemini-3-flash", inputJSON, false)
	callName := gjson.GetBytes(out, "request.contents.0.parts.0.functionCall.name").String()
	responseName := gjson.GetBytes(out, "request.contents.1.parts.0.functionResponse.name").String()
	if callName == "" || responseName != callName {
		t.Fatalf(
			"function names call=%q response=%q, want the same non-empty mapping. Output: %s", callName, responseName,
			out,
		)
	}
}

func TestConvertClaudeRequestToAntigravity_ToolChoice_SpecificTool(t *testing.T) {
	inputJSON := []byte(`{
		"model": "gemini-3-flash-preview",
		"messages": [
			{
				"role": "user",
				"content": [
					{"type": "text", "text": "hi"}
				]
			}
		],
		"tools": [
			{
				"name": "json",
				"description": "A JSON tool",
				"input_schema": {
					"type": "object",
					"properties": {}
				}
			}
		],
		"tool_choice": {"type": "tool", "name": "json"}
	}`)

	output := convertClaudeRequestToAntigravity("gemini-3-flash-preview", inputJSON, false)
	outputStr := string(output)

	if got := gjson.Get(outputStr, "request.toolConfig.functionCallingConfig.mode").String(); got != "ANY" {
		t.Fatalf("Expected toolConfig.functionCallingConfig.mode 'ANY', got '%s'", got)
	}
	allowed := gjson.Get(outputStr, "request.toolConfig.functionCallingConfig.allowedFunctionNames").Array()
	if len(allowed) != 1 || allowed[0].String() != "json" {
		t.Fatalf(
			"Expected allowedFunctionNames ['json'], got %s",
			gjson.Get(outputStr, "request.toolConfig.functionCallingConfig.allowedFunctionNames").Raw,
		)
	}
}

func TestConvertClaudeRequestToAntigravity_ToolUse(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "call_123",
						"name": "get_weather",
						"input": "{\"location\": \"Paris\"}"
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	// Now we expect only 1 part (tool_use), no dummy thinking block injected
	parts := gjson.Get(outputStr, "request.contents.0.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("Expected 1 part (tool only, no dummy injection), got %d", len(parts))
	}

	// Check function call conversion at parts[0]
	funcCall := parts[0].Get("functionCall")
	if !funcCall.Exists() {
		t.Error("functionCall should exist at parts[0]")
	}
	if funcCall.Get("name").String() != "get_weather" {
		t.Errorf("Expected function name 'get_weather', got '%s'", funcCall.Get("name").String())
	}
	if funcCall.Get("id").String() != "call_123" {
		t.Errorf("Expected function id 'call_123', got '%s'", funcCall.Get("id").String())
	}
	if parts[0].Get("thoughtSignature").Exists() {
		t.Errorf(
			"Claude target tool call should not receive Gemini sentinel, got %q",
			parts[0].Get("thoughtSignature").String(),
		)
	}
}

func TestConvertClaudeRequestToAntigravity_ToolUsePreservesPresentNonObjectInput(t *testing.T) {
	tests := []struct {
		name             string
		inputJSON        string
		wantArgs         string
		wantFunctionCall bool
	}{
		{name: "plain string", inputJSON: `"plain"`, wantArgs: `"plain"`, wantFunctionCall: true},
		{name: "array", inputJSON: `[1,"two"]`, wantArgs: `[1,"two"]`, wantFunctionCall: true},
		{name: "number", inputJSON: `42`, wantArgs: `42`, wantFunctionCall: true},
		{name: "boolean", inputJSON: `true`, wantArgs: `true`, wantFunctionCall: true},
		{name: "null", inputJSON: `null`, wantArgs: `{}`, wantFunctionCall: true},
		{name: "missing"},
	}

	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				inputField := ""
				if test.inputJSON != "" {
					inputField = fmt.Sprintf(`,"input":%s`, test.inputJSON)
				}
				inputJSON := []byte(fmt.Sprintf(
					`{
				"model":"claude-sonnet-4-5",
				"messages":[{
					"role":"assistant",
					"content":[{
						"type":"tool_use",
						"id":"call_123",
						"name":"run"%s
					}]
				}]
			}`, inputField,
				))

				output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
				functionCall := gjson.GetBytes(output, "request.contents.0.parts.0.functionCall")
				if test.wantFunctionCall {
					if !functionCall.Exists() {
						t.Fatalf("functionCall should exist, output: %s", output)
					}
					if got := functionCall.Get("args").Raw; got != test.wantArgs {
						t.Fatalf("functionCall.args = %q, want %q; output: %s", got, test.wantArgs, output)
					}
					return
				}
				if functionCall.Exists() {
					t.Fatalf("missing input should not create functionCall, output: %s", output)
				}
			},
		)
	}
}

func TestConvertClaudeRequestToAntigravity_ToolUse_WithSignature(t *testing.T) {
	cache.ClearSignatureCache("")

	validSignature := validClaudeThinkingSignatureForAntigravityTest()
	thinkingText := "Let me think..."

	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [
			{
				"role": "user",
				"content": [{"type": "text", "text": "Test user message"}]
			},
			{
				"role": "assistant",
				"content": [
					{"type": "thinking", "thinking": "` + thinkingText + `", "signature": "` + validSignature + `"},
					{
						"type": "tool_use",
						"id": "call_123",
						"name": "get_weather",
						"input": "{\"location\": \"Paris\"}"
					}
				]
			}
		]
	}`)

	cache.StoreSignature("claude-sonnet-4-5-thinking", thinkingText, validSignature)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5-thinking", inputJSON, false)
	outputStr := string(output)

	// Check function call has the signature from the preceding thinking block (now in contents.1)
	part := gjson.Get(outputStr, "request.contents.1.parts.1")
	if part.Get("functionCall.name").String() != "get_weather" {
		t.Errorf("Expected functionCall, got %s", part.Raw)
	}
	expectedSignature, compatible := signature.CompatibleAntigravityClaudeThinkingSignature(validSignature)
	if !compatible {
		t.Fatal("test signature should be valid for Antigravity Claude replay")
	}
	if part.Get("thoughtSignature").String() != expectedSignature {
		t.Errorf(
			"Expected thoughtSignature '%s' on tool_use, got '%s'", expectedSignature,
			part.Get("thoughtSignature").String(),
		)
	}
}

func TestConvertClaudeRequestToAntigravity_ReorderThinking(t *testing.T) {
	cache.ClearSignatureCache("")

	// Case: text block followed by thinking block -> should be reordered to thinking first
	validSignature := validClaudeThinkingSignatureForAntigravityTest()
	thinkingText := "Planning..."

	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [
			{
				"role": "user",
				"content": [{"type": "text", "text": "Test user message"}]
			},
			{
				"role": "assistant",
				"content": [
					{"type": "text", "text": "Here is the plan."},
					{"type": "thinking", "thinking": "` + thinkingText + `", "signature": "` + validSignature + `"}
				]
			}
		]
	}`)

	cache.StoreSignature("claude-sonnet-4-5-thinking", thinkingText, validSignature)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5-thinking", inputJSON, false)
	outputStr := string(output)

	// Verify order: Thinking block MUST be first (now in contents.1 due to user message)
	parts := gjson.Get(outputStr, "request.contents.1.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("Expected 2 parts, got %d", len(parts))
	}

	if !parts[0].Get("thought").Bool() {
		t.Error("First part should be thinking block after reordering")
	}
	if parts[1].Get("text").String() != "Here is the plan." {
		t.Error("Second part should be text block")
	}
}

func TestConvertClaudeRequestToAntigravity_ReorderTextBeforeFunctionCall(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "text", "text": "Let me check."},
					{
						"type": "tool_use",
						"id": "call_abc",
						"name": "Read",
						"input": {"file": "test.go"}
					},
					{"type": "text", "text": "Reading the file now."}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	parts := gjson.Get(outputStr, "request.contents.0.parts").Array()
	if len(parts) != 3 {
		t.Fatalf("Expected 3 parts, got %d", len(parts))
	}
	if got := parts[0].Get("text").String(); got != "Let me check." {
		t.Fatalf("first part text = %q", got)
	}
	if got := parts[1].Get("text").String(); got != "Reading the file now." {
		t.Fatalf("second part text = %q", got)
	}
	if got := parts[2].Get("functionCall.name").String(); got != "Read" {
		t.Fatalf("third part functionCall.name = %q, output=%s", got, outputStr)
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResult(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "get_weather-call-123",
						"name": "get_weather",
						"input": {"location": "Paris"}
					}
				]
			},
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "get_weather-call-123",
						"content": "22C sunny"
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	// Check function response conversion
	funcResp := gjson.Get(outputStr, "request.contents.1.parts.0.functionResponse")
	if !funcResp.Exists() {
		t.Error("functionResponse should exist")
	}
	if funcResp.Get("id").String() != "get_weather-call-123" {
		t.Errorf("Expected function id, got '%s'", funcResp.Get("id").String())
	}
	if funcResp.Get("name").String() != "get_weather" {
		t.Errorf("Expected function name 'get_weather', got '%s'", funcResp.Get("name").String())
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResultName_TooluFormat(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-haiku-4-5-20251001",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "toolu_tool-48fca351f12844eabf49dad8b63886d2",
						"name": "Glob",
						"input": {"pattern": "**/*.py"}
					},
					{
						"type": "tool_use",
						"id": "toolu_tool-cf2d061f75f845c49aacc18ee75ee708",
						"name": "Bash",
						"input": {"command": "ls"}
					}
				]
			},
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "toolu_tool-48fca351f12844eabf49dad8b63886d2",
						"content": "file1.py\nfile2.py"
					},
					{
						"type": "tool_result",
						"tool_use_id": "toolu_tool-cf2d061f75f845c49aacc18ee75ee708",
						"content": "total 10"
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-haiku-4-5-20251001", inputJSON, false)
	outputStr := string(output)

	funcResp0 := gjson.Get(outputStr, "request.contents.1.parts.0.functionResponse")
	if !funcResp0.Exists() {
		t.Fatal("first functionResponse should exist")
	}
	if got := funcResp0.Get("name").String(); got != "Glob" {
		t.Errorf("Expected name 'Glob' for toolu_ format, got '%s'", got)
	}

	funcResp1 := gjson.Get(outputStr, "request.contents.1.parts.1.functionResponse")
	if !funcResp1.Exists() {
		t.Fatal("second functionResponse should exist")
	}
	if got := funcResp1.Get("name").String(); got != "Bash" {
		t.Errorf("Expected name 'Bash' for toolu_ format, got '%s'", got)
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResultName_CustomFormat(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-haiku-4-5-20251001",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "Read-1773420180464065165-1327",
						"name": "Read",
						"input": {"file_path": "/tmp/test.py"}
					}
				]
			},
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "Read-1773420180464065165-1327",
						"content": "file content here"
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-haiku-4-5-20251001", inputJSON, false)
	outputStr := string(output)

	funcResp := gjson.Get(outputStr, "request.contents.1.parts.0.functionResponse")
	if !funcResp.Exists() {
		t.Fatal("functionResponse should exist")
	}
	if got := funcResp.Get("name").String(); got != "Read" {
		t.Errorf("Expected name 'Read', got '%s'", got)
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResultName_NoMatchingToolUse_Heuristic(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "get_weather-call-123",
						"content": "22C sunny"
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	funcResp := gjson.Get(outputStr, "request.contents.0.parts.0.functionResponse")
	if !funcResp.Exists() {
		t.Fatal("functionResponse should exist")
	}
	if got := funcResp.Get("name").String(); got != "get_weather" {
		t.Errorf("Expected heuristic-derived name 'get_weather', got '%s'", got)
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResultName_NoMatchingToolUse_RawID(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "toolu_tool-48fca351f12844eabf49dad8b63886d2",
						"content": "result data"
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	funcResp := gjson.Get(outputStr, "request.contents.0.parts.0.functionResponse")
	if !funcResp.Exists() {
		t.Fatal("functionResponse should exist")
	}
	got := funcResp.Get("name").String()
	if got == "" {
		t.Error("functionResponse.name must not be empty")
	}
	if got != "toolu_tool-48fca351f12844eabf49dad8b63886d2" {
		t.Errorf("Expected raw ID as last-resort name, got '%s'", got)
	}
}

func TestConvertClaudeRequestToAntigravity_ThinkingConfig(t *testing.T) {
	// Note: This test requires the model to be registered in the registry
	// with Thinking metadata. If the registry is not populated in test environment,
	// thinkingConfig won't be added. We'll test the basic structure only.
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [],
		"thinking": {
			"type": "enabled",
			"budget_tokens": 8000
		}
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5-thinking", inputJSON, false)
	outputStr := string(output)

	// Check thinking config conversion (only if model supports thinking in registry)
	thinkingConfig := gjson.Get(outputStr, "request.generationConfig.thinkingConfig")
	if thinkingConfig.Exists() {
		if thinkingConfig.Get("thinkingBudget").Int() != 8000 {
			t.Errorf("Expected thinkingBudget 8000, got %d", thinkingConfig.Get("thinkingBudget").Int())
		}
		if thinkingConfig.Get("includeThoughts").Exists() {
			t.Error("includeThoughts should be absent without explicit Claude display intent")
		}
	} else {
		t.Log("thinkingConfig not present - model may not be registered in test registry")
	}
}

func TestConvertClaudeRequestToAntigravity_ImageContent(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "image",
						"source": {
							"type": "base64",
							"media_type": "image/png",
							"data": "iVBORw0KGgoAAAANSUhEUg=="
						}
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	// Check inline data conversion
	inlineData := gjson.Get(outputStr, "request.contents.0.parts.0.inlineData")
	if !inlineData.Exists() {
		t.Error("inlineData should exist")
	}
	if inlineData.Get("mimeType").String() != "image/png" {
		t.Error("mimeType mismatch")
	}
	if !strings.Contains(inlineData.Get("data").String(), "iVBORw0KGgo") {
		t.Error("data mismatch")
	}
}

func TestConvertClaudeRequestToAntigravity_GenerationConfig(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [],
		"temperature": 0.7,
		"top_p": 0.9,
		"top_k": 40,
		"max_tokens": 2000
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	genConfig := gjson.Get(outputStr, "request.generationConfig")
	if genConfig.Get("temperature").Float() != 0.7 {
		t.Errorf("Expected temperature 0.7, got %f", genConfig.Get("temperature").Float())
	}
	if genConfig.Get("topP").Float() != 0.9 {
		t.Errorf("Expected topP 0.9, got %f", genConfig.Get("topP").Float())
	}
	if genConfig.Get("topK").Float() != 40 {
		t.Errorf("Expected topK 40, got %f", genConfig.Get("topK").Float())
	}
	if genConfig.Get("maxOutputTokens").Float() != 2000 {
		t.Errorf("Expected maxOutputTokens 2000, got %f", genConfig.Get("maxOutputTokens").Float())
	}
}

// ============================================================================
// Trailing Unsigned Thinking Block Removal
// ============================================================================

func TestConvertClaudeRequestToAntigravity_TrailingUnsignedThinking_Removed(t *testing.T) {
	// Last assistant message ends with unsigned thinking block - should be removed
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [
			{
				"role": "user",
				"content": [{"type": "text", "text": "Hello"}]
			},
			{
				"role": "assistant",
				"content": [
					{"type": "text", "text": "Here is my answer"},
					{"type": "thinking", "thinking": "I should think more..."}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5-thinking", inputJSON, false)
	outputStr := string(output)

	// The last part of the last assistant message should NOT be a thinking block
	lastMessageParts := gjson.Get(outputStr, "request.contents.1.parts")
	if !lastMessageParts.IsArray() {
		t.Fatal("Last message should have parts array")
	}
	parts := lastMessageParts.Array()
	if len(parts) == 0 {
		t.Fatal("Last message should have at least one part")
	}

	// The unsigned thinking should be removed, leaving only the text
	lastPart := parts[len(parts)-1]
	if lastPart.Get("thought").Bool() {
		t.Error("Trailing unsigned thinking block should be removed")
	}
}

func TestConvertClaudeRequestToAntigravity_TrailingSignedThinking_Kept(t *testing.T) {
	cache.ClearSignatureCache("")

	// Last assistant message ends with signed thinking block - should be kept
	validSignature := validClaudeThinkingSignatureForAntigravityTest()
	thinkingText := "Valid thinking..."

	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [
			{
				"role": "user",
				"content": [{"type": "text", "text": "Hello"}]
			},
			{
				"role": "assistant",
				"content": [
					{"type": "text", "text": "Here is my answer"},
					{"type": "thinking", "thinking": "` + thinkingText + `", "signature": "` + validSignature + `"}
				]
			}
		]
	}`)

	cache.StoreSignature("claude-sonnet-4-5-thinking", thinkingText, validSignature)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5-thinking", inputJSON, false)
	outputStr := string(output)

	// The signed thinking block should be preserved
	lastMessageParts := gjson.Get(outputStr, "request.contents.1.parts")
	parts := lastMessageParts.Array()
	if len(parts) < 2 {
		t.Error("Signed thinking block should be preserved")
	}
}

func TestConvertClaudeRequestToAntigravity_MiddleUnsignedThinking_Removed(t *testing.T) {
	// Middle message has unsigned thinking - should be removed entirely
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{"type": "thinking", "thinking": "Middle thinking..."},
					{"type": "text", "text": "Answer"}
				]
			},
			{
				"role": "user",
				"content": [{"type": "text", "text": "Follow up"}]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5-thinking", inputJSON, false)
	outputStr := string(output)

	// Unsigned thinking should be removed entirely
	parts := gjson.Get(outputStr, "request.contents.0.parts").Array()
	if len(parts) != 1 {
		t.Fatalf("Expected 1 part (thinking removed), got %d", len(parts))
	}

	// Only text part should remain
	if parts[0].Get("thought").Bool() {
		t.Error("Thinking block should be removed, not preserved")
	}
	if parts[0].Get("text").String() != "Answer" {
		t.Errorf("Expected text 'Answer', got '%s'", parts[0].Get("text").String())
	}
}

// ============================================================================
// Tool + Thinking System Hint Injection
// ============================================================================

func TestConvertClaudeRequestToAntigravity_ToolAndThinking_HintInjected(t *testing.T) {
	// When both tools and thinking are enabled, hint should be injected into system instruction
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Hello"}]}],
		"system": [{"type": "text", "text": "You are helpful."}],
		"tools": [
			{
				"name": "get_weather",
				"description": "Get weather",
				"input_schema": {"type": "object", "properties": {"location": {"type": "string"}}}
			}
		],
		"thinking": {"type": "enabled", "budget_tokens": 8000}
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5-thinking", inputJSON, false)
	outputStr := string(output)

	// System instruction should contain the interleaved thinking hint
	sysInstruction := gjson.Get(outputStr, "request.systemInstruction")
	if !sysInstruction.Exists() {
		t.Fatal("systemInstruction should exist")
	}

	// Check if hint is appended
	sysText := sysInstruction.Get("parts").Array()
	found := false
	for _, part := range sysText {
		if strings.Contains(part.Get("text").String(), "Interleaved thinking is enabled") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf(
			"Interleaved thinking hint should be injected when tools and thinking are both active, got: %v",
			sysInstruction.Raw,
		)
	}
}

func TestConvertClaudeRequestToAntigravity_ToolsOnly_NoHint(t *testing.T) {
	// When only tools are present (no thinking), hint should NOT be injected
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Hello"}]}],
		"system": [{"type": "text", "text": "You are helpful."}],
		"tools": [
			{
				"name": "get_weather",
				"description": "Get weather",
				"input_schema": {"type": "object", "properties": {"location": {"type": "string"}}}
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	// System instruction should NOT contain the hint
	sysInstruction := gjson.Get(outputStr, "request.systemInstruction")
	if sysInstruction.Exists() {
		for _, part := range sysInstruction.Get("parts").Array() {
			if strings.Contains(part.Get("text").String(), "Interleaved thinking is enabled") {
				t.Error("Hint should NOT be injected when only tools are present (no thinking)")
			}
		}
	}
}

func TestConvertClaudeRequestToAntigravity_ThinkingOnly_NoHint(t *testing.T) {
	// When only thinking is enabled (no tools), hint should NOT be injected
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Hello"}]}],
		"system": [{"type": "text", "text": "You are helpful."}],
		"thinking": {"type": "enabled", "budget_tokens": 8000}
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5-thinking", inputJSON, false)
	outputStr := string(output)

	// System instruction should NOT contain the hint (no tools)
	sysInstruction := gjson.Get(outputStr, "request.systemInstruction")
	if sysInstruction.Exists() {
		for _, part := range sysInstruction.Get("parts").Array() {
			if strings.Contains(part.Get("text").String(), "Interleaved thinking is enabled") {
				t.Error("Hint should NOT be injected when only thinking is present (no tools)")
			}
		}
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResultNoContent(t *testing.T) {
	// Bug repro: tool_result with no content field produces invalid JSON
	inputJSON := []byte(`{
		"model": "claude-opus-4-6-thinking",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "MyTool-123-456",
						"name": "MyTool",
						"input": {"key": "value"}
					}
				]
			},
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "MyTool-123-456"
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-opus-4-6-thinking", inputJSON, true)
	outputStr := string(output)

	if !gjson.Valid(outputStr) {
		t.Errorf("Result is not valid JSON:\n%s", outputStr)
	}

	// Verify the functionResponse has a valid result value
	fr := gjson.Get(outputStr, "request.contents.1.parts.0.functionResponse.response.result")
	if !fr.Exists() {
		t.Error("functionResponse.response.result should exist")
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResultNullContent(t *testing.T) {
	// Bug repro: tool_result with null content produces invalid JSON
	inputJSON := []byte(`{
		"model": "claude-opus-4-6-thinking",
		"messages": [
			{
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "MyTool-123-456",
						"name": "MyTool",
						"input": {"key": "value"}
					}
				]
			},
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "MyTool-123-456",
						"content": null
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-opus-4-6-thinking", inputJSON, true)
	outputStr := string(output)

	if !gjson.Valid(outputStr) {
		t.Errorf("Result is not valid JSON:\n%s", outputStr)
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResultWithImage(t *testing.T) {
	// tool_result with array content containing text + image should place
	// image data inside functionResponse.parts as inlineData, not as a
	// sibling part in the outer content (to avoid base64 context bloat).
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "Read-123-456",
						"content": [
							{
								"type": "text",
								"text": "File content here"
							},
							{
								"type": "image",
								"source": {
									"type": "base64",
									"media_type": "image/png",
									"data": "iVBORw0KGgoAAAANSUhEUg=="
								}
							}
						]
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	if !gjson.Valid(outputStr) {
		t.Fatalf("Result is not valid JSON:\n%s", outputStr)
	}

	// Image should be inside functionResponse.parts, not as outer sibling part
	funcResp := gjson.Get(outputStr, "request.contents.0.parts.0.functionResponse")
	if !funcResp.Exists() {
		t.Fatal("functionResponse should exist")
	}

	// Text content should be in response.result
	resultText := funcResp.Get("response.result.text").String()
	if resultText != "File content here" {
		t.Errorf("Expected response.result.text = 'File content here', got '%s'", resultText)
	}

	// Image should be in functionResponse.parts[0].inlineData
	inlineData := funcResp.Get("parts.0.inlineData")
	if !inlineData.Exists() {
		t.Fatal("functionResponse.parts[0].inlineData should exist")
	}
	if inlineData.Get("mimeType").String() != "image/png" {
		t.Errorf("Expected mimeType 'image/png', got '%s'", inlineData.Get("mimeType").String())
	}
	if !strings.Contains(inlineData.Get("data").String(), "iVBORw0KGgo") {
		t.Error("data mismatch")
	}

	// Image should NOT be in outer parts (only functionResponse part should exist)
	outerParts := gjson.Get(outputStr, "request.contents.0.parts")
	if outerParts.IsArray() && len(outerParts.Array()) > 1 {
		t.Errorf("Expected only 1 outer part (functionResponse), got %d", len(outerParts.Array()))
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResultWithSingleImage(t *testing.T) {
	// tool_result with single image object as content should place
	// image data inside functionResponse.parts, not as outer sibling part.
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "Read-789-012",
						"content": {
							"type": "image",
							"source": {
								"type": "base64",
								"media_type": "image/jpeg",
								"data": "/9j/4AAQSkZJRgABAQ=="
							}
						}
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	if !gjson.Valid(outputStr) {
		t.Fatalf("Result is not valid JSON:\n%s", outputStr)
	}

	funcResp := gjson.Get(outputStr, "request.contents.0.parts.0.functionResponse")
	if !funcResp.Exists() {
		t.Fatal("functionResponse should exist")
	}

	// response.result should be empty (image only)
	if funcResp.Get("response.result").String() != "" {
		t.Errorf(
			"Expected empty response.result for image-only content, got '%s'", funcResp.Get("response.result").String(),
		)
	}

	// Image should be in functionResponse.parts[0].inlineData
	inlineData := funcResp.Get("parts.0.inlineData")
	if !inlineData.Exists() {
		t.Fatal("functionResponse.parts[0].inlineData should exist")
	}
	if inlineData.Get("mimeType").String() != "image/jpeg" {
		t.Errorf("Expected mimeType 'image/jpeg', got '%s'", inlineData.Get("mimeType").String())
	}

	// Image should NOT be in outer parts
	outerParts := gjson.Get(outputStr, "request.contents.0.parts")
	if outerParts.IsArray() && len(outerParts.Array()) > 1 {
		t.Errorf("Expected only 1 outer part, got %d", len(outerParts.Array()))
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResultWithMultipleImagesAndTexts(t *testing.T) {
	// tool_result with array content: 2 text items + 2 images
	// All images go into functionResponse.parts, texts into response.result array
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "Multi-001",
						"content": [
							{"type": "text", "text": "First text"},
							{
								"type": "image",
								"source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}
							},
							{"type": "text", "text": "Second text"},
							{
								"type": "image",
								"source": {"type": "base64", "media_type": "image/jpeg", "data": "BBBB"}
							}
						]
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	if !gjson.Valid(outputStr) {
		t.Fatalf("Result is not valid JSON:\n%s", outputStr)
	}

	funcResp := gjson.Get(outputStr, "request.contents.0.parts.0.functionResponse")
	if !funcResp.Exists() {
		t.Fatal("functionResponse should exist")
	}

	// Multiple text items => response.result is an array
	resultArr := funcResp.Get("response.result")
	if !resultArr.IsArray() {
		t.Fatalf("Expected response.result to be an array, got: %s", resultArr.Raw)
	}
	results := resultArr.Array()
	if len(results) != 2 {
		t.Fatalf("Expected 2 result items, got %d", len(results))
	}

	// Both images should be in functionResponse.parts
	imgParts := funcResp.Get("parts").Array()
	if len(imgParts) != 2 {
		t.Fatalf("Expected 2 image parts in functionResponse.parts, got %d", len(imgParts))
	}
	if imgParts[0].Get("inlineData.mimeType").String() != "image/png" {
		t.Errorf("Expected first image mimeType 'image/png', got '%s'", imgParts[0].Get("inlineData.mimeType").String())
	}
	if imgParts[0].Get("inlineData.data").String() != "AAAA" {
		t.Errorf("Expected first image data 'AAAA', got '%s'", imgParts[0].Get("inlineData.data").String())
	}
	if imgParts[1].Get("inlineData.mimeType").String() != "image/jpeg" {
		t.Errorf(
			"Expected second image mimeType 'image/jpeg', got '%s'", imgParts[1].Get("inlineData.mimeType").String(),
		)
	}
	if imgParts[1].Get("inlineData.data").String() != "BBBB" {
		t.Errorf("Expected second image data 'BBBB', got '%s'", imgParts[1].Get("inlineData.data").String())
	}

	// Only 1 outer part (the functionResponse itself)
	outerParts := gjson.Get(outputStr, "request.contents.0.parts").Array()
	if len(outerParts) != 1 {
		t.Errorf("Expected 1 outer part, got %d", len(outerParts))
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResultWithOnlyMultipleImages(t *testing.T) {
	// tool_result with only images (no text) — response.result should be empty string
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "ImgOnly-001",
						"content": [
							{
								"type": "image",
								"source": {"type": "base64", "media_type": "image/png", "data": "PNG1"}
							},
							{
								"type": "image",
								"source": {"type": "base64", "media_type": "image/gif", "data": "GIF1"}
							}
						]
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	if !gjson.Valid(outputStr) {
		t.Fatalf("Result is not valid JSON:\n%s", outputStr)
	}

	funcResp := gjson.Get(outputStr, "request.contents.0.parts.0.functionResponse")
	if !funcResp.Exists() {
		t.Fatal("functionResponse should exist")
	}

	// No text => response.result should be empty string
	if funcResp.Get("response.result").String() != "" {
		t.Errorf("Expected empty response.result, got '%s'", funcResp.Get("response.result").String())
	}

	// Both images in functionResponse.parts
	imgParts := funcResp.Get("parts").Array()
	if len(imgParts) != 2 {
		t.Fatalf("Expected 2 image parts, got %d", len(imgParts))
	}
	if imgParts[0].Get("inlineData.mimeType").String() != "image/png" {
		t.Error("first image mimeType mismatch")
	}
	if imgParts[1].Get("inlineData.mimeType").String() != "image/gif" {
		t.Error("second image mimeType mismatch")
	}

	// Only 1 outer part
	outerParts := gjson.Get(outputStr, "request.contents.0.parts").Array()
	if len(outerParts) != 1 {
		t.Errorf("Expected 1 outer part, got %d", len(outerParts))
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResultImageNotBase64(t *testing.T) {
	// image with source.type != "base64" should be treated as non-image (falls through)
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "NotB64-001",
						"content": [
							{"type": "text", "text": "some output"},
							{
								"type": "image",
								"source": {"type": "url", "url": "https://example.com/img.png"}
							}
						]
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	if !gjson.Valid(outputStr) {
		t.Fatalf("Result is not valid JSON:\n%s", outputStr)
	}

	funcResp := gjson.Get(outputStr, "request.contents.0.parts.0.functionResponse")
	if !funcResp.Exists() {
		t.Fatal("functionResponse should exist")
	}

	// Non-base64 image is treated as non-image, so it goes into the filtered results
	// along with the text item. Since there are 2 non-image items, result is array.
	resultArr := funcResp.Get("response.result")
	if !resultArr.IsArray() {
		t.Fatalf("Expected response.result to be an array (2 non-image items), got: %s", resultArr.Raw)
	}
	results := resultArr.Array()
	if len(results) != 2 {
		t.Fatalf("Expected 2 result items, got %d", len(results))
	}

	// No functionResponse.parts (no base64 images collected)
	if funcResp.Get("parts").Exists() {
		t.Error("functionResponse.parts should NOT exist when no base64 images")
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResultImageMissingData(t *testing.T) {
	// image with source.type=base64 but missing data field
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "NoData-001",
						"content": [
							{"type": "text", "text": "output"},
							{
								"type": "image",
								"source": {"type": "base64", "media_type": "image/png"}
							}
						]
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	if !gjson.Valid(outputStr) {
		t.Fatalf("Result is not valid JSON:\n%s", outputStr)
	}

	funcResp := gjson.Get(outputStr, "request.contents.0.parts.0.functionResponse")
	if !funcResp.Exists() {
		t.Fatal("functionResponse should exist")
	}

	// The image is still classified as base64 image (type check passes),
	// but data field is missing => inlineData has mimeType but no data
	imgParts := funcResp.Get("parts").Array()
	if len(imgParts) != 1 {
		t.Fatalf("Expected 1 image part, got %d", len(imgParts))
	}
	if imgParts[0].Get("inlineData.mimeType").String() != "image/png" {
		t.Error("mimeType should still be set")
	}
	if imgParts[0].Get("inlineData.data").Exists() {
		t.Error("data should not exist when source.data is missing")
	}
}

func TestConvertClaudeRequestToAntigravity_ToolResultImageMissingMediaType(t *testing.T) {
	// image with source.type=base64 but missing media_type field
	inputJSON := []byte(`{
		"model": "claude-3-5-sonnet-20240620",
		"messages": [
			{
				"role": "user",
				"content": [
					{
						"type": "tool_result",
						"tool_use_id": "NoMime-001",
						"content": [
							{"type": "text", "text": "output"},
							{
								"type": "image",
								"source": {"type": "base64", "data": "AAAA"}
							}
						]
					}
				]
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	outputStr := string(output)

	if !gjson.Valid(outputStr) {
		t.Fatalf("Result is not valid JSON:\n%s", outputStr)
	}

	funcResp := gjson.Get(outputStr, "request.contents.0.parts.0.functionResponse")
	if !funcResp.Exists() {
		t.Fatal("functionResponse should exist")
	}

	// The image is still classified as base64 image,
	// but media_type is missing => inlineData has data but no mimeType
	imgParts := funcResp.Get("parts").Array()
	if len(imgParts) != 1 {
		t.Fatalf("Expected 1 image part, got %d", len(imgParts))
	}
	if imgParts[0].Get("inlineData.mimeType").Exists() {
		t.Error("mimeType should not exist when media_type is missing")
	}
	if imgParts[0].Get("inlineData.data").String() != "AAAA" {
		t.Error("data should still be set")
	}
}

func TestConvertClaudeRequestToAntigravity_ToolAndThinking_NoExistingSystem(t *testing.T) {
	// When tools + thinking but no system instruction, should create one with hint
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-5-thinking",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Hello"}]}],
		"tools": [
			{
				"name": "get_weather",
				"description": "Get weather",
				"input_schema": {"type": "object", "properties": {"location": {"type": "string"}}}
			}
		],
		"thinking": {"type": "enabled", "budget_tokens": 8000}
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5-thinking", inputJSON, false)
	outputStr := string(output)

	// System instruction should be created with hint
	sysInstruction := gjson.Get(outputStr, "request.systemInstruction")
	if !sysInstruction.Exists() {
		t.Fatal("systemInstruction should be created when tools + thinking are active")
	}

	sysText := sysInstruction.Get("parts").Array()
	found := false
	for _, part := range sysText {
		if strings.Contains(part.Get("text").String(), "Interleaved thinking is enabled") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Interleaved thinking hint should be in created systemInstruction, got: %v", sysInstruction.Raw)
	}
}

func TestConvertClaudeRequestToAntigravity_MapsTypedWebSearchToIndependentSearchRequest(t *testing.T) {
	registry.GetGlobalRegistry().RegisterClient(
		"test-antigravity-claude-websearch", "antigravity", []*registry.ModelInfo{
			{ID: "gemini-3.1-flash-lite", SupportsWebSearch: true},
		},
	)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("test-antigravity-claude-websearch") })

	inputJSON := []byte(`{
		"model": "gemini-3.1-flash-lite",
		"messages": [{"role": "user", "content": "北京天气 2026-06-12"}],
		"tools": [{"type": "web_search_20250305", "name": "web_search", "max_uses": 8, "allowed_domains": ["www.baidu.com", "weather.com.cn"]}]
	}`)

	output := convertClaudeRequestToAntigravity("gemini-3.1-flash-lite", inputJSON, true)
	if got := gjson.GetBytes(output, "requestType").String(); got != "web_search" {
		t.Fatalf("requestType = %q, want web_search: %s", got, output)
	}
	if got := gjson.GetBytes(output, "request.contents.0.parts.0.text").String(); got != "北京天气 2026-06-12" {
		t.Fatalf("search query = %q, want original user query: %s", got, output)
	}
	if got := gjson.GetBytes(
		output, "request.systemInstruction.parts.0.text",
	).String(); got != antigravityWebSearchSystemInstruction {
		t.Fatalf("unexpected search system instruction: %q", got)
	}
	if got := gjson.GetBytes(
		output, "request.tools.0.googleSearch.enhancedContent.imageSearch.maxResultCount",
	).Int(); got != 8 {
		t.Fatalf("image search maxResultCount = %d, want 8: %s", got, output)
	}
	if got := gjson.GetBytes(
		output, "request.tools.0.googleSearch.includedDomains.0",
	).String(); got != "www.baidu.com" {
		t.Fatalf("includedDomains.0 = %q, want www.baidu.com: %s", got, output)
	}
	if got := gjson.GetBytes(
		output, "request.tools.0.googleSearch.includedDomains.1",
	).String(); got != "weather.com.cn" {
		t.Fatalf("includedDomains.1 = %q, want weather.com.cn: %s", got, output)
	}
	if got := gjson.GetBytes(output, "request.generationConfig.candidateCount").Int(); got != 1 {
		t.Fatalf("candidateCount = %d, want 1: %s", got, output)
	}
}

func TestConvertClaudeRequestToAntigravity_UsesDefaultWebSearchMaxResultCountWithoutMaxUses(t *testing.T) {
	registry.GetGlobalRegistry().RegisterClient(
		"test-antigravity-claude-websearch-default-max", "antigravity", []*registry.ModelInfo{
			{ID: "gemini-3.1-flash-lite", SupportsWebSearch: true},
		},
	)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("test-antigravity-claude-websearch-default-max") })

	inputJSON := []byte(`{
		"model": "gemini-3.1-flash-lite",
		"messages": [{"role": "user", "content": "北京天气 2026-06-12"}],
		"tools": [{"type": "web_search_20250305", "name": "web_search"}]
	}`)

	output := convertClaudeRequestToAntigravity("gemini-3.1-flash-lite", inputJSON, true)
	if got := gjson.GetBytes(
		output, "request.tools.0.googleSearch.enhancedContent.imageSearch.maxResultCount",
	).Int(); got != 5 {
		t.Fatalf("image search maxResultCount = %d, want default 5: %s", got, output)
	}
}

func TestConvertClaudeRequestToAntigravity_DoesNotMapTypedWebSearchWhenMixedWithCustomTools(t *testing.T) {
	registry.GetGlobalRegistry().RegisterClient(
		"test-antigravity-claude-websearch-mixed", "antigravity", []*registry.ModelInfo{
			{ID: "gemini-3.1-flash-lite", SupportsWebSearch: true},
		},
	)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("test-antigravity-claude-websearch-mixed") })

	inputJSON := []byte(`{
		"model": "gemini-3.1-flash-lite",
		"messages": [{"role": "user", "content": "Search current weather"}],
		"tools": [
			{"type": "web_search_20250305", "name": "web_search", "max_uses": 8},
			{"name": "lookup", "description": "Lookup local data", "input_schema": {"type": "object", "properties": {}}}
		]
	}`)

	output := convertClaudeRequestToAntigravity("gemini-3.1-flash-lite", inputJSON, true)
	if got := gjson.GetBytes(output, "requestType").String(); got == "web_search" {
		t.Fatalf("mixed tools should not become independent web_search request: %s", output)
	}
	if got := gjson.GetBytes(output, "request.tools.#(googleSearch)").Raw; got != "" {
		t.Fatalf("mixed tools should not inject native googleSearch into chat request: %s", output)
	}
	if got := gjson.GetBytes(output, `request.tools.#.functionDeclarations.#(name=="lookup")`).Raw; got == "" {
		t.Fatalf("custom tool declaration should be preserved: %s", output)
	}
}

func TestConvertClaudeRequestToAntigravity_DoesNotMapTypedWebSearchForUnsupportedRouteModel(t *testing.T) {
	registry.GetGlobalRegistry().RegisterClient(
		"test-antigravity-claude-websearch-route", "antigravity", []*registry.ModelInfo{
			{ID: "gemini-3.5-flash"},
			{ID: "gemini-3.1-flash-lite", SupportsWebSearch: true},
		},
	)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("test-antigravity-claude-websearch-route") })

	inputJSON := []byte(`{
		"model": "gemini-3.5-flash",
		"messages": [{"role": "user", "content": "Perform a web search"}],
		"tools": [{"type": "web_search_20250305", "name": "web_search", "max_uses": 8}]
	}`)

	output := convertClaudeRequestToAntigravity("gemini-3.5-flash", inputJSON, true)
	if got := gjson.GetBytes(output, "model").String(); got != "gemini-3.5-flash" {
		t.Fatalf("web search request model = %q, want original route model: %s", got, output)
	}
	if got := gjson.GetBytes(output, "request.tools.#(googleSearch)").Raw; got != "" {
		t.Fatalf("typed web_search should not become native googleSearch for unsupported route model: %s", output)
	}
}

func TestConvertClaudeRequestToAntigravity_DoesNotMapTypedWebSearchForFlashAgentWithoutCapability(t *testing.T) {
	registry.GetGlobalRegistry().RegisterClient(
		"test-antigravity-claude-websearch-flash-agent", "antigravity", []*registry.ModelInfo{
			{ID: "gemini-3-flash-agent"},
			{ID: "gemini-3.1-flash-lite", SupportsWebSearch: true},
		},
	)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient("test-antigravity-claude-websearch-flash-agent") })

	inputJSON := []byte(`{
		"model": "gemini-3-flash-agent",
		"messages": [{"role": "user", "content": "Perform a web search"}],
		"tools": [{"type": "web_search_20250305", "name": "web_search", "max_uses": 8}]
	}`)

	output := convertClaudeRequestToAntigravity("gemini-3-flash-agent", inputJSON, true)
	if got := gjson.GetBytes(output, "model").String(); got != "gemini-3-flash-agent" {
		t.Fatalf("web search request model = %q, want original route model: %s", got, output)
	}
	if got := gjson.GetBytes(output, "request.tools.#(googleSearch)").Raw; got != "" {
		t.Fatalf(
			"typed web_search should not become native googleSearch for flash-agent without capability: %s", output,
		)
	}
}

func TestConvertClaudeRequestToAntigravity_DoesNotMapTypedWebSearchForOtherModels(t *testing.T) {
	inputJSON := []byte(`{
		"model": "claude-sonnet-4-6",
		"messages": [{"role": "user", "content": "Search current weather"}],
		"tools": [{"type": "web_search_20250305", "name": "web_search", "max_uses": 8}]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-6", inputJSON, true)
	if got := gjson.GetBytes(output, "request.tools.#(googleSearch)").Raw; got != "" {
		t.Fatalf("model without Antigravity web search capability should not get native googleSearch: %s", output)
	}
}

func TestConvertClaudeRequestToAntigravityStripsPropertyNames(t *testing.T) {
	inputJSON := []byte(`{
		"model":"claude-sonnet-4-5",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{
				"name":"notion-create-pages",
				"input_schema":{
					"type":"object",
					"properties":{
						"records":{
							"type":"array",
							"items":{
								"type":"object",
								"properties":{"name":{"type":"string"}},
								"propertyNames":{"type":"string"}
							}
						}
					}
				}
			},
			{
				"name":"notion-update-page",
				"input_schema":{
					"type":"object",
					"properties":{
						"properties":{"type":"object","propertyNames":{"type":"string"}}
					}
				}
			}
		]
	}`)

	output := convertClaudeRequestToAntigravity("claude-sonnet-4-5", inputJSON, false)
	declarations := gjson.GetBytes(output, "request.tools.0.functionDeclarations")
	if !declarations.IsArray() || len(declarations.Array()) != 2 {
		t.Fatalf("expected two function declarations, got: %s", declarations.Raw)
	}
	if strings.Contains(declarations.Raw, `"propertyNames"`) {
		t.Errorf("propertyNames survived translation: %s", declarations.Raw)
	}
	if !declarations.Get("0.parametersJsonSchema.properties.records.items.properties.name").Exists() {
		t.Errorf("array item property was removed: %s", declarations.Get("0").Raw)
	}
	if !declarations.Get("1.parametersJsonSchema.properties.properties").Exists() {
		t.Errorf("property named properties was removed: %s", declarations.Get("1").Raw)
	}
}

func TestConvertClaudeRequestToAntigravityToolChoiceNoneOmitsToolsAndHint(t *testing.T) {
	for _, choice := range []string{`"none"`, `{"type":"none"}`} {
		input := []byte(`{
			"messages":[{"role":"user","content":"hi"}],
			"tools":[{"name":"lookup","input_schema":{"type":"object"}}],
			"tool_choice":` + choice + `,
			"thinking":{"type":"enabled","budget_tokens":1024}
		}`)
		out := convertClaudeRequestToAntigravity("claude-sonnet-4-5-thinking", input, false)
		if got := gjson.GetBytes(out, "request.toolConfig.functionCallingConfig.mode").String(); got != "NONE" {
			t.Fatalf("tool mode = %q, want NONE; output=%s", got, out)
		}
		if gjson.GetBytes(out, "request.tools").Exists() {
			t.Fatalf("request.tools must be omitted; output=%s", out)
		}
		if strings.Contains(gjson.GetBytes(out, "request.systemInstruction").Raw, "Interleaved thinking is enabled") {
			t.Fatalf("interleaved thinking hint must be omitted; output=%s", out)
		}
	}
}

func TestResolveThinkingSignaturePrefersExplicitClientValue(t *testing.T) {
	cache.ClearSignatureCache("")
	t.Cleanup(func() { cache.ClearSignatureCache("") })

	modelName := "claude-sonnet-4-5-thinking"
	thinkingText := "client/cache precedence"
	clientSignature := claudeThinkingSignatureForAntigravityTest(12)
	cachedSignature := claudeThinkingSignatureForAntigravityTest(11)
	cache.StoreSignature(modelName, thinkingText, cachedSignature)

	wantClient, compatible := signature.CompatibleAntigravityClaudeThinkingSignature(clientSignature)
	if !compatible {
		t.Fatal("client test signature should be compatible")
	}
	if got := resolveThinkingSignature(modelName, thinkingText, clientSignature); got != wantClient {
		t.Fatalf("explicit client signature = %q, want %q", got, wantClient)
	}
	if got := resolveThinkingSignature(modelName, thinkingText, "invalid-explicit-signature"); got != "" {
		t.Fatalf("invalid explicit signature was replaced from cache: %q", got)
	}

	wantCached, compatible := signature.CompatibleAntigravityClaudeThinkingSignature(cachedSignature)
	if !compatible {
		t.Fatal("cached test signature should be compatible")
	}
	if got := resolveThinkingSignature(modelName, thinkingText, "  "); got != wantCached {
		t.Fatalf("blank client signature resolved to %q, want cached %q", got, wantCached)
	}
}

func TestConvertClaudeRequestToAntigravityKeepsMidSessionSystemMessagesInUserTurn(t *testing.T) {
	for _, role := range []string{"system", "developer"} {
		t.Run(role, func(t *testing.T) {
			input := []byte(`{
				"system":"global instruction",
				"messages":[
					{"role":"user","content":"first user"},
					{"role":"assistant","content":"first answer"},
					{"role":"` + role + `","content":"mid-session notice"},
					{"role":"user","content":"second user"}
				]
			}`)

			out := convertClaudeRequestToAntigravity("claude-sonnet-4-5", input, false)
			if got := gjson.GetBytes(out, "request.systemInstruction.parts.0.text").String(); got != "global instruction" {
				t.Fatalf("global system instruction = %q; output=%s", got, out)
			}
			contents := gjson.GetBytes(out, "request.contents").Array()
			if len(contents) != 3 {
				t.Fatalf("contents count = %d, want 3; output=%s", len(contents), out)
			}
			parts := contents[2].Get("parts").Array()
			if len(parts) != 2 {
				t.Fatalf("merged user parts count = %d, want 2; output=%s", len(parts), out)
			}
			if !strings.Contains(parts[0].Get("text").String(), "mid-session notice") {
				t.Fatalf("mid-session notice missing from user boundary; output=%s", out)
			}
			if got := parts[1].Get("text").String(); got != "second user" {
				t.Fatalf("second user text = %q; output=%s", got, out)
			}
		})
	}
}

func TestConvertClaudeRequestToAntigravityAlignsOnlyMatchedParallelResultSlots(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"call_a","name":"tool_a","input":{}},
				{"type":"tool_use","id":"call_b","name":"tool_b","input":{}}
			]},
			{"role":"user","content":[
				{"type":"text","text":"keep-first"},
				{"type":"tool_result","tool_use_id":"call_b","content":"B"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"IMAGE"}},
				{"type":"tool_result","tool_use_id":"unmatched","content":"X"},
				{"type":"tool_result","tool_use_id":"call_a","content":"A"}
			]}
		]
	}`)

	out := convertClaudeRequestToAntigravity("claude-sonnet-4-5", input, false)
	parts := gjson.GetBytes(out, "request.contents.1.parts").Array()
	if len(parts) != 5 {
		t.Fatalf("result parts count = %d, want 5; output=%s", len(parts), out)
	}
	if got := parts[0].Get("text").String(); got != "keep-first" {
		t.Fatalf("text slot moved or changed: %q; output=%s", got, out)
	}
	if got := parts[1].Get("functionResponse.id").String(); got != "call_a" {
		t.Fatalf("first matched result = %q, want call_a; output=%s", got, out)
	}
	if got := parts[2].Get("inlineData.data").String(); got != "IMAGE" {
		t.Fatalf("image slot moved or changed: %q; output=%s", got, out)
	}
	if got := parts[3].Get("functionResponse.id").String(); got != "unmatched" {
		t.Fatalf("unmatched result slot moved or changed: %q; output=%s", got, out)
	}
	if got := parts[4].Get("functionResponse.id").String(); got != "call_b" {
		t.Fatalf("second matched result = %q, want call_b; output=%s", got, out)
	}
}

func TestConvertClaudeRequestToAntigravityDeduplicatesRawToolResultIDs(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"call.a","name":"tool_a","input":{}},
				{"type":"tool_use","id":"call_a","name":"tool_b","input":{}}
			]},
			{"role":"user","content":[
				{"type":"text","text":"keep-first"},
				{"type":"tool_result","tool_use_id":"call.a","content":"old"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"IMAGE"}},
				{"type":"tool_result","tool_use_id":"call_a","content":"other"},
				{"type":"tool_result","tool_use_id":"call.a","content":"new"}
			]}
		]
	}`)

	out := convertClaudeRequestToAntigravity("claude-sonnet-4-5", input, false)
	parts := gjson.GetBytes(out, "request.contents.1.parts").Array()
	if len(parts) != 4 {
		t.Fatalf("deduplicated parts count = %d, want 4; output=%s", len(parts), out)
	}
	if got := parts[0].Get("text").String(); got != "keep-first" {
		t.Fatalf("text slot moved or changed: %q; output=%s", got, out)
	}
	if got := parts[1].Get("functionResponse.id").String(); got != "call.a" {
		t.Fatalf("first raw result ID = %q, want call.a; output=%s", got, out)
	}
	if got := parts[1].Get("functionResponse.response.result").String(); got != "new" {
		t.Fatalf("deduplicated result = %q, want new; output=%s", got, out)
	}
	if got := parts[2].Get("inlineData.data").String(); got != "IMAGE" {
		t.Fatalf("image slot moved or changed: %q; output=%s", got, out)
	}
	if got := parts[3].Get("functionResponse.id").String(); got != "call_a" {
		t.Fatalf("distinct raw result ID = %q, want call_a; output=%s", got, out)
	}
}

func TestConvertClaudeRequestToAntigravityKeepsPendingResultsAcrossDeveloperMessage(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","id":"call_a","name":"tool_a","input":{}},
				{"type":"tool_use","id":"call_b","name":"tool_b","input":{}}
			]},
			{"role":"developer","content":"permission notice"},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_b","content":"B"},
				{"type":"tool_result","tool_use_id":"call_a","content":"A"}
			]}
		]
	}`)

	out := convertClaudeRequestToAntigravity("claude-sonnet-4-5", input, false)
	parts := gjson.GetBytes(out, "request.contents.1.parts").Array()
	if len(parts) != 3 {
		t.Fatalf("merged developer/result parts count = %d, want 3; output=%s", len(parts), out)
	}
	if !strings.Contains(parts[0].Get("text").String(), "permission notice") {
		t.Fatalf("developer notice missing; output=%s", out)
	}
	if got := parts[1].Get("functionResponse.id").String(); got != "call_a" {
		t.Fatalf("first result = %q, want call_a; output=%s", got, out)
	}
	if got := parts[2].Get("functionResponse.id").String(); got != "call_b" {
		t.Fatalf("second result = %q, want call_b; output=%s", got, out)
	}
}

func TestConvertClaudeRequestToAntigravityMapsExternalToolNameChain(t *testing.T) {
	input := []byte(`{
		"messages":[
			{"role":"assistant","content":[{
				"type":"tool_use","id":"call_1","name":"external_external_read_file","input":{}
			}]},
			{"role":"user","content":[{
				"type":"tool_result","tool_use_id":"call_1","content":"ok"
			}]}
		],
		"tools":[{"name":"external_external_read_file","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"external_external_read_file"}
	}`)

	out := convertClaudeRequestToAntigravity("gemini-3-flash", input, false)
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
