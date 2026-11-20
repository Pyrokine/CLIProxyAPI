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
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
)

type pluginVirtualManagementHost struct {
	parseAuths func(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error)
}

func (*pluginVirtualManagementHost) RegisteredPlugins() []pluginhost.RegisteredPluginInfo { return nil }
func (*pluginVirtualManagementHost) PluginBusy(string) bool                               { return false }
func (*pluginVirtualManagementHost) PluginRuntimeIdentity(string) pluginhost.PluginRuntimeIdentity {
	return pluginhost.PluginRuntimeIdentity{}
}
func (*pluginVirtualManagementHost) UnloadPluginWithError(string) (bool, error) { return false, nil }

func (*pluginVirtualManagementHost) UnloadPluginIfCurrentWithError(
	pluginhost.PluginRuntimeIdentity,
) (bool, error) {
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
func (h *pluginVirtualManagementHost) ParseAuths(
	ctx context.Context,
	req pluginapi.AuthParseRequest,
) ([]*coreauth.Auth, bool, error) {
	if h.parseAuths != nil {
		return h.parseAuths(ctx, req)
	}
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
		auths = append(
			auths, &coreauth.Auth{
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
			},
		)
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
		if _, errRegister := manager.Register(
			coreauth.WithSkipPersist(context.Background()), auth,
		); errRegister != nil {
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

func TestPatchPluginVirtualSourceStatusDisablesAllExpandedAuths(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		ids       []string
	}{
		{"expanded children", `{"type":"gemini-cli"}`, []string{"auth-1", "auth-2"}},
		{"source identity", `{"type":"gemini-cli","disabled":false}`, []string{"source.json", "virtual-project-a"}},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				authDir := t.TempDir()
				sourcePath := filepath.Join(authDir, "source.json")
				if err := os.WriteFile(sourcePath, []byte(tc.raw), 0o600); err != nil {
					t.Fatal(err)
				}
				host := &pluginVirtualManagementHost{
					parseAuths: func(
						context.Context, pluginapi.AuthParseRequest,
					) ([]*coreauth.Auth, bool, error) {
						return []*coreauth.Auth{
							{
								ID: tc.ids[0], FileName: "source.json", Provider: "gemini-cli",
								Status:   coreauth.StatusActive,
								Metadata: map[string]any{"type": "gemini-cli"},
							},
							{
								ID: tc.ids[1], FileName: "source.json", Provider: "gemini-cli",
								Status:   coreauth.StatusActive,
								Metadata: map[string]any{"type": "gemini-cli"},
							},
						}, true, nil
					},
				}
				parseCtx := sdkAuth.WithPluginAuthParser(t.Context(), host)
				auths, handled, err := sdkAuth.ParsePluginAuthFiles(
					parseCtx, "gemini-cli", sourcePath, "source.json", []byte(tc.raw),
				)
				if err != nil || !handled || len(auths) != 2 {
					t.Fatalf("expanded auths = %#v, handled = %t, error = %v", auths, handled, err)
				}
				store := sdkAuth.NewFileTokenStore()
				store.SetBaseDir(authDir)
				manager := coreauth.NewManager(store, nil, nil)
				for _, auth := range auths {
					if _, err := manager.Register(coreauth.WithSkipPersist(t.Context()), auth); err != nil {
						t.Fatal(err)
					}
				}
				handler := NewHandler(&config.Config{AuthDir: authDir}, "", manager, withTokenStore(store))
				t.Cleanup(func() { _ = handler.Close() })
				handler.pluginHost = host
				childID := tc.ids[len(tc.ids)-1]
				childStatus := patchPluginVirtualStatus(t, handler, `{"name":"`+childID+`","disabled":true}`)
				if childStatus.Code != http.StatusConflict {
					t.Fatalf("child status = %d, body = %s", childStatus.Code, childStatus.Body.String())
				}
				childFields := runConfigListRequest(
					handler.PatchAuthFileFields, http.MethodPatch,
					"/v0/management/auth-files/fields", `{"name":"`+childID+`","note":"hello"}`,
				)
				if childFields.Code != http.StatusConflict {
					t.Fatalf("child fields = %d, body = %s", childFields.Code, childFields.Body.String())
				}
				response := patchPluginVirtualStatus(t, handler, `{"name":"source.json","disabled":true}`)
				if response.Code != http.StatusOK {
					t.Fatalf("source status = %d, body = %s", response.Code, response.Body.String())
				}
				raw, err := os.ReadFile(sourcePath)
				if err != nil || !strings.Contains(string(raw), `"disabled":true`) {
					t.Fatalf("source = %s, error = %v", raw, err)
				}
				for _, id := range tc.ids {
					auth, exists := manager.GetByID(id)
					if !exists || auth == nil || !auth.Disabled || auth.Status != coreauth.StatusDisabled {
						t.Fatalf("disabled auth %s = %#v", id, auth)
					}
				}
				deleted := runConfigListRequest(
					handler.DeleteAuthFile, http.MethodDelete,
					"/v0/management/auth-files?name=source.json", "",
				)
				if deleted.Code != http.StatusOK {
					t.Fatalf("source delete = %d, body = %s", deleted.Code, deleted.Body.String())
				}
				if _, err := os.Stat(sourcePath); !errors.Is(err, os.ErrNotExist) || len(manager.List()) != 0 {
					t.Fatalf("source deletion: file error = %v, runtime records = %d", err, len(manager.List()))
				}
			},
		)
	}
}

func TestPatchAuthFileStatusUpdatesPluginVirtualSource(t *testing.T) {
	handler, manager, _, sourcePath, indexes := newPluginVirtualManagementHandler(t)
	hookCalls := 0
	handler.SetPostAuthPersistHook(
		func(context.Context, *coreauth.Auth) error {
			hookCalls++
			return nil
		},
	)

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

func TestPatchPluginVirtualStatusDoesNotBlockOtherAuths(t *testing.T) {
	handler, manager, _, _, _ := newPluginVirtualManagementHandler(t)
	if _, err := manager.Register(
		coreauth.WithSkipPersist(t.Context()), &coreauth.Auth{
			ID: "ordinary.json", FileName: "ordinary.json", Provider: "qwen", Status: coreauth.StatusActive,
			Metadata: map[string]any{"type": "qwen"},
		},
	); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	handler.SetPostAuthPersistHook(
		func(_ context.Context, auth *coreauth.Auth) error {
			if auth.ID == "plugin-a" && auth.Disabled {
				close(started)
				<-release
			}
			return nil
		},
	)
	pluginDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		pluginDone <- patchPluginVirtualStatus(t, handler, `{"name":"plugin-source.json","disabled":true}`)
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("plugin runtime hook did not start")
	}
	ordinaryDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		ordinaryDone <- runConfigListRequest(
			handler.PatchAuthFileStatus, http.MethodPatch,
			"/auth-files/status", `{"name":"ordinary.json","disabled":true}`,
		)
	}()
	select {
	case response := <-ordinaryDone:
		if response.Code != http.StatusOK {
			t.Fatalf("ordinary status = %d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("plugin runtime hook blocked an unrelated status update")
	}
	secondPluginDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		secondPluginDone <- patchPluginVirtualStatus(t, handler, `{"name":"plugin-source.json","disabled":false}`)
	}()
	select {
	case <-secondPluginDone:
		t.Fatal("second update bypassed the plugin source transaction")
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	for _, done := range []<-chan *httptest.ResponseRecorder{pluginDone, secondPluginDone} {
		select {
		case response := <-done:
			if response.Code != http.StatusOK {
				t.Fatalf("plugin status = %d body=%s", response.Code, response.Body.String())
			}
		case <-time.After(2 * time.Second):
			t.Fatal("plugin status did not complete after runtime hook")
		}
	}
	current, ok := manager.GetByID("plugin-a")
	if !ok || current.Disabled {
		t.Fatalf("latest plugin status was not applied: %#v", current)
	}
}

func TestPatchPluginVirtualStatusPreservesConcurrentUploadBeforeSourceSave(t *testing.T) {
	handler, manager, _, sourcePath, _ := newPluginVirtualManagementHandler(t)
	host := handler.pluginHost.(*pluginVirtualManagementHost)
	parser := *host
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	host.parseAuths = func(ctx context.Context, req pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
		var root struct{ Disabled bool }
		if err := json.Unmarshal(req.RawJSON, &root); err != nil {
			return nil, true, err
		}
		if root.Disabled {
			close(started)
			<-release
		}
		return parser.ParseAuths(ctx, req)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- patchPluginVirtualStatus(t, handler, `{"name":"plugin-source.json","disabled":true}`)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("source status did not reach the parser")
	}
	uploaded := `{"type":"plugin-provider","accounts":[{"id":"a"},{"id":"b","disabled":true}],"note":"uploaded"}`
	response := runConfigListRequest(
		handler.UploadAuthFile, http.MethodPost,
		"/v0/management/auth-files?name=plugin-source.json", uploaded,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", response.Code, response.Body.String())
	}
	unblock()
	select {
	case response := <-done:
		if response.Code != http.StatusInternalServerError ||
			!strings.Contains(response.Body.String(), coreauth.ErrAuthGenerationConflict.Error()) {
			t.Fatalf("stale source update = %d, body = %s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("stale source status update did not finish")
	}
	current, err := os.ReadFile(sourcePath)
	first, _ := manager.GetByID("plugin-a")
	if err != nil || string(current) != uploaded || first == nil || first.Disabled {
		t.Fatalf("newer upload changed: file = %s, error = %v, runtime = %#v", current, err, first)
	}
}

func TestPatchPluginVirtualStatusRollbackPreservesConcurrentUpload(t *testing.T) {
	handler, manager, _, sourcePath, _ := newPluginVirtualManagementHandler(t)
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	handler.SetPostAuthPersistHook(
		func(_ context.Context, auth *coreauth.Auth) error {
			if auth.ID == "plugin-a" && auth.Disabled {
				close(started)
				<-release
				return errors.New("injected source status runtime failure")
			}
			return nil
		},
	)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- patchPluginVirtualStatus(t, handler, `{"name":"plugin-source.json","disabled":true}`)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("source status did not reach the runtime hook")
	}
	uploaded := `{"type":"plugin-provider","accounts":[{"id":"a"},{"id":"b","disabled":true}],"note":"uploaded"}`
	response := runConfigListRequest(
		handler.UploadAuthFile, http.MethodPost,
		"/v0/management/auth-files?name=plugin-source.json", uploaded,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("upload status = %d, body = %s", response.Code, response.Body.String())
	}
	unblock()
	select {
	case response := <-done:
		if response.Code != http.StatusInternalServerError {
			t.Fatalf("failed status update = %d, body = %s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("failed source status update did not finish")
	}
	current, err := os.ReadFile(sourcePath)
	if err != nil || string(current) != uploaded {
		t.Fatalf("newer uploaded file was replaced: %s, error = %v", current, err)
	}
	first, _ := manager.GetByID("plugin-a")
	second, _ := manager.GetByID("plugin-b")
	if first == nil || first.Disabled || second == nil || !second.Disabled {
		t.Fatalf("newer runtime was replaced: first = %#v, second = %#v", first, second)
	}
}

func TestPatchAuthFileStatusRestoresPluginVirtualSourceAfterHookFailure(t *testing.T) {
	for _, failingID := range []string{"plugin-a", "plugin-b"} {
		t.Run(
			failingID, func(t *testing.T) {
				handler, manager, _, sourcePath, _ := newPluginVirtualManagementHandler(t)
				original, errRead := os.ReadFile(sourcePath)
				if errRead != nil {
					t.Fatal(errRead)
				}
				handler.SetPostAuthPersistHook(
					func(_ context.Context, updated *coreauth.Auth) error {
						if updated.ID == failingID && updated.Disabled {
							return errors.New("injected runtime sync failure")
						}
						return nil
					},
				)
				response := patchPluginVirtualStatus(
					t,
					handler,
					`{"name":"plugin-source.json","disabled":true}`,
				)
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
					t.Fatalf(
						"runtime after failure: first disabled=%t second disabled=%t body=%s",
						first != nil &&
							first.Disabled,
						second != nil && second.Disabled,
						response.Body.String(),
					)
				}
			},
		)
	}
}

type uncertainPluginVirtualStore struct {
	coreauth.Store
	saveCalls int
	failure   error
}

func (store *uncertainPluginVirtualStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	path, errSave := store.Store.Save(ctx, auth)
	if errSave != nil {
		return path, errSave
	}
	store.saveCalls++
	if store.saveCalls == 1 {
		if store.failure != nil {
			return path, store.failure
		}
		return path, coreauth.ErrAuthDurableStateUncertain
	}
	return path, nil
}

func TestPatchAuthFileStatusRestoresPluginVirtualSourceAfterUncertainSave(t *testing.T) {
	for _, failure := range []error{
		coreauth.ErrAuthDurableStateUncertain, errors.New("injected save failure after write"),
	} {
		t.Run(
			failure.Error(), func(t *testing.T) {
				handler, manager, fileStore, sourcePath, _ := newPluginVirtualManagementHandler(t)
				original, errRead := os.ReadFile(sourcePath)
				if errRead != nil {
					t.Fatal(errRead)
				}
				store := &uncertainPluginVirtualStore{Store: fileStore, failure: failure}
				handler.tokenStore = store
				response := patchPluginVirtualStatus(
					t,
					handler,
					`{"name":"plugin-source.json","disabled":true}`,
				)
				if response.Code != http.StatusInternalServerError || !strings.Contains(
					response.Body.String(), failure.Error(),
				) {
					t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
				}
				restored, errRead := os.ReadFile(sourcePath)
				if errRead != nil || string(restored) != string(original) || store.saveCalls != 2 {
					t.Fatalf(
						"source after failed save = %q calls=%d err=%v",
						restored,
						store.saveCalls,
						errRead,
					)
				}
				first, _ := manager.GetByID("plugin-a")
				second, _ := manager.GetByID("plugin-b")
				if first == nil || first.Disabled || second == nil || !second.Disabled {
					t.Fatalf(
						"runtime after failed save: first disabled=%t second disabled=%t",
						first != nil && first.Disabled, second != nil && second.Disabled,
					)
				}
			},
		)
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
		t.Run(
			test.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(
					http.MethodPatch, "/auth-files/bulk/"+test.name,
					strings.NewReader(`{"names":["plugin-source.json"]}`),
				)
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
					t.Fatalf(
						"bulk %s first disabled=%t second disabled=%t", test.name,
						first != nil && first.Disabled, second != nil && second.Disabled,
					)
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
					t.Fatalf(
						"bulk %s source disabled = %#v, want %t",
						test.name,
						persisted["disabled"],
						test.disabled,
					)
				}
			},
		)
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
	if statusResponse.Code != http.StatusConflict || !strings.Contains(
		statusResponse.Body.String(), errPluginVirtualAuth.Error(),
	) {
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
	if fieldsResponse.Code != http.StatusConflict || !strings.Contains(
		fieldsResponse.Body.String(), errPluginVirtualAuth.Error(),
	) {
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
	if deleteResponse.Code != http.StatusConflict || !strings.Contains(
		deleteResponse.Body.String(), errPluginVirtualAuth.Error(),
	) {
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

func TestDeletePluginVirtualSourceNameCollision(t *testing.T) {
	for _, mode := range []string{"single", "batch"} {
		t.Run(
			mode, func(t *testing.T) {
				authDir := t.TempDir()
				sourcePath := filepath.Join(authDir, "plugin-a.json")
				raw := []byte(`{"type":"plugin-provider","accounts":[{"id":"a.json"},{"id":"b"}]}`)
				if err := os.WriteFile(sourcePath, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				host := &pluginVirtualManagementHost{}
				parseCtx := sdkAuth.WithPluginAuthParser(context.Background(), host)
				auths, handled, err := sdkAuth.ParsePluginAuthFiles(
					parseCtx, "plugin-provider", sourcePath, "plugin-a.json", raw,
				)
				if err != nil || !handled || len(auths) != 2 {
					t.Fatalf("ParsePluginAuthFiles() handled=%t len=%d err=%v", handled, len(auths), err)
				}
				store := sdkAuth.NewFileTokenStore()
				store.SetBaseDir(authDir)
				manager := coreauth.NewManager(store, nil, nil)
				for _, auth := range auths {
					if _, err := manager.Register(coreauth.WithSkipPersist(context.Background()), auth); err != nil {
						t.Fatal(err)
					}
				}
				handler := NewHandler(&config.Config{AuthDir: authDir}, "", manager, withTokenStore(store))
				handler.pluginHost = host
				t.Cleanup(func() { _ = handler.Close() })
				child, ok := manager.GetByID("plugin-a.json")
				if !ok || child == nil || child.FileName != "plugin-a.json" ||
					!coreauth.IsPluginVirtualAuth(child) {
					t.Fatalf("virtual source identity = %#v", child)
				}
				blocked := httptest.NewRecorder()
				blockedCtx, _ := gin.CreateTestContext(blocked)
				blockedCtx.Request = httptest.NewRequest(
					http.MethodDelete,
					"/v0/management/auth-files?name=plugin-a.json&auth_index="+url.QueryEscape(
						child.EnsureIndex(),
					),
					nil,
				)
				handler.DeleteAuthFile(blockedCtx)
				if blocked.Code != http.StatusConflict {
					t.Fatalf(
						"indexed virtual child delete status=%d body=%s",
						blocked.Code,
						blocked.Body.String(),
					)
				}
				if _, err := os.Stat(sourcePath); err != nil || len(manager.List()) != 2 {
					t.Fatalf("blocked deletion changed the source: stat=%v auths=%d", err, len(manager.List()))
				}
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				if mode == "single" {
					ctx.Request = httptest.NewRequest(
						http.MethodDelete, "/v0/management/auth-files?name=plugin-a.json", nil,
					)
				} else {
					ctx.Request = httptest.NewRequest(
						http.MethodDelete, "/v0/management/"+
							"auth-files", strings.NewReader(
							`{"names":["plugin-`+
								`a.json"]}`,
						),
					)
					ctx.Request.Header.Set("Content-Type", "application/json")
				}
				handler.DeleteAuthFile(ctx)
				if recorder.Code != http.StatusOK {
					t.Fatalf("source delete status=%d body=%s", recorder.Code, recorder.Body.String())
				}
				if _, err := os.Stat(sourcePath); !os.IsNotExist(err) {
					t.Fatalf("deleted source stat error=%v", err)
				}
				if auths := manager.List(); len(auths) != 0 {
					t.Fatalf("source deletion retained runtime identities: %#v", auths)
				}
			},
		)
	}
}
