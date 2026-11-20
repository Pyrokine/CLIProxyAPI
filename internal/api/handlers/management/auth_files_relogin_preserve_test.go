package management

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestSaveTokenRecord_PostPersistHookReceivesCanonicalClaudeOAuth(t *testing.T) {
	for _, mode := range []string{"without_manager", "with_manager"} {
		t.Run(
			mode, func(t *testing.T) {
				dir := t.TempDir()
				store := sdkAuth.NewFileTokenStore()
				store.SetBaseDir(dir)
				var manager *coreauth.Manager
				if mode == "with_manager" {
					manager = coreauth.NewManager(store, nil, nil)
				}
				h := NewHandler(&config.Config{AuthDir: dir}, "", manager, withTokenStore(store))
				t.Cleanup(func() { _ = h.Close() })
				var persisted *coreauth.Auth
				h.SetPostAuthPersistHook(
					func(_ context.Context, auth *coreauth.Auth) error {
						persisted = auth.Clone()
						return nil
					},
				)
				storage := &claude.TokenStorage{
					AccessToken: "sk-ant-oat01-hot-reload", RefreshToken: "refresh-token",
					LastRefresh: "2026-03-09T00:00:00Z", Email: "user@example.com", Expire: "2026-12-31T23:59:59Z",
				}
				const name = "claude-user@example.com.json"
				record := &coreauth.Auth{
					ID: name, FileName: name, Provider: "claude", Storage: storage,
					Metadata: map[string]any{"email": storage.Email},
				}
				path, err := h.saveTokenRecord(coreauth.WithAuthCreationIntent(t.Context()), dir, record)
				if err != nil {
					t.Fatal(err)
				}
				if persisted == nil || persisted.Metadata["access_token"] != storage.AccessToken ||
					persisted.AuthKind() != coreauth.AuthKindOAuth ||
					persisted.Status != coreauth.StatusActive {
					t.Fatalf("post-persist canonical auth=%+v", persisted)
				}
				if path != filepath.Join(
					dir, name,
				) ||
					persisted.ID !=
						name ||
					persisted.FileName !=
						name ||
					persisted.Attributes[coreauth.AttributePath] !=
						path {
					t.Fatalf("canonical identity=%+v path=%q", persisted, path)
				}
			},
		)
	}
}

func TestSaveTokenRecord_PreservesExistingAuthFileSettings(t *testing.T) {
	dir := t.TempDir()
	const name = "codex-user@example.com.json"
	path := filepath.Join(dir, name)
	initial := map[string]any{
		"type": "codex", "email": "user@example.com", "access_token": "old-access", "refresh_token": "old-refresh",
		"prefix": "custom-prefix", "websockets": false, "note": "my important account",
		"proxy_url": "http://127.0.0.1:8080",
		"weight":    float64(5), "headers": map[string]any{"User-Agent": "Custom"}, "models": []any{"o3-mini"},
		"thinking": map[string]any{"enabled": true}, "priority": float64(2),
	}
	raw, err := json.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	h := NewHandler(&config.Config{AuthDir: dir}, "", nil, withTokenStore(store))
	t.Cleanup(func() { _ = h.Close() })
	storage := &codex.TokenStorage{
		Type: "codex", Email: "user@example.com", AccessToken: "new-access-token", RefreshToken: "new-refresh-token",
		IDToken: "new-id-token", AccountID: "act-123", Expire: "2026-12-31T23:59:59Z",
	}
	record := &coreauth.Auth{
		ID: name, FileName: name, Provider: "codex", Storage: storage,
		Metadata: map[string]any{"email": storage.Email, "account_id": storage.AccountID},
	}
	savedPath, err := h.saveTokenRecord(coreauth.WithAuthCreationIntent(t.Context()), dir, record)
	if err != nil || savedPath != path {
		t.Fatalf("saved path=%q error=%v", savedPath, err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["access_token"] != storage.AccessToken || saved["refresh_token"] != storage.RefreshToken {
		t.Fatalf("new OAuth tokens were not persisted: %#v", saved)
	}
	for _, key := range []string{
		"prefix", "websockets", "note", "proxy_url", "weight", "headers", "models", "thinking", "priority",
	} {
		if !reflect.DeepEqual(saved[key], initial[key]) {
			t.Errorf("%s=%#v want %#v", key, saved[key], initial[key])
		}
	}
}

func TestSaveTokenRecord_MigratesMatchingLegacyClaudeCredential(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "claude-user@example.com.json")
	name := claude.CredentialFileName("user@example.com", "organization-a", "account-a")
	target := filepath.Join(dir, name)
	existing := map[string]any{
		"type": "claude", "email": "user@example.com", "organization_uuid": "organization-a",
		"account_uuid": "account-a",
		"access_token": "old-token", "refresh_token": "old-refresh", "prefix": "team",
		"proxy_url": "http://127.0.0.1:8080",
		"disabled":  true, "weight": float64(5),
	}
	raw, err := json.Marshal(existing)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	storage := &claude.TokenStorage{
		AccessToken: "new-token", RefreshToken: "new-refresh", Email: "user@example.com",
		OrganizationUUID: "organization-a", AccountUUID: "account-a", Expire: "2026-12-31T23:59:59Z",
	}
	record := &coreauth.Auth{
		ID: name, FileName: name, Provider: "claude", Storage: storage,
		Metadata: map[string]any{
			"email": storage.Email, "organization_uuid": storage.OrganizationUUID, "account_uuid": storage.AccountUUID,
		},
	}
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	h := NewHandler(&config.Config{AuthDir: dir}, "", nil, withTokenStore(store))
	t.Cleanup(func() { _ = h.Close() })
	savedPath, err := h.saveTokenRecord(coreauth.WithAuthCreationIntent(t.Context()), dir, record)
	if err != nil || savedPath != target {
		t.Fatalf("saved path=%q error=%v", savedPath, err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy credential remains: %v", err)
	}
	raw, err = os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"access_token": "new-token", "refresh_token": "new-refresh", "prefix": "team",
		"proxy_url": "http://127.0.0.1:8080", "disabled": true, "weight": float64(5),
	} {
		if saved[key] != want {
			t.Errorf("%s=%#v want %#v", key, saved[key], want)
		}
	}
}

func TestPatchAuthFileFields_DeletesPluginFields(t *testing.T) {
	dir := t.TempDir()
	const name = "plugin-auth.json"
	path := filepath.Join(dir, name)
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	if _, err := manager.Register(
		t.Context(), &coreauth.Auth{
			ID: name, FileName: name, Provider: "demo-plugin",
			Metadata: map[string]any{
				"type": "demo-plugin", "token": "tok-123", "weight": float64(10),
				"headers": map[string]any{"X-Header": "val"},
			},
		},
	); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, manager, withTokenStore(store))
	t.Cleanup(func() { _ = h.Close() })
	ctx, rec := newArtifactCacheTestContext(
		http.MethodPatch, "/v0/management/auth-files/fields", `{"name":"plugin-auth.json","weight":null}`,
	)
	h.PatchAuthFileFields(ctx)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status=%d body=%s", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if _, present := saved["weight"]; present || saved["token"] != "tok-123" {
		t.Fatalf("cleared plugin fields=%#v", saved)
	}
}
