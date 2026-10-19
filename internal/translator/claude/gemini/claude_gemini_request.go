// Package gemini provides request translation functionality for Gemini to Claude Code API compatibility.
// It handles parsing and transforming Gemini API requests into Claude Code API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Gemini API format and Claude Code API's expected format.
package gemini

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToClaude parses and transforms a Gemini API request into Claude Code API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Claude Code API.
// The function performs comprehensive transformation including:
// 1. Model name mapping and generation configuration extraction
// 2. System instruction conversion to Claude Code format
// 3. Message content conversion with proper role mapping
// 4. Tool call and tool result handling with FIFO queue for ID matching
// 5. Image and file data conversion to Claude Code base64 format
// 6. Tool declaration and tool choice configuration mapping
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the Gemini API
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in Claude Code API format
func ConvertGeminiRequestToClaude(modelName string, inputRawJSON []byte, stream bool) []byte {
	out, err := ConvertGeminiRequestToClaudeChecked(modelName, inputRawJSON, stream)
	if err != nil {
		log.Warnf("gemini to claude request rejected: %v", err)
		return nil
	}
	return out
}

func ConvertGeminiRequestToClaudeChecked(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	rawJSON := inputRawJSON
	var drops translatorcommon.UserTurnDrops

	// Base Claude message payload
	out := []byte(`{"model":"","max_tokens":32000,"messages":[],"metadata":{}}`)
	out, _ = sjson.SetBytes(out, "metadata.user_id", deriveClaudeMetadataUserID(modelName, rawJSON))

	root := gjson.ParseBytes(rawJSON)
	toolNames := util.ClaudeFunctionNameMap(rawJSON)
	mapToolName := func(name string) string {
		if mapped := toolNames[name]; mapped != "" {
			return mapped
		}
		return util.SanitizeClaudeFunctionName(name)
	}
	messageAccumulator := translatorcommon.NewClaudeMessageAccumulator(
		int(root.Get("contents.#").Int()) + 1,
	)

	var toolCalls translatorcommon.GeminiToolCallQueue
	toolCallCounter := 0
	newToolCallID := func() string {
		toolCallCounter++
		return fmt.Sprintf("toolu_gemini_%016d", toolCallCounter)
	}

	// Model mapping to specify which Claude Code model to use
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Generation config extraction from Gemini format
	if genConfig := root.Get("generationConfig"); genConfig.Exists() {
		// Max output tokens configuration
		if maxTokens := genConfig.Get("maxOutputTokens"); maxTokens.Exists() {
			out, _ = sjson.SetBytes(out, "max_tokens", maxTokens.Int())
		}
		// Top P setting for nucleus sampling.
		if topP := genConfig.Get("topP"); topP.Exists() {
			out, _ = sjson.SetBytes(out, "top_p", topP.Float())
		}
		// Stop sequences configuration for custom termination conditions
		if stopSeqs := genConfig.Get("stopSequences"); stopSeqs.Exists() && stopSeqs.IsArray() {
			var stopSequences []string
			stopSeqs.ForEach(
				func(_, value gjson.Result) bool {
					stopSequences = append(stopSequences, value.String())
					return true
				},
			)
			if len(stopSequences) > 0 {
				out, _ = sjson.SetBytes(out, "stop_sequences", stopSequences)
			}
		}
		// Include thoughts configuration for reasoning process visibility
		// Translator only does format conversion, ApplyThinking handles model capability validation.
		if thinkingConfig := genConfig.Get("thinkingConfig"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
			mi := registry.LookupModelInfo(modelName, "claude")
			supportsAdaptive := mi != nil && mi.Thinking != nil && len(mi.Thinking.Levels) > 0
			supportsMax := supportsAdaptive && thinking.HasLevel(mi.Thinking.Levels, string(thinking.LevelMax))

			// MapToClaudeEffort normalizes levels (e.g. minimal→low, xhigh→high) to avoid
			// validation errors since validate treats same-provider unsupported levels as errors.
			thinkingLevel := thinkingConfig.Get("thinkingLevel")
			if !thinkingLevel.Exists() {
				thinkingLevel = thinkingConfig.Get("thinking_level")
			}
			if thinkingLevel.Exists() {
				level := strings.ToLower(strings.TrimSpace(thinkingLevel.String()))
				if supportsAdaptive {
					switch level {
					case "":
					case "none":
						out, _ = sjson.SetBytes(out, "thinking.type", "disabled")
						out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
						out, _ = sjson.DeleteBytes(out, "output_config.effort")
					default:
						if mapped, ok := thinking.MapToClaudeEffort(level, supportsMax); ok {
							level = mapped
						}
						out, _ = sjson.SetBytes(out, "thinking.type", "adaptive")
						out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
						out, _ = sjson.SetBytes(out, "output_config.effort", level)
					}
				} else {
					switch level {
					case "":
					case "none":
						out, _ = sjson.SetBytes(out, "thinking.type", "disabled")
						out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
					case "auto":
						out, _ = sjson.SetBytes(out, "thinking.type", "enabled")
						out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
					default:
						if budget, ok := thinking.ConvertLevelToBudget(level); ok {
							out, _ = sjson.SetBytes(out, "thinking.type", "enabled")
							out, _ = sjson.SetBytes(out, "thinking.budget_tokens", budget)
						}
					}
				}
			} else {
				thinkingBudget := thinkingConfig.Get("thinkingBudget")
				if !thinkingBudget.Exists() {
					thinkingBudget = thinkingConfig.Get("thinking_budget")
				}
				if thinkingBudget.Exists() {
					budget := int(thinkingBudget.Int())
					if supportsAdaptive {
						switch budget {
						case 0:
							out, _ = sjson.SetBytes(out, "thinking.type", "disabled")
							out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
							out, _ = sjson.DeleteBytes(out, "output_config.effort")
						default:
							level, ok := thinking.ConvertBudgetToLevel(budget)
							if ok {
								if mapped, okM := thinking.MapToClaudeEffort(level, supportsMax); okM {
									level = mapped
								}
								out, _ = sjson.SetBytes(out, "thinking.type", "adaptive")
								out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
								out, _ = sjson.SetBytes(out, "output_config.effort", level)
							}
						}
					} else {
						switch budget {
						case 0:
							out, _ = sjson.SetBytes(out, "thinking.type", "disabled")
							out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
						case -1:
							out, _ = sjson.SetBytes(out, "thinking.type", "enabled")
							out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
						default:
							out, _ = sjson.SetBytes(out, "thinking.type", "enabled")
							out, _ = sjson.SetBytes(out, "thinking.budget_tokens", budget)
						}
					}
				}
			}
		}
	}

	// System instruction conversion to Claude Code format
	sysInstr := root.Get("systemInstruction")
	if !sysInstr.Exists() {
		sysInstr = root.Get("system_instruction")
	}
	if sysInstr.Exists() {
		if parts := sysInstr.Get("parts"); parts.Exists() && parts.IsArray() {
			var systemText strings.Builder
			parts.ForEach(
				func(_, part gjson.Result) bool {
					if translatorcommon.IsGeminiThoughtPart(part) {
						return true
					}
					if text := part.Get("text"); text.Exists() {
						if systemText.Len() > 0 {
							systemText.WriteString("\n")
						}
						systemText.WriteString(text.String())
					}
					return true
				},
			)
			if systemText.Len() > 0 {
				// Create system message in Claude Code format
				systemMessage := []byte(`{"role":"user","content":[{"type":"text","text":""}]}`)
				systemMessage, _ = sjson.SetBytes(systemMessage, "content.0.text", systemText.String())
				messageAccumulator.Append(systemMessage)
				messageAccumulator.Flush()
			}
		}
	}

	// Contents conversion to messages with proper role mapping
	if contents := root.Get("contents"); contents.Exists() && contents.IsArray() {
		contents.ForEach(
			func(_, content gjson.Result) bool {
				role := content.Get("role").String()
				// Map Gemini roles to Claude Code roles
				if role == "model" {
					role = "assistant"
				}

				if role == "function" {
					role = "user"
				}

				if role == "tool" {
					role = "user"
				}

				sendable := 0
				// Create message structure in Claude Code format
				msg := []byte(`{"role":"","content":[]}`)
				msg, _ = sjson.SetBytes(msg, "role", role)

				if parts := content.Get("parts"); parts.Exists() && parts.IsArray() {
					parts.ForEach(
						func(_, part gjson.Result) bool {
							if translatorcommon.IsGeminiThoughtPart(part) {
								return true
							}

							// Text content conversion
							if text := part.Get("text"); text.Exists() {
								textContent := []byte(`{"type":"text","text":""}`)
								textContent, _ = sjson.SetBytes(textContent, "text", text.String())
								msg, _ = sjson.SetRawBytes(msg, "content.-1", textContent)
								if strings.TrimSpace(text.String()) != "" {
									sendable++
								}
								return true
							}

							// Function call (from model/assistant) conversion to tool use
							if fc := part.Get("functionCall"); fc.Exists() && role == "assistant" {
								toolUse := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)

								functionName := fc.Get("name").String()
								toolID := translatorcommon.GeminiToolCallID(fc)
								if toolID == "" {
									toolID = newToolCallID()
								}
								toolCalls.Add(functionName, toolID)
								toolUse, _ = sjson.SetBytes(toolUse, "id", toolID)
								toolUse, _ = sjson.SetBytes(toolUse, "name", mapToolName(functionName))
								if args := fc.Get("args"); args.Exists() && args.IsObject() {
									toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte(args.Raw))
								}
								msg, _ = sjson.SetRawBytes(msg, "content.-1", toolUse)
								sendable++
								return true
							}

							// Function response (from user) conversion to tool result
							if fr := part.Get("functionResponse"); fr.Exists() {
								toolResult := []byte(`{"type":"tool_result","tool_use_id":"","content":""}`)

								functionName := fr.Get("name").String()
								toolID := toolCalls.Match(functionName, translatorcommon.GeminiToolCallID(fr))
								if toolID == "" {
									toolID = newToolCallID()
								}
								toolResult, _ = sjson.SetBytes(toolResult, "tool_use_id", toolID)

								// Extract result content from the function response
								if result := fr.Get("response.result"); result.Exists() {
									toolResult, _ = sjson.SetBytes(toolResult, "content", result.String())
								} else if response := fr.Get("response"); response.Exists() {
									toolResult, _ = sjson.SetBytes(toolResult, "content", response.Raw)
								}
								msg, _ = sjson.SetRawBytes(msg, "content.-1", toolResult)
								sendable++
								return true
							}

							// Inline data conversion to Claude Code content format
							if inlineData := geminiClaudeInlineData(part); inlineData.Exists() {
								if contentPart, ok := claudeContentPartFromGeminiInlineData(inlineData, role == "assistant"); ok {
									msg, _ = sjson.SetRawBytes(msg, "content.-1", contentPart)
									sendable++
								} else if role != "assistant" {
									drops.Drop("inlineData")
								}
								return true
							}

							// File data conversion to Claude Code content format
							if fileData := geminiClaudeFileData(part); fileData.Exists() {
								if contentPart, ok := claudeContentPartFromGeminiFileData(fileData, role == "assistant"); ok {
									msg, _ = sjson.SetRawBytes(msg, "content.-1", contentPart)
									sendable++
								} else if role != "assistant" {
									drops.Drop("fileData")
								}
								return true
							}

							return true
						},
					)
				}

				if role != "assistant" {
					drops.EndTurn(sendable)
				}

				// Only add message if it has content
				if contentArray := gjson.GetBytes(
					msg, "content",
				); contentArray.Exists() && len(contentArray.Array()) > 0 {
					messageAccumulator.Append(msg)
				}

				return true
			},
		)
	}
	out = translatorcommon.SetRawArrayItems(out, "messages", messageAccumulator.Messages())

	// Tools mapping: Gemini functionDeclarations -> Claude Code tools
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		var anthropicTools []any

		tools.ForEach(
			func(_, tool gjson.Result) bool {
				if funcDecls := tool.Get("functionDeclarations"); funcDecls.Exists() && funcDecls.IsArray() {
					funcDecls.ForEach(
						func(_, funcDecl gjson.Result) bool {
							anthropicTool := []byte(`{"name":"","description":"","input_schema":{"type":"object","properties":{}}}`)

							if name := funcDecl.Get("name"); name.Exists() {
								anthropicTool, _ = sjson.SetBytes(anthropicTool, "name", mapToolName(name.String()))
							}
							if desc := funcDecl.Get("description"); desc.Exists() {
								anthropicTool, _ = sjson.SetBytes(anthropicTool, "description", desc.String())
							}
							if params := funcDecl.Get("parameters"); params.Exists() {
								cleaned := normalizeClaudeToolSchema(params)
								anthropicTool, _ = sjson.SetRawBytes(anthropicTool, "input_schema", cleaned)
							} else if params = funcDecl.Get("parametersJsonSchema"); params.Exists() {
								cleaned := normalizeClaudeToolSchema(params)
								anthropicTool, _ = sjson.SetRawBytes(anthropicTool, "input_schema", cleaned)
							}

							anthropicTool = lowercaseClaudeToolSchemaTypes(anthropicTool)
							anthropicTools = append(anthropicTools, gjson.ParseBytes(anthropicTool).Value())
							return true
						},
					)
				}
				return true
			},
		)

		if len(anthropicTools) > 0 {
			out, _ = sjson.SetBytes(out, "tools", anthropicTools)
		}
	}

	// Tool config mapping from Gemini format to Claude Code format
	toolConfig := root.Get("toolConfig")
	if !toolConfig.Exists() {
		toolConfig = root.Get("tool_config")
	}
	funcCalling := toolConfig.Get("functionCallingConfig")
	if !funcCalling.Exists() {
		funcCalling = toolConfig.Get("function_calling_config")
	}
	switch funcCalling.Get("mode").String() {
	case "AUTO":
		out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"auto"}`))
	case "NONE":
		out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"none"}`))
	case "ANY":
		allowedNames := funcCalling.Get("allowedFunctionNames")
		if !allowedNames.Exists() {
			allowedNames = funcCalling.Get("allowed_function_names")
		}
		if names := allowedNames.Array(); allowedNames.IsArray() && len(names) == 1 {
			choice := []byte(`{"type":"tool","name":""}`)
			choice, _ = sjson.SetBytes(choice, "name", mapToolName(names[0].String()))
			out, _ = sjson.SetRawBytes(out, "tool_choice", choice)
		} else {
			out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"any"}`))
		}
	}

	// Stream setting configuration
	out, _ = sjson.SetBytes(out, "stream", stream)

	return out, drops.Err()
}

func deriveClaudeMetadataUserID(modelName string, rawJSON []byte) string {
	root := gjson.ParseBytes(rawJSON)
	if userID := root.Get("metadata.user_id"); userID.Type == gjson.String && strings.TrimSpace(userID.String()) != "" {
		return userID.String()
	}

	var seed string
	for _, path := range []string{
		"prompt_cache_key",
		"promptCacheKey",
		"session_id",
		"sessionId",
		"metadata.session_id",
		"metadata.sessionId",
		"metadata.canonical_session_id",
		"canonical_session_id",
	} {
		if value := strings.TrimSpace(root.Get(path).String()); value != "" {
			seed = path + "\x00" + value
			break
		}
	}

	if seed == "" {
		conversation := root.Get("conversation")
		switch {
		case strings.TrimSpace(conversation.Get("id").String()) != "":
			seed = "conversation.id\x00" + strings.TrimSpace(conversation.Get("id").String())
		case conversation.Type == gjson.String && strings.TrimSpace(conversation.String()) != "":
			seed = "conversation\x00" + strings.TrimSpace(conversation.String())
		case strings.TrimSpace(root.Get("conversation_id").String()) != "":
			seed = "conversation_id\x00" + strings.TrimSpace(root.Get("conversation_id").String())
		}
	}

	if seed == "" {
		for _, content := range root.Get("contents").Array() {
			role := strings.ToLower(strings.TrimSpace(content.Get("role").String()))
			if role != "" && role != "user" {
				continue
			}
			var text strings.Builder
			for _, part := range content.Get("parts").Array() {
				if translatorcommon.IsGeminiThoughtPart(part) {
					continue
				}
				if value := strings.TrimSpace(part.Get("text").String()); value != "" {
					if text.Len() > 0 {
						text.WriteByte('\n')
					}
					text.WriteString(value)
				}
			}
			if text.Len() > 0 {
				seed = "content\x00" + text.String()
				break
			}
		}
	}

	if seed == "" {
		seed = "model\x00" + strings.TrimSpace(modelName)
		if systemInstruction := root.Get("system_instruction"); systemInstruction.Exists() {
			seed += "\x00system_instruction\x00" + systemInstruction.Raw
		} else if systemInstruction = root.Get("systemInstruction"); systemInstruction.Exists() {
			seed += "\x00systemInstruction\x00" + systemInstruction.Raw
		}
	}
	if strings.Trim(seed, "\x00") == "model" {
		return "unknown"
	}

	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func geminiClaudeInlineData(part gjson.Result) gjson.Result {
	inlineData := part.Get("inlineData")
	if inlineData.Exists() {
		return inlineData
	}
	return part.Get("inline_data")
}

func geminiClaudeFileData(part gjson.Result) gjson.Result {
	fileData := part.Get("fileData")
	if fileData.Exists() {
		return fileData
	}
	return part.Get("file_data")
}

func claudeContentPartFromGeminiInlineData(inlineData gjson.Result, keepPlaceholder bool) ([]byte, bool) {
	mimeType := inlineData.Get("mimeType").String()
	if mimeType == "" {
		mimeType = inlineData.Get("mime_type").String()
	}
	data := inlineData.Get("data").String()
	if mimeType == "" || data == "" {
		return nil, false
	}
	lowerMIMEType := strings.ToLower(mimeType)
	switch {
	case strings.HasPrefix(lowerMIMEType, "image/"):
		imageContent := []byte(`{"type":"image","source":{"type":"base64","media_type":"","data":""}}`)
		imageContent, _ = sjson.SetBytes(imageContent, "source.media_type", mimeType)
		imageContent, _ = sjson.SetBytes(imageContent, "source.data", data)
		return imageContent, true
	case strings.HasPrefix(lowerMIMEType, "application/"), strings.HasPrefix(lowerMIMEType, "text/"):
		documentContent := []byte(`{"type":"document","source":{"type":"base64","media_type":"","data":""}}`)
		documentContent, _ = sjson.SetBytes(documentContent, "source.media_type", mimeType)
		documentContent, _ = sjson.SetBytes(documentContent, "source.data", data)
		return documentContent, true
	default:
		if !keepPlaceholder {
			return nil, false
		}
		return claudeTextContentPart(fmt.Sprintf("Media content: inline data (Type: %s)", mimeType)), true
	}
}

func claudeContentPartFromGeminiFileData(fileData gjson.Result, keepPlaceholder bool) ([]byte, bool) {
	fileURI := fileData.Get("fileUri").String()
	if fileURI == "" {
		fileURI = fileData.Get("file_uri").String()
	}
	if fileURI == "" {
		return nil, false
	}
	mimeType := fileData.Get("mimeType").String()
	if mimeType == "" {
		mimeType = fileData.Get("mime_type").String()
	}
	lowerMIMEType := strings.ToLower(mimeType)
	switch {
	case strings.HasPrefix(lowerMIMEType, "image/"):
		imageContent := []byte(`{"type":"image","source":{"type":"url","url":""}}`)
		imageContent, _ = sjson.SetBytes(imageContent, "source.url", fileURI)
		return imageContent, true
	case strings.HasPrefix(lowerMIMEType, "application/"), strings.HasPrefix(lowerMIMEType, "text/"):
		documentContent := []byte(`{"type":"document","source":{"type":"url","url":""}}`)
		documentContent, _ = sjson.SetBytes(documentContent, "source.url", fileURI)
		if mimeType != "" {
			documentContent, _ = sjson.SetBytes(documentContent, "source.media_type", mimeType)
		}
		return documentContent, true
	default:
		if !keepPlaceholder {
			return nil, false
		}
		fileInfo := "File: " + fileURI
		if mimeType != "" {
			fileInfo += " (Type: " + mimeType + ")"
		}
		return claudeTextContentPart(fileInfo), true
	}
}

func claudeTextContentPart(text string) []byte {
	textContent := []byte(`{"type":"text","text":""}`)
	textContent, _ = sjson.SetBytes(textContent, "text", text)
	return textContent
}

func normalizeClaudeToolSchema(parameters gjson.Result) []byte {
	cleaned := []byte(parameters.Raw)
	if !parameters.IsObject() ||
		parameters.Get("type").String() != "object" ||
		!parameters.Get("properties").IsObject() ||
		parameters.Get("anyOf").Exists() ||
		parameters.Get("oneOf").Exists() ||
		parameters.Get("allOf").Exists() {
		wrapped := []byte(`{"input_schema":{}}`)
		if normalizedWrapped, errSet := sjson.SetRawBytes(wrapped, "input_schema", cleaned); errSet == nil {
			wrapped = lowercaseClaudeToolSchemaTypes(normalizedWrapped)
			cleaned = []byte(gjson.GetBytes(wrapped, "input_schema").Raw)
		}
		cleaned = util.NormalizeClaudeToolInputSchema(cleaned)
	}

	normalized := gjson.ParseBytes(cleaned)
	if normalized.Get("additionalProperties").Type != gjson.False {
		cleaned, _ = sjson.SetBytes(cleaned, "additionalProperties", false)
	}
	const schema = "http://json-schema.org/draft-07/schema#"
	currentSchema := normalized.Get("$schema")
	if currentSchema.Type != gjson.String || currentSchema.String() != schema {
		cleaned, _ = sjson.SetBytes(cleaned, "$schema", schema)
	}
	return cleaned
}

func lowercaseClaudeToolSchemaTypes(tool []byte) []byte {
	var pathsToLower []string
	util.Walk(gjson.ParseBytes(tool), "", "type", &pathsToLower)
	for _, path := range pathsToLower {
		typeValue := gjson.GetBytes(tool, path)
		normalizedType := strings.ToLower(typeValue.String())
		if typeValue.Type == gjson.String && normalizedType == typeValue.String() {
			continue
		}
		tool, _ = sjson.SetBytes(tool, path, normalizedType)
	}
	return tool
}
