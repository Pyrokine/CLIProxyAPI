package openai

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	clipexec "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestNormalizeSubsequentRequestDropsConsumedCompactionTrigger(t *testing.T) {
	lastRequest := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":[
		{"type":"message","role":"user","id":"msg-old","content":"old prompt"}
	]}`)
	triggerRequest := []byte(`{"type":"response.create","previous_response_id":"resp-before-compact","input":[
		{"type":"message","role":"user","id":"msg-tool-output","content":"done"},
		{"type":"compaction_trigger"}
	]}`)
	_, stateAfterTrigger, errMsg := normalizeResponsesWebsocketRequestWithMode(triggerRequest, lastRequest, nil, false)
	if errMsg != nil {
		t.Fatalf("normalize trigger request: %v", errMsg.Error)
	}
	compactionOutput := []byte(`[
		{"type":"compaction","id":"cmp-1","encrypted_content":"opaque"}
	]`)
	replayRequest := []byte(`{"type":"response.create","input":[
		{"type":"message","role":"developer","id":"msg-new-context","content":"new context"},
		{"type":"compaction","id":"cmp-1","encrypted_content":"opaque"},
		{"type":"message","role":"user","id":"msg-next","content":"continue"}
	]}`)
	normalized, _, errMsg := normalizeResponsesWebsocketRequestWithMode(
		replayRequest, stateAfterTrigger, compactionOutput, false,
	)
	if errMsg != nil {
		t.Fatalf("normalize compact replay: %v", errMsg.Error)
	}
	for _, item := range gjson.GetBytes(normalized, "input").Array() {
		if item.Get("type").String() == "compaction_trigger" {
			t.Fatalf("consumed compaction_trigger was replayed: %s", normalized)
		}
	}
	if gjson.GetBytes(normalized, "model").String() != "gpt-5.6-sol" || !gjson.GetBytes(normalized, "stream").Bool() {
		t.Fatalf("continuation lost its model or stream flag: %s", normalized)
	}
}

func TestWebsocketDownstreamSessionKeyKeepsOriginalHeaderPrecedence(t *testing.T) {
	if got := websocketDownstreamSessionKey(nil); got != "" {
		t.Fatalf("nil request key=%q", got)
	}
	for _, test := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"hyphen", map[string]string{"Session-Id": " legacy-session "}, "legacy-session"},
		{"underscore", map[string]string{"Session_id": " underscore-session "}, "underscore-session"},
		{
			"both spellings",
			map[string]string{"Session-Id": "legacy-session", "Session_id": "underscore-session"},
			"legacy-session",
		},
		{
			"blank hyphen", map[string]string{"Session-Id": " ", "Session_id": "underscore-session"},
			"underscore-session",
		},
		{
			"turn metadata",
			map[string]string{"X-Codex-Turn-Metadata": `{"session_id":"turn-session"}`, "Session-Id": "legacy-session"},
			"turn-session",
		},
		{
			"request id",
			map[string]string{
				"X-Client-Request-Id":   "request-session",
				"X-Codex-Turn-Metadata": `{"session_id":"turn-session"}`,
				"Session-Id":            "legacy-session",
			},
			"request-session",
		},
		{
			"invalid metadata",
			map[string]string{"X-Codex-Turn-Metadata": `{`, "Session-Id": "legacy-session"},
			"legacy-session",
		},
		{"empty", nil, ""},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodGet, "http://example.invalid/v1/responses", nil)
				for key, value := range test.headers {
					request.Header.Set(key, value)
				}
				if got := websocketDownstreamSessionKey(request); got != test.want {
					t.Fatalf("session key=%q, want %q", got, test.want)
				}
			},
		)
	}
}

func TestResponsesWebsocketPreviousResponseKeepsCompleteCalls(t *testing.T) {
	for _, item := range []struct {
		name     string
		raw      string
		complete bool
	}{
		{"function", `{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"}`, true},
		{
			"empty function arguments", `{"type":"function_call","call_id":"call-1","name":"lookup","arguments":""}`,
			true,
		},
		{"custom", `{"type":"custom_tool_call","call_id":"call-1","name":"lookup","input":"echo hello"}`, true},
		{"empty custom input", `{"type":"custom_tool_call","call_id":"call-1","name":"lookup","input":""}`, true},
		{"missing arguments", `{"type":"function_call","call_id":"call-1","name":"lookup"}`, false},
		{"object arguments", `{"type":"function_call","call_id":"call-1","name":"lookup","arguments":{}}`, false},
		{"missing custom input", `{"type":"custom_tool_call","call_id":"call-1","name":"lookup"}`, false},
		{"missing name", `{"type":"function_call","call_id":"call-1","arguments":"{}"}`, false},
		{"missing call id", `{"type":"function_call","name":"lookup","arguments":"{}"}`, false},
	} {
		for _, previous := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/previous_%t", item.name, previous), func(t *testing.T) {
					outputCache := newWebsocketToolOutputCache(0, 8)
					callCache := newWebsocketToolOutputCache(0, 8)
					parent := ""
					if previous {
						parent = `"previous_response_id":"resp-1",`
					}
					payload := []byte(`{` + parent + `"input":[{"type":"message","id":"keep"},` +
						item.raw + `],"metadata":{"n":9007199254740993}}`)
					before := bytes.Clone(payload)
					got := repairResponsesWebsocketToolCallsWithCaches(outputCache, callCache, t.Name(), payload)
					if !bytes.Equal(payload, before) || gjson.GetBytes(got, "metadata.n").Raw != "9007199254740993" {
						t.Fatal("repair changed the caller buffer or unrelated bytes")
					}
					items := gjson.GetBytes(got, "input").Array()
					if previous && item.complete {
						if len(items) != 2 || items[1].Raw != item.raw || !bytes.Equal(got, before) {
							t.Fatalf("complete continuation call changed: %s", got)
						}
					} else if len(items) != 1 || items[0].Get("id").String() != "keep" {
						t.Fatalf("unpaired or incomplete call retained: %s", got)
					}
				},
			)
		}
	}
}

func TestResponsesWebsocketLegacySessionCacheKeepsOwnedTurnUntilLastRelease(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://example.invalid/v1/responses", nil)
	request.Header.Set("Session-Id", t.Name())
	key := websocketDownstreamSessionKey(request)
	if key != t.Name() {
		t.Fatalf("legacy session key=%q", key)
	}
	retainResponsesWebsocketToolCaches(key)
	retainResponsesWebsocketToolCaches(key)
	remaining := 2
	t.Cleanup(
		func() {
			for remaining > 0 {
				releaseResponsesWebsocketToolCaches(key)
				remaining--
			}
		},
	)
	payload := []byte(`{"previous_response_id":"resp-1","input":[` +
		`{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call-1","output":"owned-result"}]}`)
	_, turn := prepareResponsesWebsocketFallbackTurn(key, payload)
	if turn == nil {
		t.Fatal("legacy session did not create a pending turn")
	}
	want := bytes.Clone(turn.calls["call-1"])
	wantOutput := bytes.Clone(turn.outputs["call-1"])
	if len(want) == 0 || len(wantOutput) == 0 {
		t.Fatal("pending turn did not retain both input items")
	}
	for index := range payload {
		payload[index] = 'x'
	}
	for _, cache := range []*websocketToolOutputCache{defaultWebsocketToolCallCache, defaultWebsocketToolOutputCache} {
		if _, found := cache.get(key, "call-1"); found {
			t.Fatal("pending turn published before delivery")
		}
	}
	turn.commit()
	got, found := defaultWebsocketToolCallCache.get(key, "call-1")
	if !found || !bytes.Equal(got, want) {
		t.Fatalf("committed call aliases its source: %s", got)
	}
	if output, found := defaultWebsocketToolOutputCache.get(key, "call-1"); !found || !bytes.Equal(output, wantOutput) {
		t.Fatalf("committed output aliases its source: %s", output)
	}
	got[0] = 'x'
	if again, _ := defaultWebsocketToolCallCache.get(key, "call-1"); !bytes.Equal(again, want) {
		t.Fatal("cache reader changed the stored call")
	}
	releaseResponsesWebsocketToolCaches(key)
	remaining--
	for _, cache := range []*websocketToolOutputCache{defaultWebsocketToolCallCache, defaultWebsocketToolOutputCache} {
		if _, found := cache.get(key, "call-1"); !found {
			t.Fatal("closing one of two users deleted the shared session")
		}
	}
	releaseResponsesWebsocketToolCaches(key)
	remaining--
	for _, cache := range []*websocketToolOutputCache{defaultWebsocketToolCallCache, defaultWebsocketToolOutputCache} {
		if _, found := cache.get(key, "call-1"); found {
			t.Fatal("last session release retained the cache")
		}
	}
}

type codexContinuationCaptureExecutor struct {
	prewarmCaptureExecutor
	rejectContinuation bool
}

func (*codexContinuationCaptureExecutor) Identifier() string { return "codex" }

func (e *codexContinuationCaptureExecutor) ExecuteStream(
	ctx context.Context, auth *coreauth.Auth, req clipexec.Request, opts clipexec.Options,
) (*clipexec.StreamResult, error) {
	result, err := e.prewarmCaptureExecutor.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	if e.rejectContinuation && gjson.GetBytes(req.Payload, "previous_response_id").String() != "" {
		return nil, &clipexec.RequestTerminatedError{HTTPStatus: http.StatusBadRequest}
	}
	return result, nil
}

func TestResponsesWebsocketLegacySessionAndCompleteCallsReachExecutor(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []struct {
		call   string
		reject bool
	}{
		{`{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"}`, false},
		{`{"type":"custom_tool_call","call_id":"call-1","name":"lookup","input":"echo hello"}`, false},
		{`{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}"}`, true},
		{`{"type":"custom_tool_call","call_id":"call-1","name":"lookup","input":"echo hello"}`, true},
	} {
		call := scenario.call
		t.Run(
			fmt.Sprintf("%s/reject_%t", gjson.Get(call, "type").String(), scenario.reject), func(t *testing.T) {
				retained := false
				if scenario.reject {
					retainResponsesWebsocketToolCaches(t.Name())
					retained = true
					t.Cleanup(
						func() {
							if retained {
								releaseResponsesWebsocketToolCaches(t.Name())
							}
						},
					)
				}
				executor := &codexContinuationCaptureExecutor{rejectContinuation: scenario.reject}
				manager := coreauth.NewManager(nil, nil, nil)
				manager.RegisterExecutor(executor)
				const model = "codex-original-input-model"
				auth := &coreauth.Auth{
					ID: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
					Attributes: map[string]string{"api_key": "synthetic-token", "websockets": "true"},
				}
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatal(err)
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				handler := NewResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
				finished := make(chan struct{})
				router := gin.New()
				router.GET(
					"/v1/responses", func(c *gin.Context) {
						handler.ResponsesWebsocket(c)
						close(finished)
					},
				)
				server := httptest.NewServer(router)
				defer server.Close()
				client, _, err := websocket.DefaultDialer.Dial(
					"ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses",
					http.Header{"Session-Id": {t.Name()}},
				)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = client.Close() }()
				if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
					t.Fatal(err)
				}
				exchange := func(payload, wantEvent string) {
					t.Helper()
					if err := client.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
						t.Fatal(err)
					}
					_, response, err := client.ReadMessage()
					if err != nil || gjson.GetBytes(response, "type").String() != wantEvent {
						t.Fatalf("event=%s want=%s error=%v", response, wantEvent, err)
					}
				}
				exchange(
					`{"type":"response.create","model":"`+model+`","input":[`+
						`{"type":"message","role":"user","content":"start"},`+
						`{"type":"function_call","call_id":"seed-call","name":"seed","arguments":"{}"},`+
						`{"type":"function_call_output","call_id":"seed-call","output":"previous-result"}]}`,
					wsEventTypeCompleted,
				)
				continuationEvent := wsEventTypeCompleted
				if scenario.reject {
					continuationEvent = "error"
				}
				exchange(
					`{"type":"response.create","previous_response_id":"generated-1","input":[`+call+`]}`,
					continuationEvent,
				)
				outputType := "function_call_output"
				if gjson.Get(call, "type").String() == "custom_tool_call" {
					outputType = "custom_tool_call_output"
				}
				if !scenario.reject {
					exchange(
						`{"type":"response.create","previous_response_id":"generated-2","input":[`+
							`{"type":"`+outputType+`","call_id":"call-1","output":"done"}]}`, wsEventTypeCompleted,
					)
				}
				executor.mu.Lock()
				payloads := append([][]byte(nil), executor.payloads...)
				executor.mu.Unlock()
				wantCalls := 3
				if scenario.reject {
					wantCalls = 2
				}
				if len(payloads) != wantCalls || gjson.GetBytes(
					payloads[1], "previous_response_id",
				).String() != "generated-1" ||
					gjson.GetBytes(payloads[1], "input.0").Raw != call {
					t.Fatalf("complete native continuation changed: %q", payloads)
				}
				if !scenario.reject {
					third := gjson.GetBytes(payloads[2], "input").Array()
					if len(third) != 1 || third[0].Get("type").String() != outputType ||
						third[0].Get("call_id").String() != "call-1" {
						t.Fatalf("native continuation gained an unnecessary cached companion: %s", payloads[2])
					}
					if cached, found := defaultWebsocketToolCallCache.get(t.Name(), "call-1"); !found ||
						string(cached) != call {
						t.Fatalf("native continuation changed the delivered call cache: %s", cached)
					}
				}
				if err := client.Close(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					t.Fatal("websocket handler did not release its session")
				}
				if scenario.reject {
					if _, found := defaultWebsocketToolCallCache.get(t.Name(), "call-1"); found {
						t.Fatal("failed continuation published its pending tool call")
					}
					for _, cache := range []*websocketToolOutputCache{
						defaultWebsocketToolCallCache, defaultWebsocketToolOutputCache,
					} {
						if _, found := cache.get(t.Name(), "seed-call"); !found {
							t.Fatal("failed continuation removed the previously committed turn")
						}
					}
					releaseResponsesWebsocketToolCaches(t.Name())
					retained = false
				}
				for _, cache := range []*websocketToolOutputCache{
					defaultWebsocketToolCallCache, defaultWebsocketToolOutputCache,
				} {
					for _, id := range []string{"call-1", "seed-call"} {
						if _, found := cache.get(t.Name(), id); found {
							t.Fatal("last session release retained the tool cache")
						}
					}
				}
			},
		)
	}
}
