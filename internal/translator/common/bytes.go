package common

import (
	"fmt"
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// EchoRequestFields copies known OpenAI Responses API request fields from req
// into dest under the given key prefix. For streaming response.completed events
// the prefix is "response." ; for non-streaming responses it is "".
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

// ClaudeSSEEvent is a single SSE event to emit.
type ClaudeSSEEvent struct {
	Event string
	Data  string
}

// EmitTextOrThinkingBlock generates the SSE events needed to stream a Gemini
// text/thought part as Claude-format content blocks. It mutates st in place.
func EmitTextOrThinkingBlock(st *ClaudeBlockState, text string, isThought bool) []ClaudeSSEEvent {
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

	var events []ClaudeSSEEvent
	if st.ResponseType == targetType {
		// Continue existing block
		events = append(events, ClaudeSSEEvent{"content_block_delta", makeDelta()})
		st.HasContent = true
	} else {
		// Transition: close existing block if any
		if st.ResponseType != 0 {
			events = append(
				events, ClaudeSSEEvent{
					"content_block_stop",
					fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, st.ResponseIndex),
				},
			)
			st.ResponseIndex++
		}
		// Start new block
		events = append(
			events, ClaudeSSEEvent{
				"content_block_start",
				fmt.Sprintf(
					`{"type":"content_block_start","index":%d,"content_block":{"type":"%s","%s":""}}`,
					st.ResponseIndex, blockType, deltaKey,
				),
			},
		)
		events = append(events, ClaudeSSEEvent{"content_block_delta", makeDelta()})
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

func SSEEventData(event string, payload []byte) []byte {
	out := make([]byte, 0, len(event)+len(payload)+14)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, payload...)
	return out
}

func AppendSSEEventString(out []byte, event, payload string, trailingNewlines int) []byte {
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	out = append(out, payload...)
	for i := 0; i < trailingNewlines; i++ {
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
	for i := 0; i < trailingNewlines; i++ {
		out = append(out, '\n')
	}
	return out
}
