package claude

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesUsageCacheWriteDeduction(t *testing.T) {
	for _, tt := range []struct {
		name  string
		usage string
		want  int64
		write int64
	}{
		{"read and write", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":30}}`, 50, 30},
		{"creation alias", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":20,"cache_write_tokens":-1,"cache_creation_tokens":30}}`, 50, 30},
		{"negative input", `{"input_tokens":-10}`, 0, 0},
		{"saturating deductions", fmt.Sprintf(`{"input_tokens":%d,"input_tokens_details":{"cached_tokens":%d,"cache_write_tokens":%d}}`, int64(math.MaxInt64), int64(math.MaxInt64), int64(math.MaxInt64)), 0, math.MaxInt64},
	} {
		t.Run(tt.name, func(t *testing.T) {
			input, _, _, write := extractResponsesUsage(gjson.Parse(tt.usage))
			if input != tt.want || write != tt.write {
				t.Fatalf("input/write = %d/%d, want %d/%d", input, write, tt.want, tt.write)
			}
			response := []byte(`{"type":"response.completed","response":{"id":"fixture","output":[],"usage":` + tt.usage + `}}`)
			out := ConvertCodexResponseToClaudeNonStream(context.Background(), "fixture", nil, nil, response, nil)
			if got := gjson.GetBytes(out, "usage.input_tokens").Int(); got != tt.want {
				t.Fatalf("nonstream input = %d, want %d; %s", got, tt.want, out)
			}
		})
	}
}
