package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func registerSameFileDevinAuths(t *testing.T) (*coreauth.Manager, *memoryAuthStore, map[string]string) {
	t.Helper()
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	sharedPath := filepath.Join(t.TempDir(), "devin-shared.json")
	indexes := make(map[string]string)
	for _, id := range []string{"devin-a", "devin-b"} {
		auth := &coreauth.Auth{
			ID:       id,
			FileName: "devin-shared.json",
			Provider: "devin",
			Status:   coreauth.StatusActive,
			Metadata: map[string]any{"type": "devin", "priority": 1},
			Attributes: map[string]string{
				coreauth.AttributePath:          sharedPath,
				coreauth.AttributeAuthIndexSeed: id,
				"priority":                      "1",
			},
		}
		indexes[id] = auth.EnsureIndex()
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	return manager, store, indexes
}

func TestPatchAuthFileStatusTargetsSameFileIdentity(t *testing.T) {
	manager, store, indexes := registerSameFileDevinAuths(t)
	handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/status",
		strings.NewReader(`{"name":"devin-shared.json","auth_index":"`+indexes["devin-a"]+`","disabled":true}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchAuthFileStatus(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	for id, wantDisabled := range map[string]bool{"devin-a": true, "devin-b": false} {
		auth, ok := manager.GetByID(id)
		if !ok || auth == nil || auth.Disabled != wantDisabled {
			t.Fatalf("auth %s = %#v, want disabled=%v", id, auth, wantDisabled)
		}
	}
}

func TestPatchAuthFileFieldsTargetsSameFileIdentity(t *testing.T) {
	manager, store, indexes := registerSameFileDevinAuths(t)
	handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/fields",
		strings.NewReader(`{"name":"devin-shared.json","auth-index":"`+indexes["devin-b"]+`","priority":7}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchAuthFileFields(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	for id, wantPriority := range map[string]string{"devin-a": "1", "devin-b": "7"} {
		auth, ok := manager.GetByID(id)
		if !ok || auth == nil || auth.Attributes["priority"] != wantPriority {
			t.Fatalf("auth %s = %#v, want priority=%s", id, auth, wantPriority)
		}
	}
}

func TestPatchAuthFileFieldsRejectsMismatchedSameFileIdentity(t *testing.T) {
	manager, store, indexes := registerSameFileDevinAuths(t)
	handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/fields",
		strings.NewReader(`{"name":"other.json","auth-index":"`+indexes["devin-a"]+`","priority":7}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchAuthFileFields(ctx)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
}
