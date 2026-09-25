// Package gemini provides response translation functionality for Claude Code to Gemini API compatibility.
// This package handles the conversion of Claude Code API responses into Gemini-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by Gemini API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, and usage metadata appropriately.
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

func geminiResponseSignature(rawSignature string, blockKind signature.BlockKind) string {
	if strings.TrimSpace(rawSignature) == "" {
		return ""
	}
	if normalized, ok := signature.CompatibleSignatureForProvider(
		signature.ProviderGemini,
		rawSignature,
		blockKind,
	); ok {
		return normalized
	}
	return signature.GeminiReplaySignatureOrBypass(rawSignature, blockKind)
}

func rawClaudeContentBlockSignature(block gjson.Result) string {
	for _, path := range []string{
		"signature",
		"thought_signature",
		"extra_content.google.thought_signature",
	} {
		if value := strings.TrimSpace(block.Get(path).String()); value != "" {
			return value
		}
	}
	return ""
}

func claudeContentBlockSignature(block gjson.Result, blockKind signature.BlockKind) string {
	return geminiResponseSignature(rawClaudeContentBlockSignature(block), blockKind)
}

func appendClaudeThinkingSignature(
	state *convertAnthropicResponseToGeminiParams,
	index int,
	fragment string,
) {
	fragment = strings.TrimSpace(fragment)
	if state == nil || fragment == "" {
		return
	}
	if state.ThinkingSignatures == nil {
		state.ThinkingSignatures = make(map[int]*strings.Builder)
	}
	builder := state.ThinkingSignatures[index]
	if builder == nil {
		builder = &strings.Builder{}
		state.ThinkingSignatures[index] = builder
	}
	builder.WriteString(fragment)
}

func takeGeminiThinkingSignature(state *convertAnthropicResponseToGeminiParams, index int) string {
	if state == nil || state.ThinkingSignatures == nil {
		return ""
	}
	builder := state.ThinkingSignatures[index]
	delete(state.ThinkingSignatures, index)
	if builder == nil {
		return ""
	}
	return geminiResponseSignature(builder.String(), signature.BlockKindGeminiModelPart)
}

// convertAnthropicResponseToGeminiParams holds parameters for response conversion
// It also carries minimal streaming state across calls to assemble tool_use input_json_delta.
// This structure maintains state information needed for proper conversion of streaming responses
// from Claude Code format to Gemini format, particularly for handling tool calls that span
// multiple streaming events.
type convertAnthropicResponseToGeminiParams struct {
	Model             string
	CreatedAt         int64
	ResponseID        string
	LastStorageOutput []byte
	IsStreaming       bool

	// Streaming state for tool_use assembly
	// Keyed by content_block index from Claude SSE events
	ToolUseNames       map[int]string           // function/tool name per block index
	ToolUseArgs        map[int]*strings.Builder // accumulates partial_json across deltas
	ToolUseIDs         map[int]string           // provider-visible tool ID per block index
	ToolUseSignatures  map[int]string           // Gemini thought signature per tool block index
	ThinkingSignatures map[int]*strings.Builder // raw Claude signature fragments per thinking block index
}

// ConvertClaudeResponseToGemini converts Claude Code streaming response format to Gemini format.
// This function processes various Claude Code event types and transforms them into Gemini-compatible JSON responses.
// It handles text content, tool calls, reasoning content, and usage metadata, outputting responses that match
// the Gemini API format. The function supports incremental updates for streaming responses and maintains
// state information to properly assemble multipart tool calls.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Claude Code API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of Gemini-compatible JSON responses
func ConvertClaudeResponseToGemini(_ context.Context, modelName string, _, _, rawJSON []byte, param *any) [][]byte {
	if *param == nil {
		*param = &convertAnthropicResponseToGeminiParams{
			Model:      modelName,
			CreatedAt:  0,
			ResponseID: "",
		}
	}

	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	root := gjson.ParseBytes(rawJSON)
	eventType := root.Get("type").String()

	// Base Gemini response template with default values
	template := []byte(`{"candidates":[{"content":{"role":"model","parts":[]}}],"usageMetadata":{"trafficType":"PROVISIONED_THROUGHPUT"},"modelVersion":"","createTime":"","responseId":""}`)

	// Set model version
	if (*param).(*convertAnthropicResponseToGeminiParams).Model != "" {
		// Map Claude model names back to Gemini model names
		template, _ = sjson.SetBytes(template, "modelVersion", (*param).(*convertAnthropicResponseToGeminiParams).Model)
	}

	// Set response ID and creation time
	if (*param).(*convertAnthropicResponseToGeminiParams).ResponseID != "" {
		template, _ = sjson.SetBytes(
			template, "responseId", (*param).(*convertAnthropicResponseToGeminiParams).ResponseID,
		)
	}

	// Set creation time to current time if not provided
	if (*param).(*convertAnthropicResponseToGeminiParams).CreatedAt == 0 {
		(*param).(*convertAnthropicResponseToGeminiParams).CreatedAt = time.Now().Unix()
	}
	template, _ = sjson.SetBytes(
		template, "createTime",
		time.Unix((*param).(*convertAnthropicResponseToGeminiParams).CreatedAt, 0).Format(time.RFC3339Nano),
	)

	switch eventType {
	case "message_start":
		// Initialize response with message metadata when a new message begins
		if message := root.Get("message"); message.Exists() {
			(*param).(*convertAnthropicResponseToGeminiParams).ResponseID = message.Get("id").String()
			(*param).(*convertAnthropicResponseToGeminiParams).Model = message.Get("model").String()
		}
		return [][]byte{}

	case "content_block_start":
		// Start of a content block - record tool_use or thinking state.
		if cb := root.Get("content_block"); cb.Exists() {
			params := (*param).(*convertAnthropicResponseToGeminiParams)
			idx := int(root.Get("index").Int())
			switch cb.Get("type").String() {
			case "tool_use":
				if params.ToolUseNames == nil {
					params.ToolUseNames = map[int]string{}
				}
				if name := cb.Get("name"); name.Exists() {
					params.ToolUseNames[idx] = name.String()
				}
				if toolID := strings.TrimSpace(cb.Get("id").String()); toolID != "" {
					if params.ToolUseIDs == nil {
						params.ToolUseIDs = map[int]string{}
					}
					params.ToolUseIDs[idx] = toolID
				}
				if thoughtSignature := claudeContentBlockSignature(
					cb,
					signature.BlockKindGeminiFunctionCall,
				); thoughtSignature != "" {
					if params.ToolUseSignatures == nil {
						params.ToolUseSignatures = map[int]string{}
					}
					params.ToolUseSignatures[idx] = thoughtSignature
				}
			case "thinking":
				appendClaudeThinkingSignature(params, idx, rawClaudeContentBlockSignature(cb))
			}
		}
		return [][]byte{}

	case "content_block_delta":
		// Handle content delta (text, thinking, or tool use arguments)
		if delta := root.Get("delta"); delta.Exists() {
			deltaType := delta.Get("type").String()

			switch deltaType {
			case "text_delta":
				// Regular text content delta for normal response text
				if text := delta.Get("text"); text.Exists() && text.String() != "" {
					textPart := []byte(`{"text":""}`)
					textPart, _ = sjson.SetBytes(textPart, "text", text.String())
					template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", textPart)
				}
			case "thinking_delta":
				// Thinking/reasoning content delta for models with reasoning capabilities
				if text := delta.Get("thinking"); text.Exists() && text.String() != "" {
					thinkingPart := []byte(`{"thought":true,"text":""}`)
					thinkingPart, _ = sjson.SetBytes(thinkingPart, "text", text.String())
					template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", thinkingPart)
				}
			case "signature_delta":
				idx := int(root.Get("index").Int())
				appendClaudeThinkingSignature(
					(*param).(*convertAnthropicResponseToGeminiParams),
					idx,
					delta.Get("signature").String(),
				)
				return [][]byte{}
			case "input_json_delta":
				// Tool use input delta - accumulate partial_JSON by index for later assembly at content_block_stop
				idx := int(root.Get("index").Int())
				if (*param).(*convertAnthropicResponseToGeminiParams).ToolUseArgs == nil {
					(*param).(*convertAnthropicResponseToGeminiParams).ToolUseArgs = map[int]*strings.Builder{}
				}
				b, ok := (*param).(*convertAnthropicResponseToGeminiParams).ToolUseArgs[idx]
				if !ok || b == nil {
					bb := &strings.Builder{}
					(*param).(*convertAnthropicResponseToGeminiParams).ToolUseArgs[idx] = bb
					b = bb
				}
				if pj := delta.Get("partial_json"); pj.Exists() {
					b.WriteString(pj.String())
				}
				return [][]byte{}
			}
		}
		return [][]byte{template}

	case "content_block_stop":
		// End of content block - finalize tool calls or an aggregated thinking signature.
		idx := int(root.Get("index").Int())
		// Claude's content_block_stop often doesn't include content_block payload (see docs/response-claude.txt)
		// So we finalize using accumulated state captured during content_block_start and input_json_delta.
		params := (*param).(*convertAnthropicResponseToGeminiParams)
		name := ""
		if params.ToolUseNames != nil {
			name = params.ToolUseNames[idx]
		}
		var argsTrim string
		if params.ToolUseArgs != nil {
			if builder := params.ToolUseArgs[idx]; builder != nil {
				argsTrim = strings.TrimSpace(builder.String())
			}
		}
		if name != "" || argsTrim != "" {
			functionCall := []byte(`{"functionCall":{"name":"","args":{}}}`)
			if name != "" {
				functionCall, _ = sjson.SetBytes(functionCall, "functionCall.name", name)
			}
			if argsTrim != "" {
				functionCall, _ = sjson.SetRawBytes(functionCall, "functionCall.args", []byte(argsTrim))
			}
			if toolID := params.ToolUseIDs[idx]; toolID != "" {
				functionCall, _ = sjson.SetBytes(functionCall, "functionCall.id", toolID)
			}
			if thoughtSignature := params.ToolUseSignatures[idx]; thoughtSignature != "" {
				functionCall, _ = sjson.SetBytes(functionCall, "thoughtSignature", thoughtSignature)
			}
			template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", functionCall)
			template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")
			params.LastStorageOutput = append([]byte(nil), template...)
			delete(params.ToolUseArgs, idx)
			delete(params.ToolUseNames, idx)
			delete(params.ToolUseIDs, idx)
			delete(params.ToolUseSignatures, idx)
			delete(params.ThinkingSignatures, idx)
			return [][]byte{template}
		}
		if thoughtSignature := takeGeminiThinkingSignature(params, idx); thoughtSignature != "" {
			thinkingPart := []byte(`{"thought":true,"thoughtSignature":""}`)
			thinkingPart, _ = sjson.SetBytes(thinkingPart, "thoughtSignature", thoughtSignature)
			template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", thinkingPart)
			return [][]byte{template}
		}
		return [][]byte{}

	case "message_delta":
		// Handle message-level changes (like stop reason and usage information)
		if delta := root.Get("delta"); delta.Exists() {
			if stopReason := delta.Get("stop_reason"); stopReason.Exists() {
				switch stopReason.String() {
				case "end_turn":
					template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")
				case "tool_use":
					template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")
				case "max_tokens":
					template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "MAX_TOKENS")
				case "stop_sequence":
					template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")
				default:
					template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")
				}
			}
		}

		if usage := root.Get("usage"); usage.Exists() {
			// Basic token counts for prompt and completion
			inputTokens := usage.Get("input_tokens").Int()
			outputTokens := usage.Get("output_tokens").Int()

			// Set basic usage metadata according to Gemini API specification
			template, _ = sjson.SetBytes(template, "usageMetadata.promptTokenCount", inputTokens)
			template, _ = sjson.SetBytes(template, "usageMetadata.candidatesTokenCount", outputTokens)
			template, _ = sjson.SetBytes(template, "usageMetadata.totalTokenCount", inputTokens+outputTokens)

			// Add cache-related token counts if present (Claude Code API cache fields)
			if cacheCreationTokens := usage.Get("cache_creation_input_tokens"); cacheCreationTokens.Exists() {
				template, _ = sjson.SetBytes(
					template, "usageMetadata.cachedContentTokenCount", cacheCreationTokens.Int(),
				)
			}
			if cacheReadTokens := usage.Get("cache_read_input_tokens"); cacheReadTokens.Exists() {
				// Add cache read tokens to cached content count
				existingCacheTokens := usage.Get("cache_creation_input_tokens").Int()
				totalCacheTokens := existingCacheTokens + cacheReadTokens.Int()
				template, _ = sjson.SetBytes(template, "usageMetadata.cachedContentTokenCount", totalCacheTokens)
			}

			// Add thinking tokens if present (for models with reasoning capabilities)
			if thinkingTokens := usage.Get("thinking_tokens"); thinkingTokens.Exists() {
				template, _ = sjson.SetBytes(template, "usageMetadata.thoughtsTokenCount", thinkingTokens.Int())
			}

			// Set traffic type (required by Gemini API)
			template, _ = sjson.SetBytes(template, "usageMetadata.trafficType", "PROVISIONED_THROUGHPUT")
		}
		template, _ = sjson.SetBytes(template, "candidates.0.finishReason", "STOP")

		return [][]byte{template}
	case "message_stop":
		// Final message with usage information - no additional output needed
		return [][]byte{}
	case "error":
		// Handle error responses and convert to Gemini error format
		errorMsg := root.Get("error.message").String()
		if errorMsg == "" {
			errorMsg = "Unknown error occurred"
		}

		// Create error response in Gemini format
		errorResponse := []byte(`{"error":{"code":400,"message":"","status":"INVALID_ARGUMENT"}}`)
		errorResponse, _ = sjson.SetBytes(errorResponse, "error.message", errorMsg)
		return [][]byte{errorResponse}

	default:
		// Unknown event type, return empty response
		return [][]byte{}
	}
}

// ConvertClaudeResponseToGeminiNonStream converts a non-streaming Claude Code response to a non-streaming Gemini response.
// This function processes the complete Claude Code response and transforms it into a single Gemini-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the Gemini API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Claude Code API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - []byte: A Gemini-compatible JSON response containing all message content and metadata
func ConvertClaudeResponseToGeminiNonStream(_ context.Context, modelName string, _, _, rawJSON []byte, _ *any) []byte {
	// Base Gemini response template for non-streaming with default values
	template := []byte(`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"trafficType":"PROVISIONED_THROUGHPUT"},"modelVersion":"","createTime":"","responseId":""}`)

	// Set model version
	template, _ = sjson.SetBytes(template, "modelVersion", modelName)

	streamingEvents := make([][]byte, 0)

	scanner := bufio.NewScanner(bytes.NewReader(rawJSON))
	buffer := make([]byte, 52_428_800) // 50MB
	scanner.Buffer(buffer, 52_428_800)
	for scanner.Scan() {
		line := scanner.Bytes()
		// log.Debug(string(line))
		if bytes.HasPrefix(line, dataTag) {
			jsonData := bytes.TrimSpace(line[5:])
			streamingEvents = append(streamingEvents, jsonData)
		}
	}
	// log.Debug("streamingEvents: ", streamingEvents)
	// log.Debug("rawJSON: ", string(rawJSON))

	// Initialize parameters for streaming conversion with proper state management
	newParam := &convertAnthropicResponseToGeminiParams{
		Model:             modelName,
		CreatedAt:         0,
		ResponseID:        "",
		LastStorageOutput: nil,
		IsStreaming:       false,
		ToolUseNames:      nil,
		ToolUseArgs:       nil,
		ToolUseIDs:        nil,
		ToolUseSignatures: nil,
	}

	// Process each streaming event and collect parts
	var allParts [][]byte
	var finalUsageJSON []byte
	var responseID string
	var createdAt int64

	for _, eventData := range streamingEvents {
		if len(eventData) == 0 {
			continue
		}

		root := gjson.ParseBytes(eventData)
		eventType := root.Get("type").String()

		switch eventType {
		case "message_start":
			// Extract response metadata including ID, model, and creation time
			if message := root.Get("message"); message.Exists() {
				responseID = message.Get("id").String()
				newParam.ResponseID = responseID
				newParam.Model = message.Get("model").String()

				// Set creation time to current time if not provided
				createdAt = time.Now().Unix()
				newParam.CreatedAt = createdAt
			}

		case "content_block_start":
			// Prepare for content block and retain request-local tool or thinking state.
			idx := int(root.Get("index").Int())
			if cb := root.Get("content_block"); cb.Exists() {
				switch cb.Get("type").String() {
				case "tool_use":
					if newParam.ToolUseNames == nil {
						newParam.ToolUseNames = map[int]string{}
					}
					if name := cb.Get("name"); name.Exists() {
						newParam.ToolUseNames[idx] = name.String()
					}
					if toolID := strings.TrimSpace(cb.Get("id").String()); toolID != "" {
						if newParam.ToolUseIDs == nil {
							newParam.ToolUseIDs = map[int]string{}
						}
						newParam.ToolUseIDs[idx] = toolID
					}
					if thoughtSignature := claudeContentBlockSignature(
						cb,
						signature.BlockKindGeminiFunctionCall,
					); thoughtSignature != "" {
						if newParam.ToolUseSignatures == nil {
							newParam.ToolUseSignatures = map[int]string{}
						}
						newParam.ToolUseSignatures[idx] = thoughtSignature
					}
				case "thinking":
					appendClaudeThinkingSignature(newParam, idx, rawClaudeContentBlockSignature(cb))
				}
			}
			continue

		case "content_block_delta":
			// Handle content delta (text, thinking, or tool input)
			if delta := root.Get("delta"); delta.Exists() {
				deltaType := delta.Get("type").String()
				switch deltaType {
				case "text_delta":
					// Process regular text content
					if text := delta.Get("text"); text.Exists() && text.String() != "" {
						partJSON := []byte(`{"text":""}`)
						partJSON, _ = sjson.SetBytes(partJSON, "text", text.String())
						allParts = append(allParts, partJSON)
					}
				case "thinking_delta":
					// Process reasoning/thinking content
					if text := delta.Get("thinking"); text.Exists() && text.String() != "" {
						partJSON := []byte(`{"thought":true,"text":""}`)
						partJSON, _ = sjson.SetBytes(partJSON, "text", text.String())
						allParts = append(allParts, partJSON)
					}
				case "signature_delta":
					appendClaudeThinkingSignature(
						newParam,
						int(root.Get("index").Int()),
						delta.Get("signature").String(),
					)
				case "input_json_delta":
					// accumulate args partial_JSON for this index
					idx := int(root.Get("index").Int())
					if newParam.ToolUseArgs == nil {
						newParam.ToolUseArgs = map[int]*strings.Builder{}
					}
					if _, ok := newParam.ToolUseArgs[idx]; !ok || newParam.ToolUseArgs[idx] == nil {
						newParam.ToolUseArgs[idx] = &strings.Builder{}
					}
					if pj := delta.Get("partial_json"); pj.Exists() {
						newParam.ToolUseArgs[idx].WriteString(pj.String())
					}
				}
			}

		case "content_block_stop":
			// Handle tool use completion by assembling accumulated arguments
			idx := int(root.Get("index").Int())
			// Claude's content_block_stop often doesn't include content_block payload (see docs/response-claude.txt)
			// So we finalize using accumulated state captured during content_block_start and input_json_delta.
			name := ""
			if newParam.ToolUseNames != nil {
				name = newParam.ToolUseNames[idx]
			}
			var argsTrim string
			if newParam.ToolUseArgs != nil {
				if b := newParam.ToolUseArgs[idx]; b != nil {
					argsTrim = strings.TrimSpace(b.String())
				}
			}
			if name != "" || argsTrim != "" {
				functionCallJSON := []byte(`{"functionCall":{"name":"","args":{}}}`)
				if name != "" {
					functionCallJSON, _ = sjson.SetBytes(functionCallJSON, "functionCall.name", name)
				}
				if argsTrim != "" {
					functionCallJSON, _ = sjson.SetRawBytes(functionCallJSON, "functionCall.args", []byte(argsTrim))
				}
				if toolID := newParam.ToolUseIDs[idx]; toolID != "" {
					functionCallJSON, _ = sjson.SetBytes(functionCallJSON, "functionCall.id", toolID)
				}
				if thoughtSignature := newParam.ToolUseSignatures[idx]; thoughtSignature != "" {
					functionCallJSON, _ = sjson.SetBytes(functionCallJSON, "thoughtSignature", thoughtSignature)
				}
				allParts = append(allParts, functionCallJSON)
				delete(newParam.ToolUseArgs, idx)
				delete(newParam.ToolUseNames, idx)
				delete(newParam.ToolUseIDs, idx)
				delete(newParam.ToolUseSignatures, idx)
				delete(newParam.ThinkingSignatures, idx)
				continue
			}
			if thoughtSignature := takeGeminiThinkingSignature(newParam, idx); thoughtSignature != "" {
				partJSON := []byte(`{"thought":true,"thoughtSignature":""}`)
				partJSON, _ = sjson.SetBytes(partJSON, "thoughtSignature", thoughtSignature)
				allParts = append(allParts, partJSON)
			}

		case "message_delta":
			// Extract final usage information using sjson for token counts and metadata
			if usage := root.Get("usage"); usage.Exists() {
				usageJSON := []byte(`{}`)

				// Basic token counts for prompt and completion
				inputTokens := usage.Get("input_tokens").Int()
				outputTokens := usage.Get("output_tokens").Int()

				// Set basic usage metadata according to Gemini API specification
				usageJSON, _ = sjson.SetBytes(usageJSON, "promptTokenCount", inputTokens)
				usageJSON, _ = sjson.SetBytes(usageJSON, "candidatesTokenCount", outputTokens)
				usageJSON, _ = sjson.SetBytes(usageJSON, "totalTokenCount", inputTokens+outputTokens)

				// Add cache-related token counts if present (Claude Code API cache fields)
				if cacheCreationTokens := usage.Get("cache_creation_input_tokens"); cacheCreationTokens.Exists() {
					usageJSON, _ = sjson.SetBytes(usageJSON, "cachedContentTokenCount", cacheCreationTokens.Int())
				}
				if cacheReadTokens := usage.Get("cache_read_input_tokens"); cacheReadTokens.Exists() {
					// Add cache read tokens to cached content count
					existingCacheTokens := usage.Get("cache_creation_input_tokens").Int()
					totalCacheTokens := existingCacheTokens + cacheReadTokens.Int()
					usageJSON, _ = sjson.SetBytes(usageJSON, "cachedContentTokenCount", totalCacheTokens)
				}

				// Add thinking tokens if present (for models with reasoning capabilities)
				if thinkingTokens := usage.Get("thinking_tokens"); thinkingTokens.Exists() {
					usageJSON, _ = sjson.SetBytes(usageJSON, "thoughtsTokenCount", thinkingTokens.Int())
				}

				// Set traffic type (required by Gemini API)
				usageJSON, _ = sjson.SetBytes(usageJSON, "trafficType", "PROVISIONED_THROUGHPUT")

				finalUsageJSON = usageJSON
			}
		}
	}

	// Set response metadata
	if responseID != "" {
		template, _ = sjson.SetBytes(template, "responseId", responseID)
	}
	if createdAt > 0 {
		template, _ = sjson.SetBytes(template, "createTime", time.Unix(createdAt, 0).Format(time.RFC3339Nano))
	}

	// Consolidate consecutive text parts and thinking parts for cleaner output
	consolidatedParts := consolidateParts(allParts)

	// Set the consolidated parts array
	if len(consolidatedParts) > 0 {
		partsJSON := []byte(`[]`)
		for _, partJSON := range consolidatedParts {
			partsJSON, _ = sjson.SetRawBytes(partsJSON, "-1", partJSON)
		}
		template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts", partsJSON)
	}

	// Set usage metadata
	if len(finalUsageJSON) > 0 {
		template, _ = sjson.SetRawBytes(template, "usageMetadata", finalUsageJSON)
	}

	return template
}

func tokenCount(_ context.Context, count int64) []byte {
	return translatorcommon.GeminiTokenCountJSON(count)
}

// consolidateParts merges consecutive text parts and thinking parts to create a cleaner response.
// This function processes the parts array to combine adjacent text elements and thinking elements
// into single consolidated parts, which results in a more readable and efficient response structure.
// Tool calls and other non-text parts are preserved as separate elements.
func consolidateParts(parts [][]byte) [][]byte {
	if len(parts) == 0 {
		return parts
	}

	var consolidated [][]byte
	var currentTextPart strings.Builder
	var currentThoughtPart strings.Builder
	var currentThoughtSignature string
	var hasText, hasThought bool

	flushText := func() {
		// Flush accumulated text content to the consolidated parts array
		if hasText && currentTextPart.Len() > 0 {
			textPartJSON := []byte(`{"text":""}`)
			textPartJSON, _ = sjson.SetBytes(textPartJSON, "text", currentTextPart.String())
			consolidated = append(consolidated, textPartJSON)
			currentTextPart.Reset()
			hasText = false
		}
	}

	flushThought := func() {
		// Flush accumulated thinking content and its latest non-empty signature.
		if hasThought && (currentThoughtPart.Len() > 0 || currentThoughtSignature != "") {
			thoughtPartJSON := []byte(`{"thought":true,"text":""}`)
			thoughtPartJSON, _ = sjson.SetBytes(thoughtPartJSON, "text", currentThoughtPart.String())
			if currentThoughtSignature != "" {
				thoughtPartJSON, _ = sjson.SetBytes(
					thoughtPartJSON,
					"thoughtSignature",
					currentThoughtSignature,
				)
			}
			consolidated = append(consolidated, thoughtPartJSON)
			currentThoughtPart.Reset()
			currentThoughtSignature = ""
			hasThought = false
		}
	}

	for _, partJSON := range parts {
		part := gjson.ParseBytes(partJSON)
		if !part.Exists() || !part.IsObject() {
			// Flush any pending parts and add this non-text part
			flushText()
			flushThought()
			consolidated = append(consolidated, partJSON)
			continue
		}

		thought := part.Get("thought")
		if thought.Exists() && thought.Type == gjson.True {
			// This is a thinking part - flush any pending text first
			flushText() // Flush any pending text first

			if text := part.Get("text"); text.Exists() && text.Type == gjson.String {
				currentThoughtPart.WriteString(text.String())
				hasThought = true
			}
			if thoughtSignature := part.Get("thoughtSignature"); thoughtSignature.Type == gjson.String && thoughtSignature.String() != "" {
				currentThoughtSignature = thoughtSignature.String()
				hasThought = true
			}
		} else if text := part.Get("text"); text.Exists() && text.Type == gjson.String {
			// This is a regular text part - flush any pending thought first
			flushThought() // Flush any pending thought first

			currentTextPart.WriteString(text.String())
			hasText = true
		} else {
			// This is some other type of part (like function call) - flush both text and thought
			flushText()
			flushThought()
			consolidated = append(consolidated, partJSON)
		}
	}

	// Flush any remaining parts
	flushThought() // Flush thought first to maintain order
	flushText()

	return consolidated
}
