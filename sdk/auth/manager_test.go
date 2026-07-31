package auth

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

type cancellingManagerAuthenticator struct {
	cancel context.CancelFunc
}

func (a *cancellingManagerAuthenticator) Provider() string {
	return "cancel-test"
}

func (a *cancellingManagerAuthenticator) Login(
	context.Context,
	*config.Config,
	*LoginOptions,
) (*coreauth.Auth, error) {
	if a.cancel != nil {
		a.cancel()
	}
	return &coreauth.Auth{ID: "cancel-test.json", Provider: a.Provider()}, nil
}

func (*cancellingManagerAuthenticator) RefreshLead() *time.Duration {
	return nil
}

type recordingManagerStore struct {
	saves int
}

func (*recordingManagerStore) List(context.Context) ([]*coreauth.Auth, error) {
	return nil, nil
}

func (s *recordingManagerStore) Save(context.Context, *coreauth.Auth) (string, error) {
	s.saves++
	return "saved.json", nil
}

func (*recordingManagerStore) Delete(context.Context, string) error {
	return nil
}

type cancellingBaseDirManagerStore struct {
	recordingManagerStore
	cancel context.CancelFunc
}

func (s *cancellingBaseDirManagerStore) SetBaseDir(string) {
	s.cancel()
}

func TestManagerLoginDoesNotSaveAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &recordingManagerStore{}
	manager := NewManager(store, &cancellingManagerAuthenticator{cancel: cancel})

	record, path, err := manager.Login(ctx, "cancel-test", &config.Config{}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Login error = %v, want context.Canceled", err)
	}
	if record == nil || record.ID != "cancel-test.json" {
		t.Fatalf("Login record = %#v", record)
	}
	if path != "" {
		t.Fatalf("Login path = %q, want empty", path)
	}
	if store.saves != 0 {
		t.Fatalf("store saves = %d, want 0", store.saves)
	}
}

func TestManagerLoginDoesNotMutateSharedStoreBaseDir(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &cancellingBaseDirManagerStore{cancel: cancel}
	manager := NewManager(store, &cancellingManagerAuthenticator{})

	record, path, err := manager.Login(
		ctx,
		"cancel-test",
		&config.Config{AuthDir: t.TempDir()},
		nil,
	)
	if err != nil {
		t.Fatalf("Login error = %v", err)
	}
	if record == nil || record.ID != "cancel-test.json" {
		t.Fatalf("Login record = %#v", record)
	}
	if path != "saved.json" {
		t.Fatalf("Login path = %q, want saved.json", path)
	}
	if store.saves != 1 {
		t.Fatalf("store saves = %d, want 1", store.saves)
	}
	if ctx.Err() != nil {
		t.Fatalf("shared base-dir setter was called: %v", ctx.Err())
	}
}

type fixedManagerAuthenticator struct {
	provider string
	id       string
}

func (a fixedManagerAuthenticator) Provider() string { return a.provider }

func (a fixedManagerAuthenticator) Login(
	context.Context,
	*config.Config,
	*LoginOptions,
) (*coreauth.Auth, error) {
	return &coreauth.Auth{ID: a.id, Provider: a.provider}, nil
}

func (fixedManagerAuthenticator) RefreshLead() *time.Duration { return nil }

type scopedManagerStore struct {
	mu           sync.Mutex
	dirs         map[string]string
	unscopedSave int
}

func (*scopedManagerStore) List(context.Context) ([]*coreauth.Auth, error) { return nil, nil }
func (*scopedManagerStore) Delete(context.Context, string) error           { return nil }
func (s *scopedManagerStore) Save(context.Context, *coreauth.Auth) (string, error) {
	s.mu.Lock()
	s.unscopedSave++
	s.mu.Unlock()
	return "", errors.New("unscoped save called")
}
func (s *scopedManagerStore) SaveWithBaseDir(
	_ context.Context,
	dir string,
	auth *coreauth.Auth,
) (string, error) {
	s.mu.Lock()
	if s.dirs == nil {
		s.dirs = make(map[string]string)
	}
	s.dirs[auth.ID] = dir
	s.mu.Unlock()
	return filepath.Join(dir, auth.ID), nil
}

func TestManagerConcurrentLoginUsesOperationScopedAuthDirs(t *testing.T) {
	store := &scopedManagerStore{}
	firstDir := t.TempDir()
	secondDir := t.TempDir()
	first := NewManager(store, fixedManagerAuthenticator{provider: "first", id: "first.json"})
	second := NewManager(store, fixedManagerAuthenticator{provider: "second", id: "second.json"})

	var wait sync.WaitGroup
	wait.Add(2)
	errorsByLogin := make(chan error, 2)
	go func() {
		defer wait.Done()
		_, path, err := first.Login(
			context.Background(), "first", &config.Config{AuthDir: firstDir}, nil,
		)
		if err == nil && path != filepath.Join(firstDir, "first.json") {
			err = errors.New("first login used the wrong auth directory")
		}
		errorsByLogin <- err
	}()
	go func() {
		defer wait.Done()
		_, path, err := second.Login(
			context.Background(), "second", &config.Config{AuthDir: secondDir}, nil,
		)
		if err == nil && path != filepath.Join(secondDir, "second.json") {
			err = errors.New("second login used the wrong auth directory")
		}
		errorsByLogin <- err
	}()
	wait.Wait()
	close(errorsByLogin)
	for err := range errorsByLogin {
		if err != nil {
			t.Fatal(err)
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.unscopedSave != 0 || store.dirs["first.json"] != firstDir || store.dirs["second.json"] != secondDir {
		t.Fatalf("scoped saves = %#v, unscoped = %d", store.dirs, store.unscopedSave)
	}
}
