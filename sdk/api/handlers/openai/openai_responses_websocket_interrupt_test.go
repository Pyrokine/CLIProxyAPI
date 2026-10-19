package openai

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestResponsesLocalInterruptRejectsDifferentPendingResponse(t *testing.T) {
	local := newResponsesLocalInterrupt()
	first := []byte(`{"type":"response.interrupt","response_id":"r1"}`)
	local.begin()
	if err := local.deliver(first); err == nil {
		t.Fatal("interrupt was accepted before the response was created")
	}
	local.setResponseID("r1")
	if err := local.deliver([]byte(`{"type":"response.interrupt","response_id":"r2"}`)); err == nil {
		t.Fatal("interrupt for another response was accepted")
	}
	if err := local.deliver(first); err != nil {
		t.Fatalf("first interrupt: %v", err)
	}
	if err := local.deliver(first); err != nil {
		t.Fatalf("duplicate interrupt: %v", err)
	}
	if err := local.deliver([]byte(`{"type":"response.interrupt","response_id":"r2"}`)); err == nil {
		t.Fatal("different pending response was accepted")
	}
	if payload := <-local.framesChan(); !bytes.Equal(payload, first) {
		t.Fatalf("queued interrupt changed: %s", payload)
	}
	local.end()
	if err := local.deliver(first); !errors.Is(err, cliproxyexecutor.ErrNoActiveUpstreamWebsocket) {
		t.Fatalf("interrupt after turn ended = %v", err)
	}
}

func TestResponsesWebsocketInterruptCancelsHTTPTurnAndKeepsSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type outcome struct {
		output     []byte
		responseID string
		cancelled  bool
		next       []byte
		err        error
	}
	result := make(chan outcome, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		conn, err := responsesWebsocketUpgrader.Upgrade(w, request, nil)
		if err != nil {
			result <- outcome{err: err}
			return
		}
		defer func() { _ = conn.Close() }()
		socketCtx, cancelSocket := context.WithCancelCause(request.Context())
		defer cancelSocket(nil)
		writer := newResponsesWebsocketWriter(conn)
		local := newResponsesLocalInterrupt()
		handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
		input, readerDone := readResponsesWebsocketInput(socketCtx, cancelSocket, conn, func(payload []byte) error {
			return handler.forwardResponsesWebsocketInterrupt(socketCtx, "offline-session", payload)
		}, local, writer, nil)
		defer func() {
			cancelSocket(nil)
			_ = conn.Close()
			<-readerDone
		}()
		ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ginCtx.Request = request.WithContext(socketCtx)
		data := make(chan cliproxyexecutor.StreamChunk, 2)
		data <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\"}}\n\n")}
		data <- cliproxyexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg1\",\"content\":[]}}\n\n")}
		turnCancelled := false
		output, responseID, _, errMsg, errForward := handler.forwardResponsesWebsocket(
			ginCtx, writer, func(...any) { turnCancelled = true }, data, nil,
			nil, "offline-session", "offline-client",
			responsesWebsocketForwardOptions{localInterrupt: local},
		)
		if errMsg != nil || errForward != nil {
			result <- outcome{err: errors.Join(errForward, func() error {
				if errMsg != nil {
					return errMsg.Error
				}
				return nil
			}())}
			return
		}
		select {
		case next, ok := <-input:
			if !ok {
				result <- outcome{err: context.Cause(socketCtx)}
				return
			}
			result <- outcome{output: output, responseID: responseID, cancelled: turnCancelled, next: next}
		case <-time.After(3 * time.Second):
			result <- outcome{err: errors.New("next request blocked after interrupt")}
		}
	}))
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial downstream websocket: %v", err)
	}
	defer func() { _ = client.Close() }()
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	for _, event := range []string{"response.created", "response.output_item.done"} {
		_, payload, errRead := client.ReadMessage()
		if errRead != nil || gjson.GetBytes(payload, "type").String() != event {
			t.Fatalf("expected %s event, got %s, error %v", event, payload, errRead)
		}
	}
	if errWrite := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.interrupt","response_id":"r-old"}`)); errWrite != nil {
		t.Fatalf("send unrelated interrupt: %v", errWrite)
	}
	_, mismatch, errRead := client.ReadMessage()
	if errRead != nil || gjson.GetBytes(mismatch, "type").String() != "error" ||
		!bytes.Contains(mismatch, []byte("does not match the active response")) {
		t.Fatalf("unrelated interrupt error = %s, read error %v", mismatch, errRead)
	}
	interrupt := []byte(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items"}`)
	if errWrite := client.WriteMessage(websocket.TextMessage, interrupt); errWrite != nil {
		t.Fatalf("send interrupt: %v", errWrite)
	}
	_, incomplete, errRead := client.ReadMessage()
	if errRead != nil || gjson.GetBytes(incomplete, "type").String() != "response.incomplete" ||
		gjson.GetBytes(incomplete, "response.id").String() != "r1" ||
		gjson.GetBytes(incomplete, "response.incomplete_details.reason").String() != "interrupted" ||
		gjson.GetBytes(incomplete, "response.output.0.id").String() != "msg1" {
		t.Fatalf("incomplete response = %s, error %v", incomplete, errRead)
	}
	if errWrite := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.interrupt"}`)); errWrite != nil {
		t.Fatalf("send invalid interrupt: %v", errWrite)
	}
	_, errorPayload, errRead := client.ReadMessage()
	if errRead != nil || gjson.GetBytes(errorPayload, "type").String() != "error" ||
		!bytes.Contains(errorPayload, []byte("requires response_id")) {
		t.Fatalf("invalid interrupt error = %s, read error %v", errorPayload, errRead)
	}
	next := []byte(`{"type":"response.create","model":"gpt-5-codex","input":[]}`)
	if errWrite := client.WriteMessage(websocket.TextMessage, next); errWrite != nil {
		t.Fatalf("send next request: %v", errWrite)
	}
	select {
	case got := <-result:
		if got.err != nil || !got.cancelled || got.responseID != "r1" || !bytes.Equal(got.next, next) ||
			gjson.GetBytes(got.output, "0.id").String() != "msg1" {
			t.Fatalf("forward outcome = %+v", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interrupted HTTP turn did not finish")
	}
}

func TestResponsesWebsocketInterruptDiagnosticsAndReaderCompletion(t *testing.T) {
	payload := []byte(`{"type":"response.interrupt","response_id":"private-response-token","mode":"private-mode","token":"synthetic-private-token","url":"https://synthetic.invalid/private","extension":{"secret":"private-extension"}}`)
	next := []byte(`{"type":"response.create","model":"offline-model","input":[]}`)
	idHash := sha256.Sum256([]byte("private-response-token"))
	wantHash := fmt.Sprintf("%x", idHash[:8])
	for _, test := range []struct {
		name    string
		err     error
		active  bool
		outcome string
	}{
		{name: "upstream", outcome: "handled"},
		{name: "local HTTP", err: cliproxyexecutor.ErrNoActiveUpstreamWebsocket, active: true, outcome: "local_http"},
		{name: "no active response", err: cliproxyexecutor.ErrNoActiveUpstreamWebsocket, outcome: "rejected"},
		{name: "dependency failure", err: errors.New("synthetic-private-token https://synthetic.invalid/private"), outcome: "rejected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			type result struct {
				timeline string
				err      error
			}
			results := make(chan result, 1)
			received := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				conn, err := responsesWebsocketUpgrader.Upgrade(w, request, nil)
				if err != nil {
					results <- result{err: err}
					return
				}
				ctx, cancel := context.WithCancelCause(request.Context())
				local := newResponsesLocalInterrupt()
				if test.active {
					local.begin()
					local.setResponseID("private-response-token")
				}
				timeline := newWebsocketTimelineRecorder(nil)
				input, done := readResponsesWebsocketInput(ctx, cancel, conn, func(frame []byte) error {
					received <- bytes.Clone(frame)
					return test.err
				}, local, newResponsesWebsocketWriter(conn), timeline)
				select {
				case frame := <-input:
					if !bytes.Equal(frame, next) {
						err = fmt.Errorf("unexpected queued frame: %s", frame)
					}
				case <-time.After(3 * time.Second):
					err = errors.New("next request blocked")
				}
				cancel(nil)
				_ = conn.Close()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					err = errors.Join(err, errors.New("reader did not finish"))
				}
				results <- result{timeline: timeline.String(), err: err}
			}))
			defer server.Close()
			client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatalf("dial downstream: %v", err)
			}
			defer func() { _ = client.Close() }()
			_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
			if err = client.WriteMessage(websocket.TextMessage, payload); err != nil {
				t.Fatalf("send interrupt: %v", err)
			}
			if test.outcome == "rejected" {
				_, frame, errRead := client.ReadMessage()
				if errRead != nil || gjson.GetBytes(frame, "type").String() != "error" {
					t.Fatalf("rejected interrupt = %s, %v", frame, errRead)
				}
			}
			if err = client.WriteMessage(websocket.TextMessage, next); err != nil {
				t.Fatalf("send next request: %v", err)
			}
			select {
			case frame := <-received:
				if !bytes.Equal(frame, payload) {
					t.Fatalf("interrupt payload changed: %s", frame)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("interrupt did not reach the callback")
			}
			select {
			case got := <-results:
				if got.err != nil {
					t.Fatal(got.err)
				}
				for _, secret := range []string{"private-response-token", "private-mode", "synthetic-private-token", "https://synthetic.invalid/private", "private-extension"} {
					if strings.Contains(got.timeline, secret) {
						t.Fatalf("timeline includes private data %q: %s", secret, got.timeline)
					}
				}
				var diagnostics []gjson.Result
				for _, line := range strings.Split(got.timeline, "\n") {
					if gjson.Valid(line) {
						diagnostics = append(diagnostics, gjson.Parse(line))
					}
				}
				if len(diagnostics) != 2 || !strings.Contains(got.timeline, "websocket.interrupt\n") || !strings.Contains(got.timeline, "websocket.interrupt.outcome\n") {
					t.Fatalf("missing interrupt diagnostics: %s", got.timeline)
				}
				for index, diagnostic := range diagnostics {
					wantFields := 2
					if index == 1 {
						wantFields = 3
						if diagnostic.Get("outcome").String() != test.outcome {
							t.Fatalf("outcome = %s, want %s", diagnostic.Raw, test.outcome)
						}
					}
					if diagnostic.Get("type").String() != "response.interrupt" || diagnostic.Get("response_id_hash").String() != wantHash || len(diagnostic.Map()) != wantFields {
						t.Fatalf("unexpected diagnostic: %s", diagnostic.Raw)
					}
				}
			case <-time.After(3 * time.Second):
				t.Fatal("reader completion did not yield a timeline")
			}
		})
	}
}

func TestWebsocketTimelineRecorderConcurrentAppendAndSnapshot(t *testing.T) {
	recorder := newWebsocketTimelineRecorder(nil)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 32 {
				appendWebsocketEvent(recorder, "response", []byte(`{"type":"response.created"}`))
				_ = recorder.String()
			}
		})
	}
	workers.Wait()
	if count := strings.Count(recorder.String(), "websocket.response\n"); count != 256 {
		t.Fatalf("timeline event count = %d, want 256", count)
	}
}

type interruptCaptureExecutor struct {
	*compactCaptureExecutor
	mu        sync.Mutex
	active    bool
	sessionID string
	calls     int
	release   chan struct{}
	received  chan []byte
}

func (*interruptCaptureExecutor) Identifier() string { return "codex" }

func (e *interruptCaptureExecutor) ExecuteStream(
	ctx context.Context, _ *coreauth.Auth, _ cliproxyexecutor.Request, opts cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	call := e.calls
	if call == 1 {
		e.active = true
		e.sessionID, _ = opts.Metadata[cliproxyexecutor.ExecutionSessionMetadataKey].(string)
		e.release = make(chan struct{})
	}
	release := e.release
	e.mu.Unlock()
	chunks := make(chan cliproxyexecutor.StreamChunk, 2)
	go func() {
		defer close(chunks)
		if call != 1 {
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"type":"response.created","response":{"id":"r2"}}`)}
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"type":"response.completed","response":{"id":"r2","output":[]}}`)}
			return
		}
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"type":"response.created","response":{"id":"r1"}}`)}
		select {
		case <-release:
		case <-ctx.Done():
			return
		}
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"type":"response.incomplete","response":{"id":"r1","status":"incomplete","output":[]}}`)}
		e.mu.Lock()
		e.active = false
		e.mu.Unlock()
	}()
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *interruptCaptureExecutor) InterruptExecutionSession(ctx context.Context, sessionID string, payload []byte) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.active {
		return cliproxyexecutor.ErrNoActiveUpstreamWebsocket
	}
	if sessionID != e.sessionID || !cliproxyexecutor.WebsocketAuthEnabled(ctx, "offline-codex-auth") {
		return errors.New("interrupt auth or session mismatch")
	}
	e.received <- bytes.Clone(payload)
	close(e.release)
	return nil
}

func TestResponsesWebsocketInterruptRoutesThroughAuthManager(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &interruptCaptureExecutor{
		compactCaptureExecutor: &compactCaptureExecutor{},
		received:               make(chan []byte, 1),
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	const authID = "offline-codex-auth"
	const model = "offline-codex-model"
	_, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: authID, Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{"api_key": "synthetic-test-token"},
	})
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, "codex", []*registry.ModelInfo{{ID: model}})
	defer registry.GetGlobalRegistry().UnregisterClient(authID)

	handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	finished := make(chan string, 1)
	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) {
		handler.ResponsesWebsocket(c)
		body, _ := c.Get(wsTimelineBodyKey)
		data, _ := body.([]byte)
		finished <- string(data)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatalf("dial downstream websocket: %v", err)
	}
	defer func() { _ = client.Close() }()
	_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
	send := func(payload []byte) {
		t.Helper()
		if errSend := client.WriteMessage(websocket.TextMessage, payload); errSend != nil {
			t.Fatalf("send downstream frame: %v", errSend)
		}
	}
	expect := func(want string) {
		t.Helper()
		_, payload, errRead := client.ReadMessage()
		if errRead != nil || gjson.GetBytes(payload, "type").String() != want {
			t.Fatalf("expected %q, got %s, error %v", want, payload, errRead)
		}
	}
	interrupt := []byte(`{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items","extension":42}`)
	send(interrupt)
	expect("error")
	send([]byte(`{"type":"response.create","model":"offline-codex-model","input":[]}`))
	expect("response.created")
	send(interrupt)
	expect("response.incomplete")
	select {
	case got := <-executor.received:
		if !bytes.Equal(got, interrupt) {
			t.Fatalf("interrupt payload changed: %s", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("bound executor did not receive the interrupt")
	}
	send([]byte(`{"type":"response.create","model":"offline-codex-model","input":[]}`))
	expect("response.created")
	expect("response.done")
	_ = client.Close()
	select {
	case timeline := <-finished:
		if strings.Count(timeline, "websocket.interrupt\n") != 2 ||
			!strings.Contains(timeline, `"outcome":"rejected"`) ||
			!strings.Contains(timeline, `"outcome":"handled"`) ||
			strings.Contains(timeline, "discard_partial_items") || strings.Contains(timeline, `"extension":42`) {
			t.Fatalf("handler timeline = %s", timeline)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not finish after the reader disconnected")
	}
}
