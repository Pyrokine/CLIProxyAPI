package executor

import (
	"testing"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
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

func TestParseCodexOpenAIImageEventAcceptsMultilineData(t *testing.T) {
	frame := []byte("event: response.completed\ndata: {\"type\":\ndata: \"response.completed\",\"response\":{}}\n\n")
	eventData, ok, err := parseCodexOpenAIImageEvent(frame)
	if err != nil {
		t.Fatalf("parseCodexOpenAIImageEvent error: %v", err)
	}
	if !ok {
		t.Fatal("parseCodexOpenAIImageEvent returned no data")
	}
	if got := gjson.GetBytes(eventData, "type").String(); got != "response.completed" {
		t.Fatalf("event type = %q, want response.completed", got)
	}
}

func TestParseCodexOpenAIImageEventNormalizesResponseDone(t *testing.T) {
	frame := []byte("event: response.done\ndata: {\"type\":\"response.done\",\"response\":{}}\n\n")
	eventData, ok, err := parseCodexOpenAIImageEvent(frame)
	if err != nil {
		t.Fatalf("parseCodexOpenAIImageEvent error: %v", err)
	}
	if !ok {
		t.Fatal("parseCodexOpenAIImageEvent returned no data")
	}
	if got := gjson.GetBytes(eventData, "type").String(); got != "response.completed" {
		t.Fatalf("event type = %q, want response.completed", got)
	}
}

func TestParseCodexImageToolUsage(t *testing.T) {
	detail, ok := parseCodexImageToolUsage([]byte(`{"response":{"service_tier":"priority","tool_usage":{"image_gen":{"input_tokens":7,"output_tokens":9,"total_tokens":16,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}}}`))
	if !ok {
		t.Fatal("expected image usage to parse")
	}
	if detail.InputTokens != 7 || detail.OutputTokens != 9 || detail.TotalTokens != 16 || detail.CachedTokens != 3 || detail.ReasoningTokens != 2 {
		t.Fatalf("detail = %#v, want parsed OpenAI-style usage", detail)
	}
	if detail.ResponseServiceTier != "priority" {
		t.Fatalf("response service tier = %q, want priority", detail.ResponseServiceTier)
	}
}

func TestParseCodexImageToolUsagePreservesZeroTokens(t *testing.T) {
	detail, ok := parseCodexImageToolUsage(
		[]byte(`{"response":{"service_tier":"priority","tool_usage":{"image_gen":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}}`),
	)
	if !ok {
		t.Fatal("expected zero image usage to parse")
	}
	if detail.ResponseServiceTier != "priority" || detail.InputTokens != 0 || detail.OutputTokens != 0 || detail.TotalTokens != 0 {
		t.Fatalf("detail = %#v", detail)
	}
	if !detail.TokenBreakdown.Valid() || detail.TokenBreakdown.Quality != usage.TokenAccountingQualityComplete {
		t.Fatalf("token breakdown = %+v, want valid complete", detail.TokenBreakdown)
	}
}

func TestCodexOpenAIImageServiceTierDoesNotInjectDefaultMetadata(t *testing.T) {
	req := cliproxyexecutor.Request{Payload: []byte(`{"model":"gpt-image-2"}`)}
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ServiceTierMetadataKey: usage.DefaultServiceTier,
		},
	}

	if got := codexOpenAIImageServiceTier(req, opts); got != "" {
		t.Fatalf("service tier = %q, want empty", got)
	}

	opts.Metadata[cliproxyexecutor.ServiceTierMetadataKey] = "flex"
	if got := codexOpenAIImageServiceTier(req, opts); got != "flex" {
		t.Fatalf("service tier = %q, want flex", got)
	}

	req.Payload = []byte(`{"model":"gpt-image-2","service_tier":"priority"}`)
	if got := codexOpenAIImageServiceTier(req, opts); got != "priority" {
		t.Fatalf("service tier = %q, want priority", got)
	}
}
