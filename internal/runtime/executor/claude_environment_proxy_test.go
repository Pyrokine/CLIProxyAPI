package executor

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProtectedUtlsClientHTTPSUsesEnvironmentProxy(t *testing.T) {
	if os.Getenv("CPA_AUDIT_HTTPS_PROXY_CHILD") == "1" {
		var proxyCalls, directCalls atomic.Int32
		proxy := httptest.NewServer(
			http.HandlerFunc(
				func(writer http.ResponseWriter, request *http.Request) {
					proxyCalls.Add(1)
					if request.Method != http.MethodConnect || request.Host != "audit-upstream.invalid:443" {
						t.Errorf(
							"proxy request = %s %s, want CONNECT audit-upstream.invalid:443", request.Method,
							request.Host,
						)
					}
					writer.WriteHeader(http.StatusBadGateway)
				},
			),
		)
		defer proxy.Close()
		t.Setenv("HTTPS_PROXY", proxy.URL)
		t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
		t.Setenv("NO_PROXY", "")
		t.Setenv("no_proxy", "")
		client := newProtectedUtlsHTTPClient(t.Context(), nil, nil, time.Second)
		transport := client.Transport.(*claudeFallbackRoundTripper).fallback.(*http.Transport)
		t.Cleanup(transport.CloseIdleConnections)
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if address != proxy.Listener.Addr().String() {
				directCalls.Add(1)
				return nil, errors.New("unexpected direct or HTTP proxy dial")
			}
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, network, address)
		}
		response, err := client.Get("https://audit-upstream.invalid/path")
		if response != nil {
			_ = response.Body.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "Bad Gateway") ||
			proxyCalls.Load() != 1 || directCalls.Load() != 0 {
			t.Fatalf(
				"HTTPS proxy error=%v requests=%d direct=%d, want proxy CONNECT rejection/1/0",
				err, proxyCalls.Load(), directCalls.Load(),
			)
		}
		return
	}
	command := exec.CommandContext(
		t.Context(), os.Args[0],
		"-test.run=^TestProtectedUtlsClientHTTPSUsesEnvironmentProxy$", "-test.timeout=15s",
	)
	command.Env = append(os.Environ(), "CPA_AUDIT_HTTPS_PROXY_CHILD=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("HTTPS environment proxy: %v\n%s", err, output)
	}
}
