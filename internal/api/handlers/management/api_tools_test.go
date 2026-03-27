package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

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
	token, err := h.resolveTokenForAuth(context.Background(), auth)
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
	token, err := h.resolveTokenForAuth(context.Background(), auth)
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
		{"iflow.cn", true},
		{"foo.iflow.cn", true},
		{"evil.com", false},
	}
	for _, tt := range tests {
		got := isAllowedTokenTarget(tt.hostname)
		if got != tt.want {
			t.Errorf("isAllowedTokenTarget(%q) = %v, want %v", tt.hostname, got, tt.want)
		}
	}
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
