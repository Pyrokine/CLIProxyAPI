package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type originalLifecycleExecutor struct {
	auditLegacyVersionExecutor
	mutate func(context.Context, *Auth) (*Auth, error)
}

func (*originalLifecycleExecutor) ShouldPrepareRequestAuth(*Auth) bool { return true }
func (e *originalLifecycleExecutor) PrepareRequestAuth(ctx context.Context, auth *Auth) (*Auth, error) {
	return e.mutate(ctx, auth)
}
func (e *originalLifecycleExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return e.mutate(ctx, auth)
}

func TestManagerUsesPublishedLifecycleSurface(t *testing.T) {
	for _, name := range []string{
		"UpdatePreparedAuth", "UpdateRefreshedAuth", "SetHomeExecutionRegistry", "ClearHomeExecutionRegistry",
		"HomeExecutionRegistry",
	} {
		if _, found := reflect.TypeFor[*Manager]().MethodByName(name); found {
			t.Fatalf("unpublished compatibility method %s was added to Manager", name)
		}
	}
}

func assertOriginalUnrelatedAuthReadable(t *testing.T, manager *Manager) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		if _, found := manager.GetByID("B"); !found {
			done <- errors.New("B disappeared")
			return
		}
		selected, err := manager.SelectAuth(context.Background(), "audit-b", "", executor.Options{})
		if err == nil && (selected == nil || selected.ID != "B") {
			err = errors.New("did not select B")
		}
		done <- err
	}()
	synctest.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Error(err)
		}
	default:
		t.Error("storage I/O blocked reading/selecting unrelated B")
	}
}

func TestOriginalManagerBlockedSaveSerializesLoad(t *testing.T) {
	for _, operation := range []string{"Register", "ReRegister", "Update", "Prepare", "Refresh", "MarkResult"} {
		t.Run(
			operation, func(t *testing.T) {
				synctest.Test(
					t, func(t *testing.T) {
						ctx := context.Background()
						store := &auditPublicSaveStore{
							records: make(
								map[string]*Auth,
							), entered: make(chan struct{}), release: make(chan struct{}),
							listed: make(chan struct{}),
						}
						manager := NewManager(store, nil, nil)
						manager.RegisterExecutor(&auditLegacyVersionExecutor{id: "audit-b"})
						preparer := &originalLifecycleExecutor{
							id: "meta",
							mutate: func(_ context.Context, auth *Auth) (*Auth, error) {
								updated := auth.Clone()
								updated.Metadata["access_token"] = "minted"
								return updated, nil
							},
						}
						manager.RegisterExecutor(preparer)
						b, err := manager.Register(
							WithSkipPersist(ctx), &Auth{ID: "B", Provider: "audit-b", Status: StatusActive},
						)
						if err != nil {
							t.Fatal(err)
						}
						store.records[b.ID] = b.Clone()
						base := &Auth{
							ID: "A", Provider: "meta", Status: StatusActive,
							Metadata: map[string]any{"access_token": "old"},
						}
						if operation != "Register" {
							base, err = manager.Register(WithSkipPersist(ctx), base)
							if err != nil {
								t.Fatal(err)
							}
							store.records[base.ID] = base.Clone()
						}
						updated := base.Clone()
						updated.Metadata["access_token"] = "minted"
						saveDone := make(chan error, 1)
						go func() {
							var errSave error
							switch operation {
							case "Register", "ReRegister":
								_, errSave = manager.Register(ctx, updated)
							case "Update":
								_, errSave = manager.Update(ctx, updated)
							case "Prepare":
								_, errSave = manager.PrepareRequestAuth(ctx, preparer, base)
							case "Refresh":
								_, errSave = manager.ForceRefreshAuth(ctx, base.ID)
							case "MarkResult":
								manager.MarkResult(
									ctx, Result{
										AuthID: base.ID, AuthGeneration: base.Generation, Provider: base.Provider,
										Success: true,
									},
								)
							}
							saveDone <- errSave
						}()
						<-store.entered
						loadDone := make(chan error, 1)
						go func() { loadDone <- manager.Load(ctx) }()
						synctest.Wait()
						select {
						case <-store.listed:
							t.Error("Load read stale records before Save/publication completed")
						default:
						}
						assertOriginalUnrelatedAuthReadable(t, manager)
						close(store.release)
						if errSave := <-saveDone; errSave != nil {
							t.Errorf("save/publication failed: %v", errSave)
						}
						if errLoad := <-loadDone; errLoad != nil {
							t.Fatal(errLoad)
						}
						got, found := manager.GetByID(base.ID)
						if !found {
							t.Fatal("Load lost the saved credential")
						}
						store.mu.Lock()
						saved := store.records[base.ID].Clone()
						store.mu.Unlock()
						savedJSON, errSaved := json.Marshal(saved)
						currentJSON, errCurrent := json.Marshal(got)
						if errSaved != nil || errCurrent != nil || !bytes.Equal(savedJSON, currentJSON) ||
							saved.FileName !=
								got.FileName ||
							saved.Index !=
								got.Index ||
							saved.Success !=
								got.Success ||
							saved.Failed !=
								got.Failed ||
							got.Generation <= saved.Generation {
							t.Fatalf(
								"store and manager diverged: saved=%s current=%s errors=%v/%v",
								savedJSON,
								currentJSON,
								errSaved,
								errCurrent,
							)
						}
						manager.scheduler.mu.Lock()
						scheduled := manager.scheduler.providers["meta"].auths[base.ID].auth.Clone()
						manager.scheduler.mu.Unlock()
						scheduledJSON, errScheduled := json.Marshal(scheduled)
						if errScheduled != nil || !bytes.Equal(
							currentJSON, scheduledJSON,
						) || got.Generation != scheduled.Generation {
							t.Fatalf(
								"scheduler differs from reloaded credential: %s, %v",
								scheduledJSON,
								errScheduled,
							)
						}
					},
				)
			},
		)
	}
}

func TestOriginalLifecyclePersistenceFailuresReturnErrors(t *testing.T) {
	for _, test := range []struct {
		operation, id, provider string
		metadata                map[string]any
	}{
		{"Register", "auth-register-persist-1", "claude", map[string]any{"access_token": "at", "refresh_token": "rt"}},
		{"Update", "auth-update-persist-1", "claude", map[string]any{"access_token": "at", "refresh_token": "rt"}},
		{
			"Refresh", "auth-refresh-persist-1", "claude",
			map[string]any{"access_token": "stale-access-token", "refresh_token": "rt"},
		},
		{"MetaRefresh", "auth-meta-persist-1", "meta", map[string]any{"dca_token": "dca:valid"}},
	} {
		t.Run(
			test.operation, func(t *testing.T) {
				failure := errors.New("persist store failure: permission denied")
				store := &auditLegacyUpdateStore{fail: failure}
				manager := NewManager(store, nil, nil)
				t.Cleanup(
					func() {
						manager.cancelPendingRefreshPersistence(test.id)
						if err := manager.StopDeferredAuthRecreates(context.Background()); err != nil &&
							!errors.Is(
								err, ErrAuthDurableStateUncertain,
							) {
							t.Errorf("stop persistence work: %v", err)
						}
					},
				)
				auth := &Auth{ID: test.id, Provider: test.provider, Metadata: test.metadata}
				if test.operation != "Register" {
					if _, err := manager.Register(WithSkipPersist(t.Context()), auth); err != nil {
						t.Fatal(err)
					}
				}
				before, _ := manager.GetByID(test.id)
				var err error
				switch test.operation {
				case "Register":
					_, err = manager.Register(t.Context(), auth)
				case "Update":
					_, err = manager.Update(t.Context(), auth)
				case "Refresh", "MetaRefresh":
					provider := &originalLifecycleExecutor{
						id: test.provider,
						mutate: func(_ context.Context, auth *Auth) (*Auth, error) {
							updated := auth.Clone()
							updated.Metadata["access_token"] = "fresh-access-token"
							return updated, nil
						},
					}
					manager.RegisterExecutor(provider)
					_, err = manager.ForceRefreshAuth(t.Context(), auth.ID)
				}
				if !errors.Is(err, failure) || store.saved != nil {
					t.Fatalf("persistence failure was hidden: error=%v saved=%+v", err, store.saved)
				}
				after, _ := manager.GetByID(test.id)
				if (test.operation == "Register" || test.operation == "Update") &&
					!reflect.DeepEqual(before, after) {
					t.Fatalf(
						"failed ordinary save changed published state: before=%+v after=%+v",
						before,
						after,
					)
				}
				if test.operation == "Refresh" || test.operation == "MetaRefresh" {
					if !errors.Is(
						err, ErrAuthDurableStateUncertain,
					) || after == nil || authAccessToken(after) != "fresh-access-token" {
						t.Fatalf(
							"issued refresh material or durable-state error was lost: current=%+v error=%v",
							after,
							err,
						)
					}
				}
			},
		)
	}
}

func TestOriginalManagerBlockedLoadSerializesNewRegistration(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			ctx := context.Background()
			store := &auditPublicSaveStore{
				records: make(map[string]*Auth), entered: make(chan struct{}), release: make(chan struct{}),
				listed: make(chan struct{}), listRelease: make(chan struct{}),
			}
			manager := NewManager(store, nil, nil)
			manager.RegisterExecutor(&auditLegacyVersionExecutor{id: "audit-b"})
			b, err := manager.Register(
				WithSkipPersist(ctx),
				&Auth{ID: "B", Provider: "audit-b", Status: StatusActive},
			)
			if err != nil {
				t.Fatal(err)
			}
			store.records[b.ID] = b.Clone()
			loadDone := make(chan error, 1)
			go func() { loadDone <- manager.Load(ctx) }()
			<-store.listed
			saveDone := make(chan error, 1)
			go func() {
				_, errRegister := manager.Register(
					ctx, &Auth{ID: "new", Provider: "audit", Metadata: map[string]any{"type": "audit"}},
				)
				saveDone <- errRegister
			}()
			synctest.Wait()
			select {
			case <-store.entered:
				t.Error("new registration overtook Load's whole-map replacement")
			default:
			}
			assertOriginalUnrelatedAuthReadable(t, manager)
			close(store.listRelease)
			if errLoad := <-loadDone; errLoad != nil {
				t.Fatal(errLoad)
			}
			<-store.entered
			close(store.release)
			if errSave := <-saveDone; errSave != nil {
				t.Fatal(errSave)
			}
			if got, found := manager.GetByID("new"); !found || got.FileName != "new.json" {
				t.Fatal("Load discarded a new registration")
			}
		},
	)
}
