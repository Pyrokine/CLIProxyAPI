package auth

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
)

type claudeMigrationVersionStore struct {
	*FileTokenStore
	legacyPath                  string
	replacement                 []byte
	deletes, conditionalDeletes int
	deletedVersion              coreauth.PersistedVersion
}

func (store *claudeMigrationVersionStore) SaveWithBaseDir(
	ctx context.Context,
	base string,
	record *coreauth.Auth,
) (string, error) {
	path, err := store.FileTokenStore.SaveWithBaseDir(ctx, base, record)
	if err == nil && len(store.replacement) != 0 {
		err = os.WriteFile(store.legacyPath, store.replacement, 0o600)
	}
	return path, err
}

func (store *claudeMigrationVersionStore) Delete(ctx context.Context, id string) error {
	store.deletes++
	return store.FileTokenStore.Delete(ctx, id)
}

func (store *claudeMigrationVersionStore) DeleteIfVersion(
	ctx context.Context,
	id string,
	version coreauth.PersistedVersion,
) (bool, error) {
	store.conditionalDeletes++
	store.deletedVersion = version
	return store.FileTokenStore.DeleteIfVersion(ctx, id, version)
}

func TestSDKLoginPreservesExistingCredentialExactNumbers(t *testing.T) {
	root := t.TempDir()
	const fileName = "synthetic-existing.json"
	const original = `{"type":"synthetic","access_token":"synthetic-old","opaque_integer":90` +
		`07199254740993,"opaque_decimal":0.1000000000000000000000000000001}`
	if err := os.WriteFile(filepath.Join(root, fileName), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	login := auditIdentityAuthenticator{
		provider: "synthetic", build: func() (*coreauth.Auth, error) {
			return &coreauth.Auth{
				ID: fileName, FileName: fileName, Provider: "synthetic", Metadata: map[string]any{
					"type": "synthetic", "access_token": "synthetic-new",
				},
			}, nil
		},
	}
	_, _, err := NewManager(NewFileTokenStore(), login).Login(
		t.Context(), "synthetic", &config.Config{AuthDir: root}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(root, fileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"opaque_integer":9007199254740993`, `"opaque_decimal":0.1000000000000000000000000000001`,
		`"access_token":"synthetic-new"`,
	} {
		if !strings.Contains(string(current), field) {
			t.Errorf("saved credential lost exact field %s", field)
		}
	}
}

func TestClaudeLoginMigrationPreservesUnversionedStoreDeleteContract(t *testing.T) {
	root := t.TempDir()
	legacyPath := filepath.Join(root, claudeauth.CredentialFileName("synthetic@example.test", "", ""))
	if err := os.WriteFile(
		legacyPath,
		[]byte((`{"type":"claude","email":"synthetic@example.test","account_uuid":"synthetic-account","` +
			`organization_uuid":"synthetic-org","priority":7}`)),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	fileStore := NewFileTokenStore()
	fileStore.SetBaseDir(root)
	observed := &auditIdentityStore{FileTokenStore: fileStore}
	store := struct{ coreauth.Store }{Store: observed}
	var warnings bytes.Buffer
	previousOutput := log.StandardLogger().Out
	log.SetOutput(&warnings)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	canonical := claudeauth.CredentialFileName("synthetic@example.test", "synthetic-org", "synthetic-account")
	login := auditIdentityAuthenticator{
		provider: "claude", build: func() (*coreauth.Auth, error) {
			return &coreauth.Auth{
				ID: canonical, FileName: canonical, Provider: "claude", Metadata: map[string]any{
					"type": "claude", "email": "synthetic@example.test", "account_uuid": "synthetic-account",
					"organization_uuid": "synthetic-org",
				},
			}, nil
		},
	}
	_, saved, err := NewManager(store, login).Login(t.Context(), "claude", &config.Config{AuthDir: root}, nil)
	if err != nil || filepath.Base(saved) != canonical || observed.deletes != 1 {
		t.Fatalf("legacy Store migration path=%q deletes=%d error=%v", saved, observed.deletes, err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy Store did not perform its Delete contract: %v", err)
	}
	if !strings.Contains(warnings.String(), "auth store has no conditional deletion") {
		t.Fatal("unversioned cleanup was not visible")
	}
}

func TestClaudeLoginMigrationDeletesOnlyTheMatchedLegacyVersion(t *testing.T) {
	for _, relative := range []bool{false, true} {
		for _, replaced := range []bool{false, true} {
			name := map[bool]string{false: "absolute", true: "dot"}[relative] + map[bool]string{
				false: "/same_version", true: "/replaced_version",
			}[replaced]
			t.Run(
				name, func(t *testing.T) {
					root := t.TempDir()
					authDir := root
					if relative {
						t.Chdir(root)
						authDir = "."
					}
					legacyPath := filepath.Join(root, claudeauth.CredentialFileName("synthetic@example.test", "", ""))
					original := []byte((`{"type":"claude","email":"synthetic@example.test","account_uuid":"` +
						`synthetic-account","organization_uuid":"synthetic-org","access_token":"synthetic-` +
						`original","priority":7,"legacyCounter":9007199254740993}`))
					if err := os.WriteFile(legacyPath, original, 0o600); err != nil {
						t.Fatal(err)
					}
					store := &claudeMigrationVersionStore{FileTokenStore: NewFileTokenStore(), legacyPath: legacyPath}
					if replaced {
						store.replacement = []byte(`{"type":"claude","access_token":"synthetic-external-replacement"}`)
					}
					canonical := claudeauth.CredentialFileName(
						"synthetic@example.test", "synthetic-org", "synthetic-account",
					)
					login := auditIdentityAuthenticator{
						provider: "claude", build: func() (*coreauth.Auth, error) {
							return &coreauth.Auth{
								ID: canonical, FileName: canonical, Provider: "claude", Metadata: map[string]any{
									"type": "claude", "email": "synthetic@example.test",
									"account_uuid": "synthetic-account", "organization_uuid": "synthetic-org",
									"access_token": "synthetic-new",
								},
							}, nil
						},
					}
					_, saved, err := NewManager(store, login).Login(
						t.Context(), "claude", &config.Config{AuthDir: authDir}, nil,
					)
					if filepath.Base(saved) != canonical {
						t.Fatalf("canonical credential was not saved: path=%q error=%v", saved, err)
					}
					if replaced {
						if err == nil || !strings.Contains(err.Error(), "cleanup failed") {
							t.Errorf("concurrent replacement did not report cleanup conflict: %v", err)
						}
						if current, readErr := os.ReadFile(legacyPath); readErr != nil ||
							string(current) != string(store.replacement) {
							t.Errorf("external replacement was removed or changed: read error=%v", readErr)
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						if _, statErr := os.Stat(legacyPath); !errors.Is(statErr, os.ErrNotExist) {
							t.Errorf("matched legacy version remains: %v", statErr)
						}
					}
					if store.deletes != 0 || store.conditionalDeletes != 1 {
						t.Errorf(
							"cleanup calls: unconditional=%d conditional=%d", store.deletes, store.conditionalDeletes,
						)
					}
					digest, digestErr := coreauth.DigestPersistedJSON(original)
					if digestErr != nil || store.deletedVersion.Path != legacyPath ||
						store.deletedVersion.SHA256 != digest {
						t.Errorf("cleanup did not consume the original matched-file version: %v", digestErr)
					}
					current, readErr := os.ReadFile(filepath.Join(root, canonical))
					if readErr != nil || !strings.Contains(
						string(current), `"access_token":"synthetic-new"`,
					) || !strings.Contains(
						string(current), `"legacyCounter":9007199254740993`,
					) || !strings.Contains(string(current), `"priority":7`) {
						t.Errorf("canonical credential or preserved metadata changed: read error=%v", readErr)
					}
				},
			)
		}
	}
}
