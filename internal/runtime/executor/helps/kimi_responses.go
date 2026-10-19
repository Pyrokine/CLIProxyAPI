package helps

import (
	"fmt"
	"strings"

	kimiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/kimi"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ResolveKimiBaseURL returns the configured endpoint for the selected Kimi credential.
func ResolveKimiBaseURL(auth *cliproxyauth.Auth) string {
	if auth != nil {
		if raw := strings.TrimRight(strings.TrimSpace(auth.Attributes["base_url"]), "/"); raw != "" {
			return raw
		}
		if baseURL, ok := auth.Metadata["base_url"].(string); ok {
			if raw := strings.TrimRight(strings.TrimSpace(baseURL), "/"); raw != "" {
				return raw
			}
		}
		if kimiauth.IsKimiAIAuth(auth) {
			return kimiauth.KimiAIAPIBaseURL
		}
	}
	return kimiauth.APIBaseURL
}

// ResolveKimiResponsesURL resolves the upstream URL for Kimi Responses API requests.
func ResolveKimiResponsesURL(auth *cliproxyauth.Auth) string {
	baseURL := ResolveKimiBaseURL(auth)
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/responses"
	}
	return baseURL + "/v1/responses"
}

// ResolveKimiChatURL resolves the upstream URL for Kimi chat completions.
func ResolveKimiChatURL(auth *cliproxyauth.Auth) string {
	baseURL := ResolveKimiBaseURL(auth)
	if strings.HasSuffix(baseURL, "/v1") {
		return baseURL + "/chat/completions"
	}
	return baseURL + "/v1/chat/completions"
}

// ResolveKimiClaudeBaseURL resolves the endpoint used by the Claude Messages adapter.
func ResolveKimiClaudeBaseURL(auth *cliproxyauth.Auth) string {
	return strings.TrimSuffix(ResolveKimiBaseURL(auth), "/v1")
}

func isResponsesToolCall(item gjson.Result) bool {
	typ := strings.TrimSpace(item.Get("type").String())
	return typ == "function_call" || typ == "custom_tool_call"
}

func isResponsesToolOutput(item gjson.Result) bool {
	typ := strings.TrimSpace(item.Get("type").String())
	return typ == "function_call_output" || typ == "custom_tool_call_output"
}

// NormalizeKimiResponsesInput 让完整调用批次的工具输出连续出现，保留不完整批次的原序
func NormalizeKimiResponsesInput(body []byte) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return body, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}
	items := input.Array()
	result := make([]gjson.Result, 0, len(items))
	reordered := false
	for i := 0; i < len(items); {
		if !isResponsesToolCall(items[i]) {
			result = append(result, items[i])
			i++
			continue
		}
		start := i
		needed := make(map[string]int)
		remaining := 0
		for i < len(items) && isResponsesToolCall(items[i]) {
			if id := translatorcommon.ExtractResponsesCallID(items[i]); id != "" {
				needed[id]++
				remaining++
			}
			i++
		}
		result = append(result, items[start:i]...)
		if remaining == 0 {
			continue
		}
		outputs := make([]gjson.Result, 0, remaining)
		deferred := make([]gjson.Result, 0)
		end := i
		for end < len(items) && remaining > 0 && !isResponsesToolCall(items[end]) {
			item := items[end]
			id := translatorcommon.ExtractResponsesCallID(item)
			if isResponsesToolOutput(item) && needed[id] > 0 {
				needed[id]--
				remaining--
				outputs = append(outputs, item)
			} else {
				deferred = append(deferred, item)
			}
			end++
		}
		if remaining != 0 {
			continue
		}
		result = append(result, outputs...)
		result = append(result, deferred...)
		reordered = reordered || len(deferred) > 0
		i = end
	}
	if !reordered {
		return body, nil
	}
	raw := make([]string, 0, len(result))
	for _, item := range result {
		raw = append(raw, item.Raw)
	}
	updated, err := sjson.SetRawBytes(body, "input", []byte("["+strings.Join(raw, ",")+"]"))
	if err != nil {
		return body, fmt.Errorf("normalize kimi responses input: %w", err)
	}
	return updated, nil
}
