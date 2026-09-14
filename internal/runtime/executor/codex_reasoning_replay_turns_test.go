package executor

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type noCASCodexReplayHomeClient struct{}

func (noCASCodexReplayHomeClient) Enabled() bool {
	return true
}

func (noCASCodexReplayHomeClient) HeartbeatOK() bool {
	return true
}

func (noCASCodexReplayHomeClient) KVGet(
	context.Context,
	string,
) ([]byte, bool, error) {
	return nil, false, nil
}

func (noCASCodexReplayHomeClient) KVSet(
	context.Context,
	string,
	[]byte,
	home.KVSetOptions,
) (bool, error) {
	return true, nil
}

func (noCASCodexReplayHomeClient) KVDel(context.Context, ...string) (int64, error) {
	return 0, nil
}

func (noCASCodexReplayHomeClient) KVExpire(
	context.Context,
	string,
	time.Duration,
) (bool, error) {
	return true, nil
}

func validCodexReplayEncryptedContentForTest(seed byte) string {
	payload := make([]byte, 1+8+16+16+32)
	payload[0] = 0x80
	for index := 9; index < len(payload); index++ {
		payload[index] = seed + byte(index)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func TestCodexReasoningReplaySessionKeyIsolatesClaudeCodeAgents(t *testing.T) {
	from := sdktranslator.FromString("claude")
	req := cliproxyexecutor.Request{
		Model:   "local-alias-high",
		Payload: []byte(`{"model":"local-alias","messages":[{"role":"user","content":"next"}]}`),
	}
	body := []byte(`{"model":"gpt-5.4","prompt_cache_key":"shared-client-key","input":[{"type":"message","role":"user","content":"next"}]}`)
	rootHeaders := http.Header{}
	rootHeaders.Set("X-Claude-Code-Session-Id", "session-agents")
	childAHeaders := rootHeaders.Clone()
	childAHeaders.Set("X-Claude-Code-Agent-Id", "agent-a")
	childBHeaders := rootHeaders.Clone()
	childBHeaders.Set("X-Claude-Code-Agent-Id", "agent-b")
	metadata := map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "shared-execution-session"}

	root := codexReasoningReplayScopeFromRequest(
		context.Background(),
		from,
		req,
		cliproxyexecutor.Options{SourceFormat: from, Headers: rootHeaders, Metadata: metadata},
		body,
	)
	childA := codexReasoningReplayScopeFromRequest(
		context.Background(),
		from,
		req,
		cliproxyexecutor.Options{SourceFormat: from, Headers: childAHeaders, Metadata: metadata},
		body,
	)
	childB := codexReasoningReplayScopeFromRequest(
		context.Background(),
		from,
		req,
		cliproxyexecutor.Options{SourceFormat: from, Headers: childBHeaders, Metadata: metadata},
		body,
	)
	if root.modelName != "gpt-5.4" || childA.modelName != "gpt-5.4" || childB.modelName != "gpt-5.4" {
		t.Fatalf("replay scopes did not use resolved model: root=%#v a=%#v b=%#v", root, childA, childB)
	}
	if root.sessionKey != "claude:session-agents:agent:main" {
		t.Fatalf("root session key = %q", root.sessionKey)
	}
	if root.sessionKey == childA.sessionKey || childA.sessionKey == childB.sessionKey || root.sessionKey == childB.sessionKey {
		t.Fatalf("agent replay scopes are not isolated: root=%#v a=%#v b=%#v", root, childA, childB)
	}
}

func TestCodexPromptCacheFromClaudeRequestIsolatesAgents(t *testing.T) {
	payload := []byte(`{"model":"local-alias","messages":[{"role":"user","content":"next"}]}`)
	rootHeaders := http.Header{"X-Claude-Code-Session-Id": []string{"session-cache"}}
	childHeaders := rootHeaders.Clone()
	childHeaders.Set("X-Claude-Code-Agent-Id", "agent-a")

	rootFirst, rootOK := codexPromptCacheFromClaudeRequest(
		context.Background(),
		"gpt-5.4-high",
		payload,
		rootHeaders,
	)
	rootSecond, secondOK := codexPromptCacheFromClaudeRequest(
		context.Background(),
		"gpt-5.4-high",
		payload,
		rootHeaders,
	)
	child, childOK := codexPromptCacheFromClaudeRequest(
		context.Background(),
		"gpt-5.4-high",
		payload,
		childHeaders,
	)
	if !rootOK || !secondOK || !childOK {
		t.Fatal("prompt cache scope was not generated")
	}
	if rootFirst.ID != rootSecond.ID {
		t.Fatalf("root prompt cache key changed: %q != %q", rootFirst.ID, rootSecond.ID)
	}
	if rootFirst.ID == child.ID {
		t.Fatalf("root and child prompt cache keys are equal: %q", rootFirst.ID)
	}
}

func TestCodexPromptCacheMatchesAcrossHTTPAndWebsocket(t *testing.T) {
	executor := &codexExecutor{}
	url := "https://example.com/responses"
	req := cliproxyexecutor.Request{
		Model:   "requested-alias-high",
		Payload: []byte(`{"model":"requested-alias","messages":[{"role":"user","content":"hello"}]}`),
	}
	headers := http.Header{
		"X-Claude-Code-Session-Id": []string{"resolved-model-session"},
		"X-Claude-Code-Agent-Id":   []string{"agent-a"},
	}
	rawJSON := []byte(`{"model":"gpt-5.4","stream":true}`)

	httpRequest, _, _, err := executor.cacheHelper(
		context.Background(),
		sdktranslator.FromString("claude"),
		url,
		nil,
		req,
		req.Payload,
		rawJSON,
		headers,
	)
	if err != nil {
		t.Fatalf("cacheHelper() error = %v", err)
	}
	httpBody, err := io.ReadAll(httpRequest.Body)
	if err != nil {
		t.Fatalf("read HTTP body: %v", err)
	}
	httpKey := gjson.GetBytes(httpBody, "prompt_cache_key").String()

	aliasReq := req
	aliasReq.Model = "another-local-alias-low"
	websocketBody, _ := applyCodexPromptCacheHeadersWithContext(
		context.Background(),
		sdktranslator.FromString("claude"),
		aliasReq,
		rawJSON,
		headers,
	)
	websocketKey := gjson.GetBytes(websocketBody, "prompt_cache_key").String()
	if httpKey == "" || websocketKey == "" || httpKey != websocketKey {
		t.Fatalf("HTTP/WebSocket prompt keys differ: http=%q websocket=%q", httpKey, websocketKey)
	}
}

func TestCodexReasoningReplayRestoresCumulativeToolTurns(t *testing.T) {
	internalcache.ClearCodexReasoningReplayCache()
	t.Cleanup(internalcache.ClearCodexReasoningReplayCache)
	ctx := home.WithClientScope(
		context.Background(),
		home.ClientScope{ID: "executor-test", Enabled: false},
	)
	scope := codexReasoningReplayScope{
		modelName:  "gpt-5.4",
		sessionKey: "claude:session-cumulative-tools:agent:main",
	}
	firstEncrypted := validCodexReplayEncryptedContentForTest(21)
	secondEncrypted := validCodexReplayEncryptedContentForTest(22)
	cacheCodexReasoningReplayFromCompleted(
		ctx, scope, []byte(`{"response":{"output":[`+
			`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"`+firstEncrypted+`"},`+
			`{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"first\"}"}`+
			`]}}`),
	)
	cacheCodexReasoningReplayFromCompleted(
		ctx, scope, []byte(`{"response":{"output":[`+
			`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"`+secondEncrypted+`"},`+
			`{"type":"function_call","call_id":"call_2","name":"lookup","arguments":"{\"q\":\"second\"}"}`+
			`]}}`),
	)

	body := []byte(`{"model":"gpt-5.4","input":[` +
		`{"type":"message","role":"user","content":"first"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"one"},` +
		`{"type":"message","role":"user","content":"second"},` +
		`{"type":"function_call_output","call_id":"call_2","output":"two"},` +
		`{"type":"message","role":"user","content":"third"}` +
		`]}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"metadata":{"user_id":"{\"session_id\":\"session-cumulative-tools\"}"}}`),
	}
	updated, gotScope, err := applyCodexReasoningReplayCacheRequired(
		ctx,
		sdktranslator.FromString("claude"),
		req,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
		body,
	)
	if err != nil {
		t.Fatalf("applyCodexReasoningReplayCacheRequired() error = %v", err)
	}
	if gotScope.modelName != scope.modelName || gotScope.sessionKey != scope.sessionKey {
		t.Fatalf("replay scope = %#v, want model/session %#v", gotScope, scope)
	}
	wantTypes := []string{
		"message",
		"reasoning",
		"function_call",
		"function_call_output",
		"message",
		"reasoning",
		"function_call",
		"function_call_output",
		"message",
	}
	gotItems := gjson.GetBytes(updated, "input").Array()
	if len(gotItems) != len(wantTypes) {
		t.Fatalf("input length = %d, want %d; body=%s", len(gotItems), len(wantTypes), updated)
	}
	for index, wantType := range wantTypes {
		if gotType := gotItems[index].Get("type").String(); gotType != wantType {
			t.Fatalf("input.%d.type = %q, want %q; body=%s", index, gotType, wantType, updated)
		}
	}
	if gotItems[1].Get("encrypted_content").String() != firstEncrypted ||
		gotItems[5].Get("encrypted_content").String() != secondEncrypted {
		t.Fatalf("cumulative reasoning was not restored in turn order: %s", updated)
	}
}

func TestCodexReasoningReplaySkipsDetachedTurnAfterCompaction(t *testing.T) {
	internalcache.ClearCodexReasoningReplayCache()
	t.Cleanup(internalcache.ClearCodexReasoningReplayCache)
	ctx := home.WithClientScope(
		context.Background(),
		home.ClientScope{ID: "executor-test", Enabled: false},
	)
	scope := codexReasoningReplayScope{
		modelName:  "gpt-5.4",
		sessionKey: "claude:session-compacted:agent:main",
	}
	detachedEncrypted := validCodexReplayEncryptedContentForTest(25)
	retainedEncrypted := validCodexReplayEncryptedContentForTest(26)
	cacheCodexReasoningReplayFromCompleted(
		ctx, scope, []byte(`{"response":{"output":[`+
			`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"`+detachedEncrypted+`"},`+
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"removed answer"}]}`+
			`]}}`),
	)
	cacheCodexReasoningReplayFromCompleted(
		ctx, scope, []byte(`{"response":{"output":[`+
			`{"type":"reasoning","summary":[],"content":null,"encrypted_content":"`+retainedEncrypted+`"},`+
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"retained answer"}]}`+
			`]}}`),
	)

	body := []byte(`{"model":"gpt-5.4","input":[` +
		`{"type":"message","role":"user","content":"compacted summary"},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"retained answer"}]},` +
		`{"type":"message","role":"user","content":"continue"}` +
		`]}`)
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"metadata":{"user_id":"{\"session_id\":\"session-compacted\"}"}}`),
	}
	updated, _, err := applyCodexReasoningReplayCacheRequired(
		ctx,
		sdktranslator.FromString("claude"),
		req,
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
		body,
	)
	if err != nil {
		t.Fatalf("applyCodexReasoningReplayCacheRequired() error = %v", err)
	}
	gotItems := gjson.GetBytes(updated, "input").Array()
	if len(gotItems) != 4 || gotItems[1].Get("encrypted_content").String() != retainedEncrypted {
		t.Fatalf("retained turn reasoning was not restored: %s", updated)
	}
	for _, item := range gotItems {
		if item.Get("encrypted_content").String() == detachedEncrypted {
			t.Fatalf("detached reasoning moved into compacted history: %s", updated)
		}
	}
}

func TestCodexReplayPrefixFingerprintsMatchesDirectComputation(t *testing.T) {
	items := []gjson.Result{
		gjson.Parse(`{"type":"message","role":"user","content":"a"}`),
		gjson.Parse(`{"type":"reasoning","encrypted_content":"abc"}`),
		gjson.Parse(`{"type":"function_call","call_id":"call_1"}`),
		gjson.Parse(`{"type":"function_call_output","call_id":"call_1","output":"ok"}`),
	}
	fingerprints := newCodexReplayPrefixFingerprints(items)
	for _, end := range []int{4, 2, 0, 3, 1, 4, 2} {
		want := codexReplayInputPrefixFingerprint(items, end)
		if got := fingerprints.at(end); got != want {
			t.Fatalf("fingerprints.at(%d) = %q, want %q", end, got, want)
		}
	}
	for _, end := range []int{-1, 5} {
		if got := fingerprints.at(end); got != "" {
			t.Fatalf("fingerprints.at(%d) = %q, want empty", end, got)
		}
	}
}

func TestCodexExecutorsPropagateRequiredReplayHomeFailure(t *testing.T) {
	ctx := home.WithClientScope(
		context.Background(),
		home.ClientScope{
			Client:  noCASCodexReplayHomeClient{},
			ID:      "home-required",
			Enabled: true,
		},
	)
	from := sdktranslator.FromString("claude")
	req := cliproxyexecutor.Request{
		Model: "gpt-5.4",
		Payload: []byte(`{
			"model":"gpt-5.4",
			"metadata":{"user_id":"{\"session_id\":\"session-required\"}"},
			"messages":[{"role":"user","content":"next"}]
		}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: from}
	auth := &cliproxyauth.Auth{
		ID: "auth-required-replay",
		Attributes: map[string]string{
			"base_url": "http://127.0.0.1:1",
			"api_key":  "test",
		},
	}
	httpExecutor := newCodexExecutor(&config.Config{})
	websocketExecutor := newCodexWebsocketsExecutor(&config.Config{})
	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "http_non_stream",
			call: func() error {
				_, err := httpExecutor.Execute(ctx, auth, req, opts)
				return err
			},
		},
		{
			name: "http_stream",
			call: func() error {
				_, err := httpExecutor.ExecuteStream(ctx, auth, req, opts)
				return err
			},
		},
		{
			name: "websocket_non_stream",
			call: func() error {
				_, err := websocketExecutor.Execute(ctx, auth, req, opts)
				return err
			},
		},
		{
			name: "websocket_stream",
			call: func() error {
				_, err := websocketExecutor.ExecuteStream(ctx, auth, req, opts)
				return err
			},
		},
	}
	for _, testCase := range tests {
		t.Run(
			testCase.name, func(t *testing.T) {
				err := testCase.call()
				if !errors.Is(err, home.ErrCompareAndSwapUnsupported) {
					t.Fatalf("error = %v, want %v", err, home.ErrCompareAndSwapUnsupported)
				}
			},
		)
	}
}
