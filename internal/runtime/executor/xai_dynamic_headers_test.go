package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestXAICompactUsesExplicitDownstreamHeaderBoundary(t *testing.T) {
	var received string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Get("X-Tenant")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque"}]}`)
	}))
	defer server.Close()

	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":        server.URL,
		"api_key":         "fixture",
		"header:X-Tenant": "$X-Downstream-Tenant",
	}}
	_, err := newXAIExecutor(&config.Config{}).Execute(
		context.Background(),
		auth,
		cliproxyexecutor.Request{Model: "grok-4.3", Payload: []byte(`{"model":"grok-4.3","input":"hello"}`)},
		cliproxyexecutor.Options{
			Alt:          "responses/compact",
			SourceFormat: sdktranslator.FormatOpenAIResponse,
			Headers:      http.Header{"X-Downstream-Tenant": []string{"tenant-a"}},
		},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if received != "tenant-a" {
		t.Fatalf("X-Tenant = %q, want tenant-a", received)
	}
}

func TestXAIImageAndWebsocketHeadersUseExplicitDownstreamBoundary(t *testing.T) {
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"header:X-Tenant": "$X-Downstream-Tenant",
	}}
	incoming := http.Header{"X-Downstream-Tenant": []string{"tenant-b"}}

	imageRequest := httptest.NewRequest(http.MethodPost, "https://api.x.ai/v1/images/generations", bytes.NewReader(nil))
	applyXAIHeaders(imageRequest, auth, "fixture", false, "", incoming)
	if got := imageRequest.Header.Get("X-Tenant"); got != "tenant-b" {
		t.Fatalf("image X-Tenant = %q, want tenant-b", got)
	}

	websocketHeaders := applyXAIWebsocketHeaders(auth, "fixture", "", incoming)
	if got := websocketHeaders.Get("X-Tenant"); got != "tenant-b" {
		t.Fatalf("websocket X-Tenant = %q, want tenant-b", got)
	}
}
