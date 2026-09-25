// Package chat_completions provides request translation functionality for OpenAI to Claude Code API compatibility.
// It handles parsing and transforming OpenAI Chat Completions API requests into Claude Code API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between OpenAI API format and Claude Code API's expected format.
package chat_completions

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// convertOpenAIRequestToClaudeDefault parses and transforms an OpenAI Chat Completions API request into Claude Code API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Claude Code API.
// The function performs comprehensive transformation including:
// 1. Model name mapping and parameter extraction (max_tokens, temperature, top_p, etc.)
// 2. Message content conversion from OpenAI to Claude Code format
// 3. Tool call and tool result handling with proper ID mapping
// 4. Image data conversion from OpenAI data URLs to Claude Code base64 format
// 5. Stop sequence and streaming configuration handling
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in Claude Code API format
func convertOpenAIRequestToClaudeDefault(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertOpenAIRequestToClaude(modelName, inputRawJSON, stream, false)
}

// ConvertOpenAIRequestToClaudeWithCompat preserves assistant reasoning content
// as an unsigned thinking block for configured compatibility endpoints.
func ConvertOpenAIRequestToClaudeWithCompat(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertOpenAIRequestToClaude(modelName, inputRawJSON, stream, true)
}

func convertOpenAIRequestToClaude(
	modelName string,
	inputRawJSON []byte,
	stream bool,
	preserveThinkingBlocks bool,
) []byte {
	rawJSON := inputRawJSON
	root := gjson.ParseBytes(rawJSON)

	// Base Claude Code API template with a deterministic request identity.
	out := []byte(`{"model":"","max_tokens":32000,"messages":[],"metadata":{}}`)
	out, _ = sjson.SetBytes(out, "metadata.user_id", deriveClaudeUserID(root, modelName, "messages"))

	// Convert OpenAI reasoning_effort to Claude thinking config.
	if v := root.Get("reasoning_effort"); v.Exists() {
		effort := strings.ToLower(strings.TrimSpace(v.String()))
		if effort != "" {
			mi := registry.LookupModelInfo(modelName, "claude")
			supportsAdaptive := mi != nil && mi.Thinking != nil && len(mi.Thinking.Levels) > 0
			supportsMax := supportsAdaptive && thinking.HasLevel(mi.Thinking.Levels, string(thinking.LevelMax))

			// Claude 4.6 supports adaptive thinking with output_config.effort.
			// MapToClaudeEffort normalizes levels (e.g. minimal→low, xhigh→high) to avoid
			// validation errors since validate treats same-provider unsupported levels as errors.
			if supportsAdaptive {
				switch effort {
				case "none":
					out, _ = sjson.SetBytes(out, "thinking.type", "disabled")
					out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
					out, _ = sjson.DeleteBytes(out, "output_config.effort")
				case "auto":
					out, _ = sjson.SetBytes(out, "thinking.type", "adaptive")
					out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
					out, _ = sjson.DeleteBytes(out, "output_config.effort")
				default:
					if mapped, ok := thinking.MapToClaudeEffort(effort, supportsMax); ok {
						effort = mapped
					}
					out, _ = sjson.SetBytes(out, "thinking.type", "adaptive")
					out, _ = sjson.DeleteBytes(out, "thinking.budget_tokens")
					out, _ = sjson.SetBytes(out, "output_config.effort", effort)
				}
			} else {
				// Legacy/manual thinking (budget_tokens).
				budget, ok := thinking.ConvertLevelToBudget(effort)
				if ok {
					switch budget {
					case 0:
						out, _ = sjson.SetBytes(out, "thinking.type", "disabled")
					case -1:
						out, _ = sjson.SetBytes(out, "thinking.type", "enabled")
					default:
						if budget > 0 {
							out, _ = sjson.SetBytes(out, "thinking.type", "enabled")
							out, _ = sjson.SetBytes(out, "thinking.budget_tokens", budget)
						}
					}
				}
			}
		}
	}

	// Helper for generating tool call IDs in the form: toolu_<alphanum>
	// This ensures unique identifiers for tool calls in the Claude Code format
	genToolCallID := func() string {
		const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		var b strings.Builder
		// 24 chars random suffix for uniqueness
		for range 24 {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
			b.WriteByte(letters[n.Int64()])
		}
		return "toolu_" + b.String()
	}

	// Model mapping to specify which Claude Code model to use
	out, _ = sjson.SetBytes(out, "model", modelName)

	// OpenAI deprecated max_tokens in favor of max_completion_tokens, so
	// accept either spelling while preserving max_tokens precedence.
	maxTokens := root.Get("max_tokens")
	if !maxTokens.Exists() {
		maxTokens = root.Get("max_completion_tokens")
	}
	if maxTokens.Exists() {
		out, _ = sjson.SetBytes(out, "max_tokens", maxTokens.Int())
	}

	// Top P setting for nucleus sampling.
	if topP := root.Get("top_p"); topP.Exists() {
		out, _ = sjson.SetBytes(out, "top_p", topP.Float())
	}

	// Stop sequences configuration for custom termination conditions
	if stop := root.Get("stop"); stop.Exists() {
		if stop.IsArray() {
			var stopSequences []string
			stop.ForEach(
				func(_, value gjson.Result) bool {
					stopSequences = append(stopSequences, value.String())
					return true
				},
			)
			if len(stopSequences) > 0 {
				out, _ = sjson.SetBytes(out, "stop_sequences", stopSequences)
			}
		} else {
			out, _ = sjson.SetBytes(out, "stop_sequences", []string{stop.String()})
		}
	}

	// Stream configuration to enable or disable streaming responses
	out, _ = sjson.SetBytes(out, "stream", stream)

	// Process messages and transform them to Claude Code format
	systemBlocks := make([][]byte, 0)
	var messageBlocks [][]byte
	if messages := root.Get("messages"); messages.Exists() && messages.IsArray() {
		lastToolMessage := make(map[string]gjson.Result)
		messages.ForEach(func(_, message gjson.Result) bool {
			if message.Get("role").String() == "tool" {
				if rawID := message.Get("tool_call_id").String(); rawID != "" {
					lastToolMessage[rawID] = message
				}
			}
			return true
		})
		emittedToolResults := make(map[string]struct{})
		messageAccumulator := translatorcommon.NewClaudeMessageAccumulator(
			int(messages.Get("#").Int()),
		)
		messages.ForEach(
			func(_, message gjson.Result) bool {
				role := message.Get("role").String()
				contentResult := message.Get("content")

				switch role {
				case "system", "developer":
					systemStart := len(systemBlocks)
					if contentResult.Exists() && contentResult.Type == gjson.String && contentResult.String() != "" {
						textPart := []byte(`{"type":"text","text":""}`)
						textPart, _ = sjson.SetBytes(textPart, "text", contentResult.String())
						textPart = translatorcommon.AttachCacheControl(textPart, message)
						systemBlocks = append(systemBlocks, textPart)
					} else if contentResult.Exists() && contentResult.IsArray() {
						contentResult.ForEach(
							func(_, part gjson.Result) bool {
								if part.Get("type").String() == "text" {
									textPart := []byte(`{"type":"text","text":""}`)
									textPart, _ = sjson.SetBytes(textPart, "text", part.Get("text").String())
									textPart = translatorcommon.AttachCacheControl(textPart, part)
									systemBlocks = append(systemBlocks, textPart)
								}
								return true
							},
						)
						if message.Get("cache_control").Exists() && len(systemBlocks) > systemStart {
							lastIndex := len(systemBlocks) - 1
							if !gjson.GetBytes(systemBlocks[lastIndex], "cache_control").Exists() {
								systemBlocks[lastIndex] = translatorcommon.AttachCacheControl(
									systemBlocks[lastIndex],
									message,
								)
							}
						}
					}
				case "user", "assistant":
					msg := []byte(`{"role":"","content":[]}`)
					msg, _ = sjson.SetBytes(msg, "role", role)
					if preserveThinkingBlocks && role == "assistant" {
						reasoningContent := message.Get("reasoning_content")
						if reasoningContent.Type == gjson.String &&
							strings.TrimSpace(reasoningContent.String()) != "" {
							part := []byte(`{"type":"thinking","thinking":"","signature":""}`)
							part, _ = sjson.SetBytes(
								part,
								"thinking",
								reasoningContent.String(),
							)
							msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
						}
					}

					// Handle content based on its type (string or array)
					if contentResult.Exists() && contentResult.Type == gjson.String && contentResult.String() != "" {
						part := []byte(`{"type":"text","text":""}`)
						part, _ = sjson.SetBytes(part, "text", contentResult.String())
						msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
					} else if contentResult.Exists() && contentResult.IsArray() {
						contentResult.ForEach(
							func(_, part gjson.Result) bool {
								claudePart := convertOpenAIContentPartToClaudePart(part)
								if claudePart != "" {
									msg, _ = sjson.SetRawBytes(msg, "content.-1", []byte(claudePart))
								}
								return true
							},
						)
					}

					// Handle tool calls (for assistant messages)
					if toolCalls := message.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() && role == "assistant" {
						toolCalls.ForEach(
							func(_, toolCall gjson.Result) bool {
								if toolCall.Get("type").String() == "function" {
									toolCallID := toolCall.Get("id").String()
									if toolCallID == "" {
										toolCallID = genToolCallID()
									} else {
										toolCallID = util.SanitizeClaudeToolID(toolCallID)
									}

									function := toolCall.Get("function")
									toolUse := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
									toolUse, _ = sjson.SetBytes(toolUse, "id", toolCallID)
									toolUse, _ = sjson.SetBytes(toolUse, "name", function.Get("name").String())

									// Parse arguments for the tool call
									if args := function.Get("arguments"); args.Exists() {
										argsStr := args.String()
										if argsStr != "" && gjson.Valid(argsStr) {
											argsJSON := gjson.Parse(argsStr)
											if argsJSON.IsObject() {
												toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte(argsJSON.Raw))
											} else {
												toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte("{}"))
											}
										} else {
											toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte("{}"))
										}
									} else {
										toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte("{}"))
									}

									msg, _ = sjson.SetRawBytes(msg, "content.-1", toolUse)
								}
								return true
							},
						)
					}

					messageAccumulator.Append(msg)

				case "tool":
					// Deduplicate by the unsanitized call ID so collisions introduced by
					// Claude ID normalization do not merge unrelated tool results. The
					// first slot is retained while the last result value wins.
					rawID := message.Get("tool_call_id").String()
					if rawID != "" {
						if _, exists := emittedToolResults[rawID]; exists {
							return true
						}
						emittedToolResults[rawID] = struct{}{}
					}
					targetMessage := message
					if lastMessage, exists := lastToolMessage[rawID]; rawID != "" && exists {
						targetMessage = lastMessage
					}
					toolCallID := util.SanitizeClaudeToolID(rawID)
					toolContentResult := targetMessage.Get("content")

					msg := []byte(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"","content":""}]}`)
					msg, _ = sjson.SetBytes(msg, "content.0.tool_use_id", toolCallID)
					toolResultContent, toolResultContentRaw := convertOpenAIToolResultContent(toolContentResult)
					if toolResultContentRaw {
						msg, _ = sjson.SetRawBytes(msg, "content.0.content", []byte(toolResultContent))
					} else {
						msg, _ = sjson.SetBytes(msg, "content.0.content", toolResultContent)
					}
					messageAccumulator.Append(msg)
				}
				return true
			},
		)

		messageBlocks = messageAccumulator.Messages()
	}

	if formatInstruction := claudeStructuredOutputInstruction(root.Get("response_format")); formatInstruction != "" {
		systemBlock := []byte(`{"type":"text","text":""}`)
		systemBlock, _ = sjson.SetBytes(systemBlock, "text", formatInstruction)
		systemBlocks = append(systemBlocks, systemBlock)
	}
	if len(messageBlocks) == 0 && len(systemBlocks) > 0 {
		messageBlocks = append(
			messageBlocks,
			[]byte(`{"role":"user","content":[{"type":"text","text":""}]}`),
		)
	}
	if len(systemBlocks) > 0 {
		out, _ = sjson.SetRawBytes(out, "system", translatorcommon.JoinRawArray(systemBlocks))
	}
	if len(messageBlocks) > 0 {
		out = translatorcommon.SetRawArrayItems(out, "messages", messageBlocks)
	}

	// Tools mapping: OpenAI tools -> Claude Code tools
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
		hasAnthropicTools := false
		tools.ForEach(
			func(_, tool gjson.Result) bool {
				if tool.Get("type").String() == "function" {
					function := tool.Get("function")
					anthropicTool := []byte(`{"name":"","description":"","input_schema":{"type":"object","properties":{}}}`)
					anthropicTool, _ = sjson.SetBytes(anthropicTool, "name", function.Get("name").String())
					anthropicTool, _ = sjson.SetBytes(
						anthropicTool, "description", function.Get("description").String(),
					)

					// Convert parameters schema for the tool
					if parameters := function.Get("parameters"); parameters.Exists() {
						anthropicTool, _ = sjson.SetRawBytes(
							anthropicTool,
							"input_schema",
							util.NormalizeClaudeToolInputSchema([]byte(parameters.Raw)),
						)
					} else if parameters := function.Get("parametersJsonSchema"); parameters.Exists() {
						anthropicTool, _ = sjson.SetRawBytes(
							anthropicTool,
							"input_schema",
							util.NormalizeClaudeToolInputSchema([]byte(parameters.Raw)),
						)
					}

					out, _ = sjson.SetRawBytes(out, "tools.-1", anthropicTool)
					hasAnthropicTools = true
				}
				return true
			},
		)

		if !hasAnthropicTools {
			out, _ = sjson.DeleteBytes(out, "tools")
		}
	}

	// Tool choice mapping from OpenAI format to Claude Code format
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		switch toolChoice.Type {
		case gjson.String:
			choice := toolChoice.String()
			switch choice {
			case "none":
				// Don't set tool_choice, Claude Code will not use tools
			case "auto":
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"auto"}`))
			case "required":
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"any"}`))
			}
		case gjson.JSON:
			// Specific tool choice mapping
			if toolChoice.Get("type").String() == "function" {
				functionName := toolChoice.Get("function.name").String()
				toolChoiceJSON := []byte(`{"type":"tool","name":""}`)
				toolChoiceJSON, _ = sjson.SetBytes(toolChoiceJSON, "name", functionName)
				out, _ = sjson.SetRawBytes(out, "tool_choice", toolChoiceJSON)
			}
		default:
		}
	}

	return out
}

func convertOpenAIContentPartToClaudePart(part gjson.Result) string {
	switch part.Get("type").String() {
	case "text":
		textPart := []byte(`{"type":"text","text":""}`)
		textPart, _ = sjson.SetBytes(textPart, "text", part.Get("text").String())
		return string(textPart)

	case "image_url":
		return convertOpenAIImageURLToClaudePart(part.Get("image_url.url").String())

	case "file":
		fileData := part.Get("file.file_data").String()
		if strings.HasPrefix(fileData, "data:") {
			semicolonIdx := strings.Index(fileData, ";")
			commaIdx := strings.Index(fileData, ",")
			if semicolonIdx != -1 && commaIdx != -1 && commaIdx > semicolonIdx {
				mediaType := strings.TrimPrefix(fileData[:semicolonIdx], "data:")
				data := fileData[commaIdx+1:]
				docPart := []byte(`{"type":"document","source":{"type":"base64","media_type":"","data":""}}`)
				docPart, _ = sjson.SetBytes(docPart, "source.media_type", mediaType)
				docPart, _ = sjson.SetBytes(docPart, "source.data", data)
				return string(docPart)
			}
		}
	}

	return ""
}

func convertOpenAIImageURLToClaudePart(imageURL string) string {
	if imageURL == "" {
		return ""
	}

	if strings.HasPrefix(imageURL, "data:") {
		parts := strings.SplitN(imageURL, ",", 2)
		if len(parts) != 2 {
			return ""
		}

		mediaTypePart, _, _ := strings.Cut(parts[0], ";")
		mediaType := strings.TrimPrefix(mediaTypePart, "data:")
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}

		imagePart := []byte(`{"type":"image","source":{"type":"base64","media_type":"","data":""}}`)
		imagePart, _ = sjson.SetBytes(imagePart, "source.media_type", mediaType)
		imagePart, _ = sjson.SetBytes(imagePart, "source.data", parts[1])
		return string(imagePart)
	}

	imagePart := []byte(`{"type":"image","source":{"type":"url","url":""}}`)
	imagePart, _ = sjson.SetBytes(imagePart, "source.url", imageURL)
	return string(imagePart)
}

func convertOpenAIToolResultContent(content gjson.Result) (string, bool) {
	if !content.Exists() {
		return "", false
	}

	if content.Type == gjson.String {
		return content.String(), false
	}

	if content.IsArray() {
		claudeContent := []byte("[]")
		partCount := 0

		content.ForEach(
			func(_, part gjson.Result) bool {
				if part.Type == gjson.String {
					textPart := []byte(`{"type":"text","text":""}`)
					textPart, _ = sjson.SetBytes(textPart, "text", part.String())
					claudeContent, _ = sjson.SetRawBytes(claudeContent, "-1", textPart)
					partCount++
					return true
				}

				claudePart := convertOpenAIContentPartToClaudePart(part)
				if claudePart != "" {
					claudeContent, _ = sjson.SetRawBytes(claudeContent, "-1", []byte(claudePart))
					partCount++
				}
				return true
			},
		)

		if partCount > 0 || len(content.Array()) == 0 {
			return string(claudeContent), true
		}

		return content.Raw, false
	}

	if content.IsObject() {
		claudePart := convertOpenAIContentPartToClaudePart(content)
		if claudePart != "" {
			claudeContent := []byte("[]")
			claudeContent, _ = sjson.SetRawBytes(claudeContent, "-1", []byte(claudePart))
			return string(claudeContent), true
		}
		return content.Raw, false
	}

	return content.Raw, false
}

func deriveClaudeUserID(root gjson.Result, modelName, messagePath string) string {
	for _, path := range []string{"metadata.user_id", "user"} {
		value := root.Get(path)
		if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			return value.String()
		}
	}

	var seed string
	for _, path := range []string{"prompt_cache_key", "session_id", "sessionId", "conversation.id", "conversation_id"} {
		if value := strings.TrimSpace(root.Get(path).String()); value != "" {
			seed = path + "\x00" + value
			break
		}
	}
	if seed == "" {
		for _, message := range root.Get(messagePath).Array() {
			if strings.EqualFold(strings.TrimSpace(message.Get("role").String()), "user") {
				if text := stableMessageText(message.Get("content")); text != "" {
					seed = "content\x00" + text
					break
				}
			}
		}
	}
	if seed == "" {
		seed = "model\x00" + strings.TrimSpace(modelName)
		for _, path := range []string{"instructions", "system"} {
			if value := root.Get(path); value.Exists() {
				seed += "\x00" + path + "\x00" + value.Raw
			}
		}
	}
	if strings.Trim(seed, "\x00") == "model" {
		return "unknown"
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

func stableMessageText(content gjson.Result) string {
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String())
	}
	if !content.IsArray() {
		return ""
	}
	var texts []string
	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "text", "input_text", "output_text":
			if text := strings.TrimSpace(part.Get("text").String()); text != "" {
				texts = append(texts, text)
			}
		}
		return true
	})
	return strings.Join(texts, "\n")
}

func claudeStructuredOutputInstruction(format gjson.Result) string {
	if !format.Exists() {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(format.Get("type").String())) {
	case "json_object":
		return "You must format your entire response as a valid JSON object. Do not include any explanations, markdown code blocks (such as ```json), or any text outside of the JSON object."
	case "json_schema":
		jsonSchema := format.Get("json_schema")
		schema := jsonSchema.Get("schema")
		if !schema.Exists() {
			schema = format.Get("schema")
		}
		if !schema.Exists() {
			return "You must format your entire response as a valid JSON object. Do not include any explanations, markdown code blocks (such as ```json), or any text outside of the JSON object."
		}
		var instruction strings.Builder
		instruction.WriteString("You must format your entire response as valid JSON that conforms strictly to the following JSON schema:\n")
		name := strings.TrimSpace(jsonSchema.Get("name").String())
		if name == "" {
			name = strings.TrimSpace(format.Get("name").String())
		}
		if name != "" {
			instruction.WriteString("Schema Name: ")
			instruction.WriteString(name)
			instruction.WriteByte('\n')
		}
		description := strings.TrimSpace(jsonSchema.Get("description").String())
		if description == "" {
			description = strings.TrimSpace(format.Get("description").String())
		}
		if description != "" {
			instruction.WriteString("Schema Description: ")
			instruction.WriteString(description)
			instruction.WriteByte('\n')
		}
		instruction.WriteString("JSON Schema:\n")
		instruction.WriteString(schema.Raw)
		instruction.WriteString("\nDo not include any explanations, markdown code blocks (such as ```json), or any text outside of the JSON object.")
		return instruction.String()
	default:
		return ""
	}
}
