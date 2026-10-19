package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
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

type aistudioCountingBodyReader struct {
	remaining int64
	read      int64
}

func (r *aistudioCountingBodyReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 'x'
	}
	n := len(p)
	r.remaining -= int64(n)
	r.read += int64(n)
	return n, nil
}

func TestAIStudioHTTPRejectsOversizedBody(t *testing.T) {
	const limit = 50 << 20
	body := &aistudioCountingBodyReader{remaining: limit + 1}
	request := httptest.NewRequest(http.MethodPost, "https://example.invalid/generate", body)
	relay := wsrelay.NewManager(wsrelay.Options{})
	executor := NewAIStudioExecutor(&config.Config{}, "aistudio", relay)
	_, errRequest := executor.HttpRequest(t.Context(), &cliproxyauth.Auth{ID: "synthetic-body-limit"}, request)
	var status interface{ StatusCode() int }
	if !errors.As(errRequest, &status) || status.StatusCode() != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized relay body error=%v, want HTTP 413", errRequest)
	}
	if body.read > limit+1 {
		t.Errorf("body read=%d, want at most %d", body.read, limit+1)
	}
}

func TestAIStudioTranslationFailureReleasesRelaySource(t *testing.T) {
	countReaders := func() int {
		buffer := make([]byte, 1<<20)
		n := runtime.Stack(buffer, true)
		return strings.Count(string(buffer[:n]), "/internal/wsrelay.streamPendingEvents.func1(")
	}
	before := countReaders()
	const authID = "synthetic-relay-source-lifecycle"
	connected := make(chan struct{})
	relay := wsrelay.NewManager(wsrelay.Options{
		ProviderFactory: func(*http.Request) (string, error) { return authID, nil },
		OnConnected:     func(string) { close(connected) },
	})
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = relay.Stop(context.Background()) })
	conn, response, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+relay.Path(), nil)
	if response != nil && response.Body != nil {
		t.Cleanup(func() { _ = response.Body.Close() })
	}
	if errDial != nil {
		t.Fatal(errDial)
	}
	t.Cleanup(func() { _ = conn.Close() })
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("relay connection was not registered")
	}
	clientDone := make(chan error, 1)
	go func() {
		var request struct {
			ID string `json:"id"`
		}
		if errRead := conn.ReadJSON(&request); errRead != nil {
			clientDone <- errRead
			return
		}
		for _, message := range []map[string]any{
			{"id": request.ID, "type": wsrelay.MessageTypeStreamStart, "payload": map[string]any{"status": http.StatusOK}},
			{"id": request.ID, "type": wsrelay.MessageTypeStreamChunk, "payload": map[string]any{"data": "data: {\"responseId\":\"lifecycle\",\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"apply_patch\",\"args\":{\"input\":7}}}]},\"finishReason\":\"STOP\"}]}\n\n"}},
		} {
			if errWrite := conn.WriteJSON(message); errWrite != nil {
				clientDone <- errWrite
				return
			}
		}
		clientDone <- nil
	}()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	executor := NewAIStudioExecutor(&config.Config{}, "aistudio", relay)
	request := cliproxyexecutor.Request{Model: "gemini-3.1-pro-preview", Payload: []byte(`{"input":"synthetic patch","tools":[{"type":"custom","name":"apply_patch"}]}`)}
	stream, errStream := executor.ExecuteStream(ctx, &cliproxyauth.Auth{ID: authID, Provider: "aistudio"}, request,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: request.Payload})
	if errStream != nil {
		t.Fatal(errStream)
	}
	failures := 0
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			failures++
		}
	}
	if failures != 1 {
		t.Fatalf("translation failures=%d, want 1", failures)
	}
	if errClient := <-clientDone; errClient != nil {
		t.Fatal(errClient)
	}
	deadline := time.Now().Add(time.Second)
	for countReaders() > before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if remaining := countReaders() - before; remaining != 0 {
		t.Errorf("translation failure left %d relay source readers waiting", remaining)
	}
	if ctx.Err() != nil {
		t.Errorf("executor canceled caller context: %v", ctx.Err())
	}
}

func TestAIStudioFailedResponsesPublishFailure(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, scenario := range []struct {
			name   string
			body   string
			failed bool
		}{
			{"provider-error", `{"error":{"code":503,"message":"synthetic upstream failure"},"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`, true},
			{"missing-finish", `{"responseId":"synthetic","candidates":[{"content":{"parts":[{"text":"partial"}]}}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`, true},
			{"completed", `{"responseId":"synthetic","candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`, false},
		} {
			name := scenario.name + "/execute"
			if streaming {
				name = scenario.name + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				authID := "synthetic-" + t.Name()
				connected := make(chan struct{})
				relay := wsrelay.NewManager(wsrelay.Options{
					ProviderFactory: func(*http.Request) (string, error) { return authID, nil },
					OnConnected:     func(string) { close(connected) },
				})
				server := httptest.NewServer(relay.Handler())
				t.Cleanup(server.Close)
				t.Cleanup(func() { _ = relay.Stop(context.Background()) })
				conn, response, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+relay.Path(), nil)
				if response != nil && response.Body != nil {
					t.Cleanup(func() { _ = response.Body.Close() })
				}
				if errDial != nil {
					t.Fatal(errDial)
				}
				t.Cleanup(func() { _ = conn.Close() })
				select {
				case <-connected:
				case <-time.After(time.Second):
					t.Fatal("relay connection was not registered")
				}
				clientDone := make(chan error, 1)
				go func() {
					var request struct {
						ID string `json:"id"`
					}
					if errRead := conn.ReadJSON(&request); errRead != nil {
						clientDone <- errRead
						return
					}
					messages := []map[string]any{{"id": request.ID, "type": wsrelay.MessageTypeHTTPResp, "payload": map[string]any{"status": http.StatusOK, "body": scenario.body}}}
					if streaming {
						messages = []map[string]any{
							{"id": request.ID, "type": wsrelay.MessageTypeStreamStart, "payload": map[string]any{"status": http.StatusOK}},
							{"id": request.ID, "type": wsrelay.MessageTypeStreamChunk, "payload": map[string]any{"data": "data: " + scenario.body + "\n\n"}},
							{"id": request.ID, "type": wsrelay.MessageTypeStreamEnd},
						}
					}
					for _, message := range messages {
						if errWrite := conn.WriteJSON(message); errWrite != nil {
							clientDone <- errWrite
							return
						}
					}
					clientDone <- nil
				}()
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
				registerTestUsagePlugin(t, plugin)
				ctx := usage.WithRequestedModelAlias(t.Context(), t.Name())
				executor := NewAIStudioExecutor(&config.Config{}, "aistudio", relay)
				request := cliproxyexecutor.Request{Model: "gemini-3.6-flash", Payload: []byte(`{"input":"synthetic response"}`)}
				options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: request.Payload}
				auth := &cliproxyauth.Auth{ID: authID, Provider: "aistudio"}
				var failure error
				if streaming {
					stream, errStream := executor.ExecuteStream(ctx, auth, request, options)
					if errStream != nil {
						t.Fatal(errStream)
					}
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							if failure != nil {
								t.Error("duplicate stream error")
							}
							failure = chunk.Err
						}
					}
				} else {
					response, errExecute := executor.Execute(ctx, auth, request, options)
					failure = errExecute
					if scenario.failed && len(response.Payload) != 0 {
						t.Error("failed response returned a payload")
					}
				}
				if scenario.failed {
					var status interface{ StatusCode() int }
					if !errors.As(failure, &status) || status.StatusCode() != http.StatusBadGateway {
						t.Errorf("failed response error=%v, want HTTP 502", failure)
					}
				} else if failure != nil {
					t.Errorf("successful response error=%v", failure)
				}
				if errClient := <-clientDone; errClient != nil {
					t.Fatal(errClient)
				}
				select {
				case record := <-plugin.records:
					if record.Alias != t.Name() || record.Failed != scenario.failed || record.Completed == scenario.failed {
						t.Errorf("usage alias=%q failed=%v completed=%v, want failed=%v", record.Alias, record.Failed, record.Completed, scenario.failed)
					}
					if record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 3 {
						t.Errorf("observed usage was lost: %+v", record.Detail)
					}
				case <-time.After(time.Second):
					t.Fatal("usage record was not published")
				}
			})
		}
	}
}

func TestAIStudioPreparationFailurePublishesUsageOnce(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "execute"
		if streaming {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
			registerTestUsagePlugin(t, plugin)
			ctx := usage.WithRequestedModelAlias(t.Context(), t.Name())
			executor := NewAIStudioExecutor(&config.Config{}, "aistudio", wsrelay.NewManager(wsrelay.Options{}))
			auth := &cliproxyauth.Auth{ID: "synthetic-preparation-failure", Provider: "aistudio"}
			request := cliproxyexecutor.Request{Model: "gemini-3.6-flash", Payload: []byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"synthetic-file-not-stored"}}]}]}`)}
			options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
			var errRequest error
			if streaming {
				_, errRequest = executor.ExecuteStream(ctx, auth, request, options)
			} else {
				_, errRequest = executor.Execute(ctx, auth, request, options)
			}
			var unsupported *translatorcommon.UnsupportedPartError
			if !errors.As(errRequest, &unsupported) || unsupported.StatusCode() != http.StatusBadRequest || !unsupported.IsRequestScoped() {
				t.Fatalf("preparation error=%v, want request-scoped HTTP 400", errRequest)
			}
			usage.PublishRecord(context.Background(), usage.Record{Alias: t.Name() + "-barrier"})
			count := 0
			for {
				select {
				case record := <-plugin.records:
					if record.Alias == t.Name()+"-barrier" {
						if count != 1 {
							t.Errorf("preparation failure usage records=%d, want 1", count)
						}
						return
					}
					count++
					if record.Alias != t.Name() || !record.Failed || record.Completed || record.Fail.StatusCode != http.StatusBadRequest {
						t.Errorf("preparation failure usage=%+v", record)
					}
				case <-time.After(time.Second):
					t.Fatal("usage dispatcher barrier did not arrive")
				}
			}
		})
	}
}
