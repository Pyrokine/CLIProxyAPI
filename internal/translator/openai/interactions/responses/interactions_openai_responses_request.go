package responses

import (
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func convertOpenAIResponsesRequestToInteractions(modelName string, inputRawJSON []byte, stream bool) []byte {
	out, _ := convertOpenAIResponsesRequestToInteractionsChecked(modelName, inputRawJSON, stream)
	return out
}

func convertOpenAIResponsesRequestToInteractionsChecked(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	root := gjson.ParseBytes(inputRawJSON)
	out := []byte(`{"model":"","input":[]}`)
	model := requestModel(modelName, root)
	out, _ = sjson.SetBytes(out, "model", model)
	if streamValue, ok := requestStreamValue(root, stream); ok {
		out, _ = sjson.SetBytes(out, "stream", streamValue)
	}
	if instructions := root.Get("instructions"); instructions.Exists() {
		out, _ = sjson.SetBytes(out, "system_instruction", responsesInstructionsText(instructions))
	}
	if previousResponseID := firstNonEmpty(
		root.Get("previous_response_id").String(), root.Get("previous_interaction_id").String(),
	); previousResponseID != "" {
		out, _ = sjson.SetBytes(out, "previous_interaction_id", previousResponseID)
	}
	if environmentID := firstNonEmpty(root.Get("environment_id").String(), root.Get("environment.id").String()); environmentID != "" {
		out, _ = sjson.SetBytes(out, "environment_id", environmentID)
	}
	if agentConfig := root.Get("agent_config"); agentConfig.Exists() {
		out, _ = sjson.SetRawBytes(out, "agent_config", []byte(agentConfig.Raw))
	}
	forAntigravity := isAntigravityModel(model)
	forDevin := !forAntigravity || strings.Contains(strings.ToLower(model), "devin")
	identity := translatorcommon.NewResponsesToolIdentity(inputRawJSON)
	if input := root.Get("input"); input.Exists() {
		var err error
		out, err = setResponsesInputOnInteractions(out, input, forAntigravity)
		if err != nil {
			return nil, err
		}
	}
	out = appendResponsesToolsToInteractions(out, identity, forAntigravity, forDevin)
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		toolChoiceRaw := []byte(toolChoice.Raw)
		if toolChoice.IsObject() {
			namePath := "name"
			name := toolChoice.Get(namePath).String()
			if toolChoice.Get("function.name").Exists() {
				namePath = "function.name"
				name = toolChoice.Get(namePath).String()
			} else if toolChoice.Get("custom.name").Exists() {
				namePath = "custom.name"
				name = toolChoice.Get(namePath).String()
			}
			namespace := firstNonEmpty(
				toolChoice.Get("namespace").String(), toolChoice.Get("function.namespace").String(),
				toolChoice.Get("custom.namespace").String(),
			)
			qualifiedName := name
			if namespace != "" {
				qualifiedName = identity.ResolveName(name, namespace)
			}
			if forDevin && (translatorcommon.IsDevinCodexAppAutomationUpdate(namespace, name) ||
				translatorcommon.IsDevinCodexAppAutomationUpdate("", qualifiedName)) {
				toolChoiceRaw = nil
			} else if name != "" {
				if forAntigravity {
					qualifiedName = translatorcommon.AntigravityToolNameToUpstream(qualifiedName)
				}
				toolChoiceRaw, _ = sjson.SetBytes(toolChoiceRaw, namePath, qualifiedName)
			}
		}
		if toolChoiceRaw != nil {
			out, _ = sjson.SetRawBytes(out, "generation_config.tool_choice", toolChoiceRaw)
		}
	}
	if effort := root.Get("reasoning.effort"); effort.Exists() && effort.Type == gjson.String {
		out, _ = sjson.SetBytes(
			out, "generation_config.thinking_level", strings.ToLower(strings.TrimSpace(effort.String())),
		)
	}
	if summary := normalizedResponsesSummary(root.Get("reasoning.summary")); summary != "" {
		if summary != "none" {
			summary = "auto"
		}
		out, _ = sjson.SetBytes(out, "generation_config.thinking_summaries", summary)
	}
	if format := root.Get("response_format"); format.Exists() {
		out, _ = sjson.SetRawBytes(out, "response_format", []byte(format.Raw))
	} else if format := root.Get("text.format"); format.Exists() {
		out, _ = sjson.SetRawBytes(out, "response_format", []byte(format.Raw))
	}
	if forAntigravity {
		maxOutputTokens := firstExisting(
			root.Get("max_output_tokens"), root.Get("max_tokens"), root.Get("max_completion_tokens"),
		)
		if maxOutputTokens.Exists() && !root.Get("agent_config.max_total_tokens").Exists() {
			out, _ = sjson.SetRawBytes(out, "agent_config.max_total_tokens", []byte(maxOutputTokens.Raw))
		}
	} else {
		if maxOutputTokens := firstExisting(root.Get("max_output_tokens"), root.Get("max_tokens"), root.Get("max_completion_tokens")); maxOutputTokens.Exists() {
			out, _ = sjson.SetRawBytes(out, "generation_config.max_output_tokens", []byte(maxOutputTokens.Raw))
		}
		for _, field := range []string{"temperature", "top_p", "presence_penalty", "frequency_penalty"} {
			if value := root.Get(field); value.Exists() {
				out, _ = sjson.SetRawBytes(out, "generation_config."+field, []byte(value.Raw))
			}
		}
		if stop := firstExisting(root.Get("stop"), root.Get("stop_sequences")); stop.Exists() {
			out, _ = sjson.SetRawBytes(out, "generation_config.stop_sequences", []byte(stop.Raw))
		}
	}
	return out, nil
}

func convertInteractionsRequestToOpenAIResponses(modelName string, inputRawJSON []byte, stream bool) []byte {
	root := gjson.ParseBytes(inputRawJSON)
	out := []byte(`{"model":"","input":[]}`)
	model := requestModel(modelName, root)
	out, _ = sjson.SetBytes(out, "model", model)
	if stream || root.Get("stream").Bool() {
		out, _ = sjson.SetBytes(out, "stream", true)
	}
	if instructions := interactionsSystemInstructionText(root); instructions != "" {
		out, _ = sjson.SetBytes(out, "instructions", instructions)
	}
	if previousInteractionID := firstNonEmpty(
		root.Get("previous_interaction_id").String(), root.Get("previous_response_id").String(),
	); previousInteractionID != "" {
		out, _ = sjson.SetBytes(out, "previous_response_id", previousInteractionID)
	}
	if environmentID := firstNonEmpty(root.Get("environment_id").String(), root.Get("environment.id").String()); environmentID != "" {
		out, _ = sjson.SetBytes(out, "environment_id", environmentID)
	}
	if agentConfig := root.Get("agent_config"); agentConfig.Exists() {
		out, _ = sjson.SetRawBytes(out, "agent_config", []byte(agentConfig.Raw))
	}
	forAntigravity := isAntigravityModel(model)
	if input := root.Get("input"); input.Exists() {
		out = setInteractionsInputOnResponses(out, input, forAntigravity)
	}
	out = appendInteractionsToolsToResponses(out, root.Get("tools"), forAntigravity)
	toolChoice := root.Get("generation_config.tool_choice")
	if !toolChoice.Exists() {
		toolChoice = root.Get("generationConfig.toolChoice")
	}
	if !toolChoice.Exists() {
		toolChoice = root.Get("tool_choice")
	}
	if toolChoice.Exists() {
		toolChoiceRaw := []byte(toolChoice.Raw)
		if forAntigravity && toolChoice.IsObject() {
			toolChoiceRaw = mapAntigravityToolChoiceName(toolChoiceRaw, toolChoice, translatorcommon.AntigravityUpstreamToolNameToClient)
		}
		out, _ = sjson.SetRawBytes(out, "tool_choice", toolChoiceRaw)
	}
	if effort := interactionsThinkingEffort(root); effort != "" {
		out, _ = sjson.SetBytes(out, "reasoning.effort", effort)
	}
	if summary := normalizedInteractionsSummary(root); summary == "auto" {
		out, _ = sjson.SetBytes(out, "reasoning.summary", summary)
	}
	if responseModalities := root.Get("response_modalities"); responseModalities.Exists() {
		out, _ = sjson.SetRawBytes(out, "modalities", []byte(responseModalities.Raw))
	}
	if serviceTier := root.Get("service_tier"); serviceTier.Exists() && serviceTier.Type == gjson.String {
		out, _ = sjson.SetBytes(out, "service_tier", serviceTier.String())
	}
	if format := root.Get("response_format"); format.Exists() {
		out, _ = sjson.SetRawBytes(out, "text.format", []byte(format.Raw))
	}
	return out
}

func normalizedResponsesSummary(summary gjson.Result) string {
	if summary.Type != gjson.String {
		return ""
	}
	switch value := strings.ToLower(strings.TrimSpace(summary.String())); value {
	case "auto", "concise", "detailed", "none":
		return value
	default:
		return ""
	}
}

func normalizedInteractionsSummary(root gjson.Result) string {
	for _, path := range []string{
		"generation_config.thinking_summaries",
		"generation_config.thinkingSummaries",
		"generationConfig.thinking_summaries",
		"generationConfig.thinkingSummaries",
		"reasoning.summary",
	} {
		summary := root.Get(path)
		if summary.Type != gjson.String {
			continue
		}
		switch value := strings.ToLower(strings.TrimSpace(summary.String())); value {
		case "auto", "none":
			return value
		}
	}
	return ""
}

func requestModel(modelName string, root gjson.Result) string {
	if strings.TrimSpace(modelName) != "" {
		return modelName
	}
	return root.Get("model").String()
}

func requestStreamValue(root gjson.Result, stream bool) (bool, bool) {
	if value := root.Get("stream"); value.Exists() {
		return value.Bool(), true
	}
	if stream {
		return true, true
	}
	return false, false
}

func responsesInstructionsText(instructions gjson.Result) string {
	if instructions.Type == gjson.String {
		return instructions.String()
	}
	if text := instructions.Get("text"); text.Exists() {
		return text.String()
	}
	if parts := instructions.Get("content"); parts.Exists() && parts.IsArray() {
		var builder strings.Builder
		parts.ForEach(
			func(_, part gjson.Result) bool {
				if text := part.Get("text").String(); text != "" {
					builder.WriteString(text)
				}
				return true
			},
		)
		return builder.String()
	}
	return instructions.String()
}

func interactionsSystemInstructionText(root gjson.Result) string {
	sys := root.Get("system_instruction")
	if !sys.Exists() {
		return ""
	}
	if sys.Type == gjson.String {
		return sys.String()
	}
	if text := sys.Get("text"); text.Exists() {
		return text.String()
	}
	if parts := sys.Get("parts"); parts.Exists() && parts.IsArray() {
		var builder strings.Builder
		parts.ForEach(
			func(_, part gjson.Result) bool {
				if text := part.Get("text").String(); text != "" {
					builder.WriteString(text)
				}
				return true
			},
		)
		return builder.String()
	}
	return ""
}

func interactionsThinkingEffort(root gjson.Result) string {
	for _, path := range []string{
		"generation_config.thinking_level",
		"generation_config.thinkingConfig.thinkingLevel",
		"generation_config.thinkingConfig.thinking_level",
		"generation_config.thinking_config.thinking_level",
	} {
		if level := root.Get(path); level.Exists() && level.Type == gjson.String {
			return strings.ToLower(strings.TrimSpace(level.String()))
		}
	}
	return ""
}

func setResponsesInputOnInteractions(out []byte, input gjson.Result, forAntigravity bool) ([]byte, error) {
	functionNamesByCallID := make(map[string]string)
	items := translatorcommon.NewRawArrayItems(input.Get("#").Int())
	var drops translatorcommon.UserTurnDrops
	appendItem := func(item gjson.Result) {
		converted := responsesInputItemToInteractions(item, functionNamesByCallID, forAntigravity)
		if partType := unsupportedResponsesUserPart(item); partType != "" {
			drops.Drop(partType)
			drops.EndTurn(sendableInteractionsParts(converted))
		}
		if converted != nil {
			items = append(items, converted)
		}
	}
	if input.Type == gjson.String {
		items = append(items, interactionsTextStep("user_input", input.String()))
	} else if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			appendItem(item)
			return true
		})
	} else if input.IsObject() {
		appendItem(input)
	}
	if err := drops.Err(); err != nil {
		return nil, err
	}
	if len(items) > 0 {
		out = translatorcommon.SetRawArrayItems(out, "input", items)
	}
	return out, nil
}

func unsupportedResponsesUserPart(item gjson.Result) string {
	itemType := item.Get("type").String()
	if itemType == "message" {
		role := item.Get("role").String()
		if role != "user" && role != "" {
			return ""
		}
		content := item.Get("content")
		if content.IsArray() {
			for _, part := range content.Array() {
				if partType := unsupportedResponsesContentPart(part); partType != "" {
					return partType
				}
			}
			return ""
		}
		return unsupportedResponsesContentPart(content)
	}
	if itemType == "input_image" || itemType == "input_file" || itemType == "input_audio" {
		return unsupportedResponsesContentPart(item)
	}
	if strings.HasPrefix(itemType, "input_") && itemType != "input_text" {
		return itemType
	}
	return ""
}

func unsupportedResponsesContentPart(part gjson.Result) string {
	partType := part.Get("type").String()
	switch partType {
	case "input_text", "text", "output_text":
		return ""
	case "input_image", "output_image":
		if part.Get("image_url").String() != "" || part.Get("url").String() != "" || part.Get("data").String() != "" {
			return ""
		}
		return partType
	case "input_file", "file":
		if len(responsesFilePartToInteractions(part)) > 0 {
			return ""
		}
		return partType
	case "input_audio", "audio":
		if len(responsesAudioPartToInteractions(part)) > 0 {
			return ""
		}
		return partType
	default:
		if part.Get("text").Exists() || !part.IsObject() {
			return ""
		}
		if partType == "" {
			return "unknown"
		}
		return partType
	}
}

func sendableInteractionsParts(step []byte) int {
	if len(step) == 0 {
		return 0
	}
	count := 0
	for _, part := range gjson.GetBytes(step, "content").Array() {
		if part.Get("type").String() != "text" || strings.TrimSpace(part.Get("text").String()) != "" {
			count++
		}
	}
	return count
}

func responsesInputItemToInteractions(
	item gjson.Result,
	functionNamesByCallID map[string]string,
	forAntigravity bool,
) []byte {
	switch item.Get("type").String() {
	case "message":
		stepType := "user_input"
		if role := item.Get("role").String(); role == "assistant" || role == "model" {
			stepType = "model_output"
		}
		step := []byte(`{"type":"","content":[]}`)
		step, _ = sjson.SetBytes(step, "type", stepType)
		return appendResponsesContentToInteractions(step, item.Get("content"), stepType)
	case "function_call", "custom_tool_call":
		callID := firstNonEmpty(item.Get("call_id").String(), item.Get("id").String())
		if name := responsesQualifiedInputToolName(item); callID != "" && name != "" {
			functionNamesByCallID[callID] = name
		}
		return responsesFunctionCallToInteractions(item, forAntigravity)
	case "function_call_output", "custom_tool_call_output":
		return responsesFunctionOutputToInteractions(item, functionNamesByCallID, forAntigravity)
	case "input_text", "output_text", "text":
		stepType := "user_input"
		if item.Get("type").String() == "output_text" {
			stepType = "model_output"
		}
		return interactionsTextStep(stepType, item.Get("text").String())
	case "input_image", "output_image", "input_file", "input_audio":
		stepType := "user_input"
		if item.Get("type").String() == "output_image" {
			stepType = "model_output"
		}
		step := []byte(`{"type":"","content":[]}`)
		step, _ = sjson.SetBytes(step, "type", stepType)
		if part, ok := responsesContentPartToInteractions(item); ok {
			step, _ = sjson.SetRawBytes(step, "content.-1", part)
		}
		return step
	default:
		if content := item.Get("content"); content.Exists() {
			step := []byte(`{"type":"user_input","content":[]}`)
			return appendResponsesContentToInteractions(step, content, "user_input")
		}
	}
	return nil
}

func appendResponsesContentToInteractions(step []byte, content gjson.Result, stepType string) []byte {
	if content.Type == gjson.String {
		part := []byte(`{"type":"text","text":""}`)
		part, _ = sjson.SetBytes(part, "text", content.String())
		step, _ = sjson.SetRawBytes(step, "content.-1", part)
		return step
	}
	if content.IsArray() {
		content.ForEach(
			func(_, item gjson.Result) bool {
				if part, ok := responsesContentPartToInteractions(item); ok {
					step, _ = sjson.SetRawBytes(step, "content.-1", part)
				}
				return true
			},
		)
		return step
	}
	if content.IsObject() {
		if part, ok := responsesContentPartToInteractions(content); ok {
			step, _ = sjson.SetRawBytes(step, "content.-1", part)
		}
		return step
	}
	if stepType == "model_output" {
		return step
	}
	return step
}

func responsesContentPartToInteractions(part gjson.Result) ([]byte, bool) {
	switch part.Get("type").String() {
	case "input_text", "output_text", "text":
		out := []byte(`{"type":"text","text":""}`)
		out, _ = sjson.SetBytes(out, "text", part.Get("text").String())
		return out, true
	case "input_image", "output_image":
		if unsupportedResponsesContentPart(part) == "" {
			return responsesImagePartToInteractions(part), true
		}
		return nil, false
	case "input_file", "file":
		converted := responsesFilePartToInteractions(part)
		return converted, len(converted) > 0
	case "input_audio", "audio":
		converted := responsesAudioPartToInteractions(part)
		return converted, len(converted) > 0
	}
	if text := part.Get("text"); text.Exists() {
		out := []byte(`{"type":"text","text":""}`)
		out, _ = sjson.SetBytes(out, "text", text.String())
		return out, true
	}
	return nil, false
}

func responsesImagePartToInteractions(part gjson.Result) []byte {
	out := []byte(`{"type":"image"}`)
	imageURL := firstNonEmpty(part.Get("image_url").String(), part.Get("url").String())
	if mimeType, data, ok := parseDataURL(imageURL); ok {
		out, _ = sjson.SetBytes(out, "mime_type", mimeType)
		out, _ = sjson.SetBytes(out, "data", data)
		return out
	}
	if data := part.Get("data").String(); data != "" {
		out, _ = sjson.SetBytes(out, "data", data)
		if mimeType := part.Get("mime_type").String(); mimeType != "" {
			out, _ = sjson.SetBytes(out, "mime_type", mimeType)
		}
		return out
	}
	if imageURL != "" {
		out, _ = sjson.SetBytes(out, "image_url", imageURL)
	}
	return out
}

func responsesFilePartToInteractions(part gjson.Result) []byte {
	filename := firstNonEmpty(part.Get("filename").String(), part.Get("file.filename").String())
	mimeType := firstNonEmpty(part.Get("mime_type").String(), part.Get("file.mime_type").String())
	fileData := firstNonEmpty(part.Get("file_data").String(), part.Get("file.file_data").String(), part.Get("data").String())
	fileURL := firstNonEmpty(part.Get("file_url").String(), part.Get("file.file_url").String(), part.Get("url").String())
	mimeType, data, hasData := translatorcommon.NormalizeOpenAIFileData(filename, mimeType, fileData)
	if !hasData && fileURL == "" {
		return nil
	}
	out := []byte(`{"type":"document"}`)
	if filename != "" {
		out, _ = sjson.SetBytes(out, "filename", filename)
	}
	if hasData {
		out, _ = sjson.SetBytes(out, "mime_type", mimeType)
		out, _ = sjson.SetBytes(out, "data", data)
	}
	if fileURL != "" {
		out, _ = sjson.SetBytes(out, "file_url", fileURL)
	}
	return out
}

func responsesAudioPartToInteractions(part gjson.Result) []byte {
	data := firstNonEmpty(part.Get("input_audio.data").String(), part.Get("audio.data").String(), part.Get("data").String())
	if data == "" {
		return nil
	}
	format := strings.ToLower(firstNonEmpty(part.Get("input_audio.format").String(), part.Get("audio.format").String(), part.Get("format").String()))
	mimeType := ""
	switch format {
	case "wav":
		mimeType = "audio/wav"
	case "mp3":
		mimeType = "audio/mpeg"
	case "flac", "opus", "ogg", "webm", "aac":
		mimeType = "audio/" + format
	case "pcm16":
		mimeType = "audio/pcm"
	case "g711_ulaw", "g711_alaw":
		mimeType = "audio/basic"
	default:
		if strings.HasPrefix(format, "audio/") {
			mimeType = format
		}
	}
	if mimeType == "" {
		return nil
	}
	out := []byte(`{"type":"audio","data":"","mime_type":""}`)
	out, _ = sjson.SetBytes(out, "data", data)
	out, _ = sjson.SetBytes(out, "mime_type", mimeType)
	return out
}

func responsesQualifiedInputToolName(item gjson.Result) string {
	return translatorcommon.QualifyResponsesNamespaceToolName(item.Get("namespace").String(), item.Get("name").String())
}

func responsesFunctionCallToInteractions(item gjson.Result, forAntigravity bool) []byte {
	out := []byte(`{"type":"function_call","name":"","arguments":{}}`)
	name := responsesQualifiedInputToolName(item)
	if forAntigravity {
		name = translatorcommon.AntigravityToolNameToUpstream(name)
	}
	out, _ = sjson.SetBytes(out, "name", name)
	if itemID := firstNonEmpty(item.Get("id").String(), item.Get("call_id").String()); itemID != "" {
		out, _ = sjson.SetBytes(out, "id", itemID)
	}
	if callID := firstNonEmpty(item.Get("call_id").String(), item.Get("id").String()); callID != "" {
		out, _ = sjson.SetBytes(out, "call_id", callID)
	}
	if item.Get("type").String() == "custom_tool_call" {
		if input := item.Get("input"); input.Exists() {
			out, _ = sjson.SetBytes(out, "arguments.input", input.String())
			return out
		}
	}
	setJSONValue(&out, "arguments", item.Get("arguments"), []byte(`{}`))
	return out
}

func responsesFunctionOutputToInteractions(
	item gjson.Result,
	functionNamesByCallID map[string]string,
	forAntigravity bool,
) []byte {
	out := []byte(`{"type":"function_result","name":"","result":{}}`)
	callID := firstNonEmpty(item.Get("call_id").String(), item.Get("id").String())
	name := responsesQualifiedInputToolName(item)
	if name == "" {
		name = functionNamesByCallID[callID]
	}
	if name != "" {
		if forAntigravity {
			name = translatorcommon.AntigravityToolNameToUpstream(name)
		}
		out, _ = sjson.SetBytes(out, "name", name)
	}
	if callID != "" {
		out, _ = sjson.SetBytes(out, "call_id", callID)
	}
	result := item.Get("output")
	if !result.Exists() {
		result = item.Get("result")
	}
	setJSONValue(&out, "result", result, []byte(`{}`))
	return out
}

func interactionsTextStep(stepType, text string) []byte {
	step := []byte(`{"type":"","content":[{"type":"text","text":""}]}`)
	step, _ = sjson.SetBytes(step, "type", stepType)
	step, _ = sjson.SetBytes(step, "content.0.text", text)
	return step
}

func appendResponsesToolsToInteractions(
	out []byte,
	identity *translatorcommon.ResponsesToolIdentity,
	forAntigravity, forDevin bool,
) []byte {
	if identity == nil {
		return out
	}
	var tools [][]byte
	for _, declaration := range identity.Declarations() {
		if forDevin && translatorcommon.IsDevinCodexAppAutomationUpdate(declaration.Namespace, declaration.Name) {
			continue
		}
		name := declaration.QualifiedName
		if forAntigravity {
			name = translatorcommon.AntigravityToolNameToUpstream(name)
		}
		tool := gjson.Parse(declaration.Raw)
		item := []byte(`{"type":"function","name":""}`)
		item, _ = sjson.SetBytes(item, "name", name)
		description := firstNonEmpty(tool.Get("description").String(), tool.Get("function.description").String())
		if forDevin {
			description = translatorcommon.SanitizeDevinToolDescription(declaration.QualifiedName, description)
			description = translatorcommon.SanitizeDevinToolDescription(declaration.Name, description)
		}
		if description != "" {
			item, _ = sjson.SetBytes(item, "description", description)
		}
		if declaration.Kind == translatorcommon.ResponsesCustomTool {
			item, _ = sjson.SetRawBytes(item, "parameters", []byte(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}`))
		} else if parameters := firstExisting(
			tool.Get("parameters"), tool.Get("parametersJsonSchema"), tool.Get("input_schema"),
			tool.Get("function.parameters"), tool.Get("function.parametersJsonSchema"),
		); parameters.Exists() {
			item, _ = sjson.SetRawBytes(item, "parameters", []byte(parameters.Raw))
		}
		tools = append(tools, item)
	}
	if len(tools) > 0 {
		out, _ = sjson.SetRawBytes(out, "tools", translatorcommon.JoinRawArray(tools))
	}
	return out
}

func setInteractionsInputOnResponses(out []byte, input gjson.Result, forAntigravity bool) []byte {
	items := translatorcommon.NewRawArrayItems(input.Get("#").Int())
	if input.Type == gjson.String {
		items = append(items, interactionsTextMessage(input.String()))
	} else if input.IsArray() {
		input.ForEach(
			func(_, item gjson.Result) bool {
				if converted := interactionsInputItemToResponses(item, forAntigravity); converted != nil {
					items = append(items, converted)
				}
				return true
			},
		)
	} else if input.IsObject() {
		if converted := interactionsInputItemToResponses(input, forAntigravity); converted != nil {
			items = append(items, converted)
		}
	}
	if len(items) > 0 {
		out = translatorcommon.SetRawArrayItems(out, "input", items)
	}
	return out
}

func interactionsTextMessage(text string) []byte {
	item := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`)
	item, _ = sjson.SetBytes(item, "content.0.text", text)
	return item
}

func interactionsInputItemToResponses(item gjson.Result, forAntigravity bool) []byte {
	switch item.Get("type").String() {
	case "user_input":
		return interactionsMessageToResponses(item, "user")
	case "model_output":
		return interactionsMessageToResponses(item, "assistant")
	case "thought":
		return interactionsThoughtToResponses(item)
	case "function_call":
		return interactionsFunctionCallToResponses(item, forAntigravity)
	case "function_result":
		return interactionsFunctionResultToResponses(item, forAntigravity)
	default:
		if item.Type == gjson.String {
			return interactionsTextMessage(item.String())
		}
	}
	return nil
}

func interactionsMessageToResponses(item gjson.Result, role string) []byte {
	out := []byte(`{"type":"message","role":"","content":[]}`)
	out, _ = sjson.SetBytes(out, "role", role)
	content := item.Get("content")
	if content.Type == gjson.String {
		partType := "input_text"
		if role == "assistant" {
			partType = "output_text"
		}
		part := []byte(`{"type":"","text":""}`)
		part, _ = sjson.SetBytes(part, "type", partType)
		part, _ = sjson.SetBytes(part, "text", content.String())
		out, _ = sjson.SetRawBytes(out, "content.-1", part)
		return out
	}
	content.ForEach(
		func(_, part gjson.Result) bool {
			if converted, ok := interactionsContentPartToResponses(part, role); ok {
				out, _ = sjson.SetRawBytes(out, "content.-1", converted)
			}
			return true
		},
	)
	return out
}

func interactionsThoughtToResponses(item gjson.Result) []byte {
	out := []byte(`{"type":"reasoning","summary":[]}`)
	for _, text := range interactionsContentTexts(item.Get("content")) {
		part := []byte(`{"type":"summary_text","text":""}`)
		part, _ = sjson.SetBytes(part, "text", text)
		out, _ = sjson.SetRawBytes(out, "summary.-1", part)
	}
	return out
}

func interactionsContentPartToResponses(part gjson.Result, role string) ([]byte, bool) {
	partType := part.Get("type").String()
	if partType == "" && part.Get("text").Exists() {
		partType = "text"
	}
	switch partType {
	case "text":
		outType := "input_text"
		if role == "assistant" {
			outType = "output_text"
		}
		out := []byte(`{"type":"","text":""}`)
		out, _ = sjson.SetBytes(out, "type", outType)
		out, _ = sjson.SetBytes(out, "text", part.Get("text").String())
		return out, true
	case "image":
		outType := "input_image"
		if role == "assistant" {
			outType = "output_image"
		}
		out := []byte(`{"type":""}`)
		out, _ = sjson.SetBytes(out, "type", outType)
		imageURL := interactionsMediaDataURL(part)
		if imageURL != "" {
			out, _ = sjson.SetBytes(out, "image_url", imageURL)
		}
		return out, true
	case "audio":
		out := []byte(`{"type":"output_text","text":""}`)
		format := mediaFormat(part.Get("mime_type").String())
		out, _ = sjson.SetBytes(out, "text", "Audio content: inline data (Format: "+format+")")
		return out, true
	case "video", "document":
		outType := "input_file"
		if role == "assistant" {
			outType = "output_file"
		}
		out := []byte(`{"type":""}`)
		out, _ = sjson.SetBytes(out, "type", outType)
		if dataURL := interactionsMediaDataURL(part); dataURL != "" {
			out, _ = sjson.SetBytes(out, "file_data", dataURL)
		}
		if filename := part.Get("filename").String(); filename != "" {
			out, _ = sjson.SetBytes(out, "filename", filename)
		}
		return out, true
	}
	return nil, false
}

func interactionsFunctionCallToResponses(item gjson.Result, forAntigravity bool) []byte {
	out := []byte(`{"type":"function_call","call_id":"","name":"","arguments":"{}"}`)
	if itemID := firstNonEmpty(item.Get("id").String(), item.Get("call_id").String()); itemID != "" {
		out, _ = sjson.SetBytes(out, "id", itemID)
	}
	if callID := firstNonEmpty(item.Get("call_id").String(), item.Get("id").String()); callID != "" {
		out, _ = sjson.SetBytes(out, "call_id", callID)
	}
	name := item.Get("name").String()
	if forAntigravity {
		name = translatorcommon.AntigravityUpstreamToolNameToClient(name)
	}
	out, _ = sjson.SetBytes(out, "name", name)
	out, _ = translatorcommon.SetStringWithoutHTMLEscape(out, "arguments", jsonStringValue(item.Get("arguments"), "{}"))
	return out
}

func interactionsFunctionResultToResponses(item gjson.Result, forAntigravity bool) []byte {
	out := []byte(`{"type":"function_call_output","call_id":"","output":""}`)
	if callID := firstNonEmpty(item.Get("call_id").String(), item.Get("id").String()); callID != "" {
		out, _ = sjson.SetBytes(out, "call_id", callID)
	}
	if name := item.Get("name").String(); name != "" {
		if forAntigravity {
			name = translatorcommon.AntigravityUpstreamToolNameToClient(name)
		}
		out, _ = sjson.SetBytes(out, "name", name)
	}
	result := item.Get("result")
	if !result.Exists() {
		result = item.Get("output")
	}
	out, _ = sjson.SetBytes(out, "output", jsonStringValue(result, ""))
	return out
}

func appendInteractionsToolsToResponses(out []byte, tools gjson.Result, forAntigravity bool) []byte {
	if !tools.Exists() || !tools.IsArray() {
		return out
	}
	tools.ForEach(
		func(_, tool gjson.Result) bool {
			if converted, ok := responsesToolFromInteractionsTool(tool, forAntigravity); ok {
				out, _ = sjson.SetRawBytes(out, "tools.-1", converted)
			}
			if decls := tool.Get("function_declarations"); decls.Exists() && decls.IsArray() {
				decls.ForEach(
					func(_, decl gjson.Result) bool {
						if converted, ok := responsesToolFromInteractionsTool(decl, forAntigravity); ok {
							out, _ = sjson.SetRawBytes(out, "tools.-1", converted)
						}
						return true
					},
				)
			}
			return true
		},
	)
	return out
}

func responsesToolFromInteractionsTool(tool gjson.Result, forAntigravity bool) ([]byte, bool) {
	name := firstNonEmpty(tool.Get("name").String(), tool.Get("function.name").String())
	if name == "" {
		return nil, false
	}
	if forAntigravity {
		name = translatorcommon.AntigravityUpstreamToolNameToClient(name)
	}
	out := []byte(`{"type":"function","name":""}`)
	out, _ = sjson.SetBytes(out, "name", name)
	copyOptionalString(&out, "description", firstExisting(tool.Get("description"), tool.Get("function.description")))
	copyOptionalRaw(
		&out, "parameters",
		firstExisting(tool.Get("parameters"), tool.Get("function.parameters"), tool.Get("parametersJsonSchema")),
	)
	return out, true
}

func interactionsContentTexts(content gjson.Result) []string {
	texts := make([]string, 0)
	if content.Type == gjson.String {
		return append(texts, content.String())
	}
	if content.IsArray() {
		content.ForEach(
			func(_, part gjson.Result) bool {
				if text := firstNonEmpty(part.Get("text").String(), part.Get("content.text").String()); text != "" {
					texts = append(texts, text)
				}
				return true
			},
		)
	}
	return texts
}

func interactionsMediaDataURL(part gjson.Result) string {
	if url := firstNonEmpty(
		part.Get("image_url").String(), part.Get("file_data").String(), part.Get("url").String(),
	); url != "" {
		return url
	}
	data := part.Get("data").String()
	if data == "" {
		return ""
	}
	mimeType := part.Get("mime_type").String()
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return "data:" + mimeType + ";base64," + data
}

func mediaFormat(mimeType string) string {
	if mimeType == "" {
		return "unknown"
	}
	if _, format, ok := strings.Cut(mimeType, "/"); ok && format != "" {
		return format
	}
	return mimeType
}

func parseDataURL(value string) (string, string, bool) {
	if !strings.HasPrefix(value, "data:") {
		return "", "", false
	}
	header, data, ok := strings.Cut(strings.TrimPrefix(value, "data:"), ",")
	if !ok {
		return "", "", false
	}
	mimeType, _, _ := strings.Cut(header, ";")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	return mimeType, data, true
}

func setJSONValue(out *[]byte, path string, value gjson.Result, defaultRaw []byte) {
	if !value.Exists() {
		*out, _ = sjson.SetRawBytes(*out, path, defaultRaw)
		return
	}
	if value.Type == gjson.String && gjson.Valid(value.String()) {
		*out, _ = sjson.SetRawBytes(*out, path, []byte(value.String()))
		return
	}
	if value.Type == gjson.String {
		*out, _ = sjson.SetBytes(*out, path, value.String())
		return
	}
	*out, _ = sjson.SetRawBytes(*out, path, []byte(value.Raw))
}

func jsonStringValue(value gjson.Result, fallback string) string {
	if !value.Exists() {
		return fallback
	}
	if value.Type == gjson.String {
		return value.String()
	}
	return value.Raw
}

func copyOptionalString(out *[]byte, path string, value gjson.Result) {
	if value.Exists() {
		*out, _ = sjson.SetBytes(*out, path, value.String())
	}
}

func copyOptionalRaw(out *[]byte, path string, value gjson.Result) {
	if value.Exists() {
		*out, _ = sjson.SetRawBytes(*out, path, []byte(value.Raw))
	}
}

func isAntigravityModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "antigravity")
}

func mapAntigravityToolChoiceName(
	toolChoiceRaw []byte,
	toolChoice gjson.Result,
	mapName func(string) string,
) []byte {
	if name := toolChoice.Get("function.name").String(); name != "" {
		toolChoiceRaw, _ = sjson.SetBytes(toolChoiceRaw, "function.name", mapName(name))
	} else if name := toolChoice.Get("name").String(); name != "" {
		toolChoiceRaw, _ = sjson.SetBytes(toolChoiceRaw, "name", mapName(name))
	}
	return toolChoiceRaw
}

func firstExisting(values ...gjson.Result) gjson.Result {
	for _, value := range values {
		if value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
