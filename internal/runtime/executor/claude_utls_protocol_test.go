package executor

import (
	"context"
	stdtls "crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

func TestClaudeUtlsNegotiatedHTTPProtocolReusesHandshake(t *testing.T) {
	for _, protocol := range []string{"h2", "http/1.1", ""} {
		for _, cancelBody := range []bool{false, true} {
			t.Run(protocol+"/"+map[bool]string{false: "complete", true: "canceled"}[cancelBody], func(t *testing.T) {
				var handshakes, requests atomic.Int32
				server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					w.Header().Set("X-Fixture-Protocol", r.Proto)
					_, _ = io.WriteString(w, "fixture response")
					if cancelBody {
						w.(http.Flusher).Flush()
						<-r.Context().Done()
					}
				}))
				server.EnableHTTP2 = protocol == "h2"
				server.TLS = &stdtls.Config{NextProtos: []string{}, GetConfigForClient: func(*stdtls.ClientHelloInfo) (*stdtls.Config, error) {
					handshakes.Add(1)
					return nil, nil
				}}
				if protocol != "" {
					server.TLS.NextProtos = []string{protocol}
				}
				server.StartTLS()
				defer server.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				addr := server.Listener.Addr().String()
				conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
				if err != nil {
					t.Fatal(err)
				}
				tracked := &trackedClaudeNetConn{Conn: conn}
				defer tracked.Close()
				roots := x509.NewCertPool()
				roots.AddCert(server.Certificate())
				tlsConn := utls.UClient(tracked, &utls.Config{ServerName: "127.0.0.1", RootCAs: roots}, utls.HelloChrome_Auto)
				if err = tlsConn.HandshakeContext(ctx); err != nil {
					t.Fatal(err)
				}
				if got := tlsConn.ConnectionState().NegotiatedProtocol; got != protocol {
					t.Fatalf("negotiated protocol = %q, want %q", got, protocol)
				}
				clientConn, err := initializeClaudeHTTPClientConn(ctx, addr, tlsConn, nil)
				if err != nil {
					t.Fatal(err)
				}
				roundTripper := &claudeUtlsRoundTripper{connectionFactory: func(context.Context, string, string) (claudeHTTPClientConn, error) {
					return clientConn, nil
				}}
				response, err := roundTripper.RoundTrip(newPublicClaudeRequest(t, ctx))
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				if cancelBody {
					cancel()
				}
				body, readErr := io.ReadAll(response.Body)
				if cancelBody {
					if !errors.Is(readErr, context.Canceled) && !errors.Is(readErr, net.ErrClosed) {
						t.Fatalf("canceled body error = %v", readErr)
					}
				} else if readErr != nil || string(body) != "fixture response" {
					t.Fatalf("body = %q, error=%v", body, readErr)
				}
				if err = response.Body.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					t.Fatal(err)
				}
				waitForClaudeClose(t, &tracked.closeCount)
				if handshakes.Load() != 1 || requests.Load() != 1 {
					t.Fatalf("handshakes/requests = %d/%d, want 1/1", handshakes.Load(), requests.Load())
				}
				wantProto := "HTTP/1.1"
				if protocol == "h2" {
					wantProto = "HTTP/2.0"
				}
				if response.Header.Get("X-Fixture-Protocol") != wantProto {
					t.Fatalf("wire protocol = %q, want %q", response.Header.Get("X-Fixture-Protocol"), wantProto)
				}
			})
		}
	}
}

func TestClaudeUtlsUnsupportedALPNClosesConnection(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("HTTP request was sent despite unsupported ALPN")
	}))
	server.TLS = &stdtls.Config{NextProtos: []string{"fixture-unsupported"}}
	server.StartTLS()
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	addr := server.Listener.Addr().String()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	tracked := &trackedClaudeNetConn{Conn: conn}
	defer tracked.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	tlsConn := utls.UClient(tracked, &utls.Config{ServerName: "127.0.0.1", RootCAs: roots, NextProtos: []string{"fixture-unsupported"}}, utls.HelloGolang)
	if err = tlsConn.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	clientConn, err := initializeClaudeHTTPClientConn(ctx, addr, tlsConn, nil)
	if clientConn != nil || err == nil || !strings.Contains(err.Error(), "unsupported negotiated protocol") {
		t.Fatalf("unsupported ALPN: client=%v err=%v", clientConn, err)
	}
	if tracked.closeCount.Load() != 1 {
		t.Fatalf("close count = %d, want 1", tracked.closeCount.Load())
	}
}
