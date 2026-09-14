package executor

import (
	"context"
	"net/http"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestNormalizeCodexParallelToolCalls(t *testing.T) {
	body := []byte(`{"input":[],"parallel_tool_calls":true}`)
	if got := normalizeCodexParallelToolCalls(body, http.Header{}); gjson.GetBytes(
		got, "parallel_tool_calls",
	).Exists() {
		t.Fatalf("parallel_tool_calls should be removed without tools: %s", got)
	}

	headers := http.Header{}
	headers.Set(codexResponsesLiteHeader, "true")
	if got := normalizeCodexParallelToolCalls(body, headers); !gjson.GetBytes(
		got, "parallel_tool_calls",
	).Exists() || gjson.GetBytes(got, "parallel_tool_calls").Bool() {
		t.Fatalf("responses-lite parallel_tool_calls should be false: %s", got)
	}

	withTools := []byte(`{"tools":[{"type":"function","name":"lookup"}],"parallel_tool_calls":true}`)
	if got := normalizeCodexParallelToolCalls(withTools, http.Header{}); !gjson.GetBytes(
		got, "parallel_tool_calls",
	).Bool() {
		t.Fatalf("parallel_tool_calls should remain true with tools: %s", got)
	}
}

func TestPatchCodexCompletedOutputHydratesMissingItemID(t *testing.T) {
	event := []byte(`{"type":"response.completed","response":{"output":[{"type":"message","id":""},{"type":"function_call","id":"fc-existing"}]}}`)
	items := map[int64][]byte{
		0: []byte(`{"type":"message","id":"msg-restored"}`),
		1: []byte(`{"type":"function_call","id":"fc-stream"}`),
	}

	got := patchCodexCompletedOutput(event, items, nil)
	if id := gjson.GetBytes(got, "response.output.0.id").String(); id != "msg-restored" {
		t.Fatalf("response.output.0.id = %q, want msg-restored", id)
	}
	if id := gjson.GetBytes(got, "response.output.1.id").String(); id != "fc-existing" {
		t.Fatalf("response.output.1.id = %q, want fc-existing", id)
	}
}

func TestCodexResponsesBodyPreservesSequentialReasoningSummaryDelivery(t *testing.T) {
	cfg := &config.Config{}
	cfg.DisableImageGeneration = config.DisableImageGenerationAll
	from := sdktranslator.FormatOpenAIResponse
	payload := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"hello"}],"stream_options":{"reasoning_summary_delivery":"sequential_cutoff","include_usage":true}}`)

	prepared, err := codexResponsesBody(
		context.Background(), cfg, &cliproxyauth.Auth{}, "gpt-5.6-sol", "gpt-5.6-sol", "codex", from,
		cliproxyexecutor.Options{SourceFormat: from},
		cliproxyexecutor.Request{Model: "gpt-5.6-sol", Payload: payload}, true,
	)
	if err != nil {
		t.Fatalf("codexResponsesBody() error = %v", err)
	}
	if got := gjson.GetBytes(
		prepared.Body, "stream_options.reasoning_summary_delivery",
	).String(); got != "sequential_cutoff" {
		t.Fatalf("reasoning_summary_delivery = %q, want sequential_cutoff; body=%s", got, prepared.Body)
	}
}
