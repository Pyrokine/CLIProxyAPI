package proxyutil

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// Mode describes how a proxy setting should be interpreted.
type Mode int

const (
	// ModeInherit means no explicit proxy behavior was configured.
	ModeInherit Mode = iota
	// ModeDirect means outbound requests must bypass proxies explicitly.
	ModeDirect
	// ModeProxy means a concrete proxy URL was configured.
	ModeProxy
	// ModeInvalid means the proxy setting is present but malformed or unsupported.
	ModeInvalid
)

// Setting is the normalized interpretation of a proxy configuration value.
type Setting struct {
	Raw  string
	Mode Mode
	URL  *url.URL
}

// Parse normalizes a proxy configuration value into inherit, direct, or proxy modes.
func Parse(raw string) (Setting, error) {
	trimmed := strings.TrimSpace(raw)
	setting := Setting{Raw: trimmed}

	if trimmed == "" {
		setting.Mode = ModeInherit
		return setting, nil
	}

	if strings.EqualFold(trimmed, "direct") || strings.EqualFold(trimmed, "none") {
		setting.Mode = ModeDirect
		return setting, nil
	}

	parsedURL, errParse := url.Parse(trimmed)
	if errParse != nil {
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("parse proxy URL failed: %w", errParse)
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("proxy URL missing scheme/host")
	}

	switch parsedURL.Scheme {
	case "socks5", "socks5h", "http", "https":
		setting.Mode = ModeProxy
		setting.URL = parsedURL
		return setting, nil
	default:
		setting.Mode = ModeInvalid
		return setting, fmt.Errorf("unsupported proxy scheme: %s", parsedURL.Scheme)
	}
}

func ValidateProxyURL(raw string) error {
	_, errParse := Parse(raw)
	return errParse
}

// ValidatePublicProxyURL is kept for SDK compatibility. Proxy endpoints may be local or private;
// only target URLs fetched directly by CPA should use public-host validation.
// noinspection GoUnusedExportedFunction
func ValidatePublicProxyURL(raw string) error {
	return ValidateProxyURL(raw)
}

func ValidateHTTPURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	parsedURL, errParse := url.Parse(trimmed)
	if errParse != nil {
		return fmt.Errorf("parse URL failed: %w", errParse)
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return fmt.Errorf("URL missing scheme/host")
	}
	switch parsedURL.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("unsupported URL scheme: %s", parsedURL.Scheme)
	}
	return nil
}

func ValidatePublicHTTPURL(raw string) error {
	if err := ValidateHTTPURL(raw); err != nil {
		return err
	}
	trimmed := strings.TrimSpace(raw)
	parsedURL, _ := url.Parse(trimmed)
	if IsPrivateHost(parsedURL.Hostname()) {
		return fmt.Errorf("URL points to private/internal address")
	}
	return nil
}

func IsPrivateHost(hostname string) bool {
	_, private := ResolvePublicHost(hostname)
	return private
}

func IsRestrictedCustomHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "api-key", "cookie", "host", "proxy-authorization", "x-api-key", "x-goog-api-key", "x-management-key":
		return true
	default:
		return false
	}
}

func ResolvePublicHost(hostname string) (string, bool) {
	if hostname == "" {
		return "", true
	}
	ip := net.ParseIP(hostname)
	if ip != nil {
		if isPrivateIP(ip) {
			return "", true
		}
		return hostname, false
	}
	addrs, errLookup := net.LookupHost(hostname)
	if errLookup != nil {
		return "", true
	}
	for _, addr := range addrs {
		if parsed := net.ParseIP(addr); parsed != nil && isPrivateIP(parsed) {
			return "", true
		}
	}
	if len(addrs) == 0 {
		return "", true
	}
	return addrs[0], false
}

var privateRanges = []*net.IPNet{
	mustParseCIDR("0.0.0.0/8"),
	mustParseCIDR("10.0.0.0/8"),
	mustParseCIDR("100.64.0.0/10"),
	mustParseCIDR("172.16.0.0/12"),
	mustParseCIDR("192.0.0.0/24"),
	mustParseCIDR("192.0.2.0/24"),
	mustParseCIDR("192.88.99.0/24"),
	mustParseCIDR("192.168.0.0/16"),
	mustParseCIDR("198.18.0.0/15"),
	mustParseCIDR("198.51.100.0/24"),
	mustParseCIDR("203.0.113.0/24"),
	mustParseCIDR("240.0.0.0/4"),
	mustParseCIDR("64:ff9b::/96"),
	mustParseCIDR("64:ff9b:1::/48"),
	mustParseCIDR("100::/64"),
	mustParseCIDR("100:0:0:1::/64"),
	mustParseCIDR("2001::/23"),
	mustParseCIDR("2001:2::/48"),
	mustParseCIDR("2001:db8::/32"),
	mustParseCIDR("2001:10::/28"),
	mustParseCIDR("2002::/16"),
	mustParseCIDR("3fff::/20"),
	mustParseCIDR("5f00::/16"),
	mustParseCIDR("fc00::/7"),
}

func isPrivateIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() {
		return true
	}
	for _, network := range privateRanges {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func mustParseCIDR(cidr string) *net.IPNet {
	_, network, errParse := net.ParseCIDR(cidr)
	if errParse != nil {
		panic("invalid CIDR: " + cidr)
	}
	return network
}

// NewDirectTransport returns an HTTP transport that bypasses environment proxies.
func NewDirectTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		clone := transport.Clone()
		clone.Proxy = nil
		return clone
	}
	return &http.Transport{Proxy: nil}
}

type publicHostRoundTripper struct {
	base           http.RoundTripper
	allowIPLiteral bool
}

func PublicHostTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return publicHostRoundTripper{base: base}
}

// noinspection GoUnusedExportedFunction
func PublicDNSRebindTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return publicHostRoundTripper{base: base, allowIPLiteral: true}
}

func (t publicHostRoundTripper) BaseTransport() http.RoundTripper {
	return t.base
}

func (t publicHostRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("request URL is nil")
	}
	hostname := req.URL.Hostname()
	pinnedAddr, private := resolveTransportHost(hostname, t.allowIPLiteral)
	if private {
		return nil, fmt.Errorf("requests to private/internal addresses are not allowed")
	}
	return PinnedTransport(t.base, hostname, pinnedAddr).RoundTrip(req)
}

func resolveTransportHost(hostname string, allowIPLiteral bool) (string, bool) {
	if allowIPLiteral {
		if ip := net.ParseIP(hostname); ip != nil {
			return hostname, false
		}
	}
	return ResolvePublicHost(hostname)
}

// PinnedTransport wraps a base RoundTripper with a custom DialContext that
// forces the initial hostname to connect to the pre-resolved pinnedAddr.
func PinnedTransport(base http.RoundTripper, hostname string, pinnedAddr string) http.RoundTripper {
	if pinnedAddr == "" || hostname == "" {
		return base
	}
	transport, ok := base.(*http.Transport)
	if !ok {
		return base
	}
	clone := transport.Clone()
	originalDial := clone.DialContext
	if originalDial == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		originalDial = dialer.DialContext
	}
	clone.DialContext = func(ctx context.Context, network string, addr string) (net.Conn, error) {
		host, port, errSplit := net.SplitHostPort(addr)
		if errSplit != nil {
			return originalDial(ctx, network, addr)
		}
		if host == hostname {
			addr = net.JoinHostPort(pinnedAddr, port)
		}
		return originalDial(ctx, network, addr)
	}
	return clone
}

// BuildHTTPTransport constructs an HTTP transport for the provided proxy setting.
func BuildHTTPTransport(raw string) (*http.Transport, Mode, error) {
	setting, errParse := Parse(raw)
	if errParse != nil {
		return nil, 0, errParse
	}

	switch setting.Mode {
	case ModeInherit:
		return nil, setting.Mode, nil
	case ModeDirect:
		return NewDirectTransport(), setting.Mode, nil
	case ModeProxy:
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok || base == nil {
			base = &http.Transport{}
		}
		clone := base.Clone()
		switch setting.URL.Scheme {
		case "socks5", "socks5h":
			dialer, errDialer := NewDialerForURL(setting.URL)
			if errDialer != nil {
				return nil, setting.Mode, errDialer
			}
			clone.Proxy = nil
			clone.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			}
		case "http", "https":
			clone.Proxy = http.ProxyURL(setting.URL)
		default:
			return nil, setting.Mode, fmt.Errorf("unsupported proxy scheme: %s", setting.URL.Scheme)
		}
		return clone, setting.Mode, nil
	default:
		return nil, setting.Mode, nil
	}
}

// BuildDialer constructs a proxy dialer for settings that operate at the connection layer.
// noinspection GoUnusedExportedFunction — public SDK API.
func BuildDialer(raw string) (proxy.Dialer, Mode, error) {
	setting, errParse := Parse(raw)
	if errParse != nil {
		return nil, 0, errParse
	}

	switch setting.Mode {
	case ModeInherit:
		return nil, setting.Mode, nil
	case ModeDirect:
		return proxy.Direct, setting.Mode, nil
	case ModeProxy:
		dialer, errDialer := NewDialerForURL(setting.URL)
		if errDialer != nil {
			return nil, setting.Mode, errDialer
		}
		return dialer, setting.Mode, nil
	default:
		return nil, setting.Mode, nil
	}
}

// NewDialerForURL returns a TCP dialer for SOCKS and HTTP CONNECT proxies.
func NewDialerForURL(proxyURL *url.URL) (proxy.Dialer, error) {
	if proxyURL == nil {
		return nil, fmt.Errorf("proxy URL is nil")
	}
	switch proxyURL.Scheme {
	case "socks5", "socks5h":
		return newSOCKS5Dialer(proxyURL)
	case "http", "https":
		return &httpConnectDialer{proxyURL: proxyURL, dialer: proxy.Direct}, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme: %s", proxyURL.Scheme)
	}
}

func newSOCKS5Dialer(proxyURL *url.URL) (proxy.Dialer, error) {
	var proxyAuth *proxy.Auth
	if proxyURL.User != nil {
		username := proxyURL.User.Username()
		password, _ := proxyURL.User.Password()
		proxyAuth = &proxy.Auth{User: username, Password: password}
	}
	dialer, errSOCKS5 := proxy.SOCKS5("tcp", proxyURL.Host, proxyAuth, proxy.Direct)
	if errSOCKS5 != nil {
		return nil, fmt.Errorf("create SOCKS5 dialer failed: %w", errSOCKS5)
	}
	return dialer, nil
}

type httpConnectDialer struct {
	proxyURL *url.URL
	dialer   proxy.Dialer
}

func (d *httpConnectDialer) Dial(network string, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *httpConnectDialer) DialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
	if d.proxyURL == nil {
		return nil, errors.New("proxy URL is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	contextDialer, ok := d.dialer.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("HTTP proxy base dialer does not support context cancellation")
	}
	proxyConnection, errDial := contextDialer.DialContext(ctx, network, proxyDialAddress(d.proxyURL))
	if errDial != nil {
		return nil, fmt.Errorf("dial HTTP proxy failed: %w", errDial)
	}

	connection := proxyConnection
	cancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(
		ctx, func() {
			_ = proxyConnection.Close()
			close(cancelDone)
		},
	)
	defer func() {
		if !stopCancel() {
			<-cancelDone
		}
	}()
	if d.proxyURL.Scheme == "https" {
		// A successful TLS connection is returned to the caller; failure closes the underlying connection.
		// noinspection GoResourceLeak
		tlsConnection := tls.Client(connection, &tls.Config{ServerName: d.proxyURL.Hostname()})
		if errHandshake := tlsConnection.HandshakeContext(ctx); errHandshake != nil {
			if errClose := connection.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
				return nil, fmt.Errorf("HTTPS proxy TLS handshake failed: %w; close failed: %v", errHandshake, errClose)
			}
			return nil, fmt.Errorf("HTTPS proxy TLS handshake failed: %w", errHandshake)
		}
		connection = tlsConnection
	}

	request := (&http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Host: addr},
		Host:   addr,
		Header: make(http.Header),
	}).WithContext(ctx)
	if d.proxyURL.User != nil {
		request.Header.Set("Proxy-Authorization", proxyAuthorization(d.proxyURL.User))
	}
	if errWrite := request.Write(connection); errWrite != nil {
		if errClose := connection.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			return nil, fmt.Errorf("write CONNECT request failed: %w; close failed: %v", errWrite, errClose)
		}
		return nil, fmt.Errorf("write CONNECT request failed: %w", errWrite)
	}

	reader := bufio.NewReader(connection)
	// A successful CONNECT response body is the tunnel connection and must remain open for the caller.
	// noinspection GoResourceLeak
	response, errResponse := http.ReadResponse(reader, request)
	if errResponse != nil {
		if errClose := connection.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			return nil, fmt.Errorf("read CONNECT response failed: %w; close failed: %v", errResponse, errClose)
		}
		return nil, fmt.Errorf("read CONNECT response failed: %w", errResponse)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
		if errClose := connection.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			return nil, fmt.Errorf("proxy CONNECT failed with status %s; close failed: %v", response.Status, errClose)
		}
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = response.Status
		}
		return nil, fmt.Errorf("proxy CONNECT failed: %s", message)
	}
	if errContext := ctx.Err(); errContext != nil {
		if errClose := connection.Close(); errClose != nil && !errors.Is(errClose, net.ErrClosed) {
			return nil, fmt.Errorf("HTTP proxy context ended: %w; close failed: %v", errContext, errClose)
		}
		return nil, errContext
	}
	if reader.Buffered() > 0 {
		return &bufferedConnection{Conn: connection, reader: reader}, nil
	}
	return connection, nil
}

func proxyDialAddress(proxyURL *url.URL) string {
	port := proxyURL.Port()
	if port == "" {
		port = "80"
		if proxyURL.Scheme == "https" {
			port = "443"
		}
	}
	return net.JoinHostPort(proxyURL.Hostname(), port)
}

func proxyAuthorization(user *url.Userinfo) string {
	username := user.Username()
	password, _ := user.Password()
	encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	return "Basic " + encoded
}

type bufferedConnection struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConnection) Read(destination []byte) (int, error) {
	if c.reader.Buffered() > 0 {
		return c.reader.Read(destination)
	}
	return c.Conn.Read(destination)
}
