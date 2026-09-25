package devin

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// CredentialFileName derives a stable, non-plaintext Devin credential identity.
// Complete account identity includes both user and organization IDs; incomplete
// enrichment falls back to the session-token digest.
func CredentialFileName(userID, orgID, sessionToken string) string {
	userID = strings.TrimSpace(userID)
	orgID = strings.TrimSpace(orgID)
	identity := "token\x00" + FormatSessionToken(sessionToken)
	if userID != "" && orgID != "" {
		identity = "user\x00" + userID + "\x00org\x00" + orgID
	}
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("devin-user-%s.json", hex.EncodeToString(digest[:8]))
}

// FindMatchingCredential finds an existing local Devin credential that uses the
// same session token when profile or status enrichment could not provide the
// complete user/organization identity.
func FindMatchingCredential(
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
	if target == nil || !strings.EqualFold(strings.TrimSpace(target.Provider), "devin") ||
		strings.TrimSpace(authDir) == "" {
		return nil, nil
	}
	if metadataString(target.Metadata, "user_id") != "" && metadataString(target.Metadata, "org_id") != "" {
		return nil, nil
	}
	targetToken := credentialToken(target.Metadata)
	if targetToken == "" {
		return nil, nil
	}
	targetDigest := sha256.Sum256([]byte(targetToken))

	var matches []*coreauth.Auth
	errWalk := filepath.WalkDir(authDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if errContext := ctx.Err(); errContext != nil {
			return errContext
		}
		if entry.IsDir() || !strings.HasPrefix(strings.ToLower(entry.Name()), "devin-") ||
			!strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			return nil
		}
		candidate, errRead := readDevinCredential(path)
		if errRead != nil {
			return errRead
		}
		if candidate == nil {
			return nil
		}
		candidateToken := credentialToken(candidate.Metadata)
		if candidateToken == "" {
			return nil
		}
		candidateDigest := sha256.Sum256([]byte(candidateToken))
		if subtle.ConstantTimeCompare(targetDigest[:], candidateDigest[:]) == 1 {
			matches = append(matches, candidate)
		}
		return nil
	})
	if errors.Is(errWalk, os.ErrNotExist) {
		return nil, nil
	}
	if errWalk != nil {
		return nil, fmt.Errorf("find existing Devin credential: %w", errWalk)
	}
	if len(matches) == 0 {
		return nil, nil
	}
	best := matches[0]
	bestRank := devinCredentialRank(best)
	ambiguous := false
	for _, candidate := range matches[1:] {
		rank := devinCredentialRank(candidate)
		switch {
		case rank > bestRank:
			best = candidate
			bestRank = rank
			ambiguous = false
		case rank == bestRank && filepath.Clean(candidate.ID) != filepath.Clean(best.ID):
			ambiguous = true
		}
	}
	if ambiguous {
		return nil, fmt.Errorf("find existing Devin credential: multiple credentials use the same session token")
	}
	return best, nil
}

// ReuseExistingCredentialIdentity keeps the stable local filename and restores
// identity attributes after a best-effort enrichment failure.
func ReuseExistingCredentialIdentity(target, existing *coreauth.Auth) {
	if target == nil || existing == nil {
		return
	}
	fileName := strings.TrimSpace(existing.FileName)
	if fileName == "" {
		fileName = filepath.Base(strings.TrimSpace(existing.ID))
	}
	if fileName != "" {
		target.ID = fileName
		target.FileName = fileName
	}
	ApplyMetadataIdentity(target)
}

// ApplyMetadataIdentity synchronizes the persistent Devin identity into runtime
// attributes after metadata preservation.
func ApplyMetadataIdentity(target *coreauth.Auth) {
	if target == nil {
		return
	}
	if target.Attributes == nil {
		target.Attributes = make(map[string]string)
	}
	for _, key := range []string{"user_name", "user_id", "org_id", "email", "plan", "base_url", "auth_kind"} {
		if value := metadataString(target.Metadata, key); value != "" {
			target.Attributes[key] = value
		}
	}
	identity := firstNonEmptyDevinIdentity(
		metadataString(target.Metadata, "user_name"),
		metadataString(target.Metadata, "user_id"),
	)
	if identity != "" {
		target.Label = fmt.Sprintf("Devin (%s)", identity)
		if email := metadataString(target.Metadata, "email"); email != "" {
			target.Label = fmt.Sprintf("Devin (%s - %s)", identity, email)
		}
	}
}

func readDevinCredential(path string) (*coreauth.Auth, error) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, errRead
	}
	metadata := make(map[string]any)
	if errJSON := json.Unmarshal(raw, &metadata); errJSON != nil {
		return nil, fmt.Errorf("decode Devin credential %s: %w", filepath.Base(path), errJSON)
	}
	coreauth.NormalizeCredentialMetadata(metadata)
	provider, _ := metadata["type"].(string)
	if !strings.EqualFold(strings.TrimSpace(provider), "devin") {
		return nil, nil
	}
	return &coreauth.Auth{
		ID:       path,
		Provider: "devin",
		FileName: filepath.Base(path),
		Attributes: map[string]string{
			coreauth.AttributePath: path,
		},
		Metadata: metadata,
	}, nil
}

func credentialToken(metadata map[string]any) string {
	return firstNonEmptyMetadata(metadata, "session_token", "api_key", "token")
}

func devinCredentialRank(auth *coreauth.Auth) int {
	if auth == nil {
		return 0
	}
	userID := metadataString(auth.Metadata, "user_id")
	orgID := metadataString(auth.Metadata, "org_id")
	if userID != "" && orgID != "" && strings.EqualFold(
		filepath.Base(auth.FileName), CredentialFileName(userID, orgID, credentialToken(auth.Metadata)),
	) {
		return 2
	}
	return 1
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}
