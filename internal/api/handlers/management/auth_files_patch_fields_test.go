package management

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestRegisterAuthFromFileParsesWebsockets(t *testing.T) {
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := &Handler{cfg: &config.Config{AuthDir: authDir}, authManager: manager}
	path := filepath.Join(authDir, "codex.json")
	data := []byte(`{"type":"codex","email":"user@example.com","websockets":true}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	if err := h.registerAuthFromFile(context.Background(), path, data); err != nil {
		t.Fatalf("register auth file: %v", err)
	}
	registered, ok := manager.GetByID("codex.json")
	if !ok {
		t.Fatal("registered auth not found")
	}
	if registered.Attributes["websockets"] != "true" {
		t.Fatalf("websockets attribute = %q, want true", registered.Attributes["websockets"])
	}
}

func TestListAuthFilesIncludesWebsockets(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "codex.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","websockets":true}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		FileName: "codex.json",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path":       path,
			"websockets": "true",
		},
		Metadata: map[string]any{"websockets": true, "priority": float64(7), "note": "shared account"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{cfg: &config.Config{AuthDir: authDir}, authManager: manager}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/auth-files", nil)

	h.ListAuthFiles(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files length = %d, want 1", len(payload.Files))
	}
	if payload.Files[0]["websockets"] != true {
		t.Fatalf("websockets = %#v, want true", payload.Files[0]["websockets"])
	}
	if payload.Files[0]["priority"] != float64(7) {
		t.Fatalf("priority = %#v, want 7", payload.Files[0]["priority"])
	}
	if payload.Files[0]["note"] != "shared account" {
		t.Fatalf("note = %#v, want shared account", payload.Files[0]["note"])
	}
}

func TestRemoveAuthDeletesAllAuthsFromSameFile(t *testing.T) {
	authDir := t.TempDir()
	path := filepath.Join(authDir, "codex.json")
	manager := coreauth.NewManager(nil, nil, nil)
	otherPath := filepath.Join(t.TempDir(), "codex.json")
	entries := []*coreauth.Auth{
		{ID: "codex.json", Provider: "codex", FileName: "codex.json"},
		{ID: "derived-path", Provider: "codex", Attributes: map[string]string{"path": path}},
		{ID: "derived-source", Provider: "codex", Attributes: map[string]string{"source": path}},
		{ID: "derived-file", Provider: "codex", FileName: "codex.json", Attributes: map[string]string{"path": path}},
		{
			ID: "different-source", Provider: "codex", FileName: "codex.json",
			Attributes: map[string]string{"path": otherPath},
		},
	}
	for _, entry := range entries {
		if _, err := manager.Register(context.Background(), entry); err != nil {
			t.Fatalf("register auth %s: %v", entry.ID, err)
		}
	}

	h := &Handler{cfg: &config.Config{AuthDir: authDir}, authManager: manager}
	h.removeAuth(context.Background(), path)

	for _, id := range []string{"codex.json", "derived-path", "derived-source", "derived-file"} {
		if _, ok := manager.GetByID(id); ok {
			t.Fatalf("expected auth %s to be removed", id)
		}
	}
	if _, ok := manager.GetByID("different-source"); !ok {
		t.Fatal("expected auth with same filename from another source to remain")
	}
}

func TestRemoveAuthDeletesFallbackAuthIDForPath(t *testing.T) {
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(
		context.Background(), &coreauth.Auth{ID: "nested/codex.json", Provider: "codex"},
	); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	h := &Handler{cfg: &config.Config{AuthDir: authDir}, authManager: manager}
	h.removeAuth(context.Background(), filepath.Join(authDir, "nested", "codex.json"))
	if _, ok := manager.GetByID("nested/codex.json"); ok {
		t.Fatal("expected fallback auth ID to be removed")
	}
}

func TestPatchAuthFileFieldsUpdatesWebsockets(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "auth-1",
		Provider: "codex",
		FileName: "codex.json",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"websockets": false},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{authManager: manager}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/fields",
		bytes.NewBufferString(`{"name":"auth-1","websockets":true}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	updated, ok := manager.GetByID("auth-1")
	if !ok {
		t.Fatal("updated auth not found")
	}
	if updated.Metadata["websockets"] != true {
		t.Fatalf("metadata websockets = %#v, want true", updated.Metadata["websockets"])
	}
	if updated.Attributes["websockets"] != "true" {
		t.Fatalf("attribute websockets = %q, want true", updated.Attributes["websockets"])
	}
	store.mu.Lock()
	persisted := store.items["auth-1"]
	store.mu.Unlock()
	if persisted == nil || persisted.Metadata["websockets"] != true {
		t.Fatalf("persisted websockets = %#v", persisted)
	}
}

func TestPatchAuthFileFieldsUpdatesHeadersAndNote(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:         "auth-1",
		Provider:   "claude",
		FileName:   "claude.json",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"header:X-Keep": "old", "header:X-Remove": "old"},
		Metadata: map[string]any{
			"headers": map[string]any{"X-Keep": "old", "X-Remove": "old"},
			"note":    "old note",
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{authManager: manager}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/fields",
		bytes.NewBufferString(`{"name":"auth-1","headers":{"X-New":"new","X-Remove":""},"note":" updated note "}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAuthFileFields(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	updated, ok := manager.GetByID("auth-1")
	if !ok {
		t.Fatal("updated auth not found")
	}
	headers := coreauth.ExtractCustomHeadersFromMetadata(updated.Metadata)
	if headers["X-Keep"] != "old" || headers["X-New"] != "new" {
		t.Fatalf("headers not merged: %#v", headers)
	}
	if _, exists := headers["X-Remove"]; exists {
		t.Fatalf("empty header was not removed: %#v", headers)
	}
	if updated.Attributes["header:X-Keep"] != "old" || updated.Attributes["header:X-New"] != "new" {
		t.Fatalf("attributes not synced: %#v", updated.Attributes)
	}
	if _, exists := updated.Attributes["header:X-Remove"]; exists {
		t.Fatalf("removed header attribute remains: %#v", updated.Attributes)
	}
	if updated.Metadata["note"] != "updated note" {
		t.Fatalf("note = %#v", updated.Metadata["note"])
	}
}

func tempConfigPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestPatchAPIKeyAliasRejectsInvalidKey(t *testing.T) {
	h := &Handler{cfg: &config.Config{}, configFilePath: tempConfigPath(t)}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/api-key-aliases",
		bytes.NewBufferString(`{"key":"bad key","alias":"valid_alias"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAPIKeyAlias(c)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if h.cfg.APIKeyAliases != nil {
		t.Fatalf("aliases mutated: %#v", h.cfg.APIKeyAliases)
	}
}

func TestPatchAPIKeyAliasAcceptsValidKey(t *testing.T) {
	h := &Handler{cfg: &config.Config{}, configFilePath: tempConfigPath(t)}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/api-key-aliases",
		bytes.NewBufferString(`{"key":"sk-valid123","alias":"valid_alias"}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchAPIKeyAlias(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if h.cfg.APIKeyAliases["sk-valid123"] != "valid_alias" {
		t.Fatalf("alias not stored: %#v", h.cfg.APIKeyAliases)
	}
}
