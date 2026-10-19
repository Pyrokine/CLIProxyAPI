// Package claude provides response translation functionality for Claude Code API compatibility.
// This package handles the conversion of backend client responses into Claude Code-compatible
// Server-Sent Events (SSE) format, implementing a sophisticated state machine that manages
// different response types including text content, thinking processes, and function calls.
// The translation ensures proper sequencing of SSE events and maintains state across
// multiple response chunks to provide a seamless streaming experience.
package claude

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	sigcompat "github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	geminiopenai "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func formatGeminiClaudeCarrierValue(modelName, rawSignature, direction, targetKind string) string {
	if sigcompat.ProviderFromModelName(modelName) == sigcompat.ProviderGemini {
		return encodeGeminiClaudeCarrierSignature(rawSignature, direction, targetKind)
	}
	return formatClaudeSignatureValue(modelName, rawSignature)
}

func formatClaudeSignatureValue(modelName, rawSignature string) string {
	if cache.GetModelGroup(modelName) == "gemini" {
		return rawSignature
	}
	return fmt.Sprintf("%s#%s", cache.GetModelGroup(modelName), rawSignature)
}

// params holds parameters for response conversion and maintains state across streaming chunks.
// This structure tracks the current state of the response translation process to ensure
// proper sequencing of SSE events and transitions between different content types.
type params struct {
	HasFirstResponse    bool   // Indicates if the initial message_start event has been sent
	ResponseType        int    // Current response type: 0=none, 1=content, 2=thinking, 3=function
	ResponseIndex       int    // Index counter for content blocks in the streaming response
	HasFinishReason     bool   // Tracks whether a finish reason has been observed
	FinishReason        string // The finish reason string returned by the provider
	Usage               geminiopenai.GeminiUsage
	HasSentFinalEvents  bool              // Indicates if final content/message events have been sent
	HasSentMessageStop  bool              // Indicates if the terminal message_stop event has been sent
	HasToolUse          bool              // Indicates if tool use was observed in the stream
	HasContent          bool              // Tracks whether any content (text, thinking, or tool use) has been output
	ToolNameMap         map[string]string // Maps provider-facing names back to client-facing names.
	HasSemanticContent  bool
	LastSemanticKind    string
	HasWebSearchTool    bool
	WebSearchRequests   int64
	WebSearchTextBuffer strings.Builder

	// Signature caching support
	CurrentThinkingText   strings.Builder // Accumulates thinking text for signature caching
	CurrentThinkingSigned bool            // Tracks whether the active thinking block already has its terminal signature
}

// toolUseIDCounter provides a process-wide unique counter for tool use identifiers.
var toolUseIDCounter atomic.Uint64

func antigravityClaudeToolUseID(modelName string, functionCall gjson.Result, fallback string) string {
	if sigcompat.ProviderFromModelName(modelName) == sigcompat.ProviderGemini {
		stableID := util.GeminiClaudeToolUseID(
			functionCall.Get("id").String(),
			functionCall.Get("name").String(),
			functionCall.Get("args").Raw,
		)
		if stableID != "" {
			return stableID
		}
	}
	return util.SanitizeClaudeToolID(fallback)
}

func restoreClaudeFunctionNameFromAntigravity(toolNameMap map[string]string, name string) string {
	name = translatorcommon.AntigravityUpstreamToolNameToClient(name)
	return util.RestoreSanitizedToolName(toolNameMap, name)
}

// convertAntigravityResponseToClaude performs sophisticated streaming response format conversion.
// This function implements a complex state machine that translates backend client responses
// into Claude Code-compatible Server-Sent Events (SSE) format. It manages different response types
// and handles state transitions between content blocks, thinking processes, and function calls.
//
// Response type states: 0=none, 1=content, 2=thinking, 3=function
// The function maintains state across multiple calls to ensure proper SSE event sequencing.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Gemini CLI API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of bytes, each containing a Claude Code-compatible SSE payload.
func convertAntigravityResponseToClaude(
	_ context.Context,
	_ string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) [][]byte {
	if *param == nil {
		*param = &params{
			HasFirstResponse: false,
			ResponseType:     0,
			ResponseIndex:    0,
			ToolNameMap:      util.DisambiguatedToolNameMap(originalRequestRawJSON),
		}
	}
	modelName := gjson.GetBytes(requestRawJSON, "model").String()

	params := (*param).(*params)
	if params.HasSentMessageStop {
		return [][]byte{}
	}
	previousUsage := params.Usage
	params.Usage.Update(gjson.GetBytes(rawJSON, "response.cpaUsageMetadata"))
	params.Usage.Update(gjson.GetBytes(rawJSON, "response.usageMetadata"))
	if params.HasSentFinalEvents && !bytes.Equal(rawJSON, []byte("[DONE]")) {
		if params.Usage != previousUsage {
			var output []byte
			appendFinalEvents(params, &output, false)
			return [][]byte{output}
		}
		return [][]byte{}
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		output := make([]byte, 0, 256)
		if params.HasFirstResponse && !params.HasContent {
			output = translatorcommon.AppendSSEEventString(
				output,
				"content_block_start",
				fmt.Sprintf(
					`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`,
					params.ResponseIndex,
				),
				3,
			)
			params.ResponseType = 1
			params.HasContent = true
		}
		if params.HasContent {
			appendFinalEvents(params, &output, true)
			output = translatorcommon.AppendSSEEventString(output, "message_stop", `{"type":"message_stop"}`, 3)
			params.HasSentMessageStop = true
			return [][]byte{output}
		}
		return [][]byte{}
	}

	output := make([]byte, 0, 1024)
	appendEvent := func(event, payload string) {
		output = translatorcommon.AppendSSEEventString(output, event, payload, 3)
	}
	webSearchStreamMode := shouldTranslateWebSearchGrounding(originalRequestRawJSON, requestRawJSON)
	appendThinkingSignature := func(rawSignature, direction, targetKind string) {
		if rawSignature == "" || params.ResponseType != 2 {
			return
		}
		if params.CurrentThinkingText.Len() > 0 {
			cache.StoreSignature(modelName, params.CurrentThinkingText.String(), rawSignature)
			params.CurrentThinkingText.Reset()
		}
		signatureValue := formatGeminiClaudeCarrierValue(
			modelName,
			rawSignature,
			direction,
			targetKind,
		)
		data, _ := sjson.SetBytes(
			[]byte(fmt.Sprintf(
				`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":""}}`,
				params.ResponseIndex,
			)),
			"delta.signature",
			signatureValue,
		)
		appendEvent("content_block_delta", string(data))
		params.CurrentThinkingSigned = true
		params.HasContent = true
	}
	closeCurrentBlock := func() {
		if params.ResponseType == 0 {
			return
		}
		appendEvent(
			"content_block_stop",
			fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, params.ResponseIndex),
		)
		params.ResponseIndex++
		params.ResponseType = 0
		params.CurrentThinkingSigned = false
	}
	startEmptyThinkingBlock := func() {
		appendEvent(
			"content_block_start",
			fmt.Sprintf(
				`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`,
				params.ResponseIndex,
			),
		)
		params.ResponseType = 2
		params.CurrentThinkingSigned = false
		params.HasContent = true
	}
	appendCarrierSignature := func(rawSignature, direction, targetKind string) {
		if rawSignature == "" || params.ResponseType != 2 {
			return
		}
		signatureValue := formatGeminiClaudeCarrierValue(
			modelName,
			rawSignature,
			direction,
			targetKind,
		)
		data, _ := sjson.SetBytes(
			[]byte(fmt.Sprintf(
				`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":""}}`,
				params.ResponseIndex,
			)),
			"delta.signature",
			signatureValue,
		)
		appendEvent("content_block_delta", string(data))
		params.CurrentThinkingSigned = true
		params.HasContent = true
	}
	appendPartSignature := func(rawSignature, direction, targetKind string) bool {
		if rawSignature == "" {
			return false
		}
		if params.ResponseType == 2 && !params.CurrentThinkingSigned {
			appendThinkingSignature(rawSignature, direction, targetKind)
			return false
		}
		closeCurrentBlock()
		startEmptyThinkingBlock()
		appendCarrierSignature(rawSignature, direction, targetKind)
		return true
	}

	// Initialize the streaming session with a message_start event
	// This is only sent for the very first response chunk to establish the streaming session
	if !params.HasFirstResponse {
		// Create the initial message structure with default values according to Claude Code API specification
		// This follows the Claude Code API specification for streaming message initialization
		messageStartTemplate := []byte(`{"type": "message_start", "message": {"id": "msg_1nZdL29xx5MUA1yADyHTEsnR8uuvGzszyY", "type": "message", "role": "assistant", "content": [], "model": "claude-3-5-sonnet-20241022", "stop_reason": null, "stop_sequence": null, "usage": {"input_tokens": 0, "output_tokens": 0}}}`)

		// message_start 使用已收到的用量，缓存命中单独计数
		if params.Usage.Seen {
			messageStartTemplate, _ = sjson.SetBytes(
				messageStartTemplate, "message.usage.input_tokens", max(params.Usage.PromptTokens-params.Usage.CachedTokens, 0),
			)
			if params.Usage.HasCachedTokens {
				messageStartTemplate, _ = sjson.SetBytes(
					messageStartTemplate, "message.usage.cache_read_input_tokens", params.Usage.CachedTokens,
				)
			}
			if !webSearchStreamMode {
				messageStartTemplate, _ = sjson.SetBytes(
					messageStartTemplate, "message.usage.output_tokens", params.Usage.OutputTokens(),
				)
			}
		}

		// Override default values with actual response metadata if available from the Gemini CLI response
		if modelVersionResult := gjson.GetBytes(rawJSON, "response.modelVersion"); modelVersionResult.Exists() {
			messageStartTemplate, _ = sjson.SetBytes(messageStartTemplate, "message.model", modelVersionResult.String())
		}
		if responseIDResult := gjson.GetBytes(rawJSON, "response.responseId"); responseIDResult.Exists() {
			messageStartTemplate, _ = sjson.SetBytes(messageStartTemplate, "message.id", responseIDResult.String())
		}
		appendEvent("message_start", string(messageStartTemplate))

		params.HasFirstResponse = true
	}

	handledWebSearchGrounding := false
	if webSearchStreamMode && !params.HasWebSearchTool {
		root := gjson.ParseBytes(rawJSON)
		if groundingMetadata := antigravityGroundingMetadata(root); groundingMetadata.Exists() {
			toolUseID := newClaudeWebSearchToolUseID()
			textContent := params.WebSearchTextBuffer.String() + antigravityTextContent(root)
			params.WebSearchTextBuffer.Reset()
			params.ResponseIndex = appendClaudeWebSearchStreamBlocks(
				appendEvent, params.ResponseIndex, toolUseID, textContent, groundingMetadata,
			)
			params.HasWebSearchTool = true
			params.WebSearchRequests = 1
			params.HasContent = true
			params.ResponseType = 0
			handledWebSearchGrounding = true
		}
	}

	// Process the response parts array from the backend client
	// Each part can contain text content, thinking content, or function calls
	partsResult := gjson.GetBytes(rawJSON, "response.candidates.0.content.parts")
	if partsResult.IsArray() && webSearchStreamMode && !params.HasWebSearchTool && !handledWebSearchGrounding {
		appendWebSearchBufferedText(partsResult, &params.WebSearchTextBuffer)
	} else if partsResult.IsArray() && !handledWebSearchGrounding {
		partResults := partsResult.Array()
		for i := range partResults {
			partResult := partResults[i]

			// Extract the different types of content from each part
			partTextResult := partResult.Get("text")
			functionCallResult := partResult.Get("functionCall")
			thoughtSignatureResult := partResult.Get("thoughtSignature")
			if !thoughtSignatureResult.Exists() {
				thoughtSignatureResult = partResult.Get("thought_signature")
			}
			hasThoughtSignature := thoughtSignatureResult.Exists() && thoughtSignatureResult.String() != "" && !functionCallResult.Exists()

			if hasThoughtSignature && (!partTextResult.Exists() || partTextResult.String() == "") {
				direction := geminiClaudeCarrierNext
				targetKind := geminiClaudeCarrierAny
				if params.HasSemanticContent {
					direction = geminiClaudeCarrierPrevious
					targetKind = params.LastSemanticKind
				}
				appendPartSignature(thoughtSignatureResult.String(), direction, targetKind)
				continue
			}

			// Handle text content (both regular content and thinking)
			if partTextResult.Exists() {
				partText := partTextResult.String()
				if partResult.Get("thought").Bool() {
					if partText != "" {
						params.HasSemanticContent = true
						params.LastSemanticKind = geminiClaudeCarrierText
						if params.ResponseType == 2 && params.CurrentThinkingSigned {
							closeCurrentBlock()
						}
						if params.ResponseType == 2 {
							params.CurrentThinkingText.WriteString(partText)
							data, _ := sjson.SetBytes(
								[]byte(fmt.Sprintf(
									`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":""}}`,
									params.ResponseIndex,
								)),
								"delta.thinking",
								partText,
							)
							appendEvent("content_block_delta", string(data))
							params.HasContent = true
						} else {
							closeCurrentBlock()
							appendEvent(
								"content_block_start",
								fmt.Sprintf(
									`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`,
									params.ResponseIndex,
								),
							)
							params.CurrentThinkingSigned = false
							data, _ := sjson.SetBytes(
								[]byte(fmt.Sprintf(
									`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":""}}`,
									params.ResponseIndex,
								)),
								"delta.thinking",
								partText,
							)
							appendEvent("content_block_delta", string(data))
							params.ResponseType = 2
							params.HasContent = true
							params.CurrentThinkingText.Reset()
							params.CurrentThinkingText.WriteString(partText)
						}
					}
					if hasThoughtSignature {
						appendThinkingSignature(
							thoughtSignatureResult.String(),
							geminiClaudeCarrierStandalone,
							geminiClaudeCarrierText,
						)
					}
				} else {
					signatureTargetsVisibleText := false
					if hasThoughtSignature {
						signatureTargetsVisibleText = appendPartSignature(
							thoughtSignatureResult.String(),
							geminiClaudeCarrierNext,
							geminiClaudeCarrierText,
						)
					}
					if params.ResponseType == 1 {
						data, _ := sjson.SetBytes(
							[]byte(fmt.Sprintf(
								`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`,
								params.ResponseIndex,
							)),
							"delta.text",
							partText,
						)
						appendEvent("content_block_delta", string(data))
						params.HasContent = true
					} else if partText != "" {
						closeCurrentBlock()
						appendEvent(
							"content_block_start",
							fmt.Sprintf(
								`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`,
								params.ResponseIndex,
							),
						)
						data, _ := sjson.SetBytes(
							[]byte(fmt.Sprintf(
								`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`,
								params.ResponseIndex,
							)),
							"delta.text",
							partText,
						)
						appendEvent("content_block_delta", string(data))
						params.ResponseType = 1
						params.HasContent = true
					}
					if partText != "" {
						params.HasSemanticContent = true
						params.LastSemanticKind = geminiClaudeCarrierText
						if signatureTargetsVisibleText {
							closeCurrentBlock()
						}
					}
				}
			} else if functionCallResult.Exists() {
				toolSignature := thoughtSignatureResult.String()
				if cache.GetModelGroup(modelName) != "claude" {
					appendPartSignature(
						toolSignature,
						geminiClaudeCarrierNext,
						geminiClaudeCarrierFunction,
					)
				}
				params.HasToolUse = true
				fcName := restoreClaudeFunctionNameFromAntigravity(
					params.ToolNameMap,
					functionCallResult.Get("name").String(),
				)
				closeCurrentBlock()

				data := []byte(fmt.Sprintf(
					`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`,
					params.ResponseIndex,
				))
				fallbackID := fmt.Sprintf(
					"%s-%d-%d",
					fcName,
					time.Now().UnixNano(),
					toolUseIDCounter.Add(1),
				)
				data, _ = sjson.SetBytes(
					data,
					"content_block.id",
					antigravityClaudeToolUseID(modelName, functionCallResult, fallbackID),
				)
				data, _ = sjson.SetBytes(data, "content_block.name", fcName)
				if cache.GetModelGroup(modelName) == "claude" && toolSignature != "" {
					data, _ = sjson.SetBytes(
						data,
						"content_block.signature",
						formatClaudeSignatureValue(modelName, toolSignature),
					)
				}
				appendEvent("content_block_start", string(data))

				if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
					data, _ = sjson.SetBytes(
						[]byte(fmt.Sprintf(
							`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`,
							params.ResponseIndex,
						)),
						"delta.partial_json",
						fcArgsResult.Raw,
					)
					appendEvent("content_block_delta", string(data))
				}
				params.ResponseType = 3
				params.HasContent = true
				params.HasSemanticContent = true
				params.LastSemanticKind = geminiClaudeCarrierFunction
			}
		}
	}

	if finishReasonResult := gjson.GetBytes(
		rawJSON, "response.candidates.0.finishReason",
	); finishReasonResult.Exists() {
		params.HasFinishReason = true
		params.FinishReason = finishReasonResult.String()
	}

	if webSearchStreamMode && !params.HasWebSearchTool && params.HasFinishReason && params.WebSearchTextBuffer.Len() > 0 {
		appendBufferedWebSearchTextBlock(params, appendEvent)
	}

	if params.Usage.Seen && params.HasFinishReason {
		appendFinalEvents(params, &output, false)
	}

	return [][]byte{output}
}

func appendWebSearchBufferedText(partsResult gjson.Result, buffer *strings.Builder) {
	for _, partResult := range partsResult.Array() {
		if partResult.Get("thought").Bool() || partResult.Get("functionCall").Exists() {
			continue
		}
		if partTextResult := partResult.Get("text"); partTextResult.Exists() {
			buffer.WriteString(partTextResult.String())
		}
	}
}

func appendBufferedWebSearchTextBlock(params *params, appendEvent func(string, string)) {
	text := params.WebSearchTextBuffer.String()
	params.WebSearchTextBuffer.Reset()
	if text == "" {
		return
	}
	appendEvent(
		"content_block_start",
		fmt.Sprintf(
			`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`,
			params.ResponseIndex,
		),
	)
	data, _ := sjson.SetBytes(
		[]byte(fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`, params.ResponseIndex,
		)),
		"delta.text",
		text,
	)
	appendEvent("content_block_delta", string(data))
	params.ResponseType = 1
	params.HasContent = true
}

func appendFinalEvents(params *params, output *[]byte, force bool) {
	if params.HasSentFinalEvents && force {
		return
	}

	if !params.Usage.Seen && !force {
		return
	}

	// Only send final events if we have actually output content
	if !params.HasContent {
		return
	}

	if params.ResponseType != 0 {
		*output = translatorcommon.AppendSSEEventString(
			*output, "content_block_stop",
			fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, params.ResponseIndex), 3,
		)
		params.ResponseType = 0
	}

	usageOutputTokens := params.Usage.OutputTokens()
	if usageOutputTokens == 0 && params.Usage.TotalTokens > 0 {
		usageOutputTokens = max(params.Usage.TotalTokens-params.Usage.PromptTokens, 0)
	}

	delta := []byte(fmt.Sprintf(
		`{"type":"message_delta","delta":{},"usage":{"input_tokens":%d,"output_tokens":%d}}`,
		max(params.Usage.PromptTokens-params.Usage.CachedTokens, 0), usageOutputTokens,
	))
	if !params.HasSentFinalEvents {
		delta, _ = sjson.SetBytes(delta, "delta.stop_reason", resolveStopReason(params))
		delta, _ = sjson.SetBytes(delta, "delta.stop_sequence", nil)
	}
	if params.WebSearchRequests > 0 {
		delta, _ = sjson.SetBytes(delta, "usage.server_tool_use.web_search_requests", params.WebSearchRequests)
	}
	// Add cache_read_input_tokens if cached tokens are present (indicates prompt caching is working)
	if params.Usage.HasCachedTokens {
		var err error
		delta, err = sjson.SetBytes(delta, "usage.cache_read_input_tokens", params.Usage.CachedTokens)
		if err != nil {
			log.Warnf("antigravity claude response: failed to set cache_read_input_tokens: %v", err)
		}
	}
	*output = translatorcommon.AppendSSEEventString(*output, "message_delta", string(delta), 3)

	params.HasSentFinalEvents = true
}

func resolveStopReason(params *params) string {
	if params.HasToolUse {
		return "tool_use"
	}

	switch params.FinishReason {
	case "MAX_TOKENS":
		return "max_tokens"
	case "STOP", "FINISH_REASON_UNSPECIFIED", "UNKNOWN":
		return "end_turn"
	}

	return "end_turn"
}

// convertAntigravityResponseToClaudeNonStream converts a non-streaming Gemini CLI response to a non-streaming Claude response.
//
// Parameters:
//   - ctx: The context for the request.
//   - modelName: The name of the model.
//   - rawJSON: The raw JSON response from the Gemini CLI API.
//   - param: A pointer to a parameter object for the conversion.
//
// Returns:
//   - []byte: A Claude-compatible JSON response.
func convertAntigravityResponseToClaudeNonStream(
	_ context.Context,
	_ string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	_ *any,
) []byte {
	toolNameMap := util.DisambiguatedToolNameMap(originalRequestRawJSON)
	modelName := gjson.GetBytes(requestRawJSON, "model").String()

	root := gjson.ParseBytes(rawJSON)
	var tokenUsage geminiopenai.GeminiUsage
	tokenUsage.Update(root.Get("response.cpaUsageMetadata"))
	tokenUsage.Update(root.Get("response.usageMetadata"))
	outputTokens := tokenUsage.OutputTokens()
	if outputTokens == 0 && tokenUsage.TotalTokens > 0 {
		outputTokens = max(tokenUsage.TotalTokens-tokenUsage.PromptTokens, 0)
	}

	responseJSON := []byte(`{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	responseJSON, _ = sjson.SetBytes(responseJSON, "id", root.Get("response.responseId").String())
	responseJSON, _ = sjson.SetBytes(responseJSON, "model", root.Get("response.modelVersion").String())
	responseJSON, _ = sjson.SetBytes(responseJSON, "usage.input_tokens", max(tokenUsage.PromptTokens-tokenUsage.CachedTokens, 0))
	responseJSON, _ = sjson.SetBytes(responseJSON, "usage.output_tokens", outputTokens)
	// Add cache_read_input_tokens if cached tokens are present (indicates prompt caching is working)
	if tokenUsage.HasCachedTokens {
		var err error
		responseJSON, err = sjson.SetBytes(responseJSON, "usage.cache_read_input_tokens", tokenUsage.CachedTokens)
		if err != nil {
			log.Warnf("antigravity claude response: failed to set cache_read_input_tokens: %v", err)
		}
	}

	if shouldTranslateWebSearchGrounding(originalRequestRawJSON, requestRawJSON) {
		if groundingMetadata := antigravityGroundingMetadata(root); groundingMetadata.Exists() {
			toolUseID := newClaudeWebSearchToolUseID()
			responseJSON, _ = sjson.SetRawBytes(
				responseJSON,
				"content",
				buildClaudeWebSearchContent(toolUseID, antigravityTextContent(root), groundingMetadata),
			)
			responseJSON, _ = sjson.SetBytes(responseJSON, "stop_reason", "end_turn")
			responseJSON, _ = sjson.SetBytes(responseJSON, "usage.server_tool_use.web_search_requests", 1)
			return responseJSON
		}
	}

	contentArrayInitialized := false
	ensureContentArray := func() {
		if contentArrayInitialized {
			return
		}
		responseJSON, _ = sjson.SetRawBytes(responseJSON, "content", []byte("[]"))
		contentArrayInitialized = true
	}

	parts := root.Get("response.candidates.0.content.parts")
	textBuilder := strings.Builder{}
	thinkingBuilder := strings.Builder{}
	thinkingSignature := ""
	thinkingSignatureDirection := geminiClaudeCarrierStandalone
	thinkingSignatureTargetKind := geminiClaudeCarrierText
	toolIDCounter := 0
	hasToolCall := false
	hasSemanticContent := false
	lastSemanticKind := geminiClaudeCarrierAny

	flushText := func() {
		if textBuilder.Len() == 0 {
			return
		}
		ensureContentArray()
		block := []byte(`{"type":"text","text":""}`)
		block, _ = sjson.SetBytes(block, "text", textBuilder.String())
		responseJSON, _ = sjson.SetRawBytes(responseJSON, "content.-1", block)
		textBuilder.Reset()
	}

	flushThinking := func() {
		if thinkingBuilder.Len() == 0 && thinkingSignature == "" {
			return
		}
		ensureContentArray()
		block := []byte(`{"type":"thinking","thinking":""}`)
		block, _ = sjson.SetBytes(block, "thinking", thinkingBuilder.String())
		if thinkingSignature != "" {
			block, _ = sjson.SetBytes(
				block,
				"signature",
				formatGeminiClaudeCarrierValue(
					modelName,
					thinkingSignature,
					thinkingSignatureDirection,
					thinkingSignatureTargetKind,
				),
			)
		}
		responseJSON, _ = sjson.SetRawBytes(responseJSON, "content.-1", block)
		thinkingBuilder.Reset()
		thinkingSignature = ""
		thinkingSignatureDirection = geminiClaudeCarrierStandalone
		thinkingSignatureTargetKind = geminiClaudeCarrierText
	}

	appendSignatureCarrier := func(rawSignature, direction, targetKind string) {
		if rawSignature == "" {
			return
		}
		ensureContentArray()
		carrier := []byte(`{"type":"thinking","thinking":"","signature":""}`)
		carrier, _ = sjson.SetBytes(
			carrier,
			"signature",
			formatGeminiClaudeCarrierValue(modelName, rawSignature, direction, targetKind),
		)
		responseJSON, _ = sjson.SetRawBytes(responseJSON, "content.-1", carrier)
	}

	if parts.IsArray() {
		for _, part := range parts.Array() {
			sig := part.Get("thoughtSignature")
			if !sig.Exists() {
				sig = part.Get("thought_signature")
			}
			rawSignature := ""
			if sig.Exists() {
				rawSignature = sig.String()
			}

			if functionCall := part.Get("functionCall"); functionCall.Exists() {
				signatureAttachedToThought := false
				isClaudeTarget := cache.GetModelGroup(modelName) == "claude"
				if !isClaudeTarget && rawSignature != "" &&
					thinkingBuilder.Len() > 0 && thinkingSignature == "" {
					thinkingSignature = rawSignature
					thinkingSignatureDirection = geminiClaudeCarrierNext
					thinkingSignatureTargetKind = geminiClaudeCarrierFunction
					signatureAttachedToThought = true
				}
				flushThinking()
				flushText()
				hasToolCall = true

				name := restoreClaudeFunctionNameFromAntigravity(toolNameMap, functionCall.Get("name").String())
				toolIDCounter++
				if !isClaudeTarget && rawSignature != "" && !signatureAttachedToThought {
					appendSignatureCarrier(
						rawSignature,
						geminiClaudeCarrierNext,
						geminiClaudeCarrierFunction,
					)
				}
				toolBlock := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
				toolBlock, _ = sjson.SetBytes(
					toolBlock,
					"id",
					antigravityClaudeToolUseID(modelName, functionCall, fmt.Sprintf("tool_%d", toolIDCounter)),
				)
				toolBlock, _ = sjson.SetBytes(toolBlock, "name", name)
				if isClaudeTarget && rawSignature != "" {
					toolBlock, _ = sjson.SetBytes(
						toolBlock,
						"signature",
						formatClaudeSignatureValue(modelName, rawSignature),
					)
				}

				if args := functionCall.Get("args"); args.Exists() &&
					args.Raw != "" && gjson.Valid(args.Raw) && args.IsObject() {
					toolBlock, _ = sjson.SetRawBytes(toolBlock, "input", []byte(args.Raw))
				}

				ensureContentArray()
				responseJSON, _ = sjson.SetRawBytes(responseJSON, "content.-1", toolBlock)
				hasSemanticContent = true
				lastSemanticKind = geminiClaudeCarrierFunction
				continue
			}

			text := part.Get("text")
			if part.Get("thought").Bool() {
				flushText()
				if thinkingSignature != "" {
					flushThinking()
				}
				if text.Exists() && text.String() != "" {
					thinkingBuilder.WriteString(text.String())
					hasSemanticContent = true
					lastSemanticKind = geminiClaudeCarrierText
				}
				if rawSignature != "" {
					if thinkingBuilder.Len() > 0 {
						thinkingSignature = rawSignature
						thinkingSignatureDirection = geminiClaudeCarrierStandalone
						thinkingSignatureTargetKind = geminiClaudeCarrierText
						flushThinking()
					} else if hasSemanticContent {
						appendSignatureCarrier(
							rawSignature,
							geminiClaudeCarrierPrevious,
							lastSemanticKind,
						)
					} else {
						appendSignatureCarrier(
							rawSignature,
							geminiClaudeCarrierNext,
							geminiClaudeCarrierAny,
						)
					}
				}
				continue
			}

			visibleSignatureCarrier := false
			if rawSignature != "" {
				if thinkingBuilder.Len() > 0 && thinkingSignature == "" {
					thinkingSignature = rawSignature
					thinkingSignatureDirection = geminiClaudeCarrierNext
					thinkingSignatureTargetKind = geminiClaudeCarrierText
					flushThinking()
				} else {
					flushThinking()
					flushText()
					if text.Exists() && text.String() != "" {
						appendSignatureCarrier(
							rawSignature,
							geminiClaudeCarrierNext,
							geminiClaudeCarrierText,
						)
						visibleSignatureCarrier = true
					} else if hasSemanticContent {
						appendSignatureCarrier(
							rawSignature,
							geminiClaudeCarrierPrevious,
							lastSemanticKind,
						)
					} else {
						appendSignatureCarrier(
							rawSignature,
							geminiClaudeCarrierNext,
							geminiClaudeCarrierAny,
						)
					}
				}
			}
			if text.Exists() && text.String() != "" {
				flushThinking()
				textBuilder.WriteString(text.String())
				hasSemanticContent = true
				lastSemanticKind = geminiClaudeCarrierText
				if visibleSignatureCarrier {
					flushText()
				}
			}
		}
	}

	flushThinking()
	flushText()

	stopReason := "end_turn"
	if hasToolCall {
		stopReason = "tool_use"
	} else {
		if finish := root.Get("response.candidates.0.finishReason"); finish.Exists() {
			switch finish.String() {
			case "MAX_TOKENS":
				stopReason = "max_tokens"
			case "STOP", "FINISH_REASON_UNSPECIFIED", "UNKNOWN":
				stopReason = "end_turn"
			default:
				stopReason = "end_turn"
			}
		}
	}
	responseJSON, _ = sjson.SetBytes(responseJSON, "stop_reason", stopReason)

	if tokenUsage.PromptTokens == 0 && outputTokens == 0 {
		if !root.Get("response.usageMetadata").Exists() && !root.Get("response.cpaUsageMetadata").Exists() {
			responseJSON, _ = sjson.DeleteBytes(responseJSON, "usage")
		}
	}

	return responseJSON
}

func tokenCount(_ context.Context, count int64) []byte {
	return translatorcommon.ClaudeInputTokensJSON(count)
}
