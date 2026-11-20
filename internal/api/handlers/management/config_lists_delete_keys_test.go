package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestDeleteClaudeKey_DeletesEmptyBaseURLWhenExplicitlyProvided(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{APIKey: "shared-key", BaseURL: ""},
				{APIKey: "shared-key", BaseURL: "https://claude.example.com"},
			},
		},
		configFilePath: tempConfigPath(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodDelete, "/v0/management/claude-api-key?api-key=shared-key&base-url=", nil,
	)

	h.DeleteClaudeKey(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	remaining := h.configSnapshot().ClaudeKey
	if got := len(remaining); got != 1 {
		t.Fatalf("claude keys len = %d, want 1", got)
	}
	if got := remaining[0].BaseURL; got != "https://claude.example.com" {
		t.Fatalf("remaining base-url = %q, want %q", got, "https://claude.example.com")
	}
	persisted, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if len(persisted.ClaudeKey) != 1 || persisted.ClaudeKey[0].APIKey != remaining[0].APIKey ||
		persisted.ClaudeKey[0].BaseURL != remaining[0].BaseURL {
		t.Fatalf("persisted Claude keys = %#v, want %#v", persisted.ClaudeKey, remaining)
	}
}

func TestDeleteVertexCompatKey_DeletesOnlyMatchingBaseURL(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{APIKey: "shared-key", BaseURL: "https://a.example.com"},
				{APIKey: "shared-key", BaseURL: "https://b.example.com"},
			},
		},
		configFilePath: tempConfigPath(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodDelete, "/v0/management/vertex-api-key?api-key=shared-key&base-url=https://b.example.com", nil,
	)

	h.DeleteVertexCompatKey(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	remaining := h.configSnapshot().VertexCompatAPIKey
	if got := len(remaining); got != 1 {
		t.Fatalf("vertex keys len = %d, want 1", got)
	}
	if got := remaining[0].BaseURL; got != "https://a.example.com" {
		t.Fatalf("remaining base-url = %q, want %q", got, "https://a.example.com")
	}
	persisted, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		t.Fatalf("reload saved config: %v", err)
	}
	if !reflect.DeepEqual(persisted.VertexCompatAPIKey, remaining) {
		t.Fatalf("persisted Vertex keys = %#v, want %#v", persisted.VertexCompatAPIKey, remaining)
	}
}

func TestDeleteXAIKey_RequiresBaseURLWhenAPIKeyDuplicated(t *testing.T) {
	t.Parallel()

	original := []config.XAIKey{
		{APIKey: "shared-key", BaseURL: "https://a.example.com"},
		{APIKey: "shared-key", BaseURL: "https://b.example.com"},
	}
	h := &Handler{cfg: &config.Config{XAIKey: original}, configFilePath: tempConfigPath(t)}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/xai-api-key?api-key=shared-key", nil)

	h.DeleteXAIKey(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	remaining := h.configSnapshot().XAIKey
	if got := len(remaining); got != 2 {
		t.Fatalf("xAI keys len = %d, want 2", got)
	}
	if !reflect.DeepEqual(remaining, original) {
		t.Fatalf("ambiguous delete changed xAI keys: %#v", remaining)
	}
	if data, err := os.ReadFile(h.configFilePath); err != nil || string(data) != "{}\n" {
		t.Fatalf("ambiguous delete changed config: %q, error=%v", data, err)
	}
}

func TestDeleteCodexKey_RequiresBaseURLWhenAPIKeyDuplicated(t *testing.T) {
	t.Parallel()

	original := []config.CodexKey{
		{APIKey: "shared-key", BaseURL: "https://a.example.com"},
		{APIKey: "shared-key", BaseURL: "https://b.example.com"},
	}
	h := &Handler{cfg: &config.Config{CodexKey: original}, configFilePath: tempConfigPath(t)}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/codex-api-key?api-key=shared-key", nil)

	h.DeleteCodexKey(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	remaining := h.configSnapshot().CodexKey
	if got := len(remaining); got != 2 {
		t.Fatalf("codex keys len = %d, want 2", got)
	}
	if !reflect.DeepEqual(remaining, original) {
		t.Fatalf("ambiguous delete changed Codex keys: %#v", remaining)
	}
	if data, err := os.ReadFile(h.configFilePath); err != nil || string(data) != "{}\n" {
		t.Fatalf("ambiguous delete changed config: %q, error=%v", data, err)
	}
}
