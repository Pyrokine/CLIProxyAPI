package executor

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdk "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func assertResponsesZeroDetails(t *testing.T, usage gjson.Result) {
	t.Helper()
	for _, field := range []string{"input_tokens_details.cached_tokens", "output_tokens_details.reasoning_tokens"} {
		if value := usage.Get(field); value.Type != gjson.Number || value.Int() != 0 {
			t.Fatalf("missing explicit zero %s: %s", field, usage.Raw)
		}
	}
}

func TestResponsesUsageDetailsAtKimiNativeOutput(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
				const response = `{"id":"resp_synthetic","object":"response","status":"completed","output":[]` +
					`,"usage":{"input_tokens":84,"output_tokens":16,"total_tokens":100}}`
				wire, contentType := response, "application/json"
				if stream {
					wire, contentType = "event: response.completed\ndata: {\"type\":\"response.completed\","+
						"\"response\":"+
						response+
						"}\n\n", "text/event-stream"
				}
				ctx := util.WithRoundTripper(
					t.Context(), kimiTokenRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							return &http.Response{
								StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}},
								Body: io.NopCloser(strings.NewReader(wire)), Request: request,
							}, nil
						},
					),
				)
				credential := &auth.Auth{
					ID: t.Name(), Provider: "kimi", Metadata: map[string]any{"access_token": "synthetic-kimi-key"},
				}
				req := core.Request{
					Model:   "kimi-k3",
					Payload: []byte(`{"model":"kimi-k3","input":"synthetic"}`),
				}
				opts := core.Options{
					SourceFormat:   sdk.FormatOpenAIResponse,
					ResponseFormat: sdk.FormatOpenAIResponse,
				}
				executor := NewKimiExecutor(&config.Config{})
				var payload []byte
				if stream {
					result, err := executor.ExecuteStream(ctx, credential, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						for line := range bytes.SplitSeq(chunk.Payload, []byte("\n")) {
							data := bytes.TrimPrefix(line, []byte("data: "))
							if gjson.GetBytes(data, "type").String() == "response.completed" {
								payload = []byte(gjson.GetBytes(data, "response").Raw)
							}
						}
					}
				} else {
					result, err := executor.Execute(ctx, credential, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					payload = result.Payload
				}
				usage := gjson.GetBytes(payload, "usage")
				assertResponsesZeroDetails(t, usage)
				if usage.Get("input_tokens").Int() != 84 || usage.Get("output_tokens").Int() != 16 ||
					usage.Get("total_tokens").Int() != 100 {
					t.Fatalf("native usage totals changed: %s", payload)
				}
			},
		)
	}
}

func TestResponsesRegisteredStreamKeepsOriginalUsageDetails(t *testing.T) {
	request := []byte(`{"model":"deepseek-v4-flash","input":"hi","stream":true}`)
	translated := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"` +
		`}],"stream":true,"stream_options":{"include_usage":true}}`)
	chunks := []string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"` +
			`assistant","content":"hello"},"finish_reason":null}]}`,
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{` +
			`"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens"` +
			`:84,"completion_tokens":16,"total_tokens":100}}`,
		`data: [DONE]`,
	}
	var state any
	found := false
	for _, chunk := range chunks {
		frames := sdk.TranslateStream(
			sdk.WithPluginHooks(
				t.Context(),
				nil,
			),
			sdk.FormatOpenAI,
			sdk.FormatOpenAIResponse,
			"deepseek-v4-flash",
			request,
			translated,
			[]byte(chunk),
			&state,
		)
		for _, frame := range frames {
			for line := range bytes.SplitSeq(frame, []byte("\n")) {
				data := bytes.TrimPrefix(line, []byte("data: "))
				if gjson.GetBytes(data, "type").String() != "response.completed" {
					continue
				}
				found = true
				usage := gjson.GetBytes(data, "response.usage")
				assertResponsesZeroDetails(t, usage)
				if usage.Get("input_tokens").Int() != 84 || usage.Get("output_tokens").Int() != 16 ||
					usage.Get("total_tokens").Int() != 100 {
					t.Fatalf("original registered usage totals changed: %s", data)
				}
			}
		}
	}
	if !found {
		t.Fatal("registered stream did not emit response.completed")
	}
}

func TestResponsesUsageDetailsAtXAIGenerateFalseOutput(t *testing.T) {
	for _, usage := range []string{
		"", `,"usage":null`, `,"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}`,
	} {
		payload := xaiGenerateFalseCompletedPayload(
			[]byte(
				`{"type":"response.created","response":{"id":"synthetic","object":` +
					`"response","output":` +
					`[]` +
					usage +
					`}}`,
			),
		)
		assertResponsesZeroDetails(t, gjson.GetBytes(payload, "response.usage"))
		if gjson.GetBytes(payload, "type").String() != "response.completed" || gjson.GetBytes(
			payload, "response.id",
		).String() != "synthetic" {
			t.Fatalf("warmup response identity changed: %s", payload)
		}
	}
}
