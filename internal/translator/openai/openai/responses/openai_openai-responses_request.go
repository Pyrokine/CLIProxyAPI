package responses

import (
	"fmt"
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	log "github.com/sirupsen/logrus"
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
	body, err := convertOpenAIResponsesRequestToOpenAIChatCompletions(modelName, inputRawJSON, stream)
	if err != nil {
		return nil
	}
	return body
}

func convertOpenAIResponsesRequestToOpenAIChatCompletions(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	var drops translatorcommon.UserTurnDrops
	droppedPartTypes := make(map[string]int)
	rawJSON := inputRawJSON
	// Base OpenAI chat completions template with default values
	out := []byte(`{"model":"","messages":[],"stream":false}`)

	root := gjson.ParseBytes(rawJSON)
	toolIdentity := translatorcommon.NewResponsesToolIdentity(rawJSON)
	toolNames := responsesChatToolNameMap(toolIdentity, rawJSON)
	localShell := newResponsesLocalShell(root, toolIdentity, toolNames)
	mapToolName := func(name, namespace string) string {
		qualified := toolIdentity.ResolveName(name, namespace)
		if mapped := toolNames[qualified]; mapped != "" {
			return mapped
		}
		return qualified
	}
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
		out, _ = sjson.SetRawBytes(out, "max_tokens", []byte(maxTokens.Raw))
	}

	// Convert instructions to system message
	if instructions := root.Get("instructions"); instructions.Exists() {
		systemMessage := []byte(`{"role":"system","content":""}`)
		systemMessage, _ = sjson.SetBytes(systemMessage, "content", instructions.String())
		appendMessage(systemMessage)
	}

	// Convert input array to messages
	duplicateOutputIDs := make(map[string]struct{})
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		var pendingToolCalls [][]byte
		awaitingToolOutputs := make(map[string]struct{})
		outputCounts := make(map[string]int)
		pendingReasoningContent := ""
		latestReasoningContent := ""
		hasReasoningInSession := false
		mergeableAssistantIndex := -1

		fallbackReasoning := func() string {
			if latestReasoningContent != "" {
				return latestReasoningContent
			}
			if hasReasoningInSession {
				return "[reasoning unavailable]"
			}
			return ""
		}

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
						if isUsableResponsesReasoning(combinedReasoning) {
							latestReasoningContent = combinedReasoning
						}
					} else if fallback := fallbackReasoning(); fallback != "" {
						updatedMessage, _ = sjson.SetBytes(updatedMessage, "reasoning_content", fallback)
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
					if isUsableResponsesReasoning(reasoningContent) {
						latestReasoningContent = reasoningContent
					}
				} else if fallback := fallbackReasoning(); fallback != "" {
					assistantMessage, _ = sjson.SetBytes(assistantMessage, "reasoning_content", fallback)
				}
				appendMessage(assistantMessage)
			}
			pendingToolCalls = nil
			mergeableAssistantIndex = -1
		}

		appendRegularMessage := func(message []byte) int {
			appendMessage(message)
			return len(messages) - 1
		}

		appendPendingReasoningMessage := func() {
			reasoningContent := takePendingReasoningContent()
			if reasoningContent == "" {
				return
			}
			if isUsableResponsesReasoning(reasoningContent) {
				latestReasoningContent = reasoningContent
			}
			message := []byte(`{"role":"assistant","content":"","reasoning_content":""}`)
			message, _ = sjson.SetBytes(message, "reasoning_content", reasoningContent)
			appendRegularMessage(message)
		}

		rawInput := localShell.history(input.Array())
		explicitOutputCounts := make(map[string]int)
		missingOutputIDs := 0
		for _, item := range rawInput {
			if itemType := item.Get("type").String(); itemType == "function_call_output" || itemType == "custom_tool_call_output" {
				if id := translatorcommon.ExtractResponsesCallID(item); id != "" {
					explicitOutputCounts[id]++
				} else {
					missingOutputIDs++
				}
			}
		}
		unclaimedCalls := make(map[string]struct{})
		for _, item := range rawInput {
			if itemType := item.Get("type").String(); itemType == "function_call" || itemType == "custom_tool_call" {
				if id := translatorcommon.ExtractResponsesCallID(item); id != "" && explicitOutputCounts[id] == 0 {
					unclaimedCalls[id] = struct{}{}
				}
			}
		}
		if effort := root.Get("reasoning.effort"); effort.Exists() {
			value := strings.ToLower(strings.TrimSpace(effort.String()))
			hasReasoningInSession = value != "" && value != "none" && value != "0" && value != "false"
		} else if effort := root.Get("reasoning_effort"); effort.Exists() {
			value := strings.ToLower(strings.TrimSpace(effort.String()))
			hasReasoningInSession = value != "" && value != "none" && value != "0" && value != "false"
		} else if reasoning := root.Get("reasoning"); reasoning.Exists() {
			value := strings.ToLower(strings.TrimSpace(reasoning.Raw))
			hasReasoningInSession = value != "" && value != "none" && value != "false" && value != "{}" && value != "null"
		}
		if !hasReasoningInSession {
			for _, item := range rawInput {
				if item.Get("type").String() == "reasoning" || item.Get("reasoning_content").Exists() {
					hasReasoningInSession = true
					break
				}
			}
		}

		inputArray := translatorcommon.NormalizeResponsesToolCallOutputs(rawInput)
		if missingOutputIDs > 1 || (missingOutputIDs > 0 && len(unclaimedCalls) > 1) {
			for index, item := range inputArray {
				itemType := item.Get("type").String()
				if itemType != "function_call_output" && itemType != "custom_tool_call_output" {
					continue
				}
				if translatorcommon.ExtractResponsesCallID(rawInput[index]) != "" {
					continue
				}
				output := []byte(item.Raw)
				output, _ = sjson.DeleteBytes(output, "call_id")
				output, _ = sjson.DeleteBytes(output, "tool_call_id")
				output, _ = sjson.DeleteBytes(output, "callId")
				inputArray[index] = gjson.ParseBytes(output)
			}
		}
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
				isUserTurn := role == "user"
				if role == "developer" {
					role = "user"
				}
				mergeableAssistantIndex = -1
				if role != "assistant" {
					appendPendingReasoningMessage()
					latestReasoningContent = ""
				}
				message := []byte(`{"role":"","content":[]}`)
				message, _ = sjson.SetBytes(message, "role", role)

				sendable := 0
				dropPart := func(partType string) {
					if isUserTurn {
						drops.Drop(partType)
					}
					droppedPartTypes[partType]++
				}
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
								if strings.TrimSpace(text) != "" {
									sendable++
								}
							case "input_image":
								imageURL := contentItem.Get("image_url")
								if imageURL.Type != gjson.String || strings.TrimSpace(imageURL.String()) == "" {
									dropPart(contentType)
									break
								}
								contentPart := []byte(`{"type":"image_url","image_url":{"url":""}}`)
								contentPart, _ = sjson.SetBytes(contentPart, "image_url.url", imageURL.String())
								if detail := responsesToolOutputImageDetail(contentItem); detail != "" {
									contentPart, _ = sjson.SetBytes(contentPart, "image_url.detail", detail)
								}
								message, _ = sjson.SetRawBytes(message, "content.-1", contentPart)
								sendable++
							case "input_file":
								fileID, fileData := contentItem.Get("file_id"), contentItem.Get("file_data")
								if (fileID.Type != gjson.String || strings.TrimSpace(fileID.String()) == "") && (fileData.Type != gjson.String || strings.TrimSpace(fileData.String()) == "") {
									dropPart(contentType)
									break
								}
								contentPart := []byte(`{"type":"file","file":{}}`)
								for _, field := range []string{"file_id", "file_data", "filename"} {
									if value := contentItem.Get(field); value.Type == gjson.String && value.String() != "" {
										contentPart, _ = sjson.SetBytes(contentPart, "file."+field, value.String())
									}
								}
								message, _ = sjson.SetRawBytes(message, "content.-1", contentPart)
								sendable++
							case "input_audio":
								audio := contentItem.Get("input_audio")
								data := audio.Get("data")
								if data.Type != gjson.String || strings.TrimSpace(data.String()) == "" {
									data = contentItem.Get("data")
								}
								if data.Type != gjson.String || strings.TrimSpace(data.String()) == "" {
									dropPart(contentType)
									break
								}
								format := audio.Get("format")
								if !format.Exists() || (format.Type == gjson.String && strings.TrimSpace(format.String()) == "") {
									format = contentItem.Get("format")
								}
								contentPart := []byte(`{"type":"input_audio","input_audio":{}}`)
								if audio.IsObject() {
									contentPart, _ = sjson.SetRawBytes(contentPart, "input_audio", []byte(audio.Raw))
								}
								contentPart, _ = sjson.SetBytes(contentPart, "input_audio.data", data.String())
								if format.Exists() {
									contentPart, _ = sjson.SetRawBytes(contentPart, "input_audio.format", []byte(format.Raw))
								}
								message, _ = sjson.SetRawBytes(message, "content.-1", contentPart)
								sendable++
							case "input_video", "video_url":
								contentPart := []byte(`{"type":"video_url","video_url":{}}`)
								if videoURL := contentItem.Get("video_url"); videoURL.IsObject() {
									contentPart, _ = sjson.SetRawBytes(contentPart, "video_url", []byte(videoURL.Raw))
								} else if videoURL.Exists() {
									contentPart, _ = sjson.SetRawBytes(contentPart, "video_url.url", []byte(videoURL.Raw))
								}
								if processing := contentItem.Get("processing"); processing.Exists() {
									contentPart, _ = sjson.SetRawBytes(contentPart, "video_url.processing", []byte(processing.Raw))
								}
								message, _ = sjson.SetRawBytes(message, "content.-1", contentPart)
								sendable++
							default:
								dropPart(contentType)
							}
							return true
						},
					)
				} else if content.Type == gjson.String {
					message, _ = sjson.SetBytes(message, "content", content.String())
					if strings.TrimSpace(content.String()) != "" {
						sendable++
					}
				}
				if isUserTurn {
					drops.EndTurn(sendable)
				}

				if role == "assistant" {
					reasoningContent := combineOpenAIResponsesReasoning(
						takePendingReasoningContent(),
						item.Get("reasoning_content").String(),
					)
					if reasoningContent != "" {
						message, _ = sjson.SetBytes(message, "reasoning_content", reasoningContent)
						if isUsableResponsesReasoning(reasoningContent) {
							latestReasoningContent = reasoningContent
						}
					}
				}

				messageIndex := appendRegularMessage(message)
				if role == "assistant" {
					mergeableAssistantIndex = messageIndex
				}

			case "reasoning":
				reasoningContent := collectOpenAIResponsesReasoningContent(item)
				pendingReasoningContent = combineOpenAIResponsesReasoning(pendingReasoningContent, reasoningContent)
				if isUsableResponsesReasoning(reasoningContent) {
					latestReasoningContent = reasoningContent
				}

			case "function_call", "custom_tool_call":
				reasoningContent := item.Get("reasoning_content").String()
				pendingReasoningContent = combineOpenAIResponsesReasoning(pendingReasoningContent, reasoningContent)
				if isUsableResponsesReasoning(reasoningContent) {
					latestReasoningContent = reasoningContent
				}
				toolCall := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
				if callID := translatorcommon.ExtractResponsesCallID(item); callID != "" {
					toolCall, _ = sjson.SetBytes(toolCall, "id", callID)
					awaitingToolOutputs[callID] = struct{}{}
				}
				name := mapToolName(item.Get("name").String(), item.Get("namespace").String())
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
				callID := translatorcommon.ExtractResponsesCallID(item)
				if callID != "" {
					outputCounts[callID]++
					if outputCounts[callID] > 1 {
						duplicateOutputIDs[callID] = struct{}{}
					}
				}
				output := item.Get("output")
				if _, expected := awaitingToolOutputs[callID]; expected {
					delete(awaitingToolOutputs, callID)
					toolMessage := []byte(`{"role":"tool","tool_call_id":"","content":""}`)
					toolMessage, _ = sjson.SetBytes(toolMessage, "tool_call_id", callID)
					if output.Exists() {
						toolMessage = setResponsesToolOutputContent(toolMessage, output)
					}
					appendMessage(toolMessage)
				} else if output.Exists() {
					userMessage := setResponsesToolOutputContent([]byte(`{"role":"user","content":""}`), output)
					content := gjson.GetBytes(userMessage, "content")
					if (content.Type == gjson.String && strings.TrimSpace(content.String()) != "") || (content.IsArray() && len(content.Array()) > 0) {
						appendRegularMessage(userMessage)
					}
				}

			default:
				mergeableAssistantIndex = -1
			}
		}
		emitPendingToolCalls()
		appendPendingReasoningMessage()
	} else if input.Type == gjson.String {
		msg := []byte(`{}`)
		msg, _ = sjson.SetBytes(msg, "role", "user")
		msg, _ = sjson.SetBytes(msg, "content", input.String())
		appendMessage(msg)
	}

	if len(messages) > 0 {
		var ambiguous []string
		for id := range duplicateOutputIDs {
			ambiguous = append(ambiguous, id)
		}
		messages = translatorcommon.AlignOpenAIToolCallMessages(messages, ambiguous...)
		out = translatorcommon.SetRawArrayItems(out, "messages", messages)
	}

	// Convert the winning top-level and Responses Lite additional_tools declarations.
	var chatCompletionsTools []any
	if localShell.Available {
		chatCompletionsTools = append(chatCompletionsTools, gjson.ParseBytes(localShell.declaration()).Value())
	}
	for _, declaration := range toolIdentity.Declarations() {
		declaration.QualifiedName = toolNames[declaration.QualifiedName]
		chatTool, ok := convertResponsesDeclarationToOpenAIChat(declaration)
		if !ok {
			continue
		}
		chatCompletionsTools = append(chatCompletionsTools, gjson.ParseBytes(chatTool).Value())
	}
	if len(chatCompletionsTools) > 0 {
		out, _ = sjson.SetBytes(out, "tools", chatCompletionsTools)
		if parallelToolCalls := root.Get("parallel_tool_calls"); parallelToolCalls.Exists() {
			out, _ = sjson.SetBytes(out, "parallel_tool_calls", parallelToolCalls.Bool())
		}
	}

	if reasoningEffort := root.Get("reasoning.effort"); reasoningEffort.Exists() {
		effort := strings.ToLower(strings.TrimSpace(reasoningEffort.String()))
		if effort != "" {
			out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
		}
	}

	// Convert tool_choice if present.
	if toolChoice := root.Get("tool_choice"); len(chatCompletionsTools) > 0 && toolChoice.Exists() {
		if toolChoice.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "tool_choice", toolChoice.String())
		} else if toolChoice.IsObject() {
			choiceType := strings.TrimSpace(toolChoice.Get("type").String())
			if choiceType == "shell" && localShell.Available {
				choice := []byte(`{"type":"function","function":{"name":""}}`)
				choice, _ = sjson.SetBytes(choice, "function.name", localShell.Name)
				out, _ = sjson.SetRawBytes(out, "tool_choice", choice)
			} else if choiceType == "function" || choiceType == "custom" {
				name, namespace := responsesToolChoiceName(toolChoice)
				name = mapToolName(name, namespace)
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

	if len(droppedPartTypes) > 0 {
		log.Warnf("responses->chat: dropped unsupported user content types %q (model=%q)", fmt.Sprint(droppedPartTypes), modelName)
	}
	if err := drops.Err(); err != nil {
		return nil, err
	}
	return out, nil
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
			detail := part.Get("image_url.detail")
			if part.Get("type").String() == "input_image" {
				detail = part.Get("detail")
			}
			if detail.Exists() && detail.Type != gjson.String {
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
	url := part.Get("image_url.url")
	if part.Get("type").String() == "input_image" {
		url = part.Get("image_url")
	}
	if url.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(url.String())
}

func responsesToolOutputImageDetail(part gjson.Result) string {
	detail := part.Get("image_url.detail")
	if part.Get("type").String() == "input_image" {
		detail = part.Get("detail")
	}
	if detail.Type != gjson.String {
		if detail.Exists() {
			log.Warn("responses->chat: ignored non-string image detail")
		}
		return ""
	}
	value := strings.TrimSpace(detail.String())
	switch value {
	case "original":
		return "high"
	case "auto", "low", "high", "":
		return value
	default:
		log.Warn("responses->chat: ignored unsupported image detail")
		return ""
	}
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

func isUsableResponsesReasoning(reasoning string) bool {
	trimmed := strings.TrimSpace(reasoning)
	return trimmed != "" && trimmed != "[reasoning unavailable]"
}
