package executor

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestXAIExecutorKeepsEachReplayProofBesideItsToolCall(t *testing.T) {
	first := validGrokEncryptedContentForExecutorTest()
	second := "A" + first[1:]
	completed := []byte(fmt.Sprintf(`{"response":{"output":[{"type":"reasoning","summary":[],"encrypted_content":%q},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"reasoning","summary":[],"encrypted_content":%q},{"type":"function_call","call_id":"call_2","name":"lookup","arguments":"{}"}]}}`, first, second))
	for _, tc := range []struct {
		name string
		format sdktranslator.Format
		body string
	}{
		{"chat", sdktranslator.FormatOpenAI, `{"prompt_cache_key":"fixture-chat","messages":[{"role":"user","content":"earlier"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"a"},{"role":"tool","tool_call_id":"call_2","content":"b"},{"role":"user","content":"continue"}]}`},
		{"messages", sdktranslator.FormatClaude, `{"prompt_cache_key":"fixture-messages","messages":[{"role":"user","content":"earlier"},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{}},{"type":"tool_use","id":"call_2","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"a"},{"type":"tool_result","tool_use_id":"call_2","content":"b"},{"type":"text","text":"continue"}]}]}`},
		{"responses", sdktranslator.FormatOpenAIResponse, `{"prompt_cache_key":"fixture-responses","input":[{"role":"user","content":"earlier"},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call","call_id":"call_2","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"a"},{"type":"function_call_output","call_id":"call_2","output":"b"},{"role":"user","content":"continue"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			internalcache.ClearXAIReasoningReplayCache()
			t.Cleanup(internalcache.ClearXAIReasoningReplayCache)
			ctx := xaiC03ContextWithAPIKey("proof-pairing-offline-caller")
			auth := &cliproxyauth.Auth{Provider: "xai", Attributes: map[string]string{"api_key": "offline-fixture", "base_url": "https://xai.test"}}
			req := cliproxyexecutor.Request{Model: "grok-4.6", Payload: []byte(tc.body)}
			opts := cliproxyexecutor.Options{SourceFormat: tc.format, OriginalRequest: []byte(tc.body)}
			scope := xaiReasoningReplayScopeFromRequest(ctx, auth, tc.format, req, opts, req.Payload)
			if !scope.valid() {
				t.Fatal("invalid replay scope")
			}
			cacheXAIReasoningReplayFromCompleted(ctx, scope, completed)
			cached, exists, errCache := internalcache.GetXAIReasoningReplayItemsRequired(ctx, scope.modelName, scope.sessionKey)
			if errCache != nil || !exists || len(cached) != 4 {
				t.Fatalf("seed failed: items=%d exists=%t error=%v", len(cached), exists, errCache)
			}
			var sent []byte
			ctx = util.WithRoundTripper(ctx, xaiMediaRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.URL.Host != "xai.test" {
					return nil, fmt.Errorf("unexpected host %s", request.URL.Host)
				}
				var err error
				sent, err = io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				const reply = `data: {"type":"response.completed","response":{"id":"fixture-response","status":"completed","model":"grok-4.6","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader(reply + "\n\n")), Request: request}, nil
			}))
			if _, err := newXAIExecutor(&config.Config{}).Execute(ctx, auth, req, opts); err != nil {
				t.Fatal(err)
			}
			input := gjson.GetBytes(sent, "input").Array()
			proofs, calls, outputs := 0, 0, 0
			for index, item := range input {
				switch item.Get("type").String() {
				case "reasoning":
					proofs++
				case "function_call":
					calls++
					id := item.Get("call_id").String()
					want := first
					if id == "call_2" {
						want = second
					}
					if index == 0 || input[index-1].Get("encrypted_content").String() != want {
						t.Errorf("%s lost its preceding proof", id)
					}
				case "function_call_output":
					outputs++
				}
			}
			if len(input) == 0 || input[0].Get("role").String() != "user" {
				t.Error("proofs moved before the first user turn")
			}
			if proofs != 2 || calls != 2 || outputs != 2 {
				t.Errorf("proofs=%d calls=%d outputs=%d", proofs, calls, outputs)
			}
		})
	}
}
