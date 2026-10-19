package helps

import (
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	openAIToolResultImageOmittedText = "[image omitted: unsupported by upstream]"
	claudeToolResultImageRelayNotice = "Images returned by the preceding tool call(s):"
	claudeToolResultImagePlaceholder = "[Tool returned image content; the images follow in the next user message.]"
)

// ShouldNormalizeOpenAIToolResultsForModel reports whether the selected model
// explicitly excludes image input through its input-modalities configuration.
func ShouldNormalizeOpenAIToolResultsForModel(
	compat *config.OpenAICompatibility,
	upstreamModel, requestedModel string,
) bool {
	if compat == nil {
		return false
	}

	if normalize, matched := openAICompatibilityModelExcludesImages(compat.Models, upstreamModel); matched {
		return normalize
	}
	normalize, _ := openAICompatibilityModelExcludesImages(compat.Models, requestedModel)
	return normalize
}

// NormalizeOpenAIToolResultsTextOnly 保留工具文本，用省略提示替换工具图像与其合成转发消息
func NormalizeOpenAIToolResultsTextOnly(payload []byte) []byte {
	if !gjson.ValidBytes(payload) {
		return payload
	}
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}
	result := make([]string, 0, len(messages.Array()))
	lastTool := -1
	marked := false
	changed := false
	setText := func(raw, text string) string {
		updated, err := sjson.SetBytes([]byte(raw), "content", text)
		if err != nil {
			log.Warn("text-only tool result normalization failed")
			return raw
		}
		changed = true
		return string(updated)
	}
	for _, message := range messages.Array() {
		raw := message.Raw
		content := message.Get("content")
		switch message.Get("role").String() {
		case "tool":
			if content.Exists() && content.Type != gjson.String {
				raw = setText(raw, flattenOpenAIToolResultContent(content))
			} else if content.String() == claudeToolResultImagePlaceholder {
				raw = setText(raw, openAIToolResultImageOmittedText)
				marked = true
			}
			lastTool = len(result)
		case "user":
			if lastTool >= 0 && content.IsArray() {
				parts := make([]string, 0)
				notice, images := false, false
				for _, part := range content.Array() {
					if part.Get("type").String() == "text" && part.Get("text").String() == claudeToolResultImageRelayNotice {
						notice = true
					} else if isOpenAIImageToolResultPart(part) {
						images = true
					} else {
						parts = append(parts, part.Raw)
					}
				}
				if notice && images {
					if !marked {
						text := gjson.Get(result[lastTool], "content").String()
						if !strings.Contains(text, openAIToolResultImageOmittedText) {
							if text != "" {
								text += "\n\n"
							}
							result[lastTool] = setText(result[lastTool], text+openAIToolResultImageOmittedText)
						}
					}
					lastTool, marked = -1, false
					if len(parts) == 0 {
						changed = true
						continue
					}
					updated, err := sjson.SetRawBytes([]byte(raw), "content", []byte("["+strings.Join(parts, ",")+"]"))
					if err != nil {
						log.Warn("text-only tool image relay normalization failed")
						return payload
					}
					raw = string(updated)
					changed = true
				}
			}
			lastTool, marked = -1, false
		default:
			lastTool, marked = -1, false
		}
		result = append(result, raw)
	}
	if !changed {
		return payload
	}
	updated, err := sjson.SetRawBytes(payload, "messages", []byte("["+strings.Join(result, ",")+"]"))
	if err != nil {
		log.Warn("text-only tool messages normalization failed")
		return payload
	}
	return updated
}

// NormalizeClaudeToolResultsTextOnly removes image parts before Claude requests
// are translated, preserving the omission marker on the originating tool result.
func NormalizeClaudeToolResultsTextOnly(payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.Exists() || !messages.IsArray() {
		return payload
	}

	out := payload
	for messageIndex, message := range messages.Array() {
		content := message.Get("content")
		if !content.IsArray() {
			continue
		}
		for partIndex, part := range content.Array() {
			if !strings.EqualFold(strings.TrimSpace(part.Get("type").String()), "tool_result") {
				continue
			}
			toolContent := part.Get("content")
			if !toolContent.Exists() || toolContent.Type == gjson.String {
				continue
			}
			path := fmt.Sprintf("messages.%d.content.%d.content", messageIndex, partIndex)
			updated, errSet := sjson.SetBytes(out, path, flattenOpenAIToolResultContent(toolContent))
			if errSet == nil {
				out = updated
			}
		}
	}
	return out
}

func openAICompatibilityModelExcludesImages(models []config.OpenAICompatibilityModel, model string) (bool, bool) {
	model = normalizeOpenAICompatibilityModelName(model)
	if model == "" {
		return false, false
	}

	for i := range models {
		if strings.EqualFold(model, normalizeOpenAICompatibilityModelName(models[i].Name)) {
			return inputModalitiesExcludeImages(models[i].InputModalities), true
		}
	}

	matched := false
	excludesImages := true
	for i := range models {
		if !strings.EqualFold(model, normalizeOpenAICompatibilityModelName(models[i].Alias)) {
			continue
		}
		matched = true
		if !inputModalitiesExcludeImages(models[i].InputModalities) {
			excludesImages = false
		}
	}
	return excludesImages && matched, matched
}

func inputModalitiesExcludeImages(modalities []string) bool {
	if len(modalities) == 0 {
		return false
	}

	hasText := false
	for _, rawModality := range modalities {
		switch strings.ToLower(strings.TrimSpace(rawModality)) {
		case "image":
			return false
		case "text":
			hasText = true
		}
	}
	return hasText
}

func normalizeOpenAICompatibilityModelName(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	return strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
}

func flattenOpenAIToolResultContent(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}

	if content.IsArray() {
		parts := make([]string, 0, 4)
		content.ForEach(
			func(_, item gjson.Result) bool {
				if part, ok := openAIToolResultPartText(item); ok {
					parts = append(parts, part)
				}
				return true
			},
		)
		return strings.Join(parts, "\n\n")
	}

	if content.IsObject() {
		if isOpenAIImageToolResultPart(content) {
			return openAIToolResultImageOmittedText
		}
		if text := content.Get("text"); text.Type == gjson.String {
			return text.String()
		}
	}

	return content.Raw
}

func openAIToolResultPartText(item gjson.Result) (string, bool) {
	if item.Type == gjson.String {
		return item.String(), true
	}
	if item.IsObject() {
		if isOpenAIImageToolResultPart(item) {
			return openAIToolResultImageOmittedText, true
		}
		if text := item.Get("text"); text.Type == gjson.String {
			return text.String(), true
		}
	}
	if item.Raw == "" {
		return "", false
	}
	return item.Raw, true
}

func isOpenAIImageToolResultPart(item gjson.Result) bool {
	if !item.IsObject() {
		return false
	}

	switch strings.ToLower(strings.TrimSpace(item.Get("type").String())) {
	case "image", "image_url", "input_image":
		return true
	}
	return item.Get("image_url").Exists() || item.Get("input_image").Exists()
}
