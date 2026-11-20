package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metaauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/meta"
	xaiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/xai"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

type refreshContractStore struct {
	memoryAuthStore
	fail  atomic.Bool
	saves atomic.Int32
}

func (s *refreshContractStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	s.saves.Add(1)
	if s.fail.Load() {
		return "", errors.New("synthetic credential save failure")
	}
	return s.memoryAuthStore.Save(ctx, auth)
}

func TestMetaMintSaveFailureDoesNotPublishOrReturnMintedKey(t *testing.T) {
	store := &refreshContractStore{}
	manager := coreauth.NewManager(store, nil, nil)
	registered, err := manager.Register(
		t.Context(), &coreauth.Auth{
			ID: "meta-save-failure", FileName: "meta-save-failure.json", Provider: "meta",
			Metadata: map[string]any{
				"type": "meta", "access_token": "dca:synthetic-owner", "dca_token": "dca:synthetic-owner",
			},
			Storage: &metaauth.MetaTokenStorage{
				AccessToken: "dca:synthetic-owner",
				DCAToken:    "dca:synthetic-owner",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	store.fail.Store(true)
	originalTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	var mintCalls atomic.Int32
	http.DefaultTransport = roundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			if request.URL.String() != "https://api.meta.ai/muse-code/key" ||
				request.Header.Get("Authorization") != "Bearer dca:synthetic-owner" {
				return nil, errors.New("unexpected synthetic mint request")
			}
			mintCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(
					strings.NewReader(`{"api_key":"synthetic-minted-key","base_url":"https://api.meta.ai/v1"}`),
				),
			}, nil
		},
	)
	token, err := (&Handler{authManager: manager}).resolveTokenForAuthWithProxy(
		t.Context(),
		nil,
		registered,
		"",
	)
	if err == nil || token != "" || strings.Contains(err.Error(), "dca:synthetic-owner") ||
		mintCalls.Load() != 1 {
		t.Fatalf("mint result token=%q error=%v calls=%d", token, err, mintCalls.Load())
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current.Metadata["api_key"] != nil || current.Metadata["dca_token"] != "dca:synthetic-owner" {
		t.Fatal("failed persistence published the minted API key or replaced its owner")
	}
	store.mu.Lock()
	persisted := store.items[registered.ID]
	if persisted != nil && persisted.Metadata["api_key"] != nil {
		store.mu.Unlock()
		t.Fatal("failed persistence changed the durable API key")
	}
	store.mu.Unlock()
}

func TestXAIConcurrentRefreshPublicationPersistsOneResult(t *testing.T) {
	store := &refreshContractStore{}
	manager := coreauth.NewManager(store, nil, nil)
	registered, err := manager.Register(
		t.Context(), &coreauth.Auth{
			ID: "xai-concurrent-contract.json", FileName: "xai-concurrent-contract.json", Provider: "xai",
			Metadata: map[string]any{
				"type":          "xai",
				"access_token":  "old-access",
				"refresh_token": "old-refresh",
			},
			Storage: &xaiauth.TokenStorage{AccessToken: "old-access", RefreshToken: "old-refresh"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	before := registered.Clone()
	refreshed := registered.Clone()
	refreshed.Metadata["access_token"] = "new-access"
	refreshed.Metadata["refresh_token"] = "new-refresh"
	refreshed.Metadata["expired"] = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	refreshed.Storage = &xaiauth.TokenStorage{
		AccessToken: "new-access", RefreshToken: "new-refresh", Expire: refreshed.Metadata["expired"].(string),
	}
	initialSaves := store.saves.Load()
	h := &Handler{authManager: manager}
	results := make(chan error, 32)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 32 {
		workers.Go(
			func() {
				<-start
				results <- h.persistRefreshedAuth(t.Context(), before.Clone(), refreshed.Clone())
			},
		)
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("concurrent refresh publication: %v", err)
		}
	}
	if got := store.saves.Load() - initialSaves; got != 1 {
		t.Fatalf("durable writes=%d want=1", got)
	}
	current, ok := manager.GetByID(registered.ID)
	if !ok || current.Metadata["access_token"] != "new-access" ||
		current.Metadata["refresh_token"] != "new-refresh" {
		t.Fatal("concurrent refresh did not publish the durable token pair")
	}
	storage, ok := current.Storage.(*xaiauth.TokenStorage)
	if !ok || storage.AccessToken != "new-access" || storage.RefreshToken != "new-refresh" {
		t.Fatal("concurrent refresh did not publish typed storage")
	}
}

func TestAPICallTokenSharedKeyStaysScopedToEachProvider(t *testing.T) {
	const shared = "synthetic-shared-api-key"
	var upstreamCalls atomic.Int32
	server := httptest.NewTLSServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+shared {
					t.Error("listed credential did not provide its API key")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if _, err := w.Write([]byte(`{"ok":true}`)); err != nil {
					t.Error(err)
				}
			},
		),
	)
	defer server.Close()
	installAPICallTLSTestTransport(t, server)
	manager := coreauth.NewManager(nil, nil, nil)
	indices := make(map[string]string)
	for _, provider := range []string{"codex", "claude", "xai", "meta"} {
		registered, err := manager.Register(
			t.Context(), &coreauth.Auth{
				ID: provider + "-shared", Provider: provider, Attributes: map[string]string{
					"api_key": shared, "base_url": "https://synthetic.invalid", "runtime_only": "true",
				},
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		indices[provider] = registered.Index
	}
	h := &Handler{authManager: manager}
	listed := runConfigListRequest(h.ListAuthFiles, http.MethodGet, "/auth-files", "")
	var payload struct {
		Files []struct {
			ID        string `json:"id"`
			Provider  string `json:"provider"`
			AuthIndex string `json:"auth_index"`
		} `json:"files"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if listed.Code != http.StatusOK || len(payload.Files) != len(indices) {
		t.Fatalf("shared key listing = %d %s", listed.Code, listed.Body.String())
	}
	for _, item := range payload.Files {
		provider, index := item.Provider, item.AuthIndex
		selected := h.authByIndex(index)
		if index != indices[provider] || item.ID != provider+"-shared" || selected == nil ||
			selected.Provider != provider ||
			selected.ID != item.ID {
			t.Fatalf("shared key selected another provider: %+v", item)
		}
		token, err := h.resolveTokenForAuthWithProxy(t.Context(), nil, selected, "direct")
		if err != nil || token != shared {
			t.Fatalf("provider=%s token=%q error=%v", provider, token, err)
		}
		body := fmt.Sprintf(
			`{"auth_index":%q,"method":"GET","url":"https://api.x.ai/billing",`+
				`"header":{"Authorization":"Bearer `+
				`$TOKEN$"}}`,
			index,
		)
		recorder := runConfigListRequest(h.APICall, http.MethodPost, "/api-call", body)
		var response apiCallResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != http.StatusOK || response.StatusCode != http.StatusOK ||
			response.Body != `{"ok":true}` {
			t.Fatalf("listed credential request = %d %s", recorder.Code, recorder.Body.String())
		}
		readback, ok := manager.GetByID(selected.ID)
		if !ok || readback.Provider != provider || readback.Attributes["api_key"] != shared ||
			readback.Index != index {
			t.Fatalf("shared key round trip changed provider=%s", provider)
		}
	}
	if upstreamCalls.Load() != int32(len(indices)) {
		t.Fatalf("listed credential upstream requests = %d", upstreamCalls.Load())
	}
}

func TestAuthByIndexDistinguishesSharedAPIKeysAcrossProviders(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	geminiAuth := &coreauth.Auth{
		ID: "gemini:apikey:123", Provider: "gemini", Attributes: map[string]string{"api_key": "shared-key"},
	}
	compatAuth := &coreauth.Auth{
		ID: "openai-compatibility:bohe:456", Provider: "bohe", Label: "bohe",
		Attributes: map[string]string{"api_key": "shared-key", "compat_name": "bohe", "provider_key": "bohe"},
	}
	for _, auth := range []*coreauth.Auth{geminiAuth, compatAuth} {
		if _, err := manager.Register(t.Context(), auth); err != nil {
			t.Fatal(err)
		}
	}
	geminiIndex, compatIndex := geminiAuth.EnsureIndex(), compatAuth.EnsureIndex()
	if geminiIndex == compatIndex {
		t.Fatalf("shared API key produced duplicate index %q", geminiIndex)
	}
	h := &Handler{authManager: manager}
	for index, original := range map[string]*coreauth.Auth{geminiIndex: geminiAuth, compatIndex: compatAuth} {
		selected := h.authByIndex(index)
		if selected == nil || selected.ID != original.ID || selected.Provider != original.Provider {
			t.Fatalf("shared API key index %q selected %+v", index, selected)
		}
	}
}

func TestAPICallXAIRefreshOriginalInputs(t *testing.T) {
	for _, test := range []struct {
		name, id, accessToken, refreshToken string
		concurrency                         int
		failure                             bool
	}{
		{"concurrent", "xai-concurrent@example.com.json", "xai-stale-concurrent", "xai-refresh-shared", 8, false},
		{"refresh-failure", "xai-failed-refresh@example.com.json", "xai-expired-token", "xai-bad-refresh", 1, true},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				var refreshCalls, upstreamCalls atomic.Int32
				server := httptest.NewTLSServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							w.Header().Set("Content-Type", "application/json")
							switch r.URL.Path {
							case "/oauth2/token":
								refreshCalls.Add(1)
								if err := r.ParseForm(); err != nil ||
									r.Form.Get("refresh_token") != test.refreshToken ||
									r.Form.Get("grant_type") != "refresh_token" {
									t.Error("original refresh token or grant did not reach the token endpoint")
									w.WriteHeader(http.StatusBadRequest)
									return
								}
								if test.failure {
									w.WriteHeader(http.StatusUnauthorized)
									if _, err := w.Write([]byte(`{"error":"invalid_grant"}`)); err != nil {
										t.Error(err)
									}
									return
								}
								if _, err := w.Write(
									[]byte(`{"access_token":"xai-fresh-concurrent",` +
										`"refresh_token":"xai-new-refresh","token_type":` +
										`"Bearer","expires_in":` +
										`3600}`),
								); err !=
									nil {
									t.Error(err)
								}
							case "/billing":
								upstreamCalls.Add(1)
								if r.Header.Get("Authorization") != "Bearer xai-fresh-concurrent" {
									t.Error("billing request did not use the refreshed access token")
									w.WriteHeader(http.StatusUnauthorized)
									return
								}
								if _, err := w.Write([]byte(`{"ok":true}`)); err != nil {
									t.Error(err)
								}
							default:
								t.Errorf("unexpected synthetic xAI path %q", r.URL.Path)
								w.WriteHeader(http.StatusNotFound)
							}
						},
					),
				)
				defer server.Close()
				installAPICallTLSTestTransport(t, server)
				store := &memoryAuthStore{}
				manager := coreauth.NewManager(store, nil, nil)
				expired := time.Now().Add(-time.Hour).Format(time.RFC3339)
				auth := &coreauth.Auth{
					ID: test.id, FileName: test.id, Provider: "xai",
					Attributes: map[string]string{"auth_kind": "oauth", "base_url": "https://api.x.ai/v1"},
					Metadata: map[string]any{
						"type": "xai", "auth_kind": "oauth", "access_token": test.accessToken,
						"refresh_token": test.refreshToken, "token_endpoint": "https://auth.x.ai/oauth2/token",
						"expired": expired,
					},
				}
				if !test.failure {
					auth.Storage = &xaiauth.TokenStorage{
						Type: "xai", AuthKind: "oauth", AccessToken: test.accessToken,
						RefreshToken: test.refreshToken, TokenEndpoint: "https://auth.x.ai/oauth2/token",
						Expire: expired,
					}
				}
				registered, err := manager.Register(t.Context(), auth)
				if err != nil {
					t.Fatal(err)
				}
				h := &Handler{cfg: &config.Config{}, authManager: manager, tokenStore: store}
				var workers sync.WaitGroup
				start := make(chan struct{})
				for range test.concurrency {
					workers.Go(
						func() {
							<-start
							body := fmt.Sprintf(
								`{"authIndex":%q,"method":"GET","url":"https://api.x.ai/billing",`+
									`"header":{"Authorization":"Bearer `+
									`$TOKEN$"}}`,
								registered.Index,
							)
							recorder := runConfigListRequest(h.APICall, http.MethodPost, "/api-call", body)
							if test.failure {
								if recorder.Code != http.StatusBadRequest ||
									recorder.Body.String() != `{"error":"auth token refresh failed"}` ||
									strings.Contains(
										recorder.Body.String(), test.refreshToken,
									) {
									t.Errorf("refresh failure = %d %s", recorder.Code, recorder.Body.String())
								}
								return
							}
							var response apiCallResponse
							if err := json.Unmarshal(
								recorder.Body.Bytes(), &response,
							); err !=
								nil ||
								recorder.Code !=
									http.StatusOK ||
								response.StatusCode !=
									http.StatusOK ||
								response.Body !=
									`{"ok":true}` {
								t.Errorf(
									"refreshed request = %d %s, error = %v",
									recorder.Code,
									recorder.Body.String(),
									err,
								)
							}
						},
					)
				}
				close(start)
				workers.Wait()
				if refreshCalls.Load() == 0 {
					t.Fatal("original expiry did not cause a refresh")
				}
				current, _ := manager.GetByID(registered.ID)
				if test.failure {
					if upstreamCalls.Load() != 0 || current.Metadata["access_token"] != test.accessToken ||
						current.Metadata["refresh_token"] != test.refreshToken {
						t.Fatal("failed refresh sent a billing request or changed the original credential")
					}
					return
				}
				if upstreamCalls.Load() != int32(test.concurrency) ||
					current.Metadata["access_token"] != "xai-fresh-concurrent" ||
					current.Metadata["refresh_token"] != "xai-new-refresh" {
					t.Fatal("concurrent refresh did not deliver every request with the published token pair")
				}
				store.mu.Lock()
				persisted := store.items[registered.ID].Clone()
				store.mu.Unlock()
				storage, ok := persisted.Storage.(*xaiauth.TokenStorage)
				if !ok || storage.AccessToken != "xai-fresh-concurrent" ||
					storage.RefreshToken != "xai-new-refresh" {
					t.Fatalf("durable refreshed storage = %+v", persisted.Storage)
				}
			},
		)
	}
}

func TestResolveMetaTokenUsesRequestProxyWithoutSavingOverride(t *testing.T) {
	var mintCalls, proxyCalls atomic.Int32
	server := httptest.NewTLSServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Connection", "close")
				if r.URL.Path != "/muse-code/key" || r.Header.Get("Authorization") != "Bearer dca:proxy-test" {
					t.Error("original Meta DCA did not reach the mint endpoint")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mintCalls.Add(1)
				if _, err := w.Write([]byte(`{"api_key":"LLM|proxied"}`)); err != nil {
					t.Error(err)
				}
			},
		),
	)
	defer server.Close()
	proxy := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect || r.Host != "api.meta.ai:443" {
					t.Errorf("unexpected Meta proxy request %s %s", r.Method, r.Host)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				proxyCalls.Add(1)
				upstream, err := (&net.Dialer{Timeout: time.Second}).DialContext(
					r.Context(), "tcp", server.Listener.Addr().String(),
				)
				if err != nil {
					t.Error(err)
					return
				}
				defer func(closeResource func() error) { _ = closeResource() }(upstream.Close)
				client, buffer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer func(closeResource func() error) { _ = closeResource() }(client.Close)
				if _, err := buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					t.Error(err)
					return
				}
				if err := buffer.Flush(); err != nil {
					t.Error(err)
					return
				}
				go func() {
					defer func(closeResource func() error) { _ = closeResource() }(client.Close)
					_, _ = io.Copy(client, upstream)
				}()
				_, _ = io.Copy(upstream, buffer)
			},
		),
	)
	defer proxy.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = server.Certificate().DNSNames[0]
	previousTransport := http.DefaultTransport
	http.DefaultTransport = transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth, err := manager.Register(
		coreauth.WithSkipPersist(t.Context()), &coreauth.Auth{
			ID: "meta-proxy.json", Provider: "meta", ProxyURL: "http://127.0.0.1:1",
			Metadata: map[string]any{"dca_token": "dca:proxy-test"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{}, authManager: manager, tokenStore: store}
	token, err := h.resolveMetaToken(t.Context(), h.cfg, auth.Clone(), proxy.URL)
	if err != nil || token != "LLM|proxied" || mintCalls.Load() != 1 || proxyCalls.Load() != 1 {
		t.Fatalf(
			"Meta request proxy result = %q, error = %v, mint = %d, proxy = %d", token, err, mintCalls.Load(),
			proxyCalls.Load(),
		)
	}
	current, _ := manager.GetByID(auth.ID)
	store.mu.Lock()
	persisted := store.items[auth.ID].Clone()
	store.mu.Unlock()
	if current.ProxyURL != auth.ProxyURL || persisted.ProxyURL != auth.ProxyURL ||
		current.Metadata["dca_token"] != "dca:proxy-test" ||
		persisted.Metadata["api_key"] != "LLM|proxied" {
		t.Fatal("temporary request proxy changed durable proxy or credential identity")
	}
}
