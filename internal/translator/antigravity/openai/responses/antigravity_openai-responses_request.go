package responses

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	sigcompat "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/thinking"
	antigravgemini "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/antigravity/gemini"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	geminiresp "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai/responses"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const antigravityWebSearchSystemInstruction = "You are a search engine bot. You will be given a query from a user. Your task is to search the web for relevant information that will help the user. You MUST perform a web search. Do not respond or interact with the user, please respond as if they typed the query into a search bar."

func antigravitySupportsNativeResponsesWebSearch(model string, modelInfo *registry.ModelInfo) bool {
	if modelInfo != nil && modelInfo.NativeCapabilities != nil && modelInfo.NativeCapabilities.WebSearch != nil {
		return *modelInfo.NativeCapabilities.WebSearch
	}
	model = strings.TrimSpace(thinking.ParseSuffix(strings.TrimSpace(model)).ModelName)
	if model == "" {
		return false
	}
	for _, info := range registry.GetGlobalRegistry().GetAvailableModelsByProvider("antigravity") {
		if info == nil || !strings.EqualFold(strings.TrimSpace(thinking.ParseSuffix(info.ID).ModelName), model) {
			continue
		}
		if info.NativeCapabilities != nil && info.NativeCapabilities.WebSearch != nil && !*info.NativeCapabilities.WebSearch {
			return false
		}
		return info.SupportsWebSearch
	}
	return false
}

func convertOpenAIResponsesRequestToAntigravity(modelName string, inputRawJSON []byte, stream bool) []byte {
	out, _ := convertOpenAIResponsesRequestToAntigravityChecked(modelName, inputRawJSON, stream, nil)
	return out
}

func convertOpenAIResponsesRequestToAntigravityChecked(
	modelName string, inputRawJSON []byte, stream bool, modelInfo *registry.ModelInfo,
) ([]byte, error) {
	tools := gjson.GetBytes(inputRawJSON, "tools").Array()
	nativeWebSearch := len(tools) > 0 && antigravitySupportsNativeResponsesWebSearch(modelName, modelInfo)
	for _, tool := range tools {
		switch strings.ToLower(strings.TrimSpace(tool.Get("type").String())) {
		case "web_search", "web_search_2025_08_26", "web_search_preview", "web_search_preview_2025_03_11":
		default:
			nativeWebSearch = false
		}
	}
	rawJSON, outputsByCallID := normalizeOpenAIResponsesBridgeRequest(inputRawJSON)
	geminiRequest, err := geminiresp.ConvertOpenAIResponsesRequestToGeminiChecked(modelName, rawJSON, stream, &nativeWebSearch)
	if err != nil {
		return nil, err
	}
	geminiRequest = restoreOpenAIResponsesToolOutputs(geminiRequest, outputsByCallID)
	geminiRequest = restoreOpenAIResponsesClaudeReasoning(geminiRequest, rawJSON, modelName)
	out := antigravgemini.ConvertGeminiRequestToAntigravity(modelName, geminiRequest, stream)
	if search := gjson.GetBytes(geminiRequest, "tools.0.googleSearch"); nativeWebSearch && search.Exists() {
		out, err = applyAntigravityResponsesWebSearch(out, search)
		if err != nil {
			return nil, err
		}
	}
	summaryConfig := thinking.ExtractSummaryConfig(inputRawJSON, "openai-response")
	return thinking.ApplySummaryConfig(out, "antigravity", summaryConfig), nil
}

func restoreOpenAIResponsesClaudeReasoning(geminiRequest, request []byte, model string) []byte {
	if sigcompat.ProviderFromModelName(model) != sigcompat.ProviderClaude {
		return geminiRequest
	}
	signatures := make(map[string][]string)
	items := gjson.GetBytes(request, "input").Array()
	for index, item := range items {
		if item.Get("type").String() != "reasoning" {
			continue
		}
		text := item.Get("summary.0.text").String()
		if text == "" {
			continue
		}
		rawSignature := item.Get("encrypted_content").String()
		if strings.TrimSpace(rawSignature) == "" && index+1 < len(items) {
			next := items[index+1]
			if next.Get("type").String() == "reasoning" && strings.Contains(next.Get("id").String(), "_detached_after_") &&
				strings.TrimSpace(next.Get("summary.0.text").String()) == "" {
				rawSignature = next.Get("encrypted_content").String()
			}
		}
		signature, compatible := sigcompat.CompatibleAntigravityClaudeThinkingSignature(rawSignature)
		if !compatible {
			signature = ""
		}
		signatures[text] = append(signatures[text], signature)
	}
	for contentIndex, content := range gjson.GetBytes(geminiRequest, "contents").Array() {
		if content.Get("role").String() != "model" {
			continue
		}
		for partIndex, part := range content.Get("parts").Array() {
			if !part.Get("thought").Bool() {
				continue
			}
			text := part.Get("text").String()
			queue := signatures[text]
			if len(queue) == 0 {
				continue
			}
			signatures[text] = queue[1:]
			if queue[0] != "" {
				path := fmt.Sprintf("contents.%d.parts.%d.thoughtSignature", contentIndex, partIndex)
				geminiRequest, _ = sjson.SetBytes(geminiRequest, path, queue[0])
			}
		}
	}
	return geminiRequest
}

func applyAntigravityResponsesWebSearch(payload []byte, search gjson.Result) ([]byte, error) {
	tool, err := sjson.SetRawBytes([]byte(`{"googleSearch":{}}`), "googleSearch", []byte(search.Raw))
	if err != nil {
		return nil, err
	}
	tool, err = sjson.SetBytes(tool, "googleSearch.enhancedContent.imageSearch.maxResultCount", 5)
	if err != nil {
		return nil, err
	}
	payload, err = sjson.SetBytes(payload, "requestType", "web_search")
	if err != nil {
		return nil, err
	}
	payload, err = sjson.SetRawBytes(payload, "request.tools", translatorcommon.JoinRawArray([][]byte{tool}))
	if err != nil {
		return nil, err
	}
	instruction := gjson.GetBytes(payload, "request.systemInstruction")
	for _, part := range instruction.Get("parts").Array() {
		if part.Get("text").String() == antigravityWebSearchSystemInstruction {
			return payload, nil
		}
	}
	if !instruction.Exists() {
		payload, err = sjson.SetBytes(payload, "request.systemInstruction.role", "user")
		if err != nil {
			return nil, err
		}
	}
	return sjson.SetBytes(payload, "request.systemInstruction.parts.-1", map[string]string{"text": antigravityWebSearchSystemInstruction})
}

func normalizeOpenAIResponsesBridgeRequest(inputRawJSON []byte) ([]byte, map[string]gjson.Result) {
	if len(inputRawJSON) == 0 || !gjson.ValidBytes(inputRawJSON) {
		return inputRawJSON, nil
	}
	input := gjson.GetBytes(inputRawJSON, "input")
	if !input.IsArray() {
		return inputRawJSON, nil
	}

	items := translatorcommon.NormalizeResponsesToolCallOutputs(input.Array())
	items = translatorcommon.DeduplicateToolResultsByRawID(
		items,
		func(item gjson.Result) (string, bool) {
			if !isOpenAIResponsesToolOutput(item.Get("type").String()) {
				return "", false
			}
			return translatorcommon.ExtractResponsesCallID(item), true
		},
	)
	items = reorderOpenAIResponsesToolOutputSlots(items)

	callIDs := make(map[string]struct{})
	for _, item := range items {
		if isOpenAIResponsesToolCall(item.Get("type").String()) {
			if callID := translatorcommon.ExtractResponsesCallID(item); callID != "" {
				callIDs[callID] = struct{}{}
			}
		}
	}
	itemJSONs := make([][]byte, 0, len(items))
	outputsByCallID := make(map[string]gjson.Result)
	for _, item := range items {
		itemJSON := []byte(item.Raw)
		normalizedItem := gjson.ParseBytes(itemJSON)
		if isOpenAIResponsesToolOutput(normalizedItem.Get("type").String()) {
			if callID := translatorcommon.ExtractResponsesCallID(normalizedItem); callID != "" {
				outputsByCallID[callID] = normalizedItem.Get("output")
				if _, matched := callIDs[callID]; matched {
					itemJSON, _ = sjson.SetBytes(itemJSON, "output", "")
				}
			}
		}
		itemJSONs = append(itemJSONs, itemJSON)
	}

	rawJSON, err := sjson.SetRawBytes(inputRawJSON, "input", translatorcommon.JoinRawArray(itemJSONs))
	if err != nil {
		return inputRawJSON, outputsByCallID
	}
	return rawJSON, outputsByCallID
}

func isOpenAIResponsesToolOutput(itemType string) bool {
	return itemType == "function_call_output" || itemType == "custom_tool_call_output"
}

func isOpenAIResponsesToolCall(itemType string) bool {
	return itemType == "function_call" || itemType == "custom_tool_call"
}

func reorderOpenAIResponsesToolOutputSlots(items []gjson.Result) []gjson.Result {
	orderByCallID := make(map[string]int)
	for _, item := range items {
		if !isOpenAIResponsesToolCall(item.Get("type").String()) {
			continue
		}
		callID := translatorcommon.ExtractResponsesCallID(item)
		if callID == "" {
			continue
		}
		if _, exists := orderByCallID[callID]; !exists {
			orderByCallID[callID] = len(orderByCallID)
		}
	}
	if len(orderByCallID) < 2 {
		return items
	}

	type matchedOutput struct {
		item  gjson.Result
		order int
	}
	matchedSlots := make([]int, 0)
	matchedOutputs := make([]matchedOutput, 0)
	for index, item := range items {
		if !isOpenAIResponsesToolOutput(item.Get("type").String()) {
			continue
		}
		order, exists := orderByCallID[translatorcommon.ExtractResponsesCallID(item)]
		if !exists {
			continue
		}
		matchedSlots = append(matchedSlots, index)
		matchedOutputs = append(matchedOutputs, matchedOutput{item: item, order: order})
	}
	if len(matchedOutputs) < 2 {
		return items
	}
	sort.SliceStable(matchedOutputs, func(left, right int) bool {
		return matchedOutputs[left].order < matchedOutputs[right].order
	})

	ordered := append([]gjson.Result(nil), items...)
	for index, slot := range matchedSlots {
		ordered[slot] = matchedOutputs[index].item
	}
	return ordered
}

func restoreOpenAIResponsesToolOutputs(
	geminiRequest []byte,
	outputsByCallID map[string]gjson.Result,
) []byte {
	if len(outputsByCallID) == 0 {
		return geminiRequest
	}
	contents := gjson.GetBytes(geminiRequest, "contents")
	if !contents.IsArray() {
		return geminiRequest
	}

	contentItems := make([][]byte, 0, len(contents.Array()))
	for _, content := range contents.Array() {
		contentJSON := []byte(content.Raw)
		parts := content.Get("parts")
		if parts.IsArray() {
			partItems := make([][]byte, 0, len(parts.Array()))
			partsChanged := false
			for _, part := range parts.Array() {
				partJSON := []byte(part.Raw)
				functionResponse := part.Get("functionResponse")
				functionResponsePath := "functionResponse"
				if !functionResponse.Exists() {
					functionResponse = part.Get("function_response")
					functionResponsePath = "function_response"
				}
				if functionResponse.Exists() {
					callID := translatorcommon.GeminiToolCallID(functionResponse)
					if output, ok := outputsByCallID[callID]; ok {
						partJSON = setOpenAIResponsesFunctionOutput(partJSON, functionResponsePath, output)
						partsChanged = true
					}
				}
				partItems = append(partItems, partJSON)
			}
			if partsChanged {
				contentJSON, _ = sjson.SetRawBytes(contentJSON, "parts", translatorcommon.JoinRawArray(partItems))
			}
		}
		contentItems = append(contentItems, contentJSON)
	}

	updated, err := sjson.SetRawBytes(geminiRequest, "contents", translatorcommon.JoinRawArray(contentItems))
	if err != nil {
		return geminiRequest
	}
	return updated
}

type antigravityResponsesToolOutput struct {
	result    string
	resultRaw bool
	hasResult bool
	images    [][]byte
}

// The shared Gemini bridge accepts broader image data URLs and stringifies
// scalar outputs. Reapply the original output at the Antigravity boundary so
// only inline base64 image/* blocks become functionResponse parts.
func setOpenAIResponsesFunctionOutput(partJSON []byte, functionResponsePath string, output gjson.Result) []byte {
	translated := translateOpenAIResponsesToolOutput(output)
	resultPath := functionResponsePath + ".response.result"
	partsPath := functionResponsePath + ".parts"
	partJSON, _ = sjson.DeleteBytes(partJSON, resultPath)
	partJSON, _ = sjson.DeleteBytes(partJSON, partsPath)
	if translated.hasResult {
		if translated.resultRaw {
			partJSON, _ = sjson.SetRawBytes(partJSON, resultPath, []byte(translated.result))
		} else {
			partJSON, _ = sjson.SetBytes(partJSON, resultPath, translated.result)
		}
	}
	for _, image := range translated.images {
		partJSON, _ = sjson.SetRawBytes(partJSON, partsPath+".-1", image)
	}
	return partJSON
}

func translateOpenAIResponsesToolOutput(output gjson.Result) antigravityResponsesToolOutput {
	if output.Type == gjson.String {
		return antigravityResponsesToolOutput{result: output.String(), hasResult: true}
	}
	if output.IsArray() {
		return translateOpenAIResponsesArrayOutput(output)
	}
	if output.IsObject() {
		if image, ok := openAIResponsesImagePart(output); ok {
			return antigravityResponsesToolOutput{result: "", hasResult: true, images: [][]byte{image}}
		}
		return antigravityResponsesToolOutput{result: output.Raw, resultRaw: true, hasResult: true}
	}
	if output.Raw != "" {
		return antigravityResponsesToolOutput{result: output.Raw, resultRaw: true, hasResult: true}
	}
	return antigravityResponsesToolOutput{}
}

type antigravityResponsesOutputEntry struct {
	text   string
	raw    string
	isText bool
}

func translateOpenAIResponsesArrayOutput(output gjson.Result) antigravityResponsesToolOutput {
	entries := make([]antigravityResponsesOutputEntry, 0, len(output.Array()))
	images := make([][]byte, 0)
	hasContentBlock := false
	hasStructuredEntry := false
	for _, block := range output.Array() {
		if image, ok := openAIResponsesImagePart(block); ok {
			hasContentBlock = true
			images = append(images, image)
			continue
		}
		switch block.Get("type").String() {
		case "input_text", "output_text", "text":
			hasContentBlock = true
			entries = append(entries, antigravityResponsesOutputEntry{
				text: block.Get("text").String(), raw: block.Raw, isText: true,
			})
		case "":
			if block.Type == gjson.String {
				entries = append(entries, antigravityResponsesOutputEntry{
					text: block.String(), raw: block.Raw, isText: true,
				})
				continue
			}
			hasStructuredEntry = true
			entries = append(entries, antigravityResponsesOutputEntry{raw: block.Raw})
		default:
			hasStructuredEntry = true
			entries = append(entries, antigravityResponsesOutputEntry{raw: block.Raw})
		}
	}

	if !hasContentBlock {
		return antigravityResponsesToolOutput{result: output.Raw, resultRaw: true, hasResult: true}
	}
	if len(entries) == 0 {
		return antigravityResponsesToolOutput{result: "", hasResult: true, images: images}
	}
	if len(entries) == 1 {
		if entries[0].isText {
			return antigravityResponsesToolOutput{result: entries[0].text, hasResult: true, images: images}
		}
		return antigravityResponsesToolOutput{
			result: entries[0].raw, resultRaw: true, hasResult: true, images: images,
		}
	}
	if !hasStructuredEntry {
		texts := make([]string, 0, len(entries))
		for _, entry := range entries {
			texts = append(texts, entry.text)
		}
		return antigravityResponsesToolOutput{
			result: strings.Join(texts, "\n"), hasResult: true, images: images,
		}
	}

	rawEntries := make([][]byte, 0, len(entries))
	for _, entry := range entries {
		rawEntries = append(rawEntries, []byte(entry.raw))
	}
	return antigravityResponsesToolOutput{
		result: string(translatorcommon.JoinRawArray(rawEntries)), resultRaw: true, hasResult: true, images: images,
	}
}

func openAIResponsesImagePart(block gjson.Result) ([]byte, bool) {
	blockType := block.Get("type").String()
	if blockType != "input_image" && blockType != "image_url" && blockType != "image" {
		return nil, false
	}

	imageURL := block.Get("image_url.url").String()
	if imageURL == "" && block.Get("image_url").Type == gjson.String {
		imageURL = block.Get("image_url").String()
	}
	if imageURL == "" {
		imageURL = block.Get("url").String()
	}
	if imageURL != "" {
		mimeType, data, ok := translatorcommon.NormalizeOpenAIFileData("", "", imageURL)
		if ok && strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "image/") {
			return antigravityResponsesInlineDataPart(mimeType, data), true
		}
	}

	if block.Get("source.type").String() == "base64" {
		data := block.Get("source.data").String()
		if data == "" {
			return nil, false
		}
		mimeType := block.Get("source.media_type").String()
		if mimeType == "" {
			mimeType = "image/png"
		}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "image/") {
			return antigravityResponsesInlineDataPart(mimeType, data), true
		}
	}
	return nil, false
}

func antigravityResponsesInlineDataPart(mimeType, data string) []byte {
	part := []byte(`{"inlineData":{"mimeType":"","data":""}}`)
	part, _ = sjson.SetBytes(part, "inlineData.mimeType", mimeType)
	part, _ = sjson.SetBytes(part, "inlineData.data", data)
	return part
}
