package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type originalWebsocketForwardResult struct {
	output   []byte
	response string
	pending  []string
	errMsg   *handlers.ErrorMessage
	forward  error
	timeline string
	payloads [][]byte
}

func forwardOriginalWebsocketChunks(
	t *testing.T, chunks [][]byte, closeBeforeForward bool,
) originalWebsocketForwardResult {
	t.Helper()
	gin.SetMode(gin.TestMode)
	results := make(chan originalWebsocketForwardResult, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				conn, err := responsesWebsocketUpgrader.Upgrade(w, request, nil)
				if err != nil {
					results <- originalWebsocketForwardResult{forward: err}
					return
				}
				defer func() { _ = conn.Close() }()
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = request
				data := make(chan core.StreamChunk, len(chunks))
				for _, payload := range chunks {
					data <- core.StreamChunk{Payload: payload}
				}
				close(data)
				errs := make(chan *handlers.ErrorMessage)
				close(errs)
				timeline := newWebsocketTimelineRecorder(nil)
				if closeBeforeForward {
					if err := conn.Close(); err != nil {
						results <- originalWebsocketForwardResult{forward: err}
						return
					}
				}
				handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
				result := originalWebsocketForwardResult{}
				result.output, result.response, result.pending, result.errMsg,
					result.forward = handler.forwardResponsesWebsocket(
					ctx,
					newResponsesWebsocketWriter(conn),
					func(...any) {},
					data,
					errs,
					timeline,
					"session-1",
					"downstream-1",
				)
				result.timeline = timeline.String()
				results <- result
			},
		),
	)
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var payloads [][]byte
	for {
		kind, payload, err := client.ReadMessage()
		if err != nil {
			break
		}
		if kind == websocket.TextMessage {
			payloads = append(payloads, payload)
		}
	}
	select {
	case result := <-results:
		result.payloads = payloads
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("websocket forward did not finish")
		return originalWebsocketForwardResult{}
	}
}

func TestResponseCompletedOutputFromPayload(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1",` +
		`"output":[{"type":"message","id":"out-1"}]}}`)
	items := gjson.ParseBytes(responseCompletedOutputFromPayload(payload, nil, nil)).Array()
	if len(items) != 1 || items[0].Get("id").String() != "out-1" {
		t.Fatalf("completion output = %s, want out-1", payload)
	}
}

func TestRestoreResponsesWebsocketCompletionOutputPreservesNonEmptyOutput(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1",` +
		`"output":[{"type":"message","id":"out-1"}]}}`)
	collector := map[int64][]byte{0: []byte(`{"type":"function_call","id":"call-1","call_id":"call-1"}`)}
	if restored := restoreResponsesWebsocketCompletionOutput(
		payload, collector, nil,
	); string(restored) != string(payload) {
		t.Fatalf("non-empty completion was overwritten: %s", restored)
	}
}

func TestForwardResponsesWebsocketRestoresAndForwardsCompletedOutput(t *testing.T) {
	result := forwardOriginalWebsocketChunks(
		t, [][]byte{
			[]byte(`{"type":"response.output_item.done","output_index":0,` +
				`"item":{"type":"function_call","id":"call-1","call_id":"call-1","name":"lookup","arguments":"{}"}}`),
			[]byte("data: " + `{"type":"response.completed","response":{"id":"resp-1","output":[]}}` + "\n\n"),
		}, false,
	)
	if result.forward != nil || result.errMsg != nil || result.response != "resp-1" ||
		gjson.GetBytes(result.output, "0.id").String() != "call-1" ||
		len(result.pending) != 1 || result.pending[0] != "call-1" {
		t.Fatalf("completed output result = %+v", result)
	}
	if !strings.Contains(result.timeline, "websocket.response\n") {
		t.Fatalf("timeline did not capture the downstream response: %s", result.timeline)
	}
	if len(result.payloads) != 2 ||
		gjson.GetBytes(result.payloads[0], "type").String() != "response.output_item.done" {
		t.Fatalf("forwarded frames = %q", result.payloads)
	}
	completion := result.payloads[1]
	if gjson.GetBytes(completion, "type").String() != wsEventTypeCompleted ||
		strings.Contains(string(completion), "response.done") ||
		gjson.GetBytes(completion, "response.output.0.id").String() != "call-1" {
		t.Fatalf("completion type or restored output changed: %s", completion)
	}
}

func TestForwardResponsesWebsocketTreatsResponseDoneAsTerminalWithoutRewriting(t *testing.T) {
	payload := []byte(`{"type":"response.done","response":{"id":"resp-1",` +
		`"output":[{"type":"message","id":"out-1"}]}}`)
	result := forwardOriginalWebsocketChunks(t, [][]byte{payload}, false)
	if result.forward != nil || result.errMsg != nil || result.response != "resp-1" ||
		gjson.GetBytes(result.output, "0.id").String() != "out-1" || len(result.pending) != 0 {
		t.Fatalf("response.done result = %+v", result)
	}
	if len(result.payloads) != 1 || gjson.GetBytes(result.payloads[0], "type").String() != "response.done" {
		t.Fatalf("response.done frame changed: %q", result.payloads)
	}
}

func TestForwardResponsesWebsocketTreatsErrorPayloadAsTerminal(t *testing.T) {
	payload := []byte(`{"type":"error","status":429,"error":{"message":"upstream failed"}}`)
	result := forwardOriginalWebsocketChunks(t, [][]byte{payload}, false)
	if !errors.Is(result.forward, websocket.ErrCloseSent) || result.errMsg == nil ||
		result.errMsg.StatusCode != http.StatusTooManyRequests || result.errMsg.Error == nil ||
		!strings.Contains(result.errMsg.Error.Error(), "upstream failed") {
		t.Fatalf("terminal error result = %+v", result)
	}
	if len(result.payloads) != 1 || gjson.GetBytes(result.payloads[0], "type").String() != wsEventTypeError {
		t.Fatalf("terminal error frame changed: %q", result.payloads)
	}
}

func TestForwardResponsesWebsocketLogsAttemptedResponseOnWriteFailure(t *testing.T) {
	payload := []byte("data: " + `{"type":"response.completed","response":{"id":"resp-1",` +
		`"output":[{"type":"message","id":"out-1"}]}}` + "\n\n")
	result := forwardOriginalWebsocketChunks(t, [][]byte{payload}, true)
	if result.forward == nil {
		t.Fatal("expected websocket write failure")
	}
	if !strings.Contains(result.timeline, "websocket.response\n") ||
		!strings.Contains(result.timeline, `"type":"response.completed"`) {
		t.Fatalf("timeline lost the attempted downstream response: %s", result.timeline)
	}
}

func TestResponsesWebsocketTimelineRecordsDisconnectEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	handler := NewResponsesAPIHandler(
		handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{RequestLog: true}, manager),
	)
	logsDir := t.TempDir()
	result := make(chan originalWebsocketForwardResult, 1)
	router := gin.New()
	router.GET(
		"/v1/responses/ws", func(ctx *gin.Context) {
			source, err := internallogging.NewFileBodySourceInDir(logsDir, "websocket-timeline-test")
			if err != nil {
				result <- originalWebsocketForwardResult{forward: err}
				return
			}
			defer func() {
				if err := source.Cleanup(); err != nil {
					t.Errorf("clean up timeline source: %v", err)
				}
			}()
			ctx.Set(internallogging.WebsocketTimelineSourceContextKey, source)
			handler.ResponsesWebsocket(ctx)
			body, err := source.Bytes()
			result <- originalWebsocketForwardResult{timeline: string(body), forward: err}
		},
	)
	server := httptest.NewServer(router)
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	if err := client.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseGoingAway, "client closing"), time.Now().Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case actual := <-result:
		if actual.forward != nil || !strings.Contains(actual.timeline, "websocket.disconnect\n") {
			t.Fatalf("disconnect timeline = %q, error = %v", actual.timeline, actual.forward)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for websocket timeline")
	}
}

func TestResponsesWebsocketOutputCollectorRestoresCompletedOutput(t *testing.T) {
	indexed := make(map[int64][]byte)
	var fallback [][]byte
	for _, payload := range [][]byte{
		[]byte(`{"type":"response.output_item.done","output_index":1,` +
			`"item":{"type":"message","id":"reply-1","role":"assistant"}}`),
		[]byte(`{"type":"response.output_item.done","output_index":0,` +
			`"item":{"type":"reasoning","id":"summary-1","summary":[]}}`),
		[]byte(`{"type":"response.output_item.done",` +
			`"item":{"type":"function_call","id":"call-1","call_id":"call-1","name":"exec","arguments":"{}"}}`),
	} {
		collectResponsesWebsocketOutputItem(payload, indexed, &fallback)
	}
	output := responseCompletedOutputFromPayload(
		[]byte(`{"type":"response.completed","response":{"id":"resp-1","output":[]}}`), indexed, fallback,
	)
	items := gjson.ParseBytes(output).Array()
	if len(items) != 3 {
		t.Fatalf("collected output length = %d, want 3: %s", len(items), output)
	}
	for index, want := range []string{"summary-1", "reply-1", "call-1"} {
		if got := items[index].Get("id").String(); got != want {
			t.Fatalf("output[%d].id = %q, want %q", index, got, want)
		}
	}
}
