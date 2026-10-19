// Package meta implements Meta Muse device authorization and API key minting.
package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
)

const (
	DefaultAPIBaseURL           = "https://api.meta.ai/v1"
	DeviceAuthorizationEndpoint = "https://auth.meta.com/oidc/device/authorization/"
	TokenEndpoint               = "https://auth.meta.com/oidc/device/token/"
	ClientID                    = "1031625952748946"
	MaxPollDuration             = 15 * time.Minute
	metaMintEndpoint            = "https://api.meta.ai/muse-code/key"
	metaDeviceGrantType         = "urn:ietf:params:oauth:grant-type:device_code"
	metaMaxResponseBytes        = 1 << 20
)

type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	TokenEndpoint           string `json:"-"`
}

type TokenData struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	ExpiresAt   int64  `json:"expires_at"`
}

type MintedKeyResponse struct {
	APIKey           string `json:"api_key"`
	BaseURL          string `json:"base_url"`
	UserEmail        string `json:"user_email"`
	UserFullName     string `json:"user_full_name"`
	SubsTierName     string `json:"subs_tier_name"`
	SubsTierID       string `json:"subs_tier_id"`
	IsSubsActive     bool   `json:"is_subs_active"`
	HasPaymentMethod bool   `json:"has_payment_method"`
}

type MetaAuthBundle struct {
	TokenData *TokenData
	MintedKey *MintedKeyResponse
}

// MetaAuth owns the device flow and keeps the DCA token on the server.
type MetaAuth struct {
	httpClient *http.Client
	mintURL    string
}

func NewMetaAuth(cfg *config.Config) *MetaAuth {
	return NewMetaAuthWithProxyURL(cfg, "")
}

func NewMetaAuthWithProxyURL(cfg *config.Config, proxyURL string) *MetaAuth {
	client := &http.Client{Timeout: 30 * time.Second}
	proxyCfg := &config.SDKConfig{}
	if cfg != nil {
		*proxyCfg = cfg.SDKConfig
	}
	if strings.TrimSpace(proxyURL) != "" {
		proxyCfg.ProxyURL = proxyURL
	}
	util.SetProxy(proxyCfg, client)
	return &MetaAuth{httpClient: client, mintURL: metaMintEndpoint}
}

func (auth *MetaAuth) SetMintURL(mintURL string) {
	auth.mintURL = mintURL
}

func (auth *MetaAuth) SetHTTPClient(client *http.Client) {
	auth.httpClient = client
}

func readMetaResponse(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, metaMaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > metaMaxResponseBytes {
		return nil, fmt.Errorf("meta response exceeds size limit")
	}
	return data, nil
}

func (auth *MetaAuth) StartDeviceFlow(ctx context.Context) (*DeviceCodeResponse, error) {
	return auth.StartDeviceFlowWithEndpoint(ctx, DeviceAuthorizationEndpoint)
}

func (auth *MetaAuth) StartDeviceFlowWithEndpoint(ctx context.Context, endpoint string) (*DeviceCodeResponse, error) {
	if ctx == nil {
		return nil, fmt.Errorf("meta device authorization requires a context")
	}
	form := url.Values{"client_id": {ClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create Meta device request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "muse-code/1.0.2")
	resp, err := auth.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("meta device request: %w", err)
	}
	defer resp.Body.Close()
	body, err := readMetaResponse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read Meta device response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("meta device authorization rejected (HTTP %d)", resp.StatusCode)
	}
	var result DeviceCodeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse Meta device response: %w", err)
	}
	if strings.TrimSpace(result.DeviceCode) == "" || strings.TrimSpace(result.UserCode) == "" ||
		(strings.TrimSpace(result.VerificationURI) == "" && strings.TrimSpace(result.VerificationURIComplete) == "") {
		return nil, fmt.Errorf("meta device response is incomplete")
	}
	result.TokenEndpoint = TokenEndpoint
	return &result, nil
}

func (auth *MetaAuth) WaitForAuthorization(ctx context.Context, device *DeviceCodeResponse) (*MetaAuthBundle, error) {
	if ctx == nil || device == nil || strings.TrimSpace(device.DeviceCode) == "" {
		return nil, fmt.Errorf("meta authorization requires a device code and context")
	}
	interval := time.Duration(device.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	maxDuration := MaxPollDuration
	if device.ExpiresIn > 0 && time.Duration(device.ExpiresIn)*time.Second < maxDuration {
		maxDuration = time.Duration(device.ExpiresIn) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, maxDuration)
	defer cancel()
	tokenURL := device.TokenEndpoint
	if tokenURL == "" {
		tokenURL = TokenEndpoint
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("meta authorization ended: %w", ctx.Err())
		case <-ticker.C:
			form := url.Values{
				"client_id":   {ClientID},
				"device_code": {device.DeviceCode},
				"grant_type":  {metaDeviceGrantType},
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
			if err != nil {
				return nil, fmt.Errorf("create Meta token request: %w", err)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", "muse-code/1.0.2")
			resp, err := auth.httpClient.Do(req)
			if err != nil {
				return nil, fmt.Errorf("meta token polling failed: %w", err)
			}
			body, err := readMetaResponse(resp.Body)
			resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("read Meta token response: %w", err)
			}
			if resp.StatusCode == http.StatusOK {
				var token TokenData
				if err := json.Unmarshal(body, &token); err != nil {
					return nil, fmt.Errorf("parse Meta token response: %w", err)
				}
				if strings.TrimSpace(token.AccessToken) == "" {
					return nil, fmt.Errorf("meta token response omitted access token")
				}
				if token.ExpiresIn > 0 {
					token.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).Unix()
				}
				minted, err := auth.MintAPIKey(ctx, token.AccessToken)
				if err != nil {
					return nil, fmt.Errorf("meta device authorized, but API key mint failed: %w", err)
				}
				return &MetaAuthBundle{TokenData: &token, MintedKey: minted}, nil
			}
			var providerError struct {
				Code string `json:"error"`
			}
			if err := json.Unmarshal(body, &providerError); err != nil {
				return nil, fmt.Errorf("meta token request rejected (HTTP %d)", resp.StatusCode)
			}
			switch providerError.Code {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				ticker.Reset(interval)
			case "access_denied":
				return nil, fmt.Errorf("meta device authorization denied")
			case "expired_token":
				return nil, fmt.Errorf("meta device authorization expired")
			default:
				return nil, fmt.Errorf("meta token request rejected (HTTP %d)", resp.StatusCode)
			}
		}
	}
}

func (auth *MetaAuth) MintAPIKey(ctx context.Context, dcaToken string) (*MintedKeyResponse, error) {
	if ctx == nil || strings.TrimSpace(dcaToken) == "" {
		return nil, fmt.Errorf("meta API key mint requires a DCA token and context")
	}
	body, err := json.Marshal(struct {
		DCAToken string `json:"dca_token"`
	}{DCAToken: strings.TrimSpace(dcaToken)})
	if err != nil {
		return nil, fmt.Errorf("encode Meta key request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.mintURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Meta key request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "muse-code/1.0.2")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(dcaToken))
	resp, err := auth.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("meta key request failed: %w", err)
	}
	defer resp.Body.Close()
	response, err := readMetaResponse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read Meta key response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("meta key request rejected (HTTP %d)", resp.StatusCode)
	}
	var minted MintedKeyResponse
	if err := json.Unmarshal(response, &minted); err != nil {
		return nil, fmt.Errorf("parse Meta key response: %w", err)
	}
	if strings.TrimSpace(minted.APIKey) == "" || strings.HasPrefix(strings.ToLower(strings.TrimSpace(minted.APIKey)), "dca:") {
		return nil, fmt.Errorf("meta key response did not contain a usable API key")
	}
	return &minted, nil
}
