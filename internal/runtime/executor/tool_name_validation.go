package executor

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const providerToolNameMaxBytes = 64

func validateProviderToolName(name string) bool {
	if name == "" || len(name) > providerToolNameMaxBytes {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func invalidProviderToolNameError(path string) error {
	return fmt.Errorf(
		"invalid tool name at %s: expected 1-%d ASCII characters matching ^[a-zA-Z0-9_-]+$",
		path,
		providerToolNameMaxBytes,
	)
}

func validateCodexResponsesToolNames(body []byte, requireDeclaredToolChoice bool) error {
	declared := make(map[string]struct{})
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		for index, tool := range tools.Array() {
			toolType := strings.TrimSpace(tool.Get("type").String())
			if toolType != "function" && toolType != "custom" {
				continue
			}
			name := tool.Get("name").String()
			path := fmt.Sprintf("tools[%d].name", index)
			if !validateProviderToolName(name) {
				return invalidProviderToolNameError(path)
			}
			declared[name] = struct{}{}
		}
	}

	input := gjson.GetBytes(body, "input")
	if input.IsArray() {
		for index, item := range input.Array() {
			itemType := strings.TrimSpace(item.Get("type").String())
			if itemType != "function_call" && itemType != "custom_tool_call" {
				continue
			}
			name := item.Get("name").String()
			path := fmt.Sprintf("input[%d].name", index)
			if !validateProviderToolName(name) {
				return invalidProviderToolNameError(path)
			}
			// Tool declarations describe the current turn. Historical calls may
			// reference tools that are no longer available after the client changes
			// its active tool set, so only their provider-facing syntax is validated.
		}
	}

	return validateCodexToolChoiceNames(body, declared, requireDeclaredToolChoice)
}

func validateCodexToolChoiceNames(body []byte, declared map[string]struct{}, requireDeclared bool) error {
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.IsObject() {
		return nil
	}
	choices := []struct {
		path string
		item gjson.Result
	}{{path: "tool_choice", item: choice}}
	if allowed := choice.Get("tools"); allowed.IsArray() {
		for index, item := range allowed.Array() {
			choices = append(
				choices, struct {
					path string
					item gjson.Result
				}{path: fmt.Sprintf("tool_choice.tools[%d]", index), item: item},
			)
		}
	}
	for _, candidate := range choices {
		itemType := strings.TrimSpace(candidate.item.Get("type").String())
		if itemType != "function" && itemType != "custom" {
			continue
		}
		name := candidate.item.Get("name").String()
		path := candidate.path + ".name"
		if !validateProviderToolName(name) {
			return invalidProviderToolNameError(path)
		}
		if requireDeclared {
			if _, ok := declared[name]; !ok {
				return fmt.Errorf("undeclared tool name at %s", path)
			}
		}
	}
	return nil
}

func invalidXAIToolCallEvent(message string) []byte {
	event := []byte(`{"type":"response.failed","response":{"status":"failed","status_code":502,"error":{"type":"api_error","code":"invalid_tool_name","message":""}}}`)
	event, _ = sjson.SetBytes(event, "response.error.message", message)
	return event
}
