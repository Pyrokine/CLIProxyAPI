package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	// "direct" is not a valid proxy URL scheme (no "://"), so buildProxyTransport
	// returns nil and the client falls through to context RoundTripper (also nil).
	// The result is a client with no transport, effectively bypassing any proxy.
	client := newProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	if client.Transport != nil {
		t.Fatalf("expected nil transport for unrecognized proxy URL %q, got %T", "direct", client.Transport)
	}
}

func TestNewProxyAwareHTTPClientHTTPProxySetsTransport(t *testing.T) {
	t.Parallel()

	client := newProxyAwareHTTPClient(
		context.Background(),
		&config.Config{},
		&cliproxyauth.Auth{ProxyURL: "http://proxy.example.com:3128"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
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
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy == nil {
		t.Fatal("expected proxy function from global config")
	}
}
