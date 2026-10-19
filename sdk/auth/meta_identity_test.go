package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	metaauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/meta"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestMetaCredentialIdentityUsesNormalizedEmail(t *testing.T) {
	original := metaauth.CredentialFileName(" User@Example.Invalid ", "dca:original-fixture")
	if got := metaauth.CredentialFileName("user@example.invalid", "dca:replacement-fixture"); got != original {
		t.Fatal("DCA rotation changed the identity of the same Meta account")
	}
	if got := metaauth.CredentialFileName("other@example.invalid", "dca:original-fixture"); got == original {
		t.Fatal("different Meta accounts share a credential identity")
	}
	withoutEmail := metaauth.CredentialFileName("", " dca:original-fixture ")
	if got := metaauth.CredentialFileName(" ", "dca:original-fixture"); got != withoutEmail {
		t.Fatal("blank email did not retain the normalized DCA identity")
	}
	if got := metaauth.CredentialFileName("", "dca:other-fixture"); got == withoutEmail {
		t.Fatal("distinct credentials without email share an identity")
	}
}

func TestMetaReauthenticationPreservesStoredSettings(t *testing.T) {
	dir := t.TempDir()
	authSvc := metaauth.NewMetaAuth(nil)
	first, err := MetaRecordFromBundle(authSvc, &metaauth.MetaAuthBundle{
		TokenData: &metaauth.TokenData{AccessToken: "dca:original-fixture"},
		MintedKey: &metaauth.MintedKeyResponse{APIKey: "original-api-key-fixture", UserEmail: "user@example.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, first.FileName)
	writeManagerCredential(t, path, map[string]any{
		"type": "meta", "auth_kind": "oauth", "email": "user@example.invalid",
		"access_token": "original-api-key-fixture", "dca_token": "dca:original-fixture",
		"api_key": "original-api-key-fixture", "disabled": true,
		"weight": 4, "proxy_url": "http://proxy.example.invalid", "prefix": "team",
		"headers": map[string]any{"X-Team": "blue"}, "note": "keep",
	})
	second, err := MetaRecordFromBundle(authSvc, &metaauth.MetaAuthBundle{
		TokenData: &metaauth.TokenData{AccessToken: "dca:replacement-fixture"},
		MintedKey: &metaauth.MintedKeyResponse{APIKey: "replacement-api-key-fixture", UserEmail: " User@Example.Invalid "},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(NewFileTokenStore(), recordManagerAuthenticator{provider: "meta", record: second})
	record, savedPath, err := manager.Login(context.Background(), "meta", &config.Config{AuthDir: dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != first.ID || record.FileName != first.FileName || savedPath != path {
		t.Fatal("Meta reauthentication created a different credential file")
	}
	saved := readManagerCredential(t, savedPath)
	if saved["access_token"] != "replacement-api-key-fixture" || saved["api_key"] != "replacement-api-key-fixture" ||
		saved["dca_token"] != "dca:replacement-fixture" || saved["note"] != "keep" || saved["disabled"] != true {
		t.Fatal("Meta reauthentication lost settings or retained old token material")
	}
	if !record.Disabled || record.ProxyURL != "http://proxy.example.invalid" || record.Prefix != "team" ||
		record.Attributes[coreauth.AttributeWeight] != "4" || record.Attributes["header:X-Team"] != "blue" {
		t.Fatal("Meta reauthentication lost runtime credential settings")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("credential count = %d, error = %v", len(entries), err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential permissions = %v, error = %v", info, err)
	}
}
