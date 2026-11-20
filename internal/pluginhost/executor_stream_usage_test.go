package pluginhost

import (
	"bytes"
	"context"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestExecutorAdapterCountsCompleteJSONPayloadsWithoutNewlines(t *testing.T) {
	first := `{"usage":{"input_tokens":5,"output_tokens":0},"service_tier":"flex"}`
	last := `{"usage":{"input_tokens":5,"output_tokens":8},"service_tier":"priority"}`
	wantFirst := `{"usage":{"input_tokens":5,"output_tokens":0,"input_tokens_details":{"` +
		`cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}},"ser` +
		`vice_tier":"flex"}`
	wantLast := `{"usage":{"input_tokens":5,"output_tokens":8,"input_tokens_details":{"` +
		`cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}},"ser` +
		`vice_tier":"priority"}`
	for _, scenario := range []struct {
		name     string
		payloads []string
		want     string
	}{
		{"complete payloads", []string{first, last}, wantFirst + wantLast},
		{"fragmented payloads", []string{first[:20], first[20:], last[:30], last[30:]}, first + last},
		{
			"SSE control", []string{"data: " + first + "\n\n", "data: " + last + "\n\n"},
			"data: " + wantFirst + "\n\ndata: " + wantLast + "\n\n",
		},
	} {
		t.Run(
			scenario.name, func(t *testing.T) {
				records := registerExecutorUsageCapture(t)
				chunks := make(chan pluginapi.ExecutorStreamChunk, len(scenario.payloads))
				for _, payload := range scenario.payloads {
					chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte(payload)}
				}
				close(chunks)
				format := sdktranslator.FormatOpenAIResponse
				adapter := &executorAdapter{
					host: New(), pluginID: "fixture-usage", provider: "fixture-provider",
					executor: &fakeExecutor{
						executeStream: func(
							context.Context,
							pluginapi.ExecutorRequest,
						) (pluginapi.ExecutorStreamResponse, error) {
							return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
						},
					},
					inputFormats: []sdktranslator.Format{format}, outputFormats: []sdktranslator.Format{format},
				}
				auth := &coreauth.Auth{ID: "fixture-selected", Provider: adapter.provider, Index: "fixture-index"}
				result, err := adapter.ExecuteStream(
					context.Background(), auth, coreexecutor.Request{
						Model: "fixture-model", Format: format, Payload: []byte(`{"model":"fixture-model"}`),
					}, coreexecutor.Options{Stream: true},
				)
				if err != nil {
					t.Fatal(err)
				}
				var delivered []byte
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
					delivered = append(delivered, chunk.Payload...)
				}
				if !bytes.Equal(delivered, []byte(scenario.want)) {
					t.Fatalf("payload changed: %q, want %q", delivered, scenario.want)
				}
				record := readExecutorUsageRecord(t, records)
				if record.Failed || !record.Stream || record.AuthID != auth.ID || record.AuthIndex != auth.Index ||
					record.Detail.InputTokens != 5 ||
					record.Detail.OutputTokens != 8 ||
					record.Detail.TotalTokens != 13 ||
					record.ResponseServiceTier != "priority" {
					t.Fatalf("native stream usage = %#v, want selected identity and 5/8/13 tokens", record)
				}
				assertNoSecondExecutorUsage(t, records)
			},
		)
	}
}
