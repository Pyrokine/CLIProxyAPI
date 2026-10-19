package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestNativeCodexApplyPatchWebsocketRoundTrip(t *testing.T) {
	const prefix = "*** Begin Patch\n*** Update File: file.txt\n@@\n-旧\n"
	const suffix = "+新 😀\n*** End Patch"
	const patch = prefix + suffix
	const tool = `{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: /./"}}`
	requests := make(chan []byte, 3)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, body, errRead := conn.ReadMessage()
			if errRead != nil {
				return
			}
			requests <- body
			call := calls.Add(1)
			if call == 2 {
				err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-2","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"patched"}]}]}}`))
			} else {
				item := fmt.Sprintf(`{"type":"custom_tool_call","id":"item-1","call_id":"call-patch","name":"apply_patch","input":%q}`, patch)
				events := []string{`{"type":"response.created","response":{"id":"resp-1","model":"gpt-6-astra"}}`}
				if call == 3 {
					events = append(events,
						`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"item-1","call_id":"call-patch","name":"apply_patch","input":""}}`,
						fmt.Sprintf(`{"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"item-1","delta":%q}`, prefix),
						fmt.Sprintf(`{"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"item-1","delta":%q}`, suffix),
						fmt.Sprintf(`{"type":"response.custom_tool_call_input.done","output_index":0,"item_id":"item-1","input":%q}`, patch),
					)
				}
				events = append(events,
					fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":%s}`, item),
					fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","output":[%s]}}`, item),
				)
				for _, event := range events {
					if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
						break
					}
				}
			}
			if err != nil {
				t.Errorf("write event: %v", err)
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	exec := newCodexWebsocketsExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
	sessionID := t.Name()
	t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	auth := &cliproxyauth.Auth{ID: "codex-patch-test", Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Headers:        http.Header{"X-OpenAI-Internal-Codex-Responses-Lite": []string{"true"}},
		Metadata:       map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID},
	}
	payload := fmt.Sprintf(`{"model":"gpt-6-astra","input":"Apply the patch","tools":[%s],"tool_choice":{"type":"custom","name":"apply_patch"}}`, tool)
	request := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(payload)}
	response, err := exec.Execute(ctx, auth, request, opts)
	if err != nil {
		t.Fatal(err)
	}
	first := <-requests
	if gjson.GetBytes(first, "type").String() != "response.create" || gjson.GetBytes(first, "tools.0").Raw != tool || gjson.GetBytes(first, "tool_choice.type").String() != "custom" {
		t.Fatalf("native declaration changed: %s", first)
	}
	if gjson.GetBytes(response.Payload, "output.0.input").String() != patch || gjson.GetBytes(response.Payload, "output.0.call_id").String() != "call-patch" {
		t.Fatalf("native patch response: %s", response.Payload)
	}

	followUp := fmt.Sprintf(`{"model":"gpt-6-astra","input":[{"type":"custom_tool_call","call_id":"call-patch","name":"apply_patch","input":%q},{"type":"custom_tool_call_output","call_id":"call-patch","output":"patched"}],"tools":[%s]}`, patch, tool)
	response, err = exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: request.Model, Payload: []byte(followUp)}, opts)
	if err != nil {
		t.Fatal(err)
	}
	second := <-requests
	if gjson.GetBytes(second, "input.0.type").String() != "custom_tool_call" || gjson.GetBytes(second, "input.0.input").String() != patch || gjson.GetBytes(second, "input.1.call_id").String() != "call-patch" {
		t.Fatalf("native history changed: %s", second)
	}
	if gjson.GetBytes(response.Payload, "output.0.content.0.text").String() != "patched" {
		t.Fatalf("history response: %s", response.Payload)
	}

	stream, err := exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(ctx), auth, request, opts)
	if err != nil {
		t.Fatal(err)
	}
	var deltas strings.Builder
	var done, itemDone, terminal string
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		data := jsonPayload(chunk.Payload)
		switch gjson.GetBytes(data, "type").String() {
		case "response.custom_tool_call_input.delta":
			deltas.WriteString(gjson.GetBytes(data, "delta").String())
		case "response.custom_tool_call_input.done":
			done = gjson.GetBytes(data, "input").String()
		case "response.output_item.done":
			itemDone = gjson.GetBytes(data, "item.input").String()
		case "response.completed":
			terminal = gjson.GetBytes(data, "response.output.0.input").String()
		}
		if chunk.Acknowledge != nil {
			chunk.Acknowledge(nil)
		}
	}
	<-requests
	if deltas.String() != patch || done != patch || itemDone != patch || terminal != patch || calls.Load() != 3 {
		t.Fatalf("delta=%q done=%q item=%q terminal=%q calls=%d", deltas.String(), done, itemDone, terminal, calls.Load())
	}
}

func TestNativeCodexApplyPatchWebsocketFailure(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
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
				if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status":400,"error":{"code":"invalid_tool_schema","message":"patch tool is unsupported"}}`)); err != nil {
					t.Errorf("write: %v", err)
				}
			}))
			defer server.Close()
			exec := newCodexWebsocketsExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
			req := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse}
			if !streaming {
				response, err := exec.Execute(ctx, auth, req, opts)
				if err == nil || !strings.Contains(err.Error(), "invalid_tool_schema") || len(response.Payload) != 0 {
					t.Fatalf("failure response=%s error=%v", response.Payload, err)
				}
				return
			}
			stream, err := exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(ctx), auth, req, opts)
			failed := err != nil && strings.Contains(err.Error(), "invalid_tool_schema")
			if err == nil {
				for chunk := range stream.Chunks {
					if chunk.Err != nil && strings.Contains(chunk.Err.Error(), "invalid_tool_schema") {
						failed = true
					}
					if gjson.GetBytes(jsonPayload(chunk.Payload), "type").String() == "response.completed" {
						t.Fatal("failed tool produced a completed response")
					}
					if chunk.Acknowledge != nil {
						chunk.Acknowledge(nil)
					}
				}
			}
			if !failed {
				t.Fatalf("missing upstream tool failure: %v", err)
			}
		})
	}
}

func TestCodexChatToolSnapshotsAcrossTransports(t *testing.T) {
	for _, transport := range []string{"HTTP", "WebSocket"} {
		for _, kind := range []string{"function_call", "custom_tool_call"} {
			for _, completion := range []string{"arguments done", "item done", "terminal"} {
				for _, conflict := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/%s/conflict=%t", transport, kind, completion, conflict), func(t *testing.T) {
						field, event, name := "arguments", "response.function_call_arguments", "search"
						tool := `{"type":"function","function":{"name":"search","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}`
						prefix, full := `{"q":`, `{"q":"测试"}`
						if kind == "custom_tool_call" {
							field, event, name = "input", "response.custom_tool_call_input", "apply_patch"
							tool = `{"type":"custom","name":"apply_patch","format":{"type":"text"}}`
							prefix, full = "*** Begin Patch\n", "*** Begin Patch\n*** Add File: fixture.txt\n+测试\n*** End Patch"
						}
						snapshot := full
						if conflict {
							snapshot = "different"
						}
						item := fmt.Sprintf(`{"type":%q,"id":"item-fixture","call_id":"call-fixture","name":%q,%q:""}`, kind, name, field)
						events := []string{
							`{"type":"response.created","response":{"id":"response-fixture","model":"gpt-6-astra"}}`,
							`{"type":"response.output_item.added","output_index":0,"item":` + item + `}`,
							fmt.Sprintf(`{"type":%q,"item_id":"item-fixture","output_index":0,"delta":%q}`, event+".delta", prefix),
						}
						item = fmt.Sprintf(`{"type":%q,"id":"item-fixture","call_id":"call-fixture","name":%q,%q:%q}`, kind, name, field, snapshot)
						switch completion {
						case "arguments done":
							events = append(events, fmt.Sprintf(`{"type":%q,"item_id":"item-fixture","output_index":0,%q:%q}`, event+".done", field, snapshot))
							events = append(events, `{"type":"response.output_item.done","output_index":0,"item":`+item+`}`)
						case "item done":
							events = append(events, `{"type":"response.output_item.done","output_index":0,"item":`+item+`}`)
						}
						events = append(events, `{"type":"response.completed","response":{"id":"response-fixture","status":"completed","output":[`+item+`]}}`)
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if transport == "WebSocket" {
								conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
								if err != nil {
									t.Errorf("upgrade: %v", err)
									return
								}
								defer func() { _ = conn.Close() }()
								_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
								if _, _, err = conn.ReadMessage(); err != nil {
									t.Errorf("read request: %v", err)
									return
								}
								for _, value := range events {
									if err = conn.WriteMessage(websocket.TextMessage, []byte(value)); err != nil {
										t.Errorf("write event: %v", err)
										return
									}
								}
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							for _, value := range events {
								if _, err := io.WriteString(w, "data: "+value+"\n\n"); err != nil {
									t.Errorf("write event: %v", err)
									return
								}
							}
						}))
						defer server.Close()
						cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
						execute := newCodexExecutor(cfg).ExecuteStream
						if transport == "WebSocket" {
							exec := newCodexWebsocketsExecutor(cfg)
							defer exec.CloseExecutionSession(t.Name())
							execute = exec.ExecuteStream
						}
						ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
						defer cancel()
						auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{"api_key": "fixture", "base_url": server.URL}}
						request := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"messages":[{"role":"user","content":"fixture"}],"tools":[` + tool + `]}`)}
						opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: t.Name()}}
						stream, err := execute(ctx, auth, request, opts)
						if err != nil {
							t.Fatal(err)
						}
						var arguments strings.Builder
						var streamError error
						var finish string
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								streamError = chunk.Err
							}
							data := jsonPayload(chunk.Payload)
							for _, call := range gjson.GetBytes(data, "choices.0.delta.tool_calls").Array() {
								arguments.WriteString(call.Get("function.arguments").String())
							}
							if reason := gjson.GetBytes(data, "choices.0.finish_reason").String(); reason != "" {
								finish = reason
							}
							if chunk.Acknowledge != nil {
								chunk.Acknowledge(nil)
							}
						}
						if conflict {
							if streamError == nil || statusErrCode(streamError) != http.StatusBadGateway || finish != "" {
								t.Fatalf("冲突快照未终止交付：error=%v finish=%q", streamError, finish)
							}
						} else if streamError != nil || arguments.String() != full || finish != "tool_calls" {
							t.Fatalf("完整工具参数交付失败：arguments=%q finish=%q error=%v", arguments.String(), finish, streamError)
						}
					})
				}
			}
		}
	}
}
