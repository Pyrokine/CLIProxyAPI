// Package xai provides OAuth2 authentication helpers for xAI Grok.
package xai

import "time"

const (
	// DefaultAPIBaseURL is the default official xAI API base URL.
	// It is used for WebSocket and media requests, and for non-media HTTP chat
	// when the auth selects the official API path.
	DefaultAPIBaseURL = "https://api.x.ai/v1"
	// CLIChatProxyBaseURL is the Grok CLI chat-proxy base URL for non-media
	// HTTP chat when using_api is false, including the OAuth default.
	CLIChatProxyBaseURL = "https://cli-chat-proxy.grok.com/v1"
	// issuer is xAI's OAuth issuer.
	issuer = "https://auth.x.ai"
	// discoveryURL is the OIDC discovery endpoint used to resolve OAuth endpoints.
	discoveryURL = issuer + "/.well-known/openid-configuration"
	// clientID is the public xAI Grok CLI OAuth client ID.
	clientID = "b1a00492-073a-47ea-816f-4c329264a828"
	// scope is the OAuth scope set required for xAI API access.
	scope = "openid profile email offline_access grok-cli:access api:access"
	// deviceCodeGrantType is the OAuth 2.0 device authorization grant type.
	deviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	// MaxPollDuration bounds device authorization polling.
	MaxPollDuration = 30 * time.Minute

	defaultPollInterval = 5 * time.Second
	httpClientTimeout   = 30 * time.Second
)

var refreshLead = 5 * time.Minute

// RefreshLead returns the refresh lead time for xAI OAuth credentials.
func RefreshLead() time.Duration {
	return refreshLead
}

// discovery contains OAuth endpoints resolved from xAI OIDC discovery.
type discovery struct {
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}

// DeviceCodeResponse represents an xAI device authorization response.
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	TokenEndpoint           string `json:"-"`
}

// TokenData holds xAI OAuth token data.
type TokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	Expire       string `json:"expired,omitempty"`
	Email        string `json:"email,omitempty"`
	Subject      string `json:"sub,omitempty"`
}

// AuthBundle aggregates token data and OAuth metadata for persistence.
type AuthBundle struct {
	TokenData     TokenData
	LastRefresh   string
	BaseURL       string
	RedirectURI   string
	TokenEndpoint string
}
