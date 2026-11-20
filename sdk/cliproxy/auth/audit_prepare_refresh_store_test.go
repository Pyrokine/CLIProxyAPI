package auth

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"

	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type auditPublicSaveStore struct {
	mu          sync.Mutex
	records     map[string]*Auth
	entered     chan struct{}
	release     chan struct{}
	listed      chan struct{}
	listRelease chan struct{}
}

func (*auditPublicSaveStore) Delete(context.Context, string) error { return nil }

func (s *auditPublicSaveStore) Save(_ context.Context, auth *Auth) (string, error) {
	close(s.entered)
	<-s.release
	auth.FileName = auth.ID + ".json"
	s.mu.Lock()
	s.records[auth.ID] = auth.Clone()
	s.mu.Unlock()
	return auth.FileName, nil
}

func (s *auditPublicSaveStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	items := make([]*Auth, 0, len(s.records))
	for _, auth := range s.records {
		items = append(items, auth.Clone())
	}
	s.mu.Unlock()
	close(s.listed)
	if s.listRelease != nil {
		<-s.listRelease
	}
	return items, nil
}

func TestAuditActualPrepareRefreshSaveSerializesLoad(t *testing.T) {
	for _, operation := range []string{"Prepare", "Refresh"} {
		t.Run(
			operation, func(t *testing.T) {
				synctest.Test(
					t, func(t *testing.T) {
						ctx := t.Context()
						store := &auditPublicSaveStore{
							records: make(map[string]*Auth), entered: make(chan struct{}), release: make(chan struct{}),
							listed: make(chan struct{}),
						}
						manager := NewManager(store, nil, nil)
						manager.SetHomeClientProvider(func() HomeClient { return nil })
						executor := &auditPublicationExecutor{}
						manager.RegisterExecutor(executor)
						registerSchedulerModels(t, "test", "save-model", "B")
						var base *Auth
						for _, id := range []string{"A", "B"} {
							registered, err := manager.Register(
								WithSkipPersist(ctx), &Auth{
									ID: id, Provider: "test", Status: StatusActive, FileName: id + ".json",
									Metadata: map[string]any{
										"access_token": "synthetic-initial", "refresh_token": "synthetic-owner",
									},
								},
							)
							if err != nil {
								t.Fatal(err)
							}
							store.records[id] = registered.Clone()
							if id == "A" {
								base = registered
							}
						}
						saveDone := make(chan error, 1)
						go func() {
							var err error
							if operation == "Prepare" {
								_, err = manager.PrepareRequestAuth(ctx, executor, base)
							} else {
								_, err = manager.ForceRefreshAuth(ctx, base.ID)
							}
							saveDone <- err
						}()
						<-store.entered
						loadDone := make(chan error, 1)
						go func() { loadDone <- manager.Load(ctx) }()
						synctest.Wait()
						select {
						case <-store.listed:
							t.Error("Load observed storage before Save/publication completed")
						default:
						}
						if unrelated, ok := manager.GetByID("B"); !ok || unrelated == nil {
							t.Error("unrelated B disappeared during Save")
						}
						if selected, err := manager.scheduler.pickSingle(
							ctx, "test", "save-model", core.Options{}, nil,
						); err != nil || selected == nil || selected.ID != "B" {
							t.Errorf("unrelated selection=%#v error=%v", selected, err)
						}
						close(store.release)
						if err := <-saveDone; err != nil {
							t.Fatal(err)
						}
						if err := <-loadDone; err != nil {
							t.Fatal(err)
						}
						current, _ := manager.GetByID("A")
						store.mu.Lock()
						saved := store.records["A"].Clone()
						store.mu.Unlock()
						if !reflect.DeepEqual(saved.Metadata, current.Metadata) || saved.FileName != current.FileName {
							t.Fatalf("store/runtime differ: saved=%#v current=%#v", saved, current)
						}
						if operation == "Prepare" && current.Metadata["project_id"] != "synthetic-project-1" {
							t.Fatalf("actual preparation was not persisted: %#v", current.Metadata)
						}
						if operation == "Refresh" && current.Metadata["access_token"] != "synthetic-access-1" {
							t.Fatalf("actual refresh was not persisted: %#v", current.Metadata)
						}
						manager.scheduler.mu.Lock()
						scheduled := manager.scheduler.providers["test"].auths["A"].auth.Clone()
						manager.scheduler.mu.Unlock()
						if !reflect.DeepEqual(
							current.Metadata, scheduled.Metadata,
						) || current.Generation != scheduled.Generation {
							t.Fatal("scheduler differs from the reloaded credential")
						}
					},
				)
			},
		)
	}
}
