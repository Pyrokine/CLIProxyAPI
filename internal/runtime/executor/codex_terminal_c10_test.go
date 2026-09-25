package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestCodexResponsesLiteDetectorAcceptsLowerCaseLiteralHeader(t *testing.T) {
	headers := http.Header{
		"x-openai-internal-codex-responses-lite": []string{" false ", " TrUe "},
	}
	if !isCodexResponsesLiteRequest(nil, headers) {
		t.Fatal("lower-case Responses-Lite header was not detected")
	}
}

func TestCodexHTTPNonStreamTerminalOutputRespectsNativeMode(t *testing.T) {
	const outputItem = `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`
	const terminal = `{"type":"response.completed","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`

	for _, tt := range []struct {
		name   string
		native bool
	}{
		{name: "compatibility", native: false},
		{name: "native", native: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: " + outputItem + "\n\ndata: " + terminal + "\n\n"))
			}))
			defer server.Close()

			cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
			exec := newCodexExecutor(cfg)
			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
			}
			if tt.native {
				opts.Headers = http.Header{
					"x-openai-internal-codex-responses-lite": []string{"true"},
				}
			}
			resp, err := exec.Execute(
				context.Background(),
				&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}},
				cliproxyexecutor.Request{
					Model:   "gpt-5.5",
					Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
				},
				opts,
			)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			output := gjson.GetBytes(resp.Payload, "output")
			if tt.native {
				if output.Exists() {
					t.Fatalf("native terminal output was hydrated: %s", resp.Payload)
				}
				return
			}
			if !output.IsArray() || len(output.Array()) != 1 || output.Get("0.id").String() != "msg-1" {
				t.Fatalf("compatibility terminal output was not hydrated: %s", resp.Payload)
			}
		})
	}
}

func TestCodexHTTPStreamTerminalOutputRespectsNativeMode(t *testing.T) {
	const outputItem = `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg-1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`
	const terminal = `{"type":"response.completed","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`

	for _, tt := range []struct {
		name   string
		native bool
	}{
		{name: "compatibility", native: false},
		{name: "native", native: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: " + outputItem + "\n\ndata: " + terminal + "\n\n"))
			}))
			defer server.Close()

			cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
			exec := newCodexExecutor(cfg)
			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAIResponse,
				Stream:         true,
			}
			if tt.native {
				opts.Headers = http.Header{
					"x-openai-internal-codex-responses-lite": []string{"true"},
				}
			}
			stream, err := exec.ExecuteStream(
				context.Background(),
				&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL}},
				cliproxyexecutor.Request{
					Model:   "gpt-5.5",
					Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
				},
				opts,
			)
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}
			var completed []byte
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream chunk error = %v", chunk.Err)
				}
				payload := jsonPayload(chunk.Payload)
				if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) == "response.completed" {
					completed = append([]byte(nil), payload...)
				}
			}
			if len(completed) == 0 {
				t.Fatal("missing response.completed event")
			}
			output := gjson.GetBytes(completed, "response.output")
			if tt.native {
				if output.Exists() {
					t.Fatalf("native terminal output was hydrated: %s", completed)
				}
				return
			}
			if !output.IsArray() || len(output.Array()) != 1 || output.Get("0.id").String() != "msg-1" {
				t.Fatalf("compatibility terminal output was not hydrated: %s", completed)
			}
		})
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
