package executor

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

// newProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use the execution-scoped proxy override when present
// 2. Use auth.ProxyURL, then cfg.ProxyURL
// 3. Use RoundTripper from context when no proxy is configured
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func newProxyAwareHTTPClient(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	timeout time.Duration,
) *http.Client {
	return NewProxyAwareHTTPClient(ctx, cfg, auth, timeout)
}

type proxyErrorTransport struct {
	err error
}

func (t proxyErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

func NewProxyAwareHTTPClient(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	timeout time.Duration,
) *http.Client {
	httpClient := &http.Client{}
	if timeout > 0 {
		httpClient.Timeout = timeout
	}

	proxyURL := effectiveProxyURL(ctx, cfg, auth)

	// If we have a proxy URL configured, use either its transport or a rejecting transport.
	if proxyURL != "" {
		httpClient.Transport = buildProxyTransport(proxyURL)
		return httpClient
	}

	// Priority 3: Use RoundTripper from context (typically from RoundTripperFor)
	if rt, ok := util.RoundTripperValue(ctx).(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
		return httpClient
	}

	return httpClient
}

type devinTransportKey struct {
	proxyURL      string
	baseTransport *http.Transport
}

var devinTransports = helps.NewTransportCache[devinTransportKey](helps.DefaultTransportCacheCapacity)

// newDevinHTTPClient preserves Connect-RPC framing by disabling automatic gzip
// while reusing a bounded transport pool across Devin requests.
func newDevinHTTPClient(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	timeout time.Duration,
) *http.Client {
	if ctx != nil && cliproxyexecutor.RequestProxyURL(ctx) == "" {
		if roundTripper, ok := util.RoundTripperValue(ctx).(http.RoundTripper); ok && roundTripper != nil {
			if transport, isTransport := roundTripper.(*http.Transport); isTransport {
				key := devinTransportKey{baseTransport: transport}
				cloned, err := devinTransports.Get(key, func() (*http.Transport, error) {
					copyTransport := transport.Clone()
					copyTransport.DisableCompression = true
					return copyTransport, nil
				})
				if err == nil {
					return &http.Client{Transport: cloned, Timeout: timeout}
				}
			}
			return &http.Client{
				Transport: devinNoGzipRoundTripper{base: roundTripper},
				Timeout:   timeout,
			}
		}
	}

	proxyURL := effectiveProxyURL(ctx, cfg, auth)

	key := devinTransportKey{proxyURL: proxyURL}
	transport, err := devinTransports.Get(key, func() (*http.Transport, error) {
		if proxyURL != "" {
			if errValidate := proxyutil.ValidateProxyURL(proxyURL); errValidate != nil {
				return nil, errValidate
			}
			built, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
			if errBuild != nil {
				return nil, errBuild
			}
			built.DisableCompression = true
			return built, nil
		}
		if defaultTransport, ok := http.DefaultTransport.(*http.Transport); ok {
			cloned := defaultTransport.Clone()
			cloned.DisableCompression = true
			return cloned, nil
		}
		return &http.Transport{DisableCompression: true}, nil
	})
	if err != nil {
		return &http.Client{Transport: proxyErrorTransport{err: err}, Timeout: timeout}
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

type devinNoGzipRoundTripper struct {
	base http.RoundTripper
}

func (r devinNoGzipRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "identity")
	}
	return r.base.RoundTrip(req)
}

func effectiveProxyURL(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth) string {
	if override := cliproxyexecutor.RequestProxyURL(ctx); override != "" {
		return override
	}
	if auth != nil && strings.TrimSpace(auth.ProxyURL) != "" {
		return strings.TrimSpace(auth.ProxyURL)
	}
	if cfg != nil {
		return strings.TrimSpace(cfg.ProxyURL)
	}
	return ""
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
// It supports SOCKS5, HTTP, and HTTPS proxy protocols.
//
// Parameters:
//   - proxyURL: The proxy URL string (e.g., "socks5://user:pass@host:port", "http://host:port")
//
// Returns:
//   - http.RoundTripper: A configured transport, or a rejecting transport if the proxy URL is invalid
func buildProxyTransport(proxyURL string) http.RoundTripper {
	if errValidate := proxyutil.ValidateProxyURL(proxyURL); errValidate != nil {
		log.Errorf("proxy URL rejected: %v", errValidate)
		return proxyErrorTransport{err: errValidate}
	}
	transport, _, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("build proxy transport failed: %v", errBuild)
		return proxyErrorTransport{err: errBuild}
	}
	return transport
}
