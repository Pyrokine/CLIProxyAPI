package proxyutil_test

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
)

func TestPublicProxyRedactionPreservesPublishedContract(t *testing.T) {
	//goland:noinspection HttpUrlsUsage
	for _, tt := range []struct{ input, want string }{
		{"http://user:pass@proxy.example.com:8080/path?token=secret", "http://redacted@proxy.example.com:8080"},
		{"socks5://proxy.example.com:1080", "socks5://proxy.example.com:1080"},
		{"bad-value", "<invalid proxy URL>"},
		{"", ""},
		{" \t ", ""},
		{"https://user:pass@[::1]:8443/path?token=secret#fragment", "https://redacted@[::1]:8443"},
		{"http://user:pass@proxy.example.com/%zz", "<invalid proxy URL>"},
	} {
		t.Run(
			tt.input, func(t *testing.T) {
				if got := proxyutil.Redact(tt.input); got != tt.want {
					t.Fatalf("redacted=%q want=%q", got, tt.want)
				}
			},
		)
	}
}

func TestPublicHTTPSProxyForwardsOriginalAndClonedTransport(t *testing.T) {
	for _, protocol := range []string{"http", "https"} {
		t.Run(
			protocol, func(t *testing.T) {
				target := httptest.NewUnstartedServer(
					http.HandlerFunc(
						func(writer http.ResponseWriter, request *http.Request) {
							if request.Method != http.MethodGet || request.URL.Path != "/resource" {
								t.Errorf("target received method=%q url=%q", request.Method, request.URL)
							}
							if _, err := io.WriteString(writer, "cloned target ok"); err != nil {
								t.Errorf("write target response: %v", err)
							}
						},
					),
				)
				if protocol == "https" {
					target.StartTLS()
				} else {
					target.Start()
				}
				defer target.Close()
				targetURL, err := url.Parse(target.URL)
				if err != nil {
					t.Fatal(err)
				}
				forwarder := httputil.NewSingleHostReverseProxy(targetURL)
				forwardTransport := &http.Transport{Proxy: nil}
				defer forwardTransport.CloseIdleConnections()
				forwarder.Transport = forwardTransport
				proxy := httptest.NewUnstartedServer(
					http.HandlerFunc(
						func(writer http.ResponseWriter, request *http.Request) {
							if request.TLS == nil || request.TLS.NegotiatedProtocol != "http/1.1" {
								t.Errorf("proxy TLS state=%#v, want HTTP/1.1", request.TLS)
							}
							if protocol == "https" {
								if request.Method != http.MethodConnect || request.Host != targetURL.Host {
									t.Errorf("proxy CONNECT method=%q host=%q", request.Method, request.Host)
									writer.WriteHeader(http.StatusBadRequest)
									return
								}
								forwardPublicProxyTunnel(t, writer, request.Host)
								return
							}
							if request.URL.String() != target.URL+"/resource" {
								t.Errorf("proxy request URL=%q want=%q", request.URL, target.URL+"/resource")
							}
							forwarder.ServeHTTP(writer, request)
						},
					),
				)
				proxy.EnableHTTP2 = true
				proxy.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
				proxy.StartTLS()
				defer proxy.Close()

				for _, clone := range []bool{false, true} {
					name := "original"
					if clone {
						name = "clone"
					}
					t.Run(
						name, func(t *testing.T) {
							transport, mode, err := proxyutil.BuildHTTPTransport(proxy.URL)
							if err != nil || mode != proxyutil.ModeProxy {
								t.Fatalf("proxy transport mode=%d error=%v", mode, err)
							}
							defer transport.CloseIdleConnections()
							transport.TLSClientConfig =
								proxy.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
							transport.TLSClientConfig.RootCAs = x509.NewCertPool()
							transport.TLSClientConfig.RootCAs.AddCert(proxy.Certificate())
							if protocol == "https" {
								transport.TLSClientConfig.RootCAs.AddCert(target.Certificate())
							}
							if clone {
								transport = transport.Clone()
								defer transport.CloseIdleConnections()
							}
							client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
							response, err := client.Get(target.URL + "/resource")
							if err != nil {
								t.Fatalf("request through HTTPS proxy: %v", err)
							}
							body, err := io.ReadAll(response.Body)
							closeErr := response.Body.Close()
							if err != nil || closeErr != nil {
								t.Fatalf("read=%v close=%v", err, closeErr)
							}
							if response.StatusCode != http.StatusOK || string(body) != "cloned target ok" {
								t.Fatalf("status=%d body=%q", response.StatusCode, body)
							}
							if protocol == "https" && response.TLS == nil {
								t.Fatal("HTTPS target response lost its inner TLS connection")
							}
						},
					)
				}
			},
		)
	}
}

func forwardPublicProxyTunnel(t *testing.T, writer http.ResponseWriter, address string) {
	t.Helper()
	upstream, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Error(err)
		writer.WriteHeader(http.StatusBadGateway)
		return
	}
	defer func(closeResource func() error) { _ = closeResource() }(upstream.Close)
	client, buffered, err := writer.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	defer func(closeResource func() error) { _ = closeResource() }(client.Close)
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		t.Error(err)
		return
	}
	if err := buffered.Flush(); err != nil {
		t.Error(err)
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(upstream, buffered)
		_ = upstream.Close()
	}()
	_, _ = io.Copy(client, upstream)
	_ = client.Close()
	<-done
}
