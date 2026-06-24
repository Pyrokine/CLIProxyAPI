package amp

import (
	"strings"
	"testing"
)

func TestRewriteModelInResponse_TopLevel(t *testing.T) {
	rw := &responseRewriter{originalModel: "gpt-5.2-codex"}

	input := []byte(`{"id":"resp_1","model":"gpt-5.3-codex","output":[]}`)
	result := rw.rewriteModelInResponse(input)

	expected := `{"id":"resp_1","model":"gpt-5.2-codex","output":[]}`
	if string(result) != expected {
		t.Errorf("expected %s, got %s", expected, string(result))
	}
}

func TestRewriteModelInResponse_ResponseModel(t *testing.T) {
	rw := &responseRewriter{originalModel: "gpt-5.2-codex"}

	input := []byte(
		`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.3-codex","status":"completed"}}`,
	)
	result := rw.rewriteModelInResponse(input)

	expected := `{"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.2-codex","status":"completed"}}`
	if string(result) != expected {
		t.Errorf("expected %s, got %s", expected, string(result))
	}
}

func TestRewriteModelInResponse_ResponseCreated(t *testing.T) {
	rw := &responseRewriter{originalModel: "gpt-5.2-codex"}

	input := []byte(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.3-codex","status":"in_progress"}}`,
	)
	result := rw.rewriteModelInResponse(input)

	expected := `{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.2-codex","status":"in_progress"}}`
	if string(result) != expected {
		t.Errorf("expected %s, got %s", expected, string(result))
	}
}

func TestRewriteModelInResponse_NoModelField(t *testing.T) {
	rw := &responseRewriter{originalModel: "gpt-5.2-codex"}

	input := []byte(`{"type":"response.output_item.added","item":{"id":"item_1","type":"message"}}`)
	result := rw.rewriteModelInResponse(input)

	if string(result) != string(input) {
		t.Errorf("expected no modification, got %s", string(result))
	}
}

func TestRewriteModelInResponse_EmptyOriginalModel(t *testing.T) {
	rw := &responseRewriter{originalModel: ""}

	input := []byte(`{"model":"gpt-5.3-codex"}`)
	result := rw.rewriteModelInResponse(input)

	if string(result) != string(input) {
		t.Errorf("expected no modification when originalModel is empty, got %s", string(result))
	}
}

func TestRewriteStreamChunk_SSEWithResponseModel(t *testing.T) {
	rw := &responseRewriter{originalModel: "gpt-5.2-codex"}

	chunk := []byte(
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.3-codex\",\"status\":\"completed\"}}\n\n",
	)
	result := rw.rewriteStreamChunk(chunk)

	expected := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.2-codex\",\"status\":\"completed\"}}\n\n"
	if string(result) != expected {
		t.Errorf("expected %s, got %s", expected, string(result))
	}
}

func TestRewriteStreamChunk_MultipleEvents(t *testing.T) {
	rw := &responseRewriter{originalModel: "gpt-5.2-codex"}

	chunk := []byte(
		"data: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-5.3-codex\"}}\n\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"item_1\"}}\n\n",
	)
	result := rw.rewriteStreamChunk(chunk)

	if string(result) == string(chunk) {
		t.Error("expected response.model to be rewritten in SSE stream")
	}
	if !contains(result, []byte(`"model":"gpt-5.2-codex"`)) {
		t.Errorf("expected rewritten model in output, got %s", string(result))
	}
}

func TestRewriteStreamChunk_MessageModel(t *testing.T) {
	rw := &responseRewriter{originalModel: "claude-opus-4.5"}

	chunk := []byte("data: {\"message\":{\"model\":\"claude-sonnet-4\",\"role\":\"assistant\"}}\n\n")
	result := rw.rewriteStreamChunk(chunk)

	expected := "data: {\"message\":{\"model\":\"claude-opus-4.5\",\"role\":\"assistant\"}}\n\n"
	if string(result) != expected {
		t.Errorf("expected %s, got %s", expected, string(result))
	}
}

func TestRewriteStreamChunkPreservesThinkingWithSignatureInjection(t *testing.T) {
	rw := &responseRewriter{}
	chunk := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"abc\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"name\":\"bash\",\"input\":{}}}\n\n")
	result := rw.rewriteStreamChunk(chunk)

	if !contains(result, []byte(`"content_block":{"type":"thinking"`)) {
		t.Fatalf("expected thinking content_block_start to be preserved, got %s", string(result))
	}
	if !contains(result, []byte(`"delta":{"type":"thinking_delta"`)) {
		t.Fatalf("expected thinking_delta to be preserved, got %s", string(result))
	}
	if !contains(result, []byte(`"type":"content_block_stop","index":0`)) {
		t.Fatalf("expected content_block_stop for thinking block to be preserved, got %s", string(result))
	}
	if !contains(result, []byte(`"content_block":{"type":"tool_use"`)) {
		t.Fatalf("expected tool_use content_block frame to remain, got %s", string(result))
	}
	if count := strings.Count(string(result), `"signature":""`); count != 2 {
		t.Fatalf("expected 2 signature injections, got %d in %s", count, string(result))
	}
}

func TestRewriteModelInResponseInjectsSignatureAndSuppressesThinking(t *testing.T) {
	rw := &responseRewriter{}
	input := []byte(`{"content":[{"type":"thinking","thinking":"hide"},{"type":"tool_use","name":"bash","input":{}}]}`)
	result := rw.rewriteModelInResponse(input)

	if contains(result, []byte(`"thinking":"hide"`)) {
		t.Fatalf("expected non-streaming thinking block to be suppressed, got %s", string(result))
	}
	if !contains(result, []byte(`"type":"tool_use"`)) {
		t.Fatalf("expected tool_use block to remain, got %s", string(result))
	}
	if !contains(result, []byte(`"signature":""`)) {
		t.Fatalf("expected tool_use signature injection, got %s", string(result))
	}
}

func TestRewriteModelInResponseRestoresRequestToolCasing(t *testing.T) {
	rw := newResponseRewriterForRequest(nil, "", []byte(`{"tools":[{"name":"CustomTool"},{"name":"Bash"}]}`))
	input := []byte(`{"content":[{"type":"tool_use","name":"customtool"},{"type":"tool_use","name":"bash"}]}`)
	result := rw.rewriteModelInResponse(input)

	if !contains(result, []byte(`"name":"CustomTool"`)) {
		t.Fatalf("expected CustomTool casing restored, got %s", result)
	}
	if !contains(result, []byte(`"name":"Bash"`)) {
		t.Fatalf("expected Bash casing restored, got %s", result)
	}
}

func TestRewriteStreamChunkRestoresRequestToolCasing(t *testing.T) {
	rw := newResponseRewriterForRequest(nil, "", []byte(`{"tools":[{"name":"CustomTool"}]}`))
	chunk := []byte("data: {\"content_block\":{\"type\":\"tool_use\",\"name\":\"customtool\"}}\n\n")
	result := rw.rewriteStreamChunk(chunk)

	if !contains(result, []byte(`"name":"CustomTool"`)) {
		t.Fatalf("expected CustomTool casing restored, got %s", result)
	}
	if !contains(result, []byte(`"signature":""`)) {
		t.Fatalf("expected signature injection, got %s", result)
	}
}

func TestRewriteModelInResponseKeepsConflictingRequestToolCasing(t *testing.T) {
	rw := newResponseRewriterForRequest(nil, "", []byte(`{"tools":[{"name":"CustomTool"},{"name":"customtool"}]}`))
	input := []byte(`{"content":[{"type":"tool_use","name":"customtool"}]}`)
	result := rw.rewriteModelInResponse(input)

	if !contains(result, []byte(`"name":"customtool"`)) {
		t.Fatalf("expected conflicting tool casing to stay unchanged, got %s", result)
	}
	if contains(result, []byte(`"name":"CustomTool"`)) {
		t.Fatalf("expected conflicting tool casing not to be rewritten, got %s", result)
	}
	if !contains(result, []byte(`"signature":""`)) {
		t.Fatalf("expected signature injection, got %s", result)
	}
}

func contains(data, substr []byte) bool {
	for i := 0; i <= len(data)-len(substr); i++ {
		if string(data[i:i+len(substr)]) == string(substr) {
			return true
		}
	}
	return false
}
