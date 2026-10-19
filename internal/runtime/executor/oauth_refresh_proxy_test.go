package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestOAuthRefreshUsesCredentialProxy(t *testing.T) {
	for _, provider := range []string{"codex", "kimi", "kimi-ai"} {
		for _, selection := range []string{"global", "credential", "credential-without-global", "request"} {
			t.Run(provider+"/"+selection, func(t *testing.T) {
				hits := make(chan string, 4)
				proxy := func(name string) *httptest.Server {
					return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						hits <- name + "/" + r.Method + "/" + r.Host
						w.WriteHeader(http.StatusForbidden)
					}))
				}
				global := proxy("global")
				defer global.Close()
				credential := proxy("credential")
				defer credential.Close()
				request := proxy("request")
				defer request.Close()
				cfg := &config.Config{ProxyURL: global.URL}
				auth := &cliproxyauth.Auth{
					ID:       provider + "-" + selection,
					Provider: provider,
					Metadata: map[string]any{"refresh_token": "fixture-" + provider + "-" + selection},
				}
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				defer cancel()
				want := "global"
				if selection != "global" {
					auth.ProxyURL = " " + credential.URL + " "
					want = "credential"
				}
				if selection == "credential-without-global" {
					cfg.ProxyURL = ""
				}
				if selection == "request" {
					ctx = cliproxyexecutor.WithRequestProxyURL(ctx, request.URL)
					want = "request"
				}
				originalProxy := cfg.ProxyURL
				var err error
				if provider == "codex" {
					_, err = newCodexExecutor(cfg).Refresh(ctx, auth)
				} else {
					_, err = NewKimiExecutor(cfg).Refresh(ctx, auth)
				}
				if err == nil {
					t.Fatal("rejecting fixture proxy returned success")
				}
				wantHost := "auth.openai.com:443"
				if provider != "codex" {
					wantHost = "auth.kimi.com:443"
					if provider == "kimi-ai" {
						wantHost = "auth.kimi.ai:443"
					}
				}
				select {
				case hit := <-hits:
					if hit != want+"/CONNECT/"+wantHost {
						t.Fatalf("proxy dispatch = %q, want %s/CONNECT/%s", hit, want, wantHost)
					}
				default:
					t.Fatal("selected proxy did not receive the refresh request")
				}
				if cfg.ProxyURL != originalProxy || auth.Metadata["access_token"] != nil {
					t.Fatal("failed refresh changed shared configuration or credential metadata")
				}
			})
		}
	}
}

func TestOAuthRefreshUsesContextTransportAndPreservesHome(t *testing.T) {
	for _, provider := range []string{"codex", "kimi", "kimi-ai"} {
		t.Run(provider, func(t *testing.T) {
			calls := 0
			roundTripper := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodPost || !strings.Contains(req.URL.Path, "token") {
					t.Fatalf("unexpected refresh request: %s %s", req.Method, req.URL.Path)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"access_token":"fixture-access","refresh_token":"fixture-rotated","expires_in":3600}`)),
					Request:    req,
				}, nil
			})
			ctx := util.WithRoundTripper(context.Background(), roundTripper)
			cfg := &config.Config{}
			auth := &cliproxyauth.Auth{ID: "fixture-" + provider, Provider: provider, Metadata: map[string]any{"refresh_token": "fixture-context-" + provider}}
			refresh := func(ctx context.Context) (*cliproxyauth.Auth, error) {
				if provider == "codex" {
					return newCodexExecutor(cfg).Refresh(ctx, auth)
				}
				return NewKimiExecutor(cfg).Refresh(ctx, auth)
			}
			updated, err := refresh(ctx)
			if err != nil || updated == nil || metaStringValue(updated.Metadata, "access_token") != "fixture-access" || calls != 1 {
				t.Fatalf("refresh via context transport: calls=%d updated=%v err=%v", calls, updated != nil, err)
			}

			cfg.Home.Enabled = true
			homeClient := &testHomeRefreshClient{heartbeat: true, payload: []byte(fmt.Sprintf(`{"auth":{"provider":%q,"metadata":{"access_token":"fixture-home"}}}`, provider))}
			ctx = cliproxyauth.WithHomeRefreshClient(ctx, homeClient)
			updated, err = refresh(ctx)
			if err != nil || updated == nil || metaStringValue(updated.Metadata, "access_token") != "fixture-home" || calls != 1 {
				t.Fatalf("Home refresh: calls=%d updated=%v err=%v", calls, updated != nil, err)
			}
		})
	}
}
