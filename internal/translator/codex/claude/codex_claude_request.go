// Package claude provides request translation functionality for Claude Code API compatibility.
// It handles parsing and transforming Claude Code API requests into the internal client format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package also performs JSON data cleaning and transformation to ensure compatibility
// between Claude Code API format and the internal client's expected format.
package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	sigcompat "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertClaudeRequestToCodex parses and transforms a Claude Code API request into the internal client format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the internal client.
// The function performs the following transformations:
// 1. Sets the model name and empty instructions field
// 2. Processes system messages and converts them to developer input content
// 3. Transforms message contents (text, image, document, tool_use, tool_result) to appropriate formats
// 4. Convert tools declarations to the expected format
// 5. Adds additional configuration parameters for the Codex API
// 6. Maps Claude thinking configuration to Codex reasoning settings
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the Claude Code API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in internal client format
func ConvertClaudeRequestToCodex(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertClaudeRequestToCodex(modelName, inputRawJSON, stream, false)
}

// ConvertClaudeRequestToCodexWithCompat preserves assistant thinking blocks with
// empty or unknown-format signatures for configured compatibility endpoints.
func ConvertClaudeRequestToCodexWithCompat(modelName string, inputRawJSON []byte, stream bool) []byte {
	return convertClaudeRequestToCodex(modelName, inputRawJSON, stream, true)
}

func convertClaudeRequestToCodex(
	modelName string,
	inputRawJSON []byte,
	stream bool,
	preserveCompatThinkingBlocks bool,
) []byte {
	body, err := convertClaudeRequestToCodexChecked(modelName, inputRawJSON, stream, preserveCompatThinkingBlocks)
	if err != nil {
		log.WithError(err).Warn("Claude to Codex request translation rejected")
		return nil
	}
	return body
}

// ConvertClaudeRequestToCodexWithCompatReturningError reports unsupported user attachments.
func ConvertClaudeRequestToCodexWithCompatReturningError(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return convertClaudeRequestToCodexChecked(modelName, inputRawJSON, stream, true)
}

func convertClaudeRequestToCodexChecked(
	modelName string,
	inputRawJSON []byte,
	_ bool,
	preserveCompatThinkingBlocks bool,
) ([]byte, error) {
	rootResult := gjson.ParseBytes(inputRawJSON)
	var drops translatorcommon.UserTurnDrops
	inputItems := translatorcommon.NewRawArrayItems(rootResult.Get("messages.#").Int())

	toolsResult := rootResult.Get("tools")
	hasTools := toolsResult.IsArray()
	toolResults := toolsResult.Array()
	toolNameMap := map[string]string{}
	if hasTools {
		names := make([]string, 0, len(toolResults))
		for i := range toolResults {
			if name := toolResults[i].Get("name").String(); name != "" {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			toolNameMap = buildShortNameMap(names)
		}
	}

	// Process system messages and convert them to input content format.
	systemsResult := rootResult.Get("system")
	if systemsResult.Exists() {
		message := []byte(`{"type":"message","role":"developer","content":[]}`)
		contentIndex := 0

		appendSystemText := func(text string) {
			if text == "" || util.IsClaudeCodeAttributionSystemText(text) {
				return
			}

			message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.type", contentIndex), "input_text")
			message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.text", contentIndex), text)
			contentIndex++
		}

		if systemsResult.Type == gjson.String {
			appendSystemText(systemsResult.String())
		} else if systemsResult.IsArray() {
			systemResults := systemsResult.Array()
			for i := range systemResults {
				systemResult := systemResults[i]
				if systemResult.Get("type").String() == "text" {
					appendSystemText(systemResult.Get("text").String())
				}
			}
		}

		if contentIndex > 0 {
			inputItems = append(inputItems, message)
		}
	}

	// Process messages and transform their contents to appropriate formats.
	messagesResult := rootResult.Get("messages")
	if messagesResult.IsArray() {
		messageResults := messagesResult.Array()
		var pendingToolUseIDs []string
		var pendingSystemReminders [][]byte
		flushSystemReminders := func() {
			inputItems = append(inputItems, pendingSystemReminders...)
			pendingSystemReminders = nil
		}

		for i := range messageResults {
			messageResult := messageResults[i]
			messageRole := messageResult.Get("role").String()
			if messageRole == "system" {
				if reminderText, ok := translatorcommon.ClaudeMessageSystemReminderText(messageResult.Get("content")); ok {
					message := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`)
					message, _ = sjson.SetBytes(message, "content.0.text", reminderText)
					if len(pendingToolUseIDs) > 0 {
						pendingSystemReminders = append(pendingSystemReminders, message)
					} else {
						inputItems = append(inputItems, message)
					}
				}
				continue
			}

			messageContentsResult := messageResult.Get("content")
			if messageRole == "user" && len(pendingToolUseIDs) > 0 {
				messageContentsResult = translatorcommon.AlignClaudeToolResults(messageContentsResult, pendingToolUseIDs)
			}
			pendingToolUseIDs = nil

			newMessage := func() []byte {
				msg := []byte(`{"type":"message","role":"","content":[]}`)
				msg, _ = sjson.SetBytes(msg, "role", messageRole)
				return msg
			}

			turnSendable := 0
			message := newMessage()
			contentIndex := 0
			hasContent := false

			flushMessage := func() {
				if hasContent {
					inputItems = append(inputItems, message)
					message = newMessage()
					contentIndex = 0
					hasContent = false
				}
			}

			appendTextContent := func(text string) {
				flushSystemReminders()
				if strings.TrimSpace(text) != "" {
					turnSendable++
				}
				partType := "input_text"
				if messageRole == "assistant" {
					partType = "output_text"
				}
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.type", contentIndex), partType)
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.text", contentIndex), text)
				contentIndex++
				hasContent = true
			}

			appendImageContent := func(part []byte) {
				flushSystemReminders()
				turnSendable++
				message, _ = sjson.SetRawBytes(message, fmt.Sprintf("content.%d", contentIndex), part)
				contentIndex++
				hasContent = true
			}

			appendDocumentContent := func(dataURL string) {
				flushSystemReminders()
				turnSendable++
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.type", contentIndex), "input_file")
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.file_data", contentIndex), dataURL)
				message, _ = sjson.SetBytes(message, fmt.Sprintf("content.%d.filename", contentIndex), "document.pdf")
				contentIndex++
				hasContent = true
			}

			appendReasoningContent := func(part gjson.Result) {
				if messageRole != "assistant" {
					return
				}

				signatureValue := part.Get("signature")
				rawSignature := signatureValue.String()
				options := sigcompat.SignatureCompatibilityOptions{}
				if preserveCompatThinkingBlocks {
					options.PreserveUnknownNonEmpty = true
				}
				replaySignature, ok := "", false
				if signatureValue.Type == gjson.String {
					replaySignature, ok = sigcompat.CompatibleSignatureForProviderWithOptions(
						sigcompat.ProviderGPT,
						rawSignature,
						sigcompat.BlockKindClaudeThinking,
						options,
					)
				}
				if !ok && signatureValue.Type == gjson.String &&
					strings.Contains(strings.ToLower(thinking.ParseSuffix(modelName).ModelName), "grok") {
					if _, err := sigcompat.InspectGrokEncryptedContent(rawSignature); err == nil {
						replaySignature, ok = rawSignature, true
					}
				}
				if !ok && preserveCompatThinkingBlocks && strings.TrimSpace(rawSignature) == "" {
					replaySignature = rawSignature
					ok = true
				}
				if !ok {
					return
				}

				flushMessage()
				reasoningItem := []byte(`{"type":"reasoning","summary":[],"content":null}`)
				reasoningItem, _ = sjson.SetBytes(reasoningItem, "encrypted_content", replaySignature)
				inputItems = append(inputItems, reasoningItem)
			}

			if messageContentsResult.IsArray() {
				messageContentResults := messageContentsResult.Array()
				for j := range messageContentResults {
					messageContentResult := messageContentResults[j]
					contentType := messageContentResult.Get("type").String()

					switch contentType {
					case "text":
						appendTextContent(messageContentResult.Get("text").String())
					case "thinking":
						appendReasoningContent(messageContentResult)
					case "image":
						if part, ok := claudeImageInputPart(messageContentResult.Get("source")); ok {
							appendImageContent(part)
						} else if messageRole == "user" {
							drops.Drop(contentType)
						}
					case "document", "container_upload":
						sourceResult := messageContentResult.Get("source")
						mediaType := strings.TrimSpace(sourceResult.Get("media_type").String())
						data := sourceResult.Get("data").String()
						if data == "" {
							data = sourceResult.Get("base64").String()
						}
						if sourceResult.Get("type").String() == "base64" &&
							strings.EqualFold(mediaType, "application/pdf") && data != "" {
							appendDocumentContent(fmt.Sprintf("data:%s;base64,%s", mediaType, data))
						} else if messageRole == "user" {
							drops.Drop(contentType)
						}
					case "tool_use":
						flushMessage()
						pendingToolUseIDs = append(pendingToolUseIDs, messageContentResult.Get("id").String())
						functionCallMessage := []byte(`{"type":"function_call"}`)
						functionCallMessage, _ = sjson.SetBytes(
							functionCallMessage, "call_id",
							shortenCodexCallIDIfNeeded(messageContentResult.Get("id").String()),
						)
						{
							name := messageContentResult.Get("name").String()
							if short, ok := toolNameMap[name]; ok {
								name = short
							} else {
								name = shortenNameIfNeeded(name)
							}
							functionCallMessage, _ = sjson.SetBytes(functionCallMessage, "name", name)
						}
						functionCallMessage, _ = sjson.SetBytes(
							functionCallMessage, "arguments", messageContentResult.Get("input").Raw,
						)
						inputItems = append(inputItems, functionCallMessage)
					case "tool_result":
						flushMessage()
						functionCallOutputMessage := []byte(`{"type":"function_call_output"}`)
						functionCallOutputMessage, _ = sjson.SetBytes(
							functionCallOutputMessage, "call_id",
							shortenCodexCallIDIfNeeded(messageContentResult.Get("tool_use_id").String()),
						)

						contentResult := messageContentResult.Get("content")
						if contentResult.IsArray() {
							toolResultContentIndex := 0
							toolResultContent := []byte(`[]`)
							contentResults := contentResult.Array()
							for k := range contentResults {
								toolResultContentType := contentResults[k].Get("type").String()
								if toolResultContentType == "image" {
									if part, ok := claudeImageInputPart(contentResults[k].Get("source")); ok {
										toolResultContent, _ = sjson.SetRawBytes(toolResultContent, strconv.Itoa(toolResultContentIndex), part)
										toolResultContentIndex++
									}
								} else if toolResultContentType == "text" {
									toolResultContent, _ = sjson.SetBytes(
										toolResultContent, fmt.Sprintf("%d.type", toolResultContentIndex), "input_text",
									)
									toolResultContent, _ = sjson.SetBytes(
										toolResultContent, fmt.Sprintf("%d.text", toolResultContentIndex),
										contentResults[k].Get("text").String(),
									)
									toolResultContentIndex++
								}
							}
							if toolResultContentIndex > 0 {
								functionCallOutputMessage, _ = sjson.SetRawBytes(
									functionCallOutputMessage, "output", toolResultContent,
								)
							} else {
								functionCallOutputMessage, _ = sjson.SetBytes(
									functionCallOutputMessage, "output", messageContentResult.Get("content").String(),
								)
							}
						} else {
							functionCallOutputMessage, _ = sjson.SetBytes(
								functionCallOutputMessage, "output", messageContentResult.Get("content").String(),
							)
						}

						inputItems = append(inputItems, functionCallOutputMessage)
						turnSendable++
					}
				}
				flushMessage()
			} else if messageContentsResult.Type == gjson.String {
				appendTextContent(messageContentsResult.String())
				flushMessage()
			}
			flushSystemReminders()
			if messageRole == "user" {
				drops.EndTurn(turnSendable)
			}
		}
		flushSystemReminders()
	}

	// Convert tools declarations to the expected format for the Codex API.
	outputTools := make([][]byte, 0, len(toolResults))
	hasWebSearchTool := false
	webSearchToolNames := make(map[string]struct{})
	if hasTools {
		for i := range toolResults {
			toolResult := toolResults[i]
			// Special handling: map Claude web search tool to Codex web_search
			toolType := toolResult.Get("type").String()
			if toolType == "web_search_20250305" || toolType == "web_search_20260209" {
				hasWebSearchTool = true
				if name := toolResult.Get("name").String(); name != "" {
					webSearchToolNames[name] = struct{}{}
				}
				tool := []byte(`{"type":"web_search"}`)
				if domains := toolResult.Get("allowed_domains"); domains.IsArray() {
					tool, _ = sjson.SetRawBytes(tool, "filters.allowed_domains", []byte(domains.Raw))
				}
				if location := toolResult.Get("user_location"); location.IsObject() {
					tool, _ = sjson.SetRawBytes(tool, "user_location", []byte(location.Raw))
				}
				outputTools = append(outputTools, tool)
				continue
			}
			tool := []byte(toolResult.Raw)
			if toolResult.Get("type").Type != gjson.String || toolResult.Get("type").String() != "function" {
				tool, _ = sjson.SetBytes(tool, "type", "function")
			}
			// Apply shortened name if needed
			if v := toolResult.Get("name"); v.Exists() {
				originalName := v.String()
				name := originalName
				if short, ok := toolNameMap[name]; ok {
					name = short
				} else {
					name = shortenNameIfNeeded(name)
				}
				if v.Type != gjson.String || name != originalName {
					tool, _ = sjson.SetBytes(tool, "name", name)
				}
			}
			tool, _ = sjson.SetRawBytes(
				tool, "parameters", []byte(normalizeToolParameters(toolResult.Get("input_schema").Raw)),
			)
			for _, path := range []string{"input_schema", "parameters.$schema", "cache_control", "defer_loading"} {
				if gjson.GetBytes(tool, path).Exists() {
					tool, _ = sjson.DeleteBytes(tool, path)
				}
			}
			if gjson.GetBytes(tool, "strict").Type != gjson.False {
				tool, _ = sjson.SetBytes(tool, "strict", false)
			}
			outputTools = append(outputTools, tool)
		}
	}

	// Default to parallel tool calls unless tool_choice explicitly disables them.
	parallelToolCalls := true
	if disableParallelToolUse := rootResult.Get("tool_choice.disable_parallel_tool_use"); disableParallelToolUse.Exists() {
		parallelToolCalls = !disableParallelToolUse.Bool()
	}

	// Convert thinking.budget_tokens to reasoning.effort.
	reasoningEffort := "medium"
	if thinkingConfig := rootResult.Get("thinking"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
		switch thinkingConfig.Get("type").String() {
		case "enabled":
			if budgetTokens := thinkingConfig.Get("budget_tokens"); budgetTokens.Exists() {
				budget := int(budgetTokens.Int())
				if effort, ok := thinking.ConvertBudgetToLevel(budget); ok && effort != "" {
					reasoningEffort = effort
				}
			}
		case "adaptive", "auto":
			// Adaptive thinking can carry an explicit effort in output_config.effort (Claude 4.6).
			// Pass through directly; ApplyThinking handles clamping to target model's levels.
			effort := ""
			if v := rootResult.Get("output_config.effort"); v.Exists() && v.Type == gjson.String {
				effort = strings.ToLower(strings.TrimSpace(v.String()))
			}
			if effort != "" {
				reasoningEffort = effort
			} else {
				reasoningEffort = string(thinking.LevelXHigh)
			}
		case "disabled":
			if effort, ok := thinking.ConvertBudgetToLevel(0); ok && effort != "" {
				reasoningEffort = effort
			}
		}
	}

	serviceTier := normalizeCodexServiceTier(rootResult.Get("service_tier"))
	if speed := rootResult.Get("speed"); speed.Type == gjson.String && speed.String() == "fast" {
		serviceTier = "priority"
	}
	textFormat := mapClaudeOutputFormatToCodex(rootResult.Get("output_config.format"))

	capacity := len(modelName) + len(reasoningEffort) + len(serviceTier) + len(textFormat) + 240
	for i := 0; i < len(inputItems); i++ {
		capacity += len(inputItems[i]) + 1
	}
	for i := 0; i < len(outputTools); i++ {
		capacity += len(outputTools[i]) + 1
	}
	output := make([]byte, 0, capacity)
	output = append(output, `{"model":`...)
	output = appendJSONString(output, modelName)
	output = append(output, `,"instructions":"","input":[`...)
	for i := 0; i < len(inputItems); i++ {
		if i > 0 {
			output = append(output, ',')
		}
		output = append(output, inputItems[i]...)
	}
	output = append(output, ']')
	if hasTools {
		output = append(output, `,"tools":[`...)
		for i := 0; i < len(outputTools); i++ {
			if i > 0 {
				output = append(output, ',')
			}
			output = append(output, outputTools[i]...)
		}
		output = append(output, `],"tool_choice":`...)
		choice := rootResult.Get("tool_choice")
		choiceType := choice.Get("type").String()
		if choice.Type == gjson.String {
			choiceType = choice.String()
		}
		switch choiceType {
		case "any":
			output = append(output, `"required"`...)
		case "none":
			output = append(output, `"none"`...)
		case "tool":
			name := choice.Get("name").String()
			if _, ok := webSearchToolNames[name]; ok {
				output = append(output, `{"type":"web_search"}`...)
			} else if name != "" {
				if short, ok := toolNameMap[name]; ok {
					name = short
				} else {
					name = shortenNameIfNeeded(name)
				}
				output = append(output, `{"type":"function","name":`...)
				output = appendJSONString(output, name)
				output = append(output, '}')
			} else {
				output = append(output, `"auto"`...)
			}
		default:
			output = append(output, `"auto"`...)
		}
	}
	output = append(output, `,"parallel_tool_calls":`...)
	output = strconv.AppendBool(output, parallelToolCalls)
	output = append(output, `,"reasoning":{"effort":`...)
	output = appendJSONString(output, reasoningEffort)
	output = append(output, '}')
	if len(textFormat) > 0 {
		output = append(output, `,"text":{"format":`...)
		output = append(output, textFormat...)
		output = append(output, '}')
	}
	if serviceTier != "" {
		output = append(output, `,"service_tier":`...)
		output = appendJSONString(output, serviceTier)
	}
	output = append(output, `,"stream":true,"store":false,"include":["reasoning.encrypted_content"`...)
	if hasWebSearchTool {
		output = append(output, `,"web_search_call.action.sources"`...)
	}
	output = append(output, ']', '}')
	return output, drops.Err()
}

func mapClaudeOutputFormatToCodex(format gjson.Result) []byte {
	if !format.IsObject() || format.Get("type").String() != "json_schema" {
		return nil
	}
	schema := format.Get("schema")
	if !schema.IsObject() {
		return nil
	}

	normalizedSchema := util.NormalizeClaudeSchemaDialect([]byte(schema.Raw))
	name := strings.TrimSpace(format.Get("name").String())
	if name == "" {
		name = "cli_proxy_structured_output"
	}
	strict := true
	if value := format.Get("strict"); value.Exists() && value.Type == gjson.False {
		strict = false
	}
	if strict && util.ClaudeSchemaMissesRequired(normalizedSchema) {
		strict = false
	}

	translated := []byte(`{"type":"json_schema","name":"","strict":true,"schema":{}}`)
	translated, _ = sjson.SetBytes(translated, "name", name)
	translated, _ = sjson.SetBytes(translated, "strict", strict)
	translated, _ = sjson.SetRawBytes(translated, "schema", normalizedSchema)
	return translated
}

func normalizeCodexServiceTier(result gjson.Result) string {
	if !result.Exists() || result.Type != gjson.String {
		return ""
	}

	switch strings.ToLower(strings.TrimSpace(result.String())) {
	case "fast", "priority":
		return "priority"
	default:
		return ""
	}
}

func appendJSONString(dst []byte, value string) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return append(dst, encoded...)
}

func shortenCodexCallIDIfNeeded(id string) string {
	const limit = 64
	if len(id) <= limit {
		return id
	}

	sum := sha256.Sum256([]byte(id))
	suffix := "_" + hex.EncodeToString(sum[:8])
	prefixLen := limit - len(suffix)
	if prefixLen <= 0 {
		return suffix[len(suffix)-limit:]
	}
	return id[:prefixLen] + suffix
}

// shortenNameIfNeeded applies a simple shortening rule for a single name.
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			candidate := "mcp__" + name[idx+2:]
			if len(candidate) > limit {
				return candidate[:limit]
			}
			return candidate
		}
	}
	return name[:limit]
}

// buildShortNameMap ensures uniqueness of shortened names within a request.
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				candidate := "mcp__" + n[idx+2:]
				if len(candidate) > limit {
					candidate = candidate[:limit]
				}
				return candidate
			}
		}
		return n[:limit]
	}

	makeUnique := func(candidate string) string {
		if _, ok := used[candidate]; !ok {
			return candidate
		}
		base := candidate
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := max(limit-len(suffix), 0)
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		candidate := baseCandidate(n)
		uniq := makeUnique(candidate)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}

// normalizeToolParameters 保留 Schema 类型和字面值，移除不支持的方言元数据与正则转义
func normalizeToolParameters(raw string) string {
	raw = strings.TrimSpace(raw)
	if !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		return `{"type":"object","properties":{}}`
	}
	normalized := util.NormalizeClaudeSchemaDialect([]byte(raw))
	schema := gjson.ParseBytes(normalized)
	schemaType := schema.Get("type")
	isObject := schemaType.String() == "object"
	if !schemaType.Exists() || schemaType.Type == gjson.Null || schemaType.String() == "" {
		normalized, _ = sjson.SetBytes(normalized, "type", "object")
		isObject = true
	} else if schemaType.IsArray() {
		for _, item := range schemaType.Array() {
			if item.String() == "object" {
				isObject = true
				break
			}
		}
	}
	if properties := schema.Get("properties"); isObject && (!properties.Exists() || properties.Type == gjson.Null) {
		normalized, _ = sjson.SetRawBytes(normalized, "properties", []byte(`{}`))
	}
	return string(normalized)
}

func claudeImageInputPart(source gjson.Result) ([]byte, bool) {
	if !source.IsObject() {
		return nil, false
	}
	part := []byte(`{"type":"input_image"}`)
	data := source.Get("data")
	if data.Type != gjson.String || data.String() == "" {
		data = source.Get("base64")
	}
	switch {
	case data.Type == gjson.String && strings.TrimSpace(data.String()) != "":
		mediaType := source.Get("media_type").String()
		if mediaType == "" {
			mediaType = source.Get("mime_type").String()
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		part, _ = sjson.SetBytes(part, "image_url", fmt.Sprintf("data:%s;base64,%s", mediaType, data.String()))
	case source.Get("type").String() == "url":
		imageURL := source.Get("url")
		if imageURL.Type != gjson.String {
			return nil, false
		}
		value := strings.TrimSpace(imageURL.String())
		parsed, err := url.Parse(value)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, false
		}
		part, _ = sjson.SetBytes(part, "image_url", value)
	case source.Get("type").String() == "file":
		fileID := source.Get("file_id")
		if fileID.Type != gjson.String || strings.TrimSpace(fileID.String()) == "" {
			return nil, false
		}
		part, _ = sjson.SetBytes(part, "file_id", fileID.String())
	default:
		return nil, false
	}
	return part, true
}
