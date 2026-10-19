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
	"testing"
	"time"

	xaiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/xai"
	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	multiagentv2 "github.com/Pyrokine/CLIProxyAPI/v6/internal/client/codex/optimize-multi-agent-v2"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type xaiMediaRoundTripper func(*http.Request) (*http.Response, error)

func (f xaiMediaRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type xaiMediaReadErrorBody struct{}

func (xaiMediaReadErrorBody) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (xaiMediaReadErrorBody) Close() error             { return nil }

func waitForXAIUsageRecord(t *testing.T, records <-chan usage.Record) usage.Record {
	t.Helper()
	select {
	case record := <-records:
		select {
		case duplicate := <-records:
			t.Fatalf("duplicate usage record: %#v", duplicate)
		case <-time.After(25 * time.Millisecond):
		}
		return record
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for xAI usage record")
		return usage.Record{}
	}
}

func TestXAIImageUsagePublishesExactlyOnce(t *testing.T) {
	tests := []struct {
		name       string
		ctx        func() context.Context
		baseURL    string
		transport  xaiMediaRoundTripper
		wantFailed bool
	}{
		{
			name:    "success",
			baseURL: "https://xai.test",
			transport: func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(`{"data":[]}`)),
				}, nil
			},
		},
		{
			name:       "request build failure",
			baseURL:    "://invalid",
			wantFailed: true,
		},
		{
			name:    "dial failure",
			baseURL: "https://xai.test",
			transport: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed")
			},
			wantFailed: true,
		},
		{
			name:    "body read failure",
			baseURL: "https://xai.test",
			transport: func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK, Header: make(http.Header), Body: xaiMediaReadErrorBody{},
				}, nil
			},
			wantFailed: true,
		},
		{
			name:    "non-2xx",
			baseURL: "https://xai.test",
			transport: func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusBadGateway, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(`{"error":"upstream"}`)),
				}, nil
			},
			wantFailed: true,
		},
		{
			name: "cancellation",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			baseURL: "https://xai.test",
			transport: func(req *http.Request) (*http.Response, error) {
				return nil, req.Context().Err()
			},
			wantFailed: true,
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
				registerTestUsagePlugin(t, plugin)
				ctx := context.Background()
				if tt.ctx != nil {
					ctx = tt.ctx()
				}
				if tt.transport != nil {
					ctx = util.WithRoundTripper(ctx, tt.transport)
				}
				exec := newXAIExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{
					Provider: "xai", Attributes: map[string]string{"base_url": tt.baseURL, "api_key": "test-key"},
				}
				_, errExecute := exec.Execute(
					ctx, auth, cliproxyexecutor.Request{
						Model: "grok-imagine", Payload: []byte(`{"model":"grok-imagine","prompt":"draw"}`),
					}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(xaiImageHandlerType)},
				)
				if tt.wantFailed != (errExecute != nil) {
					t.Fatalf("Execute() error = %v, wantFailed = %t", errExecute, tt.wantFailed)
				}
				record := waitForXAIUsageRecord(t, plugin.records)
				if record.Failed != tt.wantFailed || record.Model != "grok-imagine" {
					t.Fatalf("usage record = %#v", record)
				}
			},
		)
	}
}

func TestXAIVideoUsagePublishesExactlyOnce(t *testing.T) {
	tests := []struct {
		name       string
		payload    []byte
		wantMethod string
		wantPath   string
		cancel     bool
		wantFailed bool
	}{
		{
			name: "generation post", payload: []byte(`{"model":"grok-video","prompt":"move"}`),
			wantMethod: http.MethodPost, wantPath: xaiVideosGenerationsPath,
		},
		{
			name: "polling get", payload: []byte(`{"model":"grok-video","request_id":"request/1"}`),
			wantMethod: http.MethodGet, wantPath: xaiVideosPath + "/request%2F1",
		},
		{
			name: "cancellation", payload: []byte(`{"model":"grok-video","prompt":"move"}`),
			wantMethod: http.MethodPost, wantPath: xaiVideosGenerationsPath, cancel: true, wantFailed: true,
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
				registerTestUsagePlugin(t, plugin)
				ctx := context.Background()
				if tt.cancel {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				ctx = util.WithRoundTripper(
					ctx, xaiMediaRoundTripper(
						func(req *http.Request) (*http.Response, error) {
							if req.Method != tt.wantMethod || req.URL.EscapedPath() != tt.wantPath {
								t.Fatalf(
									"video request = %s %s, want %s %s", req.Method, req.URL.EscapedPath(),
									tt.wantMethod, tt.wantPath,
								)
							}
							if tt.cancel {
								return nil, req.Context().Err()
							}
							return &http.Response{
								StatusCode: http.StatusOK, Header: make(http.Header),
								Body: io.NopCloser(strings.NewReader(`{"id":"video-1"}`)),
							}, nil
						},
					),
				)
				exec := newXAIExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{
					Provider:   "xai",
					Attributes: map[string]string{"base_url": "https://xai.test", "api_key": "test-key"},
				}
				_, errExecute := exec.Execute(
					ctx, auth, cliproxyexecutor.Request{Model: "grok-video", Payload: tt.payload},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(xaiVideoHandlerType)},
				)
				if tt.wantFailed != (errExecute != nil) {
					t.Fatalf("Execute() error = %v, wantFailed = %t", errExecute, tt.wantFailed)
				}
				record := waitForXAIUsageRecord(t, plugin.records)
				if record.Failed != tt.wantFailed || record.Model != "grok-video" {
					t.Fatalf("usage record = %#v", record)
				}
			},
		)
	}
}

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
	executor := newXAIExecutor(&config.Config{})
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

func TestXAIReasoningEffortUsesRegistryThinkingMetadata(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	clientID := "test-xai-registry-thinking"
	reg.RegisterClient(
		clientID, "xai", []*registry.ModelInfo{
			{ID: "custom-xai-thinking", Type: "xai", Thinking: &registry.ThinkingSupport{Levels: []string{"low"}}},
			{ID: "custom-xai-no-thinking", Type: "xai"},
		},
	)
	t.Cleanup(func() { reg.UnregisterClient(clientID) })

	if !xaiSupportsReasoningEffort("custom-xai-thinking") {
		t.Fatal("custom-xai-thinking should support reasoning effort from registry thinking metadata")
	}
	if xaiSupportsReasoningEffort("custom-xai-no-thinking") {
		t.Fatal("custom-xai-no-thinking should not support reasoning effort without registry thinking metadata")
	}
}

func TestSanitizeXAIResponsesBodyOnlyStripsReasoningEffort(t *testing.T) {
	body := sanitizeXAIResponsesBody(
		[]byte(`{"reasoning":{"effort":"low","summary":"auto"},"include":["reasoning.encrypted_content","web_search_call.action.sources"]}`),
		"custom-xai-no-thinking", cliproxyexecutor.Request{},
	)

	if gjson.GetBytes(body, "reasoning.effort").Exists() {
		t.Fatalf("reasoning.effort should be removed: %s", body)
	}
	if got := gjson.GetBytes(body, "reasoning.summary").String(); got != "auto" {
		t.Fatalf("reasoning.summary = %q, want auto; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "include.0").String(); got != "web_search_call.action.sources" {
		t.Fatalf("include = %s, want only web_search_call.action.sources", gjson.GetBytes(body, "include").Raw)
	}
}

func TestXAIResponsesUsesResolvedThinkingSupport(t *testing.T) {
	const remoteModel = "grok-resolved-only-thinking-test"
	if registry.LookupModelInfo(remoteModel, "xai") != nil {
		t.Fatal("test model must be absent from the static registry")
	}
	for _, tc := range []struct {
		name        string
		model       string
		metadataKey string
		thinking    *registry.ThinkingSupport
		wantEffort  string
	}{
		{name: "home model", model: remoteModel, metadataKey: "cliproxy.resolved_home_model_info", thinking: &registry.ThinkingSupport{Levels: []string{"high"}}, wantEffort: "high"},
		{name: "configured model", model: remoteModel, metadataKey: "cliproxy.resolved_api_key_model_info", thinking: &registry.ThinkingSupport{Levels: []string{"high"}}, wantEffort: "high"},
		{name: "resolved disables registry", model: "grok-4.5", metadataKey: "cliproxy.resolved_home_model_info"},
		{name: "registry fallback", model: "grok-4.5", wantEffort: "high"},
		{name: "unknown fallback", model: remoteModel},
	} {
		for _, override := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/override=%t", tc.name, override), func(t *testing.T) {
				cfg := &config.Config{}
				wantEffort := tc.wantEffort
				if override {
					wantEffort = "high"
					cfg.Payload.Override = []config.PayloadRule{{
						Models: []config.PayloadModelRule{{Name: tc.model}},
						Params: map[string]any{"reasoning.effort": wantEffort},
					}}
				}
				executor := newXAIExecutor(cfg)
				req := cliproxyexecutor.Request{
					Model:   tc.model,
					Payload: []byte(fmt.Sprintf(`{"model":%q,"input":"hello","reasoning":{"effort":"high"}}`, tc.model)),
				}
				if tc.metadataKey != "" {
					req.Metadata = map[string]any{
						tc.metadataKey: &registry.ModelInfo{ID: tc.model, Type: "xai", Thinking: tc.thinking},
					}
				}
				prepared, errPrepare := executor.prepareResponsesRequest(req, cliproxyexecutor.Options{
					SourceFormat: sdktranslator.FormatOpenAIResponse,
				}, false)
				if errPrepare != nil {
					t.Fatalf("prepareResponsesRequest() error = %v", errPrepare)
				}
				if got := gjson.GetBytes(prepared.body, "reasoning.effort").String(); got != wantEffort {
					t.Fatalf("reasoning.effort = %q, want %q; body=%s", got, wantEffort, prepared.body)
				}
			})
		}
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
	exec := newXAIExecutor(&config.Config{})
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
					context.Background(), nil, cliproxyexecutor.Request{
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

	exec := newXAIExecutor(&config.Config{})
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

	exec := newXAIExecutor(&config.Config{})
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
			SourceFormat:    sdktranslator.FormatOpenAIResponse,
			Stream:          true,
			OriginalRequest: []byte(`{"model":"original-grok-model","stream":true,"input":[{"role":"user","content":"hello"},{"type":"compaction_trigger"}]}`),
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
	if strings.Count(output, `"model":"original-grok-model"`) != 2 {
		t.Fatalf("original request model missing from created/in_progress events: %s", output)
	}
	if !strings.Contains(output, `"type":"compaction"`) || !strings.Contains(output, `"encrypted_content":"opaque"`) {
		t.Fatalf("compaction output missing from stream: %s", output)
	}
	if !strings.Contains(output, `"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}`) {
		t.Fatalf("usage missing from completed stream: %s", output)
	}
}

func TestXAIChatBaseURL(t *testing.T) {
	tests := []struct {
		name string
		auth *cliproxyauth.Auth
		want string
	}{
		{name: "nil auth", want: xaiauth.DefaultAPIBaseURL},
		{name: "api key default", auth: &cliproxyauth.Auth{Provider: "xai"}, want: xaiauth.DefaultAPIBaseURL},
		{
			name: "OAuth default",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"auth_kind": "oauth",
					"base_url":  xaiauth.DefaultAPIBaseURL,
				},
			},
			want: xaiauth.CLIChatProxyBaseURL,
		},
		{
			name: "metadata OAuth default",
			auth: &cliproxyauth.Auth{
				Metadata: map[string]any{
					"auth_kind": "oauth",
					"base_url":  xaiauth.DefaultAPIBaseURL,
				},
			},
			want: xaiauth.CLIChatProxyBaseURL,
		},
		{
			name: "using api false",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{xaiUsingAPIAttr: "false"}},
			want: xaiauth.CLIChatProxyBaseURL,
		},
		{
			name: "metadata using api false",
			auth: &cliproxyauth.Auth{Metadata: map[string]any{xaiUsingAPIAttr: false}},
			want: xaiauth.CLIChatProxyBaseURL,
		},
		{
			name: "OAuth using api true",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"auth_kind":     "oauth",
					xaiUsingAPIAttr: "true",
				},
			},
			want: xaiauth.DefaultAPIBaseURL,
		},
		{
			name: "custom gateway",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"auth_kind":     "oauth",
					"base_url":      "https://gateway.example.com/v1",
					xaiUsingAPIAttr: "false",
				},
			},
			want: "https://gateway.example.com/v1",
		},
		{
			name: "invalid using api follows OAuth default",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"auth_kind":     "oauth",
					xaiUsingAPIAttr: "invalid",
				},
			},
			want: xaiauth.CLIChatProxyBaseURL,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				if got := xaiChatBaseURL(tt.auth); got != tt.want {
					t.Fatalf("xaiChatBaseURL() = %q, want %q", got, tt.want)
				}
			},
		)
	}
}

func TestXAIURLNormalization(t *testing.T) {
	for _, rawURL := range []string{
		xaiauth.CLIChatProxyBaseURL,
		xaiauth.CLIChatProxyBaseURL + "/",
		"https://CLI-CHAT-PROXY.GROK.COM/v1",
		"https://cli-chat-proxy.grok.com:443/v1",
	} {
		if !xaiIsCLIChatProxyBaseURL(rawURL) {
			t.Fatalf("xaiIsCLIChatProxyBaseURL(%q) = false", rawURL)
		}
	}
	if xaiIsCLIChatProxyBaseURL("https://cli-chat-proxy.grok.com/v2") {
		t.Fatal("xaiIsCLIChatProxyBaseURL accepted a different path")
	}
}

func TestApplyXAIRefreshTokenPreservesCustomGateway(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "xai",
		Attributes: map[string]string{
			"source": "/tmp/xai.json",
		},
		Metadata: map[string]any{
			"auth_kind":     "oauth",
			"refresh_token": "refresh-custom-gateway",
			"base_url":      "https://gateway.example.com/v1",
			xaiUsingAPIAttr: false,
		},
	}

	refreshed := applyXAIRefreshToken(
		auth,
		&xaiauth.TokenData{AccessToken: "refreshed-token", ExpiresIn: 3600},
		"https://auth.x.ai/oauth2/token",
		"https://gateway.example.com/v1",
	)
	if got := refreshed.Attributes["base_url"]; got != "https://gateway.example.com/v1" {
		t.Fatalf("attribute base_url = %q, want custom gateway", got)
	}
	if got := xaiMetadataString(refreshed.Metadata, "base_url"); got != "https://gateway.example.com/v1" {
		t.Fatalf("metadata base_url = %q, want custom gateway", got)
	}
	if got := xaiChatBaseURL(refreshed); got != "https://gateway.example.com/v1" {
		t.Fatalf("xaiChatBaseURL() after refresh = %q, want custom gateway", got)
	}
}

func TestApplyXAIRefreshTokenSynchronizesTokenStorage(t *testing.T) {
	storage := &xaiauth.TokenStorage{
		Type:          "xai",
		AccessToken:   "old-access",
		RefreshToken:  "old-refresh",
		IDToken:       "old-id",
		TokenType:     "Bearer",
		ExpiresIn:     60,
		Expire:        "old-expiry",
		LastRefresh:   "old-refresh-time",
		Email:         "old@example.com",
		Subject:       "old-subject",
		BaseURL:       "https://gateway.example.com/v1",
		TokenEndpoint: "https://auth.x.ai/oauth2/token",
		AuthKind:      "oauth",
	}
	published := &cliproxyauth.Auth{
		Provider: "xai",
		Storage:  storage,
		Metadata: map[string]any{
			"auth_kind":     "oauth",
			"refresh_token": "old-refresh",
			"base_url":      "https://gateway.example.com/v1",
		},
	}
	auth := published.Clone()
	tokenData := &xaiauth.TokenData{
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		IDToken:      "new-id",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		Expire:       "new-expiry",
		Email:        "new@example.com",
		Subject:      "new-subject",
	}

	refreshed := applyXAIRefreshToken(
		auth,
		tokenData,
		"https://auth.x.ai/oauth2/token",
		"https://gateway.example.com/v1",
	)
	refreshedStorage, ok := refreshed.Storage.(*xaiauth.TokenStorage)
	if !ok || refreshedStorage == storage {
		t.Fatalf("refresh reused published token storage: %#v", refreshed.Storage)
	}
	if published.Storage != storage || storage.AccessToken != "old-access" ||
		storage.RefreshToken != "old-refresh" || storage.LastRefresh != "old-refresh-time" ||
		storage.Metadata != nil || published.Metadata["access_token"] != nil {
		t.Fatalf("refresh mutated published auth or storage: %#v, %#v", published, storage)
	}
	if refreshedStorage.AccessToken != tokenData.AccessToken ||
		refreshedStorage.RefreshToken != tokenData.RefreshToken ||
		refreshedStorage.IDToken != tokenData.IDToken || refreshedStorage.ExpiresIn != tokenData.ExpiresIn ||
		refreshedStorage.Expire != tokenData.Expire || refreshedStorage.Email != tokenData.Email ||
		refreshedStorage.Subject != tokenData.Subject {
		t.Fatalf("refreshed xAI token storage was not synchronized: %#v", refreshedStorage)
	}
	if refreshedStorage.LastRefresh == "" || refreshedStorage.LastRefresh == "old-refresh-time" {
		t.Fatalf("refreshed xAI token storage LastRefresh = %q", refreshedStorage.LastRefresh)
	}
	if refreshedStorage.Metadata == nil || refreshedStorage.Metadata["access_token"] != tokenData.AccessToken ||
		refreshedStorage.Metadata["refresh_token"] != tokenData.RefreshToken {
		t.Fatalf("refreshed xAI token storage metadata = %#v", refreshedStorage.Metadata)
	}
}

func TestXAIExecutorPrepareHonorsInjectXSearchConfig(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *config.Config
		wantXSearch bool
	}{
		{name: "default disabled", cfg: &config.Config{}, wantXSearch: false},
		{
			name: "explicitly enabled", cfg: &config.Config{XAI: config.XAIConfig{InjectXSearch: true}},
			wantXSearch: true,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				exec := newXAIExecutor(tt.cfg)
				prepared, err := exec.prepareResponsesRequest(
					cliproxyexecutor.Request{
						Model: "grok-4.5",
						Payload: []byte(`{
						"model":"grok-4.5",
						"input":"search X",
						"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
						"tool_choice":{"type":"allowed_tools","tools":[{"type":"function","name":"lookup"}]}
					}`),
					},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
					false,
				)
				if err != nil {
					t.Fatalf("prepareResponsesRequest() error = %v", err)
				}

				toolCount := 0
				for _, tool := range gjson.GetBytes(prepared.body, "tools").Array() {
					if tool.Get("type").String() == xaiXSearchToolType {
						toolCount++
					}
				}
				allowedCount := 0
				for _, tool := range gjson.GetBytes(prepared.body, "tool_choice.tools").Array() {
					if tool.Get("type").String() == xaiXSearchToolType {
						allowedCount++
					}
				}
				wantCount := 0
				if tt.wantXSearch {
					wantCount = 1
				}
				if toolCount != wantCount || allowedCount != wantCount {
					t.Fatalf(
						"x_search counts = tools:%d allowed:%d, want %d; body=%s", toolCount, allowedCount, wantCount,
						prepared.body,
					)
				}
				if prepared.filterInternalXSearch != tt.wantXSearch {
					t.Fatalf("filterInternalXSearch = %t, want %t", prepared.filterInternalXSearch, tt.wantXSearch)
				}
			},
		)
	}
}

func TestXAIExecutorPrepareRequestAppliesChatProxyIdentity(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://CLI-CHAT-PROXY.GROK.COM:443/v1/responses", nil)
	auth := &cliproxyauth.Auth{
		Metadata: map[string]any{"access_token": "xai-token"},
		Attributes: map[string]string{
			"header:" + xaiClientVersionHeader: "custom-client-version",
		},
	}

	executor := newXAIExecutor(&config.Config{})
	if err := executor.PrepareRequest(req, auth); err != nil {
		t.Fatalf("PrepareRequest() error = %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer xai-token" {
		t.Fatalf("Authorization = %q, want Bearer xai-token", got)
	}
	if got := req.Header.Get(xaiTokenAuthHeader); got != xaiTokenAuthValue {
		t.Fatalf("%s = %q, want %q", xaiTokenAuthHeader, got, xaiTokenAuthValue)
	}
	if got := req.Header.Get(xaiClientVersionHeader); got != "custom-client-version" {
		t.Fatalf("%s = %q, want custom-client-version", xaiClientVersionHeader, got)
	}
}

func TestApplyXAIChatHeaders(t *testing.T) {
	t.Run(
		"OAuth chat proxy", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, xaiauth.CLIChatProxyBaseURL+"/responses", nil)
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"auth_kind": "oauth"}}
			applyXAIChatHeaders(req, auth, "xai-token", true, "conv-1")

			if got := req.Header.Get("Authorization"); got != "Bearer xai-token" {
				t.Fatalf("Authorization = %q, want Bearer xai-token", got)
			}
			if got := req.Header.Get(xaiTokenAuthHeader); got != xaiTokenAuthValue {
				t.Fatalf("%s = %q, want %q", xaiTokenAuthHeader, got, xaiTokenAuthValue)
			}
			if got := req.Header.Get(xaiClientVersionHeader); got != xaiClientVersionValue {
				t.Fatalf("%s = %q, want %q", xaiClientVersionHeader, got, xaiClientVersionValue)
			}
			if got := req.Header.Get("User-Agent"); got != "xai-grok-workspace/1.0.46" {
				t.Fatalf("User-Agent = %q, want xai-grok-workspace/1.0.46", got)
			}
			if got := req.Header.Get(xaiClientIdentifierHeader); got != xaiClientIdentifierValue {
				t.Fatalf("%s = %q, want %q", xaiClientIdentifierHeader, got, xaiClientIdentifierValue)
			}
			if got := req.Header.Get(xaiAuthenticateResponseHeader); got != xaiAuthenticateResponseValue {
				t.Fatalf("%s = %q, want %q", xaiAuthenticateResponseHeader, got, xaiAuthenticateResponseValue)
			}
		},
	)

	t.Run(
		"custom headers override chat proxy", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, xaiauth.CLIChatProxyBaseURL+"/responses", nil)
			auth := &cliproxyauth.Auth{
				Attributes: map[string]string{
					"auth_kind":                        "oauth",
					"header:Authorization":             "blocked-custom-auth",
					"header:" + xaiTokenAuthHeader:     "custom-token-auth",
					"header:" + xaiClientVersionHeader: "custom-client-version",
				},
			}
			applyXAIChatHeaders(req, auth, "xai-token", true, "")

			if got := req.Header.Get("Authorization"); got != "Bearer xai-token" {
				t.Fatalf("Authorization = %q, want protected Bearer token", got)
			}
			if got := req.Header.Get(xaiTokenAuthHeader); got != "custom-token-auth" {
				t.Fatalf("%s = %q, want custom-token-auth", xaiTokenAuthHeader, got)
			}
			if got := req.Header.Get(xaiClientVersionHeader); got != "custom-client-version" {
				t.Fatalf("%s = %q, want custom-client-version", xaiClientVersionHeader, got)
			}
		},
	)

	t.Run(
		"using API suppresses chat proxy identity", func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://CLI-CHAT-PROXY.GROK.COM:443/v1/responses", nil)
			auth := &cliproxyauth.Auth{
				Attributes: map[string]string{
					"auth_kind":     "oauth",
					xaiUsingAPIAttr: "true",
				},
			}
			applyXAIChatHeaders(req, auth, "xai-token", true, "")

			if got := req.Header.Get(xaiTokenAuthHeader); got != "" {
				t.Fatalf("%s = %q, want empty", xaiTokenAuthHeader, got)
			}
			if got := req.Header.Get(xaiClientVersionHeader); got != "" {
				t.Fatalf("%s = %q, want empty", xaiClientVersionHeader, got)
			}
		},
	)

	for _, tt := range []struct {
		name string
		auth *cliproxyauth.Auth
	}{
		{name: "official API", auth: &cliproxyauth.Auth{Attributes: map[string]string{xaiUsingAPIAttr: "true"}}},
		{
			name: "custom gateway", auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"auth_kind": "oauth", "base_url": "https://gateway.example.com/v1",
				},
			},
		},
	} {
		t.Run(
			tt.name, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
				applyXAIChatHeaders(req, tt.auth, "xai-token", false, "")
				if got := req.Header.Get(xaiTokenAuthHeader); got != "" {
					t.Fatalf("%s = %q, want empty", xaiTokenAuthHeader, got)
				}
				if got := req.Header.Get(xaiClientVersionHeader); got != "" {
					t.Fatalf("%s = %q, want empty", xaiClientVersionHeader, got)
				}
			},
		)
	}
}

func TestXAIExecutorFinalToolChoiceMatchesDispatchedTools(t *testing.T) {
	for _, tt := range []struct {
		name         string
		payload      string
		injectSearch bool
		wantTools    int
	}{
		{"absent tools", `{"input":"hello","tool_choice":"required"}`, false, 0},
		{"filtered tool", `{"input":"hello","tools":[{"type":"tool_search"}],"tool_choice":{"type":"tool_search"},"parallel_tool_calls":true}`, false, 0},
		{"injected search after filter", `{"input":"hello","tools":[{"type":"tool_search"}],"tool_choice":{"type":"tool_search"}}`, true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			exec := newXAIExecutor(&config.Config{XAI: config.XAIConfig{InjectXSearch: tt.injectSearch}})
			prepared, err := exec.prepareResponsesRequest(cliproxyexecutor.Request{Model: "grok-4.6", Payload: []byte(tt.payload)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(gjson.GetBytes(prepared.body, "tools").Array()); got != tt.wantTools {
				t.Fatalf("tools = %d, want %d: %s", got, tt.wantTools, prepared.body)
			}
			if gjson.GetBytes(prepared.body, "tool_choice").Exists() || gjson.GetBytes(prepared.body, "parallel_tool_calls").Exists() {
				t.Fatalf("unmatched tool controls remain after final normalization: %s", prepared.body)
			}
		})
	}
}

func TestNormalizeXAIToolsNormalizesCodexAutomationSchemaWithoutFabrication(t *testing.T) {
	largeParameters := `{"oneOf":[{"type":"object","properties":{"mode":{"type":"string"}}}],"$defs":{"mode":{"type":"string"}}}`
	body := []byte(`{"tools":[{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"automation_update","strict":true,"parameters":` + largeParameters + `}]},{"type":"function","name":"exec_command","strict":true,"parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}]}`)

	normalized, err := normalizeXAITools(body)
	if err != nil {
		t.Fatalf("normalizeXAITools() error = %v", err)
	}
	tools := gjson.GetBytes(normalized, "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("tools length = %d, want 2", len(tools))
	}
	for _, tool := range tools {
		switch tool.Get("name").String() {
		case "codex_app__automation_update":
			if tool.Get("parameters.type").String() != "object" || tool.Get("parameters.oneOf.0.type").String() != "object" {
				t.Fatalf("automation_update object union not normalized: %s", tool.Raw)
			}
			if got := tool.Get("parameters.oneOf.0.properties.mode.type").String(); got != "string" {
				t.Fatalf("automation_update schema was discarded: %s", tool.Raw)
			}
			if !tool.Get("parameters.$defs.mode").Exists() {
				t.Fatalf("automation_update $defs was discarded: %s", tool.Raw)
			}
			if tool.Get("strict").Type != gjson.True {
				t.Fatalf("automation_update strict = %s, want true", tool.Get("strict").Raw)
			}
		case "exec_command":
			if got := tool.Get("parameters.properties.cmd.type").String(); got != "string" {
				t.Fatalf("exec_command schema changed: %s", tool.Raw)
			}
		}
	}
}

func TestXAICodexToolIntegerSchemaReachesBuildPreparation(t *testing.T) {
	payload := []byte(`{"model":"grok-4.6","input":"run","tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"timeout_ms":{"type":["number","null","integer"]},"max_output_tokens":{"type":"number"},"cmd":{"type":"number"}}}},{"type":"function","name":"unrelated_tool","parameters":{"type":"object","properties":{"timeout_ms":{"type":"number"}}}}]}`)
	for _, test := range []struct {
		name, userAgent, wantType, wantUnion string
	}{
		{"Codex client", "codex-tui/0.145.0", "integer", `["integer","null"]`},
		{"other client", "curl/8.7.1", "number", `["number","null","integer"]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := newXAIExecutor(&config.Config{}).prepareResponsesRequestWithContext(
				context.Background(), nil, cliproxyexecutor.Request{Model: "grok-4.6", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Headers: http.Header{"User-Agent": {test.userAgent}}},
				false, sdktranslator.FormatCodex,
			)
			if err != nil {
				t.Fatal(err)
			}
			tools := gjson.GetBytes(prepared.body, "tools").Array()
			if len(tools) != 2 || tools[0].Get("parameters.properties.max_output_tokens.type").String() != test.wantType ||
				tools[0].Get("parameters.properties.timeout_ms.type").Raw != test.wantUnion ||
				tools[0].Get("parameters.properties.cmd.type").String() != "number" ||
				tools[1].Get("parameters.properties.timeout_ms.type").String() != "number" {
				t.Fatalf("Codex integer tool schema changed unexpectedly: %s", prepared.body)
			}
		})
	}
}

func TestXAICodexIntegerToolFilterPreservesUnprovenSchemas(t *testing.T) {
	body := []byte(`{"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"yield_time_ms":{"type":"number"}}}}]},{"type":"function","name":"notes__read_file","parameters":{"type":"object","properties":{"start_line":{"anyOf":[{"type":"number"},{"type":"null"}]}}}}]}`)
	codex := http.Header{"User-Agent": {"Codex Desktop/0.146.0"}}
	out := normalizeCodexToolIntegerTypes(body, codex)
	if gjson.GetBytes(out, "tools.0.tools.0.parameters.properties.yield_time_ms.type").String() != "integer" ||
		gjson.GetBytes(out, "tools.1.parameters.properties.start_line.anyOf.0.type").String() != "integer" {
		t.Fatalf("known Codex integer arguments were not normalized: %s", out)
	}
	if again := normalizeCodexToolIntegerTypes(out, codex); !bytes.Equal(again, out) {
		t.Fatalf("normalization not idempotent: first=%s second=%s", out, again)
	}
	if other := normalizeCodexToolIntegerTypes(body, http.Header{"User-Agent": {"curl/8.7.1"}}); !bytes.Equal(other, body) {
		t.Fatalf("unrelated client schema modified: %s", other)
	}
}

func TestNormalizeXAIToolsPreservesDeepReferencedObjectUnion(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"choose","parameters":{"$ref":"#/$defs/Deep","$defs":{"Deep":{"anyOf":[{"$ref":"#/$defs/Union"},{"type":"string"}]},"Union":{"oneOf":[{"type":"object","properties":{"left":{"type":"string"}},"required":["left"]},{"type":"object","properties":{"right":{"type":"number"}},"required":["right"]},{"type":"null"}]}}}}]}`)
	normalized, err := normalizeXAITools(body)
	if err != nil {
		t.Fatalf("normalizeXAITools() error = %v", err)
	}
	parameters := gjson.GetBytes(normalized, "tools.0.parameters")
	if parameters.Get("type").String() != "object" || len(parameters.Get("anyOf").Array()) != 1 {
		t.Fatalf("deep root union lost object branch: %s", parameters.Raw)
	}
	branches := parameters.Get("anyOf.0.oneOf").Array()
	if len(branches) != 2 || branches[0].Get("required.0").String() != "left" || branches[1].Get("required.0").String() != "right" {
		t.Fatalf("exclusive required branches were merged or lost: %s", parameters.Raw)
	}
	if parameters.Get("required").Exists() || parameters.Get("properties").Exists() || strings.Contains(parameters.Raw, `"$ref"`) {
		t.Fatalf("schema was flattened, kept an unresolved ref, or added global required: %s", parameters.Raw)
	}
}

func TestNormalizeXAIToolsPreservesUnrelatedAutomationSchemas(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{
			name: "top-level",
			body: []byte(`{"tools":[{"type":"function","name":"automation_update","strict":true,"parameters":{"type":"object","properties":{"cron":{"type":"string"}},"additionalProperties":false}}]}`),
		},
		{
			name: "different namespace",
			body: []byte(`{"tools":[{"type":"namespace","name":"calendar","tools":[{"type":"function","name":"automation_update","strict":true,"parameters":{"type":"object","properties":{"cron":{"type":"string"}},"additionalProperties":false}}]}]}`),
		},
		{
			name: "different namespace case",
			body: []byte(`{"tools":[{"type":"namespace","name":"Codex_App","tools":[{"type":"function","name":"automation_update","strict":true,"parameters":{"type":"object","properties":{"cron":{"type":"string"}},"additionalProperties":false}}]}]}`),
		},
		{
			name: "different function case",
			body: []byte(`{"tools":[{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"Automation_Update","strict":true,"parameters":{"type":"object","properties":{"cron":{"type":"string"}},"additionalProperties":false}}]}]}`),
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				normalized, err := normalizeXAITools(tt.body)
				if err != nil {
					t.Fatalf("normalizeXAITools() error = %v", err)
				}
				tool := gjson.GetBytes(normalized, "tools.0")
				if tool.Get("strict").Type != gjson.True {
					t.Fatalf("strict changed: %s", tool.Raw)
				}
				if got := tool.Get("parameters.properties.cron.type").String(); got != "string" {
					t.Fatalf("parameters changed: %s", tool.Raw)
				}
				if tool.Get("parameters.additionalProperties").Type != gjson.False {
					t.Fatalf("additionalProperties changed: %s", tool.Raw)
				}
			},
		)
	}
}

func TestXAIStatusErrBadCredentials403RemapsToUnauthorized(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"code":"unauthenticated:bad-credentials","error":"The OAuth2 access token could not be validated."}`),
		[]byte(`{"error":{"code":"unauthenticated:bad-credentials","message":"The OAuth2 access token could not be validated."}}`),
		[]byte(`{"error":"The OAuth2 access token could not be validated."}`),
	} {
		err := xaiStatusErr(http.StatusForbidden, body)
		if err.StatusCode() != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", err.StatusCode(), body)
		}
		if err.Error() != string(body) {
			t.Fatalf("error body = %q, want %q", err.Error(), body)
		}
		if err.RetryAfter() != nil {
			t.Fatalf("RetryAfter = %v, want nil", err.RetryAfter())
		}
	}
}

func TestXAIStatusErrContentSafety403IsRequestScoped(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"code":"permission-denied","error":"Content violates usage guidelines. Failed check: SAFETY_CHECK_TYPE_BIO"}`),
		[]byte(`{"error":{"code":"permission-denied","message":"Failed check: SAFETY_CHECK_TYPE_VIOLENCE"}}`),
	} {
		err := xaiStatusErr(http.StatusForbidden, body)
		if err.StatusCode() != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", err.StatusCode(), body)
		}
		if !err.IsRequestScoped() {
			t.Fatalf("IsRequestScoped() = false, want true; body=%s", body)
		}
	}
}

func TestXAIStatusErrGeneric403Unchanged(t *testing.T) {
	err := xaiStatusErr(
		http.StatusForbidden,
		[]byte(`{"code":"permission_denied","error":"model access is not allowed"}`),
	)
	if err.StatusCode() != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", err.StatusCode())
	}
	if err.IsRequestScoped() {
		t.Fatal("generic permission denial became request-scoped")
	}
}

func TestXAIStatusErrSetsFreeUsageCooldown(t *testing.T) {
	err := xaiStatusErr(
		http.StatusTooManyRequests,
		[]byte(`{"code":"subscription:free-usage-exhausted","error":"included free usage exhausted"}`),
	)
	if err.retryAfter == nil || *err.retryAfter != xaiFreeUsageExhaustedCooldown {
		t.Fatalf("retryAfter = %v, want %v", err.retryAfter, xaiFreeUsageExhaustedCooldown)
	}

	generic := xaiStatusErr(http.StatusTooManyRequests, []byte(`{"error":"rate limited"}`))
	if generic.retryAfter != nil {
		t.Fatalf("generic retryAfter = %v, want nil", generic.retryAfter)
	}
}

func TestXAISupportsNativeImageGeneration(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{model: "", want: false},
		{model: "grok-4.5", want: false},
		{model: "grok-4.20-0309-reasoning", want: false},
		{model: "grok-build-0.1", want: false},
		{model: "grok-composer-2.5-fast", want: false},
		{model: "grok-4.6", want: true},
		{model: "grok-4.6(high)", want: true},
		{model: "xai/grok-4.6", want: true},
		{model: "grok-4.7", want: true},
		{model: "grok-5", want: true},
	}
	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			if got := xaiSupportsNativeImageGeneration(test.model); got != test.want {
				t.Fatalf("xaiSupportsNativeImageGeneration(%q) = %t, want %t", test.model, got, test.want)
			}
		})
	}
}

func TestXAIExecutorPrepareRejectsUnsupportedImageGenerationModel(t *testing.T) {
	exec := newXAIExecutor(&config.Config{})
	_, err := exec.prepareResponsesRequest(
		cliproxyexecutor.Request{
			Model: "grok-4.5",
			Payload: []byte(`{
				"model":"grok-4.5",
				"input":"draw",
				"tools":[{"type":"image_generation"}],
				"tool_choice":{"type":"image_generation"}
			}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
		false,
	)
	if err == nil {
		t.Fatal("prepareResponsesRequest() error = nil")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusBadRequest {
		t.Fatalf("error = %#v, want status 400", err)
	}
	if !strings.Contains(err.Error(), "does not support the image_generation tool") {
		t.Fatalf("error = %q", err)
	}
}

func TestXAIExecutorPrepareImageGenerationChoiceMatrix(t *testing.T) {
	exec := newXAIExecutor(&config.Config{XAI: config.XAIConfig{InjectXSearch: true}})
	prepare := func(t *testing.T, choice string) []byte {
		t.Helper()
		payload := fmt.Sprintf(`{
			"model":"grok-4.6",
			"input":"draw or look up",
			"tools":[
				{"type":"image_generation","action":"generate"},
				{"type":"function","name":"lookup","parameters":{"type":"object"}}
			],
			"tool_choice":%s
		}`, choice)
		prepared, err := exec.prepareResponsesRequest(
			cliproxyexecutor.Request{Model: "grok-4.6", Payload: []byte(payload)},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
			false,
		)
		if err != nil {
			t.Fatalf("prepareResponsesRequest() error = %v", err)
		}
		return prepared.body
	}

	for _, choice := range []string{`"required"`, `"auto"`} {
		body := prepare(t, choice)
		if got := gjson.GetBytes(body, "tool_choice").String(); got != strings.Trim(choice, `"`) {
			t.Fatalf("tool_choice = %q, want %s; body=%s", got, choice, body)
		}
		if len(gjson.GetBytes(body, "tools").Array()) != 3 {
			t.Fatalf("required/auto tools should keep image, function, and injected x_search: %s", body)
		}
	}

	forced := prepare(t, `{"type":"image_generation"}`)
	if got := gjson.GetBytes(forced, "tool_choice").String(); got != "required" {
		t.Fatalf("forced tool_choice = %q, want required; body=%s", got, forced)
	}
	if tools := gjson.GetBytes(forced, "tools").Array(); len(tools) != 1 || tools[0].Get("type").String() != xaiImageGenerationToolType {
		t.Fatalf("forced tools = %s, want image_generation only", gjson.GetBytes(forced, "tools").Raw)
	}

	autoOnly := prepare(t, `{"type":"allowed_tools","mode":"auto","tools":[{"type":"image_generation"}]}`)
	if got := gjson.GetBytes(autoOnly, "tool_choice").String(); got != "auto" {
		t.Fatalf("image-only auto choice = %q, want auto; body=%s", got, autoOnly)
	}
	if tools := gjson.GetBytes(autoOnly, "tools").Array(); len(tools) != 1 || tools[0].Get("type").String() != xaiImageGenerationToolType {
		t.Fatalf("image-only auto tools = %s", gjson.GetBytes(autoOnly, "tools").Raw)
	}

	mixed := prepare(t, `{"type":"allowed_tools","mode":"required","tools":[{"type":"image_generation"},{"type":"function","name":"lookup"}]}`)
	allowed := gjson.GetBytes(mixed, "tool_choice.tools").Array()
	if len(allowed) != 2 || allowed[0].Get("name").String() != "lookup" || allowed[1].Get("type").String() != xaiXSearchToolType {
		t.Fatalf("mixed allowed_tools = %s, want lookup and injected x_search", gjson.GetBytes(mixed, "tool_choice.tools").Raw)
	}
}

func TestXAIExecutorPrepareWebSearchChoiceMatrix(t *testing.T) {
	exec := newXAIExecutor(&config.Config{XAI: config.XAIConfig{InjectXSearch: true}})
	for _, tc := range []struct {
		name              string
		choice            string
		tools             string
		wantChoice        string
		wantTools         int
		wantWebSearchOnly bool
	}{
		{name: "forced", choice: `{"type":"web_search"}`, wantChoice: "required", wantTools: 1, wantWebSearchOnly: true},
		{name: "forced with native x_search", choice: `{"type":"web_search"}`,
			tools:      `[{"type":"web_search"},{"type":"x_search"}]`,
			wantChoice: "required", wantTools: 1, wantWebSearchOnly: true},
		{name: "web only auto", choice: `{"type":"allowed_tools","mode":"auto","tools":[{"type":"web_search"}]}`, wantChoice: "auto", wantTools: 1, wantWebSearchOnly: true},
		{name: "web only required", choice: `{"type":"allowed_tools","mode":"required","tools":[{"type":"web_search"}]}`, wantChoice: "required", wantTools: 1, wantWebSearchOnly: true},
		{name: "mixed", choice: `{"type":"allowed_tools","mode":"required","tools":[{"type":"web_search"},{"type":"function","name":"lookup"}]}`, wantTools: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			toolDefinitions := tc.tools
			if toolDefinitions == "" {
				toolDefinitions = `[{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object"}}]`
			}
			payload := fmt.Sprintf(`{"model":"grok-4.6","input":"search or lookup","tools":%s,"tool_choice":%s}`, toolDefinitions, tc.choice)
			prepared, err := exec.prepareResponsesRequest(
				cliproxyexecutor.Request{Model: "grok-4.6", Payload: []byte(payload)},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, false,
			)
			if err != nil {
				t.Fatal(err)
			}
			choice := gjson.GetBytes(prepared.body, "tool_choice")
			tools := gjson.GetBytes(prepared.body, "tools").Array()
			if len(tools) != tc.wantTools {
				t.Fatalf("tool count = %d, want %d: %s", len(tools), tc.wantTools, prepared.body)
			}
			if tc.wantWebSearchOnly {
				if choice.String() != tc.wantChoice || tools[0].Get("type").String() != xaiWebSearchToolType {
					t.Fatalf("forced web search was broadened: %s", prepared.body)
				}
				return
			}
			allowed := choice.Get("tools").Array()
			if len(allowed) != 2 || allowed[0].Get("name").String() != "lookup" || allowed[1].Get("type").String() != xaiXSearchToolType {
				t.Fatalf("mixed tool choice was not normalized: %s", prepared.body)
			}
		})
	}
}

func TestXAIExecutorPrepareInlinesLocalToolReferences(t *testing.T) {
	exec := newXAIExecutor(&config.Config{})
	prepared, err := exec.prepareResponsesRequest(
		cliproxyexecutor.Request{
			Model: "grok-4.6",
			Payload: []byte(`{
				"model":"grok-4.6",
				"input":"query",
				"tools":[{"type":"function","name":"query_user","parameters":{
					"type":"object",
					"properties":{"user":{"$ref":"#/$defs/User"}},
					"$defs":{"User":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}
				}}]
			}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
		false,
	)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}
	tool := gjson.GetBytes(prepared.body, "tools.0")
	if got := tool.Get("parameters.properties.user.properties.name.type").String(); got != "string" {
		t.Fatalf("inlined user.name.type = %q, want string; tool=%s", got, tool.Raw)
	}
	if tool.Get("parameters.$defs").Exists() || tool.Get("parameters.definitions").Exists() {
		t.Fatalf("consumed definitions remain in tool schema: %s", tool.Raw)
	}
}

func TestXAIPatchGrammarSurvivesNamespaceFolding(t *testing.T) {
	const grammar = " \nstart: begin_patch hunk+ end_patch\r\n\n  "
	for _, folded := range []bool{false, true} {
		t.Run(fmt.Sprintf("folded=%t", folded), func(t *testing.T) {
			children := []string{fmt.Sprintf(`{"type":"custom","name":"apply_patch","format":{"definition":%q}}`, grammar)}
			if folded {
				for index := range xaiMaxTools {
					children = append(children, fmt.Sprintf(`{"type":"function","name":"fixture_%d","parameters":{"type":"object","properties":{}}}`, index))
				}
			}
			payload := []byte(fmt.Sprintf(`{"model":"grok-4.7","input":"synthetic patch","tools":[{"type":"namespace","name":"editor","tools":[%s]}]}`, strings.Join(children, ",")))
			prepared, err := newXAIExecutor(&config.Config{}).prepareResponsesRequest(
				cliproxyexecutor.Request{Model: "grok-4.7", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}, false,
			)
			if err != nil {
				t.Fatal(err)
			}
			tool := gjson.GetBytes(prepared.body, "tools.0")
			wantName := "editor__apply_patch"
			if folded {
				wantName = "editor"
			}
			if got := tool.Get("name").String(); got != wantName {
				t.Fatalf("tool = %q, want %q", got, wantName)
			}
			if !strings.Contains(tool.Get("description").String(), grammar) {
				t.Fatalf("complete patch grammar was not preserved: %q", tool.Get("description").String())
			}
		})
	}
}

func TestXAIExecutorPrepareFoldsNamespaceDispatchers(t *testing.T) {
	namespaces := make([]string, 0, 21)
	for namespaceIndex := range 21 {
		children := make([]string, 0, 10)
		for toolIndex := range 10 {
			parameters := `{"type":"object","properties":{"value":{"type":"string"}}}`
			if namespaceIndex == 0 && toolIndex == 3 {
				parameters = `{"type":"object","properties":{"value":{"$ref":"#/$defs/Value"}},"$defs":{"Value":{"type":"string"}}}`
			}
			children = append(children, fmt.Sprintf(
				`{"type":"function","name":"tool_%d","description":"child %d","parameters":%s}`,
				toolIndex, toolIndex, parameters,
			))
		}
		namespaces = append(namespaces, fmt.Sprintf(
			`{"type":"namespace","name":"mcp__app_%d","tools":[%s]}`,
			namespaceIndex, strings.Join(children, ","),
		))
	}
	payload := []byte(fmt.Sprintf(`{
		"model":"grok-4.6",
		"tools":[%s],
		"tool_choice":{"type":"function","namespace":"mcp__app_0","name":"tool_3"},
		"input":[
			{"type":"function_call","namespace":"mcp__app_0","name":"tool_3","call_id":"call_1","arguments":"{\"value\":\"old\"}"},
			{"role":"user","content":"continue"}
		]
	}`, strings.Join(namespaces, ",")))

	exec := newXAIExecutor(&config.Config{})
	prepared, err := exec.prepareResponsesRequest(
		cliproxyexecutor.Request{Model: "grok-4.6", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
		false,
	)
	if err != nil {
		t.Fatalf("prepareResponsesRequest() error = %v", err)
	}
	tools := gjson.GetBytes(prepared.body, "tools").Array()
	if len(tools) != 21 {
		t.Fatalf("tools length = %d, want 21 dispatchers", len(tools))
	}
	if got := tools[0].Get("name").String(); got != "mcp__app_0" {
		t.Fatalf("first dispatcher name = %q", got)
	}
	if description := tools[0].Get("description").String(); !strings.Contains(description, `"value":{"type":"string"}`) || strings.Contains(description, `"$ref"`) {
		t.Fatalf("dispatcher description did not inline child schema: %s", description)
	}
	if got := gjson.GetBytes(prepared.body, "tool_choice.name").String(); got != "mcp__app_0" {
		t.Fatalf("dispatcher tool_choice.name = %q; body=%s", got, prepared.body)
	}
	if gjson.GetBytes(prepared.body, "tool_choice.namespace").Exists() {
		t.Fatalf("dispatcher tool_choice namespace remains: %s", prepared.body)
	}
	if got := gjson.GetBytes(prepared.body, "input.0.name").String(); got != "mcp__app_0" {
		t.Fatalf("historical dispatcher call name = %q; body=%s", got, prepared.body)
	}
	if got := gjson.GetBytes(prepared.body, "input.0.arguments").String(); !strings.Contains(got, `"name":"tool_3"`) {
		t.Fatalf("historical dispatcher arguments = %q", got)
	}

	normalizer := prepared.newResponseNormalizer()
	added := normalizer.normalize([]byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","name":"mcp__app_0"}}`))
	if len(added) != 1 {
		t.Fatalf("added events = %q, want one", added)
	}
	if got := gjson.GetBytes(added[0], "item.namespace").String(); got != "mcp__app_0" {
		t.Fatalf("added item namespace = %q, want mcp__app_0; event=%s", got, added)
	}
	argumentsDone := normalizer.normalize([]byte(`{"type":"response.function_call_arguments.done","item_id":"item_1","output_index":0,"arguments":"{\"name\":\"tool_3\",\"arguments\":{\"value\":\"new\"}}"}`))
	if len(argumentsDone) != 1 {
		t.Fatalf("arguments.done events = %q, want one", argumentsDone)
	}
	if got := gjson.GetBytes(argumentsDone[0], "arguments").String(); got != `{"value":"new"}` {
		t.Fatalf("restored arguments.done = %q; event=%s", got, argumentsDone)
	}
	done := normalizer.normalize([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","name":"mcp__app_0","arguments":"{\"name\":\"tool_3\",\"arguments\":{\"value\":\"new\"}}"}}`))
	if len(done) != 1 {
		t.Fatalf("item.done events = %q, want one", done)
	}
	if got := gjson.GetBytes(done[0], "item.name").String(); got != "tool_3" {
		t.Fatalf("restored item name = %q; event=%s", got, done)
	}
	if got := gjson.GetBytes(done[0], "item.namespace").String(); got != "mcp__app_0" {
		t.Fatalf("restored item namespace = %q; event=%s", got, done)
	}
}

func TestXAIExecutorStreamsReplayOnlyAfterSuccessfulTerminalDelivery(t *testing.T) {
	for _, test := range []struct {
		name      string
		trailing  string
		wantError bool
	}{
		{name: "completed and EOF"},
		{name: "error after completed", trailing: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"late failure\"}}\n\n", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			internalcache.ClearXAIReasoningReplayCache()
			t.Cleanup(internalcache.ClearXAIReasoningReplayCache)
			const terminal = `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call","call_id":"call_new","name":"lookup","arguments":"{}"}],"usage":{"input_tokens":8,"output_tokens":3,"total_tokens":11}}}`
			ctx := util.WithRoundTripper(xaiC03ContextWithAPIKey("stream-caller"), xaiMediaRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "xai.test" {
					t.Fatalf("unexpected upstream host: %s", req.URL.Host)
				}
				body := "data: " + terminal + "\n\n" + test.trailing
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			}))
			payload := []byte(`{"model":"grok-4.6","prompt_cache_key":"stream-terminal","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"input":[{"role":"user","content":"hi"}]}`)
			req := cliproxyexecutor.Request{Model: "grok-4.6", Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true}
			auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "offline-token", "base_url": "https://xai.test"}}
			scope := xaiReasoningReplayScopeFromRequest(ctx, auth, opts.SourceFormat, req, opts, payload)
			if !scope.valid() {
				t.Fatal("invalid replay scope")
			}
			exec := newXAIExecutor(&config.Config{})
			result, err := exec.ExecuteStream(ctx, auth, req, opts)
			if err != nil {
				t.Fatal(err)
			}
			var streamError error
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					streamError = chunk.Err
				}
			}
			if (streamError != nil) != test.wantError {
				t.Fatalf("stream error = %v, wantError %t", streamError, test.wantError)
			}
			items, stored := internalcache.GetXAIReasoningReplayItems(scope.modelName, scope.sessionKey)
			if stored == test.wantError {
				t.Fatalf("replay stored = %t, want %t; items=%q", stored, !test.wantError, items)
			}
			if stored && (len(items) != 1 || gjson.GetBytes(items[0], "call_id").String() != "call_new") {
				t.Fatalf("replay = %q, want completed tool call", items)
			}
		})
	}
}

func TestXAIExecutorAcceptsIncompleteWithoutReplacingReplay(t *testing.T) {
	internalcache.ClearXAIReasoningReplayCache()
	t.Cleanup(internalcache.ClearXAIReasoningReplayCache)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"partial\"}]} }\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":1,\"total_tokens\":9}}}\n\n")
	}))
	defer server.Close()

	ctx := xaiC03ContextWithAPIKey("incomplete-caller")
	payload := []byte(`{"model":"grok-4.6","prompt_cache_key":"incomplete-session","input":[{"role":"user","content":"hi"}]}`)
	req := cliproxyexecutor.Request{Model: "grok-4.6", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
	auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "offline-token", "base_url": server.URL}}
	scope := xaiReasoningReplayScopeFromRequest(ctx, auth, opts.SourceFormat, req, opts, payload)
	seedXAIReasoningReplay(t, scope)

	exec := newXAIExecutor(&config.Config{})
	resp, err := exec.Execute(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "status").String(); got != "incomplete" {
		t.Fatalf("status = %q, want incomplete; payload=%s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.#").Int(); got != 1 {
		t.Fatalf("output length = %d, want 1; payload=%s", got, resp.Payload)
	}
	if _, ok := internalcache.GetXAIReasoningReplayItems(scope.modelName, scope.sessionKey); !ok {
		t.Fatal("response.incomplete replaced or deleted completed-only replay state")
	}
}

func TestXAIExecutorExecuteStreamAcceptsIncomplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"partial\"}]} }\n\n")
		_, _ = fmt.Fprint(w, "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_1\",\"status\":\"incomplete\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":1,\"total_tokens\":9}}}\n\n")
	}))
	defer server.Close()

	exec := newXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"base_url": server.URL}}
	result, err := exec.ExecuteStream(
		context.Background(),
		auth,
		cliproxyexecutor.Request{Model: "grok-4.6", Payload: []byte(`{"model":"grok-4.6","input":"hi"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var stream bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		stream.Write(chunk.Payload)
		stream.WriteByte('\n')
	}
	if !strings.Contains(stream.String(), `"type":"response.incomplete"`) || !strings.Contains(stream.String(), `"text":"partial"`) {
		t.Fatalf("incomplete terminal output missing: %s", stream.String())
	}
}

func TestXAIExecutorMediaUsesOAuthChatBaseURL(t *testing.T) {
	requests := make([]string, 0, 3)
	ctx := util.WithRoundTripper(context.Background(), xaiMediaRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.URL.String())
		body := `{"request_id":"video_1"}`
		if strings.Contains(req.URL.Path, "/images/") {
			body = `{"data":[]}`
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}))
	auth := &cliproxyauth.Auth{
		Provider:   "xai",
		Attributes: map[string]string{"auth_kind": "oauth", "base_url": xaiauth.DefaultAPIBaseURL},
		Metadata:   map[string]any{"access_token": "oauth-token"},
	}
	exec := newXAIExecutor(&config.Config{})
	if _, err := exec.Execute(
		ctx,
		auth,
		cliproxyexecutor.Request{Model: "grok-imagine-image", Payload: []byte(`{"model":"grok-imagine-image","prompt":"draw"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(xaiImageHandlerType)},
	); err != nil {
		t.Fatalf("image Execute() error = %v", err)
	}
	if _, err := exec.Execute(
		ctx,
		auth,
		cliproxyexecutor.Request{Model: "grok-imagine-image-2.0", Payload: []byte(`{"model":"grok-imagine-image-2.0","prompt":"draw"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(xaiImageHandlerType)},
	); err != nil {
		t.Fatalf("image 2.0 Execute() error = %v", err)
	}
	if _, err := exec.Execute(
		ctx,
		auth,
		cliproxyexecutor.Request{Model: "grok-imagine-video", Payload: []byte(`{"model":"grok-imagine-video","prompt":"move"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString(xaiVideoHandlerType)},
	); err != nil {
		t.Fatalf("video Execute() error = %v", err)
	}
	want := []string{
		xaiauth.CLIChatProxyBaseURL + xaiImagesGenerationsPath,
		xaiauth.CLIChatProxyBaseURL + xaiImagesGenerationsPath,
		xaiauth.CLIChatProxyBaseURL + xaiVideosGenerationsPath,
	}
	if len(requests) != len(want) {
		t.Fatalf("requests = %v, want %v", requests, want)
	}
	for index := range want {
		if requests[index] != want[index] {
			t.Fatalf("requests[%d] = %q, want %q", index, requests[index], want[index])
		}
	}
}

func TestXAIExecutorCompactPreservesPreviousIDAndDropsToolControls(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_compact","output":[{"type":"compaction","encrypted_content":"opaque"}]}`))
	}))
	defer server.Close()

	exec := newXAIExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"base_url": server.URL}}
	_, err := exec.Execute(
		context.Background(),
		auth,
		cliproxyexecutor.Request{
			Model: "grok-4.6",
			Payload: []byte(`{
				"model":"grok-4.6",
				"previous_response_id":"resp_previous",
				"input":[{"type":"compaction_trigger"}],
				"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
				"tool_choice":{"type":"function","name":"lookup"},
				"parallel_tool_calls":true
			}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Alt: "responses/compact"},
	)
	if err != nil {
		t.Fatalf("Execute compact error = %v", err)
	}
	if got := gjson.GetBytes(gotBody, "previous_response_id").String(); got != "resp_previous" {
		t.Fatalf("previous_response_id = %q, want resp_previous; body=%s", got, gotBody)
	}
	for _, field := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
		if gjson.GetBytes(gotBody, field).Exists() {
			t.Fatalf("%s remains in compact body: %s", field, gotBody)
		}
	}
	if xaiInputHasItemType(gotBody, "compaction_trigger") {
		t.Fatalf("compaction_trigger remains in compact body: %s", gotBody)
	}
}

func TestXAIInvalidPatchFailurePreservesObservedUsage(t *testing.T) {
	for _, transport := range []string{"http", "http-stream", "websocket-sse", "websocket-raw"} {
		t.Run(transport, func(t *testing.T) {
			terminal := `{"type":"response.completed","response":{"output":[{"type":"function_call","id":"call_patch","name":"apply_patch","arguments":"{}"}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
				if strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
					conn, err := (&websocket.Upgrader{}).Upgrade(writer, req, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"r"}}`)); err != nil {
						return
					}
					_ = conn.WriteMessage(websocket.TextMessage, []byte(terminal))
					return
				}
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(writer, "data: %s\n\n", terminal)
			}))
			defer server.Close()
			plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
			registerTestUsagePlugin(t, plugin)
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "xai", Attributes: map[string]string{"base_url": server.URL, "api_key": "synthetic-token"}}
			req := cliproxyexecutor.Request{Model: "grok-4", Payload: []byte(`{"input":"patch","tools":[{"type":"custom","name":"apply_patch"}]}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: req.Payload}
			ctx := context.Background()
			if transport == "websocket-raw" {
				ctx = cliproxyexecutor.WithDownstreamWebsocket(ctx)
			}
			var executor cliproxyauth.ProviderExecutor = newXAIExecutor(&config.Config{})
			if strings.HasPrefix(transport, "websocket") {
				ws := newXAIWebsocketsExecutor(&config.Config{})
				defer ws.CloseExecutionSession(cliproxyauth.CloseAllExecutionSessionsID)
				executor = ws
			}
			if transport == "http" {
				if _, err := executor.Execute(ctx, auth, req, opts); err == nil {
					t.Fatal("invalid patch returned success")
				}
			} else {
				result, err := executor.ExecuteStream(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				failed := false
				for chunk := range result.Chunks {
					failed = failed || chunk.Err != nil
				}
				if !failed {
					t.Fatal("invalid patch stream returned success")
				}
			}
			record := waitForXAIUsageRecord(t, plugin.records)
			if !record.Failed || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 8 {
				t.Fatalf("failed usage lost observed tokens: %+v", record)
			}
		})
	}
}

func TestXAIComposerUsesCanonicalClaudeSessionAtManager(t *testing.T) {
	keys := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
			return
		}
		keys <- gjson.GetBytes(body, "prompt_cache_key").String()
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
	}))
	defer server.Close()
	manager := cliproxyauth.NewManager(nil, nil, nil)
	manager.SetHomeClientProvider(func() cliproxyauth.HomeClient { return nil })
	manager.RegisterExecutor(newXAIExecutor(&config.Config{}))
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "xai", Attributes: map[string]string{"base_url": server.URL, "api_key": "synthetic-token"}}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "grok-composer-2.5-fast"}})
	defer registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	req := cliproxyexecutor.Request{Model: "grok-composer-2.5-fast", Payload: []byte(`{"model":"grok-composer-2.5-fast","max_tokens":64,"metadata":{"user_id":"{\"session_id\":\"cache-session-1\"}"},"messages":[{"role":"user","content":"hello"}]}`)}
	for range 2 {
		if _, err := manager.Execute(context.Background(), []string{"xai"}, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); err != nil {
			t.Fatal(err)
		}
	}
	if len(keys) != 2 {
		t.Fatalf("captured conversation keys=%d, want 2", len(keys))
	}
	first, second := <-keys, <-keys
	if first == "" || first != second {
		t.Fatalf("same Claude session produced different conversation keys: %q, %q", first, second)
	}
}

func TestXAIAgentMessageCountMatchesNormalizedRequest(t *testing.T) {
	cfg := &config.Config{Codex: config.CodexConfig{OptimizeMultiAgentV2: true}}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Headers: http.Header{"User-Agent": {"Codex Desktop/0.146.0-alpha.3.1"}}}
	payload := []byte(`{"model":"grok-4.5","input":[{"type":"agent_message","id":"amsg_1","author":"/root","recipient":"/root/task","content":[{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},{"type":"encrypted_content","encrypted_content":"请出一道四则运算题"}]}]}`)
	normalized := multiagentv2.RewriteCodexMultiAgentV2Input(context.Background(), opts.Headers, payload, cfg)
	executor := newXAIExecutor(cfg)
	actual, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{Model: "grok-4.5", Payload: payload}, opts)
	if err != nil {
		t.Fatal(err)
	}
	control, err := executor.CountTokens(context.Background(), nil, cliproxyexecutor.Request{Model: "grok-4.5", Payload: normalized}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(control.Payload, "response.usage.input_tokens").Int() <= 0 || !bytes.Equal(actual.Payload, control.Payload) {
		t.Fatalf("agent count=%s, normalized=%s", actual.Payload, control.Payload)
	}
}
