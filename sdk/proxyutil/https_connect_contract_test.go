package proxyutil_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/proxyutil"
	"golang.org/x/net/proxy"
)

func TestPublicHTTPSDialerCONNECTKeepsTunnelBytes(t *testing.T) {
	if args := flag.Args(); len(args) == 4 && args[0] == "cpa-https-dialer-fixture" {
		certificate, err := os.ReadFile(args[3])
		if err != nil {
			t.Fatal(err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(certificate) {
			t.Fatal("fixture certificate was not loaded")
		}
		x509.SetFallbackRoots(roots)
		dialer, mode, err := proxyutil.BuildDialer(args[1])
		if err != nil || mode != proxyutil.ModeProxy {
			t.Fatalf("dialer mode=%d error=%v", mode, err)
		}
		contextDialer, valid := dialer.(proxy.ContextDialer)
		if !valid {
			t.Fatal("public HTTPS dialer does not support context cancellation")
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		connection, err := contextDialer.DialContext(ctx, "tcp", args[2])
		if err != nil {
			t.Fatal(err)
		}
		defer func(closeResource func() error) { _ = closeResource() }(connection.Close)
		if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, 2)
		if _, err := io.ReadFull(connection, buffer); err != nil || string(buffer) != "ok" {
			t.Fatalf("buffered tunnel data=%q error=%v", buffer, err)
		}
		if _, err := io.WriteString(connection, "ping"); err != nil {
			t.Fatal(err)
		}
		return
	}

	serverDone := make(chan error, 1)
	server := httptest.NewUnstartedServer(
		http.HandlerFunc(
			func(writer http.ResponseWriter, request *http.Request) {
				//goland:noinspection SpellCheckingInspection
				if request.Method != http.MethodConnect || request.Host != "synthetic.example:443" ||
					request.Header.Get("Proxy-Authorization") != "Basic dXNlcjpwYXNz" ||
					request.TLS == nil || request.TLS.NegotiatedProtocol != "http/1.1" {
					serverDone <- fmt.Errorf(
						"CONNECT method=%s host=%s TLS=%+v", request.Method, request.Host, request.TLS,
					)
					writer.WriteHeader(http.StatusBadRequest)
					return
				}
				connection, buffered, err := writer.(http.Hijacker).Hijack()
				if err != nil {
					serverDone <- err
					return
				}
				defer func(closeResource func() error) { _ = closeResource() }(connection.Close)
				if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
					serverDone <- err
					return
				}
				if _, err := io.WriteString(connection, "HTTP/1.1 200 Connection Established\r\n\r\nok"); err != nil {
					serverDone <- err
					return
				}
				payload := make([]byte, 4)
				_, err = io.ReadFull(buffered, payload)
				if err == nil && string(payload) != "ping" {
					err = fmt.Errorf("tunneled payload=%q, want ping", payload)
				}
				serverDone <- err
			},
		),
	)
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	server.StartTLS()
	defer server.Close()
	certificatePath := filepath.Join(t.TempDir(), "proxy-ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(certificatePath, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	proxyURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL.User = url.UserPassword("user", "pass")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx, binary, "-test.run=^TestPublicHTTPSDialerCONNECTKeepsTunnelBytes$", "-test.timeout=6s", "--",
		"cpa-https-dialer-fixture", proxyURL.String(), "synthetic.example:443", certificatePath,
	)
	command.Env = append(os.Environ(), "GODEBUG="+os.Getenv("GODEBUG")+",x509usefallbackroots=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("public HTTPS dialer child: %v\n%s", err, output)
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("HTTPS CONNECT proxy did not finish its tunnel exchange")
	}
}
