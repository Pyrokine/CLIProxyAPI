package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/gin-gonic/gin"
)

func TestPatchCodexKeyUpdatesAlphaSearch(t *testing.T) {
	handler := &Handler{
		cfg: &config.Config{
			CodexKey: []config.CodexKey{
				{
					APIKey:  "codex-key",
					BaseURL: "https://codex.example.com",
				},
			},
		},
		configFilePath: tempConfigPath(t),
	}
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(
		http.MethodPatch,
		"/v0/management/codex-api-key",
		strings.NewReader(`{"index":0,"value":{"alpha-search":true}}`),
	)
	context.Request.Header.Set("Content-Type", "application/json")

	handler.PatchCodexKey(context)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if !handler.cfg.CodexKey[0].AlphaSearch {
		t.Fatal("alpha-search = false, want true")
	}
}
