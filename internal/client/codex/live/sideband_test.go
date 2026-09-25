package live

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/clienterror"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

type sidebandUpstreamCapture struct {
	headers chan http.Header
	paths   chan string
	pings   chan string
}

func newSidebandUpstreamServer(t *testing.T, subprotocol string) (*httptest.Server, *sidebandUpstreamCapture) {
	t.Helper()
	capture := &sidebandUpstreamCapture{
		headers: make(chan http.Header, 1),
		paths:   make(chan string, 1),
		pings:   make(chan string, 1),
	}
	server := httptest.NewServer(
		http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				upgrader := websocket.Upgrader{
					Subprotocols: []string{subprotocol},
					CheckOrigin:  func(*http.Request) bool { return true },
				}
				conn, errUpgrade := upgrader.Upgrade(writer, request, nil)
				if errUpgrade != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				capture.headers <- request.Header.Clone()
				capture.paths <- request.URL.RequestURI()
				conn.SetPingHandler(
					func(payload string) error {
						capture.pings <- payload
						return conn.WriteControl(
							websocket.PongMessage,
							[]byte(payload),
							time.Now().Add(websocketControlTimeout),
						)
					},
				)
				for {
					messageType, payload, errRead := conn.ReadMessage()
					if errRead != nil {
						return
					}
					response := append([]byte("echo:"), payload...)
					if errWrite := conn.WriteMessage(messageType, response); errWrite != nil {
						return
					}
				}
			},
		),
	)
	return server, capture
}

func websocketURL(serverURL, path string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http") + path
}

func websocketUpgradeRequest(method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	return req
}

func waitForSessionRemoval(t *testing.T, handler *Handler, callID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := handler.sessions.peek(callID); !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("live session %q was not removed", callID)
}

func TestHandleSidebandPinsAuthAndRelaysFrames(t *testing.T) {
	upstreamServer, capture := newSidebandUpstreamServer(t, "live.v1")
	defer upstreamServer.Close()

	manager := auth.NewManager(nil, nil, nil)
	executor := &captureExecutor{}
	manager.RegisterExecutor(executor)
	registerCredential(
		t, manager, &auth.Auth{
			ID:       "other-oauth",
			Provider: "codex",
			Status:   auth.StatusActive,
			Metadata: map[string]any{"access_token": "other-token", "account_id": "other-account"},
		},
	)
	registerCredential(
		t, manager, &auth.Auth{
			ID:       "pinned-oauth",
			Provider: "codex",
			Status:   auth.StatusActive,
			Metadata: map[string]any{"access_token": "pinned-token", "account_id": "pinned-account"},
		},
	)

	handler := NewHandler(manager, nil)
	handler.sidebandAPIBaseURL = websocketURL(upstreamServer.URL, "/v1")
	handler.sessions.put("call-sideband", liveSession{authID: "pinned-oauth", model: defaultLiveModel})
	router := http.NewServeMux()
	ginRouter := newSidebandGinRouter(handler)
	router.Handle("/", ginRouter)
	downstreamServer := httptest.NewServer(router)
	defer downstreamServer.Close()

	dialer := websocket.Dialer{Subprotocols: []string{"live.v1"}}
	headers := http.Header{
		"OpenAI-Alpha":      []string{"quicksilver=v2"},
		"X-Oai-Attestation": []string{"attestation-token"},
	}
	client, response, errDial := dialer.Dial(
		websocketURL(downstreamServer.URL, "/v1/live/call-sideband"),
		headers,
	)
	if errDial != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial downstream sideband: %v", errDial)
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	defer func() { _ = client.Close() }()
	if client.Subprotocol() != "live.v1" {
		t.Fatalf("downstream subprotocol = %q, want live.v1", client.Subprotocol())
	}

	pongPayload := make(chan string, 1)
	client.SetPongHandler(
		func(payload string) error {
			pongPayload <- payload
			return nil
		},
	)
	if errPing := client.WriteControl(
		websocket.PingMessage,
		[]byte("health"),
		time.Now().Add(websocketControlTimeout),
	); errPing != nil {
		t.Fatalf("write sideband ping: %v", errPing)
	}
	if errWrite := client.WriteMessage(websocket.TextMessage, []byte("text")); errWrite != nil {
		t.Fatalf("write sideband text: %v", errWrite)
	}
	messageType, payload, errRead := client.ReadMessage()
	if errRead != nil {
		t.Fatalf("read sideband text: %v", errRead)
	}
	if messageType != websocket.TextMessage || string(payload) != "echo:text" {
		t.Fatalf("sideband text type=%d payload=%q", messageType, payload)
	}
	if errWrite := client.WriteMessage(websocket.BinaryMessage, []byte{0, 1, 2}); errWrite != nil {
		t.Fatalf("write sideband binary: %v", errWrite)
	}
	messageType, payload, errRead = client.ReadMessage()
	if errRead != nil {
		t.Fatalf("read sideband binary: %v", errRead)
	}
	if messageType != websocket.BinaryMessage || !bytes.Equal(payload, []byte("echo:\x00\x01\x02")) {
		t.Fatalf("sideband binary type=%d payload=%q", messageType, payload)
	}

	select {
	case ping := <-capture.pings:
		if ping != "health" {
			t.Fatalf("upstream ping payload = %q", ping)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive the sideband ping")
	}
	select {
	case pong := <-pongPayload:
		if pong != "health" {
			t.Fatalf("downstream pong payload = %q", pong)
		}
	case <-time.After(time.Second):
		t.Fatal("downstream did not receive the upstream pong")
	}

	select {
	case captured := <-capture.headers:
		if got := captured.Get("Authorization"); got != "Bearer pinned-token" {
			t.Fatalf("upstream Authorization = %q, want pinned OAuth token", got)
		}
		if got := captured.Get("Chatgpt-Account-Id"); got != "pinned-account" {
			t.Fatalf("upstream Chatgpt-Account-Id = %q, want pinned-account", got)
		}
		if got := captured.Get("OpenAI-Alpha"); got != "quicksilver=v2" {
			t.Fatalf("upstream OpenAI-Alpha = %q", got)
		}
		if got := captured.Get("Sec-WebSocket-Protocol"); got != "live.v1" {
			t.Fatalf("upstream subprotocol offer = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream sideband headers were not captured")
	}
	select {
	case path := <-capture.paths:
		if path != "/v1/live/call-sideband" {
			t.Fatalf("upstream sideband path = %q", path)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream sideband path was not captured")
	}

	if errClose := client.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"),
		time.Now().Add(websocketControlTimeout),
	); errClose != nil {
		t.Fatalf("close downstream sideband: %v", errClose)
	}
	_, _, _ = client.ReadMessage()
	waitForSessionRemoval(t, handler, "call-sideband")
}

func newSidebandGinRouter(handler *Handler) http.Handler {
	router := newTestGinEngine()
	router.GET("/v1/live/:call_id", handler.HandleSideband)
	router.GET("/v1/realtime/calls/:call_id", handler.HandleSideband)
	router.GET("/v1/realtime", handler.HandleSideband)
	return router
}

func newTestGinEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	return gin.New()
}

func TestHandleSidebandReusesRetainedHomeSelection(t *testing.T) {
	upstreamServer, capture := newSidebandUpstreamServer(t, "live.home")
	defer upstreamServer.Close()

	manager := auth.NewManager(nil, nil, nil)
	manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
	dispatcher := &homeDispatcher{}
	registry := executionregistry.New()
	manager.PublishHomeDispatch(dispatcher, registry, 1)
	manager.RegisterExecutor(
		&captureExecutor{
			responseBody: &trackedResponseBody{Reader: strings.NewReader("v=0\r\n")},
		},
	)
	handler := NewHandler(manager, nil)
	handler.sidebandAPIBaseURL = websocketURL(upstreamServer.URL, "/v1")
	router := newTestGinEngine()
	router.POST("/v1/live", handler.Handle)
	sidebandDone := make(chan struct{})
	router.GET("/v1/live/:call_id", func(c *gin.Context) {
		handler.HandleSideband(c)
		close(sidebandDone)
	})
	server := httptest.NewServer(router)
	defer server.Close()

	bootstrapRecorder := httptest.NewRecorder()
	bootstrapRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/live",
		strings.NewReader(`{"model":"home-live-model"}`),
	)
	bootstrapRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(bootstrapRecorder, bootstrapRequest)
	if bootstrapRecorder.Code != http.StatusCreated {
		t.Fatalf("bootstrap status = %d; body=%s", bootstrapRecorder.Code, bootstrapRecorder.Body.String())
	}
	stored, ok := handler.sessions.peek("call-123")
	if !ok || stored.homeSelection == nil || !stored.homeSelection.Active() {
		t.Fatalf("stored Home session = %#v, ok=%t", stored, ok)
	}
	if calls := dispatcher.calls.Load(); calls != 1 {
		t.Fatalf("Home dispatch calls after bootstrap = %d, want 1", calls)
	}

	dialer := websocket.Dialer{Subprotocols: []string{"live.home"}}
	client, response, errDial := dialer.Dial(websocketURL(server.URL, "/v1/live/call-123"), nil)
	if errDial != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		t.Fatalf("dial Home sideband: %v", errDial)
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if errWrite := client.WriteMessage(websocket.TextMessage, []byte("home")); errWrite != nil {
		t.Fatalf("write Home sideband: %v", errWrite)
	}
	_, payload, errRead := client.ReadMessage()
	if errRead != nil {
		t.Fatalf("read Home sideband: %v", errRead)
	}
	if string(payload) != "echo:home" {
		t.Fatalf("Home sideband payload = %q", payload)
	}
	_ = client.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"),
		time.Now().Add(websocketControlTimeout),
	)
	_, _, _ = client.ReadMessage()
	_ = client.Close()

	select {
	case <-sidebandDone:
	case <-time.After(time.Second):
		t.Fatal("Home sideband handler did not complete")
	}
	if _, ok := handler.sessions.peek("call-123"); ok {
		t.Fatal("Home live session was not removed")
	}
	if calls := dispatcher.calls.Load(); calls != 1 {
		t.Fatalf("Home dispatch calls after sideband = %d, want 1", calls)
	}
	if stored.homeSelection.Active() {
		t.Fatal("Home selection remained active after sideband completion")
	}
	if errDrain := registry.Drain(context.Background()); errDrain != nil {
		t.Fatalf("Drain() error = %v", errDrain)
	}
	select {
	case captured := <-capture.headers:
		if got := captured.Get("Authorization"); got != "Bearer home-live-token" {
			t.Fatalf("Home upstream Authorization = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Home upstream headers were not captured")
	}
}

func TestHandleSidebandValidatesBeforeClaim(t *testing.T) {
	handler := NewHandler(auth.NewManager(nil, nil, nil), nil)
	handler.sessions.put("call-1", liveSession{authID: "auth-1", model: defaultLiveModel})
	router := newSidebandGinRouter(handler)

	tests := []struct {
		name   string
		target string
		status int
	}{
		{name: "plain GET", target: "/v1/live/call-1", status: http.StatusUpgradeRequired},
		{name: "invalid call ID", target: "/v1/realtime?call_id=bad%20id", status: http.StatusBadRequest},
		{name: "missing session", target: "/v1/live/call-missing", status: http.StatusNotFound},
	}
	for _, test := range tests {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, test.target, nil)
		if test.name != "plain GET" {
			req = websocketUpgradeRequest(http.MethodGet, test.target)
		}
		router.ServeHTTP(recorder, req)
		if recorder.Code != test.status {
			t.Fatalf("%s status = %d, want %d", test.name, recorder.Code, test.status)
		}
	}

	session, claim := handler.sessions.claim("call-1")
	if claim != sessionClaimAcquired {
		t.Fatalf("session claim after rejected request = %v, want acquired", claim)
	}
	handler.sessions.complete(session, "test_complete")
}

func TestHandleSidebandRejectsBusySession(t *testing.T) {
	handler := NewHandler(auth.NewManager(nil, nil, nil), nil)
	handler.sessions.put("call-busy", liveSession{authID: "auth-1", model: defaultLiveModel})
	session, claim := handler.sessions.claim("call-busy")
	if claim != sessionClaimAcquired {
		t.Fatalf("initial claim = %v, want acquired", claim)
	}

	recorder := httptest.NewRecorder()
	newSidebandGinRouter(handler).ServeHTTP(
		recorder,
		websocketUpgradeRequest(http.MethodGet, "/v1/live/call-busy"),
	)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("busy sideband status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	handler.sessions.complete(session, "test_complete")
}

func TestHandleSidebandDialFailureReleasesSession(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen: %v", errListen)
	}
	closedAddress := listener.Addr().String()
	if errClose := listener.Close(); errClose != nil {
		t.Fatalf("close listener: %v", errClose)
	}

	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{})
	registerCredential(
		t, manager, &auth.Auth{
			ID:       "pinned-oauth",
			Provider: "codex",
			Status:   auth.StatusActive,
			Metadata: map[string]any{"access_token": "pinned-token"},
		},
	)
	cfg := &config.Config{}
	cfg.ProxyURL = "direct"
	handler := NewHandler(manager, func() *config.Config { return cfg })
	handler.sidebandAPIBaseURL = "ws://" + closedAddress + "/v1"
	handler.sessions.put("call-retry", liveSession{authID: "pinned-oauth", model: defaultLiveModel})
	server := httptest.NewServer(newSidebandGinRouter(handler))
	defer server.Close()

	client, response, errDial := websocket.DefaultDialer.Dial(
		websocketURL(server.URL, "/v1/live/call-retry"),
		nil,
	)
	if client != nil {
		_ = client.Close()
	}
	if errDial == nil {
		t.Fatal("sideband dial unexpectedly succeeded")
	}
	if response == nil || response.StatusCode != http.StatusBadGateway {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("downstream handshake status = %d, want %d", status, http.StatusBadGateway)
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}

	session, claim := handler.sessions.claim("call-retry")
	if claim != sessionClaimAcquired {
		t.Fatalf("session claim after dial failure = %v, want acquired", claim)
	}
	handler.sessions.complete(session, "test_complete")
}

func TestSessionStoreLifecycleAndGenerationIsolation(t *testing.T) {
	store := newSessionStore()
	store.lifetime = 20 * time.Millisecond
	store.put("call-claim", liveSession{authID: "auth-1", model: defaultLiveModel})

	first, claim := store.claim("call-claim")
	if claim != sessionClaimAcquired {
		t.Fatalf("first claim = %v, want acquired", claim)
	}
	if _, duplicateClaim := store.claim("call-claim"); duplicateClaim != sessionClaimBusy {
		t.Fatalf("duplicate claim = %v, want busy", duplicateClaim)
	}
	store.release(first)
	old, retryClaim := store.claim("call-claim")
	if retryClaim != sessionClaimAcquired {
		t.Fatalf("retry claim = %v, want acquired", retryClaim)
	}

	store.put("call-claim", liveSession{authID: "auth-2", model: "replacement-model"})
	replacement, ok := store.peek("call-claim")
	if !ok || replacement.authID != "auth-2" || replacement.token == old.token {
		t.Fatalf("replacement session = %#v, ok=%t", replacement, ok)
	}
	store.expire("call-claim", old.token)
	store.complete(old, "old_complete")
	if got, stillStored := store.peek("call-claim"); !stillStored || got.token != replacement.token {
		t.Fatalf("old generation removed replacement: %#v, stored=%t", got, stillStored)
	}

	claimedReplacement, replacementClaim := store.claim("call-claim")
	if replacementClaim != sessionClaimAcquired {
		t.Fatalf("replacement claim = %v, want acquired", replacementClaim)
	}
	store.complete(claimedReplacement, "test_complete")
	if _, stillStored := store.peek("call-claim"); stillStored {
		t.Fatal("completed replacement session remained stored")
	}
}

func TestSessionStoreReleasedSessionExpires(t *testing.T) {
	store := newSessionStore()
	store.lifetime = 20 * time.Millisecond
	store.put("call-expire", liveSession{authID: "auth-1", model: defaultLiveModel})
	session, claim := store.claim("call-expire")
	if claim != sessionClaimAcquired {
		t.Fatalf("claim = %v, want acquired", claim)
	}
	store.release(session)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := store.peek("call-expire"); !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("released live session did not expire")
}

func TestSidebandURLShapes(t *testing.T) {
	if got := buildSidebandURL(
		defaultSidebandAPIBaseURL, sidebandFrameless, "rtc_1",
	); got != "wss://api.openai.com/v1/live/rtc_1" {
		t.Fatalf("Frameless sideband URL = %q", got)
	}
	if got := buildSidebandURL(
		defaultSidebandAPIBaseURL, sidebandRealtimeCalls, "rtc_1",
	); got != "wss://api.openai.com/v1/realtime/calls/rtc_1" {
		t.Fatalf("Realtime calls sideband URL = %q", got)
	}
	if got := buildSidebandURL(
		defaultSidebandAPIBaseURL, sidebandRealtimeQuery, "rtc_2",
	); got != "wss://api.openai.com/v1/realtime?intent=quicksilver&call_id=rtc_2" {
		t.Fatalf("Realtime query sideband URL = %q", got)
	}
	if got := websocketHTTPURL("wss://api.openai.com/v1/live/rtc_1"); got != "https://api.openai.com/v1/live/rtc_1" {
		t.Fatalf("WebSocket HTTP URL = %q", got)
	}
}

func TestProxyURLForAuthPrecedence(t *testing.T) {
	cfg := &config.Config{}
	cfg.ProxyURL = "http://global.example:8080"
	selected := &auth.Auth{ProxyURL: "socks5://credential.example:1080"}
	if got := proxyURLForAuth(cfg, selected); got != selected.ProxyURL {
		t.Fatalf("credential proxy = %q", got)
	}
	selected.ProxyURL = ""
	if got := proxyURLForAuth(cfg, selected); got != cfg.ProxyURL {
		t.Fatalf("global proxy = %q", got)
	}
	selected.ProxyURL = "direct"
	if got := proxyURLForAuth(cfg, selected); got != "direct" {
		t.Fatalf("direct proxy override = %q", got)
	}
	if got := proxyURLForAuth(nil, nil); got != "" {
		t.Fatalf("empty proxy = %q", got)
	}
}

func TestNewSidebandDialerModes(t *testing.T) {
	direct := newSidebandDialer("direct")
	if direct.Proxy != nil {
		t.Fatal("direct sideband dialer retained a proxy function")
	}

	httpDialer := newSidebandDialer("http://proxy.example:8080")
	if httpDialer.Proxy == nil {
		t.Fatal("HTTP sideband dialer has no proxy function")
	}
	proxyURL, errProxy := httpDialer.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "api.openai.com"}})
	if errProxy != nil {
		t.Fatalf("HTTP proxy lookup error = %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://proxy.example:8080" {
		t.Fatalf("HTTP proxy URL = %v", proxyURL)
	}

	socksDialer := newSidebandDialer("socks5h://proxy.example:1080")
	if socksDialer.Proxy != nil || socksDialer.NetDialContext == nil {
		t.Fatal("SOCKS sideband dialer was not configured for connection-layer proxying")
	}
}

func TestHandleSidebandDialErrorMapsContextStatuses(t *testing.T) {
	for _, test := range []struct {
		name     string
		err      error
		response *http.Response
		want     int
	}{
		{name: "canceled", err: context.Canceled, want: clienterror.StatusClientClosedRequest},
		{name: "deadline", err: context.DeadlineExceeded, want: http.StatusGatewayTimeout},
		{
			name: "handshake response wins",
			err:  context.Canceled,
			response: &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("unauthorized")),
			},
			want: http.StatusUnauthorized,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				gin.SetMode(gin.TestMode)
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				handleSidebandDialError(ctx, context.Background(), nil, test.response, test.err)
				if recorder.Code != test.want {
					t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.want, recorder.Body.String())
				}
			},
		)
	}
}

func TestWebsocketCloseDetails(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantCode   int
		wantReason string
	}{
		{name: "EOF", err: io.EOF, wantCode: websocket.CloseNormalClosure},
		{
			name: "abnormal", err: &websocket.CloseError{Code: websocket.CloseAbnormalClosure},
			wantCode: websocket.CloseNormalClosure,
		},
		{
			name: "provider close", err: &websocket.CloseError{Code: 4001, Text: "provider"}, wantCode: 4001,
			wantReason: "provider",
		},
		{
			name: "internal", err: errors.New("secret internal error"), wantCode: websocket.CloseInternalServerErr,
			wantReason: "relay closed",
		},
	} {
		code, reason := websocketCloseDetails(test.err)
		if code != test.wantCode || reason != test.wantReason {
			t.Fatalf("%s close = (%d, %q), want (%d, %q)", test.name, code, reason, test.wantCode, test.wantReason)
		}
	}
}
