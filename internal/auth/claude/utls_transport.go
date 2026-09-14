package claude

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/httpwire"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	tls "github.com/refraction-networking/utls"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

type claudeRefreshHandshakeTimeoutContextKey struct{}

var claudeOAuthRefreshHeaderOrder = []string{
	"Accept",
	"Content-Type",
	"User-Agent",
	"Content-Length",
	"Accept-Encoding",
	"Host",
	"Connection",
}

var claudeOAuthInspectHeaderOrder = []string{
	"Accept",
	"Content-Type",
	"Authorization",
	"Cache-Control",
	"User-Agent",
	"Accept-Encoding",
	"Host",
	"Connection",
}

var claudeOAuthInspectTargets = []string{
	"/api/oauth/profile",
	"/api/oauth/claude_cli/roles",
}

func claudeOAuthRequestHeaderOrder(method, requestTarget string) []string {
	if method == http.MethodGet {
		for _, target := range claudeOAuthInspectTargets {
			if strings.HasPrefix(requestTarget, target) {
				return claudeOAuthInspectHeaderOrder
			}
		}
	}
	return claudeOAuthRefreshHeaderOrder
}

const (
	claudeOAuthSessionCacheCapacity      = 8
	claudeOAuthProxySessionCacheCapacity = 64
)

var claudeOAuthSessionCaches = internalcache.NewBoundedLRU[string, tls.ClientSessionCache](
	claudeOAuthProxySessionCacheCapacity,
	nil,
)

func claudeOAuthSessionCache(proxyURL string) tls.ClientSessionCache {
	return claudeOAuthSessionCaches.GetOrAdd(
		proxyURL, func() tls.ClientSessionCache {
			return tls.NewLRUClientSessionCache(claudeOAuthSessionCacheCapacity)
		},
	)
}

func newClaudeOAuthTLSConfig(host string, sessionCache tls.ClientSessionCache) *tls.Config {
	return &tls.Config{
		ServerName:                         host,
		ClientSessionCache:                 sessionCache,
		OmitEmptyPsk:                       true,
		PreferSkipResumptionOnNilExtension: true,
	}
}

func claudeOAuthTLSClientHelloSpec() *tls.ClientHelloSpec {
	return &tls.ClientHelloSpec{
		TLSVersMin:         tls.VersionTLS12,
		TLSVersMax:         tls.VersionTLS13,
		CompressionMethods: []uint8{0},
		CipherSuites: []uint16{
			tls.TLS_AES_128_GCM_SHA256,
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		},
		Extensions: []tls.TLSExtension{
			&tls.SNIExtension{},
			&tls.ExtendedMasterSecretExtension{},
			&tls.RenegotiationInfoExtension{Renegotiation: tls.RenegotiateOnceAsClient},
			&tls.SupportedCurvesExtension{Curves: []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384}},
			&tls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&tls.SessionTicketExtension{},
			&tls.SignatureAlgorithmsExtension{
				SupportedSignatureAlgorithms: []tls.SignatureScheme{
					tls.ECDSAWithP256AndSHA256,
					tls.PSSWithSHA256,
					tls.PKCS1WithSHA256,
					tls.ECDSAWithP384AndSHA384,
					tls.PSSWithSHA384,
					tls.PKCS1WithSHA384,
					tls.PSSWithSHA512,
					tls.PKCS1WithSHA512,
					tls.PKCS1WithSHA1,
				},
			},
			&tls.KeyShareExtension{KeyShares: []tls.KeyShare{{Group: tls.X25519}}},
			&tls.PSKKeyExchangeModesExtension{Modes: []uint8{tls.PskModeDHE}},
			&tls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}},
			&tls.UtlsPreSharedKeyExtension{},
		},
	}
}

type utlsRoundTripper struct {
	dialer       proxy.Dialer
	sessionCache tls.ClientSessionCache
	transport    *http.Transport
}

type proxyErrorDialer struct {
	err error
}

func (d proxyErrorDialer) Dial(string, string) (net.Conn, error) {
	return nil, d.err
}

func newUtlsRoundTripper(cfg *config.SDKConfig) *utlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	var proxyURL string
	if cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	if proxyURL != "" {
		if errValidate := proxyutil.ValidateProxyURL(proxyURL); errValidate != nil {
			log.Errorf("Claude OAuth proxy URL rejected: %v", errValidate)
			dialer = proxyErrorDialer{err: errValidate}
		} else if proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL); errBuild != nil {
			log.Errorf("failed to configure Claude OAuth proxy: %v", errBuild)
			dialer = proxyErrorDialer{err: errBuild}
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}

	roundTripper := &utlsRoundTripper{
		dialer:       dialer,
		sessionCache: claudeOAuthSessionCache(proxyURL),
	}
	roundTripper.transport = &http.Transport{
		ForceAttemptHTTP2: false,
		DialTLSContext:    roundTripper.dialTLSContext,
	}
	return roundTripper
}

func (t *utlsRoundTripper) dialTLSContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var (
		conn net.Conn
		err  error
	)
	if contextDialer, ok := t.dialer.(proxy.ContextDialer); ok {
		conn, err = contextDialer.DialContext(ctx, network, addr)
	} else {
		conn, err = t.dialer.Dial(network, addr)
	}
	if err != nil {
		return nil, fmt.Errorf("claude oauth tls: dial upstream: %w", err)
	}

	host, _, errSplit := net.SplitHostPort(addr)
	if errSplit != nil {
		if errClose := conn.Close(); errClose != nil {
			log.Debugf("claude oauth tls: close failed connection: %v", errClose)
		}
		return nil, fmt.Errorf("claude oauth tls: split upstream address: %w", errSplit)
	}
	tlsConn := tls.UClient(conn, newClaudeOAuthTLSConfig(host, t.sessionCache), tls.HelloCustom)
	if errPreset := tlsConn.ApplyPreset(claudeOAuthTLSClientHelloSpec()); errPreset != nil {
		if errClose := tlsConn.Close(); errClose != nil {
			log.Debugf("claude oauth tls: close connection after preset failure: %v", errClose)
		}
		return nil, fmt.Errorf("claude oauth tls: apply ClientHello: %w", errPreset)
	}
	handshakeCtx := ctx
	if handshakeTimeout, _ := ctx.Value(claudeRefreshHandshakeTimeoutContextKey{}).(time.Duration); handshakeTimeout > 0 {
		var cancelHandshake context.CancelFunc
		handshakeCtx, cancelHandshake = context.WithTimeout(ctx, handshakeTimeout)
		defer cancelHandshake()
	}
	if errHandshake := tlsConn.HandshakeContext(handshakeCtx); errHandshake != nil {
		if errClose := tlsConn.Close(); errClose != nil {
			log.Debugf("claude oauth tls: close connection after handshake failure: %v", errClose)
		}
		return nil, fmt.Errorf("claude oauth tls: handshake upstream: %w", errHandshake)
	}
	return httpwire.NewOrderedRequestConn(tlsConn, claudeOAuthRequestHeaderOrder), nil
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.transport.RoundTrip(req)
}

func (t *utlsRoundTripper) CloseIdleConnections() {
	t.transport.CloseIdleConnections()
}

func newAnthropicHttpClient(cfg *config.SDKConfig) *http.Client {
	return &http.Client{Transport: newUtlsRoundTripper(cfg)}
}
