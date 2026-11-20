package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

var (
	_ = api.NewHandler
	_ = api.NewHandlerWithoutConfigFilePath
	_ = api.WriteConfig
	_ = api.RegisterOAuthSession
	_ = api.SetOAuthSessionError
	_ = api.CompleteOAuthSession
	_ = api.CompleteOAuthSessionsByProvider
	_ = api.GetOAuthSession
	_ = api.IsOAuthSessionPending
	_ = api.ValidateOAuthState
	_ = api.NormalizeOAuthProvider
	_ = api.WriteOAuthCallbackFile
	_ = api.WriteOAuthCallbackFileForPendingSession
	_ = api.PopulateAuthContext
)

func TestLegacyManagementConstructorsDelegateToCurrentHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{AuthDir: t.TempDir(), Debug: true}
	for _, handler := range []*api.Handler{
		api.NewHandler(cfg, filepath.Join(t.TempDir(), "config.yaml"), nil),
		api.NewHandlerWithoutConfigFilePath(cfg, nil),
	} {
		t.Cleanup(
			func() {
				if err := handler.Close(); err != nil {
					t.Error(err)
				}
			},
		)
		recorder := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(recorder)
		request.Request = httptest.NewRequest(http.MethodGet, "/config", nil)
		handler.GetConfig(request)
		var response struct {
			Debug bool `json:"debug"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || recorder.Code != http.StatusOK ||
			!response.Debug {
			t.Fatalf("configuration response=%s status=%d error=%v", recorder.Body.Bytes(), recorder.Code, err)
		}
	}
}

func TestLegacyManagementWriteConfigKeepsValidatedPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	input := []byte("host: localhost\n  # SDK comment\nport: 8080\n")
	if err := api.WriteConfig(path, input); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "host: localhost\n# SDK comment\nport: 8080\n" {
		t.Fatalf("configuration=%q error=%v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("configuration permissions=%v error=%v", info, err)
	}
	if err := api.WriteConfig(path, []byte("port: [\n")); err == nil {
		t.Fatal("invalid YAML was accepted")
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != string(data) {
		t.Fatalf("failed write changed configuration: %q error=%v", current, err)
	}
	if err := api.WriteConfig(filepath.Join(t.TempDir(), "missing", "config.yaml"), input); err == nil {
		t.Fatal("configuration write failure was hidden")
	}
}

func TestLegacyManagementOAuthSessionHelpers(t *testing.T) {
	state := "legacy-sdk-session"
	api.RegisterOAuthSession(state, "codex")
	t.Cleanup(func() { api.CompleteOAuthSession(state) })
	if provider, status, ok := api.GetOAuthSession(state); provider != "codex" || status != "" || !ok ||
		!api.IsOAuthSessionPending(state, "codex") {
		t.Fatalf("registered provider=%q status=%q found=%t", provider, status, ok)
	}
	api.SetOAuthSessionError(state, "synthetic failure")
	if provider, status, ok := api.GetOAuthSession(state); provider != "codex" || status != "synthetic failure" ||
		!ok ||
		api.IsOAuthSessionPending(state, "codex") {
		t.Fatalf("failed provider=%q status=%q found=%t", provider, status, ok)
	}
	api.CompleteOAuthSession(state)
	api.CompleteOAuthSession(state)
	if _, _, found := api.GetOAuthSession(state); found {
		t.Fatal("completed session is still available")
	}
	for _, state := range []string{"legacy-sdk-batch-a", "legacy-sdk-batch-b"} {
		api.RegisterOAuthSession(state, "codex")
		t.Cleanup(func() { api.CompleteOAuthSession(state) })
	}
	other := "legacy-sdk-other-provider"
	api.RegisterOAuthSession(other, "gemini")
	t.Cleanup(func() { api.CompleteOAuthSession(other) })
	if count := api.CompleteOAuthSessionsByProvider("codex"); count != 2 {
		t.Fatalf("completed session count=%d, want 2", count)
	}
	if count := api.CompleteOAuthSessionsByProvider("codex"); count != 0 || !api.IsOAuthSessionPending(
		other, "gemini",
	) {
		t.Fatalf("repeated completion count=%d or another provider was changed", count)
	}
}

func TestLegacyManagementCallbackHelpersKeepPendingStateAndPermissions(t *testing.T) {
	dir := t.TempDir()
	state := "legacy-sdk-callback"
	api.RegisterOAuthSession(state, "codex")
	t.Cleanup(func() { api.CompleteOAuthSession(state) })
	if _, err := api.WriteOAuthCallbackFileForPendingSession(dir, "gemini", state, "code", ""); err == nil {
		t.Fatal("another provider was accepted")
	}
	if _, err := api.WriteOAuthCallbackFileForPendingSession(
		dir, "codex", "legacy-sdk-unknown", "code", "",
	); err == nil {
		t.Fatal("unknown session was accepted")
	}
	path, err := api.WriteOAuthCallbackFileForPendingSession(dir, "openai", state, " code ", "")
	if err != nil || path != filepath.Join(dir, ".oauth-codex-"+state+".oauth") {
		t.Fatalf("callback path=%q error=%v", path, err)
	}
	data, err := os.ReadFile(path)
	var payload struct{ Code, State, Error string }
	if err != nil || json.Unmarshal(data, &payload) != nil || payload.Code != "code" || payload.State != state ||
		payload.Error != "" {
		t.Fatalf("callback=%q error=%v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("callback permissions=%v error=%v", info, err)
	}
	if _, err := api.WriteOAuthCallbackFileForPendingSession(dir, "codex", state, "again", ""); err == nil {
		t.Fatal("accepted callback was published again")
	}
	for _, invalid := range []string{"", "../parent", "back\\slash", strings.Repeat("x", 129)} {
		if err := api.ValidateOAuthState(invalid); err == nil {
			t.Fatalf("invalid OAuth state accepted: %q", invalid)
		}
		if _, err := api.WriteOAuthCallbackFile(dir, "codex", invalid, "code", ""); err == nil {
			t.Fatalf("callback accepted invalid state: %q", invalid)
		}
	}
	if provider, err := api.NormalizeOAuthProvider(" OpenAI "); err != nil || provider != "codex" {
		t.Fatalf("normalized provider=%q error=%v", provider, err)
	}
	if _, err := api.NormalizeOAuthProvider("unsupported-provider"); err == nil {
		t.Fatal("unsupported built-in provider was accepted")
	}
	if _, err := api.WriteOAuthCallbackFile(dir, "fixture-plugin", "legacy-plugin-callback", "code", ""); err != nil {
		t.Fatalf("legacy plugin callback: %v", err)
	}
}

func TestLegacyManagementPopulateAuthContextPreservesRequestInfo(t *testing.T) {
	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	request.Request = httptest.NewRequest(http.MethodGet, "/oauth?scope=fixture", nil)
	request.Request.Header.Set("X-Fixture", "synthetic")
	ctx := api.PopulateAuthContext(context.Background(), request)
	info := coreauth.GetRequestInfo(ctx)
	if info == nil || info.Query.Get("scope") != "fixture" || info.Headers.Get("X-Fixture") != "synthetic" {
		t.Fatalf("request info=%+v", info)
	}
}
