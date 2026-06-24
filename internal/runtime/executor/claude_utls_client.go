package executor

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	tls "github.com/refraction-networking/utls"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

type claudeUtlsRoundTripper struct {
	mu          sync.Mutex
	connections map[string]*http2.ClientConn
	pending     map[string]*sync.Cond
	dialer      proxy.Dialer
}

func newClaudeUtlsRoundTripper(proxyURL string) *claudeUtlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("utls: failed to configure proxy dialer for %q: %v", proxyURL, errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	return &claudeUtlsRoundTripper{
		connections: make(map[string]*http2.ClientConn),
		pending:     make(map[string]*sync.Cond),
		dialer:      dialer,
	}
}

func (t *claudeUtlsRoundTripper) getOrCreateConnection(host string, addr string) (*http2.ClientConn, error) {
	t.mu.Lock()
	if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
		t.mu.Unlock()
		return h2Conn, nil
	}
	if cond, ok := t.pending[host]; ok {
		cond.Wait()
		if h2Conn, ok := t.connections[host]; ok && h2Conn.CanTakeNewRequest() {
			t.mu.Unlock()
			return h2Conn, nil
		}
	}
	cond := sync.NewCond(&t.mu)
	t.pending[host] = cond
	t.mu.Unlock()

	h2Conn, err := t.createConnection(host, addr)

	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.pending, host)
	cond.Broadcast()
	if err != nil {
		return nil, err
	}
	t.connections[host] = h2Conn
	return h2Conn, nil
}

func (t *claudeUtlsRoundTripper) createConnection(host string, addr string) (*http2.ClientConn, error) {
	conn, err := t.dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	tlsConn := tls.UClient(conn, &tls.Config{ServerName: host}, tls.HelloChrome_Auto)
	if err := tlsConn.Handshake(); err != nil {
		_ = tlsConn.Close()
		return nil, err
	}

	h2Conn, err := (&http2.Transport{}).NewClientConn(tlsConn)
	if err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	return h2Conn, nil
}

func (t *claudeUtlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	addr := req.URL.Host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}
	hostname := req.URL.Hostname()
	h2Conn, err := t.getOrCreateConnection(hostname, addr)
	if err != nil {
		return nil, err
	}
	resp, err := h2Conn.RoundTrip(req)
	if err != nil {
		t.mu.Lock()
		if cached, ok := t.connections[hostname]; ok && cached == h2Conn {
			delete(t.connections, hostname)
		}
		t.mu.Unlock()
		return nil, err
	}
	return resp, nil
}

type claudeFallbackRoundTripper struct {
	utls     http.RoundTripper
	fallback http.RoundTripper
}

var utlsProtectedHosts = map[string]struct{}{
	"api.anthropic.com": {},
	"chatgpt.com":       {},
}

func (f *claudeFallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL != nil && req.URL.Scheme == "https" {
		if _, ok := utlsProtectedHosts[strings.ToLower(req.URL.Hostname())]; ok {
			return f.utls.RoundTrip(req)
		}
	}
	return f.fallback.RoundTrip(req)
}

func newProtectedUtlsHTTPClient(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	timeout time.Duration,
) *http.Client {
	proxyURL := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	var utlsTransport http.RoundTripper = newClaudeUtlsRoundTripper(proxyURL)
	var fallback http.RoundTripper = &http.Transport{
		DialContext: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	if proxyURL != "" {
		if transport := buildProxyTransport(proxyURL); transport != nil {
			fallback = transport
		}
	} else if roundTripper, ok := util.RoundTripperValue(ctx).(http.RoundTripper); ok && roundTripper != nil {
		utlsTransport = roundTripper
		fallback = roundTripper
	}

	client := &http.Client{
		Transport: &claudeFallbackRoundTripper{
			utls:     utlsTransport,
			fallback: fallback,
		},
	}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}

func newClaudeUtlsHTTPClient(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	timeout time.Duration,
) *http.Client {
	return newProtectedUtlsHTTPClient(ctx, cfg, auth, timeout)
}
