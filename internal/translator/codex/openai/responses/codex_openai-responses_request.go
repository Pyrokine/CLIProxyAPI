package responses

import (
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func ConvertOpenAIResponsesRequestToCodex(_ string, inputRawJSON []byte, _ bool) []byte {
	rawJSON := inputRawJSON

	inputResult := util.GetGJSONBytesNoCopy(rawJSON, "input")
	if inputResult.Type == gjson.String {
		input, _ := sjson.SetBytes(
			[]byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`),
			"0.content.0.text", inputResult.String(),
		)
		rawJSON, _ = sjson.SetRawBytes(rawJSON, "input", input)
		inputResult = util.GetGJSONBytesNoCopy(rawJSON, "input")
	}

	rawJSON = setCodexRequiredBool(rawJSON, "stream", true)
	rawJSON = setCodexRequiredBool(rawJSON, "store", false)
	rawJSON = setCodexRequiredBool(rawJSON, "parallel_tool_calls", true)
	rawJSON = setCodexRequiredInclude(rawJSON)

	// Codex Responses rejects token limit fields, so strip them out before forwarding.
	rawJSON = deleteCodexRequestFields(
		rawJSON,
		"max_output_tokens",
		"max_completion_tokens",
		"temperature",
		"top_p",
		"stream_options",
	)
	if serviceTier := gjson.GetBytes(
		rawJSON, "service_tier",
	); serviceTier.Exists() && serviceTier.String() != "priority" {
		rawJSON = deleteCodexRequestFields(rawJSON, "service_tier")
	}

	rawJSON = deleteCodexRequestFields(rawJSON, "truncation")
	rawJSON = applyResponsesCompactionCompatibility(rawJSON)

	// Delete the user field as it is not supported by the Codex upstream.
	rawJSON = deleteCodexRequestFields(rawJSON, "user")

	// Convert role "system" to "developer" in input array to comply with Codex API requirements.
	rawJSON = convertSystemRoleToDeveloperWithInput(rawJSON, inputResult)
	rawJSON = normalizeCodexBuiltinTools(rawJSON)

	return rawJSON
}

func setCodexRequiredBool(rawJSON []byte, path string, value bool) []byte {
	current := gjson.GetBytes(rawJSON, path)
	if (value && current.Type == gjson.True) || (!value && current.Type == gjson.False) {
		return rawJSON
	}

	updated, err := sjson.SetBytes(rawJSON, path, value)
	if err != nil {
		return rawJSON
	}
	return updated
}

func setCodexRequiredInclude(rawJSON []byte) []byte {
	current := gjson.GetBytes(rawJSON, "include")
	values := current.Array()
	if current.IsArray() && len(values) == 1 &&
		values[0].Type == gjson.String &&
		values[0].String() == "reasoning.encrypted_content" {
		return rawJSON
	}

	updated, err := sjson.SetRawBytes(
		rawJSON,
		"include",
		[]byte(`["reasoning.encrypted_content"]`),
	)
	if err != nil {
		return rawJSON
	}
	return updated
}

func deleteCodexRequestFields(rawJSON []byte, paths ...string) []byte {
	for _, path := range paths {
		if !gjson.GetBytes(rawJSON, path).Exists() {
			continue
		}

		updated, err := sjson.DeleteBytes(rawJSON, path)
		if err == nil {
			rawJSON = updated
		}
	}
	return rawJSON
}

// applyResponsesCompactionCompatibility handles OpenAI Responses context_management.compaction
// for Codex upstream compatibility.
//
// Codex /responses currently rejects context_management with:
// {"detail":"Unsupported parameter: context_management"}.
//
// Compatibility strategy:
// 1) Remove context_management before forwarding to Codex upstream.
func applyResponsesCompactionCompatibility(rawJSON []byte) []byte {
	if !gjson.GetBytes(rawJSON, "context_management").Exists() {
		return rawJSON
	}

	rawJSON, _ = sjson.DeleteBytes(rawJSON, "context_management")
	return rawJSON
}

func convertSystemRoleToDeveloperWithInput(
	rawJSON []byte,
	inputResult gjson.Result,
) []byte {
	if !inputResult.IsArray() {
		return rawJSON
	}

	inputItems := inputResult.Array()
	hasSystemRole := false
	for _, item := range inputItems {
		if item.IsObject() && item.Get("role").String() == "system" {
			hasSystemRole = true
			break
		}
	}
	if !hasSystemRole {
		return rawJSON
	}

	rebuiltInput := make([][]byte, 0, len(inputItems))
	for _, item := range inputItems {
		itemRaw := []byte(item.Raw)
		if item.IsObject() && item.Get("role").String() == "system" {
			updatedItem, err := sjson.SetBytes(itemRaw, "role", "developer")
			if err != nil {
				return rawJSON
			}
			itemRaw = updatedItem
		}
		rebuiltInput = append(rebuiltInput, itemRaw)
	}

	updated, err := sjson.SetRawBytes(
		rawJSON,
		"input",
		translatorcommon.JoinRawArray(rebuiltInput),
	)
	if err != nil {
		return rawJSON
	}
	return updated
}

func normalizeCodexBuiltinTools(rawJSON []byte) []byte {
	result := normalizeCodexBuiltinToolArray(rawJSON, "tools")
	result = normalizeCodexBuiltinToolAtPath(result, "tool_choice.type")
	return normalizeCodexBuiltinToolArray(result, "tool_choice.tools")
}

func normalizeCodexBuiltinToolArray(rawJSON []byte, path string) []byte {
	tools := util.GetGJSONBytesNoCopy(rawJSON, path)
	if !tools.IsArray() {
		return rawJSON
	}

	changed := false
	var toolItems [][]byte
	tools.ForEach(
		func(_, tool gjson.Result) bool {
			item := []byte(tool.Raw)
			currentType := tool.Get("type").String()
			normalizedType := normalizeCodexBuiltinToolType(currentType)
			if normalizedType != "" {
				updated, err := sjson.SetBytes(item, "type", normalizedType)
				if err == nil {
					item = updated
					changed = true
					log.Debugf(
						"codex responses: normalized builtin tool type at %s.%d.type from %q to %q",
						path,
						len(toolItems),
						currentType,
						normalizedType,
					)
				}
			}
			toolItems = append(toolItems, item)
			return true
		},
	)
	if !changed {
		return rawJSON
	}

	updated, err := sjson.SetRawBytes(
		rawJSON,
		path,
		translatorcommon.JoinRawArray(toolItems),
	)
	if err != nil {
		return rawJSON
	}
	return updated
}

func normalizeCodexBuiltinToolAtPath(rawJSON []byte, path string) []byte {
	currentType := gjson.GetBytes(rawJSON, path).String()
	normalizedType := normalizeCodexBuiltinToolType(currentType)
	if normalizedType == "" {
		return rawJSON
	}

	updated, err := sjson.SetBytes(rawJSON, path, normalizedType)
	if err != nil {
		return rawJSON
	}

	log.Debugf("codex responses: normalized builtin tool type at %s from %q to %q", path, currentType, normalizedType)
	return updated
}

func normalizeCodexBuiltinToolType(toolType string) string {
	switch toolType {
	case "web_search_preview", "web_search_preview_2025_03_11":
		return "web_search"
	default:
		return ""
	}
}
