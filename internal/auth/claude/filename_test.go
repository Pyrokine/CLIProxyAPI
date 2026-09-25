package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestCredentialFileNameSeparatesOrganizations(t *testing.T) {
	first := CredentialFileName(" user@example.com ", "organization-a", "account-a")
	second := CredentialFileName("user@example.com", "organization-b", "account-a")
	if first == second {
		t.Fatalf("organization filenames collide: %q", first)
	}
	if got := CredentialFileName(" user@example.com ", "", ""); got != "claude-user@example.com.json" {
		t.Fatalf("legacy filename = %q", got)
	}
}

func TestFindMatchingLegacyCredentialHonorsOrganization(t *testing.T) {
	authDir := t.TempDir()
	const email = "user@example.com"
	writeClaudeCredential(t, filepath.Join(authDir, CredentialFileName(email, "", "")), map[string]any{
		"type":              "claude",
		"email":             email,
		"organization_uuid": "organization-a",
		"account_uuid":      "shared-account",
		"prefix":            "team-a",
	})
	target := claudeTarget(email, "organization-b", "shared-account")
	matched, errMatch := FindMatchingLegacyCredential(context.Background(), authDir, target)
	if errMatch != nil {
		t.Fatal(errMatch)
	}
	if matched != nil {
		t.Fatalf("matched credential from a different organization: %#v", matched.Metadata)
	}

	target = claudeTarget(email, "organization-a", "shared-account")
	matched, errMatch = FindMatchingLegacyCredential(context.Background(), authDir, target)
	if errMatch != nil || matched == nil {
		t.Fatalf("matching organization result = %#v, error = %v", matched, errMatch)
	}
}

func TestFindMatchingLegacyCredentialAcceptsAccountHashedPredecessor(t *testing.T) {
	authDir := t.TempDir()
	const email = "user@example.com"
	predecessor := CredentialFileName(email, "", "account-a")
	writeClaudeCredential(t, filepath.Join(authDir, predecessor), map[string]any{
		"type":         "claude",
		"email":        email,
		"account_uuid": "account-a",
		"weight":       5,
	})

	matched, errMatch := FindMatchingLegacyCredential(
		context.Background(), authDir, claudeTarget(email, "organization-a", "account-a"),
	)
	if errMatch != nil || matched == nil || matched.FileName != predecessor {
		t.Fatalf("account predecessor = %#v, error = %v", matched, errMatch)
	}
}

func claudeTarget(email, organizationUUID, accountUUID string) *coreauth.Auth {
	fileName := CredentialFileName(email, organizationUUID, accountUUID)
	return &coreauth.Auth{
		ID:       fileName,
		Provider: "claude",
		FileName: fileName,
		Metadata: map[string]any{
			"type":              "claude",
			"email":             email,
			"organization_uuid": organizationUUID,
			"account_uuid":      accountUUID,
		},
	}
}

func writeClaudeCredential(t *testing.T, path string, metadata map[string]any) {
	t.Helper()
	raw, errJSON := json.Marshal(metadata)
	if errJSON != nil {
		t.Fatal(errJSON)
	}
	if errWrite := os.WriteFile(path, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
}
