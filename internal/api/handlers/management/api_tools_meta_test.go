package management

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	metaauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/meta"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestManagementAPICallNeverUsesMetaDCAToken(t *testing.T) {
	h := &Handler{}
	for _, tc := range []struct {
		name string
		auth *coreauth.Auth
		want string
	}{
		{name: "minted key", auth: &coreauth.Auth{Provider: "meta", Metadata: map[string]any{"api_key": "meta-api-key", "access_token": "dca:server-only"}}, want: "meta-api-key"},
		{name: "api key config", auth: &coreauth.Auth{Provider: "meta", Attributes: map[string]string{"api_key": "configured-api-key"}}, want: "configured-api-key"},
		{name: "stale dca metadata", auth: &coreauth.Auth{Provider: "meta", Metadata: map[string]any{"api_key": "dca:server-only", "access_token": "meta-api-key"}}, want: "meta-api-key"},
		{name: "storage key", auth: &coreauth.Auth{Provider: "meta", Storage: &metaauth.MetaTokenStorage{APIKey: "stored-meta-key", DCAToken: "dca:server-only"}}, want: "stored-meta-key"},
		{name: "opaque dca", auth: &coreauth.Auth{Provider: "meta", Metadata: map[string]any{"api_key": "opaque-dca", "dca_token": "opaque-dca"}}},
		{name: "only dca", auth: &coreauth.Auth{Provider: "meta", Metadata: map[string]any{"access_token": "dca:server-only"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := h.resolveTokenForAuthWithProxy(context.Background(), nil, tc.auth, "")
			if tc.want == "" {
				if err == nil || got != "" || strings.Contains(err.Error(), "server-only") || strings.Contains(err.Error(), "opaque-dca") {
					t.Fatalf("DCA-only Meta auth resolved: token=%q error=%v", got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Meta API key = %q, error = %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestManagementAPICallMintsAndPersistsMetaAPIKey(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	registered, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "meta-mint-test", FileName: "meta-mint-test.json", Provider: "meta",
		Metadata: map[string]any{"access_token": "dca:management-test", "dca_token": "dca:management-test"},
		Storage:  &metaauth.MetaTokenStorage{AccessToken: "dca:management-test", DCAToken: "dca:management-test"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var calls atomic.Int32
	originalTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.String() != "https://api.meta.ai/muse-code/key" || req.Method != http.MethodPost ||
			req.Header.Get("Authorization") != "Bearer dca:management-test" {
			t.Errorf("unexpected Meta mint request: %s %s", req.Method, req.URL)
			return nil, errors.New("unexpected request")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"api_key":"minted-meta-key","base_url":"https://api.meta.ai/v1"}`)),
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	h := &Handler{authManager: manager}
	results := make(chan struct {
		token string
		err   error
	}, 8)
	for range 8 {
		go func() {
			token, err := h.resolveTokenForAuthWithProxy(context.Background(), nil, h.authByIndex(registered.Index), "")
			results <- struct {
				token string
				err   error
			}{token: token, err: err}
		}()
	}
	for range 8 {
		result := <-results
		if result.err != nil || result.token != "minted-meta-key" {
			t.Fatalf("Meta API key = %q, error = %v", result.token, result.err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("Meta mint requests = %d, want 1", calls.Load())
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current == nil || current.Metadata["api_key"] != "minted-meta-key" || current.Metadata["dca_token"] != "dca:management-test" {
		t.Fatalf("manager credential not updated safely: %#v", current)
	}
	store.mu.Lock()
	persisted := store.items[registered.ID].Clone()
	store.mu.Unlock()
	if persisted.Metadata["api_key"] != "minted-meta-key" || persisted.Metadata["dca_token"] != "dca:management-test" {
		t.Fatalf("minted key not persisted: %#v", persisted)
	}
}

func TestManagementAPICallMetaMintFailureDoesNotExposeDCAToken(t *testing.T) {
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	registered, err := manager.Register(context.Background(), &coreauth.Auth{
		ID: "meta-mint-failure-test", FileName: "meta-mint-failure-test.json", Provider: "meta",
		Metadata: map[string]any{"access_token": "dca:failure-test", "dca_token": "dca:failure-test"},
	})
	if err != nil {
		t.Fatal(err)
	}

	originalTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":"dca:failure-test"}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	h := &Handler{authManager: manager}
	token, err := h.resolveTokenForAuthWithProxy(context.Background(), nil, registered, "")
	if token != "" || err == nil || strings.Contains(err.Error(), "dca:failure-test") {
		t.Fatalf("Meta mint unexpectedly exposed DCA: token=%q error=%v", token, err)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current.Metadata["api_key"] != nil {
		t.Fatalf("failed mint changed credential: %#v", current)
	}
}

func TestManagementAPICallAllowsOnlyKnownMetaAndKimiAPIHosts(t *testing.T) {
	for _, raw := range []string{
		"https://api.meta.ai/v1/models", "https://api.meta.ai:443/v1/models",
		"https://api.kimi.ai/coding/v1/chat/completions", "https://api.kimi.com/coding/v1/chat/completions",
	} {
		u, err := url.Parse(raw)
		if err != nil || !isAllowedTokenTargetURL(u) {
			t.Fatalf("known provider URL rejected: %q, %v", raw, err)
		}
	}
	for _, raw := range []string{
		"https://api.meta.ai.evil.test/v1/models", "https://api.kimi.ai.evil.test/coding/v1/models",
		"https://sub.api.meta.ai/v1/models", "https://api.meta.ai:8443/v1/models",
	} {
		u, err := url.Parse(raw)
		if err != nil || isAllowedTokenTargetURL(u) {
			t.Fatalf("untrusted provider URL accepted: %q, %v", raw, err)
		}
	}
}
