package auth

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type enrichingAuthStore struct{ err error }

func (*enrichingAuthStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (*enrichingAuthStore) Delete(context.Context, string) error  { return nil }
func (*enrichingAuthStore) DeleteIfVersion(context.Context, string, PersistedVersion) (bool, error) {
	return true, nil
}
func (s *enrichingAuthStore) Save(_ context.Context, auth *Auth) (string, error) {
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	auth.Attributes[AttributePath] = "/fixture/" + auth.ID + ".json"
	auth.Attributes[AttributeSource] = auth.Attributes[AttributePath]
	auth.Attributes[AttributeSourceBackend] = AuthSourceFile
	auth.FileName = auth.ID + ".json"
	auth.Metadata["disabled"] = auth.Disabled
	return auth.Attributes[AttributePath], s.err
}

type blockingEnrichingAuthStore struct {
	enrichingAuthStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	saved   []*Auth
}

func (s *blockingEnrichingAuthStore) Save(ctx context.Context, auth *Auth) (string, error) {
	s.once.Do(
		func() {
			close(s.entered)
			<-s.release
		},
	)
	path, err := s.enrichingAuthStore.Save(ctx, auth)
	s.mu.Lock()
	s.saved = append(s.saved, auth.Clone())
	s.mu.Unlock()
	return path, err
}

func TestManagerBlockedSaveAllowsUnrelatedCredential(t *testing.T) {
	for _, operation := range []string{"Register", "Update"} {
		t.Run(
			operation, func(t *testing.T) {
				ctx := context.Background()
				store := &blockingEnrichingAuthStore{entered: make(chan struct{}), release: make(chan struct{})}
				manager := NewManager(store, nil, nil)
				manager.RegisterExecutor(&roundExecutor{provider: "audit"})
				if _, err := manager.Register(
					WithSkipPersist(ctx), &Auth{ID: "B", Provider: "audit", Status: StatusActive},
				); err != nil {
					t.Fatal(err)
				}
				input := &Auth{
					ID: "A", Provider: "audit-a", Status: StatusActive, Metadata: map[string]any{"type": "audit-a"},
				}
				if operation == "Update" {
					if _, err := manager.Register(WithSkipPersist(ctx), input); err != nil {
						t.Fatal(err)
					}
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					if operation == "Register" {
						_, _ = manager.Register(ctx, input)
					} else {
						_, _ = manager.Update(ctx, input)
					}
				}()
				<-store.entered
				readDone := make(chan error, 1)
				go func() {
					if _, ok := manager.GetByID("B"); !ok {
						readDone <- errors.New("missing B")
						return
					}
					selected, err := manager.SelectAuth(ctx, "audit", "", cliproxyexecutor.Options{})
					if err == nil && (selected == nil || selected.ID != "B") {
						err = errors.New("did not select B")
					}
					readDone <- err
				}()
				select {
				case err := <-readDone:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(2 * time.Second):
					t.Error("blocked Save(A) prevented reading/selecting unrelated B")
				}
				close(store.release)
				<-done
			},
		)
	}
}

type callbackAuthStore struct{ save func(*Auth) error }

func (*callbackAuthStore) List(context.Context) ([]*Auth, error) { return nil, nil }
func (*callbackAuthStore) Delete(context.Context, string) error  { return nil }
func (s *callbackAuthStore) Save(_ context.Context, auth *Auth) (string, error) {
	return "", s.save(auth)
}

func TestManagerSaveReadCallbackAndCustomFields(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			ctx := context.Background()
			store := &callbackAuthStore{}
			manager := NewManager(store, nil, nil)
			store.save = func(auth *Auth) error {
				manager.GetByID(auth.ID)
				manager.List()
				auth.Prefix = "custom-prefix"
				auth.ProxyURL = "http://custom.invalid"
				auth.Metadata["custom"] = map[string]any{"value": "saved"}
				delete(auth.Metadata, "remove")
				return nil
			}
			for _, operation := range []string{"Register", "Update"} {
				input := &Auth{
					ID: "callback", Provider: "audit", Status: StatusActive, Metadata: map[string]any{"remove": true},
				}
				var result *Auth
				var err error
				if operation == "Register" {
					result, err = manager.Register(ctx, input)
				} else {
					result, err = manager.Update(ctx, input)
				}
				if err != nil {
					t.Fatal(err)
				}
				got, _ := manager.GetByID(input.ID)
				if got.Prefix != "custom-prefix" || got.ProxyURL != "http://custom.invalid" ||
					got.Metadata["custom"] == nil {
					t.Fatalf("custom store fields missing: %+v", got)
				}
				if _, ok := got.Metadata["remove"]; ok {
					t.Fatal("store deletion lost")
				}
				if !reflect.DeepEqual(result, got) {
					t.Fatal("return differs from manager")
				}
			}
		},
	)
}

func TestManagerSavedFieldsSkipPersistence(t *testing.T) {
	ctx := context.Background()
	store := &callbackAuthStore{save: func(*Auth) error { t.Fatal("unexpected Save"); return nil }}
	for _, mode := range []string{"context", "nil-metadata", "runtime-only", "config-api-key", "plugin-virtual"} {
		t.Run(
			mode, func(t *testing.T) {
				manager := NewManager(store, nil, nil)
				input := &Auth{
					ID: mode, Provider: "audit", Status: StatusActive, Metadata: map[string]any{"type": "audit"},
				}
				saveCtx := ctx
				switch mode {
				case "context":
					saveCtx = WithSkipPersist(ctx)
				case "nil-metadata":
					input.Metadata = nil
				case "runtime-only":
					input.Attributes = map[string]string{"runtime_only": "true"}
				case "config-api-key":
					input.Attributes = map[string]string{"source": "config", "api_key": "test"}
				case "plugin-virtual":
					MarkPluginVirtualAuth(input, "fixture", 0)
				}
				for _, operation := range []string{"Register", "Update"} {
					var result *Auth
					var err error
					if operation == "Register" {
						result, err = manager.Register(saveCtx, input)
					} else {
						result, err = manager.Update(saveCtx, input)
					}
					if err != nil {
						t.Fatal(err)
					}
					got, _ := manager.GetByID(input.ID)
					if !reflect.DeepEqual(got, result) {
						t.Fatal("skip return differs from manager")
					}
				}
			},
		)
	}
}
