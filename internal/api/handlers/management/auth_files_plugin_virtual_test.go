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
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
)

type pluginVirtualManagementHost struct{}

func (*pluginVirtualManagementHost) RegisteredPlugins() []pluginhost.RegisteredPluginInfo { return nil }
func (*pluginVirtualManagementHost) PluginBusy(string) bool                               { return false }
func (*pluginVirtualManagementHost) PluginRuntimeIdentity(string) pluginhost.PluginRuntimeIdentity {
	return pluginhost.PluginRuntimeIdentity{}
}
func (*pluginVirtualManagementHost) UnloadPluginWithError(string) (bool, error) { return false, nil }
func (*pluginVirtualManagementHost) UnloadPluginIfCurrentWithError(pluginhost.PluginRuntimeIdentity) (bool, error) {
	return false, nil
}
func (h *pluginVirtualManagementHost) ParseAuth(
	ctx context.Context,
	req pluginapi.AuthParseRequest,
) (*coreauth.Auth, bool, error) {
	auths, handled, errParse := h.ParseAuths(ctx, req)
	if errParse != nil || !handled || len(auths) == 0 {
		return nil, handled, errParse
	}
	return auths[0], true, nil
}
func (*pluginVirtualManagementHost) ParseAuths(
	_ context.Context,
	req pluginapi.AuthParseRequest,
) ([]*coreauth.Auth, bool, error) {
	var source struct {
		Accounts []struct {
			ID       string `json:"id"`
			Disabled bool   `json:"disabled"`
		} `json:"accounts"`
	}
	if errUnmarshal := json.Unmarshal(req.RawJSON, &source); errUnmarshal != nil {
		return nil, true, errUnmarshal
	}
	auths := make([]*coreauth.Auth, 0, len(source.Accounts))
	for _, account := range source.Accounts {
		status := coreauth.StatusActive
		if account.Disabled {
			status = coreauth.StatusDisabled
		}
		auths = append(auths, &coreauth.Auth{
			ID:       "plugin-" + account.ID,
			Provider: req.Provider,
			FileName: req.FileName,
			Disabled: account.Disabled,
			Status:   status,
			Metadata: map[string]any{
				"type":       req.Provider,
				"account_id": account.ID,
				"disabled":   account.Disabled,
			},
		})
	}
	return auths, true, nil
}

func newPluginVirtualManagementHandler(
	t *testing.T,
) (*Handler, *coreauth.Manager, *sdkAuth.FileTokenStore, string, map[string]string) {
	t.Helper()
	authDir := t.TempDir()
	sourcePath := filepath.Join(authDir, "plugin-source.json")
	raw := []byte(`{"type":"plugin-provider","accounts":[{"id":"a"},{"id":"b","disabled":true}]}`)
	if errWrite := os.WriteFile(sourcePath, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	host := &pluginVirtualManagementHost{}
	parseCtx := sdkAuth.WithPluginAuthParser(context.Background(), host)
	auths, handled, errParse := sdkAuth.ParsePluginAuthFiles(
		parseCtx,
		"plugin-provider",
		sourcePath,
		filepath.Base(sourcePath),
		raw,
	)
	if errParse != nil || !handled || len(auths) != 2 {
		t.Fatalf("ParsePluginAuthFiles() handled=%t len=%d err=%v", handled, len(auths), errParse)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(authDir)
	manager := coreauth.NewManager(store, nil, nil)
	indexes := make(map[string]string, len(auths))
	for _, auth := range auths {
		indexes[auth.ID] = auth.EnsureIndex()
		if _, errRegister := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	handler := NewHandler(&config.Config{AuthDir: authDir}, "", manager, withTokenStore(store))
	handler.pluginHost = host
	return handler, manager, store, sourcePath, indexes
}

func patchPluginVirtualStatus(t *testing.T, handler *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/status", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchAuthFileStatus(ctx)
	return recorder
}

func TestPatchAuthFileStatusUpdatesPluginVirtualSource(t *testing.T) {
	handler, manager, _, sourcePath, indexes := newPluginVirtualManagementHandler(t)
	hookCalls := 0
	handler.SetPostAuthPersistHook(func(context.Context, *coreauth.Auth) error {
		hookCalls++
		return nil
	})

	disabledResponse := patchPluginVirtualStatus(
		t,
		handler,
		`{"name":"plugin-source.json","disabled":true}`,
	)
	if disabledResponse.Code != http.StatusOK {
		t.Fatalf("disable status = %d body=%s", disabledResponse.Code, disabledResponse.Body.String())
	}
	for id, index := range indexes {
		auth, ok := manager.GetByID(id)
		if !ok || auth == nil || !auth.Disabled || auth.Status != coreauth.StatusDisabled {
			t.Fatalf("disabled auth %s = %#v", id, auth)
		}
		if auth.EnsureIndex() != index {
			t.Fatalf("auth %s index = %q, want %q", id, auth.EnsureIndex(), index)
		}
		if auth.Attributes[coreauth.PersistedSHA256Attribute] == "" {
			t.Fatalf("auth %s missing persisted digest", id)
		}
	}
	var persisted map[string]any
	raw, errRead := os.ReadFile(sourcePath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if errUnmarshal := json.Unmarshal(raw, &persisted); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	if disabled, _ := persisted["disabled"].(bool); !disabled {
		t.Fatalf("persisted disabled = %#v, want true", persisted["disabled"])
	}

	enabledResponse := patchPluginVirtualStatus(
		t,
		handler,
		`{"name":"plugin-source.json","disabled":false}`,
	)
	if enabledResponse.Code != http.StatusOK {
		t.Fatalf("enable status = %d body=%s", enabledResponse.Code, enabledResponse.Body.String())
	}
	first, _ := manager.GetByID("plugin-a")
	second, _ := manager.GetByID("plugin-b")
	if first == nil || first.Disabled || first.Status != coreauth.StatusActive {
		t.Fatalf("first auth after enable = %#v, want active", first)
	}
	if second == nil || !second.Disabled || second.Status != coreauth.StatusDisabled {
		t.Fatalf("second auth after enable = %#v, want account-level disabled", second)
	}
	if hookCalls != 4 {
		t.Fatalf("post-persist hook calls = %d, want 4", hookCalls)
	}
}

func TestPatchAuthFileStatusRestoresPluginVirtualSourceAfterHookFailure(t *testing.T) {
	for _, failingID := range []string{"plugin-a", "plugin-b"} {
		t.Run(failingID, func(t *testing.T) {
			handler, manager, _, sourcePath, _ := newPluginVirtualManagementHandler(t)
			original, errRead := os.ReadFile(sourcePath)
			if errRead != nil {
				t.Fatal(errRead)
			}
			handler.SetPostAuthPersistHook(func(_ context.Context, updated *coreauth.Auth) error {
				if updated.ID == failingID && updated.Disabled {
					return errors.New("injected runtime sync failure")
				}
				return nil
			})
			response := patchPluginVirtualStatus(t, handler, `{"name":"plugin-source.json","disabled":true}`)
			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
			}
			restored, errRead := os.ReadFile(sourcePath)
			if errRead != nil || string(restored) != string(original) {
				t.Fatalf("source after failure = %q err=%v", restored, errRead)
			}
			first, _ := manager.GetByID("plugin-a")
			second, _ := manager.GetByID("plugin-b")
			if first == nil || first.Disabled || first.Status != coreauth.StatusActive ||
				second == nil || !second.Disabled || second.Status != coreauth.StatusDisabled {
				t.Fatalf("runtime after failure: first disabled=%t second disabled=%t body=%s",
					first != nil && first.Disabled, second != nil && second.Disabled, response.Body.String())
			}
		})
	}
}

type uncertainPluginVirtualStore struct {
	coreauth.Store
	saveCalls int
}

func (store *uncertainPluginVirtualStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	path, errSave := store.Store.Save(ctx, auth)
	if errSave != nil {
		return path, errSave
	}
	store.saveCalls++
	if store.saveCalls == 1 {
		return path, coreauth.ErrAuthDurableStateUncertain
	}
	return path, nil
}

func TestPatchAuthFileStatusRestoresPluginVirtualSourceAfterUncertainSave(t *testing.T) {
	handler, manager, fileStore, sourcePath, _ := newPluginVirtualManagementHandler(t)
	original, errRead := os.ReadFile(sourcePath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	store := &uncertainPluginVirtualStore{Store: fileStore}
	handler.tokenStore = store
	response := patchPluginVirtualStatus(t, handler, `{"name":"plugin-source.json","disabled":true}`)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(),
		coreauth.ErrAuthDurableStateUncertain.Error()) {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	restored, errRead := os.ReadFile(sourcePath)
	if errRead != nil || string(restored) != string(original) || store.saveCalls != 2 {
		t.Fatalf("source after uncertain save = %q calls=%d err=%v", restored, store.saveCalls, errRead)
	}
	first, _ := manager.GetByID("plugin-a")
	second, _ := manager.GetByID("plugin-b")
	if first == nil || first.Disabled || second == nil || !second.Disabled {
		t.Fatalf("runtime after uncertain save: first disabled=%t second disabled=%t",
			first != nil && first.Disabled, second != nil && second.Disabled)
	}
}

func TestBulkAuthFileStatusPersistsPluginVirtualSource(t *testing.T) {
	handler, manager, _, sourcePath, _ := newPluginVirtualManagementHandler(t)
	for _, test := range []struct {
		name     string
		disabled bool
	}{
		{name: "disable", disabled: true},
		{name: "enable", disabled: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPatch, "/auth-files/bulk/"+test.name,
				strings.NewReader(`{"names":["plugin-source.json"]}`))
			ctx.Request.Header.Set("Content-Type", "application/json")
			if test.disabled {
				handler.PatchAuthFilesBulkDisable(ctx)
			} else {
				handler.PatchAuthFilesBulkEnable(ctx)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("bulk %s status = %d body=%s", test.name, recorder.Code, recorder.Body.String())
			}
			first, _ := manager.GetByID("plugin-a")
			second, _ := manager.GetByID("plugin-b")
			if first == nil || first.Disabled != test.disabled || second == nil || !second.Disabled {
				t.Fatalf("bulk %s first disabled=%t second disabled=%t", test.name,
					first != nil && first.Disabled, second != nil && second.Disabled)
			}
			raw, errRead := os.ReadFile(sourcePath)
			if errRead != nil {
				t.Fatal(errRead)
			}
			var persisted map[string]any
			if errUnmarshal := json.Unmarshal(raw, &persisted); errUnmarshal != nil {
				t.Fatal(errUnmarshal)
			}
			if persisted["disabled"] != test.disabled {
				t.Fatalf("bulk %s source disabled = %#v, want %t", test.name, persisted["disabled"], test.disabled)
			}
		})
	}
}

func TestSaveUploadedAuthPublishesPluginVirtualChildren(t *testing.T) {
	handler, manager, _, sourcePath, _ := newPluginVirtualManagementHandler(t)
	authDir := filepath.Dir(sourcePath)
	updatedRaw := []byte(`{"type":"plugin-provider","accounts":[{"id":"a"},{"id":"c"}]}`)
	if errSave := handler.saveUploadedAuth(
		context.Background(),
		authDir,
		sourcePath,
		updatedRaw,
	); errSave != nil {
		t.Fatal(errSave)
	}

	for _, id := range []string{"plugin-a", "plugin-c"} {
		auth, ok := manager.GetByID(id)
		if !ok || auth == nil || !coreauth.IsPluginVirtualAuth(auth) {
			t.Fatalf("uploaded auth %s = %#v", id, auth)
		}
		if auth.Attributes[coreauth.AttributeVirtualSource] != sourcePath ||
			auth.Attributes[coreauth.PersistedSHA256Attribute] == "" {
			t.Fatalf("uploaded auth %s attributes = %#v", id, auth.Attributes)
		}
	}
	if stale, ok := manager.GetByID("plugin-b"); ok || stale != nil {
		t.Fatalf("stale plugin-b still registered: %#v", stale)
	}
	persisted, errRead := os.ReadFile(sourcePath)
	if errRead != nil || string(persisted) != string(updatedRaw) {
		t.Fatalf("persisted source = %s err=%v", persisted, errRead)
	}

	emptyRaw := []byte(`{"type":"plugin-provider","accounts":[]}`)
	if errSave := handler.saveUploadedAuth(
		context.Background(),
		authDir,
		sourcePath,
		emptyRaw,
	); errSave != nil {
		t.Fatal(errSave)
	}
	for _, id := range []string{"plugin-a", "plugin-c"} {
		if auth, ok := manager.GetByID(id); ok || auth != nil {
			t.Fatalf("auth %s remained after handled empty upload: %#v", id, auth)
		}
	}
	persisted, errRead = os.ReadFile(sourcePath)
	if errRead != nil || string(persisted) != string(emptyRaw) {
		t.Fatalf("empty source = %s err=%v", persisted, errRead)
	}
}

func TestPluginVirtualAuthRejectsChildMutations(t *testing.T) {
	handler, manager, _, sourcePath, indexes := newPluginVirtualManagementHandler(t)
	first, _ := manager.GetByID("plugin-a")
	if first == nil {
		t.Fatal("plugin-a not registered")
	}

	statusResponse := patchPluginVirtualStatus(
		t,
		handler,
		`{"name":"plugin-source.json","auth_index":"`+indexes["plugin-a"]+`","disabled":true}`,
	)
	if statusResponse.Code != http.StatusConflict || !strings.Contains(statusResponse.Body.String(), errPluginVirtualAuth.Error()) {
		t.Fatalf("child status = %d body=%s", statusResponse.Code, statusResponse.Body.String())
	}

	fieldsResponse := httptest.NewRecorder()
	fieldsCtx, _ := gin.CreateTestContext(fieldsResponse)
	fieldsCtx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/fields",
		strings.NewReader(`{"name":"plugin-source.json","auth-index":"`+indexes["plugin-a"]+`","priority":7}`),
	)
	fieldsCtx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchAuthFileFields(fieldsCtx)
	if fieldsResponse.Code != http.StatusConflict || !strings.Contains(fieldsResponse.Body.String(), errPluginVirtualAuth.Error()) {
		t.Fatalf("child fields = %d body=%s", fieldsResponse.Code, fieldsResponse.Body.String())
	}

	deleteResponse := httptest.NewRecorder()
	deleteCtx, _ := gin.CreateTestContext(deleteResponse)
	deleteCtx.Request = httptest.NewRequest(
		http.MethodDelete,
		"/auth-files?name="+url.QueryEscape(first.ID),
		nil,
	)
	handler.DeleteAuthFile(deleteCtx)
	if deleteResponse.Code != http.StatusConflict || !strings.Contains(deleteResponse.Body.String(), errPluginVirtualAuth.Error()) {
		t.Fatalf("child delete = %d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}

	batchDeleteResponse := httptest.NewRecorder()
	batchDeleteCtx, _ := gin.CreateTestContext(batchDeleteResponse)
	batchDeleteCtx.Request = httptest.NewRequest(
		http.MethodDelete,
		"/auth-files",
		strings.NewReader(`{"names":["`+first.ID+`"]}`),
	)
	batchDeleteCtx.Request.Header.Set("Content-Type", "application/json")
	handler.DeleteAuthFile(batchDeleteCtx)
	if batchDeleteResponse.Code != http.StatusMultiStatus ||
		!strings.Contains(batchDeleteResponse.Body.String(), errPluginVirtualAuth.Error()) {
		t.Fatalf("batch child delete = %d body=%s", batchDeleteResponse.Code, batchDeleteResponse.Body.String())
	}
	if _, errStat := os.Stat(sourcePath); errStat != nil {
		t.Fatalf("source changed after rejected child mutations: %v", errStat)
	}
}

func TestDeleteAuthFileRemovesPluginVirtualSourceAndChildren(t *testing.T) {
	handler, manager, _, sourcePath, _ := newPluginVirtualManagementHandler(t)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodDelete,
		"/auth-files?name="+url.QueryEscape(filepath.Base(sourcePath)),
		nil,
	)
	handler.DeleteAuthFile(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if _, errStat := os.Stat(sourcePath); !os.IsNotExist(errStat) {
		t.Fatalf("source still exists: %v", errStat)
	}
	for _, id := range []string{"plugin-a", "plugin-b"} {
		if auth, ok := manager.GetByID(id); ok || auth != nil {
			t.Fatalf("auth %s still registered: %#v", id, auth)
		}
	}
}
