package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestC09MessageTooBigMappingIsRequestScoped(t *testing.T) {
	err := mapCodexWebsocketReadError(
		&websocket.CloseError{
			Code: websocket.CloseMessageTooBig,
			Text: "request exceeded upstream websocket limit",
		},
	)
	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusRequestEntityTooLarge {
		t.Fatalf("mapped error = %T %v, want status 413", err, err)
	}
	var requestErr cliproxyexecutor.RequestScopedError
	if !errors.As(err, &requestErr) || !requestErr.IsRequestScoped() {
		t.Fatalf("mapped error = %T, want request scoped", err)
	}
	var dispositionErr cliproxyexecutor.ExecutionDispositionError
	if !errors.As(err, &dispositionErr) {
		t.Fatalf("mapped error = %T, want execution disposition", err)
	}
	disposition := dispositionErr.ExecutionDisposition()
	if disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry ||
		disposition.EffectScope != cliproxyexecutor.StateEffectScopeRequest {
		t.Fatalf("mapped disposition = %#v, want request-scoped no-retry", disposition)
	}
}

func TestC09WebsocketCloseStateIsConnectionScoped(t *testing.T) {
	sess := &codexWebsocketSession{}
	first := &websocket.Conn{}
	second := &websocket.Conn{}

	sess.resetUpstreamDisconnectError(first)
	sess.setUpstreamDisconnectError(first, &websocket.CloseError{Code: websocket.CloseMessageTooBig})
	if got := sess.upstreamDisconnectError(first); got == nil {
		t.Fatal("first connection close state was not retained")
	}

	sess.resetUpstreamDisconnectError(second)
	sess.setUpstreamDisconnectError(first, fmt.Errorf("stale close"))
	if got := sess.upstreamDisconnectError(second); got != nil {
		t.Fatalf("second connection inherited stale close state: %v", got)
	}
}

func TestC09TerminalReadInvalidatesFullChannelBeforeWaiting(t *testing.T) {
	ch := make(chan codexWebsocketRead, 1)
	ch <- codexWebsocketRead{payload: []byte("occupied")}
	done := make(chan struct{})
	invalidated := make(chan struct{})
	returned := make(chan bool, 1)

	go func() {
		returned <- sendTerminalWebsocketRead(
			ch,
			done,
			codexWebsocketRead{err: errors.New("terminal")},
			func() { close(invalidated) },
		)
	}()

	select {
	case <-invalidated:
	case <-time.After(time.Second):
		t.Fatal("full terminal channel blocked connection invalidation")
	}

	<-ch
	select {
	case got := <-returned:
		if !got {
			t.Fatal("sendTerminalWebsocketRead() did not report invalidation")
		}
	case <-time.After(time.Second):
		t.Fatal("terminal event did not complete after channel capacity became available")
	}

	event := <-ch
	if event.err == nil || event.err.Error() != "terminal" {
		t.Fatalf("terminal event = %#v, want terminal error", event)
	}
	close(done)
}

func TestC09ProviderSessionUseRejectsInvalidExplicitIDs(t *testing.T) {
	derivedID := "ctx:v1:provider-boundary"
	metadata := map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: derivedID}
	invalidPromptCachePayload := []byte(`{"prompt_cache_key":"bad\nsession","input":"hello"}`)
	request := cliproxyexecutor.Request{
		Model:    "gpt-5",
		Payload:  invalidPromptCachePayload,
		Metadata: metadata,
	}

	wantCodexID := helps.ProviderSessionUUID("codex", metadata)
	_, body, _, err := (&codexExecutor{}).cacheHelper(
		context.Background(),
		sdktranslator.FormatOpenAIResponse,
		"https://example.com/responses",
		nil,
		request,
		invalidPromptCachePayload,
		invalidPromptCachePayload,
	)
	if err != nil {
		t.Fatalf("cacheHelper() error = %v", err)
	}
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != wantCodexID {
		t.Fatalf("Codex HTTP prompt cache = %q, want derived %q", got, wantCodexID)
	}

	websocketBody, websocketHeaders := applyCodexPromptCacheHeaders(
		sdktranslator.FormatOpenAIResponse,
		request,
		invalidPromptCachePayload,
	)
	if got := gjson.GetBytes(websocketBody, "prompt_cache_key").String(); got != wantCodexID {
		t.Fatalf("Codex WebSocket prompt cache = %q, want derived %q", got, wantCodexID)
	}
	if got := websocketHeaders.Get("Session-Id"); got != wantCodexID {
		t.Fatalf("Codex WebSocket session header = %q, want derived %q", got, wantCodexID)
	}

	invalidExecutionMetadata := map[string]any{
		cliproxyexecutor.ExecutionSessionMetadataKey: "bad\nsession",
		cliproxyexecutor.DerivedSessionIDMetadataKey: derivedID,
	}
	wantXAIID := helps.DerivedSessionUUID("xai", invalidExecutionMetadata)
	if got := xaiExecutionSessionID(
		cliproxyexecutor.Request{Payload: invalidPromptCachePayload},
		cliproxyexecutor.Options{Metadata: invalidExecutionMetadata},
	); got != wantXAIID {
		t.Fatalf("xAI session = %q, want derived %q", got, wantXAIID)
	}

	antigravityPayload := []byte(`{"request":{"sessionId":"bad\nsession"}}`)
	antigravityBody := geminiToAntigravity("gemini-2.5-pro", antigravityPayload, "project", "-123456")
	if got := gjson.GetBytes(antigravityBody, "request.sessionId").String(); got != "-123456" {
		t.Fatalf("Antigravity session = %q, want derived -123456", got)
	}

	scope := antigravityReasoningReplayScopeFromRequest(
		context.Background(),
		"gemini-2.5-pro",
		cliproxyexecutor.Request{Payload: antigravityPayload, Metadata: metadata},
		cliproxyexecutor.Options{},
		antigravityPayload,
	)
	if scope.sessionKey != "derived:"+derivedID {
		t.Fatalf("Antigravity replay scope = %q, want derived identity", scope.sessionKey)
	}
}

func TestC09CodexReplayRejectsInvalidExplicitIDs(t *testing.T) {
	oversized := strings.Repeat("x", 257)
	payload := []byte(`{"prompt_cache_key":"bad\nsession","client_metadata":{"x-codex-window-id":"` + oversized + `"}}`)
	if got := codexReasoningReplaySessionKeyFromPayload(payload); got != "" {
		t.Fatalf("Codex payload replay key = %q, want empty", got)
	}
	if got := codexReasoningReplaySessionKeyFromTurnMetadata(`{"prompt_cache_key":"bad\nsession","window_id":"` + oversized + `"}`); got != "" {
		t.Fatalf("Codex turn metadata replay key = %q, want empty", got)
	}

	headers := http.Header{"X-Codex-Window-Id": {"bad\nsession", "valid-window"}}
	if got := codexReasoningReplaySessionKeyFromHeaders(headers); got != "window:valid-window" {
		t.Fatalf("Codex header replay key = %q, want later valid value", got)
	}
	if got := codexPromptCacheClaudeScope([]byte(`{"metadata":{"user_id":"` + oversized + `"}}`)); got != "" {
		t.Fatalf("Codex Claude cache scope = %q, want empty", got)
	}
}

func TestC09RequiredCodexWebsocketRejectsMissingActiveConnection(t *testing.T) {
	exec := newCodexWebsocketsExecutor(&config.Config{})
	ctx := cliproxyexecutor.WithRequiredUpstreamWebsocket(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
	)
	result, err := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{
			ID:       "c09-codex-required",
			Provider: "codex",
			Attributes: map[string]string{
				"api_key":    "token",
				"base_url":   "https://example.com",
				"websockets": "true",
			},
		},
		cliproxyexecutor.Request{
			Model:   "gpt-5",
			Payload: []byte(`{"model":"gpt-5","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-2"}]}`),
		},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse,
			Stream:       true,
			Metadata: map[string]any{
				cliproxyexecutor.ExecutionSessionMetadataKey: "c09-codex-session",
			},
		},
	)
	if result != nil {
		t.Fatalf("ExecuteStream() result = %#v, want nil", result)
	}
	if !cliproxyexecutor.IsUpstreamWebsocketReplayRequired(err) {
		t.Fatalf("ExecuteStream() error = %T %v, want replay-required", err, err)
	}
}

func TestC09RequiredXAIWebsocketRejectsMissingActiveConnection(t *testing.T) {
	exec := newXAIWebsocketsExecutor(&config.Config{})
	ctx := cliproxyexecutor.WithRequiredUpstreamWebsocket(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
	)
	result, err := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{
			ID:       "c09-xai-required",
			Provider: "xai",
			Attributes: map[string]string{
				"api_key":    "token",
				"base_url":   "https://example.com",
				"websockets": "true",
			},
		},
		cliproxyexecutor.Request{
			Model:   "grok-4.3",
			Payload: []byte(`{"type":"response.append","model":"grok-4.3","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-2"}]}`),
		},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse,
			Stream:       true,
			Metadata: map[string]any{
				cliproxyexecutor.ExecutionSessionMetadataKey: "c09-xai-session",
			},
		},
	)
	if result != nil {
		t.Fatalf("ExecuteStream() result = %#v, want nil", result)
	}
	if !cliproxyexecutor.IsUpstreamWebsocketReplayRequired(err) {
		t.Fatalf("ExecuteStream() error = %T %v, want replay-required", err, err)
	}
}

func TestC09XAICompactedTranscriptReplaysOnceAcrossReset(t *testing.T) {
	store := &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
	state := store.get("c09-xai-compact-reset")
	if err := state.replaceTranscriptWithItems(
		[]byte(`{"type":"compaction","encrypted_content":"compact-state"}`),
	); err != nil {
		t.Fatalf("replaceTranscriptWithItems() error = %v", err)
	}
	state.mapDownstreamToUpstream("resp-compact", "")

	appendPayload := []byte(`{"type":"response.append","model":"grok-4.3","input":[{"type":"message","id":"msg-2"}]}`)
	mapper := newXAIWebsocketRequestIDMapper(store, "c09-xai-compact-reset", appendPayload)
	upstream, err := mapper.upstreamRequestPayload(appendPayload)
	if err != nil {
		t.Fatalf("upstreamRequestPayload() error = %v", err)
	}
	input := gjson.GetBytes(upstream, "input").Array()
	if len(input) != 2 || input[0].Get("type").String() != "compaction" || input[1].Get("id").String() != "msg-2" {
		t.Fatalf("post-compact append input = %s", gjson.GetBytes(upstream, "input").Raw)
	}
	if !mapper.replayedCompactedTranscript {
		t.Fatal("post-compact append did not mark replayed compact transcript")
	}
	if err := mapper.recordTranscriptTurn(
		upstream,
		[]byte(`{"type":"response.completed","response":{"id":"resp-2","output":[{"type":"message","id":"out-2"}]}}`),
		true,
	); err != nil {
		t.Fatalf("recordTranscriptTurn() error = %v", err)
	}

	resetPayload := []byte(`{"type":"response.create","model":"grok-4.3","input":[{"type":"message","id":"msg-3"}]}`)
	resetMapper := newXAIWebsocketRequestIDMapper(store, "c09-xai-compact-reset", resetPayload)
	reset, err := resetMapper.upstreamRequestPayload(resetPayload)
	if err != nil {
		t.Fatalf("reset upstreamRequestPayload() error = %v", err)
	}
	resetInput := gjson.GetBytes(reset, "input").Array()
	if len(resetInput) != 1 || resetInput[0].Get("id").String() != "msg-3" {
		t.Fatalf("full reset replayed stale compact transcript: %s", reset)
	}
}

func TestC09ValidateXAIWebsocketCompactionResponse(t *testing.T) {
	valid := []byte(`{"id":"resp-compact","output":[{"type":"compaction","encrypted_content":"opaque"}]}`)
	responseID, item, err := validateXAIWebsocketCompactionResponse(valid)
	if err != nil {
		t.Fatalf("validate valid response error = %v", err)
	}
	if responseID != xaiCompactionResponseID(valid) || gjson.GetBytes(item, "encrypted_content").String() != "opaque" {
		t.Fatalf("validated response = id:%q item:%s", responseID, item)
	}
	for _, invalid := range [][]byte{
		nil,
		[]byte(`{}`),
		[]byte(`{"id":"resp-empty","output":[]}`),
		[]byte(`{"id":"resp-missing","output":[{"type":"compaction"}]}`),
	} {
		if _, _, errInvalid := validateXAIWebsocketCompactionResponse(invalid); errInvalid == nil {
			t.Fatalf("invalid compact response accepted: %s", invalid)
		}
	}
}
