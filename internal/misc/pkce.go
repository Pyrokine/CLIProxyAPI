package misc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCECodes holds PKCE verification codes for OAuth2 PKCE flow.
type PKCECodes struct {
	// CodeVerifier is the cryptographically random string used to correlate
	// the authorization request to the token request
	CodeVerifier string `json:"code_verifier"`
	// CodeChallenge is the SHA256 hash of the code verifier, base64url-encoded
	CodeChallenge string `json:"code_challenge"`
}

// GeneratePKCECodes generates a PKCE code verifier and challenge pair
// following RFC 7636 specifications for OAuth 2.0 PKCE extension.
func GeneratePKCECodes() (*PKCECodes, error) {
	// Generate code verifier: 43-128 characters, URL-safe
	codeVerifier, err := generateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("failed to generate code verifier: %w", err)
	}

	// Generate code challenge using S256 method
	codeChallenge := generateCodeChallenge(codeVerifier)

	return &PKCECodes{
		CodeVerifier:  codeVerifier,
		CodeChallenge: codeChallenge,
	}, nil
}

// generateCodeVerifier creates a cryptographically random string
// of 128 characters using URL-safe base64 encoding.
func generateCodeVerifier() (string, error) {
	// Generate 96 random bytes (will result in 128 base64 characters)
	b := make([]byte, 96)
	_, err := rand.Read(b)
	if err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}

	// Encode to URL-safe base64 without padding
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b), nil
}

// generateCodeChallenge creates a SHA256 hash of the code verifier
// and encodes it using URL-safe base64 encoding without padding.
func generateCodeChallenge(codeVerifier string) string {
	hash := sha256.Sum256([]byte(codeVerifier))
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(hash[:])
}
