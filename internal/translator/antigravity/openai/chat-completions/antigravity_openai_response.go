// Package chat_completions provides response translation functionality for Gemini CLI to OpenAI API compatibility.
// This package handles the conversion of Gemini CLI API responses into OpenAI Chat Completions-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by OpenAI API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, reasoning content, and usage metadata appropriately.
package chat_completions

import (
	"bytes"
	"context"
	"strings"

	. "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai/chat-completions"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// antigravityResponseParams holds state tracked across streaming chunks.
// Unlike gemini-cli, antigravity defers finish_reason to the final chunk
// and tracks tool call presence across the entire stream.
type antigravityResponseParams struct {
	UnixTimestamp        int64
	FunctionIndex        int
	SawToolCall          bool   // Tracks if any tool call was seen in the entire stream
	UpstreamFinishReason string // Caches the upstream finish reason for final chunk
}

// convertAntigravityResponseToOpenAI translates a single chunk of a streaming response from the
// Gemini CLI API format to the OpenAI Chat Completions streaming format.
// It processes various Gemini CLI event types and transforms them into OpenAI-compatible JSON responses.
// The function handles text content, tool calls, reasoning content, and usage metadata, outputting
// responses that match the OpenAI API format. It supports incremental updates for streaming responses.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Gemini CLI API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - []string: A slice of strings, each containing an OpenAI-compatible JSON response
func convertAntigravityResponseToOpenAI(
	_ context.Context,
	_ string,
	_, _, rawJSON []byte,
	param *any,
) []string {
	if *param == nil {
		*param = &antigravityResponseParams{}
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		return []string{}
	}

	p := (*param).(*antigravityResponseParams)

	// Process the common chunk (model, time, id, usage, content parts).
	result := common.ProcessCLIResponseChunk(rawJSON, &p.UnixTimestamp, &p.FunctionIndex)

	// Track tool call presence across the entire stream.
	if result.HasToolCall {
		p.SawToolCall = true
	}

	// Cache the finish reason - do NOT set it in output yet (will be set on final chunk).
	if fr := gjson.GetBytes(rawJSON, "response.candidates.0.finishReason"); fr.Exists() {
		p.UpstreamFinishReason = strings.ToUpper(fr.String())
	}

	// Determine finish_reason only on the final chunk (has both finishReason and usage metadata).
	usageExists := gjson.GetBytes(rawJSON, "response.usageMetadata").Exists()
	isFinalChunk := p.UpstreamFinishReason != "" && usageExists

	template := result.Template
	if isFinalChunk {
		var finishReason string
		if p.SawToolCall {
			finishReason = "tool_calls"
		} else if p.UpstreamFinishReason == "MAX_TOKENS" {
			finishReason = "max_tokens"
		} else {
			finishReason = "stop"
		}
		template, _ = sjson.Set(template, "choices.0.finish_reason", finishReason)
		template, _ = sjson.Set(template, "choices.0.native_finish_reason", strings.ToLower(p.UpstreamFinishReason))
	}

	return []string{template}
}

// convertAntigravityResponseToOpenAINonStream converts a non-streaming Gemini CLI response to a non-streaming OpenAI response.
// This function processes the complete Gemini CLI response and transforms it into a single OpenAI-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the OpenAI API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Gemini CLI API
//   - param: A pointer to a parameter object for the conversion
//
// Returns:
//   - string: An OpenAI-compatible JSON response containing all message content and metadata
func convertAntigravityResponseToOpenAINonStream(
	ctx context.Context,
	modelName string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) string {
	responseResult := gjson.GetBytes(rawJSON, "response")
	if responseResult.Exists() {
		return ConvertGeminiResponseToOpenAINonStream(
			ctx, modelName, originalRequestRawJSON, requestRawJSON, []byte(responseResult.Raw), param,
		)
	}
	return ""
}
