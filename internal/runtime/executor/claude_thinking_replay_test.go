package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	homekv "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const claudeReplayResolvedModelInfoKey = "cliproxy.resolved_api_key_model_info"

func claudeReplayTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "claude-replay-auth",
		Provider: "claude",
		Attributes: map[string]string{
			cliproxyauth.AttributeAPIKey:   "key-claude-replay",
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindAPIKey,
			"base_url":                     baseURL,
		},
	}
}

func claudeReplayTestRequest(
	payload []byte,
	sessionID string,
	isCompat bool,
) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	req := cliproxyexecutor.Request{
		Model:   "claude-synthetic-4772",
		Payload: payload,
		Metadata: map[string]any{
			claudeReplayResolvedModelInfoKey: &registry.ModelInfo{IsCompat: isCompat},
		},
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		ResponseFormat:  sdktranslator.FormatClaude,
		OriginalRequest: payload,
	}
	if sessionID != "" {
		opts.Metadata = map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: sessionID,
		}
	}
	return req, opts
}

func claudeReplayLocalContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return homekv.WithClientScope(
		ctx, homekv.ClientScope{
			ID:      "claude-replay-runtime",
			Enabled: false,
		},
	)
}

func claudeReplaySignedContent(label, toolID string) []byte {
	return []byte(`[{"type":"thinking","thinking":"` + label + ` reasoning","signature":"sig-` + label + `"},` +
		`{"type":"tool_use","id":"` + toolID + `","name":"Read","input":{"path":"` + label + `"}}]`)
}

func claudeReplaySignedResponse(id, label, toolID string) string {
	return `{"id":"` + id + `","type":"message","role":"assistant","model":"claude-synthetic-4772",` +
		`"content":` + string(claudeReplaySignedContent(label, toolID)) + `,"stop_reason":"tool_use",` +
		`"usage":{"input_tokens":1,"output_tokens":2}}`
}

func claudeReplayTextResponse(id string) string {
	return `{"id":"` + id + `","type":"message","role":"assistant","model":"claude-synthetic-4772",` +
		`"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`
}

func claudeReplayThinkingStream(id, label, toolID string) string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"` + id + `","type":"message","role":"assistant","model":"claude-synthetic-4772","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"` + label + ` reasoning"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-` + label + `"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"` + toolID + `","name":"Read","input":{}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"` + label + `\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

func claudeReplayTextStream(id string) string {
	return "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"` + id + `","type":"message","role":"assistant","model":"claude-synthetic-4772","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

func claudeReplayOmittedPayload(
	turns ...struct {
		toolID string
		path   string
	},
) []byte {
	var body strings.Builder
	body.WriteString(`{"model":"claude-synthetic-4772","max_tokens":32,"messages":[{"role":"user","content":"inspect"}`)
	for _, turn := range turns {
		body.WriteString(`,{"role":"assistant","content":[{"type":"tool_use","id":"`)
		body.WriteString(turn.toolID)
		body.WriteString(`","name":"Read","input":{"path":"`)
		body.WriteString(turn.path)
		body.WriteString(`"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"`)
		body.WriteString(turn.toolID)
		body.WriteString(`","content":"ok"}]}`)
	}
	body.WriteString(`]}`)
	return []byte(body.String())
}

func drainClaudeReplayStream(t *testing.T, result *cliproxyexecutor.StreamResult) bool {
	t.Helper()
	hasError := false
	for chunk := range result.Chunks {
		hasError = hasError || chunk.Err != nil
	}
	return hasError
}

func seedClaudeThinkingReplay(
	t *testing.T,
	ctx context.Context,
	scope claudeThinkingReplayScope,
	content []byte,
) {
	t.Helper()
	_, snapshot, _, errGet := internalcache.GetClaudeThinkingReplayWithSnapshotRequired(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
	)
	if errGet != nil {
		t.Fatal(errGet)
	}
	written, errReplace := internalcache.ReplaceClaudeThinkingReplayIfUnchanged(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
		snapshot,
		content,
	)
	if errReplace != nil || !written {
		t.Fatalf("seed Claude replay = %v, %v", written, errReplace)
	}
}

func claudeThinkingReplayFound(
	t *testing.T,
	ctx context.Context,
	scope claudeThinkingReplayScope,
) bool {
	t.Helper()
	_, _, found, errGet := internalcache.GetClaudeThinkingReplayWithSnapshotRequired(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
	)
	if errGet != nil {
		t.Fatal(errGet)
	}
	return found
}

func TestClaudeThinkingReplayEnabledRequiresExactCompatAPIKeyModel(t *testing.T) {
	payload := []byte(`{"model":"claude-synthetic-4772","messages":[]}`)
	baseRequest, baseOptions := claudeReplayTestRequest(payload, "scope", true)
	baseAuth := claudeReplayTestAuth("http://127.0.0.1")

	tests := []struct {
		name       string
		auth       *cliproxyauth.Auth
		request    cliproxyexecutor.Request
		options    cliproxyexecutor.Options
		wantEnable bool
	}{
		{name: "compat Claude API key", auth: baseAuth, request: baseRequest, options: baseOptions, wantEnable: true},
		{
			name: "non compat model",
			auth: baseAuth,
			request: func() cliproxyexecutor.Request {
				req, _ := claudeReplayTestRequest(payload, "scope", false)
				return req
			}(),
			options: baseOptions,
		},
		{
			name: "unresolved model",
			auth: baseAuth,
			request: cliproxyexecutor.Request{
				Model:   baseRequest.Model,
				Payload: payload,
			},
			options: baseOptions,
		},
		{
			name: "OAuth auth kind",
			auth: func() *cliproxyauth.Auth {
				auth := baseAuth.Clone()
				auth.Attributes[cliproxyauth.AttributeAuthKind] = cliproxyauth.AuthKindOAuth
				return auth
			}(),
			request: baseRequest,
			options: baseOptions,
		},
		{
			name: "OAuth token",
			auth: func() *cliproxyauth.Auth {
				auth := baseAuth.Clone()
				auth.Attributes[cliproxyauth.AttributeAPIKey] = "sk-ant-oat01-test"
				return auth
			}(),
			request: baseRequest,
			options: baseOptions,
		},
		{
			name: "other provider",
			auth: func() *cliproxyauth.Auth {
				auth := baseAuth.Clone()
				auth.Provider = "kimi"
				return auth
			}(),
			request: baseRequest,
			options: baseOptions,
		},
		{
			name:    "OpenAI source",
			auth:    baseAuth,
			request: baseRequest,
			options: func() cliproxyexecutor.Options {
				opts := baseOptions
				opts.SourceFormat = sdktranslator.FormatOpenAI
				return opts
			}(),
		},
		{
			name: "empty credential",
			auth: func() *cliproxyauth.Auth {
				auth := baseAuth.Clone()
				auth.Attributes[cliproxyauth.AttributeAPIKey] = ""
				return auth
			}(),
			request: baseRequest,
			options: baseOptions,
		},
	}

	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				if got := claudeThinkingReplayEnabled(
					testCase.auth, testCase.request, testCase.options,
				); got != testCase.wantEnable {
					t.Fatalf("claudeThinkingReplayEnabled() = %v, want %v", got, testCase.wantEnable)
				}
			},
		)
	}
}

func TestClaudeThinkingReplayScopeIsolation(t *testing.T) {
	payload := []byte(`{"model":"claude-synthetic-4772","prompt_cache_key":"shared","messages":[{"role":"user","content":"hi"}]}`)
	req, opts := claudeReplayTestRequest(payload, "", true)
	auth := claudeReplayTestAuth("https://provider-a.example")
	ctxA := claudeReplayLocalContext(xaiC03ContextWithAPIKey("caller-a"))
	ctxB := claudeReplayLocalContext(xaiC03ContextWithAPIKey("caller-b"))
	scopeA := claudeThinkingReplayScopeFromRequest(ctxA, auth, req, opts)
	scopeB := claudeThinkingReplayScopeFromRequest(ctxB, auth, req, opts)
	if !scopeA.valid() || !scopeB.valid() || scopeA.sessionKey == scopeB.sessionKey {
		t.Fatalf("caller-isolated scopes = A %+v, B %+v", scopeA, scopeB)
	}
	if scope := claudeThinkingReplayScopeFromRequest(
		claudeReplayLocalContext(context.Background()),
		auth,
		req,
		opts,
	); scope.valid() {
		t.Fatalf("scope without caller identity = %+v, want disabled", scope)
	}

	_, executionOptions := claudeReplayTestRequest(payload, "trusted-execution", true)
	executionScope := claudeThinkingReplayScopeFromRequest(
		claudeReplayLocalContext(context.Background()),
		auth,
		req,
		executionOptions,
	)
	if !executionScope.valid() || executionScope.sessionKey != "execution:trusted-execution" {
		t.Fatalf("execution scope = %+v", executionScope)
	}

	otherEndpoint := auth.Clone()
	otherEndpoint.Attributes["base_url"] = "https://provider-b.example"
	otherCredential := auth.Clone()
	otherCredential.Attributes[cliproxyauth.AttributeAPIKey] = "other-key"
	if claudeThinkingReplayModelFamily(auth, req.Model) == claudeThinkingReplayModelFamily(otherEndpoint, req.Model) ||
		claudeThinkingReplayModelFamily(auth, req.Model) == claudeThinkingReplayModelFamily(
			otherCredential, req.Model,
		) ||
		claudeThinkingReplayModelFamily(auth, req.Model) == claudeThinkingReplayModelFamily(auth, "claude-other") {
		t.Fatal("model family did not isolate endpoint, credential, and model")
	}
}

func TestRestoreClaudeThinkingReplayContentsRestoresMultipleTurns(t *testing.T) {
	first := claudeReplaySignedContent("one", "tool-one")
	second := claudeReplaySignedContent("two", "tool-two")
	body := claudeReplayOmittedPayload(
		struct {
			toolID string
			path   string
		}{toolID: "tool-one", path: "one"},
		struct {
			toolID string
			path   string
		}{toolID: "tool-two", path: "two"},
	)
	updated, restored := restoreClaudeThinkingReplayContents(body, [][]byte{first, second})
	if !restored || gjson.GetBytes(updated, "messages.1.content.0.signature").String() != "sig-one" ||
		gjson.GetBytes(updated, "messages.3.content.0.signature").String() != "sig-two" {
		t.Fatalf("multi-turn restore failed: %s", updated)
	}

	existing := []byte(`{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"},{"type":"tool_use","id":"tool-one","name":"Read","input":{"path":"one"}}]}]}`)
	unchanged, restored := restoreClaudeThinkingReplayContents(existing, [][]byte{first})
	if restored || !bytes.Equal(unchanged, existing) {
		t.Fatalf("existing thinking was overwritten: %s", unchanged)
	}
}

func TestClaudeExecutorThinkingReplayRestoresNativeResponse(t *testing.T) {
	internalcache.ClearClaudeThinkingReplayCache()
	t.Cleanup(internalcache.ClearClaudeThinkingReplayCache)
	ctx := claudeReplayLocalContext(context.Background())
	var mu sync.Mutex
	var requestBodies [][]byte
	var calls int
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					t.Error(errRead)
					return
				}
				mu.Lock()
				requestBodies = append(requestBodies, bytes.Clone(body))
				calls++
				call := calls
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if call == 1 {
					_, _ = w.Write([]byte(claudeReplaySignedResponse("msg-1", "one", "tool-one")))
					return
				}
				_, _ = w.Write([]byte(claudeReplayTextResponse("msg-2")))
			},
		),
	)
	defer server.Close()

	executor := NewClaudeExecutor(nil)
	auth := claudeReplayTestAuth(server.URL)
	firstPayload := []byte(`{"model":"claude-synthetic-4772","max_tokens":32,"messages":[{"role":"user","content":"inspect"}]}`)
	firstReq, firstOpts := claudeReplayTestRequest(firstPayload, "native-replay", true)
	if _, errExecute := executor.Execute(ctx, auth, firstReq, firstOpts); errExecute != nil {
		t.Fatalf("first Execute() error = %v", errExecute)
	}
	secondPayload := claudeReplayOmittedPayload(
		struct {
			toolID string
			path   string
		}{toolID: "tool-one", path: "one"},
	)
	secondReq, secondOpts := claudeReplayTestRequest(secondPayload, "native-replay", true)
	if _, errExecute := executor.Execute(ctx, auth, secondReq, secondOpts); errExecute != nil {
		t.Fatalf("second Execute() error = %v", errExecute)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requestBodies) != 2 || gjson.GetBytes(
		requestBodies[1], "messages.1.content.0.signature",
	).String() != "sig-one" {
		t.Fatalf("restored upstream request = %s", requestBodies[1])
	}
}

func TestClaudeExecutorThinkingReplayCachesExecuteSSE(t *testing.T) {
	internalcache.ClearClaudeThinkingReplayCache()
	t.Cleanup(internalcache.ClearClaudeThinkingReplayCache)
	ctx := claudeReplayLocalContext(context.Background())
	var mu sync.Mutex
	var requestBodies [][]byte
	var calls int
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				requestBodies = append(requestBodies, bytes.Clone(body))
				calls++
				call := calls
				mu.Unlock()
				if call == 1 {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte(claudeReplayThinkingStream("msg-sse", "sse", "tool-sse")))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(claudeReplayTextResponse("msg-json")))
			},
		),
	)
	defer server.Close()

	executor := NewClaudeExecutor(nil)
	auth := claudeReplayTestAuth(server.URL)
	firstPayload := []byte(`{"model":"claude-synthetic-4772","max_tokens":32,"messages":[{"role":"user","content":"inspect"}]}`)
	firstReq, firstOpts := claudeReplayTestRequest(firstPayload, "execute-sse-replay", true)
	firstOpts.ResponseFormat = sdktranslator.FormatOpenAI
	if _, errExecute := executor.Execute(ctx, auth, firstReq, firstOpts); errExecute != nil {
		t.Fatalf("SSE Execute() error = %v", errExecute)
	}
	secondPayload := claudeReplayOmittedPayload(
		struct {
			toolID string
			path   string
		}{toolID: "tool-sse", path: "sse"},
	)
	secondReq, secondOpts := claudeReplayTestRequest(secondPayload, "execute-sse-replay", true)
	if _, errExecute := executor.Execute(ctx, auth, secondReq, secondOpts); errExecute != nil {
		t.Fatalf("second Execute() error = %v", errExecute)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requestBodies) != 2 || gjson.GetBytes(
		requestBodies[1], "messages.1.content.0.signature",
	).String() != "sig-sse" {
		t.Fatalf("SSE replay request = %s", requestBodies[1])
	}
}

func TestClaudeExecutorThinkingReplayRestoresStreamResponse(t *testing.T) {
	internalcache.ClearClaudeThinkingReplayCache()
	t.Cleanup(internalcache.ClearClaudeThinkingReplayCache)
	ctx := claudeReplayLocalContext(context.Background())
	var mu sync.Mutex
	var requestBodies [][]byte
	var calls int
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				requestBodies = append(requestBodies, bytes.Clone(body))
				calls++
				call := calls
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				if call == 1 {
					_, _ = w.Write([]byte(claudeReplayThinkingStream("msg-1", "stream", "tool-stream")))
					return
				}
				_, _ = w.Write([]byte(claudeReplayTextStream("msg-2")))
			},
		),
	)
	defer server.Close()

	executor := NewClaudeExecutor(nil)
	auth := claudeReplayTestAuth(server.URL)
	firstPayload := []byte(`{"model":"claude-synthetic-4772","max_tokens":32,"messages":[{"role":"user","content":"inspect"}]}`)
	firstReq, firstOpts := claudeReplayTestRequest(firstPayload, "stream-replay", true)
	firstResult, errExecute := executor.ExecuteStream(ctx, auth, firstReq, firstOpts)
	if errExecute != nil || drainClaudeReplayStream(t, firstResult) {
		t.Fatalf("first ExecuteStream() error = %v", errExecute)
	}
	secondPayload := claudeReplayOmittedPayload(
		struct {
			toolID string
			path   string
		}{toolID: "tool-stream", path: "stream"},
	)
	secondReq, secondOpts := claudeReplayTestRequest(secondPayload, "stream-replay", true)
	secondResult, errExecute := executor.ExecuteStream(ctx, auth, secondReq, secondOpts)
	if errExecute != nil || drainClaudeReplayStream(t, secondResult) {
		t.Fatalf("second ExecuteStream() error = %v", errExecute)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requestBodies) != 2 || gjson.GetBytes(
		requestBodies[1], "messages.1.content.0.signature",
	).String() != "sig-stream" {
		t.Fatalf("stream replay request = %s", requestBodies[1])
	}
}

func TestClaudeExecutorThinkingReplayHTTPRejectionClearsOnly400And422(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		status    int
		wantFound bool
	}{
		{name: "400 clears", status: http.StatusBadRequest},
		{name: "422 clears", status: http.StatusUnprocessableEntity},
		{name: "401 preserves", status: http.StatusUnauthorized, wantFound: true},
		{name: "403 preserves", status: http.StatusForbidden, wantFound: true},
		{name: "429 preserves", status: http.StatusTooManyRequests, wantFound: true},
		{name: "500 preserves", status: http.StatusInternalServerError, wantFound: true},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				internalcache.ClearClaudeThinkingReplayCache()
				t.Cleanup(internalcache.ClearClaudeThinkingReplayCache)
				ctx := claudeReplayLocalContext(context.Background())
				var secondBody []byte
				var calls int
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							calls++
							if calls == 1 {
								w.Header().Set("Content-Type", "application/json")
								_, _ = w.Write([]byte(claudeReplaySignedResponse("msg-1", "reject", "tool-reject")))
								return
							}
							secondBody, _ = io.ReadAll(r.Body)
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(testCase.status)
							_, _ = w.Write([]byte(`{"type":"error","error":{"message":"rejected"}}`))
						},
					),
				)
				defer server.Close()

				executor := NewClaudeExecutor(nil)
				auth := claudeReplayTestAuth(server.URL)
				firstPayload := []byte(`{"model":"claude-synthetic-4772","max_tokens":32,"messages":[{"role":"user","content":"inspect"}]}`)
				firstReq, firstOpts := claudeReplayTestRequest(firstPayload, "http-rejection", true)
				if _, errExecute := executor.Execute(ctx, auth, firstReq, firstOpts); errExecute != nil {
					t.Fatal(errExecute)
				}
				secondPayload := claudeReplayOmittedPayload(
					struct {
						toolID string
						path   string
					}{toolID: "tool-reject", path: "reject"},
				)
				secondReq, secondOpts := claudeReplayTestRequest(secondPayload, "http-rejection", true)
				if _, errExecute := executor.Execute(ctx, auth, secondReq, secondOpts); errExecute == nil {
					t.Fatal("second Execute() returned nil error")
				}
				if gjson.GetBytes(secondBody, "messages.1.content.0.signature").String() != "sig-reject" {
					t.Fatalf("replay was not applied before rejection: %s", secondBody)
				}
				scope := claudeThinkingReplayScopeFromRequest(ctx, auth, firstReq, firstOpts)
				if found := claudeThinkingReplayFound(t, ctx, scope); found != testCase.wantFound {
					t.Fatalf("cache after %d = %v, want %v", testCase.status, found, testCase.wantFound)
				}
			},
		)
	}
}

func TestClaudeExecutorThinkingReplayStreamHTTPRejection(t *testing.T) {
	for _, testCase := range []struct {
		status    int
		wantFound bool
	}{
		{status: http.StatusBadRequest},
		{status: http.StatusInternalServerError, wantFound: true},
	} {
		t.Run(
			http.StatusText(testCase.status), func(t *testing.T) {
				internalcache.ClearClaudeThinkingReplayCache()
				t.Cleanup(internalcache.ClearClaudeThinkingReplayCache)
				ctx := claudeReplayLocalContext(context.Background())
				var upstreamBody []byte
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							upstreamBody, _ = io.ReadAll(r.Body)
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(testCase.status)
							_, _ = w.Write([]byte(`{"error":{"message":"rejected"}}`))
						},
					),
				)
				defer server.Close()
				auth := claudeReplayTestAuth(server.URL)
				payload := claudeReplayOmittedPayload(
					struct {
						toolID string
						path   string
					}{toolID: "tool-stream-http", path: "stream-http"},
				)
				req, opts := claudeReplayTestRequest(payload, "stream-http-rejection", true)
				scope := claudeThinkingReplayScopeFromRequest(ctx, auth, req, opts)
				seedClaudeThinkingReplay(t, ctx, scope, claudeReplaySignedContent("stream-http", "tool-stream-http"))
				if _, errExecute := NewClaudeExecutor(nil).ExecuteStream(ctx, auth, req, opts); errExecute == nil {
					t.Fatal("ExecuteStream() returned nil error")
				}
				if gjson.GetBytes(upstreamBody, "messages.1.content.0.signature").String() != "sig-stream-http" {
					t.Fatalf("stream replay was not applied: %s", upstreamBody)
				}
				if found := claudeThinkingReplayFound(t, ctx, scope); found != testCase.wantFound {
					t.Fatalf("cache after %d = %v, want %v", testCase.status, found, testCase.wantFound)
				}
			},
		)
	}
}

func TestClaudeThinkingReplayCachesCitationStream(t *testing.T) {
	internalcache.ClearClaudeThinkingReplayCache()
	t.Cleanup(internalcache.ClearClaudeThinkingReplayCache)
	ctx := claudeReplayLocalContext(context.Background())
	auth := claudeReplayTestAuth("https://provider.example")
	initialPayload := []byte(`{"model":"claude-synthetic-4772","max_tokens":32,"messages":[{"role":"user","content":"inspect"}]}`)
	initialReq, initialOpts := claudeReplayTestRequest(initialPayload, "citation-stream", true)
	_, scope := prepareClaudeThinkingReplayRequest(ctx, auth, initialReq, initialOpts)
	if !scope.cacheReady {
		t.Fatal("citation replay cache is not ready")
	}
	stream := strings.Join(
		[]string{
			`data: {"type":"message_start","message":{"id":"msg","model":"claude-synthetic-4772"}}`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"check","signature":"signed-citation"}}`,
			`data: {"type":"content_block_stop","index":0}`,
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"answer"}}`,
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.com","title":"Example"}}}`,
			`data: {"type":"content_block_stop","index":1}`,
			`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tool-citation","name":"Read","input":{}}}`,
			`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"citation\"}"}}`,
			`data: {"type":"content_block_stop","index":2}`,
			`data: {"type":"message_stop"}`,
		}, "\n",
	)
	cacheClaudeThinkingReplayResponse(ctx, scope, []byte(stream))
	contents, _, found, errGet := internalcache.GetClaudeThinkingReplayWithSnapshotRequired(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
	)
	if errGet != nil || !found || len(contents) != 1 {
		t.Fatalf("citation replay = %d turns, found %v, error %v", len(contents), found, errGet)
	}
	if got := gjson.GetBytes(contents[0], "1.citations.0.url").String(); got != "https://example.com" {
		t.Fatalf("cached citation URL = %q; content=%s", got, contents[0])
	}

	omittedPayload := []byte(`{"model":"claude-synthetic-4772","max_tokens":32,"messages":[` +
		`{"role":"user","content":"inspect"},` +
		`{"role":"assistant","content":[` +
		`{"type":"text","text":"answer","citations":[{"type":"web_search_result_location","url":"https://example.com","title":"Example"}]},` +
		`{"type":"tool_use","id":"tool-citation","name":"Read","input":{"path":"citation"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-citation","content":"ok"}]}]}`)
	omittedReq, omittedOpts := claudeReplayTestRequest(omittedPayload, "citation-stream", true)
	restoredReq, restoredScope := prepareClaudeThinkingReplayRequest(ctx, auth, omittedReq, omittedOpts)
	if !restoredScope.replayApplied ||
		gjson.GetBytes(restoredReq.Payload, "messages.1.content.0.signature").String() != "signed-citation" {
		t.Fatalf("citation replay was not restored: %s", restoredReq.Payload)
	}
}

func TestClaudeExecutorThinkingReplayStreamClearsOnlyRawUpstreamError(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		stream    string
		wantFound bool
	}{
		{
			name:   "invalid request error clears",
			stream: `data: {"type":"error","error":{"type":"invalid_request_error","message":"bad signature"}}`,
		},
		{
			name:      "rate limit error preserves",
			stream:    `data: {"type":"error","error":{"type":"rate_limit_error","message":"try later"}}`,
			wantFound: true,
		},
		{
			name:      "overloaded error preserves",
			stream:    `data: {"type":"error","error":{"type":"overloaded_error","message":"try later"}}`,
			wantFound: true,
		},
		{
			name:      "malformed stream preserves",
			stream:    `data: {not-json}`,
			wantFound: true,
		},
		{
			name:      "incomplete stream preserves",
			stream:    `data: {"type":"message_start","message":{"id":"msg","model":"claude-synthetic-4772"}}`,
			wantFound: true,
		},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				internalcache.ClearClaudeThinkingReplayCache()
				t.Cleanup(internalcache.ClearClaudeThinkingReplayCache)
				ctx := claudeReplayLocalContext(context.Background())
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, _ *http.Request) {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = w.Write([]byte(testCase.stream))
						},
					),
				)
				defer server.Close()
				auth := claudeReplayTestAuth(server.URL)
				payload := claudeReplayOmittedPayload(
					struct {
						toolID string
						path   string
					}{toolID: "tool-stream-error", path: "stream-error"},
				)
				req, opts := claudeReplayTestRequest(payload, "stream-error", true)
				scope := claudeThinkingReplayScopeFromRequest(ctx, auth, req, opts)
				seedClaudeThinkingReplay(t, ctx, scope, claudeReplaySignedContent("stream-error", "tool-stream-error"))
				result, errExecute := NewClaudeExecutor(nil).ExecuteStream(ctx, auth, req, opts)
				if errExecute != nil {
					t.Fatal(errExecute)
				}
				if !drainClaudeReplayStream(t, result) {
					t.Fatal("invalid stream did not return an error chunk")
				}
				if found := claudeThinkingReplayFound(t, ctx, scope); found != testCase.wantFound {
					t.Fatalf("cache after stream = %v, want %v", found, testCase.wantFound)
				}
			},
		)
	}
}

func TestClaudeThinkingReplayStreamObserverOutcomeMatrix(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		outcome   claudeStreamOutcome
		observe   func(claudeStreamObserver)
		wantFound bool
	}{
		{name: "incomplete preserves", outcome: claudeStreamOutcomeIncomplete, wantFound: true},
		{name: "validation failure preserves", outcome: claudeStreamOutcomeValidationFailure, wantFound: true},
		{name: "read failure preserves", outcome: claudeStreamOutcomeReadFailure, wantFound: true},
		{name: "cancel preserves", outcome: claudeStreamOutcomeCanceled, wantFound: true},
		{
			name:    "complete non-replayable clears",
			outcome: claudeStreamOutcomeComplete,
			observe: func(observer claudeStreamObserver) {
				observer.ObserveClaudeSSELine([]byte(claudeReplayTextStream("msg-text")))
			},
		},
		{
			name:    "complete oversized clears",
			outcome: claudeStreamOutcomeComplete,
			observe: func(observer claudeStreamObserver) {
				replayObserver := observer.(*claudeThinkingReplayStreamObserver)
				replayObserver.accumulator.abandoned = true
				replayObserver.accumulator.complete = true
				replayObserver.accumulator.discardOldOnComplete = true
			},
		},
	} {
		t.Run(
			testCase.name, func(t *testing.T) {
				internalcache.ClearClaudeThinkingReplayCache()
				t.Cleanup(internalcache.ClearClaudeThinkingReplayCache)
				ctx := claudeReplayLocalContext(context.Background())
				auth := claudeReplayTestAuth("https://provider.example")
				payload := claudeReplayOmittedPayload(
					struct {
						toolID string
						path   string
					}{toolID: "tool-outcome", path: "outcome"},
				)
				req, opts := claudeReplayTestRequest(payload, "observer-outcome", true)
				scope := claudeThinkingReplayScopeFromRequest(ctx, auth, req, opts)
				seedClaudeThinkingReplay(t, ctx, scope, claudeReplaySignedContent("outcome", "tool-outcome"))
				_, preparedScope := prepareClaudeThinkingReplayRequest(ctx, auth, req, opts)
				if !preparedScope.replayApplied {
					t.Fatal("seeded replay was not applied")
				}
				observer := newClaudeThinkingReplayStreamObserver(ctx, preparedScope)
				if observer == nil {
					t.Fatal("replay observer is nil")
				}
				if testCase.observe != nil {
					testCase.observe(observer)
				}
				observer.FinishClaudeStream(testCase.outcome)
				if found := claudeThinkingReplayFound(t, ctx, scope); found != testCase.wantFound {
					t.Fatalf("cache after outcome %d = %v, want %v", testCase.outcome, found, testCase.wantFound)
				}
			},
		)
	}
}

func TestClaudeExecutorThinkingReplayCacheReadFailureContinuesUpstream(t *testing.T) {
	internalcache.ClearClaudeThinkingReplayCache()
	t.Cleanup(internalcache.ClearClaudeThinkingReplayCache)
	var calls atomic.Int32
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(claudeReplayTextResponse("msg")))
			},
		),
	)
	defer server.Close()
	ctx := homekv.WithClientScope(
		context.Background(), homekv.ClientScope{
			ID:      "unavailable-home",
			Enabled: true,
		},
	)
	auth := claudeReplayTestAuth(server.URL)
	payload := []byte(`{"model":"claude-synthetic-4772","max_tokens":32,"messages":[{"role":"user","content":"inspect"}]}`)
	req, opts := claudeReplayTestRequest(payload, "home-failure", true)
	if _, errExecute := NewClaudeExecutor(nil).Execute(ctx, auth, req, opts); errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
}
