package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

const clineFixtureModel = "cline-pass/deepseek-v4.1-flash-1017"

func TestOpenAICompatClineNonstreamResponse(t *testing.T) {
	const completion = `{"id":"cline-fixture","object":"chat.completion","model":"` + clineFixtureModel + `","choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":8}}`
	const textFrame = `data: {"id":"cline-fixture","model":"` + clineFixtureModel + `","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}` + "\n\n"
	const usageFrame = `data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":8}}` + "\n\n"
	for _, tc := range []struct {
		name        string
		body        string
		contentType string
		wantPath    string
		wantText    string
		wantStop    string
		wantError   bool
		wantUsage   bool
	}{
		{name: "wrapped completion", body: `{"success":true,"data":` + completion + `}`, contentType: "application/json", wantPath: "content.0.text", wantText: "answer", wantStop: "end_turn", wantUsage: true},
		{name: "native completion", body: completion, contentType: "application/json", wantPath: "content.0.text", wantText: "answer", wantStop: "end_turn", wantUsage: true},
		{name: "completion without headers", body: completion, wantPath: "content.0.text", wantText: "answer", wantStop: "end_turn", wantUsage: true},
		{name: "streamed completion", body: textFrame + usageFrame + "data: [DONE]\n\n", contentType: "text/event-stream", wantPath: "content.0.text", wantText: "answer", wantStop: "end_turn", wantUsage: true},
		{name: "finished stream without done", body: textFrame + usageFrame, contentType: "text/event-stream", wantPath: "content.0.text", wantText: "answer", wantStop: "end_turn", wantUsage: true},
		{name: "keepalive and multiline CRLF", body: ": keepalive\r\n\r\ndata:\r\n\r\ndata: {\"choices\":[\r\ndata: {\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\r\n\r\n" + usageFrame + "data: [DONE]\r\n\r\n", contentType: "text/event-stream", wantPath: "content.0.text", wantText: "answer", wantStop: "end_turn", wantUsage: true},
		{name: "one token reasoning", body: `data: {"id":"cline-fixture","model":"` + clineFixtureModel + `","choices":[{"index":0,"delta":{"reasoning_content":"Think"},"finish_reason":"length"}]}` + "\n\n" + usageFrame + "data: [DONE]\n\n", contentType: "text/event-stream", wantPath: "content.0.thinking", wantText: "Think", wantStop: "max_tokens", wantUsage: true},
		{name: "business failure", body: `{"success":false,"error":"empty response content"}`, contentType: "application/json", wantError: true},
		{name: "invalid wrapper", body: `{"success":true,"data":null}`, contentType: "application/json", wantError: true},
		{name: "empty completion", body: `{"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`, contentType: "application/json", wantError: true},
		{name: "empty stream", body: "data: [DONE]\n\n", contentType: "text/event-stream", wantError: true},
		{name: "stream business failure", body: `data: {"success":false,"error":"empty response content"}` + "\n\n", contentType: "text/event-stream", wantError: true},
		{name: "unfinished stream", body: `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n" + usageFrame, contentType: "text/event-stream", wantError: true, wantUsage: true},
		{name: "stream error after content", body: textFrame + usageFrame + `data: {"error":{"message":"upstream failed"}}` + "\n\n", contentType: "text/event-stream", wantError: true, wantUsage: true},
		{name: "failure retains usage", body: `{"success":false,"error":"failed","usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":8}}`, contentType: "application/json", wantError: true, wantUsage: true},
		{name: "non boolean success", body: `{"success":"true","data":` + completion + `}`, contentType: "application/json", wantError: true},
		{name: "failed wrapper with valid completion", body: `{"success":false,"data":` + completion + `}`, contentType: "application/json", wantError: true},
		{name: "unsupported content block", body: `{"choices":[{"message":{"content":[{"type":"unknown","text":"lost"}]}}]}`, contentType: "application/json", wantError: true},
		{name: "invalid content array", body: `{"choices":[{"message":{"content":["lost"]}}]}`, contentType: "application/json", wantError: true},
		{name: "role only", body: `data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n", contentType: "text/event-stream", wantError: true},
		{name: "usage only", body: usageFrame + "data: [DONE]\n\n", contentType: "text/event-stream", wantError: true, wantUsage: true},
		{name: "bad JSON frame", body: textFrame + usageFrame + "data: {invalid}\n\n", contentType: "text/event-stream", wantError: true, wantUsage: true},
		{name: "truncated frame", body: strings.TrimSuffix(textFrame, "\n\n"), contentType: "text/event-stream", wantError: true},
		{name: "trailing JSON", body: completion + `{}`, contentType: "application/json", wantError: true},
		{name: "non JSON SSE body", body: completion, contentType: "text/event-stream", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, finish := xaiOutcomeContext(t)
			calls := 0
			var requestBody []byte
			ctx = util.WithRoundTripper(ctx, openAICompatRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.String() != "https://api.cline.bot/api/v1/chat/completions" {
					return nil, fmt.Errorf("unexpected fixture URL: %s", request.URL)
				}
				var err error
				requestBody, err = io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				var headers http.Header
				if tc.contentType != "" {
					headers = http.Header{"Content-Type": {tc.contentType}}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(tc.body)), Request: request}, nil
			}))
			payload := []byte(`{"model":"` + clineFixtureModel + `","messages":[{"role":"user","content":"Reply OK"}],"max_tokens":1,"stream":false}`)
			original := bytes.Clone(payload)
			executor := NewOpenAICompatExecutor("custom-provider-name", &config.Config{})
			auth := &cliproxyauth.Auth{ID: "cline-offline-fixture", Provider: executor.Identifier(), Attributes: map[string]string{"base_url": "https://api.cline.bot/api/v1", "api_key": "synthetic-fixture"}}
			response, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: clineFixtureModel, Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatClaude, OriginalRequest: original})
			if tc.wantError {
				if statusErrCode(err) != http.StatusBadGateway || len(response.Payload) != 0 {
					t.Errorf("invalid upstream response accepted: error=%v payload=%s", err, response.Payload)
				}
			} else if err != nil {
				t.Errorf("Execute: %v", err)
			} else {
				if got := gjson.GetBytes(response.Payload, tc.wantPath).String(); got != tc.wantText {
					t.Errorf("%s=%q, want %q in %s", tc.wantPath, got, tc.wantText, response.Payload)
				}
				if got := gjson.GetBytes(response.Payload, "stop_reason").String(); got != tc.wantStop {
					t.Errorf("stop_reason=%q, want %q", got, tc.wantStop)
				}
				if tc.wantUsage && (gjson.GetBytes(response.Payload, "usage.input_tokens").Int() != 7 || gjson.GetBytes(response.Payload, "usage.output_tokens").Int() != 1) {
					t.Errorf("translated usage lost: %s", response.Payload)
				}
			}
			if calls != 1 || !gjson.GetBytes(requestBody, "stream").Bool() || gjson.GetBytes(requestBody, "max_tokens").Int() != 1 || !gjson.GetBytes(requestBody, "stream_options.include_usage").Bool() {
				t.Errorf("dispatch or original budget changed: calls=%d request=%s", calls, requestBody)
			}
			if !bytes.Equal(payload, original) {
				t.Error("original request mutated")
			}
			records := finish()
			if len(records) != 1 {
				t.Fatalf("usage records=%d, want 1", len(records))
			}
			if records[0].Failed != tc.wantError || records[0].Completed == tc.wantError {
				t.Errorf("wrong final usage outcome: %+v", records[0])
			}
			if tc.wantUsage && (records[0].Detail.InputTokens != 7 || records[0].Detail.OutputTokens != 1 || records[0].Detail.TotalTokens != 8) {
				t.Errorf("observed usage lost: %+v", records[0].Detail)
			}
		})
	}
}

func TestOpenAICompatClineStreamAssembly(t *testing.T) {
	frames := []string{
		`{"id":"assembled","model":"actual-cline-model","choices":[{"index":1,"delta":{"content":"other"}},{"index":0,"delta":{"content":"an","reasoning_details":[{"text":"think"}],"provider_metadata":{"gateway":{"cost":"0.0001"}},"tool_calls":[{"index":1,"id":"call_second","type":"function","function":{"name":"ping","arguments":"{}"}},{"index":0,"id":"call_first","type":"function","function":{"name":"look","arguments":"{\"value\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"swer","reasoning_details":[{"text":"ing"}],"tool_calls":[{"index":0,"function":{"name":"up","arguments":"9007199254740993}"}}]},"finish_reason":"tool_calls"},{"index":1,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`,
		`{"choices":[],"usage":{"prompt_tokens":17,"completion_tokens":2,"total_tokens":19,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":1}},"service_tier":"priority"}`,
		`{"id":"","model":"","choices":[],"usage":null}`,
		`{"choices":[],"usage":{}}`,
		`{"choices":[{"index":0,"delta":{"provider_metadata":{"gateway":{"cost":"0.0002"}}}}]}`,
	}
	var stream strings.Builder
	for _, frame := range frames {
		stream.WriteString("data: " + frame + "\n\n")
	}
	stream.WriteString("data: [DONE]\n\n")
	for _, format := range []sdktranslator.Format{sdktranslator.FormatOpenAI, sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse} {
		t.Run(format.String(), func(t *testing.T) {
			ctx, finish := xaiOutcomeContext(t)
			ctx = util.WithRoundTripper(ctx, openAICompatRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream.String())), Request: request}, nil
			}))
			executor := NewOpenAICompatExecutor("cline-fixture", &config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://api.cline.bot:443/api/v1/"}}
			payload := []byte(`{"model":"` + clineFixtureModel + `","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`)
			response, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: clineFixtureModel, Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: format, OriginalRequest: payload})
			if err != nil {
				t.Fatal(err)
			}
			if response.Headers.Get("Content-Type") != "application/json" {
				t.Errorf("nonstream response retained SSE headers: %v", response.Headers)
			}
			switch format {
			case sdktranslator.FormatOpenAI:
				for path, want := range map[string]string{"id": "assembled", "model": "actual-cline-model", "choices.0.message.content": "answer", "choices.1.message.content": "other", "choices.0.message.tool_calls.0.id": "call_first", "choices.0.message.tool_calls.0.function.name": "lookup", "choices.0.message.tool_calls.0.function.arguments": `{"value":9007199254740993}`, "choices.0.message.tool_calls.1.id": "call_second", "usage.prompt_tokens": "17", "usage.completion_tokens_details.reasoning_tokens": "1", "service_tier": "priority", "choices.0.message.provider_metadata.gateway.cost": "0.0002"} {
					if got := gjson.GetBytes(response.Payload, path).String(); got != want {
						t.Errorf("%s=%q, want %q in %s", path, got, want, response.Payload)
					}
				}
			case sdktranslator.FormatClaude:
				var thinking strings.Builder
				for _, block := range gjson.GetBytes(response.Payload, "content").Array() {
					if block.Get("type").String() == "thinking" {
						thinking.WriteString(block.Get("thinking").String())
					}
				}
				if thinking.String() != "thinking" {
					t.Errorf("thinking fragments lost: %s", response.Payload)
				}
				for path, want := range map[string]string{`content.#(type=="text").text`: "answer", `content.#(type=="tool_use").name`: "lookup", `content.#(type=="tool_use").input.value`: "9007199254740993", "stop_reason": "tool_use", "usage.input_tokens": "14", "usage.output_tokens": "2", "usage.cache_read_input_tokens": "3"} {
					if got := gjson.GetBytes(response.Payload, path).String(); got != want {
						t.Errorf("%s=%q, want %q in %s", path, got, want, response.Payload)
					}
				}
			case sdktranslator.FormatOpenAIResponse:
				if !bytes.Contains(response.Payload, []byte("9007199254740993")) || !bytes.Contains(response.Payload, []byte("lookup")) || !bytes.Contains(response.Payload, []byte("answer")) {
					t.Errorf("Responses output lost actual content: %s", response.Payload)
				}
			}
			records := finish()
			if len(records) != 1 || records[0].Failed || records[0].Detail.InputTokens != 17 || records[0].Detail.OutputTokens != 2 || records[0].Detail.ReasoningTokens != 1 || records[0].ResponseModel != "actual-cline-model" {
				t.Errorf("usage snapshot or actual model lost: %+v", records)
			}
		})
	}
}

func TestOpenAICompatClinePayloadOverrides(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{Name: "cline-fixture", Models: []config.OpenAICompatibilityModel{{Name: clineFixtureModel, UseMaxCompletionTokens: true}}}},
		Payload:             config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: clineFixtureModel, Protocol: "openai"}}, Params: map[string]any{"stream": false, "stream_options.include_usage": false}}}},
	}
	ctx := util.WithRoundTripper(t.Context(), openAICompatRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "stream_options.include_usage").Bool() || gjson.GetBytes(body, "max_completion_tokens").Int() != 1 || gjson.GetBytes(body, "max_tokens").Exists() {
			t.Errorf("explicit payload or original budget changed: %s", body)
		}
		if request.Header.Get("Accept") == "text/event-stream" {
			t.Error("SSE requested after an explicit nonstream override")
		}
		return xaiOutcomeHTTPResponse(request, `{"success":true,"data":{"choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]}}`), nil
	}))
	executor := NewOpenAICompatExecutor("cline-fixture", cfg)
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://api.cline.bot/api/v1", "compat_name": "cline-fixture", "config_index": "0"}}
	payload := []byte(`{"messages":[{"role":"user","content":"hello"}],"max_tokens":1}`)
	response, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: clineFixtureModel, Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
	if err != nil || gjson.GetBytes(response.Payload, "choices.0.message.content").String() != "answer" {
		t.Fatalf("explicit nonstream result: error=%v payload=%s", err, response.Payload)
	}
}

type clineResponseReaderFunc func([]byte) (int, error)

func (read clineResponseReaderFunc) Read(buffer []byte) (int, error) { return read(buffer) }

func TestOpenAICompatClineReadFailureAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%t", cancelled), func(t *testing.T) {
			ctx, finish := xaiOutcomeContext(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			closed := make(chan struct{}, 1)
			ctx = util.WithRoundTripper(ctx, openAICompatRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				frame := `data: {"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":8}}` + "\n\n"
				failure := io.Reader(iotest.ErrReader(io.ErrUnexpectedEOF))
				if cancelled {
					failure = clineResponseReaderFunc(func([]byte) (int, error) { cancel(); return 0, context.Canceled })
				}
				body := io.NopCloser(io.MultiReader(strings.NewReader(frame), failure))
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &openAICompatObservedBody{ReadCloser: body, closed: closed}, Request: request}, nil
			}))
			executor := NewOpenAICompatExecutor("cline-fixture", &config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://api.cline.bot/api/v1"}}
			response, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: clineFixtureModel, Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
			wantError := error(io.ErrUnexpectedEOF)
			if cancelled {
				wantError = context.Canceled
			}
			if !errors.Is(err, wantError) || len(response.Payload) != 0 {
				t.Errorf("partial response accepted: error=%v output=%s", err, response.Payload)
			}
			select {
			case <-closed:
			default:
				t.Error("failed request retained response body")
			}
			records := finish()
			if len(records) != 1 || records[0].Completed || records[0].Detail.InputTokens != 7 || records[0].Detail.OutputTokens != 1 {
				t.Errorf("failure lost observed usage: %+v", records)
			}
		})
	}
}

func TestOpenAICompatClineResponseLimit(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streamed), func(t *testing.T) {
			ctx := util.WithRoundTripper(t.Context(), openAICompatRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				contentType, body := "application/json", strings.Repeat(" ", maxStreamUsageLineBytes+1)
				if streamed {
					contentType = "text/event-stream"
					body = strings.Repeat(": "+strings.Repeat("x", 32764)+"\n\n", maxStreamUsageLineBytes/32768+1)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			}))
			executor := NewOpenAICompatExecutor("cline-fixture", &config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": "https://api.cline.bot/api/v1"}}
			response, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: clineFixtureModel, Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
			if statusErrCode(err) != http.StatusBadGateway || len(response.Payload) != 0 {
				t.Fatalf("oversized response accepted: error=%v", err)
			}
		})
	}
}

func TestOpenAICompatClineDoesNotChangeOtherProviders(t *testing.T) {
	for _, baseURL := range []string{
		"https://example.invalid/api/v1", "https://api.cline.bot.example.invalid/api/v1",
		"http://api.cline.bot/api/v1", "https://api.cline.bot:8443/api/v1",
		"https://api.cline.bot/other", "https://api.cline.bot/api/v1?fixture=1",
	} {
		t.Run(baseURL, func(t *testing.T) {
			ctx := util.WithRoundTripper(context.Background(), openAICompatRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				if gjson.GetBytes(body, "stream").Bool() || gjson.GetBytes(body, "stream_options.include_usage").Exists() {
					t.Errorf("unrelated provider request changed: %s", body)
				}
				return xaiOutcomeHTTPResponse(request, `{"choices":[{"index":0,"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}]}`), nil
			}))
			executor := NewOpenAICompatExecutor("cline-name-is-not-an-endpoint", &config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": baseURL}}
			_, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: "fixture", Payload: []byte(`{"messages":[{"role":"user","content":"hello"}],"stream":false}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
