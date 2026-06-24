package claude

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func collectCodexClaudeEvents(chunks ...string) string {
	var param any
	var builder strings.Builder
	for _, chunk := range chunks {
		for _, out := range ConvertCodexResponseToClaude(
			context.TODO(), "gpt-5.5", nil, nil, []byte("data: "+chunk), &param,
		) {
			builder.Write(out)
		}
	}
	return builder.String()
}

func TestConvertCodexResponseToClaude_DropsUnsignedReasoningSummary(t *testing.T) {
	out := collectCodexClaudeEvents(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.reasoning_summary_part.added"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"thinking without signature"}`,
		`{"type":"response.reasoning_summary_part.done"}`,
		`{"type":"response.content_part.added"}`,
		`{"type":"response.output_text.delta","delta":"hello"}`,
		`{"type":"response.content_part.done"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	if strings.Contains(out, `"type":"thinking"`) || strings.Contains(out, "thinking_delta") {
		t.Fatalf("unsigned reasoning summary produced Claude thinking block: %s", out)
	}
	if !strings.Contains(out, `"type":"text_delta"`) {
		t.Fatalf("text output missing: %s", out)
	}
}

func TestConvertCodexResponseToClaude_EmitsReasoningSignature(t *testing.T) {
	const signature = "gAAAAAAAAAAAERERERERERERERERERERESIiIiIiIiIiIiIiIiIiIiIzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMw"
	out := collectCodexClaudeEvents(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"`+signature+`"}}`,
		`{"type":"response.reasoning_summary_part.added"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"signed thinking"}`,
		`{"type":"response.reasoning_summary_part.done"}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"`+signature+`"}}`,
	)

	if !strings.Contains(out, `"type":"thinking"`) {
		t.Fatalf("signed reasoning did not produce thinking block: %s", out)
	}
	if !strings.Contains(out, `"type":"signature_delta"`) || !strings.Contains(out, signature) {
		t.Fatalf("signature delta missing: %s", out)
	}
}

func TestConvertCodexResponseToClaude_EmitsReasoningWhenSignatureArrivesAfterSummary(t *testing.T) {
	const signature = "gAAAAAAAAAAAERERERERERERERERERERESIiIiIiIiIiIiIiIiIiIiIzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMw"
	out := collectCodexClaudeEvents(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.reasoning_summary_part.added"}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"signed thinking"}`,
		`{"type":"response.reasoning_summary_part.done"}`,
		`{"type":"response.output_item.done","item":{"type":"reasoning","encrypted_content":"`+signature+`"}}`,
	)

	if !strings.Contains(out, `"type":"thinking"`) || !strings.Contains(out, "signed thinking") {
		t.Fatalf("signed reasoning summary missing when signature arrived late: %s", out)
	}
	if !strings.Contains(out, `"type":"signature_delta"`) || !strings.Contains(out, signature) {
		t.Fatalf("late signature delta missing: %s", out)
	}
}

func TestConvertCodexResponseToClaudeNonStream_DropsUnsignedReasoning(t *testing.T) {
	out := ConvertCodexResponseToClaudeNonStream(
		context.TODO(), "gpt-5.5", nil, nil, []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_1",
			"model":"gpt-5.5",
			"usage":{"input_tokens":1,"output_tokens":2},
			"output":[{"type":"reasoning","summary":[{"text":"unsigned summary"}]}]
		}
	}`), nil,
	)

	if strings.Contains(string(out), `"type":"thinking"`) || strings.Contains(string(out), "unsigned summary") {
		t.Fatalf("unsigned nonstream reasoning produced Claude thinking block: %s", out)
	}
}

func TestConvertCodexResponseToClaude_EmitsDoneOnlyMessageText(t *testing.T) {
	out := collectCodexClaudeEvents(
		`{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`,
		`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"done-only text"}]}}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`,
	)

	if !strings.Contains(out, "done-only text") {
		t.Fatalf("done-only message text missing: %s", out)
	}
	if strings.Count(out, "content_block_start") != 2 || strings.Count(out, "content_block_stop") != 2 {
		t.Fatalf("done-only text block framing mismatch: %s", out)
	}
}

func TestConvertCodexResponseToClaudeNonStream_IncludesReasoningSignature(t *testing.T) {
	const signature = "gAAAAAAAAAAAERERERERERERERERERERESIiIiIiIiIiIiIiIiIiIiIzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMzMw"
	out := ConvertCodexResponseToClaudeNonStream(
		context.TODO(), "gpt-5.5", nil, nil, []byte(`{
		"type":"response.completed",
		"response":{
			"id":"resp_1",
			"model":"gpt-5.5",
			"usage":{"input_tokens":1,"output_tokens":2},
			"output":[{"type":"reasoning","encrypted_content":"`+signature+`","summary":[{"text":"signed summary"}]}]
		}
	}`), nil,
	)

	content := gjson.GetBytes(out, "content.0")
	if content.Get("type").String() != "thinking" {
		t.Fatalf("content.0 type = %q, want thinking; output: %s", content.Get("type").String(), out)
	}
	if content.Get("signature").String() != signature {
		t.Fatalf("signature = %q, want %q; output: %s", content.Get("signature").String(), signature, out)
	}
}

func TestConvertCodexResponseToClaude_TranslatesCyberPolicyStreamError(t *testing.T) {
	out := collectCodexClaudeEvents(
		`{"type":"response.error","error":{"type":"invalid_request","code":"cyber_policy","message":"blocked by policy"}}`,
	)

	if !strings.Contains(out, "event: error") {
		t.Fatalf("error event missing: %s", out)
	}
	if !strings.Contains(out, `"type":"invalid_request_error"`) {
		t.Fatalf("Claude error type missing: %s", out)
	}
	if !strings.Contains(out, `"code":"cyber_policy"`) || !strings.Contains(out, "blocked by policy") {
		t.Fatalf("Claude error payload incomplete: %s", out)
	}
}

func TestConvertCodexResponseToClaude_TranslatesStreamErrorWithoutMessage(t *testing.T) {
	out := collectCodexClaudeEvents(
		`{"type":"error","error_type":"rate_limit_error"}`,
	)

	if !strings.Contains(out, "event: error") {
		t.Fatalf("error event missing: %s", out)
	}
	if !strings.Contains(out, `"type":"rate_limit_error"`) || !strings.Contains(out, `"message":"rate_limit_error"`) {
		t.Fatalf("fallback error payload missing: %s", out)
	}
}
