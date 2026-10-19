package claude

import (
	"bytes"
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexClaudeSearchSourcesCompatibility(t *testing.T) {
	request := ConvertClaudeRequestToCodex("fixture-model", []byte(`{"messages":[{"role":"user","content":"query"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`), false)
	found := false
	for _, value := range gjson.GetBytes(request, "include").Array() {
		found = found || value.String() == "web_search_call.action.sources"
	}
	if !found {
		t.Errorf("native search sources not requested: %s", request)
	}
	item := []byte(`{"id":"search_a","type":"web_search_call","action":{"query":"query","sources":[{"url":"https://example.com/source","title":"Source"}]}}`)
	out := ConvertCodexResponseToClaudeNonStream(context.Background(), "fixture-model", nil, nil, []byte(`{"type":"response.completed","response":{"output":[`+string(item)+`]}}`), nil)
	if got := gjson.GetBytes(out, "content.1.content.0.url").String(); got != "https://example.com/source" {
		t.Errorf("nonstream sources lost: %s", out)
	}
	var param any
	ConvertCodexResponseToClaude(context.Background(), "fixture-model", nil, nil, []byte(`data: {"type":"response.created","response":{"id":"fixture"}}`), &param)
	chunks := ConvertCodexResponseToClaude(context.Background(), "fixture-model", nil, nil, []byte(`data: {"type":"response.output_item.done","item":`+string(item)+`}`), &param)
	if !bytes.Contains(bytes.Join(chunks, nil), []byte(`"url":"https://example.com/source"`)) {
		t.Errorf("stream sources lost: %q", chunks)
	}
}
