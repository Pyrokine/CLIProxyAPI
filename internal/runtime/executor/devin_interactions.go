package executor

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	internalsignature "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

type devinRequestPayload struct {
	systemPrompt  string
	prompts       []helps.DevinPrompt
	tools         []helps.DevinTool
	temperature   *float64
	maxTokens     int
	sessionID     string
	cascadeID     string
	thinkingLevel string
	budgetTokens  int
}

func parseDevinInteractionsPayload(payload, originalRequest []byte) devinRequestPayload {
	root := gjson.ParseBytes(payload)
	parsed := devinRequestPayload{
		systemPrompt: strings.TrimSpace(firstNonEmptyDevinValue(
			root.Get("system_instruction").String(),
			root.Get("systemInstruction").String(),
		)),
	}

	generationConfig := root.Get("generation_config")
	if !generationConfig.Exists() {
		generationConfig = root.Get("generationConfig")
	}
	if generationConfig.Exists() {
		if temperature := generationConfig.Get("temperature"); temperature.Exists() {
			value := temperature.Float()
			parsed.temperature = &value
		}
		parsed.maxTokens = int(firstExistingDevinResult(
			generationConfig.Get("max_output_tokens"),
			generationConfig.Get("maxOutputTokens"),
		).Int())
		parsed.thinkingLevel = strings.TrimSpace(firstNonEmptyDevinValue(
			generationConfig.Get("thinking_level").String(),
			generationConfig.Get("thinkingLevel").String(),
		))
		parsed.budgetTokens = int(firstExistingDevinResult(
			generationConfig.Get("thinking_config.thinking_budget"),
			generationConfig.Get("thinkingConfig.thinkingBudget"),
		).Int())
	}
	if parsed.temperature == nil {
		if temperature := root.Get("temperature"); temperature.Exists() {
			value := temperature.Float()
			parsed.temperature = &value
		} else if original := gjson.ParseBytes(originalRequest); original.Get("temperature").Exists() {
			value := original.Get("temperature").Float()
			parsed.temperature = &value
		}
	}
	if parsed.maxTokens <= 0 {
		parsed.maxTokens = helps.DevinDefaultMaxTokens
	}

	parsed.sessionID = devinSessionIDFromJSON(root)
	if parsed.sessionID == "" && len(originalRequest) > 0 {
		parsed.sessionID = devinSessionIDFromJSON(gjson.ParseBytes(originalRequest))
	}
	parsed.cascadeID = strings.TrimSpace(firstNonEmptyDevinValue(
		root.Get("cascade_id").String(),
		root.Get("cascadeId").String(),
	))
	if parsed.cascadeID == "" {
		parsed.cascadeID = parsed.sessionID
	}

	input := root.Get("input")
	if input.IsArray() {
		parsed.prompts = parseDevinInteractionSteps(input.Array())
	} else if messages := root.Get("messages"); messages.IsArray() {
		parsed.prompts, parsed.systemPrompt = parseDevinMessages(messages.Array(), parsed.systemPrompt)
	}
	if len(originalRequest) > 0 {
		supplementDevinSignatures(originalRequest, parsed.prompts)
		supplementDevinImages(originalRequest, parsed.prompts)
	}
	parsed.tools = parseDevinTools(root.Get("tools"))
	return parsed
}

func devinSessionIDFromJSON(root gjson.Result) string {
	return strings.TrimSpace(firstNonEmptyDevinValue(
		root.Get("session_id").String(),
		root.Get("sessionId").String(),
		root.Get("conversation_id").String(),
		root.Get("conversationId").String(),
		root.Get("previous_interaction_id").String(),
		root.Get("previousInteractionId").String(),
	))
}

func parseDevinInteractionSteps(steps []gjson.Result) []helps.DevinPrompt {
	prompts := make([]helps.DevinPrompt, 0, len(steps))
	for _, step := range steps {
		switch strings.ToLower(strings.TrimSpace(step.Get("type").String())) {
		case "user_input":
			text, images := extractDevinContent(step)
			prompts = append(prompts, helps.DevinPrompt{
				MessageID: uuid.NewString(),
				Source:    1,
				Content:   text,
				Images:    images,
			})
		case "thought":
			text := extractDevinText(step)
			signature, signatureType := parseDevinSignature(firstNonEmptyDevinValue(
				step.Get("signature").String(),
				step.Get("thought_signature").String(),
			))
			assistant := ensureDevinAssistantPrompt(&prompts)
			if assistant.Thinking == "" {
				assistant.Thinking = text
			} else if text != "" {
				assistant.Thinking += "\n\n" + text
			}
			mergeDevinSignature(assistant, signature, signatureType)
		case "model_output":
			text := extractDevinText(step)
			signature, signatureType := parseDevinSignature(firstNonEmptyDevinValue(
				step.Get("signature").String(),
				step.Get("thought_signature").String(),
			))
			assistant := ensureDevinAssistantPrompt(&prompts)
			if assistant.Content == "" {
				assistant.Content = text
			} else if text != "" {
				assistant.Content += "\n" + text
			}
			mergeDevinSignature(assistant, signature, signatureType)
		case "function_call":
			call := helps.DevinToolCall{
				ID: firstNonEmptyDevinValue(
					step.Get("id").String(),
					step.Get("call_id").String(),
				),
				Name: firstNonEmptyDevinValue(
					step.Get("name").String(),
					step.Get("function.name").String(),
				),
				Arguments: devinJSONValue(firstExistingDevinResult(
					step.Get("arguments"),
					step.Get("function.arguments"),
				)),
			}
			assistant := ensureDevinAssistantPrompt(&prompts)
			assistant.ToolCalls = append(assistant.ToolCalls, call)
		case "function_result":
			prompts = append(prompts, helps.DevinPrompt{
				MessageID: uuid.NewString(),
				Source:    4,
				ToolCallID: firstNonEmptyDevinValue(
					step.Get("id").String(),
					step.Get("call_id").String(),
				),
				Content: devinJSONValue(firstExistingDevinResult(
					step.Get("result"),
					step.Get("output"),
					step.Get("content"),
				)),
			})
		}
	}
	return prompts
}

func parseDevinMessages(messages []gjson.Result, systemPrompt string) ([]helps.DevinPrompt, string) {
	prompts := make([]helps.DevinPrompt, 0, len(messages))
	for _, message := range messages {
		switch strings.ToLower(strings.TrimSpace(message.Get("role").String())) {
		case "system", "developer":
			text := extractDevinText(message)
			if systemPrompt == "" {
				systemPrompt = text
			} else if text != "" {
				systemPrompt += "\n\n" + text
			}
		case "user":
			text, images := extractDevinContent(message)
			prompts = append(prompts, helps.DevinPrompt{
				MessageID: uuid.NewString(),
				Source:    1,
				Content:   text,
				Images:    images,
			})
		case "assistant":
			prompt := helps.DevinPrompt{
				MessageID: uuid.NewString(),
				Source:    2,
				Content:   extractDevinText(message),
			}
			for _, call := range message.Get("tool_calls").Array() {
				prompt.ToolCalls = append(prompt.ToolCalls, helps.DevinToolCall{
					ID: call.Get("id").String(),
					Name: firstNonEmptyDevinValue(
						call.Get("function.name").String(),
						call.Get("name").String(),
					),
					Arguments: devinJSONValue(firstExistingDevinResult(
						call.Get("function.arguments"),
						call.Get("arguments"),
					)),
				})
			}
			prompts = append(prompts, prompt)
		case "tool":
			prompts = append(prompts, helps.DevinPrompt{
				MessageID: uuid.NewString(),
				Source:    4,
				ToolCallID: firstNonEmptyDevinValue(
					message.Get("tool_call_id").String(),
					message.Get("id").String(),
				),
				Content: devinJSONValue(firstExistingDevinResult(
					message.Get("content"),
					message.Get("output"),
					message.Get("result"),
				)),
			})
		}
	}
	return prompts, systemPrompt
}

func ensureDevinAssistantPrompt(prompts *[]helps.DevinPrompt) *helps.DevinPrompt {
	items := *prompts
	if len(items) > 0 && items[len(items)-1].Source == 2 {
		return &items[len(items)-1]
	}
	*prompts = append(items, helps.DevinPrompt{MessageID: uuid.NewString(), Source: 2})
	return &(*prompts)[len(*prompts)-1]
}

func mergeDevinSignature(prompt *helps.DevinPrompt, signature []byte, signatureType string) {
	if prompt == nil || len(prompt.Signature) > 0 || len(signature) == 0 {
		return
	}
	prompt.Signature = signature
	prompt.SignatureType = signatureType
}

func parseDevinTools(result gjson.Result) []helps.DevinTool {
	if !result.IsArray() {
		return nil
	}
	tools := make([]helps.DevinTool, 0, len(result.Array()))
	for _, tool := range result.Array() {
		name := firstNonEmptyDevinValue(tool.Get("name").String(), tool.Get("function.name").String())
		description := firstNonEmptyDevinValue(
			tool.Get("description").String(),
			tool.Get("function.description").String(),
		)
		parameters := firstExistingDevinResult(
			tool.Get("parameters"),
			tool.Get("input_schema"),
			tool.Get("function.parameters"),
		)
		if name == "" {
			continue
		}
		tools = append(tools, helps.DevinTool{
			Name:        name,
			Description: description,
			Parameters:  []byte(parameters.Raw),
		})
	}
	return tools
}

func extractDevinContent(container gjson.Result) (string, []helps.DevinImage) {
	content := container.Get("content")
	if content.Type == gjson.String {
		return content.String(), nil
	}
	if !content.IsArray() {
		return container.Get("text").String(), nil
	}

	var textParts []string
	var images []helps.DevinImage
	for _, part := range content.Array() {
		switch strings.ToLower(strings.TrimSpace(part.Get("type").String())) {
		case "text", "input_text", "output_text":
			if text := part.Get("text").String(); text != "" {
				textParts = append(textParts, text)
			}
		case "image", "input_image", "image_url":
			if image, ok := extractDevinImage(part); ok {
				images = append(images, image)
			}
		default:
			if text := part.Get("text").String(); text != "" {
				textParts = append(textParts, text)
			}
		}
	}
	text := strings.Join(textParts, "\n")
	return addDevinImageHeaders(text, images), images
}

func extractDevinImage(part gjson.Result) (helps.DevinImage, bool) {
	data := strings.TrimSpace(firstNonEmptyDevinValue(
		part.Get("data").String(),
		part.Get("source.data").String(),
		part.Get("inline_data.data").String(),
	))
	mimeType := strings.TrimSpace(firstNonEmptyDevinValue(
		part.Get("mime_type").String(),
		part.Get("source.media_type").String(),
		part.Get("inline_data.mime_type").String(),
	))
	if data == "" {
		rawURL := firstNonEmptyDevinValue(
			part.Get("image_url.url").String(),
			part.Get("image_url").String(),
			part.Get("url").String(),
		)
		if dataMIME, dataPayload, ok := parseDevinDataURL(rawURL); ok {
			data = dataPayload
			if mimeType == "" {
				mimeType = dataMIME
			}
		}
	}
	if data == "" {
		return helps.DevinImage{}, false
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	return helps.DevinImage{Base64Data: data, MimeType: mimeType}, true
}

func addDevinImageHeaders(text string, images []helps.DevinImage) string {
	if len(images) == 0 || strings.Contains(text, "[Image ") {
		return text
	}
	headers := make([]string, 0, len(images))
	for index, image := range images {
		headers = append(headers, fmt.Sprintf(
			"[Image %d: pasted_image_%d.%s]",
			index+1,
			index+1,
			devinMIMEExtension(image.MimeType),
		))
	}
	header := strings.Join(headers, "\n")
	if text == "" {
		return header
	}
	return header + "\n\n" + text
}

func extractDevinText(container gjson.Result) string {
	content := container.Get("content")
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		parts := make([]string, 0, len(content.Array()))
		for _, part := range content.Array() {
			if text := firstNonEmptyDevinValue(part.Get("text").String(), part.Get("thinking").String()); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return firstNonEmptyDevinValue(container.Get("text").String(), container.Get("thinking").String())
}

func devinJSONValue(result gjson.Result) string {
	if !result.Exists() {
		return ""
	}
	if result.Type == gjson.String {
		return result.String()
	}
	return result.Raw
}

func parseDevinDataURL(raw string) (string, string, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "data:") {
		return "", "", false
	}
	comma := strings.IndexByte(raw, ',')
	if comma < 0 {
		return "", "", false
	}
	header := raw[len("data:"):comma]
	mimeType := strings.TrimSpace(strings.Split(header, ";")[0])
	if mimeType == "" {
		mimeType = "image/png"
	}
	return mimeType, raw[comma+1:], true
}

func devinMIMEExtension(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		return "png"
	}
}

type devinAssistantMetadata struct {
	signature     []byte
	signatureType string
	thinking      string
}

func supplementDevinSignatures(original []byte, prompts []helps.DevinPrompt) {
	messages := gjson.GetBytes(original, "messages")
	if !messages.IsArray() {
		return
	}
	assistantMetadata := make([]devinAssistantMetadata, 0)
	for _, message := range messages.Array() {
		if !strings.EqualFold(message.Get("role").String(), "assistant") {
			continue
		}
		var metadata devinAssistantMetadata
		for _, part := range message.Get("content").Array() {
			if !strings.EqualFold(part.Get("type").String(), "thinking") {
				continue
			}
			if signature := part.Get("signature").String(); signature != "" {
				metadata.signature, metadata.signatureType = parseDevinSignature(signature)
			}
			metadata.thinking = part.Get("thinking").String()
		}
		assistantMetadata = append(assistantMetadata, metadata)
	}

	assistantIndex := 0
	for index := range prompts {
		if prompts[index].Source != 2 || assistantIndex >= len(assistantMetadata) {
			continue
		}
		metadata := assistantMetadata[assistantIndex]
		mergeDevinSignature(&prompts[index], metadata.signature, metadata.signatureType)
		if prompts[index].Thinking == "" {
			prompts[index].Thinking = metadata.thinking
		}
		assistantIndex++
	}
}

func supplementDevinImages(original []byte, prompts []helps.DevinPrompt) {
	messages := gjson.GetBytes(original, "messages")
	if !messages.IsArray() {
		return
	}
	userImages := make([][]helps.DevinImage, 0)
	for _, message := range messages.Array() {
		if !strings.EqualFold(message.Get("role").String(), "user") {
			continue
		}
		_, images := extractDevinContent(message)
		userImages = append(userImages, images)
	}

	userIndex := 0
	for index := range prompts {
		if prompts[index].Source != 1 {
			continue
		}
		if len(prompts[index].Images) == 0 && userIndex < len(userImages) {
			prompts[index].Images = userImages[userIndex]
			prompts[index].Content = addDevinImageHeaders(prompts[index].Content, prompts[index].Images)
		}
		userIndex++
	}
}

func parseDevinSignature(raw string) ([]byte, string) {
	signature := strings.TrimSpace(raw)
	if signature == "" {
		return nil, ""
	}
	for prefix, provider := range map[string]string{
		"claude#": "anthropic",
		"gpt#":    "openai",
		"gemini#": "gemini",
	} {
		if strings.HasPrefix(signature, prefix) {
			return []byte(strings.TrimPrefix(signature, prefix)), provider
		}
	}
	if strings.HasPrefix(signature, "sealed.v1.") {
		return []byte(signature), "sealed"
	}
	if provider := detectDevinSignatureProvider(signature); provider != "" {
		return []byte(signature), provider
	}
	if decoded, err := base64.StdEncoding.DecodeString(signature); err == nil && len(decoded) > 0 {
		decodedString := string(decoded)
		if strings.HasPrefix(decodedString, "sealed.v1.") {
			return decoded, "sealed"
		}
		if provider := detectDevinSignatureProvider(decodedString); provider != "" {
			if provider == "gemini" {
				return []byte(signature), provider
			}
			return decoded, provider
		}
		if decoded[0] == 0x01 {
			return []byte(signature), "gemini"
		}
	}
	return []byte(signature), "sealed"
}

func detectDevinSignatureProvider(signature string) string {
	switch internalsignature.DetectProvider(strings.TrimSpace(signature)) {
	case internalsignature.ProviderClaude:
		return "anthropic"
	case internalsignature.ProviderGPT:
		return "openai"
	case internalsignature.ProviderGemini:
		return "gemini"
	}
	switch {
	case strings.HasPrefix(signature, "CAQS"), strings.HasPrefix(signature, "CAIS"):
		return "anthropic"
	case strings.HasPrefix(signature, "gAAAA"):
		return "openai"
	case strings.HasPrefix(signature, "AY"):
		return "gemini"
	default:
		return ""
	}
}

func firstExistingDevinResult(results ...gjson.Result) gjson.Result {
	for _, result := range results {
		if result.Exists() {
			return result
		}
	}
	return gjson.Result{}
}

func firstNonEmptyDevinValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
