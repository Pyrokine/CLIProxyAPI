package devin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCECodes contains an OAuth PKCE verifier and S256 challenge.
type PKCECodes struct {
	CodeVerifier  string
	CodeChallenge string
}

// GeneratePKCECodes creates a cryptographically random PKCE pair.
func GeneratePKCECodes() (*PKCECodes, error) {
	random := make([]byte, 64)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("generate Devin PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(random)
	digest := sha256.Sum256([]byte(verifier))
	return &PKCECodes{
		CodeVerifier:  verifier,
		CodeChallenge: base64.RawURLEncoding.EncodeToString(digest[:]),
	}, nil
}
