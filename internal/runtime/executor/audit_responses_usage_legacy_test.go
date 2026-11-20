package executor

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/runtime/executor/helps"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestAuditUsageReporterRetainsLegacyBaseURLSources(t *testing.T) {
	for _, test := range []struct {
		name, attribute, metadata, want string
	}{
		{"attribute", "https://custom.endpoint.example.com/v1", "", "https://custom.endpoint.example.com/v1"},
		{"metadata", "", "https://meta.endpoint.example.com/v1", "https://meta.endpoint.example.com/v1"},
		{"attribute-precedence", "https://attr.example.com", "https://meta.example.com", "https://attr.example.com"},
		{"blank-attribute", "   ", "https://meta.example.com", "https://meta.example.com"},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				capture := newCodexContractUsageCapture(t)
				credential := &auth.Auth{
					ID: capture.authID, Provider: "openai", Attributes: map[string]string{"base_url": test.attribute},
					Metadata: map[string]any{"base_url": test.metadata},
				}
				reporter := NewPluginExecutorUsageReporter(t.Context(), "openai", "gpt-5.4", credential)
				reporter.Publish(t.Context(), usage.Detail{TotalTokens: 10})
				record := capture.next(t)
				if record.BaseURL != test.want || record.Detail.TotalTokens != 10 {
					t.Fatalf(
						"selected credential source changed: baseURL=%q want=%q total=%d",
						record.BaseURL,
						test.want,
						record.Detail.TotalTokens,
					)
				}
			},
		)
	}
}

func TestAuditResponsesDetailsPreserveCompactionAndScalarBuckets(t *testing.T) {
	for _, payload := range []string{
		`{"object":"response.compaction","usage":{"input_tokens":10,"output_tokens":4}}`,
		"event: response.compaction\r\ndata: {\"object\":\"response.compaction" +
			"\",\"usage\":{\"input_tokens\":10,\"output_tokens\":4}}\r\n\r\n",
	} {
		if result := helps.EnsureResponsesUsageDetails([]byte(payload)); string(result) != payload {
			t.Fatalf("opaque compaction changed: %s", result)
		}
	}
	payload := []byte((`{"usage":{"input_tokens":10,"input_tokens_details":"unknown","output_tokens":4,"` +
		`output_tokens_details":[],"total_tokens":14}}`))
	result := helps.EnsureResponsesUsageDetails(payload)
	if !gjson.GetBytes(result, "usage.input_tokens_details").IsObject() || !gjson.GetBytes(
		result, "usage.output_tokens_details",
	).IsObject() || gjson.GetBytes(result, "usage.total_tokens").Int() != 14 {
		t.Fatalf("legacy non-object detail buckets not normalized: %s", result)
	}
	if again := helps.EnsureResponsesUsageDetails(result); !bytes.Equal(again, result) {
		t.Fatalf("usage normalization is not idempotent: %s", again)
	}
}

func TestAuditNativeResponsesRetainLegacyUsageDetails(t *testing.T) {
	for _, test := range []struct {
		name, usage, output string
		input, outputCount  int64
	}{
		{
			name: "missing", usage: `{"input_tokens":84,"output_tokens":16,"total_tokens":100}`, input: 84,
			outputCount: 16,
		},
		{
			name: "data-substring", usage: `{"input_tokens":84,"output_tokens":16,"total_tokens":100}`,
			output: `[{"id":"msg_synthetic","type":"message","role":"assistant","status":"completed","` +
				`content":[{"type":"output_text","text":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg"}]}]`,
			input: 84, outputCount: 16,
		},
		{
			name: "existing",
			usage: `{"input_tokens":10,"input_tokens_details":{"cached_tokens":3},"output_` +
				`tokens":4,"output_tokens_details":{"reasoning_tokens":2},"total_tokens` +
				`":14}`,
			input: 10, outputCount: 4,
		},
		{
			name: "null-empty",
			usage: `{"input_tokens":10,"input_tokens_details":null,"output_tokens":4,` +
				`"output_tokens_details":{},"total_tokens":` +
				`14}`,
			input: 10, outputCount: 4,
		},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/stream=%t", test.name, streaming), func(t *testing.T) {
					output := test.output
					if output == "" {
						output = "[]"
					}
					terminal := `{"type":"response.completed","response":{"id":"resp_synthetic",` +
						`"model":"gpt-5.5","status":"completed","output":` +
						output +
						`,"usage":` +
						test.usage +
						`}}`
					wire := "event: response.completed\ndata: " + terminal + "\n\n"
					calls := 0
					ctx := util.WithRoundTripper(
						t.Context(), devinTestRoundTripper(
							func(request *http.Request) (*http.Response, error) {
								calls++
								return &http.Response{
									StatusCode: http.StatusOK,
									Header:     http.Header{"Content-Type": {"text/event-stream"}},
									Body:       io.NopCloser(strings.NewReader(wire)), Request: request,
								}, nil
							},
						),
					)
					credential := runtimeCodexAuth("https://synthetic.invalid")
					request := core.Request{
						Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"synthetic"}`),
					}
					opts := core.Options{
						SourceFormat: translator.FormatCodex, ResponseFormat: translator.FormatOpenAIResponse,
					}
					execute := NewCodexAutoExecutor(&config.Config{})
					var payload []byte
					if streaming {
						result, err := execute.ExecuteStream(ctx, credential, request, opts)
						if err != nil {
							t.Fatal(err)
						}
						for chunk := range result.Chunks {
							if chunk.Acknowledge != nil {
								chunk.Acknowledge(chunk.Err)
							}
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							for line := range bytes.SplitSeq(chunk.Payload, []byte("\n")) {
								line = bytes.TrimPrefix(line, []byte("data: "))
								if gjson.GetBytes(line, "type").String() == "response.completed" {
									payload = []byte(gjson.GetBytes(line, "response").Raw)
								}
							}
						}
					} else {
						result, err := execute.Execute(ctx, credential, request, opts)
						if err != nil {
							t.Fatal(err)
						}
						payload = result.Payload
					}
					usageData := gjson.GetBytes(payload, "usage")
					cached, reasoning := usageData.Get(
						"input_tokens_details.cached_tokens",
					), usageData.Get(
						"output_tokens_details.reasoning_tokens",
					)
					wantCached, wantReasoning := int64(0), int64(0)
					if test.name == "existing" {
						wantCached, wantReasoning = 3, 2
					}
					if calls != 1 || !cached.Exists() || !reasoning.Exists() || cached.Int() != wantCached ||
						reasoning.Int() != wantReasoning ||
						usageData.Get("input_tokens").Int() != test.input ||
						usageData.Get("output_tokens").Int() != test.outputCount {
						t.Fatalf("legacy usage details changed: calls=%d payload=%s", calls, payload)
					}
					if test.output != "" && !bytes.Contains(
						payload, []byte("data:image/png;base64,iVBORw0KGgoAAAANSUhEUg"),
					) {
						t.Fatalf("JSON text containing data: changed: %s", payload)
					}
				},
			)
		}
	}
}
