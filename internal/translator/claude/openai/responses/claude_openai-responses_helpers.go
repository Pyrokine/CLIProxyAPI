package responses

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func deriveClaudeResponsesUserID(root gjson.Result, modelName string) string {
	for _, path := range []string{"metadata.user_id", "user"} {
		value := root.Get(path)
		if value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
			return value.String()
		}
	}

	var seed string
	for _, path := range []string{
		"prompt_cache_key",
		"session_id",
		"sessionId",
		"conversation.id",
		"conversation",
		"conversation_id",
	} {
		if value := strings.TrimSpace(root.Get(path).String()); value != "" {
			seed = path + "\x00" + value
			break
		}
	}
	if seed == "" {
		for _, item := range root.Get("input").Array() {
			if !strings.EqualFold(strings.TrimSpace(item.Get("role").String()), "user") {
				continue
			}
			if text := stableResponsesMessageText(item.Get("content")); text != "" {
				seed = "content\x00" + text
				break
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

func stableResponsesMessageText(content gjson.Result) string {
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

func claudeResponsesStructuredOutputInstruction(format gjson.Result) string {
	if !format.Exists() {
		return ""
	}
	jsonObjectInstruction := "You must format your entire response as a valid JSON object. Do not include any explanations, markdown code blocks (such as ```json), or any text outside of the JSON object."
	switch strings.ToLower(strings.TrimSpace(format.Get("type").String())) {
	case "json_object":
		return jsonObjectInstruction
	case "json_schema":
		jsonSchema := format.Get("json_schema")
		schema := jsonSchema.Get("schema")
		if !schema.Exists() {
			schema = format.Get("schema")
		}
		if !schema.Exists() {
			return jsonObjectInstruction
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

func dropUnsupportedClaudeAssistantPrefill(modelName string, messages [][]byte) [][]byte {
	if !claudeModelRejectsAssistantPrefill(modelName) || len(messages) == 0 {
		return messages
	}
	last := gjson.ParseBytes(messages[len(messages)-1])
	if !strings.EqualFold(strings.TrimSpace(last.Get("role").String()), "assistant") {
		return messages
	}
	return messages[:len(messages)-1]
}

func stripTrailingClaudeThinkingBlocks(messages [][]byte) [][]byte {
	if len(messages) == 0 {
		return messages
	}
	lastIndex := len(messages) - 1
	last := gjson.ParseBytes(messages[lastIndex])
	if !strings.EqualFold(strings.TrimSpace(last.Get("role").String()), "assistant") {
		return messages
	}
	content := last.Get("content")
	if !content.IsArray() {
		return messages
	}
	parts := content.Array()
	end := len(parts)
	for end > 0 {
		partType := strings.TrimSpace(parts[end-1].Get("type").String())
		if partType != "thinking" && partType != "redacted_thinking" {
			break
		}
		end--
	}
	if end == len(parts) {
		return messages
	}
	if end == 0 {
		return messages[:lastIndex]
	}
	remainingParts := make([][]byte, end)
	for index := range end {
		remainingParts[index] = []byte(parts[index].Raw)
	}
	var updated []byte
	if end == 1 && parts[0].Get("type").String() == "text" &&
		!parts[0].Get("cache_control").Exists() && !parts[0].Get("citations").Exists() {
		updated, _ = sjson.SetBytes(messages[lastIndex], "content", parts[0].Get("text").String())
	} else {
		updated, _ = sjson.SetRawBytes(messages[lastIndex], "content", translatorcommon.JoinRawArray(remainingParts))
	}
	messages[lastIndex] = updated
	return messages
}

func claudeModelRejectsAssistantPrefill(modelName string) bool {
	normalized := strings.ToLower(strings.TrimSpace(modelName))
	for _, family := range []string{"fable", "opus-5", "sonnet-4-6"} {
		if strings.Contains(normalized, family) {
			return true
		}
	}
	return false
}

func normalizeCodexAgentMessages(payload []byte) []byte {
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload
	}
	updated := payload
	changed := false
	for itemIndex, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "agent_message" {
			continue
		}
		itemPath := "input." + strconv.Itoa(itemIndex)
		if content := item.Get("content"); content.IsArray() {
			for partIndex, part := range content.Array() {
				if strings.TrimSpace(part.Get("type").String()) != "encrypted_content" {
					continue
				}
				encrypted := part.Get("encrypted_content")
				if encrypted.Type != gjson.String {
					continue
				}
				partPath := itemPath + ".content." + strconv.Itoa(partIndex)
				var err error
				updated, err = sjson.SetBytes(updated, partPath+".type", "input_text")
				if err != nil {
					return payload
				}
				updated, err = sjson.SetBytes(updated, partPath+".text", encrypted.String())
				if err != nil {
					return payload
				}
				updated, err = sjson.DeleteBytes(updated, partPath+".encrypted_content")
				if err != nil {
					return payload
				}
			}
		}
		var err error
		updated, err = sjson.SetBytes(updated, itemPath+".role", "user")
		if err != nil {
			return payload
		}
		updated, err = sjson.SetBytes(updated, itemPath+".type", "message")
		if err != nil {
			return payload
		}
		changed = true
	}
	if !changed {
		return payload
	}
	return updated
}
