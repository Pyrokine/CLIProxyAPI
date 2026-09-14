package management

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestSaveUploadedAuthSerializesCaseVariantPaths(t *testing.T) {
	authDir := t.TempDir()
	lowerPath := filepath.Join(authDir, "shared.json")
	upperPath := filepath.Join(authDir, "SHARED.json")
	lowerData := []byte(`{"type":"codex","token":"lower"}`)
	upperData := []byte(`{"type":"codex","token":"upper"}`)

	lowerAuth, _, errLower := authFromFile(authDir, lowerPath, lowerData)
	if errLower != nil {
		t.Fatal(errLower)
	}
	upperAuth, _, errUpper := authFromFile(authDir, upperPath, upperData)
	if errUpper != nil {
		t.Fatal(errUpper)
	}
	if lowerAuth.ID != upperAuth.ID {
		t.Fatalf("case-variant auth IDs = %q and %q, want one mutation identity", lowerAuth.ID, upperAuth.ID)
	}
	lowerDigest := lowerAuth.Attributes[coreauth.PersistedSHA256Attribute]
	upperDigest := upperAuth.Attributes[coreauth.PersistedSHA256Attribute]
	if len(lowerDigest) != 64 || len(upperDigest) != 64 || lowerDigest == upperDigest {
		t.Fatalf("persisted digests = %q and %q", lowerDigest, upperDigest)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	start := make(chan struct{})
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for _, upload := range []struct {
		path string
		data []byte
	}{
		{path: lowerPath, data: lowerData},
		{path: upperPath, data: upperData},
	} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			errors <- handler.saveUploadedAuth(context.Background(), authDir, upload.path, upload.data)
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	for errSave := range errors {
		if errSave != nil {
			t.Fatalf("saveUploadedAuth() error = %v", errSave)
		}
	}

	persisted, errRead := os.ReadFile(lowerPath)
	if errRead != nil {
		t.Fatal(errRead)
	}
	var persistedMetadata map[string]any
	if errUnmarshal := json.Unmarshal(persisted, &persistedMetadata); errUnmarshal != nil {
		t.Fatal(errUnmarshal)
	}
	current, ok := manager.GetByID(lowerAuth.ID)
	if !ok || current == nil {
		t.Fatal("serialized auth is missing from manager")
	}
	if current.Metadata["token"] != persistedMetadata["token"] {
		t.Fatalf("runtime token = %v, persisted token = %v", current.Metadata["token"], persistedMetadata["token"])
	}
	if auths := manager.List(); len(auths) != 1 {
		t.Fatalf("manager auth count = %d, want 1", len(auths))
	}
}
