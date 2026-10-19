// Package claude provides response translation functionality for Claude API.
// This package handles the conversion of backend client responses into Claude-compatible
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

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	geminiopenai "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// params holds parameters for response conversion.
type params struct {
	IsGlAPIKey       bool
	HasFirstResponse bool
	ResponseType     int
	ResponseIndex    int
	HasContent       bool // Tracks whether any content (text, thinking, or tool use) has been output
	ToolNameMap      map[string]string
	ToolCallCounter  int
	SawToolCall      bool
	HasFinalEvents   bool
	Done             bool
	FinishReason     string
	Usage            geminiopenai.GeminiUsage
}

func geminiClaudeStopReason(finishReason string, sawToolCall bool) string {
	if sawToolCall {
		return "tool_use"
	}
	switch finishReason {
	case "MAX_TOKENS":
		return "max_tokens"
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "SPII", "BLOCKLIST", "MALFORMED_FUNCTION_CALL", "IMAGE_SAFETY":
		return "refusal"
	default:
		return "end_turn"
	}
}

func compatibleGeminiPartSignature(part gjson.Result, blockKind signature.BlockKind) (string, bool) {
	for _, path := range []string{"thoughtSignature", "thought_signature"} {
		value := part.Get(path)
		if value.Type != gjson.String || strings.TrimSpace(value.String()) == "" {
			continue
		}
		return signature.CompatibleSignatureForProvider(
			signature.ProviderGemini,
			value.String(),
			blockKind,
		)
	}
	return "", false
}

func nextClaudeToolUseID(state *params, functionCall gjson.Result) string {
	if nativeID := translatorcommon.GeminiToolCallID(functionCall); nativeID != "" {
		return util.SanitizeClaudeToolID(nativeID)
	}
	state.ToolCallCounter++
	return fmt.Sprintf("toolu_gemini_%016d", state.ToolCallCounter)
}

func claudeUsageFromGemini(usage gjson.Result) (inputTokens, outputTokens, cacheReadTokens int64) {
	var counters geminiopenai.GeminiUsage
	counters.Update(usage)
	cacheReadTokens = counters.CachedTokens
	inputTokens = max(counters.PromptTokens-cacheReadTokens, 0)
	outputTokens = counters.OutputTokens()
	return inputTokens, outputTokens, cacheReadTokens
}

// convertGeminiResponseToClaude performs sophisticated streaming response format conversion.
// This function implements a complex state machine that translates backend client responses
// into Claude-compatible Server-Sent Events (SSE) format. It manages different response types
// and handles state transitions between content blocks, thinking processes, and function calls.
//
// Response type states: 0=none, 1=content, 2=thinking, 3=function
// The function maintains state across multiple calls to ensure proper SSE event sequencing.
//
// Parameters:
//   - ctx: The context for the request.
//   - modelName: The name of the model.
//   - rawJSON: The raw JSON response from the Gemini API.
//   - param: A pointer to a parameter object for the conversion.
//
// Returns:
//   - [][]byte: A slice of bytes, each containing a Claude-compatible SSE payload.
func convertGeminiResponseToClaude(
	_ context.Context,
	_ string,
	originalRequestRawJSON, _, rawJSON []byte,
	param *any,
) [][]byte {
	if *param == nil {
		*param = &params{
			IsGlAPIKey:       false,
			HasFirstResponse: false,
			ResponseType:     0,
			ResponseIndex:    0,
			ToolNameMap:      util.ToolNameMapFromClaudeRequest(originalRequestRawJSON),
			SawToolCall:      false,
		}
	}

	output := make([]byte, 0, 1024)
	appendEvent := func(event, payload string) {
		output = translatorcommon.AppendSSEEventString(output, event, payload, 3)
	}
	state := (*param).(*params)
	if state.Done {
		return nil
	}
	appendUsageDelta := func(final bool) {
		template := []byte(`{"type":"message_delta","delta":{},"usage":{"input_tokens":0,"output_tokens":0}}`)
		if final {
			template, _ = sjson.SetBytes(template, "delta.stop_reason", geminiClaudeStopReason(state.FinishReason, state.SawToolCall))
			template, _ = sjson.SetBytes(template, "delta.stop_sequence", nil)
		}
		template, _ = sjson.SetBytes(template, "usage.input_tokens", max(state.Usage.PromptTokens-state.Usage.CachedTokens, 0))
		template, _ = sjson.SetBytes(template, "usage.output_tokens", state.Usage.OutputTokens())
		if state.Usage.HasCachedTokens {
			template, _ = sjson.SetBytes(template, "usage.cache_read_input_tokens", state.Usage.CachedTokens)
		}
		appendEvent("message_delta", string(template))
	}
	finishMessage := func() {
		if !state.HasFirstResponse {
			return
		}
		if !state.HasContent {
			appendEvent("content_block_start", fmt.Sprintf(
				`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, state.ResponseIndex,
			))
			state.ResponseType = 1
			state.HasContent = true
		}
		if state.ResponseType != 0 {
			appendEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, state.ResponseIndex))
			state.ResponseType = 0
		}
		if !state.HasFinalEvents {
			appendUsageDelta(true)
			state.HasFinalEvents = true
		}
	}
	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		if !state.HasFirstResponse {
			return [][]byte{}
		}
		finishMessage()
		appendEvent("message_stop", `{"type":"message_stop"}`)
		state.Done = true
		return [][]byte{output}
	}

	appendSignatureDelta := func(signature string) {
		state := (*param).(*params)
		if signature == "" {
			return
		}
		if state.ResponseType != 2 {
			if state.ResponseType != 0 {
				appendEvent(
					"content_block_stop",
					fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, state.ResponseIndex),
				)
				state.ResponseIndex++
			}
			appendEvent(
				"content_block_start",
				fmt.Sprintf(
					`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`,
					state.ResponseIndex,
				),
			)
			state.ResponseType = 2
		}
		data, _ := sjson.SetBytes(
			[]byte(fmt.Sprintf(
				`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":""}}`,
				state.ResponseIndex,
			)), "delta.signature", signature,
		)
		appendEvent("content_block_delta", string(data))
		state.HasContent = true
	}
	appendSignatureCarrier := func(signature string) {
		if signature == "" {
			return
		}
		if state.ResponseType != 0 {
			appendEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, state.ResponseIndex))
			state.ResponseIndex++
		}
		appendEvent("content_block_start", fmt.Sprintf(
			`{"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`, state.ResponseIndex,
		))
		data, _ := sjson.SetBytes(
			[]byte(fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":""}}`, state.ResponseIndex)),
			"delta.signature", signature,
		)
		appendEvent("content_block_delta", string(data))
		appendEvent("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, state.ResponseIndex))
		state.ResponseIndex++
		state.ResponseType = 0
		state.HasContent = true
	}

	// Initialize the streaming session with a message_start event
	// This is only sent for the very first response chunk
	if !(*param).(*params).HasFirstResponse {
		// Create the initial message structure with default values
		// This follows the Claude API specification for streaming message initialization
		messageStartTemplate := []byte(`{"type":"message_start","message":{"id":"msg_1nZdL29xx5MUA1yADyHTEsnR8uuvGzszyY","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet-20241022","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`)

		// Override default values with actual response metadata if available
		if modelVersionResult := gjson.GetBytes(rawJSON, "modelVersion"); modelVersionResult.Exists() {
			messageStartTemplate, _ = sjson.SetBytes(messageStartTemplate, "message.model", modelVersionResult.String())
		}
		if responseIDResult := gjson.GetBytes(rawJSON, "responseId"); responseIDResult.Exists() {
			messageStartTemplate, _ = sjson.SetBytes(messageStartTemplate, "message.id", responseIDResult.String())
		}
		appendEvent("message_start", string(messageStartTemplate))

		(*param).(*params).HasFirstResponse = true
	}

	// Process the response parts array from the backend client
	// Each part can contain text content, thinking content, or function calls
	partsResult := gjson.GetBytes(rawJSON, "candidates.0.content.parts")
	if partsResult.IsArray() {
		partResults := partsResult.Array()
		for i := range partResults {
			partResult := partResults[i]

			// Extract the different types of content from each part
			partTextResult := partResult.Get("text")
			functionCallResult := partResult.Get("functionCall")
			blockKind := signature.BlockKindGeminiModelPart
			if functionCallResult.Exists() {
				blockKind = signature.BlockKindGeminiFunctionCall
			}
			thoughtSignature, hasThoughtSignature := compatibleGeminiPartSignature(partResult, blockKind)

			if hasThoughtSignature && !partTextResult.Exists() && !functionCallResult.Exists() {
				if state.ResponseType == 2 {
					appendSignatureDelta(thoughtSignature)
				} else {
					appendSignatureCarrier(thoughtSignature)
				}
				continue
			}

			// Handle text content (both regular content and thinking)
			if partTextResult.Exists() {
				isThinking := partResult.Get("thought").Bool()
				if hasThoughtSignature && partTextResult.String() == "" {
					if state.ResponseType == 2 {
						appendSignatureDelta(thoughtSignature)
					} else {
						appendSignatureCarrier(thoughtSignature)
					}
					continue
				}
				if hasThoughtSignature && !isThinking {
					appendSignatureCarrier(thoughtSignature)
				}
				blockState := &translatorcommon.ClaudeBlockState{
					ResponseType:  (*param).(*params).ResponseType,
					ResponseIndex: (*param).(*params).ResponseIndex,
					HasContent:    (*param).(*params).HasContent,
				}
				for _, ev := range translatorcommon.EmitTextOrThinkingBlock(
					blockState, partTextResult.String(), isThinking,
				) {
					appendEvent(ev.Event, ev.Data)
				}
				(*param).(*params).ResponseType = blockState.ResponseType
				(*param).(*params).ResponseIndex = blockState.ResponseIndex
				(*param).(*params).HasContent = blockState.HasContent
				if isThinking {
					appendSignatureDelta(thoughtSignature)
				}
			} else if functionCallResult.Exists() {
				// Handle function/tool calls from the AI model
				// This processes tool usage requests and formats them for Claude API compatibility
				(*param).(*params).SawToolCall = true
				upstreamToolName := functionCallResult.Get("name").String()
				clientToolName := util.MapToolName((*param).(*params).ToolNameMap, upstreamToolName)

				// FIX: Handle streaming split/delta where name might be empty in subsequent chunks.
				// If we are already in tool use mode and name is empty, treat as continuation (delta).
				if (*param).(*params).ResponseType == 3 && upstreamToolName == "" {
					if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
						data, _ := sjson.SetBytes(
							[]byte(fmt.Sprintf(
								`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`,
								(*param).(*params).ResponseIndex,
							)), "delta.partial_json", fcArgsResult.Raw,
						)
						appendEvent("content_block_delta", string(data))
					}
					if hasThoughtSignature {
						appendSignatureCarrier(thoughtSignature)
					}
					continue
				}

				// Handle state transitions when switching to function calls
				// Close any existing function call block first
				if (*param).(*params).ResponseType == 3 {
					appendEvent(
						"content_block_stop",
						fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, (*param).(*params).ResponseIndex),
					)
					(*param).(*params).ResponseIndex++
					(*param).(*params).ResponseType = 0
				}

				// Special handling for thinking state transition
				if (*param).(*params).ResponseType == 2 {
					// output = output + "event: content_block_delta\n"
					// output = output + fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":{"type":"signature_delta","signature":null}}`, (*param).(*Params).ResponseIndex)
					// output = output + "\n\n\n"
				}

				// Close any other existing content block
				if (*param).(*params).ResponseType != 0 {
					appendEvent(
						"content_block_stop",
						fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, (*param).(*params).ResponseIndex),
					)
					(*param).(*params).ResponseIndex++
				}

				// Start a new tool use content block
				// This creates the structure for a function call in Claude format
				// Create the tool use block with unique ID and function details
				data := []byte(fmt.Sprintf(
					`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`,
					(*param).(*params).ResponseIndex,
				))
				data, _ = sjson.SetBytes(
					data,
					"content_block.id",
					nextClaudeToolUseID((*param).(*params), functionCallResult),
				)
				data, _ = sjson.SetBytes(data, "content_block.name", clientToolName)
				if hasThoughtSignature {
					data, _ = sjson.SetBytes(data, "content_block.signature", thoughtSignature)
				}
				appendEvent("content_block_start", string(data))

				if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
					data, _ = sjson.SetBytes(
						[]byte(fmt.Sprintf(
							`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`,
							(*param).(*params).ResponseIndex,
						)), "delta.partial_json", fcArgsResult.Raw,
					)
					appendEvent("content_block_delta", string(data))
				}
				(*param).(*params).ResponseType = 3
				(*param).(*params).HasContent = true
			}
		}
	}

	if finish := gjson.GetBytes(rawJSON, "candidates.0.finishReason"); finish.Exists() {
		state.FinishReason = finish.String()
	}
	usageResult := gjson.GetBytes(rawJSON, "usageMetadata")
	previousUsage := state.Usage
	state.Usage.Update(gjson.GetBytes(rawJSON, "cpaUsageMetadata"))
	state.Usage.Update(usageResult)
	if state.HasFinalEvents && state.Usage != previousUsage {
		appendUsageDelta(false)
	}
	if usageResult.Exists() && bytes.Contains(rawJSON, []byte(`"finishReason"`)) && !state.HasFinalEvents {
		finishMessage()
	}

	return [][]byte{output}
}

// convertGeminiResponseToClaudeNonStream converts a non-streaming Gemini response to a non-streaming Claude response.
//
// Parameters:
//   - ctx: The context for the request.
//   - modelName: The name of the model.
//   - rawJSON: The raw JSON response from the Gemini API.
//   - param: A pointer to a parameter object for the conversion.
//
// Returns:
//   - []byte: A Claude-compatible JSON response.
func convertGeminiResponseToClaudeNonStream(
	_ context.Context,
	_ string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	_ *any,
) []byte {
	_ = requestRawJSON

	root := gjson.ParseBytes(rawJSON)
	toolNameMap := util.ToolNameMapFromClaudeRequest(originalRequestRawJSON)

	out := []byte(`{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	out, _ = sjson.SetBytes(out, "id", root.Get("responseId").String())
	out, _ = sjson.SetBytes(out, "model", root.Get("modelVersion").String())

	usageMetadata := root.Get("usageMetadata")
	inputTokens, outputTokens, cacheReadTokens := claudeUsageFromGemini(usageMetadata)
	out, _ = sjson.SetBytes(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.SetBytes(out, "usage.output_tokens", outputTokens)
	if usageMetadata.Get("cachedContentTokenCount").Exists() {
		out, _ = sjson.SetBytes(out, "usage.cache_read_input_tokens", cacheReadTokens)
	}

	parts := root.Get("candidates.0.content.parts")
	textBuilder := strings.Builder{}
	thinkingBuilder := strings.Builder{}
	thinkingSignature := ""
	toolState := &params{}
	hasToolCall := false

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
		if thinkingBuilder.Len() == 0 && thinkingSignature == "" {
			return
		}
		block := []byte(`{"type":"thinking","thinking":""}`)
		block, _ = sjson.SetBytes(block, "thinking", thinkingBuilder.String())
		if thinkingSignature != "" {
			block, _ = sjson.SetBytes(block, "signature", thinkingSignature)
		}
		out, _ = sjson.SetRawBytes(out, "content.-1", block)
		thinkingBuilder.Reset()
		thinkingSignature = ""
	}

	if parts.IsArray() {
		for _, part := range parts.Array() {
			if functionCall := part.Get("functionCall"); functionCall.Exists() {
				flushThinking()
				flushText()
				hasToolCall = true

				upstreamToolName := functionCall.Get("name").String()
				clientToolName := util.MapToolName(toolNameMap, upstreamToolName)
				toolBlock := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
				toolBlock, _ = sjson.SetBytes(toolBlock, "id", nextClaudeToolUseID(toolState, functionCall))
				toolBlock, _ = sjson.SetBytes(toolBlock, "name", clientToolName)
				if thoughtSignature, ok := compatibleGeminiPartSignature(
					part,
					signature.BlockKindGeminiFunctionCall,
				); ok {
					toolBlock, _ = sjson.SetBytes(toolBlock, "signature", thoughtSignature)
				}
				inputRaw := "{}"
				if args := functionCall.Get("args"); args.Exists() && gjson.Valid(args.Raw) && args.IsObject() {
					inputRaw = args.Raw
				}
				toolBlock, _ = sjson.SetRawBytes(toolBlock, "input", []byte(inputRaw))
				out, _ = sjson.SetRawBytes(out, "content.-1", toolBlock)
				continue
			}

			thoughtSignature, hasThoughtSignature := compatibleGeminiPartSignature(
				part,
				signature.BlockKindGeminiModelPart,
			)
			if text := part.Get("text"); text.Exists() {
				isThinking := part.Get("thought").Bool()
				if isThinking || (hasThoughtSignature && text.String() == "") {
					flushText()
					if !isThinking && thinkingBuilder.Len() == 0 {
						flushThinking()
					}
					thinkingBuilder.WriteString(text.String())
					if hasThoughtSignature {
						thinkingSignature = thoughtSignature
					}
					continue
				}
				if text.String() != "" {
					flushThinking()
					if hasThoughtSignature {
						flushText()
						thinkingSignature = thoughtSignature
						flushThinking()
					}
					textBuilder.WriteString(text.String())
				}
				continue
			}
			if hasThoughtSignature {
				flushText()
				if thinkingBuilder.Len() == 0 {
					flushThinking()
				}
				thinkingSignature = thoughtSignature
			}
		}
	}

	flushThinking()
	flushText()

	out, _ = sjson.SetBytes(out, "stop_reason", geminiClaudeStopReason(root.Get("candidates.0.finishReason").String(), hasToolCall))

	if inputTokens == int64(0) && outputTokens == int64(0) && !root.Get("usageMetadata").Exists() {
		out, _ = sjson.DeleteBytes(out, "usage")
	}

	return out
}

func tokenCount(_ context.Context, count int64) []byte {
	return translatorcommon.ClaudeInputTokensJSON(count)
}
