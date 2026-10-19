package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const responsesWebsocketLargeTranscriptSize = 1 << 20

var responsesWebsocketMergedInputSink []byte

func TestMergeResponsesWebsocketInputCompatibility(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		lastRequest        string
		lastResponseOutput string
		appendInput        string
		want               string
	}{
		{
			name:               "paired tool call",
			lastRequest:        `{"input":[{"type":"message","id":"msg-1"}]}`,
			lastResponseOutput: `[{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":"{}"}]`,
			appendInput:        `[{"type":"function_call_output","id":"out-1","call_id":"call-1","output":"done"}]`,
			want:               `[{"type":"message","id":"msg-1"},{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":"{}"},{"type":"function_call_output","id":"out-1","call_id":"call-1","output":"done"}]`,
		},
		{
			name:               "duplicate call keeps first",
			lastRequest:        `{"input":[{"type":"function_call","id":"first","call_id":"call-1"}]}`,
			lastResponseOutput: `[{"type":"function_call","id":"second","call_id":"call-1"}]`,
			appendInput:        `[]`,
			want:               `[{"type":"function_call","id":"first","call_id":"call-1"}]`,
		},
		{
			name:               "case insensitive metadata",
			lastRequest:        `{"input":[{"Type":"function_call","ID":"fc-old","CALL_ID":"call-1"}]}`,
			lastResponseOutput: `[{"type":"function_call","id":"fc-new","call_id":"call-1"}]`,
			appendInput:        `[{"type":"function_call_output","id":"out-1","call_id":"call-1","output":"done"}]`,
			want:               `[{"Type":"function_call","ID":"fc-old","CALL_ID":"call-1"},{"type":"function_call_output","id":"out-1","call_id":"call-1","output":"done"}]`,
		},
		{
			name:               "duplicate input keeps last array",
			lastRequest:        `{"Input":[{"id":"old"}],"INPUT":[{"id":"new"}]}`,
			lastResponseOutput: `[]`,
			appendInput:        `[]`,
			want:               `[{"id":"new"}]`,
		},
		{
			name:               "referenced duplicate id wins",
			lastRequest:        `{"input":[{"type":"function_call","id":"fc-1","call_id":"kept"}]}`,
			lastResponseOutput: `[{"type":"function_call","id":"fc-1","call_id":"other"}]`,
			appendInput:        `[{"type":"function_call_output","call_id":"kept","output":"done"}]`,
			want:               `[{"type":"function_call","id":"fc-1","call_id":"kept"},{"type":"function_call_output","call_id":"kept","output":"done"}]`,
		},
		{
			name:               "consumed compaction trigger removed",
			lastRequest:        `{"input":[{"type":"message","id":"old"},{"type":"compaction_trigger"}]}`,
			lastResponseOutput: `[{"type":"compaction","id":"cmp-1","encrypted_content":"opaque"}]`,
			appendInput:        `[{"type":"message","id":"next"}]`,
			want:               `[{"type":"message","id":"old"},{"type":"compaction","id":"cmp-1","encrypted_content":"opaque"},{"type":"message","id":"next"}]`,
		},
		{
			name:               "raw numbers remain exact",
			lastRequest:        `{"input":[9007199254740993]}`,
			lastResponseOutput: `[18446744073709551615]`,
			appendInput:        `[{"value":1.0000000000000000001}]`,
			want:               `[9007199254740993,18446744073709551615,{"value":1.0000000000000000001}]`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, errMerge := mergeResponsesWebsocketInput(
				[]byte(test.lastRequest), []byte(test.lastResponseOutput), test.appendInput,
			)
			if errMerge != nil {
				t.Fatalf("mergeResponsesWebsocketInput() error = %v", errMerge)
			}
			assertResponsesWebsocketJSONEqual(t, got, test.want)
		})
	}
}

func TestMergeResponsesWebsocketInputErrors(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		lastRequest string
		appendInput string
		wantPrefix  string
	}{
		{name: "invalid previous request", lastRequest: `{"input":`, appendInput: `[]`, wantPrefix: "invalid previous request input"},
		{name: "non-array previous input", lastRequest: `{"input":{}}`, appendInput: `[]`, wantPrefix: "invalid previous request input"},
		{name: "earlier invalid duplicate", lastRequest: `{"input":{},"input":[]}`, appendInput: `[]`, wantPrefix: "invalid previous request input"},
		{name: "invalid appended input", lastRequest: `{"input":[]}`, appendInput: `[{`, wantPrefix: "invalid request input"},
		{name: "non-array appended input", lastRequest: `{"input":[]}`, appendInput: `{}`, wantPrefix: "invalid request input"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, errMerge := mergeResponsesWebsocketInput([]byte(test.lastRequest), nil, test.appendInput)
			if errMerge == nil || !strings.HasPrefix(errMerge.Error(), test.wantPrefix+": ") {
				t.Fatalf("merge error = %v, want prefix %q", errMerge, test.wantPrefix)
			}
		})
	}
}

func TestMergeResponsesWebsocketInputDetachesSourceBuffers(t *testing.T) {
	t.Parallel()

	lastRequest := []byte(`{"input":[{"type":"message","id":"old","content":"history"}]}`)
	lastOutput := []byte(`[{"type":"message","id":"answer","content":"response"}]`)
	appendInput := `[{"type":"message","id":"next","content":"continue"}]`
	merged, errMerge := mergeResponsesWebsocketInput(lastRequest, lastOutput, appendInput)
	if errMerge != nil {
		t.Fatal(errMerge)
	}
	want := bytes.Clone(merged)
	for _, source := range [][]byte{lastRequest, lastOutput} {
		for index := range source {
			source[index] = 'x'
		}
	}
	runtime.KeepAlive(lastRequest)
	runtime.KeepAlive(lastOutput)
	if !bytes.Equal(merged, want) {
		t.Fatal("merged input aliases a source buffer")
	}
}

func TestMergeResponsesWebsocketInputBoundsLargeTranscriptAllocations(t *testing.T) {
	lastRequest := []byte(`{"model":"test","input":[{"type":"message","id":"large","content":"` +
		strings.Repeat("x", responsesWebsocketLargeTranscriptSize) + `"}]}`)
	lastOutput := []byte(`[{"type":"function_call","id":"call","call_id":"call-1","name":"lookup","arguments":"{}"}]`)
	appendInput := `[{"type":"function_call_output","id":"output","call_id":"call-1","output":"done"}]`
	inputBytes := len(lastRequest) + len(lastOutput) + len(appendInput)

	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			merged, errMerge := mergeResponsesWebsocketInput(lastRequest, lastOutput, appendInput)
			if errMerge != nil {
				b.Fatal(errMerge)
			}
			responsesWebsocketMergedInputSink = merged
		}
	})
	responsesWebsocketMergedInputSink = nil

	maxAllocatedBytes := int64(inputBytes * 2)
	t.Logf("merge allocated %d bytes per operation for %d input bytes", result.AllocedBytesPerOp(), inputBytes)
	if result.AllocedBytesPerOp() > maxAllocatedBytes {
		t.Fatalf("merge allocated %d bytes, want at most %d", result.AllocedBytesPerOp(), maxAllocatedBytes)
	}
}

func TestPrepareResponsesWebsocketFallbackTurnBoundsAllocations(t *testing.T) {
	content := strings.Repeat("x", 1024)
	var input strings.Builder
	input.WriteByte('[')
	for index := range 128 {
		if index > 0 {
			input.WriteByte(',')
		}
		fmt.Fprintf(&input, `{"type":"message","id":"msg-%d","content":%q}`, index, content)
	}
	input.WriteByte(']')
	payload := []byte(`{"model":"test","input":` + input.String() + `}`)

	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			repaired, turn := prepareResponsesWebsocketFallbackTurn("allocation-session", payload)
			runtime.KeepAlive(repaired)
			runtime.KeepAlive(turn)
		}
	})

	maxAllocatedBytes := int64(len(payload) * 8)
	t.Logf("repair allocated %d bytes per operation for %d input bytes", result.AllocedBytesPerOp(), len(payload))
	if result.AllocedBytesPerOp() > maxAllocatedBytes {
		t.Fatalf("repair allocated %d bytes, want at most %d", result.AllocedBytesPerOp(), maxAllocatedBytes)
	}
}

func TestRepairResponsesWebsocketToolCallsPreservesNamedFunctionOutput(t *testing.T) {
	t.Parallel()

	cache := newWebsocketToolOutputCache(time.Minute, 10)
	payload := []byte(`{"input":[{"type":"function_call_output","name":"automation_update","output":"heartbeat"},{"type":"custom_tool_call_output","name":"custom","output":"drop"},{"type":"function_call_output","output":"drop"}]}`)
	repaired := repairResponsesWebsocketToolCallsWithCache(cache, "named-output-session", payload)
	items := gjson.GetBytes(repaired, "input").Array()
	if len(items) != 1 || items[0].Get("name").String() != "automation_update" {
		t.Fatalf("named function output was not preserved: %s", repaired)
	}
}

func TestNormalizeResponsesWebsocketPrewarmFollowupPreservesWarmInput(t *testing.T) {
	t.Parallel()

	warmup := []byte(`{"model":"test","instructions":"base","input":[{"type":"additional_tools","id":"tools","role":"developer","tools":[]},{"type":"message","id":"base","role":"developer","content":"base"}]}`)
	followup := []byte(`{"type":"response.create","previous_response_id":"resp_prewarm_test","input":[{"type":"compaction","encrypted_content":"opaque"},{"type":"function_call_output","name":"automation_update","output":"heartbeat"}],"client_metadata":{"keep":"yes"}}`)
	normalized, next, errMsg := normalizeResponsesWebsocketPrewarmFollowup(followup, warmup)
	if errMsg != nil {
		t.Fatalf("normalize prewarm followup: %v", errMsg.Error)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 4 || input[0].Get("id").String() != "tools" || input[1].Get("id").String() != "base" {
		t.Fatalf("warm input lost or duplicated: %s", normalized)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() ||
		gjson.GetBytes(normalized, "client_metadata.keep").String() != "yes" {
		t.Fatalf("prewarm followup metadata changed: %s", normalized)
	}
	if !bytes.Equal(normalized, next) {
		t.Fatal("prewarm next state differs from normalized request")
	}
}

func TestSyntheticResponsesWebsocketPrewarmPayloads(t *testing.T) {
	t.Parallel()

	payloads, errPayloads := syntheticResponsesWebsocketPrewarmPayloads([]byte(`{"model":"test"}`))
	if errPayloads != nil {
		t.Fatal(errPayloads)
	}
	if len(payloads) != 2 || gjson.GetBytes(payloads[0], "type").String() != "response.created" ||
		gjson.GetBytes(payloads[1], "type").String() != wsEventTypeDone {
		t.Fatalf("unexpected synthetic prewarm payloads: %q", payloads)
	}
	firstID := gjson.GetBytes(payloads[0], "response.id").String()
	if !strings.HasPrefix(firstID, "resp_prewarm_") ||
		gjson.GetBytes(payloads[1], "response.id").String() != firstID {
		t.Fatalf("synthetic prewarm response IDs differ: %q", payloads)
	}
}

func TestIsCodexResponsesLiteRequest(t *testing.T) {
	t.Parallel()

	headers := make(http.Header)
	headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", " TRUE ")
	if !isCodexResponsesLiteRequest(nil, headers) {
		t.Fatal("native header was not recognized")
	}
	body := []byte(`{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"TrUe"}}`)
	if !isCodexResponsesLiteRequest(body, nil) {
		t.Fatal("websocket metadata mirror was not recognized")
	}
	if isCodexResponsesLiteRequest([]byte(`{"client_metadata":{}}`), nil) {
		t.Fatal("ordinary request was classified as Responses Lite")
	}
}

func TestResponsesWebsocketHandlesPrewarmLocally(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	router := gin.New()
	router.GET("/v1/responses/ws", handler.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()

	client, _, errDial := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws", nil,
	)
	if errDial != nil {
		t.Fatal(errDial)
	}
	defer func() { _ = client.Close() }()
	if errDeadline := client.SetReadDeadline(time.Now().Add(2 * time.Second)); errDeadline != nil {
		t.Fatal(errDeadline)
	}

	prewarm := []byte(`{"type":"response.create","model":"test","generate":false,"input":[{"type":"message","id":"base","role":"developer","content":"base"}]}`)
	if errWrite := client.WriteMessage(websocket.TextMessage, prewarm); errWrite != nil {
		t.Fatal(errWrite)
	}
	_, created, errRead := client.ReadMessage()
	if errRead != nil {
		t.Fatal(errRead)
	}
	_, done, errRead := client.ReadMessage()
	if errRead != nil {
		t.Fatal(errRead)
	}
	prewarmID := gjson.GetBytes(created, "response.id").String()
	if !strings.HasPrefix(prewarmID, "resp_prewarm_") ||
		gjson.GetBytes(done, "response.id").String() != prewarmID ||
		gjson.GetBytes(done, "type").String() != wsEventTypeDone {
		t.Fatalf("unexpected prewarm events: created=%s done=%s", created, done)
	}

	wrongParent := []byte(`{"type":"response.create","previous_response_id":"resp_prewarm_wrong","input":[]}`)
	if errWrite := client.WriteMessage(websocket.TextMessage, wrongParent); errWrite != nil {
		t.Fatal(errWrite)
	}
	_, errorPayload, errRead := client.ReadMessage()
	if errRead != nil {
		t.Fatal(errRead)
	}
	if gjson.GetBytes(errorPayload, "type").String() != wsEventTypeError ||
		gjson.GetBytes(errorPayload, "error.code").String() != "previous_response_not_found" {
		t.Fatalf("wrong prewarm parent was accepted: %s", errorPayload)
	}

	secondPrewarm := []byte(`{"type":"response.create","model":"test","generate":false,"input":[{"type":"message","id":"new-base","role":"developer","content":"new"}]}`)
	if errWrite := client.WriteMessage(websocket.TextMessage, secondPrewarm); errWrite != nil {
		t.Fatal(errWrite)
	}
	_, created, errRead = client.ReadMessage()
	if errRead != nil {
		t.Fatal(errRead)
	}
	_, done, errRead = client.ReadMessage()
	if errRead != nil {
		t.Fatal(errRead)
	}
	secondID := gjson.GetBytes(created, "response.id").String()
	if !strings.HasPrefix(secondID, "resp_prewarm_") || secondID == prewarmID ||
		gjson.GetBytes(done, "response.id").String() != secondID {
		t.Fatalf("mid-session prewarm did not create a new transcript root: created=%s done=%s", created, done)
	}
	oldParent := []byte(`{"type":"response.create","previous_response_id":"` + prewarmID + `","input":[]}`)
	if errWrite := client.WriteMessage(websocket.TextMessage, oldParent); errWrite != nil {
		t.Fatal(errWrite)
	}
	_, errorPayload, errRead = client.ReadMessage()
	if errRead != nil {
		t.Fatal(errRead)
	}
	if gjson.GetBytes(errorPayload, "error.code").String() != "previous_response_not_found" {
		t.Fatalf("old prewarm parent was accepted after reset: %s", errorPayload)
	}
}

func TestForwardResponsesWebsocketEmitsPingAndPreservesNativeCompletion(t *testing.T) {
	gin.SetMode(gin.TestMode)

	resultCh := make(chan error, 1)
	data := make(chan cliproxyexecutor.StreamChunk, 2)
	errs := make(chan *handlers.ErrorMessage)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		conn, errUpgrade := responsesWebsocketUpgrader.Upgrade(w, request, nil)
		if errUpgrade != nil {
			resultCh <- errUpgrade
			return
		}
		defer func() { _ = conn.Close() }()
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = request
		interval := 20 * time.Millisecond
		handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
		_, _, _, errMsg, errForward := handler.forwardResponsesWebsocket(
			ginCtx,
			newResponsesWebsocketWriter(conn),
			func(...any) {},
			data,
			errs,
			nil,
			"ping-session",
			"ping-downstream",
			responsesWebsocketForwardOptions{
				preserveCompletionOutput: func() bool { return true },
				keepAliveInterval:        &interval,
			},
		)
		if errMsg != nil {
			resultCh <- errMsg.Error
			return
		}
		resultCh <- errForward
	}))
	defer server.Close()

	client, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if errDial != nil {
		t.Fatal(errDial)
	}
	defer func() { _ = client.Close() }()
	pingReceived := make(chan struct{}, 1)
	client.SetPingHandler(func(appData string) error {
		select {
		case pingReceived <- struct{}{}:
		default:
		}
		return client.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})
	payloadCh := make(chan []byte, 1)
	go func() {
		for {
			_, payload, errRead := client.ReadMessage()
			if errRead != nil {
				return
			}
			payloadCh <- payload
		}
	}()

	select {
	case <-pingReceived:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("websocket Ping control frame was not emitted")
	}
	data <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"streamed"}}`)}
	data <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"resp-native","output":[]}}`), Terminal: true}
	close(data)
	close(errs)

	var completion []byte
	for completion == nil {
		select {
		case payload := <-payloadCh:
			if isResponsesWebsocketCompletionEvent(gjson.GetBytes(payload, "type").String()) {
				completion = payload
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for native completion")
		}
	}
	if gjson.GetBytes(completion, "response.output").Raw != "[]" {
		t.Fatalf("native completion output was rewritten: %s", completion)
	}

	select {
	case errServer := <-resultCh:
		if errServer != nil {
			t.Fatalf("forward result: %v", errServer)
		}
	case <-time.After(time.Second):
		t.Fatal("forward did not finish")
	}
}

func assertResponsesWebsocketJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var gotValue any
	gotDecoder := json.NewDecoder(bytes.NewReader(got))
	gotDecoder.UseNumber()
	if errDecode := gotDecoder.Decode(&gotValue); errDecode != nil {
		t.Fatalf("invalid actual JSON: %v\n%s", errDecode, got)
	}
	var wantValue any
	wantDecoder := json.NewDecoder(strings.NewReader(want))
	wantDecoder.UseNumber()
	if errDecode := wantDecoder.Decode(&wantValue); errDecode != nil {
		t.Fatalf("invalid expected JSON: %v\n%s", errDecode, want)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON differs:\n got: %s\nwant: %s", got, want)
	}
}

type prewarmCaptureExecutor struct {
	compactCaptureExecutor
	mu       sync.Mutex
	payloads [][]byte
}

func (e *prewarmCaptureExecutor) ExecuteStream(
	_ context.Context,
	_ *coreauth.Auth,
	req cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.payloads = append(e.payloads, bytes.Clone(req.Payload))
	call := len(e.payloads)
	e.mu.Unlock()
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(fmt.Sprintf(
		`{"type":"response.completed","response":{"id":"generated-%d","status":"completed","output":[{"type":"message","id":"output-%d"}]}}`, call, call,
	))}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func TestResponsesWebsocketIncrementalPrewarmKeepsHistoryWithoutExecution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, warmups := range []int{1, 2} {
		for _, emptyFollowup := range []bool{false, true} {
			t.Run(fmt.Sprintf("warmups_%d_empty_%t", warmups, emptyFollowup), func(t *testing.T) {
				executor := &prewarmCaptureExecutor{}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.RegisterExecutor(executor)
				const model = "incremental-prewarm-model"
				auth := &coreauth.Auth{ID: t.Name(), Provider: executor.Identifier(), Status: coreauth.StatusActive}
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
				router := gin.New()
				router.GET("/v1/responses", handler.ResponsesWebsocket)
				server := httptest.NewServer(router)
				defer server.Close()
				client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = client.Close() }()
				if err = client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				send := func(body string) {
					t.Helper()
					if errWrite := client.WriteMessage(websocket.TextMessage, []byte(body)); errWrite != nil {
						t.Fatal(errWrite)
					}
				}
				read := func(event string) []byte {
					t.Helper()
					_, body, errRead := client.ReadMessage()
					if errRead != nil {
						t.Fatal(errRead)
					}
					if gjson.GetBytes(body, "type").String() != event {
						t.Fatalf("event = %s, want %s", body, event)
					}
					return body
				}
				send(`{"type":"response.create","model":"` + model + `","input":[{"type":"message","id":"initial-user"}]}`)
				responseID := gjson.GetBytes(read(wsEventTypeDone), "response.id").String()
				wantIDs := []string{"initial-user", "output-1"}
				for index := 1; index <= warmups; index++ {
					id := fmt.Sprintf("warmup-%d", index)
					send(fmt.Sprintf(`{"type":"response.create","previous_response_id":%q,"generate":false,"input":[{"type":"message","id":%q}]}`, responseID, id))
					created := read("response.created")
					responseID = gjson.GetBytes(created, "response.id").String()
					done := read(wsEventTypeDone)
					if !strings.HasPrefix(responseID, "resp_prewarm_") || gjson.GetBytes(done, "response.id").String() != responseID ||
						len(gjson.GetBytes(done, "response.output").Array()) != 0 || gjson.GetBytes(done, "response.usage.total_tokens").Int() != 0 {
						t.Fatalf("prewarm events = %s %s", created, done)
					}
					executor.mu.Lock()
					calls := len(executor.payloads)
					executor.mu.Unlock()
					if calls != 1 {
						t.Fatalf("upstream calls after prewarm = %d, want 1", calls)
					}
					wantIDs = append(wantIDs, id)
				}
				input := `[]`
				if !emptyFollowup {
					input = `[{"type":"message","id":"followup"}]`
					wantIDs = append(wantIDs, "followup")
				}
				send(fmt.Sprintf(`{"type":"response.create","previous_response_id":%q,"input":%s}`, responseID, input))
				read(wsEventTypeDone)
				executor.mu.Lock()
				payloads := append([][]byte(nil), executor.payloads...)
				executor.mu.Unlock()
				if len(payloads) != 2 {
					t.Fatalf("upstream calls = %d, want 2", len(payloads))
				}
				forwarded := payloads[1]
				if gjson.GetBytes(forwarded, "previous_response_id").Exists() || gjson.GetBytes(forwarded, "generate").Exists() ||
					gjson.GetBytes(forwarded, "model").String() != model {
					t.Fatalf("forwarded metadata = %s", forwarded)
				}
				items := gjson.GetBytes(forwarded, "input").Array()
				if len(items) != len(wantIDs) {
					t.Fatalf("forwarded history = %s, want IDs %v", forwarded, wantIDs)
				}
				for index, id := range wantIDs {
					if items[index].Get("id").String() != id {
						t.Fatalf("history[%d] = %s, want %s", index, items[index].Raw, id)
					}
				}
			})
		}
	}
}
