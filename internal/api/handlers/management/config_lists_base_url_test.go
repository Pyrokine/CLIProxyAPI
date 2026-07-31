package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func newConfigListTestHandler(t *testing.T, cfg *config.Config) *Handler {
	t.Helper()
	t.Setenv("MANAGEMENT_PASSWORD", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	handler := NewHandler(cfg, path, nil, WithEnvSecret(""))
	t.Cleanup(
		func() {
			if errClose := handler.Close(); errClose != nil {
				t.Errorf("Close() error = %v", errClose)
			}
		},
	)
	return handler
}

func runConfigListRequest(
	handler func(*gin.Context),
	method string,
	target string,
	body string,
) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(method, target, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	return recorder
}

func TestPatchProviderBaseURLAllowsLocalTargetsAndRejectsInvalidScheme(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *config.Config
		handler    func(*Handler, *gin.Context)
		body       string
		wantStatus int
	}{
		{
			name: "gemini loopback",
			cfg: &config.Config{
				GeminiKey: []config.GeminiKey{
					{
						APIKey: "gemini-key", BaseURL: "https://8.8.8.8/v1",
					},
				},
			},
			handler:    (*Handler).PatchGeminiKey,
			body:       `{"index":0,"value":{"base-url":"http://127.0.0.1:8080"}}`,
			wantStatus: http.StatusOK,
		},
		{
			name: "claude link local",
			cfg: &config.Config{
				ClaudeKey: []config.ClaudeKey{
					{
						APIKey: "claude-key", BaseURL: "https://8.8.8.8/v1",
					},
				},
			},
			handler:    (*Handler).PatchClaudeKey,
			body:       `{"index":0,"value":{"base-url":"http://169.254.169.254/latest/meta-data/"}}`,
			wantStatus: http.StatusOK,
		},
		{
			name: "openai compatibility private",
			cfg: &config.Config{
				OpenAICompatibility: []config.OpenAICompatibility{
					{
						Name: "openai", BaseURL: "https://8.8.8.8/v1",
					},
				},
			},
			handler:    (*Handler).PatchOpenAICompat,
			body:       `{"index":0,"value":{"base-url":"http://10.0.0.1:8080"}}`,
			wantStatus: http.StatusOK,
		},
		{
			name: "vertex private",
			cfg: &config.Config{
				VertexCompatAPIKey: []config.VertexCompatKey{
					{
						APIKey: "vertex-key", BaseURL: "https://8.8.8.8/v1",
					},
				},
			},
			handler:    (*Handler).PatchVertexCompatKey,
			body:       `{"index":0,"value":{"base-url":"http://192.168.1.10:8080"}}`,
			wantStatus: http.StatusOK,
		},
		{
			name: "codex invalid scheme",
			cfg: &config.Config{
				CodexKey: []config.CodexKey{
					{
						APIKey: "codex-key", BaseURL: "https://8.8.8.8/v1",
					},
				},
			},
			handler:    (*Handler).PatchCodexKey,
			body:       `{"index":0,"value":{"base-url":"file:///tmp/socket"}}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(
			tt.name, func(t *testing.T) {
				h := newConfigListTestHandler(t, tt.cfg)
				recorder := runConfigListRequest(
					func(c *gin.Context) { tt.handler(h, c) }, http.MethodPatch, "/provider", tt.body,
				)
				if recorder.Code != tt.wantStatus {
					t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
				}
				if tt.wantStatus == http.StatusBadRequest && !strings.Contains(
					recorder.Body.String(), "invalid base-url",
				) {
					t.Fatalf("body = %s, want invalid base-url", recorder.Body.String())
				}
			},
		)
	}
}

func TestPutProviderBaseURLAllowsLocalTargetsAndRejectsInvalidScheme(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *config.Config
		handler    func(*Handler, *gin.Context)
		body       string
		wantStatus int
	}{
		{
			name:       "gemini loopback",
			cfg:        &config.Config{},
			handler:    (*Handler).PutGeminiKeys,
			body:       `[{"api-key":"gemini-key","base-url":"http://127.0.0.1:8080"}]`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "claude link local",
			cfg:        &config.Config{},
			handler:    (*Handler).PutClaudeKeys,
			body:       `[{"api-key":"claude-key","base-url":"http://169.254.169.254/latest/meta-data/"}]`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "openai compatibility private",
			cfg:        &config.Config{},
			handler:    (*Handler).PutOpenAICompat,
			body:       `[{"name":"openai","base-url":"http://10.0.0.1:8080"}]`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "vertex private",
			cfg:        &config.Config{},
			handler:    (*Handler).PutVertexCompatKeys,
			body:       `[{"api-key":"vertex-key","base-url":"http://192.168.1.10:8080"}]`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "codex invalid scheme",
			cfg:        &config.Config{},
			handler:    (*Handler).PutCodexKeys,
			body:       `[{"api-key":"codex-key","base-url":"file:///tmp/socket"}]`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(
			tt.name, func(t *testing.T) {
				h := newConfigListTestHandler(t, tt.cfg)
				recorder := runConfigListRequest(
					func(c *gin.Context) { tt.handler(h, c) }, http.MethodPut, "/provider", tt.body,
				)
				if recorder.Code != tt.wantStatus {
					t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
				}
				if tt.wantStatus == http.StatusBadRequest && !strings.Contains(
					recorder.Body.String(), "invalid base-url",
				) {
					t.Fatalf("body = %s, want invalid base-url", recorder.Body.String())
				}
			},
		)
	}
}

func TestDeleteProviderKeyAcceptsBodyMatch(t *testing.T) {
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{
			{APIKey: "delete-me", BaseURL: "http://127.0.0.1:8080"},
			{APIKey: "keep-me", BaseURL: "http://127.0.0.1:8081"},
		},
	}
	h := newConfigListTestHandler(t, cfg)
	recorder := runConfigListRequest(
		func(c *gin.Context) { h.DeleteGeminiKey(c) },
		http.MethodDelete,
		"/gemini-api-key",
		`{"api-key":"delete-me","base-url":"http://127.0.0.1:8080"}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	snapshot := h.configSnapshot()
	if len(snapshot.GeminiKey) != 1 || snapshot.GeminiKey[0].APIKey != "keep-me" {
		t.Fatalf("gemini keys = %#v, want only keep-me", snapshot.GeminiKey)
	}
}

func TestDeleteAPIKeyAliasAcceptsBody(t *testing.T) {
	cfg := &config.Config{
		SDKConfig: config.SDKConfig{
			APIKeyAliases: map[string]string{
				"delete-me": "old", "keep-me": "new",
			},
		},
	}
	h := newConfigListTestHandler(t, cfg)
	recorder := runConfigListRequest(
		func(c *gin.Context) { h.DeleteAPIKeyAlias(c) },
		http.MethodDelete,
		"/api-key-aliases",
		`{"key":"delete-me"}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	snapshot := h.configSnapshot()
	if _, ok := snapshot.APIKeyAliases["delete-me"]; ok {
		t.Fatalf("delete-me alias still exists: %#v", snapshot.APIKeyAliases)
	}
	if snapshot.APIKeyAliases["keep-me"] != "new" {
		t.Fatalf("keep-me alias = %q", snapshot.APIKeyAliases["keep-me"])
	}
}
