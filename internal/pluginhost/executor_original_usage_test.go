package pluginhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestPluginExecutorOriginalPayloadBoundaries(t *testing.T) {
	content := []byte(`data: {"choices":[{"delta":{"content":"Hello"}}]}`)
	usage := []byte(
		`data: {"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`,
	)
	for _, mode := range []string{
		"newline-control", "complete-chunks-without-newline", "split-json-control", "single-eof-control",
		"raw-json-chunks",
	} {
		t.Run(
			mode, func(t *testing.T) {
				payloads := [][]byte{append(bytes.Clone(content), '\n'), append(bytes.Clone(usage), '\n')}
				switch mode {
				case "complete-chunks-without-newline":
					payloads = [][]byte{bytes.Clone(content), bytes.Clone(usage), []byte("data: [DONE]")}
				case "split-json-control":
					middle := len(usage) / 2
					payloads = [][]byte{bytes.Clone(usage[:middle]), append(bytes.Clone(usage[middle:]), '\n')}
				case "single-eof-control":
					payloads = [][]byte{bytes.Clone(usage)}
				case "raw-json-chunks":
					payloads = [][]byte{
						bytes.Clone(content[len("data: "):]),
						bytes.Clone(usage[len("data: "):]),
					}
				}
				manager := coreusage.NewManager(16)
				records := make(chan coreusage.Record, 4)
				manager.RegisterNamed(
					"audit72-usage",
					coreUsagePluginFunc(func(_ context.Context, record coreusage.Record) { records <- record }),
				)
				manager.Start(context.Background())
				t.Cleanup(
					func() {
						ctx, cancel := context.WithTimeout(context.Background(), time.Second)
						defer cancel()
						if err := manager.StopContext(ctx); err != nil {
							t.Errorf("usage manager stop: %v", err)
						}
					},
				)
				chunks := make(chan pluginapi.ExecutorStreamChunk, len(payloads))
				for _, payload := range payloads {
					chunks <- pluginapi.ExecutorStreamChunk{Payload: payload}
				}
				close(chunks)
				adapter := &executorAdapter{
					host: New(), pluginID: "audit72-usage", provider: "audit72-provider",
					executor: &fakeExecutor{
						executeStream: func(
							context.Context,
							pluginapi.ExecutorRequest,
						) (pluginapi.ExecutorStreamResponse, error) {
							return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
						},
					},
					inputFormats:  []sdktranslator.Format{sdktranslator.FormatOpenAI},
					outputFormats: []sdktranslator.Format{sdktranslator.FormatOpenAI},
				}
				ctx := coreusage.WithManager(context.Background(), manager)
				auth := &coreauth.Auth{
					ID:       "audit72-selected",
					Provider: "audit72-provider",
					Index:    "audit72-index",
				}
				stream, err := adapter.ExecuteStream(
					ctx, auth,
					coreexecutor.Request{Model: "audit72-model", Payload: []byte(`{"model":"audit72-model"}`)},
					coreexecutor.Options{Stream: true},
				)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
				stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := manager.StopContext(stopCtx); err != nil {
					t.Fatal(err)
				}
				if len(records) != 1 {
					t.Fatalf("usage record count=%d, want 1", len(records))
				}
				record := <-records
				t.Logf(
					"mode=%s input=%d output=%d total=%d failed=%v auth=%s", mode, record.Detail.InputTokens,
					record.Detail.OutputTokens, record.Detail.TotalTokens, record.Failed, record.AuthID,
				)
				if record.Detail.InputTokens != 11 || record.Detail.OutputTokens != 22 ||
					record.Detail.TotalTokens != 33 ||
					record.Failed ||
					record.AuthID != auth.ID {
					t.Errorf("usage attribution/content=%+v, want 11/22/33 and selected auth", record)
				}
			},
		)
	}
}

func TestPluginHandlerOriginalSelectedResponsesUsageOnce(t *testing.T) {
	const provider = "audit72-handler-provider"
	const model = "deepseek/deepseek-v4.1-flash"
	body := []byte(`{"model":"deepseek/deepseek-v4.1-flash","input":"Write me a poem"}`)
	response := []byte(`{"id":"resp_1","object":"response","service_tier":"default","usage":{"` +
		`input_tokens":34,"output_tokens":499,"total_tokens":533}}`)
	usageManager := coreusage.NewManager(16)
	records := make(chan coreusage.Record, 4)
	usageManager.RegisterNamed(
		"audit72-handler",
		coreUsagePluginFunc(func(_ context.Context, record coreusage.Record) { records <- record }),
	)
	usageManager.Start(context.Background())
	t.Cleanup(
		func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := usageManager.StopContext(ctx); err != nil {
				t.Errorf("usage manager stop: %v", err)
			}
		},
	)
	calls := 0
	adapter := &executorAdapter{
		host: New(), pluginID: "audit72-handler", provider: provider,
		executor: &fakeExecutor{
			execute: func(_ context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
				calls++
				if !bytes.Equal(req.Payload, body) || !bytes.Equal(
					req.OriginalRequest, body,
				) || req.AuthID != "audit72-handler-auth" || req.SourceFormat != "openai-response" {
					t.Errorf(
						"plugin request changed: auth=%s source=%s body=%q original=%q",
						req.AuthID,
						req.SourceFormat,
						req.Payload,
						req.OriginalRequest,
					)
				}
				return pluginapi.ExecutorResponse{
					Payload: response, Headers: http.Header{"Content-Type": {"application/json"}},
				}, nil
			},
		},
		inputFormats:  []sdktranslator.Format{sdktranslator.FormatOpenAIResponse},
		outputFormats: []sdktranslator.Format{sdktranslator.FormatOpenAIResponse},
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(adapter)
	auth := &coreauth.Auth{ID: "audit72-handler-auth", Provider: provider, Status: coreauth.StatusActive}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	handler := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	handler.SetUsageManager(usageManager)
	ctx, cancel := handler.GetContextWithCancel(nil, nil, context.Background())
	defer cancel()
	want := []byte(`{"id":"resp_1","object":"response","service_tier":"default","usage":{"` +
		`input_tokens":34,"output_tokens":499,"total_tokens":533,"input_tokens_` +
		`details":{"cached_tokens":0},"output_tokens_details":{"reasoning_token` +
		`s":0}}}`)
	got, _, failure := handler.ExecuteWithAuthManager(ctx, "openai-response", model, body, "")
	if failure != nil || !bytes.Equal(got, want) {
		t.Fatalf("handler response=%q error=%+v", got, failure)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := usageManager.StopContext(stopCtx); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || calls != 1 {
		t.Fatalf("executor calls=%d records=%d, want 1/1", calls, len(records))
	}
	record := <-records
	t.Logf(
		"calls=%d records=1 auth=%s input=%d output=%d total=%d request_tier=%s response_tier=%s",
		calls,
		record.AuthID,
		record.Detail.InputTokens,
		record.Detail.OutputTokens,
		record.Detail.TotalTokens,
		record.RequestServiceTier,
		record.ResponseServiceTier,
	)
	if record.AuthID != auth.ID || record.Provider != provider || record.Stream || record.Failed ||
		record.Detail.InputTokens != 34 ||
		record.Detail.OutputTokens != 499 ||
		record.Detail.TotalTokens != 533 ||
		record.ResponseServiceTier != "default" ||
		record.RequestServiceTier != "default" {
		t.Fatalf("usage record=%+v", record)
	}
}

func TestPluginExecutorOriginalProviderUsage(t *testing.T) {
	for _, tt := range []struct {
		original string
		provider string
		format   sdktranslator.Format
		model    string
		request  string
		response string
		chunks   []string
		want     coreusage.Detail
		tier     string
	}{
		{
			original: "TestHandlerPluginExecutorPublishesUsageNonStreamOpenAIResponseWithServiceTier",
			provider: "custom-responses-plugin", format: sdktranslator.FormatOpenAIResponse,
			model:   "deepseek/deepseek-v4.1-flash",
			request: `{"model":"deepseek/deepseek-v4.1-flash"}`,
			response: `{"id":"resp_1","object":"response","service_tier":"default","usage":{"` +
				`input_tokens":34,"output_tokens":499,"total_tokens":533}}`,
			want: coreusage.Detail{InputTokens: 34, OutputTokens: 499, TotalTokens: 533}, tier: "default",
		},
		{
			original: "TestHandlerPluginExecutorPublishesUsageNonStreamOpenAI",
			provider: "custom-openai-plugin", format: sdktranslator.FormatOpenAI, model: "gpt-4o",
			request: `{"model":"gpt-4o"}`,
			response: `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content"` +
				`:"hello"}}],"usage":{"prompt_tokens":12,"completion_tokens":34,"total_` +
				`tokens":46}}`,
			want: coreusage.Detail{InputTokens: 12, OutputTokens: 34, TotalTokens: 46},
		},
		{
			original: "TestHandlerPluginExecutorSkipsUsageForNestedExecution",
			provider: "nested-plugin", format: sdktranslator.FormatOpenAI, model: "gpt-4o",
			request: `{"model":"gpt-4o"}`,
			response: `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content"` +
				`:"hello"}}],"usage":{"prompt_tokens":12,"completion_tokens":34,"total_` +
				`tokens":46}}`,
			want: coreusage.Detail{InputTokens: 12, OutputTokens: 34, TotalTokens: 46},
		},
		{
			original: "TestHandlerPluginExecutorPublishesUsageStreamOpenAI",
			provider: "custom-stream-plugin", format: sdktranslator.FormatOpenAI, model: "gpt-4o",
			request: `{"model":"gpt-4o","stream":true}`,
			chunks: []string{
				"data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n",
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":15," +
					"\"completion_tokens\":25,\"total_tokens\":" +
					"40}}\n\n",
				"data: [DONE]\n\n",
			},
			want: coreusage.Detail{InputTokens: 15, OutputTokens: 25, TotalTokens: 40},
		},
		{
			original: "TestHandlerPluginExecutorPublishesUsageStreamCodex",
			provider: "custom-codex-stream-plugin", format: sdktranslator.FormatCodex, model: "codex-5.2",
			request: `{"model":"codex-5.2","stream":true}`,
			chunks: []string{
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":" +
					"{\"input_tokens\":18,\"output_tokens\":22,\"total_tokens\":40}}}\n\n",
				"data: [DONE]\n\n",
			},
			want: coreusage.Detail{InputTokens: 18, OutputTokens: 22, TotalTokens: 40},
		},
		{
			original: "TestHandlerPluginExecutorPublishesUsageNonStreamClaude",
			provider: "custom-claude-plugin", format: sdktranslator.FormatClaude, model: "claude-3-5-sonnet",
			request: `{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"hi"}]}`,
			response: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"` +
				`text","text":"hello"}],"usage":{"input_tokens":50,"output_tokens":30,"` +
				`output_tokens_details":{"thinking_tokens":10}}}`,
			want: coreusage.Detail{InputTokens: 50, OutputTokens: 30, ReasoningTokens: 10, TotalTokens: 80},
		},
		{
			original: "TestHandlerPluginExecutorPublishesUsageStreamClaude",
			provider: "custom-claude-stream-plugin", format: sdktranslator.FormatClaude, model: "claude-3-5-sonnet",
			request: `{"model":"claude-3-5-sonnet","stream":true}`,
			chunks: []string{
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{" +
					"\"id\":\"msg_1\",\"usage\":{\"input_tokens\":100,\"cache_read_input_to" +
					"kens\":50,\"cache_creation_input_tokens\":20,\"output_tokens\":1}}}\n" +
					"\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"" +
					"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":" +
					"{\"output_tokens\":25,\"output_tokens_details\":{\"thinking_tokens\":5}}}\n\n",
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			},
			want: coreusage.Detail{
				InputTokens: 100, CacheReadTokens: 50, CacheCreationTokens: 20, OutputTokens: 25, ReasoningTokens: 5,
				TotalTokens: 195,
			},
		},
		{
			original: "TestHandlerPluginExecutorPublishesUsageGemini",
			provider: "custom-gemini-plugin", format: sdktranslator.FormatGemini, model: "gemini-2.5-flash",
			request: `{"contents":[{"parts":[{"text":"hi"}]}]}`,
			response: `{"candidates":[{"content":{"parts":[{"text":"hello"}]}}],` +
				`"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":60,"totalTokenCount":100}}`,
			want: coreusage.Detail{InputTokens: 40, OutputTokens: 60, TotalTokens: 100},
		},
		{
			original: "TestHandlerPluginExecutorPublishesUsageStreamInteractions",
			provider: "custom-interactions-stream-plugin", format: sdktranslator.Format("interactions"),
			model:   "gemini-2.5-flash",
			request: `{"model":"gemini-2.5-flash","stream":true}`,
			chunks: []string{
				"data: {\"event_type\":\"finish\",\"metadata\":{\"total_usage\":{\"tota" +
					"l_input_tokens\":30,\"total_output_tokens\":70,\"total_tokens\":100}}}" +
					"\n\n",
				"data: [DONE]\n\n",
			},
			want: coreusage.Detail{InputTokens: 30, OutputTokens: 70, TotalTokens: 100},
		},
		{
			original: "TestHandlerPluginExecutorPublishesUsageStreamAntigravity",
			provider: "custom-antigravity-stream-plugin", format: sdktranslator.FormatAntigravity,
			model:   "claude-3-5-sonnet",
			request: `{"model":"claude-3-5-sonnet","stream":true}`,
			chunks: []string{
				"data: {\"response\":{\"usageMetadata\":{\"promptTokenCount\":33," +
					"\"candidatesTokenCount\":67,\"totalTokenCount\":100}}}\n\n",
				"data: [DONE]\n\n",
			},
			want: coreusage.Detail{InputTokens: 33, OutputTokens: 67, TotalTokens: 100},
		},
	} {
		t.Run(
			tt.original, func(t *testing.T) {
				records := registerExecutorUsageCapture(t)
				auth := &coreauth.Auth{ID: "original-selected", Provider: tt.provider, Index: "original-index"}
				checkRequest := func(req pluginapi.ExecutorRequest) {
					if req.AuthID != auth.ID || req.Model != tt.model ||
						!bytes.Equal(req.Payload, []byte(tt.request)) {
						t.Errorf("original plugin request changed: %#v", req)
					}
				}
				adapter := &executorAdapter{
					host: New(), pluginID: tt.provider, provider: tt.provider,
					inputFormats: []sdktranslator.Format{
						tt.format,
					}, outputFormats: []sdktranslator.Format{tt.format},
					executor: &fakeExecutor{
						execute: func(_ context.Context, req pluginapi.ExecutorRequest) (
							pluginapi.ExecutorResponse,
							error,
						) {
							checkRequest(req)
							return pluginapi.ExecutorResponse{Payload: []byte(tt.response)}, nil
						},
						executeStream: func(
							_ context.Context,
							req pluginapi.ExecutorRequest,
						) (pluginapi.ExecutorStreamResponse, error) {
							checkRequest(req)
							chunks := make(chan pluginapi.ExecutorStreamChunk, len(tt.chunks))
							for _, payload := range tt.chunks {
								chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte(payload)}
							}
							close(chunks)
							return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
						},
					},
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				request := coreexecutor.Request{Model: tt.model, Format: tt.format, Payload: []byte(tt.request)}
				stream := len(tt.chunks) > 0
				opts := coreexecutor.Options{Stream: stream, SourceFormat: tt.format, ResponseFormat: tt.format}
				if tt.original == "TestHandlerPluginExecutorSkipsUsageForNestedExecution" {
					manager := coreauth.NewManager(nil, nil, nil)
					manager.RegisterExecutor(adapter)
					if _, err := manager.Register(ctx, auth); err != nil {
						t.Fatal(err)
					}
					registry.GetGlobalRegistry().RegisterClient(
						auth.ID, auth.Provider, []*registry.ModelInfo{{ID: tt.model}},
					)
					t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					handler := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
					result, err := handler.ExecuteModel(
						ctx, handlers.ModelExecutionRequest{
							EntryProtocol: "openai", ExitProtocol: "openai", Model: tt.model, Body: request.Payload,
						},
					)
					if err != nil || !bytes.Equal(result.Body, []byte(tt.response)) {
						t.Fatalf("internal selected response=%s error=%v", result.Body, err)
					}
				} else if stream {
					result, err := adapter.ExecuteStream(ctx, auth, request, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else if result, err := adapter.Execute(
					ctx, auth, request, opts,
				); err != nil || len(result.Payload) == 0 {
					t.Fatalf("original response=%s error=%v", result.Payload, err)
				}
				record := readExecutorUsageRecord(t, records)
				got := record.Detail
				if record.Provider != tt.provider || record.AuthID != auth.ID ||
					record.AuthIndex != auth.Index ||
					record.Stream != stream ||
					record.Failed {
					t.Errorf("original usage identity/outcome = %#v", record)
				}
				outputTotal := got.OutputTokens
				if tt.format == sdktranslator.FormatClaude {
					outputTotal = got.TokenBreakdown.Output.TotalTokens
					if got.OutputTokens != tt.want.OutputTokens-tt.want.ReasoningTokens {
						t.Errorf(
							"Claude non-reasoning output = %d, want %d", got.OutputTokens,
							tt.want.OutputTokens-tt.want.ReasoningTokens,
						)
					}
				}
				if got.InputTokens != tt.want.InputTokens || outputTotal != tt.want.OutputTokens ||
					got.ReasoningTokens != tt.want.ReasoningTokens ||
					got.CacheReadTokens != tt.want.CacheReadTokens ||
					got.CacheCreationTokens != tt.want.CacheCreationTokens ||
					got.TotalTokens != tt.want.TotalTokens {
					t.Errorf("original usage = %#v, want %#v", got, tt.want)
				}
				if tt.tier != "" && record.ResponseServiceTier != tt.tier {
					t.Errorf("response tier=%q, want %q", record.ResponseServiceTier, tt.tier)
				}
				if tt.original == "TestHandlerPluginExecutorPublishesUsageStreamClaude" {
					breakdown := got.TokenBreakdown
					if !breakdown.Valid() || breakdown.TotalTokens != 195 ||
						breakdown.Input.TotalTokens != 170 ||
						breakdown.Input.UncachedTokens != 100 ||
						breakdown.Input.CacheReadTokens != 50 ||
						breakdown.Input.CacheWriteTokens != 20 ||
						breakdown.Output.TotalTokens != 25 ||
						breakdown.Output.NonReasoningTokens != 20 ||
						breakdown.Output.ReasoningTokens != 5 {
						t.Errorf("original Claude breakdown = %#v", breakdown)
					}
				}
				assertNoSecondExecutorUsage(t, records)
			},
		)
	}
}

func TestPluginExecutorOriginalFailureAndCancellation(t *testing.T) {
	for _, original := range []string{
		"TestHandlerPluginExecutorPublishesFailure",
		"TestHandlerPluginExecutorPublishesStreamFailure",
		"TestHandlerPluginExecutorPublishesStreamCancellation",
	} {
		t.Run(
			original, func(t *testing.T) {
				records := registerExecutorUsageCapture(t)
				stream := original != "TestHandlerPluginExecutorPublishesFailure"
				cancelled := original == "TestHandlerPluginExecutorPublishesStreamCancellation"
				provider := "failing-plugin"
				message := "upstream plugin failure"
				if stream {
					provider, message = "failing-stream-plugin", "upstream stream broke"
				}
				if cancelled {
					provider = "canceling-stream-plugin"
				}
				failure := errors.New(message)
				chunks := make(chan pluginapi.ExecutorStreamChunk, 5)
				chunks <- pluginapi.ExecutorStreamChunk{
					Payload: []byte("data: {\"choices\":[{\"delta\":" +
						"{\"content\":" +
						"\"Hi\"}}]}\n\n"),
				}
				if !cancelled {
					chunks <- pluginapi.ExecutorStreamChunk{Err: failure}
					close(chunks)
				}
				adapter := &executorAdapter{
					host: New(), pluginID: provider, provider: provider,
					inputFormats:  []sdktranslator.Format{sdktranslator.FormatOpenAI},
					outputFormats: []sdktranslator.Format{sdktranslator.FormatOpenAI},
					executor: &fakeExecutor{
						execute: func(context.Context, pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
							return pluginapi.ExecutorResponse{}, failure
						},
						executeStream: func(
							context.Context,
							pluginapi.ExecutorRequest,
						) (pluginapi.ExecutorStreamResponse, error) {
							return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
						},
					},
				}
				auth := &coreauth.Auth{
					ID: "original-failure-selected", Provider: provider, Index: "original-failure-index",
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				body := []byte(fmt.Sprintf(`{"model":%q}`, "gpt-4o"))
				if stream {
					body = []byte(fmt.Sprintf(`{"model":%q,"stream":true}`, "gpt-4o"))
				}
				request := coreexecutor.Request{
					Model:   "gpt-4o",
					Format:  sdktranslator.FormatOpenAI,
					Payload: body,
				}
				if stream {
					result, err := adapter.ExecuteStream(ctx, auth, request, coreexecutor.Options{Stream: true})
					if err != nil {
						t.Fatal(err)
					}
					if cancelled {
						select {
						case chunk := <-result.Chunks:
							if len(chunk.Payload) == 0 || chunk.Err != nil {
								t.Fatalf("original first chunk = %#v", chunk)
							}
						case <-time.After(time.Second):
							t.Fatal("original first chunk was not delivered")
						}
						cancel()
						close(chunks)
					}
					var streamErr error
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							streamErr = chunk.Err
						}
					}
					if !cancelled && !errors.Is(streamErr, failure) {
						t.Fatalf("original stream error = %v, want %v", streamErr, failure)
					}
				} else if _, err := adapter.Execute(ctx, auth, request, coreexecutor.Options{}); !errors.Is(
					err, failure,
				) {
					t.Fatalf("original execution error = %v, want %v", err, failure)
				}
				record := readExecutorUsageRecord(t, records)
				if record.Provider != provider || record.AuthID != auth.ID || record.AuthIndex != auth.Index ||
					record.Stream != stream ||
					!record.Failed {
					t.Fatalf("original failed usage = %#v", record)
				}
				if !cancelled && record.Fail.Body != message {
					t.Errorf("original failure body = %q, want %q", record.Fail.Body, message)
				}
				assertNoSecondExecutorUsage(t, records)
			},
		)
	}
}
