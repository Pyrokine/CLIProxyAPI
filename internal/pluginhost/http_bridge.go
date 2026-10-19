package pluginhost

import (
	"bufio"
	"bytes"
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
	"sync"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/httpwire"
	helps "github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

const maxPluginHTTPResponseBytes int64 = 64 << 20

var (
	errPluginHTTPResponseTooLarge = errors.New("pluginhost: non-stream response exceeds 64 MiB")
	pluginHTTPDefaultTimeout      = 5 * time.Minute
)

type hostHTTPClient struct {
	host            *Host
	auth            *coreauth.Auth
	provider        string
	requestProxyURL string
}

func (h *Host) newHTTPClient(auth *coreauth.Auth, providers ...string) pluginapi.HostHTTPClient {
	return h.newHTTPClientWithProxy(auth, "", providers...)
}

func (h *Host) newHTTPClientWithProxy(auth *coreauth.Auth, requestProxyURL string, providers ...string) pluginapi.HostHTTPClient {
	provider := ""
	if len(providers) > 0 {
		provider = providers[0]
	}
	return &hostHTTPClient{host: h, auth: auth, provider: provider, requestProxyURL: strings.TrimSpace(requestProxyURL)}
}

func (c *hostHTTPClient) proxyContext(ctx context.Context) context.Context {
	if c == nil {
		return cliproxyexecutor.WithoutRequestProxyURL(ctx)
	}
	return cliproxyexecutor.WithRequestProxyURL(ctx, c.requestProxyURL)
}

func (c *hostHTTPClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pluginHTTPDefaultTimeout)
		defer cancel()
	}
	resp, cfg, cleanup, errDo := c.doHTTP(ctx, req)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, cfg, errDo)
		return pluginapi.HTTPResponse{}, errDo
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Warnf("pluginhost: response body close error: %v", errClose)
		}
		if cleanup != nil {
			cleanup()
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, cfg, resp.StatusCode, resp.Header)
	body, errReadAll := readPluginHTTPResponse(resp.Body, resp.ContentLength, maxPluginHTTPResponseBytes)
	if errReadAll != nil {
		helps.RecordAPIResponseError(ctx, cfg, errReadAll)
		return pluginapi.HTTPResponse{}, fmt.Errorf("read host http response: %w", errReadAll)
	}
	helps.AppendAPIResponseChunk(ctx, cfg, body)
	return pluginapi.HTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    cloneHeader(resp.Header),
		Body:       body,
	}, nil
}

func readPluginHTTPResponse(body io.Reader, contentLength, maxBytes int64) ([]byte, error) {
	if contentLength > maxBytes {
		return nil, errPluginHTTPResponseTooLarge
	}
	data, errRead := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if errRead != nil {
		return nil, errRead
	}
	if int64(len(data)) > maxBytes {
		return nil, errPluginHTTPResponseTooLarge
	}
	return data, nil
}

func (c *hostHTTPClient) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (
	pluginapi.HTTPStreamResponse,
	error,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	resp, cfg, cleanup, errDo := c.doHTTP(ctx, req)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, cfg, errDo)
		return pluginapi.HTTPStreamResponse{}, errDo
	}
	helps.RecordAPIResponseMetadata(ctx, cfg, resp.StatusCode, resp.Header)
	chunks := make(chan pluginapi.HTTPStreamChunk)
	go func() {
		defer close(chunks)
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Warnf("pluginhost: stream response body close error: %v", errClose)
			}
			if cleanup != nil {
				cleanup()
			}
		}()
		buf := make([]byte, 32*1024)
		for {
			n, errRead := resp.Body.Read(buf)
			if n > 0 {
				payload := bytes.Clone(buf[:n])
				helps.AppendAPIResponseChunk(ctx, cfg, payload)
				select {
				case <-ctx.Done():
					return
				case chunks <- pluginapi.HTTPStreamChunk{Payload: payload}:
				}
			}
			if errRead != nil {
				if errRead != io.EOF {
					helps.RecordAPIResponseError(ctx, cfg, errRead)
					select {
					case <-ctx.Done():
					case chunks <- pluginapi.HTTPStreamChunk{Err: errRead}:
					}
				}
				return
			}
		}
	}()
	return pluginapi.HTTPStreamResponse{
		StatusCode: resp.StatusCode,
		Headers:    cloneHeader(resp.Header),
		Chunks:     chunks,
	}, nil
}

func (c *hostHTTPClient) doHTTP(
	ctx context.Context,
	req pluginapi.HTTPRequest,
) (*http.Response, *config.Config, func(), error) {
	if c == nil || c.host == nil {
		return nil, nil, nil, fmt.Errorf("host http client is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := c.host.currentRuntimeConfig()
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	httpReq, errNewRequest := http.NewRequestWithContext(
		ctx,
		method,
		req.URL,
		bytes.NewReader(bytes.Clone(req.Body)),
	)
	if errNewRequest != nil {
		return nil, cfg, nil, fmt.Errorf("create host http request: %w", errNewRequest)
	}
	httpReq.Header = cloneHeader(req.Headers)
	helps.RecordAPIRequest(ctx, cfg, req.URL, method, httpReq.Header, req.Body, c.provider, c.auth)
	client, cleanup, errClient := c.newHTTPClientForRequest(ctx, cfg, req, httpReq)
	if errClient != nil {
		return nil, cfg, nil, errClient
	}
	if client == nil {
		client = &http.Client{}
	}
	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, cfg, nil, fmt.Errorf("execute host http request: %w", errDo)
	}
	return resp, cfg, cleanup, nil
}

func (h *Host) currentRuntimeConfig() *config.Config {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runtimeConfig
}

func redactedProxyURL(raw string) string {
	proxyURL, errParse := url.Parse(raw)
	if errParse != nil {
		return "<invalid proxy URL>"
	}
	if proxyURL.User != nil {
		proxyURL.User = url.User("redacted")
	}
	proxyURL.Path = ""
	proxyURL.RawPath = ""
	proxyURL.RawQuery = ""
	proxyURL.Fragment = ""
	return proxyURL.String()
}

func (c *hostHTTPClient) newHTTPClientForRequest(
	ctx context.Context,
	cfg *config.Config,
	req pluginapi.HTTPRequest,
	httpReq *http.Request,
) (*http.Client, func(), error) {
	profile := cloneHTTPWireProfile(req.WireProfile)
	if profile == nil || !profile.HTTP1Only && !profile.DisableAutoCompression && len(profile.HeaderProfile) == 0 {
		client := helps.NewProxyAwareHTTPClient(c.proxyContext(ctx), cfg, c.auth, 0)
		if client == nil {
			client = &http.Client{}
		}
		return client, nil, nil
	}

	proxyString := strings.TrimSpace(c.requestProxyURL)
	if proxyString == "" && c.auth != nil {
		proxyString = strings.TrimSpace(c.auth.ProxyURL)
	}
	if proxyString == "" && cfg != nil {
		proxyString = strings.TrimSpace(cfg.ProxyURL)
	}

	var baseTransport *http.Transport
	explicitDirect := false
	configuredSOCKS := false
	builtByProxyutil := false
	if proxyString != "" {
		setting, errParse := proxyutil.Parse(proxyString)
		if errParse != nil {
			return nil, nil, fmt.Errorf("pluginhost: parse proxy %s: %w", redactedProxyURL(proxyString), errParse)
		}
		switch setting.Mode {
		case proxyutil.ModeDirect:
			explicitDirect = true
			baseTransport = proxyutil.NewDirectTransport()
			proxyString = ""
		case proxyutil.ModeProxy:
			builtTransport, _, errBuild := proxyutil.BuildHTTPTransport(proxyString)
			if errBuild != nil {
				return nil, nil, fmt.Errorf(
					"pluginhost: build proxy transport for %s: %w",
					redactedProxyURL(proxyString),
					errBuild,
				)
			}
			baseTransport = builtTransport
			builtByProxyutil = true
			configuredSOCKS = setting.URL != nil &&
				(strings.EqualFold(setting.URL.Scheme, "socks5") || strings.EqualFold(setting.URL.Scheme, "socks5h"))
		case proxyutil.ModeInherit:
			proxyString = ""
		}
	}

	var contextTransport *http.Transport
	if contextRoundTripper, ok := util.RoundTripperValue(ctx).(http.RoundTripper); ok && contextRoundTripper != nil {
		if transport, okTransport := contextRoundTripper.(*http.Transport); okTransport && transport != nil {
			contextTransport = transport
		} else if baseTransport == nil {
			return nil, nil, fmt.Errorf(
				"pluginhost: wire profile is not supported with custom context RoundTripper %T",
				contextRoundTripper,
			)
		}
	}

	if baseTransport == nil {
		switch {
		case contextTransport != nil:
			baseTransport = contextTransport.Clone()
		case explicitDirect:
			baseTransport = proxyutil.NewDirectTransport()
		default:
			defaultTransport, okDefault := http.DefaultTransport.(*http.Transport)
			if !okDefault || defaultTransport == nil {
				return nil, nil, fmt.Errorf(
					"pluginhost: wire profile is not supported with custom default RoundTripper %T",
					http.DefaultTransport,
				)
			}
			baseTransport = defaultTransport.Clone()
		}
	} else if contextTransport != nil && contextTransport.TLSClientConfig != nil {
		baseTransport.TLSClientConfig = contextTransport.TLSClientConfig.Clone()
	}

	if profile.DisableAutoCompression {
		baseTransport.DisableCompression = true
	}

	headerProfile := append([]string(nil), profile.HeaderProfile...)
	hasHeaderProfile := len(headerProfile) > 0
	forceHTTP1 := profile.HTTP1Only || hasHeaderProfile
	if forceHTTP1 {
		baseTransport.ForceAttemptHTTP2 = false
		baseTransport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		if baseTransport.TLSClientConfig == nil {
			baseTransport.TLSClientConfig = &tls.Config{}
		} else {
			baseTransport.TLSClientConfig = baseTransport.TLSClientConfig.Clone()
		}
		baseTransport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}

	callerTLSDialer := transportTLSDialer(contextTransport)
	if callerTLSDialer == nil && !builtByProxyutil {
		callerTLSDialer = transportTLSDialer(baseTransport)
	}
	if forceHTTP1 && !hasHeaderProfile && callerTLSDialer != nil {
		baseTransport.DialTLSContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
			tlsConn, errTLS := callerTLSDialer(dialCtx, network, address)
			if errTLS != nil {
				return nil, errTLS
			}
			if errProtocol := requireHTTP1TLS(dialCtx, tlsConn, "custom TLS dialer"); errProtocol != nil {
				return nil, errProtocol
			}
			return tlsConn, nil
		}
	}

	requestState := &requestHolder{req: httpReq}
	if hasHeaderProfile {
		headerOrder := func(_, _ string) []string {
			return append([]string(nil), headerProfile...)
		}
		originalDialContext := transportDialer(baseTransport)
		originalProxy := baseTransport.Proxy
		baseTransport.DialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
			conn, errDial := originalDialContext(dialCtx, network, address)
			if errDial != nil {
				return nil, errDial
			}
			return httpwire.NewOrderedRequestConn(conn, headerOrder), nil
		}
		baseTransport.Proxy = func(request *http.Request) (*url.URL, error) {
			if request != nil && request.URL != nil && request.URL.Scheme == "http" && originalProxy != nil {
				return originalProxy(request)
			}
			return nil, nil
		}
		baseTransport.DialTLSContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
			currentRequest := requestState.get()
			targetProxy, errProxy := resolveProxyForRequest(currentRequest, c.auth, cfg, originalProxy)
			if errProxy != nil {
				return nil, errProxy
			}
			targetHTTPS := currentRequest != nil && currentRequest.URL != nil && currentRequest.URL.Scheme == "https"
			if !targetHTTPS {
				conn, errProxyTLS := dialHTTPProxyTLS(
					dialCtx,
					network,
					address,
					callerTLSDialer,
					originalDialContext,
					baseTransport,
				)
				if errProxyTLS != nil {
					return nil, errProxyTLS
				}
				return httpwire.NewOrderedRequestConn(conn, headerOrder), nil
			}

			var rawConn net.Conn
			var errDial error
			switch {
			case configuredSOCKS:
				rawConn, errDial = originalDialContext(dialCtx, network, address)
			case targetProxy != nil:
				rawConn, errDial = dialProxyTunnel(
					dialCtx,
					originalDialContext,
					callerTLSDialer,
					targetProxy,
					address,
					baseTransport,
				)
			case callerTLSDialer != nil:
				tlsConn, errTLS := callerTLSDialer(dialCtx, network, address)
				if errTLS != nil {
					return nil, errTLS
				}
				if errProtocol := requireHTTP1TLS(dialCtx, tlsConn, "custom TLS dialer"); errProtocol != nil {
					return nil, errProtocol
				}
				return httpwire.NewOrderedRequestConn(tlsConn, headerOrder), nil
			default:
				rawConn, errDial = originalDialContext(dialCtx, network, address)
			}
			if errDial != nil {
				return nil, errDial
			}
			tlsConn, errTLS := clientTLSConnection(dialCtx, rawConn, address, baseTransport.TLSClientConfig)
			if errTLS != nil {
				return nil, errTLS
			}
			return httpwire.NewOrderedRequestConn(tlsConn, headerOrder), nil
		}
	}

	cleanup := func() {
		baseTransport.CloseIdleConnections()
	}
	baseTransport.DisableKeepAlives = true
	client := &http.Client{
		Transport: baseTransport,
		CheckRedirect: func(redirectRequest *http.Request, via []*http.Request) error {
			requestState.set(redirectRequest)
			baseTransport.CloseIdleConnections()
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}
	return client, cleanup, nil
}

func transportDialer(transport *http.Transport) func(context.Context, string, string) (net.Conn, error) {
	if transport != nil {
		if transport.DialContext != nil {
			return transport.DialContext
		}
		//lint:ignore SA1019 Caller-provided transports may still use the legacy Dial callback.
		if transport.Dial != nil {
			return func(_ context.Context, network, address string) (net.Conn, error) {
				//lint:ignore SA1019 Caller-provided transports may still use the legacy Dial callback.
				return transport.Dial(network, address)
			}
		}
	}
	return (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
}

func transportTLSDialer(transport *http.Transport) func(context.Context, string, string) (net.Conn, error) {
	if transport == nil {
		return nil
	}
	if transport.DialTLSContext != nil {
		return transport.DialTLSContext
	}
	//lint:ignore SA1019 Caller-provided transports may still use the legacy DialTLS callback.
	if transport.DialTLS != nil {
		return func(_ context.Context, network, address string) (net.Conn, error) {
			//lint:ignore SA1019 Caller-provided transports may still use the legacy DialTLS callback.
			return transport.DialTLS(network, address)
		}
	}
	return nil
}

func requireHTTP1TLS(ctx context.Context, conn net.Conn, source string) error {
	tlsConn, okTLS := conn.(*tls.Conn)
	if !okTLS || tlsConn == nil {
		return nil
	}
	if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
		_ = conn.Close()
		return fmt.Errorf("pluginhost: %s handshake: %w", source, errHandshake)
	}
	negotiated := tlsConn.ConnectionState().NegotiatedProtocol
	if negotiated != "" && negotiated != "http/1.1" {
		_ = conn.Close()
		return fmt.Errorf(
			"pluginhost: %s negotiated unsupported protocol %q; wire profile requires HTTP/1.1",
			source,
			negotiated,
		)
	}
	return nil
}

func dialHTTPProxyTLS(
	ctx context.Context,
	network string,
	address string,
	customTLSDialer func(context.Context, string, string) (net.Conn, error),
	dialContext func(context.Context, string, string) (net.Conn, error),
	transport *http.Transport,
) (net.Conn, error) {
	if customTLSDialer != nil {
		conn, errTLS := customTLSDialer(ctx, network, address)
		if errTLS != nil {
			return nil, errTLS
		}
		if errProtocol := requireHTTP1TLS(ctx, conn, "custom TLS proxy dialer"); errProtocol != nil {
			return nil, errProtocol
		}
		return conn, nil
	}
	rawConn, errDial := dialContext(ctx, network, address)
	if errDial != nil {
		return nil, errDial
	}
	return clientTLSConnection(ctx, rawConn, address, transport.TLSClientConfig)
}

func clientTLSConnection(
	ctx context.Context,
	rawConn net.Conn,
	address string,
	baseConfig *tls.Config,
) (*tls.Conn, error) {
	if rawConn == nil {
		return nil, fmt.Errorf("pluginhost: TLS connection is nil")
	}
	config := &tls.Config{}
	if baseConfig != nil {
		config = baseConfig.Clone()
	}
	host, _, errSplit := net.SplitHostPort(address)
	if errSplit != nil {
		host = address
	}
	if config.ServerName == "" {
		config.ServerName = host
	}
	config.NextProtos = []string{"http/1.1"}
	tlsConn := tls.Client(rawConn, config)
	if errHandshake := tlsConn.HandshakeContext(ctx); errHandshake != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("pluginhost TLS handshake: %w", errHandshake)
	}
	if errProtocol := requireHTTP1TLS(ctx, tlsConn, "TLS dialer"); errProtocol != nil {
		return nil, errProtocol
	}
	return tlsConn, nil
}

type requestHolder struct {
	mu  sync.Mutex
	req *http.Request
}

func (h *requestHolder) get() *http.Request {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.req
}

func (h *requestHolder) set(req *http.Request) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.req = req
	h.mu.Unlock()
}

func resolveProxyForRequest(
	request *http.Request,
	auth *coreauth.Auth,
	cfg *config.Config,
	proxyFunction func(*http.Request) (*url.URL, error),
) (*url.URL, error) {
	if auth != nil && strings.TrimSpace(auth.ProxyURL) != "" {
		setting, errParse := proxyutil.Parse(strings.TrimSpace(auth.ProxyURL))
		if errParse != nil {
			return nil, fmt.Errorf("pluginhost: parse auth proxy: %w", errParse)
		}
		switch setting.Mode {
		case proxyutil.ModeDirect:
			return nil, nil
		case proxyutil.ModeProxy:
			return setting.URL, nil
		}
	}
	if cfg != nil && strings.TrimSpace(cfg.ProxyURL) != "" {
		setting, errParse := proxyutil.Parse(strings.TrimSpace(cfg.ProxyURL))
		if errParse != nil {
			return nil, fmt.Errorf("pluginhost: parse config proxy: %w", errParse)
		}
		switch setting.Mode {
		case proxyutil.ModeDirect:
			return nil, nil
		case proxyutil.ModeProxy:
			return setting.URL, nil
		}
	}
	if proxyFunction != nil && request != nil {
		return proxyFunction(request)
	}
	return nil, nil
}

func dialProxyTunnel(
	ctx context.Context,
	baseDial func(context.Context, string, string) (net.Conn, error),
	baseDialTLS func(context.Context, string, string) (net.Conn, error),
	proxyURL *url.URL,
	targetAddress string,
	transport *http.Transport,
) (net.Conn, error) {
	if proxyURL == nil {
		return baseDial(ctx, "tcp", targetAddress)
	}
	proxyPort := proxyURL.Port()
	if proxyPort == "" {
		switch strings.ToLower(proxyURL.Scheme) {
		case "https":
			proxyPort = "443"
		case "socks5", "socks5h":
			proxyPort = "1080"
		default:
			proxyPort = "80"
		}
	}
	proxyAddress := net.JoinHostPort(proxyURL.Hostname(), proxyPort)
	if baseDial == nil {
		baseDial = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}

	if strings.EqualFold(proxyURL.Scheme, "socks5") || strings.EqualFold(proxyURL.Scheme, "socks5h") {
		var proxyAuth *proxy.Auth
		if proxyURL.User != nil {
			password, _ := proxyURL.User.Password()
			proxyAuth = &proxy.Auth{User: proxyURL.User.Username(), Password: password}
		}
		dialer, errSOCKS := proxy.SOCKS5(
			"tcp",
			proxyAddress,
			proxyAuth,
			&contextDialerWrapper{dial: baseDial},
		)
		if errSOCKS != nil {
			return nil, fmt.Errorf("create SOCKS5 dialer: %w", errSOCKS)
		}
		if contextDialer, okContext := dialer.(proxy.ContextDialer); okContext {
			return contextDialer.DialContext(ctx, "tcp", targetAddress)
		}
		return dialer.Dial("tcp", targetAddress)
	}

	var conn net.Conn
	if strings.EqualFold(proxyURL.Scheme, "https") && baseDialTLS != nil {
		proxyTLS, errTLS := baseDialTLS(ctx, "tcp", proxyAddress)
		if errTLS != nil {
			return nil, fmt.Errorf("HTTPS proxy custom TLS dial failed: %w", errTLS)
		}
		if errProtocol := requireHTTP1TLS(ctx, proxyTLS, "HTTPS proxy custom TLS dialer"); errProtocol != nil {
			return nil, errProtocol
		}
		conn = proxyTLS
	} else {
		rawConn, errDial := baseDial(ctx, "tcp", proxyAddress)
		if errDial != nil {
			return nil, fmt.Errorf("dial proxy %s: %w", redactedProxyURL(proxyURL.String()), errDial)
		}
		conn = rawConn
		if strings.EqualFold(proxyURL.Scheme, "https") {
			proxyTLS, errTLS := clientTLSConnection(ctx, rawConn, proxyAddress, transport.TLSClientConfig)
			if errTLS != nil {
				return nil, fmt.Errorf("HTTPS proxy TLS handshake: %w", errTLS)
			}
			conn = proxyTLS
		}
	}

	cancelComplete := make(chan struct{})
	stopCancellation := context.AfterFunc(ctx, func() {
		_ = conn.Close()
		close(cancelComplete)
	})
	defer func() {
		if !stopCancellation() {
			<-cancelComplete
		}
	}()

	connectRequest := (&http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: targetAddress},
		Host:   targetAddress,
		Header: make(http.Header),
	}).WithContext(ctx)
	if transport != nil {
		if transport.GetProxyConnectHeader != nil {
			headers, errHeaders := transport.GetProxyConnectHeader(ctx, proxyURL, targetAddress)
			if errHeaders != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("get proxy connect header: %w", errHeaders)
			}
			connectRequest.Header = cloneHeader(headers)
		} else {
			connectRequest.Header = cloneHeader(transport.ProxyConnectHeader)
		}
	}
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + password))
		connectRequest.Header.Set("Proxy-Authorization", "Basic "+token)
	}
	if errWrite := connectRequest.Write(conn); errWrite != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write CONNECT request: %w", errWrite)
	}
	buffered := bufio.NewReader(conn)
	response, errResponse := http.ReadResponse(buffered, connectRequest)
	if errResponse != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read CONNECT response: %w", errResponse)
	}
	if transport != nil && transport.OnProxyConnectResponse != nil {
		if errCallback := transport.OnProxyConnectResponse(ctx, proxyURL, connectRequest, response); errCallback != nil {
			_ = response.Body.Close()
			_ = conn.Close()
			return nil, fmt.Errorf("on proxy connect response: %w", errCallback)
		}
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed with status %s", response.Status)
	}
	if buffered.Buffered() > 0 {
		return &bufferedConn{Conn: conn, reader: buffered}, nil
	}
	return conn, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(data []byte) (int, error) {
	return c.reader.Read(data)
}

type contextDialerWrapper struct {
	dial func(context.Context, string, string) (net.Conn, error)
}

func (w *contextDialerWrapper) Dial(network, address string) (net.Conn, error) {
	return w.dial(context.Background(), network, address)
}

func (w *contextDialerWrapper) DialContext(
	ctx context.Context,
	network string,
	address string,
) (net.Conn, error) {
	return w.dial(ctx, network, address)
}
