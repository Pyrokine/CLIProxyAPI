package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestExistingXAIWebsocketSessionRejectsRotatedCredential(t *testing.T) {
	conn := &websocket.Conn{}
	sess := &codexWebsocketSession{
		conn:             conn,
		authID:           "auth-a",
		wsURL:            "wss://example.com/responses",
		credentialSHA256: "credential-a",
		proxyURL:         "http://proxy-a.example:8080",
	}
	if got := existingXAIWebsocketSessionConn(sess, sess.authID, sess.wsURL, "credential-a", sess.proxyURL); got != conn {
		t.Fatal("matching credential and proxy did not reuse the connection")
	}
	if got := existingXAIWebsocketSessionConn(sess, sess.authID, sess.wsURL, "credential-b", sess.proxyURL); got != nil {
		t.Fatal("rotated credential reused the old connection")
	}
	if got := existingXAIWebsocketSessionConn(sess, sess.authID, sess.wsURL, "credential-a", "http://proxy-b.example:8080"); got != nil {
		t.Fatal("changed proxy reused the old connection")
	}
}

func TestXAIWebsocketSessionRedialsAfterTerminalReadState(t *testing.T) {
	var accepted atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		accepted.Add(1)
		defer conn.Close()
		for {
			if _, _, err = conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	ctx := context.Background()
	exec := newXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "session-auth", Provider: "xai", Attributes: map[string]string{"api_key": "synthetic"}}
	sess := &codexWebsocketSession{sessionID: "terminal-read-session"}
	defer exec.closeExecutionSession(sess, "test_complete")
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/responses"
	first, _, err := exec.ensureUpstreamConn(ctx, auth, sess, auth.ID, wsURL, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	sess.setUpstreamDisconnectError(first, errors.New("terminal read"))
	if reusable := existingXAIWebsocketSessionConn(sess, auth.ID, wsURL,
		cliproxyauth.AccessTokenSHA256(auth), effectiveProxyURL(ctx, exec.cfg, auth)); reusable != nil {
		t.Fatal("terminal connection was reusable")
	}
	second, _, err := exec.ensureUpstreamConn(ctx, auth, sess, auth.ID, wsURL, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if second == first || accepted.Load() != 2 {
		t.Fatalf("terminal connection was reused: first=%p second=%p accepted=%d", first, second, accepted.Load())
	}
}

func TestXAIWebsocketReaderMarksUnexpectedBinaryBeforeSessionReuse(t *testing.T) {
	var accepted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if accepted.Add(1) == 1 {
			if err = conn.WriteMessage(websocket.BinaryMessage, []byte("unexpected")); err != nil {
				return
			}
		}
		for {
			if _, _, err = conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	ctx := context.Background()
	exec := newXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{ID: "binary-auth", Provider: "xai", Attributes: map[string]string{"api_key": "synthetic"}}
	sess := &codexWebsocketSession{sessionID: "binary-session"}
	defer exec.closeExecutionSession(sess, "test_complete")
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/responses"
	first, _, err := exec.ensureUpstreamConn(ctx, auth, sess, auth.ID, wsURL, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for sess.upstreamDisconnectError(first) == nil {
		select {
		case <-deadline:
			t.Fatal("unexpected binary did not mark the connection terminal")
		case <-time.After(time.Millisecond):
		}
	}
	second, _, err := exec.ensureUpstreamConn(ctx, auth, sess, auth.ID, wsURL, http.Header{})
	if err != nil || first == second || accepted.Load() != 2 {
		t.Fatalf("terminal connection reused: first=%p second=%p accepted=%d error=%v", first, second, accepted.Load(), err)
	}
}

func TestXAIWebsocketsCloseExecutionSessionsForAuth(t *testing.T) {
	store := &xaiWebsocketSessionStore{
		sessions: map[string]*codexWebsocketSession{
			"session-a": {sessionID: "session-a", authID: "auth-a"},
			"session-b": {sessionID: "session-b", authID: "auth-b"},
		},
	}
	idStore := &xaiWebsocketIDStateStore{
		sessions: map[string]*xaiWebsocketIDState{
			"session-a": {},
			"session-b": {},
		},
	}
	exec := &XAIWebsocketsExecutor{store: store, idStore: idStore}

	exec.CloseExecutionSessionsForAuth("auth-a")

	if store.sessions["session-a"] != nil || store.sessions["session-b"] == nil {
		t.Fatalf("sessions after auth close = %#v", store.sessions)
	}
	if idStore.sessions["session-a"] != nil || idStore.sessions["session-b"] == nil {
		t.Fatalf("ID states after auth close = %#v", idStore.sessions)
	}
}

func TestXAIWebsocketsExecuteStreamFreeUsageUpgradeSetsRetryAfter(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"code":"subscription:free-usage-exhausted","error":"included free usage exhausted"}`))
			},
		),
	)
	defer server.Close()

	exec := newXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}
	_, err := exec.ExecuteStream(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
		auth,
		cliproxyexecutor.Request{
			Model: "grok-4.3", Payload: []byte(`{"model":"grok-4.3","stream":true,"input":"hello"}`),
		},
		cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatOpenAIResponse,
			ResponseFormat: sdktranslator.FormatOpenAIResponse,
		},
	)
	if err == nil {
		t.Fatal("ExecuteStream() error = nil")
	}
	status, ok := err.(interface {
		StatusCode() int
		RetryAfter() *time.Duration
	})
	if !ok {
		t.Fatalf("ExecuteStream() error type = %T, want status error", err)
	}
	if status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", status.StatusCode())
	}
	if retryAfter := status.RetryAfter(); retryAfter == nil || *retryAfter != xaiFreeUsageExhaustedCooldown {
		t.Fatalf("retryAfter = %v, want %v", retryAfter, xaiFreeUsageExhaustedCooldown)
	}
}

func TestParseXAIWebsocketErrorBadCredentialsRemapsToUnauthorized(t *testing.T) {
	payload := []byte(`{"type":"error","status":403,"headers":{"x-request-id":"req-bad-credentials"},"error":{"code":"unauthenticated:bad-credentials","message":"The OAuth2 access token could not be validated."}}`)
	err, ok := parseXAIWebsocketError(payload)
	if !ok {
		t.Fatal("expected xAI websocket error")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("status = %#v, want 401", err)
	}
	headerSource, ok := err.(interface{ Headers() http.Header })
	if !ok {
		t.Fatalf("error type = %T, want headers", err)
	}
	if got := headerSource.Headers().Get("x-request-id"); got != "req-bad-credentials" {
		t.Fatalf("x-request-id = %q, want req-bad-credentials", got)
	}
}

func TestParseXAIWebsocketBareBadCredentialsRemapsToUnauthorized(t *testing.T) {
	payload := []byte(`{"status":403,"error":{"code":"unauthenticated:bad-credentials","message":"The OAuth2 access token could not be validated."}}`)
	err, ok := parseXAIWebsocketError(payload)
	if !ok {
		t.Fatal("expected xAI websocket error")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("status = %#v, want 401", err)
	}
}

func TestParseXAIWebsocketTypedBadCredentialsWithoutHeadersRemapsToUnauthorized(t *testing.T) {
	payload := []byte(`{"type":"error","status":403,"error":{"code":"unauthenticated:bad-credentials","message":"The OAuth2 access token could not be validated."}}`)
	err, ok := parseXAIWebsocketError(payload)
	if !ok {
		t.Fatal("expected xAI websocket error")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("status = %#v, want 401", err)
	}
}

func TestParseXAIWebsocketContentSafetyIsRequestScoped(t *testing.T) {
	payload := []byte(`{"type":"error","status":403,"headers":{"x-request-id":"req-safety"},"error":{"code":"permission-denied","message":"Content violates usage guidelines. Failed check: SAFETY_CHECK_TYPE_BIO"}}`)
	err, ok := parseXAIWebsocketError(payload)
	if !ok {
		t.Fatal("expected xAI websocket error")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusForbidden {
		t.Fatalf("status = %#v, want 403", err)
	}
	var requestScoped cliproxyexecutor.RequestScopedError
	if !errors.As(err, &requestScoped) || !requestScoped.IsRequestScoped() {
		t.Fatalf("error = %T, want request-scoped", err)
	}
}

func TestXAIWebsocketsExecuteStreamReturnsNormalizedResponseFailureAsError(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				conn, errUpgrade := upgrader.Upgrade(w, r, nil)
				if errUpgrade != nil {
					t.Errorf("upgrade websocket: %v", errUpgrade)
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, errRead := conn.ReadMessage(); errRead != nil {
					t.Errorf("read upstream websocket message: %v", errRead)
					return
				}
				payload := []byte(`{"type":"response.output_item.done","item":{"type":"function_call","name":"invalid.tool"}}`)
				if errWrite := conn.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
					t.Errorf("write upstream websocket message: %v", errWrite)
				}
			},
		),
	)
	defer server.Close()

	exec := newXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth-response-error",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "xai-response-error-session",
		},
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	result, errExecute := exec.ExecuteStream(
		ctx,
		auth,
		cliproxyexecutor.Request{Model: "grok-4.3", Payload: []byte(`{"model":"grok-4.3","input":"hello"}`)},
		opts,
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	defer exec.CloseExecutionSession("xai-response-error-session")

	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			t.Fatal("stream closed before terminal error")
		}
		if chunk.Err == nil {
			t.Fatalf("chunk = %#v, want terminal error", chunk)
		}
		if len(chunk.Payload) != 0 {
			t.Fatalf("error chunk payload = %s, want empty", chunk.Payload)
		}
		if got := statusErrCode(chunk.Err); got != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d", got, http.StatusBadGateway)
		}
		if !strings.Contains(chunk.Err.Error(), "invalid_tool_name") {
			t.Fatalf("error = %q, want invalid_tool_name", chunk.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for terminal error")
	}
}

func TestXAIWebsocketsExecuteStreamSendsResponseCreateWithPreviousResponseID(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/responses" {
					t.Errorf("path = %q, want /responses", r.URL.Path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer xai-token" {
					t.Errorf("Authorization = %q, want Bearer xai-token", got)
				}
				if got := r.Header.Get("x-grok-conv-id"); got != "execution-session-1" {
					t.Errorf("x-grok-conv-id = %q, want execution-session-1", got)
				}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade websocket: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()

				_, payload, errRead := conn.ReadMessage()
				if errRead != nil {
					t.Errorf("read upstream websocket message: %v", errRead)
					return
				}
				capturedPayload <- bytes.Clone(payload)
				completed := []byte(`{"type":"response.completed","response":{"id":"resp-xai-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
				if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
					t.Errorf("write completed websocket message: %v", errWrite)
				}
			},
		),
	)
	defer server.Close()

	exec := newXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}
	req := cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","stream":true,"previous_response_id":"resp-prev","instructions":"system prompt","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "execution-session-1",
		},
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	defer exec.CloseExecutionSession("execution-session-1")

	select {
	case payload := <-capturedPayload:
		if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
			t.Fatalf("type = %q, want response.create; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "resp-prev" {
			t.Fatalf("previous_response_id = %q, want resp-prev; payload=%s", got, payload)
		}
		if gjson.GetBytes(payload, "stream").Exists() {
			t.Fatalf("stream must be omitted for xAI websocket payload: %s", payload)
		}
		if gjson.GetBytes(payload, "instructions").Exists() {
			t.Fatalf("instructions must be omitted when previous_response_id is set: %s", payload)
		}
		if got := gjson.GetBytes(payload, "prompt_cache_key").String(); got != "execution-session-1" {
			t.Fatalf("prompt_cache_key = %q, want execution-session-1; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "store").Bool(); !got {
			t.Fatalf("store = false, want true; payload=%s", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}

	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			t.Fatal("stream closed before completed chunk")
		}
		if chunk.Err != nil {
			t.Fatalf("chunk error = %v", chunk.Err)
		}
		if got := gjson.GetBytes(bytes.TrimSpace(chunk.Payload), "type").String(); got != "response.completed" {
			t.Fatalf("chunk type = %q, want response.completed; payload=%s", got, chunk.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for completed chunk")
	}
}

func TestXAIWebsocketsExecuteStreamRewritesRepeatedResponseIDForDownstream(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPreviousIDs := make(chan string, 3)
	releaseServer := make(chan struct{})
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade websocket: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()

				for range 3 {
					_, payload, errRead := conn.ReadMessage()
					if errRead != nil {
						t.Errorf("read upstream websocket message: %v", errRead)
						return
					}
					previousID := gjson.GetBytes(payload, "previous_response_id").String()
					capturedPreviousIDs <- previousID
					completed := []byte(fmt.Sprintf(
						`{"type":"response.completed","response":{"id":"resp-real","previous_response_id":%q,"output":[{"id":"rs_resp-real","type":"reasoning","status":"completed"}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`,
						previousID,
					))
					if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
						t.Errorf("write completed websocket message: %v", errWrite)
						return
					}
				}
				<-releaseServer
			},
		),
	)
	defer server.Close()
	defer close(releaseServer)

	exec := newXAIWebsocketsExecutor(&config.Config{})
	exec.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	exec.idStore = &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
	defer exec.CloseExecutionSession("xai-id-map-session")
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth-id-map",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "xai-id-map-session",
		},
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	runRequest := func(previousID string) (string, string, string) {
		body := []byte(`{"model":"grok-4.3","input":[{"type":"message","role":"user","content":"hello"}]}`)
		if previousID != "" {
			body = []byte(fmt.Sprintf(
				`{"model":"grok-4.3","previous_response_id":%q,"input":[{"type":"function_call_output","call_id":"call-1","output":"ok"}]}`,
				previousID,
			))
		}
		result, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "grok-4.3", Payload: body}, opts)
		if err != nil {
			t.Fatalf("ExecuteStream() error = %v", err)
		}
		select {
		case chunk, ok := <-result.Chunks:
			if !ok {
				t.Fatal("stream closed before completed chunk")
			}
			if chunk.Err != nil {
				t.Fatalf("chunk error = %v", chunk.Err)
			}
			payload := bytes.TrimSpace(chunk.Payload)
			return gjson.GetBytes(payload, "response.id").String(),
				gjson.GetBytes(payload, "response.output.0.id").String(),
				gjson.GetBytes(payload, "response.previous_response_id").String()
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for completed chunk")
		}
		return "", "", ""
	}

	firstDownstreamID, firstOutputID, firstResponsePrevious := runRequest("")
	if firstDownstreamID != "resp-real" {
		t.Fatalf("first downstream id = %q, want resp-real", firstDownstreamID)
	}
	if firstOutputID != "rs_resp-real" {
		t.Fatalf("first output item id = %q, want rs_resp-real", firstOutputID)
	}
	if firstResponsePrevious != "" {
		t.Fatalf("first response previous_response_id = %q, want empty", firstResponsePrevious)
	}
	if firstUpstreamPrevious := <-capturedPreviousIDs; firstUpstreamPrevious != "" {
		t.Fatalf("first upstream previous_response_id = %q, want empty", firstUpstreamPrevious)
	}

	secondDownstreamID, secondOutputID, secondResponsePrevious := runRequest(firstDownstreamID)
	if secondDownstreamID == "" || secondDownstreamID == "resp-real" {
		t.Fatalf("second downstream id = %q, want synthetic id different from resp-real", secondDownstreamID)
	}
	if secondOutputID == "rs_resp-real" || !strings.Contains(secondOutputID, secondDownstreamID) {
		t.Fatalf("second output item id = %q, want rewritten id containing %q", secondOutputID, secondDownstreamID)
	}
	if secondResponsePrevious != firstDownstreamID {
		t.Fatalf("second response previous_response_id = %q, want %q", secondResponsePrevious, firstDownstreamID)
	}
	if secondUpstreamPrevious := <-capturedPreviousIDs; secondUpstreamPrevious != "resp-real" {
		t.Fatalf("second upstream previous_response_id = %q, want resp-real", secondUpstreamPrevious)
	}

	thirdDownstreamID, thirdOutputID, thirdResponsePrevious := runRequest(secondDownstreamID)
	if thirdDownstreamID == "" || thirdDownstreamID == "resp-real" || thirdDownstreamID == secondDownstreamID {
		t.Fatalf("third downstream id = %q, want a new synthetic id", thirdDownstreamID)
	}
	if thirdOutputID == "rs_resp-real" || !strings.Contains(thirdOutputID, thirdDownstreamID) {
		t.Fatalf("third output item id = %q, want rewritten id containing %q", thirdOutputID, thirdDownstreamID)
	}
	if thirdResponsePrevious != secondDownstreamID {
		t.Fatalf("third response previous_response_id = %q, want %q", thirdResponsePrevious, secondDownstreamID)
	}
	if thirdUpstreamPrevious := <-capturedPreviousIDs; thirdUpstreamPrevious != "resp-real" {
		t.Fatalf("third upstream previous_response_id = %q, want resp-real", thirdUpstreamPrevious)
	}
}

func TestXAIWebsocketsExecuteStreamCompactionTriggerUsesHTTPCompactWithRecordedContext(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedWebsocketPayload := make(chan []byte, 1)
	capturedCompactPayload := make(chan []byte, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/responses":
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Errorf("upgrade websocket: %v", err)
						return
					}
					defer func() { _ = conn.Close() }()

					_, payload, errRead := conn.ReadMessage()
					if errRead != nil {
						t.Errorf("read upstream websocket message: %v", errRead)
						return
					}
					capturedWebsocketPayload <- bytes.Clone(payload)
					completed := []byte(`{"type":"response.completed","response":{"id":"resp-real","output":[{"type":"message","id":"out-1","role":"assistant","content":"first answer"}],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
					if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
						t.Errorf("write completed websocket message: %v", errWrite)
					}
				case "/responses/compact":
					body, errRead := io.ReadAll(r.Body)
					if errRead != nil {
						t.Errorf("read compact body: %v", errRead)
						return
					}
					capturedCompactPayload <- bytes.Clone(body)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"resp_compact","model":"grok-4.3","output":[{"type":"compaction","encrypted_content":"opaque"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
				default:
					t.Errorf("path = %q, want /responses", r.URL.Path)
					http.Error(w, "unexpected path", http.StatusNotFound)
				}
			},
		),
	)
	defer server.Close()

	exec := newXAIWebsocketsExecutor(&config.Config{})
	exec.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	exec.idStore = &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
	defer exec.CloseExecutionSession("xai-compaction-session")
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth-compaction",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Stream:         true,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "xai-compaction-session",
		},
	}

	result, err := exec.ExecuteStream(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth, cliproxyexecutor.Request{
			Model:   "grok-4.3",
			Payload: []byte(`{"model":"grok-4.3","stream":true,"input":[{"type":"message","id":"msg-1","role":"user","content":"first"}]}`),
		}, opts,
	)
	if err != nil {
		t.Fatalf("ExecuteStream first turn error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	select {
	case payload := <-capturedWebsocketPayload:
		if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
			t.Fatalf("type = %q, want response.create; payload=%s", got, payload)
		}
		input := gjson.GetBytes(payload, "input")
		if !input.IsArray() || len(input.Array()) != 1 {
			t.Fatalf("input = %s, want one first-turn item", input.Raw)
		}
		if gjson.GetBytes(payload, "stream").Exists() {
			t.Fatalf("stream must be omitted for xAI websocket payload: %s", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}

	compactResult, err := exec.ExecuteStream(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth, cliproxyexecutor.Request{
			Model:   "grok-4.3",
			Payload: []byte(`{"model":"grok-4.3","stream":true,"previous_response_id":"resp-real","input":[{"type":"compaction_trigger"}]}`),
		}, opts,
	)
	if err != nil {
		t.Fatalf("ExecuteStream compaction trigger error: %v", err)
	}
	for chunk := range compactResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("compact stream chunk error = %v", chunk.Err)
		}
	}

	select {
	case payload := <-capturedCompactPayload:
		if xaiInputHasItemType(payload, "compaction_trigger") {
			t.Fatalf("compaction_trigger reached xai compact body: %s", payload)
		}
		input := gjson.GetBytes(payload, "input")
		if !input.IsArray() || len(input.Array()) != 2 {
			t.Fatalf("compact input = %s, want first request input plus response output", input.Raw)
		}
		if got := input.Array()[0].Get("id").String(); got != "msg-1" {
			t.Fatalf("compact input[0].id = %q, want msg-1; payload=%s", got, payload)
		}
		if got := input.Array()[1].Get("id").String(); got != "out-1" {
			t.Fatalf("compact input[1].id = %q, want out-1; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "" {
			t.Fatalf("compact previous_response_id = %q, want empty; payload=%s", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for compact HTTP payload")
	}
}

func TestXAIWebsocketIDStateConcurrentMappings(t *testing.T) {
	state := &xaiWebsocketIDState{downstreamToUpstream: make(map[string]string)}

	const mappings = 64
	var wg sync.WaitGroup
	errs := make(chan error, mappings)
	for index := range mappings {
		wg.Go(func() {
			upstreamID := fmt.Sprintf("resp-%d", index)
			downstreamID := state.downstreamIDForResponse(upstreamID, "", "")
			if downstreamID != upstreamID {
				errs <- fmt.Errorf("downstream ID = %q, want %q", downstreamID, upstreamID)
				return
			}
			if got := state.upstreamIDForDownstream(downstreamID); got != upstreamID {
				errs <- fmt.Errorf("upstream ID = %q, want %q", got, upstreamID)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestBuildXAIWebsocketRequestBodySetsStoreAndKeepsPromptCacheKey(t *testing.T) {
	body := []byte(`{"model":"grok-4.3","stream":true,"stream_options":{"include_usage":true},"background":true,"prompt_cache_key":"cache-1","previous_response_id":"resp-prev","instructions":"system prompt","input":[{"type":"message","role":"user","content":"hello"}]}`)

	payload := buildXAIWebsocketRequestBody(body)

	if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
		t.Fatalf("type = %q, want response.create; payload=%s", got, payload)
	}
	if gjson.GetBytes(payload, "stream").Exists() {
		t.Fatalf("stream must be omitted for xAI websocket payload: %s", payload)
	}
	if gjson.GetBytes(payload, "stream_options").Exists() {
		t.Fatalf("stream_options must be omitted for xAI websocket payload: %s", payload)
	}
	if gjson.GetBytes(payload, "background").Exists() {
		t.Fatalf("background must be omitted for xAI websocket payload: %s", payload)
	}
	if got := gjson.GetBytes(payload, "prompt_cache_key").String(); got != "cache-1" {
		t.Fatalf("prompt_cache_key = %q, want cache-1; payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "store").Bool(); !got {
		t.Fatalf("store = false, want true; payload=%s", payload)
	}
	if gjson.GetBytes(payload, "instructions").Exists() {
		t.Fatalf("instructions must be omitted when previous_response_id is set: %s", payload)
	}
}

func TestXAIWebsocketsExecuteStreamCompletesGenerateFalseWarmup(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	releaseServer := make(chan struct{})
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade websocket: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()

				_, payload, errRead := conn.ReadMessage()
				if errRead != nil {
					t.Errorf("read upstream websocket message: %v", errRead)
					return
				}
				capturedPayload <- bytes.Clone(payload)
				created := []byte(`{"type":"response.created","response":{"id":"resp-warmup-1","object":"response","status":"in_progress","output":[]}}`)
				if errWrite := conn.WriteMessage(websocket.TextMessage, created); errWrite != nil {
					t.Errorf("write created websocket message: %v", errWrite)
					return
				}
				<-releaseServer
			},
		),
	)
	defer server.Close()
	defer close(releaseServer)

	exec := newXAIWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "xai-auth-warmup",
		Provider: "xai",
		Attributes: map[string]string{
			"base_url":   server.URL,
			"websockets": "true",
		},
		Metadata: map[string]any{"access_token": "xai-token"},
	}
	req := cliproxyexecutor.Request{
		Model:   "grok-4.3",
		Payload: []byte(`{"model":"grok-4.3","generate":false,"input":[{"type":"message","role":"user","content":"warm up"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	select {
	case payload := <-capturedPayload:
		if got := gjson.GetBytes(payload, "generate").Bool(); got {
			t.Fatalf("generate = true, want false; payload=%s", payload)
		}
		if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
			t.Fatalf("type = %q, want response.create; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "store").Bool(); !got {
			t.Fatalf("store = false, want true; payload=%s", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}

	var gotTypes []string
	for {
		select {
		case chunk, ok := <-result.Chunks:
			if !ok {
				if len(gotTypes) != 2 {
					t.Fatalf("event types = %v, want response.created and response.completed", gotTypes)
				}
				return
			}
			if chunk.Err != nil {
				t.Fatalf("chunk error = %v", chunk.Err)
			}
			gotTypes = append(gotTypes, gjson.GetBytes(bytes.TrimSpace(chunk.Payload), "type").String())
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for warmup stream to close; event types so far: %v", gotTypes)
		}
	}
}

func TestXAIWebsocketsExecuteStreamAcceptsIncompleteWithoutReplacingReplay(t *testing.T) {
	internalcache.ClearXAIReasoningReplayCache()
	t.Cleanup(internalcache.ClearXAIReasoningReplayCache)

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	releaseServer := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read request: %v", errRead)
			return
		}
		for _, payload := range [][]byte{
			[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}}`),
			[]byte(`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","output":[],"usage":{"input_tokens":8,"output_tokens":1,"total_tokens":9}}}`),
		} {
			if errWrite := conn.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
				t.Errorf("write response: %v", errWrite)
				return
			}
		}
		<-releaseServer
	}))
	defer server.Close()
	defer close(releaseServer)

	ctx := cliproxyexecutor.WithDownstreamWebsocket(xaiC03ContextWithAPIKey("ws-incomplete-caller"))
	payload := []byte(`{"model":"grok-4.6","prompt_cache_key":"ws-incomplete-session","input":[{"role":"user","content":"hi"}]}`)
	req := cliproxyexecutor.Request{Model: "grok-4.6", Payload: payload}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "ws-incomplete-session",
		},
	}
	auth := &cliproxyauth.Auth{
		ID:         "xai-ws-incomplete-auth",
		Provider:   "xai",
		Attributes: map[string]string{"api_key": "offline-ws-token", "base_url": server.URL},
	}
	scope := xaiReasoningReplayScopeFromRequest(ctx, auth, opts.SourceFormat, req, opts, payload, server.URL)
	seedXAIReasoningReplay(t, scope)

	exec := newXAIWebsocketsExecutor(&config.Config{})
	t.Cleanup(func() { exec.CloseExecutionSession("ws-incomplete-session") })
	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var events [][]byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		events = append(events, bytes.Clone(chunk.Payload))
	}
	if len(events) != 2 || gjson.GetBytes(events[1], "type").String() != "response.incomplete" {
		t.Fatalf("events = %q, want output item and response.incomplete", events)
	}
	if got := gjson.GetBytes(events[1], "response.output.#").Int(); got != 1 {
		t.Fatalf("incomplete output length = %d, want 1; event=%s", got, events[1])
	}
	if _, ok := internalcache.GetXAIReasoningReplayItems(scope.modelName, scope.sessionKey); !ok {
		t.Fatal("response.incomplete replaced or deleted completed-only replay state")
	}
}

func TestConfigureXAIWebsocketConnPongDoesNotWaitForPayloadWrite(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverConn := make(chan *websocket.Conn, 1)
	pongReceived := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		conn.SetPongHandler(func(appData string) error {
			pongReceived <- appData
			return nil
		})
		serverConn <- conn
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	defer server.Close()

	clientConn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = clientConn.Close() }()
	upstreamConn := <-serverConn
	defer func() { _ = upstreamConn.Close() }()

	sess := &codexWebsocketSession{sessionID: "xai-pong-test"}
	configureXAIWebsocketConn(sess, clientConn)
	go func() {
		for {
			if _, _, errRead := clientConn.ReadMessage(); errRead != nil {
				return
			}
		}
	}()

	sess.writeMu.Lock()
	defer sess.writeMu.Unlock()
	if errPing := upstreamConn.WriteControl(
		websocket.PingMessage,
		[]byte("keepalive"),
		time.Now().Add(time.Second),
	); errPing != nil {
		t.Fatalf("write ping: %v", errPing)
	}
	select {
	case pong := <-pongReceived:
		if pong != "keepalive" {
			t.Fatalf("pong = %q, want keepalive", pong)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("pong waited for the payload write mutex")
	}
}

func TestBuildXAIWebsocketCompactionFallbackPayload(t *testing.T) {
	t.Run("recorded transcript", func(t *testing.T) {
		state := &xaiWebsocketIDState{transcriptInput: []json.RawMessage{
			json.RawMessage(`{"type":"message","id":"recorded","role":"user","content":"old"}`),
		}}
		payload, err := buildXAIWebsocketCompactionFallbackPayload(
			[]byte(`{"previous_response_id":"drop","input":[{"type":"compaction_trigger"}]}`),
			&xaiWebsocketRequestIDMapper{state: state},
		)
		if err != nil {
			t.Fatalf("build fallback error = %v", err)
		}
		if got := gjson.GetBytes(payload, "input.0.id").String(); got != "recorded" {
			t.Fatalf("recorded input id = %q; payload=%s", got, payload)
		}
		if gjson.GetBytes(payload, "previous_response_id").Exists() {
			t.Fatalf("previous_response_id remains with transcript: %s", payload)
		}
	})

	t.Run("request input", func(t *testing.T) {
		payload, err := buildXAIWebsocketCompactionFallbackPayload(
			[]byte(`{"previous_response_id":"drop","input":[{"type":"message","id":"request","role":"user","content":"new"},{"type":"compaction_trigger"}]}`),
			&xaiWebsocketRequestIDMapper{},
		)
		if err != nil {
			t.Fatalf("build fallback error = %v", err)
		}
		if got := gjson.GetBytes(payload, "input.0.id").String(); got != "request" {
			t.Fatalf("request input id = %q; payload=%s", got, payload)
		}
		if gjson.GetBytes(payload, "previous_response_id").Exists() {
			t.Fatalf("previous_response_id remains with request input: %s", payload)
		}
	})

	t.Run("previous response id", func(t *testing.T) {
		payload, err := buildXAIWebsocketCompactionFallbackPayload(
			[]byte(`{"previous_response_id":"downstream","input":[{"type":"compaction_trigger"}]}`),
			&xaiWebsocketRequestIDMapper{upstreamPreviousID: "upstream"},
		)
		if err != nil {
			t.Fatalf("build fallback error = %v", err)
		}
		if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "upstream" {
			t.Fatalf("previous_response_id = %q, want upstream; payload=%s", got, payload)
		}
		if xaiInputHasItemType(payload, "compaction_trigger") {
			t.Fatalf("compaction_trigger remains: %s", payload)
		}
	})

	t.Run("empty", func(t *testing.T) {
		_, err := buildXAIWebsocketCompactionFallbackPayload(
			[]byte(`{"input":[{"type":"compaction_trigger"}]}`),
			&xaiWebsocketRequestIDMapper{},
		)
		if err == nil {
			t.Fatal("build fallback error = nil")
		}
		status, ok := err.(interface{ StatusCode() int })
		if !ok || status.StatusCode() != http.StatusBadRequest {
			t.Fatalf("error = %#v, want status 400", err)
		}
	})
}

func TestXAIWebsocketDeliveryCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%t", canceled), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, err = conn.ReadMessage(); err != nil {
					return
				}
				created := `{"type":"response.created","response":{"id":"synthetic-response","status":"in_progress","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`
				if err = conn.WriteMessage(websocket.TextMessage, []byte(created)); err != nil {
					return
				}
				if !canceled {
					terminal := `{"type":"response.completed","response":{"id":"synthetic-response","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`
					if err = conn.WriteMessage(websocket.TextMessage, []byte(terminal)); err != nil {
						return
					}
				}
				for {
					if _, _, err = conn.ReadMessage(); err != nil {
						return
					}
				}
			}))
			t.Cleanup(server.Close)
			exec := newXAIWebsocketsExecutor(&config.Config{})
			exec.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
			exec.idStore = &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
			sessionID := t.Name()
			ctx, cancel := context.WithCancel(usage.WithRequestedModelAlias(t.Context(), t.Name()))
			observed, releaseObserver := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(releaseObserver) })
			t.Cleanup(func() {
				cancel()
				release()
				exec.CloseExecutionSession(sessionID)
			})
			plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
			registerTestUsagePlugin(t, plugin)
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "xai", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL}}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID}}
			if canceled {
				opts.WebSocketResponseObserver = func(_ context.Context, event cliproxyexecutor.WebSocketResponseEvent) {
					if event.EventType == "response.created" {
						close(observed)
						<-releaseObserver
					}
				}
			}
			stream, err := exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(ctx), auth, cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(`{"input":"synthetic prompt"}`)}, opts)
			if err != nil {
				t.Fatal(err)
			}
			if canceled {
				select {
				case <-observed:
				case <-time.After(3 * time.Second):
					t.Fatal("upstream event was not observed")
				}
				cancel()
				release()
			}
			for chunk := range stream.Chunks {
				if !canceled && chunk.Err != nil {
					t.Error(chunk.Err)
				}
				if chunk.Acknowledge != nil {
					chunk.Acknowledge(nil)
				}
			}
			sess := exec.store.getOrCreate(sessionID)
			sess.connMu.Lock()
			retained := sess.conn != nil
			sess.connMu.Unlock()
			if retained == canceled {
				t.Errorf("session retained=%t, want %t", retained, !canceled)
			}
			select {
			case record := <-plugin.records:
				if record.Alias != t.Name() || record.Failed != canceled || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 3 {
					t.Errorf("usage failed=%t tokens=%d/%d alias=%q, want failed=%t and 5/3", record.Failed, record.Detail.InputTokens, record.Detail.OutputTokens, record.Alias, canceled)
				}
			case <-time.After(time.Second):
				t.Error("usage record missing")
			}
			select {
			case record := <-plugin.records:
				t.Errorf("duplicate usage record: failed=%t", record.Failed)
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}

func TestXAIWebsocketDeliveryFinalOutcomes(t *testing.T) {
	for _, raw := range []bool{false, true} {
		for _, warmup := range []bool{false, true} {
			for _, mode := range []string{"cancel", "ack-error", "ack-success"} {
				t.Run(fmt.Sprintf("raw=%t/warmup=%t/%s", raw, warmup, mode), func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
						if err != nil {
							return
						}
						defer func() { _ = conn.Close() }()
						if _, _, err = conn.ReadMessage(); err != nil {
							return
						}
						created := `{"type":"response.created","response":{"id":"synthetic-boundary","status":"in_progress","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`
						if err = conn.WriteMessage(websocket.TextMessage, []byte(created)); err != nil {
							return
						}
						if !warmup {
							terminal := `{"type":"response.completed","response":{"id":"synthetic-boundary","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`
							if err = conn.WriteMessage(websocket.TextMessage, []byte(terminal)); err != nil {
								return
							}
						}
						for {
							if _, _, err = conn.ReadMessage(); err != nil {
								return
							}
						}
					}))
					t.Cleanup(server.Close)
					exec := newXAIWebsocketsExecutor(&config.Config{})
					exec.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
					exec.idStore = &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
					sessionID := t.Name()
					ctx, finish := xaiOutcomeContext(t)
					ctx, cancel := context.WithCancel(ctx)
					t.Cleanup(func() { cancel(); exec.CloseExecutionSession(sessionID) })
					if raw {
						ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
					}
					if mode != "cancel" {
						ctx = cliproxyexecutor.WithStreamDeliveryAcknowledgement(ctx)
					}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse,
						Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID}}
					if mode == "cancel" {
						opts.WebSocketResponseObserver = func(_ context.Context, event cliproxyexecutor.WebSocketResponseEvent) {
							if (!warmup && event.EventType == "response.completed") || (warmup && event.EventType == "response.created") {
								cancel()
							}
						}
					}
					request := []byte(fmt.Sprintf(`{"input":"synthetic boundary","generate":%t}`, !warmup))
					auth := &cliproxyauth.Auth{ID: sessionID, Provider: "xai", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL}}
					stream, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "grok-4.7", Payload: request}, opts)
					if err != nil {
						t.Fatal(err)
					}
					acknowledged := false
					for chunk := range stream.Chunks {
						if chunk.Err != nil && mode == "ack-success" {
							t.Error(chunk.Err)
						}
						if chunk.Acknowledge != nil {
							acknowledged = true
							if mode == "ack-error" {
								chunk.Acknowledge(errors.New("synthetic downstream write failed"))
							} else {
								chunk.Acknowledge(nil)
							}
						}
					}
					wantFailed := mode != "ack-success"
					if mode != "cancel" && !acknowledged {
						t.Error("delivery acknowledgement was not requested")
					}
					sess := exec.store.getOrCreate(sessionID)
					sess.connMu.Lock()
					retained := sess.conn != nil
					sess.connMu.Unlock()
					if retained == wantFailed {
						t.Errorf("session retained=%t, want %t", retained, !wantFailed)
					}
					records := finish()
					if len(records) != 1 {
						t.Fatalf("usage records=%d, want one", len(records))
					}
					record := records[0]
					if record.Failed != wantFailed || record.Completed == wantFailed || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 8 {
						t.Errorf("usage failed=%t completed=%t tokens=%d/%d/%d, want failed=%t and 5/3/8", record.Failed, record.Completed, record.Detail.InputTokens, record.Detail.OutputTokens, record.Detail.TotalTokens, wantFailed)
					}
				})
			}
		}
	}
}

func TestPayloadBarrierXAIWebsocketRetry(t *testing.T) {
	cfg := &config.Config{Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"store": false, "instructions": "configured"}}},
		Filter:   []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: []string{"input.0", "previous_response_id"}}},
	}}
	body := []byte(`{"previous_response_id":"previous","input":[{"content":"first"},{"content":"second"}]}`)
	req := cliproxyexecutor.Request{Model: "grok-4.3", Payload: body}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex}
	executor := newXAIWebsocketsExecutor(cfg)
	prepared, err := executor.prepareResponsesWebsocketRequest(context.Background(), &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://fixture.invalid"}}, req, opts)
	if err != nil {
		t.Fatal(err)
	}
	first := buildCodexWebsocketRequestBody(prepared.body)
	retry := buildCodexWebsocketRequestBody(prepared.body)
	if !bytes.Equal(first, retry) {
		t.Fatalf("retry changed configured body: %s != %s", first, retry)
	}
	if gjson.GetBytes(first, "store").Bool() || gjson.GetBytes(first, "instructions").String() != "configured" || gjson.GetBytes(first, "previous_response_id").Exists() || gjson.GetBytes(first, "input.#").Int() != 1 {
		t.Fatalf("invalid final payload: %s", first)
	}
	if gjson.GetBytes(first, "type").String() != "response.create" {
		t.Fatalf("missing frame type: %s", first)
	}
}
