package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestIFlowRefreshHonorsProxyPrecedenceAndDirect(t *testing.T) {
	for _, branch := range []string{"oauth", "cookie"} {
		for _, selection := range []string{
			"global", "credential", "credential-without-global", "request", "direct", "none",
		} {
			t.Run(
				branch+"/"+selection, func(t *testing.T) {
					original := http.DefaultTransport
					denied := make(chan string, 4)
					base := &http.Transport{
						DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
							host, _, err := net.SplitHostPort(address)
							if err != nil {
								return nil, err
							}
							if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
								return (&net.Dialer{}).DialContext(ctx, network, address)
							}
							denied <- address
							return nil, errors.New("synthetic refresh direct network access denied")
						},
					}
					http.DefaultTransport = base
					t.Cleanup(func() { http.DefaultTransport = original; base.CloseIdleConnections() })
					hits := make(chan string, 4)
					proxy := func(name string) *httptest.Server {
						return httptest.NewServer(
							http.HandlerFunc(
								func(w http.ResponseWriter, request *http.Request) {
									hits <- name + "/" + request.Method + "/" + request.Host
									w.WriteHeader(http.StatusForbidden)
								},
							),
						)
					}
					global, credential, requestProxy := proxy("global"), proxy("credential"), proxy("request")
					defer global.Close()
					defer credential.Close()
					defer requestProxy.Close()
					cfg := &config.Config{ProxyURL: global.URL}
					auth := &coreauth.Auth{
						ID: "synthetic-iflow-" + branch + "-" + selection, Provider: "iflow",
						Metadata: map[string]any{"refresh_token": "synthetic-refresh-token"},
					}
					if branch == "cookie" {
						auth.Metadata = map[string]any{
							"cookie": "synthetic-cookie", "email": "synthetic@example.invalid",
							"expired": "2000-01-01T00:00:00Z",
						}
					}
					ctx, cancel := context.WithTimeout(t.Context(), time.Second)
					defer cancel()
					want := "global"
					if selection != "global" {
						auth.ProxyURL, want = " "+credential.URL+" ", "credential"
					}
					if selection == "credential-without-global" {
						cfg.ProxyURL = ""
					} else if selection == "request" {
						ctx = coreexecutor.WithRequestProxyURL(ctx, requestProxy.URL)
						want = "request"
					} else if selection == "direct" || selection == "none" {
						auth.ProxyURL, want = selection, "direct"
					}
					originalProxy := cfg.ProxyURL
					updated, err := NewIFlowExecutor(cfg).Refresh(ctx, auth)
					if err == nil || updated != nil {
						t.Fatalf(
							"rejecting proxy/dial fixture returned success: updated=%t error=%v",
							updated != nil,
							err,
						)
					}
					host := "iflow.cn:443"
					if branch == "cookie" {
						host = "platform.iflow.cn:443"
					}
					if want == "direct" {
						select {
						case address := <-denied:
							if address != host {
								t.Errorf("direct refresh dial=%q, want %q", address, host)
							}
						default:
							t.Error("explicit direct/none did not select the direct transport")
						}
						if len(hits) != 0 {
							t.Error("explicit direct/none inherited a configured proxy")
						}
					} else {
						select {
						case hit := <-hits:
							if hit != want+"/CONNECT/"+host {
								t.Errorf("refresh proxy=%q, want %s/CONNECT/%s", hit, want, host)
							}
						default:
							t.Error("selected refresh proxy received no request")
						}
						if len(denied) != 0 {
							t.Error("proxy-scoped refresh attempted a direct connection")
						}
					}
					if cfg.ProxyURL != originalProxy || auth.Metadata["access_token"] != nil ||
						auth.Metadata["api_key"] != nil {
						t.Error("failed refresh changed shared config or credential state")
					}
				},
			)
		}
	}
}

func TestIFlowRefreshUsesContextTransportForAllRequests(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = proxyTestRoundTripper(
		func(*http.Request) (*http.Response, error) {
			return nil, errors.New("synthetic default transport rejects refresh egress")
		},
	)
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, branch := range []string{"oauth", "cookie"} {
		t.Run(
			branch, func(t *testing.T) {
				calls := 0
				transport := &auditClaudeContractTransport{
					respond: func(request *http.Request) (*http.Response, error) {
						calls++
						body := `{"access_token":"synthetic-access","refresh_token":` +
							`"synthetic-rotated","expires_in":` +
							`3600}`
						if branch == "cookie" {
							if request.Method != http.MethodPost ||
								request.URL.String() != "https://platform.iflow.cn/api/openapi/apikey" {
								t.Fatalf("unexpected cookie refresh route %s %s", request.Method, request.URL)
							}
							body = `{"success":true,"data":{"apiKey":"synthetic-api-key","expireTime":` +
								`"2099-01-01T00:00:` +
								`00Z"}}`
						} else if request.Method ==
							http.MethodGet &&
							request.URL.Host ==
								"iflow.cn" &&
							request.URL.Path ==
								"/api/oauth/getUserInfo" {
							body = `{"success":true,"data":{"apiKey":"synthetic-api-key","email":` +
								`"synthetic@example.invalid"}}`
						} else if request.Method !=
							http.MethodPost ||
							request.URL.String() !=
								"https://iflow.cn/oauth/token" {
							t.Fatalf("unexpected OAuth refresh route %s %s", request.Method, request.URL)
						}
						return &http.Response{
							StatusCode: http.StatusOK, Header: make(http.Header),
							Body: io.NopCloser(strings.NewReader(body)), Request: request,
						}, nil
					},
				}
				ctx := util.WithRoundTripper(t.Context(), transport)
				cfg := &config.Config{}
				auth := &coreauth.Auth{
					ID: "synthetic-iflow-context-" + branch, Provider: "iflow",
					Metadata: map[string]any{"refresh_token": "synthetic-old-refresh"},
				}
				wantCalls := 2
				if branch == "cookie" {
					auth.Metadata = map[string]any{
						"cookie": "synthetic-cookie", "email": "synthetic@example.invalid",
						"expired": "2000-01-01T00:00:00Z",
					}
					wantCalls = 1
				}
				updated, err := NewIFlowExecutor(cfg).Refresh(ctx, auth)
				if err != nil || updated == nil || calls != wantCalls ||
					updated.Attributes["api_key"] != "synthetic-api-key" {
					t.Fatalf("context refresh calls=%d updated=%t error=%v", calls, updated != nil, err)
				}
				cfg.Home.Enabled = true
				homeClient := &testHomeRefreshClient{
					heartbeat: true, payload: []byte(fmt.Sprintf(
						`{"auth":{"provider":"iflow","metadata":{"access_token":"synthetic-home-%s"}}}`, branch,
					)),
				}
				ctx = coreauth.WithHomeRefreshClient(ctx, homeClient)
				updated, err = NewIFlowExecutor(cfg).Refresh(ctx, auth)
				if err != nil || updated == nil || metaStringValue(
					updated.Metadata, "access_token",
				) != "synthetic-home-"+branch || calls != wantCalls {
					t.Fatalf("Home refresh changed local transport ownership: calls=%d error=%v", calls, err)
				}
			},
		)
	}
}
