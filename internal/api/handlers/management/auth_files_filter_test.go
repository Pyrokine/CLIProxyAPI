package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestC08AuthFileLookupValidatesNameAndIndex(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	first := &coreauth.Auth{
		ID: "first-id", FileName: "shared.json", Provider: "claude",
		Attributes: map[string]string{"runtime_only": "true"},
	}
	second := &coreauth.Auth{
		ID: "second-id", FileName: "second.json", Provider: "claude",
		Attributes: map[string]string{"runtime_only": "true"},
	}
	if _, err := manager.Register(t.Context(), first); err != nil {
		t.Fatalf("register first auth: %v", err)
	}
	if _, err := manager.Register(t.Context(), second); err != nil {
		t.Fatalf("register second auth: %v", err)
	}
	firstIndex := first.EnsureIndex()
	secondIndex := second.EnsureIndex()
	handler := NewHandler(&config.Config{AuthDir: t.TempDir()}, "", manager)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/auth-files?name=shared.json&auth_index="+firstIndex, nil)
	handler.ListAuthFiles(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("ListAuthFiles status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var listed struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(listed.Files) != 1 || listed.Files[0]["id"] != first.ID {
		t.Fatalf("filtered files = %#v", listed.Files)
	}

	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/status",
		strings.NewReader(`{"name":"shared.json","auth_index":"`+secondIndex+`","disabled":true}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchAuthFileStatus(ctx)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("mismatched identity status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	updated, _ := manager.GetByID(first.ID)
	if updated.Disabled {
		t.Fatal("mismatched auth_index mutated same-name auth")
	}
}
