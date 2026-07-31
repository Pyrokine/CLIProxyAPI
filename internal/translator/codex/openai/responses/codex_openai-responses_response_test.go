package responses

import (
	"bytes"
	"context"
	"testing"
)

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
		t.Run(tt.name, func(t *testing.T) {
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
		})
	}
}
