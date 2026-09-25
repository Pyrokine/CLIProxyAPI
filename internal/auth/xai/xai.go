package xai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

// Auth performs xAI OAuth discovery, token exchange, and refresh.
type Auth struct {
	httpClient      *http.Client
	refreshGroup    singleflight.Group
	minPollInterval time.Duration
	maxPollDuration time.Duration
}

// NewXAIAuth creates an xAI OAuth helper using config proxy settings.
func NewXAIAuth(cfg *config.Config) *Auth {
	return NewXAIAuthWithProxyURL(cfg, "")
}

// NewXAIAuthWithProxyURL creates an xAI OAuth helper with an explicit proxy URL.
func NewXAIAuthWithProxyURL(cfg *config.Config, proxyURL string) *Auth {
	effectiveProxyURL := strings.TrimSpace(proxyURL)
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	sdkCfg.ProxyURL = effectiveProxyURL
	client := util.SetProxy(&sdkCfg, &http.Client{Timeout: httpClientTimeout})
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("xai OAuth redirect limit exceeded")
		}
		if _, err := validateOAuthEndpoint(req.URL.String(), "redirect endpoint"); err != nil {
			return err
		}
		return nil
	}
	return &Auth{httpClient: client}
}

// validateOAuthEndpoint validates an endpoint returned by xAI discovery.
func validateOAuthEndpoint(rawURL string, field string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", fmt.Errorf("xai discovery %s is empty", field)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("xai discovery %s is invalid: %w", field, err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", fmt.Errorf("xai discovery %s must use https: %q", field, rawURL)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("xai discovery %s must not contain user information", field)
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	if host != "x.ai" && !strings.HasSuffix(host, ".x.ai") {
		return "", fmt.Errorf("xai discovery %s host %q is not on x.ai", field, host)
	}
	port := parsed.Port()
	parsed.Scheme = "https"
	parsed.Host = host
	if port != "" && port != "443" {
		parsed.Host += ":" + port
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}

// discover resolves xAI OAuth endpoints through OIDC discovery.
func (a *Auth) discover(ctx context.Context) (*discovery, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return nil, fmt.Errorf("xai discovery: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xai discovery: request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("xai discovery: close response body error: %v", errClose)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("xai discovery: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"xai discovery failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	var payload struct {
		DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
		TokenEndpoint               string `json:"token_endpoint"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("xai discovery: parse response: %w", err)
	}
	deviceAuthorizationEndpoint, err := validateOAuthEndpoint(
		payload.DeviceAuthorizationEndpoint,
		"device_authorization_endpoint",
	)
	if err != nil {
		return nil, err
	}
	tokenEndpoint, err := validateOAuthEndpoint(payload.TokenEndpoint, "token_endpoint")
	if err != nil {
		return nil, err
	}
	return &discovery{
		DeviceAuthorizationEndpoint: deviceAuthorizationEndpoint,
		TokenEndpoint:               tokenEndpoint,
	}, nil
}

// StartDeviceFlow requests a device code through xAI OIDC discovery.
func (a *Auth) StartDeviceFlow(ctx context.Context) (*DeviceCodeResponse, error) {
	discovery, err := a.discover(ctx)
	if err != nil {
		return nil, err
	}
	return a.requestDeviceCode(ctx, discovery.DeviceAuthorizationEndpoint, discovery.TokenEndpoint)
}

// requestDeviceCode requests a device authorization code from xAI.
func (a *Auth) requestDeviceCode(
	ctx context.Context,
	deviceAuthorizationEndpoint string,
	tokenEndpoint string,
) (*DeviceCodeResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deviceAuthorizationEndpoint, err := validateOAuthEndpoint(
		deviceAuthorizationEndpoint, "device_authorization_endpoint",
	)
	if err != nil {
		return nil, err
	}
	tokenEndpoint, err = validateOAuthEndpoint(tokenEndpoint, "token_endpoint")
	if err != nil {
		return nil, err
	}

	form := url.Values{
		"client_id": {clientID},
		"scope":     {scope},
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		deviceAuthorizationEndpoint,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("xai device code: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xai device code request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("xai device code: close response body error: %v", errClose)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("xai device code: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"xai device code request failed with status %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}

	var deviceCode DeviceCodeResponse
	if err = json.Unmarshal(body, &deviceCode); err != nil {
		return nil, fmt.Errorf("xai device code: parse response: %w", err)
	}
	if strings.TrimSpace(deviceCode.DeviceCode) == "" {
		return nil, fmt.Errorf("xai device code: response missing device_code")
	}
	if strings.TrimSpace(deviceCode.UserCode) == "" {
		return nil, fmt.Errorf("xai device code: response missing user_code")
	}
	if strings.TrimSpace(deviceCode.VerificationURI) == "" &&
		strings.TrimSpace(deviceCode.VerificationURIComplete) == "" {
		return nil, fmt.Errorf("xai device code: response missing verification URI")
	}
	deviceCode.DeviceCode = strings.TrimSpace(deviceCode.DeviceCode)
	deviceCode.UserCode = strings.TrimSpace(deviceCode.UserCode)
	deviceCode.VerificationURI = strings.TrimSpace(deviceCode.VerificationURI)
	deviceCode.VerificationURIComplete = strings.TrimSpace(deviceCode.VerificationURIComplete)
	deviceCode.TokenEndpoint = tokenEndpoint
	return &deviceCode, nil
}

// WaitForAuthorization polls until the user authorizes the device code.
func (a *Auth) WaitForAuthorization(ctx context.Context, deviceCode *DeviceCodeResponse) (*AuthBundle, error) {
	tokenData, err := a.pollForToken(ctx, deviceCode)
	if err != nil {
		return nil, err
	}
	tokenEndpoint := ""
	if deviceCode != nil {
		tokenEndpoint = strings.TrimSpace(deviceCode.TokenEndpoint)
	}
	return &AuthBundle{
		TokenData:     *tokenData,
		LastRefresh:   time.Now().UTC().Format(time.RFC3339),
		BaseURL:       DefaultAPIBaseURL,
		TokenEndpoint: tokenEndpoint,
	}, nil
}

// pollForToken polls xAI's token endpoint until authorization completes.
func (a *Auth) pollForToken(ctx context.Context, deviceCode *DeviceCodeResponse) (*TokenData, error) {
	if deviceCode == nil {
		return nil, fmt.Errorf("xai device code: response is nil")
	}
	if strings.TrimSpace(deviceCode.DeviceCode) == "" {
		return nil, fmt.Errorf("xai device code: device_code is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	tokenEndpoint := strings.TrimSpace(deviceCode.TokenEndpoint)
	if tokenEndpoint == "" {
		discovery, err := a.discover(ctx)
		if err != nil {
			return nil, err
		}
		tokenEndpoint = discovery.TokenEndpoint
	}
	validatedTokenEndpoint, err := validateOAuthEndpoint(tokenEndpoint, "token_endpoint")
	if err != nil {
		return nil, err
	}

	minInterval := defaultPollInterval
	if a != nil && a.minPollInterval > 0 {
		minInterval = a.minPollInterval
	}
	interval := time.Duration(deviceCode.Interval) * time.Second
	if a != nil && a.minPollInterval > 0 && deviceCode.Interval <= 0 {
		interval = a.minPollInterval
	} else if interval < minInterval {
		interval = minInterval
	}

	pollDuration := MaxPollDuration
	if a != nil && a.maxPollDuration > 0 {
		pollDuration = a.maxPollDuration
	}
	now := time.Now()
	deadline := now.Add(pollDuration)
	if deviceCode.ExpiresIn > 0 {
		deviceDeadline := now.Add(time.Duration(deviceCode.ExpiresIn) * time.Second)
		if deviceDeadline.Before(deadline) {
			deadline = deviceDeadline
		}
	}
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-pollCtx.Done():
			if ctx.Err() != nil {
				return nil, fmt.Errorf("xai device code: context cancelled: %w", ctx.Err())
			}
			return nil, fmt.Errorf("xai device code expired")
		case <-timer.C:
			if !time.Now().Before(deadline) {
				return nil, fmt.Errorf("xai device code expired")
			}
			token, nextInterval, shouldContinue, pollErr := a.exchangeDeviceCode(
				pollCtx,
				validatedTokenEndpoint,
				deviceCode.DeviceCode,
				interval,
			)
			if !time.Now().Before(deadline) {
				return nil, fmt.Errorf("xai device code expired")
			}
			if pollCtx.Err() != nil {
				if ctx.Err() != nil {
					return nil, fmt.Errorf("xai device code: context cancelled: %w", ctx.Err())
				}
				return nil, fmt.Errorf("xai device code expired")
			}
			if token != nil {
				return token, nil
			}
			if !shouldContinue {
				return nil, pollErr
			}
			interval = nextInterval
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nil, fmt.Errorf("xai device code expired")
			}
			if interval > remaining {
				interval = remaining
			}
			timer.Reset(interval)
		}
	}
}

func (a *Auth) exchangeDeviceCode(
	ctx context.Context,
	tokenEndpoint string,
	deviceCode string,
	interval time.Duration,
) (*TokenData, time.Duration, bool, error) {
	form := url.Values{
		"grant_type":  {deviceCodeGrantType},
		"device_code": {strings.TrimSpace(deviceCode)},
		"client_id":   {clientID},
	}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		tokenEndpoint,
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, interval, false, fmt.Errorf("xai device token: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, interval, false, fmt.Errorf("xai device token request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("xai device token: close response body error: %v", errClose)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, interval, false, fmt.Errorf("xai device token: read response: %w", err)
	}

	var payload struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		IDToken          string `json:"id_token"`
		TokenType        string `json:"token_type"`
		ExpiresIn        int    `json:"expires_in"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return nil, interval, false, fmt.Errorf("xai device token: parse response: %w", err)
	}
	if payload.Error != "" {
		switch payload.Error {
		case "authorization_pending":
			return nil, interval, true, nil
		case "slow_down":
			step := defaultPollInterval
			if a != nil && a.minPollInterval > 0 {
				step = a.minPollInterval
			}
			return nil, interval + step, true, nil
		case "expired_token":
			return nil, interval, false, fmt.Errorf("xai device code expired")
		case "access_denied":
			return nil, interval, false, fmt.Errorf("xai device authorization denied")
		default:
			description := strings.TrimSpace(payload.ErrorDescription)
			if description != "" {
				return nil, interval, false, fmt.Errorf(
					"xai device token error: %s: %s",
					payload.Error,
					description,
				)
			}
			return nil, interval, false, fmt.Errorf("xai device token error: %s", payload.Error)
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, interval, false, fmt.Errorf(
			"xai device token request failed with status %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return nil, interval, false, fmt.Errorf("xai device token response missing access_token")
	}
	email, subject := parseJWTIdentity(payload.IDToken)
	return buildTokenData(
		payload.AccessToken,
		payload.RefreshToken,
		payload.IDToken,
		payload.TokenType,
		payload.ExpiresIn,
		email,
		subject,
	), interval, false, nil
}

// RefreshTokens refreshes an xAI access token.
func (a *Auth) RefreshTokens(ctx context.Context, refreshToken, tokenEndpoint string) (*TokenData, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("xai token refresh: refresh token is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	tokenEndpoint = strings.TrimSpace(tokenEndpoint)
	if tokenEndpoint == "" {
		discovery, errDiscover := a.discover(ctx)
		if errDiscover != nil {
			return nil, errDiscover
		}
		tokenEndpoint = discovery.TokenEndpoint
	}
	validatedTokenEndpoint, errValidate := validateOAuthEndpoint(tokenEndpoint, "token_endpoint")
	if errValidate != nil {
		return nil, errValidate
	}

	if errContext := ctx.Err(); errContext != nil {
		return nil, fmt.Errorf("xai token refresh: context cancelled: %w", errContext)
	}
	singleflightKey := refreshToken + "\x00" + validatedTokenEndpoint
	sharedCtx, cancelRefresh := context.WithTimeout(context.WithoutCancel(ctx), httpClientTimeout)
	defer cancelRefresh()
	result, errRefresh, _ := a.refreshGroup.Do(
		singleflightKey, func() (any, error) {
			return a.refreshTokensSingleFlight(sharedCtx, refreshToken, validatedTokenEndpoint)
		},
	)
	if errRefresh != nil {
		return nil, errRefresh
	}
	tokenData, ok := result.(*TokenData)
	if !ok || tokenData == nil {
		return nil, fmt.Errorf("xai token refresh failed: invalid single-flight result")
	}
	return tokenData, nil
}

func (a *Auth) refreshTokensSingleFlight(ctx context.Context, refreshToken, tokenEndpoint string) (*TokenData, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}
	return a.postTokenForm(ctx, tokenEndpoint, form)
}

func (a *Auth) postTokenForm(ctx context.Context, tokenEndpoint string, form url.Values) (*TokenData, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, strings.TrimSpace(tokenEndpoint), strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("xai token request: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("xai token request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("xai token request: close response body error: %v", errClose)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("xai token response: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"xai token request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)),
		)
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err = json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("xai token response: parse body: %w", err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return nil, fmt.Errorf("xai token response missing access_token")
	}
	email, subject := parseJWTIdentity(payload.IDToken)
	return buildTokenData(
		payload.AccessToken,
		payload.RefreshToken,
		payload.IDToken,
		payload.TokenType,
		payload.ExpiresIn,
		email,
		subject,
	), nil
}

// CreateTokenStorage converts an auth bundle into persistable storage.
func (a *Auth) CreateTokenStorage(bundle *AuthBundle) *TokenStorage {
	if bundle == nil {
		return nil
	}
	return &TokenStorage{
		Type:          "xai",
		AccessToken:   bundle.TokenData.AccessToken,
		RefreshToken:  bundle.TokenData.RefreshToken,
		IDToken:       bundle.TokenData.IDToken,
		TokenType:     bundle.TokenData.TokenType,
		ExpiresIn:     bundle.TokenData.ExpiresIn,
		Expire:        bundle.TokenData.Expire,
		LastRefresh:   bundle.LastRefresh,
		Email:         strings.TrimSpace(bundle.TokenData.Email),
		Subject:       bundle.TokenData.Subject,
		BaseURL:       firstNonEmpty(bundle.BaseURL, DefaultAPIBaseURL),
		RedirectURI:   bundle.RedirectURI,
		TokenEndpoint: bundle.TokenEndpoint,
		AuthKind:      "oauth",
	}
}

func buildTokenData(
	accessToken string,
	refreshToken string,
	idToken string,
	tokenType string,
	expiresIn int,
	email string,
	subject string,
) *TokenData {
	tokenData := &TokenData{
		AccessToken:  strings.TrimSpace(accessToken),
		RefreshToken: strings.TrimSpace(refreshToken),
		IDToken:      strings.TrimSpace(idToken),
		TokenType:    strings.TrimSpace(tokenType),
		ExpiresIn:    expiresIn,
		Email:        strings.TrimSpace(email),
		Subject:      strings.TrimSpace(subject),
	}
	if expiresIn > 0 {
		tokenData.Expire = time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	return tokenData
}

func parseJWTIdentity(token string) (email string, subject string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload := parts[1]
	payload += strings.Repeat("=", (4-len(payload)%4)%4)
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return "", ""
	}
	var claims map[string]any
	if err = json.Unmarshal(raw, &claims); err != nil {
		return "", ""
	}
	if v, ok := claims["email"].(string); ok {
		email = strings.TrimSpace(v)
	}
	if v, ok := claims["sub"].(string); ok {
		subject = strings.TrimSpace(v)
	}
	return email, subject
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
