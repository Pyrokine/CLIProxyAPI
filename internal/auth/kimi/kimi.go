// Package kimi provides authentication and token management for Kimi (Moonshot AI) API.
// It handles the RFC 8628 OAuth2 Device Authorization Grant flow for secure authentication.
package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	// kimiClientID is Kimi Code's OAuth client ID.
	kimiClientID      = "17e5f671-d194-4dfb-9706-5516cb48c098"
	KimiDefaultDomain = "kimi.com"
	KimiAIDomain      = "kimi.ai"

	kimiOAuthHost   = "https://auth.kimi.com"
	kimiAIOAuthHost = "https://auth.kimi.ai"

	// APIBaseURL is the base URL for Kimi Code requests.
	APIBaseURL = "https://api.kimi.com/coding"
	// KimiAIAPIBaseURL is the base URL for Kimi.ai requests.
	KimiAIAPIBaseURL = "https://api.kimi.ai/coding"
	// defaultPollInterval is the default interval for polling token endpoint.
	defaultPollInterval = 5 * time.Second
	// maxPollDuration is the maximum time to wait for user authorization.
	maxPollDuration = 15 * time.Minute
)

var kimiRefreshGroup singleflight.Group

func knownKimiDomain(domain string) string {
	domain = strings.ToLower(strings.TrimSpace(domain))
	switch {
	case domain == KimiAIDomain, domain == "kimi-ai", domain == "ai", strings.HasSuffix(domain, ".kimi.ai"):
		return KimiAIDomain
	case domain == KimiDefaultDomain, domain == "kimi", domain == "com", strings.HasSuffix(domain, ".kimi.com"):
		return KimiDefaultDomain
	default:
		return ""
	}
}

// IsKimiAIDomain reports whether the domain identifies the Kimi.ai service.
func IsKimiAIDomain(domain string) bool {
	return knownKimiDomain(domain) == KimiAIDomain
}

func kimiDomainFromURL(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" {
		return ""
	}
	return knownKimiDomain(parsed.Hostname())
}

// ResolveKimiDomainFromAuth determines the OAuth service from an auth record.
func ResolveKimiDomainFromAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return KimiDefaultDomain
	}
	if auth.Attributes != nil {
		if domain := knownKimiDomain(auth.Attributes["domain"]); domain != "" {
			return domain
		}
		if domain := kimiDomainFromURL(auth.Attributes["base_url"]); domain != "" {
			return domain
		}
	}
	if auth.Metadata != nil {
		if value, ok := auth.Metadata["domain"].(string); ok {
			if domain := knownKimiDomain(value); domain != "" {
				return domain
			}
		}
		if value, ok := auth.Metadata["base_url"].(string); ok {
			if domain := kimiDomainFromURL(value); domain != "" {
				return domain
			}
		}
		if value, ok := auth.Metadata["type"].(string); ok {
			if domain := knownKimiDomain(value); domain != "" {
				return domain
			}
		}
	}
	if storage, ok := auth.Storage.(*TokenStorage); ok && storage != nil {
		if domain := knownKimiDomain(storage.Domain); domain != "" {
			return domain
		}
		if domain := kimiDomainFromURL(storage.BaseURL); domain != "" {
			return domain
		}
		if domain := knownKimiDomain(storage.Type); domain != "" {
			return domain
		}
	}
	if domain := knownKimiDomain(auth.Provider); domain != "" {
		return domain
	}
	for _, name := range []string{auth.ID, auth.FileName} {
		name = strings.ToLower(name)
		if strings.HasPrefix(name, "kimi-ai-") || strings.HasPrefix(name, "kimi.ai-") {
			return KimiAIDomain
		}
	}
	return KimiDefaultDomain
}

// IsKimiAIAuth reports whether an auth record belongs to Kimi.ai.
func IsKimiAIAuth(auth *coreauth.Auth) bool {
	return ResolveKimiDomainFromAuth(auth) == KimiAIDomain
}

// ResolveKimiAPIBaseURL returns the API endpoint for the specified Kimi service.
func ResolveKimiAPIBaseURL(domain string) string {
	if IsKimiAIDomain(domain) {
		return KimiAIAPIBaseURL
	}
	return APIBaseURL
}

func kimiDomain(domain string) string {
	if resolved := knownKimiDomain(domain); resolved != "" {
		return resolved
	}
	return KimiDefaultDomain
}

func kimiOAuthHostForDomain(domain string) string {
	if IsKimiAIDomain(domain) {
		return kimiAIOAuthHost
	}
	return kimiOAuthHost
}

// Auth handles Kimi authentication flow.
type Auth struct {
	deviceClient *DeviceFlowClient
	cfg          *config.Config
	domain       string
}

// NewAuth creates a new Kimi Code authentication service.
func NewAuth(cfg *config.Config) *Auth {
	return NewAuthWithDomain(cfg, KimiDefaultDomain)
}

// NewAuthWithDomain creates an authentication service for Kimi Code or Kimi.ai.
func NewAuthWithDomain(cfg *config.Config, domain string) *Auth {
	domain = kimiDomain(domain)
	return &Auth{
		deviceClient: NewDeviceFlowClientWithDomainAndDeviceID(cfg, domain, ""),
		cfg:          cfg,
		domain:       domain,
	}
}

// StartDeviceFlow initiates the device flow authentication.
func (k *Auth) StartDeviceFlow(ctx context.Context) (*DeviceCodeResponse, error) {
	return k.deviceClient.requestDeviceCode(ctx)
}

// WaitForAuthorization polls for user authorization and returns the auth bundle.
func (k *Auth) WaitForAuthorization(ctx context.Context, deviceCode *DeviceCodeResponse) (*AuthBundle, error) {
	TokenData, err := k.deviceClient.pollForToken(ctx, deviceCode)
	if err != nil {
		return nil, err
	}

	return &AuthBundle{
		TokenData: TokenData,
		DeviceID:  k.deviceClient.deviceID,
	}, nil
}

// CreateTokenStorage creates a new TokenStorage from auth bundle.
func (k *Auth) CreateTokenStorage(bundle *AuthBundle) *TokenStorage {
	expired := ""
	if bundle.TokenData.ExpiresAt > 0 {
		expired = time.Unix(bundle.TokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	provider := "kimi"
	if IsKimiAIDomain(k.domain) {
		provider = "kimi-ai"
	}
	return &TokenStorage{
		AccessToken:  bundle.TokenData.AccessToken,
		RefreshToken: bundle.TokenData.RefreshToken,
		TokenType:    bundle.TokenData.TokenType,
		Scope:        bundle.TokenData.Scope,
		DeviceID:     strings.TrimSpace(bundle.DeviceID),
		Expired:      expired,
		Type:         provider,
		Domain:       k.domain,
		BaseURL:      ResolveKimiAPIBaseURL(k.domain),
	}
}

// DeviceFlowClient handles the OAuth2 device flow for Kimi.
type DeviceFlowClient struct {
	httpClient *http.Client
	cfg        *config.Config
	deviceID   string
	domain     string
}

// NewDeviceFlowClientWithDeviceID creates a Kimi Code client with the specified device ID.
func NewDeviceFlowClientWithDeviceID(cfg *config.Config, deviceID string) *DeviceFlowClient {
	return NewDeviceFlowClientWithDomainAndDeviceID(cfg, KimiDefaultDomain, deviceID)
}

// NewDeviceFlowClientWithDomainAndDeviceID creates a domain-specific client with the specified device ID.
func NewDeviceFlowClientWithDomainAndDeviceID(cfg *config.Config, domain, deviceID string, httpClients ...*http.Client) *DeviceFlowClient {
	client := &http.Client{Timeout: 30 * time.Second}
	if len(httpClients) > 0 && httpClients[0] != nil {
		client = httpClients[0]
	} else if cfg != nil {
		client = util.SetProxy(&cfg.SDKConfig, client)
	}
	resolvedDeviceID := strings.TrimSpace(deviceID)
	if resolvedDeviceID == "" {
		resolvedDeviceID = getOrCreateDeviceID()
	}
	return &DeviceFlowClient{
		httpClient: client,
		cfg:        cfg,
		deviceID:   resolvedDeviceID,
		domain:     kimiDomain(domain),
	}
}

func (c *DeviceFlowClient) deviceCodeURL() string {
	return kimiOAuthHostForDomain(c.domain) + "/api/oauth/device_authorization"
}

func (c *DeviceFlowClient) tokenURL() string {
	return kimiOAuthHostForDomain(c.domain) + "/api/oauth/token"
}

// getOrCreateDeviceID returns an in-memory device ID for the current authentication flow.
func getOrCreateDeviceID() string {
	return uuid.New().String()
}

// getDeviceModel returns a device model string.
func getDeviceModel() string {
	osName := runtime.GOOS
	arch := runtime.GOARCH

	switch osName {
	case "darwin":
		return fmt.Sprintf("macOS %s", arch)
	case "windows":
		return fmt.Sprintf("Windows %s", arch)
	case "linux":
		return fmt.Sprintf("Linux %s", arch)
	default:
		return fmt.Sprintf("%s %s", osName, arch)
	}
}

// getHostname returns the machine hostname.
func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

// commonHeaders returns headers required for Kimi API requests.
func (c *DeviceFlowClient) commonHeaders() map[string]string {
	return map[string]string{
		"X-Msh-Platform":     "CLIProxyAPI",
		"X-Msh-Version":      buildinfo.Version,
		"X-Msh-Device-Name":  getHostname(),
		"X-Msh-Device-Model": getDeviceModel(),
		"X-Msh-Device-Id":    c.deviceID,
	}
}

// requestDeviceCode initiates the device flow by requesting a device code from Kimi.
func (c *DeviceFlowClient) requestDeviceCode(ctx context.Context) (*DeviceCodeResponse, error) {
	data := url.Values{}
	data.Set("client_id", kimiClientID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.deviceCodeURL(), strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("kimi: failed to create device code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, v := range c.commonHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kimi: device code request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("kimi device code: close body error: %v", errClose)
		}
	}()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("kimi: failed to read device code response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kimi: device code request failed with status %d", resp.StatusCode)
	}

	var deviceCode DeviceCodeResponse
	if err = json.Unmarshal(bodyBytes, &deviceCode); err != nil {
		return nil, fmt.Errorf("kimi: failed to parse device code response: %w", err)
	}

	return &deviceCode, nil
}

// pollForToken polls the token endpoint until the user authorizes or the device code expires.
func (c *DeviceFlowClient) pollForToken(ctx context.Context, deviceCode *DeviceCodeResponse) (*TokenData, error) {
	if deviceCode == nil {
		return nil, fmt.Errorf("kimi: device code is nil")
	}

	interval := max(time.Duration(deviceCode.Interval)*time.Second, defaultPollInterval)

	deadline := time.Now().Add(maxPollDuration)
	if deviceCode.ExpiresIn > 0 {
		codeDeadline := time.Now().Add(time.Duration(deviceCode.ExpiresIn) * time.Second)
		if codeDeadline.Before(deadline) {
			deadline = codeDeadline
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("kimi: context cancelled: %w", ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("kimi: device code expired")
			}

			token, pollErr, shouldContinue := c.exchangeDeviceCode(ctx, deviceCode.DeviceCode)
			if token != nil {
				return token, nil
			}
			if !shouldContinue {
				return nil, pollErr
			}
			// Continue polling
		}
	}
}

// exchangeDeviceCode attempts to exchange the device code for an access token.
// Returns (token, error, shouldContinue).
func (c *DeviceFlowClient) exchangeDeviceCode(ctx context.Context, deviceCode string) (*TokenData, error, bool) {
	data := url.Values{}
	data.Set("client_id", kimiClientID)
	data.Set("device_code", deviceCode)
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL(), strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("kimi: failed to create token request: %w", err), false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, v := range c.commonHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kimi: token request failed: %w", err), false
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("kimi token exchange: close body error: %v", errClose)
		}
	}()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("kimi: failed to read token response: %w", err), false
	}

	// Parse response - Kimi returns 200 for both success and pending states
	var oauthResp struct {
		Error        string  `json:"error"`
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		TokenType    string  `json:"token_type"`
		ExpiresIn    float64 `json:"expires_in"`
		Scope        string  `json:"scope"`
	}

	if err = json.Unmarshal(bodyBytes, &oauthResp); err != nil {
		return nil, fmt.Errorf("kimi: failed to parse token response: %w", err), false
	}

	if oauthResp.Error != "" {
		switch oauthResp.Error {
		case "authorization_pending":
			return nil, nil, true // Continue polling
		case "slow_down":
			return nil, nil, true // Continue polling (with increased interval handled by caller)
		case "expired_token":
			return nil, fmt.Errorf("kimi: device code expired"), false
		case "access_denied":
			return nil, fmt.Errorf("kimi: access denied by user"), false
		default:
			return nil, fmt.Errorf("kimi: authorization failed (status %d)", resp.StatusCode), false
		}
	}

	if oauthResp.AccessToken == "" {
		return nil, fmt.Errorf("kimi: empty access token in response"), false
	}

	var expiresAt int64
	if oauthResp.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + int64(oauthResp.ExpiresIn)
	}

	return &TokenData{
		AccessToken:  oauthResp.AccessToken,
		RefreshToken: oauthResp.RefreshToken,
		TokenType:    oauthResp.TokenType,
		ExpiresAt:    expiresAt,
		Scope:        oauthResp.Scope,
	}, nil, false
}

// RefreshToken exchanges a refresh token for a new access token.
func (c *DeviceFlowClient) RefreshToken(ctx context.Context, refreshToken string) (*TokenData, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("kimi: refresh token is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	result, err, _ := kimiRefreshGroup.Do(
		c.tokenURL()+"\x00"+refreshToken, func() (any, error) {
			return c.refreshTokenSingleFlight(context.WithoutCancel(ctx), refreshToken)
		},
	)
	if err != nil {
		return nil, err
	}
	TokenData, ok := result.(*TokenData)
	if !ok || TokenData == nil {
		return nil, fmt.Errorf("kimi: refresh token failed: invalid single-flight result")
	}
	return TokenData, nil
}

func (c *DeviceFlowClient) refreshTokenSingleFlight(ctx context.Context, refreshToken string) (*TokenData, error) {
	data := url.Values{}
	data.Set("client_id", kimiClientID)
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", refreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL(), strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("kimi: failed to create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	for k, v := range c.commonHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kimi: refresh request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("kimi refresh token: close body error: %v", errClose)
		}
	}()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("kimi: failed to read refresh response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("kimi: refresh token rejected (status %d)", resp.StatusCode)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kimi: refresh failed with status %d", resp.StatusCode)
	}

	var tokenResp struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		TokenType    string  `json:"token_type"`
		ExpiresIn    float64 `json:"expires_in"`
		Scope        string  `json:"scope"`
	}

	if err = json.Unmarshal(bodyBytes, &tokenResp); err != nil {
		return nil, fmt.Errorf("kimi: failed to parse refresh response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return nil, fmt.Errorf("kimi: empty access token in refresh response")
	}

	var expiresAt int64
	if tokenResp.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + int64(tokenResp.ExpiresIn)
	}

	return &TokenData{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    expiresAt,
		Scope:        tokenResp.Scope,
	}, nil
}
