// Package gemini provides response translation functionality for Claude Code to Gemini API compatibility.
// This package handles the conversion of Claude Code API responses into Gemini-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by Gemini API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, and usage metadata appropriately.
//
//goland:noinspection GrazieInspection
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

func geminiClaudeToolNameMap(request []byte) map[string]string {
	if nested := gjson.GetBytes(request, "request"); nested.IsObject() {
		request = []byte(nested.Raw)
	}
	return util.ReverseToolNameMap(util.ClaudeFunctionNameMap(request))
}

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
	ToolNames          map[string]string
	Usage              []byte
	err                error
}

//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (s *convertAnthropicResponseToGeminiParams) ResponseTranslationError() error {
	return s.err
}

func geminiClaudeFinishReason(reason string) string {
	switch reason {
	case "end_turn", "tool_use", "stop_sequence", "pause_turn":
		return "STOP"
	case "max_tokens":
		return "MAX_TOKENS"
	case "refusal":
		return "SAFETY"
	default:
		return "OTHER"
	}
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
//
// noinspection GoErrorStringFormat
func ConvertClaudeResponseToGemini(
	_ context.Context,
	modelName string,
	originalRequestRawJSON, _, rawJSON []byte,
	param *any,
) [][]byte {
	if *param == nil {
		*param = &convertAnthropicResponseToGeminiParams{
			Model:      modelName,
			CreatedAt:  0,
			ResponseID: "",
			ToolNames:  geminiClaudeToolNameMap(originalRequestRawJSON),
		}
	}

	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	root := gjson.ParseBytes(rawJSON)
	eventType := root.Get("type").String()

	// Base Gemini response template with default values
	template := []byte((`{"candidates":[{"content":{"role":"model","parts":[]}}],"usageMetadata` +
		`":{"trafficType":"PROVISIONED_THROUGHPUT"},"modelVersion":"","createTi` +
		`me":"","responseId":""}`))

	// Set model version
	if (*param).(*convertAnthropicResponseToGeminiParams).Model != "" {
		// Map Claude model names back to Gemini model names
		template, _ = sjson.SetBytes(
			template,
			"modelVersion",
			(*param).(*convertAnthropicResponseToGeminiParams).Model,
		)
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
			(*param).(*convertAnthropicResponseToGeminiParams).Usage = []byte(message.Get("usage").Raw)
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
					params.ToolUseNames[idx] = util.RestoreSanitizedToolName(params.ToolNames, name.String())
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
				if stopReason.Type == gjson.String && stopReason.String() != "" {
					template, _ = sjson.SetBytes(
						template, "candidates.0.finishReason", geminiClaudeFinishReason(stopReason.String()),
					)
				}
			}
		}

		params := (*param).(*convertAnthropicResponseToGeminiParams)
		if usage := root.Get("usage"); usage.Exists() {
			if len(params.Usage) == 0 {
				params.Usage = []byte(`{}`)
			}
			usage.ForEach(
				func(key, value gjson.Result) bool {
					params.Usage, _ = sjson.SetRawBytes(params.Usage, key.String(), []byte(value.Raw))
					return true
				},
			)
		}
		if usage := gjson.ParseBytes(params.Usage); usage.IsObject() {
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
				template, _ = sjson.SetBytes(
					template,
					"usageMetadata.cachedContentTokenCount",
					totalCacheTokens,
				)
			}

			// Add thinking tokens if present (for models with reasoning capabilities)
			if thinkingTokens := usage.Get("thinking_tokens"); thinkingTokens.Exists() {
				template, _ = sjson.SetBytes(template, "usageMetadata.thoughtsTokenCount", thinkingTokens.Int())
			}

			// Set traffic type (required by Gemini API)
			template, _ = sjson.SetBytes(template, "usageMetadata.trafficType", "PROVISIONED_THROUGHPUT")
		}
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
		//lint:ignore ST1005 Claude 是供应商专名，保留已有错误前缀
		(*param).(*convertAnthropicResponseToGeminiParams).err = fmt.Errorf("Claude response: %s", errorMsg)

		// Create error response in Gemini format
		errorResponse := []byte(`{"error":{"code":400,"message":"","status":"INVALID_ARGUMENT"}}`)
		errorResponse, _ = sjson.SetBytes(errorResponse, "error.message", errorMsg)
		return [][]byte{errorResponse}

	default:
		// Unknown event type, return empty response
		return [][]byte{}
	}
}

// ConvertClaudeResponseToGeminiNonStream converts a non-streaming Claude Code response to a non-streaming Gemini
// response.
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
func ConvertClaudeResponseToGeminiNonStream(
	ctx context.Context,
	modelName string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) []byte {
	var state any
	if param == nil {
		param = &state
	}
	*param = &convertAnthropicResponseToGeminiParams{
		Model: modelName, ToolNames: geminiClaudeToolNameMap(originalRequestRawJSON),
	}
	params := (*param).(*convertAnthropicResponseToGeminiParams)
	template := []byte((`{"candidates":[{"content":{"role":"model","parts":[]}}],"usageMetadata` +
		`":{"trafficType":"PROVISIONED_THROUGHPUT"},"modelVersion":"","createTi` +
		`me":"","responseId":""}`))
	template, _ = sjson.SetBytes(template, "modelVersion", modelName)
	var allParts [][]byte
	feed := func(event []byte) {
		if !gjson.ValidBytes(bytes.TrimSpace(bytes.TrimPrefix(event, dataTag))) {
			params.err = fmt.Errorf("invalid Claude response event JSON")
			return
		}
		for _, chunk := range ConvertClaudeResponseToGemini(
			ctx, modelName, originalRequestRawJSON, requestRawJSON, event, param,
		) {
			root := gjson.ParseBytes(chunk)
			for _, part := range root.Get("candidates.0.content.parts").Array() {
				allParts = append(allParts, []byte(part.Raw))
			}
			for _, path := range []string{"usageMetadata", "candidates.0.finishReason", "createTime", "responseId"} {
				if value := root.Get(path); value.Exists() {
					template, _ = sjson.SetRawBytes(template, path, []byte(value.Raw))
				}
			}
		}
	}
	if native := gjson.ParseBytes(rawJSON); gjson.ValidBytes(rawJSON) &&
		native.Get("type").String() == "message" {
		feed([]byte(fmt.Sprintf(`data: {"type":"message_start","message":%s}`, rawJSON)))
		for index, block := range native.Get("content").Array() {
			feed(
				[]byte(fmt.Sprintf(
					`data: {"type":"content_block_start","index":%d,"content_block":%s}`, index, block.Raw,
				)),
			)
			var delta []byte
			switch block.Get("type").String() {
			case "text":
				delta, _ = sjson.SetBytes([]byte(`{"type":"text_delta"}`), "text", block.Get("text").String())
			case "thinking":
				delta, _ = sjson.SetBytes(
					[]byte(`{"type":"thinking_delta"}`), "thinking", block.Get("thinking").String(),
				)
			case "tool_use":
				delta, _ = sjson.SetBytes(
					[]byte(`{"type":"input_json_delta"}`),
					"partial_json",
					block.Get("input").Raw,
				)
			}
			if len(delta) > 0 {
				feed(
					[]byte(
						fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":%s}`, index, delta),
					),
				)
			}
			feed([]byte(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, index)))
		}
		delta, _ := sjson.SetBytes(
			[]byte(
				`{"type":"message_delta","delta":{}}`,
			),
			"delta.stop_reason",
			native.Get("stop_reason").String(),
		)
		feed(append([]byte("data: "), delta...))
	} else if gjson.ValidBytes(rawJSON) {
		if gjson.GetBytes(rawJSON, "type").String() == "error" {
			feed(append([]byte("data: "), rawJSON...))
		} else {
			params.err = fmt.Errorf("unsupported Claude response JSON type")
		}
	} else {
		scanner := bufio.NewScanner(bytes.NewReader(rawJSON))
		scanner.Buffer(make([]byte, 64*1024), 52_428_800)
		for scanner.Scan() {
			// Consume before Scan reuses its buffer.
			if line := scanner.Bytes(); bytes.HasPrefix(line, dataTag) {
				feed(line)
			}
		}
		if err := scanner.Err(); err != nil {
			params.err = fmt.Errorf("scan Claude response: %w", err)
		}
	}
	if params.err != nil {
		return nil
	}
	if params.ResponseID != "" {
		template, _ = sjson.SetBytes(template, "responseId", params.ResponseID)
	}
	for _, part := range consolidateParts(allParts) {
		template, _ = sjson.SetRawBytes(template, "candidates.0.content.parts.-1", part)
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
			if thoughtSignature := part.Get("thoughtSignature"); thoughtSignature.Type == gjson.String &&
				thoughtSignature.String() != "" {
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
