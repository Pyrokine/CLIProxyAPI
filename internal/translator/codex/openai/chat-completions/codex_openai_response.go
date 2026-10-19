// Package chat_completions provides response translation functionality for Codex to OpenAI API compatibility.
// This package handles the conversion of Codex API responses into OpenAI Chat Completions-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by OpenAI API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, reasoning content, and usage metadata appropriately.
package chat_completions

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

type toolCallStreamState struct {
	Index         int
	Arguments     strings.Builder
	ArgumentsDone bool
	Done          bool
}

func (state *toolCallStreamState) completeArguments(arguments string) (string, error) {
	emitted := state.Arguments.String()
	if !strings.HasPrefix(arguments, emitted) || (state.ArgumentsDone && arguments != emitted) {
		return "", fmt.Errorf("codex tool call %d conflicts with delivered input", state.Index)
	}
	tail := arguments[len(emitted):]
	state.Arguments.WriteString(tail)
	state.ArgumentsDone = true
	return tail, nil
}

// convertCliToOpenAIParams holds parameters for response conversion.
type convertCliToOpenAIParams struct {
	ServiceTier          string
	ResponseID           string
	CreatedAt            int64
	Model                string
	FunctionCallIndex    int
	TerminalEmitted      bool
	citationKeys         map[string]struct{}
	textPartOffsets      map[string]int64
	textPartBytes        map[string]int
	emittedTextRunes     int64
	toolCallStates       map[string]*toolCallStreamState
	currentToolCallState *toolCallStreamState
	imageResults         map[string]string
	responseError        error
}

func (params *convertCliToOpenAIParams) ResponseTranslationError() error {
	return params.responseError
}

// ConvertCodexResponseToOpenAI translates a single chunk of a streaming response from the
// Codex API format to the OpenAI Chat Completions streaming format.
// It processes various Codex event types and transforms them into OpenAI-compatible JSON responses.
// The function handles text content, tool calls, reasoning content, and usage metadata, outputting
// responses that match the OpenAI API format. It supports incremental updates for streaming responses.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of OpenAI-compatible JSON responses
func ConvertCodexResponseToOpenAI(
	_ context.Context,
	modelName string,
	originalRequestRawJSON, _, rawJSON []byte,
	param *any,
) [][]byte {
	if *param == nil {
		*param = &convertCliToOpenAIParams{
			Model:             modelName,
			CreatedAt:         0,
			ResponseID:        "",
			FunctionCallIndex: -1,
			toolCallStates:    make(map[string]*toolCallStreamState),
		}
	}

	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	// Initialize the OpenAI SSE template.
	template := []byte(`{"id":"","object":"chat.completion.chunk","created":12345,"model":"model","choices":[{"index":0,"delta":{},"finish_reason":null,"native_finish_reason":null}]}`)

	rootResult := gjson.ParseBytes(rawJSON)
	params := (*param).(*convertCliToOpenAIParams)
	if params.citationKeys == nil {
		params.citationKeys = make(map[string]struct{})
		params.textPartOffsets = make(map[string]int64)
	}
	if tier := codexResponseServiceTier(rootResult.Get("response")); tier != "" {
		params.ServiceTier = tier
	} else if tier := codexResponseServiceTier(rootResult); tier != "" {
		params.ServiceTier = tier
	}
	if params.ServiceTier != "" {
		template, _ = sjson.SetBytes(template, "service_tier", params.ServiceTier)
	}
	if params.textPartBytes == nil {
		params.textPartBytes = make(map[string]int)
	}
	emitText := func(event gjson.Result, contentIndex int64, text string) string {
		key := codexTextPartKey(event, contentIndex)
		if _, exists := params.textPartOffsets[key]; !exists {
			params.textPartOffsets[key] = params.emittedTextRunes
		}
		params.textPartBytes[key] += len(text)
		params.emittedTextRunes += int64(utf8.RuneCountInString(text))
		return text
	}
	completeText := func(event gjson.Result, contentIndex int64, text string) string {
		seen := params.textPartBytes[codexTextPartKey(event, contentIndex)]
		if seen >= len(text) {
			return ""
		}
		return emitText(event, contentIndex, text[seen:])
	}
	completeMessage := func(event, item gjson.Result) (string, [][]byte) {
		var text strings.Builder
		var citations [][]byte
		for index, part := range item.Get("content").Array() {
			if part.Get("type").String() != "output_text" {
				continue
			}
			text.WriteString(completeText(event, int64(index), part.Get("text").String()))
			offset := params.textPartOffsets[codexTextPartKey(event, int64(index))]
			citations = append(citations, codexChatURLCitations(part.Get("annotations"), offset, params.citationKeys)...)
		}
		return text.String(), citations
	}
	applyCompletedContent := func(text string, citations [][]byte) {
		if text != "" || len(citations) > 0 {
			template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
		}
		if text != "" {
			template, _ = sjson.SetBytes(template, "choices.0.delta.content", text)
		}
		if len(citations) > 0 {
			template, _ = sjson.SetRawBytes(template, "choices.0.delta.annotations", translatorcommon.JoinRawArray(citations))
		}
	}

	emitImage := func(event, item gjson.Result, field string) bool {
		image := codexChatImage(item, field)
		if len(image) == 0 {
			return false
		}
		url := gjson.GetBytes(image, "image_url.url").String()
		keys := toolCallStateKeys(event, item)
		duplicate := false
		for _, key := range keys {
			if params.imageResults[key] == url {
				duplicate = true
			}
		}
		if params.imageResults == nil {
			params.imageResults = make(map[string]string)
		}
		for _, key := range keys {
			params.imageResults[key] = url
		}
		if duplicate {
			return false
		}
		template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
		template, _ = sjson.SetRawBytes(template, "choices.0.delta.images.-1", image)
		return true
	}

	emitToolCall := func(event, item gjson.Result, completed bool) bool {
		state := findToolCallState(params, event, item)
		call := []byte(`{"index":0,"function":{"arguments":""}}`)
		emit := state == nil
		if state == nil {
			params.FunctionCallIndex++
			state = &toolCallStreamState{Index: params.FunctionCallIndex}
			call = []byte(`{"index":0,"id":"","type":"function","function":{"name":"","arguments":""}}`)
			call, _ = sjson.SetBytes(call, "id", item.Get("call_id").String())
			name := item.Get("name").String()
			if original, ok := buildReverseMapFromOriginalOpenAI(originalRequestRawJSON)[name]; ok {
				name = original
			}
			call, _ = sjson.SetBytes(call, "function.name", name)
			template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
		}
		registerToolCallState(params, event, item, state)
		if completed {
			tail, err := state.completeArguments(codexToolCallArguments(item))
			if err != nil {
				params.responseError = err
				return false
			}
			state.Done = true
			call, _ = sjson.SetBytes(call, "function.arguments", tail)
			emit = emit || tail != ""
		}
		if emit {
			call, _ = sjson.SetBytes(call, "index", state.Index)
			template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", call)
		}
		return emit
	}

	typeResult := rootResult.Get("type")
	dataType := typeResult.String()
	if params.TerminalEmitted || params.responseError != nil {
		return [][]byte{}
	}
	if dataType == "response.created" {
		(*param).(*convertCliToOpenAIParams).ResponseID = rootResult.Get("response.id").String()
		(*param).(*convertCliToOpenAIParams).CreatedAt = rootResult.Get("response.created_at").Int()
		(*param).(*convertCliToOpenAIParams).Model = rootResult.Get("response.model").String()
		return [][]byte{}
	}

	// Extract and set the model version.
	cachedModel := (*param).(*convertCliToOpenAIParams).Model
	if modelResult := gjson.GetBytes(rawJSON, "model"); modelResult.Exists() {
		template, _ = sjson.SetBytes(template, "model", modelResult.String())
	} else if cachedModel != "" {
		template, _ = sjson.SetBytes(template, "model", cachedModel)
	} else if modelName != "" {
		template, _ = sjson.SetBytes(template, "model", modelName)
	}

	template, _ = sjson.SetBytes(template, "created", (*param).(*convertCliToOpenAIParams).CreatedAt)

	// Extract and set the response ID.
	template, _ = sjson.SetBytes(template, "id", (*param).(*convertCliToOpenAIParams).ResponseID)

	// Extract and set usage metadata (token counts).
	if usageResult := gjson.GetBytes(rawJSON, "response.usage"); usageResult.Exists() {
		if outputTokensResult := usageResult.Get("output_tokens"); outputTokensResult.Exists() {
			template, _ = sjson.SetBytes(template, "usage.completion_tokens", outputTokensResult.Int())
		}
		if totalTokensResult := usageResult.Get("total_tokens"); totalTokensResult.Exists() {
			template, _ = sjson.SetBytes(template, "usage.total_tokens", totalTokensResult.Int())
		}
		if inputTokensResult := usageResult.Get("input_tokens"); inputTokensResult.Exists() {
			template, _ = sjson.SetBytes(template, "usage.prompt_tokens", inputTokensResult.Int())
		}
		if cachedTokensResult := usageResult.Get("input_tokens_details.cached_tokens"); cachedTokensResult.Exists() {
			template, _ = sjson.SetBytes(
				template, "usage.prompt_tokens_details.cached_tokens", cachedTokensResult.Int(),
			)
		}
		template = setCodexCacheWriteTokens(template, usageResult)
		if reasoningTokensResult := usageResult.Get("output_tokens_details.reasoning_tokens"); reasoningTokensResult.Exists() {
			template, _ = sjson.SetBytes(
				template, "usage.completion_tokens_details.reasoning_tokens", reasoningTokensResult.Int(),
			)
		}
	}

	if dataType == "response.reasoning_summary_text.delta" || dataType == "response.reasoning_text.delta" {
		if deltaResult := rootResult.Get("delta"); deltaResult.Exists() {
			template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
			template, _ = sjson.SetBytes(template, "choices.0.delta.reasoning_content", deltaResult.String())
		}
	} else if dataType == "response.reasoning_summary_text.done" || dataType == "response.reasoning_text.done" {
		template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
		template, _ = sjson.SetBytes(template, "choices.0.delta.reasoning_content", "\n\n")
	} else if dataType == "response.output_text.delta" {
		if deltaResult := rootResult.Get("delta"); deltaResult.Exists() {
			text := emitText(rootResult, rootResult.Get("content_index").Int(), deltaResult.String())
			template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
			template, _ = sjson.SetBytes(template, "choices.0.delta.content", text)
		}
	} else if dataType == "response.output_text.annotation.added" || dataType == "response.output_text.done" || dataType == "response.content_part.done" {
		text := ""
		if dataType == "response.output_text.done" {
			text = completeText(rootResult, rootResult.Get("content_index").Int(), rootResult.Get("text").String())
		} else if dataType == "response.content_part.done" && rootResult.Get("part.type").String() == "output_text" {
			text = completeText(rootResult, rootResult.Get("content_index").Int(), rootResult.Get("part.text").String())
		}
		citations := codexStreamURLCitations(rootResult, params)
		if text == "" && len(citations) == 0 {
			return nil
		}
		applyCompletedContent(text, citations)
	} else if dataType == "response.completed" || dataType == "response.done" || dataType == "response.incomplete" {
		var text strings.Builder
		var citations [][]byte
		for index, item := range rootResult.Get("response.output").Array() {
			event := gjson.Parse(`{"output_index":` + strconv.Itoa(index) + `}`)
			if isCodexToolCallType(item.Get("type").String()) {
				emitToolCall(event, item, true)
				if params.responseError != nil {
					return nil
				}
				continue
			}
			if item.Get("type").String() == "image_generation_call" {
				emitImage(event, item, "result")
				continue
			}
			if item.Get("type").String() != "message" {
				continue
			}
			tail, itemCitations := completeMessage(event, item)
			text.WriteString(tail)
			citations = append(citations, itemCitations...)
		}
		applyCompletedContent(text.String(), citations)
		params.TerminalEmitted = true
		finishReason, nativeFinishReason := codexOpenAIFinishReason(
			dataType,
			rootResult.Get("response"),
			params.FunctionCallIndex != -1,
		)
		template, _ = sjson.SetBytes(template, "choices.0.finish_reason", finishReason)
		template, _ = sjson.SetBytes(template, "choices.0.native_finish_reason", nativeFinishReason)
	} else if dataType == "response.output_item.added" {
		itemResult := rootResult.Get("item")
		if !itemResult.Exists() || !isCodexToolCallType(itemResult.Get("type").String()) {
			return [][]byte{}
		}

		if !emitToolCall(rootResult, itemResult, false) {
			return nil
		}

	} else if dataType == "response.function_call_arguments.delta" || dataType == "response.custom_tool_call_input.delta" {
		p := (*param).(*convertCliToOpenAIParams)
		state := findToolCallState(p, rootResult, gjson.Result{})
		deltaValue := rootResult.Get("delta").String()
		if state == nil || state.Done || state.ArgumentsDone || deltaValue == "" {
			return [][]byte{}
		}
		state.Arguments.WriteString(deltaValue)

		functionCallItemTemplate := []byte(`{"index":0,"function":{"arguments":""}}`)
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "index", state.Index)
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "function.arguments", deltaValue)

		template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
		template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", functionCallItemTemplate)

	} else if dataType == "response.function_call_arguments.done" || dataType == "response.custom_tool_call_input.done" {
		p := (*param).(*convertCliToOpenAIParams)
		state := findToolCallState(p, rootResult, gjson.Result{})
		if state == nil {
			return [][]byte{}
		}
		tail, err := state.completeArguments(rootResult.Get(codexToolCallArgumentsField(dataType)).String())
		if err != nil {
			params.responseError = err
			return nil
		}
		if tail == "" {
			return nil
		}

		functionCallItemTemplate := []byte(`{"index":0,"function":{"arguments":""}}`)
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "index", state.Index)
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "function.arguments", tail)

		template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
		template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", functionCallItemTemplate)

	} else if dataType == "response.image_generation_call.partial_image" {
		if !emitImage(rootResult, rootResult, "partial_image_b64") {
			return nil
		}
	} else if dataType == "response.output_item.done" {
		itemResult := rootResult.Get("item")
		if itemResult.Get("type").String() == "image_generation_call" {
			if !emitImage(rootResult, itemResult, "result") {
				return nil
			}
			return [][]byte{template}
		}
		if itemResult.Get("type").String() == "message" {
			text, citations := completeMessage(rootResult, itemResult)
			citations = append(citations, codexStreamURLCitations(rootResult, params)...)
			if text == "" && len(citations) == 0 {
				return nil
			}
			applyCompletedContent(text, citations)
			return [][]byte{template}
		}
		if !itemResult.Exists() || !isCodexToolCallType(itemResult.Get("type").String()) {
			return [][]byte{}
		}

		if !emitToolCall(rootResult, itemResult, true) {
			return nil
		}

	} else {
		return [][]byte{}
	}

	return [][]byte{template}
}

// ConvertCodexResponseToOpenAINonStream converts a non-streaming Codex response to a non-streaming OpenAI response.
// This function processes the complete Codex response and transforms it into a single OpenAI-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the OpenAI API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - []byte: An OpenAI-compatible JSON response containing all message content and metadata
func ConvertCodexResponseToOpenAINonStream(
	_ context.Context,
	_ string,
	originalRequestRawJSON, _, rawJSON []byte,
	_ *any,
) []byte {
	rootResult := gjson.ParseBytes(rawJSON)
	responseType := rootResult.Get("type").String()
	if responseType != "response.completed" && responseType != "response.incomplete" {
		return []byte{}
	}

	unixTimestamp := time.Now().Unix()

	responseResult := rootResult.Get("response")

	template := []byte(`{"id":"","object":"chat.completion","created":123456,"model":"model","choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null,"native_finish_reason":null}]}`)
	if tier := codexResponseServiceTier(responseResult); tier != "" {
		template, _ = sjson.SetBytes(template, "service_tier", tier)
	} else if tier := codexResponseServiceTier(rootResult); tier != "" {
		template, _ = sjson.SetBytes(template, "service_tier", tier)
	}

	// Extract and set the model version.
	if modelResult := responseResult.Get("model"); modelResult.Exists() {
		template, _ = sjson.SetBytes(template, "model", modelResult.String())
	}

	// Extract and set the creation timestamp.
	if createdAtResult := responseResult.Get("created_at"); createdAtResult.Exists() {
		template, _ = sjson.SetBytes(template, "created", createdAtResult.Int())
	} else {
		template, _ = sjson.SetBytes(template, "created", unixTimestamp)
	}

	// Extract and set the response ID.
	if idResult := responseResult.Get("id"); idResult.Exists() {
		template, _ = sjson.SetBytes(template, "id", idResult.String())
	}

	// Extract and set usage metadata (token counts).
	if usageResult := responseResult.Get("usage"); usageResult.Exists() {
		if outputTokensResult := usageResult.Get("output_tokens"); outputTokensResult.Exists() {
			template, _ = sjson.SetBytes(template, "usage.completion_tokens", outputTokensResult.Int())
		}
		if totalTokensResult := usageResult.Get("total_tokens"); totalTokensResult.Exists() {
			template, _ = sjson.SetBytes(template, "usage.total_tokens", totalTokensResult.Int())
		}
		if inputTokensResult := usageResult.Get("input_tokens"); inputTokensResult.Exists() {
			template, _ = sjson.SetBytes(template, "usage.prompt_tokens", inputTokensResult.Int())
		}
		if cachedTokensResult := usageResult.Get("input_tokens_details.cached_tokens"); cachedTokensResult.Exists() {
			template, _ = sjson.SetBytes(
				template, "usage.prompt_tokens_details.cached_tokens", cachedTokensResult.Int(),
			)
		}
		template = setCodexCacheWriteTokens(template, usageResult)
		if reasoningTokensResult := usageResult.Get("output_tokens_details.reasoning_tokens"); reasoningTokensResult.Exists() {
			template, _ = sjson.SetBytes(
				template, "usage.completion_tokens_details.reasoning_tokens", reasoningTokensResult.Int(),
			)
		}
	}

	// Process the output array for content and function calls
	var toolCalls [][]byte
	outputResult := responseResult.Get("output")
	if outputResult.IsArray() {
		outputArray := outputResult.Array()
		var contentText strings.Builder
		var reasoningText strings.Builder
		var contentRuneOffset int64
		var messageAnnotations [][]byte
		annotationKeys := make(map[string]struct{})

		for _, outputItem := range outputArray {
			outputType := outputItem.Get("type").String()

			switch outputType {
			case "reasoning":
				// Preserve summary text first, then full reasoning text from the same item.
				if summaryResult := outputItem.Get("summary"); summaryResult.IsArray() {
					for _, summaryItem := range summaryResult.Array() {
						if summaryItem.Get("type").String() == "summary_text" {
							if text := summaryItem.Get("text").String(); text != "" {
								reasoningText.WriteString(text)
							}
						}
					}
				}
				if contentResult := outputItem.Get("content"); contentResult.IsArray() {
					for _, contentItem := range contentResult.Array() {
						if contentItem.Get("type").String() == "reasoning_text" {
							if text := contentItem.Get("text").String(); text != "" {
								reasoningText.WriteString(text)
							}
						}
					}
				}
			case "message":
				// Extract message content
				if contentResult := outputItem.Get("content"); contentResult.IsArray() {
					contentArray := contentResult.Array()
					for _, contentItem := range contentArray {
						if contentItem.Get("type").String() == "output_text" {
							text := contentItem.Get("text").String()
							contentText.WriteString(text)
							messageAnnotations = append(messageAnnotations, codexChatURLCitations(contentItem.Get("annotations"), contentRuneOffset, annotationKeys)...)
							contentRuneOffset += int64(utf8.RuneCountInString(text))
						}
					}
				}
			case "image_generation_call":
				if image := codexChatImage(outputItem, "result"); len(image) > 0 {
					template, _ = sjson.SetRawBytes(template, "choices.0.message.images.-1", image)
				}
			case "function_call", "custom_tool_call":
				// Handle function and custom tool call content.
				functionCallTemplate := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)

				if callIdResult := outputItem.Get("call_id"); callIdResult.Exists() {
					functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "id", callIdResult.String())
				}

				if nameResult := outputItem.Get("name"); nameResult.Exists() {
					n := nameResult.String()
					rev := buildReverseMapFromOriginalOpenAI(originalRequestRawJSON)
					if orig, ok := rev[n]; ok {
						n = orig
					}
					functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "function.name", n)
				}

				functionCallTemplate, _ = sjson.SetBytes(
					functionCallTemplate,
					"function.arguments",
					codexToolCallArguments(outputItem),
				)

				toolCalls = append(toolCalls, functionCallTemplate)
			}
		}

		// Set content and reasoning content if found
		if contentText.String() != "" {
			template, _ = sjson.SetBytes(template, "choices.0.message.content", contentText.String())
		}

		if reasoningText.String() != "" {
			template, _ = sjson.SetBytes(template, "choices.0.message.reasoning_content", reasoningText.String())
		}

		if len(messageAnnotations) > 0 {
			template, _ = sjson.SetRawBytes(template, "choices.0.message.annotations", translatorcommon.JoinRawArray(messageAnnotations))
		}

		// Add tool calls if any
		if len(toolCalls) > 0 {
			template, _ = sjson.SetRawBytes(template, "choices.0.message.tool_calls", []byte(`[]`))
			for _, toolCall := range toolCalls {
				template, _ = sjson.SetRawBytes(template, "choices.0.message.tool_calls.-1", toolCall)
			}
		}
	}

	finishReason, nativeFinishReason := codexOpenAIFinishReason(
		responseType,
		responseResult,
		len(toolCalls) > 0,
	)
	template, _ = sjson.SetBytes(template, "choices.0.finish_reason", finishReason)
	template, _ = sjson.SetBytes(template, "choices.0.native_finish_reason", nativeFinishReason)

	return template
}

func registerToolCallState(
	params *convertCliToOpenAIParams,
	eventResult gjson.Result,
	itemResult gjson.Result,
	state *toolCallStreamState,
) {
	if params.toolCallStates == nil {
		params.toolCallStates = make(map[string]*toolCallStreamState)
	}
	for _, key := range toolCallStateKeys(eventResult, itemResult) {
		params.toolCallStates[key] = state
	}
	params.currentToolCallState = state
}

func findToolCallState(
	params *convertCliToOpenAIParams,
	eventResult gjson.Result,
	itemResult gjson.Result,
) *toolCallStreamState {
	keys := toolCallStateKeys(eventResult, itemResult)
	for _, key := range keys {
		if state := params.toolCallStates[key]; state != nil {
			return state
		}
	}
	if len(keys) > 0 {
		return nil
	}
	return params.currentToolCallState
}

func toolCallStateKeys(eventResult gjson.Result, itemResult gjson.Result) []string {
	keys := make([]string, 0, 5)
	if itemID := eventResult.Get("item_id").String(); itemID != "" {
		keys = append(keys, "item:"+itemID)
	}
	if callID := eventResult.Get("call_id").String(); callID != "" {
		keys = append(keys, "call:"+callID)
	}
	if itemID := itemResult.Get("id").String(); itemID != "" {
		keys = append(keys, "item:"+itemID)
	}
	if callID := itemResult.Get("call_id").String(); callID != "" {
		keys = append(keys, "call:"+callID)
	}
	if outputIndex := eventResult.Get("output_index"); outputIndex.Exists() {
		keys = append(keys, "output:"+outputIndex.Raw)
	}
	return keys
}

func isCodexToolCallType(itemType string) bool {
	return itemType == "function_call" || itemType == "custom_tool_call"
}

func codexToolCallArguments(itemResult gjson.Result) string {
	if itemResult.Get("type").String() == "custom_tool_call" {
		return itemResult.Get("input").String()
	}
	return itemResult.Get("arguments").String()
}

func codexToolCallArgumentsField(eventType string) string {
	if eventType == "response.custom_tool_call_input.done" {
		return "input"
	}
	return "arguments"
}

func codexOpenAIFinishReason(responseType string, response gjson.Result, hasToolCalls bool) (string, string) {
	if responseType == "response.incomplete" || response.Get("status").String() == "incomplete" {
		native := response.Get("incomplete_details.reason").String()
		switch native {
		case "max_tokens", "max_output_tokens":
			return "length", native
		case "content_filter":
			return "content_filter", native
		default:
			return "stop", native
		}
	}
	if hasToolCalls {
		return "tool_calls", "tool_calls"
	}
	return "stop", "stop"
}

// buildReverseMapFromOriginalOpenAI builds a map of shortened tool name -> original tool name
// from the original OpenAI-style request JSON using the same shortening logic.
func buildReverseMapFromOriginalOpenAI(original []byte) map[string]string {
	reverseMap := map[string]string{}
	for originalName, shortName := range buildShortNameMap(original) {
		reverseMap[shortName] = originalName
	}
	return reverseMap
}

func codexChatImage(item gjson.Result, field string) []byte {
	data := item.Get(field)
	if data.Type != gjson.String || strings.TrimSpace(data.String()) == "" {
		return nil
	}
	format := strings.ToLower(strings.TrimSpace(item.Get("output_format").String()))
	switch format {
	case "":
		format = "png"
	case "jpg":
		format = "jpeg"
	case "png", "jpeg", "webp":
	default:
		log.WithField("output_format", format).Warn("Ignoring unsupported Codex image format")
		return nil
	}
	image := []byte(`{"type":"image_url","image_url":{"url":""}}`)
	image, _ = sjson.SetBytes(image, "image_url.url", "data:image/"+format+";base64,"+strings.TrimSpace(data.String()))
	return image
}

func codexResponseServiceTier(response gjson.Result) string {
	tier := response.Get("service_tier")
	if tier.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(tier.String())
}

func setCodexCacheWriteTokens(template []byte, usage gjson.Result) []byte {
	value := usage.Get("input_tokens_details.cache_write_tokens")
	if !value.Exists() || value.Type == gjson.Null {
		return template
	}

	valid := value.Type == gjson.Number && value.Raw != ""
	for _, digit := range value.Raw {
		if digit < '0' || digit > '9' {
			valid = false
			break
		}
	}
	if !valid {
		log.WithField(
			"field",
			"usage.input_tokens_details.cache_write_tokens",
		).Warn("Ignoring invalid Codex cache write token count")
		return template
	}

	template, _ = sjson.SetRawBytes(
		template,
		"usage.prompt_tokens_details.cache_write_tokens",
		[]byte(value.Raw),
	)
	template, _ = sjson.SetRawBytes(
		template,
		"usage.prompt_tokens_details.cached_creation_tokens",
		[]byte(value.Raw),
	)
	return template
}
