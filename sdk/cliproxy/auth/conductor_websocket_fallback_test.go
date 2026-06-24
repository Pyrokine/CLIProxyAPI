package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestSanitizeDownstreamWebsocketFallbackRequestRemovesGenerateForHTTPFallback(t *testing.T) {
	req := cliproxyexecutor.Request{Payload: []byte(`{"generate":true,"prompt":"hello"}`)}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	out := sanitizeDownstreamWebsocketFallbackRequest(ctx, &Auth{ID: "http-auth"}, req)

	if gjson.GetBytes(out.Payload, "generate").Exists() {
		t.Fatalf("generate still exists: %s", out.Payload)
	}
	if got := gjson.GetBytes(out.Payload, "prompt").String(); got != "hello" {
		t.Fatalf("prompt = %q, want hello; payload=%s", got, out.Payload)
	}
}

func TestSanitizeDownstreamWebsocketFallbackRequestPreservesGenerateForWebsocketAuth(t *testing.T) {
	req := cliproxyexecutor.Request{Payload: []byte(`{"generate":true,"prompt":"hello"}`)}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	auth := &Auth{ID: "ws-auth", Attributes: map[string]string{"websockets": "true"}}

	out := sanitizeDownstreamWebsocketFallbackRequest(ctx, auth, req)

	if !gjson.GetBytes(out.Payload, "generate").Exists() {
		t.Fatalf("generate was removed for websocket auth: %s", out.Payload)
	}
}

func TestSanitizeDownstreamWebsocketFallbackRequestPreservesGenerateForHTTPRequests(t *testing.T) {
	req := cliproxyexecutor.Request{Payload: []byte(`{"generate":true,"prompt":"hello"}`)}

	out := sanitizeDownstreamWebsocketFallbackRequest(context.Background(), &Auth{ID: "http-auth"}, req)

	if !gjson.GetBytes(out.Payload, "generate").Exists() {
		t.Fatalf("generate was removed for ordinary HTTP request: %s", out.Payload)
	}
}
