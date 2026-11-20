package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/api/handlers/management"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

type pausedAuthModelRegistration struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *pausedAuthModelRegistration) GetClientID() string {
	p.once.Do(
		func() {
			close(p.entered)
			<-p.release
		},
	)
	return ""
}

func TestAuthModelRegistrationDoesNotHoldGlobalUpdateLock(t *testing.T) {
	for _, entry := range []string{"watcher", "post-persist-hook"} {
		t.Run(
			entry, func(t *testing.T) {
				synctest.Test(
					t, func(t *testing.T) {
						manager := coreauth.NewManager(nil, nil, nil)
						service := &Service{cfg: &config.Config{}, coreManager: manager}
						paused := &pausedAuthModelRegistration{
							entered: make(chan struct{}), release: make(chan struct{}),
						}
						blocked := &coreauth.Auth{ID: t.Name() + "-blocked", Provider: "qwen", Runtime: paused}
						independent := &coreauth.Auth{ID: t.Name() + "-independent", Provider: "qwen"}
						t.Cleanup(
							func() {
								GlobalModelRegistry().UnregisterClient(blocked.ID)
								GlobalModelRegistry().UnregisterClient(independent.ID)
								_ = manager.StopDeferredAuthRecreates(context.Background())
							},
						)
						apply := func(auth *coreauth.Auth) {
							if entry == "post-persist-hook" {
								if err := service.runtimeAuthSyncHook()(t.Context(), auth); err != nil {
									t.Error(err)
								}
								return
							}
							update := AuthUpdate{Action: AuthUpdateActionAdd, ID: auth.ID, Auth: auth}
							update.SetRevision(7)
							if err := service.handleAuthUpdate(
								coreauth.WithSkipPersist(t.Context()),
								update,
							); err != nil {
								t.Error(err)
							}
						}
						blockedDone := make(chan struct{})
						go func() {
							apply(blocked)
							close(blockedDone)
						}()
						<-paused.entered
						if !service.authUpdateMu.TryLock() {
							t.Error("model registration still holds the global auth-update mutex")
							close(paused.release)
							<-blockedDone
							return
						}
						service.authUpdateMu.Unlock()
						independentDone := make(chan struct{})
						go func() {
							apply(independent)
							close(independentDone)
						}()
						synctest.Wait()
						select {
						case <-independentDone:
							if len(GlobalModelRegistry().GetModelsForClient(independent.ID)) == 0 {
								t.Error("independent update returned before publishing its actual models")
							}
						default:
							t.Error("independent auth update waited for another auth's model registration")
						}
						if service.authUpdateMu.TryLock() {
							service.authUpdateMu.Unlock()
						} else {
							t.Error("model registration still holds the global auth-update mutex")
						}
						close(paused.release)
						<-blockedDone
						<-independentDone
					},
				)
			},
		)
	}
}

func TestAuthUpdateDuplicateCompletionAndCanceledWaiter(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			service := &Service{cfg: &config.Config{}, coreManager: manager}
			paused := &pausedAuthModelRegistration{entered: make(chan struct{}), release: make(chan struct{})}
			auth := &coreauth.Auth{ID: t.Name(), Provider: "qwen", Runtime: paused}
			t.Cleanup(
				func() {
					GlobalModelRegistry().UnregisterClient(auth.ID)
					_ = manager.StopDeferredAuthRecreates(context.Background())
				},
			)
			update := AuthUpdate{Action: AuthUpdateActionAdd, ID: auth.ID, Auth: auth}
			update.SetRevision(7)
			originalDone := make(chan struct{})
			go func() {
				if err := service.handleAuthUpdate(coreauth.WithSkipPersist(t.Context()), update); err != nil {
					t.Error(err)
				}
				close(originalDone)
			}()
			<-paused.entered
			if !service.authUpdateMu.TryLock() {
				t.Error("global auth-update mutex prevents cancellation-aware completion waiting")
				close(paused.release)
				<-originalDone
				return
			}
			service.authUpdateMu.Unlock()
			duplicateDone := make(chan struct{})
			go func() {
				if err := service.handleAuthUpdate(coreauth.WithSkipPersist(t.Context()), update); err != nil {
					t.Error(err)
				}
				close(duplicateDone)
			}()
			waitCtx, cancel := context.WithCancel(t.Context())
			canceledDone := make(chan struct{})
			go func() {
				newer := update
				newer.SetRevision(8)
				if err := service.handleAuthUpdate(waitCtx, newer); !errors.Is(err, context.Canceled) {
					t.Errorf("canceled auth-update waiter returned %v, want context cancellation", err)
				}
				close(canceledDone)
			}()
			synctest.Wait()
			select {
			case <-duplicateDone:
				t.Error("same-revision caller returned before model publication completed")
			default:
			}
			cancel()
			synctest.Wait()
			select {
			case <-canceledDone:
			default:
				t.Error("canceled auth-registration waiter did not return independently")
			}
			close(paused.release)
			<-originalDone
			<-duplicateDone
			<-canceledDone
			if service.authUpdateRevisions[auth.ID] != 7 {
				t.Errorf("canceled waiter consumed revision %d", service.authUpdateRevisions[auth.ID])
			}
			if len(GlobalModelRegistry().GetModelsForClient(auth.ID)) == 0 {
				t.Error("completed update has no published models")
			}
			service.authApplyMu.Lock()
			remaining := len(service.authApplyLocks)
			service.authApplyMu.Unlock()
			if remaining != 0 {
				t.Errorf("completed and canceled callers retained %d per-auth wait entries", remaining)
			}
		},
	)
}

func TestAuthUpdateBatchDuplicateCompletionIsPerAuth(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			service := &Service{cfg: &config.Config{}, coreManager: manager}
			batch, ok := any(service).(interface {
				handleAuthUpdates(context.Context, []AuthUpdate)
			})
			if !ok {
				t.Fatal("auth-update batch completion entry point is missing")
			}
			paused := &pausedAuthModelRegistration{entered: make(chan struct{}), release: make(chan struct{})}
			a := &coreauth.Auth{ID: t.Name() + "-a", Provider: "qwen"}
			b := &coreauth.Auth{ID: t.Name() + "-b", Provider: "qwen", Runtime: paused}
			t.Cleanup(
				func() {
					GlobalModelRegistry().UnregisterClient(a.ID)
					GlobalModelRegistry().UnregisterClient(b.ID)
					_ = manager.StopDeferredAuthRecreates(context.Background())
				},
			)
			updates := []AuthUpdate{
				{Action: AuthUpdateActionAdd, ID: a.ID, Auth: a},
				{Action: AuthUpdateActionAdd, ID: b.ID, Auth: b},
			}
			for i := range updates {
				updates[i].SetRevision(7)
			}
			batchDone := make(chan struct{})
			go func() {
				batch.handleAuthUpdates(coreauth.WithSkipPersist(t.Context()), updates)
				close(batchDone)
			}()
			<-paused.entered
			aDone := make(chan struct{})
			go func() {
				if err := service.handleAuthUpdate(t.Context(), updates[0]); err != nil {
					t.Error(err)
				}
				close(aDone)
			}()
			synctest.Wait()
			select {
			case <-aDone:
			default:
				t.Error("completed auth A waited for unrelated auth B in the same batch")
			}
			select {
			case <-batchDone:
				t.Error("batch returned before auth B completed model publication")
			default:
			}
			close(paused.release)
			<-batchDone
			<-aDone
			for _, auth := range []*coreauth.Auth{a, b} {
				if service.authUpdateRevisions[auth.ID] != 7 ||
					len(GlobalModelRegistry().GetModelsForClient(auth.ID)) == 0 {
					t.Error("same-revision batch lost independent auth publication")
				}
			}
		},
	)
}

func TestManagementStatusPublishesIndependentAuthDuringModelRegistration(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	t.Setenv("WRITABLE_PATH", "")
	t.Setenv("writable_path", "")
	dir := t.TempDir()
	synctest.Test(
		t, func(t *testing.T) {
			store := sdkAuth.NewFileTokenStore()
			store.SetBaseDir(dir)
			manager := coreauth.NewManager(store, nil, nil)
			service := &Service{cfg: &config.Config{AuthDir: dir}, coreManager: manager}
			paused := &pausedAuthModelRegistration{entered: make(chan struct{}), release: make(chan struct{})}
			for _, file := range []string{"synthetic-status-a.json", "synthetic-status-b.json"} {
				auth := &coreauth.Auth{
					ID: file, FileName: file, Provider: "qwen", Disabled: true, Status: coreauth.StatusDisabled,
					Metadata:   map[string]any{"type": "qwen", "access_token": "synthetic-status-token"},
					Attributes: map[string]string{coreauth.AttributePath: filepath.Join(dir, file)},
				}
				if file == "synthetic-status-a.json" {
					auth.Runtime = paused
				}
				if _, err := manager.Register(t.Context(), auth); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(file) })
			}
			t.Cleanup(func() { _ = manager.StopDeferredAuthRecreates(context.Background()) })
			handler := management.NewHandler(service.cfg, "", manager)
			handler.SetPostAuthPersistHook(service.runtimeAuthSyncHook())
			t.Cleanup(
				func() {
					if err := handler.Close(); err != nil {
						t.Error(err)
					}
				},
			)
			patch := func(name string) int {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(
					http.MethodPatch, "/auth-files/status",
					strings.NewReader(`{"name":"`+name+`","disabled":false}`),
				)
				ctx.Request.Header.Set("Content-Type", "application/json")
				handler.PatchAuthFileStatus(ctx)
				return recorder.Code
			}
			aDone := make(chan int, 1)
			go func() { aDone <- patch("synthetic-status-a.json") }()
			<-paused.entered
			bDone := make(chan int, 1)
			go func() { bDone <- patch("synthetic-status-b.json") }()
			synctest.Wait()
			select {
			case code := <-bDone:
				if code != http.StatusOK ||
					len(GlobalModelRegistry().GetModelsForClient("synthetic-status-b.json")) == 0 {
					t.Errorf("independent status response=%d without complete model publication", code)
				}
			default:
				t.Error("actual management status request waited for unrelated model registration")
			}
			close(paused.release)
			if code := <-aDone; code != http.StatusOK {
				t.Errorf("original status response=%d", code)
			}
			persisted, err := store.List(t.Context())
			if err != nil || len(persisted) != 2 {
				t.Fatalf("temporary persisted auth count=%d error=%v", len(persisted), err)
			}
			for _, auth := range persisted {
				if auth.Disabled || len(GlobalModelRegistry().GetModelsForClient(auth.ID)) == 0 {
					t.Error("completed management status lost its persisted state or runtime models")
				}
			}
		},
	)
}

func TestAuthUpdateBatchPreservesUnversionedPerAuthOrder(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	id := t.Name()
	t.Cleanup(
		func() {
			GlobalModelRegistry().UnregisterClient(id)
			_ = manager.StopDeferredAuthRecreates(context.Background())
		},
	)
	var updates []AuthUpdate
	for _, marker := range []string{"first", "second", "last"} {
		updates = append(
			updates, AuthUpdate{
				Action: AuthUpdateActionModify, ID: id,
				Auth: &coreauth.Auth{ID: id, Provider: "qwen", Metadata: map[string]any{"marker": marker}},
			},
		)
	}
	service.handleAuthUpdates(coreauth.WithSkipPersist(t.Context()), updates)
	current, ok := manager.GetByID(id)
	if !ok || current.Metadata["marker"] != "last" || len(GlobalModelRegistry().GetModelsForClient(id)) == 0 {
		t.Fatal("concurrent batch processing changed unversioned per-auth input order")
	}
}

func TestAuthUpdateQueueRegistersIndependentAuthsConcurrently(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			service := &Service{cfg: &config.Config{}, coreManager: manager, authUpdates: make(chan AuthUpdate, 2)}
			paused := &pausedAuthModelRegistration{entered: make(chan struct{}), release: make(chan struct{})}
			a := &coreauth.Auth{ID: t.Name() + "-a", Provider: "qwen", Runtime: paused}
			b := &coreauth.Auth{ID: t.Name() + "-b", Provider: "qwen"}
			for _, auth := range []*coreauth.Auth{a, b} {
				update := AuthUpdate{Action: AuthUpdateActionAdd, ID: auth.ID, Auth: auth}
				update.SetRevision(7)
				service.authUpdates <- update
				t.Cleanup(func() { GlobalModelRegistry().UnregisterClient(auth.ID) })
			}
			close(service.authUpdates)
			service.authQueueWorkers.Add(1)
			done := make(chan struct{})
			go func() {
				service.consumeAuthUpdates(t.Context())
				close(done)
			}()
			<-paused.entered
			synctest.Wait()
			if _, ok := manager.GetByID(b.ID); !ok || len(GlobalModelRegistry().GetModelsForClient(b.ID)) == 0 {
				t.Error("actual queued batch serialized independent model registration behind auth A")
			}
			close(paused.release)
			<-done
			service.authQueueWorkers.Wait()
			if len(GlobalModelRegistry().GetModelsForClient(a.ID)) == 0 {
				t.Error("queued batch returned before auth A completed")
			}
			if err := manager.StopDeferredAuthRecreates(context.Background()); err != nil {
				t.Fatal(err)
			}
		},
	)
}

func TestAuthUpdateQueueClosedBatchCompletesAndReleasesWaits(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			manager := coreauth.NewManager(nil, nil, nil)
			service := &Service{cfg: &config.Config{}, coreManager: manager, authUpdates: make(chan AuthUpdate, 2)}
			auth := &coreauth.Auth{ID: t.Name(), Provider: "qwen"}
			update := AuthUpdate{Action: AuthUpdateActionAdd, ID: auth.ID, Auth: auth}
			update.SetRevision(4)
			service.authUpdates <- update
			service.authUpdates <- update
			close(service.authUpdates)
			service.authQueueWorkers.Add(1)
			service.consumeAuthUpdates(t.Context())
			service.authQueueWorkers.Wait()
			t.Cleanup(
				func() {
					GlobalModelRegistry().UnregisterClient(auth.ID)
					_ = manager.StopDeferredAuthRecreates(context.Background())
				},
			)
			if service.authUpdateRevisions[auth.ID] != 4 ||
				len(GlobalModelRegistry().GetModelsForClient(auth.ID)) == 0 {
				t.Error("closing the queue lost its last complete auth batch")
			}
			service.authApplyMu.Lock()
			remaining := len(service.authApplyLocks)
			service.authApplyMu.Unlock()
			if remaining != 0 {
				t.Errorf("closed auth-update queue retained %d waiter entries", remaining)
			}
		},
	)
}
