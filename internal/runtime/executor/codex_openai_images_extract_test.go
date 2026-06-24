package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func codexImageTestItem(result, format string) []byte {
	return []byte(`{"type":"image_generation_call","result":"` + result + `","output_format":"` + format + `","size":"1024x1024","quality":"high","background":"transparent","revised_prompt":"revised ` + result + `"}`)
}

func TestCodexExtractImageResults_FromCompletedOutput(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"created_at":123,"output":[{"type":"image_generation_call","result":"image-a","output_format":"png","size":"1024x1024","quality":"high","background":"transparent","revised_prompt":"revised prompt"}],"tool_usage":{"image_gen":{"input_tokens":11,"output_tokens":22,"total_tokens":33}}}}`)

	results, createdAt, usageRaw, firstMeta, err := codexExtractImageResults(completed, nil, nil)
	if err != nil {
		t.Fatalf("extract image results: %v", err)
	}
	if createdAt != 123 {
		t.Fatalf("createdAt = %d, want 123", createdAt)
	}
	if len(results) != 1 || results[0].Result != "image-a" {
		t.Fatalf("results = %#v, want image-a", results)
	}
	if firstMeta.OutputFormat != "png" || firstMeta.Size != "1024x1024" || firstMeta.Quality != "high" || firstMeta.Background != "transparent" {
		t.Fatalf("firstMeta = %#v, want completed item metadata", firstMeta)
	}
	if got := gjson.GetBytes(usageRaw, "total_tokens").Int(); got != 33 {
		t.Fatalf("usage total_tokens = %d, want 33", got)
	}

	response, err := codexBuildImagesAPIResponse(results, createdAt, usageRaw, firstMeta, "b64_json")
	if err != nil {
		t.Fatalf("build image API response: %v", err)
	}
	if got := gjson.GetBytes(response, "data.0.b64_json").String(); got != "image-a" {
		t.Fatalf("b64_json = %q, want image-a", got)
	}
	if got := gjson.GetBytes(response, "usage.total_tokens").Int(); got != 33 {
		t.Fatalf("response usage total_tokens = %d, want 33", got)
	}
}

func TestCodexExtractImageResults_FallbackToCollectedItemsOrdered(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"created_at":456}}`)
	itemsByIndex := map[int64][]byte{
		2: codexImageTestItem("image-b", "jpeg"),
		1: codexImageTestItem("image-a", "png"),
	}

	results, createdAt, _, _, err := codexExtractImageResults(completed, itemsByIndex, nil)
	if err != nil {
		t.Fatalf("extract image results: %v", err)
	}
	if createdAt != 456 {
		t.Fatalf("createdAt = %d, want 456", createdAt)
	}
	if len(results) != 2 || results[0].Result != "image-a" || results[1].Result != "image-b" {
		t.Fatalf("results = %#v, want index order image-a,image-b", results)
	}
}

func TestCodexExtractImageResults_PrefersCompletedOutputOverItems(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"output":[{"type":"image_generation_call","result":"completed-image","output_format":"png"}]}}`)
	itemsByIndex := map[int64][]byte{1: codexImageTestItem("fallback-image", "png")}

	results, _, _, _, err := codexExtractImageResults(completed, itemsByIndex, nil)
	if err != nil {
		t.Fatalf("extract image results: %v", err)
	}
	if len(results) != 1 || results[0].Result != "completed-image" {
		t.Fatalf("results = %#v, want completed output only", results)
	}
}

func TestCodexExtractImageResults_FallbackList(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{}}`)
	fallback := [][]byte{codexImageTestItem("fallback-a", "png"), codexImageTestItem("fallback-b", "webp")}

	results, _, _, _, err := codexExtractImageResults(completed, nil, fallback)
	if err != nil {
		t.Fatalf("extract image results: %v", err)
	}
	if len(results) != 2 || results[0].Result != "fallback-a" || results[1].OutputFormat != "webp" {
		t.Fatalf("results = %#v, want fallback list order", results)
	}
}

func TestCodexExtractImageResults_WrongEventType(t *testing.T) {
	_, _, _, _, err := codexExtractImageResults([]byte(`{"type":"response.output_item.done"}`), nil, nil)
	if err == nil {
		t.Fatal("expected error for non-completed event")
	}
}

func TestParseCodexImageToolUsage(t *testing.T) {
	detail, ok := parseCodexImageToolUsage([]byte(`{"response":{"tool_usage":{"image_gen":{"input_tokens":7,"output_tokens":9,"total_tokens":16,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}}}`))
	if !ok {
		t.Fatal("expected image usage to parse")
	}
	if detail.InputTokens != 7 || detail.OutputTokens != 9 || detail.TotalTokens != 16 || detail.CachedTokens != 3 || detail.ReasoningTokens != 2 {
		t.Fatalf("detail = %#v, want parsed OpenAI-style usage", detail)
	}
}
