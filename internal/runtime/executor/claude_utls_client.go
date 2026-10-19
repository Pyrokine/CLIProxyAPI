package executor

import (
	"context"
	stdtls "crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	helpers "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	utls "github.com/refraction-networking/utls"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

type claudeHTTPClientConn interface {
	RoundTrip(*http.Request) (*http.Response, error)
	Close() error
}

type claudeUtlsRoundTripper struct {
	dialer            proxy.Dialer
	connectionFactory func(context.Context, string, string) (claudeHTTPClientConn, error)
}

type closeConnectionBody struct {
	io.ReadCloser
	ctx             context.Context
	closeConnection func() error
	stopContext     func() bool
	once            sync.Once
	err             error
}

func (b *closeConnectionBody) Read(data []byte) (int, error) {
	count, err := b.ReadCloser.Read(data)
	if err != nil && b.ctx != nil {
		if errContext := b.ctx.Err(); errContext != nil {
			err = errContext
		}
	}
	return count, err
}

func (b *closeConnectionBody) Close() error {
	if b == nil {
		return nil
	}
	b.once.Do(func() {
		if b.stopContext != nil {
			b.stopContext()
		}
		var errConnection error
		if b.closeConnection != nil {
			errConnection = b.closeConnection()
		}
		var errBody error
		if b.ReadCloser != nil {
			errBody = b.ReadCloser.Close()
		}
		b.err = errors.Join(errBody, errConnection)
	})
	return b.err
}

func newCloseConnectionBody(
	ctx context.Context,
	body io.ReadCloser,
	closeConnection func() error,
) *closeConnectionBody {
	wrapped := &closeConnectionBody{
		ReadCloser:      body,
		ctx:             ctx,
		closeConnection: closeConnection,
	}
	if ctx != nil {
		initialized := make(chan struct{})
		wrapped.stopContext = context.AfterFunc(ctx, func() {
			<-initialized
			_ = wrapped.Close()
		})
		close(initialized)
	}
	return wrapped
}

type utlsHTTPConn struct {
	*utls.UConn
}

func (c *utlsHTTPConn) ConnectionState() stdtls.ConnectionState {
	state := c.UConn.ConnectionState()
	return stdtls.ConnectionState{
		Version:                     state.Version,
		HandshakeComplete:           state.HandshakeComplete,
		DidResume:                   state.DidResume,
		CipherSuite:                 state.CipherSuite,
		NegotiatedProtocol:          state.NegotiatedProtocol,
		ServerName:                  state.ServerName,
		PeerCertificates:            state.PeerCertificates,
		VerifiedChains:              state.VerifiedChains,
		SignedCertificateTimestamps: state.SignedCertificateTimestamps,
		OCSPResponse:                state.OCSPResponse,
		TLSUnique:                   state.TLSUnique,
		ECHAccepted:                 state.ECHAccepted,
	}
}

type proxyErrorDialer struct {
	err error
}

func (d proxyErrorDialer) Dial(string, string) (net.Conn, error) {
	return nil, d.err
}

func (d proxyErrorDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, d.err
}

func newClaudeUtlsRoundTripper(proxyURL string) *claudeUtlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		if errValidate := proxyutil.ValidateProxyURL(proxyURL); errValidate != nil {
			log.Errorf("utls: proxy URL rejected: %v", errValidate)
			dialer = proxyErrorDialer{err: errValidate}
		} else if proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL); errBuild != nil {
			log.Errorf("utls: failed to configure proxy dialer for %q: %v", proxyURL, errBuild)
			dialer = proxyErrorDialer{err: errBuild}
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	return &claudeUtlsRoundTripper{dialer: dialer}
}

func (t *claudeUtlsRoundTripper) createConnection(
	ctx context.Context,
	host string,
	addr string,
) (claudeHTTPClientConn, error) {
	if t.connectionFactory != nil {
		return t.connectionFactory(ctx, host, addr)
	}
	contextDialer, ok := t.dialer.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("utls: dialer does not support context cancellation")
	}
	conn, errDial := contextDialer.DialContext(ctx, "tcp", addr)
	if errDial != nil {
		return nil, fmt.Errorf("utls: dial upstream: %w", errDial)
	}

	tlsConn := utls.UClient(conn, &utls.Config{ServerName: host}, utls.HelloChrome_Auto)
	if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
		// HandshakeContext closes the underlying connection when its context is
		// canceled. Avoid closing it a second time while preserving the context
		// error for callers.
		if errors.Is(errHandshake, context.Canceled) || errors.Is(errHandshake, context.DeadlineExceeded) {
			return nil, fmt.Errorf("utls: TLS handshake: %w", errHandshake)
		}
		if errClose := tlsConn.Close(); errClose != nil {
			return nil, fmt.Errorf("utls: TLS handshake: %w; close connection: %v", errHandshake, errClose)
		}
		return nil, fmt.Errorf("utls: TLS handshake: %w", errHandshake)
	}
	return initializeClaudeHTTPClientConn(ctx, addr, tlsConn, nil)
}

func initializeClaudeHTTPClientConn(
	ctx context.Context,
	addr string,
	tlsConn *utls.UConn,
	initialize func(context.Context, string, string) (claudeHTTPClientConn, error),
) (claudeHTTPClientConn, error) {
	protocols := new(http.Protocols)
	switch protocol := tlsConn.ConnectionState().NegotiatedProtocol; protocol {
	case "h2":
		protocols.SetHTTP2(true)
	case "", "http/1.1":
		protocols.SetHTTP1(true)
	default:
		if errClose := tlsConn.Close(); errClose != nil {
			return nil, fmt.Errorf("utls: unsupported negotiated protocol %q; close connection: %v", protocol, errClose)
		}
		return nil, fmt.Errorf("utls: unsupported negotiated protocol %q", protocol)
	}
	if initialize == nil {
		transport := &http.Transport{
			Protocols: protocols,
			DialTLSContext: func(context.Context, string, string) (net.Conn, error) {
				return &utlsHTTPConn{UConn: tlsConn}, nil
			},
		}
		initialize = func(ctx context.Context, scheme, address string) (claudeHTTPClientConn, error) {
			return transport.NewClientConn(ctx, scheme, address)
		}
	}
	httpConn, errClientConn := initialize(ctx, "https", addr)
	if errClientConn != nil {
		if errClose := tlsConn.Close(); errClose != nil {
			return nil, fmt.Errorf(
				"utls: initialize HTTP connection: %w; close TLS connection: %v",
				errClientConn,
				errClose,
			)
		}
		return nil, fmt.Errorf("utls: initialize HTTP connection: %w", errClientConn)
	}
	return httpConn, nil
}

func (t *claudeUtlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, fmt.Errorf("request URL is nil")
	}
	hostname := req.URL.Hostname()
	pinnedAddr, private := proxyutil.ResolvePublicHost(hostname)
	if private {
		return nil, fmt.Errorf("requests to private/internal addresses are not allowed")
	}
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(pinnedAddr, port)
	httpConn, errConnect := t.createConnection(req.Context(), hostname, addr)
	if errConnect != nil {
		return nil, errConnect
	}
	resp, errRoundTrip := httpConn.RoundTrip(req)
	if errRoundTrip != nil {
		if errClose := httpConn.Close(); errClose != nil {
			log.Debugf("utls: close connection after round trip failure: %v", errClose)
		}
		return nil, errRoundTrip
	}
	if resp == nil {
		if errClose := httpConn.Close(); errClose != nil {
			log.Debugf("utls: close connection after empty response: %v", errClose)
		}
		return nil, fmt.Errorf("utls: upstream returned an empty response")
	}
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	resp.Body = newCloseConnectionBody(req.Context(), resp.Body, httpConn.Close)
	return resp, nil
}

type claudeFallbackRoundTripper struct {
	anthropic http.RoundTripper
	chrome    http.RoundTripper
	fallback  http.RoundTripper
}

func (f *claudeFallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && helpers.IsAnthropicUpstreamURL(req.URL) {
		return f.anthropic.RoundTrip(req)
	}
	if req != nil && req.URL != nil && req.URL.Scheme == "https" &&
		strings.EqualFold(req.URL.Hostname(), "chatgpt.com") {
		return f.chrome.RoundTrip(req)
	}
	return f.fallback.RoundTrip(req)
}

func newProtectedUtlsHTTPClient(
	ctx context.Context,
	cfg *config.Config,
	auth *cliproxyauth.Auth,
	timeout time.Duration,
) *http.Client {
	proxyURL := effectiveProxyURL(ctx, cfg, auth)

	var chromeTransport http.RoundTripper = newClaudeUtlsRoundTripper(proxyURL)
	var anthropicTransport = cachedClaudeCodeRoundTripper(proxyURL)
	var fallback http.RoundTripper = &http.Transport{
		DialContext: (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	if proxyURL != "" {
		fallback = buildProxyTransport(proxyURL)
	} else if roundTripper, ok := util.RoundTripperValue(ctx).(http.RoundTripper); ok && roundTripper != nil {
		chromeTransport = roundTripper
		anthropicTransport = roundTripper
		fallback = roundTripper
	}

	client := &http.Client{
		Transport: &claudeFallbackRoundTripper{
			anthropic: anthropicTransport,
			chrome:    chromeTransport,
			fallback:  fallback,
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
