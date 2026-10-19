package responses

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func modelSupportsResponsesWebSearch(modelID string, nativeWebSearch ...*bool) bool {
	if len(nativeWebSearch) > 0 && nativeWebSearch[0] != nil {
		return *nativeWebSearch[0]
	}
	info := registry.LookupModelInfo(modelID)
	antigravityInfo := registry.LookupModelInfo(modelID, "antigravity")
	for _, model := range []*registry.ModelInfo{info, antigravityInfo} {
		if model != nil && model.NativeCapabilities != nil && model.NativeCapabilities.WebSearch != nil &&
			!*model.NativeCapabilities.WebSearch {
			return false
		}
	}
	for _, model := range []*registry.ModelInfo{info, antigravityInfo} {
		if model != nil && model.NativeCapabilities != nil && model.NativeCapabilities.WebSearch != nil &&
			*model.NativeCapabilities.WebSearch {
			return true
		}
	}
	if registry.AntigravityWebSearchModelFor(modelID) != "" {
		return true
	}
	return (info != nil && info.SupportsWebSearch) || (antigravityInfo != nil && antigravityInfo.SupportsWebSearch)
}

func isResponsesWebSearchTool(toolType string) bool {
	switch strings.ToLower(strings.TrimSpace(toolType)) {
	case "web_search", "web_search_2025_08_26", "web_search_preview", "web_search_preview_2025_03_11":
		return true
	default:
		return false
	}
}

func hasResponsesWebSearchTool(root gjson.Result) bool {
	for _, tool := range root.Get("tools").Array() {
		if isResponsesWebSearchTool(tool.Get("type").String()) {
			return true
		}
	}
	return false
}

func allowsResponsesWebSearchChoice(root gjson.Result) bool {
	choice := root.Get("tool_choice")
	if !choice.Exists() {
		return true
	}
	if choice.Type == gjson.String {
		switch choice.String() {
		case "", "auto", "required":
			return true
		}
		return false
	}
	if !choice.IsObject() {
		return false
	}
	switch toolType := choice.Get("type").String(); toolType {
	case "", "auto", "required":
		return true
	case "allowed_tools":
		tools := choice.Get("tools")
		if !tools.IsArray() {
			tools = choice.Get("allowed_tools.tools")
		}
		for _, tool := range tools.Array() {
			if isResponsesWebSearchTool(tool.Get("type").String()) {
				return true
			}
		}
		return false
	default:
		return isResponsesWebSearchTool(toolType)
	}
}

func responsesStreamUsesWebSearch(modelName string, originalRequest, translatedRequest []byte) bool {
	if len(originalRequest) > 0 {
		original := unwrapRequestRoot(gjson.ParseBytes(originalRequest))
		if !allowsResponsesWebSearchChoice(original) {
			return false
		}
	}
	if len(translatedRequest) > 0 {
		translated := gjson.ParseBytes(translatedRequest)
		translatedRoot := unwrapRequestRoot(translated)
		if translatedRoot.Get("tool_choice").Exists() && !allowsResponsesWebSearchChoice(translatedRoot) {
			return false
		}
		if translated.Get("requestType").String() == "web_search" {
			return true
		}
		for _, path := range []string{"tools", "request.tools"} {
			for _, tool := range translated.Get(path).Array() {
				if tool.Get("googleSearch").Exists() {
					return true
				}
			}
		}
		if translated.Get("contents").Exists() || translated.Get("request.contents").Exists() ||
			translated.Get("requestType").Exists() {
			return false
		}
	}
	request := pickRequestJSON(originalRequest, translatedRequest)
	if len(request) == 0 {
		return false
	}
	root := unwrapRequestRoot(gjson.ParseBytes(request))
	return hasResponsesWebSearchTool(root) && allowsResponsesWebSearchChoice(root) &&
		(modelSupportsResponsesWebSearch(modelName) || modelSupportsResponsesWebSearch(root.Get("model").String()))
}

type responsesWebSearchRequestError string

func (e responsesWebSearchRequestError) Error() string { return string(e) }

func (responsesWebSearchRequestError) StatusCode() int { return 400 }

func (responsesWebSearchRequestError) IsRequestScoped() bool { return true }

func responsesWebSearchAllowedDomains(root gjson.Result) ([]string, error) {
	var domains []string
	seenSearchTool := false
	for _, tool := range root.Get("tools").Array() {
		if !isResponsesWebSearchTool(tool.Get("type").String()) {
			continue
		}
		if seenSearchTool {
			return nil, responsesWebSearchRequestError("multiple web search tools cannot be translated unambiguously")
		}
		seenSearchTool = true
		filter := tool.Get("filters.allowed_domains")
		if !filter.Exists() {
			continue
		}
		if !filter.IsArray() || len(filter.Array()) == 0 {
			return nil, responsesWebSearchRequestError("web search allowed_domains must be a non-empty array")
		}
		for _, domain := range filter.Array() {
			if domain.Type != gjson.String || strings.TrimSpace(domain.String()) == "" {
				return nil, responsesWebSearchRequestError("web search allowed_domains contains an empty or non-string domain")
			}
			domains = append(domains, strings.TrimSpace(domain.String()))
		}
	}
	return domains, nil
}

func responsesWebSearchQuery(root gjson.Result) string {
	input := root.Get("input")
	if input.Type == gjson.String {
		return strings.TrimSpace(input.String())
	}
	items := input.Array()
	flatParts := true
	var flatTexts []string
	for _, item := range items {
		if item.Get("role").Exists() || !isResponsesContentPartType(item.Get("type").String()) {
			flatParts = false
			break
		}
		if text := strings.TrimSpace(item.Get("text").String()); text != "" {
			flatTexts = append(flatTexts, text)
		}
	}
	if flatParts && len(flatTexts) > 0 {
		return strings.Join(flatTexts, "\n")
	}
	for remaining := items; len(remaining) > 0; remaining = remaining[:len(remaining)-1] {
		item := remaining[len(remaining)-1]
		if role := item.Get("role").String(); role != "" && role != "user" {
			continue
		}
		if content := item.Get("content"); content.Type == gjson.String && strings.TrimSpace(content.String()) != "" {
			return strings.TrimSpace(content.String())
		}
		var texts []string
		for _, part := range item.Get("content").Array() {
			if text := strings.TrimSpace(part.Get("text").String()); text != "" {
				texts = append(texts, text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
		if text := strings.TrimSpace(item.Get("text").String()); text != "" {
			return text
		}
	}
	return strings.TrimSpace(root.Get("instructions").String())
}

func responsesGroundingMetadata(root gjson.Result) gjson.Result {
	if grounding := root.Get("candidates.0.groundingMetadata"); grounding.Exists() {
		return grounding
	}
	return root.Get("response.candidates.0.groundingMetadata")
}

func responsesGroundingQueries(grounding gjson.Result) []string {
	var queries []string
	for _, query := range grounding.Get("webSearchQueries").Array() {
		if text := strings.TrimSpace(query.String()); text != "" {
			queries = append(queries, text)
		}
	}
	return queries
}

func responsesGroundingSources(grounding gjson.Result) [][]byte {
	var sources [][]byte
	seen := make(map[string]bool)
	for _, chunk := range grounding.Get("groundingChunks").Array() {
		uri := strings.TrimSpace(chunk.Get("web.uri").String())
		if uri == "" || seen[uri] {
			continue
		}
		seen[uri] = true
		source := []byte(`{"type":"url","url":""}`)
		source, _ = sjson.SetBytes(source, "url", uri)
		sources = append(sources, source)
	}
	return sources
}

func responsesHasWebGrounding(grounding gjson.Result) bool {
	return len(responsesGroundingQueries(grounding)) > 0 || len(responsesGroundingSources(grounding)) > 0
}

func responsesMergeGroundingMetadata(existing, update gjson.Result) gjson.Result {
	if !update.Exists() {
		return existing
	}
	merged := []byte(`{}`)
	queries := responsesGroundingQueries(existing)
	seenQueries := make(map[string]bool, len(queries))
	for _, query := range queries {
		seenQueries[query] = true
	}
	for _, query := range responsesGroundingQueries(update) {
		if !seenQueries[query] {
			queries = append(queries, query)
			seenQueries[query] = true
		}
	}
	if len(queries) > 0 {
		merged, _ = sjson.SetBytes(merged, "webSearchQueries", queries)
	}

	// Preserve cumulative raw indices: supports may refer to chunks delivered in later frames.
	chunks := make([][]byte, 0, len(existing.Get("groundingChunks").Array())+len(update.Get("groundingChunks").Array()))
	byURI := make(map[string]int)
	for _, chunk := range append(existing.Get("groundingChunks").Array(), update.Get("groundingChunks").Array()...) {
		uri := strings.TrimSpace(chunk.Get("web.uri").String())
		if previous, ok := byURI[uri]; ok && uri != "" {
			if gjson.GetBytes(chunks[previous], "web.title").String() == "" && chunk.Get("web.title").String() != "" {
				chunks[previous], _ = sjson.SetBytes(chunks[previous], "web.title", chunk.Get("web.title").String())
			}
		} else if uri != "" {
			byURI[uri] = len(chunks)
		}
		chunks = append(chunks, []byte(chunk.Raw))
	}
	if len(chunks) > 0 {
		merged, _ = sjson.SetRawBytes(merged, "groundingChunks", translatorcommon.JoinRawArray(chunks))
	}

	supports := make([][]byte, 0, len(existing.Get("groundingSupports").Array())+len(update.Get("groundingSupports").Array()))
	seenSupports := make(map[string]bool)
	for _, support := range append(existing.Get("groundingSupports").Array(), update.Get("groundingSupports").Array()...) {
		if !seenSupports[support.Raw] {
			supports = append(supports, []byte(support.Raw))
			seenSupports[support.Raw] = true
		}
	}
	if len(supports) > 0 {
		merged, _ = sjson.SetRawBytes(merged, "groundingSupports", translatorcommon.JoinRawArray(supports))
	}
	return gjson.ParseBytes(merged)
}

func responsesMergeCitations(existing, incoming [][]byte) [][]byte {
	merged := make([][]byte, 0, len(existing)+len(incoming))
	indices := make(map[string]int)
	for _, citation := range append(existing, incoming...) {
		key := fmt.Sprintf("%s:%d:%d", gjson.GetBytes(citation, "url").String(),
			gjson.GetBytes(citation, "start_index").Int(), gjson.GetBytes(citation, "end_index").Int())
		if index, ok := indices[key]; ok {
			if gjson.GetBytes(merged[index], "title").String() == "" && gjson.GetBytes(citation, "title").String() != "" {
				merged[index] = citation
			}
			continue
		}
		indices[key] = len(merged)
		merged = append(merged, citation)
	}
	return merged
}

func responsesWebSearchCallItem(id, query string, queries []string, sources [][]byte) []byte {
	item := []byte(`{"id":"","type":"web_search_call","status":"completed","action":{"type":"search","query":""}}`)
	item, _ = sjson.SetBytes(item, "id", id)
	item, _ = sjson.SetBytes(item, "action.query", query)
	if len(queries) > 0 {
		item, _ = sjson.SetBytes(item, "action.queries", queries)
	}
	if len(sources) > 0 {
		item, _ = sjson.SetRawBytes(item, "action.sources", translatorcommon.JoinRawArray(sources))
	}
	return item
}

type responsesPartMapping struct {
	partIndex int
	message   int
	startRune int64
	text      string
}

type responsesCitationRange struct {
	message int
	start   int64
	end     int64
}

func responsesByteRanges(mappings []responsesPartMapping, start, end int64) []responsesCitationRange {
	if start < 0 {
		start = 0
	}
	if start >= end {
		return nil
	}
	var offset int64
	var ranges []responsesCitationRange
	for _, mapping := range mappings {
		length := int64(len(mapping.text))
		spanStart, spanEnd := offset, offset+length
		offset = spanEnd
		if end <= spanStart || start >= spanEnd {
			continue
		}
		relStart := max(start, spanStart) - spanStart
		relEnd := min(end, spanEnd) - spanStart
		partStart := mapping.startRune + int64(utf8.RuneCountInString(mapping.text[:relStart]))
		partEnd := mapping.startRune + int64(utf8.RuneCountInString(mapping.text[:relEnd]))
		if partEnd <= partStart {
			continue
		}
		if n := len(ranges); n > 0 && ranges[n-1].message == mapping.message && ranges[n-1].end == partStart {
			ranges[n-1].end = partEnd
		} else {
			ranges = append(ranges, responsesCitationRange{mapping.message, partStart, partEnd})
		}
	}
	return ranges
}

func responsesCitationsByMessage(grounding gjson.Result, mappings []responsesPartMapping, messageTexts []string) map[int][][]byte {
	chunks := grounding.Get("groundingChunks").Array()
	if len(chunks) == 0 {
		return nil
	}
	if len(mappings) == 0 && len(messageTexts) > 0 {
		mappings = []responsesPartMapping{{message: 0, text: messageTexts[0]}}
	}
	coalesced := make([]responsesPartMapping, 0, len(mappings))
	for _, mapping := range mappings {
		if n := len(coalesced); n > 0 && coalesced[n-1].partIndex == mapping.partIndex && coalesced[n-1].message == mapping.message {
			coalesced[n-1].text += mapping.text
		} else {
			coalesced = append(coalesced, mapping)
		}
	}
	citations := make(map[int][][]byte)
	seen := make(map[string]bool)
	for _, support := range grounding.Get("groundingSupports").Array() {
		segment := support.Get("segment")
		partMappings := coalesced
		if partIndex := segment.Get("partIndex"); partIndex.Exists() {
			partMappings = nil
			for _, mapping := range coalesced {
				if mapping.partIndex == int(partIndex.Int()) {
					partMappings = append(partMappings, mapping)
				}
			}
		}
		ranges := responsesByteRanges(partMappings, segment.Get("startIndex").Int(), segment.Get("endIndex").Int())
		for _, index := range support.Get("groundingChunkIndices").Array() {
			idx := int(index.Int())
			if idx < 0 || idx >= len(chunks) {
				continue
			}
			uri := strings.TrimSpace(chunks[idx].Get("web.uri").String())
			if uri == "" {
				continue
			}
			for _, r := range ranges {
				key := fmt.Sprintf("%d:%s:%d:%d", r.message, uri, r.start, r.end)
				if seen[key] {
					continue
				}
				seen[key] = true
				citation := []byte(`{"type":"url_citation","url":"","title":"","start_index":0,"end_index":0}`)
				citation, _ = sjson.SetBytes(citation, "url", uri)
				citation, _ = sjson.SetBytes(citation, "title", chunks[idx].Get("web.title").String())
				citation, _ = sjson.SetBytes(citation, "start_index", r.start)
				citation, _ = sjson.SetBytes(citation, "end_index", r.end)
				citations[r.message] = append(citations[r.message], citation)
			}
		}
	}
	return citations
}
