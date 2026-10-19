package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	xaiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/xai"
	sdkAuth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManagementAuthPublicationXAIJWTExpiry(t *testing.T) {
	now := time.Now().UTC()
	jwt := func(exp time.Time) string {
		claims, err := json.Marshal(map[string]any{"exp": exp.Unix()})
		if err != nil {
			t.Fatal(err)
		}
		return "fixture." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
	}
	for _, tc := range []struct {
		name, token string
		metadata    map[string]any
		storage     bool
		refresh     bool
	}{
		{name: "expired JWT without expiry fields", token: jwt(now.Add(-time.Hour)), refresh: true},
		{name: "JWT within refresh lead", token: jwt(now.Add(xaiauth.RefreshLead() / 2)), refresh: true},
		{name: "expired JWT overrides future metadata", token: jwt(now.Add(-time.Hour)), metadata: map[string]any{"expired": now.Add(time.Hour).Format(time.RFC3339)}, refresh: true},
		{name: "future JWT overrides expired metadata", token: jwt(now.Add(time.Hour)), metadata: map[string]any{"expired": now.Add(-time.Hour).Format(time.RFC3339)}},
		{name: "storage JWT without expiry fields", token: jwt(now.Add(-time.Hour)), storage: true, refresh: true},
		{name: "opaque token uses relative expiry", token: "opaque", metadata: map[string]any{"expires_in": int64(3600), "timestamp": now.Add(-2 * time.Hour).UnixMilli()}, refresh: true},
		{name: "opaque token without expiry", token: "opaque"},
		{name: "malformed JWT uses expiry metadata", token: "fixture.not-json.fixture", metadata: map[string]any{"expired": now.Add(-time.Hour).Format(time.RFC3339)}, refresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := cloneMap(tc.metadata)
			if metadata == nil {
				metadata = make(map[string]any)
			}
			metadata["type"] = "xai"
			metadata["refresh_token"] = "fixture-refresh"
			metadata["token_endpoint"] = "http://127.0.0.1/oauth/token"
			auth := &coreauth.Auth{Provider: "xai", Metadata: metadata}
			if tc.storage {
				auth.Storage = &xaiauth.TokenStorage{AccessToken: tc.token}
			} else {
				metadata["access_token"] = tc.token
				raw, err := json.Marshal(metadata)
				if err != nil {
					t.Fatal(err)
				}
				auth, _, err = authFromFile(t.TempDir(), filepath.Join(t.TempDir(), "xai-fixture.json"), raw)
				if err != nil {
					t.Fatal(err)
				}
			}
			token, err := (&Handler{}).resolveTokenForAuthWithProxy(t.Context(), nil, auth, "direct")
			if tc.refresh {
				if token != "" || err == nil || !strings.Contains(err.Error(), "must use https") {
					t.Fatalf("refresh was skipped: token=%q error=%v", token, err)
				}
			} else if token != tc.token || err != nil {
				t.Fatalf("valid token was refreshed: token=%q error=%v", token, err)
			}
		})
	}
}

func TestManagementAuthPublicationIdenticalRotation(t *testing.T) {
	for _, withStorage := range []bool{false, true} {
		name := "metadata"
		if withStorage {
			name = "storage"
		}
		t.Run(name, func(t *testing.T) {
			store := &memoryAuthStore{}
			manager := coreauth.NewManager(store, nil, nil)
			now := time.Now().UTC()
			auth := &coreauth.Auth{ID: "xai-shared.json", FileName: "xai-shared.json", Provider: "xai", Metadata: map[string]any{
				"type": "xai", "access_token": "old-access", "refresh_token": "old-refresh", "sub": "fixture-account", "expired": now.Add(-time.Hour).Format(time.RFC3339),
			}}
			if withStorage {
				auth.Storage = &xaiauth.TokenStorage{AccessToken: "old-access", RefreshToken: "old-refresh", Subject: "fixture-account"}
			}
			before, err := manager.Register(t.Context(), auth)
			if err != nil {
				t.Fatal(err)
			}
			refreshed := before.Clone()
			refreshed.Metadata["access_token"] = "new-access"
			refreshed.Metadata["refresh_token"] = "new-refresh"
			refreshed.Metadata["expired"] = now.Add(time.Hour).Format(time.RFC3339)
			refreshed.Metadata["timestamp"] = now.UnixMilli()
			refreshed.Metadata["last_refresh"] = now.Format(time.RFC3339)
			refreshed.LastRefreshedAt = now
			if withStorage {
				refreshed.Storage = &xaiauth.TokenStorage{AccessToken: "new-access", RefreshToken: "new-refresh", Subject: "fixture-account", Expire: now.Add(time.Hour).Format(time.RFC3339), LastRefresh: now.Format(time.RFC3339)}
			}
			h := &Handler{authManager: manager}
			if err := h.persistRefreshedAuth(t.Context(), before, refreshed); err != nil {
				t.Fatal(err)
			}
			concurrent, _ := manager.GetByID(before.ID)
			concurrent.Label = "edited label"
			concurrent.Disabled = true
			concurrent.ProxyURL = "direct"
			concurrent.Metadata["note"] = "concurrent note"
			if concurrent.Attributes == nil {
				concurrent.Attributes = make(map[string]string)
			}
			concurrent.Attributes["note"] = "concurrent attribute"
			published, err := manager.Update(t.Context(), concurrent)
			if err != nil {
				t.Fatal(err)
			}
			second := refreshed.Clone()
			second.Metadata["timestamp"] = now.Add(time.Minute).UnixMilli()
			second.Metadata["last_refresh"] = now.Add(time.Minute).Format(time.RFC3339)
			second.Metadata["expired"] = now.Add(2 * time.Hour).Format(time.RFC3339)
			second.LastRefreshedAt = now.Add(time.Minute)
			if withStorage {
				copyStorage := *second.Storage.(*xaiauth.TokenStorage)
				copyStorage.LastRefresh = now.Add(time.Minute).Format(time.RFC3339)
				copyStorage.Expire = now.Add(2 * time.Hour).Format(time.RFC3339)
				second.Storage = &copyStorage
			}
			if err := h.persistRefreshedAuth(t.Context(), before, second); err != nil {
				t.Fatalf("same rotation rejected: %v", err)
			}
			kept, _ := manager.GetByID(before.ID)
			if !reflect.DeepEqual(kept, published) {
				t.Fatalf("duplicate changed published auth or extended its expiry: before=%#v after=%#v", published, kept)
			}
			store.mu.Lock()
			persisted := store.items[before.ID].Clone()
			store.mu.Unlock()
			if persisted.Label != "edited label" || persisted.Metadata["note"] != "concurrent note" || persisted.Metadata["expired"] != refreshed.Metadata["expired"] {
				t.Fatalf("duplicate changed persisted fields: %#v", persisted)
			}
		})
	}
}

func TestManagementAuthPublicationRejectsNewerIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*coreauth.Auth)
	}{
		{name: "different newer rotation", change: func(auth *coreauth.Auth) {
			auth.Metadata["access_token"] = "newer-access"
			auth.Metadata["refresh_token"] = "newer-refresh"
		}},
		{name: "newer access with unchanged refresh", change: func(auth *coreauth.Auth) { auth.Metadata["access_token"] = "newer-access" }},
		{name: "identity replaced with same tokens", change: func(auth *coreauth.Auth) { auth.Metadata["sub"] = "other-account" }},
		{name: "provider replaced", change: func(auth *coreauth.Auth) { auth.Provider = "codex" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &memoryAuthStore{}
			manager := coreauth.NewManager(store, nil, nil)
			before, err := manager.Register(t.Context(), &coreauth.Auth{ID: "xai-newer.json", Provider: "xai", Metadata: map[string]any{"access_token": "old-access", "refresh_token": "old-refresh", "sub": "fixture-account"}})
			if err != nil {
				t.Fatal(err)
			}
			refreshed := before.Clone()
			refreshed.Metadata["access_token"] = "stale-access"
			refreshed.Metadata["refresh_token"] = "stale-refresh"
			replacement := before.Clone()
			tc.change(replacement)
			published, err := manager.Update(t.Context(), replacement)
			if err != nil {
				t.Fatal(err)
			}
			if err := (&Handler{authManager: manager}).persistRefreshedAuth(t.Context(), before, refreshed); err == nil {
				t.Fatal("stale refresh accepted after identity or token replacement")
			}
			kept, _ := manager.GetByID(before.ID)
			if !reflect.DeepEqual(kept, published) {
				t.Fatal("rejected refresh changed manager state")
			}
		})
	}
}

func TestManagementAuthPublicationConcurrentSameRotation(t *testing.T) {
	manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
	before, err := manager.Register(t.Context(), &coreauth.Auth{ID: "xai-parallel.json", Provider: "xai", Metadata: map[string]any{"access_token": "old-access", "refresh_token": "old-refresh"}})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := range 2 {
		refreshed := before.Clone()
		refreshed.Metadata["access_token"] = "shared-access"
		refreshed.Metadata["refresh_token"] = "shared-refresh"
		refreshed.Metadata["timestamp"] = time.Now().UnixMilli() + int64(i)
		go func() {
			<-start
			results <- (&Handler{authManager: manager}).persistRefreshedAuth(context.Background(), before, refreshed)
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Errorf("concurrent shared result rejected: %v", err)
		}
	}
}

func TestManagementAuthPublicationCodexPlanSelection(t *testing.T) {
	for _, plan := range []string{"free", "pro"} {
		t.Run(plan, func(t *testing.T) {
			authDir := t.TempDir()
			store := sdkAuth.NewFileTokenStore()
			store.SetBaseDir(authDir)
			manager := coreauth.NewManager(nil, nil, nil)
			manager.RegisterExecutor(&publicationCodexExecutor{})
			h := &Handler{authManager: manager, tokenStore: store}
			state := "fixture-publication-" + plan
			registerOAuthSessionForAuthDir(state, "codex", "fixture-nonce", authDir)
			t.Cleanup(func() { cancelOAuthSession(state) })
			name := "codex-fixture-" + plan + ".json"
			path, err := h.saveOAuthTokenRecord(t.Context(), authDir, state, "codex", &coreauth.Auth{
				ID: name, FileName: name, Provider: "codex", Attributes: map[string]string{"plan_type": plan},
				Metadata: map[string]any{"type": "codex", "plan_type": plan, "access_token": "fixture-access", "refresh_token": "fixture-refresh"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Base(path) != name {
				t.Fatalf("OAuth file name changed: %q", path)
			}
			auths := manager.List()
			if len(auths) != 1 {
				t.Fatalf("published auth count=%d", len(auths))
			}
			if auths[0].Metadata["plan_type"] != plan || auths[0].Attributes["plan_type"] != plan {
				t.Errorf("plan lost at publication: metadata=%v attribute=%q", auths[0].Metadata["plan_type"], auths[0].Attributes["plan_type"])
			}
			selected, err := manager.SelectAuth(t.Context(), "codex", "", cliproxyexecutor.Options{})
			if err != nil || selected == nil {
				t.Fatalf("unfiltered credential unavailable: %v", err)
			}
			selected, err = manager.SelectAuth(t.Context(), "codex", "", cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.DisallowFreeAuthMetadataKey: true}})
			if plan == "free" {
				if selected != nil || err == nil {
					t.Fatalf("free credential selected despite request filter: selected=%v error=%v", selected, err)
				}
			} else if selected == nil || err != nil {
				t.Fatalf("paid credential incorrectly filtered: selected=%v error=%v", selected, err)
			}
		})
	}
}

func TestSaveTokenRecordSettingsPersistence(t *testing.T) {
	previousSessions := oauthSessions
	oauthSessions = newOAuthSessionStore(time.Minute)
	t.Cleanup(func() { oauthSessions = previousSessions })
	const initial = `{"type":"codex","access_token":"fixture-old","priority":42,"disabled":true,"disable-cooling":true}`
	for _, tc := range []struct {
		name         string
		oauth        bool
		manager      bool
		existing     string
		metadata     map[string]any
		wantPriority any
		wantDisabled bool
		wantError    bool
	}{
		{name: "login without runtime", oauth: true, existing: initial, wantPriority: float64(42), wantDisabled: true},
		{name: "login with runtime", oauth: true, manager: true, existing: initial, wantPriority: float64(42), wantDisabled: true},
		{name: "explicit login settings", oauth: true, existing: initial, metadata: map[string]any{"priority": float64(0), "disabled": false, "disable_cooling": false}, wantPriority: float64(0)},
		{name: "direct save clears settings", existing: initial},
		{name: "direct save clears runtime settings", manager: true, existing: initial},
		{name: "invalid existing JSON", oauth: true, existing: "{", wantError: true},
		{name: "invalid existing JSON with runtime", oauth: true, manager: true, existing: "{", wantError: true},
		{name: "new disabled login", oauth: true, metadata: map[string]any{"disabled": true}, wantDisabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authDir := t.TempDir()
			const name = "codex-settings.json"
			path := filepath.Join(authDir, name)
			if tc.existing != "" {
				if err := os.WriteFile(path, []byte(tc.existing), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			store := sdkAuth.NewFileTokenStore()
			store.SetBaseDir(authDir)
			h := &Handler{tokenStore: store}
			if tc.manager {
				h.authManager = coreauth.NewManager(nil, nil, nil)
			}
			metadata := cloneMap(tc.metadata)
			if metadata == nil {
				metadata = make(map[string]any)
			}
			metadata["type"] = "codex"
			metadata["access_token"] = "fixture-new"
			disabled, _ := metadata["disabled"].(bool)
			record := &coreauth.Auth{ID: name, FileName: name, Provider: "codex", Metadata: metadata, Disabled: disabled}
			var err error
			if tc.oauth {
				const state = "fixture-settings-login"
				registerOAuthSessionForAuthDir(state, "codex", "fixture-nonce", authDir)
				_, err = h.saveOAuthTokenRecord(t.Context(), authDir, state, "codex", record)
			} else {
				_, err = h.saveTokenRecord(t.Context(), authDir, record)
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("save error = %v, want error %v", err, tc.wantError)
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.wantError {
				if string(data) != tc.existing {
					t.Fatal("failed login changed the existing file")
				}
				if h.authManager != nil && len(h.authManager.List()) != 0 {
					t.Fatal("failed login published a credential")
				}
				return
			}
			var saved map[string]any
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved["access_token"] != "fixture-new" || saved["priority"] != tc.wantPriority || saved["disabled"] != tc.wantDisabled {
				t.Fatalf("unexpected saved settings: %#v", saved)
			}
			if !tc.oauth && (saved["disable_cooling"] != nil || saved["disable-cooling"] != nil) {
				t.Fatal("direct save restored cleared cooling settings")
			}
			if tc.metadata != nil && saved["disable_cooling"] != tc.metadata["disable_cooling"] {
				t.Fatalf("explicit cooling setting changed: %#v", saved)
			}
			if h.authManager != nil {
				current, ok := h.authManager.GetByID(name)
				if !ok || current.Metadata["priority"] != tc.wantPriority || current.Disabled != tc.wantDisabled || current.Metadata["access_token"] != "fixture-new" {
					t.Fatalf("unexpected published settings: %#v", current)
				}
			}
		})
	}
}

type publicationCodexExecutor struct{ safeRefreshExecutor }

func (*publicationCodexExecutor) Identifier() string { return "codex" }
