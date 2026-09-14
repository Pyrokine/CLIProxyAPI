package proxyutil

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    Mode
		wantErr bool
	}{
		{name: "inherit", input: "", want: ModeInherit},
		{name: "direct", input: "direct", want: ModeDirect},
		{name: "none", input: "none", want: ModeDirect},
		{name: "http", input: "http://proxy.example.com:8080", want: ModeProxy},
		{name: "https", input: "https://proxy.example.com:8443", want: ModeProxy},
		{name: "socks5", input: "socks5://proxy.example.com:1080", want: ModeProxy},
		{name: "socks5h", input: "socks5h://proxy.example.com:1080", want: ModeProxy},
		{name: "invalid", input: "bad-value", want: ModeInvalid, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()

				setting, errParse := Parse(tt.input)
				if tt.wantErr && errParse == nil {
					t.Fatal("expected error, got nil")
				}
				if !tt.wantErr && errParse != nil {
					t.Fatalf("unexpected error: %v", errParse)
				}
				if setting.Mode != tt.want {
					t.Fatalf("mode = %d, want %d", setting.Mode, tt.want)
				}
			},
		)
	}
}

func TestValidateProxyURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "direct", input: "direct"},
		{name: "public", input: "http://8.8.8.8:8080"},
		{name: "loopback", input: "http://127.0.0.1:8080"},
		{name: "private", input: "socks5://10.0.0.1:1080"},
		{name: "documentation", input: "http://203.0.113.1:8080"},
		{name: "unsupported", input: "file:///tmp/socket", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				errValidate := ValidateProxyURL(tt.input)
				if tt.wantErr && errValidate == nil {
					t.Fatal("expected validation error, got nil")
				}
				if !tt.wantErr && errValidate != nil {
					t.Fatalf("unexpected validation error: %v", errValidate)
				}
			},
		)
	}
}

func TestValidateHTTPURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "public http", input: "http://8.8.8.8/v1"},
		{name: "public https", input: "https://example.com/v1"},
		{name: "loopback", input: "http://127.0.0.1:8080"},
		{name: "link local", input: "http://169.254.169.254/latest/meta-data/"},
		{name: "missing host", input: "https://", wantErr: true},
		{name: "unsupported scheme", input: "file:///tmp/socket", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				errValidate := ValidateHTTPURL(tt.input)
				if tt.wantErr && errValidate == nil {
					t.Fatal("expected validation error, got nil")
				}
				if !tt.wantErr && errValidate != nil {
					t.Fatalf("unexpected validation error: %v", errValidate)
				}
			},
		)
	}
}

func TestValidatePublicHTTPURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "public http", input: "http://8.8.8.8/v1"},
		{name: "public https", input: "https://example.com/v1"},
		{name: "missing host", input: "https://", wantErr: true},
		{name: "unsupported scheme", input: "file:///tmp/socket", wantErr: true},
		{name: "loopback", input: "http://127.0.0.1:8080", wantErr: true},
		{name: "link local", input: "http://169.254.169.254/latest/meta-data/", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				t.Parallel()
				errValidate := ValidatePublicHTTPURL(tt.input)
				if tt.wantErr && errValidate == nil {
					t.Fatal("expected validation error, got nil")
				}
				if !tt.wantErr && errValidate != nil {
					t.Fatalf("unexpected validation error: %v", errValidate)
				}
			},
		)
	}
}

func TestBuildHTTPTransportDirectBypassesProxy(t *testing.T) {
	t.Parallel()

	transport, mode, errBuild := BuildHTTPTransport("direct")
	if errBuild != nil {
		t.Fatalf("BuildHTTPTransport returned error: %v", errBuild)
	}
	if mode != ModeDirect {
		t.Fatalf("mode = %d, want %d", mode, ModeDirect)
	}
	if transport == nil {
		t.Fatal("expected transport, got nil")
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestBuildHTTPTransportHTTPProxy(t *testing.T) {
	t.Parallel()

	transport, mode, errBuild := BuildHTTPTransport("http://proxy.example.com:8080")
	if errBuild != nil {
		t.Fatalf("BuildHTTPTransport returned error: %v", errBuild)
	}
	if mode != ModeProxy {
		t.Fatalf("mode = %d, want %d", mode, ModeProxy)
	}
	if transport == nil {
		t.Fatal("expected transport, got nil")
	}

	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}

	proxyURL, errProxy := transport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("transport.Proxy returned error: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://proxy.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://proxy.example.com:8080", proxyURL)
	}
}

func TestBuildHTTPTransportSOCKS5hProxy(t *testing.T) {
	t.Parallel()

	transport, mode, errBuild := BuildHTTPTransport("socks5h://proxy.example.com:1080")
	if errBuild != nil {
		t.Fatalf("BuildHTTPTransport returned error: %v", errBuild)
	}
	if mode != ModeProxy {
		t.Fatalf("mode = %d, want %d", mode, ModeProxy)
	}
	if transport == nil || transport.DialContext == nil {
		t.Fatalf("expected transport with SOCKS DialContext, got %#v", transport)
	}
	if transport.Proxy != nil {
		t.Fatal("SOCKS transport must not use http.ProxyURL")
	}
}

func TestBuildDialerHTTPConnectProxy(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen failed: %v", errListen)
	}
	defer func() { _ = listener.Close() }()

	requests := make(chan string, 1)
	serverDone := make(chan error, 1)
	go func() {
		connection, errAccept := listener.Accept()
		if errAccept != nil {
			serverDone <- errAccept
			return
		}
		defer func() { _ = connection.Close() }()
		request, errRequest := http.ReadRequest(bufio.NewReader(connection))
		if errRequest != nil {
			serverDone <- errRequest
			return
		}
		requests <- fmt.Sprintf("%s %s %s", request.Method, request.Host, request.Header.Get("Proxy-Authorization"))
		if _, errWrite := connection.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\nok")); errWrite != nil {
			serverDone <- errWrite
			return
		}
		payload := make([]byte, len("ping"))
		if _, errRead := io.ReadFull(connection, payload); errRead != nil {
			serverDone <- errRead
			return
		}
		if string(payload) != "ping" {
			serverDone <- fmt.Errorf("tunneled payload = %q, want ping", payload)
			return
		}
		serverDone <- nil
	}()

	dialer, mode, errBuild := BuildDialer("http://user:pass@" + listener.Addr().String())
	if errBuild != nil {
		t.Fatalf("BuildDialer returned error: %v", errBuild)
	}
	if mode != ModeProxy {
		t.Fatalf("mode = %d, want %d", mode, ModeProxy)
	}
	connection, errDial := dialer.Dial("tcp", "example.com:443")
	if errDial != nil {
		t.Fatalf("Dial returned error: %v", errDial)
	}
	defer func() { _ = connection.Close() }()

	got := <-requests
	if !strings.HasPrefix(got, "CONNECT example.com:443 Basic ") {
		t.Fatalf("proxy request = %q", got)
	}
	buffered := make([]byte, len("ok"))
	if _, errRead := io.ReadFull(connection, buffered); errRead != nil {
		t.Fatalf("read buffered tunnel payload: %v", errRead)
	}
	if string(buffered) != "ok" {
		t.Fatalf("buffered tunnel payload = %q, want ok", buffered)
	}
	if _, errWrite := connection.Write([]byte("ping")); errWrite != nil {
		t.Fatalf("write tunneled payload: %v", errWrite)
	}
	if errServer := <-serverDone; errServer != nil {
		t.Fatalf("proxy server returned error: %v", errServer)
	}
}

func TestBuildDialerHTTPProxyCONNECTCancellation(t *testing.T) {
	listener, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("listen failed: %v", errListen)
	}
	defer func() { _ = listener.Close() }()

	requestRead := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		connection, errAccept := listener.Accept()
		if errAccept != nil {
			serverDone <- errAccept
			return
		}
		defer func() { _ = connection.Close() }()
		if _, errRead := http.ReadRequest(bufio.NewReader(connection)); errRead != nil {
			serverDone <- errRead
			return
		}
		close(requestRead)
		if errDeadline := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); errDeadline != nil {
			serverDone <- errDeadline
			return
		}
		var buffer [1]byte
		_, errRead := connection.Read(buffer[:])
		serverDone <- errRead
	}()

	dialer, mode, errBuild := BuildDialer("http://" + listener.Addr().String())
	if errBuild != nil || mode != ModeProxy {
		t.Fatalf("BuildDialer mode=%d error=%v", mode, errBuild)
	}
	contextDialer, ok := dialer.(interface {
		DialContext(context.Context, string, string) (net.Conn, error)
	})
	if !ok {
		t.Fatal("HTTP CONNECT dialer does not support context cancellation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	dialDone := make(chan error, 1)
	go func() {
		connection, errDial := contextDialer.DialContext(ctx, "tcp", "20.42.0.20:443")
		if connection != nil {
			_ = connection.Close()
		}
		dialDone <- errDial
	}()
	select {
	case <-requestRead:
	case <-time.After(time.Second):
		t.Fatal("proxy did not receive CONNECT request")
	}
	cancel()
	select {
	case errDial := <-dialDone:
		if errDial == nil {
			t.Fatal("canceled CONNECT dial returned nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled CONNECT dial did not return")
	}
	select {
	case errServer := <-serverDone:
		if errServer == nil {
			t.Fatal("proxy connection stayed open after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("proxy connection was not closed after cancellation")
	}
}
