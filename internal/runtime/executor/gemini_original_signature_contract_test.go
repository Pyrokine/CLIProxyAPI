package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/signature"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"google.golang.org/protobuf/encoding/protowire"
)

//goland:noinspection SpellCheckingInspection
const originalClaudeCAISSignature = "CAISqwIKiAEIEBgCKkBHRlRBsNiptQUWfPoOhuQKwi5LnncZVO9bB5jqOs76D7uBtgktML0zqJtNmLHX" +
	"HHcgD6lk4MQu4QBXzFd1lbC3Mg5jbGF1ZGUtZmFibGUtNTgBQgh0aGlua2luZ1okZDk3NDM5NzUtNGJi" +
	"MC00OTM2LTllMjgtZDViMGQyMWJkYzQ4EgxCGh+XVFFFeySAjtAaDL/A1LltGu6MMJ+eXSIwsN0oBpDr" +
	"qLv22UBfkMnTotnIbkvkOyb9xZHgigG6OZVHaI3gThm+maLKmgO5PrFLKlDFYp+YZksy/wKwszJlnLTP" +
	"zAK+NUlfzagOE1ymtZTXhAYK260XyFYmg/te/C231+Fr/hoX+EJoUBnrn0gD7hqMISOT+TaFEuOXYsN5" +
	"17GfaxgB"

func TestGeminiOriginalSignaturesReachExecutors(t *testing.T) {
	inner := protowire.AppendTag(nil, 1, protowire.BytesType)
	inner = protowire.AppendBytes(inner, []byte{0x01, 0x0c, 0x39, 0xd6, 0xc7, 0x34})
	encoded := protowire.AppendTag(nil, 2, protowire.BytesType)
	encoded = protowire.AppendBytes(encoded, inner)
	nativeSignature := base64.StdEncoding.EncodeToString(encoded)
	claudePayload := `{
		"model":"claude-3-7-sonnet-20250219",
		"messages":[
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"Let me think...","signature":"` + originalClaudeCAISSignature + `"},
				{"type":"text","text":"Here is the response."}
			]},
			{"role":"user","content":[{"type":"text","text":"Follow up question."}]}
		]
	}`
	geminiPayload := `{"contents":[
		{"role":"model","parts":[
			{"text":"Let me think...","thought":true,"thoughtSignature":"` + originalClaudeCAISSignature + `"},
			{"text":"Here is the response."}
		]},
		{"role":"user","parts":[{"text":"Follow up question."}]}
	]}`
	functionPayload := `{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"search","args":{"q":"go"}},
			"thoughtSignature":"` + originalClaudeCAISSignature + `"}]},
		{"role":"user","parts":[{"functionResponse":{"name":"search","response":{"result":"found"}}}]}
	]}`
	unsignedPayload := `{"contents":[{"role":"user","parts":[{"text":"Hello world"}]}]}`
	complete := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`
	completeWithUsage := `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},` +
		`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`
	partial := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"chunk\"}]}}]}\n\n"

	for _, test := range []struct {
		name, provider, method, payload, response, wantSignature string
		format                                                   translator.Format
	}{
		{
			"TestGeminiExecutorExecute_SanitizesClaudeCAISSignature",
			"gemini", "execute", claudePayload, completeWithUsage, "", translator.FormatClaude,
		},
		{
			"TestGeminiExecutorExecuteStream_SanitizesClaudeCAISSignature",
			"gemini", "stream", claudePayload, partial, "", translator.FormatClaude,
		},
		{
			"TestGeminiExecutorCountTokens_SanitizesClaudeCAISSignature",
			"gemini", "count", claudePayload, `{"totalTokens": 42}`, "", translator.FormatClaude,
		},
		{
			"TestGeminiExecutorExecute_FunctionCall_ReplacesClaudeSignatureWithBypass",
			"gemini", "execute", functionPayload, complete,
			signature.GeminiSkipThoughtSignatureValidator, translator.FormatGemini,
		},
		{
			"TestGeminiExecutorExecute_PreservesNativeGeminiSignature",
			"gemini", "execute", strings.ReplaceAll(functionPayload, originalClaudeCAISSignature, nativeSignature),
			complete, nativeSignature, translator.FormatGemini,
		},
		{
			"TestGeminiExecutorExecute_UnsignedRequestNotCorrupted",
			"gemini", "execute", unsignedPayload, complete, "", translator.FormatGemini,
		},
		{
			"TestGeminiVertexExecutorExecute_GeminiPayload_SanitizesClaudeCAISSignature",
			"vertex", "execute", geminiPayload, complete, "", translator.FormatGemini,
		},
		{
			"TestGeminiVertexExecutorExecuteStream_GeminiPayload_SanitizesClaudeCAISSignature",
			"vertex", "stream", geminiPayload, partial, "", translator.FormatGemini,
		},
		{
			"TestGeminiVertexExecutorCountTokens_GeminiPayload_SanitizesClaudeCAISSignature",
			"vertex", "count", geminiPayload, `{"totalTokens": 42}`, "", translator.FormatGemini,
		},
		{
			"TestGeminiVertexExecutorExecute_PreservesNativeGeminiSignature",
			"vertex", "execute", strings.ReplaceAll(functionPayload, originalClaudeCAISSignature, nativeSignature),
			complete, nativeSignature, translator.FormatGemini,
		},
		{
			"TestGeminiVertexExecutorExecute_UnsignedRequestNotCorrupted",
			"vertex", "execute", unsignedPayload, complete, "", translator.FormatGemini,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				var upstreamBody []byte
				calls := 0
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				ctx = util.WithRoundTripper(
					ctx, devinTestRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							calls++
							if request.URL.Host != "original-signature.invalid" || request.Method != http.MethodPost {
								t.Errorf("unexpected request: %s %s", request.Method, request.URL)
							}
							action := "generateContent"
							if test.method == "stream" {
								action = "streamGenerateContent"
							} else if test.method == "count" {
								action = "countTokens"
							}
							if !strings.HasSuffix(request.URL.Path, "/gemini-2.5-flash:"+action) {
								t.Errorf("unexpected endpoint: %s", request.URL.Path)
							}
							var err error
							upstreamBody, err = io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							contentType := "application/json"
							if test.method == "stream" {
								contentType = "text/event-stream"
							}
							return &http.Response{
								StatusCode: http.StatusOK, Request: request,
								Header: http.Header{"Content-Type": {contentType}},
								Body:   io.NopCloser(strings.NewReader(test.response)),
							}, nil
						},
					),
				)
				var execute auth.ProviderExecutor = NewGeminiExecutor(&config.Config{})
				key := "test-api-key"
				if test.provider == "vertex" {
					execute = NewGeminiVertexExecutor(&config.Config{})
					key = "test-vertex-key"
				}
				credential := &auth.Auth{
					ID: t.Name(), Provider: test.provider, Attributes: map[string]string{
						"api_key": key, "base_url": "https://original-signature.invalid",
					},
				}
				request := core.Request{Model: "gemini-2.5-flash", Payload: []byte(test.payload)}
				if test.format == translator.FormatClaude {
					request.Metadata = map[string]any{
						"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{IsCompat: true},
					}
				}
				opts := core.Options{SourceFormat: test.format}
				var result core.Response
				var err error
				switch test.method {
				case "count":
					result, err = execute.CountTokens(ctx, credential, request, opts)
				case "stream":
					var stream *core.StreamResult
					stream, err = execute.ExecuteStream(ctx, credential, request, opts)
					if err == nil {
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								var status interface{ StatusCode() int }
								if !errors.As(chunk.Err, &status) || status.StatusCode() != http.StatusBadGateway {
									t.Errorf("partial original response returned unexpected error: %v", chunk.Err)
								}
							}
						}
					}
				default:
					result, err = execute.Execute(ctx, credential, request, opts)
				}
				if err != nil {
					t.Fatal(err)
				}
				if calls != 1 || len(upstreamBody) == 0 {
					t.Fatalf("upstream calls=%d, body=%s", calls, upstreamBody)
				}
				if bytes.Contains(upstreamBody, []byte(originalClaudeCAISSignature)) {
					t.Fatalf("raw Claude CAIS signature reached %s: %s", test.provider, upstreamBody)
				}
				if test.wantSignature != "" {
					if got := gjson.GetBytes(
						upstreamBody, "contents.1.parts.0.thoughtSignature",
					).String(); got != test.wantSignature {
						t.Fatalf("thoughtSignature=%q, want %q; body=%s", got, test.wantSignature, upstreamBody)
					}
				}
				if test.payload == unsignedPayload &&
					gjson.GetBytes(upstreamBody, "contents.0.parts.0.text").String() != "Hello world" {
					t.Fatalf("unsigned content changed: %s", upstreamBody)
				}
				if test.method == "count" {
					path := "totalTokens"
					if test.format == translator.FormatClaude {
						path = "input_tokens"
					}
					if gjson.GetBytes(result.Payload, path).Int() != 42 {
						t.Fatalf("CountTokens response lost original count: %s", result.Payload)
					}
				}
			},
		)
	}
}

func TestGeminiCountTokensOriginalSignatureTypedResponse(t *testing.T) {
	for _, provider := range []string{"gemini", "vertex"} {
		t.Run(
			provider, func(t *testing.T) {
				calls := 0
				ctx := util.WithRoundTripper(
					t.Context(), devinTestRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							calls++
							body, err := io.ReadAll(request.Body)
							if err != nil {
								return nil, err
							}
							if !strings.HasSuffix(request.URL.Path, ":countTokens") ||
								bytes.Contains(body, []byte(originalClaudeCAISSignature)) {
								t.Errorf("invalid actual countTokens request: %s %s", request.URL, body)
							}
							return &http.Response{
								StatusCode: http.StatusBadRequest, Request: request, Header: http.Header{},
								Body: io.NopCloser(
									strings.NewReader(`{"error":{"code":400,"message":"synthetic rejection"}}`),
								),
							}, nil
						},
					),
				)
				var execute auth.ProviderExecutor = NewGeminiExecutor(&config.Config{})
				if provider == "vertex" {
					execute = NewGeminiVertexExecutor(&config.Config{})
				}
				request := core.Request{
					Model: "gemini-2.5-flash", Payload: []byte(`{"contents":[
				{"role":"model","parts":[{"text":"Let me think...","thought":true,
					"thoughtSignature":"` + originalClaudeCAISSignature + `"}]},
				{"role":"user","parts":[{"text":"Follow up question."}]}
			]}`),
				}
				_, err := execute.CountTokens(
					ctx, &auth.Auth{
						ID: t.Name(), Provider: provider,
						Attributes: map[string]string{
							"api_key": "test-vertex-key", "base_url": "https://original-signature.invalid",
						},
					},
					request, core.Options{SourceFormat: translator.FormatGemini},
				)
				var typed core.ExecutionDispositionError
				if calls != 1 || !errors.As(err, &typed) {
					t.Fatalf("actual dispatches=%d, typed error=%v", calls, err)
				}
				disposition := typed.ExecutionDisposition()
				if disposition.DispatchPhase != core.DispatchPhaseResponseStarted ||
					disposition.Outcome != core.ExecutionOutcomeRejectedBeforeProcessing ||
					disposition.RetryDirective != core.RetryDirectiveNoRetry ||
					disposition.EffectScope != core.StateEffectScopeRequest {
					t.Fatalf("countTokens disposition=%+v", disposition)
				}
			},
		)
	}
}
