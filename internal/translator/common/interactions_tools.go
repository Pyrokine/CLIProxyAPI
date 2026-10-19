package common

import (
	"encoding/json"

	"github.com/tidwall/gjson"
)

// GeminiToolFromInteractions 保留工具配置，将 Interactions 内置工具名称转换为 Gemini 字段
func GeminiToolFromInteractions(tool gjson.Result) map[string]any {
	if !tool.IsObject() {
		return nil
	}
	values := make(map[string]any)
	for key, value := range tool.Map() {
		values[key] = json.RawMessage(value.Raw)
	}
	for _, mapping := range []struct{ typ, native string }{
		{"url_context", "urlContext"},
		{"code_execution", "codeExecution"},
		{"google_search", "googleSearch"},
		{"web_search", "googleSearch"},
	} {
		if tool.Get("type").String() == mapping.typ {
			config := tool.Get(mapping.typ)
			if !config.IsObject() && mapping.typ == "web_search" {
				config = tool.Get("google_search")
			}
			if !config.IsObject() {
				config = tool.Get(mapping.native)
			}
			value := json.RawMessage(`{}`)
			if config.IsObject() {
				value = json.RawMessage(config.Raw)
			}
			return map[string]any{mapping.native: value}
		}
		if tool.Get("type").String() == "" {
			if value, exists := values[mapping.typ]; exists {
				if _, nativeExists := values[mapping.native]; !nativeExists {
					values[mapping.native] = value
				}
				delete(values, mapping.typ)
			}
		}
	}
	return values
}
