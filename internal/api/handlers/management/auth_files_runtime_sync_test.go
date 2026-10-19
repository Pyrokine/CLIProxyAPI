package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/gin-gonic/gin"
)

func TestPatchAuthFileStatusInvokesPersistHook(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		hookErr    error
		wantStatus int
	}{
		{name: "success", wantStatus: http.StatusOK},
		{name: "failure", hookErr: errors.New("runtime sync failed"), wantStatus: http.StatusInternalServerError},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &memoryAuthStore{}
			manager := coreauth.NewManager(store, nil, nil)
			auth := &coreauth.Auth{
				ID:         "status.json",
				FileName:   "status.json",
				Provider:   "qwen",
				Status:     coreauth.StatusActive,
				Metadata:   map[string]any{"type": "qwen"},
				Attributes: map[string]string{coreauth.AttributePath: filepath.Join(t.TempDir(), "status.json")},
			}
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatal(errRegister)
			}
			handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))
			var hooked *coreauth.Auth
			handler.SetPostAuthPersistHook(func(_ context.Context, updated *coreauth.Auth) error {
				hooked = updated.Clone()
				return testCase.hookErr
			})

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(
				http.MethodPatch,
				"/auth-files/status",
				strings.NewReader(`{"name":"status.json","disabled":true}`),
			)
			ctx.Request.Header.Set("Content-Type", "application/json")
			handler.PatchAuthFileStatus(ctx)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if hooked == nil || !hooked.Disabled || hooked.Status != coreauth.StatusDisabled {
				t.Fatalf("persist hook auth = %#v, want disabled", hooked)
			}
			current, ok := manager.GetByID(auth.ID)
			if !ok || current == nil || !current.Disabled {
				t.Fatalf("persisted runtime auth = %#v, want disabled", current)
			}
			if testCase.hookErr != nil && !strings.Contains(recorder.Body.String(), testCase.hookErr.Error()) {
				t.Fatalf("response body = %s, want hook error", recorder.Body.String())
			}
		})
	}
}

func TestPatchAuthFileStatusDoesNotHoldLockDuringPersistHook(t *testing.T) {
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	for _, id := range []string{"status-a.json", "status-b.json"} {
		if _, err := manager.Register(t.Context(), &coreauth.Auth{
			ID: id, FileName: id, Provider: "qwen", Status: coreauth.StatusActive,
			Metadata: map[string]any{"type": "qwen"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	handler := NewHandler(&config.Config{}, "", manager)
	hookStarted := make(chan struct{})
	releaseHook := make(chan struct{})
	handler.SetPostAuthPersistHook(func(_ context.Context, auth *coreauth.Auth) error {
		if auth.ID == "status-a.json" {
			close(hookStarted)
			<-releaseHook
		}
		return nil
	})
	doneA := make(chan int, 1)
	go func() {
		response := runConfigListRequest(handler.PatchAuthFileStatus, http.MethodPatch, "/auth-files/status", `{"name":"status-a.json","disabled":true}`)
		doneA <- response.Code
	}()
	t.Cleanup(func() {
		close(releaseHook)
		select {
		case <-doneA:
		case <-time.After(2 * time.Second):
		}
	})
	select {
	case <-hookStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first status hook did not start")
	}
	doneB := make(chan int, 1)
	go func() {
		response := runConfigListRequest(handler.PatchAuthFileStatus, http.MethodPatch, "/auth-files/status", `{"name":"status-b.json","disabled":true}`)
		doneB <- response.Code
	}()
	select {
	case code := <-doneB:
		if code != http.StatusOK {
			t.Fatalf("second status=%d, want 200", code)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated status update blocked by the first persist hook")
	}
}

type cancelingManagementAuthStore struct {
	memoryAuthStore
	cancel context.CancelFunc
}

func (s *cancelingManagementAuthStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	savedPath, errSave := s.memoryAuthStore.Save(ctx, auth)
	if s.cancel != nil {
		s.cancel()
	}
	return savedPath, errSave
}

func TestPatchAuthFileStatusPublishesAfterPersistenceCancelsRequest(t *testing.T) {
	store := &cancelingManagementAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "canceled-status.json",
		FileName: "canceled-status.json",
		Provider: "qwen",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "qwen"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))
	hookCalled := false
	handler.SetPostAuthPersistHook(func(_ context.Context, updated *coreauth.Auth) error {
		hookCalled = updated != nil && updated.Disabled
		return nil
	})

	requestCtx, cancel := context.WithCancel(context.Background())
	store.cancel = cancel
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/status",
		strings.NewReader(`{"name":"canceled-status.json","disabled":true}`),
	).WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchAuthFileStatus(ctx)

	if requestCtx.Err() == nil {
		t.Fatal("test store did not cancel the request context")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !hookCalled {
		t.Fatal("persisted status did not reach the runtime synchronization hook")
	}
	current, ok := manager.GetByID(auth.ID)
	if !ok || current == nil || !current.Disabled {
		t.Fatalf("published auth = %#v, want disabled", current)
	}
}

func TestBulkAuthFileStatusInvokesPersistHook(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		hookErr    error
		wantStatus int
	}{
		{name: "success", wantStatus: http.StatusOK},
		{
			name:       "failure",
			hookErr:    errors.New("bulk runtime sync failed"),
			wantStatus: http.StatusUnprocessableEntity,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := &memoryAuthStore{}
			manager := coreauth.NewManager(store, nil, nil)
			auth := &coreauth.Auth{
				ID:       "bulk-status.json",
				FileName: "bulk-status.json",
				Provider: "qwen",
				Status:   coreauth.StatusActive,
				Metadata: map[string]any{"type": "qwen"},
			}
			if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
				t.Fatal(errRegister)
			}
			handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))
			var hooked *coreauth.Auth
			handler.SetPostAuthPersistHook(func(_ context.Context, updated *coreauth.Auth) error {
				hooked = updated.Clone()
				return testCase.hookErr
			})

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(
				http.MethodPatch,
				"/auth-files/bulk/disable",
				strings.NewReader(`{"names":["bulk-status.json"]}`),
			)
			ctx.Request.Header.Set("Content-Type", "application/json")
			handler.PatchAuthFilesBulkDisable(ctx)

			if recorder.Code != testCase.wantStatus {
				t.Fatalf("status = %d, want %d body=%s", recorder.Code, testCase.wantStatus, recorder.Body.String())
			}
			if hooked == nil || !hooked.Disabled || hooked.Status != coreauth.StatusDisabled {
				t.Fatalf("persist hook auth = %#v, want disabled", hooked)
			}
			current, ok := manager.GetByID(auth.ID)
			if !ok || current == nil || !current.Disabled {
				t.Fatalf("persisted runtime auth = %#v, want disabled", current)
			}
			if testCase.hookErr != nil && !strings.Contains(recorder.Body.String(), testCase.hookErr.Error()) {
				t.Fatalf("response body = %s, want hook error", recorder.Body.String())
			}
		})
	}
}

func TestBulkAuthFileStatusUpdatesEveryIdentityBackedByTheFile(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	sharedPath := filepath.Join(t.TempDir(), "devin-shared.json")
	for _, auth := range []*coreauth.Auth{
		{
			ID:         "devin-a",
			FileName:   "devin-shared.json",
			Provider:   "devin",
			Status:     coreauth.StatusActive,
			Metadata:   map[string]any{"type": "devin"},
			Attributes: map[string]string{coreauth.AttributePath: sharedPath},
		},
		{
			ID:         "devin-b",
			FileName:   "devin-shared.json",
			Provider:   "devin",
			Status:     coreauth.StatusActive,
			Metadata:   map[string]any{"type": "devin"},
			Attributes: map[string]string{coreauth.AttributePath: sharedPath},
		},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}

	handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))
	hooked := make(map[string]bool)
	handler.SetPostAuthPersistHook(func(_ context.Context, updated *coreauth.Auth) error {
		hooked[updated.ID] = updated.Disabled
		return nil
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/bulk/disable",
		strings.NewReader(`{"names":["devin-shared.json"]}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchAuthFilesBulkDisable(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, id := range []string{"devin-a", "devin-b"} {
		current, ok := manager.GetByID(id)
		if !ok || current == nil || !current.Disabled {
			t.Fatalf("runtime auth %s = %#v, want disabled", id, current)
		}
		if !hooked[id] {
			t.Fatalf("persist hook did not publish disabled identity %s", id)
		}
	}
}

func TestBulkAuthFileStatusPublishesAfterPersistenceCancelsRequest(t *testing.T) {
	store := &cancelingManagementAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "canceled-bulk-status.json",
		FileName: "canceled-bulk-status.json",
		Provider: "qwen",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "qwen"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))
	hookCalled := false
	handler.SetPostAuthPersistHook(func(_ context.Context, updated *coreauth.Auth) error {
		hookCalled = updated != nil && updated.Disabled
		return nil
	})

	requestCtx, cancel := context.WithCancel(context.Background())
	store.cancel = cancel
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/bulk/disable",
		strings.NewReader(`{"names":["canceled-bulk-status.json"]}`),
	).WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchAuthFilesBulkDisable(ctx)

	if requestCtx.Err() == nil {
		t.Fatal("test store did not cancel the request context")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !hookCalled {
		t.Fatal("persisted bulk status did not reach the runtime synchronization hook")
	}
	current, ok := manager.GetByID(auth.ID)
	if !ok || current == nil || !current.Disabled {
		t.Fatalf("published auth = %#v, want disabled", current)
	}
}

func TestPatchAuthFileFieldsInvokesPersistHook(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "fields.json",
		FileName: "fields.json",
		Provider: "qwen",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "qwen"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))
	var hooked *coreauth.Auth
	handler.SetPostAuthPersistHook(func(_ context.Context, updated *coreauth.Auth) error {
		hooked = updated.Clone()
		return nil
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/fields",
		strings.NewReader(`{"name":"fields.json","note":"updated"}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.PatchAuthFileFields(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if hooked == nil || hooked.Metadata["note"] != "updated" {
		t.Fatalf("persist hook auth = %#v, want updated note", hooked)
	}
}

func TestPatchAuthFileFieldsSharesStatusMutationLock(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "locked-fields.json",
		FileName: "locked-fields.json",
		Provider: "qwen",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "qwen"},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	handler := NewHandler(&config.Config{}, "", manager, withTokenStore(store))

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/auth-files/fields",
		strings.NewReader(`{"name":"locked-fields.json","note":"updated"}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.authStatusMu.Lock()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		handler.PatchAuthFileFields(ctx)
		close(done)
	}()
	<-started

	completedWhileLocked := false
	select {
	case <-done:
		completedWhileLocked = true
	case <-time.After(50 * time.Millisecond):
	}
	handler.authStatusMu.Unlock()
	if completedWhileLocked {
		t.Fatal("field update completed outside the shared status mutation lock")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("field update did not complete after releasing the shared mutation lock")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestSavedAuthRecordsInvokePersistHook(t *testing.T) {
	for _, testCase := range []struct {
		name string
		save func(context.Context, *Handler, string, *coreauth.Auth) error
	}{
		{
			name: "token_record",
			save: func(ctx context.Context, handler *Handler, authDir string, auth *coreauth.Auth) error {
				_, errSave := handler.saveTokenRecord(ctx, authDir, auth)
				return errSave
			},
		},
		{
			name: "uploaded_auth",
			save: func(ctx context.Context, handler *Handler, authDir string, auth *coreauth.Auth) error {
				path := filepath.Join(authDir, auth.FileName)
				return handler.saveUploadedAuth(ctx, authDir, path, []byte(`{"type":"qwen"}`))
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			authDir := t.TempDir()
			store := sdkAuth.NewFileTokenStore()
			store.SetBaseDir(authDir)
			manager := coreauth.NewManager(store, nil, nil)
			handler := NewHandler(
				&config.Config{AuthDir: authDir},
				"",
				manager,
				withTokenStore(store),
			)
			targetPath := filepath.Join(authDir, "saved.json")
			oldAuth := &coreauth.Auth{
				ID:         "old-saved.json",
				FileName:   "saved.json",
				Provider:   "qwen",
				Status:     coreauth.StatusActive,
				Metadata:   map[string]any{"type": "qwen"},
				Attributes: map[string]string{coreauth.AttributePath: targetPath},
			}
			if _, errRegister := manager.Register(
				coreauth.WithSkipPersist(context.Background()),
				oldAuth,
			); errRegister != nil {
				t.Fatal(errRegister)
			}

			hooked := make(map[string]*coreauth.Auth)
			handler.SetPostAuthPersistHook(func(_ context.Context, updated *coreauth.Auth) error {
				hooked[updated.ID] = updated.Clone()
				return nil
			})
			auth := &coreauth.Auth{
				ID:       "saved.json",
				FileName: "saved.json",
				Provider: "qwen",
				Metadata: map[string]any{"type": "qwen"},
			}

			if errSave := testCase.save(context.Background(), handler, authDir, auth); errSave != nil {
				t.Fatal(errSave)
			}
			saved := hooked[auth.ID]
			if saved == nil || saved.Attributes[coreauth.AttributePath] == "" {
				t.Fatalf("persist hook auth = %#v, want saved auth", saved)
			}
			disabled := hooked[oldAuth.ID]
			if disabled == nil || !disabled.Disabled || disabled.Status != coreauth.StatusDisabled {
				t.Fatalf("superseded hook auth = %#v, want disabled", disabled)
			}
			currentOld, ok := manager.GetByID(oldAuth.ID)
			if !ok || currentOld == nil || !currentOld.Disabled {
				t.Fatalf("superseded manager auth = %#v, want disabled", currentOld)
			}
		})
	}
}
