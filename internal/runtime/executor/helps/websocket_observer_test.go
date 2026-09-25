package helps

import (
	"bytes"
	"context"
	"testing"

	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestEmitWebSocketResponseEventCopiesRawFrameAndIdentity(t *testing.T) {
	payload := []byte(`{"type":"response.created","response":{"id":"resp-1"}}`)
	var received cliproxyexecutor.WebSocketResponseEvent
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAIResponse,
		Metadata: map[string]any{
			"request_id": "request-1",
			"trace_id":   "trace-1",
			cliproxyexecutor.RequestedModelMetadataKey: "requested-alias",
		},
		WebSocketResponseObserver: func(_ context.Context, event cliproxyexecutor.WebSocketResponseEvent) {
			received = event
			event.Payload[0] = 'x'
		},
	}
	auth := &cliproxyauth.Auth{
		ID:       "auth-1",
		Label:    "primary",
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}

	EmitWebSocketResponseEvent(context.Background(), opts, auth, "codex", "upstream-model", payload)

	if received.RequestID != "request-1" || received.TraceID != "trace-1" {
		t.Fatalf("request identity = (%q, %q)", received.RequestID, received.TraceID)
	}
	if received.SourceFormat != sdktranslator.FormatOpenAIResponse.String() ||
		received.Model != "upstream-model" || received.RequestedModel != "requested-alias" {
		t.Fatalf("model identity = %+v", received)
	}
	if received.Provider != "codex" || received.AuthID != "auth-1" ||
		received.AuthLabel != "primary" || received.AuthType != "oauth" {
		t.Fatalf("auth identity = %+v", received)
	}
	if received.EventType != "response.created" {
		t.Fatalf("event type = %q", received.EventType)
	}
	if !bytes.Equal(payload, []byte(`{"type":"response.created","response":{"id":"resp-1"}}`)) {
		t.Fatalf("observer mutated source payload: %s", payload)
	}
}

func TestEmitWebSocketResponseEventWithoutObserverIsNoOp(t *testing.T) {
	EmitWebSocketResponseEvent(
		context.Background(),
		cliproxyexecutor.Options{},
		nil,
		"codex",
		"model",
		[]byte(`{"type":"response.created"}`),
	)
}
