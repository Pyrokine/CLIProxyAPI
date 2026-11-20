package claude_test

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	sdkauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestAuditLegacyClaudeMatcherBindsSameReadPersistedVersion(t *testing.T) {
	root := t.TempDir()
	legacyPath := filepath.Join(root, claude.CredentialFileName("synthetic@example.test", "", ""))
	raw := []byte(`{"type":"claude","email":"synthetic@example.test","organization_uuid":` +
		`"synthetic-org","account_uuid":"synthetic-account","refreshToken":"syn` +
		`thetic-refresh","legacyCounter":9007199254740993}`)
	if err := os.WriteFile(legacyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	name := claude.CredentialFileName("synthetic@example.test", "synthetic-org", "synthetic-account")
	target := &coreauth.Auth{
		ID: name, FileName: name, Provider: "claude", Metadata: map[string]any{
			"email": "synthetic@example.test", "organization_uuid": "synthetic-org", "account_uuid": "synthetic-" +
				"account",
		},
	}
	matched, err := claude.FindMatchingLegacyCredential(t.Context(), root, target)
	if err != nil || matched == nil {
		t.Fatalf("legacy match=%#v error=%v", matched, err)
	}
	digest, err := coreauth.DigestPersistedJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if matched.Attributes[coreauth.PersistedSHA256Attribute] != hex.EncodeToString(digest[:]) ||
		matched.ID != legacyPath ||
		matched.Attributes[coreauth.AttributePath] != legacyPath {
		t.Fatalf("same-read version was not attached: id=%q attributes=%#v", matched.ID, matched.Attributes)
	}
	large, ok := matched.Metadata["legacyCounter"].(json.Number)
	if !ok || large.String() != "9007199254740993" {
		t.Fatalf("legacy number lost precision: %#v", matched.Metadata["legacyCounter"])
	}
	version := coreauth.PersistedVersion{Path: matched.ID, SHA256: digest}
	replacement := []byte(
		`{"type":"claude","organization_uuid":"synthetic-other-org","access_token":"synthetic-replacement"}`,
	)
	if err := os.WriteFile(legacyPath, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	store := sdkauth.NewFileTokenStore()
	if removed, err := store.DeleteIfVersion(t.Context(), matched.ID, version); err != nil || removed {
		t.Fatalf("same-read guard removed a replaced file: removed=%t error=%v", removed, err)
	}
	if current, err := os.ReadFile(legacyPath); err != nil || string(current) != string(replacement) {
		t.Fatalf("replacement changed: %s error=%v", current, err)
	}
	if err := os.WriteFile(legacyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := store.DeleteIfVersion(t.Context(), matched.ID, version); err != nil || !removed {
		t.Fatalf("matching original version retained: removed=%t error=%v", removed, err)
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("matching legacy file remains: %v", err)
	}
}
