package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// EchoRequestFields copies known OpenAI Responses API request fields from req
// into dest under the given key prefix. The prefix is "response." for streaming
// response.completed events and empty for non-streaming responses.
func EchoRequestFields(dest []byte, req gjson.Result, prefix string) []byte {
	type field struct {
		key  string
		kind byte // s=String, i=Int, b=Bool, f=Float, v=Value(raw JSON)
	}
	fields := [...]field{
		{"instructions", 's'},
		{"max_output_tokens", 'i'},
		{"max_tool_calls", 'i'},
		{"model", 's'},
		{"parallel_tool_calls", 'b'},
		{"previous_response_id", 's'},
		{"prompt_cache_key", 's'},
		{"reasoning", 'v'},
		{"safety_identifier", 's'},
		{"service_tier", 's'},
		{"store", 'b'},
		{"temperature", 'f'},
		{"text", 'v'},
		{"tool_choice", 'v'},
		{"tools", 'v'},
		{"top_logprobs", 'i'},
		{"top_p", 'f'},
		{"truncation", 's'},
		{"user", 'v'},
		{"metadata", 'v'},
	}
	for _, f := range fields {
		v := req.Get(f.key)
		if !v.Exists() {
			continue
		}
		target := prefix + f.key
		switch f.kind {
		case 's':
			dest, _ = sjson.SetBytes(dest, target, v.String())
		case 'i':
			dest, _ = sjson.SetBytes(dest, target, v.Int())
		case 'b':
			dest, _ = sjson.SetBytes(dest, target, v.Bool())
		case 'f':
			dest, _ = sjson.SetBytes(dest, target, v.Float())
		case 'v':
			dest, _ = sjson.SetBytes(dest, target, v.Value())
		}
	}
	return dest
}

// ClaudeBlockState tracks the state machine for streaming Claude-format
// content blocks (thinking / text). Callers should persist this across chunks.
type ClaudeBlockState struct {
	ResponseType  int  // 0=none, 1=text, 2=thinking
	ResponseIndex int  // Current content block index
	HasContent    bool // Whether any content has been emitted
}

// claudeSSEEvent is a single SSE event to emit.
type claudeSSEEvent struct {
	Event string
	Data  string
}

// EmitTextOrThinkingBlock generates the SSE events needed to stream a Gemini
// text/thought part as Claude-format content blocks. It mutates st in place.
func EmitTextOrThinkingBlock(st *ClaudeBlockState, text string, isThought bool) []claudeSSEEvent {
	var (
		targetType int
		deltaType  string
		deltaKey   string
		blockType  string
	)
	if isThought {
		targetType, deltaType, deltaKey, blockType = 2, "thinking_delta", "thinking", "thinking"
	} else {
		targetType, deltaType, deltaKey, blockType = 1, "text_delta", "text", "text"
	}

	makeDelta := func() string {
		data, _ := sjson.SetBytes(
			[]byte(fmt.Sprintf(
				`{"type":"content_block_delta","index":%d,"delta":{"type":"%s","%s":""}}`,
				st.ResponseIndex, deltaType, deltaKey,
			)), "delta."+deltaKey, text,
		)
		return string(data)
	}

	var events []claudeSSEEvent
	if st.ResponseType == targetType {
		// Continue existing block
		events = append(events, claudeSSEEvent{"content_block_delta", makeDelta()})
		st.HasContent = true
	} else {
		// Transition: close existing block if any
		if st.ResponseType != 0 {
			events = append(
				events, claudeSSEEvent{
					"content_block_stop",
					fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, st.ResponseIndex),
				},
			)
			st.ResponseIndex++
		}
		// Start new block
		events = append(
			events, claudeSSEEvent{
				"content_block_start",
				fmt.Sprintf(
					`{"type":"content_block_start","index":%d,"content_block":{"type":"%s","%s":""}}`,
					st.ResponseIndex, blockType, deltaKey,
				),
			},
		)
		events = append(events, claudeSSEEvent{"content_block_delta", makeDelta()})
		st.ResponseType = targetType
		st.HasContent = true
	}
	return events
}

func WrapGeminiCLIResponse(response []byte) []byte {
	out, err := sjson.SetRawBytes([]byte(`{"response":{}}`), "response", response)
	if err != nil {
		return response
	}
	return out
}

func GeminiTokenCountJSON(count int64) []byte {
	out := make([]byte, 0, 96)
	out = append(out, `{"totalTokens":`...)
	out = strconv.AppendInt(out, count, 10)
	out = append(out, `,"promptTokensDetails":[{"modality":"TEXT","tokenCount":`...)
	out = strconv.AppendInt(out, count, 10)
	out = append(out, `}]}`...)
	return out
}

func ClaudeInputTokensJSON(count int64) []byte {
	out := make([]byte, 0, 32)
	out = append(out, `{"input_tokens":`...)
	out = strconv.AppendInt(out, count, 10)
	out = append(out, '}')
	return out
}

func NewRawArrayItems(count int64) [][]byte {
	if count <= 0 {
		return nil
	}
	return make([][]byte, 0, int(count))
}

func JoinRawArray(items [][]byte) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	size := len(items) + 1
	for _, item := range items {
		size += len(item)
	}
	out := make([]byte, 0, size)
	out = append(out, '[')
	for index, item := range items {
		if index > 0 {
			out = append(out, ',')
		}
		out = append(out, item...)
	}
	return append(out, ']')
}

// SetRawArrayItems replaces an empty JSON array at path with raw items.
// The single-item path avoids allocating an intermediate joined array.
func SetRawArrayItems(data []byte, path string, items [][]byte) []byte {
	if len(items) == 0 {
		return data
	}
	if len(items) == 1 {
		array := gjson.GetBytes(data, path)
		if array.Raw == "[]" && array.Index >= 0 && array.Index+len(array.Raw) <= len(data) {
			out := make([]byte, 0, len(data)+len(items[0]))
			out = append(out, data[:array.Index]...)
			out = append(out, '[')
			out = append(out, items[0]...)
			out = append(out, ']')
			return append(out, data[array.Index+len(array.Raw):]...)
		}
	}
	data, _ = sjson.SetRawBytes(data, path, JoinRawArray(items))
	return data
}

func SSEEventData(event string, payload []byte) []byte {
	out := make([]byte, 0, len(event)+len(payload)+16)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, payload...)
	out = append(out, '\n', '\n')
	return out
}

func AppendSSEEventString(out []byte, event, payload string, trailingNewlines int) []byte {
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, payload...)
	for range trailingNewlines {
		out = append(out, '\n')
	}
	return out
}

func AppendSSEEventBytes(out []byte, event string, payload []byte, trailingNewlines int) []byte {
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, payload...)
	for range trailingNewlines {
		out = append(out, '\n')
	}
	return out
}

// SetStringWithoutHTMLEscape writes a JSON string without replacing HTML characters with Unicode escapes.
func SetStringWithoutHTMLEscape(data []byte, path, value string) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("encode JSON string: %w", err)
	}
	return sjson.SetRawBytes(data, path, bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}))
}
