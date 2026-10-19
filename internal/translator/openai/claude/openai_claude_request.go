// Package claude provides request translation functionality for Anthropic to OpenAI API.
// It handles parsing and transforming Anthropic API requests into OpenAI Chat Completions API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Anthropic API format and OpenAI API's expected format.
package claude

import (
	"encoding/base64"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// convertClaudeRequestToOpenAIDefault parses and transforms an Anthropic API request into OpenAI Chat Completions API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the OpenAI API.
func convertClaudeRequestToOpenAIDefault(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertClaudeRequestToOpenAI(modelName, inputRawJSON, stream, false)
}

// ConvertClaudeRequestToOpenAIWithCompat preserves assistant thinking text for
// configured compatibility endpoints.
func ConvertClaudeRequestToOpenAIWithCompat(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertClaudeRequestToOpenAI(modelName, inputRawJSON, stream, true)
}

func convertClaudeRequestToOpenAI(
	modelName string,
	inputRawJSON []byte,
	stream bool,
	preserveThinkingBlocks bool,
) []byte {
	body, _ := convertClaudeRequestToOpenAIChecked(modelName, inputRawJSON, stream, preserveThinkingBlocks)
	return body
}

// ConvertClaudeRequestToOpenAIWithCompatReturningError reports unsupported user attachments.
func ConvertClaudeRequestToOpenAIWithCompatReturningError(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return convertClaudeRequestToOpenAIChecked(modelName, inputRawJSON, stream, true)
}

func convertClaudeRequestToOpenAIChecked(
	modelName string,
	inputRawJSON []byte,
	stream bool,
	preserveThinkingBlocks bool,
) ([]byte, error) {
	var drops translatorcommon.UserTurnDrops
	rawJSON := inputRawJSON
	// Base OpenAI Chat Completions API template
	out := []byte(`{"model":"","messages":[]}`)

	root := gjson.ParseBytes(rawJSON)

	// Model mapping
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Max tokens
	if maxTokens := root.Get("max_tokens"); maxTokens.Exists() {
		out, _ = sjson.SetBytes(out, "max_tokens", maxTokens.Int())
	}

	// Temperature
	if temp := root.Get("temperature"); temp.Exists() {
		out, _ = sjson.SetBytes(out, "temperature", temp.Float())
	} else if topP := root.Get("top_p"); topP.Exists() { // Top P
		out, _ = sjson.SetBytes(out, "top_p", topP.Float())
	}

	// Stop sequences -> stop
	if stopSequences := root.Get("stop_sequences"); stopSequences.Exists() {
		if stopSequences.IsArray() {
			var stops []string
			stopSequences.ForEach(
				func(_, value gjson.Result) bool {
					stops = append(stops, value.String())
					return true
				},
			)
			if len(stops) > 0 {
				out, _ = sjson.SetBytes(out, "stop", stops)
			}
		}
	}

	// Stream
	out, _ = sjson.SetBytes(out, "stream", stream)

	// Thinking: Convert Claude thinking.budget_tokens to OpenAI reasoning_effort
	if thinkingConfig := root.Get("thinking"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
		if thinkingType := thinkingConfig.Get("type"); thinkingType.Exists() {
			switch thinkingType.String() {
			case "enabled":
				if budgetTokens := thinkingConfig.Get("budget_tokens"); budgetTokens.Exists() {
					budget := int(budgetTokens.Int())
					if effort, ok := thinking.ConvertBudgetToLevel(budget); ok && effort != "" {
						out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
					}
				} else if effort := root.Get("output_config.effort"); effort.Type == gjson.String && strings.TrimSpace(effort.String()) != "" {
					out, _ = sjson.SetBytes(out, "reasoning_effort", strings.ToLower(strings.TrimSpace(effort.String())))
				} else {
					// No budget_tokens specified, default to "auto" for enabled thinking
					if effort, ok := thinking.ConvertBudgetToLevel(-1); ok && effort != "" {
						out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
					}
				}
			case "adaptive", "auto":
				// Adaptive thinking can carry an explicit effort in output_config.effort (Claude 4.6).
				// Pass through directly; ApplyThinking handles clamping to target model's levels.
				effort := ""
				if v := root.Get("output_config.effort"); v.Exists() && v.Type == gjson.String {
					effort = strings.ToLower(strings.TrimSpace(v.String()))
				}
				if effort != "" {
					out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
				} else {
					out, _ = sjson.SetBytes(out, "reasoning_effort", string(thinking.LevelXHigh))
				}
			case "disabled":
				if effort, ok := thinking.ConvertBudgetToLevel(0); ok && effort != "" {
					out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
				}
			}
		}
	}

	// Process messages and system.
	messageItems := make([][]byte, 0, int(root.Get("messages.#").Int())+1)

	systemItems := make([][]byte, 0, 2)
	appendSystemContent := func(content gjson.Result) {
		if !content.Exists() {
			return
		}
		if content.Type == gjson.String {
			if content.String() == "" || util.IsClaudeCodeAttributionSystemText(content.String()) {
				return
			}
			item := []byte(`{"type":"text","text":""}`)
			item, _ = sjson.SetBytes(item, "text", content.String())
			systemItems = append(systemItems, item)
			return
		}
		if content.IsArray() {
			content.ForEach(func(_, item gjson.Result) bool {
				if contentItem, ok := convertClaudeContentPart(item); ok {
					systemItems = append(systemItems, []byte(contentItem))
				}
				return true
			})
		}
	}
	appendSystemContent(root.Get("system"))
	if len(systemItems) > 0 {
		systemMessage := []byte(`{"role":"system","content":[]}`)
		systemMessage, _ = sjson.SetRawBytes(systemMessage, "content", translatorcommon.JoinRawArray(systemItems))
		messageItems = append(messageItems, systemMessage)
	}

	if messages := root.Get("messages"); messages.Exists() && messages.IsArray() {
		var pendingToolUseIDs []string
		var pendingSystemReminders [][]byte
		toolNameByID := make(map[string]string)

		messages.ForEach(func(_, message gjson.Result) bool {
			role := message.Get("role").String()
			contentResult := message.Get("content")
			if role == "system" {
				if reminderText, ok := translatorcommon.ClaudeMessageSystemReminderText(contentResult); ok {
					reminder := []byte(`{"role":"user","content":[{"type":"text","text":""}]}`)
					reminder, _ = sjson.SetBytes(reminder, "content.0.text", reminderText)
					if len(pendingToolUseIDs) > 0 {
						pendingSystemReminders = append(pendingSystemReminders, reminder)
					} else {
						messageItems = append(messageItems, reminder)
					}
				}
				return true
			}

			if contentResult.Exists() && contentResult.IsArray() {
				if role == "user" && len(pendingToolUseIDs) > 0 {
					contentResult = translatorcommon.AlignClaudeToolResults(contentResult, pendingToolUseIDs)
				}
				precedingToolCallsPending := len(pendingToolUseIDs) > 0
				pendingToolUseIDs = nil

				contentItems := make([][]byte, 0)
				var reasoningParts []string
				var toolCalls []any
				toolResults := make([][]byte, 0)
				relayedToolImages := make([][]byte, 0)

				contentResult.ForEach(func(_, part gjson.Result) bool {
					switch part.Get("type").String() {
					case "thinking":
						if role == "assistant" && shouldMapClaudeThinkingToGPTReasoning(part, preserveThinkingBlocks) {
							if text := thinking.GetThinkingText(part); strings.TrimSpace(text) != "" {
								reasoningParts = append(reasoningParts, text)
							}
						}
					case "redacted_thinking":
					case "text", "image", "document", "container_upload":
						if contentItem, ok := convertClaudeContentPart(part); ok {
							contentItems = append(contentItems, []byte(contentItem))
						} else if role == "user" {
							switch part.Get("type").String() {
							case "image", "document", "container_upload":
								drops.Drop(part.Get("type").String())
							}
						}
					case "tool_use":
						if role == "assistant" {
							toolUseID := part.Get("id").String()
							toolName := part.Get("name").String()
							if toolUseID != "" {
								pendingToolUseIDs = append(pendingToolUseIDs, toolUseID)
								if toolName != "" {
									toolNameByID[toolUseID] = toolName
								}
							}
							toolCall := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
							toolCall, _ = sjson.SetBytes(toolCall, "id", toolUseID)
							toolCall, _ = sjson.SetBytes(toolCall, "function.name", toolName)
							if input := part.Get("input"); input.Exists() {
								toolCall, _ = sjson.SetBytes(toolCall, "function.arguments", input.Raw)
							} else {
								toolCall, _ = sjson.SetBytes(toolCall, "function.arguments", "{}")
							}
							toolCalls = append(toolCalls, gjson.ParseBytes(toolCall).Value())
						}
					case "tool_result":
						toolUseID := part.Get("tool_use_id").String()
						toolResult := []byte(`{"role":"tool","tool_call_id":"","content":""}`)
						toolResult, _ = sjson.SetBytes(toolResult, "tool_call_id", toolUseID)
						if toolName := toolNameByID[toolUseID]; toolName != "" {
							toolResult, _ = sjson.SetBytes(toolResult, "name", toolName)
						}
						text, images := convertClaudeToolResultContent(part.Get("content"))
						toolResult, _ = sjson.SetBytes(toolResult, "content", text)
						toolResults = append(toolResults, toolResult)
						relayedToolImages = append(relayedToolImages, images...)
					}
					return true
				})

				hasContent := len(contentItems) > 0
				hasReasoning := len(reasoningParts) > 0
				hasToolCalls := len(toolCalls) > 0
				hasToolResults := len(toolResults) > 0
				if role == "user" {
					drops.EndTurn(len(contentItems) + len(toolResults) + len(relayedToolImages))
				}
				if precedingToolCallsPending && !hasToolResults && len(pendingSystemReminders) > 0 {
					messageItems = append(messageItems, pendingSystemReminders...)
					pendingSystemReminders = nil
				}
				messageItems = append(messageItems, toolResults...)

				if len(relayedToolImages) > 0 {
					relayItems := make([][]byte, 0, len(relayedToolImages)+1)
					notice := []byte(`{"type":"text","text":""}`)
					notice, _ = sjson.SetBytes(notice, "text", toolResultImageRelayNotice)
					relayItems = append(relayItems, notice)
					relayItems = append(relayItems, relayedToolImages...)
					if role == "user" && hasContent {
						contentItems = append(relayItems, contentItems...)
					} else {
						relay := []byte(`{"role":"user","content":[]}`)
						relay, _ = sjson.SetRawBytes(relay, "content", translatorcommon.JoinRawArray(relayItems))
						messageItems = append(messageItems, relay)
					}
				}
				if len(pendingSystemReminders) > 0 {
					messageItems = append(messageItems, pendingSystemReminders...)
					pendingSystemReminders = nil
				}

				if role == "assistant" {
					if hasContent || hasReasoning || hasToolCalls {
						msg := []byte(`{"role":"assistant","content":""}`)
						if hasContent {
							msg, _ = sjson.SetRawBytes(msg, "content", translatorcommon.JoinRawArray(contentItems))
						}
						if hasReasoning {
							msg, _ = sjson.SetBytes(msg, "reasoning_content", strings.Join(reasoningParts, "\n\n"))
						}
						if hasToolCalls {
							msg, _ = sjson.SetBytes(msg, "tool_calls", toolCalls)
						}
						messageItems = append(messageItems, msg)
					}
				} else if hasContent {
					msg := []byte(`{"role":"","content":[]}`)
					msg, _ = sjson.SetBytes(msg, "role", role)
					msg, _ = sjson.SetRawBytes(msg, "content", translatorcommon.JoinRawArray(contentItems))
					messageItems = append(messageItems, msg)
				}
			} else if contentResult.Exists() && contentResult.Type == gjson.String {
				if len(pendingToolUseIDs) > 0 && len(pendingSystemReminders) > 0 {
					messageItems = append(messageItems, pendingSystemReminders...)
					pendingSystemReminders = nil
					pendingToolUseIDs = nil
				}
				msg := []byte(`{"role":"","content":""}`)
				msg, _ = sjson.SetBytes(msg, "role", role)
				msg, _ = sjson.SetBytes(msg, "content", contentResult.String())
				messageItems = append(messageItems, msg)
			}
			return true
		})
		messageItems = append(messageItems, pendingSystemReminders...)
	}

	if len(messageItems) > 0 {
		messageItems = translatorcommon.AlignOpenAIToolCallMessages(messageItems)
		out = translatorcommon.SetRawArrayItems(out, "messages", messageItems)
	}

	// Process tools - convert Anthropic tools to OpenAI functions
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		toolsJSON := []byte(`[]`)

		tools.ForEach(
			func(_, tool gjson.Result) bool {
				openAIToolJSON := []byte(`{"type":"function","function":{"name":"","description":""}}`)
				openAIToolJSON, _ = sjson.SetBytes(openAIToolJSON, "function.name", tool.Get("name").String())
				openAIToolJSON, _ = sjson.SetBytes(
					openAIToolJSON, "function.description", tool.Get("description").String(),
				)

				// Convert Anthropic input_schema to an OpenAI object schema.
				inputSchema := tool.Get("input_schema")
				if !inputSchema.Exists() || inputSchema.Type == gjson.Null {
					openAIToolJSON, _ = sjson.SetRawBytes(
						openAIToolJSON,
						"function.parameters",
						[]byte(`{"type":"object","properties":{}}`),
					)
				} else {
					schema := gjson.ParseBytes(util.NormalizeClaudeToolInputSchema([]byte(inputSchema.Raw)))
					openAIToolJSON, _ = sjson.SetRawBytes(
						openAIToolJSON,
						"function.parameters",
						[]byte(normalizeOpenAIBooleanSubschemas(schema)),
					)
				}

				toolsJSON, _ = sjson.SetRawBytes(toolsJSON, "-1", openAIToolJSON)
				return true
			},
		)

		if parsed := gjson.ParseBytes(toolsJSON); parsed.IsArray() && len(parsed.Array()) > 0 {
			out, _ = sjson.SetRawBytes(out, "tools", toolsJSON)
		}
	}

	// Tool choice mapping - convert Anthropic tool_choice to OpenAI format
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() && toolChoice.Type != gjson.Null {
		choiceType := toolChoice.Get("type").String()
		if choiceType == "" && toolChoice.Type == gjson.String {
			choiceType = toolChoice.String()
		}
		switch choiceType {
		case "auto":
			out, _ = sjson.SetBytes(out, "tool_choice", "auto")
		case "any":
			out, _ = sjson.SetBytes(out, "tool_choice", "required")
		case "none":
			out, _ = sjson.SetBytes(out, "tool_choice", "none")
		case "tool":
			toolName := toolChoice.Get("name").String()
			if toolName != "" {
				toolChoiceJSON := []byte(`{"type":"function","function":{"name":""}}`)
				toolChoiceJSON, _ = sjson.SetBytes(toolChoiceJSON, "function.name", toolName)
				out, _ = sjson.SetRawBytes(out, "tool_choice", toolChoiceJSON)
			} else {
				out, _ = sjson.SetBytes(out, "tool_choice", "none")
			}
		default:
			out, _ = sjson.SetBytes(out, "tool_choice", "none")
		}
		if toolChoice.Get("disable_parallel_tool_use").Type == gjson.True {
			out, _ = sjson.SetBytes(out, "parallel_tool_calls", false)
		}
	}

	// Handle user parameter (for tracking)
	if user := root.Get("user"); user.Exists() {
		out, _ = sjson.SetBytes(out, "user", user.String())
	}

	return out, drops.Err()
}

func normalizeOpenAIBooleanSubschemas(schema gjson.Result) string {
	if schema.Type == gjson.True {
		return "{}"
	}
	if schema.IsArray() {
		parts := make([]string, 0, len(schema.Array()))
		for _, child := range schema.Array() {
			parts = append(parts, normalizeOpenAIBooleanSubschemas(child))
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	if !schema.IsObject() {
		return schema.Raw
	}
	out := schema.Raw
	for _, keyword := range util.SchemaMapKeywords {
		children := schema.Get(keyword)
		if !children.IsObject() {
			continue
		}
		var parts []string
		children.ForEach(func(key, child gjson.Result) bool {
			parts = append(parts, key.Raw+":"+normalizeOpenAIBooleanSubschemas(child))
			return true
		})
		out, _ = sjson.SetRaw(out, keyword, "{"+strings.Join(parts, ",")+"}")
	}
	for _, keyword := range util.SchemaValueKeywords {
		child := schema.Get(keyword)
		if !child.Exists() || keyword == "additionalProperties" && child.Type == gjson.True {
			continue
		}
		out, _ = sjson.SetRaw(out, keyword, normalizeOpenAIBooleanSubschemas(child))
	}
	return out
}

func shouldMapClaudeThinkingToGPTReasoning(
	part gjson.Result,
	preserveThinkingBlocks ...bool,
) bool {
	if len(preserveThinkingBlocks) > 0 && preserveThinkingBlocks[0] {
		return true
	}
	rawSignature := strings.TrimSpace(part.Get("signature").String())
	if rawSignature == "" {
		return true
	}
	_, ok := signature.CompatibleSignatureForProvider(
		signature.ProviderGPT, rawSignature, signature.BlockKindGPTReasoning,
	)
	return ok
}

func convertClaudeContentPart(part gjson.Result) (string, bool) {
	partType := part.Get("type").String()

	switch partType {
	case "text":
		text := part.Get("text").String()
		if strings.TrimSpace(text) == "" {
			return "", false
		}
		textContent := []byte(`{"type":"text","text":""}`)
		textContent, _ = sjson.SetBytes(textContent, "text", text)
		return string(textContent), true

	case "image":
		var imageURL string

		if source := part.Get("source"); source.Exists() {
			sourceType := source.Get("type").String()
			switch sourceType {
			case "base64":
				mediaType := source.Get("media_type").String()
				if mediaType == "" {
					mediaType = "application/octet-stream"
				}
				data := source.Get("data").String()
				if data != "" {
					imageURL = "data:" + mediaType + ";base64," + data
				}
			case "url":
				imageURL = source.Get("url").String()
			}
		}

		if imageURL == "" {
			imageURL = part.Get("url").String()
		}

		if imageURL == "" {
			return "", false
		}

		imageContent := []byte(`{"type":"image_url","image_url":{"url":""}}`)
		imageContent, _ = sjson.SetBytes(imageContent, "image_url.url", imageURL)

		return string(imageContent), true

	case "document", "container_upload":
		source := part.Get("source")
		if source.Get("type").String() != "base64" {
			return "", false
		}
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(source.Get("data").String()))
		if err != nil || len(data) == 0 {
			return "", false
		}
		mimeType := source.Get("media_type").String()
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		file := []byte(`{"type":"file","file":{"filename":"","file_data":""}}`)
		file, _ = sjson.SetBytes(file, "file.filename", part.Get("filename").String())
		file, _ = sjson.SetBytes(file, "file.file_data", "data:"+mimeType+";base64,"+base64.StdEncoding.EncodeToString(data))
		return string(file), true

	default:
		return "", false
	}
}

const toolResultImagePlaceholder = "[Tool returned image content; the images follow in the next user message.]"

const toolResultImageRelayNotice = "Images returned by the preceding tool call(s):"

func convertClaudeToolResultContent(content gjson.Result) (string, [][]byte) {
	if !content.Exists() {
		return "", nil
	}
	if content.Type == gjson.String {
		return content.String(), nil
	}
	if content.IsArray() {
		var parts []string
		var images [][]byte
		content.ForEach(func(_, item gjson.Result) bool {
			switch {
			case item.Type == gjson.String:
				parts = append(parts, item.String())
			case item.IsObject() && item.Get("type").String() == "text":
				parts = append(parts, item.Get("text").String())
			case item.IsObject() && item.Get("type").String() == "image":
				if image, ok := convertClaudeContentPart(item); ok {
					images = append(images, []byte(image))
				} else {
					parts = append(parts, item.Raw)
				}
			case item.IsObject() && item.Get("text").Type == gjson.String:
				parts = append(parts, item.Get("text").String())
			default:
				parts = append(parts, item.Raw)
			}
			return true
		})
		text := strings.Join(parts, "\n\n")
		if strings.TrimSpace(text) == "" {
			if len(images) > 0 {
				return toolResultImagePlaceholder, images
			}
			return content.Raw, nil
		}
		return text, images
	}
	if content.IsObject() {
		if content.Get("type").String() == "image" {
			if image, ok := convertClaudeContentPart(content); ok {
				return toolResultImagePlaceholder, [][]byte{[]byte(image)}
			}
		}
		if text := content.Get("text"); text.Type == gjson.String {
			return text.String(), nil
		}
		return content.Raw, nil
	}
	return content.Raw, nil
}
