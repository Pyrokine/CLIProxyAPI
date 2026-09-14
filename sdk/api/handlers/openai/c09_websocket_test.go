package openai

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type c09MessageTooBigStatusError struct{}

func (c09MessageTooBigStatusError) Error() string {
	return `{"error":{"message":"upstream websocket message too big","type":"invalid_request_error","code":"message_too_big"}}`
}

func (c09MessageTooBigStatusError) StatusCode() int {
	return http.StatusRequestEntityTooLarge
}

func TestC09WebsocketClosePayloadPreservesUpstream1009(t *testing.T) {
	matched, payload := websocketClosePayloadForUpstreamError(c09MessageTooBigStatusError{})
	if !matched {
		t.Fatal("message-too-big error was not mapped to websocket close")
	}
	if len(payload) < 2 {
		t.Fatalf("close payload length = %d, want at least 2", len(payload))
	}
	if code := int(binary.BigEndian.Uint16(payload[:2])); code != websocket.CloseMessageTooBig {
		t.Fatalf("close code = %d, want %d", code, websocket.CloseMessageTooBig)
	}
	if reason := string(payload[2:]); reason != "upstream websocket message too big" {
		t.Fatalf("close reason = %q", reason)
	}
}

func TestC09WebsocketCloseReasonBoundaryPreservesUTF8(t *testing.T) {
	reason := strings.Repeat("界", 80)
	truncated := truncateWebsocketCloseReason(reason, wsCloseReasonMaxBytes)
	if len(truncated) > wsCloseReasonMaxBytes {
		t.Fatalf("truncated length = %d, want <= %d", len(truncated), wsCloseReasonMaxBytes)
	}
	if !utf8.ValidString(truncated) {
		t.Fatalf("truncated reason is invalid UTF-8: %q", truncated)
	}
}

func TestC09WebsocketReplayCloseRequiresTypedSignal(t *testing.T) {
	matched, payload := websocketClosePayloadForUpstreamError(
		cliproxyexecutor.NewUpstreamWebsocketReplayRequiredError(),
	)
	if !matched || len(payload) < 2 {
		t.Fatalf("replay close matched=%t payload_len=%d", matched, len(payload))
	}
	if code := int(binary.BigEndian.Uint16(payload[:2])); code != websocket.CloseServiceRestart {
		t.Fatalf("close code = %d, want %d", code, websocket.CloseServiceRestart)
	}
	if reason := string(payload[2:]); reason != wsHTTPReplayRequiredCloseReason {
		t.Fatalf("close reason = %q, want %q", reason, wsHTTPReplayRequiredCloseReason)
	}
	if matched, _ := websocketClosePayloadForUpstreamError(c09MessageTooBigStatusError{}); !matched {
		t.Fatal("existing message-too-big mapping regressed")
	}
}

func TestC09LocalCompactionReplacesTranscriptAndOldResponseID(t *testing.T) {
	lastRequest := []byte(`{"model":"gpt-5.6-sol","stream":true,"instructions":"keep","input":[{"type":"message","id":"old-user","role":"user","content":"old"}]}`)
	lastOutput := []byte(`[{"type":"message","id":"old-assistant","role":"assistant","content":"old answer"}]`)
	raw := []byte(fmt.Sprintf(
		`{"type":"response.create","input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","id":"compact-summary","role":"user","content":%q},{"type":"message","id":"new-user","role":"user","content":"continue"}]}`,
		codexLocalCompactionSummaryPrefix+"\nsummary body",
	))

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastOutput, false)
	if errMsg != nil {
		t.Fatalf("normalize error = %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("replacement retained previous_response_id: %s", normalized)
	}
	if gjson.GetBytes(normalized, "input").Raw != gjson.GetBytes(raw, "input").Raw {
		t.Fatalf("replacement did not preserve compacted input: %s", normalized)
	}
	if bytes.Contains(normalized, []byte("old-user")) || bytes.Contains(normalized, []byte("old-assistant")) {
		t.Fatalf("replacement retained stale transcript: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatal("next transcript snapshot differs from replacement")
	}

	for _, previous := range []string{`""`, `null`, `"resp-old"`} {
		withPrevious := []byte(fmt.Sprintf(`{"type":"response.create","previous_response_id":%s}`, previous))
		if shouldReplaceCodexLocalCompactionTranscript(withPrevious, gjson.GetBytes(raw, "input")) {
			t.Fatalf("local compact matched request carrying previous_response_id=%s", previous)
		}
	}
}

func TestC09TransportSwitchUsesFullTranscriptReplay(t *testing.T) {
	lastRequest := []byte(`{"model":"gpt-5","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastOutput := []byte(`[{"type":"message","id":"out-1"}]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-2"}]}`)

	normalized, _, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastOutput, false)
	if errMsg != nil {
		t.Fatalf("normalize error = %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("full replay retained previous_response_id: %s", normalized)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	wantIDs := []string{"msg-1", "out-1", "msg-2"}
	if len(input) != len(wantIDs) {
		t.Fatalf("full replay input len = %d, want %d: %s", len(input), len(wantIDs), normalized)
	}
	for index, wantID := range wantIDs {
		if got := input[index].Get("id").String(); got != wantID {
			t.Fatalf("input[%d].id = %q, want %q", index, got, wantID)
		}
	}

	errMsg = responsesWebsocketPreviousResponseNotFoundError()
	if errMsg.StatusCode != http.StatusConflict || gjson.Get(
		errMsg.Error.Error(), "error.code",
	).String() != "previous_response_not_found" {
		t.Fatalf("fresh old response error = %#v", errMsg)
	}
}

func TestC09ToolCacheTurnPublishesOnlyAfterCommit(t *testing.T) {
	const sessionKey = "c09-tool-cache-commit"
	defer defaultWebsocketToolOutputCache.deleteSession(sessionKey)
	defer defaultWebsocketToolCallCache.deleteSession(sessionKey)

	turn := newResponsesWebsocketToolCacheTurn(sessionKey)
	turn.recordRequest([]byte(`{"input":[{"type":"function_call_output","id":"fco-1","call_id":"call-1","output":"ok"}]}`))
	turn.recordResponse([]byte(`{"type":"response.completed","response":{"output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":"{}"}]}}`))
	probe := []byte(`{"input":[{"type":"function_call_output","id":"fco-next","call_id":"call-1","output":"next"}]}`)
	if count := gjson.GetBytes(
		repairResponsesWebsocketToolCallsWithoutRecording(sessionKey, probe), "input.#",
	).Int(); count != 0 {
		t.Fatalf("uncommitted turn became visible with %d items", count)
	}
	turn.commit()
	if count := gjson.GetBytes(
		repairResponsesWebsocketToolCallsWithoutRecording(sessionKey, probe), "input.#",
	).Int(); count != 2 {
		t.Fatalf("committed turn repair item count = %d, want 2", count)
	}
}

func TestC09ToolCacheTurnCommitIsAtomicallyVisible(t *testing.T) {
	const sessionKey = "c09-tool-cache-atomic"
	retainResponsesWebsocketToolCaches(sessionKey)
	defer releaseResponsesWebsocketToolCaches(sessionKey)

	turn := newResponsesWebsocketToolCacheTurn(sessionKey)
	turn.recordRequest([]byte(`{"input":[{"type":"function_call_output","id":"fco-1","call_id":"call-atomic","output":"ok"}]}`))
	turn.recordResponse([]byte(`{"type":"response.completed","response":{"output":[{"type":"function_call","id":"fc-1","call_id":"call-atomic","name":"lookup","arguments":"{}"}]}}`))
	probe := []byte(`{"input":[{"type":"function_call_output","id":"fco-next","call_id":"call-atomic","output":"next"}]}`)

	start := make(chan struct{})
	errs := make(chan string, 16)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			<-start
			for range 100 {
				count := gjson.GetBytes(
					repairResponsesWebsocketToolCallsWithoutRecording(sessionKey, probe), "input.#",
				).Int()
				if count != 0 && count != 2 {
					errs <- fmt.Sprintf("observed partial cache with %d items", count)
					return
				}
			}
		})
	}
	close(start)
	turn.commit()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestResponseCompletedOutputFromPayloadDropsIncompleteCollectedToolCalls(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[]}}`)
	collector := map[int64][]byte{
		0: []byte(`{"type":"message","id":"msg-1"}`),
		1: []byte(`{"type":"function_call","call_id":"call-1","name":"exec"}`),
		2: []byte(`{"type":"custom_tool_call","call_id":"call-2","name":"exec","input":"pwd"}`),
	}

	output := responseCompletedOutputFromPayload(payload, collector, nil)
	items := gjson.ParseBytes(output).Array()
	if len(items) != 2 {
		t.Fatalf("output len = %d, want 2: %s", len(items), output)
	}
	if items[0].Get("type").String() != "message" || items[0].Get("id").String() != "msg-1" {
		t.Fatalf("unexpected first output item: %s", items[0].Raw)
	}
	if items[1].Get("type").String() != "custom_tool_call" ||
		items[1].Get("call_id").String() != "call-2" {
		t.Fatalf("unexpected second output item: %s", items[1].Raw)
	}
}

func TestRestoreResponsesWebsocketCompletionOutputReconcilesConflictingToolCall(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"message","id":"msg-1"},{"type":"function_call","call_id":"call-1","name":"exec"}]}}`)
	collector := map[int64][]byte{
		0: []byte(`{"type":"custom_tool_call","id":"ctc-1","call_id":"call-1","name":"exec","input":"pwd","status":"completed"}`),
	}

	restored := restoreResponsesWebsocketCompletionOutput(payload, collector, nil)
	output := gjson.GetBytes(restored, "response.output").Array()
	if len(output) != 2 {
		t.Fatalf("restored output len = %d, want 2: %s", len(output), restored)
	}
	if output[0].Get("type").String() != "message" || output[0].Get("id").String() != "msg-1" {
		t.Fatalf("unrelated completion item changed: %s", output[0].Raw)
	}
	if output[1].Get("type").String() != "custom_tool_call" ||
		output[1].Get("call_id").String() != "call-1" {
		t.Fatalf("conflicting tool call was not reconciled: %s", output[1].Raw)
	}
	if input := output[1].Get("input"); input.Type != gjson.String || input.String() != "pwd" {
		t.Fatalf("reconciled custom tool input = %s, want string pwd", input.Raw)
	}

	lastRequest := []byte(`{"model":"gpt-test","stream":true,"input":[{"type":"message","id":"user-1","role":"user","content":"run pwd"}]}`)
	nextRequest := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"custom_tool_call_output","call_id":"call-1","output":"ok"}]}`)
	completedOutput := []byte(gjson.GetBytes(restored, "response.output").Raw)
	normalized, _, errMsg := normalizeResponsesWebsocketRequestWithIncrementalState(
		nextRequest,
		lastRequest,
		completedOutput,
		"resp-1",
		[]string{"call-1"},
		false,
	)
	if errMsg != nil {
		t.Fatalf("normalize next request: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must not be forwarded to HTTP/SSE upstream: %s", normalized)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 4 {
		t.Fatalf("replayed input len = %d, want 4: %s", len(input), normalized)
	}
	if input[2].Get("type").String() != "custom_tool_call" || input[2].Get("input").String() != "pwd" {
		t.Fatalf("replayed tool call is invalid: %s", input[2].Raw)
	}
	if input[3].Get("type").String() != "custom_tool_call_output" ||
		input[3].Get("call_id").String() != "call-1" {
		t.Fatalf("replayed tool output is invalid: %s", input[3].Raw)
	}

	cache := newWebsocketToolOutputCache(0, 10)
	donePayload := []byte(`{"type":"response.output_item.done","item":{"type":"custom_tool_call","id":"ctc-1","call_id":"call-1","name":"exec","input":"pwd","status":"completed"}}`)
	recordResponsesWebsocketToolCallsFromPayloadWithCache(cache, "session-1", donePayload)
	recordResponsesWebsocketToolCallsFromPayloadWithCache(cache, "session-1", restored)
	cached, ok := cache.get("session-1", "call-1")
	if !ok {
		t.Fatal("reconciled custom tool call was not cached")
	}
	if gjson.GetBytes(cached, "type").String() != "custom_tool_call" ||
		gjson.GetBytes(cached, "input").String() != "pwd" {
		t.Fatalf("cached tool call is invalid: %s", cached)
	}
}

func TestRestoreResponsesWebsocketCompletionOutputIgnoresIncompleteCollectedToolCall(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"function_call","call_id":"call-1","name":"exec"}]}}`)
	collector := map[int64][]byte{
		0: []byte(`{"type":"custom_tool_call","call_id":"call-1","name":"exec"}`),
	}

	restored := restoreResponsesWebsocketCompletionOutput(payload, collector, nil)
	if string(restored) != string(payload) {
		t.Fatalf("incomplete collected tool call overwrote completion output: %s", restored)
	}
}

func TestRestoreResponsesWebsocketCompletionOutputPreservesFinalToolCallFields(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"function_call","call_id":"call-1","name":"exec","provider_extension":"keep"}]}}`)
	collector := map[int64][]byte{
		0: []byte(`{"type":"function_call","id":"fc-1","call_id":"call-1","name":"exec","arguments":"{}","status":"completed"}`),
	}

	restored := restoreResponsesWebsocketCompletionOutput(payload, collector, nil)
	item := gjson.GetBytes(restored, "response.output.0")
	if item.Get("provider_extension").String() != "keep" {
		t.Fatalf("final provider extension was lost: %s", item.Raw)
	}
	if item.Get("id").String() != "fc-1" || item.Get("arguments").String() != "{}" ||
		item.Get("status").String() != "completed" {
		t.Fatalf("collected canonical fields were not merged: %s", item.Raw)
	}
}

func TestRestoreResponsesWebsocketCompletionOutputReusesEquivalentFinalToolCall(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":"exec","arguments":"{}","status":"completed","provider_extension":"keep"}]}}`)
	collector := map[int64][]byte{
		0: []byte(`{"arguments":"{}","name":"exec","call_id":"call-1","id":"fc-1","type":"function_call","status":"completed"}`),
	}

	restored := restoreResponsesWebsocketCompletionOutput(payload, collector, nil)
	if !bytes.Equal(restored, payload) {
		t.Fatalf("equivalent final tool call was rewritten: %s", restored)
	}
}

func TestCollectResponsesWebsocketOutputItemIgnoresInvalidOutputIndexes(t *testing.T) {
	outputItemsByIndex := map[int64][]byte{
		0: []byte(`{"type":"custom_tool_call","id":"valid","call_id":"call-1","name":"exec","input":"pwd"}`),
	}
	var outputItemsFallback [][]byte
	for _, outputIndex := range []string{"null", "true", `"0"`, "0.5", "-1", "1e0"} {
		payload := []byte(`{"type":"response.output_item.done","output_index":` + outputIndex + `,"item":{"type":"function_call","id":"invalid","call_id":"call-1","name":"exec","arguments":"{}"}}`)
		collectResponsesWebsocketOutputItem(payload, outputItemsByIndex, &outputItemsFallback)
	}

	if len(outputItemsByIndex) != 1 || gjson.GetBytes(outputItemsByIndex[0], "id").String() != "valid" {
		t.Fatalf("invalid output_index changed indexed items: %v", outputItemsByIndex)
	}
	if len(outputItemsFallback) != 0 {
		t.Fatalf("invalid output_index was treated as fallback: %d", len(outputItemsFallback))
	}
}
