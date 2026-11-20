package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestAuditXAIResponseIDsPreserveBusinessJSON(t *testing.T) {
	//goland:noinspection SpellCheckingInspection
	const input = `{"id":"resp-up","response_id":"resp-up","previous_response_id":"prev-up","model":"resp-up-` +
		`finetune","delta":"literal resp-up and prev-up","number":9007199254740993,"custom":["resp-up",{"id":` +
		`"resp-up"}],"response":{"id":"resp-up","previous_response_id":"prev-up","model":"resp-up-finetune","` +
		`metadata":{"id":"resp-up","previous_response_id":"prev-up"},"output":[{"type":"function_call","id":"` +
		`fc_resp-up","call_id":"call_resp-up","arguments":"{\"response_id\":\"resp-up\"}"}]}}`
	out := rewriteXAIWebsocketDownstreamIDs([]byte(input), "resp-up", "resp-down", "prev-up", "prev-down")
	for path, expected := range map[string]string{
		"id": "resp-down", "response_id": "resp-down", "previous_response_id": "prev-down", "response.id": "resp-down",
		"response.previous_response_id": "prev-down", "response.output.0.id": "fc_resp-down",
		"response.output.0.call_id": "call_resp-down",
	} {
		if got := gjson.GetBytes(out, path).String(); got != expected {
			t.Fatalf("%s=%q want=%q payload=%s", path, got, expected, out)
		}
	}
	for _, path := range []string{
		"model", "delta", "number", "custom", "response.model", "response.metadata", "response.output.0.arguments",
	} {
		if gjson.GetBytes(out, path).Raw != gjson.Get(input, path).Raw {
			t.Fatalf("business value changed at %s: %s", path, out)
		}
	}
}

func TestAuditXAIWebsocketFourTurnStructuredIdentity(t *testing.T) {
	const text = "literal resp-up and prev-up"
	const args = `{"value":"resp-up and prev-up"}`
	requests := make(chan []byte, 4)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
				for range 4 {
					_, body, err := peer.ReadMessage()
					if err != nil {
						t.Error(err)
						return
					}
					requests <- body
					previous := gjson.GetBytes(body, "previous_response_id").String()
					frames := []string{
						`{"type":"response.created","response":{"id":"resp-up","model":` +
							`"resp-up-finetune","output":` +
							`[]}}`,
						fmt.Sprintf(
							`{"type":"response.output_text.delta","response_id":"resp-up",`+
								`"item_id":"msg_resp-up","delta":`+
								`%q}`,
							text,
						),
						fmt.Sprintf(
							`{"type":"response.completed","response":{"id":"resp-up","previous_response_id":%`+
								`q,"status":"completed","model":"resp-up-finetune","metadata":{"id":"resp-up"`+
								`},"output":[{"type":"message","id":"msg_resp-up","role":"assistant","`+
								`content":[{"type":"output_text","text":%q}]},{"type":"function_call","id":"`+
								`fc_resp-up","call_id":"call-1","name":"lookup","arguments":%q}],"usage":{"`+
								`input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
							previous, text, args,
						),
					}
					for _, frame := range frames {
						if err = peer.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
							t.Error(err)
							return
						}
					}
				}
				_, _, _ = peer.ReadMessage()
			},
		),
	)
	defer server.Close()
	executor := newXAIWebsocketsExecutor(&config.Config{})
	executor.store = &xaiWebsocketSessionStore{sessions: make(map[string]*codexWebsocketSession)}
	executor.idStore = &xaiWebsocketIDStateStore{sessions: make(map[string]*xaiWebsocketIDState)}
	defer executor.CloseExecutionSession(t.Name())
	credential := &auth.Auth{
		ID: t.Name(), Provider: "xai", Attributes: map[string]string{
			"api_key":  "synthetic-key",
			"base_url": server.URL,
		},
	}
	opts := core.Options{
		SourceFormat: translator.FormatOpenAIResponse, ResponseFormat: translator.FormatOpenAIResponse,
		Metadata: map[string]any{core.ExecutionSessionMetadataKey: t.Name()},
	}
	ctx, cancel := context.WithTimeout(
		core.WithStreamDeliveryAcknowledgement(
			core.WithDownstreamWebsocket(context.Background()),
		),
		5*time.Second,
	)
	defer cancel()
	previous := ""
	seen := make(map[string]bool)
	for turn := range 4 {
		request := core.Request{
			Model: "grok-4.3", Payload: []byte(fmt.Sprintf(
				`{"model":"grok-4.3","previous_response_id":%q,"input":[{"type":"message","role":"user","`+
					`content":"synthetic"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":`+
					`"object"}}]}`,
				previous,
			)),
		}
		stream, err := executor.ExecuteStream(ctx, credential, request, opts)
		if err != nil {
			t.Fatal(err)
		}
		current, sawDelta, sawCompleted := "", false, false
		for chunk := range stream.Chunks {
			if chunk.Acknowledge != nil {
				chunk.Acknowledge(nil)
			}
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
			if len(bytes.TrimSpace(chunk.Payload)) == 0 {
				continue
			}
			root := gjson.ParseBytes(chunk.Payload)
			if id := root.Get("response.id").String(); id != "" {
				if current != "" && current != id {
					t.Fatalf(
						"turn %d changed response ID between lifecycle events: %s -> %s",
						turn,
						current,
						id,
					)
				}
				current = id
			}
			if model := root.Get("response.model").String(); model != "" && model != "resp-up-finetune" {
				t.Fatalf("model changed: %s", chunk.Payload)
			}
			switch root.Get("type").String() {
			case "response.output_text.delta":
				sawDelta = true
				if root.Get("response_id").String() != current ||
					root.Get("item_id").String() != "msg_"+current ||
					root.Get("delta").String() != text {
					t.Fatalf("delta ID/text changed: %s", chunk.Payload)
				}
			case "response.completed":
				sawCompleted = true
				if root.Get("response.previous_response_id").String() != previous ||
					root.Get("response.output.0.content.0.text").String() != text ||
					root.Get("response.output.1.arguments").String() != args ||
					root.Get("response.metadata.id").String() != "resp-up" {
					t.Fatalf("response IDs or business content changed: %s", chunk.Payload)
				}
			}
		}
		wire := <-requests
		wantPrevious := "resp-up"
		if turn == 0 {
			wantPrevious = ""
		}
		if got := gjson.GetBytes(wire, "previous_response_id").String(); got != wantPrevious {
			t.Fatalf("turn %d upstream previous=%q want=%q", turn, got, wantPrevious)
		}
		if current == "" || seen[current] || !sawDelta || !sawCompleted {
			t.Fatalf(
				"turn=%d id=%q repeated=%t delta=%t completed=%t",
				turn,
				current,
				seen[current],
				sawDelta,
				sawCompleted,
			)
		}
		seen[current] = true
		if got := executor.idStore.get(t.Name()).upstreamIDForDownstream(current); got != "resp-up" {
			t.Fatalf("turn %d stored mapping=%q", turn, got)
		}
		previous = current
	}
}
