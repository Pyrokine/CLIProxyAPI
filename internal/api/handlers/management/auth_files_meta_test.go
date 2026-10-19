package management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metaauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/meta"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestRequestMetaTokenPublishesCredential(t *testing.T) {
	previousSessions := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previousSessions })

	originalTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.URL.String() {
		case metaauth.DeviceAuthorizationEndpoint:
			body = `{"device_code":"fixture-device","user_code":"FIXTURE","verification_uri":"https://www.meta.com/device","expires_in":30,"interval":1}`
		case metaauth.TokenEndpoint:
			body = `{"access_token":"dca:fixture-only","expires_in":3600}`
		case "https://api.meta.ai/muse-code/key":
			if req.Header.Get("Authorization") != "Bearer dca:fixture-only" {
				return nil, errors.New("unexpected Meta mint authorization")
			}
			body = `{"api_key":"fixture-meta-key","base_url":"https://api.meta.ai/v1","user_email":"fixture@example.test","subs_tier_name":"Muse Pro","subs_tier_id":"pro","is_subs_active":true,"has_payment_method":true}`
		default:
			return nil, errors.New("unexpected Meta request")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	for _, tc := range []struct {
		name     string
		existing bool
		loaded   bool
	}{
		{name: "new"},
		{name: "existing file", existing: true},
		{name: "registered credential", existing: true, loaded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authDir := t.TempDir()
			fileName := metaauth.CredentialFileName("fixture@example.test", "dca:fixture-only")
			path := filepath.Join(authDir, fileName)
			store := sdkAuth.NewFileTokenStore()
			store.SetBaseDir(authDir)
			manager := coreauth.NewManager(nil, nil, nil)
			if tc.existing {
				initial := `{"type":"meta","email":"fixture@example.test","access_token":"fixture-old-key","dca_token":"dca:fixture-old","expired":"2020-01-01T00:00:00Z","priority":42,"disable-cooling":true,"models":["muse-latest"],"disabled":true,"prefix":"fixture-meta","proxy_url":"direct"}`
				if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
					t.Fatal(err)
				}
				if tc.loaded {
					existing, _, err := authFromFile(authDir, path, nil)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := manager.Register(t.Context(), existing); err != nil {
						t.Fatal(err)
					}
				}
			}
			handler := &Handler{cfg: &config.Config{AuthDir: authDir}, authManager: manager, tokenStore: store}

			response := performOAuthSessionRequest(t, http.MethodGet, "/meta-auth-url?nonce=fixture-nonce", handler.RequestMetaToken)
			if response.Code != http.StatusOK {
				t.Fatalf("request Meta token: status %d body %s", response.Code, response.Body.String())
			}
			payload := decodeOAuthSessionResponse(t, response)
			state, _ := payload["state"].(string)
			if !strings.HasPrefix(state, "meta-") || payload["flow"] != "device" || payload["user_code"] != "FIXTURE" {
				t.Fatalf("unexpected Meta device response: %#v", payload)
			}
			if status := performOAuthSessionRequest(t, http.MethodGet, "/auth-status?state="+state, handler.GetAuthStatus); status.Code != http.StatusUnauthorized {
				t.Fatalf("Meta session did not require nonce: %d", status.Code)
			}

			deadline := time.After(5 * time.Second)
			for {
				_, status, completed, ok := getOAuthSessionDetails(state)
				if !ok || completed || status != "" {
					if !ok || !completed || status != "" {
						t.Fatalf("Meta authorization did not complete: exists=%v completed=%v status=%q", ok, completed, status)
					}
					break
				}
				select {
				case <-deadline:
					cancelOAuthSession(state)
					t.Fatal("Meta authorization did not finish")
				case <-time.After(10 * time.Millisecond):
				}
			}
			status := performOAuthSessionRequest(t, http.MethodGet, "/auth-status?state="+state+"&nonce=fixture-nonce", handler.GetAuthStatus)
			if result := decodeOAuthSessionResponse(t, status); result["status"] != "ok" || result["phase"] != "success" {
				t.Fatalf("completed Meta session response: %#v", result)
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read saved Meta credential: %v", err)
			}
			var saved map[string]any
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatalf("decode saved Meta credential: %v", err)
			}
			if saved["access_token"] != "fixture-meta-key" || saved["dca_token"] != "dca:fixture-only" ||
				saved["subs_tier_name"] != "Muse Pro" || saved["subs_tier_id"] != "pro" ||
				saved["is_subs_active"] != true || saved["has_payment_method"] != true || saved["expired"] != nil {
				t.Fatalf("saved Meta credential fields do not match minted key: %#v", saved)
			}
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("Meta credential permission: info=%v err=%v", info, err)
			}
			current, ok := manager.GetByID(fileName)
			if !ok || current.Provider != "meta" || current.Metadata["api_key"] != "fixture-meta-key" ||
				current.Metadata["subs_tier_name"] != "Muse Pro" || current.Attributes[coreauth.AttributePath] != path {
				t.Fatalf("runtime Meta credential does not match saved file: %#v", current)
			}
			if tc.existing {
				for _, metadata := range []map[string]any{saved, current.Metadata} {
					models, _ := metadata["models"].([]any)
					if metadata["priority"] != float64(42) || metadata["disable_cooling"] != true ||
						metadata["disabled"] != true || metadata["prefix"] != "fixture-meta" || metadata["proxy_url"] != "direct" ||
						len(models) != 1 || models[0] != "muse-latest" {
						t.Errorf("re-login lost credential settings: %#v", metadata)
					}
				}
				if !current.Disabled || current.Status != coreauth.StatusDisabled || current.Prefix != "fixture-meta" || current.ProxyURL != "direct" {
					t.Errorf("re-login settings were not applied to runtime: %#v", current)
				}
			}
		})
	}
}

func TestRequestMetaTokenDeviceExpiry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		present   bool
		expiresIn int
		want      int
	}{
		{name: "missing", want: int(metaauth.MaxPollDuration / time.Second)},
		{name: "zero", present: true, want: int(metaauth.MaxPollDuration / time.Second)},
		{name: "negative", present: true, expiresIn: -1, want: int(metaauth.MaxPollDuration / time.Second)},
		{name: "provided", present: true, expiresIn: 30, want: 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousSessions := oauthSessions
			oauthSessions = newOAuthSessionStore(time.Minute)
			t.Cleanup(func() { oauthSessions = previousSessions })
			payload := map[string]any{
				"device_code": "fixture-device", "user_code": "FIXTURE",
				"verification_uri": "https://www.meta.com/device", "interval": 1,
			}
			if tc.present {
				payload["expires_in"] = tc.expiresIn
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			originalTransport := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.String() {
				case metaauth.DeviceAuthorizationEndpoint:
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
				case metaauth.TokenEndpoint:
					return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"access_denied"}`))}, nil
				default:
					return nil, errors.New("unexpected Meta expiry test request")
				}
			})
			t.Cleanup(func() { http.DefaultTransport = originalTransport })
			handler := &Handler{cfg: &config.Config{AuthDir: t.TempDir()}}
			response := performOAuthSessionRequest(t, http.MethodGet, "/meta-auth-url", handler.RequestMetaToken)
			if response.Code != http.StatusOK {
				t.Fatalf("Meta device response: %d, %s", response.Code, response.Body.String())
			}
			result := decodeOAuthSessionResponse(t, response)
			state, _ := result["state"].(string)
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, status, _, ok := getOAuthSessionDetails(state)
				if ok && status != "" {
					break
				}
				if time.Now().After(deadline) {
					cancelOAuthSession(state)
					t.Fatal("Meta expiry fixture did not finish authorization")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if result["expires_in"] != float64(tc.want) {
				t.Fatalf("Meta device expiry = %v, want %d", result["expires_in"], tc.want)
			}
		})
	}
}
