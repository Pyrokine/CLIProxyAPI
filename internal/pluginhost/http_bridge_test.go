package pluginhost

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestHostHTTPClientClearsAmbientRequestProxy(t *testing.T) {
	ambient := cliproxyexecutor.WithRequestProxyURL(context.Background(), "http://ambient-proxy.example:8081")
	client := &hostHTTPClient{}
	if got := cliproxyexecutor.RequestProxyURL(client.proxyContext(ambient)); got != "" {
		t.Fatalf("ambient proxy = %q, want cleared", got)
	}
	client.requestProxyURL = "http://request-proxy.example:8082"
	if got := cliproxyexecutor.RequestProxyURL(client.proxyContext(ambient)); got != client.requestProxyURL {
		t.Fatalf("request proxy = %q, want %q", got, client.requestProxyURL)
	}
	if got := cliproxyexecutor.RequestProxyURL(ambient); got != "http://ambient-proxy.example:8081" {
		t.Fatalf("parent context proxy changed to %q", got)
	}
}
