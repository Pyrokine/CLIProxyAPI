// Package common provides shared response translation helpers for Gemini API variants.
// 最后编译日期: 2026-03-19 � Yi
package common

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// claudeToolUseIDCounter provides a process-wide unique counter for Claude SSE tool use identifiers.
var claudeToolUseIDCounter uint64

// StreamState holds the state machine fields shared by all Gemini-to-Claude streaming converters.
// Response type states: 0=none, 1=content, 2=thinking, 3=function.
type StreamState struct {
	HasFirstResponse bool // Indicates if the initial message_start event has been sent
	ResponseType     int  // Current response type
	ResponseIndex    int  // Index counter for content blocks
	HasContent       bool // Tracks whether any content has been output
}

// PathConfig specifies JSON paths that differ between Gemini API variants.
type PathConfig struct {
	// ModelVersion is the gjson path to the model version (e.g. "modelVersion" or "response.modelVersion").
	ModelVersion string
	// ResponseID is the gjson path to the response ID.
	ResponseID string
	// Parts is the gjson path to the content parts array.
	Parts string
	// UsageMetadata is the gjson path to the usage metadata object.
	UsageMetadata string
	// FinishReason is the gjson path to the finish reason.
	FinishReason string
	// FinishReasonRaw is the raw byte substring to detect finishReason presence.
	FinishReasonRaw []byte
}

// GeminiDirectPaths returns PathConfig for the direct Gemini API (no "response." prefix).
func GeminiDirectPaths() PathConfig {
	return PathConfig{
		ModelVersion:    "modelVersion",
		ResponseID:      "responseId",
		Parts:           "candidates.0.content.parts",
		UsageMetadata:   "usageMetadata",
		FinishReason:    "candidates.0.finishReason",
		FinishReasonRaw: []byte(`"finishReason"`),
	}
}

// GeminiCLIPaths returns PathConfig for the Gemini CLI API ("response." prefix).
func GeminiCLIPaths() PathConfig {
	return PathConfig{
		ModelVersion:    "response.modelVersion",
		ResponseID:      "response.responseId",
		Parts:           "response.candidates.0.content.parts",
		UsageMetadata:   "response.usageMetadata",
		FinishReason:    "response.candidates.0.finishReason",
		FinishReasonRaw: []byte(`"finishReason"`),
	}
}

// closeBlock appends a content_block_stop SSE event for the given index.
func closeBlock(buf *strings.Builder, index int) {
	buf.WriteString("event: content_block_stop\n")
	_, _ = fmt.Fprintf(buf, `data: {"type":"content_block_stop","index":%d}`, index)
	buf.WriteString("\n\n\n")
}

// thinkingDelta builds a thinking_delta SSE event string.
func thinkingDelta(index int, text string) string {
	data, _ := sjson.Set(
		fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"thinking_delta","thinking":""}}`,
			index,
		), "delta.thinking", text,
	)
	return "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n\n", data)
}

// textDelta builds a text_delta SSE event string.
func textDelta(index int, text string) string {
	data, _ := sjson.Set(
		fmt.Sprintf(
			`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":""}}`,
			index,
		), "delta.text", text,
	)
	return "event: content_block_delta\n" + fmt.Sprintf("data: %s\n\n\n", data)
}

// closeCurrentBlock closes the current content block if one is open, and advances the index.
func closeCurrentBlock(buf *strings.Builder, s *StreamState) {
	if s.ResponseType != 0 {
		closeBlock(buf, s.ResponseIndex)
		s.ResponseIndex++
	}
}

// AppendThinkingBlock appends thinking content SSE events, handling state transitions.
func AppendThinkingBlock(buf *strings.Builder, s *StreamState, text string) {
	if s.ResponseType == 2 {
		// Continue existing thinking block
		buf.WriteString(thinkingDelta(s.ResponseIndex, text))
		s.HasContent = true
		return
	}

	// Transition: close any existing block, then start a new thinking block
	closeCurrentBlock(buf, s)

	buf.WriteString("event: content_block_start\n")
	_, _ = fmt.Fprintf(buf,
		`data: {"type":"content_block_start","index":%d,"content_block":{"type":"thinking","thinking":""}}`,
		s.ResponseIndex,
	)
	buf.WriteString("\n\n\n")
	buf.WriteString(thinkingDelta(s.ResponseIndex, text))
	s.ResponseType = 2
	s.HasContent = true
}

// AppendTextBlock appends text content SSE events, handling state transitions.
func AppendTextBlock(buf *strings.Builder, s *StreamState, text string) {
	if s.ResponseType == 1 {
		// Continue existing text block
		buf.WriteString(textDelta(s.ResponseIndex, text))
		s.HasContent = true
		return
	}

	// Transition: close any existing block, then start a new text block
	closeCurrentBlock(buf, s)

	buf.WriteString("event: content_block_start\n")
	_, _ = fmt.Fprintf(buf,
		`data: {"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`,
		s.ResponseIndex,
	)
	buf.WriteString("\n\n\n")
	buf.WriteString(textDelta(s.ResponseIndex, text))
	s.ResponseType = 1
	s.HasContent = true
}

// AppendToolUseBlock appends function/tool call SSE events, handling state transitions.
// Returns true if the tool block was emitted (caller should set usedTool = true).
func AppendToolUseBlock(buf *strings.Builder, s *StreamState, functionCallResult gjson.Result) {
	fcName := functionCallResult.Get("name").String()

	// Close any existing function call block
	if s.ResponseType == 3 {
		closeBlock(buf, s.ResponseIndex)
		s.ResponseIndex++
		s.ResponseType = 0
	}

	// Close any other existing content block
	closeCurrentBlock(buf, s)

	// Start a new tool use content block
	buf.WriteString("event: content_block_start\n")
	data := fmt.Sprintf(
		`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`,
		s.ResponseIndex,
	)
	data, _ = sjson.Set(
		data, "content_block.id",
		fmt.Sprintf("%s-%d-%d", fcName, time.Now().UnixNano(), atomic.AddUint64(&claudeToolUseIDCounter, 1)),
	)
	data, _ = sjson.Set(data, "content_block.name", fcName)
	buf.WriteString(fmt.Sprintf("data: %s\n\n\n", data))

	if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
		buf.WriteString("event: content_block_delta\n")
		argData, _ := sjson.Set(
			fmt.Sprintf(
				`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`,
				s.ResponseIndex,
			), "delta.partial_json", fcArgsResult.Raw,
		)
		buf.WriteString(fmt.Sprintf("data: %s\n\n\n", argData))
	}
	s.ResponseType = 3
	s.HasContent = true
}

// AppendToolUseDelta appends a continuation delta for a streaming tool call (args-only chunk).
func AppendToolUseDelta(buf *strings.Builder, s *StreamState, functionCallResult gjson.Result) {
	if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
		buf.WriteString("event: content_block_delta\n")
		data, _ := sjson.Set(
			fmt.Sprintf(
				`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":""}}`,
				s.ResponseIndex,
			), "delta.partial_json", fcArgsResult.Raw,
		)
		buf.WriteString(fmt.Sprintf("data: %s\n\n\n", data))
	}
}

// AppendMessageStart prepends the message_start SSE event using model/id from rawJSON.
func AppendMessageStart(buf *strings.Builder, s *StreamState, rawJSON []byte, paths PathConfig) {
	if s.HasFirstResponse {
		return
	}

	buf.WriteString("event: message_start\n")

	// Create the initial message structure with default values
	tpl := `{"type": "message_start", "message": {"id": "msg_1nZdL29xx5MUA1yADyHTEsnR8uuvGzszyY", "type": "message", "role": "assistant", "content": [], "model": "claude-3-5-sonnet-20241022", "stop_reason": null, "stop_sequence": null, "usage": {"input_tokens": 0, "output_tokens": 0}}}`

	if v := gjson.GetBytes(rawJSON, paths.ModelVersion); v.Exists() {
		tpl, _ = sjson.Set(tpl, "message.model", v.String())
	}
	if v := gjson.GetBytes(rawJSON, paths.ResponseID); v.Exists() {
		tpl, _ = sjson.Set(tpl, "message.id", v.String())
	}
	buf.WriteString(fmt.Sprintf("data: %s\n\n\n", tpl))

	s.HasFirstResponse = true
}

// AppendMessageDelta appends the final content_block_stop and message_delta SSE events
// with usage information.
func AppendMessageDelta(buf *strings.Builder, s *StreamState, rawJSON []byte, paths PathConfig, usedTool bool) {
	usageResult := gjson.GetBytes(rawJSON, paths.UsageMetadata)
	if !usageResult.Exists() || !bytes.Contains(rawJSON, paths.FinishReasonRaw) {
		return
	}
	candidatesTokenCountResult := usageResult.Get("candidatesTokenCount")
	if !candidatesTokenCountResult.Exists() {
		return
	}
	if !s.HasContent {
		return
	}

	// Close the final content block
	closeBlock(buf, s.ResponseIndex)

	// Build message_delta with usage
	buf.WriteString("event: message_delta\n")
	buf.WriteString(`data: `)

	tpl := `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`
	if usedTool {
		tpl = `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`
	} else if finish := gjson.GetBytes(rawJSON, paths.FinishReason); finish.Exists() && finish.String() == "MAX_TOKENS" {
		tpl = `{"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`
	}

	thoughtsTokenCount := usageResult.Get("thoughtsTokenCount").Int()
	tpl, _ = sjson.Set(tpl, "usage.output_tokens", candidatesTokenCountResult.Int()+thoughtsTokenCount)
	tpl, _ = sjson.Set(tpl, "usage.input_tokens", usageResult.Get("promptTokenCount").Int())

	buf.WriteString(tpl)
	buf.WriteString("\n\n\n")
}

// BuildNonStreamResponse converts a complete Gemini response to a non-streaming Claude response.
func BuildNonStreamResponse(rawJSON []byte, paths PathConfig) string {
	root := gjson.ParseBytes(rawJSON)

	out := `{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`
	out, _ = sjson.Set(out, "id", root.Get(paths.ResponseID).String())
	out, _ = sjson.Set(out, "model", root.Get(paths.ModelVersion).String())

	inputTokens := root.Get(paths.UsageMetadata + ".promptTokenCount").Int()
	outputTokens := root.Get(paths.UsageMetadata+".candidatesTokenCount").Int() +
		root.Get(paths.UsageMetadata+".thoughtsTokenCount").Int()
	out, _ = sjson.Set(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.Set(out, "usage.output_tokens", outputTokens)

	parts := root.Get(paths.Parts)
	textBuilder := strings.Builder{}
	thinkingBuilder := strings.Builder{}
	toolIDCounter := 0
	hasToolCall := false

	flushText := func() {
		if textBuilder.Len() == 0 {
			return
		}
		block := `{"type":"text","text":""}`
		block, _ = sjson.Set(block, "text", textBuilder.String())
		out, _ = sjson.SetRaw(out, "content.-1", block)
		textBuilder.Reset()
	}

	flushThinking := func() {
		if thinkingBuilder.Len() == 0 {
			return
		}
		block := `{"type":"thinking","thinking":""}`
		block, _ = sjson.Set(block, "thinking", thinkingBuilder.String())
		out, _ = sjson.SetRaw(out, "content.-1", block)
		thinkingBuilder.Reset()
	}

	if parts.IsArray() {
		for _, part := range parts.Array() {
			if text := part.Get("text"); text.Exists() && text.String() != "" {
				if part.Get("thought").Bool() {
					flushText()
					thinkingBuilder.WriteString(text.String())
					continue
				}
				flushThinking()
				textBuilder.WriteString(text.String())
				continue
			}

			if functionCall := part.Get("functionCall"); functionCall.Exists() {
				flushThinking()
				flushText()
				hasToolCall = true

				name := functionCall.Get("name").String()
				toolIDCounter++
				toolBlock := `{"type":"tool_use","id":"","name":"","input":{}}`
				toolBlock, _ = sjson.Set(toolBlock, "id", fmt.Sprintf("tool_%d", toolIDCounter))
				toolBlock, _ = sjson.Set(toolBlock, "name", name)
				inputRaw := "{}"
				if args := functionCall.Get("args"); args.Exists() && gjson.Valid(args.Raw) && args.IsObject() {
					inputRaw = args.Raw
				}
				toolBlock, _ = sjson.SetRaw(toolBlock, "input", inputRaw)
				out, _ = sjson.SetRaw(out, "content.-1", toolBlock)
				continue
			}
		}
	}

	flushThinking()
	flushText()

	stopReason := "end_turn"
	if hasToolCall {
		stopReason = "tool_use"
	} else {
		if finish := root.Get(paths.FinishReason); finish.Exists() {
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
	out, _ = sjson.Set(out, "stop_reason", stopReason)

	if inputTokens == 0 && outputTokens == 0 && !root.Get(paths.UsageMetadata).Exists() {
		out, _ = sjson.Delete(out, "usage")
	}

	return out
}

// ClaudeTokenCount formats a token count response in Claude API format.
func ClaudeTokenCount(_ context.Context, count int64) string {
	return fmt.Sprintf(`{"input_tokens":%d}`, count)
}
