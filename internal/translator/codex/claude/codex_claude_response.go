// Package claude provides response translation functionality for Codex to Claude Code API compatibility.
// This package handles the conversion of Codex API responses into Claude Code-compatible
// Server-Sent Events (SSE) format, implementing a sophisticated state machine that manages
// different response types including text content, thinking processes, and function calls.
// The translation ensures proper sequencing of SSE events and maintains state across
// multiple response chunks to provide a seamless streaming experience.
package claude

import (
	"bytes"
	"context"
	"slices"
	"strconv"
	"strings"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

const codexThinkingSummaryPartSeparator = "\n\n"

// convertCodexResponseToClaudeParams holds parameters for response conversion.
type convertCodexResponseToClaudeParams struct {
	HasToolCall              bool
	BlockIndex               int
	ThinkingBlockOpen        bool
	ThinkingBlockIndex       int
	ThinkingSignature        string
	PendingThinkingActive    bool
	PendingThinkingText      string
	PendingThinkingPartCount int
	TextBlocks               map[string]*codexClaudeTextBlock
	TextBlockOrder           []*codexClaudeTextBlock
	LastTextBlock            *codexClaudeTextBlock
	FunctionBlocks           map[string]*codexClaudeFunctionBlock
	FunctionBlockOrder       []*codexClaudeFunctionBlock
	FunctionBlockQueue       []*codexClaudeFunctionBlock
	ActiveFunctionBlock      *codexClaudeFunctionBlock
	LastFunctionBlock        *codexClaudeFunctionBlock
	DeferredStreamEvents     [][]byte
	WebSearchToolUseIDs      map[string]struct{}
	WebSearchToolResultIDs   map[string]struct{}
	LastWebSearchToolUseID   string
	MessageStarted           bool
	TerminalEmitted          bool
}

type codexClaudeTextBlock struct {
	Index    int
	Started  bool
	Stopped  bool
	HasDelta bool
}

type codexClaudeFunctionBlock struct {
	Index                  int
	CallID                 string
	Name                   string
	Arguments              string
	EmittedArgumentsLength int
	Started                bool
	Done                   bool
	Stopped                bool
}

// ConvertCodexResponseToClaude performs sophisticated streaming response format conversion.
// This function implements a complex state machine that translates Codex API responses
// into Claude Code-compatible Server-Sent Events (SSE) format. It manages different response types
// and handles state transitions between content blocks, thinking processes, and function calls.
//
// Response type states: 0=none, 1=content, 2=thinking, 3=function
// The function maintains state across multiple calls to ensure proper SSE event sequencing.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The fallback model name when response.created is missing or delayed
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of Claude Code-compatible JSON responses
func ConvertCodexResponseToClaude(
	_ context.Context,
	modelName string,
	originalRequestRawJSON, _, rawJSON []byte,
	param *any,
) [][]byte {
	if *param == nil {
		*param = &convertCodexResponseToClaudeParams{}
	}

	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	streamEventRawJSON := bytes.Clone(rawJSON)
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	output := make([]byte, 0, 512)
	rootResult := gjson.ParseBytes(rawJSON)
	params := (*param).(*convertCodexResponseToClaudeParams)
	if params.TerminalEmitted {
		return [][]byte{output}
	}

	eventType := rootResult.Get("type").String()
	if params.ActiveFunctionBlock != nil && shouldDeferCodexClaudeStreamEvent(eventType, rootResult) {
		params.DeferredStreamEvents = append(params.DeferredStreamEvents, streamEventRawJSON)
		return [][]byte{output}
	}
	if codexClaudeEventStartsMessage(eventType) {
		output = append(output, startCodexClaudeMessage(params, modelName, rootResult)...)
	}

	switch eventType {
	case "response.created":
	case "response.reasoning_summary_part.added":
		output = append(output, stopOpenCodexClaudeTextBlocks(params)...)
		if !params.PendingThinkingActive {
			params.PendingThinkingActive = true
		}
		if params.PendingThinkingPartCount > 0 {
			params.PendingThinkingText += codexThinkingSummaryPartSeparator
		}
		params.PendingThinkingPartCount++
	case "response.reasoning_summary_text.delta":
		output = append(output, stopOpenCodexClaudeTextBlocks(params)...)
		if !params.PendingThinkingActive {
			params.PendingThinkingActive = true
		}
		params.PendingThinkingText += rootResult.Get("delta").String()
	case "response.reasoning_summary_part.done":
	case "response.content_part.added":
		output = append(output, emitPendingCodexThinkingBlock(params)...)
		block := codexClaudeTextBlockForEvent(params, rootResult, gjson.Result{}, true)
		output = append(output, startCodexClaudeTextBlock(params, block)...)
	case "response.output_text.delta":
		block := codexClaudeTextBlockForEvent(params, rootResult, gjson.Result{}, true)
		output = append(output, startCodexClaudeTextBlock(params, block)...)
		output = append(output, appendCodexClaudeTextDelta(block, rootResult.Get("delta").String())...)
	case "response.output_text.done":
		block := codexClaudeTextBlockForEvent(params, rootResult, gjson.Result{}, true)
		if !block.HasDelta {
			output = append(output, startCodexClaudeTextBlock(params, block)...)
			output = append(output, appendCodexClaudeTextDelta(block, rootResult.Get("text").String())...)
		}
		output = append(output, stopCodexClaudeTextBlock(block)...)
	case "response.content_part.done":
		block := codexClaudeTextBlockForEvent(params, rootResult, gjson.Result{}, false)
		if block == nil {
			if text := rootResult.Get("part.text").String(); text != "" {
				block = codexClaudeTextBlockForEvent(params, rootResult, gjson.Result{}, true)
				output = append(output, startCodexClaudeTextBlock(params, block)...)
				output = append(output, appendCodexClaudeTextDelta(block, text)...)
			}
		}
		output = append(output, stopCodexClaudeTextBlock(block)...)
	case "response.output_item.added":
		itemResult := rootResult.Get("item")
		switch itemResult.Get("type").String() {
		case "function_call":
			block := codexClaudeFunctionBlockForEvent(params, rootResult, itemResult, true)
			updateCodexClaudeFunctionIdentity(params, block, rootResult, itemResult)
		case "reasoning":
			output = append(output, stopOpenCodexClaudeTextBlocks(params)...)
			output = append(output, emitPendingCodexThinkingBlock(params)...)
			params.PendingThinkingActive = true
			if signature := itemResult.Get("encrypted_content").String(); signature != "" {
				params.ThinkingSignature = signature
			}
		case "web_search_call":
			// Wait for output_item.done, which carries action.query and results.
		}
	case "response.output_item.done":
		itemResult := rootResult.Get("item")
		switch itemResult.Get("type").String() {
		case "message":
			output = append(output, emitCodexDoneOnlyMessageText(params, rootResult, itemResult)...)
		case "function_call":
			block := codexClaudeFunctionBlockForEvent(params, rootResult, itemResult, true)
			updateCodexClaudeFunctionIdentity(params, block, rootResult, itemResult)
			updateCodexClaudeFunctionArguments(block, itemResult.Get("arguments").String(), false)
			block.Done = true
		case "reasoning":
			output = append(output, stopOpenCodexClaudeTextBlocks(params)...)
			if !params.PendingThinkingActive {
				params.PendingThinkingActive = true
			}
			if signature := itemResult.Get("encrypted_content").String(); signature != "" {
				params.ThinkingSignature = signature
			}
			output = append(output, emitPendingCodexThinkingBlock(params)...)
		case "web_search_call":
			output = appendCodexWebSearchToolResult(output, params, rootResult, itemResult)
		}
	case "response.function_call_arguments.delta":
		block := codexClaudeFunctionBlockForEvent(params, rootResult, gjson.Result{}, true)
		updateCodexClaudeFunctionArguments(block, rootResult.Get("delta").String(), true)
	case "response.function_call_arguments.done":
		block := codexClaudeFunctionBlockForEvent(params, rootResult, gjson.Result{}, true)
		updateCodexClaudeFunctionArguments(block, rootResult.Get("arguments").String(), false)
	case "response.web_search_call.searching",
		"response.web_search_call.completed",
		"response.web_search_call.in_progress":
		// Wait for output_item.done, which carries action.query and results.
	case "response.completed", "response.done", "response.incomplete":
		responseData := rootResult.Get("response")
		output = append(output, emitPendingCodexThinkingBlock(params)...)
		output = append(output, stopOpenCodexClaudeTextBlocks(params)...)
		output = hydrateCodexClaudeFunctionBlocksFromTerminal(output, params, originalRequestRawJSON, responseData)
		if len(params.FunctionBlockQueue) == 0 {
			output = appendDeferredCodexClaudeStreamEvents(
				output,
				params,
				originalRequestRawJSON,
				param,
			)
		}
		output = append(output, emitPendingCodexThinkingBlock(params)...)
		output = append(output, finalizeCodexClaudeOpenBlocks(params)...)

		template := []byte(`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`)
		template, _ = sjson.SetBytes(
			template,
			"delta.stop_reason",
			mapCodexStopReasonToClaude(codexStopReason(responseData), params.HasToolCall),
		)
		template = setClaudeStopSequence(template, "delta.stop_sequence", responseData)
		inputTokens, outputTokens, cachedTokens, cacheWriteTokens := extractResponsesUsage(responseData.Get("usage"))
		template, _ = sjson.SetBytes(template, "usage.input_tokens", inputTokens)
		template, _ = sjson.SetBytes(template, "usage.output_tokens", outputTokens)
		if cachedTokens > 0 {
			template, _ = sjson.SetBytes(template, "usage.cache_read_input_tokens", cachedTokens)
		}
		if cacheWriteTokens > 0 {
			template, _ = sjson.SetBytes(template, "usage.cache_creation_input_tokens", cacheWriteTokens)
		}
		template = setClaudeReasoningUsage(template, responseData.Get("usage"))
		output = translatorcommon.AppendSSEEventBytes(output, "message_delta", template, 2)
		output = translatorcommon.AppendSSEEventBytes(output, "message_stop", []byte(`{"type":"message_stop"}`), 2)
		params.TerminalEmitted = true
	case "error", "response.error", "response.failed":
		output = append(output, emitPendingCodexThinkingBlock(params)...)
		output = append(output, finalizeCodexClaudeOpenBlocks(params)...)
		output = translatorcommon.AppendSSEEventBytes(output, "error", codexClaudeErrorEvent(rootResult), 2)
		params.TerminalEmitted = true
	}

	if !params.TerminalEmitted {
		output = appendCodexClaudeFunctionBlockQueue(output, params, originalRequestRawJSON)
		if len(params.FunctionBlockQueue) == 0 {
			output = appendDeferredCodexClaudeStreamEvents(
				output,
				params,
				originalRequestRawJSON,
				param,
			)
		}
	}

	return [][]byte{output}
}

func codexClaudeEventStartsMessage(eventType string) bool {
	switch eventType {
	case "response.created",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_part.done",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.added",
		"response.output_item.done",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.web_search_call.searching",
		"response.web_search_call.completed",
		"response.web_search_call.in_progress",
		"response.completed",
		"response.done",
		"response.incomplete",
		"error",
		"response.error",
		"response.failed":
		return true
	default:
		return false
	}
}

func startCodexClaudeMessage(
	params *convertCodexResponseToClaudeParams,
	modelName string,
	rootResult gjson.Result,
) []byte {
	if params.MessageStarted {
		return nil
	}

	template := []byte(`{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"claude-opus-4-1-20250805","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0},"content":[],"stop_reason":null}}`)
	if responseModel := rootResult.Get("response.model").String(); responseModel != "" {
		modelName = responseModel
	}
	if modelName != "" {
		template, _ = sjson.SetBytes(template, "message.model", modelName)
	}
	messageID := rootResult.Get("response.id").String()
	if messageID == "" {
		messageID = rootResult.Get("response_id").String()
	}
	if messageID != "" {
		template, _ = sjson.SetBytes(template, "message.id", messageID)
	}
	params.MessageStarted = true
	return translatorcommon.AppendSSEEventBytes(nil, "message_start", template, 2)
}

func codexClaudeErrorEvent(root gjson.Result) []byte {
	errorResult := root.Get("error")
	if !errorResult.Exists() || errorResult.Type == gjson.Null {
		errorResult = root.Get("response.error")
	}
	errType := strings.TrimSpace(errorResult.Get("type").String())
	if errType == "" {
		errType = strings.TrimSpace(root.Get("error_type").String())
	}
	if errType == "" {
		errType = "api_error"
	}

	code := strings.TrimSpace(errorResult.Get("code").String())
	message := strings.TrimSpace(errorResult.Get("message").String())
	if message == "" {
		message = strings.TrimSpace(root.Get("message").String())
	}
	if message == "" {
		message = code
	}
	if message == "" {
		message = errType
	}

	if code == "cyber_policy" || errType == "invalid_request" {
		errType = "invalid_request_error"
	}

	payload := []byte(`{"type":"error","error":{"type":"api_error","message":""}}`)
	payload, _ = sjson.SetBytes(payload, "error.type", errType)
	payload, _ = sjson.SetBytes(payload, "error.message", message)
	if code != "" {
		payload, _ = sjson.SetBytes(payload, "error.code", code)
	}
	return payload
}

func resetPendingCodexThinkingBlock(params *convertCodexResponseToClaudeParams) {
	params.PendingThinkingActive = false
	params.PendingThinkingText = ""
	params.PendingThinkingPartCount = 0
	params.ThinkingSignature = ""
}

func codexClaudeBlockKeys(rootResult, itemResult gjson.Result) []string {
	keys := make([]string, 0, 6)
	appendKey := func(prefix string, value gjson.Result) {
		if !value.Exists() || value.String() == "" {
			return
		}
		key := prefix + value.String()
		if slices.Contains(keys, key) {
			return
		}
		keys = append(keys, key)
	}

	appendKey("item:", rootResult.Get("item_id"))
	appendKey("item:", itemResult.Get("id"))
	appendKey("call:", rootResult.Get("call_id"))
	appendKey("call:", itemResult.Get("call_id"))
	appendKey("output:", rootResult.Get("output_index"))
	appendKey("output:", itemResult.Get("output_index"))
	if outputIndex := rootResult.Get("output_index"); outputIndex.Exists() {
		if contentIndex := rootResult.Get("content_index"); contentIndex.Exists() {
			appendKey("content:"+outputIndex.String()+":", contentIndex)
		}
	}
	return keys
}

func codexClaudeTextBlockForEvent(
	params *convertCodexResponseToClaudeParams,
	rootResult, itemResult gjson.Result,
	create bool,
) *codexClaudeTextBlock {
	return codexClaudeTextBlockForKeys(
		params,
		codexClaudeBlockKeys(rootResult, itemResult),
		codexClaudeTextContentKey(rootResult, itemResult, rootResult.Get("content_index")),
		create,
	)
}

func codexClaudeTextContentKey(rootResult, itemResult, contentIndex gjson.Result) string {
	if !contentIndex.Exists() {
		return ""
	}
	outputIndex := rootResult.Get("output_index")
	if !outputIndex.Exists() {
		outputIndex = itemResult.Get("output_index")
	}
	if outputIndex.Exists() {
		return "content:" + outputIndex.String() + ":" + contentIndex.String()
	}
	itemID := rootResult.Get("item_id").String()
	if itemID == "" {
		itemID = itemResult.Get("id").String()
	}
	if itemID != "" {
		return "item-content:" + itemID + ":" + contentIndex.String()
	}
	return ""
}

func codexClaudeTextBlockForKeys(
	params *convertCodexResponseToClaudeParams,
	keys []string,
	contentKey string,
	create bool,
) *codexClaudeTextBlock {
	if params == nil {
		return nil
	}
	if contentKey != "" {
		if block := params.TextBlocks[contentKey]; block != nil {
			return block
		}
		if !create {
			return nil
		}
	} else {
		for _, key := range keys {
			if block := params.TextBlocks[key]; block != nil {
				return block
			}
		}
		if len(keys) == 0 && params.LastTextBlock != nil && !params.LastTextBlock.Stopped {
			return params.LastTextBlock
		}
		if !create {
			return nil
		}
	}

	block := &codexClaudeTextBlock{Index: -1}
	if params.TextBlocks == nil {
		params.TextBlocks = make(map[string]*codexClaudeTextBlock)
	}
	if contentKey != "" {
		params.TextBlocks[contentKey] = block
	}
	for _, key := range keys {
		if params.TextBlocks[key] == nil {
			params.TextBlocks[key] = block
		}
	}
	params.TextBlockOrder = append(params.TextBlockOrder, block)
	params.LastTextBlock = block
	return block
}

func startCodexClaudeTextBlock(params *convertCodexResponseToClaudeParams, block *codexClaudeTextBlock) []byte {
	if params == nil || block == nil || block.Started || block.Stopped {
		return nil
	}
	block.Index = params.BlockIndex
	params.BlockIndex++
	block.Started = true

	start := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	start, _ = sjson.SetBytes(start, "index", block.Index)
	return translatorcommon.AppendSSEEventBytes(nil, "content_block_start", start, 2)
}

func appendCodexClaudeTextDelta(block *codexClaudeTextBlock, text string) []byte {
	if block == nil || !block.Started || block.Stopped || text == "" {
		return nil
	}
	block.HasDelta = true
	delta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`)
	delta, _ = sjson.SetBytes(delta, "index", block.Index)
	delta, _ = sjson.SetBytes(delta, "delta.text", text)
	return translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", delta, 2)
}

func stopCodexClaudeTextBlock(block *codexClaudeTextBlock) []byte {
	if block == nil || !block.Started || block.Stopped {
		return nil
	}
	block.Stopped = true
	stop := []byte(`{"type":"content_block_stop","index":0}`)
	stop, _ = sjson.SetBytes(stop, "index", block.Index)
	return translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", stop, 2)
}

func emitCodexDoneOnlyMessageText(
	params *convertCodexResponseToClaudeParams,
	rootResult, itemResult gjson.Result,
) []byte {
	if params == nil {
		return nil
	}
	content := itemResult.Get("content")
	if !content.Exists() {
		return nil
	}

	keys := codexClaudeBlockKeys(rootResult, itemResult)
	output := make([]byte, 0, 256)
	emitText := func(contentIndex gjson.Result, text string) {
		if text == "" {
			return
		}
		block := codexClaudeTextBlockForKeys(
			params,
			keys,
			codexClaudeTextContentKey(rootResult, itemResult, contentIndex),
			true,
		)
		if block == nil || block.Stopped {
			return
		}
		if !block.HasDelta {
			output = append(output, startCodexClaudeTextBlock(params, block)...)
			output = append(output, appendCodexClaudeTextDelta(block, text)...)
		}
		output = append(output, stopCodexClaudeTextBlock(block)...)
	}
	if content.IsArray() {
		content.ForEach(
			func(index, part gjson.Result) bool {
				if part.Get("type").String() == "output_text" {
					emitText(index, part.Get("text").String())
				}
				return true
			},
		)
	} else {
		emitText(gjson.Parse("0"), content.String())
	}
	return output
}

func codexClaudeFunctionBlockForEvent(
	params *convertCodexResponseToClaudeParams,
	rootResult, itemResult gjson.Result,
	create bool,
) *codexClaudeFunctionBlock {
	if params == nil {
		return nil
	}
	keys := codexClaudeBlockKeys(rootResult, itemResult)
	for _, key := range keys {
		if block := params.FunctionBlocks[key]; block != nil {
			return block
		}
	}
	if len(keys) == 0 && params.LastFunctionBlock != nil && !params.LastFunctionBlock.Stopped {
		return params.LastFunctionBlock
	}
	if !create {
		return nil
	}

	block := &codexClaudeFunctionBlock{Index: -1}
	if params.FunctionBlocks == nil {
		params.FunctionBlocks = make(map[string]*codexClaudeFunctionBlock)
	}
	for _, key := range keys {
		params.FunctionBlocks[key] = block
	}
	params.FunctionBlockOrder = append(params.FunctionBlockOrder, block)
	params.FunctionBlockQueue = append(params.FunctionBlockQueue, block)
	params.LastFunctionBlock = block
	return block
}

func addCodexClaudeFunctionAliases(
	params *convertCodexResponseToClaudeParams,
	block *codexClaudeFunctionBlock,
	keys []string,
) {
	if params == nil || block == nil {
		return
	}
	if params.FunctionBlocks == nil {
		params.FunctionBlocks = make(map[string]*codexClaudeFunctionBlock)
	}
	for _, key := range keys {
		params.FunctionBlocks[key] = block
	}
}

func updateCodexClaudeFunctionIdentity(
	params *convertCodexResponseToClaudeParams,
	block *codexClaudeFunctionBlock,
	rootResult, itemResult gjson.Result,
) {
	if block == nil {
		return
	}
	if callID := itemResult.Get("call_id").String(); callID != "" {
		block.CallID = callID
	} else if callID := rootResult.Get("call_id").String(); callID != "" {
		block.CallID = callID
	}
	if name := itemResult.Get("name").String(); name != "" {
		block.Name = name
	}
	addCodexClaudeFunctionAliases(params, block, codexClaudeBlockKeys(rootResult, itemResult))
}

func updateCodexClaudeFunctionArguments(block *codexClaudeFunctionBlock, arguments string, delta bool) {
	if block == nil || arguments == "" {
		return
	}
	if delta {
		block.Arguments += arguments
		return
	}
	if block.Arguments == "" || strings.HasPrefix(arguments, block.Arguments) {
		block.Arguments = arguments
	}
}

func shouldDeferCodexClaudeStreamEvent(eventType string, rootResult gjson.Result) bool {
	switch eventType {
	case "error",
		"response.error",
		"response.failed",
		"response.completed",
		"response.done",
		"response.incomplete",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done":
		return false
	case "response.output_item.added", "response.output_item.done":
		return rootResult.Get("item.type").String() != "function_call"
	default:
		return true
	}
}

func appendDeferredCodexClaudeStreamEvents(
	output []byte,
	params *convertCodexResponseToClaudeParams,
	originalRequestRawJSON []byte,
	param *any,
) []byte {
	if params == nil || len(params.DeferredStreamEvents) == 0 {
		return output
	}

	events := params.DeferredStreamEvents
	params.DeferredStreamEvents = nil
	for _, event := range events {
		translated := ConvertCodexResponseToClaude(
			context.Background(),
			"",
			originalRequestRawJSON,
			nil,
			event,
			param,
		)
		for _, chunk := range translated {
			output = append(output, chunk...)
		}
	}
	return output
}

func hasOpenCodexClaudeTextBlock(params *convertCodexResponseToClaudeParams) bool {
	if params == nil {
		return false
	}
	for _, block := range params.TextBlockOrder {
		if block != nil && block.Started && !block.Stopped {
			return true
		}
	}
	return false
}

func appendCodexClaudeFunctionBlockQueue(
	output []byte,
	params *convertCodexResponseToClaudeParams,
	originalRequestRawJSON []byte,
) []byte {
	if params == nil {
		return output
	}

	for {
		if active := params.ActiveFunctionBlock; active != nil {
			output = append(output, flushCodexClaudeFunctionArguments(active)...)
			if !active.Done {
				return output
			}
			output = append(output, stopCodexClaudeFunctionBlock(active)...)
			params.ActiveFunctionBlock = nil
			removeCodexClaudeFunctionBlockFromQueue(params, active)
		}

		for len(params.FunctionBlockQueue) > 0 && params.FunctionBlockQueue[0].Stopped {
			params.FunctionBlockQueue = params.FunctionBlockQueue[1:]
		}
		if len(params.FunctionBlockQueue) == 0 || hasOpenCodexClaudeTextBlock(params) || params.ThinkingBlockOpen {
			return output
		}
		output = append(output, emitPendingCodexThinkingBlock(params)...)

		block := params.FunctionBlockQueue[0]
		if block.Name == "" {
			return output
		}
		output = append(output, startCodexClaudeFunctionBlock(params, block, originalRequestRawJSON)...)
		params.ActiveFunctionBlock = block
		output = append(output, flushCodexClaudeFunctionArguments(block)...)
	}
}

func removeCodexClaudeFunctionBlockFromQueue(
	params *convertCodexResponseToClaudeParams,
	block *codexClaudeFunctionBlock,
) {
	if params == nil || block == nil {
		return
	}
	for index, queued := range params.FunctionBlockQueue {
		if queued != block {
			continue
		}
		params.FunctionBlockQueue = append(
			params.FunctionBlockQueue[:index],
			params.FunctionBlockQueue[index+1:]...,
		)
		return
	}
}

func startCodexClaudeFunctionBlock(
	params *convertCodexResponseToClaudeParams,
	block *codexClaudeFunctionBlock,
	originalRequestRawJSON []byte,
) []byte {
	if params == nil || block == nil || block.Started || block.Stopped || block.Name == "" {
		return nil
	}
	block.Index = params.BlockIndex
	params.BlockIndex++
	block.Started = true
	params.HasToolCall = true

	callID := block.CallID
	if callID == "" {
		callID = "call_" + strconv.Itoa(block.Index)
	}
	name := block.Name
	if original, ok := buildReverseMapFromClaudeOriginalShortToOriginal(originalRequestRawJSON)[name]; ok {
		name = original
	}
	start := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`)
	start, _ = sjson.SetBytes(start, "index", block.Index)
	start, _ = sjson.SetBytes(
		start,
		"content_block.id",
		shortenCodexCallIDIfNeeded(util.SanitizeClaudeToolID(callID)),
	)
	start, _ = sjson.SetBytes(start, "content_block.name", name)
	output := translatorcommon.AppendSSEEventBytes(nil, "content_block_start", start, 2)

	delta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
	delta, _ = sjson.SetBytes(delta, "index", block.Index)
	return translatorcommon.AppendSSEEventBytes(output, "content_block_delta", delta, 2)
}

func flushCodexClaudeFunctionArguments(block *codexClaudeFunctionBlock) []byte {
	if block == nil || !block.Started || block.Stopped || block.EmittedArgumentsLength >= len(block.Arguments) {
		return nil
	}
	arguments := block.Arguments[block.EmittedArgumentsLength:]
	block.EmittedArgumentsLength = len(block.Arguments)
	delta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
	delta, _ = sjson.SetBytes(delta, "index", block.Index)
	delta, _ = sjson.SetBytes(delta, "delta.partial_json", arguments)
	return translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", delta, 2)
}

func stopCodexClaudeFunctionBlock(block *codexClaudeFunctionBlock) []byte {
	if block == nil || !block.Started || block.Stopped {
		return nil
	}
	block.Stopped = true
	stop := []byte(`{"type":"content_block_stop","index":0}`)
	stop, _ = sjson.SetBytes(stop, "index", block.Index)
	return translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", stop, 2)
}

func hydrateCodexClaudeFunctionBlocksFromTerminal(
	output []byte,
	params *convertCodexResponseToClaudeParams,
	originalRequestRawJSON []byte,
	responseData gjson.Result,
) []byte {
	responseData.Get("output").ForEach(
		func(index, itemResult gjson.Result) bool {
			if itemResult.Get("type").String() != "function_call" {
				return true
			}
			keys := codexClaudeBlockKeys(gjson.Result{}, itemResult)
			keys = append(keys, "output:"+index.String())
			var block *codexClaudeFunctionBlock
			for _, key := range keys {
				if candidate := params.FunctionBlocks[key]; candidate != nil {
					block = candidate
					break
				}
			}
			if block == nil {
				block = &codexClaudeFunctionBlock{Index: -1}
				params.FunctionBlockOrder = append(params.FunctionBlockOrder, block)
				params.FunctionBlockQueue = append(params.FunctionBlockQueue, block)
				params.LastFunctionBlock = block
			}
			addCodexClaudeFunctionAliases(params, block, keys)
			updateCodexClaudeFunctionIdentity(params, block, gjson.Result{}, itemResult)
			updateCodexClaudeFunctionArguments(block, itemResult.Get("arguments").String(), false)
			block.Done = true
			return true
		},
	)
	for _, block := range params.FunctionBlockQueue {
		if block == nil || block.Stopped {
			continue
		}
		if block.Name == "" {
			block.Stopped = true
			continue
		}
		block.Done = true
	}
	return appendCodexClaudeFunctionBlockQueue(output, params, originalRequestRawJSON)
}

func stopOpenCodexClaudeTextBlocks(params *convertCodexResponseToClaudeParams) []byte {
	if params == nil {
		return nil
	}
	output := make([]byte, 0, 128)
	for _, block := range params.TextBlockOrder {
		output = append(output, stopCodexClaudeTextBlock(block)...)
	}
	return output
}

func finalizeCodexClaudeOpenBlocks(params *convertCodexResponseToClaudeParams) []byte {
	if params == nil {
		return nil
	}
	output := make([]byte, 0, 256)
	for index := 0; index < params.BlockIndex; index++ {
		if params.ThinkingBlockOpen && params.ThinkingBlockIndex == index {
			output = append(output, finalizeCodexThinkingBlock(params)...)
		}
		for _, block := range params.TextBlockOrder {
			if block != nil && block.Index == index {
				output = append(output, stopCodexClaudeTextBlock(block)...)
			}
		}
		for _, block := range params.FunctionBlockOrder {
			if block != nil && block.Index == index {
				output = append(output, flushCodexClaudeFunctionArguments(block)...)
				output = append(output, stopCodexClaudeFunctionBlock(block)...)
			}
		}
	}
	return output
}

func emitPendingCodexThinkingBlock(params *convertCodexResponseToClaudeParams) []byte {
	if !params.PendingThinkingActive {
		return nil
	}
	output := stopOpenCodexClaudeTextBlocks(params)
	if params.ThinkingSignature == "" {
		resetPendingCodexThinkingBlock(params)
		return output
	}

	output = append(output, startCodexThinkingBlock(params)...)
	if params.PendingThinkingText != "" {
		delta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`)
		delta, _ = sjson.SetBytes(delta, "index", params.ThinkingBlockIndex)
		delta, _ = sjson.SetBytes(delta, "delta.thinking", params.PendingThinkingText)
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", delta, 2)
	}
	output = append(output, finalizeCodexThinkingBlock(params)...)
	resetPendingCodexThinkingBlock(params)
	return output
}

func startCodexThinkingBlock(params *convertCodexResponseToClaudeParams) []byte {
	if params == nil || params.ThinkingBlockOpen {
		return nil
	}
	params.ThinkingBlockIndex = params.BlockIndex
	params.BlockIndex++
	params.ThinkingBlockOpen = true

	template := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
	template, _ = sjson.SetBytes(template, "index", params.ThinkingBlockIndex)
	return translatorcommon.AppendSSEEventBytes(nil, "content_block_start", template, 2)
}

func finalizeCodexThinkingBlock(params *convertCodexResponseToClaudeParams) []byte {
	if params == nil || !params.ThinkingBlockOpen {
		return nil
	}

	output := make([]byte, 0, 256)
	if params.ThinkingSignature != "" {
		signatureDelta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}`)
		signatureDelta, _ = sjson.SetBytes(signatureDelta, "index", params.ThinkingBlockIndex)
		signatureDelta, _ = sjson.SetBytes(signatureDelta, "delta.signature", params.ThinkingSignature)
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", signatureDelta, 2)
	}
	contentBlockStop := []byte(`{"type":"content_block_stop","index":0}`)
	contentBlockStop, _ = sjson.SetBytes(contentBlockStop, "index", params.ThinkingBlockIndex)
	output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", contentBlockStop, 2)

	params.ThinkingBlockOpen = false
	params.ThinkingSignature = ""
	return output
}

// ConvertCodexResponseToClaudeNonStream converts a non-streaming Codex response to a non-streaming Claude Code response.
// This function processes the complete Codex response and transforms it into a single Claude Code-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the Claude Code API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - []byte: A Claude Code-compatible JSON response containing all message content and metadata
func ConvertCodexResponseToClaudeNonStream(
	_ context.Context,
	_ string,
	originalRequestRawJSON, _ []byte,
	rawJSON []byte,
	_ *any,
) []byte {
	revNames := buildReverseMapFromClaudeOriginalShortToOriginal(originalRequestRawJSON)

	rootResult := gjson.ParseBytes(rawJSON)
	responseType := rootResult.Get("type").String()
	if responseType != "response.completed" && responseType != "response.incomplete" {
		return []byte{}
	}

	responseData := rootResult.Get("response")
	if !responseData.Exists() {
		return []byte{}
	}

	out := []byte(`{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	out, _ = sjson.SetBytes(out, "id", responseData.Get("id").String())
	out, _ = sjson.SetBytes(out, "model", responseData.Get("model").String())
	inputTokens, outputTokens, cachedTokens, cacheWriteTokens := extractResponsesUsage(responseData.Get("usage"))
	out, _ = sjson.SetBytes(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.SetBytes(out, "usage.output_tokens", outputTokens)
	if cachedTokens > 0 {
		out, _ = sjson.SetBytes(out, "usage.cache_read_input_tokens", cachedTokens)
	}
	if cacheWriteTokens > 0 {
		out, _ = sjson.SetBytes(out, "usage.cache_creation_input_tokens", cacheWriteTokens)
	}
	out = setClaudeReasoningUsage(out, responseData.Get("usage"))

	hasToolCall := false
	webSearchSeen := make(map[string]struct{})

	if output := responseData.Get("output"); output.Exists() && output.IsArray() {
		output.ForEach(
			func(_, item gjson.Result) bool {
				switch item.Get("type").String() {
				case "reasoning":
					thinkingBuilder := strings.Builder{}
					if summary := item.Get("summary"); summary.Exists() {
						if summary.IsArray() {
							summary.ForEach(
								func(_, part gjson.Result) bool {
									if txt := part.Get("text"); txt.Exists() {
										thinkingBuilder.WriteString(txt.String())
									} else {
										thinkingBuilder.WriteString(part.String())
									}
									return true
								},
							)
						} else {
							thinkingBuilder.WriteString(summary.String())
						}
					}
					if thinkingBuilder.Len() == 0 {
						if content := item.Get("content"); content.Exists() {
							if content.IsArray() {
								content.ForEach(
									func(_, part gjson.Result) bool {
										if txt := part.Get("text"); txt.Exists() {
											thinkingBuilder.WriteString(txt.String())
										} else {
											thinkingBuilder.WriteString(part.String())
										}
										return true
									},
								)
							} else {
								thinkingBuilder.WriteString(content.String())
							}
						}
					}
					signature := item.Get("encrypted_content").String()
					if signature != "" {
						block := []byte(`{"type":"thinking","thinking":""}`)
						block, _ = sjson.SetBytes(block, "thinking", thinkingBuilder.String())
						block, _ = sjson.SetBytes(block, "signature", signature)
						out, _ = sjson.SetRawBytes(out, "content.-1", block)
					}
				case "message":
					if content := item.Get("content"); content.Exists() {
						if content.IsArray() {
							content.ForEach(
								func(_, part gjson.Result) bool {
									if part.Get("type").String() == "output_text" {
										text := part.Get("text").String()
										if text != "" {
											block := []byte(`{"type":"text","text":""}`)
											block, _ = sjson.SetBytes(block, "text", text)
											out, _ = sjson.SetRawBytes(out, "content.-1", block)
										}
									}
									return true
								},
							)
						} else {
							text := content.String()
							if text != "" {
								block := []byte(`{"type":"text","text":""}`)
								block, _ = sjson.SetBytes(block, "text", text)
								out, _ = sjson.SetRawBytes(out, "content.-1", block)
							}
						}
					}
				case "web_search_call":
					out = appendCodexWebSearchNonStreamContent(out, item, webSearchSeen)
				case "function_call":
					hasToolCall = true
					name := item.Get("name").String()
					if original, ok := revNames[name]; ok {
						name = original
					}

					toolBlock := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
					toolBlock, _ = sjson.SetBytes(
						toolBlock, "id",
						shortenCodexCallIDIfNeeded(util.SanitizeClaudeToolID(item.Get("call_id").String())),
					)
					toolBlock, _ = sjson.SetBytes(toolBlock, "name", name)
					inputRaw := "{}"
					if argsStr := item.Get("arguments").String(); argsStr != "" && gjson.Valid(argsStr) {
						argsJSON := gjson.Parse(argsStr)
						if argsJSON.IsObject() {
							inputRaw = argsJSON.Raw
						}
					}
					toolBlock, _ = sjson.SetRawBytes(toolBlock, "input", []byte(inputRaw))
					out, _ = sjson.SetRawBytes(out, "content.-1", toolBlock)
				}
				return true
			},
		)
	}

	out, _ = sjson.SetBytes(
		out,
		"stop_reason",
		mapCodexStopReasonToClaude(codexStopReason(responseData), hasToolCall),
	)
	out = setClaudeStopSequence(out, "stop_sequence", responseData)

	return out
}

func codexStopReason(responseData gjson.Result) string {
	if stopReason := responseData.Get("stop_reason"); stopReason.Exists() && stopReason.String() != "" {
		if stopReason.String() == "stop" && codexStopSequence(responseData).String() != "" {
			return "stop_sequence"
		}
		return stopReason.String()
	}
	if reason := responseData.Get("incomplete_details.reason"); reason.Exists() && reason.String() != "" {
		return reason.String()
	}
	if codexStopSequence(responseData).String() != "" {
		return "stop_sequence"
	}
	return ""
}

func mapCodexStopReasonToClaude(stopReason string, hasToolCall bool) string {
	if hasToolCall {
		return "tool_use"
	}

	switch stopReason {
	case "", "stop", "completed":
		return "end_turn"
	case "max_tokens", "max_output_tokens":
		return "max_tokens"
	case "tool_use", "tool_calls", "function_call":
		return "end_turn"
	case "end_turn", "stop_sequence", "pause_turn", "refusal", "model_context_window_exceeded":
		return stopReason
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func codexStopSequence(responseData gjson.Result) gjson.Result {
	return responseData.Get("stop_sequence")
}

func setClaudeStopSequence(out []byte, path string, responseData gjson.Result) []byte {
	if stopSequence := codexStopSequence(responseData); stopSequence.Exists() && stopSequence.String() != "" {
		out, _ = sjson.SetRawBytes(out, path, []byte(stopSequence.Raw))
	}
	return out
}

func extractResponsesUsage(usage gjson.Result) (int64, int64, int64, int64) {
	if !usage.Exists() || usage.Type == gjson.Null {
		return 0, 0, 0, 0
	}

	inputTokens := usage.Get("input_tokens").Int()
	outputTokens := usage.Get("output_tokens").Int()
	cachedTokens := usage.Get("input_tokens_details.cached_tokens").Int()
	cacheWriteTokens := usage.Get("input_tokens_details.cache_write_tokens").Int()
	if cacheWriteTokens == 0 {
		cacheWriteTokens = usage.Get("input_tokens_details.cache_creation_tokens").Int()
	}

	if cachedTokens > 0 {
		if inputTokens >= cachedTokens {
			inputTokens -= cachedTokens
		} else {
			inputTokens = 0
		}
	}

	return inputTokens, outputTokens, cachedTokens, cacheWriteTokens
}

func setClaudeReasoningUsage(out []byte, usage gjson.Result) []byte {
	detail := usage.Get("output_tokens_details.reasoning_tokens")
	if !detail.Exists() || detail.Type != gjson.Number || strings.HasPrefix(detail.Raw, "-") || detail.Num < 0 {
		return out
	}

	outputTokens := max(int64(0), usage.Get("output_tokens").Int())
	tokens := detail.Int()
	if detail.Num >= float64(outputTokens) {
		tokens = outputTokens
	}
	updated, err := sjson.SetBytes(out, "usage.output_tokens_details.thinking_tokens", tokens)
	if err != nil {
		return out
	}
	return updated
}

// buildReverseMapFromClaudeOriginalShortToOriginal builds a map[short]original from original Claude request tools.
func buildReverseMapFromClaudeOriginalShortToOriginal(original []byte) map[string]string {
	tools := gjson.GetBytes(original, "tools")
	rev := map[string]string{}
	if !tools.IsArray() {
		return rev
	}
	var names []string
	arr := tools.Array()
	for i := range arr {
		n := arr[i].Get("name").String()
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		m := buildShortNameMap(names)
		for orig, short := range m {
			rev[short] = orig
		}
	}
	return rev
}

func tokenCount(_ context.Context, count int64) []byte {
	return translatorcommon.ClaudeInputTokensJSON(count)
}
