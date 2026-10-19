package kimi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestKimiServicesUseDistinctOAuthHostsAndCredentialMetadata(t *testing.T) {
	for _, tc := range []struct {
		domain   string
		provider string
		host     string
		baseURL  string
	}{
		{domain: KimiDefaultDomain, provider: "kimi", host: "auth.kimi.com", baseURL: APIBaseURL},
		{domain: KimiAIDomain, provider: "kimi-ai", host: "auth.kimi.ai", baseURL: KimiAIAPIBaseURL},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			client := NewDeviceFlowClientWithDomainAndDeviceID(nil, tc.domain, "device-1")
			var paths []string
			client.httpClient = &http.Client{Transport: kimiRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != tc.host {
					t.Errorf("OAuth host = %q, want %q", req.URL.Host, tc.host)
				}
				paths = append(paths, req.URL.Path)
				body := `{"access_token":"access-key","refresh_token":"refresh-key","expires_in":3600}`
				if strings.HasSuffix(req.URL.Path, "/device_authorization") {
					body = `{"device_code":"device-code","user_code":"CODE","verification_uri":"https://verify.example/device","interval":5}`
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
			})}
			device, err := client.requestDeviceCode(context.Background())
			if err != nil || device.DeviceCode != "device-code" {
				t.Fatalf("request device code: %v, %+v", err, device)
			}
			token, err, retry := client.exchangeDeviceCode(context.Background(), device.DeviceCode)
			if err != nil || retry || token.AccessToken != "access-key" {
				t.Fatalf("exchange device code: %v, %v, %+v", err, retry, token)
			}
			if _, err := client.RefreshToken(context.Background(), "refresh-key"); err != nil {
				t.Fatalf("refresh token: %v", err)
			}
			if len(paths) != 3 || paths[0] != "/api/oauth/device_authorization" ||
				paths[1] != "/api/oauth/token" || paths[2] != "/api/oauth/token" {
				t.Fatalf("OAuth paths = %q", paths)
			}
			svc := NewAuthWithDomain(nil, tc.domain)
			storage := svc.CreateTokenStorage(&AuthBundle{TokenData: token, DeviceID: "device-1"})
			if storage.Type != tc.provider || storage.Domain != tc.domain || storage.BaseURL != tc.baseURL {
				t.Fatalf("credential classification = %+v", storage)
			}
			path := filepath.Join(t.TempDir(), tc.provider+".json")
			storage.SetMetadata(map[string]any{
				"type": "kimi", "domain": KimiDefaultDomain, "base_url": APIBaseURL,
				"access_token": "stale-key", "priority": float64(7),
			})
			if err := storage.SaveTokenToFile(path); err != nil {
				t.Fatalf("save Kimi token: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read Kimi token: %v", err)
			}
			var saved map[string]any
			if err = json.Unmarshal(raw, &saved); err != nil {
				t.Fatalf("decode Kimi token: %v", err)
			}
			if saved["type"] != tc.provider || saved["domain"] != tc.domain || saved["base_url"] != tc.baseURL ||
				saved["access_token"] != "access-key" || saved["priority"] != float64(7) {
				t.Fatalf("saved Kimi service metadata differs: %+v", saved)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("credential file permissions = %v, err=%v", info, err)
			}
		})
	}
}

func TestResolveKimiDomainFromAuthPrefersExplicitService(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth *coreauth.Auth
		want string
	}{
		{name: "default", auth: nil, want: KimiDefaultDomain},
		{name: "provider", auth: &coreauth.Auth{Provider: "kimi-ai"}, want: KimiAIDomain},
		{name: "metadata", auth: &coreauth.Auth{Provider: "kimi", Metadata: map[string]any{"type": "kimi-ai"}}, want: KimiAIDomain},
		{name: "attributes", auth: &coreauth.Auth{Provider: "kimi", Attributes: map[string]string{"domain": "kimi.ai"}}, want: KimiAIDomain},
		{name: "base URL", auth: &coreauth.Auth{Provider: "kimi", Attributes: map[string]string{"base_url": KimiAIAPIBaseURL}}, want: KimiAIDomain},
		{name: "storage", auth: &coreauth.Auth{Storage: &TokenStorage{Domain: KimiAIDomain}}, want: KimiAIDomain},
		{name: "filename", auth: &coreauth.Auth{FileName: "kimi-ai-account.json"}, want: KimiAIDomain},
		{name: "explicit old domain wins", auth: &coreauth.Auth{Provider: "kimi-ai", Attributes: map[string]string{"domain": KimiDefaultDomain}}, want: KimiDefaultDomain},
		{name: "lookalike host", auth: &coreauth.Auth{Provider: "kimi", Attributes: map[string]string{"base_url": "https://api.kimi.ai.bad.example/coding"}}, want: KimiDefaultDomain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveKimiDomainFromAuth(tc.auth); got != tc.want || IsKimiAIAuth(tc.auth) != (tc.want == KimiAIDomain) {
				t.Fatalf("resolved service = %q, isAI = %v; want %q", got, IsKimiAIAuth(tc.auth), tc.want)
			}
		})
	}
}

func TestKimiOAuthErrorDoesNotIncludeProviderBody(t *testing.T) {
	client := NewDeviceFlowClientWithDomainAndDeviceID(nil, KimiAIDomain, "device-1")
	client.httpClient = &http.Client{Transport: kimiRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader(`{"error":"secret@example.com","error_description":"token=private"}`)),
			Header:     make(http.Header), Request: req,
		}, nil
	})}
	if _, err := client.requestDeviceCode(context.Background()); err == nil ||
		strings.Contains(err.Error(), "secret@example.com") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("device code error included sensitive provider body: %v", err)
	}
	if _, err, retry := client.exchangeDeviceCode(context.Background(), "device-code"); err == nil || retry ||
		strings.Contains(err.Error(), "secret@example.com") || strings.Contains(err.Error(), "private") {
		t.Fatalf("token exchange error included sensitive provider body: %v", err)
	}
	if _, err := client.RefreshToken(context.Background(), "refresh-key"); err == nil ||
		strings.Contains(err.Error(), "secret@example.com") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("refresh error included sensitive provider body: %v", err)
	}
}
