// Package claude provides response translation functionality for OpenAI to Anthropic API.
// This package handles the conversion of OpenAI Chat Completions API responses into Anthropic API-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by Anthropic API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, and usage metadata appropriately.
package claude

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

// convertOpenAIResponseToAnthropicParams holds parameters for response conversion
type convertOpenAIResponseToAnthropicParams struct {
	MessageID   string
	Model       string
	CreatedAt   int64
	ToolNameMap map[string]string
	SawToolCall bool
	// Content accumulator for streaming
	ContentAccumulator strings.Builder
	// Tool calls accumulator for streaming
	ToolCallsAccumulator map[int]*toolCallAccumulator
	// Track if text content block has been started
	TextContentBlockStarted bool
	// Track if thinking content block has been started
	ThinkingContentBlockStarted bool
	// Track finish reason for later use
	FinishReason string
	// Track if content blocks have been stopped
	ContentBlocksStopped bool
	// Track if message_delta has been sent
	MessageDeltaSent bool
	// Track if message_start has been sent
	MessageStarted bool
	// Track if message_stop has been sent
	MessageStopSent bool
	// Tool call content block index mapping
	ToolCallBlockIndexes map[int]int
	// Index assigned to text content block
	TextContentBlockIndex int
	// Index assigned to thinking content block
	ThinkingContentBlockIndex int
	// Next available content block index
	NextContentBlockIndex int
	// Currently open tool call index (-1 if none)
	OpenToolCallIndex int
	// Content that arrived while a tool block was active
	InterleavedContentChunks []interleavedContentChunk
	// Usage cached independently from finish_reason
	UsageInputTokens      int64
	UsageOutputTokens     int64
	UsageCachedTokens     int64
	UsageCacheWriteTokens int64
}

type interleavedContentChunk struct {
	Type string
	Text string
}

// toolCallAccumulator holds the state for accumulating tool call data
type toolCallAccumulator struct {
	ID           string
	Name         string
	Arguments    strings.Builder
	StartEmitted bool
}

// convertOpenAIResponseToClaude converts OpenAI streaming response format to Anthropic API format.
// This function processes OpenAI streaming chunks and transforms them into Anthropic-compatible JSON responses.
// It handles text content, tool calls, and usage metadata, outputting responses that match the Anthropic API format.
//
// Parameters:
//   - ctx: The context for the request.
//   - modelName: The name of the model.
//   - rawJSON: The raw JSON response from the OpenAI API.
//   - param: A pointer to a parameter object for the conversion.
//
// Returns:
//   - [][]byte: A slice of byte chunks, each containing an Anthropic-compatible JSON response.
func convertOpenAIResponseToClaude(
	_ context.Context,
	_ string,
	originalRequestRawJSON, _, rawJSON []byte,
	param *any,
) [][]byte {
	if *param == nil {
		*param = &convertOpenAIResponseToAnthropicParams{
			MessageID:                   "",
			Model:                       "",
			CreatedAt:                   0,
			ToolNameMap:                 nil,
			SawToolCall:                 false,
			ContentAccumulator:          strings.Builder{},
			ToolCallsAccumulator:        nil,
			TextContentBlockStarted:     false,
			ThinkingContentBlockStarted: false,
			FinishReason:                "",
			ContentBlocksStopped:        false,
			MessageDeltaSent:            false,
			ToolCallBlockIndexes:        make(map[int]int),
			TextContentBlockIndex:       -1,
			ThinkingContentBlockIndex:   -1,
			NextContentBlockIndex:       0,
			OpenToolCallIndex:           -1,
		}
	}

	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	if (*param).(*convertOpenAIResponseToAnthropicParams).ToolNameMap == nil {
		(*param).(*convertOpenAIResponseToAnthropicParams).ToolNameMap = util.ToolNameMapFromClaudeRequest(originalRequestRawJSON)
	}

	// Check if this is the [DONE] marker
	if bytes.Equal(bytes.TrimSpace(rawJSON), []byte("[DONE]")) {
		return convertOpenAIDoneToAnthropic((*param).(*convertOpenAIResponseToAnthropicParams))
	}

	streamResult := gjson.GetBytes(originalRequestRawJSON, "stream")
	if !streamResult.Exists() || (streamResult.Exists() && streamResult.Type == gjson.False) {
		return convertOpenAINonStreamingToAnthropic(rawJSON)
	}
	return convertOpenAIStreamingChunkToAnthropic(rawJSON, (*param).(*convertOpenAIResponseToAnthropicParams))
}

func effectiveOpenAIFinishReason(param *convertOpenAIResponseToAnthropicParams) string {
	if param == nil {
		return ""
	}
	if param.FinishReason == "length" || param.FinishReason == "content_filter" {
		return param.FinishReason
	}
	if param.SawToolCall {
		for _, call := range param.ToolCallsAccumulator {
			if call == nil || call.Arguments.Len() == 0 {
				continue
			}
			arguments := util.FixJSON(strings.TrimSpace(call.Arguments.String()))
			if !gjson.Valid(arguments) || !gjson.Parse(arguments).IsObject() {
				return "length"
			}
		}
		return "tool_calls"
	}
	return param.FinishReason
}

func terminalOpenAIFinishReason(param *convertOpenAIResponseToAnthropicParams) string {
	if reason := effectiveOpenAIFinishReason(param); reason != "" {
		return reason
	}
	return "stop"
}

// convertOpenAIStreamingChunkToAnthropic converts OpenAI streaming chunk to Anthropic streaming events
func convertOpenAIStreamingChunkToAnthropic(
	rawJSON []byte,
	param *convertOpenAIResponseToAnthropicParams,
) [][]byte {
	root := gjson.ParseBytes(rawJSON)
	var results [][]byte

	if param.MessageID == "" {
		param.MessageID = root.Get("id").String()
	}
	if param.Model == "" {
		param.Model = root.Get("model").String()
	}
	if param.CreatedAt == 0 {
		param.CreatedAt = root.Get("created").Int()
	}

	if delta := root.Get("choices.0.delta"); delta.Exists() {
		if !param.MessageStarted {
			messageStart := []byte(`{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`)
			messageStart, _ = sjson.SetBytes(messageStart, "message.id", param.MessageID)
			messageStart, _ = sjson.SetBytes(messageStart, "message.model", param.Model)
			results = append(results, translatorcommon.AppendSSEEventBytes(nil, "message_start", messageStart, 2))
			param.MessageStarted = true
		}

		if reasoning := delta; reasoning.Exists() {
			for _, text := range collectOpenAIObjectReasoningTexts(reasoning) {
				if text == "" {
					continue
				}
				if param.OpenToolCallIndex != -1 {
					appendInterleavedContent(param, "thinking", text)
					continue
				}
				stopTextContentBlock(param, &results)
				if !param.ThinkingContentBlockStarted {
					if param.ThinkingContentBlockIndex == -1 {
						param.ThinkingContentBlockIndex = param.NextContentBlockIndex
						param.NextContentBlockIndex++
					}
					startEvent := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
					startEvent, _ = sjson.SetBytes(startEvent, "index", param.ThinkingContentBlockIndex)
					results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_start", startEvent, 2))
					param.ThinkingContentBlockStarted = true
				}
				deltaEvent := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`)
				deltaEvent, _ = sjson.SetBytes(deltaEvent, "index", param.ThinkingContentBlockIndex)
				deltaEvent, _ = sjson.SetBytes(deltaEvent, "delta.thinking", text)
				results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", deltaEvent, 2))
			}
		}

		if content := delta.Get("content"); content.Exists() && content.String() != "" {
			if param.OpenToolCallIndex != -1 {
				appendInterleavedContent(param, "text", content.String())
				param.ContentAccumulator.WriteString(content.String())
			} else {
				if !param.TextContentBlockStarted {
					stopThinkingContentBlock(param, &results)
					if param.TextContentBlockIndex == -1 {
						param.TextContentBlockIndex = param.NextContentBlockIndex
						param.NextContentBlockIndex++
					}
					startEvent := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
					startEvent, _ = sjson.SetBytes(startEvent, "index", param.TextContentBlockIndex)
					results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_start", startEvent, 2))
					param.TextContentBlockStarted = true
				}
				deltaEvent := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`)
				deltaEvent, _ = sjson.SetBytes(deltaEvent, "index", param.TextContentBlockIndex)
				deltaEvent, _ = sjson.SetBytes(deltaEvent, "delta.text", content.String())
				results = append(results, translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", deltaEvent, 2))
				param.ContentAccumulator.WriteString(content.String())
			}
		}

		if toolCalls := delta.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() {
			if param.ToolCallsAccumulator == nil {
				param.ToolCallsAccumulator = make(map[int]*toolCallAccumulator)
			}
			toolCalls.ForEach(func(arrayIndex, toolCall gjson.Result) bool {
				index := int(toolCall.Get("index").Int())
				if !toolCall.Get("index").Exists() {
					index = int(arrayIndex.Int())
				}
				accumulator := param.ToolCallsAccumulator[index]
				if accumulator == nil {
					accumulator = &toolCallAccumulator{}
					param.ToolCallsAccumulator[index] = accumulator
				}
				if id := toolCall.Get("id"); id.Type == gjson.String && id.String() != "" {
					accumulator.ID = id.String()
				}
				if function := toolCall.Get("function"); function.Exists() {
					if !accumulator.StartEmitted {
						if name := function.Get("name"); name.Type == gjson.String && name.String() != "" {
							accumulator.Name = util.MapToolName(param.ToolNameMap, name.String())
						}
					}
					if arguments := function.Get("arguments"); arguments.Exists() && arguments.String() != "" {
						accumulator.Arguments.WriteString(arguments.String())
					}
				}
				if !accumulator.StartEmitted && accumulator.Name != "" && accumulator.ID != "" &&
					!param.ContentBlocksStopped && param.OpenToolCallIndex == -1 {
					emitToolUseStart(param, index, accumulator, &results)
				}
				return true
			})
		}
	}

	if finishReason := root.Get("choices.0.finish_reason"); finishReason.Exists() && finishReason.String() != "" {
		reason := finishReason.String()
		switch {
		case reason == "tool_calls" && !param.SawToolCall:
			param.FinishReason = "stop"
		default:
			param.FinishReason = reason
		}
		finalizeOpenAIAnthropicContentBlocks(param, &results)
	}

	usage := root.Get("usage")
	hasUsage := usage.Exists() && usage.Type != gjson.Null
	if hasUsage {
		param.UsageInputTokens, param.UsageOutputTokens, param.UsageCachedTokens, param.UsageCacheWriteTokens =
			extractOpenAIUsage(usage)
	}
	trailingUsage := hasUsage && !root.Get("choices.0").Exists() &&
		(param.FinishReason != "" || param.SawToolCall || param.TextContentBlockStarted ||
			param.ThinkingContentBlockStarted || param.ContentAccumulator.Len() > 0 ||
			len(param.InterleavedContentChunks) > 0)
	if !param.MessageDeltaSent && trailingUsage {
		finalizeOpenAIAnthropicContentBlocks(param, &results)
		emitAnthropicMessageDelta(param, &results)
		emitMessageStopIfNeeded(param, &results)
	}
	return results
}

// convertOpenAIDoneToAnthropic handles the [DONE] marker and sends final events.
func convertOpenAIDoneToAnthropic(param *convertOpenAIResponseToAnthropicParams) [][]byte {
	var results [][]byte
	finalizeOpenAIAnthropicContentBlocks(param, &results)
	if !param.MessageDeltaSent {
		emitAnthropicMessageDelta(param, &results)
	}
	emitMessageStopIfNeeded(param, &results)
	return results
}

// convertOpenAINonStreamingToAnthropic converts OpenAI non-streaming response to Anthropic format
func convertOpenAINonStreamingToAnthropic(rawJSON []byte) [][]byte {
	root := gjson.ParseBytes(rawJSON)

	out := []byte(`{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	out, _ = sjson.SetBytes(out, "id", root.Get("id").String())
	out, _ = sjson.SetBytes(out, "model", root.Get("model").String())

	// Process message content and tool calls
	if choices := root.Get("choices"); choices.Exists() && choices.IsArray() && len(choices.Array()) > 0 {
		choice := choices.Array()[0] // Take first choice

		for _, reasoningText := range collectOpenAIObjectReasoningTexts(choice.Get("message")) {
			if reasoningText == "" {
				continue
			}
			block := []byte(`{"type":"thinking","thinking":""}`)
			block, _ = sjson.SetBytes(block, "thinking", reasoningText)
			out, _ = sjson.SetRawBytes(out, "content.-1", block)
		}

		// Handle text content
		if content := choice.Get("message.content"); content.Exists() && content.String() != "" {
			block := []byte(`{"type":"text","text":""}`)
			block, _ = sjson.SetBytes(block, "text", content.String())
			out, _ = sjson.SetRawBytes(out, "content.-1", block)
		}

		// Handle tool calls
		if toolCalls := choice.Get("message.tool_calls"); toolCalls.Exists() && toolCalls.IsArray() {
			toolCalls.ForEach(
				func(_, toolCall gjson.Result) bool {
					toolUseBlock := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
					toolUseBlock, _ = sjson.SetBytes(
						toolUseBlock, "id", util.SanitizeClaudeToolID(toolCall.Get("id").String()),
					)
					toolUseBlock, _ = sjson.SetBytes(toolUseBlock, "name", toolCall.Get("function.name").String())

					argsStr := util.FixJSON(toolCall.Get("function.arguments").String())
					if argsStr != "" && gjson.Valid(argsStr) {
						argsJSON := gjson.Parse(argsStr)
						if argsJSON.IsObject() {
							toolUseBlock, _ = sjson.SetRawBytes(toolUseBlock, "input", []byte(argsJSON.Raw))
						} else {
							toolUseBlock, _ = sjson.SetRawBytes(toolUseBlock, "input", []byte(`{}`))
						}
					} else {
						toolUseBlock, _ = sjson.SetRawBytes(toolUseBlock, "input", []byte(`{}`))
					}

					out, _ = sjson.SetRawBytes(out, "content.-1", toolUseBlock)
					return true
				},
			)
		}

		// Set stop reason
		if finishReason := choice.Get("finish_reason"); finishReason.Exists() {
			out, _ = sjson.SetBytes(out, "stop_reason", mapOpenAIFinishReasonToAnthropic(finishReason.String()))
		}
	}

	// Set usage information
	if usage := root.Get("usage"); usage.Exists() {
		inputTokens, outputTokens, cachedTokens, cacheWriteTokens := extractOpenAIUsage(usage)
		out, _ = sjson.SetBytes(out, "usage.input_tokens", inputTokens)
		out, _ = sjson.SetBytes(out, "usage.output_tokens", outputTokens)
		if cachedTokens > 0 {
			out, _ = sjson.SetBytes(out, "usage.cache_read_input_tokens", cachedTokens)
		}
		if cacheWriteTokens > 0 {
			out, _ = sjson.SetBytes(out, "usage.cache_creation_input_tokens", cacheWriteTokens)
		}
	}

	return [][]byte{out}
}

// mapOpenAIFinishReasonToAnthropic maps OpenAI finish reasons to Anthropic equivalents
func mapOpenAIFinishReasonToAnthropic(openAIReason string) string {
	switch openAIReason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "end_turn" // Anthropic doesn't have direct equivalent
	case "function_call": // Legacy OpenAI
		return "tool_use"
	default:
		return "end_turn"
	}
}

func (p *convertOpenAIResponseToAnthropicParams) toolContentBlockIndex(openAIToolIndex int) int {
	if idx, ok := p.ToolCallBlockIndexes[openAIToolIndex]; ok {
		return idx
	}
	idx := p.NextContentBlockIndex
	p.NextContentBlockIndex++
	p.ToolCallBlockIndexes[openAIToolIndex] = idx
	return idx
}

func appendInterleavedContent(param *convertOpenAIResponseToAnthropicParams, contentType, text string) {
	if text == "" {
		return
	}
	if count := len(param.InterleavedContentChunks); count > 0 &&
		param.InterleavedContentChunks[count-1].Type == contentType {
		param.InterleavedContentChunks[count-1].Text += text
		return
	}
	param.InterleavedContentChunks = append(param.InterleavedContentChunks, interleavedContentChunk{
		Type: contentType,
		Text: text,
	})
}

func emitToolUseStart(
	param *convertOpenAIResponseToAnthropicParams,
	openAIToolIndex int,
	accumulator *toolCallAccumulator,
	results *[][]byte,
) {
	stopThinkingContentBlock(param, results)
	stopTextContentBlock(param, results)

	blockIndex := param.toolContentBlockIndex(openAIToolIndex)
	startEvent := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`)
	startEvent, _ = sjson.SetBytes(startEvent, "index", blockIndex)
	startEvent, _ = sjson.SetBytes(startEvent, "content_block.id", util.SanitizeClaudeToolID(accumulator.ID))
	startEvent, _ = sjson.SetBytes(startEvent, "content_block.name", accumulator.Name)
	*results = append(*results, translatorcommon.AppendSSEEventBytes(nil, "content_block_start", startEvent, 2))
	accumulator.StartEmitted = true
	param.SawToolCall = true
	param.OpenToolCallIndex = openAIToolIndex
}

func emitBelatedToolUseStart(
	param *convertOpenAIResponseToAnthropicParams,
	openAIToolIndex int,
	accumulator *toolCallAccumulator,
	results *[][]byte,
) bool {
	if accumulator == nil {
		return false
	}
	if accumulator.StartEmitted {
		return true
	}
	if accumulator.Name == "" && accumulator.ID == "" && accumulator.Arguments.Len() == 0 {
		return false
	}
	if accumulator.Name == "" {
		accumulator.Name = fmt.Sprintf("tool_%d", openAIToolIndex)
	}
	emitToolUseStart(param, openAIToolIndex, accumulator, results)
	return true
}

func finalizeSingleToolCall(
	param *convertOpenAIResponseToAnthropicParams,
	openAIToolIndex int,
	results *[][]byte,
) {
	accumulator := param.ToolCallsAccumulator[openAIToolIndex]
	if accumulator == nil {
		return
	}
	if !accumulator.StartEmitted && !emitBelatedToolUseStart(param, openAIToolIndex, accumulator, results) {
		return
	}
	blockIndex := param.toolContentBlockIndex(openAIToolIndex)
	if accumulator.Arguments.Len() > 0 {
		deltaEvent := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
		deltaEvent, _ = sjson.SetBytes(deltaEvent, "index", blockIndex)
		deltaEvent, _ = sjson.SetBytes(deltaEvent, "delta.partial_json", util.FixJSON(accumulator.Arguments.String()))
		*results = append(*results, translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", deltaEvent, 2))
	}
	stopEvent := []byte(`{"type":"content_block_stop","index":0}`)
	stopEvent, _ = sjson.SetBytes(stopEvent, "index", blockIndex)
	*results = append(*results, translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", stopEvent, 2))
	delete(param.ToolCallBlockIndexes, openAIToolIndex)
	param.OpenToolCallIndex = -1
}

func emitBufferedInterleavedContent(param *convertOpenAIResponseToAnthropicParams, results *[][]byte) {
	for _, chunk := range param.InterleavedContentChunks {
		if chunk.Text == "" {
			continue
		}
		index := param.NextContentBlockIndex
		param.NextContentBlockIndex++
		var blockType, deltaType, valuePath string
		switch chunk.Type {
		case "thinking":
			blockType = "thinking"
			deltaType = "thinking_delta"
			valuePath = "delta.thinking"
		case "text":
			blockType = "text"
			deltaType = "text_delta"
			valuePath = "delta.text"
		default:
			continue
		}
		startEvent := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"","text":""}}`)
		startEvent, _ = sjson.SetBytes(startEvent, "index", index)
		startEvent, _ = sjson.SetBytes(startEvent, "content_block.type", blockType)
		if blockType == "thinking" {
			startEvent, _ = sjson.DeleteBytes(startEvent, "content_block.text")
			startEvent, _ = sjson.SetBytes(startEvent, "content_block.thinking", "")
		}
		*results = append(*results, translatorcommon.AppendSSEEventBytes(nil, "content_block_start", startEvent, 2))
		deltaEvent := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":""}}`)
		deltaEvent, _ = sjson.SetBytes(deltaEvent, "index", index)
		deltaEvent, _ = sjson.SetBytes(deltaEvent, "delta.type", deltaType)
		deltaEvent, _ = sjson.SetBytes(deltaEvent, valuePath, chunk.Text)
		*results = append(*results, translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", deltaEvent, 2))
		stopEvent := []byte(`{"type":"content_block_stop","index":0}`)
		stopEvent, _ = sjson.SetBytes(stopEvent, "index", index)
		*results = append(*results, translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", stopEvent, 2))
	}
	param.InterleavedContentChunks = nil
}

func finalizeOpenAIAnthropicContentBlocks(
	param *convertOpenAIResponseToAnthropicParams,
	results *[][]byte,
) {
	if param == nil {
		return
	}
	stopThinkingContentBlock(param, results)
	stopTextContentBlock(param, results)
	if param.ContentBlocksStopped {
		return
	}
	if param.OpenToolCallIndex != -1 {
		finalizeSingleToolCall(param, param.OpenToolCallIndex, results)
	}
	for _, index := range toolCallAccumulatorIndexes(param.ToolCallsAccumulator) {
		accumulator := param.ToolCallsAccumulator[index]
		if accumulator == nil || accumulator.StartEmitted {
			continue
		}
		finalizeSingleToolCall(param, index, results)
	}
	param.ContentBlocksStopped = true
	emitBufferedInterleavedContent(param, results)
}

func emitAnthropicMessageDelta(
	param *convertOpenAIResponseToAnthropicParams,
	results *[][]byte,
) {
	if param == nil || param.MessageDeltaSent {
		return
	}
	messageDelta := []byte(`{"type":"message_delta","delta":{"stop_reason":"","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`)
	messageDelta, _ = sjson.SetBytes(
		messageDelta,
		"delta.stop_reason",
		mapOpenAIFinishReasonToAnthropic(terminalOpenAIFinishReason(param)),
	)
	messageDelta, _ = sjson.SetBytes(messageDelta, "usage.input_tokens", param.UsageInputTokens)
	messageDelta, _ = sjson.SetBytes(messageDelta, "usage.output_tokens", param.UsageOutputTokens)
	if param.UsageCachedTokens > 0 {
		messageDelta, _ = sjson.SetBytes(messageDelta, "usage.cache_read_input_tokens", param.UsageCachedTokens)
	}
	if param.UsageCacheWriteTokens > 0 {
		messageDelta, _ = sjson.SetBytes(
			messageDelta,
			"usage.cache_creation_input_tokens",
			param.UsageCacheWriteTokens,
		)
	}
	*results = append(*results, translatorcommon.AppendSSEEventBytes(nil, "message_delta", messageDelta, 2))
	param.MessageDeltaSent = true
}

func toolCallAccumulatorIndexes(accumulators map[int]*toolCallAccumulator) []int {
	indexes := make([]int, 0, len(accumulators))
	for index := range accumulators {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return indexes
}

func collectOpenAIObjectReasoningTexts(node gjson.Result) []string {
	for _, field := range []string{"reasoning_content", "reasoning", "reasoning_text", "reasoning_details"} {
		if texts := collectOpenAIReasoningTexts(node.Get(field)); len(texts) > 0 {
			return texts
		}
	}
	return nil
}

func collectOpenAIReasoningTexts(node gjson.Result) []string {
	var texts []string
	if !node.Exists() {
		return texts
	}

	if node.IsArray() {
		node.ForEach(
			func(_, value gjson.Result) bool {
				texts = append(texts, collectOpenAIReasoningTexts(value)...)
				return true
			},
		)
		return texts
	}

	// noinspection GoSwitchMissingCasesForIotaConsts — only String and JSON carry extractable text.
	switch node.Type {
	case gjson.String:
		if text := node.String(); text != "" {
			texts = append(texts, text)
		}
	case gjson.JSON:
		if text := node.Get("text"); text.Exists() {
			if textStr := text.String(); textStr != "" {
				texts = append(texts, textStr)
			}
		} else if raw := node.Raw; raw != "" && !strings.HasPrefix(raw, "{") && !strings.HasPrefix(raw, "[") {
			texts = append(texts, raw)
		}
	}

	return texts
}

func stopThinkingContentBlock(param *convertOpenAIResponseToAnthropicParams, results *[][]byte) {
	if !param.ThinkingContentBlockStarted {
		return
	}
	contentBlockStopJSON := []byte(`{"type":"content_block_stop","index":0}`)
	contentBlockStopJSON, _ = sjson.SetBytes(contentBlockStopJSON, "index", param.ThinkingContentBlockIndex)
	*results = append(
		*results, translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", contentBlockStopJSON, 2),
	)
	param.ThinkingContentBlockStarted = false
	param.ThinkingContentBlockIndex = -1
}

func emitMessageStopIfNeeded(param *convertOpenAIResponseToAnthropicParams, results *[][]byte) {
	if param.MessageStopSent {
		return
	}
	*results = append(
		*results, translatorcommon.AppendSSEEventBytes(nil, "message_stop", []byte(`{"type":"message_stop"}`), 2),
	)
	param.MessageStopSent = true
}

func stopTextContentBlock(param *convertOpenAIResponseToAnthropicParams, results *[][]byte) {
	if !param.TextContentBlockStarted {
		return
	}
	contentBlockStopJSON := []byte(`{"type":"content_block_stop","index":0}`)
	contentBlockStopJSON, _ = sjson.SetBytes(contentBlockStopJSON, "index", param.TextContentBlockIndex)
	*results = append(
		*results, translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", contentBlockStopJSON, 2),
	)
	param.TextContentBlockStarted = false
	param.TextContentBlockIndex = -1
}

// convertOpenAIResponseToClaudeNonStream converts a non-streaming OpenAI response to a non-streaming Anthropic response.
//
// Parameters:
//   - ctx: The context for the request.
//   - modelName: The name of the model.
//   - rawJSON: The raw JSON response from the OpenAI API.
//   - param: A pointer to a parameter object for the conversion.
//
// Returns:
//   - []byte: An Anthropic-compatible JSON response.
func convertOpenAIResponseToClaudeNonStream(
	_ context.Context,
	_ string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	_ *any,
) []byte {
	_ = requestRawJSON

	root := gjson.ParseBytes(rawJSON)
	toolNameMap := util.ToolNameMapFromClaudeRequest(originalRequestRawJSON)
	out := []byte(`{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	out, _ = sjson.SetBytes(out, "id", root.Get("id").String())
	out, _ = sjson.SetBytes(out, "model", root.Get("model").String())

	hasToolCall := false
	stopReasonSet := false

	if choices := root.Get("choices"); choices.Exists() && choices.IsArray() && len(choices.Array()) > 0 {
		choice := choices.Array()[0]

		if finishReason := choice.Get("finish_reason"); finishReason.Exists() {
			out, _ = sjson.SetBytes(out, "stop_reason", mapOpenAIFinishReasonToAnthropic(finishReason.String()))
			stopReasonSet = true
		}

		if message := choice.Get("message"); message.Exists() {
			if contentResult := message.Get("content"); contentResult.Exists() {
				if contentResult.IsArray() {
					var textBuilder strings.Builder
					var thinkingBuilder strings.Builder

					flushText := func() {
						if textBuilder.Len() == 0 {
							return
						}
						block := []byte(`{"type":"text","text":""}`)
						block, _ = sjson.SetBytes(block, "text", textBuilder.String())
						out, _ = sjson.SetRawBytes(out, "content.-1", block)
						textBuilder.Reset()
					}

					flushThinking := func() {
						if thinkingBuilder.Len() == 0 {
							return
						}
						block := []byte(`{"type":"thinking","thinking":""}`)
						block, _ = sjson.SetBytes(block, "thinking", thinkingBuilder.String())
						out, _ = sjson.SetRawBytes(out, "content.-1", block)
						thinkingBuilder.Reset()
					}

					for _, item := range contentResult.Array() {
						switch item.Get("type").String() {
						case "text":
							flushThinking()
							textBuilder.WriteString(item.Get("text").String())
						case "tool_calls":
							flushThinking()
							flushText()
							toolCalls := item.Get("tool_calls")
							if toolCalls.IsArray() {
								toolCalls.ForEach(
									func(_, tc gjson.Result) bool {
										hasToolCall = true
										toolUse := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
										toolUse, _ = sjson.SetBytes(
											toolUse, "id", util.SanitizeClaudeToolID(tc.Get("id").String()),
										)
										toolUse, _ = sjson.SetBytes(
											toolUse, "name",
											util.MapToolName(toolNameMap, tc.Get("function.name").String()),
										)

										argsStr := util.FixJSON(tc.Get("function.arguments").String())
										if argsStr != "" && gjson.Valid(argsStr) {
											argsJSON := gjson.Parse(argsStr)
											if argsJSON.IsObject() {
												toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte(argsJSON.Raw))
											} else {
												toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte(`{}`))
											}
										} else {
											toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte(`{}`))
										}

										out, _ = sjson.SetRawBytes(out, "content.-1", toolUse)
										return true
									},
								)
							}
						case "reasoning":
							flushText()
							if thinking := item.Get("text"); thinking.Exists() {
								thinkingBuilder.WriteString(thinking.String())
							}
						default:
							flushThinking()
							flushText()
						}
					}

					flushThinking()
					flushText()
				} else if contentResult.Type == gjson.String {
					textContent := contentResult.String()
					if textContent != "" {
						block := []byte(`{"type":"text","text":""}`)
						block, _ = sjson.SetBytes(block, "text", textContent)
						out, _ = sjson.SetRawBytes(out, "content.-1", block)
					}
				}
			}

			for _, reasoningText := range collectOpenAIObjectReasoningTexts(message) {
				if reasoningText == "" {
					continue
				}
				block := []byte(`{"type":"thinking","thinking":""}`)
				block, _ = sjson.SetBytes(block, "thinking", reasoningText)
				out, _ = sjson.SetRawBytes(out, "content.-1", block)
			}

			if toolCalls := message.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() {
				toolCalls.ForEach(
					func(_, toolCall gjson.Result) bool {
						hasToolCall = true
						toolUseBlock := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
						toolUseBlock, _ = sjson.SetBytes(
							toolUseBlock, "id", util.SanitizeClaudeToolID(toolCall.Get("id").String()),
						)
						toolUseBlock, _ = sjson.SetBytes(
							toolUseBlock, "name", util.MapToolName(toolNameMap, toolCall.Get("function.name").String()),
						)

						argsStr := util.FixJSON(toolCall.Get("function.arguments").String())
						if argsStr != "" && gjson.Valid(argsStr) {
							argsJSON := gjson.Parse(argsStr)
							if argsJSON.IsObject() {
								toolUseBlock, _ = sjson.SetRawBytes(toolUseBlock, "input", []byte(argsJSON.Raw))
							} else {
								toolUseBlock, _ = sjson.SetRawBytes(toolUseBlock, "input", []byte(`{}`))
							}
						} else {
							toolUseBlock, _ = sjson.SetRawBytes(toolUseBlock, "input", []byte(`{}`))
						}

						out, _ = sjson.SetRawBytes(out, "content.-1", toolUseBlock)
						return true
					},
				)
			}
		}
	}

	if respUsage := root.Get("usage"); respUsage.Exists() {
		inputTokens, outputTokens, cachedTokens, cacheWriteTokens := extractOpenAIUsage(respUsage)
		out, _ = sjson.SetBytes(out, "usage.input_tokens", inputTokens)
		out, _ = sjson.SetBytes(out, "usage.output_tokens", outputTokens)
		if cachedTokens > 0 {
			out, _ = sjson.SetBytes(out, "usage.cache_read_input_tokens", cachedTokens)
		}
		if cacheWriteTokens > 0 {
			out, _ = sjson.SetBytes(out, "usage.cache_creation_input_tokens", cacheWriteTokens)
		}
	}

	if !stopReasonSet {
		if hasToolCall {
			out, _ = sjson.SetBytes(out, "stop_reason", "tool_use")
		} else {
			out, _ = sjson.SetBytes(out, "stop_reason", "end_turn")
		}
	}

	return out
}

func tokenCount(_ context.Context, count int64) []byte {
	return translatorcommon.ClaudeInputTokensJSON(count)
}

func extractOpenAIUsage(usage gjson.Result) (int64, int64, int64, int64) {
	if !usage.Exists() || usage.Type == gjson.Null {
		return 0, 0, 0, 0
	}

	inputTokens := usage.Get("prompt_tokens").Int()
	outputTokens := usage.Get("completion_tokens").Int()
	cachedTokens := usage.Get("prompt_tokens_details.cached_tokens").Int()
	cacheWriteTokens := usage.Get("prompt_tokens_details.cache_write_tokens").Int()
	if cacheWriteTokens <= 0 {
		cacheWriteTokens = usage.Get("prompt_tokens_details.cache_creation_tokens").Int()
	}

	inputTokens = max(int64(0), inputTokens)
	for _, deduction := range []int64{cachedTokens, cacheWriteTokens} {
		if deduction > 0 {
			inputTokens = max(int64(0), inputTokens-deduction)
		}
	}

	return inputTokens, outputTokens, cachedTokens, cacheWriteTokens
}
