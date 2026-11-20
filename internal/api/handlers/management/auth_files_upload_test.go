package management

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestUploadAuthFile_PreservesPriorityAttributes(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	dir := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager, withTokenStore(store))
	t.Cleanup(func() { _ = h.Close() })
	const name = "codex-user@example.com-plus.json"
	const content = `{"type":"codex","email":"user@example.com","priority":98}`
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
	h.UploadAuthFile(ctx)
	if response.Code != http.StatusOK {
		t.Fatalf("upload status=%d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("upload response=%#v", payload)
	}
	auth, ok := manager.GetByID(name)
	if !ok || auth == nil || auth.Attributes["priority"] != "98" {
		t.Fatalf("uploaded priority attributes=%+v", auth)
	}
	priority, err := json.Marshal(auth.Metadata["priority"])
	if err != nil || string(priority) != "98" {
		t.Fatalf("priority metadata=%s error=%v", priority, err)
	}
}

func TestUploadAuthFile_InvokesPostAuthPersistHook(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	dir := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager, withTokenStore(store))
	t.Cleanup(func() { _ = h.Close() })
	var hooked *coreauth.Auth
	h.SetPostAuthPersistHook(
		func(_ context.Context, auth *coreauth.Auth) error {
			if auth != nil {
				hooked = auth.Clone()
			}
			return nil
		},
	)
	const name = "codex-user@example.com.json"
	const content = `{"type":"codex","email":"user@example.com","access_token":"token-123"}`
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())
	h.UploadAuthFile(ctx)
	if response.Code != http.StatusOK {
		t.Fatalf("upload status=%d body=%s", response.Code, response.Body.String())
	}
	if hooked == nil || hooked.ID != name || hooked.FileName != name || hooked.Metadata["access_token"] != "token-123" {
		t.Fatalf("post-persist uploaded target=%+v", hooked)
	}
}
