package live

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestHandleHangupForwardsPinnedOAuthCallForOwner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := auth.NewManager(nil, nil, nil)
	executor := &captureExecutor{
		statusCode:   http.StatusOK,
		responseBody: io.NopCloser(strings.NewReader(`{"status":"ok"}`)),
	}
	manager.RegisterExecutor(executor)
	registerCredential(t, manager, &auth.Auth{
		ID:       "codex-oauth",
		Provider: "codex",
		Status:   auth.StatusActive,
		Metadata: map[string]any{"access_token": "oauth-token"},
	})
	handler := NewHandler(manager, nil)
	handler.sessions.put("call-123", liveSession{
		authID:         "codex-oauth",
		model:          defaultLiveModel,
		ownerPrincipal: "owner-key",
		ownerProvider:  "config-inline",
	})

	router := gin.New()
	router.POST("/v1/realtime/calls/:call_id/hangup", func(c *gin.Context) {
		c.Set("apiKey", "owner-key")
		c.Set("accessProvider", "config-inline")
		c.Next()
	}, handler.HandleHangup)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodPost, "/v1/realtime/calls/call-123/hangup", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if executor.request == nil ||
		executor.request.URL.String() != "https://api.openai.com/v1/realtime/calls/call-123/hangup" {
		t.Fatalf("upstream request = %#v", executor.request)
	}
	if got := executor.request.Header.Get("Authorization"); got != "Bearer oauth-token" {
		t.Fatalf("upstream Authorization = %q", got)
	}
	if _, ok := handler.sessions.peek("call-123"); ok {
		t.Fatal("successful hangup retained session")
	}
}

func TestHandleHangupEnforcesOwnerAndRejectsClientSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name       string
		middleware gin.HandlerFunc
	}{
		{
			name: "different owner",
			middleware: func(c *gin.Context) {
				c.Set("apiKey", "other-key")
				c.Set("accessProvider", "config-inline")
				c.Next()
			},
		},
		{
			name: "ephemeral key",
			middleware: func(c *gin.Context) {
				c.Set("apiKey", "owner-key")
				c.Set("accessProvider", "config-inline")
				c.Set(ClientSecretPrincipalContextKey, "sess_1")
				c.Next()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(auth.NewManager(nil, nil, nil), nil)
			handler.sessions.put("call-123", liveSession{
				authID:         "codex-oauth",
				model:          defaultLiveModel,
				ownerPrincipal: "owner-key",
				ownerProvider:  "config-inline",
			})
			router := gin.New()
			router.POST("/v1/realtime/calls/:call_id/hangup", test.middleware, handler.HandleHangup)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(
				recorder,
				httptest.NewRequest(http.MethodPost, "/v1/realtime/calls/call-123/hangup", nil),
			)
			if recorder.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusForbidden, recorder.Body.String())
			}
			if _, ok := handler.sessions.peek("call-123"); !ok {
				t.Fatal("rejected hangup removed session")
			}
		})
	}
}

func TestUnsupportedRealtimeCapabilitiesUseExplicitErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := NewHandler(nil, nil)
	router := gin.New()
	router.POST("/v1/realtime/transcription_sessions", handler.HandleTranscriptionSession)
	router.GET("/v1/realtime/translations", handler.HandleTranslation)
	router.POST("/v1/realtime/calls/:call_id/accept", handler.HandleSIPControl)
	for _, test := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/v1/realtime/transcription_sessions"},
		{method: http.MethodGet, path: "/v1/realtime/translations"},
		{method: http.MethodPost, path: "/v1/realtime/calls/call-123/accept"},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(test.method, test.path, nil))
		if recorder.Code != http.StatusNotImplemented ||
			!strings.Contains(recorder.Body.String(), `"type":"not_supported_error"`) ||
			!strings.Contains(recorder.Body.String(), `"code":"realtime_capability_not_supported"`) {
			t.Fatalf("%s %s status=%d body=%s", test.method, test.path, recorder.Code, recorder.Body.String())
		}
	}
}
