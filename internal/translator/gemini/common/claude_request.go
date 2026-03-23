package common

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ClaudeRequestConfig controls the behavioral differences between Gemini API variants
// when converting Claude requests.
type ClaudeRequestConfig struct {
	// BaseEnvelope is the initial JSON skeleton for the output.
	BaseEnvelope string
	// ModelPath is the JSON path to set the model name (e.g., "model").
	ModelPath string
	// ContentsPath is the path for contents array (e.g., "request.contents" or "contents").
	ContentsPath string
	// SystemInstructionPath is the path for system instruction (e.g., "request.systemInstruction" or "system_instruction").
	SystemInstructionPath string
	// ToolsPath is the path for tools array (e.g., "request.tools" or "tools").
	ToolsPath string
	// GenConfigPath is the path for generationConfig (e.g., "request.generationConfig" or "generationConfig").
	GenConfigPath string
	// SafetyPath is the path for safetySettings (e.g., "request.safetySettings" or "safetySettings").
	SafetyPath string
	// SupportImage enables image content type handling.
	SupportImage bool
}

// ConvertClaudeRequest converts a Claude API request into a Gemini-family request
// using the given config to control path layout.
func ConvertClaudeRequest(modelName string, inputRawJSON []byte, cfg *ClaudeRequestConfig) []byte {
	rawJSON := inputRawJSON
	rawJSON = bytes.Replace(
		rawJSON, []byte(`"url":{"type":"string","format":"uri",`), []byte(`"url":{"type":"string",`), -1,
	)

	out := cfg.BaseEnvelope
	out, _ = sjson.Set(out, cfg.ModelPath, modelName)

	// system instruction
	out = convertClaudeSystemInstruction(rawJSON, out, cfg.SystemInstructionPath)

	// contents
	out = convertClaudeMessages(rawJSON, out, cfg.ContentsPath, cfg.SupportImage)

	// tools
	out = convertClaudeTools(rawJSON, out, cfg.ToolsPath)

	// thinking config
	out = convertClaudeThinking(rawJSON, out, cfg.GenConfigPath)

	// generation parameters
	out = convertClaudeGenParams(rawJSON, out, cfg.GenConfigPath)

	outBytes := []byte(out)
	outBytes = AttachDefaultSafetySettings(outBytes, cfg.SafetyPath)
	return outBytes
}

// convertClaudeSystemInstruction converts Claude system instructions to Gemini format.
func convertClaudeSystemInstruction(rawJSON []byte, out string, sysPath string) string {
	systemResult := gjson.GetBytes(rawJSON, "system")
	if systemResult.IsArray() {
		systemInstruction := `{"role":"user","parts":[]}`
		hasSystemParts := false
		systemResult.ForEach(
			func(_, systemPromptResult gjson.Result) bool {
				if systemPromptResult.Get("type").String() == "text" {
					textResult := systemPromptResult.Get("text")
					if textResult.Type == gjson.String {
						part := `{"text":""}`
						part, _ = sjson.Set(part, "text", textResult.String())
						systemInstruction, _ = sjson.SetRaw(systemInstruction, "parts.-1", part)
						hasSystemParts = true
					}
				}
				return true
			},
		)
		if hasSystemParts {
			out, _ = sjson.SetRaw(out, sysPath, systemInstruction)
		}
	} else if systemResult.Type == gjson.String {
		out, _ = sjson.Set(out, sysPath+".parts.-1.text", systemResult.String())
	}
	return out
}

// convertClaudeMessages converts Claude messages to Gemini contents.
func convertClaudeMessages(rawJSON []byte, out string, contentsPath string, supportImage bool) string {
	messagesResult := gjson.GetBytes(rawJSON, "messages")
	if !messagesResult.IsArray() {
		return out
	}

	messagesResult.ForEach(
		func(_, messageResult gjson.Result) bool {
			roleResult := messageResult.Get("role")
			if roleResult.Type != gjson.String {
				return true
			}
			role := roleResult.String()
			if role == "assistant" {
				role = "model"
			}

			contentJSON := `{"role":"","parts":[]}`
			contentJSON, _ = sjson.Set(contentJSON, "role", role)

			contentsResult := messageResult.Get("content")
			if contentsResult.IsArray() {
				contentsResult.ForEach(
					func(_, contentResult gjson.Result) bool {
						contentJSON = convertClaudeContentPart(contentResult, contentJSON, supportImage)
						return true
					},
				)
				out, _ = sjson.SetRaw(out, contentsPath+".-1", contentJSON)
			} else if contentsResult.Type == gjson.String {
				part := `{"text":""}`
				part, _ = sjson.Set(part, "text", contentsResult.String())
				contentJSON, _ = sjson.SetRaw(contentJSON, "parts.-1", part)
				out, _ = sjson.SetRaw(out, contentsPath+".-1", contentJSON)
			}
			return true
		},
	)
	return out
}

// convertClaudeContentPart converts a single Claude content part to Gemini format.
func convertClaudeContentPart(contentResult gjson.Result, contentJSON string, supportImage bool) string {
	switch contentResult.Get("type").String() {
	case "text":
		part := `{"text":""}`
		part, _ = sjson.Set(part, "text", contentResult.Get("text").String())
		contentJSON, _ = sjson.SetRaw(contentJSON, "parts.-1", part)

	case "tool_use":
		functionName := contentResult.Get("name").String()
		functionArgs := contentResult.Get("input").String()
		argsResult := gjson.Parse(functionArgs)
		if argsResult.IsObject() && gjson.Valid(functionArgs) {
			part := `{"thoughtSignature":"","functionCall":{"name":"","args":{}}}`
			part, _ = sjson.Set(part, "thoughtSignature", functionThoughtSignature)
			part, _ = sjson.Set(part, "functionCall.name", functionName)
			part, _ = sjson.SetRaw(part, "functionCall.args", functionArgs)
			contentJSON, _ = sjson.SetRaw(contentJSON, "parts.-1", part)
		}

	case "tool_result":
		toolCallID := contentResult.Get("tool_use_id").String()
		if toolCallID == "" {
			return contentJSON
		}
		funcName := toolCallID
		toolCallIDs := strings.Split(toolCallID, "-")
		if len(toolCallIDs) > 1 {
			funcName = strings.Join(toolCallIDs[0:len(toolCallIDs)-1], "-")
		}
		responseData := contentResult.Get("content").Raw
		part := `{"functionResponse":{"name":"","response":{"result":""}}}`
		part, _ = sjson.Set(part, "functionResponse.name", funcName)
		part, _ = sjson.Set(part, "functionResponse.response.result", responseData)
		contentJSON, _ = sjson.SetRaw(contentJSON, "parts.-1", part)

	case "image":
		if supportImage {
			source := contentResult.Get("source")
			if source.Get("type").String() == "base64" {
				mimeType := source.Get("media_type").String()
				data := source.Get("data").String()
				if mimeType != "" && data != "" {
					part := `{"inlineData":{"mime_type":"","data":""}}`
					part, _ = sjson.Set(part, "inlineData.mime_type", mimeType)
					part, _ = sjson.Set(part, "inlineData.data", data)
					contentJSON, _ = sjson.SetRaw(contentJSON, "parts.-1", part)
				}
			}
		}
	}
	return contentJSON
}

// convertClaudeTools converts Claude tools to Gemini functionDeclarations.
func convertClaudeTools(rawJSON []byte, out string, toolsPath string) string {
	toolsResult := gjson.GetBytes(rawJSON, "tools")
	if !toolsResult.IsArray() {
		return out
	}

	hasTools := false
	toolsResult.ForEach(
		func(_, toolResult gjson.Result) bool {
			inputSchemaResult := toolResult.Get("input_schema")
			if !inputSchemaResult.Exists() || !inputSchemaResult.IsObject() {
				return true
			}
			inputSchema := inputSchemaResult.Raw
			tool, _ := sjson.Delete(toolResult.Raw, "input_schema")
			tool, _ = sjson.SetRaw(tool, "parametersJsonSchema", inputSchema)
			tool, _ = sjson.Delete(tool, "strict")
			tool, _ = sjson.Delete(tool, "input_examples")
			tool, _ = sjson.Delete(tool, "type")
			tool, _ = sjson.Delete(tool, "cache_control")
			if gjson.Valid(tool) && gjson.Parse(tool).IsObject() {
				if !hasTools {
					out, _ = sjson.SetRaw(out, toolsPath, `[{"functionDeclarations":[]}]`)
					hasTools = true
				}
				out, _ = sjson.SetRaw(out, toolsPath+".0.functionDeclarations.-1", tool)
			}
			return true
		},
	)
	if !hasTools {
		out, _ = sjson.Delete(out, toolsPath)
	}
	return out
}

// convertClaudeThinking maps Anthropic thinking configuration to Gemini thinkingConfig.
func convertClaudeThinking(rawJSON []byte, out string, genConfigPath string) string {
	t := gjson.GetBytes(rawJSON, "thinking")
	if !t.Exists() || !t.IsObject() {
		return out
	}

	thinkingPath := genConfigPath + ".thinkingConfig"
	switch t.Get("type").String() {
	case "enabled":
		if b := t.Get("budget_tokens"); b.Exists() && b.Type == gjson.Number {
			budget := int(b.Int())
			out, _ = sjson.Set(out, thinkingPath+".thinkingBudget", budget)
			out, _ = sjson.Set(out, thinkingPath+".includeThoughts", true)
		}
	case "adaptive", "auto":
		// Keep adaptive/auto as a high level sentinel; ApplyThinking resolves it
		// to model-specific max capability.
		out, _ = sjson.Set(out, thinkingPath+".thinkingLevel", "high")
		out, _ = sjson.Set(out, thinkingPath+".includeThoughts", true)
	}
	return out
}

// convertClaudeGenParams maps Claude generation parameters to Gemini format.
func convertClaudeGenParams(rawJSON []byte, out string, genConfigPath string) string {
	if v := gjson.GetBytes(rawJSON, "temperature"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.Set(out, genConfigPath+".temperature", v.Num)
	}
	if v := gjson.GetBytes(rawJSON, "top_p"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.Set(out, genConfigPath+".topP", v.Num)
	}
	if v := gjson.GetBytes(rawJSON, "top_k"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.Set(out, genConfigPath+".topK", v.Num)
	}
	return out
}
