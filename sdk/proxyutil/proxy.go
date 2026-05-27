package proxyutil

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

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

// NewDirectTransport returns a transport that bypasses environment proxies.
func NewDirectTransport() *http.Transport {
	if transport, ok := http.DefaultTransport.(*http.Transport); ok && transport != nil {
		clone := transport.Clone()
		clone.Proxy = nil
		return clone
	}
	return &http.Transport{Proxy: nil}
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
		return httpConnectDialer{proxyURL: proxyURL}, nil
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
}

func (d httpConnectDialer) Dial(network string, addr string) (net.Conn, error) {
	if d.proxyURL == nil {
		return nil, fmt.Errorf("proxy URL is nil")
	}
	dialAddr := d.proxyURL.Host
	if !strings.Contains(dialAddr, ":") {
		if d.proxyURL.Scheme == "https" {
			dialAddr += ":443"
		} else {
			dialAddr += ":80"
		}
	}

	conn, errDial := net.Dial(network, dialAddr)
	if errDial != nil {
		return nil, errDial
	}
	if d.proxyURL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: d.proxyURL.Hostname()})
		if errHandshake := tlsConn.Handshake(); errHandshake != nil {
			_ = conn.Close()
			return nil, errHandshake
		}
		conn = tlsConn
	}

	request := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Scheme: "http", Host: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if d.proxyURL.User != nil {
		username := d.proxyURL.User.Username()
		password, _ := d.proxyURL.User.Password()
		request.Header.Set(
			"Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)),
		)
	}
	if errWrite := request.Write(conn); errWrite != nil {
		_ = conn.Close()
		return nil, errWrite
	}

	response, errResponse := http.ReadResponse(bufio.NewReader(conn), request)
	if errResponse != nil {
		_ = conn.Close()
		return nil, errResponse
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
		_ = conn.Close()
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = response.Status
		}
		return nil, fmt.Errorf("proxy CONNECT failed: %s", msg)
	}
	return conn, nil
}
