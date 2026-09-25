package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestNewDevinHTTPClientReusesProxyTransport(t *testing.T) {
	auth := &cliproxyauth.Auth{ProxyURL: "http://8.8.8.8:3128"}
	first := newDevinHTTPClient(context.Background(), &config.Config{}, auth, 0)
	second := newDevinHTTPClient(context.Background(), &config.Config{}, auth, 0)
	if first.Transport != second.Transport {
		t.Fatalf("transports differ: %p != %p", first.Transport, second.Transport)
	}
	transport, ok := first.Transport.(*http.Transport)
	if !ok || !transport.DisableCompression {
		t.Fatalf("transport = %#v, want cached *http.Transport with compression disabled", first.Transport)
	}
}

func TestNewDevinHTTPClientReusesClonedContextTransport(t *testing.T) {
	base := &http.Transport{}
	ctx := util.WithRoundTripper(context.Background(), base)
	first := newDevinHTTPClient(ctx, nil, nil, 0)
	second := newDevinHTTPClient(ctx, nil, nil, 0)
	if first.Transport != second.Transport {
		t.Fatalf("context transports differ: %p != %p", first.Transport, second.Transport)
	}
	transport, ok := first.Transport.(*http.Transport)
	if !ok || transport == base || !transport.DisableCompression {
		t.Fatalf("transport = %#v, want reused compression-disabled clone", first.Transport)
	}
}

func TestNewDevinHTTPClientDisablesGzipForCustomRoundTripper(t *testing.T) {
	var encoding string
	roundTripper := proxyTestRoundTripper(func(req *http.Request) (*http.Response, error) {
		encoding = req.Header.Get("Accept-Encoding")
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: req}, nil
	})
	client := newDevinHTTPClient(util.WithRoundTripper(context.Background(), roundTripper), nil, nil, 0)
	req, err := http.NewRequest(http.MethodGet, "https://example.test", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	_ = resp.Body.Close()
	if encoding != "identity" {
		t.Fatalf("Accept-Encoding = %q, want identity", encoding)
	}
}
