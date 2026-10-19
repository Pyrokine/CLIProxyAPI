package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
)

func TestXAIWebsocketPatchReplayRequiresDeliveryAcknowledgement(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		name := "delivery failure"
		if delivered {
			name = "delivery success"
		}
		t.Run(name, func(t *testing.T) {
			internalcache.ClearXAIReasoningReplayCache()
			t.Cleanup(internalcache.ClearXAIReasoningReplayCache)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, err = conn.ReadMessage(); err != nil {
					t.Errorf("read: %v", err)
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[{"id":"item-1","call_id":"call-1","type":"function_call","name":"apply_patch","arguments":"{\"input\":\"patch\"}"}]}}`))
			}))
			t.Cleanup(server.Close)
			exec := newXAIWebsocketsExecutor(&config.Config{})
			sessionID := t.Name()
			t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
			base := cliproxyexecutor.WithStreamDeliveryAcknowledgement(cliproxyexecutor.WithDownstreamWebsocket(xaiC03ContextWithAPIKey("patch-delivery-caller")))
			ctx, cancel := context.WithTimeout(base, 5*time.Second)
			defer cancel()
			auth := &cliproxyauth.Auth{ID: sessionID, Provider: "xai", Attributes: map[string]string{"base_url": server.URL, "websockets": "true"}, Metadata: map[string]any{"access_token": "test"}}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID}}
			req := cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(`{"prompt_cache_key":"patch-delivery","input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`)}
			scope := xaiReasoningReplayScopeFromRequest(ctx, auth, opts.SourceFormat, req, opts, req.Payload, server.URL)
			if !scope.valid() {
				t.Fatal("invalid replay scope")
			}
			stream, err := exec.ExecuteStream(ctx, auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			sawAcknowledgement := false
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				if chunk.Terminal && chunk.Acknowledge != nil {
					sawAcknowledgement = true
					if _, stored := internalcache.GetXAIReasoningReplayItems(scope.modelName, scope.sessionKey); stored {
						t.Error("replay cached before delivery acknowledgement")
					}
					var deliveryErr error
					if !delivered {
						deliveryErr = errors.New("test delivery failure")
					}
					chunk.Acknowledge(deliveryErr)
				}
			}
			if !sawAcknowledgement {
				t.Error("missing terminal delivery acknowledgement")
			}
			if _, stored := internalcache.GetXAIReasoningReplayItems(scope.modelName, scope.sessionKey); stored != delivered {
				t.Errorf("replay stored=%t, want %t", stored, delivered)
			}
		})
	}
}
