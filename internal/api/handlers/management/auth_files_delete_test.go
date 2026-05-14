package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestDeleteAuthFile_UsesAuthPathFromManager(t *testing.T) {
	// The current DeleteAuthFile always resolves paths via validateAuthFileName,
	// which joins cfg.AuthDir + filename. It does not consult the manager's
	// auth record Attributes["path"]. This test verifies that behavior:
	// the file in authDir is deleted, not the external path.
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	fileName := "codex-user@example.com-plus.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(
		filePath, []byte(`{"type":"codex","email":"test@example.com"}`), 0o600,
	); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteReq := httptest.NewRequest(
		http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(fileName), nil,
	)
	deleteCtx.Request = deleteReq
	h.DeleteAuthFile(deleteCtx)

	if deleteRec.Code != http.StatusOK {
		t.Fatalf(
			"expected delete status %d, got %d with body %s", http.StatusOK, deleteRec.Code, deleteRec.Body.String(),
		)
	}
	if _, errStat := os.Stat(filePath); !os.IsNotExist(errStat) {
		t.Fatalf("expected auth file to be removed from auth dir, stat err: %v", errStat)
	}
}

func TestDeleteAuthFile_FallbackToAuthDirPath(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	fileName := "fallback-user.json"
	filePath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(filePath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteReq := httptest.NewRequest(
		http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(fileName), nil,
	)
	deleteCtx.Request = deleteReq
	h.DeleteAuthFile(deleteCtx)

	if deleteRec.Code != http.StatusOK {
		t.Fatalf(
			"expected delete status %d, got %d with body %s", http.StatusOK, deleteRec.Code, deleteRec.Body.String(),
		)
	}
	if _, errStat := os.Stat(filePath); !os.IsNotExist(errStat) {
		t.Fatalf("expected auth file to be removed from auth dir, stat err: %v", errStat)
	}
}

func TestDeleteAuthFile_NonExistentReturns404(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteReq := httptest.NewRequest(
		http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape("nonexistent.json"), nil,
	)
	deleteCtx.Request = deleteReq
	h.DeleteAuthFile(deleteCtx)

	if deleteRec.Code != http.StatusNotFound {
		t.Fatalf(
			"expected delete status %d, got %d with body %s", http.StatusNotFound, deleteRec.Code,
			deleteRec.Body.String(),
		)
	}
}

func TestDeleteAuthFile_BatchBody(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	for _, name := range []string{"a.json", "b.json"} {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(`{"type":"claude"}`), 0o600); err != nil {
			t.Fatalf("write auth file %s: %v", name, err)
		}
	}
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteReq := httptest.NewRequest(
		http.MethodDelete,
		"/v0/management/auth-files",
		strings.NewReader(`{"names":["a.json","b.json"]}`),
	)
	deleteReq.Header.Set("Content-Type", "application/json")
	deleteCtx.Request = deleteReq
	deleteCtx.Request.Header = deleteReq.Header
	deleteCtx.Request.Body = deleteReq.Body
	deleteCtx.Request.ContentLength = deleteReq.ContentLength
	deleteCtx.Request.GetBody = deleteReq.GetBody
	deleteCtx.Request.URL = deleteReq.URL
	deleteCtx.Request.Method = deleteReq.Method
	h.DeleteAuthFile(deleteCtx)

	if deleteRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", deleteRec.Code, deleteRec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(deleteRec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal batch delete response: %v", err)
	}
	if payload["deleted"] != float64(2) {
		t.Fatalf("deleted = %#v, want 2", payload["deleted"])
	}
}

func TestDeleteAuthFile_UnregistersQuotaEntry(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	fileName := "claude-user.json"
	filePath := filepath.Join(authDir, fileName)
	if err := os.WriteFile(filePath, []byte(`{"type":"claude","email":"user@example.com"}`), 0o600); err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:         fileName,
		FileName:   fileName,
		Provider:   "claude",
		Metadata:   map[string]any{"type": "claude", "email": "user@example.com"},
		Attributes: map[string]string{"path": filePath, "source": filePath},
	}
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	scheduler := quota.NewScheduler(
		quota.DefaultConfig(), func(entry *quota.Entry) ([]byte, error) {
			return json.Marshal(map[string]any{"file": entry.FileName})
		},
	)
	auth.EnsureIndex()
	scheduler.Register(fileName, quota.TypeClaude, auth.Index)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = store
	h.quotaScheduler = scheduler

	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteReq := httptest.NewRequest(
		http.MethodDelete, "/v0/management/auth-files?name="+url.QueryEscape(fileName), nil,
	)
	deleteCtx.Request = deleteReq
	h.DeleteAuthFile(deleteCtx)

	if deleteRec.Code != http.StatusOK {
		t.Fatalf(
			"expected delete status %d, got %d with body %s", http.StatusOK, deleteRec.Code, deleteRec.Body.String(),
		)
	}
	status := scheduler.GetStatus()
	if _, exists := status.Credentials[fileName]; exists {
		t.Fatalf("expected quota entry %s to be removed", fileName)
	}
}
