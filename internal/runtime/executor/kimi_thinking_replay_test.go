package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	homekv "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func kimiThinkingReplayTestContent(label string) []byte {
	return []byte(`[
		{"type":"thinking","thinking":"` + label + `","signature":"signature-` + label + `"},
		{"type":"text","text":"checking"},
		{"type":"tool_use","id":"tool-` + label + `","name":"shell","input":{"command":"pwd"}}
	]`)
}

func kimiThinkingReplayTestRequestContent(label string) string {
	return `[
		{"type":"text","text":"checking"},
		{"type":"tool_use","id":"tool-` + label + `","name":"shell","input":{"command":"pwd"}}
	]`
}

func kimiThinkingReplayLocalContext(ctx context.Context, scopeID string) context.Context {
	return homekv.WithClientScope(
		ctx, homekv.ClientScope{
			ID:      scopeID,
			Enabled: false,
		},
	)
}

func seedKimiThinkingReplay(
	t *testing.T,
	ctx context.Context,
	scope kimiThinkingReplayScope,
	content []byte,
) {
	t.Helper()
	_, snapshot, _, errGet := internalcache.GetKimiThinkingReplayWithSnapshotRequired(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
	)
	if errGet != nil {
		t.Fatal(errGet)
	}
	written, errReplace := internalcache.ReplaceKimiThinkingReplayIfUnchanged(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
		snapshot,
		content,
	)
	if errReplace != nil || !written {
		t.Fatalf("seed replace = %v, %v", written, errReplace)
	}
}

func TestKimiThinkingReplayModelFamily(t *testing.T) {
	tests := map[string]string{
		"kimi-k3":                         "k3",
		"kimi-k3-256k":                    "k3",
		"k3-256k(high)":                   "k3",
		"kimi-k2.7-code":                  "kimi-for-coding",
		"kimi-k2.7-code-highspeed":        "kimi-for-coding-highspeed",
		"kimi-for-coding-highspeed(high)": "kimi-for-coding-highspeed",
	}
	for input, want := range tests {
		if got := kimiThinkingReplayModelFamily(input); got != want {
			t.Errorf("kimiThinkingReplayModelFamily(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestKimiThinkingReplayScopePreservesAgentAndCallerIsolation(t *testing.T) {
	payload := []byte(`{"model":"kimi-k3","metadata":{"user_id":"{\"session_id\":\"session-a\"}"},"messages":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "kimi-k3", Payload: payload}
	baseOptions := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Headers:      http.Header{"X-Claude-Code-Agent-Id": []string{"agent-a"}},
	}

	scopeA := kimiThinkingReplayScopeFromRequest(
		xaiC03ContextWithAPIKey("caller-a"),
		req,
		baseOptions,
	)
	scopeB := kimiThinkingReplayScopeFromRequest(
		xaiC03ContextWithAPIKey("caller-b"),
		req,
		baseOptions,
	)
	optionsAgentB := baseOptions
	optionsAgentB.Headers = http.Header{"X-Claude-Code-Agent-Id": []string{"agent-b"}}
	scopeAgentB := kimiThinkingReplayScopeFromRequest(
		xaiC03ContextWithAPIKey("caller-a"),
		req,
		optionsAgentB,
	)
	if !scopeA.valid() || !scopeB.valid() || !scopeAgentB.valid() {
		t.Fatalf("invalid scopes: A=%+v B=%+v agentB=%+v", scopeA, scopeB, scopeAgentB)
	}
	if scopeA.sessionKey == scopeB.sessionKey {
		t.Fatalf("different callers share session key %q", scopeA.sessionKey)
	}
	if scopeA.sessionKey == scopeAgentB.sessionKey {
		t.Fatalf("different Claude Code agents share session key %q", scopeA.sessionKey)
	}
	for _, scope := range []kimiThinkingReplayScope{scopeA, scopeB, scopeAgentB} {
		if !strings.Contains(scope.sessionKey, "claude:session-a:agent:") {
			t.Fatalf("session key lost Claude Code lane: %q", scope.sessionKey)
		}
	}
}

func TestKimiThinkingReplayScopeTrustsExecutionSessionWithoutCallerKey(t *testing.T) {
	payload := []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hello"}]}`)
	scope := kimiThinkingReplayScopeFromRequest(
		context.Background(),
		cliproxyexecutor.Request{Model: "kimi-k3", Payload: payload},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatClaude,
			Metadata: map[string]any{
				cliproxyexecutor.ExecutionSessionMetadataKey: "execution-a",
			},
		},
	)
	if scope.sessionKey != "execution:execution-a" {
		t.Fatalf("execution session key = %q", scope.sessionKey)
	}
}

func TestKimiThinkingReplayScopeIgnoresDerivedSessionWithoutExecutionIdentity(t *testing.T) {
	payload := []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hello"}]}`)
	for _, test := range []struct {
		name string
		req  cliproxyexecutor.Request
		opts cliproxyexecutor.Options
	}{
		{
			name: "options metadata",
			req:  cliproxyexecutor.Request{Model: "kimi-k3", Payload: payload},
			opts: cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FormatClaude,
				Metadata: map[string]any{
					cliproxyexecutor.DerivedSessionIDMetadataKey: "derived-a",
				},
			},
		},
		{
			name: "request metadata",
			req: cliproxyexecutor.Request{
				Model:   "kimi-k3",
				Payload: payload,
				Metadata: map[string]any{
					cliproxyexecutor.DerivedSessionIDMetadataKey: "derived-a",
				},
			},
			opts: cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				scope := kimiThinkingReplayScopeFromRequest(
					xaiC03ContextWithAPIKey("caller-a"),
					test.req,
					test.opts,
				)
				if scope.valid() || scope.sessionKey != "" {
					t.Fatalf("derived-only scope = %+v", scope)
				}
			},
		)
	}
}

func TestRestoreKimiThinkingReplayContentMatchesNonThinkingHistory(t *testing.T) {
	cached := kimiThinkingReplayTestContent("a")
	body := []byte(`{"messages":[
		{"role":"assistant","content":` + kimiThinkingReplayTestRequestContent("a") + `},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-a","content":"ok"}]}
	]}`)

	out, restored := restoreKimiThinkingReplayContent(body, cached)
	if !restored {
		t.Fatalf("matching content was not restored: %s", out)
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.signature").String(); got != "signature-a" {
		t.Fatalf("restored signature = %q; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.0.content.2.id").String(); got != "tool-a" {
		t.Fatalf("restored tool id = %q; body=%s", got, out)
	}
}

func TestRestoreKimiThinkingReplayContentDoesNotOverwriteThinkingOrMismatch(t *testing.T) {
	cached := kimiThinkingReplayTestContent("a")
	tests := []struct {
		name string
		body string
	}{
		{
			name: "existing thinking",
			body: `{"messages":[{"role":"assistant","content":[
				{"type":"thinking","thinking":"current","signature":"current-signature"},
				{"type":"text","text":"checking"},
				{"type":"tool_use","id":"tool-a","name":"shell","input":{"command":"pwd"}}
			]}]}`,
		},
		{
			name: "existing redacted thinking",
			body: `{"messages":[{"role":"assistant","content":[
				{"type":"redacted_thinking","data":"opaque"},
				{"type":"text","text":"checking"},
				{"type":"tool_use","id":"tool-a","name":"shell","input":{"command":"pwd"}}
			]}]}`,
		},
		{
			name: "different tool input",
			body: `{"messages":[{"role":"assistant","content":[
				{"type":"text","text":"checking"},
				{"type":"tool_use","id":"tool-a","name":"shell","input":{"command":"ls"}}
			]}]}`,
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				out, restored := restoreKimiThinkingReplayContent([]byte(test.body), cached)
				if restored || !bytes.Equal(out, []byte(test.body)) {
					t.Fatalf("restore = %v; body=%s", restored, out)
				}
			},
		)
	}
}

func TestShouldClearKimiThinkingReplayAfterErrorUsesUpstreamProvenance(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "upstream 400",
			err:  claudeUpstreamHTTPError{statusErr{code: http.StatusBadRequest}},
			want: true,
		},
		{
			name: "upstream 422",
			err:  claudeUpstreamHTTPError{statusErr{code: http.StatusUnprocessableEntity}},
			want: true,
		},
		{
			name: "upstream invalid request SSE error",
			err: claudeUpstreamSSEError{
				statusErr: statusErr{code: http.StatusBadGateway},
				errorType: "invalid_request_error",
			},
			want: true,
		},
		{
			name: "upstream rate limit SSE error",
			err: claudeUpstreamSSEError{
				statusErr: statusErr{code: http.StatusBadGateway},
				errorType: "rate_limit_error",
			},
		},
		{
			name: "local 400",
			err:  statusErr{code: http.StatusBadRequest},
		},
		{
			name: "upstream 500",
			err:  claudeUpstreamHTTPError{statusErr{code: http.StatusInternalServerError}},
		},
	}
	for _, test := range tests {
		t.Run(
			test.name, func(t *testing.T) {
				if got := shouldClearKimiThinkingReplayAfterError(test.err); got != test.want {
					t.Fatalf("shouldClearKimiThinkingReplayAfterError() = %v, want %v", got, test.want)
				}
			},
		)
	}
}

func TestKimiThinkingReplayStreamAccumulatorRebuildsSignedToolContent(t *testing.T) {
	accumulator := newKimiThinkingReplayStreamAccumulator()
	for _, line := range []string{
		`data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"check"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool-a","name":"shell","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_stop"}`,
	} {
		accumulator.observe([]byte(line))
	}
	content, complete := accumulator.content()
	if !complete {
		t.Fatal("complete stream was not reconstructed")
	}
	if got := gjson.GetBytes(content, "0.signature").String(); got != "signed" {
		t.Fatalf("signature = %q; content=%s", got, content)
	}
	if got := gjson.GetBytes(content, "1.input.command").String(); got != "pwd" {
		t.Fatalf("tool input = %q; content=%s", got, content)
	}
	if !kimiThinkingReplayContentIsReplayable(content) {
		t.Fatalf("reconstructed content is not replayable: %s", content)
	}
}

func TestKimiThinkingReplayStreamAccumulatorPreservesCitations(t *testing.T) {
	accumulator := newKimiThinkingReplayStreamAccumulator()
	for _, line := range []string{
		`data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"check","signature":"signed"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":"answer"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.com","title":"Example"}}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tool-a","name":"shell","input":{}}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"pwd\"}"}}`,
		`data: {"type":"content_block_stop","index":2}`,
		`data: {"type":"message_stop"}`,
	} {
		accumulator.observe([]byte(line))
	}
	content, complete := accumulator.content()
	if !complete {
		t.Fatal("complete citation stream was not reconstructed")
	}
	if got := gjson.GetBytes(content, "1.citations.0.url").String(); got != "https://example.com" {
		t.Fatalf("citation URL = %q; content=%s", got, content)
	}
	if !kimiThinkingReplayContentIsReplayable(content) {
		t.Fatalf("citation content is not replayable: %s", content)
	}
}

func TestKimiThinkingReplayStreamAccumulatorRejectsIncompleteAndMalformedStreams(t *testing.T) {
	incomplete := newKimiThinkingReplayStreamAccumulator()
	incomplete.observe([]byte(`data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`))
	if content, complete := incomplete.content(); complete || content != nil {
		t.Fatalf("incomplete stream = %s, complete %v", content, complete)
	}

	malformed := newKimiThinkingReplayStreamAccumulator()
	malformed.observe([]byte(`data: {not-json}`))
	malformed.observe([]byte(`data: {"type":"message_stop"}`))
	if content, complete := malformed.content(); complete || content != nil {
		t.Fatalf("malformed stream = %s, complete %v", content, complete)
	}
}

func TestKimiThinkingReplayStreamAccumulatorRejectsInvalidLifecycle(t *testing.T) {
	for _, test := range []struct {
		name  string
		lines []string
	}{
		{
			name: "duplicate block index",
			lines: []string{
				`data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				`data: {"type":"message_stop"}`,
			},
		},
		{
			name: "duplicate block stop",
			lines: []string{
				`data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"message_stop"}`,
			},
		},
		{
			name: "unknown delta",
			lines: []string{
				`data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"unknown_delta"}}`,
				`data: {"type":"message_stop"}`,
			},
		},
		{
			name: "invalid partial JSON",
			lines: []string{
				`data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool-a","name":"shell","input":{}}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{"}}`,
				`data: {"type":"content_block_stop","index":0}`,
				`data: {"type":"message_stop"}`,
			},
		},
		{
			name: "sparse block index",
			lines: []string{
				`data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`,
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":"x","signature":"signed"}}`,
				`data: {"type":"content_block_stop","index":1}`,
				`data: {"type":"message_stop"}`,
			},
		},
		{
			name: "block before message start",
			lines: []string{
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				`data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`,
				`data: {"type":"message_stop"}`,
			},
		},
		{
			name: "block after message stop",
			lines: []string{
				`data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`,
				`data: {"type":"message_stop"}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			},
		},
		{
			name: "missing block index",
			lines: []string{
				`data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`,
				`data: {"type":"content_block_start","content_block":{"type":"thinking","thinking":""}}`,
				`data: {"type":"message_stop"}`,
			},
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				accumulator := newKimiThinkingReplayStreamAccumulator()
				for _, line := range test.lines {
					accumulator.observe([]byte(line))
				}
				if content, complete := accumulator.content(); complete || content != nil {
					t.Fatalf("invalid lifecycle content = %s, complete %v", content, complete)
				}
				if accumulator.discardOldOnComplete {
					t.Fatal("malformed lifecycle was classified as a size limit")
				}
			},
		)
	}
}

func TestKimiThinkingReplayStreamObserverPreservesMalformedAndClearsLimitedState(t *testing.T) {
	for _, test := range []struct {
		name      string
		observe   func(*kimiThinkingReplayStreamAccumulator)
		wantFound bool
	}{
		{
			name: "unknown delta preserves",
			observe: func(accumulator *kimiThinkingReplayStreamAccumulator) {
				for _, line := range []string{
					`data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`,
					`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"unknown_delta"}}`,
					`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
					`data: {"type":"message_stop"}`,
				} {
					accumulator.observe([]byte(line))
				}
			},
			wantFound: true,
		},
		{
			name: "byte limit clears",
			observe: func(accumulator *kimiThinkingReplayStreamAccumulator) {
				accumulator.observe([]byte(`data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`))
				accumulator.bytesUsed = internalcache.KimiThinkingReplayCacheMaxBytesPerEntry
				accumulator.observe([]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"x"}}`))
				accumulator.observe([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`))
				accumulator.observe([]byte(`data: {"type":"message_stop"}`))
			},
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				internalcache.ClearKimiThinkingReplayCache()
				t.Cleanup(internalcache.ClearKimiThinkingReplayCache)
				ctx := kimiThinkingReplayLocalContext(context.Background(), "runtime-a")
				scope := kimiThinkingReplayScope{modelFamily: "k3", sessionKey: "stream-outcome"}
				oldContent := kimiThinkingReplayTestContent("old")
				seedKimiThinkingReplay(t, ctx, scope, oldContent)
				_, snapshot, found, errGet := internalcache.GetKimiThinkingReplayWithSnapshotRequired(
					ctx,
					scope.modelFamily,
					scope.sessionKey,
				)
				if errGet != nil || !found {
					t.Fatalf("snapshot read = found %v, err %v", found, errGet)
				}
				scope.snapshot = snapshot
				scope.cacheReady = true
				observer := newKimiThinkingReplayStreamObserver(ctx, scope)
				test.observe(observer.accumulator)
				observer.FinishClaudeStream(claudeStreamOutcomeComplete)

				content, _, found, errGet := internalcache.GetKimiThinkingReplayWithSnapshotRequired(
					ctx,
					scope.modelFamily,
					scope.sessionKey,
				)
				if errGet != nil || found != test.wantFound {
					t.Fatalf("cache after stream = found %v, want %v, err %v", found, test.wantFound, errGet)
				}
				if found && !bytes.Equal(content, oldContent) {
					t.Fatalf("preserved content changed: %s", content)
				}
			},
		)
	}
}

func TestKimiThinkingReplayCompleteNonReplayableContentClearsOldState(t *testing.T) {
	for _, test := range []struct {
		name    string
		content []byte
	}{
		{name: "text only", content: []byte(`[{"type":"text","text":"done"}]`)},
		{name: "empty array", content: []byte(`[]`)},
		{name: "non array", content: []byte(`{"type":"text"}`)},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				internalcache.ClearKimiThinkingReplayCache()
				t.Cleanup(internalcache.ClearKimiThinkingReplayCache)
				ctx := kimiThinkingReplayLocalContext(context.Background(), "runtime-a")
				scope := kimiThinkingReplayScope{
					modelFamily: "k3",
					sessionKey:  "non-replayable",
				}
				seedKimiThinkingReplay(t, ctx, scope, kimiThinkingReplayTestContent("old"))
				_, snapshot, found, errGet := internalcache.GetKimiThinkingReplayWithSnapshotRequired(
					ctx,
					scope.modelFamily,
					scope.sessionKey,
				)
				if errGet != nil || !found {
					t.Fatalf("snapshot read = found %v, err %v", found, errGet)
				}
				scope.snapshot = snapshot
				scope.cacheReady = true
				cacheKimiThinkingReplayContent(ctx, scope, test.content)
				_, _, found, errGet = internalcache.GetKimiThinkingReplayWithSnapshotRequired(
					ctx,
					scope.modelFamily,
					scope.sessionKey,
				)
				if errGet != nil || found {
					t.Fatalf("cache after complete non-replayable content = found %v, err %v", found, errGet)
				}
			},
		)
	}
}

func TestKimiThinkingReplayResponseWithoutClaudeContentPreservesState(t *testing.T) {
	internalcache.ClearKimiThinkingReplayCache()
	t.Cleanup(internalcache.ClearKimiThinkingReplayCache)
	ctx := kimiThinkingReplayLocalContext(context.Background(), "runtime-a")
	scope := kimiThinkingReplayScope{modelFamily: "k3", sessionKey: "translated-response"}
	oldContent := kimiThinkingReplayTestContent("old")
	seedKimiThinkingReplay(t, ctx, scope, oldContent)
	_, snapshot, found, errGet := internalcache.GetKimiThinkingReplayWithSnapshotRequired(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
	)
	if errGet != nil || !found {
		t.Fatalf("snapshot read = found %v, err %v", found, errGet)
	}
	scope.snapshot = snapshot
	scope.cacheReady = true
	cacheKimiThinkingReplayResponse(
		ctx,
		scope,
		[]byte(`{"choices":[{"message":{"role":"assistant","content":"done"}}]}`),
	)
	content, _, found, errGet := internalcache.GetKimiThinkingReplayWithSnapshotRequired(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
	)
	if errGet != nil || !found || !bytes.Equal(content, oldContent) {
		t.Fatalf("cache after translated response = %s, found %v, err %v", content, found, errGet)
	}
}

func TestKimiExecutorClaudeNonStreamCachesAndRestoresReplay(t *testing.T) {
	internalcache.ClearKimiThinkingReplayCache()
	t.Cleanup(internalcache.ClearKimiThinkingReplayCache)
	var calls atomic.Int32
	var secondRequest []byte
	ctx := kimiThinkingReplayLocalContext(context.Background(), "runtime-a")
	ctx = util.WithRoundTripper(
		ctx, kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				body, errRead := io.ReadAll(req.Body)
				if errRead != nil {
					t.Fatal(errRead)
				}
				if calls.Add(1) == 2 {
					secondRequest = append([]byte(nil), body...)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(
						strings.NewReader(
							`{
				"id":"msg","type":"message","role":"assistant","model":"k3",
				"content":` + string(kimiThinkingReplayTestContent("a")) + `,
				"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}
			}`,
						),
					),
				}, nil
			},
		),
	)
	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "kimi",
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}
	baseOptions := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "execution-a",
		},
	}
	firstPayload := []byte(`{"model":"kimi-k3","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
	firstOptions := baseOptions
	firstOptions.OriginalRequest = firstPayload
	if _, errExecute := executor.Execute(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: firstPayload,
		}, firstOptions,
	); errExecute != nil {
		t.Fatal(errExecute)
	}

	secondPayload := []byte(`{"model":"kimi-k3","max_tokens":32,"messages":[
		{"role":"user","content":"hello"},
		{"role":"assistant","content":` + kimiThinkingReplayTestRequestContent("a") + `},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-a","content":"ok"}]}
	]}`)
	secondOptions := baseOptions
	secondOptions.OriginalRequest = secondPayload
	if _, errExecute := executor.Execute(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: secondPayload,
		}, secondOptions,
	); errExecute != nil {
		t.Fatal(errExecute)
	}
	if got := gjson.GetBytes(secondRequest, "messages.1.content.0.signature").String(); got != "signature-a" {
		t.Fatalf("second request signature = %q; body=%s", got, secondRequest)
	}
}

func TestKimiExecutorClaudeNonStreamCASConflictPreservesResponseAndNewerState(t *testing.T) {
	internalcache.ClearKimiThinkingReplayCache()
	t.Cleanup(internalcache.ClearKimiThinkingReplayCache)
	ctx := kimiThinkingReplayLocalContext(context.Background(), "runtime-a")
	payload := []byte(`{"model":"kimi-k3","max_tokens":32,"messages":[
		{"role":"assistant","content":` + kimiThinkingReplayTestRequestContent("old") + `},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-old","content":"ok"}]}
	]}`)
	req := cliproxyexecutor.Request{Model: "kimi-k3", Payload: payload}
	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "cas-conflict",
		},
	}
	scope := kimiThinkingReplayScopeFromRequest(ctx, req, opts)
	seedKimiThinkingReplay(t, ctx, scope, kimiThinkingReplayTestContent("old"))
	newerContent := kimiThinkingReplayTestContent("newer")
	upstreamContent := kimiThinkingReplayTestContent("upstream")

	ctx = util.WithRoundTripper(
		ctx, kimiTokenRoundTripper(
			func(httpReq *http.Request) (*http.Response, error) {
				body, errRead := io.ReadAll(httpReq.Body)
				if errRead != nil {
					t.Fatal(errRead)
				}
				if got := gjson.GetBytes(body, "messages.0.content.0.signature").String(); got != "signature-old" {
					t.Fatalf("replay signature = %q; body=%s", got, body)
				}
				_, snapshot, found, errGet := internalcache.GetKimiThinkingReplayWithSnapshotRequired(
					ctx,
					scope.modelFamily,
					scope.sessionKey,
				)
				if errGet != nil || !found {
					t.Fatalf("concurrent snapshot = found %v, err %v", found, errGet)
				}
				written, errReplace := internalcache.ReplaceKimiThinkingReplayIfUnchanged(
					ctx,
					scope.modelFamily,
					scope.sessionKey,
					snapshot,
					newerContent,
				)
				if errReplace != nil || !written {
					t.Fatalf("concurrent replace = %v, %v", written, errReplace)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(
						strings.NewReader(
							`{
				"id":"msg","type":"message","role":"assistant","model":"k3",
				"content":` + string(upstreamContent) + `,
				"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}
			}`,
						),
					),
				}, nil
			},
		),
	)
	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "kimi",
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}
	response, errExecute := executor.Execute(ctx, auth, req, opts)
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	if got := gjson.GetBytes(response.Payload, "content.0.signature").String(); got != "signature-upstream" {
		t.Fatalf("upstream response signature = %q; payload=%s", got, response.Payload)
	}
	content, _, found, errGet := internalcache.GetKimiThinkingReplayWithSnapshotRequired(
		ctx,
		scope.modelFamily,
		scope.sessionKey,
	)
	if errGet != nil || !found || !bytes.Equal(content, newerContent) {
		t.Fatalf("cache after CAS conflict = %s, found %v, err %v", content, found, errGet)
	}
}

func TestKimiExecutorClaudeStreamCachesRawSignedContent(t *testing.T) {
	internalcache.ClearKimiThinkingReplayCache()
	t.Cleanup(internalcache.ClearKimiThinkingReplayCache)
	ctx := kimiThinkingReplayLocalContext(context.Background(), "runtime-a")
	ctx = util.WithRoundTripper(
		ctx, kimiTokenRoundTripper(
			func(_ *http.Request) (*http.Response, error) {
				stream := strings.Join(
					[]string{
						`data: {"type":"message_start","message":{"id":"msg","model":"k3","usage":{"input_tokens":1,"output_tokens":0}}}`,
						`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
						`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"check"}}`,
						`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed"}}`,
						`data: {"type":"content_block_stop","index":0}`,
						`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool-a","name":"shell","input":{"command":"pwd"}}}`,
						`data: {"type":"content_block_stop","index":1}`,
						`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}`,
						`data: {"type":"message_stop"}`,
					}, "\n",
				)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(stream)),
				}, nil
			},
		),
	)
	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "kimi",
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}
	payload := []byte(`{"model":"kimi-k3","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
	opts := cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "stream-a",
		},
	}
	result, errExecute := executor.ExecuteStream(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: payload,
		}, opts,
	)
	if errExecute != nil {
		t.Fatal(errExecute)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error = %v", chunk.Err)
		}
	}
	content, _, found, errGet := internalcache.GetKimiThinkingReplayWithSnapshotRequired(
		ctx,
		"k3",
		"execution:stream-a",
	)
	if errGet != nil || !found {
		t.Fatalf("stream cache read = found %v, err %v", found, errGet)
	}
	if got := gjson.GetBytes(content, "0.signature").String(); got != "signed" {
		t.Fatalf("cached signature = %q; content=%s", got, content)
	}
	if got := gjson.GetBytes(content, "1.id").String(); got != "tool-a" {
		t.Fatalf("cached tool id = %q; content=%s", got, content)
	}
}

func TestKimiExecutorReplayHomeFailureStopsBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(_ *http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, nil
			},
		),
	)
	ctx = homekv.WithClientScope(
		ctx, homekv.ClientScope{
			ID:      "runtime-a",
			Enabled: true,
		},
	)
	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "kimi",
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}
	payload := []byte(`{"model":"kimi-k3","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "kimi-k3", Payload: payload}
	opts := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatClaude,
		OriginalRequest: payload,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "home-failure",
		},
	}
	if _, errExecute := executor.Execute(ctx, auth, req, opts); errExecute == nil ||
		!strings.Contains(errExecute.Error(), "thinking replay cache read failed") {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if _, errStream := executor.ExecuteStream(ctx, auth, req, opts); errStream == nil ||
		!strings.Contains(errStream.Error(), "thinking replay cache read failed") {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

func TestKimiExecutorClaudeHTTPReplayRejectionClearsOnly400And422(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		wantFound bool
	}{
		{name: "400 clears", status: http.StatusBadRequest},
		{name: "422 clears", status: http.StatusUnprocessableEntity},
		{name: "500 preserves", status: http.StatusInternalServerError, wantFound: true},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				internalcache.ClearKimiThinkingReplayCache()
				t.Cleanup(internalcache.ClearKimiThinkingReplayCache)
				var upstreamBody []byte
				ctx := kimiThinkingReplayLocalContext(context.Background(), "runtime-a")
				ctx = util.WithRoundTripper(
					ctx, kimiTokenRoundTripper(
						func(req *http.Request) (*http.Response, error) {
							body, errRead := io.ReadAll(req.Body)
							if errRead != nil {
								t.Fatal(errRead)
							}
							upstreamBody = append([]byte(nil), body...)
							return &http.Response{
								StatusCode: test.status,
								Header:     http.Header{"Content-Type": []string{"application/json"}},
								Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"rejected"}}`)),
							}, nil
						},
					),
				)
				payload := []byte(`{"model":"kimi-k3","max_tokens":32,"messages":[
				{"role":"assistant","content":` + kimiThinkingReplayTestRequestContent("a") + `},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-a","content":"ok"}]}
			]}`)
				req := cliproxyexecutor.Request{Model: "kimi-k3", Payload: payload}
				opts := cliproxyexecutor.Options{
					SourceFormat:    sdktranslator.FormatClaude,
					OriginalRequest: payload,
					Metadata: map[string]any{
						cliproxyexecutor.ExecutionSessionMetadataKey: "http-rejection",
					},
				}
				scope := kimiThinkingReplayScopeFromRequest(ctx, req, opts)
				seedKimiThinkingReplay(t, ctx, scope, kimiThinkingReplayTestContent("a"))
				executor := NewKimiExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{
					Provider:   "kimi",
					Attributes: map[string]string{},
					Metadata:   map[string]any{"access_token": "test-token"},
				}
				if _, errExecute := executor.Execute(ctx, auth, req, opts); errExecute == nil {
					t.Fatal("Execute() returned nil error")
				}
				if got := gjson.GetBytes(
					upstreamBody, "messages.0.content.0.signature",
				).String(); got != "signature-a" {
					t.Fatalf("replay was not applied before rejection: %q; body=%s", got, upstreamBody)
				}
				_, _, found, errGet := internalcache.GetKimiThinkingReplayWithSnapshotRequired(
					ctx,
					scope.modelFamily,
					scope.sessionKey,
				)
				if errGet != nil || found != test.wantFound {
					t.Fatalf(
						"cache after status %d = found %v, want %v, err %v", test.status, found, test.wantFound, errGet,
					)
				}
			},
		)
	}
}

func TestKimiExecutorClaudeStreamClearsOnlyRawUpstreamError(t *testing.T) {
	for _, test := range []struct {
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
			name:      "malformed stream preserves",
			stream:    `data: {not-json}`,
			wantFound: true,
		},
		{
			name:      "incomplete stream preserves",
			stream:    `data: {"type":"message_start","message":{"id":"msg","model":"k3"}}`,
			wantFound: true,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				internalcache.ClearKimiThinkingReplayCache()
				t.Cleanup(internalcache.ClearKimiThinkingReplayCache)
				var upstreamBody []byte
				ctx := kimiThinkingReplayLocalContext(context.Background(), "runtime-a")
				ctx = util.WithRoundTripper(
					ctx, kimiTokenRoundTripper(
						func(req *http.Request) (*http.Response, error) {
							body, errRead := io.ReadAll(req.Body)
							if errRead != nil {
								t.Fatal(errRead)
							}
							upstreamBody = append([]byte(nil), body...)
							return &http.Response{
								StatusCode: http.StatusOK,
								Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
								Body:       io.NopCloser(strings.NewReader(test.stream)),
							}, nil
						},
					),
				)
				payload := []byte(`{"model":"kimi-k3","max_tokens":32,"messages":[
				{"role":"assistant","content":` + kimiThinkingReplayTestRequestContent("a") + `},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-a","content":"ok"}]}
			]}`)
				req := cliproxyexecutor.Request{Model: "kimi-k3", Payload: payload}
				opts := cliproxyexecutor.Options{
					Stream:          true,
					SourceFormat:    sdktranslator.FormatClaude,
					OriginalRequest: payload,
					Metadata: map[string]any{
						cliproxyexecutor.ExecutionSessionMetadataKey: "stream-rejection",
					},
				}
				scope := kimiThinkingReplayScopeFromRequest(ctx, req, opts)
				seedKimiThinkingReplay(t, ctx, scope, kimiThinkingReplayTestContent("a"))
				executor := NewKimiExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{
					Provider:   "kimi",
					Attributes: map[string]string{},
					Metadata:   map[string]any{"access_token": "test-token"},
				}
				result, errExecute := executor.ExecuteStream(ctx, auth, req, opts)
				if errExecute != nil {
					t.Fatal(errExecute)
				}
				hasError := false
				for chunk := range result.Chunks {
					hasError = hasError || chunk.Err != nil
				}
				if !hasError {
					t.Fatal("invalid stream did not return an error chunk")
				}
				if got := gjson.GetBytes(
					upstreamBody, "messages.0.content.0.signature",
				).String(); got != "signature-a" {
					t.Fatalf("replay was not applied before stream failure: %q; body=%s", got, upstreamBody)
				}
				_, _, found, errGet := internalcache.GetKimiThinkingReplayWithSnapshotRequired(
					ctx,
					scope.modelFamily,
					scope.sessionKey,
				)
				if errGet != nil || found != test.wantFound {
					t.Fatalf("cache after stream = found %v, want %v, err %v", found, test.wantFound, errGet)
				}
			},
		)
	}
}

func TestKimiNonClaudePathsSkipThinkingReplayCache(t *testing.T) {
	newAuth := func() *cliproxyauth.Auth {
		return &cliproxyauth.Auth{
			Provider:   "kimi",
			Attributes: map[string]string{},
			Metadata:   map[string]any{"access_token": "test-token"},
		}
	}
	payload := []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hello"}]}`)

	t.Run(
		"OpenAI non-stream", func(t *testing.T) {
			var calls atomic.Int32
			ctx := util.WithRoundTripper(
				context.Background(), kimiTokenRoundTripper(
					func(_ *http.Request) (*http.Response, error) {
						calls.Add(1)
						return &http.Response{
							StatusCode: http.StatusInternalServerError,
							Header:     http.Header{"Content-Type": []string{"application/json"}},
							Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"expected"}}`)),
						}, nil
					},
				),
			)
			ctx = homekv.WithClientScope(ctx, homekv.ClientScope{ID: "runtime-a", Enabled: true})
			executor := NewKimiExecutor(&config.Config{})
			_, errExecute := executor.Execute(
				ctx,
				newAuth(),
				cliproxyexecutor.Request{Model: "kimi-k3", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: payload},
			)
			if errExecute == nil || strings.Contains(errExecute.Error(), "thinking replay cache") {
				t.Fatalf("Execute() error = %v", errExecute)
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1", calls.Load())
			}
		},
	)

	t.Run(
		"OpenAI stream", func(t *testing.T) {
			var calls atomic.Int32
			ctx := util.WithRoundTripper(
				context.Background(), kimiTokenRoundTripper(
					func(_ *http.Request) (*http.Response, error) {
						calls.Add(1)
						return &http.Response{
							StatusCode: http.StatusInternalServerError,
							Header:     http.Header{"Content-Type": []string{"application/json"}},
							Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"expected"}}`)),
						}, nil
					},
				),
			)
			ctx = homekv.WithClientScope(ctx, homekv.ClientScope{ID: "runtime-a", Enabled: true})
			executor := NewKimiExecutor(&config.Config{})
			_, errExecute := executor.ExecuteStream(
				ctx,
				newAuth(),
				cliproxyexecutor.Request{Model: "kimi-k3", Payload: payload},
				cliproxyexecutor.Options{
					Stream:          true,
					SourceFormat:    sdktranslator.FormatOpenAI,
					OriginalRequest: payload,
				},
			)
			if errExecute == nil || strings.Contains(errExecute.Error(), "thinking replay cache") {
				t.Fatalf("ExecuteStream() error = %v", errExecute)
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1", calls.Load())
			}
		},
	)

	t.Run(
		"count tokens", func(t *testing.T) {
			var calls atomic.Int32
			ctx := util.WithRoundTripper(
				context.Background(), kimiTokenRoundTripper(
					func(_ *http.Request) (*http.Response, error) {
						calls.Add(1)
						return &http.Response{
							StatusCode: http.StatusOK,
							Header:     make(http.Header),
							Body:       io.NopCloser(strings.NewReader(`{"input_tokens":7}`)),
						}, nil
					},
				),
			)
			ctx = homekv.WithClientScope(ctx, homekv.ClientScope{ID: "runtime-a", Enabled: true})
			executor := NewKimiExecutor(&config.Config{})
			response, errCount := executor.CountTokens(
				ctx,
				newAuth(),
				cliproxyexecutor.Request{Model: "kimi-k3", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
			)
			if errCount != nil {
				t.Fatal(errCount)
			}
			if calls.Load() != 1 || gjson.GetBytes(response.Payload, "input_tokens").Int() != 7 {
				t.Fatalf("CountTokens() calls=%d payload=%s", calls.Load(), response.Payload)
			}
		},
	)
}
