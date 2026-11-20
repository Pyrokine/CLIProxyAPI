package auth

import (
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestManagerAuthDirContract(t *testing.T) {
	for _, input := range []string{"~/tokens", "tokens"} {
		t.Run(
			input, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("HOME", root)
				t.Chdir(root)
				dir := filepath.Join(root, "tokens")
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				const name = "synthetic.json"
				writeManagerCredential(
					t, filepath.Join(dir, name), map[string]any{
						"type": "codex", "access_token": "synthetic-old", "note": "preserve", "disabled": true,
					},
				)
				record := &coreauth.Auth{
					ID: name, FileName: name, Provider: "codex",
					Metadata: map[string]any{"type": "codex", "access_token": "synthetic-new"},
				}
				cfg := &config.Config{AuthDir: input}
				store := NewFileTokenStore()
				store.SetBaseDir(t.TempDir())
				manager := NewManager(store, recordManagerAuthenticator{provider: "synthetic", record: record})
				_, saved, err := manager.Login(t.Context(), "synthetic", cfg, nil)
				if err != nil || saved != filepath.Join(dir, name) {
					t.Fatalf("saved path = %q, error = %v", saved, err)
				}
				metadata := readManagerCredential(t, saved)
				if metadata["access_token"] != "synthetic-new" || metadata["note"] != "preserve" ||
					metadata["disabled"] != true {
					t.Fatalf("preserved metadata = %#v", metadata)
				}
				if cfg.AuthDir != input {
					t.Fatalf("operation changed shared config: %q", cfg.AuthDir)
				}
				if auths, err := store.List(t.Context()); err != nil || len(auths) != 0 {
					t.Fatalf("operation changed shared store directory: %#v, error = %v", auths, err)
				}
			},
		)
	}
}

func TestManagerAuthDirResolutionFailureAndOptionalPersistence(t *testing.T) {
	t.Setenv("HOME", "")
	record := &coreauth.Auth{ID: "synthetic.json", Provider: "codex"}
	authenticator := recordManagerAuthenticator{provider: "synthetic", record: record}
	store := &recordingManagerStore{}
	manager := NewManager(store, authenticator)
	if _, saved, err := manager.Login(
		t.Context(), "synthetic", &config.Config{AuthDir: "~/tokens"}, nil,
	); err == nil || saved != "" || store.saves != 0 {
		t.Fatalf("unresolvable directory: saved = %q, error = %v, saves = %d", saved, err, store.saves)
	}
	for _, cfg := range []*config.Config{nil, {}} {
		if _, _, err := manager.Login(t.Context(), "synthetic", cfg, nil); err != nil {
			t.Fatalf("legacy optional directory rejected: %v", err)
		}
	}
	manager.SetStore(nil)
	if got, saved, err := manager.Login(
		t.Context(), "synthetic", &config.Config{AuthDir: "~/tokens"}, nil,
	); err != nil || got != record || saved != "" {
		t.Fatalf(
			"non-persisting login gained a directory prerequisite: record = %v, saved = %q, error = %v",
			got,
			saved,
			err,
		)
	}
}
