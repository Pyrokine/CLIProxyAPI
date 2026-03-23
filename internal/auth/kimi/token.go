// Package kimi provides authentication and token management functionality
// for Kimi (Moonshot AI) services. It handles OAuth2 device flow token storage,
// serialization, and retrieval for maintaining authenticated sessions with the Kimi API.
package kimi

import (
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
)

// TokenStorage stores OAuth2 token information for Kimi API authentication.
type TokenStorage struct {
	// AccessToken is the OAuth2 access token used for authenticating API requests.
	AccessToken string `json:"access_token"`
	// RefreshToken is the OAuth2 refresh token used to obtain new access tokens.
	RefreshToken string `json:"refresh_token"`
	// TokenType is the type of token, typically "Bearer".
	TokenType string `json:"token_type"`
	// Scope is the OAuth2 scope granted to the token.
	Scope string `json:"scope,omitempty"`
	// DeviceID is the OAuth device flow identifier used for Kimi requests.
	DeviceID string `json:"device_id,omitempty"`
	// Expired is the RFC3339 timestamp when the access token expires.
	Expired string `json:"expired,omitempty"`
	// Type indicates the authentication provider type, always "kimi" for this storage.
	Type string `json:"type"`

	// Metadata holds arbitrary key-value pairs injected via hooks.
	// It is not exported to JSON directly to allow flattening during serialization.
	Metadata map[string]any `json:"-"`
}

// BuildMetadata constructs the standard metadata map for a Kimi auth bundle.
func BuildMetadata(bundle *AuthBundle) map[string]any {
	metadata := map[string]any{
		"type":          "kimi",
		"access_token":  bundle.TokenData.AccessToken,
		"refresh_token": bundle.TokenData.RefreshToken,
		"token_type":    bundle.TokenData.TokenType,
		"scope":         bundle.TokenData.Scope,
		"timestamp":     time.Now().UnixMilli(),
	}
	if bundle.TokenData.ExpiresAt > 0 {
		metadata["expired"] = time.Unix(bundle.TokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	if deviceID := strings.TrimSpace(bundle.DeviceID); deviceID != "" {
		metadata["device_id"] = deviceID
	}
	return metadata
}

// TokenData holds the raw OAuth token response from Kimi.
type TokenData struct {
	// AccessToken is the OAuth2 access token.
	AccessToken string `json:"access_token"`
	// RefreshToken is the OAuth2 refresh token.
	RefreshToken string `json:"refresh_token"`
	// TokenType is the type of token, typically "Bearer".
	TokenType string `json:"token_type"`
	// ExpiresAt is the Unix timestamp when the token expires.
	ExpiresAt int64 `json:"expires_at"`
	// Scope is the OAuth2 scope granted to the token.
	Scope string `json:"scope"`
}

// AuthBundle bundles authentication data for storage.
type AuthBundle struct {
	// TokenData contains the OAuth token information.
	TokenData *TokenData
	// DeviceID is the device identifier used during OAuth device flow.
	DeviceID string
}

// DeviceCodeResponse represents Kimi's device code response.
type DeviceCodeResponse struct {
	// DeviceCode is the device verification code.
	DeviceCode string `json:"device_code"`
	// UserCode is the code the user must enter at the verification URI.
	UserCode string `json:"user_code"`
	// VerificationURI is the URL where the user should enter the code.
	VerificationURI string `json:"verification_uri,omitempty"`
	// VerificationURIComplete is the URL with the code pre-filled.
	VerificationURIComplete string `json:"verification_uri_complete"`
	// ExpiresIn is the number of seconds until the device code expires.
	ExpiresIn int `json:"expires_in"`
	// Interval is the minimum number of seconds to wait between polling requests.
	Interval int `json:"interval"`
}

// SaveTokenToFile serializes the token storage to a JSON file.
func (ts *TokenStorage) SaveTokenToFile(authFilePath string) error {
	ts.Type = "kimi"
	return auth.SaveTokenJSON(authFilePath, ts, ts.Metadata)
}

