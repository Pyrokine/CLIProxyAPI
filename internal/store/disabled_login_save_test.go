package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRemoteStoresRequireCreationIntentForMissingDisabledCredential(t *testing.T) {
	tests := []struct {
		name  string
		store cliproxyauth.Store
	}{
		{
			name: "postgres",
			store: &PostgresStore{
				authDir: filepath.Join(t.TempDir(), "auths"),
			},
		},
		{
			name: "object",
			store: &ObjectTokenStore{
				authDir: filepath.Join(t.TempDir(), "auths"),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			errReachedStorage := errors.New("reached token storage")
			storageReached := false
			auth := &cliproxyauth.Auth{
				ID:       "canonical-disabled.json",
				FileName: "canonical-disabled.json",
				Provider: "claude",
				Disabled: true,
				Storage: &callbackTokenStorage{save: func(string) error {
					storageReached = true
					return errReachedStorage
				}},
				Metadata: map[string]any{"type": "claude"},
			}

			savedPath, errSave := test.store.Save(context.Background(), auth)
			if errSave != nil {
				t.Fatalf("runtime Save() error = %v", errSave)
			}
			if savedPath != "" {
				t.Fatalf("runtime Save() path = %q, want empty", savedPath)
			}
			if storageReached {
				t.Fatal("runtime Save() reached token storage for a missing disabled credential")
			}

			ctx := cliproxyauth.WithAuthCreationIntent(context.Background())
			_, errSave = test.store.Save(ctx, auth)
			if !errors.Is(errSave, errReachedStorage) {
				t.Fatalf("creation-intent Save() error = %v, want token storage sentinel", errSave)
			}
			if !storageReached {
				t.Fatal("creation-intent Save() did not reach token storage")
			}
		})
	}
}
