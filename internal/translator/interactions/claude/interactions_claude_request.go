package claude

import (
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func convertClaudeRequestToInteractionsDefault(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertClaudeRequestToInteractions(modelName, inputRawJSON, stream, false)
}

// ConvertClaudeRequestToInteractionsWithCompat preserves empty assistant
// thinking blocks for configured compatibility endpoints.
func ConvertClaudeRequestToInteractionsWithCompat(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertClaudeRequestToInteractions(modelName, inputRawJSON, stream, true)
}

func convertClaudeRequestToInteractions(
	modelName string,
	inputRawJSON []byte,
	stream bool,
	preserveEmptyThinkingBlocks bool,
) []byte {
	body, err := convertClaudeRequestToInteractionsChecked(modelName, inputRawJSON, stream, preserveEmptyThinkingBlocks)
	if err != nil {
		log.WithError(err).Warn("Claude to Interactions request translation rejected")
		return nil
	}
	return body
}

// ConvertClaudeRequestToInteractionsWithCompatReturningError reports unsupported user attachments.
func ConvertClaudeRequestToInteractionsWithCompatReturningError(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return convertClaudeRequestToInteractionsChecked(modelName, inputRawJSON, stream, true)
}

func convertClaudeRequestToInteractionsChecked(
	modelName string,
	inputRawJSON []byte,
	stream bool,
	preserveEmptyThinkingBlocks bool,
) ([]byte, error) {
	root := gjson.ParseBytes(inputRawJSON)
	out := []byte(`{"model":"","input":[]}`)
	out, _ = sjson.SetBytes(out, "model", firstNonEmpty(modelName, root.Get("model").String()))
	if streamValue, ok := claudeRequestStreamValue(root, stream); ok {
		out, _ = sjson.SetBytes(out, "stream", streamValue)
	}
	out = copyClaudeSystemToInteractions(out, root)
	out = copyClaudeGenerationConfigToInteractions(out, root)
	var drops translatorcommon.UserTurnDrops
	out = appendClaudeMessagesToInteractions(
		out,
		root.Get("messages"),
		preserveEmptyThinkingBlocks,
		&drops,
	)
	out = copyClaudeToolsToInteractions(out, root)
	return out, drops.Err()
}

func claudeRequestStreamValue(root gjson.Result, stream bool) (bool, bool) {
	if value := root.Get("stream"); value.Exists() {
		return value.Bool(), true
	}
	if stream {
		return true, true
	}
	return false, false
}

func copyClaudeSystemToInteractions(out []byte, root gjson.Result) []byte {
	text := claudeText(root.Get("system"))
	if text == "" {
		return out
	}
	out, _ = sjson.SetBytes(out, "system_instruction", text)
	return out
}

func copyClaudeGenerationConfigToInteractions(out []byte, root gjson.Result) []byte {
	out = copyClaudeJSONField(out, root, "max_tokens", "generation_config.max_output_tokens")
	out = copyClaudeJSONField(out, root, "temperature", "generation_config.temperature")
	out = copyClaudeJSONField(out, root, "top_p", "generation_config.top_p")
	out = copyClaudeJSONField(out, root, "stop_sequences", "generation_config.stop_sequences")
	out = copyClaudeThinkingToInteractions(out, root)
	return copyClaudeToolChoiceToInteractions(out, root.Get("tool_choice"))
}

func copyClaudeJSONField(out []byte, root gjson.Result, from, to string) []byte {
	value := root.Get(from)
	if !value.Exists() {
		return out
	}
	out, _ = sjson.SetRawBytes(out, to, []byte(value.Raw))
	return out
}

func copyClaudeThinkingToInteractions(out []byte, root gjson.Result) []byte {
	thinking := root.Get("thinking")
	if thinking.Exists() {
		switch strings.ToLower(strings.TrimSpace(thinking.Get("type").String())) {
		case "disabled":
			out, _ = sjson.SetBytes(out, "generation_config.thinking_level", "none")
		case "enabled":
			if budget := thinking.Get("budget_tokens"); budget.Exists() {
				out, _ = sjson.SetRawBytes(out, "generation_config.thinking_config.thinking_budget", []byte(budget.Raw))
			} else {
				out, _ = sjson.SetBytes(out, "generation_config.thinking_level", "high")
			}
		case "adaptive":
			out, _ = sjson.SetBytes(out, "generation_config.thinking_level", "auto")
		}
	}
	if effort := root.Get("output_config.effort"); effort.Exists() && effort.Type == gjson.String {
		out, _ = sjson.SetBytes(
			out, "generation_config.thinking_level", strings.ToLower(strings.TrimSpace(effort.String())),
		)
	}
	return out
}

func copyClaudeToolChoiceToInteractions(out []byte, toolChoice gjson.Result) []byte {
	if !toolChoice.Exists() {
		return out
	}
	// Tool choice accepts only the documented string and object forms.
	// noinspection GoSwitchMissingCasesForIotaConsts
	switch toolChoice.Type {
	case gjson.String:
		switch strings.ToLower(strings.TrimSpace(toolChoice.String())) {
		case "auto":
			out, _ = sjson.SetBytes(out, "generation_config.tool_choice", "auto")
		case "any", "required":
			out, _ = sjson.SetBytes(out, "generation_config.tool_choice", "required")
		}
	case gjson.JSON:
		toolType := strings.ToLower(strings.TrimSpace(toolChoice.Get("type").String()))
		switch toolType {
		case "auto":
			out, _ = sjson.SetBytes(out, "generation_config.tool_choice", "auto")
		case "any", "required":
			out, _ = sjson.SetBytes(out, "generation_config.tool_choice", "required")
		case "tool":
			name := strings.TrimSpace(toolChoice.Get("name").String())
			if name != "" {
				choice := []byte(`{"type":"function","name":""}`)
				choice, _ = sjson.SetBytes(choice, "name", name)
				out, _ = sjson.SetRawBytes(out, "generation_config.tool_choice", choice)
			}
		}
	}
	return out
}

func appendClaudeMessagesToInteractions(
	out []byte,
	messages gjson.Result,
	preserveEmptyThinkingBlocks bool,
	drops *translatorcommon.UserTurnDrops,
) []byte {
	if !messages.Exists() || !messages.IsArray() {
		return out
	}
	toolNamesByID := make(map[string]string)
	var pendingToolUseIDs []string
	var pendingSystemReminders [][]byte
	for _, message := range messages.Array() {
		role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
		if role == "system" || role == "developer" {
			if text, ok := translatorcommon.ClaudeMessageSystemReminderText(message.Get("content")); ok {
				step, _ := sjson.SetBytes([]byte(`{"type":"user_input","content":[{"type":"text","text":""}]}`), "content.0.text", text)
				if len(pendingToolUseIDs) > 0 {
					pendingSystemReminders = append(pendingSystemReminders, step)
				} else {
					out, _ = sjson.SetRawBytes(out, "input.-1", step)
				}
			}
			continue
		}
		if role == "user" && len(pendingToolUseIDs) > 0 && message.Get("content").IsArray() {
			content := translatorcommon.AlignClaudeToolResults(message.Get("content"), pendingToolUseIDs)
			body, _ := sjson.SetRawBytes([]byte(message.Raw), "content", []byte(content.Raw))
			message = gjson.ParseBytes(body)
		}
		pendingToolUseIDs = nil
		out = appendClaudeMessageToInteractions(out, message, preserveEmptyThinkingBlocks, toolNamesByID, drops, &pendingToolUseIDs, &pendingSystemReminders)
		for _, reminder := range pendingSystemReminders {
			out, _ = sjson.SetRawBytes(out, "input.-1", reminder)
		}
		pendingSystemReminders = nil
	}
	for _, reminder := range pendingSystemReminders {
		out, _ = sjson.SetRawBytes(out, "input.-1", reminder)
	}
	return out
}

func appendClaudeMessageToInteractions(
	out []byte,
	message gjson.Result,
	preserveEmptyThinkingBlocks bool,
	toolNamesByID map[string]string,
	drops *translatorcommon.UserTurnDrops,
	pendingToolUseIDs *[]string,
	pendingSystemReminders *[][]byte,
) []byte {
	role := strings.ToLower(strings.TrimSpace(message.Get("role").String()))
	sendable := 0
	flushReminders := func() {
		for _, reminder := range *pendingSystemReminders {
			out, _ = sjson.SetRawBytes(out, "input.-1", reminder)
		}
		*pendingSystemReminders = nil
	}
	defaultStepType := "user_input"
	if role == "assistant" {
		defaultStepType = "model_output"
	}
	content := message.Get("content")
	if content.Type == gjson.String {
		flushReminders()
		step := []byte(`{"type":"","content":[{"type":"text","text":""}]}`)
		step, _ = sjson.SetBytes(step, "type", defaultStepType)
		step, _ = sjson.SetBytes(step, "content.0.text", content.String())
		out, _ = sjson.SetRawBytes(out, "input.-1", step)
		return out
	}
	if !content.IsArray() {
		return out
	}
	stepContent := []byte(`[]`)
	flushContent := func() {
		if len(gjson.ParseBytes(stepContent).Array()) == 0 {
			return
		}
		step := []byte(`{"type":"","content":[]}`)
		step, _ = sjson.SetBytes(step, "type", defaultStepType)
		step, _ = sjson.SetRawBytes(step, "content", stepContent)
		out, _ = sjson.SetRawBytes(out, "input.-1", step)
		stepContent = []byte(`[]`)
	}
	content.ForEach(
		func(_, part gjson.Result) bool {
			partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
			switch partType {
			case "text":
				if text := part.Get("text").String(); text != "" {
					if len(*pendingSystemReminders) > 0 {
						flushContent()
						flushReminders()
					}
					contentPart := []byte(`{"type":"text","text":""}`)
					contentPart, _ = sjson.SetBytes(contentPart, "text", text)
					stepContent, _ = sjson.SetRawBytes(stepContent, "-1", contentPart)
					if strings.TrimSpace(text) != "" {
						sendable++
					}
				}
			case "thinking":
				flushContent()
				text := part.Get("thinking").String()
				if text != "" || preserveEmptyThinkingBlocks {
					step := []byte(`{"type":"thought","content":[{"type":"text","text":""}]}`)
					step, _ = sjson.SetBytes(step, "content.0.text", text)
					out, _ = sjson.SetRawBytes(out, "input.-1", step)
					if strings.TrimSpace(text) != "" {
						sendable++
					}
				}
			case "image", "document", "container_upload":
				if mediaPart, ok := claudeMediaPartToInteractions(part, partType); ok {
					if len(*pendingSystemReminders) > 0 {
						flushContent()
						flushReminders()
					}
					stepContent, _ = sjson.SetRawBytes(stepContent, "-1", mediaPart)
					sendable++
				} else if role == "user" {
					drops.Drop(partType)
				}
			case "tool_use":
				flushContent()
				if id := part.Get("id").String(); id != "" {
					*pendingToolUseIDs = append(*pendingToolUseIDs, id)
					if name := part.Get("name").String(); name != "" {
						toolNamesByID[id] = name
					}
				}
				out = appendClaudeToolUseToInteractions(out, part)
				sendable++
			case "tool_result":
				flushContent()
				out = appendClaudeToolResultToInteractions(out, part, toolNamesByID)
				sendable++
			}
			return true
		},
	)
	flushContent()
	if role == "user" {
		drops.EndTurn(sendable)
	}
	return out
}

func claudeMediaPartToInteractions(part gjson.Result, partType string) ([]byte, bool) {
	source := part.Get("source")
	if sourceType := source.Get("type").String(); sourceType != "" && sourceType != "base64" {
		return nil, false
	}
	mimeType := source.Get("media_type").String()
	data := source.Get("data").String()
	if mimeType == "" {
		mimeType = part.Get("mime_type").String()
	}
	if data == "" {
		data = part.Get("data").String()
	}
	if mimeType == "" || data == "" {
		return nil, false
	}
	out := []byte(`{"type":"","mime_type":"","data":""}`)
	out, _ = sjson.SetBytes(out, "type", partType)
	out, _ = sjson.SetBytes(out, "mime_type", mimeType)
	out, _ = sjson.SetBytes(out, "data", data)
	return out, true
}

func appendClaudeToolUseToInteractions(out []byte, part gjson.Result) []byte {
	step := []byte(`{"type":"function_call","name":"","arguments":{}}`)
	step, _ = sjson.SetBytes(step, "name", part.Get("name").String())
	if id := part.Get("id").String(); id != "" {
		step, _ = sjson.SetBytes(step, "id", id)
	}
	input := part.Get("input")
	if input.Exists() && input.IsObject() {
		step, _ = sjson.SetRawBytes(step, "arguments", []byte(input.Raw))
	}
	out, _ = sjson.SetRawBytes(out, "input.-1", step)
	return out
}

func appendClaudeToolResultToInteractions(out []byte, part gjson.Result, toolNamesByID map[string]string) []byte {
	step := []byte(`{"type":"function_result","call_id":"","result":""}`)
	if id := part.Get("tool_use_id").String(); id != "" {
		step, _ = sjson.SetBytes(step, "call_id", id)
	}
	name := part.Get("name").String()
	if name == "" {
		name = toolNamesByID[part.Get("tool_use_id").String()]
	}
	if name != "" {
		step, _ = sjson.SetBytes(step, "name", name)
	}
	if part.Get("is_error").Bool() {
		step, _ = sjson.SetBytes(step, "is_error", true)
	}
	result := part.Get("content")
	if result.Exists() {
		switch {
		case result.Type == gjson.String:
			step, _ = sjson.SetBytes(step, "result", result.String())
		case result.IsArray():
			converted := []byte(`[]`)
			result.ForEach(
				func(_, item gjson.Result) bool {
					contentPart := []byte(item.Raw)
					switch partType := item.Get("type").String(); partType {
					case "text":
						pureText := true
						item.ForEach(func(key, _ gjson.Result) bool {
							switch key.String() {
							case "type", "text", "cache_control":
							default:
								pureText = false
							}
							return pureText
						})
						if pureText {
							contentPart, _ = sjson.SetBytes([]byte(`{"type":"text","text":""}`), "text", item.Get("text").String())
						}
					case "image", "document", "container_upload":
						if media, ok := claudeMediaPartToInteractions(item, partType); ok {
							contentPart = media
						}
					}
					converted, _ = sjson.SetRawBytes(converted, "-1", contentPart)
					return true
				},
			)
			step, _ = sjson.SetRawBytes(step, "result", converted)
		default:
			step, _ = sjson.SetRawBytes(step, "result", []byte(result.Raw))
		}
	}
	out, _ = sjson.SetRawBytes(out, "input.-1", step)
	return out
}

func copyClaudeToolsToInteractions(out []byte, root gjson.Result) []byte {
	tools := root.Get("tools")
	if !tools.Exists() || !tools.IsArray() {
		return out
	}
	converted := []byte(`[]`)
	tools.ForEach(
		func(_, tool gjson.Result) bool {
			name := strings.TrimSpace(tool.Get("name").String())
			if name == "" {
				return true
			}
			item := []byte(`{"type":"function","name":"","parameters":{}}`)
			item, _ = sjson.SetBytes(item, "name", name)
			if desc := tool.Get("description"); desc.Exists() {
				item, _ = sjson.SetBytes(item, "description", desc.String())
			}
			if schema := tool.Get("input_schema"); schema.Exists() && schema.IsObject() {
				item, _ = sjson.SetRawBytes(item, "parameters", []byte(schema.Raw))
			}
			converted, _ = sjson.SetRawBytes(converted, "-1", item)
			return true
		},
	)
	if len(gjson.ParseBytes(converted).Array()) > 0 {
		out, _ = sjson.SetRawBytes(out, "tools", converted)
	}
	return out
}

func claudeText(value gjson.Result) string {
	if !value.Exists() {
		return ""
	}
	if value.Type == gjson.String {
		return value.String()
	}
	if text := value.Get("text"); text.Exists() {
		return text.String()
	}
	if value.IsArray() {
		var builder strings.Builder
		value.ForEach(
			func(_, item gjson.Result) bool {
				text := claudeText(item)
				if text == "" {
					return true
				}
				if builder.Len() > 0 {
					builder.WriteByte('\n')
				}
				builder.WriteString(text)
				return true
			},
		)
		return builder.String()
	}
	return ""
}
