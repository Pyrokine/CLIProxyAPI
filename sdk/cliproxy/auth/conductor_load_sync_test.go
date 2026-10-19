package auth

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
)

type loadDuringSaveStore struct {
	mu          sync.Mutex
	auths       map[string]*Auth
	blockSave   bool
	saveEntered chan struct{}
	saveRelease chan struct{}
	listEntered chan struct{}
}

func (s *loadDuringSaveStore) List(context.Context) ([]*Auth, error) {
	s.mu.Lock()
	items := make([]*Auth, 0, len(s.auths))
	for _, auth := range s.auths {
		items = append(items, auth.Clone())
	}
	s.mu.Unlock()
	close(s.listEntered)
	return items, nil
}

func (s *loadDuringSaveStore) Save(_ context.Context, auth *Auth) (string, error) {
	s.mu.Lock()
	block := s.blockSave
	s.mu.Unlock()
	if block {
		close(s.saveEntered)
		<-s.saveRelease
	}
	s.mu.Lock()
	s.auths[auth.ID] = auth.Clone()
	s.mu.Unlock()
	return auth.ID, nil
}

func (*loadDuringSaveStore) Delete(context.Context, string) error { return nil }

func TestLoadWaitsForInFlightResultSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &loadDuringSaveStore{
			auths: make(map[string]*Auth), saveEntered: make(chan struct{}),
			saveRelease: make(chan struct{}), listEntered: make(chan struct{}),
		}
		manager := NewManager(store, nil, nil)
		registered, err := manager.Register(context.Background(), &Auth{
			ID: "load-result.json", Provider: "fixture", Status: StatusActive,
			Metadata: map[string]any{"access_token": "token"},
		})
		if err != nil {
			t.Fatal(err)
		}
		store.mu.Lock()
		store.blockSave = true
		store.mu.Unlock()
		saveDone := make(chan struct{})
		go func() {
			manager.MarkResult(context.Background(), Result{
				AuthID: registered.ID, AuthGeneration: registered.Generation,
				Provider: registered.Provider, Model: "model", Success: true,
			})
			close(saveDone)
		}()
		<-store.saveEntered
		loadDone := make(chan error, 1)
		go func() { loadDone <- manager.Load(context.Background()) }()
		synctest.Wait()
		select {
		case <-store.listEntered:
			t.Error("Load read an old store snapshot before result Save completed")
		default:
		}
		close(store.saveRelease)
		<-saveDone
		if errLoad := <-loadDone; errLoad != nil {
			t.Fatal(errLoad)
		}
		current, ok := manager.GetByID(registered.ID)
		if !ok || current.Success != 1 {
			t.Fatalf("Load lost the saved result: %#v", current)
		}
	})
}
