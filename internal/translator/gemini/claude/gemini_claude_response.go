// Package claude provides response translation functionality for Claude API.
// This package handles the conversion of backend client responses into Claude-compatible
// Server-Sent Events (SSE) format, implementing a sophisticated state machine that manages
// different response types including text content, thinking processes, and function calls.
// The translation ensures proper sequencing of SSE events and maintains state across
// multiple response chunks to provide a seamless streaming experience.
// 最后编译日期: 2026-03-19 � Yi
package claude

import (
	"bytes"
	"context"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/tidwall/gjson"
)

// paths defines the JSON paths for the direct Gemini API variant.
var paths = common.GeminiDirectPaths()

// convertGeminiResponseToClaude performs streaming response format conversion.
// It translates Gemini backend responses into Claude-compatible SSE format,
// managing state transitions between content blocks, thinking processes, and function calls.
//
// Response type states: 0=none, 1=content, 2=thinking, 3=function
func convertGeminiResponseToClaude(
	_ context.Context,
	_ string,
	_, _, rawJSON []byte,
	param *any,
) []string {
	if *param == nil {
		*param = &common.StreamState{}
	}
	s := (*param).(*common.StreamState)

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		if s.HasContent {
			return []string{
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n\n",
			}
		}
		return []string{}
	}

	usedTool := false
	var buf strings.Builder

	// Initialize the streaming session with a message_start event
	common.AppendMessageStart(&buf, s, rawJSON, paths)

	// Process the response parts array from the backend client
	partsResult := gjson.GetBytes(rawJSON, paths.Parts)
	if partsResult.IsArray() {
		for _, partResult := range partsResult.Array() {
			partTextResult := partResult.Get("text")
			functionCallResult := partResult.Get("functionCall")

			if partTextResult.Exists() {
				if partResult.Get("thought").Bool() {
					common.AppendThinkingBlock(&buf, s, partTextResult.String())
				} else {
					common.AppendTextBlock(&buf, s, partTextResult.String())
				}
			} else if functionCallResult.Exists() {
				usedTool = true

				// Handle streaming split/delta where name might be empty in subsequent chunks.
				// If already in tool use mode and name is empty, treat as continuation (delta).
				fcName := functionCallResult.Get("name").String()
				if s.ResponseType == 3 && fcName == "" {
					common.AppendToolUseDelta(&buf, s, functionCallResult)
					continue
				}

				common.AppendToolUseBlock(&buf, s, functionCallResult)
			}
		}
	}

	// Process usage metadata and finish reason
	common.AppendMessageDelta(&buf, s, rawJSON, paths, usedTool)

	return []string{buf.String()}
}

// convertGeminiResponseToClaudeNonStream converts a non-streaming Gemini response
// to a non-streaming Claude response.
func convertGeminiResponseToClaudeNonStream(
	_ context.Context,
	_ string,
	_, _, rawJSON []byte,
	_ *any,
) string {
	return common.BuildNonStreamResponse(rawJSON, paths)
}

// tokenCount formats a token count response in Claude API format.
var tokenCount = common.ClaudeTokenCount
