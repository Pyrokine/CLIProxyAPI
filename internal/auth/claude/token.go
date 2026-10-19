// Package claude provides authentication and token management functionality
// for Anthropic's Claude AI services. It handles OAuth2 token storage, serialization,
// and retrieval for maintaining authenticated sessions with the Claude API.
package claude

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// TokenStorage stores OAuth2 token information for Anthropic Claude API authentication.
// It maintains compatibility with the existing auth system while adding Claude-specific fields
// for managing access tokens, refresh tokens, and user account information.
type TokenStorage struct {
	// IDToken is the JWT ID token containing user claims and identity information.
	IDToken string `json:"id_token"`

	// AccessToken is the OAuth2 access token used for authenticating API requests.
	AccessToken string `json:"access_token"`

	// RefreshToken is used to obtain new access tokens when the current one expires.
	RefreshToken string `json:"refresh_token"`

	// LastRefresh is the timestamp of the last token refresh operation.
	LastRefresh string `json:"last_refresh"`

	// Email is the Anthropic account email address associated with this token.
	Email string `json:"email"`

	// AccountUUID identifies the Anthropic account returned by OAuth.
	AccountUUID string `json:"account_uuid,omitempty"`

	// OrganizationUUID identifies the Anthropic organization returned by OAuth.
	OrganizationUUID string `json:"organization_uuid,omitempty"`

	// OrganizationName is the display name returned by OAuth.
	OrganizationName string `json:"organization_name,omitempty"`

	// DeviceIDs contains the device identity assigned to this credential.
	DeviceIDs []string `json:"claude_device_ids,omitempty"`

	// Type indicates the authentication provider type, always "claude" for this storage.
	Type string `json:"type"`

	// Expire is the timestamp when the current access token expires.
	Expire string `json:"expired"`

	// Metadata holds arbitrary key-value pairs injected via hooks.
	// It is not exported to JSON directly to allow flattening during serialization.
	Metadata map[string]any `json:"-"`
}

func (ts *TokenStorage) CloneForPersistence() coreauth.TokenStorage {
	if ts == nil {
		return nil
	}
	copyStorage := *ts
	return &copyStorage
}

// SetMetadata injects metadata before the token is persisted.
func (ts *TokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile serializes the Claude token storage to a JSON file.
// This method creates the necessary directory structure and writes the token
// data in JSON format to the specified file path for persistent storage.
// It merges any injected metadata into the top-level JSON object.
//
// Parameters:
//   - authFilePath: The full path where the token file should be saved
//
// Returns:
//   - error: An error if the operation fails, nil otherwise
func (ts *TokenStorage) SaveTokenToFile(authFilePath string) error {
	ts.Type = "claude"
	return auth.SaveTokenJSON(authFilePath, ts, ts.Metadata)
}
