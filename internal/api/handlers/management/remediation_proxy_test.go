package management

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestConfiguredAPICallProxyErrorsNeverConnectDirectly(t *testing.T) {
	originalTransport := http.DefaultTransport
	var connections atomic.Int32
	http.DefaultTransport = &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			connections.Add(1)
			return nil, errors.New("test network access blocked")
		},
	}
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	defer func() {
		if connections.Load() != 0 {
			t.Fatalf("invalid proxy attempted %d network connections", connections.Load())
		}
	}()
	for _, kind := range []string{"invalid", "socks4"} {
		for _, scope := range []string{"global", "credential", "request"} {
			t.Run(
				kind+"/"+scope, func(t *testing.T) {
					invalidProxy := "invalid-" + scope + "-proxy"
					if kind == "socks4" {
						invalidProxy = "socks4://127.0.0.1:1080"
					}
					cfg := &config.Config{}
					auth := &coreauth.Auth{
						ID: "proxy.json", Provider: "codex", Metadata: map[string]any{
							"access_token": "synthetic-access", "refresh_token": "synthetic-refresh",
							"expired": "2000-01-01T00:00:00Z",
						},
					}
					requestProxy := ""
					switch scope {
					case "global":
						cfg.ProxyURL = invalidProxy
					case "credential":
						cfg.ProxyURL = "http://127.0.0.1:8080"
						auth.ProxyURL = invalidProxy
					case "request":
						cfg.ProxyURL = "http://127.0.0.1:8080"
						auth.ProxyURL = "http://127.0.0.1:8081"
						requestProxy = invalidProxy
					}
					h := &Handler{cfg: cfg}
					req, err := http.NewRequest(http.MethodGet, "https://8.8.8.8/test", nil)
					if err != nil {
						t.Fatal(err)
					}
					if response, err := h.apiCallTransport(
						cfg, auth, requestProxy,
					).RoundTrip(req); err == nil || response != nil || !strings.Contains(
						err.Error(), "invalid configured proxy",
					) {
						t.Fatalf("invalid proxy transport response=%v error=%v", response, err)
					}
					if token, err := h.resolveTokenForAuthWithProxy(
						context.Background(), cfg, auth, requestProxy,
					); err == nil || token != "" || !strings.Contains(err.Error(), "invalid configured proxy") {
						t.Fatalf("refresh token=%q error=%v", token, err)
					}
					manager := coreauth.NewManager(&memoryAuthStore{}, nil, nil)
					registered, err := manager.Register(context.Background(), auth)
					if err != nil {
						t.Fatal(err)
					}
					h.authManager = manager
					payload := (`{"method":"GET","url":"https://8.8.8.8/test","auth_index":` +
						`"`) +
						registered.Index +
						(`","proxy_url":` +
							`"`) +
						requestProxy +
						`"}`
					recorder := runConfigListRequest(h.APICall, http.MethodPost, "/api-call", payload)
					if recorder.Code != http.StatusBadRequest ||
						!strings.Contains(recorder.Body.String(), "proxy") {
						t.Fatalf("APICall status=%d body=%s", recorder.Code, recorder.Body.String())
					}
				},
			)
		}
	}
}
