package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func claudeInputEstimateRequest(model string, explicitOriginal bool) (core.Request, core.Options) {
	original := []byte(`{"model":"` + model + `","max_tokens":32,"stream":true,"messages":[{"role":"user",` +
		`"content":"Please explain the deterministic synthetic stream input token estimate contract."}]}`)
	request := core.Request{Model: model, Payload: bytes.Clone(original)}
	opts := core.Options{
		SourceFormat:   translator.FormatClaude,
		ResponseFormat: translator.FormatClaude,
		Stream:         true,
	}
	if explicitOriginal {
		opts.OriginalRequest = original
		request.Payload = bytes.Replace(
			request.Payload,
			[]byte("Please explain the deterministic synthetic stream input token estimate contract."),
			[]byte("A different normalized request."), 1,
		)
	}
	return request, opts
}

func claudeInputEstimateResponseEvents(model string) []string {
	return []string{
		`{"type":"response.created","response":{"id":"resp-estimate","model":"` + model +
			`","status":"in_progress"}}`,
		`{"type":"response.output_text.delta","item_id":"msg-estimate","output_index":0,` +
			`"content_index":0,"delta":"answer"}`,
		`{"type":"response.completed","response":{"id":"resp-estimate","model":"` + model +
			`","status":"completed","output":[{"type":"message","id":"msg-estimate",` +
			`"role":"assistant","content":[{"type":"output_text","text":"answer"}]}],` +
			`"usage":{"input_tokens":7,"output_tokens":5,"total_tokens":12}}}`,
	}
}

func assertClaudeInputEstimateDelivery(
	t *testing.T, ctx context.Context, result *core.StreamResult,
	records <-chan usage.Record, wantInitial int64,
) {
	t.Helper()
	starts, stops := 0, 0
	var initial int64
	var finalUsage gjson.Result
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		for line := range strings.SplitSeq(string(chunk.Payload), "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			switch event.Get("type").String() {
			case "message_start":
				starts++
				initial = event.Get("message.usage.input_tokens").Int()
			case "message_delta":
				finalUsage = event.Get("usage")
			case "message_stop":
				stops++
			}
		}
	}
	if starts != 1 || initial != wantInitial || stops != 1 {
		t.Errorf("starts=%d initial=%d want=%d stops=%d", starts, initial, wantInitial, stops)
	}
	if finalUsage.Get("input_tokens").Int() != 7 || finalUsage.Get("output_tokens").Int() != 5 {
		t.Errorf("final upstream usage changed: %s", finalUsage.Raw)
	}
	select {
	case record := <-records:
		if record.Failed || record.Detail.InputTokens != 7 || record.Detail.OutputTokens != 5 ||
			record.Detail.TotalTokens != 12 || record.Detail.CachedTokens != 0 {
			t.Errorf("recorded billable usage changed: %+v", record)
		}
	case <-ctx.Done():
		t.Fatal("usage was not published")
	}
}

func TestTranslatedClaudeInputEstimateReachesHTTPConsumers(t *testing.T) {
	for _, provider := range []string{"compat", "codex", "xai", "antigravity"} {
		for _, explicitOriginal := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/original=%t", provider, explicitOriginal), func(t *testing.T) {
					cfg := &config.Config{DisableImageGeneration: config.DisableImageGenerationAll}
					credential := googlePayloadAuth(provider)
					credential.ID = t.Name()
					delete(credential.Attributes, "interactions")
					model := "synthetic-model"
					var implementation coreauth.ProviderExecutor = NewOpenAICompatExecutor("compat", cfg)
					switch provider {
					case "codex":
						implementation, model = newCodexExecutor(cfg), "gpt-5.5"
					case "xai":
						implementation, model = newXAIExecutor(cfg), "grok-4.3"
					case "antigravity":
						implementation, model = NewAntigravityExecutor(cfg), "gemini-2.5-flash"
					}
					request, opts := claudeInputEstimateRequest(model, explicitOriginal)
					before, originalBefore := bytes.Clone(request.Payload), bytes.Clone(opts.OriginalRequest)
					plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
					registerTestUsagePlugin(t, plugin)
					calls := 0
					transport := &auditClaudeContractTransport{
						respond: func(req *http.Request) (*http.Response, error) {
							calls++
							if req.URL.Host != "payload-contract.invalid" {
								return nil, fmt.Errorf("unexpected isolated route %s", req.URL)
							}
							if _, err := io.ReadAll(req.Body); err != nil {
								return nil, err
							}
							wire := "data: " + `{"id":"chat-estimate","model":"synthetic-model","choices":[` +
								(`{"index":0,"delta":{"role":"assistant","content":"answer"},` +
									`"finish_reason":` +
									`null}]}`) +
								"\n\n" +
								"data: " +
								(`{"id":"chat-estimate","choices":[{"index":0,"delta":{},` +
									`"finish_reason":"stop"}],`) +
								(`"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":` +
									`12}}`) +
								("\n\ndata: " +
									"[DONE]\n\n")
							if provider == "codex" || provider == "xai" {
								wire = "data: " + strings.Join(
									claudeInputEstimateResponseEvents(model), "\n\ndata: ",
								) + "\n\n"
							}
							if provider == "antigravity" {
								wire = "data: " + `{"response":{"candidates":[{"content":{"role":"model",` +
									`"parts":[{"text":"answer"}]}}]}}` + "\n\n" +
									"data: " + `{"response":{"candidates":[{"content":{"role":"model","parts":[]},` +
									`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,` +
									`"candidatesTokenCount":5,"totalTokenCount":12}}}` + "\n\n"
							}
							return &http.Response{
								StatusCode: http.StatusOK, Request: req,
								Header: http.Header{"Content-Type": {"text/event-stream"}},
								Body:   io.NopCloser(strings.NewReader(wire)),
							}, nil
						},
					}
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					isolated := auditClaudeContractContext(t, cfg, credential, transport)
					ctx = util.WithRoundTripper(ctx, util.RoundTripperValue(isolated))
					if newProxyAwareHTTPClient(ctx, cfg, credential, 0).Transport != transport ||
						newAntigravityHTTPClient(ctx, cfg, credential, 0).Transport != transport {
						t.Fatal("actual upstream transport is not memory-isolated")
					}
					result, err := implementation.ExecuteStream(ctx, credential, request, opts)
					if err != nil {
						t.Fatal(err)
					}
					assertClaudeInputEstimateDelivery(t, ctx, result, plugin.records, 13)
					if calls != 1 || !bytes.Equal(request.Payload, before) ||
						!bytes.Equal(opts.OriginalRequest, originalBefore) {
						t.Fatalf("calls=%d, caller request changed", calls)
					}
				},
			)
		}
	}
}
