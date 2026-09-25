package live

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestHandleDirectWebsocketUsesOAuthAndAppliesClientSecretSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamRequest := make(chan *http.Request, 1)
	upstreamUpdate := make(chan []byte, 1)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		connection, errUpgrade := upgrader.Upgrade(writer, request, nil)
		if errUpgrade != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		upstreamRequest <- request.Clone(request.Context())
		_, update, errRead := connection.ReadMessage()
		if errRead != nil {
			return
		}
		upstreamUpdate <- append([]byte(nil), update...)
		_ = connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`))
		_, _, _ = connection.ReadMessage()
	}))
	defer upstreamServer.Close()

	manager := auth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&captureExecutor{})
	registerCredential(t, manager, &auth.Auth{
		ID:       "codex-oauth",
		Provider: "codex",
		Status:   auth.StatusActive,
		Metadata: map[string]any{
			"access_token": "oauth-token",
			"account_id":   "account-123",
		},
	})
	handler := NewHandler(manager, nil)
	handler.sidebandAPIBaseURL = "ws" + strings.TrimPrefix(upstreamServer.URL, "http") + "/v1"
	router := gin.New()
	router.GET("/v1/realtime", func(c *gin.Context) {
		c.Set("apiKey", "issuer-key")
		c.Set("accessProvider", "config-inline")
		c.Set(ClientSecretPrincipalContextKey, "sess_123")
		c.Set(
			ClientSecretSessionContextKey,
			json.RawMessage(`{"type":"realtime","model":"gpt-live-1-codex","instructions":"help"}`),
		)
		c.Next()
	}, handler.HandleRealtimeWebsocket)
	downstreamServer := httptest.NewServer(router)
	defer downstreamServer.Close()

	wsURL := "ws" + strings.TrimPrefix(downstreamServer.URL, "http") + "/v1/realtime?model=gpt-realtime"
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer ek_secret")
	headers.Set("OpenAI-Alpha", "must-not-forward")
	connection, _, errDial := websocket.DefaultDialer.Dial(wsURL, headers)
	if errDial != nil {
		t.Fatalf("dial downstream websocket: %v", errDial)
	}
	defer func() { _ = connection.Close() }()
	_, created, errRead := connection.ReadMessage()
	if errRead != nil {
		t.Fatalf("read session.created: %v", errRead)
	}
	if string(created) != `{"type":"session.created"}` {
		t.Fatalf("session.created = %s", created)
	}

	select {
	case request := <-upstreamRequest:
		if got := request.Header.Get("Authorization"); got != "Bearer oauth-token" {
			t.Fatalf("upstream Authorization = %q", got)
		}
		if got := request.Header.Get("Chatgpt-Account-Id"); got != "account-123" {
			t.Fatalf("Chatgpt-Account-Id = %q", got)
		}
		if got := request.Header.Get("OpenAI-Alpha"); got != "" {
			t.Fatalf("OpenAI-Alpha leaked as %q", got)
		}
		query, errParse := url.ParseQuery(request.URL.RawQuery)
		if errParse != nil {
			t.Fatalf("parse query: %v", errParse)
		}
		if query.Get("model") != "gpt-realtime" || query.Has("intent") {
			t.Fatalf("upstream query = %v", query)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream request was not captured")
	}
	select {
	case update := <-upstreamUpdate:
		var event struct {
			Type    string `json:"type"`
			Session struct {
				Model        string `json:"model"`
				Instructions string `json:"instructions"`
			} `json:"session"`
		}
		if errUnmarshal := json.Unmarshal(update, &event); errUnmarshal != nil {
			t.Fatalf("unmarshal update: %v; payload=%s", errUnmarshal, update)
		}
		if event.Type != "session.update" || event.Session.Model != "" || event.Session.Instructions != "help" {
			t.Fatalf("session update = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("session update was not captured")
	}
	_ = connection.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"),
		time.Now().Add(websocketControlTimeout),
	)
}

func TestHandleDirectWebsocketRejectsClientSecretModelMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(auth.NewManager(nil, nil, nil), nil)
	router := gin.New()
	router.GET("/v1/realtime", func(c *gin.Context) {
		c.Set(
			ClientSecretSessionContextKey,
			json.RawMessage(`{"type":"realtime","model":"gpt-live-1-codex"}`),
		)
		c.Set(ClientSecretPrincipalContextKey, "sess_123")
		c.Next()
	}, handler.HandleRealtimeWebsocket)
	request := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=another-live-model", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden ||
		!strings.Contains(recorder.Body.String(), "realtime_client_secret_scope_mismatch") {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRealtimeSessionUpdateRemovesServerOwnedFields(t *testing.T) {
	update, errUpdate := realtimeSessionUpdate(
		json.RawMessage(`{
			"type":"realtime",
			"model":"gpt-live-1-codex",
			"id":"sess_1",
			"object":"realtime.session",
			"expires_at":123,
			"client_secret":{"value":"secret"},
			"instructions":"help"
		}`),
	)
	if errUpdate != nil {
		t.Fatalf("realtimeSessionUpdate() error = %v", errUpdate)
	}
	var value map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(update, &value); errUnmarshal != nil {
		t.Fatalf("unmarshal update: %v", errUnmarshal)
	}
	for _, field := range []string{"model", "id", "object", "expires_at", "client_secret"} {
		if _, ok := value[field]; ok {
			t.Errorf("server-owned field %q remained", field)
		}
	}
	if _, ok := value["instructions"]; !ok {
		t.Fatal("instructions were removed")
	}
}
