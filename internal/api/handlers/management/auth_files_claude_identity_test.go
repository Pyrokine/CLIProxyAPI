package management

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestClaudeManagementSavePreservesDifferentOrganizationsForSameEmail(t *testing.T) {
	previousSessions := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previousSessions })
	const email, account = "synthetic-user@example.invalid", "synthetic-same-account"
	for _, withManager := range []bool{false, true} {
		name := "file_only"
		if withManager {
			name = "runtime_publication"
		}
		t.Run(
			name, func(t *testing.T) {
				dir := t.TempDir()
				store := sdkAuth.NewFileTokenStore()
				store.SetBaseDir(dir)
				h := &Handler{cfg: &config.Config{AuthDir: dir}, tokenStore: store}
				if withManager {
					h.authManager = coreauth.NewManager(store, nil, nil)
				}
				publications := 0
				h.postAuthPersistHook = func(_ context.Context, saved *coreauth.Auth) error {
					if saved.Provider != "claude" || saved.Metadata["email"] != email ||
						saved.Metadata["account_uuid"] != account {
						t.Fatalf("publication lost Claude identity: %+v", saved)
					}
					publications++
					return nil
				}
				for _, entry := range []struct{ organization, token string }{
					{"synthetic-org-a", "synthetic-token-a"},
					{"synthetic-org-b", "synthetic-token-b"},
					{"synthetic-org-a", "synthetic-token-a-updated"},
				} {
					storage := &claude.TokenStorage{
						Email: email, AccountUUID: account, OrganizationUUID: entry.organization,
						OrganizationName: entry.organization, AccessToken: entry.token,
					}
					fileName := claude.CredentialFileName(storage.Email, storage.OrganizationUUID, storage.AccountUUID)
					record := &coreauth.Auth{
						ID: fileName, FileName: fileName, Provider: "claude", Storage: storage,
						Metadata: map[string]any{
							"email": storage.Email, "account_uuid": storage.AccountUUID,
							"organization_uuid": storage.OrganizationUUID,
							"organization_name": storage.OrganizationName,
						},
					}
					state := "synthetic-save-" + entry.token
					registerOAuthSessionForAuthDir(state, "anthropic", "", dir)
					savedPath, err := h.saveOAuthTokenRecord(t.Context(), dir, state, "anthropic", record)
					if err != nil || savedPath != filepath.Join(dir, fileName) {
						t.Fatalf("saved path=%q err=%v", savedPath, err)
					}
					completeOAuthSession(state)
				}
				files, err := os.ReadDir(dir)
				if err != nil || len(files) != 2 || publications != 3 {
					t.Fatalf("credential files=%d publications=%d err=%v", len(files), publications, err)
				}
				if withManager && len(h.authManager.List()) != 2 {
					t.Fatalf(
						"same-email organizations collapsed into %d runtime credentials", len(h.authManager.List()),
					)
				}
				for organization, token := range map[string]string{
					"synthetic-org-a": "synthetic-token-a-updated",
					"synthetic-org-b": "synthetic-token-b",
				} {
					fileName := claude.CredentialFileName(email, organization, account)
					raw, err := os.ReadFile(filepath.Join(dir, fileName))
					if err != nil {
						t.Fatal(err)
					}
					var saved map[string]any
					if err := json.Unmarshal(raw, &saved); err != nil {
						t.Fatal(err)
					}
					if saved["email"] != email || saved["organization_uuid"] != organization ||
						saved["organization_name"] != organization ||
						saved["account_uuid"] != account ||
						saved["access_token"] != token {
						t.Fatalf("persisted organization identity=%v", saved)
					}
					if withManager {
						current, exists := h.authManager.GetByID(fileName)
						if !exists || current.FileName != fileName ||
							current.Metadata["organization_uuid"] != organization ||
							current.Metadata["access_token"] != token {
							t.Fatalf("runtime organization credential=%+v", current)
						}
					}
				}
			},
		)
	}
}
