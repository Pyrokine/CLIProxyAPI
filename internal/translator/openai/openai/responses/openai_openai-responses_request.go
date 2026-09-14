package responses

import (
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIResponsesRequestToOpenAIChatCompletions converts OpenAI responses format to OpenAI chat completions format.
// It transforms the OpenAI responses API format (with instructions and input array) into the standard
// OpenAI chat completions format (with messages array and system content).
//
// The conversion handles:
// 1. Model name and streaming configuration
// 2. Instructions to system message conversion
// 3. Input array to messages array transformation
// 4. Tool definitions and tool choice conversion
// 5. Function calls and function results handling
// 6. Generation parameters mapping (max_tokens, reasoning, etc.)
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data in OpenAI responses format
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in OpenAI chat completions format
func ConvertOpenAIResponsesRequestToOpenAIChatCompletions(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	// Base OpenAI chat completions template with default values
	out := []byte(`{"model":"","messages":[],"stream":false}`)

	root := gjson.ParseBytes(rawJSON)
	toolIdentity := translatorcommon.NewResponsesToolIdentity(rawJSON)
	messages := make([][]byte, 0)
	appendMessage := func(message []byte) {
		messages = append(messages, message)
	}

	// Set model name
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Set stream configuration
	out, _ = sjson.SetBytes(out, "stream", stream)
	if serviceTier := root.Get("service_tier"); serviceTier.Type == gjson.String {
		if tier := strings.TrimSpace(serviceTier.String()); tier != "" {
			out, _ = sjson.SetBytes(out, "service_tier", tier)
		}
	}

	if textFormat := root.Get("text.format"); textFormat.Exists() {
		if responseFormat := convertResponsesTextFormatToChatResponseFormat(textFormat); len(responseFormat) > 0 {
			out, _ = sjson.SetRawBytes(out, "response_format", responseFormat)
		}
	}

	// Map generation parameters from responses format to chat completions format
	if maxTokens := root.Get("max_output_tokens"); maxTokens.Exists() {
		out, _ = sjson.SetBytes(out, "max_tokens", maxTokens.Int())
	}

	if parallelToolCalls := root.Get("parallel_tool_calls"); parallelToolCalls.Exists() {
		out, _ = sjson.SetBytes(out, "parallel_tool_calls", parallelToolCalls.Bool())
	}

	// Convert instructions to system message
	if instructions := root.Get("instructions"); instructions.Exists() {
		systemMessage := []byte(`{"role":"system","content":""}`)
		systemMessage, _ = sjson.SetBytes(systemMessage, "content", instructions.String())
		appendMessage(systemMessage)
	}

	// Convert input array to messages
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		var pendingToolCalls [][]byte
		pendingToolCallIDs := map[string]struct{}{}
		pendingToolOutputIDs := map[string]struct{}{}
		pendingReasoningContent := ""
		var deferredMessages [][]byte
		mergeableAssistantIndex := -1

		takePendingReasoningContent := func() string {
			reasoningContent := pendingReasoningContent
			pendingReasoningContent = ""
			return reasoningContent
		}

		emitPendingToolCalls := func() {
			if len(pendingToolCalls) == 0 {
				return
			}

			reasoningContent := takePendingReasoningContent()
			mergedIntoAssistant := false
			if mergeableAssistantIndex >= 0 && mergeableAssistantIndex == len(messages)-1 {
				assistantMessage := gjson.ParseBytes(messages[mergeableAssistantIndex])
				if assistantMessage.Get("role").String() == "assistant" && !assistantMessage.Get("tool_calls").Exists() {
					updatedMessage := translatorcommon.SetRawArrayItems(
						messages[mergeableAssistantIndex],
						"tool_calls",
						pendingToolCalls,
					)
					combinedReasoning := combineOpenAIResponsesReasoning(
						assistantMessage.Get("reasoning_content").String(),
						reasoningContent,
					)
					if combinedReasoning != "" {
						updatedMessage, _ = sjson.SetBytes(updatedMessage, "reasoning_content", combinedReasoning)
					}
					messages[mergeableAssistantIndex] = updatedMessage
					mergedIntoAssistant = true
				}
			}
			if !mergedIntoAssistant {
				assistantMessage := []byte(`{"role":"assistant","tool_calls":[]}`)
				assistantMessage = translatorcommon.SetRawArrayItems(assistantMessage, "tool_calls", pendingToolCalls)
				if reasoningContent != "" {
					assistantMessage, _ = sjson.SetBytes(assistantMessage, "reasoning_content", reasoningContent)
				}
				appendMessage(assistantMessage)
			}
			pendingToolCalls = nil
			mergeableAssistantIndex = -1
		}

		emitDeferredMessages := func() {
			for i := range deferredMessages {
				appendMessage(deferredMessages[i])
			}
			deferredMessages = nil
			pendingToolCallIDs = map[string]struct{}{}
			pendingToolOutputIDs = map[string]struct{}{}
		}

		allPendingToolOutputsReceived := func() bool {
			if len(pendingToolCallIDs) == 0 {
				return true
			}
			for callID := range pendingToolCallIDs {
				if _, ok := pendingToolOutputIDs[callID]; !ok {
					return false
				}
			}
			return true
		}

		appendRegularMessage := func(message []byte) int {
			if !allPendingToolOutputsReceived() {
				deferredMessages = append(deferredMessages, message)
				return -1
			}
			appendMessage(message)
			return len(messages) - 1
		}

		appendPendingReasoningMessage := func() {
			reasoningContent := takePendingReasoningContent()
			if reasoningContent == "" {
				return
			}
			message := []byte(`{"role":"assistant","content":"","reasoning_content":""}`)
			message, _ = sjson.SetBytes(message, "reasoning_content", reasoningContent)
			appendRegularMessage(message)
		}

		inputArray := input.Array()
		for _, item := range inputArray {
			itemType := item.Get("type").String()
			if itemType == "" && item.Get("role").String() != "" {
				itemType = "message"
			}
			if itemType != "function_call" && itemType != "custom_tool_call" {
				emitPendingToolCalls()
			}

			switch itemType {
			case "message", "":
				role := item.Get("role").String()
				if role == "developer" {
					role = "user"
				}
				mergeableAssistantIndex = -1
				if role != "assistant" {
					appendPendingReasoningMessage()
				}
				message := []byte(`{"role":"","content":[]}`)
				message, _ = sjson.SetBytes(message, "role", role)

				if content := item.Get("content"); content.Exists() && content.IsArray() {
					content.ForEach(
						func(_, contentItem gjson.Result) bool {
							contentType := contentItem.Get("type").String()
							if contentType == "" {
								contentType = "input_text"
							}

							switch contentType {
							case "input_text", "output_text":
								text := contentItem.Get("text").String()
								contentPart := []byte(`{"type":"text","text":""}`)
								contentPart, _ = sjson.SetBytes(contentPart, "text", text)
								message, _ = sjson.SetRawBytes(message, "content.-1", contentPart)
							case "input_image":
								imageURL := contentItem.Get("image_url").String()
								contentPart := []byte(`{"type":"image_url","image_url":{"url":""}}`)
								contentPart, _ = sjson.SetBytes(contentPart, "image_url.url", imageURL)
								message, _ = sjson.SetRawBytes(message, "content.-1", contentPart)
							}
							return true
						},
					)
				} else if content.Type == gjson.String {
					message, _ = sjson.SetBytes(message, "content", content.String())
				}

				if role == "assistant" {
					reasoningContent := combineOpenAIResponsesReasoning(
						takePendingReasoningContent(),
						item.Get("reasoning_content").String(),
					)
					if reasoningContent != "" {
						message, _ = sjson.SetBytes(message, "reasoning_content", reasoningContent)
					}
				}

				messageIndex := appendRegularMessage(message)
				if role == "assistant" {
					mergeableAssistantIndex = messageIndex
				}

			case "reasoning":
				pendingReasoningContent = combineOpenAIResponsesReasoning(
					pendingReasoningContent,
					collectOpenAIResponsesReasoningContent(item),
				)

			case "function_call", "custom_tool_call":
				pendingReasoningContent = combineOpenAIResponsesReasoning(
					pendingReasoningContent,
					item.Get("reasoning_content").String(),
				)
				toolCall := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
				if callID := item.Get("call_id"); callID.Exists() {
					toolCall, _ = sjson.SetBytes(toolCall, "id", callID.String())
					if callID.String() != "" {
						pendingToolCallIDs[callID.String()] = struct{}{}
					}
				}
				name := toolIdentity.ResolveName(item.Get("name").String(), item.Get("namespace").String())
				toolCall, _ = sjson.SetBytes(toolCall, "function.name", name)
				if itemType == "custom_tool_call" {
					toolCall, _ = sjson.SetBytes(
						toolCall, "function.arguments", translatorcommon.WrapResponsesCustomToolInput(item.Get("input")),
					)
				} else if arguments := item.Get("arguments"); arguments.Exists() {
					toolCall, _ = sjson.SetBytes(toolCall, "function.arguments", arguments.String())
				}
				pendingToolCalls = append(pendingToolCalls, toolCall)

			case "function_call_output", "custom_tool_call_output":
				mergeableAssistantIndex = -1
				toolMessage := []byte(`{"role":"tool","tool_call_id":"","content":""}`)
				if callID := item.Get("call_id"); callID.Exists() {
					toolMessage, _ = sjson.SetBytes(toolMessage, "tool_call_id", callID.String())
					if callID.String() != "" {
						pendingToolOutputIDs[callID.String()] = struct{}{}
					}
				}
				if output := item.Get("output"); output.Exists() {
					toolMessage = setResponsesToolOutputContent(toolMessage, output)
				}
				appendMessage(toolMessage)
				if allPendingToolOutputsReceived() {
					emitDeferredMessages()
				}

			default:
				mergeableAssistantIndex = -1
			}
		}
		emitPendingToolCalls()
		appendPendingReasoningMessage()
		emitDeferredMessages()
	} else if input.Type == gjson.String {
		msg := []byte(`{}`)
		msg, _ = sjson.SetBytes(msg, "role", "user")
		msg, _ = sjson.SetBytes(msg, "content", input.String())
		appendMessage(msg)
	}

	if len(messages) > 0 {
		out = translatorcommon.SetRawArrayItems(out, "messages", messages)
	}

	// Convert the winning top-level and Responses Lite additional_tools declarations.
	var chatCompletionsTools []any
	for _, declaration := range toolIdentity.Declarations() {
		chatTool, ok := convertResponsesDeclarationToOpenAIChat(declaration)
		if !ok {
			continue
		}
		chatCompletionsTools = append(chatCompletionsTools, gjson.ParseBytes(chatTool).Value())
	}
	if len(chatCompletionsTools) > 0 {
		out, _ = sjson.SetBytes(out, "tools", chatCompletionsTools)
	}

	if reasoningEffort := root.Get("reasoning.effort"); reasoningEffort.Exists() {
		effort := strings.ToLower(strings.TrimSpace(reasoningEffort.String()))
		if effort != "" {
			out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
		}
	}

	// Convert tool_choice if present.
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		if toolChoice.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "tool_choice", toolChoice.String())
		} else if toolChoice.IsObject() {
			choiceType := strings.TrimSpace(toolChoice.Get("type").String())
			if choiceType == "function" || choiceType == "custom" {
				name := toolChoice.Get("name").String()
				if name == "" {
					name = toolChoice.Get("function.name").String()
				}
				if name == "" {
					name = toolChoice.Get("custom.name").String()
				}
				namespace := toolChoice.Get("namespace").String()
				if namespace == "" {
					namespace = toolChoice.Get("function.namespace").String()
				}
				if namespace == "" {
					namespace = toolChoice.Get("custom.namespace").String()
				}
				name = toolIdentity.ResolveName(name, namespace)
				if name != "" {
					choice := []byte(`{"type":"function","function":{"name":""}}`)
					choice, _ = sjson.SetBytes(choice, "function.name", name)
					out, _ = sjson.SetRawBytes(out, "tool_choice", choice)
				}
			} else {
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(toolChoice.Raw))
			}
		}
	}

	return out
}

func convertResponsesTextFormatToChatResponseFormat(textFormat gjson.Result) []byte {
	formatType := textFormat.Get("type").String()
	switch formatType {
	case "text", "json_object":
		responseFormat := []byte(`{"type":""}`)
		responseFormat, _ = sjson.SetBytes(responseFormat, "type", formatType)
		return responseFormat
	case "json_schema":
		responseFormat := []byte(`{"type":"json_schema","json_schema":{}}`)
		for _, field := range []string{"name", "description", "strict"} {
			if value := textFormat.Get(field); value.Exists() {
				responseFormat, _ = sjson.SetBytes(responseFormat, "json_schema."+field, value.Value())
			}
		}
		if schema := textFormat.Get("schema"); schema.Exists() {
			responseFormat, _ = sjson.SetRawBytes(responseFormat, "json_schema.schema", []byte(schema.Raw))
		}
		return responseFormat
	default:
		return nil
	}
}

func setResponsesToolOutputContent(toolMessage []byte, output gjson.Result) []byte {
	structured := output
	if output.Type == gjson.String {
		if !gjson.Valid(output.String()) {
			toolMessage, _ = sjson.SetBytes(toolMessage, "content", output.String())
			return toolMessage
		}
		structured = gjson.Parse(output.String())
	}

	if !responsesToolOutputHasImage(structured) {
		toolMessage, _ = sjson.SetBytes(toolMessage, "content", translatorcommon.ResponsesToolOutputText(output))
		return toolMessage
	}

	toolMessage, _ = sjson.SetRawBytes(toolMessage, "content", []byte(`[]`))
	for _, part := range structured.Array() {
		toolMessage, _ = sjson.SetRawBytes(toolMessage, "content.-1", responsesChatToolOutputPart(part))
	}
	return toolMessage
}

func responsesToolOutputHasImage(output gjson.Result) bool {
	if !output.IsArray() {
		return false
	}
	hasImage := false
	for _, part := range output.Array() {
		switch part.Get("type").String() {
		case "text", "input_text", "output_text":
			if part.Get("text").Type != gjson.String {
				return false
			}
		case "image_url", "input_image":
			if responsesToolOutputImageURL(part) == "" {
				return false
			}
			hasImage = true
		default:
			return false
		}
	}
	return hasImage
}

func responsesChatToolOutputPart(part gjson.Result) []byte {
	switch part.Get("type").String() {
	case "text", "input_text", "output_text":
		content := []byte(`{"type":"text","text":""}`)
		content, _ = sjson.SetBytes(content, "text", part.Get("text").String())
		return content
	case "image_url", "input_image":
		content := []byte(`{"type":"image_url","image_url":{"url":""}}`)
		content, _ = sjson.SetBytes(content, "image_url.url", responsesToolOutputImageURL(part))
		if detail := responsesToolOutputImageDetail(part); detail != "" {
			content, _ = sjson.SetBytes(content, "image_url.detail", detail)
		}
		return content
	default:
		content := []byte(`{"type":"text","text":""}`)
		content, _ = sjson.SetBytes(content, "text", part.Raw)
		return content
	}
}

func responsesToolOutputImageURL(part gjson.Result) string {
	if part.Get("type").String() == "input_image" {
		return strings.TrimSpace(part.Get("image_url").String())
	}
	return strings.TrimSpace(part.Get("image_url.url").String())
}

func responsesToolOutputImageDetail(part gjson.Result) string {
	if part.Get("type").String() == "input_image" {
		return strings.TrimSpace(part.Get("detail").String())
	}
	return strings.TrimSpace(part.Get("image_url.detail").String())
}

func collectOpenAIResponsesReasoningContent(item gjson.Result) string {
	var reasoningText strings.Builder
	if summary := item.Get("summary"); summary.Exists() && summary.IsArray() {
		summary.ForEach(
			func(_, summaryItem gjson.Result) bool {
				if summaryItem.Get("type").String() == "summary_text" {
					reasoningText.WriteString(summaryItem.Get("text").String())
				}
				return true
			},
		)
	}
	if reasoningText.Len() == 0 {
		return "[reasoning unavailable]"
	}
	return reasoningText.String()
}

func combineOpenAIResponsesReasoning(existing, incoming string) string {
	existingTrimmed := strings.TrimSpace(existing)
	incomingTrimmed := strings.TrimSpace(incoming)

	switch {
	case existingTrimmed == "":
		return incoming
	case incomingTrimmed == "":
		return existing
	case existingTrimmed == "[reasoning unavailable]":
		return incoming
	case incomingTrimmed == "[reasoning unavailable]", existingTrimmed == incomingTrimmed:
		return existing
	default:
		return existing + "\n\n" + incoming
	}
}
