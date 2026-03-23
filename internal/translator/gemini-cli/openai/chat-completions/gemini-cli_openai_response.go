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

// geminiCLIResponseParams holds state tracked across streaming chunks.
type geminiCLIResponseParams struct {
	UnixTimestamp int64
	FunctionIndex int
}

// convertCliResponseToOpenAI translates a single chunk of a streaming response from the
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
func convertCliResponseToOpenAI(
	_ context.Context,
	_ string,
	_, _, rawJSON []byte,
	param *any,
) []string {
	if *param == nil {
		*param = &geminiCLIResponseParams{}
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		return []string{}
	}

	p := (*param).(*geminiCLIResponseParams)

	// Process the common chunk (model, time, id, usage, content parts).
	result := common.ProcessCLIResponseChunk(rawJSON, &p.UnixTimestamp, &p.FunctionIndex)

	// Extract finish reason from either stop_reason or candidates.0.finishReason.
	finishReason := ""
	if v := gjson.GetBytes(rawJSON, "response.stop_reason"); v.Exists() {
		finishReason = v.String()
	}
	if finishReason == "" {
		if v := gjson.GetBytes(rawJSON, "response.candidates.0.finishReason"); v.Exists() {
			finishReason = v.String()
		}
	}
	finishReason = strings.ToLower(finishReason)

	template := result.Template
	if result.HasToolCall {
		template, _ = sjson.Set(template, "choices.0.finish_reason", "tool_calls")
		template, _ = sjson.Set(template, "choices.0.native_finish_reason", "tool_calls")
	} else if finishReason != "" && p.FunctionIndex == 0 {
		// Only pass through specific finish reasons
		if finishReason == "max_tokens" || finishReason == "stop" {
			template, _ = sjson.Set(template, "choices.0.finish_reason", finishReason)
			template, _ = sjson.Set(template, "choices.0.native_finish_reason", finishReason)
		}
	}

	return []string{template}
}

// convertCliResponseToOpenAINonStream converts a non-streaming Gemini CLI response to a non-streaming OpenAI response.
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
func convertCliResponseToOpenAINonStream(
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
