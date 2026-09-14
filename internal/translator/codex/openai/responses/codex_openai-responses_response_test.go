package responses

import (
	"bytes"
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertCodexResponseToOpenAIResponsesCreatedIncludesOriginalRequestModel(t *testing.T) {
	originalRequest := []byte(`{"model":"original-codex-model"}`)
	translatedRequest := []byte(`{"model":"translated-codex-model"}`)

	for _, event := range []string{"response.created", "response.in_progress"} {
		raw := []byte(`data: {"type":"` + event + `","response":{"id":"resp_1"}}`)
		outputs := ConvertCodexResponseToOpenAIResponses(
			context.Background(),
			"fallback-model",
			originalRequest,
			translatedRequest,
			raw,
			nil,
		)
		if len(outputs) != 1 {
			t.Fatalf("%s outputs = %d, want 1", event, len(outputs))
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(outputs[0], []byte("data:")))
		if got := gjson.GetBytes(payload, "response.model").String(); got != "original-codex-model" {
			t.Fatalf("%s model = %q, want original-codex-model; payload=%s", event, got, payload)
		}
	}
}

func TestConvertCodexResponseToOpenAIResponsesPreservesExistingModel(t *testing.T) {
	raw := []byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"upstream-model"}}`)
	outputs := ConvertCodexResponseToOpenAIResponses(
		context.Background(),
		"fallback-model",
		[]byte(`{"model":"original-codex-model"}`),
		nil,
		raw,
		nil,
	)
	if len(outputs) != 1 {
		t.Fatalf("outputs = %d, want 1", len(outputs))
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(outputs[0], []byte("data:")))
	if got := gjson.GetBytes(payload, "response.model").String(); got != "upstream-model" {
		t.Fatalf("model = %q, want upstream-model; payload=%s", got, payload)
	}
}

func TestConvertCodexResponseToOpenAIResponsesNonStreamIncomplete(t *testing.T) {
	raw := []byte(`{"type":"response.incomplete","response":{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)

	out := ConvertCodexResponseToOpenAIResponsesNonStream(
		context.Background(), "gpt-5.5", nil, nil, raw, nil,
	)
	if got := gjson.GetBytes(out, "status").String(); got != "incomplete" {
		t.Fatalf("status = %q, want incomplete. Output: %s", got, out)
	}
	if got := gjson.GetBytes(out, "incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("incomplete reason = %q, want max_output_tokens. Output: %s", got, out)
	}
}

func TestConvertCodexResponseToOpenAIResponsesWritesCompleteFrame(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "data field",
			raw:  `data: {"type":"response.completed"}`,
			want: "data: {\"type\":\"response.completed\"}\n\n",
		},
		{
			name: "event and data fields",
			raw:  "event: response.completed\ndata: {\"type\":\"response.completed\"}",
			want: "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				chunks := ConvertCodexResponseToOpenAIResponses(
					context.Background(),
					"gpt-5-codex",
					nil,
					nil,
					[]byte(tt.raw),
					nil,
				)
				if len(chunks) != 1 {
					t.Fatalf("chunks = %q, want one chunk", chunks)
				}
				if !bytes.Equal(chunks[0], []byte(tt.want)) {
					t.Fatalf("chunk = %q, want %q", chunks[0], tt.want)
				}
			},
		)
	}
}
