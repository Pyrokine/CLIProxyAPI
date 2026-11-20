package executor

import (
	"bytes"
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
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestXAIThinkingPassthroughReachesHTTPAndWebsocket(t *testing.T) {
	for _, profile := range []string{"unknown", "user-defined"} {
		for _, suffix := range []bool{false, true} {
			for _, route := range []string{"http", "http-stream", "websocket"} {
				t.Run(
					fmt.Sprintf("%s/%s/suffix=%t", profile, route, suffix), func(t *testing.T) {
						model := "audit-xai-effort-" + profile
						if registry.LookupModelInfo(model, "xai") != nil {
							t.Fatal("fixture model must be absent from registry")
						}
						requests := make(chan []byte, 1)
						const completed = `{"type":"response.completed","response":{"id":"effort-response",` +
							`"status":"completed","output":` +
							`[]}}`
						server := httptest.NewServer(
							http.HandlerFunc(
								func(w http.ResponseWriter, r *http.Request) {
									if r.URL.Path != "/responses" {
										t.Errorf("unexpected upstream path: %s", r.URL.Path)
										w.WriteHeader(http.StatusNotFound)
										return
									}
									if websocket.IsWebSocketUpgrade(r) {
										conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
										if err != nil {
											t.Errorf("upgrade: %v", err)
											return
										}
										defer func() { _ = conn.Close() }()
										if _, body, errRead := conn.ReadMessage(); errRead != nil {
											t.Errorf("read websocket: %v", errRead)
											return
										} else {
											requests <- body
										}
										if errWrite := conn.WriteMessage(
											websocket.TextMessage, []byte(completed),
										); errWrite != nil {
											t.Errorf("write websocket: %v", errWrite)
										}
										return
									}
									body, errRead := io.ReadAll(r.Body)
									if errRead != nil {
										t.Errorf("read HTTP: %v", errRead)
										return
									}
									requests <- body
									w.Header().Set("Content-Type", "text/event-stream")
									_, _ = io.WriteString(w, "data: "+completed+"\n\n")
								},
							),
						)
						defer server.Close()
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						auth := &cliproxyauth.Auth{
							ID: t.Name(), Provider: "xai", ProxyURL: "direct",
							Attributes: map[string]string{"base_url": server.URL, "api_key": "synthetic"},
						}
						requestedModel, bodyEffort := model, "high"
						if suffix {
							requestedModel, bodyEffort = model+"(high)", "low"
						}
						req := cliproxyexecutor.Request{
							Model: requestedModel, Payload: []byte(fmt.Sprintf(
								`{"model":%q,"input":"hello","reasoning":{"effort":%q,"summary":"auto"}`+
									`,"stop":["STOP"],"include":["reasoning.encrypted_content","web_search_`+
									`call.action.sources"]}`,
								requestedModel, bodyEffort,
							)),
						}
						if profile == "user-defined" {
							req.Metadata = map[string]any{
								"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{
									ID: model, Type: "xai", UserDefined: true,
								},
							}
						}
						original := bytes.Clone(req.Payload)
						opts := cliproxyexecutor.Options{
							SourceFormat:   sdktranslator.FormatOpenAIResponse,
							ResponseFormat: sdktranslator.FormatOpenAIResponse,
							Metadata: map[string]any{
								cliproxyexecutor.ExecutionSessionMetadataKey: t.Name(),
							},
						}
						exec := newXAIExecutor(&config.Config{})
						if route == "http" {
							response, err := exec.Execute(ctx, auth, req, opts)
							if err != nil || !gjson.ValidBytes(response.Payload) {
								t.Fatalf("Execute: response=%s error=%v", response.Payload, err)
							}
						} else {
							var stream *cliproxyexecutor.StreamResult
							var err error
							if route == "websocket" {
								ws := newXAIWebsocketsExecutor(&config.Config{})
								t.Cleanup(func() { ws.CloseExecutionSession(t.Name()) })
								stream, err = ws.ExecuteStream(
									cliproxyexecutor.WithDownstreamWebsocket(ctx), auth, req, opts,
								)
							} else {
								stream, err = exec.ExecuteStream(ctx, auth, req, opts)
							}
							if err != nil {
								t.Fatal(err)
							}
							var output strings.Builder
							for chunk := range stream.Chunks {
								if chunk.Err != nil {
									t.Error(chunk.Err)
								}
								output.Write(chunk.Payload)
								if chunk.Acknowledge != nil {
									chunk.Acknowledge(nil)
								}
							}
							if !strings.Contains(output.String(), "response.completed") {
								t.Fatalf("terminal response missing: %s", output.String())
							}
						}
						select {
						case body := <-requests:
							if gjson.GetBytes(body, "reasoning.effort").String() != "high" ||
								gjson.GetBytes(body, "reasoning.summary").String() != "auto" {
								t.Fatalf("shared thinking result changed before send: %s", body)
							}
							if gjson.GetBytes(body, "stop").Exists() ||
								gjson.GetBytes(body, "include.0").String() != "web_search_call.action.sources" {
								t.Fatalf("Responses sanitation changed: %s", body)
							}
						case <-ctx.Done():
							t.Fatal("local upstream did not receive a request")
						}
						if !bytes.Equal(req.Payload, original) {
							t.Fatal("caller payload was modified")
						}
					},
				)
			}
		}
	}
}
