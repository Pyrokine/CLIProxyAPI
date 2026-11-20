package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexExecutorExecutePublishesMainUsageBeforeImageUsage(t *testing.T) {
	for _, mainUsage := range []bool{true, false} {
		for _, imageUsage := range []struct {
			name   string
			fields string
			want   coreusage.Detail
		}{
			{name: "zero", fields: `,"tool_usage":{"image_gen":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`},
			{
				name: "nonzero",
				fields: `,"tool_usage":{"image_gen":{"input_tokens":10,"output_tokens":20,` +
					`"total_tokens":30,"input_tokens_details":{"cached_tokens":3}}}`,
				want: coreusage.Detail{
					InputTokens: 10, OutputTokens: 20, TotalTokens: 30, CachedTokens: 3, CacheReadTokens: 3,
				},
			},
			{name: "missing"},
		} {
			if !mainUsage && imageUsage.fields == "" {
				continue
			}
			t.Run(
				fmt.Sprintf("main_%t/image_%s", mainUsage, imageUsage.name), func(t *testing.T) {
					var mainFields string
					var wantMain coreusage.Detail
					if mainUsage {
						mainFields = `,"usage":{"input_tokens":100,"output_tokens":40,"total_tokens":140,` +
							`"input_tokens_details":{"cached_tokens":25}}`
						wantMain = coreusage.Detail{
							InputTokens: 100, OutputTokens: 40, TotalTokens: 140, CachedTokens: 25, CacheReadTokens: 25,
						}
					}
					server := httptest.NewServer(
						http.HandlerFunc(
							func(w http.ResponseWriter, r *http.Request) {
								if r.Method != http.MethodPost || r.URL.Path != "/responses" {
									t.Errorf(
										"upstream request = %s %s, want POST /responses",
										r.Method,
										r.URL.Path,
									)
								}
								w.Header().Set("Content-Type", "text/event-stream")
								body := (`{"type":"response.completed","response":{"id":"resp_usage","object":` +
									`"response",`) +
									(`"status":"completed","model":"gpt-5.5","output":[{"type":"message",` +
										`"role":"assistant",`) +
									(`"content":[{"type":"output_text","text":` +
										`"ok"}]}]`) +
									mainFields +
									imageUsage.fields +
									`}}`
								if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
									t.Errorf("write upstream response: %v", err)
								}
							},
						),
					)
					t.Cleanup(server.Close)
					capture := newCodexContractUsageCapture(t)
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					t.Cleanup(cancel)
					ctx = coreusage.WithRequestedModelAlias(ctx, t.Name())
					auth := runtimeCodexAuth(server.URL)
					auth.ID = capture.authID
					executor := newCodexExecutor(&config.Config{})
					resp, err := executor.Execute(
						ctx, auth, cliproxyexecutor.Request{
							Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hi"}`),
						}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
					)
					if err != nil {
						t.Fatal(err)
					}
					if len(resp.Payload) == 0 {
						t.Fatal("Execute returned an empty response")
					}
					main := capture.next(t)
					if main.Model != "gpt-5.5" || main.Alias != t.Name() || main.Failed {
						t.Fatalf("main usage did not arrive first with its requested alias: %+v", main)
					}
					if got := main.Detail; got.InputTokens != wantMain.InputTokens ||
						got.OutputTokens != wantMain.OutputTokens || got.TotalTokens != wantMain.TotalTokens ||
						got.CachedTokens != wantMain.CachedTokens || got.CacheReadTokens != wantMain.CacheReadTokens {
						t.Errorf("main usage = %+v, want %+v", got, wantMain)
					}
					if imageUsage.name == "nonzero" {
						image := capture.next(t)
						if image.Model != codexDefaultImageToolModel || image.Alias != image.Model ||
							image.ResponseModel != "" || image.Failed {
							t.Fatalf(
								"image usage did not retain its own model, alias and empty response model: %+v",
								image,
							)
						}
						if got, want := image.Detail, imageUsage.want; got.InputTokens != want.InputTokens ||
							got.OutputTokens != want.OutputTokens || got.TotalTokens != want.TotalTokens ||
							got.CachedTokens != want.CachedTokens || got.CacheReadTokens != want.CacheReadTokens {
							t.Errorf("image usage = %+v, want %+v", got, want)
						}
					}
				},
			)
		}
	}
}

func TestCodexExecutorExecuteRecordsFinalizedPayloadOverrideReasoningEffort(t *testing.T) {
	testCodexFinalizedReasoningEffort(t, "execute")
}

func TestCodexExecutorExecuteStreamRecordsFinalizedPayloadOverrideReasoningEffort(t *testing.T) {
	testCodexFinalizedReasoningEffort(t, "stream")
}

func TestCodexExecutorExecuteCompactRecordsFinalizedPayloadOverrideReasoningEffort(t *testing.T) {
	testCodexFinalizedReasoningEffort(t, "compact")
}

func testCodexFinalizedReasoningEffort(t *testing.T, operation string) {
	t.Helper()
	models := []string{"gpt-5.6-luna"}
	if operation == "compact" {
		models = []string{"", "codex-served-synthetic"}
	}
	for _, responseModel := range models {
		t.Run(
			"response_model="+responseModel, func(t *testing.T) {
				protocol := "codex"
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
				payload := []byte(`{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"hello"}]}`)
				if operation == "compact" {
					protocol = "openai-response"
					opts.SourceFormat = sdktranslator.FormatOpenAIResponse
					opts.Alt = "responses/compact"
					payload = []byte(`{"model":"gpt-5.6-luna","input":[{"type":"message","role":"user",` +
						`"content":"history"},{"type":"compaction_trigger"}]}`)
				}
				cfg := &config.Config{
					Payload: config.PayloadConfig{
						Override: []config.PayloadRule{
							{
								Models: []config.PayloadModelRule{{Name: "gpt-5.6-luna", Protocol: protocol}},
								Params: map[string]any{"reasoning.effort": "high"},
							},
						},
					},
				}
				requests := make(chan []byte, 1)
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							body, err := io.ReadAll(r.Body)
							if err != nil {
								t.Error(err)
								return
							}
							requests <- body
							response := `{"id":"resp_1","object":"response","status":"completed",` +
								`"model":"gpt-5.6-luna","output":[{"type":"message","role":"assistant",` +
								`"content":[{"type":"output_text","text":"ok"}]}],` +
								`"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}`
							if operation == "compact" {
								if r.URL.Path != "/responses/compact" {
									t.Errorf("compact path = %q", r.URL.Path)
								}
								response = `{"id":"resp_1","object":"response.compaction",` +
									`"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`
								if responseModel != "" {
									response, err = sjson.Set(response, "model", responseModel)
									if err != nil {
										t.Error(err)
										return
									}
								}
								w.Header().Set("Content-Type", "application/json")
							} else {
								w.Header().Set("Content-Type", "text/event-stream")
								response = "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
							}
							if _, err := io.WriteString(w, response); err != nil {
								t.Error(err)
							}
						},
					),
				)
				t.Cleanup(server.Close)
				capture := newCodexContractUsageCapture(t)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				t.Cleanup(cancel)
				ctx = coreusage.WithRequestedModelAlias(ctx, t.Name())
				auth := runtimeCodexAuth(server.URL)
				auth.ID = capture.authID
				executor := newCodexExecutor(cfg)
				req := cliproxyexecutor.Request{Model: "gpt-5.6-luna", Payload: payload}
				if operation == "stream" {
					opts.Stream = true
					result, err := executor.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else {
					result, err := executor.Execute(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					if len(result.Payload) == 0 {
						t.Fatal("Execute returned an empty response")
					}
				}
				select {
				case body := <-requests:
					if effort := gjson.GetBytes(body, "reasoning.effort").String(); effort != "high" {
						t.Fatalf("upstream reasoning.effort = %q, want high", effort)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				record := capture.next(t)
				if record.ReasoningEffort != "high" || record.Model != req.Model || record.Alias != t.Name() ||
					record.ResponseModel != responseModel || record.Failed {
					t.Fatalf(
						"usage lost finalized effort, requested identity or actual response model: %+v",
						record,
					)
				}
			},
		)
	}
}
