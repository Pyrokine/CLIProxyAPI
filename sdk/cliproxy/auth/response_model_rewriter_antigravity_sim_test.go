package auth

import (
	"bytes"
	"slices"
	"testing"

	gemresponses "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/gemini/openai/responses"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func antigravityLiveSSEChunks(t *testing.T) [][]byte {
	t.Helper()
	rawOK := `{"response": {"candidates": [{"content": {"role": "model",` +
		`"parts": [{"text": "OK"}]}}],"usageMetadata": {"promptTokenCount": 21,` +
		`"candidatesTokenCount": 1,"totalTokenCount": 131,"thoughtsTokenCount": 109},` +
		`"modelVersion": "gemini-3-flash-a","responseId": "tjVCavaJBYjgz7IP-NnfSQ"},` +
		`"traceId": "x","metadata": {}}`
	rawStop := `{"response": {"candidates": [{"content": {"role": "model",` +
		`"parts": [{"thoughtSignature": "sig","text": ""}]},"finishReason": "STOP"}],` +
		`"usageMetadata": {"promptTokenCount": 21,"candidatesTokenCount": 1,` +
		`"totalTokenCount": 131,"thoughtsTokenCount": 109},` +
		`"modelVersion": "gemini-3-flash-a","responseId": "tjVCavaJBYjgz7IP-NnfSQ"},` +
		`"traceId": "x","metadata": {}}`
	request := []byte(`{"model":"gemini-3.5-flash","input":[]}`)
	var param any
	var chunks [][]byte
	for _, raw := range []string{rawOK, rawStop} {
		chunks = append(
			chunks, gemresponses.ConvertGeminiResponseToOpenAIResponses(
				t.Context(), "gemini-3.5-flash", request, request, []byte("data: "+raw), &param,
			)...,
		)
	}
	if len(chunks) == 0 {
		t.Fatal("translator produced no chunks")
	}
	return chunks
}

func TestAntigravityTranslatorEmitsCompletedWithoutRewriter(t *testing.T) {
	var types []string
	for _, chunk := range antigravityLiveSSEChunks(t) {
		types = append(types, parseResponsesWSDataEventTypes(chunk)...)
	}
	if !slices.Contains(types, "response.completed") {
		t.Fatalf("translator event types = %v", types)
	}
}

func TestRewriteForceMappedStreamChunkAntigravityTranslatorEventChunksPreservesCompleted(t *testing.T) {
	chunks := antigravityLiveSSEChunks(t)
	rewriter := NewStreamRewriter(StreamRewriteOptions{RewriteModel: "gemini-3.5-flash"})
	var want, got []string
	for _, chunk := range chunks {
		want = append(want, parseResponsesWSDataEventTypes(chunk)...)
		got = append(got, parseResponsesWSDataEventTypes(rewriteForceMappedStreamChunk(rewriter, chunk))...)
	}
	got = append(got, parseResponsesWSDataEventTypes(finishForceMappedStreamChunks(rewriter))...)
	if !slices.Equal(got, want) || !slices.Contains(got, "response.completed") {
		t.Fatalf("forwarded event types = %v, want %v", got, want)
	}
}

func TestRewriteForceMappedStreamChunkAntigravityGluedEventFramesFlushCompleted(t *testing.T) {
	chunks := antigravityLiveSSEChunks(t)
	var want []string
	for index, chunk := range chunks {
		want = append(want, parseResponsesWSDataEventTypes(chunk)...)
		chunks[index] = bytes.Trim(chunk, "\r\n")
	}
	rewriter := NewStreamRewriter(StreamRewriteOptions{RewriteModel: "gemini-3.5-flash"})
	out := rewriteForceMappedStreamChunk(rewriter, bytes.Join(chunks, nil))
	out = append(out, finishForceMappedStreamChunks(rewriter)...)
	if got := parseResponsesWSDataEventTypes(out); !slices.Equal(got, want) ||
		!slices.Contains(got, "response.completed") {
		t.Fatalf("forwarded event types = %v, want %v", got, want)
	}
	if tail := finishForceMappedStreamChunks(rewriter); len(tail) != 0 {
		t.Fatalf("completed events delivered again: %q", tail)
	}
}

func TestForceMappedManagerPreservesGluedCompletion(t *testing.T) {
	remaining := make(chan cliproxyexecutor.StreamChunk)
	close(remaining)
	manager := NewManager(nil, nil, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	input := []byte(`data: {"type":"response.created","response":{"model":"gpt-5.4"}}` +
		`data: {"type":"response.completed","response":{"model":"gpt-5.4","output":[]}}`)
	result := manager.wrapStreamResultWithOptions(
		t.Context(), t.Context(), &Auth{ID: t.Name(), Provider: "fixture"},
		"fixture", "gpt-5.4", cliproxyexecutor.Options{}, nil,
		[]cliproxyexecutor.StreamChunk{{Payload: input}}, remaining,
		OAuthModelAliasResult{ForceMapping: true, OriginalAlias: "gpt-5.4-fast"},
	)
	var types []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		if !bytes.Contains(chunk.Payload, []byte(`"model":"gpt-5.4-fast"`)) ||
			bytes.Contains(chunk.Payload, []byte(`"model":"gpt-5.4"`)) {
			t.Fatalf("managed chunk model = %q", chunk.Payload)
		}
		types = append(types, parseResponsesWSDataEventTypes(chunk.Payload)...)
	}
	if !slices.Equal(types, []string{"response.created", "response.completed"}) {
		t.Fatalf("managed event types = %v", types)
	}
}
