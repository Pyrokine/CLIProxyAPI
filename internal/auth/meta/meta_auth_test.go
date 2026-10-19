package meta

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMetaDeviceFlowMintsAPIKeyOnServer(t *testing.T) {
	dcaToken := "dca:server-only-marker"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/device":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"device_code":"device-code","user_code":"CODE","verification_uri":"https://www.meta.com/device","expires_in":30,"interval":1}`))
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"dca:server-only-marker","expires_in":3600}`))
		case "/mint":
			if r.Header.Get("Authorization") != "Bearer "+dcaToken {
				t.Error("Meta DCA token was not sent in server-side Authorization")
			}
			var payload struct{ DCAToken string `json:"dca_token"` }
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.DCAToken != dcaToken {
				t.Errorf("Meta server-side DCA exchange failed: %v", err)
			}
			_, _ = w.Write([]byte(`{"api_key":"meta-test-key","base_url":"https://api.meta.ai/v1","subs_tier_name":"Muse Pro","is_subs_active":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	auth := NewMetaAuth(nil)
	auth.SetMintURL(server.URL + "/mint")
	device, err := auth.StartDeviceFlowWithEndpoint(context.Background(), server.URL+"/device")
	if err != nil {
		t.Fatalf("start Meta device flow: %v", err)
	}
	device.TokenEndpoint = server.URL + "/token"
	bundle, err := auth.WaitForAuthorization(context.Background(), device)
	if err != nil {
		t.Fatalf("exchange Meta device code: %v", err)
	}
	storage := auth.CreateTokenStorage(bundle)
	if storage == nil || storage.AccessToken != "meta-test-key" || storage.APIKey != "meta-test-key" ||
		storage.DCAToken != dcaToken || storage.Expired != "" || storage.DCAExpiresAt == 0 {
		t.Fatalf("Meta token storage did not separate DCA and API key: %#v", storage)
	}
}

func TestMetaMintErrorNeverIncludesProviderSecret(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"secret@example.com","token":"dca:provider-secret"}`))
	}))
	defer server.Close()
	auth := NewMetaAuth(nil)
	auth.SetMintURL(server.URL)
	_, err := auth.MintAPIKey(context.Background(), "dca:provider-secret")
	if err == nil || !strings.Contains(err.Error(), "401") ||
		strings.Contains(err.Error(), "secret@example.com") || strings.Contains(err.Error(), "dca:provider-secret") {
		t.Fatalf("Meta mint error disclosed provider response: %v", err)
	}
}

func TestMetaStorageRejectsStaleCredentialMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta-test.json")
	storage := &MetaTokenStorage{
		AccessToken: "current-api-key",
		DCAToken:    "dca:current-token",
		APIKey:      "current-api-key",
		Metadata: map[string]any{
			"access_token": "stale-api-key", "api_key": "stale-api-key", "dca_token": "dca:stale-token",
			"priority": 5, "subs_tier_name": "Muse Pro",
		},
	}
	if err := storage.SaveTokenToFile(path); err != nil {
		t.Fatalf("save Meta credential: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read Meta credential: %v", err)
	}
	if strings.Contains(string(data), "stale-") || !strings.Contains(string(data), "current-api-key") ||
		!strings.Contains(string(data), `"priority":5`) {
		t.Fatalf("Meta credential save lost current token or settings")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("Meta credential file permission = %v, err=%v", info.Mode(), err)
	}
}
