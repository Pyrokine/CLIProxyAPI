package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// CredentialFileName returns the canonical Claude credential filename. The
// organization identity is preferred so one email can safely belong to more
// than one organization.
func CredentialFileName(email, organizationUUID, accountUUID string) string {
	email = strings.TrimSpace(email)
	identity := strings.TrimSpace(organizationUUID)
	if identity == "" {
		identity = strings.TrimSpace(accountUUID)
	}
	if identity == "" {
		return fmt.Sprintf("claude-%s.json", email)
	}
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("claude-%s-%s.json", hex.EncodeToString(digest[:])[:8], email)
}

// FindMatchingLegacyCredential finds an email-only or account-hashed predecessor
// that is provably the same Claude account as target.
func FindMatchingLegacyCredential(
	ctx context.Context,
	authDir string,
	target *coreauth.Auth,
) (*coreauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if errContext := ctx.Err(); errContext != nil {
		return nil, errContext
	}
	if !isHashedCredentialTarget(target) || strings.TrimSpace(authDir) == "" {
		return nil, nil
	}

	targetEmail := metadataString(target.Metadata, "email")
	targetOrganization := metadataString(target.Metadata, "organization_uuid")
	targetAccount := metadataString(target.Metadata, "account_uuid")
	legacyFileName := CredentialFileName(targetEmail, "", "")
	candidateNames := []string{legacyFileName}
	accountFileName := ""
	if targetOrganization != "" && targetAccount != "" {
		accountFileName = CredentialFileName(targetEmail, "", targetAccount)
		if !strings.EqualFold(accountFileName, legacyFileName) {
			candidateNames = append(candidateNames, accountFileName)
		}
	}

	for _, candidateName := range candidateNames {
		if errContext := ctx.Err(); errContext != nil {
			return nil, errContext
		}
		candidate, errRead := readCredential(filepath.Join(authDir, candidateName))
		if errors.Is(errRead, os.ErrNotExist) {
			continue
		}
		if errRead != nil {
			return nil, errRead
		}
		candidateOrganization := metadataString(candidate.Metadata, "organization_uuid")
		candidateAccount := metadataString(candidate.Metadata, "account_uuid")
		isEmailLegacy := strings.EqualFold(candidateName, legacyFileName)
		isAccountPredecessor := accountFileName != "" && strings.EqualFold(candidateName, accountFileName)
		switch {
		case targetOrganization != "" && candidateOrganization != "" &&
			strings.EqualFold(candidateOrganization, targetOrganization):
			return candidate, nil
		case targetOrganization != "" && candidateOrganization == "" && isAccountPredecessor &&
			candidateAccount != "" && strings.EqualFold(candidateAccount, targetAccount):
			return candidate, nil
		case targetOrganization == "" && isEmailLegacy && candidateOrganization == "" &&
			targetAccount != "" && candidateAccount != "" && strings.EqualFold(candidateAccount, targetAccount):
			return candidate, nil
		}
	}
	return nil, nil
}

func readCredential(path string) (*coreauth.Auth, error) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, errRead
	}
	metadata := make(map[string]any)
	if errJSON := json.Unmarshal(raw, &metadata); errJSON != nil {
		return nil, fmt.Errorf("read Claude legacy credential %s: %w", filepath.Base(path), errJSON)
	}
	coreauth.NormalizeCredentialMetadata(metadata)
	provider, _ := metadata["type"].(string)
	if !strings.EqualFold(strings.TrimSpace(provider), "claude") {
		return nil, fmt.Errorf("claude legacy credential %s has provider %q", filepath.Base(path), provider)
	}
	return &coreauth.Auth{
		ID:       path,
		Provider: "claude",
		FileName: filepath.Base(path),
		Attributes: map[string]string{
			coreauth.AttributePath: path,
		},
		Metadata: metadata,
	}, nil
}

func isHashedCredentialTarget(target *coreauth.Auth) bool {
	if target == nil || !strings.EqualFold(strings.TrimSpace(target.Provider), "claude") {
		return false
	}
	email := metadataString(target.Metadata, "email")
	organizationUUID := metadataString(target.Metadata, "organization_uuid")
	accountUUID := metadataString(target.Metadata, "account_uuid")
	if email == "" || organizationUUID == "" && accountUUID == "" {
		return false
	}
	fileName := strings.TrimSpace(target.FileName)
	if fileName == "" {
		fileName = strings.TrimSpace(target.ID)
	}
	return strings.EqualFold(filepath.Base(fileName), CredentialFileName(email, organizationUUID, accountUUID))
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}
