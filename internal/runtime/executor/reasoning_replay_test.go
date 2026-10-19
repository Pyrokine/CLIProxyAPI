package executor

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"testing"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func validGrokEncryptedContentForExecutorTest() string {
	payload := make([]byte, 0, 128)
	for i := byte(0); len(payload) < 128; i++ {
		sum := sha256.Sum256([]byte{i, i + 31, i + 79})
		payload = append(payload, sum[:]...)
	}
	payload = payload[:128]
	return base64.RawStdEncoding.EncodeToString(payload)
}

func TestSanitizeXAIInputEncryptedContentDropsForeignReasoningAndCompaction(t *testing.T) {
	valid := validGrokEncryptedContentForExecutorTest()
	body := []byte(fmt.Sprintf(
		`{
		"input":[
			{"type":"reasoning","encrypted_content":"gAAAAforeign-signature","summary":[]},
			{"type":"compaction","encrypted_content":"claude-foreign-signature"},
			{"type":"reasoning","encrypted_content":%q,"summary":[]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`, valid,
	))

	out := sanitizeXAIInputEncryptedContent(context.Background(), body)

	if gjson.GetBytes(out, "input.0.encrypted_content").Exists() {
		t.Fatalf("foreign reasoning encrypted_content was not removed: %s", string(out))
	}
	if got := gjson.GetBytes(out, "input.1.type").String(); got != "reasoning" {
		t.Fatalf("invalid compaction item was not removed, input.1.type = %q body=%s", got, string(out))
	}
	if got := gjson.GetBytes(out, "input.1.encrypted_content").String(); got != valid {
		t.Fatalf("valid Grok encrypted_content = %q, want preserved", got)
	}
}

func TestApplyXAIReasoningReplayCacheInsertsCachedReasoningAndToolCall(t *testing.T) {
	internalcache.ClearXAIReasoningReplayCache()
	t.Cleanup(internalcache.ClearXAIReasoningReplayCache)

	valid := validGrokEncryptedContentForExecutorTest()
	model := "grok-4.3"
	body := []byte(`{
		"model":"grok-4.3",
		"input":[
			{"type":"function_call_output","call_id":"call_1","output":"ok"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)
	req := cliproxyexecutor.Request{Model: model, Payload: body}
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "xai-session-1"}}
	wantScope := xaiReasoningReplayScopeFromRequest(context.Background(), xaiReplayTestAuth, sdktranslator.FormatClaude, req, opts, body)
	if !wantScope.valid() {
		t.Fatal("expected credential-scoped replay")
	}
	if ok := internalcache.XAIReasoningReplayItems(
		model, wantScope.sessionKey, [][]byte{
			[]byte(fmt.Sprintf(
				`{"type":"reasoning","encrypted_content":%q,"summary":[{"type":"summary_text","text":"old"}]}`, valid,
			)),
			[]byte(`{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}`),
		},
	); !ok {
		t.Fatal("XAIReasoningReplayItems returned false")
	}
	updated, scope := applyXAIReasoningReplayCache(
		context.Background(), xaiReplayTestAuth, sdktranslator.FormatClaude, req, opts, body,
	)

	if !scope.valid() {
		t.Fatal("expected valid replay scope")
	}
	if got := gjson.GetBytes(updated, "input.0.type").String(); got != "reasoning" {
		t.Fatalf("input.0.type = %q, want reasoning; body=%s", got, string(updated))
	}
	if got := gjson.GetBytes(updated, "input.0.encrypted_content").String(); got != valid {
		t.Fatalf("replayed encrypted_content = %q, want valid Grok signature", got)
	}
	if got := gjson.GetBytes(updated, "input.1.type").String(); got != "function_call" {
		t.Fatalf("input.1.type = %q, want function_call; body=%s", got, string(updated))
	}
	if got := gjson.GetBytes(updated, "input.2.type").String(); got != "function_call_output" {
		t.Fatalf("input.2.type = %q, want function_call_output; body=%s", got, string(updated))
	}
}

func TestAntigravityReasoningReplayCachesFunctionCallBeforeFunctionResponse(t *testing.T) {
	internalcache.ClearAntigravityReasoningReplayCache()
	t.Cleanup(internalcache.ClearAntigravityReasoningReplayCache)

	model := "gemini-3-pro"
	requestPayload := []byte(`{"sessionId":"ag-session-1","request":{"contents":[{"role":"user","parts":[{"text":"call tool"}]}]}}`)
	scope := antigravityReasoningReplayScopeFromPayload(model, requestPayload)
	if !scope.valid() {
		t.Fatal("expected valid Antigravity replay scope")
	}
	responsePayload := []byte(`{
		"response":{"candidates":[{"finishReason":"STOP","content":{"parts":[
			{"functionCall":{"id":"call_1","name":"read_file","args":{"path":"a.txt"}},"thoughtSignature":"thought_signature_1234567890"}
		]}}]}
	}`)
	cacheAntigravityReasoningReplayFromResponse(context.Background(), scope, requestPayload, responsePayload)

	nextPayload := []byte(`{
		"sessionId":"ag-session-1",
		"request":{"contents":[
			{"role":"user","parts":[{"text":"call tool"}]},
			{"role":"user","parts":[{"functionResponse":{"id":"call_1","name":"read_file","response":{"content":"ok"}}}]}
		]}
	}`)
	updated, nextScope, errReplay := prepareAntigravityGeminiReasoningReplayPayload(
		context.Background(), model, cliproxyexecutor.Request{Model: model, Payload: nextPayload},
		cliproxyexecutor.Options{}, nextPayload,
	)
	if errReplay != nil {
		t.Fatal(errReplay)
	}

	if !nextScope.valid() {
		t.Fatal("expected valid next replay scope")
	}
	if got := gjson.GetBytes(updated, "request.contents.1.role").String(); got != "model" {
		t.Fatalf("request.contents.1.role = %q, want model; body=%s", got, string(updated))
	}
	if got := gjson.GetBytes(updated, "request.contents.1.parts.0.functionCall.id").String(); got != "call_1" {
		t.Fatalf("replayed functionCall id = %q, want call_1; body=%s", got, string(updated))
	}
	if got := gjson.GetBytes(
		updated, "request.contents.1.parts.0.thoughtSignature",
	).String(); got != "thought_signature_1234567890" {
		t.Fatalf("replayed thoughtSignature = %q", got)
	}
	if got := gjson.GetBytes(updated, "request.contents.2.parts.0.functionResponse.id").String(); got != "call_1" {
		t.Fatalf("functionResponse moved unexpectedly, id = %q body=%s", got, string(updated))
	}
}
