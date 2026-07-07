package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

type proxyTestRoundTripper func(*http.Request) (*http.Response, error)

type baseTransportGetter interface {
	BaseTransport() http.RoundTripper
}

func (f proxyTestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func unwrapPublicHostTransport(rt http.RoundTripper) http.RoundTripper {
	if getter, ok := rt.(baseTransportGetter); ok {
		return getter.BaseTransport()
	}
	return rt
}

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := newProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://8.8.4.4:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport, ok := unwrapPublicHostTransport(client.Transport).(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("direct transport must disable proxy function")
	}
}

func TestNewProxyAwareHTTPClientHTTPProxySetsTransport(t *testing.T) {
	t.Parallel()

	client := newProxyAwareHTTPClient(
		context.Background(),
		&config.Config{},
		&cliproxyauth.Auth{ProxyURL: "http://8.8.8.8:3128"},
		0,
	)

	transport, ok := unwrapPublicHostTransport(client.Transport).(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy == nil {
		t.Fatal("expected proxy function to be set for HTTP proxy URL")
	}
}

func TestNewProxyAwareHTTPClientGlobalProxyFallback(t *testing.T) {
	t.Parallel()

	client := newProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://8.8.4.4:8080"}},
		&cliproxyauth.Auth{},
		0,
	)

	transport, ok := unwrapPublicHostTransport(client.Transport).(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy == nil {
		t.Fatal("expected proxy function from global config")
	}
}

func TestNewProxyAwareHTTPClientDefaultLeavesTransportUnset(t *testing.T) {
	t.Parallel()

	client := newProxyAwareHTTPClient(context.Background(), &config.Config{}, &cliproxyauth.Auth{}, 0)
	if client.Transport != nil {
		t.Fatalf("transport type = %T, want nil default transport", client.Transport)
	}
}

func TestNewProxyAwareHTTPClientUsesContextTransportForLocalTarget(t *testing.T) {
	t.Parallel()

	called := false
	ctx := util.WithRoundTripper(
		context.Background(),
		proxyTestRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				called = true
				if req.URL.Hostname() != "localhost" {
					t.Fatalf("hostname = %q, want localhost", req.URL.Hostname())
				}
				return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: req}, nil
			},
		),
	)
	client := newProxyAwareHTTPClient(ctx, &config.Config{}, &cliproxyauth.Auth{}, 0)
	req, errRequest := http.NewRequest(http.MethodGet, "http://localhost:11434/v1/models", nil)
	if errRequest != nil {
		t.Fatalf("NewRequest returned error: %v", errRequest)
	}
	resp, errRoundTrip := client.Do(req)
	if errRoundTrip != nil {
		t.Fatalf("local target request returned error: %v", errRoundTrip)
	}
	_ = resp.Body.Close()
	if !called {
		t.Fatal("expected context transport to be called")
	}
}

func TestNewProxyAwareHTTPClientInvalidProxyDoesNotFallback(t *testing.T) {
	t.Parallel()

	called := false
	ctx := util.WithRoundTripper(
		context.Background(),
		proxyTestRoundTripper(
			func(*http.Request) (*http.Response, error) {
				called = true
				return nil, nil
			},
		),
	)
	client := newProxyAwareHTTPClient(
		ctx,
		&config.Config{},
		&cliproxyauth.Auth{ProxyURL: "file:///tmp/socket"},
		0,
	)
	if client.Transport == nil {
		t.Fatal("expected rejecting transport")
	}
	req, err := http.NewRequest(http.MethodGet, "https://8.8.8.8", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	_, err = client.Transport.RoundTrip(req)
	if err == nil {
		t.Fatal("expected invalid proxy to reject request")
	}
	if called {
		t.Fatal("context transport should not be called when proxy URL is invalid")
	}
}
