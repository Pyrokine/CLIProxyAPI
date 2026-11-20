package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func assertOriginalCodexInputIDs(t *testing.T, body []byte, longCallID, longOutputID string) {
	t.Helper()
	input := gjson.GetBytes(body, "input").Array()
	if len(input) != 3 || input[0].Get("type").String() != "function_call" {
		t.Fatalf("upstream input = %s, want call/output/message without encrypted reasoning", body)
	}
	for index, original := range []string{longCallID, longOutputID} {
		id := input[index].Get("id").String()
		if id == original || id == "" || len([]rune(id)) > 64 {
			t.Fatalf("input.%d.id = %q, want shortened ID within 64 characters", index, id)
		}
		if input[index].Get("call_id").String() != "call-1" {
			t.Fatalf("input.%d.call_id changed: %s", index, body)
		}
	}
	if input[0].Get("id").String() == input[1].Get("id").String() {
		t.Fatalf("distinct long IDs produced the same shortened ID: %s", body)
	}
	if input[2].Get("id").String() != "msg-1" {
		t.Fatalf("valid message ID changed: %s", body)
	}
}

func TestCodexExecutorExecuteStreamSanitizesOverlongInputItemIDs(t *testing.T) {
	longReasoningID := "rs_" + strings.Repeat("a", 64)
	longCallID := strings.Repeat("grok-call-item-", 6)
	longOutputID := strings.Repeat("grok-output-item-", 6)
	encrypted := validOpenAIResponsesReasoningEncryptedContentForTest()
	received := make(chan []byte, 1)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Errorf("read request: %v", err)
					return
				}
				received <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(
					[]byte("data: " + `{"type":"response.completed","response":{"id":"resp_1","output":[],` +
						`"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}` + "\n\n"),
				)
			},
		),
	)
	defer server.Close()
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":[` +
		`{"type":"reasoning","id":"` + longReasoningID + `","encrypted_content":"` + encrypted + `","summary":[]},` +
		`{"type":"function_call","id":"` + longCallID + `","call_id":"call-1","name":"lookup","arguments":"{}"},` +
		`{"type":"function_call_output","id":"` + longOutputID + `","call_id":"call-1","output":"ok"},` +
		`{"type":"message","id":"msg-1","role":"user","content":"continue"}]}`)
	before := bytes.Clone(body)
	implementation := newCodexExecutor(&config.Config{DisableImageGeneration: config.DisableImageGenerationAll})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := implementation.ExecuteStream(
		ctx,
		&coreauth.Auth{
			ProxyURL:   "none",
			Attributes: map[string]string{"base_url": server.URL, "api_key": "test"},
		},
		core.Request{Model: "gpt-5.4", Payload: body},
		core.Options{SourceFormat: translator.FormatOpenAIResponse, Stream: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
	select {
	case sent := <-received:
		assertOriginalCodexInputIDs(t, sent, longCallID, longOutputID)
	case <-ctx.Done():
		t.Fatal("upstream request was not received")
	}
	if !bytes.Equal(body, before) {
		t.Fatal("caller request changed")
	}
}

func TestCodexInputIDsDropOverlongEncryptedReasoningAcrossHTTPModes(t *testing.T) {
	for _, mode := range []string{"execute", "stream", "compact"} {
		t.Run(
			mode, func(t *testing.T) {
				longCallID := strings.Repeat("grok-call-item-", 6)
				longOutputID := strings.Repeat("grok-output-item-", 6)
				body := []byte(
					`{"model":"gpt-5.4","input":[{"type":"reasoning","id":"rs_` + strings.Repeat("a", 64) +
						(`","encrypted_content":"gAAAA-encrypted","summary":[]},{"type":` +
							`"function_call","id":` +
							`"`) +
						longCallID +
						`","call_id":"call-1","name":"lookup"},{"type":"function_call_output","id":"` + longOutputID +
						`","call_id":"call-1","output":"ok"},{"type":"message","id":"msg-1"}]}`,
				)
				received := make(chan []byte, 1)
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, request *http.Request) {
							payload, err := io.ReadAll(request.Body)
							if err != nil {
								t.Errorf("read upstream request: %v", err)
								return
							}
							received <- payload
							response := `{"id":"resp_1","object":"response.compaction","output":[],` +
								`"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`
							if mode == "compact" {
								w.Header().Set("Content-Type", "application/json")
								_, _ = io.WriteString(w, response)
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(
								w, "data: "+`{"type":"response.completed","response":`+response+"}\n\n",
							)
						},
					),
				)
				defer server.Close()
				implementation := newCodexExecutor(
					&config.Config{DisableImageGeneration: config.DisableImageGenerationAll},
				)
				credential := &coreauth.Auth{
					ProxyURL:   "none",
					Attributes: map[string]string{"api_key": "test", "base_url": server.URL},
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				request := core.Request{Model: "gpt-5.4", Payload: body}
				opts := core.Options{SourceFormat: translator.FormatCodex}
				if mode == "stream" {
					result, err := implementation.ExecuteStream(ctx, credential, request, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else {
					if mode == "compact" {
						opts.Alt = "responses/compact"
					}
					if _, err := implementation.Execute(ctx, credential, request, opts); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case sent := <-received:
					assertOriginalCodexInputIDs(t, sent, longCallID, longOutputID)
				case <-ctx.Done():
					t.Fatal("upstream request was not received")
				}
			},
		)
	}
}

func TestXAIWebsocketFrameKeepsInputIDNormalizationProviderScoped(t *testing.T) {
	longCallID := strings.Repeat("grok-call-item-", 6)
	longOutputID := strings.Repeat("grok-output-item-", 6)
	body := []byte(`{"model":"grok-4.3","input":[{"type":"function_call","id":"` + longCallID +
		`","call_id":"call-1","name":"lookup","arguments":"{}"},{"type":"function_call_output","id":"` +
		longOutputID + `","call_id":"call-1","output":"ok"}]}`)
	implementation := newXAIWebsocketsExecutor(&config.Config{})
	prepared, err := implementation.prepareResponsesWebsocketRequest(
		t.Context(),
		&coreauth.Auth{
			Attributes: map[string]string{
				"api_key": "synthetic-key", "base_url": "https://fixture.invalid",
			},
		},
		core.Request{Model: "grok-4.3", Payload: body}, core.Options{SourceFormat: translator.FormatCodex},
	)
	if err != nil {
		t.Fatal(err)
	}
	frame := buildCodexWebsocketRequestBody(prepared.body)
	for index, want := range []string{longCallID, longOutputID} {
		if got := gjson.GetBytes(frame, "input").Array()[index].Get("id").String(); got != want {
			t.Fatalf("xAI input.%d.id = %q, want original %q", index, got, want)
		}
	}
}

func TestBuildCodexWebsocketRequestBodySanitizesOverlongInputItemIDs(t *testing.T) {
	longReasoningID := "rs_" + strings.Repeat("a", 64)
	longCallID := strings.Repeat("grok-call-item-", 6)
	longOutputID := strings.Repeat("grok-output-item-", 6)
	body := []byte(`{"model":"gpt-5-codex","input":[{"type":"reasoning","id":"` + longReasoningID +
		`","encrypted_content":"gAAAA-encrypted","summary":[]},{"type":"function_call","id":"` + longCallID +
		`","call_id":"call-1","name":"lookup"},{"type":"function_call_output","id":"` + longOutputID +
		`","call_id":"call-1","output":"ok"},{"type":"message","id":"msg-1"}]}`)
	before := bytes.Clone(body)
	received := make(chan []byte, 2)
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, request *http.Request) {
				upgrader := websocket.Upgrader{}
				conn, err := upgrader.Upgrade(w, request, nil)
				if err != nil {
					t.Errorf("upgrade: %v", err)
					return
				}
				defer func() { _ = conn.Close() }()
				_, payload, err := conn.ReadMessage()
				if err != nil {
					t.Errorf("read upstream frame: %v", err)
					return
				}
				received <- payload
				if err := conn.WriteMessage(
					websocket.TextMessage,
					[]byte(`{"type":"response.completed",`+
						(`"response":{"id":"resp_1","output":[],"usage":{"input_tokens":0,`+
							`"output_tokens":0,"total_tokens":`+
							`0}}}`)),
				); err != nil {
					t.Errorf("write completed: %v", err)
				}
			},
		),
	)
	defer server.Close()
	implementation := newCodexWebsocketsExecutor(
		&config.Config{DisableImageGeneration: config.DisableImageGenerationAll},
	)
	credential := &coreauth.Auth{
		ProxyURL:   "none",
		Attributes: map[string]string{"base_url": server.URL, "api_key": "test"},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var first []byte
	for attempt := range 2 {
		result, err := implementation.ExecuteStream(
			ctx, credential, core.Request{Model: "gpt-5-codex", Payload: body},
			core.Options{SourceFormat: translator.FormatCodex, Stream: true},
		)
		if err != nil {
			t.Fatal(err)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
		}
		select {
		case sent := <-received:
			assertOriginalCodexInputIDs(t, sent, longCallID, longOutputID)
			if gjson.GetBytes(sent, "type").String() != "response.create" {
				t.Fatalf("upstream frame type changed: %s", sent)
			}
			if attempt == 0 {
				first = sent
			} else {
				for _, path := range []string{"input.0.id", "input.1.id"} {
					if gjson.GetBytes(sent, path).String() != gjson.GetBytes(first, path).String() {
						t.Fatalf("input ID normalization is not deterministic: %s versus %s", first, sent)
					}
				}
			}
		case <-ctx.Done():
			t.Fatal("upstream request was not received")
		}
	}
	if !bytes.Equal(body, before) {
		t.Fatal("caller request changed")
	}
}
