package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAICompatExecutorUsesConfiguredCompletionTokenField(t *testing.T) {
	for _, tc := range []struct {
		name            string
		stream          bool
		responses       bool
		useMCT          bool
		requestField    string
		upstreamField   string
		unexpectedField string
		value           string
	}{
		{name: "nonstream uses completion tokens", useMCT: true,
			requestField: "max_tokens", upstreamField: "max_completion_tokens", unexpectedField: "max_tokens", value: "128"},
		{name: "stream uses completion tokens", stream: true, useMCT: true,
			requestField: "max_tokens", upstreamField: "max_completion_tokens", unexpectedField: "max_tokens", value: "128"},
		{name: "nonstream uses legacy tokens", requestField: "max_completion_tokens",
			upstreamField: "max_tokens", unexpectedField: "max_completion_tokens", value: "128"},
		{name: "stream uses legacy tokens", stream: true, requestField: "max_completion_tokens",
			upstreamField: "max_tokens", unexpectedField: "max_completion_tokens", value: "128"},
		{name: "Responses use completion tokens", responses: true, useMCT: true,
			requestField: "max_output_tokens", upstreamField: "max_completion_tokens", unexpectedField: "max_tokens", value: "128"},
		{name: "Responses use legacy tokens", responses: true,
			requestField: "max_output_tokens", upstreamField: "max_tokens", unexpectedField: "max_completion_tokens", value: "128"},
		{name: "Responses null completion tokens", responses: true, useMCT: true,
			requestField: "max_output_tokens", upstreamField: "max_completion_tokens", unexpectedField: "max_tokens", value: "null"},
		{name: "Responses null legacy tokens", responses: true,
			requestField: "max_output_tokens", upstreamField: "max_tokens", unexpectedField: "max_completion_tokens", value: "null"},
		{name: "stream Responses null completion tokens", responses: true, stream: true, useMCT: true,
			requestField: "max_output_tokens", upstreamField: "max_completion_tokens", unexpectedField: "max_tokens", value: "null"},
		{name: "stream Responses null legacy tokens", responses: true, stream: true,
			requestField: "max_output_tokens", upstreamField: "max_tokens", unexpectedField: "max_completion_tokens", value: "null"},
		{name: "null completion tokens", useMCT: true,
			requestField: "max_tokens", upstreamField: "max_completion_tokens", unexpectedField: "max_tokens", value: "null"},
		{name: "null legacy tokens", requestField: "max_completion_tokens",
			upstreamField: "max_tokens", unexpectedField: "max_completion_tokens", value: "null"},
		{name: "stream null completion tokens", stream: true, useMCT: true,
			requestField: "max_tokens", upstreamField: "max_completion_tokens", unexpectedField: "max_tokens", value: "null"},
		{name: "stream null legacy tokens", stream: true,
			requestField: "max_completion_tokens", upstreamField: "max_tokens", unexpectedField: "max_completion_tokens", value: "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var err error
				gotBody, err = io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream request: %v", err)
				}
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chatcmpl_1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer server.Close()

			cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
				Name: "compat", Models: []config.OpenAICompatibilityModel{{
					Name: "upstream-model", Alias: "client-model", UseMaxCompletionTokens: tc.useMCT,
				}},
			}}}
			executor := NewOpenAICompatExecutor("compat", cfg)
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url": server.URL + "/v1", "api_key": "test", "compat_name": "compat", "config_index": "0",
			}}
			payload := []byte(`{"model":"upstream-model","messages":[{"role":"user","content":"hi"}],"` + tc.requestField + `":` + tc.value + `}`)
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: tc.stream}
			if tc.responses {
				payload = []byte(`{"model":"upstream-model","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"` + tc.requestField + `":` + tc.value + `}`)
				opts.SourceFormat = sdktranslator.FormatOpenAIResponse
			}
			req := cliproxyexecutor.Request{Model: "upstream-model", Payload: payload}
			if tc.stream {
				result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatalf("ExecuteStream: %v", err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk: %v", chunk.Err)
					}
				}
			} else if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got := gjson.GetBytes(gotBody, tc.upstreamField).Raw; got != tc.value {
				t.Fatalf("upstream %s = %s, want %s in %s", tc.upstreamField, got, tc.value, gotBody)
			}
			if gjson.GetBytes(gotBody, tc.unexpectedField).Exists() {
				t.Fatalf("unexpected %s in %s", tc.unexpectedField, gotBody)
			}
		})
	}
}
