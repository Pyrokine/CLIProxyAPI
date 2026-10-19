package executor

import (
	"fmt"
	"net/http"
	"strings"

	multiagentv2 "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/codex/optimize-multi-agent-v2"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex declares known integer-only tool arguments as JSON Schema numbers.
// These paths need integer types when Codex clients use a non-Codex executor.
var codexClientIntegerToolFields = map[string][]string{
	"exec_command":                {"yield_time_ms", "max_output_tokens", "timeout_ms"},
	"write_stdin":                 {"session_id", "yield_time_ms", "max_output_tokens"},
	"sleep":                       {"duration_ms"},
	"wait_agent":                  {"timeout_ms"},
	"wait":                        {"yield_time_ms", "max_tokens"},
	"tool_search":                 {"limit"},
	"test_sync_tool":              {"sleep_before_ms", "sleep_after_ms", "participants", "timeout_ms", "barrier.properties.participants", "barrier.properties.timeout_ms"},
	"create_goal":                 {"token_budget"},
	"get_channels":                {"limit"},
	"list_threads":                {"limit", "max_chars_per_post"},
	"search_posts":                {"limit", "max_chars_per_post"},
	"read_thread":                 {"limit", "max_chars_per_post"},
	"read_post":                   {"offset_chars", "limit_chars"},
	"memories__list":              {"max_results"},
	"memories__read":              {"line_offset", "max_lines"},
	"memories__search":            {"context_lines", "max_results"},
	"history__list_windows":       {"limit"},
	"history__list_items":         {"limit", "max_chars_per_item"},
	"history__read_item":          {"offset_chars", "limit_chars"},
	"history__search_contents":    {"limit"},
	"notes__list_files_by_prefix": {"max_results"},
	"notes__read_file":            {"start_line", "stop_line", "start_line.anyOf.0", "stop_line.anyOf.0"},
	"notes__search_contents":      {"max_matches_per_file", "max_files"},
	"image_gen__imagegen":         {"num_last_images_to_include"},
	"web__run":                    {"search_query.items.properties.recency", "image_query.items.properties.recency", "open.items.properties.lineno", "click.items.properties.id", "screenshot.items.properties.pageno", "weather.items.properties.duration", "sports.items.properties.num_games"},
}

func normalizeCodexToolIntegerTypes(body []byte, headers http.Header) []byte {
	if len(body) == 0 || !multiagentv2.IsCodexClientUserAgent(headerValueCaseInsensitive(headers, "User-Agent")) {
		return body
	}
	if updated, changed := normalizeCodexIntegerToolList(gjson.GetBytes(body, "tools"), ""); changed {
		if next, err := sjson.SetRawBytes(body, "tools", updated); err == nil {
			body = next
		}
	}
	for index, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("type").String() != "additional_tools" {
			continue
		}
		if updated, changed := normalizeCodexIntegerToolList(item.Get("tools"), ""); changed {
			if next, err := sjson.SetRawBytes(body, fmt.Sprintf("input.%d.tools", index), updated); err == nil {
				body = next
			}
		}
	}
	return body
}

func normalizeCodexIntegerToolList(tools gjson.Result, namespace string) ([]byte, bool) {
	if !tools.IsArray() {
		return nil, false
	}
	var updated []byte
	offset := 0
	tools.ForEach(func(_, tool gjson.Result) bool {
		updatedTool, changed := normalizeCodexIntegerTool(tool, namespace)
		if !changed {
			return true
		}
		if updated == nil {
			updated = make([]byte, 0, len(tools.Raw))
		}
		start := tool.Index - tools.Index
		updated = append(updated, tools.Raw[offset:start]...)
		updated = append(updated, updatedTool...)
		offset = start + len(tool.Raw)
		return true
	})
	if updated == nil {
		return nil, false
	}
	return append(updated, tools.Raw[offset:]...), true
}

func normalizeCodexIntegerTool(tool gjson.Result, namespace string) ([]byte, bool) {
	toolJSON := []byte(tool.Raw)
	if tool.Get("type").String() == "namespace" {
		if namespace != "" || tool.Get("name").String() == "" {
			return nil, false
		}
		updated, changed := normalizeCodexIntegerToolList(tool.Get("tools"), tool.Get("name").String())
		if !changed {
			return nil, false
		}
		toolJSON, err := sjson.SetRawBytes(toolJSON, "tools", updated)
		return toolJSON, err == nil
	}
	for _, key := range []string{"function_declarations", "functionDeclarations"} {
		updated, changed := normalizeCodexIntegerToolList(tool.Get(key), namespace)
		if !changed {
			continue
		}
		toolJSON, err := sjson.SetRawBytes(toolJSON, key, updated)
		return toolJSON, err == nil
	}

	name := strings.TrimSpace(tool.Get("name").String())
	parametersPath := "parameters"
	parameters := tool.Get(parametersPath)
	if !parameters.IsObject() {
		parametersPath = "function.parameters"
		parameters = tool.Get(parametersPath)
		if name == "" {
			name = tool.Get("function.name").String()
		}
	}
	if !parameters.IsObject() {
		parametersPath = "input_schema"
		parameters = tool.Get(parametersPath)
	}
	if !parameters.IsObject() {
		parametersPath = "parametersJsonSchema"
		parameters = tool.Get(parametersPath)
	}
	if !parameters.IsObject() || name == "" {
		return nil, false
	}
	if namespace != "" {
		name = namespace + "__" + name
	}
	if strings.HasPrefix(name, "functions__") {
		name = strings.TrimPrefix(name, "functions__")
	} else {
		name = strings.TrimPrefix(name, "collab__")
	}
	switch name {
	case "multi_agent_v1__wait_agent", "collaboration__wait_agent":
		name = "wait_agent"
	case "collaboration__get_channels", "collaboration__list_threads", "collaboration__search_posts", "collaboration__read_thread", "collaboration__read_post":
		name = strings.TrimPrefix(name, "collaboration__")
	}
	fields := codexClientIntegerToolFields[name]
	if len(fields) == 0 {
		return nil, false
	}
	changed := false
	for _, field := range fields {
		path := parametersPath + ".properties." + field + ".type"
		value := gjson.GetBytes(toolJSON, path)
		switch {
		case value.Type == gjson.String && value.String() == "number":
			if updated, err := sjson.SetBytes(toolJSON, path, "integer"); err == nil {
				toolJSON = updated
				changed = true
			}
		case value.IsArray():
			seen := make(map[string]bool)
			var types []string
			containsNumber := false
			for _, item := range value.Array() {
				if item.Type != gjson.String {
					containsNumber = false
					break
				}
				itemType := item.String()
				if itemType == "number" {
					itemType = "integer"
					containsNumber = true
				}
				if !seen[itemType] {
					seen[itemType] = true
					types = append(types, itemType)
				}
			}
			if containsNumber {
				if updated, err := sjson.SetBytes(toolJSON, path, types); err == nil {
					toolJSON = updated
					changed = true
				}
			}
		}
	}
	return toolJSON, changed
}
