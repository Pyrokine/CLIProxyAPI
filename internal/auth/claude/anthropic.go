package claude

// TokenData holds OAuth token information from Anthropic.
type TokenData struct {
	// AccessToken is the OAuth2 access token for API access
	AccessToken string `json:"access_token"`
	// RefreshToken is used to obtain new access tokens
	RefreshToken string `json:"refresh_token"`
	// Email is the Anthropic account email
	Email string `json:"email"`
	// Expire is the timestamp of the token expire
	Expire string `json:"expired"`
}

// AuthBundle aggregates authentication data after OAuth flow completion.
type AuthBundle struct {
	// APIKey is the Anthropic API key obtained from token exchange
	APIKey string `json:"api_key"`
	// TokenData contains the OAuth tokens from the authentication flow
	TokenData TokenData `json:"token_data"`
	// LastRefresh is the timestamp of the last token refresh
	LastRefresh string `json:"last_refresh"`
}
