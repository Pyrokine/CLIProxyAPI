// Package claude provides request translation functionality for Claude API.
// It handles parsing and transforming Claude API requests into the internal client format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package also performs JSON data cleaning and transformation to ensure compatibility
// between Claude API format and the internal client's expected format.
package claude

import (
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// convertClaudeRequestToGeminiDefault parses a Claude API request and returns a complete
// Gemini CLI request body (as JSON bytes) ready to be sent via SendRawMessageStream.
// All JSON transformations are performed using gjson/sjson.
//
// Parameters:
//   - modelName: The name of the model.
//   - rawJSON: The raw JSON request from the Claude API.
//   - stream: A boolean indicating if the request is for a streaming response.
//
// Returns:
//   - []byte: The transformed request in Gemini CLI format.
func convertClaudeRequestToGeminiDefault(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertClaudeRequestToGemini(modelName, inputRawJSON, stream, false)
}

// ConvertClaudeRequestToGeminiWithCompat preserves assistant thinking blocks
// with empty signatures for configured compatibility endpoints.
func ConvertClaudeRequestToGeminiWithCompat(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertClaudeRequestToGemini(modelName, inputRawJSON, stream, true)
}

func convertClaudeRequestToGemini(
	modelName string,
	inputRawJSON []byte,
	stream bool,
	preserveEmptyThinkingBlocks bool,
) []byte {
	body, _ := convertClaudeRequestToGeminiChecked(modelName, inputRawJSON, stream, preserveEmptyThinkingBlocks)
	return body
}

// ConvertClaudeRequestToGeminiWithCompatReturningError reports unsupported user attachments.
func ConvertClaudeRequestToGeminiWithCompatReturningError(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return convertClaudeRequestToGeminiChecked(modelName, inputRawJSON, stream, true)
}

func convertClaudeRequestToGeminiChecked(
	modelName string,
	inputRawJSON []byte,
	_ bool,
	preserveEmptyThinkingBlocks bool,
) ([]byte, error) {
	var drops translatorcommon.UserTurnDrops
	rawJSON := inputRawJSON

	// Build output Gemini CLI request JSON
	out := []byte(`{"contents":[]}`)
	out, _ = sjson.SetBytes(out, "model", modelName)

	// system instruction
	if systemResult := gjson.GetBytes(rawJSON, "system"); systemResult.IsArray() {
		systemInstruction := []byte(`{"role":"user","parts":[]}`)
		hasSystemParts := false
		systemResult.ForEach(
			func(_, systemPromptResult gjson.Result) bool {
				if systemPromptResult.Get("type").String() == "text" {
					textResult := systemPromptResult.Get("text")
					if textResult.Type == gjson.String {
						if util.IsClaudeCodeAttributionSystemText(textResult.String()) {
							return true
						}
						part := []byte(`{"text":""}`)
						part, _ = sjson.SetBytes(part, "text", textResult.String())
						systemInstruction, _ = sjson.SetRawBytes(systemInstruction, "parts.-1", part)
						hasSystemParts = true
					}
				}
				return true
			},
		)
		if hasSystemParts {
			out, _ = sjson.SetRawBytes(out, "systemInstruction", systemInstruction)
		}
	} else if systemResult.Type == gjson.String && !util.IsClaudeCodeAttributionSystemText(systemResult.String()) {
		out, _ = sjson.SetBytes(out, "systemInstruction.parts.-1.text", systemResult.String())
	}

	// contents
	if messagesResult := gjson.GetBytes(rawJSON, "messages"); messagesResult.IsArray() {
		var contentItems [][]byte
		toolNameByID := make(map[string]string)
		var pendingToolUseIDs []string
		userRunStart := 0
		flushUserRun := func() {
			merged := translatorcommon.MergeAdjacentGeminiContents(contentItems[userRunStart:])
			if len(merged) > 0 {
				parts := translatorcommon.AlignGeminiFunctionResponses(gjson.GetBytes(merged[0], "parts"), pendingToolUseIDs)
				merged[0], _ = sjson.SetRawBytes(merged[0], "parts", []byte(parts.Raw))
			}
			contentItems = append(contentItems[:userRunStart], merged...)
			userRunStart = len(contentItems)
		}
		messagesResult.ForEach(
			func(_, messageResult gjson.Result) bool {
				roleResult := messageResult.Get("role")
				if roleResult.Type != gjson.String {
					return true
				}
				originalRole := roleResult.String()
				role := originalRole
				if role == "assistant" {
					role = "model"
				} else if role == "system" || role == "developer" {
					role = "user"
				}
				if role != "user" {
					flushUserRun()
					clear(toolNameByID)
					pendingToolUseIDs = nil
				}

				contentJSON := []byte(`{"role":"","parts":[]}`)
				contentJSON, _ = sjson.SetBytes(contentJSON, "role", role)

				contentsResult := messageResult.Get("content")
				if originalRole == "system" || originalRole == "developer" {
					if reminderText, ok := translatorcommon.ClaudeMessageSystemReminderText(contentsResult); ok {
						part := []byte(`{"text":""}`)
						part, _ = sjson.SetBytes(part, "text", reminderText)
						contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts.-1", part)
						contentItems = append(contentItems, contentJSON)
					}
					return true
				}
				if contentsResult.IsArray() {
					contentsResult.ForEach(
						func(_, contentResult gjson.Result) bool {
							switch contentResult.Get("type").String() {
							case "text":
								text := contentResult.Get("text").String()
								if text == "" {
									return true
								}
								part := []byte(`{"text":""}`)
								part, _ = sjson.SetBytes(part, "text", text)
								contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts.-1", part)

							case "thinking":
								if role != "model" {
									return true
								}
								thinkingText := contentResult.Get("thinking").String()
								thoughtSignature, keep := resolveGeminiThinkingSignature(
									modelName,
									thinkingText,
									contentResult.Get("signature").String(),
									preserveEmptyThinkingBlocks,
								)
								if !keep {
									return true
								}
								part := []byte(`{"text":"","thought":true,"thoughtSignature":""}`)
								part, _ = sjson.SetBytes(part, "text", thinkingText)
								part, _ = sjson.SetBytes(part, "thoughtSignature", thoughtSignature)
								contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts.-1", part)

							case "tool_use":
								functionName := util.SanitizeFunctionName(contentResult.Get("name").String())
								toolUseID := contentResult.Get("id").String()
								functionArgs := contentResult.Get("input").String()
								argsResult := gjson.Parse(functionArgs)
								if argsResult.IsObject() && gjson.Valid(functionArgs) {
									part := []byte(`{"thoughtSignature":"","functionCall":{"name":"","args":{}}}`)
									part, _ = sjson.SetBytes(
										part,
										"thoughtSignature",
										resolveGeminiFunctionCallSignature(contentResult, preserveEmptyThinkingBlocks),
									)
									part, _ = sjson.SetBytes(part, "functionCall.name", functionName)
									if toolUseID != "" {
										part, _ = sjson.SetBytes(part, "functionCall.id", toolUseID)
										if originalRole == "assistant" {
											toolNameByID[toolUseID] = functionName
											pendingToolUseIDs = append(pendingToolUseIDs, toolUseID)
										}
									}
									part, _ = sjson.SetRawBytes(part, "functionCall.args", []byte(functionArgs))
									contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts.-1", part)
								}

							case "tool_result":
								toolCallID := contentResult.Get("tool_use_id").String()
								if toolCallID == "" {
									return true
								}
								funcName := toolNameByID[toolCallID]
								if funcName == "" {
									funcName = toolNameFromClaudeToolUseID(toolCallID)
								}
								if funcName == "" {
									funcName = toolCallID
								}
								result, images := translatorcommon.ClaudeToolResultContent(contentResult.Get("content"))
								part := []byte(`{"functionResponse":{"name":"","response":{"result":""}}}`)
								part, _ = sjson.SetBytes(part, "functionResponse.id", toolCallID)
								part, _ = sjson.SetBytes(part, "functionResponse.name", util.SanitizeFunctionName(funcName))
								part = translatorcommon.SetGeminiFunctionResponseRaw(part, "functionResponse.response.result", result.Raw)
								contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts.-1", part)
								for _, source := range images {
									imagePart := []byte(`{"inline_data":{"mime_type":"","data":""}}`)
									imagePart, _ = sjson.SetBytes(imagePart, "inline_data.mime_type", source.Get("media_type").String())
									imagePart, _ = sjson.SetBytes(imagePart, "inline_data.data", source.Get("data").String())
									contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts.-1", imagePart)
								}

							case "image", "document", "container_upload":
								source := contentResult.Get("source")
								mimeType := source.Get("media_type").String()
								data := source.Get("data").String()
								if source.Get("type").String() == "base64" && mimeType != "" && data != "" {
									part := []byte(`{"inline_data":{"mime_type":"","data":""}}`)
									part, _ = sjson.SetBytes(part, "inline_data.mime_type", mimeType)
									part, _ = sjson.SetBytes(part, "inline_data.data", data)
									contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts.-1", part)
								} else if roleResult.String() == "user" {
									drops.Drop(contentResult.Get("type").String())
								}
							}
							return true
						},
					)
					parts := gjson.GetBytes(contentJSON, "parts").Array()
					if role == "user" {
						rawParts := make([][]byte, 0, len(parts))
						for _, part := range parts {
							rawParts = append(rawParts, []byte(part.Raw))
						}
						contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts", translatorcommon.JoinRawArray(translatorcommon.ReorderGeminiUserParts(rawParts)))
					}
					if roleResult.String() == "user" {
						sendable := 0
						for _, part := range parts {
							if translatorcommon.GeminiPartIsSendable([]byte(part.Raw)) {
								sendable++
							}
						}
						drops.EndTurn(sendable)
					}
					if len(parts) > 0 {
						contentItems = append(contentItems, contentJSON)
					}
				} else if contentsResult.Type == gjson.String {
					part := []byte(`{"text":""}`)
					part, _ = sjson.SetBytes(part, "text", contentsResult.String())
					contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts.-1", part)
					contentItems = append(contentItems, contentJSON)
				}
				if role != "user" {
					userRunStart = len(contentItems)
				}
				return true
			},
		)
		flushUserRun()
		out, _ = sjson.SetRawBytes(out, "contents", translatorcommon.JoinRawArray(contentItems))
	}

	contents := gjson.GetBytes(out, "contents")
	if contents.Exists() && contents.IsArray() {
		arr := contents.Array()
		if len(arr) > 0 {
			last := arr[len(arr)-1]
			if last.Get("role").String() == "model" {
				hasFunctionCall := false
				last.Get("parts").ForEach(
					func(_, part gjson.Result) bool {
						if part.Get("functionCall").Exists() {
							hasFunctionCall = true
							return false
						}
						return true
					},
				)
				if hasFunctionCall {
					out, _ = sjson.DeleteBytes(out, fmt.Sprintf("contents.%d", len(arr)-1))
				}
			}
		}
	}

	// tools
	var toolItems [][]byte
	hasStrictTool := false
	if toolsResult := gjson.GetBytes(rawJSON, "tools"); toolsResult.IsArray() {
		toolsResult.ForEach(
			func(_, toolResult gjson.Result) bool {
				if toolResult.Get("strict").Type == gjson.True {
					hasStrictTool = true
				}
				inputSchemaResult := toolResult.Get("input_schema")
				if inputSchemaResult.Exists() && inputSchemaResult.IsObject() {
					inputSchema := util.CleanJSONSchemaForGeminiJSONSchema(inputSchemaResult.Raw)
					tool := []byte(toolResult.Raw)
					var err error
					tool, err = sjson.DeleteBytes(tool, "input_schema")
					if err != nil {
						return true
					}
					tool, err = sjson.SetRawBytes(tool, "parametersJsonSchema", []byte(inputSchema))
					if err != nil {
						return true
					}
					for _, path := range []string{
						"strict",
						"input_examples",
						"type",
						"cache_control",
						"defer_loading",
						"eager_input_streaming",
					} {
						if toolResult.Get(path).Exists() {
							tool, _ = sjson.DeleteBytes(tool, path)
						}
					}
					nameResult := toolResult.Get("name")
					originalName := nameResult.String()
					sanitizedName := util.SanitizeFunctionName(originalName)
					if nameResult.Type != gjson.String || sanitizedName != originalName {
						tool, _ = sjson.SetBytes(tool, "name", sanitizedName)
					}
					if gjson.ValidBytes(tool) && gjson.ParseBytes(tool).IsObject() {
						toolItems = append(toolItems, tool)
					}
				}
				return true
			},
		)
		if len(toolItems) > 0 {
			tools := []byte(`[{"functionDeclarations":[]}]`)
			tools, _ = sjson.SetRawBytes(
				tools,
				"0.functionDeclarations",
				translatorcommon.JoinRawArray(toolItems),
			)
			out, _ = sjson.SetRawBytes(out, "tools", tools)
		}
	}

	// tool_choice
	toolChoiceResult := gjson.GetBytes(rawJSON, "tool_choice")
	if toolChoiceResult.Exists() && toolChoiceResult.Type != gjson.Null {
		toolChoiceType := ""
		toolChoiceName := ""
		if toolChoiceResult.IsObject() {
			toolChoiceType = toolChoiceResult.Get("type").String()
			toolChoiceName = toolChoiceResult.Get("name").String()
		} else if toolChoiceResult.Type == gjson.String {
			toolChoiceType = toolChoiceResult.String()
		}

		switch toolChoiceType {
		case "auto":
			if hasStrictTool {
				out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "VALIDATED")
			} else {
				out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "AUTO")
			}
		case "none":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "NONE")
		case "any":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "ANY")
		case "tool":
			out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "ANY")
			if toolChoiceName != "" {
				out, _ = sjson.SetBytes(
					out,
					"toolConfig.functionCallingConfig.allowedFunctionNames",
					[]string{util.SanitizeFunctionName(toolChoiceName)},
				)
			}
		}
	} else if hasStrictTool && len(toolItems) > 0 {
		out, _ = sjson.SetBytes(out, "toolConfig.functionCallingConfig.mode", "VALIDATED")
	}

	// Map Anthropic thinking -> Gemini thinking config when enabled
	// Translator only does format conversion, ApplyThinking handles model capability validation.
	if t := gjson.GetBytes(rawJSON, "thinking"); t.Exists() && t.IsObject() {
		switch t.Get("type").String() {
		case "enabled":
			if b := t.Get("budget_tokens"); b.Exists() && b.Type == gjson.Number {
				budget := int(b.Int())
				out, _ = sjson.SetBytes(out, "generationConfig.thinkingConfig.thinkingBudget", budget)
			}
		case "adaptive", "auto":
			// For adaptive thinking:
			// - If output_config.effort is explicitly present, pass through as thinkingLevel.
			// - Otherwise, treat it as "enabled with target-model maximum" and emit thinkingBudget=max.
			// ApplyThinking handles clamping to target model's supported levels.
			effort := ""
			if v := gjson.GetBytes(rawJSON, "output_config.effort"); v.Exists() && v.Type == gjson.String {
				effort = strings.ToLower(strings.TrimSpace(v.String()))
			}
			if effort != "" {
				out, _ = sjson.SetBytes(out, "generationConfig.thinkingConfig.thinkingLevel", effort)
			} else {
				maxBudget := 0
				if mi := registry.LookupModelInfo(modelName, "gemini"); mi != nil && mi.Thinking != nil {
					maxBudget = mi.Thinking.Max
				}
				if maxBudget > 0 {
					out, _ = sjson.SetBytes(out, "generationConfig.thinkingConfig.thinkingBudget", maxBudget)
				} else {
					out, _ = sjson.SetBytes(out, "generationConfig.thinkingConfig.thinkingLevel", "high")
				}
			}
		}
	}
	if v := gjson.GetBytes(rawJSON, "temperature"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.temperature", v.Num)
	}
	if v := gjson.GetBytes(rawJSON, "top_p"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.topP", v.Num)
	}
	if v := gjson.GetBytes(rawJSON, "top_k"); v.Exists() && v.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "generationConfig.topK", v.Num)
	}

	return common.AttachDefaultSafetySettings(out, "safetySettings"), drops.Err()
}

func resolveGeminiThinkingSignature(
	modelName, thinkingText, rawSignature string,
	compatibilityMode bool,
) (string, bool) {
	if strings.TrimSpace(rawSignature) == "" && thinkingText != "" {
		rawSignature = cache.GetCachedSignature(modelName, thinkingText)
	}
	if strings.TrimSpace(rawSignature) == "" {
		return "", compatibilityMode
	}

	if compatibilityMode {
		if normalized, ok := signature.CompatibleSignatureForProviderWithOptions(
			signature.ProviderGemini,
			rawSignature,
			signature.BlockKindGeminiModelPart,
			signature.SignatureCompatibilityOptions{PreserveUnknownNonEmpty: true},
		); ok {
			return normalized, true
		}
		return signature.GeminiReplaySignatureOrBypass(rawSignature, signature.BlockKindGeminiModelPart), true
	}

	return signature.CompatibleSignatureForProvider(
		signature.ProviderGemini,
		rawSignature,
		signature.BlockKindGeminiModelPart,
	)
}

func resolveGeminiFunctionCallSignature(content gjson.Result, compatibilityMode bool) string {
	rawSignature := ""
	for _, path := range []string{
		"signature",
		"thought_signature",
		"extra_content.google.thought_signature",
	} {
		if value := strings.TrimSpace(content.Get(path).String()); value != "" {
			rawSignature = value
			break
		}
	}
	if compatibilityMode {
		if normalized, ok := signature.CompatibleSignatureForProviderWithOptions(
			signature.ProviderGemini,
			rawSignature,
			signature.BlockKindGeminiFunctionCall,
			signature.SignatureCompatibilityOptions{PreserveUnknownNonEmpty: true},
		); ok {
			return normalized
		}
	}
	return signature.GeminiReplaySignatureOrBypass(rawSignature, signature.BlockKindGeminiFunctionCall)
}

func toolNameFromClaudeToolUseID(toolUseID string) string {
	parts := strings.Split(toolUseID, "-")
	if len(parts) <= 1 {
		return ""
	}
	return strings.Join(parts[0:len(parts)-1], "-")
}
