package proxyutil

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
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
		tt := tt
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
		tt := tt
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
		tt := tt
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
		tt := tt
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
	go func() {
		conn, errAccept := listener.Accept()
		if errAccept != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		request, errRequest := http.ReadRequest(bufio.NewReader(conn))
		if errRequest != nil {
			requests <- "ERR " + errRequest.Error()
			return
		}
		requests <- fmt.Sprintf("%s %s %s", request.Method, request.Host, request.Header.Get("Proxy-Authorization"))
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	}()

	dialer, mode, errBuild := BuildDialer("http://user:pass@" + listener.Addr().String())
	if errBuild != nil {
		t.Fatalf("BuildDialer returned error: %v", errBuild)
	}
	if mode != ModeProxy {
		t.Fatalf("mode = %d, want %d", mode, ModeProxy)
	}
	conn, errDial := dialer.Dial("tcp", "example.com:443")
	if errDial != nil {
		t.Fatalf("Dial returned error: %v", errDial)
	}
	_ = conn.Close()

	got := <-requests
	if !strings.HasPrefix(got, "CONNECT example.com:443 Basic ") {
		t.Fatalf("proxy request = %q", got)
	}
}
