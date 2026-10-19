package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestXAIApplyPatchTransportSentinel(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.done", "response.incomplete"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", terminal, streaming), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: {\"type\":%q,\"response\":{\"id\":\"resp-1\",\"output\":[{\"type\":\"function_call\",\"id\":\"item-1\",\"call_id\":\"call-1\",\"name\":\"apply_patch\",\"arguments\":\"{\\\"input\\\":\\\"patch\\\"}\"}]}}\n\ndata: [DONE]\n\n", terminal)
				}))
				t.Cleanup(server.Close)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				exec := newXAIExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
				req := cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(`{"input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse}
				if !streaming {
					response, err := exec.Execute(ctx, auth, req, opts)
					if err != nil || gjson.GetBytes(response.Payload, "output.0.input").String() != "patch" {
						t.Fatalf("legitimate source terminal failed: response=%s error=%v", response.Payload, err)
					}
					return
				}
				stream, err := exec.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				completed := 0
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatalf("legitimate source sentinel failed: %v", chunk.Err)
					}
					kind := gjson.GetBytes(jsonPayload(chunk.Payload), "type").String()
					if kind == "response.completed" || kind == "response.incomplete" {
						completed++
					}
					if chunk.Acknowledge != nil {
						chunk.Acknowledge(nil)
					}
				}
				if completed != 1 {
					t.Fatalf("source completion count=%d", completed)
				}
			})
		}
	}
}

func TestXAIApplyPatchSourceClosureAndSentinelLifecycle(t *testing.T) {
	normalizer := xaiPatchNamespaceNormalizer(t)
	const terminal = `{"type":"response.completed","response":{"output":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"editor","arguments":"{\"name\":\"apply_patch\",\"arguments\":{\"input\":\"patch\"}}"}]}}`
	if events := normalizer.normalize([]byte(terminal)); len(events) == 0 || gjson.GetBytes(events[len(events)-1], "type").String() != "response.completed" {
		t.Fatalf("source completion failed: %q", events)
	}
	if normalizer.patch != nil || len(normalizer.patchDispatcherCall) != 0 || normalizer.identity != nil || normalizer.dispatcher != nil {
		t.Fatal("closed source retained request-level decoder or identity state")
	}
	if events := normalizer.normalize([]byte(`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"item-1","call_id":"changed","name":"editor"}}`)); len(events) != 0 {
		t.Fatalf("closed source was reopened: %q", events)
	}
	sentinel := []byte("  [DONE]  ")
	if events := normalizer.normalize(sentinel); len(events) != 1 || !bytes.Equal(events[0], sentinel) {
		t.Fatalf("first legitimate transport sentinel changed: %q", events)
	}
	for _, event := range [][]byte{sentinel, []byte(terminal)} {
		if events := normalizer.normalize(event); len(events) != 0 {
			t.Fatalf("closed source was reopened: %q", events)
		}
	}
	failure := []byte(`{"type":"response.failed","response":{"error":{"message":"late failure"}}}`)
	if events := normalizer.normalize(failure); len(events) != 1 || !bytes.Equal(events[0], failure) {
		t.Fatalf("late transport failure was hidden: %q", events)
	}
	if events := normalizer.normalize(failure); len(events) != 0 {
		t.Fatalf("transport failure repeated: %q", events)
	}
}

func TestXAIApplyPatchMixedCompletedOutput(t *testing.T) {
	const patch = `{"type":"function_call","id":"patch-0","call_id":"call-0","name":"apply_patch","arguments":"{\"input\":\"patch\"}"}`
	const message = `{"type":"message","id":"message-1","role":"assistant","content":[{"type":"output_text","text":"earlier"}],"opaque":{"keep":true}}`
	const lookup = `{"type":"function_call","id":"lookup-2","call_id":"call-2","name":"lookup","arguments":"{\"key\":\"value\"}"}`
	const reasoning = `{"type":"reasoning","id":"reasoning-3","summary":[{"type":"summary_text","text":"reason"}]}`
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for index, item := range []string{patch, message, lookup, reasoning} {
					_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", index, item)
				}
				_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"output\":[{\"type\":\"message\",\"id\":\"message-1\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"terminal\"}],\"opaque\":{\"keep\":true}}]}}\n\ndata: [DONE]\n\n")
			}))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			exec := newXAIExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
			req := cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(`{"input":"patch","tools":[{"type":"custom","name":"apply_patch"},{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse}
			var output gjson.Result
			if !streaming {
				response, err := exec.Execute(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				output = gjson.GetBytes(response.Payload, "output")
			} else {
				stream, err := exec.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					data := jsonPayload(chunk.Payload)
					if gjson.GetBytes(data, "type").String() == "response.completed" {
						output = gjson.GetBytes(data, "response.output")
					}
					if chunk.Acknowledge != nil {
						chunk.Acknowledge(nil)
					}
				}
			}
			items := output.Array()
			if len(items) != 4 {
				t.Fatalf("mixed completed output lost source items: %s", output.Raw)
			}
			for index, id := range []string{"patch-0", "message-1", "lookup-2", "reasoning-3"} {
				if items[index].Get("id").String() != id {
					t.Fatalf("source order changed: %s", output.Raw)
				}
			}
			if items[0].Get("input").String() != "patch" || items[0].Get("arguments").Exists() || items[1].Get("content.0.text").String() != "terminal" || !items[1].Get("opaque.keep").Bool() || items[2].Get("arguments").String() != `{"key":"value"}` || items[3].Get("summary.0.text").String() != "reason" {
				t.Fatalf("mixed completed content changed: %s", output.Raw)
			}
		})
	}
}

func TestXAIPartialCompletedOutputIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		observed map[int64][]byte
		terminal string
		wantIDs  []string
	}{
		{
			name: "complete output preserves source order",
			observed: map[int64][]byte{
				0: []byte(`{"id":"first","type":"message"}`),
				1: []byte(`{"id":"second","type":"message"}`),
			},
			terminal: `{"type":"response.completed","response":{"output":[{"id":"second","type":"message"},{"id":"first","type":"message"}]}}`,
			wantIDs:  []string{"second", "first"},
		},
		{
			name: "late item ID retains call identity",
			observed: map[int64][]byte{
				0: []byte(`{"id":"first","call_id":"call-1","type":"function_call","arguments":"{}"}`),
				1: []byte(`{"id":"second","type":"message"}`),
			},
			terminal: `{"type":"response.completed","response":{"output":[{"call_id":"call-1","type":"function_call","arguments":"{\"new\":true}"}]}}`,
			wantIDs:  []string{"", "second"},
		},
		{
			name:     "unidentified source cannot replace terminal identity",
			observed: map[int64][]byte{0: []byte(`{"type":"message","content":[{"type":"output_text","text":"earlier"}]}`)},
			terminal: `{"type":"response.completed","response":{"output":[{"id":"terminal","type":"message","content":[{"type":"output_text","text":"new"}]}]}}`,
			wantIDs:  []string{"terminal"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := xaiPatchCompletedOutput([]byte(tc.terminal), tc.observed, nil)
			items := gjson.GetBytes(out, "response.output").Array()
			if len(items) != len(tc.wantIDs) {
				t.Fatalf("output identity duplicated or lost: %s", out)
			}
			for index, id := range tc.wantIDs {
				if items[index].Get("id").String() != id {
					t.Fatalf("output identity or order changed: %s", out)
				}
			}
			if tc.name == "late item ID retains call identity" && items[0].Get("arguments").String() != `{"new":true}` {
				t.Fatalf("terminal snapshot was replaced by an old one: %s", out)
			}
		})
	}
}

func TestXAIApplyPatchHTTPReplayRequiresSuccessfulTransport(t *testing.T) {
	for _, outcome := range []string{"success", "delivery failure", "cancellation", "read failure", "late provider failure", "incomplete", "premature sentinel"} {
		t.Run(outcome, func(t *testing.T) {
			internalcache.ClearXAIReasoningReplayCache()
			t.Cleanup(internalcache.ClearXAIReasoningReplayCache)
			ctx, cancel := context.WithTimeout(cliproxyexecutor.WithStreamDeliveryAcknowledgement(xaiC03ContextWithAPIKey("patch-http-ack")), 5*time.Second)
			defer cancel()
			terminalType := "response.completed"
			if outcome == "incomplete" {
				terminalType = "response.incomplete"
			}
			terminal := fmt.Sprintf(`{"type":%q,"response":{"id":"resp-1","output":[{"type":"function_call","id":"item-1","call_id":"call-1","name":"apply_patch","arguments":"{\"input\":\"patch\"}"}]}}`, terminalType)
			body := "data: " + terminal + "\n\ndata: [DONE]\n\n"
			if outcome == "premature sentinel" {
				body = "data: [DONE]\n\n" + body
			} else if outcome == "late provider failure" {
				body += "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"late failure\"}}}\n\n"
			}
			ctx = util.WithRoundTripper(ctx, xaiMediaRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "xai.test" {
					return nil, fmt.Errorf("unexpected host %s", req.URL.Host)
				}
				var reader io.Reader = strings.NewReader(body)
				if outcome == "read failure" {
					reader = io.MultiReader(reader, xaiMediaReadErrorBody{})
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(reader), Request: req}, nil
			}))
			auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "offline-fixture", "base_url": "https://xai.test"}}
			req := cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(`{"prompt_cache_key":"patch-http-ack","input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse}
			scope := xaiReasoningReplayScopeFromRequest(ctx, auth, opts.SourceFormat, req, opts, req.Payload)
			previous := []byte(`{"type":"function_call","call_id":"previous-call","name":"lookup","arguments":"{}"}`)
			if status := internalcache.StoreXAIReasoningReplayItems(ctx, scope.modelName, scope.sessionKey, [][]byte{previous}); status != internalcache.XAIReasoningReplayStored {
				t.Fatalf("cannot seed replay: %v", status)
			}
			stream, err := newXAIExecutor(&config.Config{}).ExecuteStream(ctx, auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			var streamErr error
			acknowledgements := 0
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					streamErr = chunk.Err
				}
				if chunk.Terminal && chunk.Acknowledge != nil {
					acknowledgements++
					items, stored := internalcache.GetXAIReasoningReplayItems(scope.modelName, scope.sessionKey)
					if !stored || len(items) != 1 || gjson.GetBytes(items[0], "call_id").String() != "previous-call" {
						t.Fatalf("replay replaced before transport acknowledgement: %q", items)
					}
					var deliveryErr error
					if outcome == "delivery failure" {
						deliveryErr = errors.New("fixture final write failed")
					} else if outcome == "cancellation" {
						cancel()
					}
					chunk.Acknowledge(deliveryErr)
				}
			}
			wantError := outcome == "read failure" || outcome == "late provider failure" || outcome == "premature sentinel"
			if (streamErr != nil) != wantError {
				t.Fatalf("transport error=%v, wantError=%t", streamErr, wantError)
			}
			wantAcknowledgements := 1
			if wantError || outcome == "incomplete" {
				wantAcknowledgements = 0
			}
			if acknowledgements != wantAcknowledgements {
				t.Fatalf("acknowledgements=%d, want=%d", acknowledgements, wantAcknowledgements)
			}
			items, stored := internalcache.GetXAIReasoningReplayItems(scope.modelName, scope.sessionKey)
			wantCall := "previous-call"
			if outcome == "success" {
				wantCall = "call-1"
			}
			if !stored || len(items) != 1 || gjson.GetBytes(items[0], "call_id").String() != wantCall {
				t.Fatalf("replay after transport: stored=%t items=%q want=%s", stored, items, wantCall)
			}
		})
	}
}

func TestXAIApplyPatchWebsocketCompletionKeepsConnection(t *testing.T) {
	for _, terminal := range []string{"response.completed", "response.done", "response.incomplete"} {
		t.Run(terminal, func(t *testing.T) {
			var connections atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				connections.Add(1)
				defer func() { _ = conn.Close() }()
				for turn := 0; ; turn++ {
					if _, _, err = conn.ReadMessage(); err != nil {
						return
					}
					event := fmt.Sprintf(`{"type":%q,"response":{"id":"resp-%d","output":[{"type":"function_call","id":"item-%d","call_id":"call-%d","name":"apply_patch","arguments":"{\"input\":\"patch\"}"}]}}`, terminal, turn, turn, turn)
					if err = conn.WriteMessage(websocket.TextMessage, []byte(event)); err != nil {
						return
					}
				}
			}))
			t.Cleanup(server.Close)
			exec := newXAIWebsocketsExecutor(&config.Config{})
			sessionID := t.Name()
			t.Cleanup(func() { exec.CloseExecutionSession(sessionID) })
			ctx, cancel := context.WithTimeout(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), 5*time.Second)
			defer cancel()
			auth := &cliproxyauth.Auth{ID: sessionID, Provider: "xai", Attributes: map[string]string{"base_url": server.URL, "websockets": "true"}, Metadata: map[string]any{"access_token": "test"}}
			req := cliproxyexecutor.Request{Model: "grok-4.7", Payload: []byte(`{"input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: sessionID}}
			for turn := 0; turn < 2; turn++ {
				stream, err := exec.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				completed := 0
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					data := jsonPayload(chunk.Payload)
					kind := gjson.GetBytes(data, "type").String()
					if kind == "response.completed" || kind == "response.incomplete" {
						completed++
						if gjson.GetBytes(data, "response.output.0.input").String() != "patch" {
							t.Fatalf("websocket patch changed: %s", data)
						}
					}
					if chunk.Acknowledge != nil {
						chunk.Acknowledge(nil)
					}
				}
				if completed != 1 || ctx.Err() != nil {
					t.Fatalf("request waited for connection EOF: completions=%d error=%v", completed, ctx.Err())
				}
			}
			if connections.Load() != 1 {
				t.Fatalf("request completion closed the reusable connection: connections=%d", connections.Load())
			}
		})
	}
}

func TestXAIApplyPatchPrematureSentinelDoesNotCompleteInput(t *testing.T) {
	normalizer := xaiPatchNamespaceNormalizer(t)
	for attempt := 0; attempt < 3; attempt++ {
		events := normalizer.normalize([]byte("[DONE]"))
		if attempt == 0 {
			if len(events) != 1 || gjson.GetBytes(events[0], "type").String() != "response.failed" {
				t.Fatalf("premature transport sentinel was accepted: %q", events)
			}
		} else if len(events) != 0 {
			t.Fatalf("transport failure repeated: %q", events)
		}
	}
}
