package pluginhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
)

func TestHostHTTPClientOrderedProxyPriority(t *testing.T) {
	const requestProxy = "https://request.invalid:18443"
	const credentialProxy = "https://credential.invalid:18444"
	const globalProxy = "https://global.invalid:18445"
	//goland:noinspection HttpUrlsUsage
	const invalidCredentialProxy = "http://proxy.invalid:bad"
	//goland:noinspection HttpUrlsUsage
	const invalidGlobalProxy = "http://proxy.invalid:%zz"
	for _, test := range []struct {
		name, request, auth, global, contextProxy, want string
	}{
		{
			name: "request over credential and global", request: requestProxy, auth: credentialProxy,
			global: globalProxy, want: "request.invalid:18443",
		},
		{name: "request over global", request: requestProxy, global: globalProxy, want: "request.invalid:18443"},
		{name: "request only", request: requestProxy, want: "request.invalid:18443"},
		{
			name: "request over invalid lower priority", request: requestProxy, auth: invalidCredentialProxy,
			global: invalidGlobalProxy, want: "request.invalid:18443",
		},
		{name: "credential over global", auth: credentialProxy, global: globalProxy, want: "credential.invalid:18444"},
		{name: "global only", global: globalProxy, want: "global.invalid:18445"},
		{name: "context proxy", contextProxy: "https://context.invalid:18446", want: "context.invalid:18446"},
		{
			name: "request direct", request: "direct", auth: credentialProxy, global: globalProxy,
			contextProxy: "https://context.invalid:18446", want: "target.invalid:443",
		},
		{name: "request none", request: "none", auth: credentialProxy, global: globalProxy, want: "target.invalid:443"},
		{name: "credential direct", auth: "direct", global: globalProxy, want: "target.invalid:443"},
	} {
		for _, scheme := range []string{"https", "http"} {
			if strings.HasPrefix(test.want, "target.") && scheme == "http" {
				continue
			}
			t.Run(
				test.name+"/"+scheme, func(t *testing.T) {
					stop := errors.New("fixture TLS dial stopped before network")
					selected := ""
					base := &http.Transport{
						DialTLSContext: func(_ context.Context, _, address string) (net.Conn, error) {
							selected = address
							return nil, stop
						},
					}
					if test.contextProxy != "" {
						proxyURL, err := url.Parse(test.contextProxy)
						if err != nil {
							t.Fatal(err)
						}
						base.Proxy = http.ProxyURL(proxyURL)
					}
					ctx := util.WithRoundTripper(context.Background(), base)
					cfg := &config.Config{ProxyURL: test.global}
					host := New()
					host.runtimeConfig = cfg
					client := host.newHTTPClientWithProxy(
						&coreauth.Auth{ProxyURL: test.auth}, test.request, "synthetic",
					)
					_, err := client.Do(
						ctx, pluginapi.HTTPRequest{
							URL:         scheme + "://target.invalid:443/test",
							WireProfile: &pluginapi.HTTPWireProfile{HeaderProfile: []string{"Host", "User-Agent"}},
						},
					)
					if !errors.Is(err, stop) {
						t.Fatalf("request error = %v, want local TLS stop", err)
					}
					if selected != test.want {
						t.Fatalf("selected proxy address = %q, want %q", selected, test.want)
					}
					if base.TLSClientConfig != nil || base.Proxy != nil && test.contextProxy == "" {
						t.Fatal("caller transport was modified")
					}
				},
			)
		}
	}
}

func TestHostHTTPClientHTTP1ProtocolSet(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("explicit protocols=%t", explicit), func(t *testing.T) {
				stop := errors.New("fixture dial stopped before network")
				base := &http.Transport{
					DialContext: func(context.Context, string, string) (net.Conn, error) {
						return nil, stop
					},
				}
				if explicit {
					base.Protocols = &http.Protocols{}
					base.Protocols.SetHTTP1(true)
					base.Protocols.SetHTTP2(true)
				}
				ctx := util.WithRoundTripper(context.Background(), base)
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://protocol.invalid/test", nil)
				if err != nil {
					t.Fatal(err)
				}
				client := New().newHTTPClient(nil).(*hostHTTPClient)
				built, cleanup, err := client.newHTTPClientForRequest(
					ctx, nil, pluginapi.HTTPRequest{WireProfile: &pluginapi.HTTPWireProfile{HTTP1Only: true}}, request,
				)
				if err != nil {
					t.Fatal(err)
				}
				defer cleanup()
				if _, err := built.Do(request); !errors.Is(err, stop) {
					t.Fatalf("request error = %v, want local dial stop", err)
				}
				transport := built.Transport.(*http.Transport)
				if transport.TLSNextProto["h2"] != nil || transport.Protocols != nil && transport.Protocols.HTTP2() {
					t.Fatal("HTTP1Only transport enables HTTP/2")
				}
				if explicit && !base.Protocols.HTTP2() {
					t.Fatal("caller transport protocols were modified")
				}
			},
		)
	}
}

type proxyRejectionTransport struct{ calls atomic.Int32 }

func (transport *proxyRejectionTransport) RoundTrip(*http.Request) (*http.Response, error) {
	transport.calls.Add(1)
	return nil, errors.New("unexpected fallback transport")
}

func TestProxyRejectionRedactsCredentialsAtClaudeConsumer(t *testing.T) {
	//goland:noinspection HttpUrlsUsage
	const invalidPortProxy = "http://fixture-user:%s@proxy.invalid:bad"
	//goland:noinspection HttpUrlsUsage
	const invalidHostProxy = "http://fixture-user:%s@proxy%%zz.invalid:8080"
	for _, sample := range []struct{ name, format string }{
		{name: "invalid port", format: invalidPortProxy},
		{name: "invalid host escape", format: invalidHostProxy},
		{name: "unsupported scheme control", format: "file:///fixture-%s"},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				const marker = "fixture-private-proxy-marker"
				var captured bytes.Buffer
				logger := log.StandardLogger()
				previous := logger.Out
				logger.SetOutput(&captured)
				t.Cleanup(func() { logger.SetOutput(previous) })
				transport := &proxyRejectionTransport{}
				ctx := util.WithRoundTripper(context.Background(), transport)
				ctx = cliproxyexecutor.WithRequestProxyURL(ctx, fmt.Sprintf(sample.format, marker))
				request, err := http.NewRequestWithContext(
					ctx, http.MethodGet, "https://example.invalid/proxy-error-fixture", nil,
				)
				if err != nil {
					t.Fatal(err)
				}
				response, err := executor.NewClaudeExecutor(nil).HttpRequest(ctx, nil, request)
				if response != nil {
					_ = response.Body.Close()
				}
				if err == nil || transport.calls.Load() != 0 {
					t.Fatalf("proxy rejection: error=%v transport calls=%d", err, transport.calls.Load())
				}
				for _, secret := range []string{marker, "fixture-user"} {
					if strings.Contains(err.Error(), secret) || strings.Contains(captured.String(), secret) {
						t.Fatal("proxy credential exposed in returned error or formatted server log")
					}
				}
				if !strings.Contains(captured.String(), "proxy URL rejected") {
					t.Fatalf("proxy failure was not visible in server log: %s", captured.String())
				}
			},
		)
	}
}
