package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type websocketFailoverSelector struct {
	mu    sync.Mutex
	order []string
	picks int
}

type websocketPinnedFailoverExecutor struct {
	mu                    sync.Mutex
	identifier            string
	authIDs               []string
	calls                 map[string]int
	payloads              map[string][][]byte
	bootstrapErrorAuthID  string
	streamErrorAuthID     string
	streamErrorCall       int
	streamErrorCode       string
	streamErrorStatus     int
	streamErrors          map[string]map[int]coreauth.Error
	pendingToolCallAuthID string
	pendingToolCallCall   int
}

func (s *websocketFailoverSelector) Pick(
	_ context.Context,
	_, _ string,
	_ coreexecutor.Options,
	auths []*coreauth.Auth,
) (*coreauth.Auth, error) {
	if len(auths) == 0 {
		return nil, errors.New("no auth")
	}
	s.mu.Lock()
	idx := s.picks
	if idx >= len(s.order) {
		idx = len(s.order) - 1
	}
	wanted := s.order[idx]
	s.picks++
	s.mu.Unlock()
	for _, auth := range auths {
		if auth != nil && auth.ID == wanted {
			return auth, nil
		}
	}
	return auths[0], nil
}

func (e *websocketPinnedFailoverExecutor) Identifier() string {
	if e.identifier != "" {
		return e.identifier
	}
	return "codex"
}

func (e *websocketPinnedFailoverExecutor) record(
	auth *coreauth.Auth,
	req coreexecutor.Request,
) (authID string, call int, bootstrapError bool) {
	if auth != nil {
		authID = auth.ID
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.calls == nil {
		e.calls = make(map[string]int)
	}
	if e.payloads == nil {
		e.payloads = make(map[string][][]byte)
	}
	e.authIDs = append(e.authIDs, authID)
	e.calls[authID]++
	call = e.calls[authID]
	e.payloads[authID] = append(e.payloads[authID], bytes.Clone(req.Payload))
	bootstrapError = authID != "" && authID == e.bootstrapErrorAuthID
	return authID, call, bootstrapError
}

func (e *websocketPinnedFailoverExecutor) Execute(
	_ context.Context,
	auth *coreauth.Auth,
	req coreexecutor.Request,
	_ coreexecutor.Options,
) (coreexecutor.Response, error) {
	authID, call, bootstrapError := e.record(auth, req)
	if bootstrapError {
		return coreexecutor.Response{}, &coreauth.Error{
			Code:       "upstream_unavailable",
			Message:    "bootstrap failed",
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}
	return coreexecutor.Response{Payload: []byte(fmt.Sprintf(`{"id":"resp-%s-%d"}`, authID, call))}, nil
}

func (e *websocketPinnedFailoverExecutor) ExecuteStream(
	_ context.Context,
	auth *coreauth.Auth,
	req coreexecutor.Request,
	_ coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	authID, call, bootstrapError := e.record(auth, req)
	if bootstrapError {
		return nil, &coreauth.Error{
			Code:       "upstream_unavailable",
			Message:    "bootstrap failed",
			HTTPStatus: http.StatusServiceUnavailable,
		}
	}

	chunks := make(chan coreexecutor.StreamChunk, 2)
	chunks <- coreexecutor.StreamChunk{Payload: []byte(`{"type":"response.created"}`)}
	streamError := (*coreauth.Error)(nil)
	if errorsByCall := e.streamErrors[authID]; errorsByCall != nil {
		if configuredError, ok := errorsByCall[call]; ok {
			configuredErrorCopy := configuredError
			streamError = &configuredErrorCopy
		}
	}
	if streamError == nil && authID == e.streamErrorAuthID && call == e.streamErrorCall {
		streamError = &coreauth.Error{
			Code:       e.streamErrorCode,
			Message:    "stream failed",
			HTTPStatus: e.streamErrorStatus,
		}
	}
	if streamError != nil {
		chunks <- coreexecutor.StreamChunk{Err: streamError}
		close(chunks)
		return &coreexecutor.StreamResult{Chunks: chunks}, nil
	}
	output := fmt.Sprintf(`[{"type":"message","id":"out-%s-%d"}]`, authID, call)
	if authID == e.pendingToolCallAuthID && call == e.pendingToolCallCall {
		output = fmt.Sprintf(`[{"type":"function_call","id":"fc-%s-%d","call_id":"call-%s-%d"}]`, authID, call, authID, call)
	}
	chunks <- coreexecutor.StreamChunk{
		Payload: []byte(fmt.Sprintf(
			`{"type":"response.completed","response":{"id":"resp-%s-%d","output":%s}}`,
			authID, call, output,
		)),
	}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *websocketPinnedFailoverExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *websocketPinnedFailoverExecutor) CountTokens(
	context.Context,
	*coreauth.Auth,
	coreexecutor.Request,
	coreexecutor.Options,
) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *websocketPinnedFailoverExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (
	*http.Response,
	error,
) {
	return nil, errors.New("not implemented")
}

func (e *websocketPinnedFailoverExecutor) AuthIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.authIDs...)
}

func (e *websocketPinnedFailoverExecutor) Payloads(authID string) [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	src := e.payloads[authID]
	out := make([][]byte, len(src))
	for i := range src {
		out[i] = bytes.Clone(src[i])
	}
	return out
}

func openResponsesWebsocketFailoverTest(
	t *testing.T,
	model string,
	order []string,
	executor *websocketPinnedFailoverExecutor,
	websocketsEnabled bool,
) *websocket.Conn {
	t.Helper()
	gin.SetMode(gin.TestMode)

	selector := &websocketFailoverSelector{order: order}
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(executor)
	websockets := "false"
	if websocketsEnabled {
		websockets = "true"
	}
	registered := make(map[string]struct{}, len(order))
	for _, authID := range order {
		if _, ok := registered[authID]; ok {
			continue
		}
		registered[authID] = struct{}{}
		auth := &coreauth.Auth{
			ID:         authID,
			Provider:   executor.Identifier(),
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"websockets": websockets},
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register %s: %v", authID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(
			auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}},
		)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}

	base := handlers.NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 0}}, manager,
	)
	h := NewResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws", nil,
	)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func readResponsesWebsocketTerminalType(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	for {
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read websocket response: %v", err)
		}
		eventType := gjson.GetBytes(payload, "type").String()
		if eventType == wsEventTypeError || eventType == wsEventTypeDone {
			return eventType
		}
	}
}

func TestForwardResponsesWebsocketAcknowledgesTerminalWriteFailure(t *testing.T) {
	serverConnCh := make(chan *websocket.Conn, 1)
	handlerDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("Upgrade() error = %v", errUpgrade)
			return
		}
		serverConnCh <- conn
		<-handlerDone
	}))
	defer server.Close()

	clientConn, _, errDial := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http"),
		nil,
	)
	if errDial != nil {
		close(handlerDone)
		t.Fatal(errDial)
	}
	defer clientConn.Close()
	serverConn := <-serverConnCh
	if errClose := serverConn.Close(); errClose != nil {
		close(handlerDone)
		t.Fatal(errClose)
	}
	defer close(handlerDone)

	acknowledged := make(chan error, 1)
	data := make(chan coreexecutor.StreamChunk, 1)
	data <- coreexecutor.StreamChunk{
		Payload: []byte(`{"type":"response.done","response":{"id":"response-1","output":[]}}`),
		Acknowledge: func(err error) {
			acknowledged <- err
		},
	}
	close(data)
	errs := make(chan *handlers.ErrorMessage)
	close(errs)

	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	handler := &ResponsesAPIHandler{}
	_, _, _, _, errForward := handler.forwardResponsesWebsocket(
		ginContext,
		serverConn,
		func(...any) {},
		data,
		errs,
		nil,
		"session-write-failure",
		"downstream-session",
	)
	if errForward == nil {
		t.Fatal("forwardResponsesWebsocket() error = nil, want closed connection error")
	}
	if errAcknowledge := <-acknowledged; errAcknowledge == nil {
		t.Fatal("terminal acknowledgement error = nil, want delivery failure")
	}
}

func TestResponsesWebsocketClientIPUsesTrustedRealIP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, router := gin.CreateTestContext(recorder)
	router.TrustedPlatform = gin.PlatformGoogleAppEngine
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	ctx.Request.RemoteAddr = "127.0.0.1:12345"
	ctx.Request.Header.Set("X-Real-IP", "203.0.113.10")

	if got := responsesWebsocketClientIP(ctx); got != "203.0.113.10" {
		t.Fatalf("client IP = %q, want trusted X-Real-IP", got)
	}
}

func TestNormalizeResponsesWebsocketRequestCreate(t *testing.T) {
	raw := []byte(`{"type":"response.create","model":"test-model","stream":false,"input":[{"type":"message","id":"msg-1"}]}`)

	normalized, last, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, nil, nil, true)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "type").Exists() {
		t.Fatalf("normalized create request must not include type field")
	}
	if !gjson.GetBytes(normalized, "stream").Bool() {
		t.Fatalf("normalized create request must force stream=true")
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("unexpected model: %s", gjson.GetBytes(normalized, "model").String())
	}
	if !bytes.Equal(last, normalized) {
		t.Fatalf("last request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestCreateWithHistory(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "type").Exists() {
		t.Fatalf("normalized subsequent create request must not include type field")
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("unexpected model: %s", gjson.GetBytes(normalized, "model").String())
	}

	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 4 {
		t.Fatalf("merged input len = %d, want 4", len(input))
	}
	if input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "fc-1" ||
		input[2].Get("id").String() != "assistant-1" ||
		input[3].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected merged input order")
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestResponsesWebsocketAutomaticallyContinuesSelectedWebsocketAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	selector := &websocketFailoverSelector{order: []string{"auth-c"}}
	executor := &websocketPinnedFailoverExecutor{}
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(executor)

	authC := &coreauth.Auth{
		ID:         "auth-c",
		Provider:   executor.Identifier(),
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"websockets": "true"},
	}
	if _, err := manager.Register(context.Background(), authC); err != nil {
		t.Fatalf("Register auth C: %v", err)
	}

	registry.GetGlobalRegistry().RegisterClient(authC.ID, authC.Provider, []*registry.ModelInfo{{ID: "continuation-model"}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authC.ID) })

	base := handlers.NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 0}}, manager,
	)
	h := NewResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws", nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	requests := []string{
		`{"type":"response.create","model":"continuation-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"msg-2"}]}`,
	}
	for i := range requests {
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(requests[i])); errWrite != nil {
			t.Fatalf("write websocket message %d: %v", i+1, errWrite)
		}
		for {
			_, payload, errReadMessage := conn.ReadMessage()
			if errReadMessage != nil {
				t.Fatalf("read websocket message %d: %v", i+1, errReadMessage)
			}
			gotType := gjson.GetBytes(payload, "type").String()
			if gotType == wsEventTypeError {
				t.Fatalf("websocket message %d returned error: %s", i+1, payload)
			}
			if gotType == wsEventTypeDone {
				break
			}
		}
	}

	payloads := executor.Payloads("auth-c")
	if len(payloads) != 2 {
		t.Fatalf("auth-c payload count = %d, want 2", len(payloads))
	}
	if got := gjson.GetBytes(payloads[1], "previous_response_id").String(); got != "resp-auth-c-1" {
		t.Fatalf("previous_response_id = %q, want resp-auth-c-1: %s", got, payloads[1])
	}
	input := gjson.GetBytes(payloads[1], "input").Array()
	if len(input) != 1 || input[0].Get("id").String() != "msg-2" {
		t.Fatalf("incremental continuation input = %s, want msg-2 only", gjson.GetBytes(payloads[1], "input").Raw)
	}
}

func TestResponsesContinuationDoesNotRetryAcrossAuths(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "non-streaming",
			path: "/v1/responses",
			body: `{"model":"continuation-http-model","previous_response_id":"resp-owned-by-a","input":[]}`,
		},
		{
			name: "streaming",
			path: "/v1/responses",
			body: `{"model":"continuation-http-model","previous_response_id":"resp-owned-by-a","input":[],"stream":true}`,
		},
		{
			name: "compact",
			path: "/v1/responses/compact",
			body: `{"model":"continuation-http-model","previous_response_id":"resp-owned-by-a","input":[]}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			selector := &websocketFailoverSelector{order: []string{"auth-a", "auth-b"}}
			executor := &websocketPinnedFailoverExecutor{bootstrapErrorAuthID: "auth-a"}
			manager := coreauth.NewManager(nil, selector, nil)
			manager.RegisterExecutor(executor)

			for _, authID := range []string{"auth-a", "auth-b"} {
				auth := &coreauth.Auth{
					ID:       authID,
					Provider: executor.Identifier(),
					Status:   coreauth.StatusActive,
				}
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatalf("Register %s: %v", authID, err)
				}
				registry.GetGlobalRegistry().RegisterClient(
					auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "continuation-http-model"}},
				)
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			}

			base := handlers.NewBaseAPIHandlers(
				&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 1}}, manager,
			)
			h := NewResponsesAPIHandler(base)
			router := gin.New()
			router.POST("/v1/responses", h.Responses)
			router.POST("/v1/responses/compact", h.Compact)

			req := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)

			if resp.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusServiceUnavailable, resp.Body.String())
			}
			if got := executor.AuthIDs(); len(got) != 1 || got[0] != "auth-a" {
				t.Fatalf("selected auth IDs = %v, want [auth-a]", got)
			}
			if payloads := executor.Payloads("auth-b"); len(payloads) != 0 {
				t.Fatalf("auth-b received continuation payloads: %q", payloads)
			}
		})
	}
}

func TestResponsesWebsocketInitialContinuationDoesNotRetryAcrossAuths(t *testing.T) {
	gin.SetMode(gin.TestMode)

	selector := &websocketFailoverSelector{order: []string{"auth-a", "auth-b"}}
	executor := &websocketPinnedFailoverExecutor{bootstrapErrorAuthID: "auth-a"}
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(executor)

	for _, authID := range []string{"auth-a", "auth-b"} {
		auth := &coreauth.Auth{
			ID:         authID,
			Provider:   executor.Identifier(),
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"websockets": "true"},
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register %s: %v", authID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(
			auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "continuation-owner-model"}},
		)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}

	base := handlers.NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 1}}, manager,
	)
	h := NewResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws", nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	request := []byte(
		`{"type":"response.create","model":"continuation-owner-model","previous_response_id":"resp-owned-by-a","input":[{"type":"message","id":"msg-1"}]}`,
	)
	if errWrite := conn.WriteMessage(websocket.TextMessage, request); errWrite != nil {
		t.Fatalf("write websocket request: %v", errWrite)
	}
	for {
		_, payload, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Fatalf("read websocket response: %v", errRead)
		}
		if gjson.GetBytes(payload, "type").String() == wsEventTypeError {
			break
		}
	}

	if got := executor.AuthIDs(); len(got) != 1 || got[0] != "auth-a" {
		t.Fatalf("selected auth IDs = %v, want [auth-a]", got)
	}
	if payloads := executor.Payloads("auth-b"); len(payloads) != 0 {
		t.Fatalf("auth-b received continuation payloads: %q", payloads)
	}
}

func TestResponsesWebsocketInitialContinuationRetryKeepsOwner(t *testing.T) {
	tests := []struct {
		name              string
		websocketsEnabled bool
		streamErrorCode   string
		streamErrorStatus int
	}{
		{
			name:              "incremental quota error",
			websocketsEnabled: true,
			streamErrorCode:   "rate_limit_exceeded",
			streamErrorStatus: http.StatusTooManyRequests,
		},
		{
			name:              "HTTP fallback quota error",
			websocketsEnabled: false,
			streamErrorCode:   "rate_limit_exceeded",
			streamErrorStatus: http.StatusTooManyRequests,
		},
		{
			name:              "owner unavailable",
			websocketsEnabled: true,
			streamErrorCode:   "pinned_auth_not_found",
			streamErrorStatus: http.StatusConflict,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			selector := &websocketFailoverSelector{order: []string{"auth-a", "auth-b"}}
			executor := &websocketPinnedFailoverExecutor{
				streamErrorAuthID: "auth-a",
				streamErrorCall:   1,
				streamErrorCode:   test.streamErrorCode,
				streamErrorStatus: test.streamErrorStatus,
			}
			manager := coreauth.NewManager(nil, selector, nil)
			manager.RegisterExecutor(executor)

			websockets := "false"
			if test.websocketsEnabled {
				websockets = "true"
			}
			for _, authID := range []string{"auth-a", "auth-b"} {
				auth := &coreauth.Auth{
					ID:         authID,
					Provider:   executor.Identifier(),
					Status:     coreauth.StatusActive,
					Attributes: map[string]string{"websockets": websockets},
				}
				if _, err := manager.Register(context.Background(), auth); err != nil {
					t.Fatalf("Register %s: %v", authID, err)
				}
				registry.GetGlobalRegistry().RegisterClient(
					auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "continuation-owner-retry-model"}},
				)
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			}

			base := handlers.NewBaseAPIHandlers(
				&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 0}}, manager,
			)
			h := NewResponsesAPIHandler(base)
			router := gin.New()
			router.GET("/v1/responses/ws", h.ResponsesWebsocket)
			server := httptest.NewServer(router)
			defer server.Close()

			conn, _, err := websocket.DefaultDialer.Dial(
				"ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws", nil,
			)
			if err != nil {
				t.Fatalf("dial websocket: %v", err)
			}
			defer conn.Close()

			request := []byte(
				`{"type":"response.create","model":"continuation-owner-retry-model","previous_response_id":"resp-owned-by-a","input":[{"type":"message","id":"msg-1"}]}`,
			)
			wantTypes := []string{wsEventTypeError, wsEventTypeDone}
			for i, wantType := range wantTypes {
				if errWrite := conn.WriteMessage(websocket.TextMessage, request); errWrite != nil {
					t.Fatalf("write websocket request %d: %v", i+1, errWrite)
				}
				for {
					_, payload, errRead := conn.ReadMessage()
					if errRead != nil {
						t.Fatalf("read websocket response %d: %v", i+1, errRead)
					}
					gotType := gjson.GetBytes(payload, "type").String()
					if gotType != wsEventTypeError && gotType != wsEventTypeDone {
						continue
					}
					if gotType != wantType {
						t.Fatalf("response %d type = %s, want %s: %s", i+1, gotType, wantType, payload)
					}
					break
				}
			}

			if got := executor.AuthIDs(); len(got) != 2 || got[0] != "auth-a" || got[1] != "auth-a" {
				t.Fatalf("selected auth IDs = %v, want [auth-a auth-a]", got)
			}
			if payloads := executor.Payloads("auth-b"); len(payloads) != 0 {
				t.Fatalf("auth-b received continuation payloads: %q", payloads)
			}
			for _, payload := range executor.Payloads("auth-a") {
				if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "resp-owned-by-a" {
					t.Fatalf("previous_response_id = %q, want resp-owned-by-a: %s", got, payload)
				}
			}
		})
	}
}

func TestResponsesWebsocketHTTPFallbackContinuesWithResponseID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	selector := &websocketFailoverSelector{order: []string{"auth-a", "auth-b"}}
	executor := &websocketPinnedFailoverExecutor{}
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(executor)

	for _, authID := range []string{"auth-a", "auth-b"} {
		auth := &coreauth.Auth{
			ID:         authID,
			Provider:   executor.Identifier(),
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"websockets": "false"},
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register %s: %v", authID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(
			auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "http-continuation-model"}},
		)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws", nil,
	)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	requests := []string{
		`{"type":"response.create","model":"http-continuation-model","previous_response_id":"resp-external","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"msg-2"}]}`,
	}
	for i, request := range requests {
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(request)); errWrite != nil {
			t.Fatalf("write websocket request %d: %v", i+1, errWrite)
		}
		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Fatalf("read websocket response %d: %v", i+1, errRead)
			}
			gotType := gjson.GetBytes(payload, "type").String()
			if gotType == wsEventTypeError {
				t.Fatalf("websocket response %d returned error: %s", i+1, payload)
			}
			if gotType == wsEventTypeDone {
				break
			}
		}
	}

	if got := executor.AuthIDs(); len(got) != 2 || got[0] != "auth-a" || got[1] != "auth-a" {
		t.Fatalf("selected auth IDs = %v, want [auth-a auth-a]", got)
	}
	payloads := executor.Payloads("auth-a")
	if got := gjson.GetBytes(payloads[1], "previous_response_id").String(); got != "resp-auth-a-1" {
		t.Fatalf("previous_response_id = %q, want resp-auth-a-1: %s", got, payloads[1])
	}
	input := gjson.GetBytes(payloads[1], "input").Array()
	if len(input) != 1 || input[0].Get("id").String() != "msg-2" {
		t.Fatalf("HTTP continuation input = %s, want msg-2 only", gjson.GetBytes(payloads[1], "input").Raw)
	}
}

func TestResponsesWebsocketMultiTurnReplayKeepsCompleteTranscript(t *testing.T) {
	gin.SetMode(gin.TestMode)
	selector := &websocketFailoverSelector{order: []string{"auth-a", "auth-b"}}
	executor := &websocketPinnedFailoverExecutor{
		streamErrorAuthID: "auth-a",
		streamErrorCall:   3,
		streamErrorCode:   "rate_limit_exceeded",
		streamErrorStatus: http.StatusTooManyRequests,
	}
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(executor)

	for _, authID := range []string{"auth-a", "auth-b"} {
		auth := &coreauth.Auth{
			ID:         authID,
			Provider:   executor.Identifier(),
			Status:     coreauth.StatusActive,
			Attributes: map[string]string{"websockets": "true"},
		}
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register %s: %v", authID, err)
		}
		registry.GetGlobalRegistry().RegisterClient(
			auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "multi-turn-replay-model"}},
		)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)
	server := httptest.NewServer(router)
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws", nil,
	)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	requests := []string{
		`{"type":"response.create","model":"multi-turn-replay-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"msg-2"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"msg-3"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"msg-3"}]}`,
	}
	wantTypes := []string{wsEventTypeDone, wsEventTypeDone, wsEventTypeError, wsEventTypeDone}
	for i, request := range requests {
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(request)); errWrite != nil {
			t.Fatalf("write websocket request %d: %v", i+1, errWrite)
		}
		for {
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Fatalf("read websocket response %d: %v", i+1, errRead)
			}
			gotType := gjson.GetBytes(payload, "type").String()
			if gotType != wsEventTypeError && gotType != wsEventTypeDone {
				continue
			}
			if gotType != wantTypes[i] {
				t.Fatalf("response %d type = %s, want %s: %s", i+1, gotType, wantTypes[i], payload)
			}
			break
		}
	}

	if got := executor.AuthIDs(); len(got) != 4 ||
		got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-a" || got[3] != "auth-b" {
		t.Fatalf("selected auth IDs = %v, want [auth-a auth-a auth-a auth-b]", got)
	}
	payloads := executor.Payloads("auth-b")
	if len(payloads) != 1 {
		t.Fatalf("auth-b payload count = %d, want 1", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "previous_response_id").Exists() {
		t.Fatalf("previous_response_id leaked to auth-b: %s", payloads[0])
	}
	input := gjson.GetBytes(payloads[0], "input").Array()
	wantIDs := []string{"msg-1", "out-auth-a-1", "msg-2", "out-auth-a-2", "msg-3"}
	if len(input) != len(wantIDs) {
		t.Fatalf("replay input length = %d, want %d: %s", len(input), len(wantIDs), payloads[0])
	}
	for i, wantID := range wantIDs {
		if got := input[i].Get("id").String(); got != wantID {
			t.Fatalf("replay input[%d].id = %q, want %q: %s", i, got, wantID, payloads[0])
		}
	}
}

func TestResponsesWebsocketTranscriptReplacementResetsReplayLineage(t *testing.T) {
	t.Run("successful replacement", func(t *testing.T) {
		executor := &websocketPinnedFailoverExecutor{
			pendingToolCallAuthID: "auth-a",
			pendingToolCallCall:   1,
			streamErrorAuthID:     "auth-a",
			streamErrorCall:       3,
			streamErrorCode:       "rate_limit_exceeded",
			streamErrorStatus:     http.StatusTooManyRequests,
		}
		conn := openResponsesWebsocketFailoverTest(
			t, "replacement-replay-model", []string{"auth-a", "auth-b"}, executor, true,
		)

		requests := []string{
			`{"type":"response.create","model":"replacement-replay-model","input":[{"type":"message","id":"old-msg"}]}`,
			`{"type":"response.create","input":[{"type":"message","id":"summary-1"}]}`,
			`{"type":"response.append","input":[{"type":"message","id":"next-msg"}]}`,
			`{"type":"response.append","input":[{"type":"message","id":"next-msg"}]}`,
		}
		wantTypes := []string{wsEventTypeDone, wsEventTypeDone, wsEventTypeError, wsEventTypeDone}
		for i, request := range requests {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
				t.Fatalf("write websocket request %d: %v", i+1, err)
			}
			if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
				t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
			}
		}

		if got := executor.AuthIDs(); len(got) != 4 ||
			got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-a" || got[3] != "auth-b" {
			t.Fatalf("selected auth IDs = %v, want [auth-a auth-a auth-a auth-b]", got)
		}
		payloads := executor.Payloads("auth-b")
		if len(payloads) != 1 {
			t.Fatalf("auth-b payload count = %d, want 1", len(payloads))
		}
		input := gjson.GetBytes(payloads[0], "input").Array()
		wantIDs := []string{"summary-1", "out-auth-a-2", "next-msg"}
		if len(input) != len(wantIDs) {
			t.Fatalf("replacement replay input length = %d, want %d: %s", len(input), len(wantIDs), payloads[0])
		}
		for i, wantID := range wantIDs {
			if got := input[i].Get("id").String(); got != wantID {
				t.Fatalf("replacement replay input[%d].id = %q, want %q: %s", i, got, wantID, payloads[0])
			}
		}
	})

	t.Run("replacement retry", func(t *testing.T) {
		executor := &websocketPinnedFailoverExecutor{
			pendingToolCallAuthID: "auth-a",
			pendingToolCallCall:   1,
			streamErrorAuthID:     "auth-a",
			streamErrorCall:       2,
			streamErrorCode:       "rate_limit_exceeded",
			streamErrorStatus:     http.StatusTooManyRequests,
		}
		conn := openResponsesWebsocketFailoverTest(
			t, "replacement-retry-model", []string{"auth-a", "auth-b"}, executor, true,
		)

		requests := []string{
			`{"type":"response.create","model":"replacement-retry-model","input":[{"type":"message","id":"old-msg"}]}`,
			`{"type":"response.append","input":[{"type":"message","id":"summary-1"}]}`,
			`{"type":"response.append","input":[{"type":"message","id":"summary-1"}]}`,
		}
		wantTypes := []string{wsEventTypeDone, wsEventTypeError, wsEventTypeDone}
		for i, request := range requests {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
				t.Fatalf("write websocket request %d: %v", i+1, err)
			}
			if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
				t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
			}
		}

		if got := executor.AuthIDs(); len(got) != 3 ||
			got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-b" {
			t.Fatalf("selected auth IDs = %v, want [auth-a auth-a auth-b]", got)
		}
		payloads := executor.Payloads("auth-b")
		if len(payloads) != 1 {
			t.Fatalf("auth-b payload count = %d, want 1", len(payloads))
		}
		input := gjson.GetBytes(payloads[0], "input").Array()
		if len(input) != 1 || input[0].Get("id").String() != "summary-1" {
			t.Fatalf("replacement retry input = %s, want summary only", gjson.GetBytes(payloads[0], "input").Raw)
		}
	})
}

func TestResponsesWebsocketDivergentExternalContinuationKeepsOwner(t *testing.T) {
	executor := &websocketPinnedFailoverExecutor{
		streamErrorAuthID: "auth-a",
		streamErrorCall:   3,
		streamErrorCode:   "rate_limit_exceeded",
		streamErrorStatus: http.StatusTooManyRequests,
	}
	conn := openResponsesWebsocketFailoverTest(
		t, "external-lineage-model", []string{"auth-a", "auth-b"}, executor, true,
	)

	requests := []string{
		`{"type":"response.create","model":"external-lineage-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.create","previous_response_id":"resp-external-a","input":[{"type":"message","id":"msg-2"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"msg-3"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"msg-3"}]}`,
	}
	wantTypes := []string{wsEventTypeDone, wsEventTypeDone, wsEventTypeError, wsEventTypeDone}
	for i, request := range requests {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatalf("write websocket request %d: %v", i+1, err)
		}
		if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
			t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
		}
	}

	if got := executor.AuthIDs(); len(got) != 4 ||
		got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-a" || got[3] != "auth-a" {
		t.Fatalf("selected auth IDs = %v, want auth-a for all requests", got)
	}
	if payloads := executor.Payloads("auth-b"); len(payloads) != 0 {
		t.Fatalf("auth-b received divergent continuation lineage: %q", payloads)
	}
	payloads := executor.Payloads("auth-a")
	if got := gjson.GetBytes(payloads[1], "previous_response_id").String(); got != "resp-external-a" {
		t.Fatalf("divergent previous_response_id = %q, want resp-external-a", got)
	}
	for _, payload := range payloads[2:] {
		if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "resp-auth-a-2" {
			t.Fatalf("continued previous_response_id = %q, want resp-auth-a-2: %s", got, payload)
		}
	}
}

func TestResponsesWebsocketFullTranscriptReplayDoesNotDuplicateHistory(t *testing.T) {
	executor := &websocketPinnedFailoverExecutor{
		identifier:        "test-provider",
		streamErrorAuthID: "auth-a",
		streamErrorCall:   3,
		streamErrorCode:   "rate_limit_exceeded",
		streamErrorStatus: http.StatusTooManyRequests,
	}
	conn := openResponsesWebsocketFailoverTest(
		t,
		"full-transcript-model",
		[]string{"auth-a", "auth-a", "auth-a", "auth-b"},
		executor,
		false,
	)

	requests := []string{
		`{"type":"response.create","model":"full-transcript-model","input":[{"type":"message","role":"user","content":"turn-1"}]}`,
		`{"type":"response.append","input":[{"type":"message","role":"user","content":"turn-2"}]}`,
		`{"type":"response.append","input":[{"type":"message","role":"user","content":"turn-3"}]}`,
		`{"type":"response.append","input":[{"type":"message","role":"user","content":"turn-3"}]}`,
	}
	wantTypes := []string{wsEventTypeDone, wsEventTypeDone, wsEventTypeError, wsEventTypeDone}
	for i, request := range requests {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatalf("write websocket request %d: %v", i+1, err)
		}
		if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
			t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
		}
	}

	if got := executor.AuthIDs(); len(got) != 4 ||
		got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-a" || got[3] != "auth-b" {
		t.Fatalf("selected auth IDs = %v, want [auth-a auth-a auth-a auth-b]", got)
	}
	payloads := executor.Payloads("auth-b")
	if len(payloads) != 1 {
		t.Fatalf("auth-b payload count = %d, want 1", len(payloads))
	}
	input := gjson.GetBytes(payloads[0], "input").Array()
	if len(input) != 5 {
		t.Fatalf("full transcript input length = %d, want 5: %s", len(input), payloads[0])
	}
	if input[0].Get("content").String() != "turn-1" ||
		input[1].Get("id").String() != "out-auth-a-1" ||
		input[2].Get("content").String() != "turn-2" ||
		input[3].Get("id").String() != "out-auth-a-2" ||
		input[4].Get("content").String() != "turn-3" {
		t.Fatalf("unexpected full transcript replay: %s", payloads[0])
	}
}

func TestResponsesWebsocketRetryStatePreservesNewExternalContinuation(t *testing.T) {
	t.Run("full transcript retry", func(t *testing.T) {
		executor := &websocketPinnedFailoverExecutor{
			streamErrorAuthID: "auth-a",
			streamErrorCall:   2,
			streamErrorCode:   "rate_limit_exceeded",
			streamErrorStatus: http.StatusTooManyRequests,
		}
		conn := openResponsesWebsocketFailoverTest(
			t, "retry-external-model", []string{"auth-a", "auth-b"}, executor, true,
		)

		requests := []string{
			`{"type":"response.create","model":"retry-external-model","input":[{"type":"message","id":"msg-1"}]}`,
			`{"type":"response.append","input":[{"type":"message","id":"msg-2"}]}`,
			`{"type":"response.append","previous_response_id":"resp-external-b","input":[{"type":"message","id":"external-msg"}]}`,
		}
		wantTypes := []string{wsEventTypeDone, wsEventTypeError, wsEventTypeDone}
		for i, request := range requests {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
				t.Fatalf("write websocket request %d: %v", i+1, err)
			}
			if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
				t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
			}
		}

		payloads := executor.Payloads("auth-b")
		if len(payloads) != 1 {
			t.Fatalf("auth-b payload count = %d, want 1", len(payloads))
		}
		if got := gjson.GetBytes(payloads[0], "previous_response_id").String(); got != "resp-external-b" {
			t.Fatalf("previous_response_id = %q, want resp-external-b: %s", got, payloads[0])
		}
		input := gjson.GetBytes(payloads[0], "input").Array()
		if len(input) != 1 || input[0].Get("id").String() != "external-msg" {
			t.Fatalf("external continuation input = %s, want external-msg only", gjson.GetBytes(payloads[0], "input").Raw)
		}
	})

	t.Run("replacement retry", func(t *testing.T) {
		executor := &websocketPinnedFailoverExecutor{
			pendingToolCallAuthID: "auth-a",
			pendingToolCallCall:   1,
			streamErrorAuthID:     "auth-a",
			streamErrorCall:       2,
			streamErrorCode:       "rate_limit_exceeded",
			streamErrorStatus:     http.StatusTooManyRequests,
		}
		conn := openResponsesWebsocketFailoverTest(
			t, "replacement-external-model", []string{"auth-a", "auth-b"}, executor, true,
		)

		requests := []string{
			`{"type":"response.create","model":"replacement-external-model","input":[{"type":"message","id":"old-msg"}]}`,
			`{"type":"response.append","input":[{"type":"message","id":"summary-1"}]}`,
			`{"type":"response.append","previous_response_id":"resp-external-b","input":[{"type":"message","id":"external-msg"}]}`,
		}
		wantTypes := []string{wsEventTypeDone, wsEventTypeError, wsEventTypeDone}
		for i, request := range requests {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
				t.Fatalf("write websocket request %d: %v", i+1, err)
			}
			if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
				t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
			}
		}

		payloads := executor.Payloads("auth-b")
		if len(payloads) != 1 {
			t.Fatalf("auth-b payload count = %d, want 1", len(payloads))
		}
		if got := gjson.GetBytes(payloads[0], "previous_response_id").String(); got != "resp-external-b" {
			t.Fatalf("previous_response_id = %q, want resp-external-b: %s", got, payloads[0])
		}
		input := gjson.GetBytes(payloads[0], "input").Array()
		if len(input) != 1 || input[0].Get("id").String() != "external-msg" {
			t.Fatalf("external continuation input = %s, want external-msg only", gjson.GetBytes(payloads[0], "input").Raw)
		}
	})
}

func TestResponsesWebsocketReplacementRetryCanRestorePreviousOwner(t *testing.T) {
	executor := &websocketPinnedFailoverExecutor{
		pendingToolCallAuthID: "auth-a",
		pendingToolCallCall:   1,
		streamErrorAuthID:     "auth-a",
		streamErrorCall:       2,
		streamErrorCode:       "rate_limit_exceeded",
		streamErrorStatus:     http.StatusTooManyRequests,
	}
	conn := openResponsesWebsocketFailoverTest(
		t, "replacement-owner-model", []string{"auth-a", "auth-b"}, executor, true,
	)

	requests := []string{
		`{"type":"response.create","model":"replacement-owner-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"summary-1"}]}`,
		`{"type":"response.append","previous_response_id":"resp-auth-a-1","input":[{"type":"function_call_output","id":"tool-1","call_id":"call-auth-a-1","output":"done"}]}`,
	}
	wantTypes := []string{wsEventTypeDone, wsEventTypeError, wsEventTypeDone}
	for i, request := range requests {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatalf("write websocket request %d: %v", i+1, err)
		}
		if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
			t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
		}
	}

	if got := executor.AuthIDs(); len(got) != 3 || got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-a" {
		t.Fatalf("selected auth IDs = %v, want auth-a for all requests", got)
	}
	if payloads := executor.Payloads("auth-b"); len(payloads) != 0 {
		t.Fatalf("auth-b received previous owner continuation: %q", payloads)
	}
	payloads := executor.Payloads("auth-a")
	if got := gjson.GetBytes(payloads[2], "previous_response_id").String(); got != "resp-auth-a-1" {
		t.Fatalf("previous_response_id = %q, want resp-auth-a-1: %s", got, payloads[2])
	}
}

func TestResponsesWebsocketReplacementRetryImplicitlyRestoresPreviousOwner(t *testing.T) {
	executor := &websocketPinnedFailoverExecutor{
		pendingToolCallAuthID: "auth-a",
		pendingToolCallCall:   1,
		streamErrorAuthID:     "auth-a",
		streamErrorCall:       2,
		streamErrorCode:       "rate_limit_exceeded",
		streamErrorStatus:     http.StatusTooManyRequests,
	}
	conn := openResponsesWebsocketFailoverTest(
		t, "replacement-implicit-owner-model", []string{"auth-a", "auth-b"}, executor, true,
	)

	requests := []string{
		`{"type":"response.create","model":"replacement-implicit-owner-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"summary-1"}]}`,
		`{"type":"response.append","input":[{"type":"function_call_output","id":"tool-1","call_id":"call-auth-a-1","output":"done"}]}`,
	}
	wantTypes := []string{wsEventTypeDone, wsEventTypeError, wsEventTypeDone}
	for i, request := range requests {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatalf("write websocket request %d: %v", i+1, err)
		}
		if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
			t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
		}
	}

	if got := executor.AuthIDs(); len(got) != 3 || got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-a" {
		t.Fatalf("selected auth IDs = %v, want auth-a for all requests", got)
	}
	payloads := executor.Payloads("auth-a")
	if got := gjson.GetBytes(payloads[2], "previous_response_id").String(); got != "resp-auth-a-1" {
		t.Fatalf("implicit previous_response_id = %q, want resp-auth-a-1: %s", got, payloads[2])
	}
}

func TestResponsesWebsocketForceReplayAllowsTranscriptReplacement(t *testing.T) {
	executor := &websocketPinnedFailoverExecutor{
		pendingToolCallAuthID: "auth-a",
		pendingToolCallCall:   1,
		streamErrorAuthID:     "auth-a",
		streamErrorCall:       2,
		streamErrorCode:       "rate_limit_exceeded",
		streamErrorStatus:     http.StatusTooManyRequests,
	}
	conn := openResponsesWebsocketFailoverTest(
		t, "force-replacement-model", []string{"auth-a", "auth-b"}, executor, true,
	)

	requests := []string{
		`{"type":"response.create","model":"force-replacement-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.append","input":[{"type":"function_call_output","id":"tool-1","call_id":"call-auth-a-1","output":"done"}]}`,
		`{"type":"response.create","input":[{"type":"message","id":"summary-1"}]}`,
	}
	wantTypes := []string{wsEventTypeDone, wsEventTypeError, wsEventTypeDone}
	for i, request := range requests {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatalf("write websocket request %d: %v", i+1, err)
		}
		if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
			t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
		}
	}

	payloads := executor.Payloads("auth-b")
	if len(payloads) != 1 {
		t.Fatalf("auth-b payload count = %d, want 1", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "previous_response_id").Exists() {
		t.Fatalf("replacement leaked previous_response_id: %s", payloads[0])
	}
	input := gjson.GetBytes(payloads[0], "input").Array()
	if len(input) != 1 || input[0].Get("id").String() != "summary-1" {
		t.Fatalf("force replacement input = %s, want summary only", gjson.GetBytes(payloads[0], "input").Raw)
	}
}

func TestResponsesWebsocketNonReleaseErrorRollsBackTurnState(t *testing.T) {
	executor := &websocketPinnedFailoverExecutor{
		streamErrorAuthID: "auth-a",
		streamErrorCall:   2,
		streamErrorCode:   "upstream_error",
		streamErrorStatus: http.StatusInternalServerError,
	}
	conn := openResponsesWebsocketFailoverTest(
		t, "rollback-model", []string{"auth-a", "auth-b"}, executor, true,
	)

	requests := []string{
		`{"type":"response.create","model":"rollback-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"msg-2"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"msg-2"}]}`,
	}
	wantTypes := []string{wsEventTypeDone, wsEventTypeError, wsEventTypeDone}
	for i, request := range requests {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatalf("write websocket request %d: %v", i+1, err)
		}
		if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
			t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
		}
	}

	if got := executor.AuthIDs(); len(got) != 3 || got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-a" {
		t.Fatalf("selected auth IDs = %v, want auth-a for all requests", got)
	}
	payloads := executor.Payloads("auth-a")
	for _, payload := range payloads[1:] {
		if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "resp-auth-a-1" {
			t.Fatalf("previous_response_id = %q, want resp-auth-a-1: %s", got, payload)
		}
		input := gjson.GetBytes(payload, "input").Array()
		if len(input) != 1 || input[0].Get("id").String() != "msg-2" {
			t.Fatalf("retried input = %s, want msg-2 only", gjson.GetBytes(payload, "input").Raw)
		}
	}
}

func TestResponsesWebsocketInvalidExternalFramePreservesRetryProvenance(t *testing.T) {
	executor := &websocketPinnedFailoverExecutor{
		streamErrorAuthID: "auth-a",
		streamErrorCall:   2,
		streamErrorCode:   "rate_limit_exceeded",
		streamErrorStatus: http.StatusTooManyRequests,
	}
	conn := openResponsesWebsocketFailoverTest(
		t, "invalid-external-model", []string{"auth-a", "auth-b"}, executor, true,
	)

	requests := []string{
		`{"type":"response.create","model":"invalid-external-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"msg-2"}]}`,
		`{"type":"response.append","previous_response_id":"resp-external","input":"invalid"}`,
		`{"type":"response.append","previous_response_id":"resp-auth-a-1","input":[{"type":"message","id":"msg-3"}]}`,
	}
	wantTypes := []string{wsEventTypeDone, wsEventTypeError, wsEventTypeError, wsEventTypeDone}
	for i, request := range requests {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatalf("write websocket request %d: %v", i+1, err)
		}
		if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
			t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
		}
	}

	if got := executor.AuthIDs(); len(got) != 3 || got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-b" {
		t.Fatalf("selected auth IDs = %v, want [auth-a auth-a auth-b]", got)
	}
	payloads := executor.Payloads("auth-b")
	if len(payloads) != 1 {
		t.Fatalf("auth-b payload count = %d, want 1", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "previous_response_id").Exists() {
		t.Fatalf("committed response ID leaked after invalid external frame: %s", payloads[0])
	}
	input := gjson.GetBytes(payloads[0], "input").Array()
	if len(input) != 3 || input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "out-auth-a-1" || input[2].Get("id").String() != "msg-3" {
		t.Fatalf("invalid frame changed replay baseline: %s", payloads[0])
	}
}

func TestResponsesWebsocketHistoricalResponseIDRestoresOwner(t *testing.T) {
	tests := []struct {
		name             string
		historicalError  *coreauth.Error
		wantTerminalType string
	}{
		{name: "owner available", wantTerminalType: wsEventTypeDone},
		{
			name: "owner quota error does not cross auth",
			historicalError: &coreauth.Error{
				Code:       "rate_limit_exceeded",
				Message:    "stream failed",
				HTTPStatus: http.StatusTooManyRequests,
			},
			wantTerminalType: wsEventTypeError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			streamErrors := map[string]map[int]coreauth.Error{
				"auth-a": {
					2: {Code: "rate_limit_exceeded", Message: "stream failed", HTTPStatus: http.StatusTooManyRequests},
				},
			}
			if tt.historicalError != nil {
				streamErrors["auth-a"][3] = *tt.historicalError
			}
			executor := &websocketPinnedFailoverExecutor{streamErrors: streamErrors}
			conn := openResponsesWebsocketFailoverTest(
				t, "historical-owner-model", []string{"auth-a", "auth-b"}, executor, true,
			)

			requests := []string{
				`{"type":"response.create","model":"historical-owner-model","input":[{"type":"message","id":"msg-1"}]}`,
				`{"type":"response.append","previous_response_id":"resp-auth-a-1","input":[{"type":"message","id":"msg-2"}]}`,
				`{"type":"response.append","previous_response_id":"resp-auth-a-1","input":[{"type":"message","id":"msg-2"}]}`,
				`{"type":"response.append","previous_response_id":"resp-auth-a-1","input":[{"type":"message","id":"branch-msg"}]}`,
			}
			wantTypes := []string{wsEventTypeDone, wsEventTypeError, wsEventTypeDone, tt.wantTerminalType}
			for i, request := range requests {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
					t.Fatalf("write websocket request %d: %v", i+1, err)
				}
				if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
					t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
				}
			}

			if got := executor.AuthIDs(); len(got) != 4 ||
				got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-b" || got[3] != "auth-a" {
				t.Fatalf("selected auth IDs = %v, want [auth-a auth-a auth-b auth-a]", got)
			}
			authAPayloads := executor.Payloads("auth-a")
			if got := gjson.GetBytes(authAPayloads[2], "previous_response_id").String(); got != "resp-auth-a-1" {
				t.Fatalf("historical previous_response_id = %q, want resp-auth-a-1: %s", got, authAPayloads[2])
			}
			input := gjson.GetBytes(authAPayloads[2], "input").Array()
			if len(input) != 1 || input[0].Get("id").String() != "branch-msg" {
				t.Fatalf("historical branch input = %s, want branch-msg only", gjson.GetBytes(authAPayloads[2], "input").Raw)
			}
			if payloads := executor.Payloads("auth-b"); len(payloads) != 1 {
				t.Fatalf("auth-b payload count = %d, want 1", len(payloads))
			}
		})
	}
}

func TestResponsesWebsocketExternalResponseIDOwnerSurvivesBranchSwitch(t *testing.T) {
	for _, firstExternalType := range []string{wsEventTypeDone, wsEventTypeError} {
		t.Run(firstExternalType, func(t *testing.T) {
			streamErrors := map[string]map[int]coreauth.Error{
				"auth-a": {
					2: {Code: "rate_limit_exceeded", Message: "stream failed", HTTPStatus: http.StatusTooManyRequests},
				},
			}
			if firstExternalType == wsEventTypeError {
				streamErrors["auth-b"] = map[int]coreauth.Error{
					2: {Code: "upstream_error", Message: "stream failed", HTTPStatus: http.StatusInternalServerError},
				}
			}
			executor := &websocketPinnedFailoverExecutor{streamErrors: streamErrors}
			conn := openResponsesWebsocketFailoverTest(
				t, "external-owner-history-model", []string{"auth-a", "auth-b"}, executor, true,
			)

			requests := []string{
				`{"type":"response.create","model":"external-owner-history-model","input":[{"type":"message","id":"msg-1"}]}`,
				`{"type":"response.append","previous_response_id":"resp-auth-a-1","input":[{"type":"message","id":"msg-2"}]}`,
				`{"type":"response.append","previous_response_id":"resp-auth-a-1","input":[{"type":"message","id":"msg-2"}]}`,
				`{"type":"response.append","previous_response_id":"resp-external-x","input":[{"type":"message","id":"external-1"}]}`,
				`{"type":"response.append","previous_response_id":"resp-auth-a-1","input":[{"type":"message","id":"branch-a"}]}`,
				`{"type":"response.append","previous_response_id":"resp-external-x","input":[{"type":"message","id":"external-2"}]}`,
			}
			wantTypes := []string{
				wsEventTypeDone,
				wsEventTypeError,
				wsEventTypeDone,
				firstExternalType,
				wsEventTypeDone,
				wsEventTypeDone,
			}
			for i, request := range requests {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
					t.Fatalf("write websocket request %d: %v", i+1, err)
				}
				if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
					t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
				}
			}

			if got := executor.AuthIDs(); len(got) != 6 ||
				got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-b" ||
				got[3] != "auth-b" || got[4] != "auth-a" || got[5] != "auth-b" {
				t.Fatalf("selected auth IDs = %v, want [auth-a auth-a auth-b auth-b auth-a auth-b]", got)
			}
			authBPayloads := executor.Payloads("auth-b")
			lastPayload := authBPayloads[len(authBPayloads)-1]
			if got := gjson.GetBytes(lastPayload, "previous_response_id").String(); got != "resp-external-x" {
				t.Fatalf("external previous_response_id = %q, want resp-external-x: %s", got, lastPayload)
			}
			input := gjson.GetBytes(lastPayload, "input").Array()
			if len(input) != 1 || input[0].Get("id").String() != "external-2" {
				t.Fatalf("external branch input = %s, want external-2 only", gjson.GetBytes(lastPayload, "input").Raw)
			}
		})
	}
}

func TestResponsesWebsocketExternalErrorCanReturnToCommittedOwner(t *testing.T) {
	executor := &websocketPinnedFailoverExecutor{
		streamErrors: map[string]map[int]coreauth.Error{
			"auth-a": {
				2: {Code: "rate_limit_exceeded", Message: "stream failed", HTTPStatus: http.StatusTooManyRequests},
			},
			"auth-b": {
				1: {Code: "upstream_error", Message: "stream failed", HTTPStatus: http.StatusInternalServerError},
			},
		},
	}
	conn := openResponsesWebsocketFailoverTest(
		t, "external-return-model", []string{"auth-a", "auth-b"}, executor, true,
	)

	requests := []string{
		`{"type":"response.create","model":"external-return-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"msg-2"}]}`,
		`{"type":"response.append","previous_response_id":"resp-external-b","input":[{"type":"message","id":"external-msg"}]}`,
		`{"type":"response.append","previous_response_id":"resp-auth-a-1","input":[{"type":"message","id":"msg-3"}]}`,
	}
	wantTypes := []string{wsEventTypeDone, wsEventTypeError, wsEventTypeError, wsEventTypeDone}
	for i, request := range requests {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatalf("write websocket request %d: %v", i+1, err)
		}
		if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
			t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
		}
	}

	if got := executor.AuthIDs(); len(got) != 4 ||
		got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-b" || got[3] != "auth-a" {
		t.Fatalf("selected auth IDs = %v, want [auth-a auth-a auth-b auth-a]", got)
	}
	authAPayloads := executor.Payloads("auth-a")
	if got := gjson.GetBytes(authAPayloads[2], "previous_response_id").String(); got != "resp-auth-a-1" {
		t.Fatalf("returned previous_response_id = %q, want resp-auth-a-1: %s", got, authAPayloads[2])
	}
}

func TestResponsesWebsocketExternalErrorImplicitlyReturnsToCommittedOwner(t *testing.T) {
	executor := &websocketPinnedFailoverExecutor{
		streamErrors: map[string]map[int]coreauth.Error{
			"auth-a": {
				2: {Code: "rate_limit_exceeded", Message: "stream failed", HTTPStatus: http.StatusTooManyRequests},
			},
			"auth-b": {
				1: {Code: "upstream_error", Message: "stream failed", HTTPStatus: http.StatusInternalServerError},
			},
		},
	}
	conn := openResponsesWebsocketFailoverTest(
		t, "external-implicit-return-model", []string{"auth-a", "auth-b"}, executor, true,
	)

	requests := []string{
		`{"type":"response.create","model":"external-implicit-return-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"msg-2"}]}`,
		`{"type":"response.append","previous_response_id":"resp-external-b","input":[{"type":"message","id":"external-msg"}]}`,
		`{"type":"response.append","input":[{"type":"message","id":"msg-3"}]}`,
	}
	wantTypes := []string{wsEventTypeDone, wsEventTypeError, wsEventTypeError, wsEventTypeDone}
	for i, request := range requests {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatalf("write websocket request %d: %v", i+1, err)
		}
		if got := readResponsesWebsocketTerminalType(t, conn); got != wantTypes[i] {
			t.Fatalf("response %d type = %s, want %s", i+1, got, wantTypes[i])
		}
	}

	if got := executor.AuthIDs(); len(got) != 4 ||
		got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-b" || got[3] != "auth-a" {
		t.Fatalf("selected auth IDs = %v, want [auth-a auth-a auth-b auth-a]", got)
	}
	authAPayloads := executor.Payloads("auth-a")
	if got := gjson.GetBytes(authAPayloads[2], "previous_response_id").String(); got != "resp-auth-a-1" {
		t.Fatalf("implicit previous_response_id = %q, want resp-auth-a-1: %s", got, authAPayloads[2])
	}
}

func TestResponsesWebsocketReleasesPinnedAuthAfterQuotaError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	selector := &websocketFailoverSelector{order: []string{"auth-a", "auth-b"}}
	executor := &websocketPinnedFailoverExecutor{
		streamErrorAuthID: "auth-a",
		streamErrorCall:   2,
		streamErrorCode:   "rate_limit_exceeded",
		streamErrorStatus: http.StatusTooManyRequests,
	}
	manager := coreauth.NewManager(nil, selector, nil)
	manager.RegisterExecutor(executor)

	authA := &coreauth.Auth{
		ID:         "auth-a",
		Provider:   executor.Identifier(),
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"websockets": "true"},
	}
	if _, err := manager.Register(context.Background(), authA); err != nil {
		t.Fatalf("Register auth A: %v", err)
	}
	authB := &coreauth.Auth{
		ID:         "auth-b",
		Provider:   executor.Identifier(),
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"websockets": "true"},
	}
	if _, err := manager.Register(context.Background(), authB); err != nil {
		t.Fatalf("Register auth B: %v", err)
	}

	registry.GetGlobalRegistry().RegisterClient(authA.ID, authA.Provider, []*registry.ModelInfo{{ID: "quota-model"}})
	registry.GetGlobalRegistry().RegisterClient(authB.ID, authB.Provider, []*registry.ModelInfo{{ID: "quota-model"}})
	t.Cleanup(
		func() {
			registry.GetGlobalRegistry().UnregisterClient(authA.ID)
			registry.GetGlobalRegistry().UnregisterClient(authB.ID)
		},
	)

	base := handlers.NewBaseAPIHandlers(
		&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: 0}}, manager,
	)
	h := NewResponsesAPIHandler(base)
	router := gin.New()
	router.GET("/v1/responses/ws", h.ResponsesWebsocket)

	server := httptest.NewServer(router)
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses/ws", nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Fatalf("close websocket: %v", errClose)
		}
	}()

	requests := []string{
		`{"type":"response.create","model":"quota-model","input":[{"type":"message","id":"msg-1"}]}`,
		`{"type":"response.create","previous_response_id":"resp-auth-a-1","input":[{"type":"message","id":"msg-2"}]}`,
		`{"type":"response.create","previous_response_id":"resp-auth-a-1","input":[{"type":"message","id":"msg-3"}]}`,
	}
	wantTypes := []string{wsEventTypeDone, wsEventTypeError, wsEventTypeDone}
	for i := range requests {
		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(requests[i])); errWrite != nil {
			t.Fatalf("write websocket message %d: %v", i+1, errWrite)
		}
		var payload []byte
		for {
			_, gotPayload, errReadMessage := conn.ReadMessage()
			if errReadMessage != nil {
				t.Fatalf("read websocket message %d: %v", i+1, errReadMessage)
			}
			payload = gotPayload
			gotType := gjson.GetBytes(payload, "type").String()
			if gotType == wsEventTypeDone || gotType == wsEventTypeError {
				break
			}
		}
		if got := gjson.GetBytes(payload, "type").String(); got != wantTypes[i] {
			t.Fatalf("message %d payload type = %s, want %s: %s", i+1, got, wantTypes[i], payload)
		}
		if i == 1 && int(gjson.GetBytes(payload, "status").Int()) != http.StatusTooManyRequests {
			t.Fatalf(
				"quota payload status = %d, want %d: %s", gjson.GetBytes(payload, "status").Int(),
				http.StatusTooManyRequests, payload,
			)
		}
	}

	if got := executor.AuthIDs(); len(got) != 3 || got[0] != "auth-a" || got[1] != "auth-a" || got[2] != "auth-b" {
		t.Fatalf("selected auth IDs = %v, want [auth-a auth-a auth-b]", got)
	}

	authBPayloads := executor.Payloads("auth-b")
	if len(authBPayloads) != 1 {
		t.Fatalf("auth-b payload count = %d, want 1", len(authBPayloads))
	}
	authBPayload := authBPayloads[0]
	if gjson.GetBytes(authBPayload, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id leaked after auth failover: %s", authBPayload)
	}
	authBInput := gjson.GetBytes(authBPayload, "input").Raw
	if !strings.Contains(authBInput, `"id":"msg-1"`) || !strings.Contains(authBInput, `"id":"msg-3"`) {
		t.Fatalf("auth-b replay input missing expected transcript items: %s", authBInput)
	}
}

func TestShouldReleaseResponsesWebsocketPinnedAuthWhenPinnedAuthDisappears(t *testing.T) {
	errMsg := &handlers.ErrorMessage{
		StatusCode: http.StatusConflict,
		Error: &coreauth.Error{
			Code:       "pinned_auth_not_found",
			Message:    "pinned auth unavailable",
			HTTPStatus: http.StatusConflict,
		},
	}
	if !shouldReleaseResponsesWebsocketPinnedAuth(errMsg) {
		t.Fatal("pinned auth disappearance must release continuation ownership")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDIncremental(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "type").Exists() {
		t.Fatalf("normalized request must not include type field")
	}
	if gjson.GetBytes(normalized, "previous_response_id").String() != "resp-1" {
		t.Fatalf("previous_response_id must be preserved in incremental mode")
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 1 {
		t.Fatalf("incremental input len = %d, want 1", len(input))
	}
	if input[0].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected incremental input item id: %s", input[0].Get("id").String())
	}
	if gjson.GetBytes(normalized, "model").String() != "test-model" {
		t.Fatalf("unexpected model: %s", gjson.GetBytes(normalized, "model").String())
	}
	if gjson.GetBytes(normalized, "instructions").String() != "be helpful" {
		t.Fatalf("unexpected instructions: %s", gjson.GetBytes(normalized, "instructions").String())
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestInjectsPreviousResponseIDForIncremental(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithLastResponseID(
		raw, lastRequest, lastResponseOutput, "resp-1", true,
	)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(normalized, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %q, want resp-1", got)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 1 || input[0].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected incremental input: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketAppendInjectsPreviousResponseIDForIncremental(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	raw := []byte(`{"type":"response.append","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithIncrementalState(
		raw, lastRequest, []byte(`[]`), "resp-1", []string{"call-1"}, true,
	)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(normalized, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %q, want resp-1", got)
	}
	if gjson.GetBytes(normalized, "type").Exists() {
		t.Fatalf("normalized append request must not include type field: %s", normalized)
	}
	if got := gjson.GetBytes(normalized, "model").String(); got != "test-model" {
		t.Fatalf("model = %q, want test-model", got)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 1 || input[0].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected incremental input: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestInjectsPreviousResponseIDWhenPendingOutputIsPresent(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, _, errMsg := normalizeResponsesWebsocketRequestWithIncrementalState(
		raw, lastRequest, lastResponseOutput, "resp-1", []string{"call-1"}, true,
	)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if got := gjson.GetBytes(normalized, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %q, want resp-1", got)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 1 || input[0].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected incremental input: %s", normalized)
	}
}

func TestNormalizeResponsesWebsocketRequestSkipsPreviousResponseIDWhenPendingOutputIsMissing(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"instructions":"be helpful","input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"}
	]`)
	raw := []byte(`{"type":"response.create","input":[{"type":"message","role":"user","id":"summary-1","content":"compacted summary"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithIncrementalState(
		raw, lastRequest, lastResponseOutput, "resp-1", []string{"call-1"}, true,
	)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must not be injected when pending tool output is missing: %s", normalized)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 1 || input[0].Get("id").String() != "summary-1" {
		t.Fatalf("unexpected replacement input: %s", normalized)
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match replacement request")
	}
}

func TestNormalizeResponsesWebsocketRequestWithPreviousResponseIDMergedWhenIncrementalDisabled(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"function_call","id":"fc-1","call_id":"call-1"},
		{"type":"message","id":"assistant-1"}
	]`)
	raw := []byte(`{"type":"response.create","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call-1","id":"tool-out-1"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, false)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	if gjson.GetBytes(normalized, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id must be removed when incremental mode is disabled")
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 4 {
		t.Fatalf("merged input len = %d, want 4", len(input))
	}
	if input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "fc-1" ||
		input[2].Get("id").String() != "assistant-1" ||
		input[3].Get("id").String() != "tool-out-1" {
		t.Fatalf("unexpected merged input order")
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized request")
	}
}

func TestNormalizeResponsesWebsocketRequestAppend(t *testing.T) {
	lastRequest := []byte(`{"model":"test-model","stream":true,"input":[{"type":"message","id":"msg-1"}]}`)
	lastResponseOutput := []byte(`[
		{"type":"message","id":"assistant-1"},
		{"type":"function_call_output","id":"tool-out-1"}
	]`)
	raw := []byte(`{"type":"response.append","input":[{"type":"message","id":"msg-2"},{"type":"message","id":"msg-3"}]}`)

	normalized, next, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true)
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	input := gjson.GetBytes(normalized, "input").Array()
	if len(input) != 5 {
		t.Fatalf("merged input len = %d, want 5", len(input))
	}
	if input[0].Get("id").String() != "msg-1" ||
		input[1].Get("id").String() != "assistant-1" ||
		input[2].Get("id").String() != "tool-out-1" ||
		input[3].Get("id").String() != "msg-2" ||
		input[4].Get("id").String() != "msg-3" {
		t.Fatalf("unexpected merged input order")
	}
	if !bytes.Equal(next, normalized) {
		t.Fatalf("next request snapshot should match normalized append request")
	}
}

func TestNormalizeResponsesWebsocketRequestAppendWithoutCreate(t *testing.T) {
	raw := []byte(`{"type":"response.append","input":[]}`)

	_, _, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, nil, nil, true)
	if errMsg == nil {
		t.Fatalf("expected error for append without previous request")
	}
	if errMsg.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", errMsg.StatusCode, http.StatusBadRequest)
	}
}

func TestWebsocketJSONPayloadsFromChunk(t *testing.T) {
	chunk := []byte("event: response.created\n\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\ndata: [DONE]\n")

	payloads := websocketJSONPayloadsFromChunk(chunk)
	if len(payloads) != 1 {
		t.Fatalf("payloads len = %d, want 1", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "type").String() != "response.created" {
		t.Fatalf("unexpected payload type: %s", gjson.GetBytes(payloads[0], "type").String())
	}
}

func TestWebsocketJSONPayloadsFromPlainJSONChunk(t *testing.T) {
	chunk := []byte(`{"type":"response.completed","response":{"id":"resp-1"}}`)

	payloads := websocketJSONPayloadsFromChunk(chunk)
	if len(payloads) != 1 {
		t.Fatalf("payloads len = %d, want 1", len(payloads))
	}
	if gjson.GetBytes(payloads[0], "type").String() != "response.completed" {
		t.Fatalf("unexpected payload type: %s", gjson.GetBytes(payloads[0], "type").String())
	}
}

func TestResponseCompletedOutputFromPayload(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"message","id":"out-1"}]}}`)

	output := responseCompletedOutputFromPayload(payload, nil, nil)
	items := gjson.ParseBytes(output).Array()
	if len(items) != 1 {
		t.Fatalf("output len = %d, want 1", len(items))
	}
	if items[0].Get("id").String() != "out-1" {
		t.Fatalf("unexpected output id: %s", items[0].Get("id").String())
	}
}

func TestRestoreResponsesWebsocketCompletionOutputPreservesNonEmptyOutput(t *testing.T) {
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"message","id":"out-1"}]}}`)
	indexed := map[int64][]byte{0: []byte(`{"type":"function_call","id":"call-1","call_id":"call-1"}`)}

	restored := restoreResponsesWebsocketCompletionOutput(payload, indexed, nil)
	if string(restored) != string(payload) {
		t.Fatalf("non-empty completion output was overwritten: %s", restored)
	}
}

func TestResponsesWebsocketOutputCollectorRestoresCompletedOutput(t *testing.T) {
	indexed := make(map[int64][]byte)
	var fallback [][]byte
	payloads := [][]byte{
		[]byte(`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"reply-1","role":"assistant"}}`),
		[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"summary-1","summary":[]}}`),
		[]byte(`{"type":"response.output_item.done","item":{"type":"function_call","id":"call-1","call_id":"call-1"}}`),
	}
	for _, payload := range payloads {
		collectResponsesWebsocketOutputItem(payload, indexed, &fallback)
	}

	completion := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[]}}`)
	restored := restoreResponsesWebsocketCompletionOutput(completion, indexed, fallback)
	items := gjson.GetBytes(restored, "response.output").Array()
	if len(items) != 3 {
		t.Fatalf("restored output len = %d, want 3: %s", len(items), restored)
	}
	wantIDs := []string{"summary-1", "reply-1", "call-1"}
	for i, wantID := range wantIDs {
		if got := items[i].Get("id").String(); got != wantID {
			t.Fatalf("output[%d].id = %q, want %q: %s", i, got, wantID, restored)
		}
	}
}

func TestNormalizeResponsesWebsocketReasoningContinuationForCreateAndAppend(t *testing.T) {
	lastRequest := []byte(`{"model":"gpt-5.6-terra","stream":true,"input":[{"type":"message","role":"user","id":"old-user","content":"long history"}]}`)
	lastResponseOutput := []byte(`[{"type":"function_call","id":"old-call","call_id":"old-call","name":"lookup","arguments":"{}"}]`)

	for _, requestType := range []string{"response.create", "response.append"} {
		t.Run(requestType, func(t *testing.T) {
			raw := []byte(`{"type":"` + requestType + `","previous_response_id":"resp-1","input":[{"type":"reasoning","id":"reasoning-1","summary":[]},{"type":"function_call_output","id":"output-1","call_id":"old-call","output":"result"}]}`)
			normalized, _, errMsg := normalizeResponsesWebsocketRequestWithMode(raw, lastRequest, lastResponseOutput, true)
			if errMsg != nil {
				t.Fatalf("unexpected error: %v", errMsg.Error)
			}
			if got := gjson.GetBytes(normalized, "previous_response_id").String(); got != "resp-1" {
				t.Fatalf("previous_response_id = %q, want resp-1: %s", got, normalized)
			}
			input := gjson.GetBytes(normalized, "input").Array()
			if len(input) != 2 || input[0].Get("id").String() != "reasoning-1" || input[1].Get("id").String() != "output-1" {
				t.Fatalf("incremental continuation was replaced or merged: %s", normalized)
			}
		})
	}
}

func TestRecordPendingToolCallIDsFromPayloadDropsSatisfiedCalls(t *testing.T) {
	pending := map[string]struct{}{}
	payload := []byte(`{"type":"response.completed","response":{"output":[{"type":"function_call","call_id":"call-1","id":"fc-1"},{"type":"function_call_output","call_id":"call-1","id":"out-1"},{"type":"custom_tool_call","call_id":"call-2","id":"ctc-1"},{"type":"custom_tool_call_output","call_id":"call-2","id":"custom-out-1"}]}}`)

	recordPendingToolCallIDsFromPayload(pending, payload)

	if len(pending) != 0 {
		t.Fatalf("pending tool call ids = %v, want empty", sortedStringSet(pending))
	}
}

func TestRecordPendingToolCallIDsFromOutputItemEvents(t *testing.T) {
	pending := map[string]struct{}{}

	recordPendingToolCallIDsFromPayload(
		pending,
		[]byte(`{"type":"response.output_item.added","item":{"type":"function_call","call_id":"call-1","id":"fc-1"}}`),
	)
	if _, ok := pending["call-1"]; !ok {
		t.Fatalf("pending tool call ids = %v, want call-1", sortedStringSet(pending))
	}
	recordPendingToolCallIDsFromPayload(
		pending,
		[]byte(`{"type":"response.output_item.done","item":{"type":"function_call_output","call_id":"call-1","id":"out-1"}}`),
	)
	if len(pending) != 0 {
		t.Fatalf("pending tool call ids = %v, want empty after function output", sortedStringSet(pending))
	}

	recordPendingToolCallIDsFromPayload(
		pending,
		[]byte(`{"type":"response.output_item.added","item":{"type":"custom_tool_call","call_id":"call-2","id":"ctc-1"}}`),
	)
	if _, ok := pending["call-2"]; !ok {
		t.Fatalf("pending tool call ids = %v, want call-2", sortedStringSet(pending))
	}
	recordPendingToolCallIDsFromPayload(
		pending,
		[]byte(`{"type":"response.output_item.done","item":{"type":"custom_tool_call_output","call_id":"call-2","id":"custom-out-1"}}`),
	)
	if len(pending) != 0 {
		t.Fatalf("pending tool call ids = %v, want empty after custom output", sortedStringSet(pending))
	}
}

func TestDedupeResponsesWebsocketInputItemsByIDKeepsReferencedToolCall(t *testing.T) {
	payload := []byte(`{"input":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":"exec_command"},{"type":"function_call","id":"fc-1","call_id":"call-2","name":"exec_command"},{"type":"function_call_output","id":"fco-1","call_id":"call-1"}]}`)

	deduped := dedupeResponsesWebsocketInputItemsByID(payload)

	items := gjson.GetBytes(deduped, "input").Array()
	if len(items) != 2 {
		t.Fatalf("deduped input len = %d, want 2: %s", len(items), deduped)
	}
	if items[0].Get("id").String() != "fc-1" ||
		items[0].Get("call_id").String() != "call-1" ||
		items[1].Get("id").String() != "fco-1" ||
		items[1].Get("call_id").String() != "call-1" {
		t.Fatalf("unexpected deduped input: %s", deduped)
	}
}

type unreadableWebsocketTimelineSource struct {
	payload      bytes.Buffer
	appendCalls  int
	cleanupCalls int
}

func (s *unreadableWebsocketTimelineSource) AppendBytes(data []byte) error {
	s.appendCalls++
	if s.appendCalls > 1 {
		return errors.New("injected append failure")
	}
	_, errWrite := s.payload.Write(data)
	return errWrite
}

func (s *unreadableWebsocketTimelineSource) HasPayload() bool {
	return s.payload.Len() > 0
}

func (*unreadableWebsocketTimelineSource) WritePayloadTo(io.Writer) error {
	return errors.New("injected read failure")
}

func (s *unreadableWebsocketTimelineSource) Cleanup() error {
	s.cleanupCalls++
	return nil
}

type failingWebsocketTimelineSource struct {
	payload     bytes.Buffer
	appendCalls int
	cleaned     bool
}

func (s *failingWebsocketTimelineSource) AppendBytes(data []byte) error {
	s.appendCalls++
	if s.appendCalls > 1 {
		return errors.New("injected append failure")
	}
	_, errWrite := s.payload.Write(data)
	return errWrite
}

func (s *failingWebsocketTimelineSource) HasPayload() bool {
	return s.payload.Len() > 0 && !s.cleaned
}

func (s *failingWebsocketTimelineSource) WritePayloadTo(writer io.Writer) error {
	_, errWrite := writer.Write(util.MaskSensitivePayload(s.payload.Bytes()))
	return errWrite
}

func (s *failingWebsocketTimelineSource) Cleanup() error {
	s.cleaned = true
	return nil
}

func TestAppendWebsocketEvent(t *testing.T) {
	recorder := &websocketTimelineRecorder{}

	appendWebsocketEvent(recorder, "request", []byte("  {\"type\":\"response.create\"}\n"))
	appendWebsocketEvent(recorder, "response", []byte("{\"type\":\"response.created\"}"))

	got := recorder.String()
	if !strings.Contains(got, "websocket.request\n{\"type\":\"response.create\"}\n") {
		t.Fatalf("request event not found in body: %s", got)
	}
	if !strings.Contains(got, "websocket.response\n{\"type\":\"response.created\"}\n") {
		t.Fatalf("response event not found in body: %s", got)
	}
}

func TestAppendWebsocketEventBoundsMemoryFallback(t *testing.T) {
	recorder := &websocketTimelineRecorder{}
	appendWebsocketEvent(recorder, "response", bytes.Repeat([]byte("x"), 9<<20))

	if recorder.fallback == nil || !recorder.fallback.Truncated() {
		t.Fatal("oversized websocket fallback was not truncated")
	}
}

func TestAppendWebsocketEventWritesToFileBodySource(t *testing.T) {
	gin.SetMode(gin.TestMode)
	responseRecorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(responseRecorder)
	source, errSource := internallogging.NewFileBodySourceInDir(t.TempDir(), "websocket-timeline")
	if errSource != nil {
		t.Fatalf("create source: %v", errSource)
	}
	defer source.Cleanup()
	c.Set(internallogging.WebsocketTimelineSourceContextKey, source)
	recorder := newWebsocketTimelineRecorder(c)

	appendWebsocketEvent(recorder, "request", []byte(`{"type":"response.create"}`))
	appendWebsocketEvent(recorder, "response", []byte(`{"type":"response.created"}`))

	if got := recorder.String(); got != "" {
		t.Fatalf("memory fallback = %q, want empty", got)
	}
	got, errBytes := source.Bytes()
	if errBytes != nil {
		t.Fatalf("read source: %v", errBytes)
	}
	requestIndex := bytes.Index(got, []byte("websocket.request"))
	responseIndex := bytes.Index(got, []byte("websocket.response"))
	if requestIndex < 0 || responseIndex <= requestIndex {
		t.Fatalf("timeline order is incorrect: %s", got)
	}
}

func TestAppendWebsocketEventFallsBackAfterSourceFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	responseRecorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(responseRecorder)
	source, errSource := internallogging.NewFileBodySourceInDir(t.TempDir(), "websocket-timeline")
	if errSource != nil {
		t.Fatalf("create source: %v", errSource)
	}
	if errCleanup := source.Cleanup(); errCleanup != nil {
		t.Fatalf("clean source: %v", errCleanup)
	}
	c.Set(internallogging.WebsocketTimelineSourceContextKey, source)
	recorder := newWebsocketTimelineRecorder(c)

	appendWebsocketEvent(recorder, "request", []byte(`{"type":"response.create"}`))

	if recorder.source != nil {
		t.Fatalf("failed source should be released")
	}
	if got := recorder.String(); !strings.Contains(got, "websocket.request\n{\"type\":\"response.create\"}\n") {
		t.Fatalf("fallback event not found: %s", got)
	}
}

func TestAppendWebsocketEventRetainsFailedSourceCleanup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	responseRecorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(responseRecorder)
	source, errSource := internallogging.NewFileBodySourceInDir(t.TempDir(), "websocket-timeline")
	if errSource != nil {
		t.Fatalf("create source: %v", errSource)
	}
	c.Set(internallogging.WebsocketTimelineSourceContextKey, source)
	recorder := newWebsocketTimelineRecorder(c)
	appendWebsocketEvent(recorder, "request", []byte(`{"type":"response.create"}`))
	paths := source.Paths()
	if len(paths) != 1 {
		t.Fatalf("source paths = %v, want one path", paths)
	}
	partPath := paths[0]
	sourceDir := filepath.Dir(partPath)
	if errChmod := os.Chmod(partPath, 0o400); errChmod != nil {
		t.Fatalf("chmod source part: %v", errChmod)
	}
	if errChmod := os.Chmod(sourceDir, 0o500); errChmod != nil {
		t.Fatalf("chmod source directory: %v", errChmod)
	}
	defer func() {
		_ = os.Chmod(sourceDir, 0o700)
		_ = os.Chmod(partPath, 0o600)
		_ = source.Cleanup()
	}()

	appendWebsocketEvent(recorder, "response", []byte(`{"type":"response.created"}`))

	if recorder.source != nil {
		t.Fatal("failed source should be released")
	}
	if sourceAfter, exists := c.Get(internallogging.WebsocketTimelineSourceContextKey); !exists || sourceAfter != (*internallogging.FileBodySource)(nil) {
		t.Fatalf("context source = %#v, want typed nil", sourceAfter)
	}
	if recorder.cleanupQueue.Count() != 1 {
		t.Fatalf("cleanup queue count = %d, want 1", recorder.cleanupQueue.Count())
	}
	got := recorder.String()
	if strings.Count(got, `{"type":"response.create"}`) != 1 ||
		strings.Count(got, `{"type":"response.created"}`) != 1 {
		t.Fatalf("timeline events were duplicated or lost: %s", got)
	}
	if errChmod := os.Chmod(sourceDir, 0o700); errChmod != nil {
		t.Fatalf("restore source directory permissions: %v", errChmod)
	}
	if errChmod := os.Chmod(partPath, 0o600); errChmod != nil {
		t.Fatalf("restore source part permissions: %v", errChmod)
	}
	if errCleanup := recorder.cleanupQueue.Cleanup(); errCleanup != nil {
		t.Fatalf("retry cleanup queue: %v", errCleanup)
	}
}

func TestAppendWebsocketEventRetainsSourceWhenMigrationReadFails(t *testing.T) {
	source := &unreadableWebsocketTimelineSource{}
	recorder := &websocketTimelineRecorder{source: source}

	appendWebsocketEvent(recorder, "request", []byte(`{"type":"response.create"}`))
	appendWebsocketEvent(recorder, "response", []byte(`{"type":"response.created"}`))

	if recorder.source != source {
		t.Fatal("unreadable source ownership was not retained")
	}
	if !recorder.sourceFailed {
		t.Fatal("source failure state was not retained")
	}
	if source.cleanupCalls != 0 {
		t.Fatalf("source cleanup calls = %d, want 0", source.cleanupCalls)
	}
	got := recorder.String()
	if strings.Contains(got, `{"type":"response.create"}`) ||
		!strings.Contains(got, `{"type":"response.created"}`) {
		t.Fatalf("fallback suffix = %q", got)
	}
}

func TestAppendWebsocketEventMigratesExistingSourceBeforeFallback(t *testing.T) {
	source := &failingWebsocketTimelineSource{}
	recorder := &websocketTimelineRecorder{source: source}

	appendWebsocketEvent(
		recorder,
		"request",
		[]byte(`{"type":"response.create","api_key":"source-secret","model":"gpt-5"}`),
	)
	appendWebsocketEvent(
		recorder,
		"response",
		[]byte(`{"type":"response.created","authorization":"Bearer response-secret","status":"ok"}`),
	)

	if recorder.source != nil {
		t.Fatal("failed source should be released")
	}
	if !source.cleaned {
		t.Fatal("failed source was not cleaned")
	}
	masked := util.MaskSensitivePayload([]byte(recorder.String()))
	requestIndex := bytes.Index(masked, []byte("websocket.request"))
	responseIndex := bytes.Index(masked, []byte("websocket.response"))
	if requestIndex < 0 || responseIndex <= requestIndex {
		t.Fatalf("timeline order is incorrect: %s", masked)
	}
	for _, secret := range []string{"source-secret", "response-secret"} {
		if bytes.Contains(masked, []byte(secret)) {
			t.Fatalf("timeline kept secret %q: %s", secret, masked)
		}
	}
	for _, visible := range []string{"response.create", "gpt-5", "response.created", "ok"} {
		if !bytes.Contains(masked, []byte(visible)) {
			t.Fatalf("timeline removed %q: %s", visible, masked)
		}
	}
}

func TestSetWebsocketTimelineBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	setWebsocketTimelineBody(c, " \n ")
	if _, exists := c.Get(wsTimelineBodyKey); exists {
		t.Fatalf("timeline key should not be set for empty body")
	}

	setWebsocketTimelineBody(c, "event body")
	value, exists := c.Get(wsTimelineBodyKey)
	if !exists {
		t.Fatalf("timeline key not set")
	}
	bodyBytes, ok := value.([]byte)
	if !ok {
		t.Fatalf("timeline key type mismatch")
	}
	if string(bodyBytes) != "event body" {
		t.Fatalf("timeline body = %q, want %q", string(bodyBytes), "event body")
	}
}
