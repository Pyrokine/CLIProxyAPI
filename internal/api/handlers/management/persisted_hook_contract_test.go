package management

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestSavedTokenHookUsesPersistedIdentityMetadataKindAndStatus(t *testing.T) {
	for _, withManager := range []bool{false, true} {
		name := "without-manager"
		if withManager {
			name = "with-manager"
		}
		t.Run(
			name, func(t *testing.T) {
				dir := t.TempDir()
				store := sdkauth.NewFileTokenStore()
				var manager *coreauth.Manager
				if withManager {
					manager = coreauth.NewManager(store, nil, nil)
				}
				h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager)
				h.tokenStore = store
				t.Cleanup(func() { _ = h.Close() })
				var hookRecord *coreauth.Auth
				h.SetPostAuthPersistHook(
					func(_ context.Context, record *coreauth.Auth) error {
						hookRecord = record.Clone()
						return nil
					},
				)
				record := &coreauth.Auth{
					ID: "input-identity", FileName: "canonical-codex.json", Provider: "codex", Metadata: map[string]any{
						"type": "codex", "auth_kind": "oauth", "access_token": "synthetic-only-token",
						"websockets": true, "note": "preserved-note", "request_retry": 3,
					},
				}
				var saved *coreauth.Auth
				path, err := h.saveTokenRecord(t.Context(), dir, record, &saved)
				if err != nil {
					t.Fatal(err)
				}
				wantPath := filepath.Join(dir, "canonical-codex.json")
				if path != wantPath || hookRecord == nil || saved == nil {
					t.Fatalf("saved path=%q hook=%v result=%v", path, hookRecord, saved)
				}
				for _, actual := range []*coreauth.Auth{hookRecord, saved} {
					if actual.ID != "canonical-codex.json" || actual.FileName != "canonical-codex.json" ||
						actual.Attributes["path"] != wantPath ||
						actual.AuthKind() != coreauth.AuthKindOAuth ||
						actual.Status != coreauth.StatusActive ||
						actual.Metadata["note"] != "preserved-note" ||
						actual.Attributes["websockets"] != "true" ||
						actual.Metadata["auth_kind"] != "oauth" {
						t.Fatalf("persisted hook contract changed: %+v", actual)
					}
				}
				if withManager {
					published, ok := manager.GetByID("canonical-codex.json")
					if !ok || published.FileName != hookRecord.FileName ||
						published.AuthKind() != hookRecord.AuthKind() ||
						published.Status != hookRecord.Status {
						t.Fatalf("published and hook records differ: published=%v hook=%v", published, hookRecord)
					}
				}
			},
		)
	}
}
