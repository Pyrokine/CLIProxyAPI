package responses

import (
	"regexp"
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	claudeWebSearchToolName    = "web_search"
	responsesWebSearchIDPrefix = "ws_"
	claudeServerToolIDPrefix   = "srvtoolu_"
)

var claudeServerToolIDSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_]`)

func responsesWebSearchCallID(claudeToolUseID string) string {
	return responsesWebSearchIDPrefix + claudeToolUseID
}

func claudeWebSearchToolUseID(responsesItemID string) string {
	body := strings.TrimPrefix(strings.TrimSpace(responsesItemID), responsesWebSearchIDPrefix)
	body = claudeServerToolIDSanitizer.ReplaceAllString(strings.TrimPrefix(body, claudeServerToolIDPrefix), "_")
	if body == "" {
		return ""
	}
	return claudeServerToolIDPrefix + body
}

func claudeWebSearchQuery(input string) string {
	if input == "" {
		return ""
	}
	return strings.TrimSpace(gjson.Get(input, "query").String())
}

func claudeWebSearchResultsToResponses(content gjson.Result) []byte {
	if content.IsObject() {
		return []byte(content.Raw)
	}
	if !content.IsArray() {
		return nil
	}
	var results [][]byte
	content.ForEach(func(_, entry gjson.Result) bool {
		if entry.Get("type").String() == "web_search_tool_result_error" || strings.TrimSpace(entry.Get("url").String()) != "" {
			results = append(results, []byte(entry.Raw))
		}
		return true
	})
	if len(results) == 0 {
		return []byte(`[]`)
	}
	return translatorcommon.JoinRawArray(results)
}

func buildResponsesWebSearchCallItem(claudeToolUseID, query string, results []byte) []byte {
	item := []byte(`{"id":"","type":"web_search_call","status":"completed","action":{"type":"search","query":""}}`)
	item, _ = sjson.SetBytes(item, "id", responsesWebSearchCallID(claudeToolUseID))
	item, _ = sjson.SetBytes(item, "action.query", query)
	if len(results) > 0 {
		item, _ = sjson.SetRawBytes(item, "results", results)
	}
	return item
}

func convertResponsesWebSearchCallToClaudeBlocks(item gjson.Result) [][]byte {
	toolUseID := claudeWebSearchToolUseID(strings.TrimSpace(item.Get("id").String()))
	if toolUseID == "" {
		return nil
	}

	toolUse := []byte(`{"type":"server_tool_use","id":"","name":"","input":{}}`)
	toolUse, _ = sjson.SetBytes(toolUse, "id", toolUseID)
	toolUse, _ = sjson.SetBytes(toolUse, "name", claudeWebSearchToolName)
	if query := responsesWebSearchCallQuery(item); query != "" {
		toolUse, _ = sjson.SetBytes(toolUse, "input.query", query)
	}

	result := []byte(`{"type":"web_search_tool_result","tool_use_id":"","content":[]}`)
	result, _ = sjson.SetBytes(result, "tool_use_id", toolUseID)
	if content := responsesWebSearchResultsToClaude(item.Get("results")); len(content) > 0 {
		result, _ = sjson.SetRawBytes(result, "content", content)
	}
	return [][]byte{toolUse, result}
}

func responsesWebSearchCallQuery(item gjson.Result) string {
	if query := strings.TrimSpace(item.Get("action.query").String()); query != "" {
		return query
	}
	if query := strings.TrimSpace(item.Get("action.queries.0").String()); query != "" {
		return query
	}
	return strings.TrimSpace(item.Get("action.url").String())
}

func responsesWebSearchResultsToClaude(results gjson.Result) []byte {
	if results.IsObject() {
		return []byte(results.Raw)
	}
	if !results.IsArray() {
		return nil
	}
	var blocks [][]byte
	results.ForEach(func(_, entry gjson.Result) bool {
		if entry.Get("type").String() == "web_search_tool_result_error" {
			blocks = append(blocks, []byte(entry.Raw))
			return true
		}
		if strings.TrimSpace(entry.Get("encrypted_content").String()) == "" {
			return true
		}
		block := []byte(entry.Raw)
		block, _ = sjson.SetBytes(block, "type", "web_search_result")
		blocks = append(blocks, block)
		return true
	})
	if len(blocks) == 0 {
		return nil
	}
	return translatorcommon.JoinRawArray(blocks)
}

func attachClaudeCitations(textBlock []byte, annotations gjson.Result) []byte {
	if !annotations.IsArray() {
		return textBlock
	}
	var citations [][]byte
	annotations.ForEach(func(_, annotation gjson.Result) bool {
		if strings.TrimSpace(annotation.Get("encrypted_index").String()) != "" {
			citations = append(citations, []byte(annotation.Raw))
		}
		return true
	})
	if len(citations) == 0 {
		return textBlock
	}
	updated, err := sjson.SetRawBytes(textBlock, "citations", translatorcommon.JoinRawArray(citations))
	if err != nil {
		return textBlock
	}
	return updated
}
