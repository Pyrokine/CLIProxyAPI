package auth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestFileTokenStoreListLoadsProxyURLAndPrefix(t *testing.T) {
	authDir := t.TempDir()
	credentialPath := filepath.Join(authDir, "claude.json")
	if errWrite := os.WriteFile(
		credentialPath,
		[]byte(`{"type":"claude","proxy-url":" http://proxy.example ","prefix":" /team/ "}`),
		0o600,
	); errWrite != nil {
		t.Fatalf("write credential: %v", errWrite)
	}
	store := NewFileTokenStore()
	store.SetBaseDir(authDir)

	auths, errList := store.List(context.Background())
	if errList != nil {
		t.Fatalf("List() error = %v", errList)
	}
	if len(auths) != 1 {
		t.Fatalf("List() auth count = %d, want 1", len(auths))
	}
	if auths[0].ProxyURL != "http://proxy.example" {
		t.Fatalf("ProxyURL = %q, want http://proxy.example", auths[0].ProxyURL)
	}
	if auths[0].Prefix != "team" {
		t.Fatalf("Prefix = %q, want team", auths[0].Prefix)
	}
	if _, legacy := auths[0].Metadata["proxy-url"]; legacy {
		t.Fatalf("legacy proxy key retained: %#v", auths[0].Metadata)
	}
}

func TestFileTokenStoreNormalizesLegacyCredentialMetadata(t *testing.T) {
	t.Run("save", func(t *testing.T) {
		authDir := t.TempDir()
		store := NewFileTokenStore()
		store.SetBaseDir(authDir)
		auth := &cliproxyauth.Auth{
			ID:       "legacy-save.json",
			FileName: "legacy-save.json",
			Metadata: map[string]any{
				"type":            "codex",
				"request-retry":   2,
				"request_retry":   0,
				"disable-cooling": true,
			},
		}
		path, errSave := store.Save(context.Background(), auth)
		if errSave != nil {
			t.Fatalf("Save() error = %v", errSave)
		}
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			t.Fatalf("read saved credential: %v", errRead)
		}
		var metadata map[string]any
		if errDecode := json.Unmarshal(raw, &metadata); errDecode != nil {
			t.Fatalf("decode saved credential: %v", errDecode)
		}
		if metadata["request_retry"] != float64(0) || metadata["disable_cooling"] != true {
			t.Fatalf("normalized saved metadata = %#v", metadata)
		}
		for _, legacyKey := range []string{"request-retry", "disable-cooling"} {
			if _, exists := metadata[legacyKey]; exists {
				t.Fatalf("saved metadata retained %q: %#v", legacyKey, metadata)
			}
		}
	})

	t.Run("list", func(t *testing.T) {
		authDir := t.TempDir()
		if errWrite := os.WriteFile(
			filepath.Join(authDir, "legacy-list.json"),
			[]byte(`{"type":"codex","request-retry":2,"disable-cooling":true}`),
			0o600,
		); errWrite != nil {
			t.Fatalf("write legacy credential: %v", errWrite)
		}
		store := NewFileTokenStore()
		store.SetBaseDir(authDir)
		auths, errList := store.List(context.Background())
		if errList != nil || len(auths) != 1 {
			t.Fatalf("List() auths/error = %#v/%v", auths, errList)
		}
		if auths[0].Metadata["request_retry"] != float64(2) || auths[0].Metadata["disable_cooling"] != true {
			t.Fatalf("normalized listed metadata = %#v", auths[0].Metadata)
		}
		for _, legacyKey := range []string{"request-retry", "disable-cooling"} {
			if _, exists := auths[0].Metadata[legacyKey]; exists {
				t.Fatalf("listed metadata retained %q: %#v", legacyKey, auths[0].Metadata)
			}
		}
	})
}
