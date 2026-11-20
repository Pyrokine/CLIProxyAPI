package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	metaauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/meta"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

type auditIdentityAuthenticator struct {
	provider string
	build    func() (*coreauth.Auth, error)
}

func (a auditIdentityAuthenticator) Provider() string          { return a.provider }
func (auditIdentityAuthenticator) RefreshLead() *time.Duration { return nil }

func (a auditIdentityAuthenticator) Login(
	context.Context,
	*config.Config,
	*LoginOptions,
) (*coreauth.Auth, error) {
	return a.build()
}

type auditIdentityStore struct {
	*FileTokenStore
	saves, deletes int
	names          []string
}

func (s *auditIdentityStore) SaveWithBaseDir(
	ctx context.Context,
	base string,
	record *coreauth.Auth,
) (string, error) {
	s.saves++
	s.names = append(s.names, record.FileName)
	return s.FileTokenStore.SaveWithBaseDir(ctx, base, record)
}
func (s *auditIdentityStore) Delete(ctx context.Context, id string) error {
	s.deletes++
	return s.FileTokenStore.Delete(ctx, id)
}

func TestAuditMetaLoginReusesExistingIdentityBeforeSaving(t *testing.T) {
	for _, identity := range []string{"email", "case-insensitive-email", "dca-without-email"} {
		t.Run(
			identity, func(t *testing.T) {
				root := t.TempDir()
				name := "meta-synthetic_example.test-37733ca908ecc9bf.json"
				email, oldEmail := "synthetic@example.test", "synthetic@example.test"
				if identity == "case-insensitive-email" {
					email = "SYNTHETIC@example.test"
				}
				if identity == "dca-without-email" {
					email, oldEmail = "", ""
				}
				legacy := map[string]any{
					"type": "meta", "auth_kind": "oauth", "email": oldEmail, "api_key": "synthetic-old-key",
					"access_token": "synthetic-old-key", "dca_token": "dca:synthetic-device", "priority": 7,
					"disabled": true, "note": "keep",
				}
				raw, err := json.Marshal(legacy)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(root, name), raw, 0o600); err != nil {
					t.Fatal(err)
				}
				build := func() (*coreauth.Auth, error) {
					return MetaRecordFromBundle(
						metaauth.NewMetaAuth(nil), &metaauth.MetaAuthBundle{
							TokenData: &metaauth.TokenData{
								AccessToken: "dca:synthetic-device", ExpiresAt: time.Now().Add(time.Hour).Unix(),
							}, MintedKey: &metaauth.MintedKeyResponse{APIKey: "synthetic-new-key", UserEmail: email},
						},
					)
				}
				store := &auditIdentityStore{FileTokenStore: NewFileTokenStore()}
				manager := NewManager(store, auditIdentityAuthenticator{provider: "meta", build: build})
				record, saved, err := manager.Login(
					t.Context(), "meta", &config.Config{AuthDir: root}, &LoginOptions{NoBrowser: true},
				)
				if err != nil {
					t.Fatal(err)
				}
				if record.ID != name || record.FileName != name || saved != filepath.Join(
					root, name,
				) || store.saves != 1 || store.deletes != 0 || len(store.names) != 1 || store.names[0] != name {
					t.Fatalf(
						"identity selected after saving: record=%#v saved=%q store=%+v",
						record,
						saved,
						store,
					)
				}
				entries, err := os.ReadDir(root)
				if err != nil || len(entries) != 1 {
					t.Fatalf("credential files=%d error=%v", len(entries), err)
				}
				current, err := os.ReadFile(saved)
				if err != nil {
					t.Fatal(err)
				}
				var persisted map[string]any
				if err := json.Unmarshal(current, &persisted); err != nil {
					t.Fatal(err)
				}
				if persisted["priority"] != float64(7) || persisted["disabled"] != true ||
					persisted["note"] != "keep" ||
					persisted["access_token"] != "synthetic-new-key" ||
					persisted["dca_token"] != "dca:synthetic-device" {
					t.Fatalf("old settings or new tokens lost: %#v", persisted)
				}
			},
		)
	}
}

func TestAuditMetaLoginRejectsAmbiguousExistingIdentityBeforeSaving(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"meta-old-a.json", "meta-old-b.json"} {
		if err := os.WriteFile(
			filepath.Join(root, name),
			[]byte(`{"type":"meta","auth_kind":"oauth","email":"synthetic@example.test","priority":7}`), 0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	store := &auditIdentityStore{FileTokenStore: NewFileTokenStore()}
	build := func() (*coreauth.Auth, error) {
		return MetaRecordFromBundle(
			metaauth.NewMetaAuth(nil), &metaauth.MetaAuthBundle{
				TokenData: &metaauth.TokenData{AccessToken: "dca:synthetic-device"},
				MintedKey: &metaauth.MintedKeyResponse{
					APIKey:    "synthetic-key",
					UserEmail: "synthetic@example.test",
				},
			},
		)
	}
	_, _, err := NewManager(store, auditIdentityAuthenticator{provider: "meta", build: build}).Login(
		t.Context(), "meta", &config.Config{AuthDir: root}, nil,
	)
	if err == nil || store.saves != 0 || store.deletes != 0 {
		t.Fatalf("ambiguous identity persisted: error=%v store=%+v", err, store)
	}
}

func TestAuditClaudeRelativeAuthDirMigrationCleansLegacyIdentity(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "absolute", true: "dot"}[relative], func(t *testing.T) {
				root := t.TempDir()
				authDir := root
				if relative {
					t.Chdir(root)
					authDir = "."
				}
				legacyName := claudeauth.CredentialFileName("synthetic@example.test", "", "")
				if err := os.WriteFile(
					filepath.Join(root, legacyName),
					[]byte((`{"type":"claude","email":"synthetic@example.test","account_uuid":"synthetic-` +
						`account","organization_uuid":"synthetic-org","access_token":"synthetic-old","` +
						`priority":7}`)),
					0o600,
				); err != nil {
					t.Fatal(err)
				}
				name := claudeauth.CredentialFileName(
					"synthetic@example.test",
					"synthetic-org",
					"synthetic-account",
				)
				build := func() (*coreauth.Auth, error) {
					return &coreauth.Auth{
						ID: name, FileName: name, Provider: "claude", Metadata: map[string]any{
							"type": "claude", "email": "synthetic@example.test", "account_uuid": "synthetic-account",
							"organization_uuid": "synthetic-org", "access_token": "synthetic-new",
						},
					}, nil
				}
				_, saved, err := NewManager(
					NewFileTokenStore(), auditIdentityAuthenticator{provider: "claude", build: build},
				).Login(t.Context(), "claude", &config.Config{AuthDir: authDir}, nil)
				if err != nil || filepath.Base(saved) != name {
					t.Fatalf("migration result saved=%q error=%v", saved, err)
				}
				if _, err := os.Stat(filepath.Join(root, legacyName)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("legacy file remains: %v", err)
				}
				data, err := os.ReadFile(filepath.Join(root, name))
				if err != nil || !strings.Contains(string(data), `"priority":7`) || !strings.Contains(
					string(data), `"access_token":"synthetic-new"`,
				) {
					t.Fatalf("migrated metadata=%s error=%v", data, err)
				}
			},
		)
	}
}

func TestAuditClaudeLoginKeepsSameEmailOrganizationsSeparate(t *testing.T) {
	root := t.TempDir()
	store := NewFileTokenStore()
	for _, org := range []string{"synthetic-org-a", "synthetic-org-b", "synthetic-org-a"} {
		name := claudeauth.CredentialFileName("synthetic@example.test", org, "synthetic-account")
		build := func() (*coreauth.Auth, error) {
			return &coreauth.Auth{
				ID: name, FileName: name, Provider: "claude", Metadata: map[string]any{
					"type": "claude", "email": "synthetic@example.test", "account_uuid": "synthetic-account",
					"organization_uuid": org, "access_token": "synthetic-" + org,
				},
			}, nil
		}
		if _, _, err := NewManager(
			store, auditIdentityAuthenticator{provider: "claude", build: build},
		).Login(t.Context(), "claude", &config.Config{AuthDir: root}, nil); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("organization files=%d error=%v", len(entries), err)
	}
	for _, org := range []string{"synthetic-org-a", "synthetic-org-b"} {
		name := claudeauth.CredentialFileName("synthetic@example.test", org, "synthetic-account")
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || !strings.Contains(string(data), org) {
			t.Fatalf("organization overwritten: %s error=%v", data, err)
		}
	}
}

func TestAuditMetaLoginKeepsExplicitAPIKeyIdentitySeparate(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "meta-manual.json")
	original := []byte((`{"type":"meta","auth_kind":"api_key","email":"synthetic@example.test",` +
		`"access_token":"synthetic-manual-key","priority":7}`))
	if err := os.WriteFile(oldPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	build := func() (*coreauth.Auth, error) {
		return MetaRecordFromBundle(
			metaauth.NewMetaAuth(nil), &metaauth.MetaAuthBundle{
				TokenData: &metaauth.TokenData{AccessToken: "dca:synthetic-device"},
				MintedKey: &metaauth.MintedKeyResponse{
					APIKey: "synthetic-oauth-key", UserEmail: "synthetic@example.test",
				},
			},
		)
	}
	record, saved, err := NewManager(
		NewFileTokenStore(), auditIdentityAuthenticator{provider: "meta", build: build},
	).Login(t.Context(), "meta", &config.Config{AuthDir: root}, nil)
	if err != nil || saved == oldPath || record.Metadata["priority"] != nil {
		t.Fatalf("OAuth reused an explicit API-key identity: saved=%q record=%#v error=%v", saved, record, err)
	}
	data, err := os.ReadFile(oldPath)
	if err != nil || string(data) != string(original) {
		t.Fatalf("explicit API-key credential changed: %s error=%v", data, err)
	}
}

func TestAuditMetaLoginRejectsSymlinkAuthDirWithoutCreatingCredential(t *testing.T) {
	root := t.TempDir()
	authDir := filepath.Join(t.TempDir(), "auth-link")
	if err := os.Symlink(root, authDir); err != nil {
		t.Skipf("symbolic-link directory unavailable: %v", err)
	}
	name := "meta-synthetic_example.test-37733ca908ecc9bf.json"
	original := []byte(
		(`{"type":"meta","auth_kind":"oauth","email":"synthetic@example.test",` +
			`"priority":7,"access_token":` +
			`"synthetic-old"}`),
	)
	if err := os.WriteFile(filepath.Join(root, name), original, 0o600); err != nil {
		t.Fatal(err)
	}
	build := func() (*coreauth.Auth, error) {
		return MetaRecordFromBundle(
			metaauth.NewMetaAuth(nil), &metaauth.MetaAuthBundle{
				TokenData: &metaauth.TokenData{AccessToken: "dca:synthetic-device"},
				MintedKey: &metaauth.MintedKeyResponse{
					APIKey:    "synthetic-new",
					UserEmail: "synthetic@example.test",
				},
			},
		)
	}
	_, saved, err := NewManager(
		NewFileTokenStore(), auditIdentityAuthenticator{provider: "meta", build: build},
	).Login(t.Context(), "meta", &config.Config{AuthDir: authDir}, nil)
	if err == nil || saved != "" {
		t.Fatalf("secure AuthDir rejection lost: saved=%q error=%v", saved, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Fatalf("credential files=%d error=%v", len(entries), err)
	}
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || string(data) != string(original) {
		t.Fatalf("rejected save changed the old credential: %s error=%v", data, err)
	}
}
