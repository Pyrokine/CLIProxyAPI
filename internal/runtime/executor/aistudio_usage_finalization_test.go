package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/wsrelay"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
)

func TestAIStudioPrepareRequestUsesExplicitDynamicHeaderSource(t *testing.T) {
	ctx := util.WithSessionID(context.Background(), "aistudio-session")
	exec := NewAIStudioExecutor(nil, "aistudio", nil)
	prepared, err := exec.prepareRequest(
		ctx,
		&cliproxyauth.Auth{
			ID: "aistudio-auth",
			Attributes: map[string]string{
				"header:X-Copied":       "$X-CLIENT-VALUE",
				"header:X-CPA-Session":  "$CPA-SESSION-ID",
				"header:X-Missing":      "$Missing-Header",
				"header:X-Auth-Copy":    "$Authorization",
				"header:X-Cookie-Copy":  "$Cookie",
				"header:Authorization":  "Bearer custom",
				"header:Content-Length": "999",
			},
		},
		cliproxyexecutor.Request{
			Model: "gemini-test",
			Payload: []byte(
				`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			),
		},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatGemini,
			Headers: http.Header{
				"x-cLiEnT-vAlUe": []string{"from-options"},
				"Authorization":  []string{"Bearer caller"},
				"Cookie":         []string{"caller=cookie"},
			},
		},
		false,
	)
	if err != nil {
		t.Fatalf("prepareRequest error = %v", err)
	}
	if got := prepared.wsReq.Headers.Get("X-Copied"); got != "from-options" {
		t.Fatalf("X-Copied = %q, want from-options", got)
	}
	if got := prepared.wsReq.Headers.Get("X-CPA-Session"); got != "aistudio-session" {
		t.Fatalf("X-CPA-Session = %q, want aistudio-session", got)
	}
	for _, name := range []string{
		"X-Missing", "X-Auth-Copy", "X-Cookie-Copy", "Authorization", "Content-Length",
	} {
		if got := prepared.wsReq.Headers.Get(name); got != "" {
			t.Fatalf("%s = %q, want empty", name, got)
		}
	}
	if got := prepared.wsReq.Headers.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestAIStudioStreamCancellationWinsOverBufferedUsage(t *testing.T) {
	const authID = "aistudio-cancel-auth"
	manager := wsrelay.NewManager(wsrelay.Options{
		ProviderFactory: func(*http.Request) (string, error) {
			return authID, nil
		},
	})
	server := httptest.NewServer(manager.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })

	websocketURL := "ws" + strings.TrimPrefix(server.URL, "http") + manager.Path()
	client, response, errDial := websocket.DefaultDialer.Dial(websocketURL, nil)
	if response != nil && response.Body != nil {
		t.Cleanup(func() { _ = response.Body.Close() })
	}
	if errDial != nil {
		t.Fatal(errDial)
	}
	t.Cleanup(func() { _ = client.Close() })

	clientDone := make(chan error, 1)
	go func() {
		var request struct {
			ID string `json:"id"`
		}
		if errRead := client.ReadJSON(&request); errRead != nil {
			clientDone <- errRead
			return
		}
		if errWrite := client.WriteJSON(map[string]any{
			"id":   request.ID,
			"type": wsrelay.MessageTypeStreamStart,
			"payload": map[string]any{
				"status": http.StatusOK,
			},
		}); errWrite != nil {
			clientDone <- errWrite
			return
		}
		clientDone <- client.WriteJSON(map[string]any{
			"id":   request.ID,
			"type": wsrelay.MessageTypeStreamChunk,
			"payload": map[string]any{
				"data": `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7}}` + "\n\n",
			},
		})
	}()

	plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
	registerTestUsagePlugin(t, plugin)
	baseCtx := usage.WithRequestedModelAlias(context.Background(), "aistudio-cancel-finalizer")
	ctx, cancel := context.WithCancel(baseCtx)
	exec := NewAIStudioExecutor(nil, "aistudio", manager)
	stream, errStream := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{ID: authID},
		cliproxyexecutor.Request{
			Model:   "gemini-test",
			Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
	)
	if errStream != nil {
		t.Fatal(errStream)
	}
	select {
	case chunk, ok := <-stream.Chunks:
		if !ok {
			t.Fatal("stream closed before translated chunk")
		}
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for translated chunk")
	}
	cancel()
	for range stream.Chunks {
	}
	if errClient := <-clientDone; errClient != nil {
		t.Fatal(errClient)
	}

	select {
	case record := <-plugin.records:
		if record.Alias != "aistudio-cancel-finalizer" {
			t.Fatalf("usage alias = %q", record.Alias)
		}
		if !record.Failed || record.Fail.StatusCode != 499 {
			t.Fatalf("usage record = %#v, want canceled failure", record)
		}
		if record.Detail.InputTokens != 0 || record.Detail.OutputTokens != 0 || record.Detail.TotalTokens != 0 {
			t.Fatalf("failure usage detail = %+v, want zero", record.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for canceled usage record")
	}
	select {
	case record := <-plugin.records:
		if record.Alias == "aistudio-cancel-finalizer" {
			t.Fatalf("received duplicate usage record: %#v", record)
		}
	case <-time.After(20 * time.Millisecond):
	}
}

func TestAIStudioHTTPResponseCancellationWinsOverBufferedUsage(t *testing.T) {
	const authID = "aistudio-http-cancel-auth"
	manager := wsrelay.NewManager(wsrelay.Options{
		ProviderFactory: func(*http.Request) (string, error) {
			return authID, nil
		},
	})
	server := httptest.NewServer(manager.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })

	websocketURL := "ws" + strings.TrimPrefix(server.URL, "http") + manager.Path()
	client, response, errDial := websocket.DefaultDialer.Dial(websocketURL, nil)
	if response != nil && response.Body != nil {
		t.Cleanup(func() { _ = response.Body.Close() })
	}
	if errDial != nil {
		t.Fatal(errDial)
	}
	t.Cleanup(func() { _ = client.Close() })

	clientDone := make(chan error, 1)
	go func() {
		var request struct {
			ID string `json:"id"`
		}
		if errRead := client.ReadJSON(&request); errRead != nil {
			clientDone <- errRead
			return
		}
		clientDone <- client.WriteJSON(map[string]any{
			"id":   request.ID,
			"type": wsrelay.MessageTypeHTTPResp,
			"payload": map[string]any{
				"status": http.StatusOK,
				"body":   `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]} ,"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7}}`,
			},
		})
	}()

	plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
	registerTestUsagePlugin(t, plugin)
	baseCtx := usage.WithRequestedModelAlias(context.Background(), "aistudio-http-cancel-finalizer")
	ctx, cancel := context.WithCancel(baseCtx)
	exec := NewAIStudioExecutor(nil, "aistudio", manager)
	stream, errStream := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{ID: authID},
		cliproxyexecutor.Request{
			Model:   "gemini-test",
			Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
	)
	if errStream != nil {
		t.Fatal(errStream)
	}
	cancel()
	for range stream.Chunks {
	}
	if errClient := <-clientDone; errClient != nil {
		t.Fatal(errClient)
	}

	select {
	case record := <-plugin.records:
		if record.Alias != "aistudio-http-cancel-finalizer" {
			t.Fatalf("usage alias = %q", record.Alias)
		}
		if !record.Failed || record.Fail.StatusCode != 499 {
			t.Fatalf("usage record = %#v, want canceled failure", record)
		}
		if record.Detail.InputTokens != 0 || record.Detail.OutputTokens != 0 || record.Detail.TotalTokens != 0 {
			t.Fatalf("failure usage detail = %+v, want zero", record.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for canceled usage record")
	}
	select {
	case record := <-plugin.records:
		if record.Alias == "aistudio-http-cancel-finalizer" {
			t.Fatalf("received duplicate usage record: %#v", record)
		}
	case <-time.After(20 * time.Millisecond):
	}
}

func TestAIStudioStreamPublishesSuccessfulZeroUsageOnce(t *testing.T) {
	const authID = "aistudio-zero-usage-auth"
	manager := wsrelay.NewManager(wsrelay.Options{
		ProviderFactory: func(*http.Request) (string, error) {
			return authID, nil
		},
	})
	server := httptest.NewServer(manager.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })

	websocketURL := "ws" + strings.TrimPrefix(server.URL, "http") + manager.Path()
	client, response, errDial := websocket.DefaultDialer.Dial(websocketURL, nil)
	if response != nil && response.Body != nil {
		t.Cleanup(func() { _ = response.Body.Close() })
	}
	if errDial != nil {
		t.Fatal(errDial)
	}
	t.Cleanup(func() { _ = client.Close() })

	clientDone := make(chan error, 1)
	go func() {
		var request struct {
			ID string `json:"id"`
		}
		if errRead := client.ReadJSON(&request); errRead != nil {
			clientDone <- errRead
			return
		}
		if errWrite := client.WriteJSON(map[string]any{
			"id":   request.ID,
			"type": wsrelay.MessageTypeStreamStart,
			"payload": map[string]any{
				"status": http.StatusOK,
			},
		}); errWrite != nil {
			clientDone <- errWrite
			return
		}
		clientDone <- client.WriteJSON(map[string]any{
			"id":   request.ID,
			"type": wsrelay.MessageTypeStreamEnd,
		})
	}()

	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	ctx := usage.WithRequestedModelAlias(context.Background(), "aistudio-zero-usage-finalizer")
	exec := NewAIStudioExecutor(nil, "aistudio", manager)
	stream, errStream := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{ID: authID},
		cliproxyexecutor.Request{
			Model:   "gemini-test",
			Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("gemini")},
	)
	if errStream != nil {
		t.Fatal(errStream)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
	if errClient := <-clientDone; errClient != nil {
		t.Fatal(errClient)
	}

	select {
	case record := <-plugin.records:
		if record.Alias != "aistudio-zero-usage-finalizer" {
			t.Fatalf("usage alias = %q", record.Alias)
		}
		if record.Failed {
			t.Fatalf("usage record = %#v, want success", record)
		}
		if record.Detail.InputTokens != 0 || record.Detail.OutputTokens != 0 || record.Detail.TotalTokens != 0 {
			t.Fatalf("usage detail = %+v, want zero usage", record.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for zero-usage record")
	}
	select {
	case record := <-plugin.records:
		if record.Alias == "aistudio-zero-usage-finalizer" {
			t.Fatalf("received duplicate usage record: %#v", record)
		}
	case <-time.After(20 * time.Millisecond):
	}
}
