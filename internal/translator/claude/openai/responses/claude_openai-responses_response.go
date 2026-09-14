package responses

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type claudeToResponsesState struct {
	Seq                int
	ResponseID         string
	CreatedAt          int64
	NextOutputIndex    int
	CurrentMsgID       string
	CurrentFCID        string
	InTextBlock        bool
	InFuncBlock        bool
	MessageOpen        bool
	ContentPartOpen    bool
	MessageOutputIndex int
	FuncArgsBuf        map[int]*strings.Builder // index -> args
	// function call bookkeeping for output aggregation
	FuncNames         map[int]string // Claude block index -> function name
	FuncCallIDs       map[int]string // Claude block index -> call id
	FuncCustom        map[int]bool   // Claude block index -> freeform custom tool
	FuncCustomSent    map[int]int
	FuncOutputIndices map[int]int // Claude block index -> Responses output index
	ToolIdentity      *translatorcommon.ResponsesToolIdentity
	// message text aggregation
	TextBuf            strings.Builder
	CurrentTextBuf     strings.Builder
	MessageAnnotations []any
	MessageItems       []claudeResponsesMessageItem
	// reasoning state
	ReasoningActive    bool
	ReasoningItemID    string
	ReasoningBuf       strings.Builder
	ReasoningSignature string
	ReasoningIndex     int
	ReasoningItems     []claudeResponsesReasoningItem
	// usage aggregation
	Usage claudeResponsesUsageTokens
}

type claudeResponsesMessageItem struct {
	ID          string
	OutputIndex int
	Text        string
	Annotations []any
}

type claudeResponsesReasoningItem struct {
	ID          string
	OutputIndex int
	Text        string
	Signature   string
}

type claudeResponsesUsageTokens struct {
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	HasUsage                 bool
}

var dataTag = []byte("data:")

// claudeResponsesRedactedThinkingPrefix marks a Responses reasoning item whose
// encrypted_content carries an Anthropic redacted_thinking payload instead of a
// thinking signature. Responses has no redacted reasoning item type, and
// Anthropic requires redacted_thinking blocks to be replayed verbatim, so the
// payload rides in encrypted_content behind this marker and is restored on the
// way back. The marker is not a valid signature for any provider, so a foreign
// upstream drops the block instead of replaying an unusable value.
const claudeResponsesRedactedThinkingPrefix = "claude-redacted-thinking:"

// claudeReasoningCarrier returns the encrypted_content value for the Responses
// reasoning item that mirrors a Claude thinking or redacted_thinking block.
// Streaming thinking blocks usually announce an empty signature and fill it in
// through signature_delta, so an empty result here is expected and later
// replaced.
func claudeReasoningCarrier(contentBlock gjson.Result) string {
	if contentBlock.Get("type").String() == "redacted_thinking" {
		if data := contentBlock.Get("data"); data.Exists() && data.String() != "" {
			return claudeResponsesRedactedThinkingPrefix + data.String()
		}
		return ""
	}
	if signature := contentBlock.Get("signature"); signature.Exists() {
		return signature.String()
	}
	return ""
}

func (u *claudeResponsesUsageTokens) merge(usage gjson.Result) {
	if !usage.Exists() {
		return
	}
	u.HasUsage = true
	if inputTokens := usage.Get("input_tokens"); inputTokens.Exists() {
		u.InputTokens = inputTokens.Int()
	}
	if outputTokens := usage.Get("output_tokens"); outputTokens.Exists() {
		u.OutputTokens = outputTokens.Int()
	}
	if cacheCreationInputTokens := usage.Get("cache_creation_input_tokens"); cacheCreationInputTokens.Exists() {
		u.CacheCreationInputTokens = cacheCreationInputTokens.Int()
	}
	if cacheReadInputTokens := usage.Get("cache_read_input_tokens"); cacheReadInputTokens.Exists() {
		u.CacheReadInputTokens = cacheReadInputTokens.Int()
	}
}

func (u *claudeResponsesUsageTokens) openAIResponsesUsage() (inputTokens, outputTokens, totalTokens, cachedTokens int64) {
	cachedTokens = u.CacheReadInputTokens
	inputTokens = u.InputTokens + u.CacheCreationInputTokens + cachedTokens
	outputTokens = u.OutputTokens
	totalTokens = inputTokens + outputTokens
	return inputTokens, outputTokens, totalTokens, cachedTokens
}

func pickRequestJSON(originalRequestRawJSON, requestRawJSON []byte) []byte {
	if len(originalRequestRawJSON) > 0 && gjson.ValidBytes(originalRequestRawJSON) {
		return originalRequestRawJSON
	}
	if len(requestRawJSON) > 0 && gjson.ValidBytes(requestRawJSON) {
		return requestRawJSON
	}
	return nil
}

func emitEvent(event string, payload []byte) []byte {
	return translatorcommon.SSEEventData(event, payload)
}

func noSSEOutput(out [][]byte) [][]byte {
	if out == nil {
		return [][]byte{}
	}
	return out
}

func (st *claudeToResponsesState) appendMessageAnnotation(annotation any) {
	if annotation == nil {
		return
	}
	st.MessageAnnotations = append(st.MessageAnnotations, annotation)
}

func (st *claudeToResponsesState) allocateOutputIndex() int {
	index := st.NextOutputIndex
	st.NextOutputIndex++
	return index
}

func (st *claudeToResponsesState) messageOutputIndex() int {
	if st.MessageOutputIndex < 0 {
		st.MessageOutputIndex = st.allocateOutputIndex()
	}
	return st.MessageOutputIndex
}

func (st *claudeToResponsesState) functionOutputIndex(blockIndex int) int {
	if index, ok := st.FuncOutputIndices[blockIndex]; ok {
		return index
	}
	index := st.allocateOutputIndex()
	st.FuncOutputIndices[blockIndex] = index
	return index
}

func (st *claudeToResponsesState) finalizeAssistantMessage(nextSeq func() int) [][]byte {
	if !st.MessageOpen {
		return nil
	}
	fullText := st.TextBuf.String()
	outputIndex := st.messageOutputIndex()
	var out [][]byte
	done := []byte(`{"type":"response.output_text.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"text":"","logprobs":[]}`)
	done, _ = sjson.SetBytes(done, "sequence_number", nextSeq())
	done, _ = sjson.SetBytes(done, "item_id", st.CurrentMsgID)
	done, _ = sjson.SetBytes(done, "output_index", outputIndex)
	done, _ = sjson.SetBytes(done, "text", fullText)
	out = append(out, emitEvent("response.output_text.done", done))

	partDone := []byte(`{"type":"response.content_part.done","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`)
	partDone, _ = sjson.SetBytes(partDone, "sequence_number", nextSeq())
	partDone, _ = sjson.SetBytes(partDone, "item_id", st.CurrentMsgID)
	partDone, _ = sjson.SetBytes(partDone, "output_index", outputIndex)
	partDone, _ = sjson.SetBytes(partDone, "part.text", fullText)
	if len(st.MessageAnnotations) > 0 {
		partDone, _ = sjson.SetBytes(partDone, "part.annotations", st.MessageAnnotations)
	}
	out = append(out, emitEvent("response.content_part.done", partDone))

	final := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}}`)
	final, _ = sjson.SetBytes(final, "sequence_number", nextSeq())
	final, _ = sjson.SetBytes(final, "output_index", outputIndex)
	final, _ = sjson.SetBytes(final, "item.id", st.CurrentMsgID)
	final, _ = sjson.SetBytes(final, "item.content.0.text", fullText)
	if len(st.MessageAnnotations) > 0 {
		final, _ = sjson.SetBytes(final, "item.content.0.annotations", st.MessageAnnotations)
	}
	out = append(out, emitEvent("response.output_item.done", final))

	st.MessageItems = append(
		st.MessageItems, claudeResponsesMessageItem{
			ID:          st.CurrentMsgID,
			OutputIndex: outputIndex,
			Text:        fullText,
			Annotations: append([]any(nil), st.MessageAnnotations...),
		},
	)
	st.InTextBlock = false
	st.MessageOpen = false
	st.ContentPartOpen = false
	st.CurrentMsgID = ""
	st.MessageOutputIndex = -1
	st.TextBuf.Reset()
	st.CurrentTextBuf.Reset()
	st.MessageAnnotations = nil
	return out
}

// convertClaudeResponseToOpenAIResponses converts Claude SSE to OpenAI Responses SSE events.
func convertClaudeResponseToOpenAIResponses(
	_ context.Context,
	modelName string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) [][]byte {
	if *param == nil {
		*param = &claudeToResponsesState{
			MessageOutputIndex: -1,
			ReasoningIndex:     -1,
			FuncArgsBuf:        make(map[int]*strings.Builder),
			FuncNames:          make(map[int]string),
			FuncCallIDs:        make(map[int]string),
			FuncCustom:         make(map[int]bool),
			FuncOutputIndices:  make(map[int]int),
		}
	}
	st := (*param).(*claudeToResponsesState)
	if st.ToolIdentity == nil {
		st.ToolIdentity = translatorcommon.NewResponsesToolIdentity(
			pickRequestJSON(
				originalRequestRawJSON, requestRawJSON,
			),
		)
	}

	// Expect `data: {..}` from Claude clients
	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	rawJSON = bytes.TrimSpace(rawJSON[5:])
	root := gjson.ParseBytes(rawJSON)
	ev := root.Get("type").String()
	var out [][]byte

	nextSeq := func() int { st.Seq++; return st.Seq }
	emitCustomInputDelta := func(idx int, final bool) {
		if !st.FuncCustom[idx] {
			return
		}
		arguments := ""
		if buffer := st.FuncArgsBuf[idx]; buffer != nil {
			arguments = buffer.String()
		}
		input, ok := translatorcommon.ParseResponsesCustomToolInput(arguments)
		if !ok {
			if !final {
				return
			}
			input = translatorcommon.UnwrapResponsesCustomToolInput(arguments)
		}
		sent := st.FuncCustomSent[idx]
		if sent < 0 || sent > len(input) {
			sent = 0
		}
		if sent == len(input) {
			return
		}
		callID := st.FuncCallIDs[idx]
		if callID == "" {
			callID = st.CurrentFCID
		}
		delta := []byte(`{"type":"response.custom_tool_call_input.delta","sequence_number":0,"item_id":"","output_index":0,"delta":""}`)
		delta, _ = sjson.SetBytes(delta, "sequence_number", nextSeq())
		delta, _ = sjson.SetBytes(delta, "item_id", fmt.Sprintf("ctc_%s", callID))
		delta, _ = sjson.SetBytes(delta, "output_index", st.functionOutputIndex(idx))
		delta, _ = sjson.SetBytes(delta, "delta", input[sent:])
		out = append(out, emitEvent("response.custom_tool_call_input.delta", delta))
		st.FuncCustomSent[idx] = len(input)
	}

	switch ev {
	case "message_start":
		if msg := root.Get("message"); msg.Exists() {
			st.ResponseID = msg.Get("id").String()
			st.CreatedAt = time.Now().Unix()
			// Reset per-message aggregation state
			st.TextBuf.Reset()
			st.CurrentTextBuf.Reset()
			st.MessageAnnotations = nil
			st.MessageItems = nil
			st.ReasoningBuf.Reset()
			st.ReasoningActive = false
			st.NextOutputIndex = 0
			st.InTextBlock = false
			st.InFuncBlock = false
			st.MessageOpen = false
			st.ContentPartOpen = false
			st.CurrentMsgID = ""
			st.CurrentFCID = ""
			st.MessageOutputIndex = -1
			st.ReasoningItemID = ""
			st.ReasoningSignature = ""
			st.ReasoningIndex = -1
			st.ReasoningItems = nil
			st.FuncArgsBuf = make(map[int]*strings.Builder)
			st.FuncNames = make(map[int]string)
			st.FuncCallIDs = make(map[int]string)
			st.FuncCustom = make(map[int]bool)
			st.FuncCustomSent = make(map[int]int)
			st.FuncOutputIndices = make(map[int]int)
			st.Usage = claudeResponsesUsageTokens{}
			st.Usage.merge(msg.Get("usage"))
			// response.created
			created := []byte(`{"type":"response.created","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"output":[]}}`)
			created, _ = sjson.SetBytes(created, "sequence_number", nextSeq())
			created, _ = sjson.SetBytes(created, "response.id", st.ResponseID)
			created, _ = sjson.SetBytes(created, "response.created_at", st.CreatedAt)
			requestModelName := translatorcommon.RequestModelName(originalRequestRawJSON, requestRawJSON)
			if requestModelName == "" {
				requestModelName = modelName
			}
			if requestModelName != "" {
				created, _ = sjson.SetBytes(created, "response.model", requestModelName)
			}
			out = append(out, emitEvent("response.created", created))
			// response.in_progress
			inprog := []byte(`{"type":"response.in_progress","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","output":[]}}`)
			inprog, _ = sjson.SetBytes(inprog, "sequence_number", nextSeq())
			inprog, _ = sjson.SetBytes(inprog, "response.id", st.ResponseID)
			inprog, _ = sjson.SetBytes(inprog, "response.created_at", st.CreatedAt)
			if requestModelName != "" {
				inprog, _ = sjson.SetBytes(inprog, "response.model", requestModelName)
			}
			out = append(out, emitEvent("response.in_progress", inprog))
		}
	case "content_block_start":
		cb := root.Get("content_block")
		if !cb.Exists() {
			return noSSEOutput(out)
		}
		idx := int(root.Get("index").Int())
		typ := cb.Get("type").String()
		if typ == "text" {
			st.InTextBlock = true
			outputIndex := st.messageOutputIndex()
			if st.CurrentMsgID == "" {
				st.CurrentMsgID = fmt.Sprintf("msg_%s_%d", st.ResponseID, len(st.MessageItems))
			}
			if !st.MessageOpen {
				item := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"message","status":"in_progress","content":[],"role":"assistant"}}`)
				item, _ = sjson.SetBytes(item, "sequence_number", nextSeq())
				item, _ = sjson.SetBytes(item, "output_index", outputIndex)
				item, _ = sjson.SetBytes(item, "item.id", st.CurrentMsgID)
				out = append(out, emitEvent("response.output_item.added", item))
				st.MessageOpen = true
			}
			if !st.ContentPartOpen {
				part := []byte(`{"type":"response.content_part.added","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}`)
				part, _ = sjson.SetBytes(part, "sequence_number", nextSeq())
				part, _ = sjson.SetBytes(part, "item_id", st.CurrentMsgID)
				part, _ = sjson.SetBytes(part, "output_index", outputIndex)
				out = append(out, emitEvent("response.content_part.added", part))
				st.ContentPartOpen = true
			}
		} else if typ == "tool_use" {
			out = append(out, st.finalizeAssistantMessage(nextSeq)...)
			st.InFuncBlock = true
			st.CurrentFCID = cb.Get("id").String()
			name := cb.Get("name").String()
			isCustomTool := st.ToolIdentity.IsCustom(name)
			st.FuncCustom[idx] = isCustomTool
			outputIndex := st.functionOutputIndex(idx)
			var item []byte
			if isCustomTool {
				item = []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"custom_tool_call","status":"in_progress","input":"","call_id":"","name":""}}`)
				item, _ = sjson.SetBytes(item, "item.id", fmt.Sprintf("ctc_%s", st.CurrentFCID))
				item, _ = sjson.SetBytes(item, "item.call_id", st.CurrentFCID)
				item = st.ToolIdentity.SetToolCallIdentity(item, name, "item")
			} else {
				item = []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"in_progress","arguments":"","call_id":"","name":""}}`)
				item, _ = sjson.SetBytes(item, "item.id", fmt.Sprintf("fc_%s", st.CurrentFCID))
				item, _ = sjson.SetBytes(item, "item.call_id", st.CurrentFCID)
				item = st.ToolIdentity.SetToolCallIdentity(item, name, "item")
			}
			item, _ = sjson.SetBytes(item, "sequence_number", nextSeq())
			item, _ = sjson.SetBytes(item, "output_index", outputIndex)
			out = append(out, emitEvent("response.output_item.added", item))
			if st.FuncArgsBuf[idx] == nil {
				st.FuncArgsBuf[idx] = &strings.Builder{}
			}
			// Record function metadata for aggregation.
			st.FuncCallIDs[idx] = st.CurrentFCID
			st.FuncNames[idx] = name
		} else if typ == "thinking" || typ == "redacted_thinking" {
			out = append(out, st.finalizeAssistantMessage(nextSeq)...)
			// start reasoning item
			st.ReasoningActive = true
			st.ReasoningIndex = st.allocateOutputIndex()
			st.ReasoningBuf.Reset()
			st.ReasoningSignature = claudeReasoningCarrier(cb)
			st.ReasoningItemID = fmt.Sprintf("rs_%s_%d", st.ResponseID, idx)
			item := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","status":"in_progress","encrypted_content":"","summary":[]}}`)
			item, _ = sjson.SetBytes(item, "sequence_number", nextSeq())
			item, _ = sjson.SetBytes(item, "output_index", st.ReasoningIndex)
			item, _ = sjson.SetBytes(item, "item.id", st.ReasoningItemID)
			item, _ = sjson.SetBytes(item, "item.encrypted_content", st.ReasoningSignature)
			out = append(out, emitEvent("response.output_item.added", item))
			// add a summary part placeholder
			part := []byte(`{"type":"response.reasoning_summary_part.added","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
			part, _ = sjson.SetBytes(part, "sequence_number", nextSeq())
			part, _ = sjson.SetBytes(part, "item_id", st.ReasoningItemID)
			part, _ = sjson.SetBytes(part, "output_index", st.ReasoningIndex)
			out = append(out, emitEvent("response.reasoning_summary_part.added", part))
		}
	case "content_block_delta":
		d := root.Get("delta")
		if !d.Exists() {
			return noSSEOutput(out)
		}
		dt := d.Get("type").String()
		if dt == "text_delta" {
			if t := d.Get("text"); t.Exists() {
				msg := []byte(`{"type":"response.output_text.delta","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"delta":"","logprobs":[]}`)
				msg, _ = sjson.SetBytes(msg, "sequence_number", nextSeq())
				msg, _ = sjson.SetBytes(msg, "item_id", st.CurrentMsgID)
				msg, _ = sjson.SetBytes(msg, "output_index", st.messageOutputIndex())
				msg, _ = sjson.SetBytes(msg, "delta", t.String())
				out = append(out, emitEvent("response.output_text.delta", msg))
				// aggregate text for response.output
				st.TextBuf.WriteString(t.String())
				st.CurrentTextBuf.WriteString(t.String())
			}
		} else if dt == "input_json_delta" {
			if !st.InFuncBlock || st.CurrentFCID == "" {
				return [][]byte{}
			}
			idx := int(root.Get("index").Int())
			if pj := d.Get("partial_json"); pj.Exists() {
				if st.FuncArgsBuf[idx] == nil {
					st.FuncArgsBuf[idx] = &strings.Builder{}
				}
				st.FuncArgsBuf[idx].WriteString(pj.String())
				if st.FuncCustom[idx] {
					emitCustomInputDelta(idx, false)
					return noSSEOutput(out)
				}
				outputIndex := st.functionOutputIndex(idx)
				msg := []byte(`{"type":"response.function_call_arguments.delta","sequence_number":0,"item_id":"","output_index":0,"delta":""}`)
				msg, _ = sjson.SetBytes(msg, "sequence_number", nextSeq())
				msg, _ = sjson.SetBytes(msg, "item_id", fmt.Sprintf("fc_%s", st.CurrentFCID))
				msg, _ = sjson.SetBytes(msg, "output_index", outputIndex)
				msg, _ = sjson.SetBytes(msg, "delta", pj.String())
				out = append(out, emitEvent("response.function_call_arguments.delta", msg))
			}
		} else if dt == "thinking_delta" {
			if st.ReasoningActive {
				if t := d.Get("thinking"); t.Exists() {
					st.ReasoningBuf.WriteString(t.String())
					msg := []byte(`{"type":"response.reasoning_summary_text.delta","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"delta":""}`)
					msg, _ = sjson.SetBytes(msg, "sequence_number", nextSeq())
					msg, _ = sjson.SetBytes(msg, "item_id", st.ReasoningItemID)
					msg, _ = sjson.SetBytes(msg, "output_index", st.ReasoningIndex)
					msg, _ = sjson.SetBytes(msg, "delta", t.String())
					out = append(out, emitEvent("response.reasoning_summary_text.delta", msg))
				}
			}
		} else if dt == "signature_delta" {
			if st.ReasoningActive {
				if signature := d.Get("signature"); signature.Exists() && signature.String() != "" {
					st.ReasoningSignature = signature.String()
				}
			}
			return [][]byte{}
		} else if dt == "citations_delta" {
			if citation := d.Get("citation"); citation.Exists() {
				st.appendMessageAnnotation(citation.Value())
			}
			return [][]byte{}
		}
	case "content_block_stop":
		idx := int(root.Get("index").Int())
		if st.InTextBlock {
			st.InTextBlock = false
		} else if st.InFuncBlock {
			outputIndex := st.functionOutputIndex(idx)
			args := "{}"
			if st.FuncCustom[idx] {
				args = ""
			}
			if buf := st.FuncArgsBuf[idx]; buf != nil {
				if buf.Len() > 0 {
					args = buf.String()
				}
			}
			if st.FuncCustom[idx] {
				emitCustomInputDelta(idx, true)
				input := translatorcommon.UnwrapResponsesCustomToolInput(args)
				inputDone := []byte(`{"type":"response.custom_tool_call_input.done","sequence_number":0,"item_id":"","output_index":0,"input":""}`)
				inputDone, _ = sjson.SetBytes(inputDone, "sequence_number", nextSeq())
				inputDone, _ = sjson.SetBytes(inputDone, "item_id", fmt.Sprintf("ctc_%s", st.CurrentFCID))
				inputDone, _ = sjson.SetBytes(inputDone, "output_index", outputIndex)
				inputDone, _ = sjson.SetBytes(inputDone, "input", input)
				out = append(out, emitEvent("response.custom_tool_call_input.done", inputDone))

				itemDone := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"custom_tool_call","status":"completed","input":"","call_id":"","name":""}}`)
				itemDone, _ = sjson.SetBytes(itemDone, "sequence_number", nextSeq())
				itemDone, _ = sjson.SetBytes(itemDone, "output_index", outputIndex)
				itemDone, _ = sjson.SetBytes(itemDone, "item.id", fmt.Sprintf("ctc_%s", st.CurrentFCID))
				itemDone, _ = sjson.SetBytes(itemDone, "item.input", input)
				itemDone, _ = sjson.SetBytes(itemDone, "item.call_id", st.CurrentFCID)
				itemDone = st.ToolIdentity.SetToolCallIdentity(itemDone, st.FuncNames[idx], "item")
				out = append(out, emitEvent("response.output_item.done", itemDone))
			} else {
				fcDone := []byte(`{"type":"response.function_call_arguments.done","sequence_number":0,"item_id":"","output_index":0,"arguments":""}`)
				fcDone, _ = sjson.SetBytes(fcDone, "sequence_number", nextSeq())
				fcDone, _ = sjson.SetBytes(fcDone, "item_id", fmt.Sprintf("fc_%s", st.CurrentFCID))
				fcDone, _ = sjson.SetBytes(fcDone, "output_index", outputIndex)
				fcDone, _ = sjson.SetBytes(fcDone, "arguments", args)
				out = append(out, emitEvent("response.function_call_arguments.done", fcDone))
				itemDone := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}}`)
				itemDone, _ = sjson.SetBytes(itemDone, "sequence_number", nextSeq())
				itemDone, _ = sjson.SetBytes(itemDone, "output_index", outputIndex)
				itemDone, _ = sjson.SetBytes(itemDone, "item.id", fmt.Sprintf("fc_%s", st.CurrentFCID))
				itemDone, _ = sjson.SetBytes(itemDone, "item.arguments", args)
				itemDone, _ = sjson.SetBytes(itemDone, "item.call_id", st.CurrentFCID)
				itemDone = st.ToolIdentity.SetToolCallIdentity(itemDone, st.FuncNames[idx], "item")
				out = append(out, emitEvent("response.output_item.done", itemDone))
			}
			st.InFuncBlock = false
		} else if st.ReasoningActive {
			full := st.ReasoningBuf.String()
			textDone := []byte(`{"type":"response.reasoning_summary_text.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"text":""}`)
			textDone, _ = sjson.SetBytes(textDone, "sequence_number", nextSeq())
			textDone, _ = sjson.SetBytes(textDone, "item_id", st.ReasoningItemID)
			textDone, _ = sjson.SetBytes(textDone, "output_index", st.ReasoningIndex)
			textDone, _ = sjson.SetBytes(textDone, "text", full)
			out = append(out, emitEvent("response.reasoning_summary_text.done", textDone))
			partDone := []byte(`{"type":"response.reasoning_summary_part.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
			partDone, _ = sjson.SetBytes(partDone, "sequence_number", nextSeq())
			partDone, _ = sjson.SetBytes(partDone, "item_id", st.ReasoningItemID)
			partDone, _ = sjson.SetBytes(partDone, "output_index", st.ReasoningIndex)
			partDone, _ = sjson.SetBytes(partDone, "part.text", full)
			out = append(out, emitEvent("response.reasoning_summary_part.done", partDone))
			itemDone := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","encrypted_content":"","summary":[]}}`)
			itemDone, _ = sjson.SetBytes(itemDone, "sequence_number", nextSeq())
			itemDone, _ = sjson.SetBytes(itemDone, "item.id", st.ReasoningItemID)
			itemDone, _ = sjson.SetBytes(itemDone, "output_index", st.ReasoningIndex)
			itemDone, _ = sjson.SetBytes(itemDone, "item.encrypted_content", st.ReasoningSignature)
			summary := []byte(`{"type":"summary_text","text":""}`)
			summary, _ = sjson.SetBytes(summary, "text", full)
			itemDone, _ = sjson.SetRawBytes(itemDone, "item.summary.-1", summary)
			out = append(out, emitEvent("response.output_item.done", itemDone))
			st.ReasoningItems = append(
				st.ReasoningItems, claudeResponsesReasoningItem{
					ID:          st.ReasoningItemID,
					OutputIndex: st.ReasoningIndex,
					Text:        full,
					Signature:   st.ReasoningSignature,
				},
			)
			st.ReasoningActive = false
			st.ReasoningItemID = ""
			st.ReasoningBuf.Reset()
			st.ReasoningSignature = ""
			st.ReasoningIndex = -1
		}
		return noSSEOutput(out)
	case "message_delta":
		st.Usage.merge(root.Get("usage"))
		return [][]byte{}
	case "message_stop":
		out = append(out, st.finalizeAssistantMessage(nextSeq)...)

		completed := []byte(`{"type":"response.completed","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null}}`)
		completed, _ = sjson.SetBytes(completed, "sequence_number", nextSeq())
		completed, _ = sjson.SetBytes(completed, "response.id", st.ResponseID)
		completed, _ = sjson.SetBytes(completed, "response.created_at", st.CreatedAt)
		// Inject original request fields into response as per docs/response.completed.json

		reqBytes := pickRequestJSON(originalRequestRawJSON, requestRawJSON)
		if len(reqBytes) > 0 {
			completed = translatorcommon.EchoRequestFields(
				completed, gjson.ParseBytes(reqBytes), "response.",
			)
		}

		// Build response.output from aggregated state
		outputsWrapper := []byte(`{"arr":[]}`)
		// reasoning items
		for _, reasoning := range st.ReasoningItems {
			item := []byte(`{"id":"","type":"reasoning","encrypted_content":"","summary":[]}`)
			item, _ = sjson.SetBytes(item, "id", reasoning.ID)
			item, _ = sjson.SetBytes(item, "encrypted_content", reasoning.Signature)
			summary := []byte(`{"type":"summary_text","text":""}`)
			summary, _ = sjson.SetBytes(summary, "text", reasoning.Text)
			item, _ = sjson.SetRawBytes(item, "summary.-1", summary)
			outputsWrapper, _ = sjson.SetRawBytes(outputsWrapper, fmt.Sprintf("arr.%d", reasoning.OutputIndex), item)
		}
		// assistant message items
		for _, message := range st.MessageItems {
			item := []byte(`{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}`)
			item, _ = sjson.SetBytes(item, "id", message.ID)
			item, _ = sjson.SetBytes(item, "content.0.text", message.Text)
			if len(message.Annotations) > 0 {
				item, _ = sjson.SetBytes(item, "content.0.annotations", message.Annotations)
			}
			outputsWrapper, _ = sjson.SetRawBytes(outputsWrapper, fmt.Sprintf("arr.%d", message.OutputIndex), item)
		}
		// function_call items (in ascending index order for determinism)
		if len(st.FuncArgsBuf) > 0 {
			// collect indices
			indices := make([]int, 0, len(st.FuncArgsBuf))
			for idx := range st.FuncArgsBuf {
				indices = append(indices, idx)
			}
			// simple sort (small N), avoid adding new imports
			for i := 0; i < len(indices); i++ {
				for j := i + 1; j < len(indices); j++ {
					if indices[j] < indices[i] {
						indices[i], indices[j] = indices[j], indices[i]
					}
				}
			}
			for _, idx := range indices {
				args := "{}"
				if st.FuncCustom[idx] {
					args = ""
				}
				if b := st.FuncArgsBuf[idx]; b != nil && b.Len() > 0 {
					args = b.String()
				}
				callID := st.FuncCallIDs[idx]
				name := st.FuncNames[idx]
				if callID == "" && st.CurrentFCID != "" {
					callID = st.CurrentFCID
				}
				if st.FuncCustom[idx] {
					item := []byte(`{"id":"","type":"custom_tool_call","status":"completed","input":"","call_id":"","name":""}`)
					item, _ = sjson.SetBytes(item, "id", fmt.Sprintf("ctc_%s", callID))
					item, _ = sjson.SetBytes(item, "input", translatorcommon.UnwrapResponsesCustomToolInput(args))
					item, _ = sjson.SetBytes(item, "call_id", callID)
					item = st.ToolIdentity.SetToolCallIdentity(item, name, "")
					outputsWrapper, _ = sjson.SetRawBytes(
						outputsWrapper, fmt.Sprintf("arr.%d", st.FuncOutputIndices[idx]), item,
					)
				} else {
					item := []byte(`{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}`)
					item, _ = sjson.SetBytes(item, "id", fmt.Sprintf("fc_%s", callID))
					item, _ = sjson.SetBytes(item, "arguments", args)
					item, _ = sjson.SetBytes(item, "call_id", callID)
					item = st.ToolIdentity.SetToolCallIdentity(item, name, "")
					outputsWrapper, _ = sjson.SetRawBytes(
						outputsWrapper, fmt.Sprintf("arr.%d", st.FuncOutputIndices[idx]), item,
					)
				}
			}
		}
		if gjson.GetBytes(outputsWrapper, "arr.#").Int() > 0 {
			completed, _ = sjson.SetRawBytes(
				completed, "response.output", []byte(gjson.GetBytes(outputsWrapper, "arr").Raw),
			)
		}

		reasoningLength := 0
		for _, reasoning := range st.ReasoningItems {
			reasoningLength += len(reasoning.Text)
		}
		reasoningTokens := int64(reasoningLength / 4)
		usagePresent := st.Usage.HasUsage || reasoningTokens > 0
		if usagePresent {
			inputTokens, outputTokens, totalTokens, cachedTokens := st.Usage.openAIResponsesUsage()
			completed, _ = sjson.SetBytes(completed, "response.usage.input_tokens", inputTokens)
			completed, _ = sjson.SetBytes(completed, "response.usage.input_tokens_details.cached_tokens", cachedTokens)
			completed, _ = sjson.SetBytes(completed, "response.usage.output_tokens", outputTokens)
			completed, _ = sjson.SetBytes(
				completed, "response.usage.output_tokens_details.reasoning_tokens", reasoningTokens,
			)
			if totalTokens > 0 || st.Usage.HasUsage {
				completed, _ = sjson.SetBytes(completed, "response.usage.total_tokens", totalTokens)
			}
		}
		out = append(out, emitEvent("response.completed", completed))
	}

	return noSSEOutput(out)
}

// convertClaudeResponseToOpenAIResponsesNonStream aggregates Claude SSE into a single OpenAI Responses JSON.
func convertClaudeResponseToOpenAIResponsesNonStream(
	_ context.Context,
	_ string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	_ *any,
) []byte {
	// Aggregate Claude SSE lines into a single OpenAI Responses JSON (non-stream)
	// We follow the same aggregation logic as the streaming variant but produce
	// one final object matching docs/out.json structure.

	// Collect SSE data lines and ignore other lines.
	var chunks [][]byte
	{
		// Use a simple scanner to iterate through raw bytes
		// Note: extremely large responses may require increasing the buffer
		scanner := bufio.NewScanner(bytes.NewReader(rawJSON))
		buf := make([]byte, 52_428_800) // 50MB
		scanner.Buffer(buf, 52_428_800)
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.HasPrefix(line, dataTag) {
				continue
			}
			chunks = append(chunks, line[len(dataTag):])
		}
	}

	reqBytes := pickRequestJSON(originalRequestRawJSON, requestRawJSON)
	toolIdentity := translatorcommon.NewResponsesToolIdentity(reqBytes)

	// Base OpenAI Responses (non-stream) object
	out := []byte(`{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null,"incomplete_details":null,"output":[],"usage":{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{},"total_tokens":0}}`)

	// Aggregation state
	var (
		responseID  string
		createdAt   int64
		usageTokens claudeResponsesUsageTokens
	)

	type nonStreamOutputItem struct {
		outputIndex int
		itemType    string
		id          string
		callID      string
		name        string
		text        strings.Builder
		signature   string
		annotations []any
		args        strings.Builder
	}

	blockToItem := make(map[int]*nonStreamOutputItem)
	outputItems := make([]*nonStreamOutputItem, 0)
	nextOutputIndex := 0
	messageCount := 0
	var activeMessageItem *nonStreamOutputItem
	var pendingAnnotations []any

	allocateOutputIndex := func() int {
		outputIndex := nextOutputIndex
		nextOutputIndex++
		return outputIndex
	}
	newOutputItem := func(itemType string, blockIndex int) *nonStreamOutputItem {
		item := &nonStreamOutputItem{
			outputIndex: allocateOutputIndex(),
			itemType:    itemType,
		}
		outputItems = append(outputItems, item)
		blockToItem[blockIndex] = item
		return item
	}

	// Walk through SSE chunks to fill state
	for _, ch := range chunks {
		root := gjson.ParseBytes(ch)
		ev := root.Get("type").String()

		switch ev {
		case "message_start":
			if msg := root.Get("message"); msg.Exists() {
				responseID = msg.Get("id").String()
				createdAt = time.Now().Unix()
				usageTokens.merge(msg.Get("usage"))
			}

		case "content_block_start":
			cb := root.Get("content_block")
			if !cb.Exists() {
				continue
			}
			idx := int(root.Get("index").Int())
			typ := cb.Get("type").String()
			switch typ {
			case "text":
				item := newOutputItem("message", idx)
				item.id = fmt.Sprintf("msg_%s_%d", responseID, messageCount)
				messageCount++
				if len(pendingAnnotations) > 0 {
					item.annotations = append(item.annotations, pendingAnnotations...)
					pendingAnnotations = nil
				}
				activeMessageItem = item
			case "tool_use":
				activeMessageItem = nil
				itemType := "function_call"
				if toolIdentity.IsCustom(cb.Get("name").String()) {
					itemType = "custom_tool_call"
				}
				item := newOutputItem(itemType, idx)
				item.callID = cb.Get("id").String()
				if itemType == "custom_tool_call" {
					item.id = fmt.Sprintf("ctc_%s", item.callID)
				} else {
					item.id = fmt.Sprintf("fc_%s", item.callID)
				}
				item.name = cb.Get("name").String()
			case "thinking", "redacted_thinking":
				activeMessageItem = nil
				item := newOutputItem("reasoning", idx)
				item.id = fmt.Sprintf("rs_%s_%d", responseID, idx)
				item.signature = claudeReasoningCarrier(cb)
			}

		case "content_block_delta":
			d := root.Get("delta")
			if !d.Exists() {
				continue
			}
			idx := int(root.Get("index").Int())
			item := blockToItem[idx]
			dt := d.Get("type").String()
			switch dt {
			case "text_delta":
				if item != nil && item.itemType == "message" {
					if t := d.Get("text"); t.Exists() {
						item.text.WriteString(t.String())
					}
				}
			case "input_json_delta":
				if item != nil && (item.itemType == "function_call" || item.itemType == "custom_tool_call") {
					if pj := d.Get("partial_json"); pj.Exists() {
						item.args.WriteString(pj.String())
					}
				}
			case "thinking_delta":
				if item != nil && item.itemType == "reasoning" {
					if t := d.Get("thinking"); t.Exists() {
						item.text.WriteString(t.String())
					}
				}
			case "signature_delta":
				if item != nil && item.itemType == "reasoning" {
					if signature := d.Get("signature"); signature.Exists() && signature.String() != "" {
						item.signature = signature.String()
					}
				}
			case "citations_delta":
				if citation := d.Get("citation"); citation.Exists() {
					if item != nil && item.itemType == "message" {
						item.annotations = append(item.annotations, citation.Value())
					} else if activeMessageItem != nil {
						activeMessageItem.annotations = append(activeMessageItem.annotations, citation.Value())
					} else {
						pendingAnnotations = append(pendingAnnotations, citation.Value())
					}
				}
			}

		case "content_block_stop":
			// Output items are finalized after all deltas have been aggregated.

		case "message_delta":
			usageTokens.merge(root.Get("usage"))
		}
	}

	// Populate base fields
	out, _ = sjson.SetBytes(out, "id", responseID)
	out, _ = sjson.SetBytes(out, "created_at", createdAt)

	// Inject request echo fields as top-level, matching the streaming variant.
	if len(reqBytes) > 0 {
		out = translatorcommon.EchoRequestFields(out, gjson.ParseBytes(reqBytes), "")
	}

	// Build output array in the order of the original content blocks.
	outputsWrapper := []byte(`{"arr":[]}`)
	for _, outputItem := range outputItems {
		var item []byte
		switch outputItem.itemType {
		case "reasoning":
			item = []byte(`{"id":"","type":"reasoning","encrypted_content":"","summary":[]}`)
			item, _ = sjson.SetBytes(item, "id", outputItem.id)
			item, _ = sjson.SetBytes(item, "encrypted_content", outputItem.signature)
			summary := []byte(`{"type":"summary_text","text":""}`)
			summary, _ = sjson.SetBytes(summary, "text", outputItem.text.String())
			item, _ = sjson.SetRawBytes(item, "summary.-1", summary)
		case "message":
			item = []byte(`{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}`)
			item, _ = sjson.SetBytes(item, "id", outputItem.id)
			item, _ = sjson.SetBytes(item, "content.0.text", outputItem.text.String())
			if len(outputItem.annotations) > 0 {
				item, _ = sjson.SetBytes(item, "content.0.annotations", outputItem.annotations)
			}
		case "function_call", "custom_tool_call":
			if outputItem.itemType == "custom_tool_call" {
				item = []byte(`{"id":"","type":"custom_tool_call","status":"completed","input":"","call_id":"","name":""}`)
				item, _ = sjson.SetBytes(item, "id", outputItem.id)
				item, _ = sjson.SetBytes(
					item, "input", translatorcommon.UnwrapResponsesCustomToolInput(outputItem.args.String()),
				)
				item, _ = sjson.SetBytes(item, "call_id", outputItem.callID)
				item = toolIdentity.SetToolCallIdentity(item, outputItem.name, "")
				break
			}
			args := outputItem.args.String()
			if args == "" {
				args = "{}"
			}
			item = []byte(`{"id":"","type":"function_call","status":"completed","arguments":"","call_id":"","name":""}`)
			item, _ = sjson.SetBytes(item, "id", outputItem.id)
			item, _ = sjson.SetBytes(item, "arguments", args)
			item, _ = sjson.SetBytes(item, "call_id", outputItem.callID)
			item = toolIdentity.SetToolCallIdentity(item, outputItem.name, "")
		}
		if len(item) > 0 {
			outputsWrapper, _ = sjson.SetRawBytes(outputsWrapper, fmt.Sprintf("arr.%d", outputItem.outputIndex), item)
		}
	}
	if gjson.GetBytes(outputsWrapper, "arr.#").Int() > 0 {
		out, _ = sjson.SetRawBytes(out, "output", []byte(gjson.GetBytes(outputsWrapper, "arr").Raw))
	}

	// Usage
	inputTokens, outputTokens, totalTokens, cachedTokens := usageTokens.openAIResponsesUsage()
	if inputTokens != 0 {
		out, _ = sjson.SetBytes(out, "usage.input_tokens", inputTokens)
	}
	if cachedTokens != 0 {
		out, _ = sjson.SetBytes(out, "usage.input_tokens_details.cached_tokens", cachedTokens)
	}
	if outputTokens != 0 {
		out, _ = sjson.SetBytes(out, "usage.output_tokens", outputTokens)
	}
	if totalTokens != 0 {
		out, _ = sjson.SetBytes(out, "usage.total_tokens", totalTokens)
	}
	reasoningLength := 0
	for _, outputItem := range outputItems {
		if outputItem.itemType == "reasoning" {
			reasoningLength += outputItem.text.Len()
		}
	}
	if usageTokens.HasUsage || reasoningLength > 0 {
		// Rough estimate similar to chat completions.
		reasoningTokens := int64(reasoningLength / 4)
		out, _ = sjson.SetBytes(out, "usage.output_tokens_details.reasoning_tokens", reasoningTokens)
	}

	return out
}
