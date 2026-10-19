package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
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
		&config.Config{ProxyURL: "http://8.8.4.4:8080"},
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
		&config.Config{ProxyURL: "http://8.8.4.4:8080"},
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

func TestNewProxyAwareHTTPClientRequestOverride(t *testing.T) {
	ctx := util.WithRoundTripper(context.Background(), proxyTestRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("injected transport must not override the execution proxy")
		return nil, nil
	}))
	ctx = cliproxyexecutor.WithRequestProxyURL(ctx, "http://request-proxy.example:8081")
	client := newProxyAwareHTTPClient(
		ctx,
		&config.Config{ProxyURL: "http://global-proxy.example:8082"},
		&cliproxyauth.Auth{ProxyURL: "http://auth-proxy.example:8080"},
		0,
	)
	transport, ok := unwrapPublicHostTransport(client.Transport).(*http.Transport)
	if !ok || transport.Proxy == nil {
		t.Fatalf("transport = %T, want HTTP proxy transport", client.Transport)
	}
	req, err := http.NewRequest(http.MethodGet, "http://target.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := transport.Proxy(req)
	if err != nil || proxy == nil || proxy.String() != "http://request-proxy.example:8081" {
		t.Fatalf("proxy = %v, err = %v; want request override", proxy, err)
	}

	refreshClient := newProxyAwareHTTPClient(
		cliproxyexecutor.WithoutRequestProxyURL(ctx), nil,
		&cliproxyauth.Auth{ProxyURL: "http://auth-proxy.example:8080"}, 0,
	)
	refreshTransport, ok := unwrapPublicHostTransport(refreshClient.Transport).(*http.Transport)
	if !ok || refreshTransport.Proxy == nil {
		t.Fatalf("refresh transport = %T, want auth proxy", refreshClient.Transport)
	}
	refreshProxy, err := refreshTransport.Proxy(req)
	if err != nil || refreshProxy == nil || refreshProxy.String() != "http://auth-proxy.example:8080" {
		t.Fatalf("refresh proxy = %v, err = %v; want auth proxy", refreshProxy, err)
	}
}

func TestNewDevinHTTPClientRequestOverride(t *testing.T) {
	ctx := util.WithRoundTripper(context.Background(), proxyTestRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("Devin request override must bypass injected transport")
		return nil, nil
	}))
	ctx = cliproxyexecutor.WithRequestProxyURL(ctx, "http://devin-request.example:8081")
	client := newDevinHTTPClient(ctx, &config.Config{ProxyURL: "direct"}, &cliproxyauth.Auth{ProxyURL: "direct"}, 0)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy == nil || !transport.DisableCompression {
		t.Fatalf("Devin transport = %T, want no-gzip proxy transport", client.Transport)
	}
	req, err := http.NewRequest(http.MethodGet, "http://target.example", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := transport.Proxy(req)
	if err != nil || proxy == nil || proxy.String() != "http://devin-request.example:8081" {
		t.Fatalf("Devin proxy = %v, err = %v; want request override", proxy, err)
	}
}

func (f proxyTestRoundTripper) RoundTripperFor(*cliproxyauth.Auth) http.RoundTripper { return f }

func TestManagerRoundTripperReachesActualExecutor(t *testing.T) {
	for _, useContext := range []bool{false, true} {
		t.Run(fmt.Sprint(useContext), func(t *testing.T) {
			calls := 0
			transport := proxyTestRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusOK, Request: req, Header: http.Header{"Content-Type": {"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"id":"memory","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"memory"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))}, nil
			})
			executor := NewOpenAICompatExecutor("transport-contract", &config.Config{})
			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetRetryConfig(0, 0, 1)
			manager.RegisterExecutor(executor)
			manager.SetRoundTripperProvider(transport)
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: executor.Identifier(), Status: cliproxyauth.StatusActive,
				Attributes: map[string]string{"base_url": "http://127.0.0.1:1/v1", "api_key": "synthetic-transport-key"}}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			const model = "transport-contract-model"
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			ctx := context.Background()
			if useContext {
				ctx = util.WithRoundTripper(ctx, transport)
			}
			response, err := manager.Execute(ctx, []string{auth.Provider}, cliproxyexecutor.Request{
				Model: model, Payload: []byte(`{"model":"transport-contract-model","messages":[{"role":"user","content":"same synthetic payload"}]}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
			if err != nil || calls != 1 || !strings.Contains(string(response.Payload), "memory") {
				t.Fatalf("Execute() = %s, %v, transport calls = %d", response.Payload, err, calls)
			}
		})
	}
}
