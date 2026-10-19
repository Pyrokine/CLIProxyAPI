package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/wsrelay"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestAIStudioResponseFormatIndependentFromRequest(t *testing.T) {
	const authID = "offline-aistudio-format"
	const prompt = "offline-format-prompt"
	const response = `{"candidates":[{"content":{"role":"model","parts":[{"text":"offline-format-answer"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}`
	for _, method := range []string{"execute", "stream", "stream-http", "count"} {
		for _, target := range []sdktranslator.Format{sdktranslator.FormatClaude, ""} {
			t.Run(method+"/"+target.String(), func(t *testing.T) {
				manager := wsrelay.NewManager(wsrelay.Options{ProviderFactory: func(*http.Request) (string, error) { return authID, nil }})
				server := httptest.NewServer(manager.Handler())
				t.Cleanup(server.Close)
				t.Cleanup(func() { _ = manager.Stop(context.Background()) })
				conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+manager.Path(), nil)
				if resp != nil && resp.Body != nil {
					t.Cleanup(func() { _ = resp.Body.Close() })
				}
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = conn.Close() })
				if err = conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
					t.Fatal(err)
				}
				clientDone := make(chan error, 1)
				go func() {
					var request struct {
						ID      string `json:"id"`
						Payload struct {
							Body string `json:"body"`
						} `json:"payload"`
					}
					if errRead := conn.ReadJSON(&request); errRead != nil {
						clientDone <- errRead
						return
					}
					if gjson.Get(request.Payload.Body, "contents.0.parts.0.text").String() != prompt {
						clientDone <- fmt.Errorf("Chat request was not translated to Gemini: %s", request.Payload.Body)
						return
					}
					if method == "stream" {
						for _, event := range []map[string]any{
							{"id": request.ID, "type": wsrelay.MessageTypeStreamStart, "payload": map[string]any{"status": http.StatusOK}},
							{"id": request.ID, "type": wsrelay.MessageTypeStreamChunk, "payload": map[string]any{"data": "data: " + response + "\n\n"}},
							{"id": request.ID, "type": wsrelay.MessageTypeStreamEnd},
						} {
							if errWrite := conn.WriteJSON(event); errWrite != nil {
								clientDone <- errWrite
								return
							}
						}
						clientDone <- nil
						return
					}
					body := response
					if method == "count" {
						body = `{"totalTokens":7}`
					}
					clientDone <- conn.WriteJSON(map[string]any{"id": request.ID, "type": wsrelay.MessageTypeHTTPResp, "payload": map[string]any{"status": http.StatusOK, "body": body}})
				}()
				exec := NewAIStudioExecutor(&config.Config{}, "aistudio", manager)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				req := cliproxyexecutor.Request{Model: "gemini-test", Payload: []byte(`{"model":"gemini-test","messages":[{"role":"user","content":"` + prompt + `"}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: target, OriginalRequest: req.Payload}
				if strings.HasPrefix(method, "stream") {
					opts.Stream = true
					stream, errStream := exec.ExecuteStream(ctx, &cliproxyauth.Auth{ID: authID}, req, opts)
					if errStream != nil {
						t.Fatal(errStream)
					}
					var output strings.Builder
					var streamErr error
					for chunk := range stream.Chunks {
						output.Write(chunk.Payload)
						if chunk.Err != nil {
							streamErr = chunk.Err
						}
					}
					text := strings.ReplaceAll(output.String(), ": ", ":")
					if streamErr != nil || !strings.Contains(text, "offline-format-answer") ||
						(target == sdktranslator.FormatClaude && strings.Count(text, `"type":"message_stop"`) != 1) ||
						(target == "" && (!strings.Contains(text, `"chat.completion.chunk"`) || strings.Contains(text, `"type":"message_stop"`))) {
						t.Fatalf("stream did not use the selected response format: err=%v, output=%s", streamErr, text)
					}
				} else {
					var result cliproxyexecutor.Response
					if method == "count" {
						opts.CountTokens = true
						result, err = exec.CountTokens(ctx, &cliproxyauth.Auth{ID: authID}, req, opts)
					} else {
						result, err = exec.Execute(ctx, &cliproxyauth.Auth{ID: authID}, req, opts)
					}
					if err != nil {
						t.Fatal(err)
					}
					if method == "count" {
						path := "totalTokens"
						if target == sdktranslator.FormatClaude {
							path = "input_tokens"
						}
						if gjson.GetBytes(result.Payload, path).Int() != 7 {
							t.Fatalf("token count did not use the selected response format: %s", result.Payload)
						}
					} else {
						path := "choices.0.message.content"
						if target == sdktranslator.FormatClaude {
							path = "content.0.text"
						}
						if gjson.GetBytes(result.Payload, path).String() != "offline-format-answer" {
							t.Fatalf("response did not use the selected response format: %s", result.Payload)
						}
					}
				}
				if errClient := <-clientDone; errClient != nil {
					t.Fatal(errClient)
				}
			})
		}
	}
}

func TestCodexWebsocketsResponseFormatIndependentFromRequest(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, target := range []sdktranslator.Format{sdktranslator.FormatClaude, ""} {
			t.Run(fmt.Sprintf("stream=%t/%s", streaming, target), func(t *testing.T) {
				server := newCodexTerminalWebsocketServer(t, "response.completed")
				t.Cleanup(server.Close)
				exec := newCodexWebsocketsExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				auth := &cliproxyauth.Auth{ID: "offline-codex-format", Provider: "codex", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": server.URL}}
				req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"offline-format-prompt"}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: target, Stream: streaming, OriginalRequest: req.Payload}
				if streaming {
					req.Payload = []byte(strings.Replace(string(req.Payload), `"model":`, `"stream":true,"model":`, 1))
					opts.OriginalRequest = req.Payload
					stream, err := exec.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					var output strings.Builder
					var streamErr error
					for chunk := range stream.Chunks {
						output.Write(chunk.Payload)
						if chunk.Err != nil {
							streamErr = chunk.Err
						}
						if chunk.Acknowledge != nil {
							chunk.Acknowledge(nil)
						}
					}
					text := output.String()
					if streamErr != nil || !strings.Contains(text, "hello") ||
						(target == sdktranslator.FormatClaude && strings.Count(text, `"type":"message_stop"`) != 1) ||
						(target == "" && (!strings.Contains(text, `"chat.completion.chunk"`) || strings.Contains(text, `"type":"message_stop"`))) {
						t.Fatalf("stream did not use the selected response format: err=%v, output=%s", streamErr, text)
					}
					return
				}
				response, err := exec.Execute(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				path := "choices.0.message.content"
				if target == sdktranslator.FormatClaude {
					path = "content.0.text"
				}
				if gjson.GetBytes(response.Payload, path).String() != "hello" {
					t.Fatalf("response did not use the selected response format: %s", response.Payload)
				}
			})
		}
	}
}
