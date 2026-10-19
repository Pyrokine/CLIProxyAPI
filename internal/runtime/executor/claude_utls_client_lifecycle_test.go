package executor

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	utls "github.com/refraction-networking/utls"
)

type claudeContextDialerFunc func(context.Context, string, string) (net.Conn, error)

func (f claudeContextDialerFunc) Dial(network, addr string) (net.Conn, error) {
	return f(context.Background(), network, addr)
}

func (f claudeContextDialerFunc) DialContext(
	ctx context.Context,
	network string,
	addr string,
) (net.Conn, error) {
	return f(ctx, network, addr)
}

type trackedClaudeNetConn struct {
	net.Conn
	closeCount atomic.Int32
}

func (c *trackedClaudeNetConn) Close() error {
	c.closeCount.Add(1)
	return c.Conn.Close()
}

type trackedClaudeBody struct {
	io.Reader
	closeCount atomic.Int32
	closeErr   error
	onClose    func()
}

func (b *trackedClaudeBody) Close() error {
	b.closeCount.Add(1)
	if b.onClose != nil {
		b.onClose()
	}
	return b.closeErr
}

type fakeClaudeHTTP2ClientConn struct {
	roundTrip  func(*http.Request) (*http.Response, error)
	closeCount atomic.Int32
	closeErr   error
}

func (c *fakeClaudeHTTP2ClientConn) RoundTrip(req *http.Request) (*http.Response, error) {
	if c.roundTrip == nil {
		return nil, nil
	}
	return c.roundTrip(req)
}

func (c *fakeClaudeHTTP2ClientConn) Close() error {
	c.closeCount.Add(1)
	return c.closeErr
}

func newPublicClaudeRequest(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://8.8.8.8/responses", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}
	return req
}

func waitForClaudeClose(t *testing.T, count *atomic.Int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for count.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if count.Load() == 0 {
		t.Fatal("connection was not closed")
	}
}

func TestCloseConnectionBodyReadPreservesContextError(t *testing.T) {
	bodyErr := errors.New("http2: response body closed")
	for _, state := range []string{"active", "canceled", "deadline"} {
		t.Run(state, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if state == "deadline" {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Unix(1, 0))
			}
			defer cancel()
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			if err := writer.CloseWithError(bodyErr); err != nil {
				t.Fatal(err)
			}
			body := &trackedClaudeBody{Reader: reader}
			var closed atomic.Int32
			wrapped := newCloseConnectionBody(ctx, body, func() error {
				closed.Add(1)
				return nil
			})
			defer wrapped.Close()
			if state == "canceled" {
				cancel()
			}
			_, errRead := io.ReadAll(wrapped)
			want := bodyErr
			if state == "canceled" {
				want = context.Canceled
			} else if state == "deadline" {
				want = context.DeadlineExceeded
			}
			if !errors.Is(errRead, want) {
				t.Fatalf("read error = %v, want %v", errRead, want)
			}
			if err := wrapped.Close(); err != nil {
				t.Fatal(err)
			}
			if closed.Load() != 1 || body.closeCount.Load() != 1 {
				t.Fatalf("connection/body closes = %d/%d, want 1/1", closed.Load(), body.closeCount.Load())
			}
		})
	}
}

func TestCloseConnectionBodyClosesConnectionAndBodyOnce(t *testing.T) {
	bodyErr := errors.New("body close failed")
	connectionErr := errors.New("connection close failed")
	var closeOrder []string
	body := &trackedClaudeBody{
		Reader:   strings.NewReader("response"),
		closeErr: bodyErr,
		onClose: func() {
			closeOrder = append(closeOrder, "body")
		},
	}
	connectionCloseCount := 0
	wrapped := newCloseConnectionBody(context.Background(), body, func() error {
		connectionCloseCount++
		closeOrder = append(closeOrder, "connection")
		return connectionErr
	})

	payload, errRead := io.ReadAll(wrapped)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if got, want := string(payload), "response"; got != want {
		t.Fatalf("response body = %q, want %q", got, want)
	}

	errClose := wrapped.Close()
	if !errors.Is(errClose, bodyErr) || !errors.Is(errClose, connectionErr) {
		t.Fatalf("close error = %v, want body and connection errors", errClose)
	}
	if errCloseAgain := wrapped.Close(); errCloseAgain != errClose {
		t.Fatalf("second close error = %v, want %v", errCloseAgain, errClose)
	}
	if got := body.closeCount.Load(); got != 1 {
		t.Fatalf("body close count = %d, want 1", got)
	}
	if connectionCloseCount != 1 {
		t.Fatalf("connection close count = %d, want 1", connectionCloseCount)
	}
	if want := []string{"connection", "body"}; !reflect.DeepEqual(closeOrder, want) {
		t.Fatalf("close order = %v, want %v", closeOrder, want)
	}
}

func TestClaudeUtlsRoundTripperDialUsesRequestContext(t *testing.T) {
	dialStarted := make(chan struct{})
	roundTripper := &claudeUtlsRoundTripper{
		dialer: claudeContextDialerFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(dialStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	roundTripDone := make(chan error, 1)
	go func() {
		_, errRoundTrip := roundTripper.RoundTrip(newPublicClaudeRequest(t, ctx))
		roundTripDone <- errRoundTrip
	}()

	select {
	case <-dialStarted:
	case <-time.After(time.Second):
		t.Fatal("dial did not start")
	}
	cancel()
	select {
	case errRoundTrip := <-roundTripDone:
		if !errors.Is(errRoundTrip, context.Canceled) {
			t.Fatalf("RoundTrip error = %v, want context canceled", errRoundTrip)
		}
	case <-time.After(time.Second):
		t.Fatal("RoundTrip did not stop after context cancellation")
	}
}

func TestClaudeUtlsRoundTripperPropagatesCancellationBeforeDial(t *testing.T) {
	var dialCount atomic.Int32
	roundTripper := &claudeUtlsRoundTripper{
		dialer: claudeContextDialerFunc(func(ctx context.Context, _, _ string) (net.Conn, error) {
			dialCount.Add(1)
			return nil, ctx.Err()
		}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	connection, errConnect := roundTripper.createConnection(ctx, "8.8.8.8", "8.8.8.8:443")
	if connection != nil {
		t.Fatalf("connection = %#v, want nil", connection)
	}
	if !errors.Is(errConnect, context.Canceled) {
		t.Fatalf("createConnection error = %v, want context canceled", errConnect)
	}
	if got := dialCount.Load(); got != 1 {
		t.Fatalf("dial count = %d, want 1", got)
	}
}

func TestClaudeUtlsRoundTripperHandshakeUsesRequestContext(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	trackedConn := &trackedClaudeNetConn{Conn: clientConn}
	dialDone := make(chan struct{})
	roundTripper := &claudeUtlsRoundTripper{
		dialer: claudeContextDialerFunc(func(context.Context, string, string) (net.Conn, error) {
			close(dialDone)
			return trackedConn, nil
		}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	connectionDone := make(chan error, 1)
	go func() {
		connection, errConnect := roundTripper.createConnection(ctx, "8.8.8.8", "8.8.8.8:443")
		if connection != nil {
			errConnect = errors.Join(errConnect, connection.Close())
		}
		connectionDone <- errConnect
	}()

	select {
	case <-dialDone:
	case <-time.After(time.Second):
		t.Fatal("dial did not complete")
	}
	cancel()
	select {
	case errConnect := <-connectionDone:
		if !errors.Is(errConnect, context.Canceled) {
			t.Fatalf("createConnection error = %v, want context canceled", errConnect)
		}
	case <-time.After(time.Second):
		t.Fatal("TLS handshake did not stop after context cancellation")
	}
	if got := trackedConn.closeCount.Load(); got != 1 {
		t.Fatalf("connection close count = %d, want 1", got)
	}
}

func TestClaudeUtlsRoundTripperHandshakeHonorsRequestDeadline(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	trackedConn := &trackedClaudeNetConn{Conn: clientConn}
	roundTripper := &claudeUtlsRoundTripper{
		dialer: claudeContextDialerFunc(func(context.Context, string, string) (net.Conn, error) {
			return trackedConn, nil
		}),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	connection, errConnect := roundTripper.createConnection(ctx, "8.8.8.8", "8.8.8.8:443")
	if connection != nil {
		t.Fatalf("connection = %#v, want nil", connection)
	}
	if !errors.Is(errConnect, context.DeadlineExceeded) {
		t.Fatalf("createConnection error = %v, want deadline exceeded", errConnect)
	}
	if got := trackedConn.closeCount.Load(); got != 1 {
		t.Fatalf("connection close count = %d, want 1", got)
	}
}

func TestClaudeUtlsRoundTripperClosesOrdinaryHandshakeFailureOnce(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	trackedConn := &trackedClaudeNetConn{Conn: clientConn}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		defer serverConn.Close()
		buffer := make([]byte, 4096)
		_, _ = serverConn.Read(buffer)
	}()

	roundTripper := &claudeUtlsRoundTripper{
		dialer: claudeContextDialerFunc(func(context.Context, string, string) (net.Conn, error) {
			return trackedConn, nil
		}),
	}
	connection, errConnect := roundTripper.createConnection(t.Context(), "8.8.8.8", "8.8.8.8:443")
	if connection != nil {
		t.Fatalf("connection = %#v, want nil", connection)
	}
	if errConnect == nil || !strings.Contains(errConnect.Error(), "TLS handshake") {
		t.Fatalf("createConnection error = %v, want TLS handshake failure", errConnect)
	}
	<-serverDone
	if got := trackedConn.closeCount.Load(); got != 1 {
		t.Fatalf("connection close count = %d, want 1", got)
	}
}

func TestInitializeClaudeHTTP2ClientConnClosesTLSOnFailure(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
	})
	trackedConn := &trackedClaudeNetConn{Conn: clientConn}
	tlsConn := utls.UClient(trackedConn, &utls.Config{ServerName: "8.8.8.8"}, utls.HelloChrome_Auto)
	initializeErr := errors.New("initialize HTTP/2 failed")

	connection, errConnect := initializeClaudeHTTPClientConn(
		t.Context(),
		"8.8.8.8:443",
		tlsConn,
		func(context.Context, string, string) (claudeHTTPClientConn, error) {
			return nil, initializeErr
		},
	)
	if connection != nil {
		t.Fatalf("connection = %#v, want nil", connection)
	}
	if !errors.Is(errConnect, initializeErr) {
		t.Fatalf("initialize error = %v, want %v", errConnect, initializeErr)
	}
	if got := trackedConn.closeCount.Load(); got != 1 {
		t.Fatalf("connection close count = %d, want 1", got)
	}
}

func TestClaudeUtlsRoundTripperClosesFailedAndEmptyResponses(t *testing.T) {
	roundTripErr := errors.New("round trip failed")
	cases := []struct {
		name      string
		response  *http.Response
		err       error
		wantError error
	}{
		{name: "failure", err: roundTripErr, wantError: roundTripErr},
		{name: "empty response", wantError: errors.New("empty response")},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			connection := &fakeClaudeHTTP2ClientConn{
				roundTrip: func(*http.Request) (*http.Response, error) {
					return testCase.response, testCase.err
				},
			}
			roundTripper := &claudeUtlsRoundTripper{
				connectionFactory: func(context.Context, string, string) (claudeHTTPClientConn, error) {
					return connection, nil
				},
			}

			response, errRoundTrip := roundTripper.RoundTrip(newPublicClaudeRequest(t, t.Context()))
			if response != nil {
				t.Fatalf("response = %#v, want nil", response)
			}
			if testCase.err != nil {
				if !errors.Is(errRoundTrip, testCase.wantError) {
					t.Fatalf("RoundTrip error = %v, want %v", errRoundTrip, testCase.wantError)
				}
			} else if errRoundTrip == nil || !strings.Contains(errRoundTrip.Error(), "empty response") {
				t.Fatalf("RoundTrip error = %v, want empty response error", errRoundTrip)
			}
			if got := connection.closeCount.Load(); got != 1 {
				t.Fatalf("connection close count = %d, want 1", got)
			}
		})
	}
}

func TestClaudeUtlsRoundTripperUsesNoBodyForNilResponseBody(t *testing.T) {
	connection := &fakeClaudeHTTP2ClientConn{
		roundTrip: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusNoContent}, nil
		},
	}
	roundTripper := &claudeUtlsRoundTripper{
		connectionFactory: func(context.Context, string, string) (claudeHTTPClientConn, error) {
			return connection, nil
		},
	}

	response, errRoundTrip := roundTripper.RoundTrip(newPublicClaudeRequest(t, t.Context()))
	if errRoundTrip != nil {
		t.Fatal(errRoundTrip)
	}
	wrapped, ok := response.Body.(*closeConnectionBody)
	if !ok {
		t.Fatalf("response body = %T, want *closeConnectionBody", response.Body)
	}
	if wrapped.ReadCloser != http.NoBody {
		t.Fatalf("wrapped body = %T, want http.NoBody", wrapped.ReadCloser)
	}
	if errClose := response.Body.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if got := connection.closeCount.Load(); got != 1 {
		t.Fatalf("connection close count = %d, want 1", got)
	}
}

func TestClaudeUtlsRoundTripperClosesUnreadBodyWhenContextEnds(t *testing.T) {
	body := &trackedClaudeBody{Reader: strings.NewReader("response")}
	connection := &fakeClaudeHTTP2ClientConn{
		roundTrip: func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
		},
	}
	roundTripper := &claudeUtlsRoundTripper{
		connectionFactory: func(context.Context, string, string) (claudeHTTPClientConn, error) {
			return connection, nil
		},
	}
	ctx, cancel := context.WithCancel(t.Context())
	response, errRoundTrip := roundTripper.RoundTrip(newPublicClaudeRequest(t, ctx))
	if errRoundTrip != nil {
		t.Fatal(errRoundTrip)
	}
	cancel()
	waitForClaudeClose(t, &connection.closeCount)
	if got := body.closeCount.Load(); got != 1 {
		t.Fatalf("body close count = %d, want 1", got)
	}
	if errClose := response.Body.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if got := connection.closeCount.Load(); got != 1 {
		t.Fatalf("connection close count after second close = %d, want 1", got)
	}
}

func TestProtectedUtlsHTTPClientProxyPrecedence(t *testing.T) {
	newProxy := func(name string, calls *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("X-Claude-Test-Transport", name)
			w.WriteHeader(http.StatusOK)
		}))
	}

	for _, test := range []struct {
		name      string
		useAuth   bool
		useConfig bool
		want      string
	}{
		{name: "auth proxy overrides config and context", useAuth: true, useConfig: true, want: "auth"},
		{name: "config proxy overrides context", useConfig: true, want: "config"},
		{name: "context transport used without proxy", want: "context"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var authCalls, configCalls, contextCalls atomic.Int32
			authProxy := newProxy("auth", &authCalls)
			defer authProxy.Close()
			configProxy := newProxy("config", &configCalls)
			defer configProxy.Close()

			ctx := util.WithRoundTripper(t.Context(), protectedUtlsRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				contextCalls.Add(1)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"X-Claude-Test-Transport": {"context"}},
					Body:       http.NoBody,
					Request:    req,
				}, nil
			}))
			cfg := &config.Config{}
			if test.useConfig {
				cfg.ProxyURL = configProxy.URL
			}
			auth := &cliproxyauth.Auth{}
			if test.useAuth {
				auth.ProxyURL = authProxy.URL
			}

			client := newProtectedUtlsHTTPClient(ctx, cfg, auth, time.Second)
			req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "http://public.example.invalid/test", nil)
			if errRequest != nil {
				t.Fatal(errRequest)
			}
			response, errDo := client.Do(req)
			if errDo != nil {
				t.Fatal(errDo)
			}
			defer response.Body.Close()
			if got := response.Header.Get("X-Claude-Test-Transport"); got != test.want {
				t.Fatalf("transport = %q, want %q", got, test.want)
			}

			wantAuthCalls, wantConfigCalls, wantContextCalls := int32(0), int32(0), int32(0)
			switch test.want {
			case "auth":
				wantAuthCalls = 1
			case "config":
				wantConfigCalls = 1
			case "context":
				wantContextCalls = 1
			}
			if authCalls.Load() != wantAuthCalls || configCalls.Load() != wantConfigCalls || contextCalls.Load() != wantContextCalls {
				t.Fatalf(
					"transport calls auth/config/context = %d/%d/%d, want %d/%d/%d",
					authCalls.Load(), configCalls.Load(), contextCalls.Load(),
					wantAuthCalls, wantConfigCalls, wantContextCalls,
				)
			}
		})
	}
}

func TestClaudeUtlsRoundTripperKeepsConcurrentConnectionsIndependent(t *testing.T) {
	var connectionsMu sync.Mutex
	var connections []*fakeClaudeHTTP2ClientConn
	roundTripper := &claudeUtlsRoundTripper{
		connectionFactory: func(context.Context, string, string) (claudeHTTPClientConn, error) {
			connection := &fakeClaudeHTTP2ClientConn{
				roundTrip: func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
				},
			}
			connectionsMu.Lock()
			connections = append(connections, connection)
			connectionsMu.Unlock()
			return connection, nil
		},
	}

	first, errFirst := roundTripper.RoundTrip(newPublicClaudeRequest(t, t.Context()))
	if errFirst != nil {
		t.Fatal(errFirst)
	}
	second, errSecond := roundTripper.RoundTrip(newPublicClaudeRequest(t, t.Context()))
	if errSecond != nil {
		t.Fatal(errSecond)
	}
	if len(connections) != 2 {
		t.Fatalf("connections = %d, want 2", len(connections))
	}
	if errClose := first.Body.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if got := connections[0].closeCount.Load(); got != 1 {
		t.Fatalf("first close count = %d, want 1", got)
	}
	if got := connections[1].closeCount.Load(); got != 0 {
		t.Fatalf("second close count = %d, want 0", got)
	}
	if errClose := second.Body.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if got := connections[1].closeCount.Load(); got != 1 {
		t.Fatalf("second close count = %d, want 1", got)
	}
}

func TestProtectedUtlsHTTPClientRequestProxyOverride(t *testing.T) {
	for _, factory := range []struct {
		name string
		make func(context.Context, *config.Config, *cliproxyauth.Auth, time.Duration) *http.Client
	}{
		{name: "protected", make: newProtectedUtlsHTTPClient},
		{name: "claude", make: newClaudeUtlsHTTPClient},
	} {
		for _, sample := range []string{"proxy", "direct", "invalid"} {
			t.Run(factory.name+"/"+sample, func(t *testing.T) {
				var requestCalls, authCalls, configCalls, contextCalls, directCalls atomic.Int32
				server := func(name string, calls *atomic.Int32) *httptest.Server {
					result := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
						calls.Add(1)
						writer.Header().Set("X-Claude-Test-Transport", name)
						writer.WriteHeader(http.StatusOK)
					}))
					t.Cleanup(result.Close)
					return result
				}
				requestProxy := server("request", &requestCalls)
				authProxy := server("auth", &authCalls)
				configProxy := server("config", &configCalls)
				destination := server("direct", &directCalls)
				ctx := util.WithRoundTripper(t.Context(), protectedUtlsRoundTripFunc(func(request *http.Request) (*http.Response, error) {
					contextCalls.Add(1)
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Claude-Test-Transport": {"context"}}, Body: http.NoBody, Request: request}, nil
				}))
				override := requestProxy.URL
				if sample == "direct" {
					override = "direct"
				} else if sample == "invalid" {
					override = "file:///invalid-proxy"
				}
				ctx = cliproxyexecutor.WithRequestProxyURL(ctx, override)
				client := factory.make(ctx, &config.Config{ProxyURL: configProxy.URL}, &cliproxyauth.Auth{ProxyURL: authProxy.URL}, time.Second)
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, destination.URL+"/request-proxy", nil)
				if err != nil {
					t.Fatal(err)
				}
				response, err := client.Do(request)
				if response != nil {
					defer response.Body.Close()
				}
				if sample == "invalid" {
					if err == nil || requestCalls.Load()+authCalls.Load()+configCalls.Load()+contextCalls.Load()+directCalls.Load() != 0 {
						t.Fatalf("invalid override: error=%v, calls=%d/%d/%d/%d/%d", err, requestCalls.Load(), authCalls.Load(), configCalls.Load(), contextCalls.Load(), directCalls.Load())
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				want, wantRequest, wantDirect := "request", int32(1), int32(0)
				if sample == "direct" {
					want, wantRequest, wantDirect = "direct", 0, 1
				}
				if response.Header.Get("X-Claude-Test-Transport") != want || requestCalls.Load() != wantRequest || directCalls.Load() != wantDirect || authCalls.Load()+configCalls.Load()+contextCalls.Load() != 0 {
					t.Fatalf("transport=%q, want %q, calls=%d/%d/%d/%d/%d", response.Header.Get("X-Claude-Test-Transport"), want, requestCalls.Load(), authCalls.Load(), configCalls.Load(), contextCalls.Load(), directCalls.Load())
				}
			})
		}
	}
}
