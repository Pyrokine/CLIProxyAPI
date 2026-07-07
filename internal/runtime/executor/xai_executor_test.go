package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

func TestReadXAIResponseBodyRejectsOversizedBody(t *testing.T) {
	_, err := readXAIResponseBody(strings.NewReader("abcde"), 4)
	if err == nil {
		t.Fatal("readXAIResponseBody oversized body error = nil")
	}
	if !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("readXAIResponseBody oversized body error = %v", err)
	}
}

func TestXAIExecutorAppliesThinkingSuffix(t *testing.T) {
	executor := NewXAIExecutor(&config.Config{})
	req := cliproxyexecutor.Request{
		Model:   "grok-4.3(low)",
		Payload: []byte(`{"model":"grok-4.3(low)","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}

	prepared, err := executor.prepareResponsesRequest(req, opts, true)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}

	if got := gjson.GetBytes(prepared.body, "model").String(); got != "grok-4.3" {
		t.Fatalf("model = %q, want %q, body=%s", got, "grok-4.3", prepared.body)
	}
	if got := gjson.GetBytes(prepared.body, "reasoning.effort").String(); got != "low" {
		t.Fatalf("reasoning.effort = %q, want %q, body=%s", got, "low", prepared.body)
	}
}

func TestXAIReasoningTextEventsNormalizeToReasoningSummary(t *testing.T) {
	events := xaiNormalizeReasoningSummaryDataEvents([]byte(`{"type":"response.reasoning_text.done","content_index":2,"text":"internal"}`))
	if len(events) != 2 {
		t.Fatalf("events length = %d, want 2", len(events))
	}
	if got := gjson.GetBytes(events[0], "type").String(); got != "response.reasoning_summary_text.done" {
		t.Fatalf("events[0].type = %q, want response.reasoning_summary_text.done", got)
	}
	if got := gjson.GetBytes(events[0], "summary_index").Int(); got != 2 {
		t.Fatalf("events[0].summary_index = %d, want 2", got)
	}
	if gjson.GetBytes(events[0], "content_index").Exists() {
		t.Fatalf("events[0].content_index should be removed: %s", string(events[0]))
	}
	if got := gjson.GetBytes(events[1], "type").String(); got != "response.reasoning_summary_part.done" {
		t.Fatalf("events[1].type = %q, want response.reasoning_summary_part.done", got)
	}
	if got := gjson.GetBytes(events[1], "part.type").String(); got != "summary_text" {
		t.Fatalf("events[1].part.type = %q, want summary_text", got)
	}
	if got := gjson.GetBytes(events[1], "part.text").String(); got != "internal" {
		t.Fatalf("events[1].part.text = %q, want internal", got)
	}
	if got := string(
		xaiNormalizeReasoningSummaryEventLine(
			[]byte("event: response.reasoning_text.delta"), "",
		),
	); got != "event: response.reasoning_summary_text.delta" {
		t.Fatalf("event line = %q", got)
	}
}

func TestXAIExecutorComposerSessionIsolation(t *testing.T) {
	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Metadata: map[string]any{"access_token": "xai-token"},
	}

	tests := []struct {
		name          string
		model         string
		payload       []byte
		wantGenerated bool
		wantSession   string
	}{
		{
			name:          "composer generates fresh session",
			model:         "grok-composer-2.5-fast",
			payload:       []byte(`{"model":"grok-composer-2.5-fast","input":"hello"}`),
			wantGenerated: true,
		},
		{
			name:    "grok build stays stateless without session",
			model:   "grok-build-0.1",
			payload: []byte(`{"model":"grok-build-0.1","input":"hello"}`),
		},
		{
			name:        "explicit prompt cache key is preserved",
			model:       "grok-composer-2.5-fast",
			payload:     []byte(`{"model":"grok-composer-2.5-fast","prompt_cache_key":"client-session","input":"hello"}`),
			wantSession: "client-session",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				prepared, err := exec.prepareResponsesRequestWithContext(
					context.Background(), cliproxyexecutor.Request{
						Model:   tt.model,
						Payload: tt.payload,
					}, cliproxyexecutor.Options{
						SourceFormat: sdktranslator.FormatOpenAIResponse,
						Stream:       true,
					}, true, sdktranslator.FormatCodex,
				)
				if err != nil {
					t.Fatalf("prepareResponsesRequestWithContext() error = %v", err)
				}

				gotSession := prepared.sessionID
				gotPromptCacheKey := gjson.GetBytes(prepared.body, "prompt_cache_key").String()
				httpReq, errRequest := http.NewRequest(
					http.MethodPost, "https://example.test/responses", bytes.NewReader(prepared.body),
				)
				if errRequest != nil {
					t.Fatalf("NewRequest() error = %v", errRequest)
				}
				applyXAIHeaders(httpReq, auth, "xai-token", true, gotSession)
				gotGrokConvID := httpReq.Header.Get("x-grok-conv-id")

				if tt.wantGenerated {
					if _, errParse := uuid.Parse(gotSession); errParse != nil {
						t.Fatalf("generated sessionID = %q, want UUID; body=%s", gotSession, string(prepared.body))
					}
					if gotPromptCacheKey != gotSession {
						t.Fatalf(
							"prompt_cache_key = %q, want sessionID %q; body=%s", gotPromptCacheKey, gotSession,
							string(prepared.body),
						)
					}
					if gotGrokConvID != gotSession {
						t.Fatalf("x-grok-conv-id = %q, want sessionID %q", gotGrokConvID, gotSession)
					}
					return
				}

				if tt.wantSession != "" {
					if gotSession != tt.wantSession {
						t.Fatalf("sessionID = %q, want %q", gotSession, tt.wantSession)
					}
					if gotPromptCacheKey != tt.wantSession {
						t.Fatalf(
							"prompt_cache_key = %q, want %q; body=%s", gotPromptCacheKey, tt.wantSession,
							string(prepared.body),
						)
					}
					if gotGrokConvID != tt.wantSession {
						t.Fatalf("x-grok-conv-id = %q, want %q", gotGrokConvID, tt.wantSession)
					}
					return
				}

				if gotSession != "" {
					t.Fatalf("sessionID = %q, want empty", gotSession)
				}
				if gotPromptCacheKey != "" {
					t.Fatalf("prompt_cache_key = %q, want empty; body=%s", gotPromptCacheKey, string(prepared.body))
				}
				if gotGrokConvID != "" {
					t.Fatalf("x-grok-conv-id = %q, want empty", gotGrokConvID)
				}
			},
		)
	}
}

func TestXAIExecutorCompactUsesCompactEndpoint(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotAccept string
	var gotBody []byte

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotAccept = r.Header.Get("Accept")
				var errRead error
				gotBody, errRead = io.ReadAll(r.Body)
				if errRead != nil {
					t.Fatalf("read body: %v", errRead)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque-out"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
			},
		),
	)
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "xai-token",
		},
	}

	resp, err := exec.Execute(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "grok-4.3",
			Payload: []byte(`{"model":"grok-4.3","stream":true,"input":[{"type":"compaction","encrypted_content":"opaque-in"},{"role":"user","content":"hello"}]}`),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse,
			Alt:          "responses/compact",
			Stream:       false,
		},
	)
	if err != nil {
		t.Fatalf("Execute compact error: %v", err)
	}
	if gotPath != "/responses/compact" {
		t.Fatalf("path = %q, want /responses/compact", gotPath)
	}
	if gotAuth != "Bearer xai-token" {
		t.Fatalf("Authorization = %q, want Bearer xai-token", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept = %q, want application/json", gotAccept)
	}
	if gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("stream exists in compact body: %s", string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "input.0.role").String(); got != "user" {
		t.Fatalf("invalid compaction item reached xai compact body: %s", string(gotBody))
	}
	want := `{"id":"resp_1","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"opaque-out"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`
	if string(resp.Payload) != want {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
}

func TestXAIExecutorExecuteStreamCompactionTriggerUsesCompactEndpoint(t *testing.T) {
	var gotPath string
	var gotAccept string
	var gotBody []byte

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAccept = r.Header.Get("Accept")
				var errRead error
				gotBody, errRead = io.ReadAll(r.Body)
				if errRead != nil {
					t.Fatalf("read body: %v", errRead)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_xai_1","model":"grok-4.3","output":[{"type":"compaction","encrypted_content":"opaque"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
			},
		),
	)
	defer server.Close()

	exec := NewXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"base_url": server.URL,
			"api_key":  "xai-token",
		},
	}

	result, err := exec.ExecuteStream(
		context.Background(), auth, cliproxyexecutor.Request{
			Model:   "grok-4.3",
			Payload: []byte(`{"model":"grok-4.3","stream":true,"input":[{"role":"user","content":"hello"},{"type":"compaction_trigger"}]}`),
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse,
			Stream:       true,
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream compaction trigger error: %v", err)
	}
	if gotPath != "/responses/compact" {
		t.Fatalf("path = %q, want /responses/compact", gotPath)
	}
	if gotAccept != "application/json" {
		t.Fatalf("Accept = %q, want application/json", gotAccept)
	}
	if xaiInputHasItemType(gotBody, "compaction_trigger") {
		t.Fatalf("compaction_trigger reached xai compact body: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("stream exists in compact body: %s", string(gotBody))
	}

	var streamed bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}
	output := streamed.String()
	for _, eventName := range []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.output_item.done",
		"response.completed",
	} {
		if !strings.Contains(output, "event: "+eventName+"\n") {
			t.Fatalf("missing %s event in stream: %s", eventName, output)
		}
	}
	if !strings.Contains(output, `"type":"compaction"`) || !strings.Contains(output, `"encrypted_content":"opaque"`) {
		t.Fatalf("compaction output missing from stream: %s", output)
	}
	if !strings.Contains(output, `"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}`) {
		t.Fatalf("usage missing from completed stream: %s", output)
	}
}
