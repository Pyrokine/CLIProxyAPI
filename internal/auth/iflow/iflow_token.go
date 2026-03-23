package iflow

import (
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth"
)

// TokenStorage persists iFlow OAuth credentials alongside the derived API key.
type TokenStorage struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	LastRefresh  string `json:"last_refresh"`
	Expire       string `json:"expired"`
	APIKey       string `json:"api_key"`
	Email        string `json:"email"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	Cookie       string `json:"cookie"`
	Type         string `json:"type"`

	// Metadata holds arbitrary key-value pairs injected via hooks.
	// It is not exported to JSON directly to allow flattening during serialization.
	Metadata map[string]any `json:"-"`
}

// SaveTokenToFile serialises the token storage to disk.
func (ts *TokenStorage) SaveTokenToFile(authFilePath string) error {
	ts.Type = "iflow"
	return auth.SaveTokenJSON(authFilePath, ts, ts.Metadata)
}
