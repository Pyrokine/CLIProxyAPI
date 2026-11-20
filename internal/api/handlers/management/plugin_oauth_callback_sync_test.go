package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestGetOAuthCallbackOriginalPluginProviders(t *testing.T) {
	for _, tc := range []struct{ provider, state string }{
		{"gemini-cli", "test-geminicli-state"},
		{"openai", "test-openai-plugin-state"},
	} {
		t.Run(
			tc.provider, func(t *testing.T) {
				authDir := filepath.Join(t.TempDir(), "missing-auth")
				if err := registerPluginOAuthSession(tc.state, tc.provider, "", authDir, nil); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { completeOAuthSession(tc.state) })
				handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
				t.Cleanup(func() { _ = handler.Close() })
				router := gin.New()
				router.GET("/v0/management/oauth-callback", handler.GetOAuthCallback)
				request := httptest.NewRequest(
					http.MethodGet,
					"/v0/management/oauth-callback?state="+tc.state+"&code=test-code", nil,
				)
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK {
					t.Fatalf("callback status = %d, body = %s", recorder.Code, recorder.Body.String())
				}
				path := filepath.Join(authDir, ".oauth-"+tc.provider+"-"+tc.state+".oauth")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var payload oauthCallbackFilePayload
				if err := json.Unmarshal(raw, &payload); err != nil || payload.State != tc.state ||
					payload.Code != "test-code" || payload.Error != "" {
					t.Fatalf("callback payload = %+v, error = %v", payload, err)
				}
				if _, err := os.Stat(filepath.Join(authDir, ".oauth-codex-"+tc.state+".oauth")); !os.IsNotExist(err) {
					t.Fatalf("plugin provider was aliased to codex: %v", err)
				}
			},
		)
	}
}

func TestGetOAuthCallbackUsesPendingPluginSessionWithoutManagementCredentials(t *testing.T) {
	previousSessions := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previousSessions })

	authDir := t.TempDir()
	if err := registerPluginOAuthSession("plugin-state", "custom-sso", "", authDir, nil); err != nil {
		t.Fatal(err)
	}
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	t.Cleanup(func() { _ = handler.Close() })
	gin.SetMode(gin.TestMode)
	get := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/oauth-callback"+query, nil)
		handler.GetOAuthCallback(ctx)
		return recorder
	}

	if mismatch := get("?state=plugin-state&provider=other&code=denied"); mismatch.Code != http.StatusBadRequest {
		t.Fatalf("provider mismatch status = %d, body = %s", mismatch.Code, mismatch.Body.String())
	}
	callbackPath := filepath.Join(authDir, ".oauth-custom-sso-plugin-state.oauth")
	if _, err := os.Stat(callbackPath); !os.IsNotExist(err) {
		t.Fatalf("mismatched callback created file: %v", err)
	}
	accepted := get("?state=plugin-state&code=callback-code")
	if accepted.Code != http.StatusOK || accepted.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf(
			"callback response = %d headers = %v body = %s", accepted.Code, accepted.Header(), accepted.Body.String(),
		)
	}
	payload, err := os.ReadFile(callbackPath)
	if err != nil {
		t.Fatal(err)
	}
	var data oauthCallbackFilePayload
	if err := json.Unmarshal(
		payload, &data,
	); err != nil || data.Code != "callback-code" || data.State != "plugin-state" {
		t.Fatalf("callback file = %+v, error = %v", data, err)
	}
	if replay := get("?state=plugin-state&code=other"); replay.Code != http.StatusNotFound {
		t.Fatalf("replayed callback status = %d, want not found", replay.Code)
	}
	if missing := get("?state=missing&code=other"); missing.Code != http.StatusNotFound {
		t.Fatalf("missing callback status = %d, want not found", missing.Code)
	}
	if err := registerPluginOAuthSession("denied-state", "custom-sso", "", authDir, nil); err != nil {
		t.Fatal(err)
	}
	if rejected := get("?state=denied-state&error_description=access_denied"); rejected.Code != http.StatusOK {
		t.Fatalf("provider error status = %d, body = %s", rejected.Code, rejected.Body.String())
	}
	errorPayload, err := os.ReadFile(filepath.Join(authDir, ".oauth-custom-sso-denied-state.oauth"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(errorPayload, &data); err != nil || data.Error != "access_denied" || data.Code != "" {
		t.Fatalf("provider error callback = %+v, error = %v", data, err)
	}
}
