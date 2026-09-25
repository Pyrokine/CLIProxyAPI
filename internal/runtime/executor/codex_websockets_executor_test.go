package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestPreserveCodexPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[{"type":"function_call_output","call_id":"call-1"}]}`)
	requestPayload := []byte(`{"previous_response_id":"resp-1"}`)

	restored, err := preserveCodexPreviousResponseID(body, requestPayload)
	if err != nil {
		t.Fatalf("preserveCodexPreviousResponseID() error = %v", err)
	}

	if got := gjson.GetBytes(restored, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %s, want resp-1", got)
	}
	if gjson.GetBytes(body, "previous_response_id").Exists() {
		t.Fatalf("input body was mutated: %s", body)
	}
}

func TestPreserveCodexPreviousResponseIDRejectsInvalidBody(t *testing.T) {
	_, err := preserveCodexPreviousResponseID([]byte(`{"model":`), []byte(`{"previous_response_id":"resp-1"}`))
	if err == nil {
		t.Fatal("preserveCodexPreviousResponseID() error = nil, want malformed body error")
	}
}

func TestCodexResponsesBodyPreservesPreviousResponseIDForHTTPFallback(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		prepared, err := codexResponsesBody(
			context.Background(),
			&config.Config{},
			&cliproxyauth.Auth{},
			"gpt-5-codex",
			"gpt-5-codex",
			"codex",
			sdktranslator.FormatOpenAIResponse,
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
			cliproxyexecutor.Request{
				Model: "gpt-5-codex",
				Payload: []byte(
					`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","role":"user","content":"next"}]}`,
				),
			},
			streaming,
		)
		if err != nil {
			t.Fatalf("codexResponsesBody(streaming=%t) error = %v", streaming, err)
		}
		if got := gjson.GetBytes(prepared.Body, "previous_response_id").String(); got != "resp-1" {
			t.Fatalf("codexResponsesBody(streaming=%t) previous_response_id = %q", streaming, got)
		}
	}
}

func TestBuildCodexWebsocketRequestBodyPreservesPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`)

	wsReqBody := buildCodexWebsocketRequestBody(body)

	if got := gjson.GetBytes(wsReqBody, "type").String(); got != "response.create" {
		t.Fatalf("type = %s, want response.create", got)
	}
	if got := gjson.GetBytes(wsReqBody, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %s, want resp-1", got)
	}
	if gjson.GetBytes(wsReqBody, "input.0.id").String() != "msg-1" {
		t.Fatalf("input item id mismatch")
	}
	if got := gjson.GetBytes(wsReqBody, "type").String(); got == "response.append" {
		t.Fatalf("unexpected websocket request type: %s", got)
	}
}

func TestCodexWebsocketsExecuteStreamRejectsUpgradeRequiredWithoutHTTPFallback(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
					t.Errorf("unexpected HTTP fallback request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(http.StatusUpgradeRequired)
			},
		),
	)
	defer server.Close()

	exec := newCodexWebsocketsExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
		&cliproxyauth.Auth{
			ID:         "auth-upgrade-required",
			Provider:   "codex",
			Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
		},
		cliproxyexecutor.Request{
			Model: "gpt-5-codex",
			Payload: []byte(
				`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"next"}]}`,
			),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
	)
	if stream != nil {
		t.Fatalf("ExecuteStream() result = %#v, want nil", stream)
	}
	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok || statusErr.StatusCode() != http.StatusUpgradeRequired {
		t.Fatalf("ExecuteStream() error = %T %v, want status 426", err, err)
	}
}

func TestCodexWebsocketSessionRejectsStaleActiveConnection(t *testing.T) {
	sess := &codexWebsocketSession{sessionID: "session-1"}
	connA := &websocket.Conn{}
	connB := &websocket.Conn{}
	readCh := make(chan codexWebsocketRead, 1)

	sess.setActive(connA, readCh)
	sess.setActive(connB, readCh)
	sess.clearActive(connA, readCh)

	if _, _, ok := sess.activeForConn(connA); ok {
		t.Fatal("stale connection remained active")
	}
	if gotCh, _, ok := sess.activeForConn(connB); !ok || gotCh != readCh {
		t.Fatal("current connection was cleared by stale connection")
	}
}

func TestDeliverCodexWebsocketTerminalReadWaitsForConsumer(t *testing.T) {
	readCh := make(chan codexWebsocketRead, 1)
	readCh <- codexWebsocketRead{payload: []byte("queued")}
	done := make(chan struct{})
	delivered := make(chan struct{})
	terminalErr := errors.New("upstream disconnected")

	go func() {
		deliverCodexWebsocketTerminalRead(readCh, done, codexWebsocketRead{err: terminalErr})
		close(delivered)
	}()

	select {
	case <-delivered:
		t.Fatal("terminal read returned before the full channel had space")
	case <-time.After(50 * time.Millisecond):
	}

	<-readCh
	select {
	case event := <-readCh:
		if !errors.Is(event.err, terminalErr) {
			t.Fatalf("terminal event error = %v, want %v", event.err, terminalErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for terminal websocket error")
	}
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("terminal delivery did not return after the consumer drained the channel")
	}
}

func TestExistingCodexWebsocketSessionRejectsRotatedCredential(t *testing.T) {
	conn := &websocket.Conn{}
	sess := &codexWebsocketSession{
		conn:             conn,
		authID:           "auth-a",
		wsURL:            "wss://example.com/responses",
		credentialSHA256: "credential-a",
	}
	if got := existingCodexWebsocketSessionConn(sess, sess.authID, sess.wsURL, "credential-a"); got != conn {
		t.Fatal("matching credential did not reuse the connection")
	}
	if got := existingCodexWebsocketSessionConn(sess, sess.authID, sess.wsURL, "credential-b"); got != nil {
		t.Fatal("rotated credential reused the old connection")
	}
}

func TestEnsureCodexUpstreamConnReconnectsWhenIdentityChanges(t *testing.T) {
	newServer := func(connected chan<- struct{}) *httptest.Server {
		return httptest.NewServer(
			http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(
						w, r, nil,
					)
					if err != nil {
						return
					}
					connected <- struct{}{}
					defer func() { _ = conn.Close() }()
					for {
						if _, _, errRead := conn.ReadMessage(); errRead != nil {
							return
						}
					}
				},
			),
		)
	}

	for _, tt := range []struct {
		name              string
		secondAuthID      string
		secondAccessToken string
		useSecondURL      bool
	}{
		{name: "auth", secondAuthID: "auth-b", secondAccessToken: "token-a"},
		{name: "credential", secondAuthID: "auth-a", secondAccessToken: "token-b"},
		{name: "upstream URL", secondAuthID: "auth-a", secondAccessToken: "token-a", useSecondURL: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			connectedA := make(chan struct{}, 2)
			connectedB := make(chan struct{}, 1)
			serverA := newServer(connectedA)
			t.Cleanup(serverA.Close)
			serverB := newServer(connectedB)
			t.Cleanup(serverB.Close)

			exec := newCodexWebsocketsExecutor(&config.Config{})
			const sessionID = "session-1"
			t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
			sess := exec.getOrCreateSession(sessionID)
			if sess == nil {
				t.Fatal("websocket session is nil")
			}
			wsURLA := "ws" + strings.TrimPrefix(serverA.URL, "http")
			wsURLB := wsURLA
			secondConnected := connectedA
			if tt.useSecondURL {
				wsURLB = "ws" + strings.TrimPrefix(serverB.URL, "http")
				secondConnected = connectedB
			}
			firstAuth := &cliproxyauth.Auth{
				ID:       "auth-a",
				Metadata: map[string]any{"access_token": "token-a"},
			}
			secondAuth := &cliproxyauth.Auth{
				ID:       tt.secondAuthID,
				Metadata: map[string]any{"access_token": tt.secondAccessToken},
			}
			connA, _, errFirst := exec.ensureUpstreamConn(
				context.Background(), firstAuth, sess, firstAuth.ID, wsURLA, http.Header{},
			)
			if errFirst != nil {
				t.Fatalf("first ensureUpstreamConn() error = %v", errFirst)
			}
			<-connectedA
			sess.setMultiAgentV2Optimized(connA, true)
			if !sess.isMultiAgentV2Optimized(connA) {
				t.Fatal("optimized state was not recorded for first connection")
			}

			connB, _, errSecond := exec.ensureUpstreamConn(
				context.Background(), secondAuth, sess, secondAuth.ID, wsURLB, http.Header{},
			)
			if errSecond != nil {
				t.Fatalf("second ensureUpstreamConn() error = %v", errSecond)
			}
			<-secondConnected

			if connA == connB {
				t.Fatal("connection was reused after identity changed")
			}
			if sess.isMultiAgentV2Optimized(connB) {
				t.Fatal("replacement connection inherited optimized state")
			}
			sess.connMu.Lock()
			gotAuthID := sess.authID
			gotWSURL := sess.wsURL
			gotCredential := sess.credentialSHA256
			sess.connMu.Unlock()
			if gotAuthID != secondAuth.ID || gotWSURL != wsURLB || gotCredential != cliproxyauth.AccessTokenSHA256(secondAuth) {
				t.Fatalf(
					"session identity = %q %q %q, want %q %q %q",
					gotAuthID,
					gotWSURL,
					gotCredential,
					secondAuth.ID,
					wsURLB,
					cliproxyauth.AccessTokenSHA256(secondAuth),
				)
			}
		})
	}
}

func TestApplyCodexWebsocketHeadersDefaultsToCurrentResponsesBeta(t *testing.T) {
	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, nil, "", nil, false)

	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %s, want %s", got, codexUserAgent)
	}
	if got := headers.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %s, want %s", got, codexOriginator)
	}
}

func TestApplyCodexWebsocketHeadersUsesConfiguredOAuthDefaults(t *testing.T) {
	cfg := &config.Config{
		Codex: config.CodexConfig{DisableCodexCloaking: true},
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "codex_cli_rs/0.132.0 (Linux; x64)",
			BetaFeatures: "feature-a,feature-b",
		},
	}

	headers := applyCodexWebsocketHeaders(
		context.Background(), http.Header{}, &cliproxyauth.Auth{Metadata: map[string]any{"account_id": "acct-1"}}, "",
		cfg, false,
	)

	if got := headers.Get("User-Agent"); got != cfg.CodexHeaderDefaults.UserAgent {
		t.Fatalf("User-Agent = %q, want configured default", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != cfg.CodexHeaderDefaults.BetaFeatures {
		t.Fatalf("x-codex-beta-features = %q, want configured default", got)
	}
	if got := headers.Get("Session-Id"); got != "" {
		t.Fatalf("Session-Id = %q, want empty for non-Mac configured UA", got)
	}
	if got := headers.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty without trusted client version", got)
	}
}

func TestApplyCodexWebsocketHeadersRejectsCherryClientIdentity(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ginCtx.Request.Header.Set("User-Agent", "Mozilla/5.0 CherryStudio/1.9.6 Electron/41.2.1")
	ginCtx.Request.Header.Set("Version", "cherry-version")
	ginCtx.Request.Header.Set("Session_id", "cherry-session")

	headers := applyCodexWebsocketHeaders(
		util.WithGinContext(context.Background(), ginCtx), http.Header{}, nil, "", nil, false,
	)

	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, codexUserAgent)
	}
	if got := headers.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty for non-Codex client", got)
	}
	if got := headers.Get("Session-Id"); got == "" || got == "cherry-session" {
		t.Fatalf("Session-Id = %q, want generated non-Cherry value", got)
	}
}

func TestApplyCodexWebsocketHeadersPreservesCodexClientIdentity(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ginCtx.Request.Header.Set("User-Agent", "codex_cli_rs/0.131.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9")
	ginCtx.Request.Header.Set("Version", "0.131.0")
	ginCtx.Request.Header.Set("Session_id", "codex-session")

	headers := applyCodexWebsocketHeaders(
		util.WithGinContext(context.Background(), ginCtx), http.Header{}, nil, "", nil, false,
	)

	if got := headers.Get("User-Agent"); got != ginCtx.Request.Header.Get("User-Agent") {
		t.Fatalf("User-Agent = %q, want Codex client value", got)
	}
	if got := headers.Get("Version"); got != "0.131.0" {
		t.Fatalf("Version = %q, want Codex client value", got)
	}
	if got := headers.Get("Session-Id"); got != "codex-session" {
		t.Fatalf("Session-Id = %q, want Codex client value", got)
	}
}

func TestCodexWebsocketsMultiAgentV2StateTransitionsOnPersistentConnection(t *testing.T) {
	requests := make(chan []byte, 6)
	var connectionCount atomic.Int32
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, errUpgrade := upgrader.Upgrade(w, r, nil)
				if errUpgrade != nil {
					t.Errorf("Upgrade() error = %v", errUpgrade)
					return
				}
				connectionCount.Add(1)
				defer func() { _ = conn.Close() }()

				for range 6 {
					_, payload, errRead := conn.ReadMessage()
					if errRead != nil {
						t.Errorf("ReadMessage() error = %v", errRead)
						return
					}
					requests <- bytes.Clone(payload)
					if errWrite := conn.WriteMessage(
						websocket.TextMessage,
						[]byte(`{"type":"response.done","response":{"id":"resp-1","status":"completed","output":[{"type":"function_call","id":"call-1","name":"collaboration-optimize__spawn_agent","namespace":"collaboration-optimize","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`),
					); errWrite != nil {
						t.Errorf("WriteMessage() error = %v", errWrite)
						return
					}
				}
			},
		),
	)
	t.Cleanup(server.Close)

	cfg := &config.Config{
		DisableImageGeneration: config.DisableImageGenerationAll,
		Codex: config.CodexConfig{
			DisableCodexCloaking: true,
			OptimizeMultiAgentV2: true,
		},
	}
	exec := newCodexWebsocketsExecutor(cfg)
	const sessionID = "multi-agent-v2-state"
	t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
	auth := &cliproxyauth.Auth{
		ID:       "multi-agent-v2-auth",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "test-token",
			"base_url": server.URL,
		},
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Stream:         true,
		Headers: http.Header{
			"User-Agent": []string{"codex_cli_rs/0.154.0"},
			"x-openai-internal-codex-responses-lite": []string{
				" TrUe ",
			},
		},
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: sessionID,
		},
	}
	eligiblePayload := []byte(`{"model":"gpt-5.5","input":"hello","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","description":"Spawn an agent.","parameters":{"type":"object","properties":{"message":{"type":"string"}}}}]}]}`)
	incrementalPayload := []byte(`{"model":"gpt-5.5","input":"next"}`)
	conflictPayload := []byte(`{"model":"gpt-5.5","input":"conflict","tools":[{"type":"namespace","name":"collaboration-optimize","tools":[]}]}`)
	turns := []struct {
		name                    string
		payload                 []byte
		wantUpstreamNamespace   string
		wantDownstreamNamespace string
	}{
		{
			name:                    "eligible enables state",
			payload:                 eligiblePayload,
			wantUpstreamNamespace:   "collaboration-optimize",
			wantDownstreamNamespace: "collaboration",
		},
		{
			name:                    "incremental inherits state",
			payload:                 incrementalPayload,
			wantDownstreamNamespace: "collaboration",
		},
		{
			name:                    "reserved namespace conflict clears state",
			payload:                 conflictPayload,
			wantUpstreamNamespace:   "collaboration-optimize",
			wantDownstreamNamespace: "collaboration-optimize",
		},
		{
			name:                    "incremental after conflict remains inactive",
			payload:                 incrementalPayload,
			wantDownstreamNamespace: "collaboration-optimize",
		},
		{
			name:                    "eligible re-enables state",
			payload:                 eligiblePayload,
			wantUpstreamNamespace:   "collaboration-optimize",
			wantDownstreamNamespace: "collaboration",
		},
		{
			name:                    "incremental inherits re-enabled state",
			payload:                 incrementalPayload,
			wantDownstreamNamespace: "collaboration",
		},
	}

	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	for _, turn := range turns {
		stream, errStream := exec.ExecuteStream(
			ctx,
			auth,
			cliproxyexecutor.Request{
				Model:   "gpt-5.5",
				Payload: turn.payload,
			},
			opts,
		)
		if errStream != nil {
			t.Fatalf("%s: ExecuteStream() error = %v", turn.name, errStream)
		}

		var terminal []byte
		for chunk := range stream.Chunks {
			if chunk.Err != nil {
				t.Fatalf("%s: stream chunk error = %v", turn.name, chunk.Err)
			}
			if gjson.GetBytes(chunk.Payload, "type").String() == "response.done" {
				terminal = bytes.Clone(chunk.Payload)
			}
			if chunk.Acknowledge != nil {
				chunk.Acknowledge(nil)
			}
		}
		if len(terminal) == 0 {
			t.Fatalf("%s: missing response.done event", turn.name)
		}

		var upstream []byte
		select {
		case upstream = <-requests:
		case <-time.After(time.Second):
			t.Fatalf("%s: timed out waiting for upstream request", turn.name)
		}
		if got := gjson.GetBytes(upstream, "type").String(); got != "response.create" {
			t.Fatalf("%s: upstream type = %q, want response.create; request=%s", turn.name, got, upstream)
		}
		if got := gjson.GetBytes(upstream, "tools.0.name").String(); got != turn.wantUpstreamNamespace {
			t.Fatalf(
				"%s: upstream namespace = %q, want %q; request=%s",
				turn.name,
				got,
				turn.wantUpstreamNamespace,
				upstream,
			)
		}
		if got := gjson.GetBytes(terminal, "response.output.0.namespace").String(); got != turn.wantDownstreamNamespace {
			t.Fatalf(
				"%s: downstream namespace = %q, want %q; response=%s",
				turn.name,
				got,
				turn.wantDownstreamNamespace,
				terminal,
			)
		}
	}

	if got := connectionCount.Load(); got != 1 {
		t.Fatalf("upstream connection count = %d, want 1", got)
	}
}

func TestCodexWebsocketsCancellationKeepsMultiAgentV2StateForLiveConnection(t *testing.T) {
	requestReceived := make(chan struct{})
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, errUpgrade := upgrader.Upgrade(w, r, nil)
				if errUpgrade != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, errRead := conn.ReadMessage(); errRead != nil {
					return
				}
				close(requestReceived)
				for {
					if _, _, errRead := conn.ReadMessage(); errRead != nil {
						return
					}
				}
			},
		),
	)
	t.Cleanup(server.Close)

	cfg := &config.Config{
		DisableImageGeneration: config.DisableImageGenerationAll,
		Codex: config.CodexConfig{
			DisableCodexCloaking: true,
			OptimizeMultiAgentV2: true,
		},
	}
	exec := newCodexWebsocketsExecutor(cfg)
	const sessionID = "multi-agent-v2-cancel"
	t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
	ctx, cancel := context.WithCancel(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
	)
	stream, errStream := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{
			ID:       "multi-agent-v2-cancel-auth",
			Provider: "codex",
			Attributes: map[string]string{
				"api_key":  "test-token",
				"base_url": server.URL,
			},
		},
		cliproxyexecutor.Request{
			Model: "gpt-5.5",
			Payload: []byte(
				`{"model":"gpt-5.5","input":"hello","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{}}}]}]}`,
			),
		},
		cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatOpenAIResponse,
			ResponseFormat: sdktranslator.FormatOpenAIResponse,
			Stream:         true,
			Headers: http.Header{
				"User-Agent": []string{"codex_cli_rs/0.154.0"},
			},
			Metadata: map[string]any{
				cliproxyexecutor.ExecutionSessionMetadataKey: sessionID,
			},
		},
	)
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	select {
	case <-requestReceived:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}
	cancel()
	for chunk := range stream.Chunks {
		if chunk.Err != nil && !errors.Is(chunk.Err, context.Canceled) {
			t.Fatalf("stream chunk error = %v, want context cancellation", chunk.Err)
		}
	}

	exec.sessMu.Lock()
	sess := exec.sessions[sessionID]
	exec.sessMu.Unlock()
	if sess == nil {
		t.Fatal("cancellation removed the execution session")
	}
	sess.connMu.Lock()
	conn := sess.conn
	sess.connMu.Unlock()
	if conn == nil {
		t.Fatal("cancellation invalidated the live upstream connection")
	}
	if !sess.isMultiAgentV2Optimized(conn) {
		t.Fatal("cancellation cleared connection-scoped optimized state")
	}
}

func TestApplyCodexWebsocketHeadersStrictNativeSessionThreadMatrix(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: true}}
	for _, tt := range []struct {
		name        string
		sessionID   string
		threadID    string
		wantSession bool
		wantThread  bool
	}{
		{name: "neither"},
		{name: "session only", sessionID: "session-1", wantSession: true},
		{name: "thread only", threadID: "thread-1", wantThread: true},
		{name: "session and thread", sessionID: "session-1", threadID: "thread-1", wantSession: true, wantThread: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clientHeaders := http.Header{
				"User-Agent":         []string{"codex_cli_rs/0.154.0"},
				"Conversation_id":    []string{"conversation-1"},
				"X-Codex-Turn-State": []string{"turn-state-1"},
			}
			if tt.sessionID != "" {
				clientHeaders["session_id"] = []string{tt.sessionID}
			}
			if tt.threadID != "" {
				clientHeaders["Thread-Id"] = []string{tt.threadID}
			}

			headers := applyCodexWebsocketHeaders(
				context.Background(), http.Header{},
				&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-token"}},
				"test-token", cfg, true, clientHeaders,
			)
			if got := headers.Get("Session-Id"); got != tt.sessionID {
				t.Fatalf("Session-Id = %q, want %q", got, tt.sessionID)
			}
			if got := headers.Get("Thread-Id"); got != tt.threadID {
				t.Fatalf("Thread-Id = %q, want %q", got, tt.threadID)
			}
			if (headers.Get("Session-Id") != "") != tt.wantSession {
				t.Fatalf("Session-Id presence = %t, want %t", headers.Get("Session-Id") != "", tt.wantSession)
			}
			if (headers.Get("Thread-Id") != "") != tt.wantThread {
				t.Fatalf("Thread-Id presence = %t, want %t", headers.Get("Thread-Id") != "", tt.wantThread)
			}
			if got := headers.Get("Conversation_id"); got != "conversation-1" {
				t.Fatalf("Conversation_id = %q, want conversation-1", got)
			}
			if got := headers.Get("X-Codex-Turn-State"); got != "turn-state-1" {
				t.Fatalf("X-Codex-Turn-State = %q, want turn-state-1", got)
			}
			for key := range headers {
				if strings.EqualFold(key, "Session-Id") || strings.EqualFold(key, "Session_id") {
					if key != "Session-Id" {
						t.Fatalf("session header key = %q, want exact Session-Id; headers=%#v", key, headers)
					}
				}
			}
		})
	}
}

func TestCodexHeadersUseExplicitDynamicHeaderSource(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("X-Client-Value", "from-gin")
	ctx := util.WithSessionID(util.WithGinContext(context.Background(), ginCtx), "codex-session")
	explicitHeaders := http.Header{
		"x-cLiEnT-vAlUe":     []string{"from-options"},
		"User-Agent":         []string{"codex_cli_rs/0.200.0 (Linux; x64)"},
		"Version":            []string{"0.200.0"},
		"Session_id":         []string{"native-session"},
		"Originator":         []string{"codex_cli_rs"},
		"X-Codex-Turn-State": []string{"turn-state-1"},
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":               "test-token",
		"header:X-Copied":       "$X-CLIENT-VALUE",
		"header:X-CPA-Session":  "$CPA-SESSION-ID",
		"header:Content-Length": "999",
	}}
	cfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: true}}

	httpReq := httptest.NewRequest(http.MethodPost, "https://example.com/responses", nil).WithContext(ctx)
	applyCodexHeaders(httpReq, auth, "test-token", true, cfg, explicitHeaders)
	if got := httpReq.Header.Get("X-Copied"); got != "from-options" {
		t.Fatalf("HTTP X-Copied = %q, want from-options", got)
	}
	if got := httpReq.Header.Get("X-CPA-Session"); got != "codex-session" {
		t.Fatalf("HTTP X-CPA-Session = %q, want codex-session", got)
	}
	if got := httpReq.Header.Get("Content-Length"); got != "" {
		t.Fatalf("HTTP Content-Length = %q, want empty", got)
	}
	if got := httpReq.Header.Get("Session-Id"); got != "native-session" {
		t.Fatalf("HTTP Session-Id = %q, want native-session", got)
	}
	if got := httpReq.Header.Get("X-Codex-Turn-State"); got != "turn-state-1" {
		t.Fatalf("HTTP X-Codex-Turn-State = %q, want turn-state-1", got)
	}
	if got := httpReq.Header.Get("Version"); got != "0.200.0" {
		t.Fatalf("HTTP Version = %q, want trusted Codex client value", got)
	}
	if got := httpReq.Header.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("HTTP Originator = %q, want trusted Codex client value", got)
	}

	websocketHeaders := applyCodexWebsocketHeaders(
		ctx, http.Header{}, auth, "test-token", cfg, true, explicitHeaders,
	)
	if got := websocketHeaders.Get("X-Copied"); got != "from-options" {
		t.Fatalf("WebSocket X-Copied = %q, want from-options", got)
	}
	if got := websocketHeaders.Get("X-CPA-Session"); got != "codex-session" {
		t.Fatalf("WebSocket X-CPA-Session = %q, want codex-session", got)
	}
	if got := websocketHeaders.Get("Content-Length"); got != "" {
		t.Fatalf("WebSocket Content-Length = %q, want empty", got)
	}
	if got := websocketHeaders.Get("Session-Id"); got != "native-session" {
		t.Fatalf("WebSocket Session-Id = %q, want native-session", got)
	}
	if got := websocketHeaders.Get("X-Codex-Turn-State"); got != "turn-state-1" {
		t.Fatalf("WebSocket X-Codex-Turn-State = %q, want turn-state-1", got)
	}
	if got := websocketHeaders.Get("Version"); got != "0.200.0" {
		t.Fatalf("WebSocket Version = %q, want trusted Codex client value", got)
	}
	if got := websocketHeaders.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("WebSocket Originator = %q, want trusted Codex client value", got)
	}
}

func TestCodexHeadersRejectExplicitNonCodexIdentity(t *testing.T) {
	ctx := util.WithSessionID(context.Background(), "codex-session")
	explicitHeaders := http.Header{
		"X-Client-Value":     []string{"from-options"},
		"User-Agent":         []string{"claude-cli/2.1.278 (external, cli)"},
		"Version":            []string{"0.154.0"},
		"Session_id":         []string{"claude-session"},
		"Originator":         []string{"claude-code"},
		"X-Codex-Turn-State": []string{"untrusted-turn-state"},
	}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":         "test-token",
		"header:X-Copied": "$X-CLIENT-VALUE",
	}}
	cfg := &config.Config{Codex: config.CodexConfig{DisableCodexCloaking: true}}

	httpReq := httptest.NewRequest(http.MethodPost, "https://example.com/responses", nil).WithContext(ctx)
	applyCodexHeaders(httpReq, auth, "test-token", true, cfg, explicitHeaders)
	websocketHeaders := applyCodexWebsocketHeaders(
		ctx, http.Header{}, auth, "test-token", cfg, true, explicitHeaders,
	)

	for _, testCase := range []struct {
		name    string
		headers http.Header
	}{
		{name: "HTTP", headers: httpReq.Header},
		{name: "WebSocket", headers: websocketHeaders},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := testCase.headers.Get("Version"); got != "" {
				t.Fatalf("Version = %q, want empty for non-Codex client", got)
			}
			if got := testCase.headers.Get("User-Agent"); got != codexUserAgent {
				t.Fatalf("User-Agent = %q, want %q", got, codexUserAgent)
			}
			if got := testCase.headers.Get("Session-Id"); got == "claude-session" {
				t.Fatalf("Session-Id = %q, want non-Claude value", got)
			}
			if got := testCase.headers.Get("Originator"); got != "" {
				t.Fatalf("Originator = %q, want empty for API key request", got)
			}
			if got := testCase.headers.Get("X-Codex-Turn-State"); got != "" {
				t.Fatalf("X-Codex-Turn-State = %q, want empty for non-Codex client", got)
			}
			if got := testCase.headers.Get("X-Copied"); got != "from-options" {
				t.Fatalf("X-Copied = %q, want custom header source value", got)
			}
			if got := testCase.headers.Get("Authorization"); got != "Bearer test-token" {
				t.Fatalf("Authorization = %q, want Bearer test-token", got)
			}
		})
	}
}

func TestApplyCodexWebsocketHeadersGPT56ModelOverrideDoesNotAddVersion(t *testing.T) {
	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, nil, "", nil, false)

	applyModelHeaderOverrides(headers, "gpt-5.6-sol")

	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, codexUserAgent)
	}
	if got := headers.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty without trusted client version", got)
	}
	if got := headers.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %q, want %q", got, codexOriginator)
	}
}

func TestApplyCodexWebsocketHeadersUsesCanonicalAccountHeader(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acct-1"}}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", nil, false)

	accountHeader := "ChatGPT-Account-ID"
	if got := headerValueCaseInsensitive(headers, accountHeader); got != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID = %q, want acct-1", got)
	}
	values, ok := headers[accountHeader]
	if !ok {
		t.Fatalf("expected exact ChatGPT-Account-ID key, got %#v", headers)
	}
	if len(values) != 1 || values[0] != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID values = %#v, want [acct-1]", values)
	}
}

func TestApplyCodexPromptCacheHeadersSetsSessionAndConversation(t *testing.T) {
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"prompt_cache_key":"cache-1"}`)}

	_, headers := applyCodexPromptCacheHeaders(
		sdktranslator.FormatOpenAIResponse, req, []byte(`{"model":"gpt-5-codex"}`),
	)

	if got := headers.Get("Session-Id"); got != "cache-1" {
		t.Fatalf("Session-Id = %q, want cache-1", got)
	}
	if got := headers.Get("Conversation_id"); got != "cache-1" {
		t.Fatalf("Conversation_id = %q, want cache-1", got)
	}
}

func TestApplyCodexPromptCacheHeadersClaudeGPT5UsesStableCache(t *testing.T) {
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"metadata":{"user_id":"{\"session_id\":\"claude-ws-session-1\"}"}}`),
	}

	body, headers := applyCodexPromptCacheHeaders(
		sdktranslator.FormatClaude, req, []byte(`{"model":"gpt-5.5"}`),
	)

	gotKey := gjson.GetBytes(body, "prompt_cache_key").String()
	if gotKey == "" {
		t.Fatalf("expected prompt_cache_key for gpt-5.5 Claude websocket path")
	}
	if got := headers.Get("Session-Id"); got != gotKey {
		t.Fatalf("Session-Id = %q, want %q", got, gotKey)
	}
	if got := headers.Get("Conversation_id"); got != gotKey {
		t.Fatalf("Conversation_id = %q, want %q", got, gotKey)
	}

	body2, _ := applyCodexPromptCacheHeaders(
		sdktranslator.FormatClaude, req, []byte(`{"model":"gpt-5.5"}`),
	)
	if gotKey2 := gjson.GetBytes(body2, "prompt_cache_key").String(); gotKey2 != gotKey {
		t.Fatalf("prompt_cache_key (second call) = %q, want %q", gotKey2, gotKey)
	}
}

func TestApplyCodexWebsocketHeadersKeepsPromptCacheSessionOverCodexClientSession(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ginCtx.Request.Header.Set("User-Agent", "codex_cli_rs/0.131.0 (Mac OS 26.3.1; arm64) iTerm.app/3.6.9")
	ginCtx.Request.Header.Set("Version", "0.131.0")
	ginCtx.Request.Header.Set("Session_id", "codex-client-session")

	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"prompt_cache_key":"cache-1"}`)}
	_, headers := applyCodexPromptCacheHeaders(
		sdktranslator.FormatOpenAIResponse, req, []byte(`{"model":"gpt-5-codex"}`),
	)

	headers = applyCodexWebsocketHeaders(util.WithGinContext(context.Background(), ginCtx), headers, nil, "", nil, false)

	if got := headers.Get("Session-Id"); got != "cache-1" {
		t.Fatalf("Session-Id = %q, want prompt cache key", got)
	}
	if got := headers.Get("Conversation_id"); got != "cache-1" {
		t.Fatalf("Conversation_id = %q, want prompt cache key", got)
	}
	if got := headers.Get("User-Agent"); got != ginCtx.Request.Header.Get("User-Agent") {
		t.Fatalf("User-Agent = %q, want Codex client value", got)
	}
}

func TestCodexWebsocketLoggingStoresTimelineAndUpgradeRejection(t *testing.T) {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ctx := util.WithGinContext(context.Background(), ginCtx)
	cfg := &config.Config{RequestLog: true}
	headers := http.Header{
		"Authorization": {"Bearer sk-test-secret"},
	}
	info := codexWebsocketRequestLog(
		"wss://api.openai.example/v1/responses",
		headers,
		[]byte(`{"type":"response.create"}`),
		"codex",
		&cliproxyauth.Auth{ID: "auth-1", Label: "Codex"},
	)

	recordAPIWebsocketRequest(ctx, cfg, info)
	recordAPIWebsocketHandshake(ctx, cfg, http.StatusSwitchingProtocols, http.Header{"Upgrade": {"websocket"}})
	appendAPIWebsocketResponse(ctx, cfg, []byte(`{"type":"response.completed"}`))
	recordAPIWebsocketError(ctx, cfg, "read", errCodexWebsocketTest)
	recordAPIWebsocketUpgradeRejection(
		ctx, cfg, websocketUpgradeRequestLog(info), http.StatusTooManyRequests, http.Header{"Retry-After": {"1"}},
		[]byte(`{"error":"quota"}`),
	)

	timelineValue, ok := ginCtx.Get(apiWebsocketTimelineKey)
	if !ok {
		t.Fatal("missing API_WEBSOCKET_TIMELINE")
	}
	timeline := timelineValue.(*logging.BoundedLogBuffer).String()
	for _, want := range []string{
		"Event: api.websocket.request",
		"Event: api.websocket.handshake",
		"Event: api.websocket.response",
		"Event: api.websocket.error",
		"Stage: read",
	} {
		if !strings.Contains(timeline, want) {
			t.Fatalf("timeline missing %q: %s", want, timeline)
		}
	}
	if strings.Contains(timeline, "sk-test-secret") {
		t.Fatalf("timeline leaked authorization header: %s", timeline)
	}

	requestValue, ok := ginCtx.Get(apiRequestKey)
	if !ok {
		t.Fatal("missing API_REQUEST")
	}
	requestLog := requestValue.(*logging.BoundedLogBuffer).String()
	if !strings.Contains(requestLog, "HTTP Method: GET") || !strings.Contains(
		requestLog, "Upstream URL: https://api.openai.example/v1/responses",
	) {
		t.Fatalf("upgrade rejection request log mismatch: %s", requestLog)
	}
	responseValue, ok := ginCtx.Get(apiResponseKey)
	if !ok {
		t.Fatal("missing API_RESPONSE")
	}
	responseLog := responseValue.(*logging.BoundedLogBuffer).String()
	if !strings.Contains(responseLog, "Status: 429") || !strings.Contains(responseLog, `{"error":"quota"}`) {
		t.Fatalf("upgrade rejection response log mismatch: %s", responseLog)
	}
}

func TestCodexWebsocketsGrokKeepalivePreservesRawLogAndSendsComment(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("Upgrade() error = %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("ReadMessage() error = %v", errRead)
			return
		}
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"keepalive","sequence_number":3}`)); errWrite != nil {
			t.Errorf("WriteMessage(keepalive) error = %v", errWrite)
			return
		}
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[]}}`)); errWrite != nil {
			t.Errorf("WriteMessage(completed) error = %v", errWrite)
		}
	}))
	defer server.Close()

	gin.SetMode(gin.TestMode)
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginContext.Request.Header.Set("User-Agent", "grok-pager/1.0")
	ctx := util.WithGinContext(context.Background(), ginContext)
	exec := newCodexWebsocketsExecutor(&config.Config{RequestLog: true, DisableImageGeneration: config.DisableImageGenerationAll})
	stream, errExecute := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{ID: "auth-grok-keepalive", Provider: "codex", Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL}},
		cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	var output bytes.Buffer
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error = %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if !strings.Contains(output.String(), ": keepalive") || strings.Contains(output.String(), `"type":"keepalive"`) {
		t.Fatalf("downstream keepalive = %s, want SSE comment only", output.String())
	}

	timelineValue, ok := ginContext.Get(apiWebsocketTimelineKey)
	if !ok {
		t.Fatal("missing API_WEBSOCKET_TIMELINE")
	}
	timeline := timelineValue.(*logging.BoundedLogBuffer).String()
	if !strings.Contains(timeline, `{"type":"keepalive","sequence_number":3}`) {
		t.Fatalf("raw websocket keepalive missing from timeline: %s", timeline)
	}
}

var errCodexWebsocketTest = errString("codex websocket test error")

type errString string

func (e errString) Error() string { return string(e) }

func TestNewProxyAwareWebsocketDialerUnrecognizedSchemeKeepsDefault(t *testing.T) {
	t.Parallel()

	// "direct" has no URL scheme, so it falls through to the default case in the
	// switch and the dialer retains its initial Proxy: http.ProxyFromEnvironment.
	dialer := newProxyAwareWebsocketDialer(
		&config.Config{ProxyURL: "http://global-proxy.example.com:8080"},
		&cliproxyauth.Auth{ProxyURL: "direct"},
	)

	if dialer.Proxy == nil {
		t.Fatal("expected proxy function to remain set (default http.ProxyFromEnvironment) for unrecognized scheme")
	}
}

func TestNewProxyAwareWebsocketDialerHTTPProxySetsProxy(t *testing.T) {
	t.Parallel()

	dialer := newProxyAwareWebsocketDialer(
		&config.Config{},
		&cliproxyauth.Auth{ProxyURL: "http://proxy.example.com:3128"},
	)

	if dialer.Proxy == nil {
		t.Fatal("expected proxy function to be set for HTTP proxy URL")
	}
}

func TestNormalizeCodexCompletionEventMapsResponseDone(t *testing.T) {
	payload := []byte(`{"type":"response.done","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)

	normalized := normalizeCodexCompletionEvent(payload)

	if got := gjson.GetBytes(normalized, "type").String(); got != "response.completed" {
		t.Fatalf("type = %q, want %q", got, "response.completed")
	}
	if got := gjson.GetBytes(normalized, "response.status").String(); got != "completed" {
		t.Fatalf("response.status = %q, want %q", got, "completed")
	}
}

func TestMapCodexWebsocketReadErrorMapsMessageTooBig(t *testing.T) {
	err := mapCodexWebsocketReadError(&websocket.CloseError{Code: websocket.CloseMessageTooBig})
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusRequestEntityTooLarge {
		t.Fatalf("mapped error = %#v, want 413 status", err)
	}
	if !strings.Contains(err.Error(), `"code":"message_too_big"`) {
		t.Fatalf("mapped error = %q, want message_too_big code", err.Error())
	}
}

func TestCodexWebsocketSessionMultiAgentV2StateIsConnectionScoped(t *testing.T) {
	firstConn := &websocket.Conn{}
	secondConn := &websocket.Conn{}
	sess := &codexWebsocketSession{conn: firstConn}

	sess.setMultiAgentV2Optimized(firstConn, true)
	if !sess.isMultiAgentV2Optimized(firstConn) {
		t.Fatal("optimized state was not recorded for the active connection")
	}
	if sess.isMultiAgentV2Optimized(secondConn) {
		t.Fatal("optimized state leaked to an unrelated connection")
	}

	sess.connMu.Lock()
	sess.conn = secondConn
	sess.connMu.Unlock()
	if sess.isMultiAgentV2Optimized(secondConn) {
		t.Fatal("new connection inherited optimized state")
	}
	sess.setMultiAgentV2Optimized(secondConn, true)
	if !sess.isMultiAgentV2Optimized(secondConn) {
		t.Fatal("optimized state was not recorded after explicit re-enable")
	}
}

func TestInvalidateCodexUpstreamConnClearsMultiAgentV2State(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, errUpgrade := upgrader.Upgrade(w, r, nil)
				if errUpgrade != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				for {
					if _, _, errRead := conn.ReadMessage(); errRead != nil {
						return
					}
				}
			},
		),
	)
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, response, errDial := websocket.DefaultDialer.Dial(wsURL, nil)
	if response != nil && response.Body != nil {
		t.Cleanup(func() { _ = response.Body.Close() })
	}
	if errDial != nil {
		t.Fatal(errDial)
	}
	exec := newCodexWebsocketsExecutor(&config.Config{})
	sess := &codexWebsocketSession{
		sessionID:        "invalidate-state",
		conn:             conn,
		readerConn:       conn,
		authID:           "auth-a",
		wsURL:            wsURL,
		credentialSHA256: "credential-a",
	}
	sess.setMultiAgentV2Optimized(conn, true)
	if !sess.isMultiAgentV2Optimized(conn) {
		t.Fatal("optimized state was not recorded before invalidation")
	}

	exec.invalidateUpstreamConn(sess, conn, "test", nil)

	if sess.isMultiAgentV2Optimized(conn) {
		t.Fatal("invalidation retained optimized state")
	}
	sess.connMu.Lock()
	currentConn := sess.conn
	optimizedConn := sess.multiAgentV2OptimizedConn
	sess.connMu.Unlock()
	if currentConn != nil || optimizedConn != nil {
		t.Fatalf("session state after invalidation: conn=%p optimized=%p", currentConn, optimizedConn)
	}
}

func TestCodexWebsocketsCloseAllMarkerClosesSessions(t *testing.T) {
	exec := newCodexWebsocketsExecutor(&config.Config{})
	marker := &websocket.Conn{}
	sess := &codexWebsocketSession{
		sessionID:                 "session-1",
		multiAgentV2OptimizedConn: marker,
	}
	exec.sessions["session-1"] = sess

	exec.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)

	if exec.sessions["session-1"] != nil {
		t.Fatal("CloseAllExecutionSessionsID kept active session")
	}
	if sess.multiAgentV2OptimizedConn != nil {
		t.Fatal("CloseAllExecutionSessionsID retained optimized state")
	}
}

func TestCodexWebsocketsCloseExecutionSessionsForAuth(t *testing.T) {
	exec := newCodexWebsocketsExecutor(&config.Config{})
	markerA := &websocket.Conn{}
	markerB := &websocket.Conn{}
	sessA := &codexWebsocketSession{
		sessionID:                 "session-a",
		authID:                    "auth-a",
		multiAgentV2OptimizedConn: markerA,
	}
	sessB := &codexWebsocketSession{
		sessionID:                 "session-b",
		authID:                    "auth-b",
		multiAgentV2OptimizedConn: markerB,
	}
	exec.sessions["session-a"] = sessA
	exec.sessions["session-b"] = sessB

	exec.CloseExecutionSessionsForAuth("auth-a")

	if exec.sessions["session-a"] != nil || exec.sessions["session-b"] == nil {
		t.Fatalf("sessions after auth close = %#v", exec.sessions)
	}
	if sessA.multiAgentV2OptimizedConn != nil {
		t.Fatal("auth close retained optimized state for closed session")
	}
	if sessB.multiAgentV2OptimizedConn != markerB {
		t.Fatal("auth close changed optimized state for unrelated session")
	}
}

func TestCodexWebsocketsExecuteStreamDownstreamWebsocketForwardsRawResponseDone(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Fatalf("Upgrade() error = %v", err)
				}
				defer func() { _ = conn.Close() }()

				_, _, err = conn.ReadMessage()
				if err != nil {
					t.Fatalf("ReadMessage() error = %v", err)
				}
				err = conn.WriteMessage(
					websocket.TextMessage,
					[]byte(`{"type":"response.done","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`),
				)
				if err != nil {
					t.Fatalf("WriteMessage() error = %v", err)
				}
			},
		),
	)
	defer server.Close()

	exec := newCodexWebsocketsExecutor(&config.Config{})
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	stream, err := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{
			ID:         "auth-raw-ws",
			Provider:   "codex",
			Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
		},
		cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var got [][]byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		got = append(got, bytes.Clone(chunk.Payload))
		if chunk.Acknowledge != nil {
			chunk.Acknowledge(nil)
		}
	}
	if len(got) != 1 {
		t.Fatalf("chunks length = %d, want 1; chunks=%q", len(got), got)
	}
	if bytes.HasPrefix(bytes.TrimSpace(got[0]), []byte("data:")) {
		t.Fatalf("chunk = %s, want raw JSON without SSE data prefix", got[0])
	}
	if gotType := gjson.GetBytes(got[0], "type").String(); gotType != "response.done" {
		t.Fatalf("type = %q, want response.done; chunk=%s", gotType, got[0])
	}

	select {
	case record := <-plugin.records:
		if record.Failed {
			t.Fatalf("usage record Failed = true, want false: %+v", record)
		}
		if record.Detail.TotalTokens != 3 {
			t.Fatalf("usage total tokens = %d, want 3", record.Detail.TotalTokens)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}

func newCodexTerminalWebsocketServer(t *testing.T, terminalType string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err = conn.ReadMessage(); err != nil {
			t.Errorf("ReadMessage() error = %v", err)
			return
		}
		if err = conn.WriteMessage(
			websocket.TextMessage,
			[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`),
		); err != nil {
			t.Errorf("WriteMessage(output_item.done) error = %v", err)
			return
		}
		terminal := `{"type":"` + terminalType + `","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
		if err = conn.WriteMessage(websocket.TextMessage, []byte(terminal)); err != nil {
			t.Errorf("WriteMessage(%s) error = %v", terminalType, err)
		}
	}))
}

func TestCodexWebsocketsTreatsEmptyResponseIncompleteAsTerminal(t *testing.T) {
	newServer := func(t *testing.T) *httptest.Server {
		t.Helper()
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			conn, errUpgrade := upgrader.Upgrade(w, r, nil)
			if errUpgrade != nil {
				t.Errorf("Upgrade() error = %v", errUpgrade)
				return
			}
			defer func() { _ = conn.Close() }()
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				t.Errorf("ReadMessage() error = %v", errRead)
				return
			}
			terminal := []byte(`{"type":"response.incomplete","response":{"id":"resp-incomplete","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":10,"output_tokens":0,"total_tokens":10}}}`)
			if errWrite := conn.WriteMessage(websocket.TextMessage, terminal); errWrite != nil {
				t.Errorf("WriteMessage() error = %v", errWrite)
			}
		}))
	}

	t.Run("non-stream", func(t *testing.T) {
		server := newServer(t)
		defer server.Close()
		exec := newCodexWebsocketsExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
		response, errExecute := exec.Execute(
			context.Background(),
			&cliproxyauth.Auth{ID: "auth-incomplete", Provider: "codex", Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL}},
			cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse},
		)
		if errExecute != nil {
			t.Fatalf("Execute() error = %v", errExecute)
		}
		if got := gjson.GetBytes(response.Payload, "status").String(); got != "incomplete" {
			t.Fatalf("status = %q, want incomplete; payload=%s", got, response.Payload)
		}
	})

	t.Run("stream", func(t *testing.T) {
		server := newServer(t)
		defer server.Close()
		exec := newCodexWebsocketsExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
		stream, errExecute := exec.ExecuteStream(
			cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
			&cliproxyauth.Auth{ID: "auth-incomplete-stream", Provider: "codex", Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL}},
			cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
		)
		if errExecute != nil {
			t.Fatalf("ExecuteStream() error = %v", errExecute)
		}
		var terminal []byte
		for chunk := range stream.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream error = %v", chunk.Err)
			}
			if gjson.GetBytes(chunk.Payload, "type").String() == "response.incomplete" {
				terminal = bytes.Clone(chunk.Payload)
			}
			if chunk.Acknowledge != nil {
				chunk.Acknowledge(nil)
			}
		}
		if got := gjson.GetBytes(terminal, "response.status").String(); got != "incomplete" {
			t.Fatalf("terminal status = %q, want incomplete; payload=%s", got, terminal)
		}
	})
}

func TestCodexWebsocketsExecuteTerminalOutputRespectsNativeMode(t *testing.T) {
	for _, tt := range []struct {
		name   string
		native bool
	}{
		{name: "compatibility", native: false},
		{name: "native", native: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newCodexTerminalWebsocketServer(t, "response.completed")
			defer server.Close()

			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
			}
			if tt.native {
				opts.Headers = http.Header{
					"x-openai-internal-codex-responses-lite": []string{" TrUe "},
				}
			}
			exec := newCodexWebsocketsExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
			resp, err := exec.Execute(
				context.Background(),
				&cliproxyauth.Auth{
					ID:         "auth-terminal-" + tt.name,
					Provider:   "codex",
					Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
				},
				cliproxyexecutor.Request{
					Model:   "gpt-5.5",
					Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
				},
				opts,
			)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			output := gjson.GetBytes(resp.Payload, "output")
			if tt.native {
				if output.Exists() {
					t.Fatalf("native terminal output was hydrated: %s", resp.Payload)
				}
				return
			}
			if !output.IsArray() || len(output.Array()) != 1 || output.Get("0.id").String() != "msg-1" {
				t.Fatalf("compatibility terminal output was not hydrated: %s", resp.Payload)
			}
		})
	}
}

func TestCodexWebsocketsExecuteStreamTerminalOutputRespectsNativeMode(t *testing.T) {
	for _, tt := range []struct {
		name   string
		native bool
	}{
		{name: "compatibility", native: false},
		{name: "native", native: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newCodexTerminalWebsocketServer(t, "response.done")
			defer server.Close()

			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
				Stream:         true,
			}
			if tt.native {
				opts.Headers = http.Header{
					"x-openai-internal-codex-responses-lite": []string{"true"},
				}
			}
			exec := newCodexWebsocketsExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
			stream, err := exec.ExecuteStream(
				cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
				&cliproxyauth.Auth{
					ID:         "auth-stream-terminal-" + tt.name,
					Provider:   "codex",
					Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
				},
				cliproxyexecutor.Request{
					Model:   "gpt-5.5",
					Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
				},
				opts,
			)
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}
			var terminal []byte
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream chunk error = %v", chunk.Err)
				}
				if gjson.GetBytes(chunk.Payload, "type").String() == "response.done" {
					terminal = bytes.Clone(chunk.Payload)
				}
				if chunk.Acknowledge != nil {
					chunk.Acknowledge(nil)
				}
			}
			if len(terminal) == 0 {
				t.Fatal("missing response.done event")
			}
			output := gjson.GetBytes(terminal, "response.output")
			if tt.native {
				if output.Exists() {
					t.Fatalf("native terminal output was hydrated: %s", terminal)
				}
				return
			}
			if !output.IsArray() || len(output.Array()) != 1 || output.Get("0.id").String() != "msg-1" {
				t.Fatalf("compatibility terminal output was not hydrated: %s", terminal)
			}
		})
	}
}

func TestCodexWebsocketsExecuteStreamTerminalDeliveryFailureReportsFailedUsage(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)
	written := make(chan struct{})

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("Upgrade error: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, err = conn.ReadMessage(); err != nil {
					t.Errorf("ReadMessage error: %v", err)
					return
				}
				err = conn.WriteMessage(
					websocket.TextMessage,
					[]byte(`{"type":"response.done","response":{"id":"resp-cancel","status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`),
				)
				if err != nil {
					t.Errorf("WriteMessage error: %v", err)
					return
				}
				close(written)
				<-r.Context().Done()
			},
		),
	)
	defer server.Close()

	baseCtx, cancel := context.WithCancel(context.Background())
	ctx := cliproxyexecutor.WithDownstreamWebsocket(baseCtx)
	exec := newCodexWebsocketsExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{
			ID:         "auth-cancel-ws",
			Provider:   "codex",
			Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
		},
		cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	<-written

	select {
	case record := <-plugin.records:
		t.Fatalf("usage published before terminal delivery: %+v", record)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	for range stream.Chunks {
	}

	select {
	case record := <-plugin.records:
		if !record.Failed {
			t.Fatalf("usage record Failed = false, want true: %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
}

func TestCodexWebsocketsExecuteStreamRejectsMalformedEvent(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("Upgrade error: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, err = conn.ReadMessage(); err != nil {
					t.Errorf("ReadMessage error: %v", err)
					return
				}
				if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":`)); err != nil {
					t.Errorf("WriteMessage error: %v", err)
				}
			},
		),
	)
	defer server.Close()

	exec := newCodexWebsocketsExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
		&cliproxyauth.Auth{
			ID:         "auth-malformed-ws",
			Provider:   "codex",
			Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
		},
		cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil || statusErrCode(streamErr) != http.StatusBadGateway {
		t.Fatalf("stream error = %v, want 502", streamErr)
	}

	select {
	case record := <-plugin.records:
		if !record.Failed {
			t.Fatalf("usage record Failed = false, want true: %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
}
