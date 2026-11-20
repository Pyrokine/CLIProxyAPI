package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestGeminiCLIStreamRetainsSplitAndLateUsage(t *testing.T) {
	first := `{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]}}],` +
		`"usageMetadata":{"promptTokenCount":` +
		`10}}}`
	finish := `{"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":2}}}`
	tail := `{"response":{"usageMetadata":{"cachedContentTokenCount":4,"thoughtsTokenCount":3,"totalTokenCount":15}}}`
	complete := `{"response":{"candidates":[{"content":{"parts":[{"text":"answer"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"thoughtsTokenCount":3,"` +
		`cachedContentTokenCount":4,"totalTokenCount":15}}}`
	for _, test := range []struct {
		name, alt, cancellation string
		frames                  []string
	}{
		{name: "SSE/split-late", frames: []string{first, finish, tail}},
		{
			name: "SSE/finish-without-usage", frames: []string{
				first, `{"response":{"candidates":[{"finishReason":"STOP"}]}}`,
				`{"response":{"usageMetadata":{"candidatesTokenCount":2,"cachedContentTokenCount":4,"` +
					`thoughtsTokenCount":3,"totalTokenCount":15}}}`,
			},
		},
		{
			name: "SSE/usage-before-finish", frames: []string{
				first,
				`{"response":{"usageMetadata":{"candidatesTokenCount":2,"cachedContentTokenCount":4,"` +
					`thoughtsTokenCount":3,"totalTokenCount":15}}}`,
				`{"response":{"candidates":[{"finishReason":"STOP"}]}}`,
			},
		},
		{
			name: "SSE/actual-DONE", frames: []string{
				first, finish, tail, "[DONE]",
				`{"response":{"candidates":[{"content":{"parts":[{"text":"post-terminal` +
					`"}]}}],"usageMetadata":{"promptTokenCount":999}}}`,
			},
		},
		{name: "JSON/split-late", alt: "json", frames: []string{first, finish, tail}},
		{name: "JSON/single", alt: "json", frames: []string{complete}},
		{name: "SSE/cancel-before-terminal", cancellation: "before", frames: []string{first, finish, tail}},
		{name: "SSE/cancel-after-terminal", cancellation: "after", frames: []string{first, finish, tail, "[DONE]"}},
		{name: "JSON/cancel-before-terminal", alt: "json", cancellation: "before", frames: []string{complete}},
		{name: "JSON/cancel-after-terminal", alt: "json", cancellation: "after", frames: []string{complete}},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
				registerTestUsagePlugin(t, plugin)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				body := "data: " + strings.Join(test.frames, "\n\ndata: ") + "\n\n"
				if test.alt != "" {
					body = test.frames[0]
					if len(test.frames) > 1 {
						body = "[" + strings.Join(test.frames, ",") + "]"
					}
				}
				var responseBody = io.NopCloser(strings.NewReader(body))
				if test.cancellation == "before" {
					blocked := &cancelThenEOFBody{
						reader: strings.NewReader(body), ctx: ctx, started: make(chan struct{}),
					}
					responseBody = blocked
					go func(ctx context.Context) {
						select {
						case <-blocked.started:
							cancel()
						case <-ctx.Done():
						}
					}(ctx)
				}
				calls := 0
				transport := &auditClaudeContractTransport{
					respond: func(req *http.Request) (*http.Response, error) {
						calls++
						if _, err := io.ReadAll(req.Body); err != nil {
							return nil, err
						}
						return &http.Response{
							StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
							Body: responseBody, Request: req,
						}, nil
					},
				}
				ctx = util.WithRoundTripper(ctx, transport)
				cfg, auth := &config.Config{}, googlePayloadAuth("gemini-cli")
				if newHTTPClient(
					ctx, cfg, auth, 0,
				).Transport != transport || !tokenExpiry(auth.Metadata).After(time.Now().Add(refreshSkew)) {
					t.Fatal("synthetic CLI transport or credential is not isolated")
				}
				original := []byte(`{"model":"gemini-2.5-flash","max_tokens":32,"messages":[{"role":"user"` +
					`,"content":"Please explain the deterministic synthetic stream input to` +
					`ken estimate contract."}]}`)
				stream, err := NewGeminiCLIExecutor(cfg).ExecuteStream(
					ctx, auth, core.Request{Model: "gemini-2.5-flash", Payload: original}, core.Options{
						SourceFormat: translator.FormatClaude, ResponseFormat: translator.FormatClaude,
						OriginalRequest: original, Alt: test.alt, Stream: true,
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				starts, stops, blockStarts, blockStops, finishes := 0, 0, 0, 0, 0
				var finalUsage gjson.Result
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						if test.cancellation != "before" {
							t.Error(chunk.Err)
						}
						continue
					}
					for line := range strings.SplitSeq(string(chunk.Payload), "\n") {
						if !strings.HasPrefix(line, "data:") {
							continue
						}
						event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
						if stops > 0 {
							t.Errorf("client event after message_stop: %s", event.Raw)
						}
						switch event.Get("type").String() {
						case "message_start":
							starts++
							if event.Get("message.usage.input_tokens").Int() != 13 {
								t.Errorf("initial estimate changed: %s", event.Raw)
							}
						case "content_block_start":
							blockStarts++
						case "content_block_stop":
							blockStops++
						case "message_delta":
							finalUsage = event.Get("usage")
							if event.Get("delta.stop_reason").Exists() {
								finishes++
							}
						case "message_stop":
							stops++
							if test.cancellation == "after" {
								cancel()
							}
						}
					}
				}
				canceledBefore := test.cancellation == "before"
				if !canceledBefore {
					if calls != 1 || starts != 1 || stops != 1 || blockStarts != 1 || blockStops != 1 ||
						finishes != 1 {
						t.Errorf(
							"calls=%d starts=%d stops=%d blocks=%d/%d finishes=%d",
							calls,
							starts,
							stops,
							blockStarts,
							blockStops,
							finishes,
						)
					}
					if finalUsage.Get("input_tokens").Int() != 6 ||
						finalUsage.Get("output_tokens").Int() != 5 ||
						finalUsage.Get("cache_read_input_tokens").Int() != 4 {
						t.Errorf("final client usage=%s, want input=6 output=5 cache=4", finalUsage.Raw)
					}
				} else if stops != 0 {
					t.Errorf("canceled stream emitted %d message_stop events", stops)
				}
				select {
				case record := <-plugin.records:
					if record.Failed != canceledBefore {
						t.Errorf("record failed=%t, want %t", record.Failed, canceledBefore)
					}
					if record.Detail.InputTokens != 10 || record.Detail.OutputTokens != 2 ||
						record.Detail.ReasoningTokens != 3 ||
						record.Detail.CachedTokens != 4 ||
						record.Detail.TotalTokens != 15 {
						t.Errorf("real billable usage changed: %+v", record)
					}
				case <-time.After(time.Second):
					t.Fatal("usage record missing")
				}
			},
		)
	}
}
