package management

import (
	"context"
	"testing"
	"time"

	xaiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/xai"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestResolveXAIAccessTokenDoesNotUseIDToken(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "xai",
		Metadata: map[string]any{"id_token": "identity-only"},
		Storage:  &xaiauth.TokenStorage{IDToken: "identity-from-storage"},
	}
	h := &Handler{}
	token, err := h.resolveTokenForAuth(context.Background(), nil, auth)
	if err != nil || token != "" {
		t.Fatalf("XAI token = %q, error = %v; want no access credential", token, err)
	}
	_, _, err = substituteAPICallToken(map[string]string{"Authorization": "Bearer $TOKEN$"}, "", func() (string, error) {
		return h.resolveTokenForAuth(context.Background(), nil, auth)
	})
	if err == nil {
		t.Fatal("ID-only credential must not be sent as an XAI API token")
	}
}

func TestResolveXAIAccessTokenUsesValidStorageWithoutRefreshing(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	auth := &coreauth.Auth{
		Provider: "xai",
		Storage: &xaiauth.TokenStorage{
			AccessToken: "stored-access-token", RefreshToken: "stored-refresh-token",
			Expire: future, TokenEndpoint: "https://127.0.0.1/oauth/token",
		},
	}
	token, err := (&Handler{}).resolveTokenForAuth(t.Context(), nil, auth)
	if err != nil || token != "stored-access-token" {
		t.Fatalf("valid storage token = %q, error = %v", token, err)
	}
}

func TestResolveXAIAccessTokenWithoutExpiryDoesNotRefresh(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "xai",
		Storage: &xaiauth.TokenStorage{
			AccessToken: "opaque-access-token", RefreshToken: "stored-refresh-token",
			TokenEndpoint: "https://127.0.0.1/oauth/token",
		},
	}
	token, err := (&Handler{}).resolveTokenForAuth(t.Context(), nil, auth)
	if err != nil || token != "opaque-access-token" {
		t.Fatalf("opaque token=%q error=%v", token, err)
	}
}

func TestPersistRefreshedXAIAuthWithStorageOnlyRefreshToken(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	registered, err := manager.Register(t.Context(), &coreauth.Auth{
		ID: "xai-storage.json", FileName: "xai-storage.json", Provider: "xai",
		Metadata: map[string]any{"type": "xai"},
		Storage: &xaiauth.TokenStorage{AccessToken: "old-access", RefreshToken: "old-refresh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := registered.Clone()
	refreshed := registered.Clone()
	refreshed.Metadata["access_token"] = "new-access"
	refreshed.Metadata["refresh_token"] = "new-refresh"
	refreshed.Storage = &xaiauth.TokenStorage{AccessToken: "new-access", RefreshToken: "new-refresh"}
	h := &Handler{authManager: manager}
	if err = h.persistRefreshedAuth(t.Context(), before, refreshed); err != nil {
		t.Fatalf("persist refreshed XAI auth: %v", err)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current.Metadata["access_token"] != "new-access" {
		t.Fatalf("refreshed auth not published: %#v", current)
	}
	storage, ok := current.Storage.(*xaiauth.TokenStorage)
	if !ok || storage.AccessToken != "new-access" || storage.RefreshToken != "new-refresh" {
		t.Fatalf("refreshed storage not published: %#v", current.Storage)
	}
}

func TestPersistRefreshedXAIAuthRejectsStaleStorage(t *testing.T) {
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	registered, err := manager.Register(t.Context(), &coreauth.Auth{
		ID: "xai-concurrent.json", Provider: "xai",
		Storage: &xaiauth.TokenStorage{AccessToken: "old-access", RefreshToken: "same-refresh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := registered.Clone()
	refreshed := registered.Clone()
	refreshed.Storage = &xaiauth.TokenStorage{AccessToken: "older-result", RefreshToken: "same-refresh"}
	current := registered.Clone()
	current.Storage = &xaiauth.TokenStorage{AccessToken: "newer-result", RefreshToken: "same-refresh"}
	if _, err = manager.Update(t.Context(), current); err != nil {
		t.Fatal(err)
	}
	if err = (&Handler{authManager: manager}).persistRefreshedAuth(t.Context(), before, refreshed); err == nil {
		t.Fatal("stale refresh overwrote a newer storage result")
	}
	kept, _ := manager.GetByID(registered.ID)
	if storage := kept.Storage.(*xaiauth.TokenStorage); storage.AccessToken != "newer-result" {
		t.Fatalf("newer result was replaced: %#v", storage)
	}
}

func TestResolveXAIAccessTokenSources(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth *coreauth.Auth
		want string
	}{
		{"api key", &coreauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "key"}, Metadata: map[string]any{"id_token": "identity"}}, "key"},
		{"metadata access token", &coreauth.Auth{Provider: "xai", Metadata: map[string]any{"access_token": "access", "id_token": "identity"}}, "access"},
		{"storage access token", &coreauth.Auth{Provider: "xai", Storage: &xaiauth.TokenStorage{AccessToken: "storage", IDToken: "identity"}}, "storage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := (&Handler{}).resolveTokenForAuth(context.Background(), nil, tc.auth)
			if err != nil || token != tc.want {
				t.Fatalf("XAI token = %q, error = %v; want %q", token, err, tc.want)
			}
		})
	}
}
