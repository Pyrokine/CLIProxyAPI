package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingStore struct {
	saveCount atomic.Int32
	saveErr   error
	list      []*Auth
}

func (s *countingStore) List(context.Context) ([]*Auth, error) {
	items := make([]*Auth, 0, len(s.list))
	for _, auth := range s.list {
		if auth != nil {
			items = append(items, auth.Clone())
		}
	}
	return items, nil
}

func (s *countingStore) Save(context.Context, *Auth) (string, error) {
	s.saveCount.Add(1)
	return "", s.saveErr
}

func (s *countingStore) Delete(context.Context, string) error { return nil }

func TestWithSkipPersist_DisablesUpdatePersistence(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "antigravity",
		Metadata: map[string]any{"type": "antigravity"},
	}

	if _, err := mgr.Update(context.Background(), auth); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("expected 1 Save call, got %d", got)
	}

	ctxSkip := WithSkipPersist(context.Background())
	if _, err := mgr.Update(ctxSkip, auth); err != nil {
		t.Fatalf("Update(skipPersist) returned error: %v", err)
	}
	if got := store.saveCount.Load(); got != 1 {
		t.Fatalf("expected Save call count to remain 1, got %d", got)
	}
}

func TestWithSkipPersist_DisablesRegisterPersistence(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "antigravity",
		Metadata: map[string]any{"type": "antigravity"},
	}

	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register(skipPersist) returned error: %v", err)
	}
	if got := store.saveCount.Load(); got != 0 {
		t.Fatalf("expected 0 Save calls, got %d", got)
	}
}

func TestConfigAPIKeysSkipPersistence(t *testing.T) {
	for _, source := range []string{"config", "config:fixture[synthetic]", "file"} {
		for _, operation := range []string{"Register", "Update", "MergePersistentUpdate", "MarkResult"} {
			t.Run(source+"/"+operation, func(t *testing.T) {
				store := &countingStore{}
				manager := NewManager(store, nil, nil)
				base, err := manager.Register(WithSkipPersist(t.Context()), &Auth{
					ID: "config-persistence", Provider: "fixture", Status: StatusActive,
					Attributes: map[string]string{"source": source, "api_key": "synthetic-key"},
					Metadata:   map[string]any{"request_retry": 1},
				})
				if err != nil {
					t.Fatal(err)
				}
				next := base.Clone()
				next.Label = "updated"
				switch operation {
				case "Register":
					_, err = manager.Register(t.Context(), next)
				case "Update":
					_, err = manager.Update(t.Context(), next)
				case "MergePersistentUpdate":
					_, err = manager.MergePersistentUpdate(t.Context(), base, next)
				case "MarkResult":
					manager.MarkResult(t.Context(), Result{
						AuthID: base.ID, AuthGeneration: base.Generation, Provider: base.Provider, Success: true,
					})
				}
				if err != nil {
					t.Fatal(err)
				}
				wantSaves := int32(0)
				if source == "file" {
					wantSaves = 1
				}
				if got := store.saveCount.Load(); got != wantSaves {
					t.Fatalf("Save count = %d, want %d", got, wantSaves)
				}
				current, ok := manager.GetByID(base.ID)
				if !ok || current == nil {
					t.Fatal("credential was not published")
				}
				if operation == "MarkResult" {
					if current.Success != 1 {
						t.Fatal("request accounting was not published")
					}
				} else if current.Label != next.Label {
					t.Fatal("credential update was not published")
				}
			})
		}
	}
}

func TestRegisterReturnsPersistError(t *testing.T) {
	persistErr := errors.New("persist failed")
	store := &countingStore{saveErr: persistErr}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "antigravity",
		Metadata: map[string]any{"type": "antigravity"},
	}

	if _, err := mgr.Register(context.Background(), auth); !errors.Is(err, persistErr) {
		t.Fatalf("Register error = %v, want %v", err, persistErr)
	}
	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("auth was registered after persist failure")
	}
}

func TestUpdateReturnsPersistError(t *testing.T) {
	persistErr := errors.New("persist failed")
	store := &countingStore{saveErr: persistErr}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "auth-1",
		Provider: "antigravity",
		Metadata: map[string]any{"type": "antigravity"},
	}

	if _, err := mgr.Update(context.Background(), auth); !errors.Is(err, persistErr) {
		t.Fatalf("Update error = %v, want %v", err, persistErr)
	}
	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatalf("auth was updated after persist failure")
	}
}

func TestPersistAndSetStoreAreSynchronized(t *testing.T) {
	store := &countingStore{}
	mgr := NewManager(store, nil, nil)
	auth := &Auth{
		ID:       "xai-auth",
		Provider: "xai",
		Metadata: map[string]any{"type": "xai"},
	}

	for range 100 {
		mgr.SetStore(store)
		start := make(chan struct{})
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			_, _ = mgr.persist(context.Background(), auth, true, false, 0)
		}()
		go func() {
			defer group.Done()
			<-start
			mgr.SetStore(nil)
		}()
		close(start)
		group.Wait()
	}
}

func TestLoadClearsTombstoneForLoadedAuth(t *testing.T) {
	store := &countingStore{
		list: []*Auth{
			{
				ID:       "auth-1",
				Provider: "codex",
				Metadata: map[string]any{"token": "old"},
			},
		},
	}
	mgr := NewManager(store, nil, nil)
	if errRemove := mgr.Remove(context.Background(), "auth-1"); errRemove != nil {
		t.Fatalf("Remove returned error: %v", errRemove)
	}

	if err := mgr.Load(context.Background()); err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	updated, err := mgr.Update(
		context.Background(), &Auth{
			ID:       "auth-1",
			Provider: "codex",
			Metadata: map[string]any{"token": "new"},
		},
	)
	if err != nil {
		t.Fatalf("Update after Load returned error: %v", err)
	}
	if updated == nil {
		t.Fatal("expected loaded auth to be updatable")
	}
	if updated.Metadata["token"] != "new" {
		t.Fatalf("token = %#v, want new", updated.Metadata["token"])
	}
}

func TestMarkResultPersistsWithoutDeadlock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		success bool
	}{
		{name: "success", success: true},
		{name: "failure", success: false},
	} {
		t.Run(
			tc.name, func(t *testing.T) {
				store := &countingStore{}
				mgr := NewManager(store, nil, nil)
				if _, err := mgr.Register(
					context.Background(), &Auth{
						ID:       "auth-1",
						Provider: "antigravity",
						Metadata: map[string]any{"type": "antigravity"},
					},
				); err != nil {
					t.Fatalf("register auth: %v", err)
				}
				before := store.saveCount.Load()
				done := make(chan struct{})
				go func() {
					result := Result{
						AuthID:   "auth-1",
						Provider: "antigravity",
						Model:    "claude-test",
						Success:  tc.success,
					}
					if !tc.success {
						result.Error = &Error{Message: "failed"}
					}
					mgr.MarkResult(context.Background(), result)
					close(done)
				}()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for MarkResult")
				}
				if got := store.saveCount.Load(); got <= before {
					t.Fatalf("Save count = %d, want > %d", got, before)
				}
			},
		)
	}
}
