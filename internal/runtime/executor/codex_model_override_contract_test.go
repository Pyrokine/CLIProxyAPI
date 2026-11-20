package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestApplyModelHeaderOverridesMultipleHeaders(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-model-header-override"
	reg.RegisterClient(
		clientID, "codex", []*registry.ModelInfo{
			{
				ID: "test-override-headers-model",
				Config: &registry.ModelConfig{
					OverrideHeader: map[string]string{
						"user-agent": "custom-ua/1.0", "originator": "custom-origin", "x-test-header": "forced-value",
					},
				},
			},
		},
	)
	t.Cleanup(func() { reg.UnregisterClient(clientID) })
	headers := http.Header{}
	headers.Set("User-Agent", "old-ua")
	headers.Set("Originator", "old-origin")
	headers.Set("X-Test-Header", "old-value")
	applyModelHeaderOverrides(headers, "test-override-headers-model")
	for key, want := range map[string]string{
		"User-Agent": "custom-ua/1.0", "Originator": "custom-origin", "X-Test-Header": "forced-value",
	} {
		if got := headers.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestApplyModelHeaderOverridesFromModelConfig(t *testing.T) {
	const wantUA = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CodexHeaderDefaults: config.CodexHeaderDefaults{UserAgent: "config-ua"}}
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"email": "user@example.com"}}
	applyCodexHeaders(req, auth, "oauth-token", true, cfg)
	applyModelHeaderOverrides(req.Header, "gpt-5.6-luna")
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Fatalf("User-Agent = %q, want %q", got, wantUA)
	}
	if codexSessionHeaderValue(req.Header) == "" {
		t.Fatal("Mac OS override did not set a session header")
	}
	applyModelHeaderOverrides(req.Header, "gpt-5.4")
	if got := req.Header.Get("User-Agent"); got != wantUA {
		t.Fatalf("no-op override changed User-Agent to %q", got)
	}
}

func TestCodexDynamicModelHeaderChangesReachExecutionTransports(t *testing.T) {
	for _, operation := range []string{
		"http", "http-stream", "websocket", "websocket-stream", "compact",
		"wrapped-image", "wrapped-image-stream", "direct-image", "direct-image-stream",
	} {
		t.Run(
			operation, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				type requestSnapshot struct {
					path    string
					body    []byte
					headers http.Header
				}
				requests := make(chan requestSnapshot, 4)
				modelID := "gpt-5.6-luna"
				if strings.HasPrefix(operation, "direct-image") {
					modelID = "gpt-image-1.5"
				}
				response := fmt.Sprintf(
					`{"id":"synthetic-dynamic-header","model":%q,"status":"completed","created_at":`+
						`1713833628,"output":[{"type":"image_generation_call","result":"AA==","output_format"`+
						`:"png"}]}`,
					modelID,
				)
				completed := `{"type":"response.completed","response":` + response + `}`
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							capture := func(body []byte) {
								select {
								case requests <- requestSnapshot{
									path: r.URL.Path, body: body, headers: r.Header.Clone(),
								}:
								case <-ctx.Done():
								}
							}
							if strings.HasPrefix(operation, "websocket") {
								peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
								if err != nil {
									t.Error(err)
									return
								}
								defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
								if err = peer.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
									t.Error(err)
									return
								}
								for {
									_, body, errRead := peer.ReadMessage()
									if errRead != nil {
										return
									}
									capture(body)
									if err = peer.WriteMessage(websocket.TextMessage, []byte(completed)); err != nil {
										t.Error(err)
										return
									}
								}
							}
							body, err := io.ReadAll(r.Body)
							if err != nil {
								t.Error(err)
								return
							}
							capture(body)
							switch {
							case operation == "compact":
								w.Header().Set("Content-Type", "application/json")
								_, _ = io.WriteString(w, response)
							case operation == "direct-image":
								w.Header().Set("Content-Type", "application/json")
								_, _ = io.WriteString(w, `{"created":1713833628,"data":[{"b64_json":"AA=="}]}`)
							case operation == "direct-image-stream":
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = io.WriteString(
									w,
									"event: image_generation.completed\ndata: {\"type\":"+
										"\"image_generation.completed\",\"b64_json\":"+
										"\"AA==\"}\n\n",
								)
							default:
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = io.WriteString(w, "data: "+completed+"\n\n")
							}
						},
					),
				)
				defer server.Close()
				cfg := &config.Config{GPTImage2BaseModel: modelID}
				auth := runtimeCodexAuth(server.URL)
				auth.ID = t.Name()
				executor := NewCodexAutoExecutor(cfg)
				defer executor.CloseExecutionSession(t.Name())
				req := core.Request{
					Model: modelID, Payload: []byte(fmt.Sprintf(`{"model":%q,"input":"synthetic"}`, modelID)),
				}
				opts := core.Options{
					SourceFormat: sdktranslator.FormatCodex, Metadata: map[string]any{
						core.ExecutionSessionMetadataKey: t.Name(),
					},
				}
				wantPath := "/responses"
				if strings.HasPrefix(operation, "websocket") {
					ctx = core.WithDownstreamWebsocket(ctx)
				}
				if operation == "compact" {
					opts.Alt = "responses/compact"
					wantPath = "/responses/compact"
				}
				if strings.Contains(operation, "image") {
					auth = newCodexOpenAIImageTestAuth(server.URL)
					auth.ID = t.Name()
					requestModel := "gpt-image-1"
					if strings.HasPrefix(operation, "direct-image") {
						requestModel = "codex/" + modelID
						wantPath = "/images/generations"
					}
					req = core.Request{
						Model:   requestModel,
						Payload: []byte(fmt.Sprintf(`{"model":%q,"prompt":"synthetic"}`, requestModel)),
					}
					opts = codexOpenAIImageTestOptions(
						codexImagesGenerationsPath, strings.HasSuffix(operation, "-stream"),
					)
				}
				reg := registry.GetGlobalRegistry()
				defer reg.UnregisterClient(t.Name())
				for _, revision := range []string{"first", "updated", "cleared"} {
					info := &registry.ModelInfo{ID: modelID}
					if revision != "cleared" {
						info.Config = &registry.ModelConfig{
							OverrideHeader: map[string]string{
								"user-agent":      "synthetic-" + revision,
								"originator":      "origin-" + revision,
								"x-dynamic-model": revision,
							},
						}
					}
					reg.RegisterClient(t.Name(), "codex", []*registry.ModelInfo{info})
					if strings.HasSuffix(operation, "-stream") {
						stream, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatalf("%s: %v", revision, err)
						}
						for chunk := range stream.Chunks {
							if chunk.Acknowledge != nil {
								chunk.Acknowledge(chunk.Err)
							}
							if chunk.Err != nil {
								t.Fatalf("%s: %v", revision, chunk.Err)
							}
						}
					} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
						t.Fatalf("%s: %v", revision, err)
					}
					var snapshot requestSnapshot
					select {
					case snapshot = <-requests:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if snapshot.path != wantPath || gjson.GetBytes(snapshot.body, "model").String() != modelID {
						t.Fatalf("%s: path=%q body=%s", revision, snapshot.path, snapshot.body)
					}
					if revision == "cleared" {
						if snapshot.headers.Get("X-Dynamic-Model") != "" ||
							strings.HasPrefix(snapshot.headers.Get("User-Agent"), "synthetic-") ||
							strings.HasPrefix(snapshot.headers.Get("Originator"), "origin-") {
							t.Fatalf("cleared configuration retained old headers: %v", snapshot.headers)
						}
						continue
					}
					for key, want := range map[string]string{
						"User-Agent": "synthetic-" + revision, "Originator": "origin-" + revision,
						"X-Dynamic-Model": revision,
					} {
						if got := snapshot.headers.Get(key); got != want {
							t.Fatalf("%s: %s=%q, want %q", revision, key, got, want)
						}
					}
				}
			},
		)
	}
}
