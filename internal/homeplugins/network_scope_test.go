package homeplugins

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	internalpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginstore"
	sdkpluginstore "github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginstore"
)

type networkScopeDoer func(*http.Request) (*http.Response, error)

func (do networkScopeDoer) Do(request *http.Request) (*http.Response, error) {
	return do(request)
}

func TestPluginStoreClientSharesProxyCooldown(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()

	const tokenEnv = "HOME_PLUGIN_NETWORK_SCOPE_TEST_TOKEN"
	t.Setenv(tokenEnv, t.Name())
	auth := []sdkpluginstore.AuthConfig{{
		Match: "https://api.github.com/", Type: sdkpluginstore.AuthTypeGitHubToken,
		TokenEnv: tokenEnv,
	}}
	plugin := sdkpluginstore.Plugin{Repository: "https://github.com/test/home-network-scope"}
	seed := sdkpluginstore.NewClientWithAuth(
		networkScopeDoer(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Retry-After": []string{"3600"}},
				Body:       io.NopCloser(strings.NewReader("limited")),
			}, nil
		}),
		"",
		auth,
	).WithNetworkScope(proxy.URL)
	_, errRelease := seed.FetchLatestRelease(context.Background(), plugin)
	var rateLimit *internalpluginstore.RateLimitError
	if !errors.As(errRelease, &rateLimit) {
		t.Fatalf("seed cooldown: %v", errRelease)
	}

	cfg := &config.Config{ProxyURL: " " + proxy.URL + " "}
	cfg.Plugins.StoreAuth = auth
	_, errRelease = newPluginStoreClient(cfg).FetchLatestRelease(context.Background(), plugin)
	if !errors.As(errRelease, &rateLimit) {
		t.Fatalf("Home client did not inherit proxy cooldown: %v", errRelease)
	}
	if proxyCalls.Load() != 0 {
		t.Fatalf("cooldown made %d proxy requests, want 0", proxyCalls.Load())
	}
}
