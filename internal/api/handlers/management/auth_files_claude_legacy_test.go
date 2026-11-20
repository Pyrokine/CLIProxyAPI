package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type claudeLegacyTestStore struct {
	*sdkAuth.FileTokenStore
	failSave, failDelete bool
}

func (s *claudeLegacyTestStore) SaveWithBaseDir(
	ctx context.Context,
	dir string,
	auth *coreauth.Auth,
) (string, error) {
	if s.failSave {
		return "", errors.New("synthetic canonical save failure")
	}
	return s.FileTokenStore.SaveWithBaseDir(ctx, dir, auth)
}

func (s *claudeLegacyTestStore) DeleteIfVersion(
	ctx context.Context,
	id string,
	version coreauth.PersistedVersion,
) (bool, error) {
	if s.failDelete {
		return false, errors.New("synthetic legacy cleanup failure")
	}
	return s.FileTokenStore.DeleteIfVersion(ctx, id, version)
}

func TestClaudeManagementLegacyMigrationPreservesSaveAndCleanupBoundaries(t *testing.T) {
	previousSessions := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previousSessions })
	const email, account, organization = "synthetic-user@example.invalid", "synthetic-account", "synthetic-org-a"
	for _, test := range []struct {
		name                                                       string
		manager                                                    int
		accountHashed, otherOrganization, failSave, failDelete     bool
		rotateFile, advanceGeneration, disabled, existingCanonical bool
		aliasRuntime                                               bool
		lateAlias, noLegacyRuntime                                 bool
	}{
		{name: "file_only"},
		{name: "runtime", manager: 1},
		{name: "runtime_without_store", manager: 2},
		{name: "runtime_without_store_cleanup_failure_retains_legacy", manager: 2, failDelete: true},
		{name: "runtime_without_store_file_replacement_retained", manager: 2, rotateFile: true},
		{name: "runtime_without_store_generation_replacement_retained", manager: 2, advanceGeneration: true},
		{name: "runtime_alias_success", manager: 1, aliasRuntime: true},
		{name: "runtime_without_store_alias_success", manager: 2, aliasRuntime: true},
		{name: "runtime_alias_cleanup_failure_retained", manager: 1, aliasRuntime: true, failDelete: true},
		{
			name: "runtime_without_store_alias_cleanup_failure_retained", manager: 2, aliasRuntime: true,
			failDelete: true,
		},
		{name: "runtime_alias_file_replacement_retained", manager: 1, aliasRuntime: true, rotateFile: true},
		{
			name: "runtime_without_store_alias_file_replacement_retained", manager: 2, aliasRuntime: true,
			rotateFile: true,
		},
		{
			name: "runtime_alias_generation_replacement_retained", manager: 1, aliasRuntime: true,
			advanceGeneration: true,
		},
		{
			name: "runtime_without_store_alias_generation_replacement_retained", manager: 2, aliasRuntime: true,
			advanceGeneration: true,
		},
		{name: "empty_runtime_success", manager: 1, noLegacyRuntime: true},
		{name: "empty_runtime_without_store_success", manager: 2, noLegacyRuntime: true},
		{name: "late_alias_retained", manager: 1, lateAlias: true},
		{name: "runtime_without_store_late_alias_retained", manager: 2, lateAlias: true},
		{name: "empty_runtime_late_alias_retained", manager: 1, lateAlias: true, noLegacyRuntime: true},
		{name: "empty_runtime_without_store_late_alias_retained", manager: 2, lateAlias: true, noLegacyRuntime: true},
		{name: "account_hashed_predecessor", accountHashed: true},
		{name: "other_organization_retained", otherOrganization: true, manager: 1},
		{name: "save_failure_retains_legacy", failSave: true, manager: 1},
		{name: "cleanup_failure_retains_legacy", failDelete: true, manager: 1},
		{name: "file_replacement_retained", rotateFile: true, manager: 1},
		{name: "runtime_replacement_retained", advanceGeneration: true, manager: 1},
		{name: "disabled_preserved", disabled: true, manager: 1},
		{name: "canonical_settings_take_precedence", existingCanonical: true},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				dir := t.TempDir()
				store := &claudeLegacyTestStore{FileTokenStore: sdkAuth.NewFileTokenStore()}
				store.SetBaseDir(dir)
				legacyName := claude.CredentialFileName(email, "", "")
				legacyOrganization := organization
				if test.accountHashed {
					legacyName = claude.CredentialFileName(email, "", account)
					legacyOrganization = ""
				}
				if test.otherOrganization {
					legacyOrganization = "synthetic-org-b"
				}
				legacyPath := filepath.Join(dir, legacyName)
				legacyRaw, err := json.Marshal(
					map[string]any{
						"type": "claude", "email": email, "account_uuid": account,
						"organization_uuid": legacyOrganization, "access_token": "synthetic-old-token",
						"refreshToken": "synthetic-old-refresh", "priority": 42, "disabled": test.disabled,
						"disable-cooling": true, "opaque_number": json.Number("9007199254740993"),
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(legacyPath, legacyRaw, 0o600); err != nil {
					t.Fatal(err)
				}
				h := &Handler{cfg: &config.Config{AuthDir: dir}, tokenStore: store}
				if test.manager != 0 {
					var runtimeStore coreauth.Store = store
					if test.manager == 2 {
						runtimeStore = nil
					}
					h.authManager = coreauth.NewManager(runtimeStore, nil, nil)
					legacy, _, err := authFromFile(dir, legacyPath, nil)
					if err != nil {
						t.Fatal(err)
					}
					if !test.noLegacyRuntime {
						if _, err := h.authManager.Register(coreauth.WithSkipPersist(t.Context()), legacy); err != nil {
							t.Fatal(err)
						}
					}
					if test.aliasRuntime {
						alias := legacy.Clone()
						alias.ID = legacyPath
						if _, err := h.authManager.Register(coreauth.WithSkipPersist(t.Context()), alias); err != nil {
							t.Fatalf("alias registration rejected: %v", err)
						}
						matched := authsForDeletion(h.authManager.List(), dir, legacyPath)
						if len(matched) != 2 || matched[0].ID != legacyName || matched[1].ID != legacyPath {
							t.Fatalf("unexpected alias ownership: %+v", matched)
						}
						t.Logf("accepted legacy runtime IDs: %q, %q", matched[0].ID, matched[1].ID)
					}
				}
				canonicalName := claude.CredentialFileName(email, organization, account)
				canonicalPath := filepath.Join(dir, canonicalName)
				if test.existingCanonical {
					if err := os.WriteFile(
						canonicalPath, []byte(`{"type":"claude","priority":99}`), 0o600,
					); err != nil {
						t.Fatal(err)
					}
				}
				var replacement []byte
				var replacementGeneration uint64
				var lateAlias *coreauth.Auth
				h.postAuthPersistHook = func(_ context.Context, _ *coreauth.Auth) error {
					if _, err := os.Stat(canonicalPath); err != nil {
						t.Fatalf("canonical must exist before legacy cleanup: %v", err)
					}
					if _, err := os.Stat(legacyPath); err != nil {
						t.Fatalf("legacy removed before publication callback: %v", err)
					}
					if test.lateAlias {
						alias, _, err := authFromFile(dir, legacyPath, nil)
						if err != nil {
							t.Fatal(err)
						}
						alias.ID = legacyPath
						lateAlias, err = h.authManager.Register(coreauth.WithSkipPersist(t.Context()), alias)
						if err != nil || lateAlias == nil {
							t.Fatalf("late alias registration failed: %v", err)
						}
					}
					if test.rotateFile {
						replacement = []byte(
							(`{"type":"claude","organization_uuid":"synthetic-org-b","access_token":` +
								`"synthetic-replacement"}`),
						)
						if err := os.WriteFile(legacyPath, replacement, 0o600); err != nil {
							t.Fatal(err)
						}
					}
					if test.advanceGeneration {
						replacementID := legacyName
						if test.aliasRuntime {
							replacementID = legacyPath
						}
						previous, ok := h.authManager.GetByID(replacementID)
						if !ok {
							t.Fatal("legacy runtime missing before replacement")
						}
						previous.StatusMessage = "synthetic replacement generation"
						updated, err := h.authManager.Update(coreauth.WithSkipPersist(t.Context()), previous)
						if err != nil {
							t.Fatal(err)
						}
						replacementGeneration = updated.Generation
					}
					return nil
				}
				store.failSave, store.failDelete = test.failSave, test.failDelete
				storage := &claude.TokenStorage{
					Email: email, AccountUUID: account, OrganizationUUID: organization,
					AccessToken: "synthetic-new-token",
				}
				record := &coreauth.Auth{
					ID: canonicalName, FileName: canonicalName, Provider: "claude", Storage: storage,
					Metadata: map[string]any{
						"email": email, "account_uuid": account, "organization_uuid": organization,
					},
				}
				state := "synthetic-legacy-" + test.name
				registerOAuthSessionForAuthDir(state, "anthropic", "", dir)
				savedPath, errSave := h.saveOAuthTokenRecord(t.Context(), dir, state, "anthropic", record)
				wantError := test.failSave || test.failDelete || test.rotateFile || test.advanceGeneration ||
					test.lateAlias
				if (errSave != nil) != wantError {
					t.Fatalf("save error=%v, want error=%t", errSave, wantError)
				}
				if test.aliasRuntime {
					_, firstExists := h.authManager.GetByID(legacyName)
					second, secondExists := h.authManager.GetByID(legacyPath)
					t.Logf(
						"cleanup result: saved=%q err=%v firstExists=%t secondExists=%t", savedPath, errSave,
						firstExists, secondExists,
					)
					if firstExists != wantError || secondExists != wantError ||
						(test.advanceGeneration && second.Generation != replacementGeneration) {
						t.Errorf("legacy aliases or replacement generation lost")
					}
				}
				legacyAfter, errLegacy := os.ReadFile(legacyPath)
				keepLegacy := wantError || test.otherOrganization
				if keepLegacy {
					want := legacyRaw
					if test.rotateFile {
						want = replacement
					}
					if errLegacy != nil || !bytes.Equal(legacyAfter, want) {
						if test.aliasRuntime {
							t.Errorf("retained legacy changed: %q err=%v", legacyAfter, errLegacy)
						} else {
							t.Fatalf("retained legacy changed: %q err=%v", legacyAfter, errLegacy)
						}
					}
				} else if !errors.Is(errLegacy, os.ErrNotExist) {
					t.Fatalf("matching legacy was not removed: %v", errLegacy)
				}
				if test.failSave {
					if savedPath != "" {
						t.Fatalf("failed persistence returned saved path %q", savedPath)
					}
					if _, err := os.Stat(canonicalPath); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("failed save published canonical: %v", err)
					}
					return
				}
				if savedPath != canonicalPath {
					t.Fatalf("actual canonical path lost: %q", savedPath)
				}
				raw, err := os.ReadFile(canonicalPath)
				if err != nil {
					t.Fatal(err)
				}
				var canonical map[string]any
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.UseNumber()
				if err := decoder.Decode(&canonical); err != nil {
					t.Fatal(err)
				}
				if canonical["access_token"] != "synthetic-new-token" ||
					canonical["organization_uuid"] != organization {
					t.Fatalf("new credential identity overwritten: %v", canonical)
				}
				if !test.otherOrganization {
					priority := json.Number("42")
					if test.existingCanonical {
						priority = "99"
					}
					if canonical["priority"] != priority || canonical["disabled"] != test.disabled ||
						canonical["opaque_number"] != json.Number("9007199254740993") {
						t.Fatalf("legacy settings or precision lost: %v", canonical)
					}
				}
				if h.authManager != nil {
					legacyRuntime, legacyExists := h.authManager.GetByID(legacyName)
					if legacyExists != (keepLegacy && !test.noLegacyRuntime) {
						t.Fatalf("legacy runtime exists=%t, want %t", legacyExists, keepLegacy)
					}
					if test.advanceGeneration && !test.aliasRuntime &&
						legacyRuntime.Generation != replacementGeneration {
						t.Fatalf("replacement generation changed: %+v", legacyRuntime)
					}
					if test.lateAlias {
						current, ok := h.authManager.GetByID(lateAlias.ID)
						if !ok || current.Generation != lateAlias.Generation ||
							current.Metadata["access_token"] != "synthetic-old-token" {
							t.Fatalf("late alias changed on failed cleanup: %+v", current)
						}
					}
					if current, ok := h.authManager.GetByID(canonicalName); !ok ||
						current.Disabled != test.disabled {
						t.Fatalf("canonical runtime missing or disabled state changed: %+v", current)
					}
				}
			},
		)
	}
}
