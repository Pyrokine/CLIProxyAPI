package executor

import (
	"context"
	"testing"

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestXAIC03ReplayDropsAssistantWhenToolAnchorHasNoMatchingOutput(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"new turn"}]}`)
	items := [][]byte{
		[]byte(`{"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{}"}`),
		[]byte(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"stale answer"}]}`),
	}

	if filtered := filterXAIReasoningReplayItemsForInput(body, items); len(filtered) != 0 {
		t.Fatalf("tool-less replay retained stale assistant items: %q", filtered)
	}
}

func TestXAIC03CompletedWithoutReplayableStateClearsPreviousReplay(t *testing.T) {
	internalcache.ClearXAIReasoningReplayCache()
	t.Cleanup(internalcache.ClearXAIReasoningReplayCache)

	scope := xaiReasoningReplayScope{modelName: "grok-4.3", sessionKey: "clear-previous"}
	reasoning := []byte(`{"type":"reasoning","summary":[],"encrypted_content":""}`)
	reasoning, _ = sjson.SetBytes(reasoning, "encrypted_content", validGrokEncryptedContentForExecutorTest())
	if !internalcache.XAIReasoningReplayItems(scope.modelName, scope.sessionKey, [][]byte{reasoning}) {
		t.Fatal("failed to seed previous replay")
	}

	cacheXAIReasoningReplayFromCompleted(
		context.Background(), scope, []byte(`{"response":{"output":[
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}
	]}}`),
	)
	if _, ok := internalcache.GetXAIReasoningReplayItems(scope.modelName, scope.sessionKey); ok {
		t.Fatal("non-replayable completed output retained previous replay")
	}
}

func TestXAIC03CompletedReplayUsesServiceScopedLocalState(t *testing.T) {
	internalcache.ClearXAIReasoningReplayCache()
	t.Cleanup(internalcache.ClearXAIReasoningReplayCache)

	scope := xaiReasoningReplayScope{modelName: "grok-4.3", sessionKey: "shared-session"}
	ctxA := home.WithClientScope(context.Background(), home.ClientScope{ID: "service-a"})
	ctxB := home.WithClientScope(context.Background(), home.ClientScope{ID: "service-b"})
	cacheXAIReasoningReplayFromCompleted(
		ctxA, scope, []byte(`{"response":{"output":[
		{"type":"function_call","call_id":"call_a","name":"service_a","arguments":"{}"}
	]}}`),
	)
	cacheXAIReasoningReplayFromCompleted(
		ctxB, scope, []byte(`{"response":{"output":[
		{"type":"function_call","call_id":"call_b","name":"service_b","arguments":"{}"}
	]}}`),
	)

	itemsA, okA, errA := internalcache.GetXAIReasoningReplayItemsRequired(ctxA, scope.modelName, scope.sessionKey)
	itemsB, okB, errB := internalcache.GetXAIReasoningReplayItemsRequired(ctxB, scope.modelName, scope.sessionKey)
	if errA != nil || !okA || len(itemsA) != 1 || gjson.GetBytes(itemsA[0], "name").String() != "service_a" {
		t.Fatalf("service A completed replay = %q ok %v err %v", itemsA, okA, errA)
	}
	if errB != nil || !okB || len(itemsB) != 1 || gjson.GetBytes(itemsB[0], "name").String() != "service_b" {
		t.Fatalf("service B completed replay = %q ok %v err %v", itemsB, okB, errB)
	}
}

func TestXAIC03NamespacedFunctionCallResponseCacheReplayRoundTrip(t *testing.T) {
	internalcache.ClearXAIReasoningReplayCache()
	t.Cleanup(internalcache.ClearXAIReasoningReplayCache)

	declarations := []byte(`{
		"tools":[{"type":"namespace","name":"ops","tools":[
			{"type":"function","name":"run","parameters":{"type":"object"}}
		]}],
		"input":[{"role":"user","content":"run it"}]
	}`)
	responseNormalizer := newXAIToolIdentityNormalizer(declarations).newResponseNormalizer(false)
	completed := responseNormalizer.normalize(
		[]byte(`{"type":"response.completed","response":{"output":[
		{"type":"function_call","call_id":"call_1","name":"ops__run","arguments":"{}"}
	]}}`),
	)
	if got := gjson.GetBytes(completed, "response.output.0.namespace").String(); got != "ops" {
		t.Fatalf("normalized response namespace = %q, want ops; response=%s", got, completed)
	}

	scope := xaiReasoningReplayScope{modelName: "grok-4.3", sessionKey: "namespace-round-trip"}
	cacheXAIReasoningReplayFromCompleted(context.Background(), scope, completed)
	cached, ok := internalcache.GetXAIReasoningReplayItems(scope.modelName, scope.sessionKey)
	if !ok || len(cached) != 1 {
		t.Fatalf("cached items = %q ok %v, want one function call", cached, ok)
	}
	if got := gjson.GetBytes(cached[0], "namespace").String(); got != "ops" {
		t.Fatalf("cached namespace = %q, want ops; item=%s", got, cached[0])
	}

	nextBody := []byte(`{
		"tools":[{"type":"namespace","name":"ops","tools":[
			{"type":"function","name":"run","parameters":{"type":"object"}}
		]}],
		"input":[
			{"type":"function_call_output","call_id":"call_1","output":"ok"},
			{"role":"user","content":"continue"}
		]
	}`)
	nextNormalizer := newXAIToolIdentityNormalizer(nextBody)
	nextBody, err := nextNormalizer.normalizeDeclarations(nextBody)
	if err != nil {
		t.Fatalf("normalizeDeclarations() error = %v", err)
	}
	filtered := filterXAIReasoningReplayItemsForInput(nextBody, cached)
	if len(filtered) != 1 {
		t.Fatalf("filtered replay items = %q, want one function call", filtered)
	}
	nextBody, ok = insertCodexReasoningReplayItems(nextBody, filtered)
	if !ok {
		t.Fatal("insertCodexReasoningReplayItems() failed")
	}
	nextBody = nextNormalizer.normalizeInputHistory(nextBody)

	var replayed gjson.Result
	for _, item := range gjson.GetBytes(nextBody, "input").Array() {
		if item.Get("type").String() == "function_call" && item.Get("call_id").String() == "call_1" {
			replayed = item
			break
		}
	}
	if !replayed.Exists() {
		t.Fatalf("replayed function call missing: %s", nextBody)
	}
	if got := replayed.Get("name").String(); got != "ops__run" {
		t.Fatalf("replayed function name = %q, want ops__run; body=%s", got, nextBody)
	}
	if replayed.Get("namespace").Exists() {
		t.Fatalf("replayed namespace was not flattened: %s", nextBody)
	}
}
