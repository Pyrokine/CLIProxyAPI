package responses

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type claudeToResponsesState struct {
	Seq                 int
	ResponseID          string
	CreatedAt           int64
	NextOutputIndex     int
	CurrentMsgID        string
	InTextBlock         bool
	MessageOpen         bool
	ContentPartOpen     bool
	MessageOutputIndex  int
	ToolCalls           map[int]*claudeResponsesToolCall
	ToolIdentity        *translatorcommon.ResponsesToolIdentity
	ToolNamesReverseMap map[string]string
	// message text aggregation
	TextBuf            strings.Builder
	MessageAnnotations []any
	MessageItems       []claudeResponsesMessageItem
	// reasoning state
	ReasoningActive     bool
	ReasoningItemID     string
	ReasoningBuf        strings.Builder
	ReasoningSignature  string
	ReasoningIndex      int
	ReasoningBlockIndex int
	ReasoningItems      []claudeResponsesReasoningItem
	ReasoningStopped    bool
	// server-side web search aggregation
	WebSearchByBlock  map[int]*claudeResponsesWebSearchItem
	WebSearchByToolID map[string]*claudeResponsesWebSearchItem
	WebSearchItems    []*claudeResponsesWebSearchItem
	StopReason        string
	// usage aggregation
	Usage  claudeResponsesUsageTokens
	closed bool
	err    error
}

type claudeResponsesToolCall struct {
	BlockIndex       int
	OutputIndex      int
	CallID           string
	Name             string
	Arguments        strings.Builder
	Snapshot         string
	SnapshotError    error
	IdentityConflict bool
	Patch            bool
	Custom           bool
	Added            bool
	Done             bool
	Stopped          bool
	ArgumentsSent    int
	CustomInputSent  int
	Status           string
}

func (call *claudeResponsesToolCall) itemID() string {
	if call.Custom || call.Patch {
		return "ctc_" + call.CallID
	}
	return "fc_" + call.CallID
}

func (call *claudeResponsesToolCall) arguments(status string) string {
	if call.Snapshot != "" && (call.Patch || call.Arguments.Len() == 0) {
		return call.Snapshot
	}
	arguments := call.Arguments.String()
	if arguments == "" && !call.Custom && status != "incomplete" {
		return "{}"
	}
	return arguments
}

func (call *claudeResponsesToolCall) recordSnapshot(input gjson.Result) {
	if !input.Exists() || input.IsObject() && len(input.Map()) == 0 {
		return
	}
	var decoded translatorcommon.ResponsesPatchInput
	_, err := decoded.Finish(input.Raw)
	if err == nil && call.Snapshot != "" {
		var previous translatorcommon.ResponsesPatchInput
		if _, err = previous.Finish(call.Snapshot); err == nil && previous.Input() != decoded.Input() {
			err = fmt.Errorf("conflicting complete apply_patch snapshots")
		}
	}
	if call.SnapshotError == nil {
		call.SnapshotError = err
	}
	call.Snapshot = strings.Clone(input.Raw)
}

func (call *claudeResponsesToolCall) validatePatchEvidence() error {
	if !call.Patch {
		return nil
	}
	if call.IdentityConflict {
		return fmt.Errorf("conflicting apply_patch call identity")
	}
	if call.SnapshotError != nil {
		return call.SnapshotError
	}
	if call.Snapshot != "" {
		var arguments, snapshot translatorcommon.ResponsesPatchInput
		if _, err := arguments.Push(call.Arguments.String()); err != nil {
			return err
		}
		if _, err := snapshot.Finish(call.Snapshot); err != nil {
			return err
		}
		if !strings.HasPrefix(snapshot.Input(), arguments.Input()) ||
			(gjson.Valid(call.Arguments.String()) && arguments.Input() != snapshot.Input()) {
			return fmt.Errorf("apply_patch arguments conflict with snapshot")
		}
	}
	return nil
}

type claudeResponsesWebSearchItem struct {
	ToolUseID   string
	OutputIndex int
	InputBuf    strings.Builder
	Results     []byte
	Emitted     bool
	Status      string
}

func (item *claudeResponsesWebSearchItem) render() []byte {
	out := buildResponsesWebSearchCallItem(item.ToolUseID, claudeWebSearchQuery(item.InputBuf.String()), item.Results)
	if item.Status != "" {
		out, _ = sjson.SetBytes(out, "status", item.Status)
	}
	return out
}

type claudeResponsesMessageItem struct {
	ID          string
	OutputIndex int
	Text        string
	Annotations []any
	Status      string
}

type claudeResponsesReasoningItem struct {
	ID          string
	OutputIndex int
	Text        string
	Signature   string
	Status      string
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

func claudeResponsesTerminalState(stopReason string) (event, status string, details []byte) {
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "max_tokens":
		return "response.incomplete", "incomplete", []byte(`{"reason":"max_output_tokens"}`)
	case "pause_turn":
		return "response.incomplete", "incomplete", []byte(`null`)
	default:
		return "response.completed", "completed", nil
	}
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

func (st *claudeToResponsesState) ResponseTranslationError() error {
	return st.err
}

func (st *claudeToResponsesState) failToolInput(err error, nextSeq func() int) [][]byte {
	if st.err != nil || st.closed {
		return nil
	}
	st.err = err
	st.closed = true
	payload := []byte(`{"type":"response.failed","sequence_number":0,"response":{"id":"","status":"failed","error":{"code":"invalid_tool_arguments","message":""}}}`)
	payload, _ = sjson.SetBytes(payload, "sequence_number", nextSeq())
	payload, _ = sjson.SetBytes(payload, "response.id", st.ResponseID)
	payload, _ = sjson.SetBytes(payload, "response.error.message", err.Error())
	return [][]byte{emitEvent("response.failed", payload)}
}

func (st *claudeToResponsesState) appendMessageAnnotation(annotation any) {
	if annotation != nil {
		st.MessageAnnotations = append(st.MessageAnnotations, annotation)
	}
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

func (st *claudeToResponsesState) toolCall(blockIndex int) *claudeResponsesToolCall {
	if call := st.ToolCalls[blockIndex]; call != nil {
		return call
	}
	call := &claudeResponsesToolCall{BlockIndex: blockIndex, OutputIndex: st.allocateOutputIndex()}
	st.ToolCalls[blockIndex] = call
	return call
}

func (st *claudeToResponsesState) toolCallIndices() []int {
	indices := make([]int, 0, len(st.ToolCalls))
	for index := range st.ToolCalls {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	return indices
}

func (st *claudeToResponsesState) isPatch(name string) bool {
	declaration, err := st.ToolIdentity.ResolveDeclaration(name, "")
	return err == nil && declaration.Kind == translatorcommon.ResponsesCustomTool && declaration.Name == "apply_patch"
}

func (st *claudeToResponsesState) toolItem(call *claudeResponsesToolCall, status string) []byte {
	item := []byte(`{"id":"","type":"function_call","status":"","arguments":"","call_id":"","name":""}`)
	if call.Custom {
		item = []byte(`{"id":"","type":"custom_tool_call","status":"","input":"","call_id":"","name":""}`)
		item, _ = sjson.SetBytes(item, "input", translatorcommon.UnwrapResponsesCustomToolInput(call.arguments(status)))
	} else {
		item, _ = translatorcommon.SetStringWithoutHTMLEscape(item, "arguments", call.arguments(status))
	}
	item, _ = sjson.SetBytes(item, "id", call.itemID())
	item, _ = sjson.SetBytes(item, "call_id", call.CallID)
	item, _ = sjson.SetBytes(item, "status", status)
	return st.ToolIdentity.SetToolCallIdentity(item, call.Name, "")
}

func (st *claudeToResponsesState) emitFuncItem(call *claudeResponsesToolCall, force bool, nextSeq func() int) [][]byte {
	if force && call.Name == "" {
		if name, ok := st.ToolIdentity.SingleCustomName(); ok && st.isPatch(name) {
			call.Name = name
		}
	}
	call.Patch = st.isPatch(call.Name)
	call.Custom = st.ToolIdentity.IsCustom(call.Name) && !call.Patch
	if err := call.validatePatchEvidence(); err != nil {
		return st.failToolInput(err, nextSeq)
	}
	if call.Added {
		return nil
	}
	if !force && (call.Name == "" || call.CallID == "") {
		return nil
	}
	if call.Name == "" {
		return st.failToolInput(fmt.Errorf("tool call name is missing"), nextSeq)
	}
	if call.CallID == "" {
		call.CallID = fmt.Sprintf("call_%s_%d", st.ResponseID, call.BlockIndex)
	}
	item := st.toolItem(call, "in_progress")
	field := "arguments"
	if call.Custom {
		field = "input"
	}
	item, _ = sjson.SetBytes(item, field, "")
	added := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{}}`)
	added, _ = sjson.SetBytes(added, "sequence_number", nextSeq())
	added, _ = sjson.SetBytes(added, "output_index", call.OutputIndex)
	added, _ = sjson.SetRawBytes(added, "item", item)
	call.Added = true
	return [][]byte{emitEvent("response.output_item.added", added)}
}

func (st *claudeToResponsesState) emitFuncArgs(call *claudeResponsesToolCall, final bool, nextSeq func() int) [][]byte {
	if !call.Added || call.Done || st.err != nil {
		return nil
	}
	arguments := call.Arguments.String()
	if call.Custom {
		input, ok := translatorcommon.ParseResponsesCustomToolInput(arguments)
		if !ok {
			if !final {
				return nil
			}
			input = translatorcommon.UnwrapResponsesCustomToolInput(arguments)
		}
		if call.CustomInputSent >= len(input) {
			return nil
		}
		delta := []byte(`{"type":"response.custom_tool_call_input.delta","sequence_number":0,"item_id":"","output_index":0,"delta":""}`)
		delta, _ = sjson.SetBytes(delta, "sequence_number", nextSeq())
		delta, _ = sjson.SetBytes(delta, "item_id", call.itemID())
		delta, _ = sjson.SetBytes(delta, "output_index", call.OutputIndex)
		delta, _ = sjson.SetBytes(delta, "delta", input[call.CustomInputSent:])
		call.CustomInputSent = len(input)
		return [][]byte{emitEvent("response.custom_tool_call_input.delta", delta)}
	}
	if call.ArgumentsSent >= len(arguments) {
		return nil
	}
	delta := []byte(`{"type":"response.function_call_arguments.delta","sequence_number":0,"item_id":"","output_index":0,"delta":""}`)
	delta, _ = sjson.SetBytes(delta, "sequence_number", nextSeq())
	delta, _ = sjson.SetBytes(delta, "item_id", call.itemID())
	delta, _ = sjson.SetBytes(delta, "output_index", call.OutputIndex)
	delta, _ = translatorcommon.SetStringWithoutHTMLEscape(delta, "delta", arguments[call.ArgumentsSent:])
	call.ArgumentsSent = len(arguments)
	return [][]byte{emitEvent("response.function_call_arguments.delta", delta)}
}

func (st *claudeToResponsesState) finalizeFuncItem(call *claudeResponsesToolCall, status string, nextSeq func() int) [][]byte {
	if call.Done || st.err != nil {
		return nil
	}
	out := st.emitFuncItem(call, true, nextSeq)
	if st.err != nil {
		return out
	}
	out = append(out, st.emitFuncArgs(call, true, nextSeq)...)
	kind, field := "response.function_call_arguments.done", "arguments"
	value := call.arguments(status)
	if call.Custom {
		kind, field = "response.custom_tool_call_input.done", "input"
		value = translatorcommon.UnwrapResponsesCustomToolInput(value)
	}
	done := []byte(`{"type":"","sequence_number":0,"item_id":"","output_index":0}`)
	done, _ = sjson.SetBytes(done, "type", kind)
	done, _ = sjson.SetBytes(done, "sequence_number", nextSeq())
	done, _ = sjson.SetBytes(done, "item_id", call.itemID())
	done, _ = sjson.SetBytes(done, "output_index", call.OutputIndex)
	done, _ = translatorcommon.SetStringWithoutHTMLEscape(done, field, value)
	out = append(out, emitEvent(kind, done))
	itemDone := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{}}`)
	itemDone, _ = sjson.SetBytes(itemDone, "sequence_number", nextSeq())
	itemDone, _ = sjson.SetBytes(itemDone, "output_index", call.OutputIndex)
	itemDone, _ = sjson.SetRawBytes(itemDone, "item", st.toolItem(call, status))
	out = append(out, emitEvent("response.output_item.done", itemDone))
	call.Done = true
	call.Status = status
	return out
}

func (st *claudeToResponsesState) startWebSearch(blockIndex int, toolUseID string) *claudeResponsesWebSearchItem {
	item := &claudeResponsesWebSearchItem{ToolUseID: toolUseID, OutputIndex: st.allocateOutputIndex()}
	st.WebSearchByBlock[blockIndex] = item
	st.WebSearchByToolID[toolUseID] = item
	st.WebSearchItems = append(st.WebSearchItems, item)
	return item
}

func (st *claudeToResponsesState) finalizeWebSearch(item *claudeResponsesWebSearchItem, status string, nextSeq func() int) [][]byte {
	if item == nil || item.Emitted {
		return nil
	}
	item.Emitted = true
	item.Status = status
	done := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{}}`)
	done, _ = sjson.SetBytes(done, "sequence_number", nextSeq())
	done, _ = sjson.SetBytes(done, "output_index", item.OutputIndex)
	done, _ = sjson.SetRawBytes(done, "item", item.render())
	return [][]byte{emitEvent("response.output_item.done", done)}
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
	_, status, _ := claudeResponsesTerminalState(st.StopReason)
	final, _ = sjson.SetBytes(final, "item.status", status)
	final, _ = sjson.SetBytes(final, "item.content.0.text", fullText)
	if len(st.MessageAnnotations) > 0 {
		final, _ = sjson.SetBytes(final, "item.content.0.annotations", st.MessageAnnotations)
	}
	out = append(out, emitEvent("response.output_item.done", final))
	st.MessageItems = append(st.MessageItems, claudeResponsesMessageItem{
		ID: st.CurrentMsgID, OutputIndex: outputIndex, Text: fullText,
		Annotations: append([]any(nil), st.MessageAnnotations...), Status: status,
	})
	st.InTextBlock = false
	st.MessageOpen = false
	st.ContentPartOpen = false
	st.CurrentMsgID = ""
	st.MessageOutputIndex = -1
	st.TextBuf.Reset()
	st.MessageAnnotations = nil
	return out
}

func (st *claudeToResponsesState) finalizeReasoningItem(status string, nextSeq func() int) [][]byte {
	if !st.ReasoningActive {
		return nil
	}
	full := st.ReasoningBuf.String()
	textDone := []byte(`{"type":"response.reasoning_summary_text.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"text":""}`)
	textDone, _ = sjson.SetBytes(textDone, "sequence_number", nextSeq())
	textDone, _ = sjson.SetBytes(textDone, "item_id", st.ReasoningItemID)
	textDone, _ = sjson.SetBytes(textDone, "output_index", st.ReasoningIndex)
	textDone, _ = sjson.SetBytes(textDone, "text", full)
	partDone := []byte(`{"type":"response.reasoning_summary_part.done","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
	partDone, _ = sjson.SetBytes(partDone, "sequence_number", nextSeq())
	partDone, _ = sjson.SetBytes(partDone, "item_id", st.ReasoningItemID)
	partDone, _ = sjson.SetBytes(partDone, "output_index", st.ReasoningIndex)
	partDone, _ = sjson.SetBytes(partDone, "part.text", full)
	itemDone := []byte(`{"type":"response.output_item.done","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","encrypted_content":"","summary":[]}}`)
	itemDone, _ = sjson.SetBytes(itemDone, "sequence_number", nextSeq())
	itemDone, _ = sjson.SetBytes(itemDone, "item.id", st.ReasoningItemID)
	itemDone, _ = sjson.SetBytes(itemDone, "output_index", st.ReasoningIndex)
	itemDone, _ = sjson.SetBytes(itemDone, "item.encrypted_content", st.ReasoningSignature)
	itemDone, _ = sjson.SetBytes(itemDone, "item.status", status)
	summary := []byte(`{"type":"summary_text","text":""}`)
	summary, _ = sjson.SetBytes(summary, "text", full)
	itemDone, _ = sjson.SetRawBytes(itemDone, "item.summary.-1", summary)
	st.ReasoningItems = append(st.ReasoningItems, claudeResponsesReasoningItem{
		ID: st.ReasoningItemID, OutputIndex: st.ReasoningIndex, Text: full,
		Signature: st.ReasoningSignature, Status: status,
	})
	st.ReasoningActive = false
	st.ReasoningStopped = false
	st.ReasoningItemID = ""
	st.ReasoningBuf.Reset()
	st.ReasoningSignature = ""
	st.ReasoningIndex = -1
	st.ReasoningBlockIndex = -1
	return [][]byte{emitEvent("response.reasoning_summary_text.done", textDone),
		emitEvent("response.reasoning_summary_part.done", partDone), emitEvent("response.output_item.done", itemDone)}
}

// convertClaudeResponseToOpenAIResponses converts Claude SSE to OpenAI Responses SSE events.
func convertClaudeResponseToOpenAIResponses(
	_ context.Context,
	modelName string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) [][]byte {
	if param == nil {
		param = new(any)
	}
	if *param == nil {
		request := pickRequestJSON(originalRequestRawJSON, requestRawJSON)
		identity := translatorcommon.NewResponsesToolIdentity(request)
		*param = &claudeToResponsesState{
			MessageOutputIndex: -1, ReasoningIndex: -1, ReasoningBlockIndex: -1,
			ToolCalls: make(map[int]*claudeResponsesToolCall), ToolIdentity: identity,
			ToolNamesReverseMap: util.ReverseToolNameMap(claudeResponsesFunctionNameMap(identity, gjson.ParseBytes(request))),
			WebSearchByBlock:    make(map[int]*claudeResponsesWebSearchItem),
			WebSearchByToolID:   make(map[string]*claudeResponsesWebSearchItem),
		}
	}
	st := (*param).(*claudeToResponsesState)
	if st.closed || st.err != nil {
		return nil
	}
	nextSeq := func() int { st.Seq++; return st.Seq }
	// Expect `data: {..}` from Claude clients.
	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	root := gjson.ParseBytes(bytes.TrimSpace(rawJSON[len(dataTag):]))
	var out [][]byte
	switch root.Get("type").String() {
	case "message_start":
		if msg := root.Get("message"); msg.Exists() {
			st.ResponseID = msg.Get("id").String()
			st.CreatedAt = time.Now().Unix()
			st.Usage.merge(msg.Get("usage"))
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
			return nil
		}
		idx := int(root.Get("index").Int())
		typ := cb.Get("type").String()
		if typ != "text" {
			out = append(out, st.finalizeAssistantMessage(nextSeq)...)
		}
		if st.ReasoningStopped {
			out = append(out, st.finalizeReasoningItem("completed", nextSeq)...)
		}
		for _, previous := range st.toolCallIndices() {
			call := st.ToolCalls[previous]
			if previous != idx && !call.Done && call.Stopped && call.Name != "" && typ != "tool_use" {
				out = append(out, st.finalizeFuncItem(call, "completed", nextSeq)...)
				if st.err != nil {
					return out
				}
			}
		}
		for _, item := range st.WebSearchItems {
			if !item.Emitted && item.Results != nil {
				out = append(out, st.finalizeWebSearch(item, "completed", nextSeq)...)
			}
		}
		switch typ {
		case "text":
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
			for _, citation := range cb.Get("citations").Array() {
				st.appendMessageAnnotation(citation.Value())
			}
		case "tool_use":
			call := st.toolCall(idx)
			id := cb.Get("id").String()
			name := util.RestoreSanitizedToolName(st.ToolNamesReverseMap, cb.Get("name").String())
			if id != "" && call.CallID != "" && id != call.CallID {
				call.IdentityConflict = true
			}
			if name != "" && call.Name != "" && name != call.Name && (call.Patch || st.isPatch(name)) {
				call.IdentityConflict = true
			}
			if !call.Added {
				if id != "" {
					call.CallID = id
				}
				if name != "" {
					call.Name = name
				}
			}
			call.recordSnapshot(cb.Get("input"))
			out = append(out, st.emitFuncItem(call, false, nextSeq)...)
			if st.err != nil {
				return out
			}
			out = append(out, st.emitFuncArgs(call, false, nextSeq)...)
		case "server_tool_use":
			if cb.Get("name").String() == claudeWebSearchToolName {
				item := st.startWebSearch(idx, cb.Get("id").String())
				if input := cb.Get("input"); input.IsObject() && claudeWebSearchQuery(input.Raw) != "" {
					item.InputBuf.WriteString(input.Raw)
				}
				added := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"web_search_call","status":"in_progress","action":{"type":"search","query":""}}}`)
				added, _ = sjson.SetBytes(added, "sequence_number", nextSeq())
				added, _ = sjson.SetBytes(added, "output_index", item.OutputIndex)
				added, _ = sjson.SetBytes(added, "item.id", responsesWebSearchCallID(item.ToolUseID))
				out = append(out, emitEvent("response.output_item.added", added))
			}
		case "web_search_tool_result":
			if item := st.WebSearchByToolID[cb.Get("tool_use_id").String()]; item != nil {
				item.Results = claudeWebSearchResultsToResponses(cb.Get("content"))
			}
		case "thinking", "redacted_thinking":
			st.ReasoningActive = true
			st.ReasoningIndex = st.allocateOutputIndex()
			st.ReasoningBlockIndex = idx
			st.ReasoningSignature = claudeReasoningCarrier(cb)
			st.ReasoningItemID = fmt.Sprintf("rs_%s_%d", st.ResponseID, idx)
			item := []byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"","type":"reasoning","status":"in_progress","encrypted_content":"","summary":[]}}`)
			item, _ = sjson.SetBytes(item, "sequence_number", nextSeq())
			item, _ = sjson.SetBytes(item, "output_index", st.ReasoningIndex)
			item, _ = sjson.SetBytes(item, "item.id", st.ReasoningItemID)
			item, _ = sjson.SetBytes(item, "item.encrypted_content", st.ReasoningSignature)
			out = append(out, emitEvent("response.output_item.added", item))
			part := []byte(`{"type":"response.reasoning_summary_part.added","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`)
			part, _ = sjson.SetBytes(part, "sequence_number", nextSeq())
			part, _ = sjson.SetBytes(part, "item_id", st.ReasoningItemID)
			part, _ = sjson.SetBytes(part, "output_index", st.ReasoningIndex)
			out = append(out, emitEvent("response.reasoning_summary_part.added", part))
		}
	case "content_block_delta":
		d := root.Get("delta")
		idx := int(root.Get("index").Int())
		switch d.Get("type").String() {
		case "text_delta":
			if text := d.Get("text"); text.Exists() {
				msg := []byte(`{"type":"response.output_text.delta","sequence_number":0,"item_id":"","output_index":0,"content_index":0,"delta":"","logprobs":[]}`)
				msg, _ = sjson.SetBytes(msg, "sequence_number", nextSeq())
				msg, _ = sjson.SetBytes(msg, "item_id", st.CurrentMsgID)
				msg, _ = sjson.SetBytes(msg, "output_index", st.messageOutputIndex())
				msg, _ = sjson.SetBytes(msg, "delta", text.String())
				out = append(out, emitEvent("response.output_text.delta", msg))
				st.TextBuf.WriteString(text.String())
			}
		case "input_json_delta":
			if item := st.WebSearchByBlock[idx]; item != nil {
				item.InputBuf.WriteString(d.Get("partial_json").String())
				return nil
			}
			if call := st.toolCall(idx); !call.Done {
				call.Arguments.WriteString(d.Get("partial_json").String())
				if err := call.validatePatchEvidence(); err != nil {
					return st.failToolInput(err, nextSeq)
				}
				out = append(out, st.emitFuncArgs(call, false, nextSeq)...)
			}
		case "thinking_delta":
			if st.ReasoningActive {
				if text := d.Get("thinking"); text.Exists() {
					st.ReasoningBuf.WriteString(text.String())
					msg := []byte(`{"type":"response.reasoning_summary_text.delta","sequence_number":0,"item_id":"","output_index":0,"summary_index":0,"delta":""}`)
					msg, _ = sjson.SetBytes(msg, "sequence_number", nextSeq())
					msg, _ = sjson.SetBytes(msg, "item_id", st.ReasoningItemID)
					msg, _ = sjson.SetBytes(msg, "output_index", st.ReasoningIndex)
					msg, _ = sjson.SetBytes(msg, "delta", text.String())
					out = append(out, emitEvent("response.reasoning_summary_text.delta", msg))
				}
			}
		case "signature_delta":
			if st.ReasoningActive {
				if signature := d.Get("signature"); signature.Exists() && signature.String() != "" {
					st.ReasoningSignature = signature.String()
				}
			}
		case "citations_delta":
			if citation := d.Get("citation"); citation.Exists() {
				st.appendMessageAnnotation(citation.Value())
			}
		}
	case "content_block_stop":
		idx := int(root.Get("index").Int())
		if call := st.ToolCalls[idx]; call != nil {
			call.Stopped = true
			if call.Patch {
				out = append(out, st.finalizeFuncItem(call, "completed", nextSeq)...)
			}
		} else if st.ReasoningActive && st.ReasoningBlockIndex == idx {
			st.ReasoningStopped = true
		} else if st.InTextBlock {
			st.InTextBlock = false
		}
	case "message_delta":
		st.Usage.merge(root.Get("usage"))
		if reason := root.Get("delta.stop_reason"); reason.Exists() {
			st.StopReason = reason.String()
		}
	case "message_stop":
		event, status, details := claudeResponsesTerminalState(st.StopReason)
		out = append(out, st.finalizeAssistantMessage(nextSeq)...)
		out = append(out, st.finalizeReasoningItem(status, nextSeq)...)
		for _, index := range st.toolCallIndices() {
			out = append(out, st.finalizeFuncItem(st.ToolCalls[index], status, nextSeq)...)
			if st.err != nil {
				return out
			}
		}
		for _, item := range st.WebSearchItems {
			out = append(out, st.finalizeWebSearch(item, status, nextSeq)...)
		}
		completed := []byte(`{"type":"response.completed","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null}}`)
		completed, _ = sjson.SetBytes(completed, "type", event)
		completed, _ = sjson.SetBytes(completed, "sequence_number", nextSeq())
		completed, _ = sjson.SetBytes(completed, "response.id", st.ResponseID)
		completed, _ = sjson.SetBytes(completed, "response.created_at", st.CreatedAt)
		completed, _ = sjson.SetBytes(completed, "response.status", status)
		if len(details) > 0 {
			completed, _ = sjson.SetRawBytes(completed, "response.incomplete_details", details)
		}
		if request := pickRequestJSON(originalRequestRawJSON, requestRawJSON); len(request) > 0 {
			completed = translatorcommon.EchoRequestFields(completed, gjson.ParseBytes(request), "response.")
		}
		outputs := []byte(`{"arr":[]}`)
		for _, reasoning := range st.ReasoningItems {
			item := []byte(`{"id":"","type":"reasoning","encrypted_content":"","summary":[]}`)
			item, _ = sjson.SetBytes(item, "id", reasoning.ID)
			item, _ = sjson.SetBytes(item, "encrypted_content", reasoning.Signature)
			item, _ = sjson.SetBytes(item, "status", reasoning.Status)
			summary := []byte(`{"type":"summary_text","text":""}`)
			summary, _ = sjson.SetBytes(summary, "text", reasoning.Text)
			item, _ = sjson.SetRawBytes(item, "summary.-1", summary)
			outputs, _ = sjson.SetRawBytes(outputs, fmt.Sprintf("arr.%d", reasoning.OutputIndex), item)
		}
		for _, message := range st.MessageItems {
			item := []byte(`{"id":"","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":""}],"role":"assistant"}`)
			item, _ = sjson.SetBytes(item, "id", message.ID)
			item, _ = sjson.SetBytes(item, "status", message.Status)
			item, _ = sjson.SetBytes(item, "content.0.text", message.Text)
			if len(message.Annotations) > 0 {
				item, _ = sjson.SetBytes(item, "content.0.annotations", message.Annotations)
			}
			outputs, _ = sjson.SetRawBytes(outputs, fmt.Sprintf("arr.%d", message.OutputIndex), item)
		}
		for _, search := range st.WebSearchItems {
			outputs, _ = sjson.SetRawBytes(outputs, fmt.Sprintf("arr.%d", search.OutputIndex), search.render())
		}
		for _, index := range st.toolCallIndices() {
			call := st.ToolCalls[index]
			outputs, _ = sjson.SetRawBytes(outputs, fmt.Sprintf("arr.%d", call.OutputIndex), st.toolItem(call, call.Status))
		}
		completed, _ = sjson.SetRawBytes(completed, "response.output", []byte(gjson.GetBytes(outputs, "arr").Raw))
		reasoningLength := 0
		for _, reasoning := range st.ReasoningItems {
			reasoningLength += len(reasoning.Text)
		}
		reasoningTokens := int64(reasoningLength / 4)
		if st.Usage.HasUsage || reasoningTokens > 0 {
			inputTokens, outputTokens, totalTokens, cachedTokens := st.Usage.openAIResponsesUsage()
			completed, _ = sjson.SetBytes(completed, "response.usage.input_tokens", inputTokens)
			completed, _ = sjson.SetBytes(completed, "response.usage.input_tokens_details.cached_tokens", cachedTokens)
			completed, _ = sjson.SetBytes(completed, "response.usage.output_tokens", outputTokens)
			completed, _ = sjson.SetBytes(completed, "response.usage.output_tokens_details.reasoning_tokens", reasoningTokens)
			completed, _ = sjson.SetBytes(completed, "response.usage.total_tokens", totalTokens)
		}
		st.closed = true
		out = append(out, emitEvent(event, completed))
	}
	return noSSEOutput(out)
}

// convertClaudeResponseToOpenAIResponsesNonStream aggregates Claude SSE into a single OpenAI Responses JSON.
func convertClaudeResponseToOpenAIResponsesNonStream(
	ctx context.Context,
	modelName string,
	originalRequestRawJSON, requestRawJSON, rawJSON []byte,
	param *any,
) []byte {
	if param == nil {
		param = new(any)
	}
	var response []byte
	feed := func(chunk []byte) {
		for _, frame := range convertClaudeResponseToOpenAIResponses(ctx, modelName, originalRequestRawJSON, requestRawJSON, chunk, param) {
			for _, line := range bytes.Split(frame, []byte("\n")) {
				if bytes.HasPrefix(line, dataTag) {
					payload := gjson.ParseBytes(bytes.TrimSpace(line[len(dataTag):]))
					switch payload.Get("type").String() {
					case "response.completed", "response.incomplete", "response.failed":
						response = []byte(payload.Get("response").Raw)
					}
				}
			}
		}
	}
	native := gjson.ParseBytes(rawJSON)
	nativeModel := ""
	if gjson.ValidBytes(rawJSON) && native.Get("type").String() == "message" {
		nativeModel = native.Get("model").String()
		feed([]byte(`data: {"type":"message_start","message":` + native.Raw + `}`))
		for index, block := range native.Get("content").Array() {
			start := []byte(fmt.Sprintf(`data: {"type":"content_block_start","index":%d,"content_block":%s}`, index, block.Raw))
			feed(start)
			var delta []byte
			switch block.Get("type").String() {
			case "text":
				delta = []byte(`{"type":"text_delta"}`)
				delta, _ = sjson.SetBytes(delta, "text", block.Get("text").String())
			case "thinking":
				delta = []byte(`{"type":"thinking_delta"}`)
				delta, _ = sjson.SetBytes(delta, "thinking", block.Get("thinking").String())
			case "tool_use":
				if input := block.Get("input"); input.Exists() {
					delta = []byte(`{"type":"input_json_delta"}`)
					delta, _ = sjson.SetBytes(delta, "partial_json", input.Raw)
				}
			}
			if len(delta) > 0 {
				feed([]byte(fmt.Sprintf(`data: {"type":"content_block_delta","index":%d,"delta":%s}`, index, delta)))
			}
			feed([]byte(fmt.Sprintf(`data: {"type":"content_block_stop","index":%d}`, index)))
		}
		delta := []byte(`{"type":"message_delta","delta":{}}`)
		delta, _ = sjson.SetBytes(delta, "delta.stop_reason", native.Get("stop_reason").String())
		if usage := native.Get("usage"); usage.Exists() {
			delta, _ = sjson.SetRawBytes(delta, "usage", []byte(usage.Raw))
		}
		feed(append([]byte("data: "), delta...))
	} else {
		scanner := bufio.NewScanner(bytes.NewReader(rawJSON))
		scanner.Buffer(make([]byte, 64*1024), 52_428_800)
		for scanner.Scan() {
			if line := scanner.Bytes(); bytes.HasPrefix(line, dataTag) {
				feed(line)
			}
		}
		if err := scanner.Err(); err != nil {
			if st, ok := (*param).(*claudeToResponsesState); ok {
				st.failToolInput(err, func() int { st.Seq++; return st.Seq })
			}
			return nil
		}
	}
	feed([]byte(`data: {"type":"message_stop"}`))
	if len(response) == 0 {
		return nil
	}
	if nativeModel != "" {
		response, _ = sjson.SetBytes(response, "model", nativeModel)
	}
	if !gjson.GetBytes(response, "usage").Exists() {
		response, _ = sjson.SetRawBytes(response, "usage", []byte(`{"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0,"output_tokens_details":{},"total_tokens":0}`))
	}
	return response
}
