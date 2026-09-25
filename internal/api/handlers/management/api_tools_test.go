package management

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/antigravity"
)

type memoryAuthStore struct {
	mu    sync.Mutex
	items map[string]*coreauth.Auth
}

func (s *memoryAuthStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*coreauth.Auth, 0, len(s.items))
	for _, a := range s.items {
		out = append(out, a.Clone())
	}
	return out, nil
}

func (s *memoryAuthStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	_ = ctx
	if auth == nil {
		return "", nil
	}
	s.mu.Lock()
	if s.items == nil {
		s.items = make(map[string]*coreauth.Auth)
	}
	s.items[auth.ID] = auth.Clone()
	s.mu.Unlock()
	return auth.ID, nil
}

func (s *memoryAuthStore) Delete(ctx context.Context, id string) error {
	_ = ctx
	if filepath.IsAbs(id) {
		if err := os.Remove(id); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	s.mu.Lock()
	delete(s.items, id)
	s.mu.Unlock()
	return nil
}

func TestResolveTokenForAuth_Antigravity_RefreshesExpiredToken(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				callCount++
				if r.Method != http.MethodPost {
					t.Fatalf("expected POST, got %s", r.Method)
				}
				if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
					t.Fatalf("unexpected content-type: %s", ct)
				}
				bodyBytes, _ := io.ReadAll(r.Body)
				_ = r.Body.Close()
				values, err := url.ParseQuery(string(bodyBytes))
				if err != nil {
					t.Fatalf("parse form: %v", err)
				}
				if values.Get("grant_type") != "refresh_token" {
					t.Fatalf("unexpected grant_type: %s", values.Get("grant_type"))
				}
				if values.Get("refresh_token") != "rt" {
					t.Fatalf("unexpected refresh_token: %s", values.Get("refresh_token"))
				}
				if values.Get("client_id") != antigravity.ClientID {
					t.Fatalf("unexpected client_id: %s", values.Get("client_id"))
				}
				if values.Get("client_secret") != antigravity.ClientSecret {
					t.Fatalf("unexpected client_secret")
				}

				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(
					map[string]any{
						"access_token":  "new-token",
						"refresh_token": "rt2",
						"expires_in":    int64(3600),
						"token_type":    "Bearer",
					},
				)
			},
		),
	)
	t.Cleanup(srv.Close)

	originalURL := antigravityOAuthTokenURL
	antigravityOAuthTokenURL = srv.URL
	t.Cleanup(func() { antigravityOAuthTokenURL = originalURL })

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)

	auth := &coreauth.Auth{
		ID:       "antigravity-test.json",
		FileName: "antigravity-test.json",
		Provider: "antigravity",
		Metadata: map[string]any{
			"type":          "antigravity",
			"access_token":  "old-token",
			"refresh_token": "rt",
			"expires_in":    int64(3600),
			"timestamp":     time.Now().Add(-2 * time.Hour).UnixMilli(),
			"expired":       time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{authManager: manager}
	token, err := h.resolveTokenForAuth(context.Background(), h.configSnapshot(), auth)
	if err != nil {
		t.Fatalf("resolveTokenForAuth: %v", err)
	}
	if token != "new-token" {
		t.Fatalf("expected refreshed token, got %q", token)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 refresh call, got %d", callCount)
	}

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth in manager after update")
	}
	if got := tokenValueFromMetadata(updated.Metadata); got != "new-token" {
		t.Fatalf("expected manager metadata updated, got %q", got)
	}
	store.mu.Lock()
	persisted := store.items[auth.ID].Clone()
	store.mu.Unlock()
	if got := tokenValueFromMetadata(persisted.Metadata); got != "new-token" {
		t.Fatalf("expected persisted metadata updated, got %q", got)
	}
}

func TestResolveTokenForAuthUsesRequestProxyForOAuthRefresh(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			proxyCalls.Add(1)
			if request.URL.String() != "http://oauth.example/token" {
				t.Fatalf("proxy target = %s", request.URL)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"proxied-token","refresh_token":"rt2","expires_in":3600}`))
		}),
	)
	t.Cleanup(proxy.Close)

	originalURL := antigravityOAuthTokenURL
	antigravityOAuthTokenURL = "http://oauth.example/token"
	t.Cleanup(func() { antigravityOAuthTokenURL = originalURL })

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "antigravity-proxy.json",
		FileName: "antigravity-proxy.json",
		Provider: "antigravity",
		ProxyURL: "http://credential-proxy.invalid:8080",
		Metadata: map[string]any{
			"access_token":  "expired-token",
			"refresh_token": "rt",
			"expired":       time.Now().Add(-time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	cfg := &config.Config{}
	cfg.ProxyURL = "http://global-proxy.invalid:8080"
	h := &Handler{authManager: manager}
	token, errResolve := h.resolveTokenForAuthWithProxy(
		context.Background(),
		cfg,
		auth,
		proxy.URL,
	)
	if errResolve != nil || token != "proxied-token" {
		t.Fatalf("token = %q, error = %v", token, errResolve)
	}
	if proxyCalls.Load() != 1 {
		t.Fatalf("OAuth proxy calls = %d, want 1", proxyCalls.Load())
	}
	if got := proxyURLForTransport(t, h.apiCallTransport(cfg, auth, proxy.URL)); got != proxy.URL {
		t.Fatalf("main request proxy = %q, want OAuth proxy %q", got, proxy.URL)
	}
}

func TestResolveTokenForAuthDirectDoesNotInheritOAuthProxy(t *testing.T) {
	var inheritedProxyCalls atomic.Int32
	inheritedProxy := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			inheritedProxyCalls.Add(1)
			http.Error(w, "unexpected inherited proxy", http.StatusBadGateway)
		}),
	)
	t.Cleanup(inheritedProxy.Close)
	tokenServerCalls := 0
	tokenServer := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			tokenServerCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"direct-token","refresh_token":"rt2","expires_in":3600}`))
		}),
	)
	t.Cleanup(tokenServer.Close)

	originalURL := antigravityOAuthTokenURL
	antigravityOAuthTokenURL = tokenServer.URL
	t.Cleanup(func() { antigravityOAuthTokenURL = originalURL })

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "antigravity-direct.json",
		FileName: "antigravity-direct.json",
		Provider: "antigravity",
		ProxyURL: inheritedProxy.URL,
		Metadata: map[string]any{
			"access_token":  "expired-token",
			"refresh_token": "rt",
			"expired":       time.Now().Add(-time.Hour).Format(time.RFC3339),
		},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	cfg := &config.Config{}
	cfg.ProxyURL = inheritedProxy.URL
	h := &Handler{authManager: manager}
	token, errResolve := h.resolveTokenForAuthWithProxy(context.Background(), cfg, auth, "direct")
	if errResolve != nil || token != "direct-token" {
		t.Fatalf("token = %q, error = %v", token, errResolve)
	}
	if tokenServerCalls != 1 || inheritedProxyCalls.Load() != 0 {
		t.Fatalf("direct refresh calls = %d, inherited proxy calls = %d", tokenServerCalls, inheritedProxyCalls.Load())
	}
}

func TestResolveTokenForAuthRefreshPreservesConcurrentPersistentChanges(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	srv := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				close(requestStarted)
				<-releaseResponse
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(
					map[string]any{
						"access_token":  "new-token",
						"refresh_token": "new-refresh",
						"expires_in":    int64(3600),
					},
				)
			},
		),
	)
	t.Cleanup(srv.Close)
	originalURL := antigravityOAuthTokenURL
	antigravityOAuthTokenURL = srv.URL
	t.Cleanup(func() { antigravityOAuthTokenURL = originalURL })

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	registered, errRegister := manager.Register(
		context.Background(),
		&coreauth.Auth{
			ID:       "antigravity-concurrent.json",
			FileName: "antigravity-concurrent.json",
			Provider: "antigravity",
			Label:    "original",
			Metadata: map[string]any{
				"type":          "antigravity",
				"access_token":  "old-token",
				"refresh_token": "old-refresh",
				"expired":       time.Now().Add(-time.Hour).Format(time.RFC3339),
			},
		},
	)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	h := &Handler{authManager: manager}
	result := make(
		chan struct {
			token string
			err   error
		}, 1,
	)
	go func() {
		token, errResolve := h.resolveTokenForAuth(context.Background(), h.configSnapshot(), registered.Clone())
		result <- struct {
			token string
			err   error
		}{token: token, err: errResolve}
	}()
	<-requestStarted
	concurrent, ok := manager.GetByID(registered.ID)
	if !ok || concurrent == nil {
		t.Fatal("registered auth disappeared")
	}
	concurrent.Label = "concurrent"
	concurrent.Metadata["custom"] = "preserved"
	if _, errUpdate := manager.Update(context.Background(), concurrent); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	close(releaseResponse)
	refreshResult := <-result
	if refreshResult.err != nil || refreshResult.token != "new-token" {
		t.Fatalf("refresh result = (%q, %v)", refreshResult.token, refreshResult.err)
	}
	updated, ok := manager.GetByID(registered.ID)
	if !ok || updated == nil {
		t.Fatal("refreshed auth disappeared")
	}
	if updated.Label != "concurrent" || stringValue(updated.Metadata, "custom") != "preserved" {
		t.Fatalf("concurrent fields were overwritten: %#v", updated)
	}
	if tokenValueFromMetadata(updated.Metadata) != "new-token" ||
		stringValue(updated.Metadata, "refresh_token") != "new-refresh" {
		t.Fatalf("refreshed credentials were not merged: %#v", updated.Metadata)
	}
	store.mu.Lock()
	persisted := store.items[registered.ID]
	if persisted != nil {
		persisted = persisted.Clone()
	}
	store.mu.Unlock()
	if persisted == nil || persisted.Label != "concurrent" || stringValue(
		persisted.Metadata, "custom",
	) != "preserved" ||
		tokenValueFromMetadata(persisted.Metadata) != "new-token" ||
		stringValue(persisted.Metadata, "refresh_token") != "new-refresh" {
		t.Fatalf("persisted auth did not preserve concurrent fields and refreshed credentials: %#v", persisted)
	}
}

func TestPersistRefreshedAuthRejectsCredentialReplacement(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	registered, errRegister := manager.Register(
		context.Background(),
		&coreauth.Auth{
			ID:       "replaced.json",
			FileName: "replaced.json",
			Provider: "codex",
			Metadata: map[string]any{
				"access_token":  "account-a-token",
				"refresh_token": "account-a-refresh",
			},
		},
	)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	before := registered.Clone()
	refreshed := registered.Clone()
	refreshed.Metadata["access_token"] = "account-a-new-token"
	refreshed.Metadata["refresh_token"] = "account-a-new-refresh"

	replacement := registered.Clone()
	replacement.Metadata["access_token"] = "account-b-token"
	replacement.Metadata["refresh_token"] = "account-b-refresh"
	if _, errUpdate := manager.Update(context.Background(), replacement); errUpdate != nil {
		t.Fatal(errUpdate)
	}

	h := &Handler{authManager: manager}
	errPersist := h.persistRefreshedAuth(context.Background(), before, refreshed)
	if errPersist == nil || !strings.Contains(errPersist.Error(), "credential owner changed") {
		t.Fatalf("persistRefreshedAuth() error = %v", errPersist)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current == nil || stringValue(current.Metadata, "access_token") != "account-b-token" ||
		stringValue(current.Metadata, "refresh_token") != "account-b-refresh" {
		t.Fatalf("replacement credential was overwritten: %#v", current)
	}
	store.mu.Lock()
	persisted := store.items[registered.ID]
	if persisted != nil {
		persisted = persisted.Clone()
	}
	store.mu.Unlock()
	if persisted == nil || stringValue(persisted.Metadata, "access_token") != "account-b-token" ||
		stringValue(persisted.Metadata, "refresh_token") != "account-b-refresh" {
		t.Fatalf("persisted replacement credential was overwritten: %#v", persisted)
	}
}

func TestPersistRefreshedAuthCompletesAfterRequestCancellation(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	registered, errRegister := manager.Register(
		context.Background(),
		&coreauth.Auth{
			ID:       "rotated.json",
			FileName: "rotated.json",
			Provider: "codex",
			Metadata: map[string]any{
				"access_token":  "old-token",
				"refresh_token": "old-refresh",
			},
		},
	)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	before := registered.Clone()
	refreshed := registered.Clone()
	refreshed.Metadata["access_token"] = "new-token"
	refreshed.Metadata["refresh_token"] = "new-refresh"

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := &Handler{authManager: manager}
	if errPersist := h.persistRefreshedAuth(ctx, before, refreshed); errPersist != nil {
		t.Fatalf("persistRefreshedAuth() error = %v", errPersist)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current == nil || stringValue(current.Metadata, "access_token") != "new-token" ||
		stringValue(current.Metadata, "refresh_token") != "new-refresh" {
		t.Fatalf("rotated credential was not published: %#v", current)
	}
	store.mu.Lock()
	persisted := store.items[registered.ID]
	if persisted != nil {
		persisted = persisted.Clone()
	}
	store.mu.Unlock()
	if persisted == nil || stringValue(persisted.Metadata, "access_token") != "new-token" ||
		stringValue(persisted.Metadata, "refresh_token") != "new-refresh" {
		t.Fatalf("rotated credential was not persisted: %#v", persisted)
	}
}

func TestPersistRefreshedAuthSupportsNestedGeminiRefreshToken(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	registered, errRegister := manager.Register(
		context.Background(),
		&coreauth.Auth{
			ID:       "gemini-nested.json",
			FileName: "gemini-nested.json",
			Provider: "gemini-cli",
			Metadata: map[string]any{
				"token": map[string]any{
					"access_token":  "old-token",
					"refresh_token": "old-refresh",
				},
			},
		},
	)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	before := registered.Clone()
	refreshed := registered.Clone()
	refreshed.Metadata["token"] = map[string]any{
		"access_token":  "new-token",
		"refresh_token": "new-refresh",
	}
	refreshed.Metadata["access_token"] = "new-token"
	refreshed.Metadata["refresh_token"] = "new-refresh"

	h := &Handler{authManager: manager}
	if errPersist := h.persistRefreshedAuth(context.Background(), before, refreshed); errPersist != nil {
		t.Fatalf("persistRefreshedAuth() error = %v", errPersist)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current == nil || refreshTokenValue(current.Metadata) != "new-refresh" ||
		tokenValueFromMetadata(current.Metadata) != "new-token" {
		t.Fatalf("refreshed Gemini auth = %#v", current)
	}
}

func TestResolveTokenForAuth_XAI_RejectsUntrustedRefreshEndpoint(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "xai-test.json",
		FileName: "xai-test.json",
		Provider: "xai",
		Metadata: map[string]any{
			"type":           "xai",
			"access_token":   "old-token",
			"refresh_token":  "rt",
			"token_endpoint": "http://127.0.0.1/oauth/token",
			"expired":        time.Now().Add(-time.Hour).Format(time.RFC3339),
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := &Handler{authManager: manager}
	token, err := h.resolveTokenForAuth(context.Background(), h.configSnapshot(), auth)
	if err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("resolveTokenForAuth() token = %q, error = %v", token, err)
	}
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected auth in manager after rejected refresh")
	}
	if got := tokenValueFromMetadata(updated.Metadata); got != "old-token" {
		t.Fatalf("rejected refresh changed manager metadata to %q", got)
	}
}

func TestResolveTokenForAuth_Antigravity_SkipsRefreshWhenTokenValid(t *testing.T) {
	var callCount int
	srv := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				callCount++
				w.WriteHeader(http.StatusInternalServerError)
			},
		),
	)
	t.Cleanup(srv.Close)

	originalURL := antigravityOAuthTokenURL
	antigravityOAuthTokenURL = srv.URL
	t.Cleanup(func() { antigravityOAuthTokenURL = originalURL })

	auth := &coreauth.Auth{
		ID:       "antigravity-valid.json",
		FileName: "antigravity-valid.json",
		Provider: "antigravity",
		Metadata: map[string]any{
			"type":         "antigravity",
			"access_token": "ok-token",
			"expired":      time.Now().Add(30 * time.Minute).Format(time.RFC3339),
		},
	}
	h := &Handler{}
	token, err := h.resolveTokenForAuth(context.Background(), h.configSnapshot(), auth)
	if err != nil {
		t.Fatalf("resolveTokenForAuth: %v", err)
	}
	if token != "ok-token" {
		t.Fatalf("expected existing token, got %q", token)
	}
	if callCount != 0 {
		t.Fatalf("expected no refresh calls, got %d", callCount)
	}
}

func TestIsPrivateHost(t *testing.T) {
	tests := []struct {
		hostname string
		want     bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"8.8.8.8", false},
		{"::1", true},
		{"100:0:0:1::1", true},
		{"2002::1", true},
		{"", true},
		{"0.0.0.0", true},
	}

	for _, tt := range tests {
		got := isPrivateHost(tt.hostname)
		if got != tt.want {
			t.Errorf("isPrivateHost(%q) = %v, want %v", tt.hostname, got, tt.want)
		}
	}
}

func TestIsAllowedTokenTarget(t *testing.T) {
	tests := []struct {
		hostname string
		want     bool
	}{
		{"api.openai.com", true},
		{"API.OPENAI.COM", true},
		{"sub.api.openai.com", true},
		{"api.openai.com.evil.com", false},
		{"api.fenno.ai", true},
		{"api.fenno.ai.evil.com", false},
		{"api.qnaigc.com", true},
		{"api.modelink.ai", true},
		{"api.deepseek.com", true},
		{"API.DEEPSEEK.COM", true},
		{"sub.api.deepseek.com", false},
		{"api.deepseek.com.evil.com", false},
		{"iflow.cn", true},
		{"foo.iflow.cn", true},
		{"x.ai", true},
		{"auth.x.ai", true},
		{"cli-chat-proxy.grok.com", true},
		{"api.devin.ai", true},
		{"server.codeium.com", true},
		{"sub.server.codeium.com", false},
		{"server.codeium.com.evil.com", false},
		{"evil.com", false},
	}
	for _, tt := range tests {
		got := isAllowedTokenTarget(tt.hostname)
		if got != tt.want {
			t.Errorf("isAllowedTokenTarget(%q) = %v, want %v", tt.hostname, got, tt.want)
		}
	}
}

func TestIsAllowedTokenRedirectTarget(t *testing.T) {
	tests := []struct {
		name        string
		originalURL string
		redirectURL string
		want        bool
	}{
		{
			name:        "deepseek same origin",
			originalURL: "https://api.deepseek.com/models",
			redirectURL: "https://api.deepseek.com/redirected",
			want:        true,
		},
		{
			name:        "deepseek explicit default port",
			originalURL: "https://api.deepseek.com/models",
			redirectURL: "https://api.deepseek.com:443/redirected",
			want:        true,
		},
		{
			name:        "deepseek non-default port",
			originalURL: "https://api.deepseek.com/models",
			redirectURL: "https://api.deepseek.com:8443/redirected",
			want:        false,
		},
		{
			name:        "deepseek cross provider",
			originalURL: "https://api.deepseek.com/models",
			redirectURL: "https://api.openai.com/models",
			want:        false,
		},
		{
			name:        "deepseek subdomain",
			originalURL: "https://api.deepseek.com/models",
			redirectURL: "https://sub.api.deepseek.com/models",
			want:        false,
		},
		{
			name:        "existing provider behavior",
			originalURL: "https://api.openai.com/v1/models",
			redirectURL: "https://api.anthropic.com/v1/models",
			want:        true,
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				originalURL, errOriginal := url.Parse(tt.originalURL)
				if errOriginal != nil {
					t.Fatal(errOriginal)
				}
				redirectURL, errRedirect := url.Parse(tt.redirectURL)
				if errRedirect != nil {
					t.Fatal(errRedirect)
				}
				got := isAllowedTokenRedirectTarget(originalURL, redirectURL)
				if got != tt.want {
					t.Fatalf(
						"isAllowedTokenRedirectTarget(%q, %q) = %v, want %v", tt.originalURL, tt.redirectURL, got,
						tt.want,
					)
				}
			},
		)
	}
}

func TestShouldStripTokenRedirectHeaders(t *testing.T) {
	tests := []struct {
		name             string
		originalHostname string
		redirectHostname string
		want             bool
	}{
		{
			name:             "same host",
			originalHostname: "api.deepseek.com",
			redirectHostname: "api.deepseek.com",
			want:             false,
		},
		{
			name:             "same host ignores case and whitespace",
			originalHostname: " API.DEEPSEEK.COM ",
			redirectHostname: "api.deepseek.com",
			want:             false,
		},
		{
			name:             "cross provider",
			originalHostname: "api.openai.com",
			redirectHostname: "api.anthropic.com",
			want:             true,
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				got := shouldStripTokenRedirectHeaders(tt.originalHostname, tt.redirectHostname)
				if got != tt.want {
					t.Fatalf(
						"shouldStripTokenRedirectHeaders(%q, %q) = %v, want %v", tt.originalHostname,
						tt.redirectHostname, got, tt.want,
					)
				}
			},
		)
	}
}

func TestValidateAPICallHeadersRejectsLiteralRestrictedHeader(t *testing.T) {
	err := validateAPICallHeaders(map[string]string{"Authorization": "Bearer literal"})
	if err == nil {
		t.Fatal("expected literal restricted header to be rejected")
	}
}

func TestValidateAPICallHeadersAllowsTokenRestrictedHeader(t *testing.T) {
	err := validateAPICallHeaders(map[string]string{"Authorization": "Bearer $TOKEN$"})
	if err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestValidateAPICallHeadersRejectsHostOverride(t *testing.T) {
	err := validateAPICallHeaders(map[string]string{"Host": "api.example.test"})
	if err == nil {
		t.Fatal("expected Host override to be rejected")
	}
}

func TestSubstituteAPICallTokenResolvesOnceAndEscapesJSON(t *testing.T) {
	headers := map[string]string{
		"Authorization": "Bearer $TOKEN$",
		"X-Token":       "$TOKEN$:$TOKEN$",
	}
	const token = "line1\"\\line2\n\tend"
	calls := 0
	resolved, data, errSubstitute := substituteAPICallToken(
		headers,
		`{"metadata":{"apiKey":"$TOKEN$","again":"$TOKEN$"}}`,
		func() (string, error) {
			calls++
			return token, nil
		},
	)
	if errSubstitute != nil {
		t.Fatalf("substituteAPICallToken() error = %v", errSubstitute)
	}
	if calls != 1 {
		t.Fatalf("token resolver calls = %d, want 1", calls)
	}
	if resolved != token || headers["Authorization"] != "Bearer "+token || headers["X-Token"] != token+":"+token {
		t.Fatalf("resolved token or headers mismatch: token=%q headers=%#v", resolved, headers)
	}
	var payload struct {
		Metadata struct {
			APIKey string `json:"apiKey"`
			Again  string `json:"again"`
		} `json:"metadata"`
	}
	if errDecode := json.Unmarshal([]byte(data), &payload); errDecode != nil {
		t.Fatalf("substituted body is invalid JSON: %v; body=%q", errDecode, data)
	}
	if payload.Metadata.APIKey != token || payload.Metadata.Again != token {
		t.Fatalf("substituted body token mismatch: %#v", payload.Metadata)
	}
}

func TestSubstituteAPICallTokenKeepsNonJSONTextRaw(t *testing.T) {
	multilineText := strings.Join([]string{"line1", "line2"}, "\n")
	_, data, errSubstitute := substituteAPICallToken(
		nil,
		"prefix=$TOKEN$",
		func() (string, error) { return multilineText, nil },
	)
	if errSubstitute != nil {
		t.Fatalf("substituteAPICallToken() error = %v", errSubstitute)
	}
	if data != "prefix="+multilineText {
		t.Fatalf("substituted body = %q, want raw multiline text", data)
	}
}

func TestTokenValueForAuthSupportsKnownDevinFieldsOnly(t *testing.T) {
	tests := []struct {
		name string
		auth *coreauth.Auth
		want string
	}{
		{
			name: "metadata api key",
			auth: &coreauth.Auth{Provider: "devin", Metadata: map[string]any{"api_key": "metadata-api"}},
			want: "metadata-api",
		},
		{
			name: "metadata session token",
			auth: &coreauth.Auth{Provider: "cognition", Metadata: map[string]any{"session_token": "metadata-session"}},
			want: "metadata-session",
		},
		{
			name: "attribute session token",
			auth: &coreauth.Auth{Provider: "devin", Attributes: map[string]string{"session_token": "attribute-session"}},
			want: "attribute-session",
		},
		{
			name: "unrelated provider ignores Devin session token",
			auth: &coreauth.Auth{Provider: "other", Metadata: map[string]any{"session_token": "secret"}},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tokenValueForAuth(tt.auth); got != tt.want {
				t.Fatalf("tokenValueForAuth() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveAPICallTokenPrefersDraftToken(t *testing.T) {
	h := &Handler{}
	auth := &coreauth.Auth{Attributes: map[string]string{"api_key": "stored-token"}}
	token, err := h.resolveAPICallToken(context.Background(), nil, auth, "draft-token")
	if err != nil {
		t.Fatalf("resolveAPICallToken error = %v", err)
	}
	if token != "draft-token" {
		t.Fatalf("resolveAPICallToken = %q, want draft-token", token)
	}
}

func TestAPICallRejectsDraftTokenWithoutPlaceholder(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{})
	recorder := runConfigListRequest(
		h.APICall,
		http.MethodPost,
		"/api-call",
		`{"method":"GET","url":"https://api.openai.com/v1/models","draftToken":"draft-token"}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("APICall status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "requires a $TOKEN$ header or body placeholder") {
		t.Fatalf("APICall body = %s", recorder.Body.String())
	}
}

func TestAPICallRejectsPlaceholderWithoutToken(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{})
	recorder := runConfigListRequest(
		h.APICall,
		http.MethodPost,
		"/api-call",
		`{"method":"GET","url":"https://api.openai.com/v1/models","header":{"Authorization":"Bearer $TOKEN$"}}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("APICall status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "auth token not found") {
		t.Fatalf("APICall body = %s", recorder.Body.String())
	}
}

func TestAPICallRejectsDraftTokenOverHTTP(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{})
	recorder := runConfigListRequest(
		h.APICall,
		http.MethodPost,
		"/api-call",
		`{"method":"GET","url":"http://api.openai.com/v1/models","header":{"Authorization":"Bearer $TOKEN$"},"draftToken":"draft-token"}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("APICall status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "requires HTTPS") {
		t.Fatalf("APICall body = %s", recorder.Body.String())
	}
}

func TestValidateTokenTransportURLRejectsDowngrade(t *testing.T) {
	target, err := url.Parse("http://api.openai.com/v1/models")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if errTransport := validateTokenTransportURL(target); errTransport == nil {
		t.Fatal("expected HTTP token transport to be rejected")
	}
}

func TestAPICallRejectsDraftTokenForUnknownTarget(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{})
	recorder := runConfigListRequest(
		h.APICall,
		http.MethodPost,
		"/api-call",
		`{"method":"GET","url":"https://example.com/v1/models","header":{"Authorization":"Bearer $TOKEN$"},"draftToken":"draft-token"}`,
	)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("APICall status = %d, want 403: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAPICallBodyTokenUsesSameTransportRestrictions(t *testing.T) {
	tests := []struct {
		name       string
		targetURL  string
		draftToken string
		wantStatus int
		wantError  string
	}{
		{
			name:       "missing token",
			targetURL:  "https://api.devin.ai/v1/status",
			wantStatus: http.StatusBadRequest,
			wantError:  "auth token not found",
		},
		{
			name:       "HTTP target",
			targetURL:  "http://api.devin.ai/v1/status",
			draftToken: "draft-token",
			wantStatus: http.StatusBadRequest,
			wantError:  "requires HTTPS",
		},
		{
			name:       "unknown target",
			targetURL:  "https://example.com/v1/status",
			draftToken: "draft-token",
			wantStatus: http.StatusForbidden,
			wantError:  "known provider domains",
		},
		{
			name:       "Devin alternate port",
			targetURL:  "https://api.devin.ai:8443/v1/status",
			draftToken: "draft-token",
			wantStatus: http.StatusForbidden,
			wantError:  "known provider domains",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, errMarshal := json.Marshal(
				map[string]any{
					"method":     http.MethodPost,
					"url":        tt.targetURL,
					"data":       `{"token":"$TOKEN$"}`,
					"draftToken": tt.draftToken,
				},
			)
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			h := newConfigListTestHandler(t, &config.Config{})
			recorder := runConfigListRequest(h.APICall, http.MethodPost, "/api-call", string(payload))
			if recorder.Code != tt.wantStatus || !strings.Contains(recorder.Body.String(), tt.wantError) {
				t.Fatalf("APICall status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestAPICallReplacesBodyTokenForKnownDevinEndpoint(t *testing.T) {
	const token = "line1\"\\line2\nend"
	requestCount := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestCount++
		var payload map[string]string
		if errDecode := json.NewDecoder(request.Body).Decode(&payload); errDecode != nil {
			t.Fatalf("decode body: %v", errDecode)
		}
		if payload["token"] != token {
			t.Fatalf("body token = %q, want %q", payload["token"], token)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	installAPICallTLSTestTransport(t, server)

	data, errData := json.Marshal(map[string]string{"token": "$TOKEN$"})
	if errData != nil {
		t.Fatal(errData)
	}
	body, errBody := json.Marshal(map[string]any{
		"method":     http.MethodPost,
		"url":        "https://server.codeium.com/exa.seat_management_pb.SeatManagementService/GetUserStatus",
		"data":       string(data),
		"draftToken": token,
	})
	if errBody != nil {
		t.Fatal(errBody)
	}
	h := newConfigListTestHandler(t, &config.Config{})
	recorder := runConfigListRequest(h.APICall, http.MethodPost, "/api-call", string(body))
	if recorder.Code != http.StatusOK || requestCount != 1 {
		t.Fatalf("status=%d requests=%d body=%s", recorder.Code, requestCount, recorder.Body.String())
	}
}

func TestAPICallBlocksCrossHostRedirectForBodyToken(t *testing.T) {
	requestCount := 0
	server := httptest.NewTLSServer(
		http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			requestCount++
			body, errRead := io.ReadAll(request.Body)
			if errRead != nil {
				t.Fatalf("ReadAll() error = %v", errRead)
			}
			if string(body) != `{"token":"draft-token"}` {
				t.Fatalf("request body = %q", body)
			}
			http.Redirect(
				w,
				request,
				"https://server.codeium.com/redirected",
				http.StatusTemporaryRedirect,
			)
		}),
	)
	t.Cleanup(server.Close)
	installAPICallTLSTestTransport(t, server)

	payload, errMarshal := json.Marshal(
		map[string]any{
			"method":     http.MethodPost,
			"url":        "https://api.devin.ai/status",
			"data":       `{"token":"$TOKEN$"}`,
			"draftToken": "draft-token",
		},
	)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	h := newConfigListTestHandler(t, &config.Config{})
	recorder := runConfigListRequest(h.APICall, http.MethodPost, "/api-call", string(payload))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("APICall status = %d, want 502: %s", recorder.Code, recorder.Body.String())
	}
	if requestCount != 1 {
		t.Fatalf("request count = %d, want 1", requestCount)
	}
}

func TestAPICallRejectsUnsafeDeepSeekTargets(t *testing.T) {
	tests := []struct {
		name      string
		targetURL string
	}{
		{name: "non-default port", targetURL: "https://api.deepseek.com:8443/models"},
		{name: "malicious suffix", targetURL: "https://api.deepseek.com.evil.com/models"},
		{name: "unknown subdomain", targetURL: "https://sub.api.deepseek.com/models"},
		{name: "private address", targetURL: "https://127.0.0.1/models"},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				h := newConfigListTestHandler(t, &config.Config{})
				body, errMarshal := json.Marshal(
					map[string]any{
						"method":     http.MethodGet,
						"url":        tt.targetURL,
						"header":     map[string]string{"Authorization": "Bearer $TOKEN$"},
						"draftToken": "draft-token",
					},
				)
				if errMarshal != nil {
					t.Fatal(errMarshal)
				}
				recorder := runConfigListRequest(h.APICall, http.MethodPost, "/api-call", string(body))
				if recorder.Code != http.StatusForbidden {
					t.Fatalf("APICall status = %d, want 403: %s", recorder.Code, recorder.Body.String())
				}
			},
		)
	}
}

func TestAPICallRejectsUnsafeDeepSeekRedirects(t *testing.T) {
	tests := []struct {
		name     string
		location string
	}{
		{name: "non-default port", location: "https://api.deepseek.com:8443/models"},
		{name: "cross provider", location: "https://api.openai.com/v1/models"},
		{name: "unknown subdomain", location: "https://sub.api.deepseek.com/models"},
		{name: "private address", location: "https://127.0.0.1/models"},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				requestCount := 0
				server := httptest.NewTLSServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							requestCount++
							if got := r.Header.Get("Authorization"); got != "Bearer draft-token" {
								t.Fatalf("Authorization = %q", got)
							}
							http.Redirect(w, r, tt.location, http.StatusFound)
						},
					),
				)
				t.Cleanup(server.Close)

				installAPICallTLSTestTransport(t, server)

				h := newConfigListTestHandler(t, &config.Config{})
				recorder := runConfigListRequest(
					h.APICall,
					http.MethodPost,
					"/api-call",
					`{"method":"GET","url":"https://api.deepseek.com/models","header":{"Authorization":"Bearer $TOKEN$"},"draftToken":"draft-token"}`,
				)
				if recorder.Code != http.StatusBadGateway {
					t.Fatalf("APICall status = %d, want 502: %s", recorder.Code, recorder.Body.String())
				}
				if requestCount != 1 {
					t.Fatalf("request count = %d, want 1", requestCount)
				}
			},
		)
	}
}

func TestAPICallAllowsDeepSeekDefaultPortRedirect(t *testing.T) {
	requestCount := 0
	server := httptest.NewTLSServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				requestCount++
				if got := r.Header.Get("Authorization"); got != "Bearer draft-token" {
					t.Fatalf("Authorization = %q", got)
				}
				if r.URL.Path == "/models" {
					http.Redirect(w, r, "https://api.deepseek.com:443/redirected", http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[]}`))
			},
		),
	)
	t.Cleanup(server.Close)

	installAPICallTLSTestTransport(t, server)

	h := newConfigListTestHandler(t, &config.Config{})
	recorder := runConfigListRequest(
		h.APICall,
		http.MethodPost,
		"/api-call",
		`{"method":"GET","url":"https://api.deepseek.com/models","header":{"Authorization":"Bearer $TOKEN$"},"draftToken":"draft-token"}`,
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("APICall status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if requestCount != 2 {
		t.Fatalf("request count = %d, want 2", requestCount)
	}
}

func installAPICallTLSTestTransport(t *testing.T, server *httptest.Server) {
	t.Helper()
	transport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("test server transport = %T", server.Client().Transport)
	}
	transport = transport.Clone()
	// #nosec G402 -- this transport is confined to a local httptest server.
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	serverAddr := server.Listener.Addr().String()
	dialer := &net.Dialer{Timeout: time.Second}
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, serverAddr)
	}

	originalTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
}

func TestResolveAndCheckHost_IPLiteral(t *testing.T) {
	// Public IP should return the IP itself and private=false.
	pinnedAddr, private := resolveAndCheckHost("8.8.8.8")
	if private {
		t.Fatal("expected 8.8.8.8 to not be private")
	}
	if pinnedAddr != "8.8.8.8" {
		t.Fatalf("expected pinnedAddr=8.8.8.8, got %s", pinnedAddr)
	}
}

func TestResolveAndCheckHost_PrivateIP(t *testing.T) {
	pinnedAddr, private := resolveAndCheckHost("10.0.0.1")
	if !private {
		t.Fatal("expected 10.0.0.1 to be private")
	}
	if pinnedAddr != "" {
		t.Fatalf("expected empty pinnedAddr for private IP, got %s", pinnedAddr)
	}
}

func TestResolveAndCheckHost_EmptyHost(t *testing.T) {
	_, private := resolveAndCheckHost("")
	if !private {
		t.Fatal("expected empty hostname to be treated as private")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestPublicHostTransportBlocksPrivateHost(t *testing.T) {
	called := false
	base := roundTripFunc(
		func(*http.Request) (*http.Response, error) {
			called = true
			return nil, nil
		},
	)
	transport := publicHostTransport(base)
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	_, err = transport.RoundTrip(req)
	if err == nil {
		t.Fatal("expected private host to be rejected")
	}
	if called {
		t.Fatal("base transport should not be called for private host")
	}
}

func TestPinnedTransport_EmptyParams(t *testing.T) {
	base := &http.Transport{}

	// Empty pinnedAddr should return base unchanged.
	got := pinnedTransport(base, "example.com", "")
	if got != base {
		t.Fatal("expected base transport returned for empty pinnedAddr")
	}

	// Empty hostname should return base unchanged.
	got = pinnedTransport(base, "", "1.2.3.4")
	if got != base {
		t.Fatal("expected base transport returned for empty hostname")
	}
}

func TestPinnedTransport_NonTransport(t *testing.T) {
	// Non-*http.Transport should be returned as-is.
	rt := http.DefaultClient.Transport
	if rt == nil {
		// Use a simple wrapper if nil.
		t.Skip("default transport is nil")
	}
	// pinnedTransport with a non-Transport RoundTripper returns the original.
	type customRT struct{ http.RoundTripper }
	custom := &customRT{}
	got := pinnedTransport(custom, "host", "1.2.3.4")
	if got != custom {
		t.Fatal("expected custom RoundTripper returned unchanged")
	}
}

func TestAPICallRejectsInvalidRequestProxyBeforeTargetValidation(t *testing.T) {
	h := newConfigListTestHandler(t, &config.Config{})
	recorder := runConfigListRequest(
		h.APICall,
		http.MethodPost,
		"/api-call",
		`{"method":"GET","url":"http://127.0.0.1/private","proxy_url":"not-a-proxy"}`,
	)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("APICall status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "invalid proxy_url") {
		t.Fatalf("APICall body = %s", recorder.Body.String())
	}
}

func TestAPICallTransportExplicitDirectDoesNotInherit(t *testing.T) {
	cfg := &config.Config{}
	cfg.ProxyURL = "http://1.1.1.1:8080"
	h := &Handler{cfg: cfg}
	auth := &coreauth.Auth{ProxyURL: "http://8.8.4.4:8080"}
	for _, mode := range []string{"direct", "none"} {
		t.Run(mode, func(t *testing.T) {
			transport, ok := h.apiCallTransport(cfg, auth, mode).(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T, want *http.Transport", h.apiCallTransport(cfg, auth, mode))
			}
			if transport.Proxy != nil {
				t.Fatalf("%s request proxy inherited a lower-priority proxy", mode)
			}
			if got := h.apiCallProxyURL(cfg, auth, mode); got != mode {
				t.Fatalf("OAuth proxy = %q, want %q", got, mode)
			}
		})
	}
}

func TestAPICallTransport_RequestProxyOverridesCredentialAndGlobal(t *testing.T) {
	cfg := &config.Config{}
	cfg.ProxyURL = "http://1.1.1.1:8080"
	h := &Handler{cfg: cfg}
	auth := &coreauth.Auth{ProxyURL: "http://8.8.4.4:8080"}

	got := proxyURLForTransport(t, h.apiCallTransport(cfg, auth, "http://8.8.8.8:8080"))
	if got != "http://8.8.8.8:8080" {
		t.Fatalf("expected request proxy, got %q", got)
	}
}

func TestAPICallTransport_CredentialProxyOverridesGlobal(t *testing.T) {
	cfg := &config.Config{}
	cfg.ProxyURL = "http://1.1.1.1:8080"
	h := &Handler{cfg: cfg}
	auth := &coreauth.Auth{ProxyURL: "http://8.8.4.4:8080"}

	got := proxyURLForTransport(t, h.apiCallTransport(cfg, auth, ""))
	if got != "http://8.8.4.4:8080" {
		t.Fatalf("expected credential proxy, got %q", got)
	}
}

func TestAPICallTransportAllowsPrivateProxy(t *testing.T) {
	h := &Handler{cfg: &config.Config{}}

	got := proxyURLForTransport(t, h.apiCallTransport(h.configSnapshot(), nil, "http://127.0.0.1:8080"))
	if got != "http://127.0.0.1:8080" {
		t.Fatalf("expected private proxy, got %q", got)
	}
}

func TestAPICallTransportUsesCapturedConfigGeneration(t *testing.T) {
	cfgA := &config.Config{}
	cfgA.ProxyURL = "http://1.1.1.1:8080"
	cfgB := &config.Config{}
	cfgB.ProxyURL = "http://8.8.8.8:8080"
	h := &Handler{cfg: cfgA}

	captured := h.configSnapshot()
	h.SetConfig(cfgB)

	got := proxyURLForTransport(t, h.apiCallTransport(captured, nil, ""))
	if got != cfgA.ProxyURL {
		t.Fatalf("proxy URL = %q, want generation A proxy %q", got, cfgA.ProxyURL)
	}
}

func proxyURLForTransport(t *testing.T, rt http.RoundTripper) string {
	t.Helper()
	transport, ok := rt.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", rt)
	}
	if transport.Proxy == nil {
		t.Fatal("expected proxy function")
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.com", nil)
	proxyURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("proxy lookup failed: %v", err)
	}
	if proxyURL == nil {
		t.Fatal("expected proxy URL")
	}
	return proxyURL.String()
}
