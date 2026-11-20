package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	kimiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/kimi"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestKimiExecutorResponsesStreamPassthrough(t *testing.T) {
	var upstreamURL string
	var upstreamBody []byte
	var streamHeader string

	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				upstreamURL = req.URL.String()
				streamHeader = req.Header.Get("Accept")
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					return nil, errRead
				}
				sseData := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"" +
					"id\":\"resp_stream\",\"status\":\"in_progress\",\"service_tier\":\"default\",\"model\":" +
					"\"k3\"}}\n\nevent: response.output_item.added\ndata: {\"type\":\"response.output_item." +
					"added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"" +
					"content\":[]}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response." +
					"output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello stream\"}\n" +
					"\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\"" +
					":\"resp_stream\",\"status\":\"completed\",\"usage\":{\"total_tokens\":12,\"" +
					"input_tokens\":5,\"output_tokens\":7}}}\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(sseData)),
				}, nil
			},
		),
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-kimi-key"},
	}

	payload := []byte(`{
		"model":"kimi-k3",
		"stream":true,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]
	}`)

	result, err := executor.ExecuteStream(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse,
			Stream:       true,
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	if upstreamURL != "https://api.kimi.com/coding/v1/responses" {
		t.Fatalf("upstreamURL = %q, want %q", upstreamURL, "https://api.kimi.com/coding/v1/responses")
	}
	if streamHeader != "text/event-stream" {
		t.Fatalf("Accept header = %q, want text/event-stream", streamHeader)
	}
	if gotModel := gjson.GetBytes(upstreamBody, "model").String(); gotModel != "k3" {
		t.Fatalf("upstreamBody model = %q, want k3", gotModel)
	}

	var chunks []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		chunks = append(chunks, string(chunk.Payload))
	}
	combined := strings.Join(chunks, "")
	if !strings.Contains(combined, "event: response.created") {
		t.Fatalf("stream chunks missing response.created: %s", combined)
	}
	if !strings.Contains(combined, "event: response.completed") {
		t.Fatalf("stream chunks missing response.completed: %s", combined)
	}
	if !strings.Contains(combined, "hello stream") {
		t.Fatalf("stream chunks missing expected text: %s", combined)
	}
}

func TestKimiExecutorResponsesAppliesSuffixThinking(t *testing.T) {
	var upstreamBody []byte
	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					return nil, errRead
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(
						strings.NewReader(
							`{"id":"resp_think","object":"response","status":"completed","model":` +
								`"k3","output":[],"usage":{"total_tokens":` +
								`2}}`,
						),
					),
				}, nil
			},
		),
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-kimi-key"},
	}

	payload := []byte(`{"model":"kimi-k3","input":[{"type":"message","role":"user","content":"hello"}]}`)
	_, err := executor.Execute(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3(high)",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse,
		},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gotModel := gjson.GetBytes(upstreamBody, "model").String(); gotModel != "k3" {
		t.Fatalf("upstream model = %q, want k3", gotModel)
	}
	if gotEffort := gjson.GetBytes(upstreamBody, "reasoning.effort").String(); gotEffort != "high" {
		t.Fatalf("upstream reasoning.effort = %q, want high; body=%s", gotEffort, upstreamBody)
	}
}

func TestKimiExecutorResponsesSuffixOverridesBodyThinking(t *testing.T) {
	var upstreamBody []byte
	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					return nil, errRead
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(
						strings.NewReader(
							`{"id":"resp_think","object":"response","status":"completed","model":` +
								`"k3","output":[],"usage":{"total_tokens":` +
								`2}}`,
						),
					),
				}, nil
			},
		),
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-kimi-key"},
	}

	payload := []byte(
		`{"model":"kimi-k3","reasoning":{"effort":"low"},"input":[{"type":"message","role":"user","content":"hello"}]}`,
	)
	_, err := executor.Execute(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3(max)",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse,
		},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if gotEffort := gjson.GetBytes(upstreamBody, "reasoning.effort").String(); gotEffort != "max" {
		t.Fatalf(
			"upstream reasoning.effort = %q, want max (suffix overrides body); body=%s",
			gotEffort,
			upstreamBody,
		)
	}
}

func TestKimiExecutorResponsesStreamAppliesSuffixThinking(t *testing.T) {
	var upstreamBody []byte
	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					return nil, errRead
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(
						strings.NewReader(
							"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{" +
								"\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"usage\"" +
								":{\"total_tokens\":2}}}\n\n",
						),
					),
				}, nil
			},
		),
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-kimi-key"},
	}

	payload := []byte(
		`{"model":"kimi-k3","stream":true,"input":[{"type":"message","role":"user","content":"hello"}]}`,
	)
	result, err := executor.ExecuteStream(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3(low)",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse,
			Stream:       true,
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for range result.Chunks {
	}

	if gotEffort := gjson.GetBytes(upstreamBody, "reasoning.effort").String(); gotEffort != "low" {
		t.Fatalf("upstream reasoning.effort = %q, want low; body=%s", gotEffort, upstreamBody)
	}
}

func TestKimiExecutorClaudeRequestPreservesInternalModelSemantics(t *testing.T) {
	var upstreamBody []byte
	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					return nil, errRead
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(
						strings.NewReader(
							`{"id":"msg_test","type":"message","role":"assistant","model":"k2.5","content":[{` +
								`"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"` +
								`input_tokens":1,"output_tokens":1}}`,
						),
					),
				}, nil
			},
		),
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}
	const model = "kimi-k2.5(max)"
	payload := []byte(
		`{"model":"kimi-k2.5(max)","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`,
	)
	response, err := executor.Execute(
		ctx, auth, cliproxyexecutor.Request{
			Model:   model,
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatClaude,
			OriginalRequest: payload,
		},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(upstreamBody, "model").String(); got != "k2.5" {
		t.Fatalf("upstream model = %q, want k2.5", got)
	}
	if got := gjson.GetBytes(upstreamBody, "output_config.effort").String(); got != "high" {
		t.Fatalf("upstream output_config.effort = %q, want high", got)
	}
	if got := gjson.GetBytes(response.Payload, "model").String(); got != model {
		t.Fatalf("response model = %q, want %q", got, model)
	}
}

func TestKimiExecutorCountTokensUsesCanonicalUpstreamModel(t *testing.T) {
	var upstreamRequest *http.Request
	var upstreamBody []byte
	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				upstreamRequest = req.Clone(req.Context())
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					return nil, errRead
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"input_tokens":42}`)),
				}, nil
			},
		),
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}
	payload := []byte(`{"model":"kimi-k3[1m](high)","messages":[{"role":"user","content":"hello"}]}`)
	_, err := executor.CountTokens(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3[1m](high)",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
	)
	if err != nil {
		t.Fatalf("CountTokens() error = %v", err)
	}
	if upstreamRequest == nil {
		t.Fatal("upstream request was not captured")
	}
	if got := upstreamRequest.URL.String(); got != "https://api.kimi.com/coding/v1/messages/count_tokens?beta=true" {
		t.Fatalf("upstream URL = %q, want Kimi count tokens endpoint", got)
	}
	if got := gjson.GetBytes(upstreamBody, "model").String(); got != "k3" {
		t.Fatalf("upstream model = %q, want k3", got)
	}
}

func TestKimiExecutorCountTokensInvalidGzipErrorBodyReturnsDecodeMessage(t *testing.T) {
	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Encoding": []string{"gzip"}},
					Body:       io.NopCloser(strings.NewReader("not-a-valid-gzip-stream")),
				}, nil
			},
		),
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}
	payload := []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hello"}]}`)
	_, err := executor.CountTokens(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
	)
	if code := statusErrCode(err); err == nil || code != http.StatusBadRequest {
		t.Fatalf("CountTokens() error = %v, want status 400", err)
	}
	if !strings.Contains(err.Error(), "failed to decode error response body") {
		t.Fatalf("CountTokens() error = %q, want decode failure", err)
	}
}

func TestKimiExecutorNormalizesToolSchemasForMoonshot(t *testing.T) {
	var upstreamBody []byte
	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					return nil, errRead
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(
						strings.NewReader(
							`{"id":"chatcmpl_test","object":"chat.completion","created":1,"model":"` +
								`k3","choices":[{"index":0,"message":{"role":"assistant","content":"ok"` +
								`},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_toke` +
								`ns":1,"total_tokens":2}}`,
						),
					),
				}, nil
			},
		),
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}

	// Tool with $defs and $ref sibling type, matching the shape used by Codex Desktop app tools
	payload := []byte(`{
		"model":"kimi-k3",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[
			{
				"type":"function",
				"function":{
					"name":"codex_app__automation_update",
					"description":"Update automation",
					"parameters":{
						"$defs":{
							"value":{
								"type":"string"
							}
						},
						"type":"object",
						"properties":{
							"value":{
								"$ref":"#/$defs/value",
								"type":"string",
								"description":"A value"
							}
						}
					}
				}
			}
		]
	}`)

	_, err := executor.Execute(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAI,
			OriginalRequest: payload,
		},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	tool := gjson.GetBytes(upstreamBody, "tools.0.function")
	if !tool.Exists() {
		t.Fatalf("upstream tool function not found: %s", upstreamBody)
	}

	// Moonshot rejects $ref with sibling type; verify $ref is inlined and removed
	if ref := tool.Get("parameters.properties.value.$ref"); ref.Exists() {
		t.Fatalf("upstream tool parameter still contains $ref: %s", tool.Get("parameters").Raw)
	}
	if got := tool.Get("parameters.properties.value.type").String(); got != "string" {
		t.Fatalf("upstream tool parameter value.type = %q, want %q", got, "string")
	}
	if got := tool.Get("parameters.properties.value.description").String(); got != "A value" {
		t.Fatalf("upstream tool parameter value.description = %q, want %q", got, "A value")
	}
	// Verify $defs container was pruned
	if defs := tool.Get("parameters.$defs"); defs.Exists() {
		t.Fatalf("upstream tool parameter still contains $defs: %s", tool.Get("parameters").Raw)
	}
	// Verify explicit object type
	if got := tool.Get("parameters.type").String(); got != "object" {
		t.Fatalf("upstream tool parameters.type = %q, want %q", got, "object")
	}
}

func TestKimiExecutorStreamNormalizesToolSchemasFromResponses(t *testing.T) {
	var upstreamBody []byte
	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					return nil, errRead
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(
						strings.NewReader(
							"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{" +
								"\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"usage\"" +
								":{\"total_tokens\":2}}}\n\n",
						),
					),
				}, nil
			},
		),
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}

	// Codex Responses format payload with tools containing $defs and $ref
	payload := []byte(`{
		"model":"kimi-k3",
		"input":[{"type":"message","role":"user","content":"hello"}],
		"tools":[
			{
				"type":"function",
				"name":"codex_app__automation_update",
				"description":"Update automation",
				"parameters":{
					"$defs":{
						"sub":{
							"type":"string"
						}
					},
					"properties":{
						"field":{
							"$ref":"#/$defs/sub",
							"type":"string"
						}
					}
				}
			}
		]
	}`)

	streamResult, err := executor.ExecuteStream(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAIResponse,
			OriginalRequest: payload,
			Stream:          true,
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	if streamResult != nil && streamResult.Chunks != nil {
		for range streamResult.Chunks {
		}
	}

	tool := gjson.GetBytes(upstreamBody, "tools.0")
	if !tool.Exists() {
		t.Fatalf("upstream tool not found in stream body: %s", upstreamBody)
	}
	if ref := tool.Get("parameters.properties.field.$ref"); ref.Exists() {
		t.Fatalf("upstream stream tool parameter still contains $ref: %s", tool.Get("parameters").Raw)
	}
	if got := tool.Get("parameters.properties.field.type").String(); got != "string" {
		t.Fatalf("upstream stream tool field.type = %q, want %q", got, "string")
	}
	if defs := tool.Get("parameters.$defs"); defs.Exists() {
		t.Fatalf("upstream stream tool parameter still contains $defs: %s", tool.Get("parameters").Raw)
	}
	if got := tool.Get("parameters.type").String(); got != "object" {
		t.Fatalf("upstream stream tool parameters.type = %q, want %q", got, "object")
	}
}

func TestNormalizeKimiToolsDirect(t *testing.T) {
	input := []byte(`{
		"tools":[
			{
				"type":"function",
				"function":{
					"name":"test_fn",
					"parameters":{
						"definitions":{
							"prop":{"type":"number"}
						},
						"properties":{
							"count":{"$ref":"#/definitions/prop","description":"item count"}
						}
					}
				}
			}
		],
		"functions":[
			{
				"name":"legacy_fn",
				"parameters":{
					"properties":{"name":{"type":"string"}}
				}
			}
		]
	}`)

	normalized := normalizeKimiTools(input)

	toolParams := gjson.GetBytes(normalized, "tools.0.function.parameters")
	if toolParams.Get("definitions").Exists() {
		t.Errorf("definitions was not stripped: %s", toolParams.Raw)
	}
	if toolParams.Get("properties.count.$ref").Exists() {
		t.Errorf("$ref was not inlined: %s", toolParams.Raw)
	}
	if got := toolParams.Get("properties.count.type").String(); got != "number" {
		t.Errorf("properties.count.type = %q, want number", got)
	}
	if got := toolParams.Get("properties.count.description").String(); got != "item count" {
		t.Errorf("properties.count.description = %q, want 'item count'", got)
	}
	if got := toolParams.Get("type").String(); got != "object" {
		t.Errorf("tools.0.function.parameters.type = %q, want object", got)
	}

	fnParams := gjson.GetBytes(normalized, "functions.0.parameters")
	if got := fnParams.Get("type").String(); got != "object" {
		t.Errorf("functions.0.parameters.type = %q, want object", got)
	}
}

func TestKimiExecutor_KimiAI_TargetURL(t *testing.T) {
	var requestedURL string
	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				requestedURL = req.URL.String()
				return &http.Response{
					StatusCode: http.StatusOK,
					Body: io.NopCloser(
						strings.NewReader(
							`{"id":"1","object":"chat.completion","choices":[{"message":{"role":` +
								`"assistant","content":` +
								`"hi"}}]}`,
						),
					),
					Header: make(http.Header),
				}, nil
			},
		),
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "kimi-ai-test",
		Provider: "kimi-ai",
		Metadata: map[string]any{"access_token": "ai-access-token"},
	}

	payload := []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hello"}]}`)
	_, err := executor.Execute(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAI,
		},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if requestedURL != "https://api.kimi.ai/coding/v1/chat/completions" {
		t.Fatalf("requestedURL = %q, want https://api.kimi.ai/coding/v1/chat/completions", requestedURL)
	}

	// Also test streaming
	requestedURL = ""
	streamRes, errStream := executor.ExecuteStream(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAI,
		},
	)
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	if streamRes == nil {
		t.Fatal("expected stream result")
	}
	for chunk := range streamRes.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}
	if requestedURL != "https://api.kimi.ai/coding/v1/chat/completions" {
		t.Fatalf("stream requestedURL = %q, want https://api.kimi.ai/coding/v1/chat/completions", requestedURL)
	}
}

func TestKimiExecutor_KimiAI_Refresh(t *testing.T) {
	var refreshURL string
	transport := kimiTokenRoundTripper(
		func(req *http.Request) (*http.Response, error) {
			refreshURL = req.URL.String()
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(
					strings.NewReader(
						`{
				"access_token":"new-ai-token",
				"refresh_token":"new-ai-refresh",
				"token_type":"Bearer",
				"expires_in":3600
			}`,
					),
				),
				Header: make(http.Header),
			}, nil
		},
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "kimi-ai-refresh-test",
		Provider: "kimi-ai",
		Metadata: map[string]any{
			"type":          "kimi-ai",
			"access_token":  "old-token",
			"refresh_token": "old-refresh",
		},
		Storage: &kimiauth.TokenStorage{
			AccessToken:  "old-token",
			RefreshToken: "old-refresh",
			Type:         "kimi-ai",
			Domain:       "kimi.ai",
		},
	}

	ctx := util.WithRoundTripper(context.Background(), transport)
	refreshed, err := executor.Refresh(ctx, auth)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if refreshed == nil {
		t.Fatal("expected refreshed auth")
	}
	if refreshURL != "https://auth.kimi.ai/api/oauth/token" {
		t.Fatalf("refreshURL = %q, want https://auth.kimi.ai/api/oauth/token", refreshURL)
	}
	if refreshed.Metadata["access_token"] != "new-ai-token" {
		t.Fatalf("Metadata[access_token] = %v, want new-ai-token", refreshed.Metadata["access_token"])
	}
	if refreshed.Metadata["type"] != "kimi-ai" {
		t.Fatalf("Metadata[type] = %v, want kimi-ai", refreshed.Metadata["type"])
	}
	if storage, ok := refreshed.Storage.(*kimiauth.TokenStorage); ok {
		if storage.AccessToken != "new-ai-token" {
			t.Fatalf("storage.AccessToken = %q, want new-ai-token", storage.AccessToken)
		}
		if storage.Type != "kimi-ai" {
			t.Fatalf("storage.Type = %q, want kimi-ai", storage.Type)
		}
		if storage.Domain != "kimi.ai" {
			t.Fatalf("storage.Domain = %q, want kimi.ai", storage.Domain)
		}
	}
}

func TestKimiExecutorExecuteResponses_InterleavedToolOutputsReordered(t *testing.T) {
	var upstreamBody []byte
	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					return nil, errRead
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(
						strings.NewReader(
							`{"id":"resp_test","object":"response","status":"completed","model":"ki` +
								`mi-k3","output":[{"type":"message","role":"assistant","content":[{"typ` +
								`e":"output_text","text":"done"}]}],"usage":{"total_tokens":2,"input_to` +
								`kens":1,"output_tokens":1}}`,
						),
					),
				}, nil
			},
		),
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}
	payload := []byte("{\n\t\t\"model\":\"kimi-k3\",\n\t\t\"input\":[\n\t\t\t{\"type\":\"func" +
		`tion_call","call_id":"view_image:31","name":"view_image","arguments":"` +
		"{\\\"id\\\":31}\"},\n\t\t\t{\"type\":\"function_call\",\"call_id\":\"v" +
		`iew_image:32","name":"view_image","arguments":"{\"id\":32}"},` +
		"\n\t\t\t{\"type\":\"function_call_output\",\"call_id\":\"view_image:31" +
		"\",\"output\":\"output_31\"},\n\t\t\t{\"type\":\"message\",\"role\":\"" +
		`developer","content":[{"type":"input_text","text":"interleaved develop` +
		"er context\"}]},\n\t\t\t{\"type\":\"function_call_output\",\"call_id\"" +
		":\"view_image:32\",\"output\":\"output_32\"}\n\t\t]\n\t}")

	_, errExecute := executor.Execute(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAIResponse,
			OriginalRequest: payload,
		},
	)
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}

	input := gjson.GetBytes(upstreamBody, "input").Array()
	if got := len(input); got != 5 {
		t.Fatalf("upstream input count = %d, want 5; body=%s", got, upstreamBody)
	}

	if got := input[2].Get("type").String(); got != "function_call_output" ||
		input[2].Get("call_id").String() != "view_image:31" {
		t.Fatalf("input[2] = %s, want function_call_output for view_image:31", input[2].Raw)
	}
	if got := input[3].Get("type").String(); got != "function_call_output" ||
		input[3].Get("call_id").String() != "view_image:32" {
		t.Fatalf("input[3] = %s, want function_call_output for view_image:32", input[3].Raw)
	}
	if got := input[4].Get("type").String(); got != "message" || input[4].Get("role").String() != "developer" {
		t.Fatalf("input[4] = %s, want deferred developer message", input[4].Raw)
	}
}

func TestKimiExecutorExecuteResponsesStream_InterleavedToolOutputsReordered(t *testing.T) {
	var upstreamBody []byte
	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					return nil, errRead
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(
						strings.NewReader(
							"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_str" +
								"eam\",\"status\":\"completed\",\"output\":[]}}\n\n",
						),
					),
				}, nil
			},
		),
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}
	payload := []byte(("{\n\t\t\"model\":\"kimi-k3\",\n\t\t\"stream\":true,\n\t\t\"input\":[\n\t\t\t{\"type\":" +
		"\"function_call\",\"call_id\":\"view_image:31\",\"name\":\"view_image\",\"arguments\":\"{\\\"id\\\":" +
		"31}\"},\n\t\t\t{\"type\":\"function_call\",\"call_id\":\"view_image:32\",\"name\":\"view_image\",\"" +
		"arguments\":\"{\\\"id\\\":32}\"},\n\t\t\t{\"type\":\"function_call_output\",\"call_id\":\"" +
		"view_image:31\",\"output\":\"output_31\"},\n\t\t\t{\"type\":\"message\",\"role\":\"developer\",\"" +
		"content\":[{\"type\":\"input_text\",\"text\":\"interleaved developer context\"}]},\n\t\t\t{\"type\":" +
		"\"function_call_output\",\"call_id\":\"view_image:32\",\"output\":\"output_32\"}\n\t\t]\n\t}"))

	result, errStream := executor.ExecuteStream(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAIResponse,
			OriginalRequest: payload,
			Stream:          true,
		},
	)
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
		}
	}

	input := gjson.GetBytes(upstreamBody, "input").Array()
	if got := len(input); got != 5 {
		t.Fatalf("upstream input count = %d, want 5; body=%s", got, upstreamBody)
	}

	if got := input[2].Get("type").String(); got != "function_call_output" ||
		input[2].Get("call_id").String() != "view_image:31" {
		t.Fatalf("input[2] = %s, want function_call_output for view_image:31", input[2].Raw)
	}
	if got := input[3].Get("type").String(); got != "function_call_output" ||
		input[3].Get("call_id").String() != "view_image:32" {
		t.Fatalf("input[3] = %s, want function_call_output for view_image:32", input[3].Raw)
	}
	if got := input[4].Get("type").String(); got != "message" || input[4].Get("role").String() != "developer" {
		t.Fatalf("input[4] = %s, want deferred developer message", input[4].Raw)
	}
}
