package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/quota"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestDeleteAuthFile_UsesAuthPathFromManager(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	root := t.TempDir()
	authDir, externalDir := filepath.Join(root, "auth"), filepath.Join(root, "external")
	for _, dir := range []string{authDir, externalDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fileName := "codex-user@example.com-plus.json"
	shadowPath, realPath := filepath.Join(authDir, fileName), filepath.Join(externalDir, fileName)
	files := map[string]string{
		shadowPath: `{"type":"codex","email":"shadow@example.com"}`,
		realPath:   `{"type":"codex","email":"real@example.com"}`,
	}
	for path, data := range files {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID: "legacy/" + fileName, FileName: fileName, Provider: "codex",
		Status: coreauth.StatusError, Unavailable: true,
		Attributes: map[string]string{coreauth.AttributePath: realPath},
		Metadata:   map[string]any{"type": "codex", "email": "real@example.com"},
	}
	if _, err := manager.Register(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	before, _ := manager.GetByID(record.ID)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	t.Cleanup(func() { _ = h.Close() })
	h.tokenStore = &memoryAuthStore{}

	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteCtx.Request = httptest.NewRequest(
		http.MethodDelete,
		"/v0/management/auth-files?name="+url.QueryEscape(fileName), nil,
	)
	h.DeleteAuthFile(deleteCtx)
	if deleteRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", deleteRec.Code, deleteRec.Body.String())
	}
	for path, want := range files {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Fatalf("rejected deletion changed %s: data=%s err=%v", path, data, err)
		}
	}
	if after, ok := manager.GetByID(record.ID); !ok || !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected deletion changed the runtime identity: %#v", after)
	}
}

func TestDeleteAuthFile_RemovesRuntimeAuth(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	fileName := "runtime-remove-user.json"
	filePath := filepath.Join(authDir, fileName)
	if err := os.WriteFile(filePath, []byte(`{"type":"codex","email":"runtime@example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	record := &coreauth.Auth{
		ID: "runtime-remove-auth", FileName: fileName, Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{coreauth.AttributePath: filePath},
		Metadata:   map[string]any{"type": "codex", "email": "runtime@example.com"},
	}
	if _, err := manager.Register(t.Context(), record); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	t.Cleanup(func() { _ = h.Close() })
	h.tokenStore = &memoryAuthStore{}
	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteCtx.Request = httptest.NewRequest(
		http.MethodDelete,
		"/v0/management/auth-files?name="+url.QueryEscape(fileName), nil,
	)
	h.DeleteAuthFile(deleteCtx)
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", deleteRec.Code, deleteRec.Body.String())
	}
	if _, ok := manager.GetByID(record.ID); ok {
		t.Fatalf("runtime auth %q remains registered", record.ID)
	}
	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Fatalf("removed runtime auth file remains: %v", err)
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

func TestDeleteAuthFile_AllRequiresConfirmation(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	filePath := filepath.Join(authDir, "a.json")
	if err := os.WriteFile(filePath, []byte(`{"type":"claude"}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	deleteRec := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteRec)
	deleteReq := httptest.NewRequest(http.MethodDelete, "/v0/management/auth-files?all=true", nil)
	deleteCtx.Request = deleteReq
	h.DeleteAuthFile(deleteCtx)

	if deleteRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without confirmation, got %d: %s", deleteRec.Code, deleteRec.Body.String())
	}
	if _, errStat := os.Stat(filePath); errStat != nil {
		t.Fatalf("expected auth file to remain, stat err: %v", errStat)
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
	if _, exists := status.Credentials[quota.EntryKey(quota.TypeClaude, fileName)]; exists {
		t.Fatalf("expected quota entry %s to be removed", fileName)
	}
}

type authFileDeletionStore struct {
	*sdkAuth.FileTokenStore
	beforeDelete func()
	deleteError  error
	deleteCalls  int
}

func (s *authFileDeletionStore) DeleteIfVersion(
	ctx context.Context,
	id string,
	version coreauth.PersistedVersion,
) (bool, error) {
	s.deleteCalls++
	if s.beforeDelete != nil {
		s.beforeDelete()
	}
	if s.deleteError != nil {
		return false, s.deleteError
	}
	return s.FileTokenStore.DeleteIfVersion(ctx, id, version)
}

func TestDeleteAuthFileAtomicallyRemovesPersistedFileAndAliases(t *testing.T) {
	for _, managerStore := range []string{"none", "shared"} {
		for _, mode := range []string{"success", "delete_error", "file_replacement", "publication_excluded"} {
			t.Run(
				managerStore+"/"+mode, func(t *testing.T) {
					dir := t.TempDir()
					path := filepath.Join(dir, "aliases.json")
					const oldData = `{"type":"codex","access_token":"synthetic-old"}`
					const newData = `{"type":"codex","access_token":"synthetic-new"}`
					if err := os.WriteFile(path, []byte(oldData), 0o600); err != nil {
						t.Fatal(err)
					}
					store := &authFileDeletionStore{FileTokenStore: sdkAuth.NewFileTokenStore()}
					store.SetBaseDir(dir)
					manager := coreauth.NewManager(nil, nil, nil)
					if managerStore == "shared" {
						manager.SetStore(store)
					}
					before := make(map[string]*coreauth.Auth)
					for _, id := range []string{"legacy/first", "legacy/second"} {
						auth, err := manager.Register(
							coreauth.WithSkipPersist(t.Context()), &coreauth.Auth{
								ID: id, FileName: "aliases.json", Provider: "codex", Status: coreauth.StatusActive,
								Attributes: map[string]string{coreauth.AttributePath: path},
								Metadata:   map[string]any{"type": "codex", "access_token": "synthetic-old"},
							},
						)
						if err != nil {
							t.Fatal(err)
						}
						before[id] = auth
					}
					wantStatus, wantData := http.StatusOK, oldData
					switch mode {
					case "delete_error":
						wantStatus = http.StatusInternalServerError
						store.deleteError = errors.New("synthetic conditional delete failure")
					case "file_replacement":
						wantStatus, wantData = http.StatusConflict, newData
						store.beforeDelete = func() {
							if err := os.WriteFile(path, []byte(newData), 0o600); err != nil {
								t.Fatal(err)
							}
						}
					case "publication_excluded":
						store.beforeDelete = func() {
							ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
							defer cancel()
							_, err := manager.Update(coreauth.WithSkipPersist(ctx), before["legacy/second"].Clone())
							if !errors.Is(err, context.DeadlineExceeded) {
								t.Fatalf("publication was not excluded during deletion: %v", err)
							}
						}
					}
					h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager, withTokenStore(store))
					t.Cleanup(func() { _ = h.Close() })
					response := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(response)
					ctx.Request = httptest.NewRequest(
						http.MethodDelete, "/v0/management/auth-files?name=aliases.json", nil,
					)
					h.DeleteAuthFile(ctx)
					if response.Code != wantStatus || store.deleteCalls != 1 {
						t.Fatalf(
							"status=%d body=%s delete calls=%d", response.Code, response.Body.String(),
							store.deleteCalls,
						)
					}
					for id, original := range before {
						current, exists := manager.GetByID(id)
						if exists != (wantStatus != http.StatusOK) || exists && !reflect.DeepEqual(current, original) {
							t.Fatalf("runtime identity %s after deletion: %#v", id, current)
						}
					}
					data, err := os.ReadFile(path)
					if wantStatus == http.StatusOK {
						if !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("successful deletion retained the file: %s, %v", data, err)
						}
					} else if err != nil || string(data) != wantData {
						t.Fatalf("failed deletion changed the file: %s, %v", data, err)
					}
				},
			)
		}
	}
}
