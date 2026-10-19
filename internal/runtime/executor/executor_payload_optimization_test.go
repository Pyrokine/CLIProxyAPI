package executor

import (
	"bytes"
	"context"
	"fmt"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/sjson"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestEnsureColonSpacedJSONLeavesInvalidPayloadUnchanged(t *testing.T) {
	input := []byte(`{"text":"unterminated}`)
	output := ensureColonSpacedJSON(input)
	if &output[0] != &input[0] || string(output) != string(input) {
		t.Fatal("invalid JSON payload changed")
	}
}

func TestNormalizeKimiToolMessageLinksReusesCanonicalPayload(t *testing.T) {
	input := []byte(`{"messages":[{"role":"assistant","reasoning_content":"checking","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`)
	output, errNormalize := normalizeKimiToolMessageLinks(input)
	if errNormalize != nil {
		t.Fatalf("normalizeKimiToolMessageLinks returned error: %v", errNormalize)
	}
	if &output[0] != &input[0] {
		t.Fatal("canonical Kimi tool history was copied")
	}
}

func TestNormalizeKimiToolMessageLinksPreservesLargeArguments(t *testing.T) {
	input := []byte(`{"messages":[{"role":"assistant","content":"lookup","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":{"id":9007199254740993}}}]},{"role":"tool","call_id":"call_1","content":"ok"}]}`)
	output, errNormalize := normalizeKimiToolMessageLinks(input)
	if errNormalize != nil {
		t.Fatalf("normalizeKimiToolMessageLinks returned error: %v", errNormalize)
	}
	if got := gjson.GetBytes(output, "messages.0.tool_calls.0.function.arguments.id").Raw; got != "9007199254740993" {
		t.Fatalf("argument id = %s, want exact large integer", got)
	}
	if got := gjson.GetBytes(output, "messages.1.tool_call_id").String(); got != "call_1" {
		t.Fatalf("tool_call_id = %q, want call_1", got)
	}
	if got := gjson.GetBytes(output, "messages.0.reasoning_content").String(); got != "lookup" {
		t.Fatalf("reasoning_content = %q, want lookup", got)
	}
}

func TestCodexMultipartImageEditAppendsExistingImages(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, value := range []string{"existing-1", "existing-2"} {
		if errWrite := writer.WriteField("images", value); errWrite != nil {
			t.Fatalf("write images field: %v", errWrite)
		}
	}
	imagePart, errCreate := writer.CreateFormFile("image[]", "source.png")
	if errCreate != nil {
		t.Fatalf("create image field: %v", errCreate)
	}
	if _, errWrite := imagePart.Write([]byte("png-data")); errWrite != nil {
		t.Fatalf("write image data: %v", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("close multipart writer: %v", errClose)
	}

	output, _, errRewrite := codexRewriteOpenAIImageEditMultipartToJSON(
		body.Bytes(), writer.Boundary(),
	)
	if errRewrite != nil {
		t.Fatalf("rewrite multipart payload: %v", errRewrite)
	}
	if got := gjson.GetBytes(output, "images.0").String(); got != "existing-1" {
		t.Fatalf("images.0 = %q", got)
	}
	if got := gjson.GetBytes(output, "images.1").String(); got != "existing-2" {
		t.Fatalf("images.1 = %q", got)
	}
	if got := gjson.GetBytes(output, "images.2.image_url").String(); !strings.HasPrefix(
		got, "data:application/octet-stream;base64,",
	) {
		t.Fatalf("images.2.image_url = %q", got)
	}
}

func TestCodexImageBuildersPreservePayloads(t *testing.T) {
	tool := []byte(`{"type":"image_generation","model":"gpt-image-2"}`)
	request := codexBuildImagesResponsesRequest(
		`draw "this"`, []string{"data:image/png;base64,AA==", "", "data:image/jpeg;base64,BB=="}, tool,
	)
	if !gjson.ValidBytes(request) {
		t.Fatalf("request is invalid JSON: %s", request)
	}
	if got := gjson.GetBytes(request, "input.0.content.0.text").String(); got != `draw "this"` {
		t.Fatalf("prompt = %q", got)
	}
	if got := gjson.GetBytes(request, "input.0.content.#").Int(); got != 3 {
		t.Fatalf("content count = %d, want 3", got)
	}
	if got := gjson.GetBytes(request, "tools.0.model").String(); got != "gpt-image-2" {
		t.Fatalf("tool model = %q", got)
	}

	result := codexImageCallResult{
		Result: "AA==", OutputFormat: "png", RevisedPrompt: `revised "prompt"`, Quality: "high", Size: "1024x1024",
	}
	response, errBuild := codexBuildImagesAPIResponse(
		[]codexImageCallResult{result}, 123, []byte(`{"images":1}`), result, "b64_json",
	)
	if errBuild != nil {
		t.Fatalf("codexBuildImagesAPIResponse returned error: %v", errBuild)
	}
	if !gjson.ValidBytes(response) {
		t.Fatalf("response is invalid JSON: %s", response)
	}
	if got := gjson.GetBytes(response, "data.0.b64_json").String(); got != "AA==" {
		t.Fatalf("b64_json = %q", got)
	}
	if got := gjson.GetBytes(response, "data.0.revised_prompt").String(); got != `revised "prompt"` {
		t.Fatalf("revised_prompt = %q", got)
	}
	if got := gjson.GetBytes(response, "usage.images").Int(); got != 1 {
		t.Fatalf("usage.images = %d", got)
	}
}

// noinspection GoUnusedGlobalVariable
var benchmarkExecutorPayloadOutput []byte

func BenchmarkCodexBuildImagesAPIResponseLargePayload(b *testing.B) {
	image := strings.Repeat("A", 2<<20)
	results := []codexImageCallResult{
		{Result: image, OutputFormat: "png"},
		{Result: image, OutputFormat: "png"},
		{Result: image, OutputFormat: "png"},
		{Result: image, OutputFormat: "png"},
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(image) * len(results)))
	b.ResetTimer()
	for b.Loop() {
		benchmarkExecutorPayloadOutput, _ = codexBuildImagesAPIResponse(
			results, 1, []byte(`{"images":4}`), codexImageCallResult{}, "b64_json",
		)
	}
}

func BenchmarkNormalizeKimiToolMessageLinksLargeSinglePatch(b *testing.B) {
	content := strings.Repeat("x", 8<<20)
	input := []byte(`{"messages":[{"role":"assistant","content":"` + content + `","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for b.Loop() {
		benchmarkExecutorPayloadOutput, _ = normalizeKimiToolMessageLinks(input)
	}
}

func BenchmarkNormalizeKimiToolMessageLinksLargeMultiplePatches(b *testing.B) {
	content := strings.Repeat("x", (8<<20)/32)
	var builder strings.Builder
	builder.Grow(8 << 20)
	builder.WriteString(`{"messages":[`)
	for index := range 32 {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(`{"role":"assistant","content":"`)
		builder.WriteString(content)
		builder.WriteString(`","tool_calls":[{"id":"call_`)
		builder.WriteString(strings.Repeat("x", index%3))
		builder.WriteString(`","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","call_id":"call_`)
		builder.WriteString(strings.Repeat("x", index%3))
		builder.WriteString(`","content":"ok"}`)
	}
	builder.WriteString(`]}`)
	input := []byte(builder.String())
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for b.Loop() {
		benchmarkExecutorPayloadOutput, _ = normalizeKimiToolMessageLinks(input)
	}
}

func BenchmarkNormalizeKimiToolMessageLinksLargeCanonicalPayload(b *testing.B) {
	content := strings.Repeat("x", (8<<20)/64)
	var builder strings.Builder
	builder.Grow(8 << 20)
	builder.WriteString(`{"messages":[`)
	for index := range 64 {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(`{"role":"user","content":"`)
		builder.WriteString(content)
		builder.WriteString(`"}`)
	}
	builder.WriteString(`]}`)
	input := []byte(builder.String())
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()
	for b.Loop() {
		benchmarkExecutorPayloadOutput, _ = normalizeKimiToolMessageLinks(input)
	}
}

func TestPayloadRuleSourceContextReachesProviderConsumers(t *testing.T) {
	for _, tc := range []struct {
		name, model, path string
		create            func(*config.Config) coreauth.ProviderExecutor
		compact           bool
	}{
		{"claude", "claude-sonnet-4-6", "configured_marker", func(cfg *config.Config) coreauth.ProviderExecutor { return NewClaudeExecutor(cfg) }, false},
		{"codex", "gpt-5.4", "configured_marker", func(cfg *config.Config) coreauth.ProviderExecutor { return &codexExecutor{cfg: cfg} }, false},
		{"codex-compact", "gpt-5.4", "configured_marker", func(cfg *config.Config) coreauth.ProviderExecutor { return &codexExecutor{cfg: cfg} }, true},
		{"gemini-cli", "gemini-2.5-pro", "request.configured_marker", func(cfg *config.Config) coreauth.ProviderExecutor { return NewGeminiCLIExecutor(cfg) }, false},
		{"iflow", "gpt-4o", "configured_marker", func(cfg *config.Config) coreauth.ProviderExecutor { return NewIFlowExecutor(cfg) }, false},
		{"kimi", "kimi-k2.5", "configured_marker", func(cfg *config.Config) coreauth.ProviderExecutor { return NewKimiExecutor(cfg) }, false},
		{"qwen", "qwen3-coder-plus", "configured_marker", func(cfg *config.Config) coreauth.ProviderExecutor { return NewQwenExecutor(cfg) }, false},
	} {
		for _, matching := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				if tc.compact && stream {
					continue
				}
				t.Run(fmt.Sprintf("%s/matching=%t/stream=%t", tc.name, matching, stream), func(t *testing.T) {
					var outgoing []byte
					calls := 0
					ctx := util.WithRoundTripper(context.Background(), protectedUtlsRoundTripFunc(func(req *http.Request) (*http.Response, error) {
						calls++
						var err error
						outgoing, err = io.ReadAll(req.Body)
						if err != nil {
							return nil, err
						}
						return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"synthetic request captured"}}`)), Request: req}, nil
					}))
					cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
						Models: []config.PayloadModelRule{{Name: "*", FromProtocol: "openai", Headers: map[string]string{"X-Rule": "enabled"}}},
						Params: map[string]any{"configured_marker": true},
					}}}}
					executor := tc.create(cfg)
					auth := &coreauth.Auth{ID: "synthetic-source-context-" + tc.name, Provider: executor.Identifier(),
						Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://fixture.invalid"},
						Metadata: map[string]any{"access_token": "synthetic-token", "project_id": "synthetic-project", "expired": time.Now().Add(time.Hour).Format(time.RFC3339),
							"token": map[string]any{"access_token": "synthetic-token", "token_type": "Bearer", "expiry": time.Now().Add(time.Hour).Format(time.RFC3339)}},
					}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Stream: stream, Headers: make(http.Header)}
					if matching {
						opts.Headers.Set("X-Rule", "enabled")
					}
					if tc.compact {
						opts.Alt = "responses/compact"
					}
					req := cliproxyexecutor.Request{Model: tc.model, Payload: []byte(`{"model":"` + tc.model + `","messages":[{"role":"user","content":"hi"}]}`)}
					var err error
					if stream {
						_, err = executor.ExecuteStream(ctx, auth, req, opts)
					} else {
						_, err = executor.Execute(ctx, auth, req, opts)
					}
					if calls != 1 || err == nil {
						t.Fatalf("synthetic request calls=%d error=%v, want one captured error response", calls, err)
					}
					if got := gjson.GetBytes(outgoing, tc.path).Bool(); got != matching {
						t.Errorf("configured marker=%t want=%t for source/header gates; outgoing=%s", got, matching, outgoing)
					}
				})
			}
		}
	}
	for _, upstream := range []bool{false, true} {
		for _, matching := range []bool{false, true} {
			t.Run(fmt.Sprintf("claude-count/upstream=%t/matching=%t", upstream, matching), func(t *testing.T) {
				var outgoing []byte
				calls := 0
				ctx := util.WithRoundTripper(context.Background(), protectedUtlsRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					var err error
					outgoing, err = io.ReadAll(req.Body)
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"input_tokens":7}`)), Request: req}, err
				}))
				system := strings.Repeat("synthetic configured system words ", 80)
				rule := config.PayloadRule{Models: []config.PayloadModelRule{{Name: "*", FromProtocol: "claude", Headers: map[string]string{"X-Rule": "enabled"}}}, Params: map[string]any{"system": system}}
				cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{rule}}}
				auth := &coreauth.Auth{Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://fixture.invalid"}}
				if upstream {
					auth.Attributes["base_url"] = "https://api.anthropic.com"
				}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: make(http.Header)}
				if matching {
					opts.Headers.Set("X-Rule", "enabled")
				}
				req := cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: []byte(`{"model":"claude-sonnet-4-6","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`)}
				actual, err := NewClaudeExecutor(cfg).CountTokens(ctx, auth, req, opts)
				if err != nil {
					t.Fatal(err)
				}
				if upstream {
					if calls != 1 || (gjson.GetBytes(outgoing, "system").String() == system) != matching {
						t.Fatalf("count upstream calls=%d system=%s, want configured=%t", calls, gjson.GetBytes(outgoing, "system").Raw, matching)
					}
					return
				}
				if calls != 0 {
					t.Fatalf("local token count dispatched %d requests", calls)
				}
				rule.Models = []config.PayloadModelRule{{Name: "*", Headers: map[string]string{"X-Rule": "enabled"}}}
				referenceCfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{rule}}}
				reference, err := NewClaudeExecutor(referenceCfg).CountTokens(ctx, auth, req, opts)
				if err != nil || !bytes.Equal(actual.Payload, reference.Payload) {
					t.Fatalf("local count did not consume source-scoped system: actual=%s reference=%s error=%v", actual.Payload, reference.Payload, err)
				}
			})
		}
	}
}

func TestRetainedProviderCountsUseConfiguredPayload(t *testing.T) {
	longText := strings.Repeat("synthetic configured count words ", 90)
	for _, tc := range []struct {
		name   string
		create func(*config.Config) coreauth.ProviderExecutor
	}{
		{"iflow", func(cfg *config.Config) coreauth.ProviderExecutor { return NewIFlowExecutor(cfg) }},
		{"qwen", func(cfg *config.Config) coreauth.ProviderExecutor { return NewQwenExecutor(cfg) }},
	} {
		for _, mode := range []string{"control", "override", "default-original-omission"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				ctx := util.WithRoundTripper(context.Background(), protectedUtlsRoundTripFunc(func(*http.Request) (*http.Response, error) {
					t.Error("local count attempted a transport request")
					return nil, fmt.Errorf("unexpected transport request")
				}))
				messages := []any{map[string]any{"role": "user", "content": longText}}
				rule := config.PayloadRule{Models: []config.PayloadModelRule{{Name: "*", FromProtocol: "openai", Headers: map[string]string{"X-Rule": "enabled"}}}, Params: map[string]any{"messages": messages}}
				cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{rule}}}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Headers: make(http.Header)}
				payload := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
				referencePayload := bytes.Clone(payload)
				if mode != "control" {
					opts.Headers.Set("X-Rule", "enabled")
					var err error
					referencePayload, err = sjson.SetBytes(referencePayload, "messages", messages)
					if err != nil {
						t.Fatal(err)
					}
				}
				if mode == "default-original-omission" {
					cfg.Payload.Override = nil
					cfg.Payload.Default = []config.PayloadRule{rule}
					opts.OriginalRequest = []byte(`{"model":"gpt-4o"}`)
				}
				actual, err := tc.create(cfg).CountTokens(ctx, nil, cliproxyexecutor.Request{Model: "gpt-4o", Payload: payload}, opts)
				if err != nil {
					t.Fatal(err)
				}
				refOpts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}
				reference, err := tc.create(&config.Config{}).CountTokens(ctx, nil, cliproxyexecutor.Request{Model: "gpt-4o", Payload: referencePayload}, refOpts)
				if err != nil || !bytes.Equal(actual.Payload, reference.Payload) {
					t.Errorf("configured count=%s want final-body count=%s error=%v", actual.Payload, reference.Payload, err)
				}
			})
		}
	}
	for _, matching := range []bool{false, true} {
		t.Run(fmt.Sprintf("gemini-cli/matching=%t", matching), func(t *testing.T) {
			var outgoing []byte
			calls := 0
			ctx := util.WithRoundTripper(context.Background(), protectedUtlsRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				var err error
				outgoing, err = io.ReadAll(req.Body)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"totalTokens":7}`)), Request: req}, err
			}))
			contents := []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": longText}}}}
			cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
				Models: []config.PayloadModelRule{{Name: "*", FromProtocol: "openai", Headers: map[string]string{"X-Rule": "enabled"}}}, Params: map[string]any{"contents": contents},
			}}}}
			auth := &coreauth.Auth{ID: "synthetic-cli-count", Metadata: map[string]any{"project_id": "synthetic-project", "token": map[string]any{
				"access_token": "synthetic-token", "token_type": "Bearer", "expiry": time.Now().Add(time.Hour).Format(time.RFC3339),
			}}}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Headers: make(http.Header)}
			if matching {
				opts.Headers.Set("X-Rule", "enabled")
			}
			_, err := NewGeminiCLIExecutor(cfg).CountTokens(ctx, auth, cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: []byte(`{"model":"gemini-2.5-pro","messages":[{"role":"user","content":"hi"}]}`)}, opts)
			if err != nil || calls != 1 || (gjson.GetBytes(outgoing, "request.contents.0.parts.0.text").String() == longText) != matching {
				t.Errorf("count request calls=%d configured=%t error=%v body=%s", calls, matching, err, outgoing)
			}
		})
	}
}
