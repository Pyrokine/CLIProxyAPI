package pluginhost

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

type affinityLookupFunc func(provider, model, sessionID string) (*coreauth.Auth, string)

func (f affinityLookupFunc) LookupSessionAffinity(provider, model, sessionID string) (*coreauth.Auth, string) {
	return f(provider, model, sessionID)
}

type fakeHostModelExecutor struct {
	executeModel func(context.Context, handlers.ModelExecutionRequest) (
		handlers.ModelExecutionResponse,
		*handlers.ErrorMessage,
	)
	executeModelStream func(context.Context, handlers.ModelExecutionRequest) (
		handlers.ModelExecutionStream,
		*handlers.ErrorMessage,
	)
}

func (e *fakeHostModelExecutor) ExecuteModel(
	ctx context.Context,
	req handlers.ModelExecutionRequest,
) (handlers.ModelExecutionResponse, *handlers.ErrorMessage) {
	return e.executeModel(ctx, req)
}

func (e *fakeHostModelExecutor) ExecuteModelStream(
	ctx context.Context,
	req handlers.ModelExecutionRequest,
) (handlers.ModelExecutionStream, *handlers.ErrorMessage) {
	return e.executeModelStream(ctx, req)
}

func TestHostAffinityLookupValidatesRequiredFields(t *testing.T) {
	host := New()
	for _, testCase := range []struct {
		name    string
		request string
		want    string
	}{
		{name: "invalid JSON", request: `{`, want: "decode host affinity lookup request"},
		{name: "provider", request: `{"model":"m","session_id":"s"}`, want: "provider is required"},
		{name: "model", request: `{"provider":"p","session_id":"s"}`, want: "model is required"},
		{name: "session", request: `{"provider":"p","model":"m"}`, want: "session_id is required"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, errLookup := host.callHostAffinityLookup(context.Background(), []byte(testCase.request))
			if errLookup == nil || !strings.Contains(errLookup.Error(), testCase.want) {
				t.Fatalf("callHostAffinityLookup() error = %v, want %q", errLookup, testCase.want)
			}
		})
	}
}

func TestHostAffinityLookupReturnsReadOnlyStatusContract(t *testing.T) {
	host := New()
	unsupportedRaw, errUnsupported := host.callFromPlugin(
		context.Background(),
		pluginabi.MethodHostAffinityLookup,
		[]byte(`{"provider":"openai","model":"model","session_id":"session"}`),
	)
	if errUnsupported != nil {
		t.Fatalf("unsupported lookup error = %v", errUnsupported)
	}
	unsupported, errDecode := decodeRPCEnvelope[pluginapi.HostAffinityLookupResponse](unsupportedRaw)
	if errDecode != nil || unsupported.Status != pluginapi.HostAffinityStatusUnsupported || unsupported.ObservedAt.IsZero() {
		t.Fatalf("unsupported response = %#v, error=%v", unsupported, errDecode)
	}

	calls := 0
	host.SetAffinityManager(affinityLookupFunc(func(provider, model, sessionID string) (*coreauth.Auth, string) {
		calls++
		if provider != "openai" || model != "model" || sessionID != "session" {
			t.Fatalf("lookup arguments = %q %q %q", provider, model, sessionID)
		}
		return &coreauth.Auth{
			ID:          "secret-auth-id",
			Index:       "auth-index",
			Provider:    "openai",
			Disabled:    true,
			Unavailable: true,
			Metadata:    map[string]any{"token": "secret-token"},
		}, pluginapi.HostAffinityStatusBound
	}))
	boundRaw, errBound := host.callFromPlugin(
		context.Background(),
		pluginabi.MethodHostAffinityLookup,
		[]byte(`{"provider":" openai ","model":" model ","session_id":" session "}`),
	)
	if errBound != nil {
		t.Fatalf("bound lookup error = %v", errBound)
	}
	if bytes.Contains(boundRaw, []byte("secret-auth-id")) || bytes.Contains(boundRaw, []byte("secret-token")) {
		t.Fatalf("affinity response leaked credential data: %s", boundRaw)
	}
	bound, errDecodeBound := decodeRPCEnvelope[pluginapi.HostAffinityLookupResponse](boundRaw)
	if errDecodeBound != nil {
		t.Fatalf("decode bound response: %v", errDecodeBound)
	}
	if calls != 1 || bound.Status != pluginapi.HostAffinityStatusBound || bound.AuthIndex != "auth-index" ||
		!bound.Disabled || !bound.Unavailable || bound.ObservedAt.IsZero() {
		t.Fatalf("bound response = %#v, calls=%d", bound, calls)
	}

	missingIndexAuth := &coreauth.Auth{ID: "derivable-auth-id", Provider: "openai"}
	host.SetAffinityManager(affinityLookupFunc(func(string, string, string) (*coreauth.Auth, string) {
		return missingIndexAuth, pluginapi.HostAffinityStatusBound
	}))
	if _, errMissingIndex := host.callHostAffinityLookup(
		context.Background(), []byte(`{"provider":"p","model":"m","session_id":"s"}`),
	); errMissingIndex == nil || !strings.Contains(errMissingIndex.Error(), "without auth index") {
		t.Fatalf("missing-index lookup error = %v", errMissingIndex)
	}
	if missingIndexAuth.Index != "" {
		t.Fatalf("read-only affinity lookup mutated auth index to %q", missingIndexAuth.Index)
	}

	for _, status := range []string{
		pluginapi.HostAffinityStatusUnbound,
		pluginapi.HostAffinityStatusAmbiguous,
		pluginapi.HostAffinityStatusUnsupported,
	} {
		host.SetAffinityManager(affinityLookupFunc(func(string, string, string) (*coreauth.Auth, string) {
			return nil, status
		}))
		raw, errLookup := host.callHostAffinityLookup(
			context.Background(), []byte(`{"provider":"p","model":"m","session_id":"s"}`),
		)
		if errLookup != nil {
			t.Fatalf("status %q lookup error = %v", status, errLookup)
		}
		response, errDecodeStatus := decodeRPCEnvelope[pluginapi.HostAffinityLookupResponse](raw)
		if errDecodeStatus != nil || response.Status != status || response.AuthIndex != "" || response.ObservedAt.IsZero() {
			t.Fatalf("status %q response = %#v, error=%v", status, response, errDecodeStatus)
		}
	}
}

func TestHostHTTPDoCallbackUsesHostHTTPClient(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Fatalf("method = %s, want POST", r.Method)
				}
				w.Header().Set("X-Test", "ok")
				_, _ = w.Write([]byte(`{"ok":true}`))
			},
		),
	)
	defer server.Close()

	req := pluginapi.HTTPRequest{
		Method: http.MethodPost,
		URL:    server.URL,
		Body:   []byte(`{"request":true}`),
	}
	rawReq, errMarshal := json.Marshal(req)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}

	rawResp, errCall := New().callFromPlugin(context.Background(), pluginabi.MethodHostHTTPDo, rawReq)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}

	resp, errDecode := decodeRPCEnvelope[pluginapi.HTTPResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.StatusCode != http.StatusOK || string(resp.Body) != `{"ok":true}` {
		t.Fatalf("response = %#v, want status 200 body", resp)
	}
	if resp.Headers.Get("X-Test") != "ok" {
		t.Fatalf("X-Test = %q, want ok", resp.Headers.Get("X-Test"))
	}
}

func TestDecodeHostHTTPRequestPrefersNestedWireProfileAndClonesIt(t *testing.T) {
	raw := []byte(`{
		"wire_profile":{"http1_only":true,"header_profile":["Top-Level"]},
		"request":{
			"method":"POST",
			"url":"https://example.test",
			"wire_profile":{"disable_auto_compression":true,"header_profile":["Nested-Header"]}
		}
	}`)
	decoded, _, errDecode := decodeHostHTTPRequestWithCallbackID(raw)
	if errDecode != nil {
		t.Fatalf("decodeHostHTTPRequestWithCallbackID() error = %v", errDecode)
	}
	if decoded.WireProfile == nil || decoded.WireProfile.HTTP1Only ||
		!decoded.WireProfile.DisableAutoCompression ||
		!reflect.DeepEqual(decoded.WireProfile.HeaderProfile, []string{"Nested-Header"}) {
		t.Fatalf("decoded nested wire profile = %#v", decoded.WireProfile)
	}

	profile := &pluginapi.HTTPWireProfile{HTTP1Only: true, HeaderProfile: []string{"Original"}}
	encoded, errMarshal := json.Marshal(rpcHostHTTPRequest{
		Request: &httpRequest{Method: http.MethodGet, URL: "http://example.test", WireProfile: profile},
	})
	if errMarshal != nil {
		t.Fatalf("json.Marshal() error = %v", errMarshal)
	}
	cloned, _, errClone := decodeHostHTTPRequestWithCallbackID(encoded)
	if errClone != nil {
		t.Fatalf("decodeHostHTTPRequestWithCallbackID() clone error = %v", errClone)
	}
	profile.HeaderProfile[0] = "Mutated"
	if cloned.WireProfile == nil || cloned.WireProfile.HeaderProfile[0] != "Original" {
		t.Fatalf("wire profile was not defensively copied: %#v", cloned.WireProfile)
	}
}

func TestHostHTTPClientAppliesHTTP1HeaderWireProfile(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("net.Listen() error = %v", errListen)
	}
	defer listener.Close()
	type serverResult struct {
		header string
		err    error
	}
	result := make(chan serverResult, 1)
	go func() {
		connection, errAccept := listener.Accept()
		if errAccept != nil {
			result <- serverResult{err: errAccept}
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		var raw strings.Builder
		for {
			line, errRead := reader.ReadString('\n')
			if errRead != nil {
				result <- serverResult{err: errRead}
				return
			}
			raw.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		_, errWrite := connection.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"))
		result <- serverResult{header: raw.String(), err: errWrite}
	}()

	profile := &pluginapi.HTTPWireProfile{
		HTTP1Only:              true,
		DisableAutoCompression: true,
		HeaderProfile:          []string{"x-custom-b", "X-Custom-A", "User-Agent", "Host"},
	}
	response, errDo := New().newHTTPClient(nil).Do(context.Background(), pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    "http://" + listener.Addr().String() + "/wire",
		Headers: http.Header{
			"X-Custom-A": {"a"},
			"X-Custom-B": {"b"},
			"User-Agent": {"wire-test"},
		},
		WireProfile: profile,
	})
	if errDo != nil {
		t.Fatalf("HostHTTPClient.Do() error = %v", errDo)
	}
	if response.StatusCode != http.StatusOK || string(response.Body) != "ok" {
		t.Fatalf("response = %#v", response)
	}
	server := <-result
	if server.err != nil {
		t.Fatalf("raw server error = %v", server.err)
	}
	if !strings.HasPrefix(server.header, "GET /wire HTTP/1.1\r\n") {
		t.Fatalf("request line = %q", server.header)
	}
	positions := []int{
		strings.Index(server.header, "\r\nx-custom-b: b\r\n"),
		strings.Index(server.header, "\r\nX-Custom-A: a\r\n"),
		strings.Index(server.header, "\r\nUser-Agent: wire-test\r\n"),
		strings.Index(server.header, "\r\nHost: "+listener.Addr().String()+"\r\n"),
	}
	for index, position := range positions {
		if position < 0 {
			t.Fatalf("profile header %d missing from raw request: %q", index, server.header)
		}
		if index > 0 && positions[index-1] >= position {
			t.Fatalf("profile header order = %#v in %q", positions, server.header)
		}
	}
	if strings.Contains(strings.ToLower(server.header), "accept-encoding:") {
		t.Fatalf("DisableAutoCompression injected Accept-Encoding: %q", server.header)
	}
}

func TestHostHTTPClientWireProfileUsesPlainHTTPProxy(t *testing.T) {
	proxyRequest := make(chan string, 1)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		proxyRequest <- request.Method + " " + request.URL.String()
		_, _ = w.Write([]byte("proxied"))
	}))
	defer proxyServer.Close()
	host := New()
	host.runtimeConfig = &config.Config{ProxyURL: proxyServer.URL}
	response, errDo := host.newHTTPClient(nil).Do(context.Background(), pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    "http://example.test/resource",
		WireProfile: &pluginapi.HTTPWireProfile{
			HTTP1Only:     true,
			HeaderProfile: []string{"Host", "User-Agent"},
		},
	})
	if errDo != nil {
		t.Fatalf("HostHTTPClient.Do() error = %v", errDo)
	}
	if string(response.Body) != "proxied" {
		t.Fatalf("proxy response body = %q", response.Body)
	}
	select {
	case got := <-proxyRequest:
		if got != "GET http://example.test/resource" {
			t.Fatalf("proxy request = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy did not receive request")
	}
}

func TestHostHTTPClientWireProfileRejectsCustomTLSHTTP2(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	serverTransport := server.Client().Transport.(*http.Transport)
	customTransport := &http.Transport{
		TLSClientConfig: serverTransport.TLSClientConfig.Clone(),
		DialTLSContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			rawConnection, errDial := (&net.Dialer{}).DialContext(ctx, network, address)
			if errDial != nil {
				return nil, errDial
			}
			tlsConfig := serverTransport.TLSClientConfig.Clone()
			tlsConfig.InsecureSkipVerify = true
			tlsConfig.NextProtos = []string{"h2"}
			connection := tls.Client(rawConnection, tlsConfig)
			if errHandshake := connection.HandshakeContext(ctx); errHandshake != nil {
				_ = rawConnection.Close()
				return nil, errHandshake
			}
			return connection, nil
		},
	}
	ctx := util.WithRoundTripper(context.Background(), customTransport)
	_, errDo := New().newHTTPClient(nil).Do(ctx, pluginapi.HTTPRequest{
		URL: server.URL,
		WireProfile: &pluginapi.HTTPWireProfile{
			HTTP1Only: true,
		},
	})
	if errDo == nil || !strings.Contains(errDo.Error(), "requires HTTP/1.1") {
		t.Fatalf("HostHTTPClient.Do() error = %v, want HTTP/2 rejection", errDo)
	}
}

func TestDialProxyTunnelHTTPConnectCarriesTraffic(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("net.Listen() error = %v", errListen)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	go func() {
		connection, errAccept := listener.Accept()
		if errAccept != nil {
			serverErr <- errAccept
			return
		}
		defer connection.Close()
		reader := bufio.NewReader(connection)
		for {
			line, errRead := reader.ReadString('\n')
			if errRead != nil {
				serverErr <- errRead
				return
			}
			if line == "\r\n" {
				break
			}
		}
		if _, errWrite := connection.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); errWrite != nil {
			serverErr <- errWrite
			return
		}
		payload := make([]byte, 4)
		if _, errRead := io.ReadFull(reader, payload); errRead != nil {
			serverErr <- errRead
			return
		}
		if string(payload) != "ping" {
			serverErr <- fmt.Errorf("tunnel payload = %q", payload)
			return
		}
		_, errWrite := connection.Write([]byte("pong"))
		serverErr <- errWrite
	}()
	proxyURL, errParse := url.Parse("http://" + listener.Addr().String())
	if errParse != nil {
		t.Fatalf("url.Parse() error = %v", errParse)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, errTunnel := dialProxyTunnel(
		ctx, (&net.Dialer{}).DialContext, nil, proxyURL, "target.example:443", &http.Transport{},
	)
	if errTunnel != nil {
		t.Fatalf("dialProxyTunnel() error = %v", errTunnel)
	}
	defer connection.Close()
	if _, errWrite := connection.Write([]byte("ping")); errWrite != nil {
		t.Fatalf("tunnel write error = %v", errWrite)
	}
	response := make([]byte, 4)
	if _, errRead := io.ReadFull(connection, response); errRead != nil {
		t.Fatalf("tunnel read error = %v", errRead)
	}
	if string(response) != "pong" {
		t.Fatalf("tunnel response = %q", response)
	}
	if errServer := <-serverErr; errServer != nil {
		t.Fatalf("proxy server error = %v", errServer)
	}
}

func TestDialProxyTunnelHTTPSConnectCarriesTraffic(t *testing.T) {
	serverErr := make(chan error, 1)
	proxyServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect || request.Host != "target.example:443" {
			serverErr <- fmt.Errorf("HTTPS proxy request = %s %s", request.Method, request.Host)
			http.Error(writer, "unexpected CONNECT", http.StatusBadRequest)
			return
		}
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			serverErr <- fmt.Errorf("HTTPS proxy response writer does not support hijacking")
			return
		}
		connection, buffered, errHijack := hijacker.Hijack()
		if errHijack != nil {
			serverErr <- errHijack
			return
		}
		defer connection.Close()
		if _, errWrite := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); errWrite != nil {
			serverErr <- errWrite
			return
		}
		if errFlush := buffered.Flush(); errFlush != nil {
			serverErr <- errFlush
			return
		}
		payload := make([]byte, 4)
		if _, errRead := io.ReadFull(buffered, payload); errRead != nil {
			serverErr <- errRead
			return
		}
		if string(payload) != "ping" {
			serverErr <- fmt.Errorf("HTTPS tunnel payload = %q", payload)
			return
		}
		if _, errWrite := buffered.WriteString("pong"); errWrite != nil {
			serverErr <- errWrite
			return
		}
		serverErr <- buffered.Flush()
	}))
	defer proxyServer.Close()
	proxyURL, errParse := url.Parse(proxyServer.URL)
	if errParse != nil {
		t.Fatalf("url.Parse() error = %v", errParse)
	}
	proxyTransport := proxyServer.Client().Transport.(*http.Transport)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, errTunnel := dialProxyTunnel(
		ctx,
		(&net.Dialer{}).DialContext,
		nil,
		proxyURL,
		"target.example:443",
		&http.Transport{TLSClientConfig: proxyTransport.TLSClientConfig.Clone()},
	)
	if errTunnel != nil {
		t.Fatalf("dialProxyTunnel() error = %v", errTunnel)
	}
	defer connection.Close()
	if _, errWrite := connection.Write([]byte("ping")); errWrite != nil {
		t.Fatalf("HTTPS tunnel write error = %v", errWrite)
	}
	response := make([]byte, 4)
	if _, errRead := io.ReadFull(connection, response); errRead != nil {
		t.Fatalf("HTTPS tunnel read error = %v", errRead)
	}
	if string(response) != "pong" {
		t.Fatalf("HTTPS tunnel response = %q", response)
	}
	if errServer := <-serverErr; errServer != nil {
		t.Fatalf("HTTPS proxy server error = %v", errServer)
	}
}

func TestDialProxyTunnelPreservesBufferedPayload(t *testing.T) {
	clientConnection, proxyConnection := net.Pipe()
	defer clientConnection.Close()
	defer proxyConnection.Close()
	serverErr := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(proxyConnection)
		for {
			line, errRead := reader.ReadString('\n')
			if errRead != nil {
				serverErr <- errRead
				return
			}
			if line == "\r\n" {
				break
			}
		}
		if _, errWrite := proxyConnection.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\npong")); errWrite != nil {
			serverErr <- errWrite
			return
		}
		payload := make([]byte, 4)
		if _, errRead := io.ReadFull(reader, payload); errRead != nil {
			serverErr <- errRead
			return
		}
		if string(payload) != "ping" {
			serverErr <- fmt.Errorf("tunnel payload = %q", payload)
			return
		}
		serverErr <- nil
	}()
	proxyURL, errParse := url.Parse("http://proxy.test")
	if errParse != nil {
		t.Fatalf("url.Parse() error = %v", errParse)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, errTunnel := dialProxyTunnel(
		ctx,
		func(context.Context, string, string) (net.Conn, error) {
			return clientConnection, nil
		},
		nil,
		proxyURL,
		"target.example:443",
		&http.Transport{},
	)
	if errTunnel != nil {
		t.Fatalf("dialProxyTunnel() error = %v", errTunnel)
	}
	if errDeadline := connection.SetDeadline(time.Now().Add(time.Second)); errDeadline != nil {
		t.Fatalf("SetDeadline() error = %v", errDeadline)
	}
	if _, errWrite := connection.Write([]byte("ping")); errWrite != nil {
		t.Fatalf("tunnel write error = %v", errWrite)
	}
	response := make([]byte, 4)
	if _, errRead := io.ReadFull(connection, response); errRead != nil {
		t.Fatalf("tunnel read error = %v", errRead)
	}
	if string(response) != "pong" {
		t.Fatalf("tunnel response = %q", response)
	}
	if errServer := <-serverErr; errServer != nil {
		t.Fatalf("proxy server error = %v", errServer)
	}
}

func TestDialProxyTunnelSOCKS5CarriesTraffic(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("net.Listen() error = %v", errListen)
	}
	defer listener.Close()
	serverErr := make(chan error, 1)
	go func() {
		connection, errAccept := listener.Accept()
		if errAccept != nil {
			serverErr <- errAccept
			return
		}
		defer connection.Close()
		greeting := make([]byte, 3)
		if _, errRead := io.ReadFull(connection, greeting); errRead != nil {
			serverErr <- errRead
			return
		}
		if !bytes.Equal(greeting, []byte{0x05, 0x01, 0x00}) {
			serverErr <- fmt.Errorf("SOCKS greeting = %v", greeting)
			return
		}
		if _, errWrite := connection.Write([]byte{0x05, 0x00}); errWrite != nil {
			serverErr <- errWrite
			return
		}
		requestHeader := make([]byte, 4)
		if _, errRead := io.ReadFull(connection, requestHeader); errRead != nil {
			serverErr <- errRead
			return
		}
		if requestHeader[0] != 0x05 || requestHeader[1] != 0x01 {
			serverErr <- fmt.Errorf("SOCKS request header = %v", requestHeader)
			return
		}
		switch requestHeader[3] {
		case 0x01:
			_, errAccept = io.ReadFull(connection, make([]byte, 4+2))
		case 0x03:
			length := make([]byte, 1)
			if _, errAccept = io.ReadFull(connection, length); errAccept == nil {
				_, errAccept = io.ReadFull(connection, make([]byte, int(length[0])+2))
			}
		case 0x04:
			_, errAccept = io.ReadFull(connection, make([]byte, 16+2))
		default:
			errAccept = fmt.Errorf("SOCKS address type = %d", requestHeader[3])
		}
		if errAccept != nil {
			serverErr <- errAccept
			return
		}
		if _, errWrite := connection.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 80}); errWrite != nil {
			serverErr <- errWrite
			return
		}
		payload := make([]byte, 4)
		if _, errRead := io.ReadFull(connection, payload); errRead != nil {
			serverErr <- errRead
			return
		}
		if string(payload) != "ping" {
			serverErr <- fmt.Errorf("SOCKS payload = %q", payload)
			return
		}
		_, errWrite := connection.Write([]byte("pong"))
		serverErr <- errWrite
	}()
	proxyURL, errParse := url.Parse("socks5://" + listener.Addr().String())
	if errParse != nil {
		t.Fatalf("url.Parse() error = %v", errParse)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, errTunnel := dialProxyTunnel(
		ctx, (&net.Dialer{}).DialContext, nil, proxyURL, "target.example:443", &http.Transport{},
	)
	if errTunnel != nil {
		t.Fatalf("dialProxyTunnel() error = %v", errTunnel)
	}
	defer connection.Close()
	if _, errWrite := connection.Write([]byte("ping")); errWrite != nil {
		t.Fatalf("SOCKS tunnel write error = %v", errWrite)
	}
	response := make([]byte, 4)
	if _, errRead := io.ReadFull(connection, response); errRead != nil {
		t.Fatalf("SOCKS tunnel read error = %v", errRead)
	}
	if string(response) != "pong" {
		t.Fatalf("SOCKS tunnel response = %q", response)
	}
	if errServer := <-serverErr; errServer != nil {
		t.Fatalf("SOCKS server error = %v", errServer)
	}
}

func TestDialProxyTunnelCancellationClosesConnection(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("net.Listen() error = %v", errListen)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	closed := make(chan struct{})
	go func() {
		connection, errAccept := listener.Accept()
		if errAccept != nil {
			close(accepted)
			close(closed)
			return
		}
		defer connection.Close()
		close(accepted)
		reader := bufio.NewReader(connection)
		for {
			line, errRead := reader.ReadString('\n')
			if errRead != nil {
				close(closed)
				return
			}
			if line == "\r\n" {
				break
			}
		}
		_, _ = reader.ReadByte()
		close(closed)
	}()
	proxyURL, errParse := url.Parse("http://" + listener.Addr().String())
	if errParse != nil {
		t.Fatalf("url.Parse() error = %v", errParse)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, errTunnel := dialProxyTunnel(
			ctx, (&net.Dialer{}).DialContext, nil, proxyURL, "target.example:443", &http.Transport{},
		)
		result <- errTunnel
	}()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("proxy did not accept connection")
	}
	cancel()
	select {
	case errTunnel := <-result:
		if errTunnel == nil {
			t.Fatal("dialProxyTunnel() error = nil after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("dialProxyTunnel() did not return after cancellation")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("proxy connection remained open after cancellation")
	}
}

func TestReadPluginHTTPResponseRejectsContentLengthOverLimit(t *testing.T) {
	_, err := readPluginHTTPResponse(strings.NewReader("small"), 11, 10)
	if !errors.Is(err, errPluginHTTPResponseTooLarge) {
		t.Fatalf("readPluginHTTPResponse() error = %v, want %v", err, errPluginHTTPResponseTooLarge)
	}
}

func TestReadPluginHTTPResponseRejectsChunkedBodyOverLimit(t *testing.T) {
	_, err := readPluginHTTPResponse(strings.NewReader("eleven-byte"), -1, 10)
	if !errors.Is(err, errPluginHTTPResponseTooLarge) {
		t.Fatalf("readPluginHTTPResponse() error = %v, want %v", err, errPluginHTTPResponseTooLarge)
	}
}

func TestHostHTTPDoAppliesDefaultTimeout(t *testing.T) {
	previousTimeout := pluginHTTPDefaultTimeout
	pluginHTTPDefaultTimeout = 20 * time.Millisecond
	defer func() { pluginHTTPDefaultTimeout = previousTimeout }()

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			},
		),
	)
	defer server.Close()

	_, err := New().newHTTPClient(nil).Do(
		context.Background(),
		pluginapi.HTTPRequest{Method: http.MethodGet, URL: server.URL},
	)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Do() error = %v, want context deadline exceeded", err)
	}
}

func TestHostHTTPStreamWireProfileCancellationClosesUpstream(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer does not support flushing")
			return
		}
		_, _ = w.Write([]byte("first"))
		flusher.Flush()
		<-request.Context().Done()
		close(upstreamCanceled)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	response, errDo := New().newHTTPClient(nil).DoStream(ctx, pluginapi.HTTPRequest{
		URL: server.URL,
		WireProfile: &pluginapi.HTTPWireProfile{
			HTTP1Only: true,
		},
	})
	if errDo != nil {
		cancel()
		t.Fatalf("DoStream() error = %v", errDo)
	}
	select {
	case chunk := <-response.Chunks:
		if string(chunk.Payload) != "first" || chunk.Err != nil {
			t.Fatalf("first stream chunk = %#v", chunk)
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("stream did not produce first chunk")
	}
	cancel()
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("stream cancellation did not close upstream request")
	}
	select {
	case _, open := <-response.Chunks:
		if open {
			for range response.Chunks {
			}
		}
	case <-time.After(time.Second):
		t.Fatal("stream channel did not close after cancellation")
	}
}

func TestHostHTTPDoCallbackRestoresRegisteredRequestContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := util.WithGinContext(context.Background(), ginCtx)

	host := New()
	host.mu.Lock()
	host.runtimeConfig = &config.Config{RequestLog: true}
	host.mu.Unlock()
	callbackID, closeCallback := host.openCallbackContext(ctx)
	defer closeCallback()

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				if r.Context().Err() != nil {
					t.Fatalf("request context error = %v", r.Context().Err())
				}
				w.Header().Set("X-Upstream", "ok")
				_, _ = w.Write([]byte("upstream-body"))
			},
		),
	)
	defer server.Close()

	rawReq, errMarshal := json.Marshal(
		rpcHostHTTPRequest{
			HostCallbackID: callbackID,
			Method:         http.MethodPost,
			URL:            server.URL,
			Body:           []byte(`{"request":true}`),
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	if _, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostHTTPDo, rawReq); errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}

	rawAPIRequest, okRequest := ginCtx.Get("API_REQUEST")
	if !okRequest {
		t.Fatal("API_REQUEST was not captured on the original Gin context")
	}
	apiRequest := rawAPIRequest.(*logging.BoundedLogBuffer).Bytes()
	if !bytes.Contains(apiRequest, []byte("=== API REQUEST 1 ===")) || !bytes.Contains(
		apiRequest, []byte(`{"request":true}`),
	) {
		t.Fatalf("API_REQUEST = %q, want upstream request details", apiRequest)
	}

	rawAPIResponse, okResponse := ginCtx.Get("API_RESPONSE")
	if !okResponse {
		t.Fatal("API_RESPONSE was not captured on the original Gin context")
	}
	apiResponse := rawAPIResponse.(*logging.BoundedLogBuffer).Bytes()
	if !bytes.Contains(apiResponse, []byte("=== API RESPONSE 1 ===")) || !bytes.Contains(
		apiResponse, []byte("upstream-body"),
	) {
		t.Fatalf("API_RESPONSE = %q, want upstream response details", apiResponse)
	}
}

func TestHostHTTPDoStreamCallbackReturnsBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("first"))
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				<-release
				_, _ = w.Write([]byte("second"))
			},
		),
	)
	defer server.Close()
	defer close(release)

	rawReq, errMarshal := json.Marshal(
		pluginapi.HTTPRequest{
			Method: http.MethodGet,
			URL:    server.URL,
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}

	type callResult struct {
		raw []byte
		err error
	}
	done := make(chan callResult, 1)
	host := New()
	go func() {
		rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostHTTPDoStream, rawReq)
		done <- callResult{raw: rawResp, err: errCall}
	}()

	var result callResult
	select {
	case result = <-done:
	case <-time.After(time.Second):
		t.Fatal("host.http.do_stream waited for the whole upstream response")
	}
	if result.err != nil {
		t.Fatalf("callFromPlugin() error = %v", result.err)
	}

	resp, errDecode := decodeRPCEnvelope[rpcHostHTTPStreamResponse](result.raw)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.StreamID == "" {
		t.Fatalf("stream id is empty: %#v", resp)
	}
	readReq, errMarshal := json.Marshal(rpcHostHTTPStreamReadRequest{StreamID: resp.StreamID})
	if errMarshal != nil {
		t.Fatalf("marshal read request: %v", errMarshal)
	}
	rawRead, errRead := host.callFromPlugin(context.Background(), pluginabi.MethodHostHTTPStreamRead, readReq)
	if errRead != nil {
		t.Fatalf("read callback error = %v", errRead)
	}
	chunk, errDecode := decodeRPCEnvelope[rpcHostHTTPStreamReadResponse](rawRead)
	if errDecode != nil {
		t.Fatalf("decode read response: %v", errDecode)
	}
	if string(chunk.Payload) != "first" || chunk.Done || chunk.Error != "" {
		t.Fatalf("read chunk = %#v, want first payload", chunk)
	}

	closeReq, errMarshal := json.Marshal(rpcHostHTTPStreamCloseRequest{StreamID: resp.StreamID})
	if errMarshal != nil {
		t.Fatalf("marshal close request: %v", errMarshal)
	}
	if _, errClose := host.callFromPlugin(
		context.Background(), pluginabi.MethodHostHTTPStreamClose, closeReq,
	); errClose != nil {
		t.Fatalf("close callback error = %v", errClose)
	}
}

func TestHostStreamCallbacksEmitAndClose(t *testing.T) {
	host := New()
	streamID, chunks, cleanup := host.streams.open(context.Background())
	defer cleanup()

	emitReq, errMarshal := json.Marshal(rpcStreamEmitRequest{StreamID: streamID, Payload: []byte("chunk")})
	if errMarshal != nil {
		t.Fatalf("marshal emit request: %v", errMarshal)
	}
	if _, errEmit := host.callFromPlugin(
		context.Background(), pluginabi.MethodHostStreamEmit, emitReq,
	); errEmit != nil {
		t.Fatalf("emit callback error = %v", errEmit)
	}

	closeReq, errMarshal := json.Marshal(rpcStreamCloseRequest{StreamID: streamID})
	if errMarshal != nil {
		t.Fatalf("marshal close request: %v", errMarshal)
	}
	if _, errClose := host.callFromPlugin(
		context.Background(), pluginabi.MethodHostStreamClose, closeReq,
	); errClose != nil {
		t.Fatalf("close callback error = %v", errClose)
	}

	chunk, ok := <-chunks
	if !ok {
		t.Fatalf("stream closed before chunk")
	}
	if string(chunk.Payload) != "chunk" || chunk.Err != nil {
		t.Fatalf("chunk = %#v, want payload chunk", chunk)
	}
	if _, ok = <-chunks; ok {
		t.Fatalf("stream remains open after close")
	}
}

func TestHostModelExecuteCallback(t *testing.T) {
	host := New()
	var got handlers.ModelExecutionRequest
	host.SetModelExecutor(
		&fakeHostModelExecutor{
			executeModel: func(
				ctx context.Context,
				req handlers.ModelExecutionRequest,
			) (handlers.ModelExecutionResponse, *handlers.ErrorMessage) {
				got = req
				return handlers.ModelExecutionResponse{
					StatusCode: http.StatusAccepted,
					Headers:    http.Header{"X-Model": []string{"ok"}},
					Body:       []byte(`{"response":true}`),
				}, nil
			},
		},
	)

	rawReq, errMarshal := json.Marshal(
		rpcHostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "claude",
			Model:         "model-1",
			Body:          []byte(`{"request":true}`),
			Headers:       http.Header{"X-Request": []string{"yes"}},
			Query:         url.Values{"alt": []string{"sse"}},
			Alt:           "raw",
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecute, rawReq)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}

	resp, errDecode := decodeRPCEnvelope[pluginapi.HostModelExecutionResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.StatusCode != http.StatusAccepted || string(resp.Body) != `{"response":true}` {
		t.Fatalf("response = %#v, want accepted body", resp)
	}
	if resp.Headers.Get("X-Model") != "ok" {
		t.Fatalf("X-Model = %q, want ok", resp.Headers.Get("X-Model"))
	}
	if got.EntryProtocol != "openai" || got.ExitProtocol != "claude" || got.Model != "model-1" || got.Stream {
		t.Fatalf("request protocols/model/stream = %#v", got)
	}
	if string(got.Body) != `{"request":true}` {
		t.Fatalf("request body = %q, want original body", got.Body)
	}
	if got.Headers.Get("X-Request") != "yes" {
		t.Fatalf("request header = %q, want yes", got.Headers.Get("X-Request"))
	}
	if got.Query.Get("alt") != "sse" {
		t.Fatalf("query alt = %q, want sse", got.Query.Get("alt"))
	}
	if got.Alt != "raw" {
		t.Fatalf("alt = %q, want raw", got.Alt)
	}
}

func TestHostModelExecuteCallbackCarriesCallerPluginSkipID(t *testing.T) {
	host := New()
	var got handlers.ModelExecutionRequest
	host.SetModelExecutor(
		&fakeHostModelExecutor{
			executeModel: func(
				ctx context.Context,
				req handlers.ModelExecutionRequest,
			) (handlers.ModelExecutionResponse, *handlers.ErrorMessage) {
				got = req
				return handlers.ModelExecutionResponse{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
			},
		},
	)
	callbackID, closeCallback := host.openCallbackContextForPlugin(context.Background(), "origin-plugin")
	defer closeCallback()

	rawReq, errMarshal := json.Marshal(
		rpcHostModelExecutionRequest{
			EntryProtocol:  "openai",
			ExitProtocol:   "openai",
			Model:          "model-1",
			Body:           []byte(`{"request":true}`),
			HostCallbackID: callbackID,
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	if _, errCall := host.callFromPlugin(
		context.Background(), pluginabi.MethodHostModelExecute, rawReq,
	); errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	if got.SkipInterceptorPluginID != "origin-plugin" {
		t.Fatalf("SkipInterceptorPluginID = %q, want origin-plugin", got.SkipInterceptorPluginID)
	}
}

func TestHostModelExecuteCallbackDoesNotClaimParentDeferredUsage(t *testing.T) {
	type metadataKey struct{}

	deferred := coreusage.NewDeferredRecord()
	parentCtx := coreusage.WithDeferredRecord(
		context.WithValue(context.Background(), metadataKey{}, "preserved"),
		deferred,
	)
	host := New()
	host.SetModelExecutor(
		&fakeHostModelExecutor{
			executeModel: func(
				ctx context.Context,
				req handlers.ModelExecutionRequest,
			) (handlers.ModelExecutionResponse, *handlers.ErrorMessage) {
				if ctx.Value(metadataKey{}) != "preserved" {
					t.Fatalf("callback metadata = %v, want preserved", ctx.Value(metadataKey{}))
				}
				if coreusage.DeferRecord(ctx, coreusage.Record{Model: "nested"}) {
					t.Fatal("nested model execution claimed the parent deferred record")
				}
				return handlers.ModelExecutionResponse{StatusCode: http.StatusOK}, nil
			},
		},
	)
	callbackID, closeCallback := host.openCallbackContext(parentCtx)
	defer closeCallback()

	rawReq, errMarshal := json.Marshal(
		rpcHostModelExecutionRequest{
			EntryProtocol:  "openai",
			ExitProtocol:   "openai",
			Model:          "model-1",
			HostCallbackID: callbackID,
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	if _, errCall := host.callFromPlugin(
		context.Background(), pluginabi.MethodHostModelExecute, rawReq,
	); errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	if !coreusage.DeferRecord(parentCtx, coreusage.Record{Model: "parent"}) {
		t.Fatal("parent context no longer owns its deferred record")
	}
}

func TestHostModelExecuteStreamCallbackDoesNotClaimParentDeferredUsage(t *testing.T) {
	deferred := coreusage.NewDeferredRecord()
	parentCtx := coreusage.WithDeferredRecord(context.Background(), deferred)
	host := New()
	host.SetModelExecutor(
		&fakeHostModelExecutor{
			executeModelStream: func(
				ctx context.Context,
				req handlers.ModelExecutionRequest,
			) (handlers.ModelExecutionStream, *handlers.ErrorMessage) {
				if coreusage.DeferRecord(ctx, coreusage.Record{Model: "nested-stream"}) {
					t.Fatal("nested stream execution claimed the parent deferred record")
				}
				chunks := make(chan handlers.ModelExecutionChunk)
				close(chunks)
				return handlers.ModelExecutionStream{StatusCode: http.StatusOK, Chunks: chunks}, nil
			},
		},
	)
	callbackID, closeCallback := host.openCallbackContext(parentCtx)
	defer closeCallback()

	rawReq, errMarshal := json.Marshal(
		rpcHostModelExecutionRequest{
			EntryProtocol:  "openai",
			ExitProtocol:   "openai",
			Model:          "model-1",
			Stream:         true,
			HostCallbackID: callbackID,
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	if _, errCall := host.callFromPlugin(
		context.Background(), pluginabi.MethodHostModelExecuteStream, rawReq,
	); errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	if !coreusage.DeferRecord(parentCtx, coreusage.Record{Model: "parent"}) {
		t.Fatal("parent context no longer owns its deferred record")
	}
}

func TestHostModelStreamClosesWithCallbackScope(t *testing.T) {
	host := New()
	ctxSeen := make(chan context.Context, 1)
	host.SetModelExecutor(
		&fakeHostModelExecutor{
			executeModelStream: func(
				ctx context.Context,
				req handlers.ModelExecutionRequest,
			) (handlers.ModelExecutionStream, *handlers.ErrorMessage) {
				ctxSeen <- ctx
				return handlers.ModelExecutionStream{
					StatusCode: http.StatusOK,
					Headers:    http.Header{"X-Stream": []string{"ok"}},
					Chunks:     make(chan handlers.ModelExecutionChunk),
				}, nil
			},
		},
	)
	callbackID, closeCallback := host.openCallbackContext(context.Background())
	defer closeCallback()

	rawReq, errMarshal := json.Marshal(
		rpcHostModelExecutionRequest{
			EntryProtocol:  "openai",
			ExitProtocol:   "openai",
			Model:          "model-1",
			Stream:         true,
			Body:           []byte(`{"stream":true}`),
			HostCallbackID: callbackID,
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawReq)
	if errCall != nil {
		t.Fatalf("callFromPlugin() error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostModelStreamResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if resp.StreamID == "" {
		t.Fatalf("stream id is empty: %#v", resp)
	}

	var streamCtx context.Context
	select {
	case streamCtx = <-ctxSeen:
	case <-time.After(time.Second):
		t.Fatal("model executor was not called")
	}
	closeCallback()
	select {
	case <-streamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stream context was not canceled after callback scope closed")
	}
}

func TestHostModelStreamReadAfterCallbackCloseReturnsError(t *testing.T) {
	host := New()
	chunks := make(chan handlers.ModelExecutionChunk)
	host.SetModelExecutor(
		&fakeHostModelExecutor{
			executeModelStream: func(
				ctx context.Context,
				req handlers.ModelExecutionRequest,
			) (handlers.ModelExecutionStream, *handlers.ErrorMessage) {
				return handlers.ModelExecutionStream{
					StatusCode: http.StatusOK,
					Chunks:     chunks,
				}, nil
			},
		},
	)
	callbackID, closeCallback := host.openCallbackContext(context.Background())

	rawReq, errMarshal := json.Marshal(
		rpcHostModelExecutionRequest{
			EntryProtocol:  "openai",
			ExitProtocol:   "openai",
			Model:          "model-1",
			Stream:         true,
			Body:           []byte(`{"stream":true}`),
			HostCallbackID: callbackID,
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawReq)
	if errCall != nil {
		t.Fatalf("execute stream callback error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostModelStreamResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode stream response: %v", errDecode)
	}
	if resp.StreamID == "" {
		t.Fatalf("stream id is empty: %#v", resp)
	}

	closeCallback()
	readReq, errMarshal := json.Marshal(pluginapi.HostModelStreamReadRequest{StreamID: resp.StreamID})
	if errMarshal != nil {
		t.Fatalf("marshal read request: %v", errMarshal)
	}
	_, errRead := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelStreamRead, readReq)
	if errRead == nil || !strings.Contains(errRead.Error(), "is not open") {
		t.Fatalf("read after callback close error = %v, want closed stream error", errRead)
	}
}

func TestHostModelExecuteStreamStartupErrorCleansUp(t *testing.T) {
	host := New()
	ctxSeen := make(chan context.Context, 1)
	host.SetModelExecutor(
		&fakeHostModelExecutor{
			executeModelStream: func(
				ctx context.Context,
				req handlers.ModelExecutionRequest,
			) (handlers.ModelExecutionStream, *handlers.ErrorMessage) {
				ctxSeen <- ctx
				return handlers.ModelExecutionStream{}, &handlers.ErrorMessage{
					StatusCode: http.StatusBadGateway,
				}
			},
		},
	)

	rawReq, errMarshal := json.Marshal(
		pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "openai",
			Model:         "model-1",
			Stream:        true,
			Body:          []byte(`{"stream":true}`),
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawReq)
	if errCall == nil {
		t.Fatalf("execute stream callback error is nil, raw response = %q", rawResp)
	}
	if rawResp != nil {
		t.Fatalf("raw response = %q, want nil on startup error", rawResp)
	}
	if !strings.Contains(errCall.Error(), "status 502") {
		t.Fatalf("execute stream callback error = %v, want status 502", errCall)
	}

	var streamCtx context.Context
	select {
	case streamCtx = <-ctxSeen:
	case <-time.After(time.Second):
		t.Fatal("model executor was not called")
	}
	select {
	case <-streamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stream context was not canceled after startup error")
	}
	gotCount := hostModelStreamCountForTest(t, host)
	if gotCount != 0 {
		t.Fatalf("model stream count = %d, want 0", gotCount)
	}
}

func TestHostModelCallbacksValidateStreamMode(t *testing.T) {
	host := New()

	rawExecuteReq, errMarshal := json.Marshal(
		pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "openai",
			Model:         "model-1",
			Stream:        true,
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal execute request: %v", errMarshal)
	}
	_, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecute, rawExecuteReq)
	if errCall == nil || !strings.Contains(errCall.Error(), "host.model.execute requires stream=false") {
		t.Fatalf("execute callback error = %v, want stream=false validation error", errCall)
	}

	rawStreamReq, errMarshal := json.Marshal(
		pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "openai",
			Model:         "model-1",
			Stream:        false,
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal execute stream request: %v", errMarshal)
	}
	_, errCall = host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawStreamReq)
	if errCall == nil || !strings.Contains(errCall.Error(), "host.model.execute_stream requires stream=true") {
		t.Fatalf("execute stream callback error = %v, want stream=true validation error", errCall)
	}
}

func TestHostModelCallbacksRequireExecutor(t *testing.T) {
	host := New()

	rawExecuteReq, errMarshal := json.Marshal(
		pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "openai",
			Model:         "model-1",
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal execute request: %v", errMarshal)
	}
	_, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecute, rawExecuteReq)
	if errCall == nil || !strings.Contains(errCall.Error(), "host model executor is unavailable") {
		t.Fatalf("execute callback error = %v, want unavailable executor error", errCall)
	}

	rawStreamReq, errMarshal := json.Marshal(
		pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "openai",
			Model:         "model-1",
			Stream:        true,
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal execute stream request: %v", errMarshal)
	}
	_, errCall = host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawStreamReq)
	if errCall == nil || !strings.Contains(errCall.Error(), "host model executor is unavailable") {
		t.Fatalf("execute stream callback error = %v, want unavailable executor error", errCall)
	}
}

func TestHostModelStreamReadAndCloseValidateStreamID(t *testing.T) {
	host := New()

	rawReadReq, errMarshal := json.Marshal(pluginapi.HostModelStreamReadRequest{})
	if errMarshal != nil {
		t.Fatalf("marshal read request: %v", errMarshal)
	}
	_, errRead := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelStreamRead, rawReadReq)
	if errRead == nil || !strings.Contains(errRead.Error(), "model stream id is required") {
		t.Fatalf("read callback error = %v, want required stream id error", errRead)
	}

	rawCloseReq, errMarshal := json.Marshal(pluginapi.HostModelStreamCloseRequest{})
	if errMarshal != nil {
		t.Fatalf("marshal close request: %v", errMarshal)
	}
	rawClose, errClose := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelStreamClose, rawCloseReq)
	if errClose != nil {
		t.Fatalf("close callback error = %v", errClose)
	}
	_, errDecode := decodeRPCEnvelope[rpcEmptyResponse](rawClose)
	if errDecode != nil {
		t.Fatalf("decode close response: %v", errDecode)
	}
}

func TestHostModelStreamReadReturnsPayloadAndTerminalError(t *testing.T) {
	host := New()
	chunks := make(chan handlers.ModelExecutionChunk, 2)
	chunks <- handlers.ModelExecutionChunk{Payload: []byte("first")}
	chunks <- handlers.ModelExecutionChunk{
		Err: &handlers.ModelExecutionStreamError{
			StatusCode: http.StatusBadGateway,
			Message:    "terminal boom",
		},
	}
	host.SetModelExecutor(
		&fakeHostModelExecutor{
			executeModelStream: func(
				ctx context.Context,
				req handlers.ModelExecutionRequest,
			) (handlers.ModelExecutionStream, *handlers.ErrorMessage) {
				return handlers.ModelExecutionStream{
					StatusCode: http.StatusOK,
					Headers:    http.Header{"X-Stream": []string{"ok"}},
					Chunks:     chunks,
				}, nil
			},
		},
	)

	streamID := openHostModelStreamForTest(t, host)
	readReq, errMarshal := json.Marshal(pluginapi.HostModelStreamReadRequest{StreamID: streamID})
	if errMarshal != nil {
		t.Fatalf("marshal read request: %v", errMarshal)
	}
	rawRead, errRead := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelStreamRead, readReq)
	if errRead != nil {
		t.Fatalf("read callback error = %v", errRead)
	}
	first, errDecode := decodeRPCEnvelope[pluginapi.HostModelStreamReadResponse](rawRead)
	if errDecode != nil {
		t.Fatalf("decode read response: %v", errDecode)
	}
	if string(first.Payload) != "first" || first.Done || first.Error != "" {
		t.Fatalf("first read = %#v, want payload without done", first)
	}

	rawRead, errRead = host.callFromPlugin(context.Background(), pluginabi.MethodHostModelStreamRead, readReq)
	if errRead != nil {
		t.Fatalf("terminal read callback error = %v", errRead)
	}
	terminal, errDecode := decodeRPCEnvelope[pluginapi.HostModelStreamReadResponse](rawRead)
	if errDecode != nil {
		t.Fatalf("decode terminal response: %v", errDecode)
	}
	if !terminal.Done || terminal.Error != "terminal boom" || len(terminal.Payload) != 0 {
		t.Fatalf("terminal read = %#v, want done terminal error", terminal)
	}
}

func TestHostModelStreamExplicitCloseCancelsStream(t *testing.T) {
	host := New()
	ctxSeen := make(chan context.Context, 1)
	host.SetModelExecutor(
		&fakeHostModelExecutor{
			executeModelStream: func(
				ctx context.Context,
				req handlers.ModelExecutionRequest,
			) (handlers.ModelExecutionStream, *handlers.ErrorMessage) {
				ctxSeen <- ctx
				return handlers.ModelExecutionStream{
					StatusCode: http.StatusOK,
					Chunks:     make(chan handlers.ModelExecutionChunk),
				}, nil
			},
		},
	)

	streamID := openHostModelStreamForTest(t, host)
	var streamCtx context.Context
	select {
	case streamCtx = <-ctxSeen:
	case <-time.After(time.Second):
		t.Fatal("model executor was not called")
	}
	closeReq, errMarshal := json.Marshal(pluginapi.HostModelStreamCloseRequest{StreamID: streamID})
	if errMarshal != nil {
		t.Fatalf("marshal close request: %v", errMarshal)
	}
	if _, errClose := host.callFromPlugin(
		context.Background(), pluginabi.MethodHostModelStreamClose, closeReq,
	); errClose != nil {
		t.Fatalf("close callback error = %v", errClose)
	}
	select {
	case <-streamCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("stream context was not canceled after explicit close")
	}
	if _, errClose := host.callFromPlugin(
		context.Background(), pluginabi.MethodHostModelStreamClose, closeReq,
	); errClose != nil {
		t.Fatalf("second close callback error = %v", errClose)
	}
}

func openHostModelStreamForTest(t *testing.T, host *Host) string {
	t.Helper()
	rawReq, errMarshal := json.Marshal(
		pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai",
			ExitProtocol:  "openai",
			Model:         "model-1",
			Stream:        true,
			Body:          []byte(`{"stream":true}`),
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal request: %v", errMarshal)
	}
	rawResp, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostModelExecuteStream, rawReq)
	if errCall != nil {
		t.Fatalf("execute stream callback error = %v", errCall)
	}
	resp, errDecode := decodeRPCEnvelope[pluginapi.HostModelStreamResponse](rawResp)
	if errDecode != nil {
		t.Fatalf("decode stream response: %v", errDecode)
	}
	if resp.StreamID == "" {
		t.Fatalf("stream id is empty: %#v", resp)
	}
	return resp.StreamID
}

func hostModelStreamCountForTest(t *testing.T, host *Host) int {
	t.Helper()
	host.modelStreams.mu.Lock()
	defer host.modelStreams.mu.Unlock()
	return len(host.modelStreams.streams)
}

func TestRPCUsageDeliveryErrorsAreLogged(t *testing.T) {
	lookup := newTestSymbolLookup(&testPlugin{})
	lookup.methodHandlers = map[string]func(context.Context, []byte) ([]byte, error){
		pluginabi.MethodUsageHandle: func(context.Context, []byte) ([]byte, error) {
			return marshalRPCError("delivery_failed", "usage receiver unavailable"), nil
		},
	}
	adapter := &rpcPluginAdapter{id: "usage-plugin", client: lookup}

	var output bytes.Buffer
	logger := log.StandardLogger()
	originalOutput := logger.Out
	originalFormatter := logger.Formatter
	originalLevel := logger.Level
	log.SetOutput(&output)
	log.SetFormatter(&log.TextFormatter{DisableColors: true, DisableTimestamp: true})
	log.SetLevel(log.ErrorLevel)
	defer func() {
		log.SetOutput(originalOutput)
		log.SetFormatter(originalFormatter)
		log.SetLevel(originalLevel)
	}()

	adapter.HandleUsage(context.Background(), pluginapi.UsageRecord{Provider: "openai"})
	logged := output.String()
	if !strings.Contains(logged, "usage delivery failed") ||
		!strings.Contains(logged, "usage receiver unavailable") ||
		!strings.Contains(logged, "plugin_id=usage-plugin") {
		t.Fatalf("usage error log = %q", logged)
	}
}

func TestHostLogCallbackRestoresRegisteredRequestContext(t *testing.T) {
	host := New()
	ctx := logging.WithRequestID(context.Background(), "request-123")
	callbackID, closeCallback := host.openCallbackContext(ctx)
	defer closeCallback()

	var out bytes.Buffer
	logger := log.StandardLogger()
	originalOut := logger.Out
	originalFormatter := logger.Formatter
	originalLevel := logger.Level
	log.SetOutput(&out)
	log.SetFormatter(
		&log.TextFormatter{
			DisableColors:    true,
			DisableTimestamp: true,
		},
	)
	log.SetLevel(log.InfoLevel)
	defer func() {
		log.SetOutput(originalOut)
		log.SetFormatter(originalFormatter)
		log.SetLevel(originalLevel)
	}()

	rawReq, errMarshal := json.Marshal(
		rpcHostLogRequest{
			HostCallbackID: callbackID,
			Level:          "info",
			Message:        "plugin callback message",
		},
	)
	if errMarshal != nil {
		t.Fatalf("marshal log request: %v", errMarshal)
	}
	if _, errCall := host.callFromPlugin(context.Background(), pluginabi.MethodHostLog, rawReq); errCall != nil {
		t.Fatalf("log callback error = %v", errCall)
	}

	got := out.String()
	if !strings.Contains(got, "plugin callback message") || !strings.Contains(got, "request_id=request-123") {
		t.Fatalf("log output = %q, want message and request_id field", got)
	}
}
