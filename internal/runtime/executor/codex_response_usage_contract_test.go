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
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type codexContractUsageCapture struct {
	authID  string
	records chan usage.Record
}

func (capture *codexContractUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if capture.records != nil && record.AuthID == capture.authID {
		capture.records <- record
	}
}

func newCodexContractUsageCapture(t *testing.T) *codexContractUsageCapture {
	t.Helper()
	capture := &codexContractUsageCapture{authID: t.Name(), records: make(chan usage.Record, 16)}
	usage.RegisterNamedPlugin(t.Name(), capture)
	t.Cleanup(func() { usage.RegisterNamedPlugin(t.Name(), &codexContractUsageCapture{}) })
	return capture
}

func (capture *codexContractUsageCapture) next(t *testing.T) usage.Record {
	t.Helper()
	select {
	case record := <-capture.records:
		return record
	case <-time.After(2 * time.Second):
		t.Fatal("usage consumer did not receive the expected record")
		return usage.Record{}
	}
}

func TestCodexResponsesMainUsagePrecedesImageToolUsage(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		for _, streaming := range []bool{false, true} {
			for _, mainUsage := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/main=%t", transport, streaming, mainUsage), func(t *testing.T) {
					capture := newCodexContractUsageCapture(t)
					main := ""
					if mainUsage {
						main = `,"usage":{"input_tokens":100,"output_tokens":40,"total_tokens":140}`
					}
					completed := `{"type":"response.completed","response":{"id":"synthetic-image-usage","model":"gpt-5.5","status":"completed","output":[]` + main + `,"tool_usage":{"image_gen":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}}`
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
							if err = peer.WriteMessage(websocket.TextMessage, []byte(completed)); err != nil {
								t.Error(err)
							}
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: "+completed+"\n\n")
					}))
					defer server.Close()
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					ctx = usage.WithRequestedModelAlias(ctx, "main-request-alias")
					if transport == "websocket" {
						ctx = core.WithDownstreamWebsocket(ctx)
					}
					auth := runtimeCodexAuth(server.URL)
					auth.ID = capture.authID
					executor := NewCodexAutoExecutor(&config.Config{})
					req := core.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"synthetic","tools":[{"type":"image_generation","model":"gpt-image-2.5-flare"}]}`)}
					opts := core.Options{SourceFormat: sdktranslator.FormatCodex}
					if streaming {
						result, err := executor.ExecuteStream(ctx, auth, req, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Acknowledge != nil {
								chunk.Acknowledge(chunk.Err)
							}
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
						}
					} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
					first, second := capture.next(t), capture.next(t)
					wantMain := int64(0)
					if mainUsage {
						wantMain = 140
					}
					if first.Model != "gpt-5.5" || first.Alias != "main-request-alias" || first.Failed || first.Detail.TotalTokens != wantMain {
						t.Fatalf("main usage must arrive first: %+v", first)
					}
					if second.Model != "gpt-image-2.5-flare" || second.Alias != second.Model || second.Failed || second.Detail.TotalTokens != 30 {
						t.Fatalf("image usage did not follow main usage: %+v", second)
					}
				})
			}
		}
	}
}

func TestCodexFinalModelAcrossExecutionTransports(t *testing.T) {
	for _, operation := range []string{"http", "http-stream", "websocket", "websocket-stream", "compact"} {
		for _, finalModel := range []string{"gpt-5.5", "gpt-5.3-codex-spark"} {
			t.Run(operation+"/"+finalModel, func(t *testing.T) {
				type requestSnapshot struct {
					body    []byte
					headers http.Header
				}
				requests := make(chan requestSnapshot, 1)
				completed := fmt.Sprintf(`{"type":"response.completed","response":{"id":"synthetic-override","model":%q,"status":"completed","output":[]}}`, finalModel)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(operation, "websocket") {
						peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer peer.Close()
						_, body, err := peer.ReadMessage()
						if err != nil {
							t.Error(err)
							return
						}
						requests <- requestSnapshot{body: body, headers: r.Header.Clone()}
						if err = peer.WriteMessage(websocket.TextMessage, []byte(completed)); err != nil {
							t.Error(err)
						}
						return
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					requests <- requestSnapshot{body: body, headers: r.Header.Clone()}
					if operation == "compact" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, gjson.Get(completed, "response").Raw)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+completed+"\n\n")
				}))
				defer server.Close()
				cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
					Models: []config.PayloadModelRule{{Name: "gpt-5.4"}}, Params: map[string]any{"model": finalModel},
				}}}}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if strings.HasPrefix(operation, "websocket") {
					ctx = core.WithDownstreamWebsocket(ctx)
				}
				executor := NewCodexAutoExecutor(cfg)
				req := core.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"synthetic"}`)}
				opts := core.Options{SourceFormat: sdktranslator.FormatCodex}
				if operation == "compact" {
					opts.Alt = "responses/compact"
				}
				if strings.HasSuffix(operation, "-stream") {
					result, err := executor.ExecuteStream(ctx, runtimeCodexAuth(server.URL), req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Acknowledge != nil {
							chunk.Acknowledge(chunk.Err)
						}
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if _, err := executor.Execute(ctx, runtimeCodexAuth(server.URL), req, opts); err != nil {
					t.Fatal(err)
				}
				snapshot := <-requests
				if model := gjson.GetBytes(snapshot.body, "model").String(); model != finalModel {
					t.Fatalf("wire model=%q, want %q, body=%s", model, finalModel, snapshot.body)
				}
				for key, expected := range registry.ModelOverrideHeaders(finalModel) {
					if snapshot.headers.Get(key) != expected {
						t.Fatalf("header %s=%q, want %q", key, snapshot.headers.Get(key), expected)
					}
				}
				if strings.HasSuffix(finalModel, "spark") {
					for _, tool := range gjson.GetBytes(snapshot.body, "tools").Array() {
						if tool.Get("type").String() == "image_generation" {
							t.Fatalf("image tool added to final spark model: %s", snapshot.body)
						}
					}
				}
			})
		}
	}
}
