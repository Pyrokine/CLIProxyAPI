package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const runtimeCodexCompleted = `{"type":"response.completed","response":{"id":"runtime-codex-response","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`

func runtimeCodexAuth(url string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: "runtime-codex-synthetic", Provider: "codex", Attributes: map[string]string{"api_key": "synthetic-not-a-credential", "base_url": url, "websockets": "true"}}
}

func TestCodexRuntimeCancellationOwnsEphemeralSocket(t *testing.T) {
	peerCh := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer peer.Close()
		if _, _, err = peer.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		peerCh <- peer
		<-release
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executor := NewCodexAutoExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
	result, err := executor.ExecuteStream(core.WithDownstreamWebsocket(ctx), runtimeCodexAuth(server.URL), core.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"fixture"}`)}, core.Options{SourceFormat: sdktranslator.FormatCodex})
	if err != nil {
		t.Fatal(err)
	}
	var peer *websocket.Conn
	select {
	case peer = <-peerCh:
	case <-time.After(2 * time.Second):
		t.Fatal("local peer did not receive create")
	}
	defer peer.Close()
	finished := make(chan struct{})
	go func() {
		for range result.Chunks {
		}
		close(finished)
	}()
	cancel()
	select {
	case <-finished:
		t.Log("ephemeral websocket read released on caller cancellation")
	case <-time.After(250 * time.Millisecond):
		t.Error("caller cancellation left the ephemeral websocket read blocked; releasing synthetic peer to avoid a leaked test goroutine")
		peer.Close()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Fatal("read remained blocked after synthetic peer closed")
		}
	}
}

type runtimeCodexTranslationState struct{}

func (runtimeCodexTranslationState) ResponseTranslationError() error {
	return errors.New("synthetic downstream response translation failure")
}

func TestCodexRuntimeNonstreamTranslationErrorsPropagate(t *testing.T) {
	responseFormat := sdktranslator.Format("runtime-codex-translation-failure")
	sdktranslator.Register(responseFormat, sdktranslator.FormatCodex, nil, sdktranslator.ResponseTransform{
		NonStream: func(_ context.Context, _ string, _, _, _ []byte, param *any) []byte {
			*param = runtimeCodexTranslationState{}
			return nil
		},
	})
	var directState any
	if out := sdktranslator.TranslateNonStream(t.Context(), sdktranslator.FormatCodex, responseFormat, "gpt-5.4", nil, nil, []byte(runtimeCodexCompleted), &directState); len(out) != 0 || sdktranslator.ResponseTranslationError(&directState) == nil {
		t.Fatal("synthetic translation rejection did not reach SDK error channel")
	}
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if transport == "websocket" {
					peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer peer.Close()
					if _, _, err = peer.ReadMessage(); err != nil {
						t.Error(err)
						return
					}
					if err = peer.WriteMessage(websocket.TextMessage, []byte(runtimeCodexCompleted)); err != nil {
						t.Error(err)
					}
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: "+runtimeCodexCompleted+"\n\n")
			}))
			defer server.Close()
			cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
			req := core.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"fixture"}`)}
			opts := core.Options{SourceFormat: sdktranslator.FormatCodex, ResponseFormat: responseFormat}
			var response core.Response
			var err error
			if transport == "websocket" {
				response, err = NewCodexAutoExecutor(cfg).Execute(core.WithDownstreamWebsocket(t.Context()), runtimeCodexAuth(server.URL), req, opts)
			} else {
				response, err = NewCodexAutoExecutor(cfg).Execute(t.Context(), runtimeCodexAuth(server.URL), req, opts)
			}
			if err == nil {
				t.Errorf("%s executor returned nil error after SDK rejected translation; payload length=%d", transport, len(response.Payload))
			}
		})
	}
}

func TestCodexRuntimeFinalPayloadOverrideReachesWire(t *testing.T) {
	captured := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		captured <- body
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: "+runtimeCodexCompleted+"\n\n")
	}))
	defer server.Close()
	cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll, Payload: config.PayloadConfig{
		Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gpt-5.4", Protocol: "codex"}}, Params: map[string]any{"model": "gpt-5.5", "audit_marker": "override-applied"}}},
	}}
	_, err := NewCodexAutoExecutor(cfg).Execute(t.Context(), runtimeCodexAuth(server.URL), core.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"fixture"}`)}, core.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatal(err)
	}
	body := <-captured
	if marker := gjson.GetBytes(body, "audit_marker").String(); marker != "override-applied" {
		t.Fatalf("override rule did not match: %s", body)
	}
	if model := gjson.GetBytes(body, "model").String(); model != "gpt-5.5" {
		t.Errorf("final operator model override was overwritten: model=%q body=%s", model, body)
	}
}

func TestCodexRuntimeNamedSessionCancellationControl(t *testing.T) {
	peerCh := make(chan *websocket.Conn, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer peer.Close()
		if _, _, err = peer.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		peerCh <- peer
		<-release
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	executor := NewCodexAutoExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
	defer executor.CloseExecutionSession("runtime-codex-control")
	result, err := executor.ExecuteStream(core.WithDownstreamWebsocket(ctx), runtimeCodexAuth(server.URL), core.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"fixture"}`)}, core.Options{SourceFormat: sdktranslator.FormatCodex, Metadata: map[string]any{core.ExecutionSessionMetadataKey: "runtime-codex-control"}})
	if err != nil {
		t.Fatal(err)
	}
	var peer *websocket.Conn
	select {
	case peer = <-peerCh:
	case <-time.After(2 * time.Second):
		t.Fatal("local peer did not receive create")
	}
	defer peer.Close()
	finished := make(chan struct{})
	go func() {
		for range result.Chunks {
		}
		close(finished)
	}()
	cancel()
	select {
	case <-finished:
		t.Log("ephemeral websocket read released on caller cancellation")
	case <-time.After(250 * time.Millisecond):
		t.Error("caller cancellation left the ephemeral websocket read blocked; releasing synthetic peer to avoid a leaked test goroutine")
		peer.Close()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
			t.Fatal("read remained blocked after synthetic peer closed")
		}
	}
}
