// Package chat_completions provides response translation functionality for Codex to OpenAI API compatibility.
// This package handles the conversion of Codex API responses into OpenAI Chat Completions-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by OpenAI API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, reasoning content, and usage metadata appropriately.
package chat_completions

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

type toolCallStreamState struct {
	Index            int
	ArgumentsEmitted bool
	Done             bool
}

// convertCliToOpenAIParams holds parameters for response conversion.
type convertCliToOpenAIParams struct {
	ResponseID           string
	CreatedAt            int64
	Model                string
	FunctionCallIndex    int
	TerminalEmitted      bool
	toolCallStates       map[string]*toolCallStreamState
	currentToolCallState *toolCallStreamState
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
	template := []byte(`{"id":"","object":"chat.completion.chunk","created":12345,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null,"native_finish_reason":null}]}`)

	rootResult := gjson.ParseBytes(rawJSON)
	params := (*param).(*convertCliToOpenAIParams)

	typeResult := rootResult.Get("type")
	dataType := typeResult.String()
	if params.TerminalEmitted {
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
		if cacheWriteTokensResult := usageResult.Get("input_tokens_details.cache_write_tokens"); cacheWriteTokensResult.Exists() {
			template, _ = sjson.SetBytes(
				template, "usage.prompt_tokens_details.cached_creation_tokens", cacheWriteTokensResult.Int(),
			)
		}
		if reasoningTokensResult := usageResult.Get("output_tokens_details.reasoning_tokens"); reasoningTokensResult.Exists() {
			template, _ = sjson.SetBytes(
				template, "usage.completion_tokens_details.reasoning_tokens", reasoningTokensResult.Int(),
			)
		}
	}

	if dataType == "response.reasoning_summary_text.delta" {
		if deltaResult := rootResult.Get("delta"); deltaResult.Exists() {
			template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
			template, _ = sjson.SetBytes(template, "choices.0.delta.reasoning_content", deltaResult.String())
		}
	} else if dataType == "response.reasoning_summary_text.done" {
		template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
		template, _ = sjson.SetBytes(template, "choices.0.delta.reasoning_content", "\n\n")
	} else if dataType == "response.output_text.delta" {
		if deltaResult := rootResult.Get("delta"); deltaResult.Exists() {
			template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
			template, _ = sjson.SetBytes(template, "choices.0.delta.content", deltaResult.String())
		}
	} else if dataType == "response.completed" || dataType == "response.done" || dataType == "response.incomplete" {
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

		p := (*param).(*convertCliToOpenAIParams)
		p.FunctionCallIndex++
		state := &toolCallStreamState{Index: p.FunctionCallIndex}
		registerToolCallState(p, rootResult, itemResult, state)

		functionCallItemTemplate := []byte(`{"index":0,"id":"","type":"function","function":{"name":"","arguments":""}}`)
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "index", state.Index)
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "id", itemResult.Get("call_id").String())

		name := itemResult.Get("name").String()
		rev := buildReverseMapFromOriginalOpenAI(originalRequestRawJSON)
		if orig, ok := rev[name]; ok {
			name = orig
		}
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "function.name", name)

		template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
		template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
		template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", functionCallItemTemplate)

	} else if dataType == "response.function_call_arguments.delta" || dataType == "response.custom_tool_call_input.delta" {
		p := (*param).(*convertCliToOpenAIParams)
		state := findToolCallState(p, rootResult, gjson.Result{})
		deltaValue := rootResult.Get("delta").String()
		if state == nil || state.Done || deltaValue == "" {
			return [][]byte{}
		}
		state.ArgumentsEmitted = true

		functionCallItemTemplate := []byte(`{"index":0,"function":{"arguments":""}}`)
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "index", state.Index)
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "function.arguments", deltaValue)

		template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
		template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", functionCallItemTemplate)

	} else if dataType == "response.function_call_arguments.done" || dataType == "response.custom_tool_call_input.done" {
		p := (*param).(*convertCliToOpenAIParams)
		state := findToolCallState(p, rootResult, gjson.Result{})
		if state == nil || state.Done || state.ArgumentsEmitted {
			return [][]byte{}
		}
		fullArgs := rootResult.Get(codexToolCallArgumentsField(dataType)).String()
		if fullArgs == "" {
			return [][]byte{}
		}
		state.ArgumentsEmitted = true

		functionCallItemTemplate := []byte(`{"index":0,"function":{"arguments":""}}`)
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "index", state.Index)
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "function.arguments", fullArgs)

		template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
		template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", functionCallItemTemplate)

	} else if dataType == "response.output_item.done" {
		itemResult := rootResult.Get("item")
		if !itemResult.Exists() || !isCodexToolCallType(itemResult.Get("type").String()) {
			return [][]byte{}
		}

		p := (*param).(*convertCliToOpenAIParams)
		state := findToolCallState(p, rootResult, itemResult)
		if state != nil {
			if state.Done {
				return [][]byte{}
			}
			state.Done = true
			if state.ArgumentsEmitted {
				return [][]byte{}
			}
			fullArgs := codexToolCallArguments(itemResult)
			if fullArgs == "" {
				return [][]byte{}
			}
			state.ArgumentsEmitted = true
			functionCallItemTemplate := []byte(`{"index":0,"function":{"arguments":""}}`)
			functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "index", state.Index)
			functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "function.arguments", fullArgs)
			template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
			template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", functionCallItemTemplate)
			return [][]byte{template}
		}

		p.FunctionCallIndex++
		state = &toolCallStreamState{Index: p.FunctionCallIndex, ArgumentsEmitted: true, Done: true}
		registerToolCallState(p, rootResult, itemResult, state)

		functionCallItemTemplate := []byte(`{"index":0,"id":"","type":"function","function":{"name":"","arguments":""}}`)
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "index", state.Index)
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "id", itemResult.Get("call_id").String())

		name := itemResult.Get("name").String()
		rev := buildReverseMapFromOriginalOpenAI(originalRequestRawJSON)
		if orig, ok := rev[name]; ok {
			name = orig
		}
		functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "function.name", name)
		functionCallItemTemplate, _ = sjson.SetBytes(
			functionCallItemTemplate,
			"function.arguments",
			codexToolCallArguments(itemResult),
		)
		template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
		template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
		template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", functionCallItemTemplate)

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
		if cacheWriteTokensResult := usageResult.Get("input_tokens_details.cache_write_tokens"); cacheWriteTokensResult.Exists() {
			template, _ = sjson.SetBytes(
				template, "usage.prompt_tokens_details.cached_creation_tokens", cacheWriteTokensResult.Int(),
			)
		}
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

		for _, outputItem := range outputArray {
			outputType := outputItem.Get("type").String()

			switch outputType {
			case "reasoning":
				// Extract reasoning content from summary
				if summaryResult := outputItem.Get("summary"); summaryResult.IsArray() {
					summaryArray := summaryResult.Array()
					for _, summaryItem := range summaryArray {
						if summaryItem.Get("type").String() == "summary_text" {
							if text := summaryItem.Get("text").String(); text != "" {
								reasoningText.WriteString(text)
							}
							break
						}
					}
				}
			case "message":
				// Extract message content
				if contentResult := outputItem.Get("content"); contentResult.IsArray() {
					contentArray := contentResult.Array()
					for _, contentItem := range contentArray {
						if contentItem.Get("type").String() == "output_text" {
							if text := contentItem.Get("text").String(); text != "" {
								contentText.WriteString(text)
							}
							break
						}
					}
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
	tools := gjson.GetBytes(original, "tools")
	reverseMap := map[string]string{}
	if tools.IsArray() && len(tools.Array()) > 0 {
		var names []string
		seenNames := map[string]struct{}{}
		for _, tool := range tools.Array() {
			var name string
			switch tool.Get("type").String() {
			case "function":
				name = tool.Get("function.name").String()
			case "custom":
				name = tool.Get("name").String()
			}
			if name != "" {
				if _, seen := seenNames[name]; !seen {
					names = append(names, name)
					seenNames[name] = struct{}{}
				}
			}
		}
		if len(names) > 0 {
			for originalName, shortName := range buildShortNameMap(names) {
				reverseMap[shortName] = originalName
			}
		}
	}
	return reverseMap
}
