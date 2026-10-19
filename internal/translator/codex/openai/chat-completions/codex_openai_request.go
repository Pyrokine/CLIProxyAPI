// Package chat_completions provides utilities to translate OpenAI Chat Completions
// request JSON into OpenAI Responses API request JSON using gjson/sjson.
// It supports tools, multimodal text/image inputs, and Structured Outputs.
// The package handles the conversion of OpenAI API requests into the format
// expected by the OpenAI Responses API, including proper mapping of messages,
// tools, and generation parameters.
package chat_completions

import (
	"strconv"
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIRequestToCodex converts an OpenAI Chat Completions request JSON
// into an OpenAI Responses API request JSON. The transformation follows the
// examples defined in docs/2.md exactly, including tools, multi-turn dialog,
// multimodal text/image handling, and Structured Outputs mapping.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI Chat Completions API
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in OpenAI Responses API format
func ConvertOpenAIRequestToCodex(modelName string, inputRawJSON []byte, stream bool) []byte {
	out, _ := convertOpenAIRequestToCodexChecked(modelName, inputRawJSON, stream)
	return out
}

func convertOpenAIRequestToCodexChecked(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	var drops translatorcommon.UserTurnDrops
	rawJSON := inputRawJSON
	// Start with empty JSON object
	out := []byte(`{"instructions":""}`)

	// Stream must be set to true
	out, _ = sjson.SetBytes(out, "stream", stream)

	// Codex not support temperature, top_p, top_k, max_output_tokens, so comment them
	// if v := gjson.GetBytes(rawJSON, "temperature"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "temperature", v.Value())
	// }
	// if v := gjson.GetBytes(rawJSON, "top_p"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "top_p", v.Value())
	// }
	// if v := gjson.GetBytes(rawJSON, "top_k"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "top_k", v.Value())
	// }

	// Map token limits
	// if v := gjson.GetBytes(rawJSON, "max_tokens"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "max_output_tokens", v.Value())
	// }
	// if v := gjson.GetBytes(rawJSON, "max_completion_tokens"); v.Exists() {
	// 	out, _ = sjson.SetBytes(out, "max_output_tokens", v.Value())
	// }

	// Map reasoning effort
	if v := gjson.GetBytes(rawJSON, "reasoning_effort"); v.Exists() {
		out, _ = sjson.SetBytes(out, "reasoning.effort", v.Value())
	} else {
		out, _ = sjson.SetBytes(out, "reasoning.effort", "medium")
	}
	out, _ = sjson.SetBytes(out, "parallel_tool_calls", true)
	out, _ = sjson.SetBytes(out, "include", []string{"reasoning.encrypted_content"})

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)
	if value := gjson.GetBytes(rawJSON, "service_tier"); value.Type == gjson.String {
		if tier := strings.ToLower(strings.TrimSpace(value.String())); tier != "" {
			if tier == "fast" {
				tier = "priority"
			}
			out, _ = sjson.SetBytes(out, "service_tier", tier)
		}
	}

	// Build request-local tool metadata and name shortening map.
	tools := gjson.GetBytes(rawJSON, "tools")
	toolResults := tools.Array()
	originalToolNameMap := buildShortNameMap(rawJSON)
	customToolNames := map[string]struct{}{}
	functionToolNames := map[string]struct{}{}
	if tools.IsArray() && len(toolResults) > 0 {
		for _, tool := range toolResults {
			switch tool.Get("type").String() {
			case "function":
				functionToolNames[tool.Get("function.name").String()] = struct{}{}
			case "custom":
				customToolNames[tool.Get("name").String()] = struct{}{}
			}
		}
		// A normalized function envelope cannot disambiguate declarations that share a name.
		// Preserve function behavior for such ambiguous names.
		for name := range functionToolNames {
			delete(customToolNames, name)
		}
	}

	resolveToolCall := func(toolCall gjson.Result) (callType, name, input string, valid bool) {
		switch toolCall.Get("type").String() {
		case "custom":
			return "custom", toolCall.Get("custom.name").String(), toolCall.Get("custom.input").String(), true
		case "function":
			name = toolCall.Get("function.name").String()
			callType = "function"
			input = toolCall.Get("function.arguments").String()
			if _, custom := customToolNames[name]; custom {
				callType = "custom"
				if name == "apply_patch" {
					var patchInput translatorcommon.ResponsesPatchInput
					if _, err := patchInput.Finish(input); err == nil {
						input = patchInput.Input()
					}
				}
			}
			return callType, name, input, true
		default:
			return "", "", "", false
		}
	}

	// Extract system instructions from first system message (string or text object)
	messages := gjson.GetBytes(rawJSON, "messages")
	type pendingToolCall struct {
		callID       string
		sourceCallID string
		callType     string
		consumed     bool
	}
	var pendingToolCalls []pendingToolCall
	ambiguousToolCallIDs := map[string]struct{}{}
	// if messages.IsArray() {
	// 	arr := messages.Array()
	// 	for i := 0; i < len(arr); i++ {
	// 		m := arr[i]
	// 		if m.Get("role").String() == "system" {
	// 			c := m.Get("content")
	// 			if c.Type == gjson.String {
	// 				out, _ = sjson.SetBytes(out, "instructions", c.String())
	// 			} else if c.IsObject() && c.Get("type").String() == "text" {
	// 				out, _ = sjson.SetBytes(out, "instructions", c.Get("text").String())
	// 			}
	// 			break
	// 		}
	// 	}
	// }

	// Build input from messages, handling all message types including tool calls
	out, _ = sjson.SetRawBytes(out, "input", []byte(`[]`))
	if messages.IsArray() {
		arr := messages.Array()
		for i := range arr {
			m := arr[i]
			role := m.Get("role").String()

			switch role {
			case "tool":
				toolCallID := m.Get("tool_call_id").String()
				if _, ambiguous := ambiguousToolCallIDs[toolCallID]; toolCallID != "" && ambiguous {
					continue
				}

				pendingIndex := -1
				for index := range pendingToolCalls {
					pendingCall := &pendingToolCalls[index]
					if pendingCall.consumed {
						continue
					}
					if toolCallID == "" || pendingCall.sourceCallID == toolCallID || pendingCall.callID == toolCallID {
						pendingIndex = index
						break
					}
				}
				if pendingIndex < 0 {
					continue
				}

				pendingCall := &pendingToolCalls[pendingIndex]
				pendingCall.consumed = true
				toolCallID = pendingCall.callID
				outputType := "function_call_output"
				if pendingCall.callType == "custom" {
					outputType = "custom_tool_call_output"
				}

				toolOutput := []byte(`{}`)
				toolOutput, _ = sjson.SetBytes(toolOutput, "type", outputType)
				toolOutput, _ = sjson.SetBytes(toolOutput, "call_id", toolCallID)
				toolOutput = setToolCallOutputContent(toolOutput, m.Get("content"))
				out, _ = sjson.SetRawBytes(out, "input.-1", toolOutput)

			default:
				// A new conversational message starts a new tool-call batch.
				pendingToolCalls = nil
				ambiguousToolCallIDs = map[string]struct{}{}

				msg := []byte(`{}`)
				msg, _ = sjson.SetBytes(msg, "type", "message")
				if role == "system" {
					msg, _ = sjson.SetBytes(msg, "role", "developer")
				} else {
					msg, _ = sjson.SetBytes(msg, "role", role)
				}

				msg, _ = sjson.SetRawBytes(msg, "content", []byte(`[]`))

				c := m.Get("content")
				if c.Exists() && c.Type == gjson.String && c.String() != "" {
					partType := "input_text"
					if role == "assistant" {
						partType = "output_text"
					}
					part := []byte(`{}`)
					part, _ = sjson.SetBytes(part, "type", partType)
					part, _ = sjson.SetBytes(part, "text", c.String())
					msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
				} else if c.Exists() && c.IsArray() {
					items := c.Array()
					for j := range items {
						it := items[j]
						switch it.Get("type").String() {
						case "text":
							if it.Get("text").String() == "" {
								continue
							}
							partType := "input_text"
							if role == "assistant" {
								partType = "output_text"
							}
							part := []byte(`{}`)
							part, _ = sjson.SetBytes(part, "type", partType)
							part, _ = sjson.SetBytes(part, "text", it.Get("text").String())
							msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
						case "image_url":
							if role == "user" {
								if imageURL := it.Get("image_url.url").String(); imageURL != "" {
									part := []byte(`{"type":"input_image"}`)
									part, _ = sjson.SetBytes(part, "image_url", imageURL)
									msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
								} else {
									drops.Drop("image_url")
								}
							}
						case "file":
							if role == "user" {
								parts := appendToolOutputContentPart([]byte(`[]`), it)
								part := gjson.GetBytes(parts, "0")
								if part.Get("type").String() == "input_file" {
									msg, _ = sjson.SetRawBytes(msg, "content.-1", []byte(part.Raw))
								} else {
									drops.Drop("file")
								}
							}
						case "input_audio":
							if role == "user" {
								audio := it.Get("input_audio")
								if data := audio.Get("data"); data.Type == gjson.String && data.String() != "" {
									part := []byte(`{"type":"input_audio"}`)
									part, _ = sjson.SetBytes(part, "data", data.String())
									if format := audio.Get("format"); format.Exists() {
										part, _ = sjson.SetRawBytes(part, "format", []byte(format.Raw))
									}
									msg, _ = sjson.SetRawBytes(msg, "content.-1", part)
								} else {
									drops.Drop("input_audio")
								}
							}
						case "audio", "input_file", "video", "video_url":
							if role == "user" {
								drops.Drop(it.Get("type").String())
							}
						}
					}
				}
				if role == "user" {
					drops.EndTurn(len(gjson.GetBytes(msg, "content").Array()))
				}

				// Don't emit empty assistant messages when only tool_calls
				// are present — Responses API needs function_call items
				// directly, otherwise call_id matching fails (#2132).
				if role != "assistant" || len(gjson.GetBytes(msg, "content").Array()) > 0 {
					out, _ = sjson.SetRawBytes(out, "input.-1", msg)
				}

				if role == "assistant" {
					toolCalls := m.Get("tool_calls")
					if toolCalls.Exists() && toolCalls.IsArray() {
						toolCallsArr := toolCalls.Array()
						callIDCounts := map[string]int{}
						usedCallIDs := map[string]struct{}{}
						for _, toolCall := range toolCallsArr {
							_, _, _, valid := resolveToolCall(toolCall)
							callID := toolCall.Get("id").String()
							if valid && callID != "" {
								callIDCounts[callID]++
								usedCallIDs[callID] = struct{}{}
							}
						}
						for callID, count := range callIDCounts {
							if count > 1 {
								ambiguousToolCallIDs[callID] = struct{}{}
							}
						}

						for j, toolCall := range toolCallsArr {
							toolCallType, toolCallName, toolCallInput, valid := resolveToolCall(toolCall)
							if !valid {
								continue
							}
							sourceCallID := toolCall.Get("id").String()
							if _, ambiguous := ambiguousToolCallIDs[sourceCallID]; sourceCallID != "" && ambiguous {
								continue
							}
							callID := sourceCallID
							if callID == "" {
								baseCallID := "call_missing_" + strconv.Itoa(i) + "_" + strconv.Itoa(j)
								callID = baseCallID
								for suffix := 1; ; suffix++ {
									if _, used := usedCallIDs[callID]; !used {
										break
									}
									callID = baseCallID + "_" + strconv.Itoa(suffix)
								}
								usedCallIDs[callID] = struct{}{}
							}
							pendingToolCalls = append(
								pendingToolCalls, pendingToolCall{
									callID:       callID,
									sourceCallID: sourceCallID,
									callType:     toolCallType,
								},
							)

							if short, ok := originalToolNameMap[toolCallName]; ok {
								toolCallName = short
							} else {
								toolCallName = shortenNameIfNeeded(toolCallName)
							}
							switch toolCallType {
							case "function":
								functionCall := []byte(`{}`)
								functionCall, _ = sjson.SetBytes(functionCall, "type", "function_call")
								functionCall, _ = sjson.SetBytes(functionCall, "call_id", callID)
								functionCall, _ = sjson.SetBytes(functionCall, "name", toolCallName)
								functionCall, _ = sjson.SetBytes(functionCall, "arguments", toolCallInput)
								out, _ = sjson.SetRawBytes(out, "input.-1", functionCall)
							case "custom":
								customCall := []byte(`{}`)
								customCall, _ = sjson.SetBytes(customCall, "type", "custom_tool_call")
								customCall, _ = sjson.SetBytes(customCall, "call_id", callID)
								customCall, _ = sjson.SetBytes(customCall, "name", toolCallName)
								customCall, _ = sjson.SetBytes(customCall, "input", toolCallInput)
								out, _ = sjson.SetRawBytes(out, "input.-1", customCall)
							}
						}
					}
				}
			}
		}
	}

	// Map response_format and text settings to Responses API text.format
	rf := gjson.GetBytes(rawJSON, "response_format")
	text := gjson.GetBytes(rawJSON, "text")
	if rf.Exists() {
		// Always create text object when response_format provided
		if !gjson.GetBytes(out, "text").Exists() {
			out, _ = sjson.SetRawBytes(out, "text", []byte(`{}`))
		}

		rft := rf.Get("type").String()
		switch rft {
		case "text":
			out, _ = sjson.SetBytes(out, "text.format.type", "text")
		case "json_schema":
			js := rf.Get("json_schema")
			if js.Exists() {
				out, _ = sjson.SetBytes(out, "text.format.type", "json_schema")
				if v := js.Get("name"); v.Exists() {
					out, _ = sjson.SetBytes(out, "text.format.name", v.Value())
				}
				if v := js.Get("strict"); v.Exists() {
					out, _ = sjson.SetBytes(out, "text.format.strict", v.Value())
				}
				if v := js.Get("schema"); v.Exists() {
					out, _ = sjson.SetRawBytes(out, "text.format.schema", []byte(v.Raw))
				}
			}
		}

		// Map verbosity if provided
		if text.Exists() {
			if v := text.Get("verbosity"); v.Exists() {
				out, _ = sjson.SetBytes(out, "text.verbosity", v.Value())
			}
		}
	} else if text.Exists() {
		// If only text.verbosity present (no response_format), map verbosity
		if v := text.Get("verbosity"); v.Exists() {
			if !gjson.GetBytes(out, "text").Exists() {
				out, _ = sjson.SetRawBytes(out, "text", []byte(`{}`))
			}
			out, _ = sjson.SetBytes(out, "text.verbosity", v.Value())
		}
	}

	// Map tools (flatten function fields)
	if tools.IsArray() && len(toolResults) > 0 {
		out, _ = sjson.SetRawBytes(out, "tools", []byte(`[]`))
		for _, tool := range toolResults {
			toolType := tool.Get("type").String()
			if toolType == "custom" {
				item := []byte(tool.Raw)
				name := tool.Get("name").String()
				if short, ok := originalToolNameMap[name]; ok {
					name = short
				} else {
					name = shortenNameIfNeeded(name)
				}
				item, _ = sjson.SetBytes(item, "name", name)
				out, _ = sjson.SetRawBytes(out, "tools.-1", item)
				continue
			}

			// Pass through built-in tools (e.g. {"type":"web_search"}) directly for the Responses API.
			// Only function and custom tools need structural conversion.
			if toolType != "" && toolType != "function" && tool.IsObject() {
				out, _ = sjson.SetRawBytes(out, "tools.-1", []byte(tool.Raw))
				continue
			}

			if toolType == "function" {
				item := []byte(`{}`)
				item, _ = sjson.SetBytes(item, "type", "function")
				function := tool.Get("function")
				if function.Exists() {
					if value := function.Get("name"); value.Exists() {
						name := value.String()
						if short, ok := originalToolNameMap[name]; ok {
							name = short
						} else {
							name = shortenNameIfNeeded(name)
						}
						item, _ = sjson.SetBytes(item, "name", name)
					}
					if value := function.Get("description"); value.Exists() {
						item, _ = sjson.SetBytes(item, "description", value.Value())
					}
					if value := function.Get("parameters"); value.Exists() {
						item, _ = sjson.SetRawBytes(item, "parameters", []byte(value.Raw))
					}
					if value := function.Get("strict"); value.Exists() {
						item, _ = sjson.SetBytes(item, "strict", value.Value())
					} else {
						item, _ = sjson.SetBytes(item, "strict", false)
					}
				}
				out, _ = sjson.SetRawBytes(out, "tools.-1", item)
			}
		}
	}

	// Map tool_choice when present.
	// Chat Completions: "tool_choice" can be a string ("auto"/"none") or a named tool object.
	// Responses API: keep built-in tool choices as-is and flatten named choices to {"type":"...","name":"..."}.
	if toolChoice := gjson.GetBytes(rawJSON, "tool_choice"); toolChoice.Exists() {
		switch {
		case toolChoice.Type == gjson.String:
			out, _ = sjson.SetBytes(out, "tool_choice", toolChoice.String())
		case toolChoice.IsObject():
			toolChoiceType := toolChoice.Get("type").String()
			if toolChoiceType == "function" || toolChoiceType == "custom" {
				name := toolChoice.Get("name").String()
				if toolChoiceType == "function" {
					name = toolChoice.Get("function.name").String()
					if _, custom := customToolNames[name]; custom {
						toolChoiceType = "custom"
					}
				}
				if name != "" {
					if short, ok := originalToolNameMap[name]; ok {
						name = short
					} else {
						name = shortenNameIfNeeded(name)
					}
				}
				choice := []byte(`{}`)
				choice, _ = sjson.SetBytes(choice, "type", toolChoiceType)
				if name != "" {
					choice, _ = sjson.SetBytes(choice, "name", name)
				}
				out, _ = sjson.SetRawBytes(out, "tool_choice", choice)
			} else if toolChoiceType != "" {
				// Built-in tool choices (e.g. {"type":"web_search"}) are already Responses-compatible.
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(toolChoice.Raw))
			}
		}
	}

	out, _ = sjson.SetBytes(out, "store", false)
	if err := drops.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func setToolCallOutputContent(funcOutput []byte, content gjson.Result) []byte {
	if content.Type == gjson.String && gjson.Valid(content.String()) {
		parts := gjson.Parse(content.String())
		if parts.IsArray() {
			for _, part := range parts.Array() {
				converted := appendToolOutputContentPart([]byte(`[]`), part)
				if gjson.GetBytes(converted, "0.type").String() == "input_image" {
					content = parts
					break
				}
			}
		}
	}
	switch {
	case content.Type == gjson.String:
		funcOutput, _ = sjson.SetBytes(funcOutput, "output", content.String())
	case content.IsArray():
		output := []byte(`[]`)
		for _, item := range content.Array() {
			output = appendToolOutputContentPart(output, item)
		}
		funcOutput, _ = sjson.SetRawBytes(funcOutput, "output", output)
	default:
		fallbackOutput := content.Raw
		if fallbackOutput == "" {
			fallbackOutput = content.String()
		}
		funcOutput, _ = sjson.SetBytes(funcOutput, "output", fallbackOutput)
	}
	return funcOutput
}

func appendToolOutputContentPart(output []byte, item gjson.Result) []byte {
	switch item.Get("type").String() {
	case "input_image":
		if item.Get("image_url").String() == "" && item.Get("file_id").String() == "" {
			return appendToolOutputFallbackPart(output, item)
		}
		output, _ = sjson.SetRawBytes(output, "-1", []byte(item.Raw))
	case "text", "input_text":
		part := []byte(`{}`)
		part, _ = sjson.SetBytes(part, "type", "input_text")
		part, _ = sjson.SetBytes(part, "text", item.Get("text").String())
		output, _ = sjson.SetRawBytes(output, "-1", part)
	case "image_url":
		imageURL := item.Get("image_url.url").String()
		fileID := item.Get("image_url.file_id").String()
		if imageURL == "" && fileID == "" {
			return appendToolOutputFallbackPart(output, item)
		}
		part := []byte(`{}`)
		part, _ = sjson.SetBytes(part, "type", "input_image")
		if imageURL != "" {
			part, _ = sjson.SetBytes(part, "image_url", imageURL)
		}
		if fileID != "" {
			part, _ = sjson.SetBytes(part, "file_id", fileID)
		}
		if detail := item.Get("image_url.detail").String(); detail != "" {
			part, _ = sjson.SetBytes(part, "detail", detail)
		}
		output, _ = sjson.SetRawBytes(output, "-1", part)
	case "file":
		fileID := item.Get("file.file_id").String()
		fileData := item.Get("file.file_data").String()
		fileURL := item.Get("file.file_url").String()
		if fileID == "" && fileData == "" && fileURL == "" {
			return appendToolOutputFallbackPart(output, item)
		}
		part := []byte(`{}`)
		part, _ = sjson.SetBytes(part, "type", "input_file")
		if fileID != "" {
			part, _ = sjson.SetBytes(part, "file_id", fileID)
		}
		if fileData != "" {
			part, _ = sjson.SetBytes(part, "file_data", fileData)
		}
		if fileURL != "" {
			part, _ = sjson.SetBytes(part, "file_url", fileURL)
		}
		if filename := item.Get("file.filename").String(); filename != "" {
			part, _ = sjson.SetBytes(part, "filename", filename)
		}
		output, _ = sjson.SetRawBytes(output, "-1", part)
	default:
		output = appendToolOutputFallbackPart(output, item)
	}
	return output
}

func appendToolOutputFallbackPart(output []byte, item gjson.Result) []byte {
	text := item.Raw
	if text == "" {
		text = item.String()
	}
	part := []byte(`{}`)
	part, _ = sjson.SetBytes(part, "type", "input_text")
	part, _ = sjson.SetBytes(part, "text", text)
	output, _ = sjson.SetRawBytes(output, "-1", part)
	return output
}

// shortenNameIfNeeded 保留长 MCP 名称的末段，并限制字符与长度
func shortenNameIfNeeded(name string) string {
	if len(name) > 64 && strings.HasPrefix(name, "mcp__") {
		if index := strings.LastIndex(name, "__"); index > 0 {
			name = "mcp__" + name[index+2:]
		}
	}
	return util.SanitizeClaudeFunctionName(name)
}

// buildShortNameMap 优先分配声明名称，历史和工具选择不改变已分配的名称
func buildShortNameMap(rawJSON []byte) map[string]string {
	var names []string
	for _, tool := range gjson.GetBytes(rawJSON, "tools").Array() {
		switch tool.Get("type").String() {
		case "function":
			names = append(names, tool.Get("function.name").String())
		case "custom":
			names = append(names, tool.Get("name").String())
		}
	}
	for _, message := range gjson.GetBytes(rawJSON, "messages").Array() {
		if message.Get("role").String() != "assistant" {
			continue
		}
		for _, call := range message.Get("tool_calls").Array() {
			switch call.Get("type").String() {
			case "function":
				names = append(names, call.Get("function.name").String())
			case "custom":
				names = append(names, call.Get("custom.name").String())
			}
		}
	}
	choice := gjson.GetBytes(rawJSON, "tool_choice")
	switch choice.Get("type").String() {
	case "function":
		names = append(names, choice.Get("function.name").String())
	case "custom":
		names = append(names, choice.Get("name").String())
	}

	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, exists := m[name]; exists {
			continue
		}
		base := shortenNameIfNeeded(name)
		candidate := base
		for index := 1; ; index++ {
			if _, exists := used[candidate]; !exists {
				break
			}
			suffix := "_" + strconv.Itoa(index)
			candidate = base[:min(len(base), max(limit-len(suffix), 0))] + suffix
		}
		used[candidate] = struct{}{}
		m[name] = candidate
	}
	return m
}
