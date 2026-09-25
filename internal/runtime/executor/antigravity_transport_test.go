package executor

import (
	"context"
	"crypto/tls"
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
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

type antigravityForeignRoundTripper struct{}

func (antigravityForeignRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Header:     make(http.Header),
		Body:       http.NoBody,
	}, nil
}

func resetAntigravityTransportTestCache(t *testing.T) {
	t.Helper()
	ResetAntigravityTransports()
	t.Cleanup(ResetAntigravityTransports)
}

func TestAntigravityTransportReuseAndIsolation(t *testing.T) {
	resetAntigravityTransportTestCache(t)

	firstAuth := &cliproxyauth.Auth{ID: "credential-a"}
	secondAuth := &cliproxyauth.Auth{ID: "credential-b"}
	firstBase := http.DefaultTransport.(*http.Transport).Clone()
	secondBase := http.DefaultTransport.(*http.Transport).Clone()

	first := antigravityHTTP11Transport(firstAuth, firstBase)
	if reused := antigravityHTTP11Transport(firstAuth, firstBase); reused != first {
		t.Fatalf("same credential/base transport was not reused: first=%p reused=%p", first, reused)
	}
	if isolated := antigravityHTTP11Transport(secondAuth, firstBase); isolated == first {
		t.Fatal("different credentials shared a transport")
	}
	if isolated := antigravityHTTP11Transport(firstAuth, secondBase); isolated == first {
		t.Fatal("different base transports shared a transport")
	}

	firstProxy := antigravityProxiedHTTP11Transport(firstAuth, "http://127.0.0.1:18080")
	if firstProxy == nil {
		t.Fatal("valid proxy URL produced no transport")
	}
	if reused := antigravityProxiedHTTP11Transport(firstAuth, "http://127.0.0.1:18080"); reused != firstProxy {
		t.Fatal("same credential/proxy transport was not reused")
	}
	if isolated := antigravityProxiedHTTP11Transport(firstAuth, "http://127.0.0.1:18081"); isolated == firstProxy {
		t.Fatal("different proxy URLs shared a transport")
	}
	if isolated := antigravityProxiedHTTP11Transport(secondAuth, "http://127.0.0.1:18080"); isolated == firstProxy {
		t.Fatal("different credentials shared a proxy transport")
	}
}

func TestAntigravityTransportScopeDoesNotExposeTokens(t *testing.T) {
	accessToken := "raw-access-token-value"
	refreshToken := "raw-refresh-token-value"

	accessScope := antigravityTransportScope(&cliproxyauth.Auth{Metadata: map[string]any{"access_token": accessToken}})
	if !strings.HasPrefix(accessScope, "token:") || len(strings.TrimPrefix(accessScope, "token:")) != 16 {
		t.Fatalf("access token scope = %q, want truncated digest", accessScope)
	}
	if strings.Contains(accessScope, accessToken) {
		t.Fatalf("access token scope exposes raw token: %q", accessScope)
	}

	refreshScope := antigravityTransportScope(&cliproxyauth.Auth{Metadata: map[string]any{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
	}})
	if !strings.HasPrefix(refreshScope, "refresh:") || len(strings.TrimPrefix(refreshScope, "refresh:")) != 16 {
		t.Fatalf("refresh token scope = %q, want truncated digest", refreshScope)
	}
	if strings.Contains(refreshScope, refreshToken) || strings.Contains(refreshScope, accessToken) {
		t.Fatalf("refresh token scope exposes raw token: %q", refreshScope)
	}
	if refreshScope == accessScope {
		t.Fatal("refresh and access token scopes collided")
	}
}

func TestAntigravityHTTP11TransportSuppressesHTTP2AndUsesShortMode(t *testing.T) {
	resetAntigravityTransportTestCache(t)

	base := http.DefaultTransport.(*http.Transport).Clone()
	base.ForceAttemptHTTP2 = true
	base.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true, // Test server uses an ephemeral certificate.
		NextProtos:         []string{"h2", "http/1.1"},
	}
	clone := cloneTransportWithHTTP11(base, &config.Config{})
	if clone == nil {
		t.Fatal("cloneTransportWithHTTP11 returned nil")
	}
	if clone == base {
		t.Fatal("cloneTransportWithHTTP11 returned the base transport")
	}
	if clone.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = true, want false")
	}
	if clone.TLSNextProto == nil || len(clone.TLSNextProto) != 0 {
		t.Fatalf("TLSNextProto = %#v, want non-nil empty map", clone.TLSNextProto)
	}
	if clone.TLSClientConfig == nil || clone.TLSClientConfig.NextProtos != nil {
		t.Fatalf("TLS NextProtos = %#v, want nil", clone.TLSClientConfig)
	}
	if clone.MaxIdleConnsPerHost != -1 || clone.DisableKeepAlives || clone.IdleConnTimeout != 0 {
		t.Fatalf("short-mode settings = MaxIdleConnsPerHost:%d DisableKeepAlives:%t IdleConnTimeout:%s", clone.MaxIdleConnsPerHost, clone.DisableKeepAlives, clone.IdleConnTimeout)
	}
	if !base.ForceAttemptHTTP2 || len(base.TLSClientConfig.NextProtos) != 2 {
		t.Fatal("base transport was mutated")
	}

	type requestObservation struct {
		protocol   string
		remoteAddr string
	}
	observations := make(chan requestObservation, 2)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observations <- requestObservation{protocol: r.Proto, remoteAddr: r.RemoteAddr}
		_, _ = io.WriteString(w, "ok")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	defer clone.CloseIdleConnections()

	client := &http.Client{Transport: clone}
	for requestIndex := range 2 {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatalf("HTTP/1.1 request %d: %v", requestIndex, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	got := make([]requestObservation, 0, 2)
	for len(got) < 2 {
		select {
		case observation := <-observations:
			got = append(got, observation)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for request observations")
		}
	}
	for _, observation := range got {
		if observation.protocol != "HTTP/1.1" {
			t.Fatalf("request protocol = %q, want HTTP/1.1", observation.protocol)
		}
	}
	if got[0].remoteAddr == got[1].remoteAddr {
		t.Fatalf("short-connection mode reused socket %q", got[0].remoteAddr)
	}
}

func TestAntigravityHTTP11TransportReusesSocketWhenPoolEnabled(t *testing.T) {
	enabled := true
	cfg := &config.Config{Antigravity: config.AntigravityConfig{ConnectionPool: config.AntigravityConnectionPoolConfig{
		Enabled: &enabled,
	}}}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Test server uses an ephemeral certificate.
	transport := cloneTransportWithHTTP11(base, cfg)
	defer transport.CloseIdleConnections()

	remoteAddresses := make(chan string, 2)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteAddresses <- r.RemoteAddr
		_, _ = io.WriteString(w, "ok")
	}))
	server.StartTLS()
	defer server.Close()

	client := &http.Client{Transport: transport}
	for requestIndex := range 2 {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatalf("pooled request %d: %v", requestIndex, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
	first := <-remoteAddresses
	second := <-remoteAddresses
	if first != second {
		t.Fatalf("pooled transport did not reuse socket: %q then %q", first, second)
	}
}

func TestResolveAntigravityPoolSettings(t *testing.T) {
	enabled := true
	disabled := false
	zero := 0
	negative := -1
	overLimit := 101
	tests := []struct {
		name        string
		cfg         *config.Config
		shortMode   bool
		timeout     time.Duration
		maxIdleHost int
	}{
		{name: "nil config", shortMode: true, maxIdleHost: -1},
		{
			name:        "disabled",
			cfg:         &config.Config{Antigravity: config.AntigravityConfig{ConnectionPool: config.AntigravityConnectionPoolConfig{Enabled: &disabled}}},
			shortMode:   true,
			maxIdleHost: -1,
		},
		{
			name:        "defaults",
			cfg:         &config.Config{Antigravity: config.AntigravityConfig{ConnectionPool: config.AntigravityConnectionPoolConfig{Enabled: &enabled}}},
			timeout:     antigravityDefaultIdleConnTimeout,
			maxIdleHost: antigravityDefaultMaxIdleConnsPerHost,
		},
		{
			name: "explicit zero keeps Go default",
			cfg: &config.Config{Antigravity: config.AntigravityConfig{ConnectionPool: config.AntigravityConnectionPoolConfig{
				Enabled: &enabled, MaxIdleConnsPerHost: &zero,
			}}},
			timeout: antigravityDefaultIdleConnTimeout,
		},
		{
			name: "invalid timeout uses default",
			cfg: &config.Config{Antigravity: config.AntigravityConfig{ConnectionPool: config.AntigravityConnectionPoolConfig{
				Enabled: &enabled, IdleConnTimeout: "invalid",
			}}},
			timeout:     antigravityDefaultIdleConnTimeout,
			maxIdleHost: antigravityDefaultMaxIdleConnsPerHost,
		},
		{
			name: "non-positive timeout returns short mode",
			cfg: &config.Config{Antigravity: config.AntigravityConfig{ConnectionPool: config.AntigravityConnectionPoolConfig{
				Enabled: &enabled, IdleConnTimeout: "0s",
			}}},
			shortMode:   true,
			maxIdleHost: -1,
		},
		{
			name: "negative idle count returns short mode",
			cfg: &config.Config{Antigravity: config.AntigravityConfig{ConnectionPool: config.AntigravityConnectionPoolConfig{
				Enabled: &enabled, MaxIdleConnsPerHost: &negative,
			}}},
			shortMode:   true,
			maxIdleHost: -1,
		},
		{
			name: "limits are capped",
			cfg: &config.Config{Antigravity: config.AntigravityConfig{ConnectionPool: config.AntigravityConnectionPoolConfig{
				Enabled: &enabled, IdleConnTimeout: "5m", MaxIdleConnsPerHost: &overLimit,
			}}},
			timeout:     antigravityMaxAllowedIdleConnTimeout,
			maxIdleHost: antigravityMaxAllowedIdleConnsPerHost,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := resolveAntigravityPoolSettings(test.cfg)
			if settings.shortMode != test.shortMode || settings.idleConnTimeout != test.timeout ||
				settings.maxIdleConnsPerHost != test.maxIdleHost {
				t.Fatalf("settings = %#v, want short=%t timeout=%s max=%d", settings, test.shortMode, test.timeout, test.maxIdleHost)
			}
		})
	}
}

func TestApplyAntigravityPoolLimitsHonorsBaseTransport(t *testing.T) {
	enabled := true
	maxIdle := 8
	cfg := &config.Config{Antigravity: config.AntigravityConfig{ConnectionPool: config.AntigravityConnectionPoolConfig{
		Enabled: &enabled, MaxIdleConnsPerHost: &maxIdle,
	}}}
	transport := &http.Transport{MaxIdleConns: 2, IdleConnTimeout: 90 * time.Second}
	applyAntigravityPoolLimits(transport, cfg)
	if transport.MaxIdleConnsPerHost != maxIdle || transport.MaxIdleConns != maxIdle ||
		transport.IdleConnTimeout != antigravityDefaultIdleConnTimeout {
		t.Fatalf("pooled transport = %#v", transport)
	}

	disabledBase := &http.Transport{MaxIdleConnsPerHost: -1, IdleConnTimeout: time.Minute}
	applyAntigravityPoolLimits(disabledBase, cfg)
	if disabledBase.MaxIdleConnsPerHost != -1 || disabledBase.IdleConnTimeout != time.Minute {
		t.Fatalf("explicitly disabled base transport changed: %#v", disabledBase)
	}
}

func TestNewAntigravityHTTPClientHandlesTypedNilAndPreservesForeignTransport(t *testing.T) {
	resetAntigravityTransportTestCache(t)

	var typedNil *http.Transport
	typedNilContext := util.WithRoundTripper(context.Background(), typedNil)
	client := newAntigravityHTTPClient(typedNilContext, &config.Config{}, &cliproxyauth.Auth{ID: "typed-nil"}, time.Second)
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil {
		t.Fatalf("typed-nil context transport resolved to %T, want non-nil *http.Transport", client.Transport)
	}
	if transport == antigravityBaseTransport {
		t.Fatal("typed-nil context reused mutable base transport directly")
	}

	foreign := antigravityForeignRoundTripper{}
	foreignContext := util.WithRoundTripper(context.Background(), foreign)
	foreignClient := newAntigravityHTTPClient(foreignContext, &config.Config{}, &cliproxyauth.Auth{ID: "foreign"}, time.Second)
	if _, ok := foreignClient.Transport.(antigravityForeignRoundTripper); !ok {
		t.Fatalf("foreign RoundTripper replaced with %T", foreignClient.Transport)
	}
}

func TestAntigravityGeneration429ClosesOnlyCredentialTransports(t *testing.T) {
	resetAntigravityTransportTestCache(t)

	firstAuth := antigravityCountTokensAuth("https://antigravity.test", "generation-429-first")
	secondAuth := antigravityCountTokensAuth("", "generation-429-second")
	firstBase := http.DefaultTransport.(*http.Transport).Clone()
	secondBase := http.DefaultTransport.(*http.Transport).Clone()
	firstTransport := antigravityHTTP11Transport(firstAuth, firstBase)
	_ = antigravityHTTP11Transport(firstAuth, secondBase)
	secondTransport := antigravityHTTP11Transport(secondAuth, firstBase)

	roundTripper := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"rate limited"}`)),
			Request:    req,
		}, nil
	})
	_, err := NewAntigravityExecutor(&config.Config{}).Execute(
		util.WithRoundTripper(context.Background(), roundTripper),
		firstAuth,
		cliproxyexecutor.Request{
			Model:   "gemini-2.5-flash",
			Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
	)
	if err == nil {
		t.Fatal("Execute returned nil error for 429")
	}
	if AntigravityTransportsLen() != 1 {
		t.Fatalf("transport cache length = %d, want only second credential", AntigravityTransportsLen())
	}
	if rebuilt := antigravityHTTP11Transport(firstAuth, firstBase); rebuilt == firstTransport {
		t.Fatal("429 retained the matching credential transport")
	}
	if reused := antigravityHTTP11Transport(secondAuth, firstBase); reused != secondTransport {
		t.Fatal("429 removed another credential's transport")
	}
}

func TestCloseAntigravityAuthIdleTransportsIsCredentialScoped(t *testing.T) {
	resetAntigravityTransportTestCache(t)

	firstAuth := &cliproxyauth.Auth{ID: "credential-a"}
	secondAuth := &cliproxyauth.Auth{ID: "credential-b"}
	baseA := http.DefaultTransport.(*http.Transport).Clone()
	baseB := http.DefaultTransport.(*http.Transport).Clone()

	firstA := antigravityHTTP11Transport(firstAuth, baseA)
	firstB := antigravityHTTP11Transport(firstAuth, baseB)
	second := antigravityHTTP11Transport(secondAuth, baseA)
	if AntigravityTransportsLen() != 3 {
		t.Fatalf("transport cache length = %d, want 3", AntigravityTransportsLen())
	}

	CloseAntigravityAuthIdleTransports(firstAuth)
	if AntigravityTransportsLen() != 1 {
		t.Fatalf("transport cache length after credential cleanup = %d, want 1", AntigravityTransportsLen())
	}
	if reused := antigravityHTTP11Transport(secondAuth, baseA); reused != second {
		t.Fatal("credential cleanup removed another credential's transport")
	}
	if rebuilt := antigravityHTTP11Transport(firstAuth, baseA); rebuilt == firstA {
		t.Fatal("credential cleanup retained first matching transport")
	}
	if rebuilt := antigravityHTTP11Transport(firstAuth, baseB); rebuilt == firstB {
		t.Fatal("credential cleanup retained second matching transport")
	}
}
