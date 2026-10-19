package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	claudeauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/claude"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type claudeContractTransport func(*http.Request) (*http.Response, error)

func (transport claudeContractTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return transport(req)
}

func claudeContractResponse(req *http.Request, requestBody []byte) *http.Response {
	body := `{"id":"msg_contract","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"synthetic response"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	contentType := "application/json"
	if strings.HasSuffix(req.URL.Path, "/count_tokens") {
		body = `{"input_tokens":7}`
	} else if gjson.GetBytes(requestBody, "stream").Bool() {
		contentType = "text/event-stream"
		body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_contract\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"synthetic response\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func executeClaudeContractRequest(t *testing.T, executor cliproxyauth.ProviderExecutor, ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, operation string) {
	t.Helper()
	var err error
	switch operation {
	case "messages":
		_, err = executor.Execute(ctx, auth, req, opts)
	case "stream":
		var stream *cliproxyexecutor.StreamResult
		stream, err = executor.ExecuteStream(ctx, auth, req, opts)
		if err == nil {
			for chunk := range stream.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
			}
		}
	case "count":
		_, err = executor.CountTokens(ctx, auth, req, opts)
	default:
		t.Fatalf("unknown operation %q", operation)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestClaudeExplicitSystemCacheAtTransport(t *testing.T) {
	const marker = `{ "type": "ephemeral", "ttl": "1h" }`
	for _, test := range []struct {
		name, model, mode, tail, finalModel string
		strict                              bool
	}{
		{name: "terminal system", model: "claude-opus-5", mode: "always"},
		{name: "legacy reminder", model: "claude-haiku-4-5", mode: "always"},
		{name: "mid system", model: "claude-opus-5", mode: "always", tail: `,{"role":"assistant","content":"earlier answer"}`},
		{name: "advisor system", model: "claude-opus-5", mode: "always", tail: `,{"role":"assistant","content":[{"type":"advisor_tool_result","text":"advice"}]}`},
		{name: "payload legacy model", model: "claude-opus-5", mode: "always", finalModel: "claude-haiku-4-5", tail: `,{"role":"assistant","content":"earlier answer"}`},
		{name: "caller owned system", model: "claude-opus-5", mode: "never"},
		{name: "strict system policy", model: "claude-opus-5", mode: "always", strict: true},
	} {
		for _, cached := range []bool{false, true} {
			for _, operation := range []string{"messages", "stream", "count"} {
				t.Run(fmt.Sprintf("%s/cached=%t/%s", test.name, cached, operation), func(t *testing.T) {
					cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "synthetic-contract-key", BaseURL: "https://api.anthropic.com", Cloak: &config.CloakConfig{Mode: test.mode, StrictMode: test.strict}}}}
					if test.finalModel != "" {
						cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: test.model}}, Params: map[string]any{"model": test.finalModel}}}
					}
					cacheField := ""
					if cached {
						cacheField = `,"cache_control":` + marker
					}
					payload := []byte(`{"model":"` + test.model + `","max_tokens":64,"prompt_cache_options":{"mode":"explicit"},"system":[{"type":"text","text":"caller guidance"` + cacheField + `}],"messages":[{"role":"user","content":"Please explain the build result carefully"}` + test.tail + `]}`)
					calls := 0
					var sent []byte
					ctx := util.WithRoundTripper(context.Background(), claudeContractTransport(func(req *http.Request) (*http.Response, error) {
						calls++
						if req.URL.Host != "api.anthropic.com" {
							return nil, fmt.Errorf("unexpected synthetic host: %s", req.URL)
						}
						var err error
						sent, err = io.ReadAll(req.Body)
						if err != nil {
							return nil, err
						}
						return claudeContractResponse(req, sent), nil
					}))
					auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "synthetic-contract-key", "base_url": "https://api.anthropic.com"}}
					executeClaudeContractRequest(t, NewClaudeExecutor(cfg), ctx, auth, cliproxyexecutor.Request{Model: test.model, Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, operation)
					if calls != 1 {
						t.Fatalf("synthetic requests = %d, want 1", calls)
					}
					want := 0
					if cached && !test.strict {
						want = 1
					}
					if countCacheControls(sent) != want {
						t.Fatalf("cache markers = %d, want %d: %s", countCacheControls(sent), want, sent)
					}
					if want == 1 && !strings.Contains(string(sent), `"cache_control":`+marker) {
						t.Fatalf("raw explicit metadata changed: %s", sent)
					}
					if gjson.GetBytes(sent, "prompt_cache_options").Exists() {
						t.Fatalf("proxy cache options reached transport: %s", sent)
					}
					if !test.strict && !strings.Contains(string(sent), "caller guidance") {
						t.Fatalf("caller system text missing: %s", sent)
					}
					if operation == "count" && (strings.Contains(string(sent), "Today's date is") || strings.Contains(string(sent), claudeCodeCLIIdentity)) {
						t.Fatalf("generation-only instructions reached token counting: %s", sent)
					}
				})
			}
		}
	}
}

func TestClaudeExplicitCacheMetadataSurvivesRelocation(t *testing.T) {
	const marker = `{"type":"ephemeral","ttl":"1h","scope":"global","extension":{"value":1}}`
	payload := []byte(`{"model":"claude-opus-5","prompt_cache_options":{"mode":"explicit"},"system":[{"type":"text","text":"guidance","cache_control":` + marker + `}],"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"answer"}]}`)
	after := checkClaudeSystemInstructionsWithSigningModeAt(payload, false, false, "2.1.280", "cli", "", claudeSystemCloakingTestTime, true)
	state := captureClaudeCodeSystemPlacement(payload, after, true, true)
	changed, err := sjson.SetBytes(after, "model", "claude-haiku-4-5")
	if err != nil {
		t.Fatal(err)
	}
	result := reconcileClaudeCodeSystemPlacementAfterPayload(changed, state)
	if !strings.Contains(string(result), `"cache_control":`+marker) || countCacheControls(result) != 1 {
		t.Fatalf("caller raw metadata was lost during model relocation: %s", result)
	}
	if again := reconcileClaudeCodeSystemPlacementAfterPayload(result, state); string(again) != string(result) {
		t.Fatalf("placement reconciliation changed an already relocated request: %s", again)
	}
}

func TestClaudeCountTokensPayloadRules(t *testing.T) {
	const model = "claude-opus-5"
	changed := strings.Repeat("additional configured request content ", 30)
	plain := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"original short input"}]}`)
	withSystem, err := sjson.SetBytes(plain, "system", changed)
	if err != nil {
		t.Fatal(err)
	}
	withContent, err := sjson.SetBytes(plain, "messages.0.content", changed)
	if err != nil {
		t.Fatal(err)
	}
	models := []config.PayloadModelRule{{Name: model, Protocol: "claude"}}
	rawContent, err := json.Marshal(changed)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                        string
		payload, original, expected []byte
		rules                       config.PayloadConfig
		requested                   string
		headers                     http.Header
	}{
		{name: "override", payload: plain, expected: withContent, rules: config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"messages.0.content": changed}}}}},
		{name: "default", payload: plain, expected: withSystem, rules: config.PayloadConfig{Default: []config.PayloadRule{{Models: models, Params: map[string]any{"system": changed}}}}},
		{name: "override raw", payload: plain, expected: withContent, rules: config.PayloadConfig{OverrideRaw: []config.PayloadRule{{Models: models, Params: map[string]any{"messages.0.content": string(rawContent)}}}}},
		{name: "default raw", payload: plain, expected: withSystem, rules: config.PayloadConfig{DefaultRaw: []config.PayloadRule{{Models: models, Params: map[string]any{"system": string(rawContent)}}}}},
		{name: "filter", payload: withSystem, expected: plain, rules: config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: models, Params: []string{"system"}}}}},
		{name: "original default baseline", payload: plain, original: withSystem, expected: plain, rules: config.PayloadConfig{Default: []config.PayloadRule{{Models: models, Params: map[string]any{"system": changed}}}}},
		{name: "requested alias with header", payload: plain, expected: withContent, requested: "caller-alias", headers: http.Header{"X-Contract": {"yes"}}, rules: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "caller-alias", Protocol: "claude", Headers: map[string]string{"X-Contract": "yes"}}}, Params: map[string]any{"messages.0.content": changed}}}}},
		{name: "unmatched protocol", payload: plain, expected: plain, rules: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: model, Protocol: "gemini"}}, Params: map[string]any{"messages.0.content": changed}}}}},
		{name: "unmatched header", payload: plain, expected: plain, rules: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: model, Headers: map[string]string{"X-Contract": "yes"}}}, Params: map[string]any{"messages.0.content": changed}}}}},
	} {
		for _, upstream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/upstream=%t", test.name, upstream), func(t *testing.T) {
				baseURL := "https://gateway.invalid"
				if upstream {
					baseURL = "https://api.anthropic.com"
				}
				calls := 0
				var sent []byte
				ctx := util.WithRoundTripper(context.Background(), claudeContractTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.URL.Host != "api.anthropic.com" || req.URL.Path != "/v1/messages/count_tokens" {
						return nil, fmt.Errorf("unexpected synthetic request: %s", req.URL)
					}
					var readErr error
					sent, readErr = io.ReadAll(req.Body)
					if readErr != nil {
						return nil, readErr
					}
					return claudeContractResponse(req, sent), nil
				}))
				auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "synthetic-contract-key", "base_url": baseURL}}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: test.original, Headers: test.headers}
				if test.requested != "" {
					opts.Metadata = map[string]any{cliproxyexecutor.RequestedModelMetadataKey: test.requested}
				}
				executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true, Payload: test.rules})
				got, err := executor.CountTokens(ctx, auth, cliproxyexecutor.Request{Model: model, Payload: test.payload}, opts)
				if err != nil {
					t.Fatal(err)
				}
				if upstream {
					if calls != 1 {
						t.Fatalf("synthetic requests = %d, want 1", calls)
					}
					var actual, expected any
					if err := json.Unmarshal(sent, &actual); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(test.expected, &expected); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(actual, expected) {
						t.Fatalf("count payload = %s, want %s", sent, test.expected)
					}
				} else {
					if calls != 0 {
						t.Fatalf("local counting made %d requests", calls)
					}
					want, err := countClaudeInputTokens(test.expected)
					if err != nil {
						t.Fatal(err)
					}
					if gjson.GetBytes(got.Payload, "input_tokens").Int() != want {
						t.Fatalf("local count = %s, want %d", got.Payload, want)
					}
				}
			})
		}
	}
}

func TestClaudeCountTokensValidatesPayloadRulesBeforeTransport(t *testing.T) {
	for _, upstream := range []bool{false, true} {
		t.Run(fmt.Sprintf("upstream=%t", upstream), func(t *testing.T) {
			baseURL := "https://gateway.invalid"
			if upstream {
				baseURL = "https://api.anthropic.com"
			}
			calls := 0
			ctx := util.WithRoundTripper(context.Background(), claudeContractTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				return claudeContractResponse(req, nil), nil
			}))
			cfg := &config.Config{Payload: config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "claude-opus-5"}}, Params: []string{"messages"}}}}}
			auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "synthetic-contract-key", "base_url": baseURL}}
			_, err := NewClaudeExecutor(cfg).CountTokens(ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
			if err == nil || calls != 0 {
				t.Fatalf("invalid configured shape: error=%v, requests=%d", err, calls)
			}
			scoped, ok := err.(interface{ IsRequestScoped() bool })
			if !ok || !scoped.IsRequestScoped() {
				t.Fatalf("validation error is not request-scoped: %T %v", err, err)
			}
		})
	}
}

func TestKimiAIClaudeAttributionAtTransport(t *testing.T) {
	for _, operation := range []string{"messages", "stream", "count"} {
		t.Run(operation, func(t *testing.T) {
			calls := 0
			var sent []byte
			ctx := util.WithRoundTripper(context.Background(), claudeContractTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Host != "api.kimi.ai" || !strings.HasPrefix(req.URL.Path, "/coding/v1/messages") {
					return nil, fmt.Errorf("unexpected synthetic Kimi route: %s", req.URL)
				}
				var err error
				sent, err = io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				return claudeContractResponse(req, sent), nil
			}))
			auth := &cliproxyauth.Auth{Provider: "kimi-ai", Metadata: map[string]any{"access_token": "synthetic-contract-kimi-token"}}
			payload := []byte(`{"model":"kimi-k2.5","max_tokens":64,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.280.abc; cc_entrypoint=cli;"},{"type":"text","text":"ordinary caller guidance"}],"messages":[{"role":"user","content":"explain build output"}]}`)
			executeClaudeContractRequest(t, NewKimiAIExecutor(&config.Config{}), ctx, auth, cliproxyexecutor.Request{Model: "kimi-k2.5", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, operation)
			if calls != 1 || strings.Contains(string(sent), "x-anthropic-billing-header") || !strings.Contains(string(sent), "ordinary caller guidance") {
				t.Fatalf("Kimi attribution contract: calls=%d, body=%s", calls, sent)
			}
			if auth.Attributes != nil {
				t.Fatalf("caller auth mutated: %#v", auth.Attributes)
			}
		})
	}
}

func TestKimiAIClaudeCountTokensPayloadRules(t *testing.T) {
	changed := strings.Repeat("additional configured request content ", 30)
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "k2.5", Protocol: "claude"}},
		Params: map[string]any{"messages.0.content": changed},
	}}}}
	calls := 0
	ctx := util.WithRoundTripper(context.Background(), claudeContractTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Host != "api.kimi.ai" || req.URL.Path != "/coding/v1/messages/count_tokens" {
			return nil, fmt.Errorf("unexpected synthetic Kimi route: %s", req.URL)
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if claudeMessageContentText(gjson.GetBytes(body, "messages.0.content")) != changed {
			t.Errorf("Kimi count payload did not apply the rule: %s", body)
		}
		return claudeContractResponse(req, body), nil
	}))
	auth := &cliproxyauth.Auth{Provider: "kimi-ai", Metadata: map[string]any{"access_token": "synthetic-contract-kimi-token"}}
	executeClaudeContractRequest(t, NewKimiAIExecutor(cfg), ctx, auth,
		cliproxyexecutor.Request{Model: "kimi-k2.5", Payload: []byte(`{"model":"kimi-k2.5","messages":[{"role":"user","content":"original short input"}]}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, "count")
	if calls != 1 {
		t.Fatalf("synthetic requests = %d, want 1", calls)
	}
}

func TestClaudeSystemReminderDeduplicationIncludesCacheMetadata(t *testing.T) {
	const marker = `{"type":"ephemeral","ttl":"1h"}`
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>\nguidance\n</system-reminder>","cache_control":` + marker + `},{"type":"text","text":"hello"}]}]}`)
	blocks := []claudeForwardedSystemBlock{{text: "guidance"}, {text: "guidance", cacheControl: marker}}
	result := prependClaudeSystemRemindersToFirstUserMessage(payload, blocks)
	content := gjson.GetBytes(result, "messages.0.content").Array()
	if len(content) != 3 || countCacheControls(result) != 1 {
		t.Fatalf("distinct caller blocks were combined: %s", result)
	}
	if again := prependClaudeSystemRemindersToFirstUserMessage(result, blocks); string(again) != string(result) {
		t.Fatalf("caller reminder insertion is not idempotent: %s", again)
	}
}

func TestClaudeExplicitSystemCacheFromOriginalRequest(t *testing.T) {
	const marker = `{"type":"ephemeral","ttl":"1h"}`
	payload := []byte(`{"model":"claude-opus-5","max_tokens":64,"system":[{"type":"text","text":"guidance","cache_control":` + marker + `}],"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"answer"}]}`)
	original, err := sjson.SetBytes(payload, "prompt_cache_options.mode", "explicit")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "synthetic-contract-key", BaseURL: "https://gateway.invalid", Cloak: &config.CloakConfig{Mode: "always"}}},
		Payload: config.PayloadConfig{Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "claude-opus-5"}}, Params: map[string]any{"model": "claude-haiku-4-5"}}}}}
	auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "synthetic-contract-key", "base_url": "https://gateway.invalid"}}
	prepared, err := NewClaudeExecutor(cfg).prepareClaudeRequest(context.Background(), auth,
		cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: original}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.httpReq.Body.Close()
	body, err := io.ReadAll(prepared.httpReq.Body)
	if err != nil {
		t.Fatal(err)
	}
	if countCacheControls(body) != 1 || !strings.Contains(string(body), `"cache_control":`+marker) {
		t.Fatalf("original explicit mode was lost after relocation: %s", body)
	}
}

func TestKimiMessagesUpstreamIdentity(t *testing.T) {
	for _, test := range []struct {
		provider, endpoint string
		want               bool
	}{
		{"kimi", "https://gateway.invalid", true},
		{" KIMI-AI ", "https://gateway.invalid", true},
		{"claude", " https://API.KIMI.AI/coding ", true},
		{"claude", "https://api.kimi.com/coding", true},
		{"claude", "https://api.kimi.ai.example.invalid/coding", false},
		{"claude", "https://api.kimi.ai@evil.invalid/coding", false},
		{"kimi.ai", "https://gateway.invalid", false},
		{"kimi.com", "https://gateway.invalid", false},
	} {
		t.Run(test.provider+"/"+test.endpoint, func(t *testing.T) {
			if got := isKimiMessagesUpstream(&cliproxyauth.Auth{Provider: test.provider}, test.endpoint); got != test.want {
				t.Fatalf("upstream identity = %t, want %t", got, test.want)
			}
		})
	}
}

func TestClaudeWebSearchDomainNormalizationAtWire(t *testing.T) {
	for _, operation := range []string{"messages", "stream", "count"} {
		t.Run(operation, func(t *testing.T) {
			payload := []byte(`{"model":"claude-opus-5","max_tokens":64,"tools":[{"type":"web_search_20250305","name":"web_search","allowed_domains":["anthropic.com"],"blocked_domains":[],"max_uses":8}],"messages":[{"role":"user","content":"Search for the public release notes"}]}`)
			calls := 0
			var sent []byte
			ctx := util.WithRoundTripper(context.Background(), claudeContractTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.Host != "api.anthropic.com" {
					return nil, fmt.Errorf("unexpected synthetic URL: %s", req.URL)
				}
				var err error
				sent, err = io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				return claudeContractResponse(req, sent), nil
			}))
			auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "synthetic-domain-key", "base_url": "https://api.anthropic.com"}}
			executeClaudeContractRequest(t, NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true}), ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, operation)
			if calls != 1 {
				t.Fatalf("synthetic calls=%d, want 1", calls)
			}
			if gjson.GetBytes(sent, "tools.0.blocked_domains").Exists() || gjson.GetBytes(sent, "tools.0.allowed_domains.0").String() != "anthropic.com" || gjson.GetBytes(sent, "tools.0.max_uses").Int() != 8 {
				t.Errorf("web-search domain conflict was not normalized: %s", sent)
			}
		})
	}
}

func TestClaudeHybridPassthroughMCPAtWire(t *testing.T) {
	for _, shape := range []string{"replaced server", "prepended server", "exact control", "unknown control"} {
		for _, operation := range []string{"messages", "stream"} {
			t.Run(shape+"/"+operation, func(t *testing.T) {
				const model = "claude-opus-5"
				payload := []byte(`{"model":"claude-opus-5","max_tokens":64,"tools":[{"name":"Bash","input_schema":{"type":"object"}},{"name":"mcp__acme__link_pull_request","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"Link this change to the pull request"}]}`)
				calls := 0
				ctx := util.WithRoundTripper(context.Background(), claudeContractTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if req.URL.Host != "api.anthropic.com" {
						return nil, fmt.Errorf("unexpected synthetic URL: %s", req.URL)
					}
					sent, err := io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					parts, ok := parseClaudeMCPAlias(gjson.GetBytes(sent, "tools.0.name").String())
					if !ok || gjson.GetBytes(sent, "tools.1.name").String() != "mcp__acme__link_pull_request" {
						return nil, fmt.Errorf("missing generated or passthrough wire declaration: %s", sent)
					}
					name := "mcp__" + parts.server + "__link_pull_request"
					switch shape {
					case "prepended server":
						name = "mcp__" + parts.server + "__acme__link_pull_request"
					case "exact control":
						name = "mcp__acme__link_pull_request"
					case "unknown control":
						name = "mcp__" + parts.server + "__missing_tool"
					}
					tool := fmt.Sprintf(`{"type":"tool_use","id":"toolu_1","name":%q,"input":{}}`, name)
					body := `{"id":"msg_hybrid","type":"message","role":"assistant","model":"claude-opus-5","content":[` + tool + `],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`
					contentType := "application/json"
					if operation == "stream" {
						contentType = "text/event-stream"
						body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_hybrid\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
							"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":" + tool + "}\n\n" +
							"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
							"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\n" +
							"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				}))
				auth := &cliproxyauth.Auth{ID: "synthetic-hybrid-" + shape + operation, Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-oat-synthetic-hybrid", "base_url": "https://api.anthropic.com", "cloak_mode": "always"}, Metadata: map[string]any{"account_uuid": "11111111-2222-4333-8444-555555555555", "claude_device_ids": []string{"00000000000000000000000000000000000000000000000000000000000000d1"}}}
				executor := NewClaudeExecutor(&config.Config{})
				req := cliproxyexecutor.Request{Model: model, Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}
				var output strings.Builder
				var err error
				if operation == "stream" {
					var result *cliproxyexecutor.StreamResult
					result, err = executor.ExecuteStream(ctx, auth, req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
							output.Write(chunk.Payload)
						}
					}
				} else {
					var result cliproxyexecutor.Response
					result, err = executor.Execute(ctx, auth, req, opts)
					output.Write(result.Payload)
				}
				if calls != 1 {
					t.Fatalf("synthetic calls=%d, want 1", calls)
				}
				if shape == "unknown control" {
					if err == nil {
						t.Fatal("unknown virtual alias was forwarded without an error")
					}
					return
				}
				if err != nil {
					t.Fatalf("declared passthrough tool was rejected: %v", err)
				}
				if !strings.Contains(output.String(), `"name":"mcp__acme__link_pull_request"`) {
					t.Fatalf("caller MCP name missing from response: %s", output.String())
				}
			})
		}
	}
}

func TestClaudeFingerprintProfileConcurrentMetadata(t *testing.T) {
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"fingerprint_profile": "claude-code-cli"}}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for range 1_000 {
			profile, exists, err := cliproxyauth.ClaudeFingerprintProfile(auth)
			if err != nil || !exists || profile != config.ClaudeFingerprintProfileClaudeCodeCLI {
				t.Errorf("ClaudeFingerprintProfile() = %q, %t, %v", profile, exists, err)
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for range 1_000 {
			claudeauth.StoreMetadataString(&auth.Metadata, "account_uuid", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		}
	}()
	workers.Wait()
}

func TestClaudeToolIDRepairPrecedesPayloadRules(t *testing.T) {
	const payload = `{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"use tools"},{"role":"assistant","content":[{"type":"tool_use","id":"Write:1#abc","name":"Write","input":{"literal_id":"Write:1#abc"}},{"type":"tool_use","id":"call_1","name":"Read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"Write:1#abc","content":"result for Write:1#abc"},{"type":"tool_result","tool_use_id":"call_1","content":"done"}]}]}`
	for _, mode := range []string{"repair", "override", "filter"} {
		for _, operation := range []string{"messages", "stream", "count", "local"} {
			t.Run(mode+"/"+operation, func(t *testing.T) {
				cfg := &config.Config{DisableClaudeCloakMode: true}
				expected, err := sjson.SetBytes([]byte(payload), "messages.1.content.0.id", "cpa_tid_v1_V3JpdGU6MSNhYmM")
				if err != nil {
					t.Fatal(err)
				}
				expected, err = sjson.SetBytes(expected, "messages.2.content.0.tool_use_id", "cpa_tid_v1_V3JpdGU6MSNhYmM")
				if err != nil {
					t.Fatal(err)
				}
				if mode == "override" {
					params := map[string]any{"messages.1.content.0.id": "configured:id#override", "messages.2.content.0.tool_use_id": "configured:id#override", "max_tokens": 32}
					cfg.Payload.Override = []config.PayloadRule{{
						Models: []config.PayloadModelRule{{Name: "*", NotMatch: []map[string]any{{"messages.1.content.0.id": "Write:1#abc"}}}},
						Params: params,
					}}
					for path, value := range params {
						expected, err = sjson.SetBytes(expected, path, value)
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				if mode == "filter" {
					paths := []string{"messages.1.content.0.id", "messages.2.content.0.tool_use_id"}
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: paths}}
					for _, path := range paths {
						expected, err = sjson.DeleteBytes(expected, path)
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				calls := 0
				var sent []byte
				ctx := util.WithRoundTripper(t.Context(), claudeContractTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					var readErr error
					sent, readErr = io.ReadAll(req.Body)
					if readErr != nil {
						return nil, readErr
					}
					return claudeContractResponse(req, sent), nil
				}))
				baseURL := "https://api.anthropic.com"
				if operation == "local" {
					baseURL = "https://gateway.invalid"
				}
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "synthetic-tool-id-key", "base_url": baseURL}}
				req := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(payload)}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude, OriginalRequest: []byte(payload)}
				executor := NewClaudeExecutor(cfg)
				if operation == "local" {
					response, countErr := executor.CountTokens(ctx, auth, req, opts)
					if countErr != nil {
						t.Fatal(countErr)
					}
					want, countErr := countClaudeInputTokens(expected)
					if countErr != nil {
						t.Fatal(countErr)
					}
					if calls != 0 || gjson.GetBytes(response.Payload, "input_tokens").Int() != want {
						t.Fatalf("local count = %s, want %d without upstream calls; calls=%d", response.Payload, want, calls)
					}
				} else {
					executeClaudeContractRequest(t, executor, ctx, auth, req, opts, operation)
					if calls != 1 {
						t.Fatalf("upstream calls = %d, want 1", calls)
					}
					for _, path := range []string{"messages.1.content.0.id", "messages.2.content.0.tool_use_id", "messages.1.content.1.id", "messages.2.content.1.tool_use_id", "messages.1.content.0.input.literal_id", "messages.2.content.0.content", "max_tokens"} {
						if gjson.GetBytes(sent, path).Raw != gjson.GetBytes(expected, path).Raw {
							t.Fatalf("%s = %s, want %s", path, gjson.GetBytes(sent, path).Raw, gjson.GetBytes(expected, path).Raw)
						}
					}
				}
				if string(req.Payload) != payload || string(opts.OriginalRequest) != payload {
					t.Fatal("caller request buffers changed")
				}
			})
		}
	}
}

func TestClaudeFallbackUsesFinalModelAtTransport(t *testing.T) {
	for _, test := range []struct {
		name, model, finalModel, extra, want string
		filter                               bool
	}{
		{name: "Opus default", model: "claude-opus-5-5", want: "claude-opus-4-8"},
		{name: "Fable default", model: "claude-fable-5-1", want: "claude-opus-5"},
		{name: "Opus becomes Fable", model: "claude-opus-5-5", finalModel: "claude-fable-5-1", want: "claude-opus-5"},
		{name: "Fable becomes Opus", model: "claude-fable-5-1", finalModel: "claude-opus-5-5", want: "claude-opus-4-8"},
		{name: "Sonnet becomes Opus", model: "claude-sonnet-5", finalModel: "claude-opus-5-5", want: "claude-opus-4-8"},
		{name: "Opus becomes Sonnet", model: "claude-opus-5-5", finalModel: "claude-sonnet-5"},
		{name: "caller fallback", model: "claude-opus-5-5", finalModel: "claude-fable-5-1", extra: `,"fallbacks":[{"model":"caller-model"}]`, want: "caller-model"},
		{name: "filtered fallback", model: "claude-opus-5-5", filter: true},
	} {
		for _, operation := range []string{"messages", "stream"} {
			t.Run(test.name+"/"+operation, func(t *testing.T) {
				cfg := &config.Config{}
				models := []config.PayloadModelRule{{Name: "*", Protocol: "claude"}}
				if test.finalModel != "" {
					cfg.Payload.Override = []config.PayloadRule{{Models: models, Params: map[string]any{"model": test.finalModel}}}
				}
				if test.filter {
					cfg.Payload.Filter = []config.PayloadFilterRule{{Models: models, Params: []string{"fallbacks"}}}
				}
				payload := []byte(`{"model":"` + test.model + `","max_tokens":64,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"Explain the result"}]` + test.extra + `}`)
				before := string(payload)
				calls := 0
				var sent []byte
				ctx := util.WithRoundTripper(context.Background(), claudeContractTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					var err error
					sent, err = io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					return claudeContractResponse(req, sent), nil
				}))
				auth := &cliproxyauth.Auth{ID: "synthetic-fallback-" + test.name + operation, Attributes: map[string]string{"api_key": "sk-ant-oat-synthetic-fallback", "base_url": "https://api.anthropic.com"}, Metadata: map[string]any{"account_uuid": "11111111-2222-4333-8444-555555555555", "claude_device_ids": []string{strings.Repeat("0", 64)}}}
				executeClaudeContractRequest(t, NewClaudeExecutor(cfg), ctx, auth, cliproxyexecutor.Request{Model: test.model, Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, operation)
				if calls != 1 || string(payload) != before {
					t.Fatalf("calls=%d; original payload preserved=%t", calls, string(payload) == before)
				}
				if got := gjson.GetBytes(sent, "fallbacks.0.model").String(); got != test.want {
					t.Fatalf("fallback=%q, want %q: %s", got, test.want, sent)
				}
				if test.filter && gjson.GetBytes(sent, "fallbacks").Exists() {
					t.Fatalf("filtered fallback was recreated: %s", sent)
				}
			})
		}
	}
}

func TestClaudeThreadReservesCacheBreakpoint(t *testing.T) {
	for _, test := range []struct {
		name, thread string
		limit, want  int
	}{
		{name: "absent", limit: 4, want: 4},
		{name: "null", thread: `,"thread":null`, limit: 4, want: 4},
		{name: "create", thread: `,"thread":{"type":"create"}`, limit: 4, want: 3},
		{name: "continue", thread: `,"thread":{"type":"continue","previous_message_id":"msg-prior"}`, limit: 4, want: 3},
		{name: "zero budget", thread: `,"thread":{"type":"create"}`, limit: 0},
		{name: "one breakpoint budget", thread: `,"thread":{"type":"create"}`, limit: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"tools":[{"name":"first","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"one","cache_control":{"type":"ephemeral"}},{"type":"text","text":"two","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"three","cache_control":{"type":"ephemeral"}},{"type":"text","text":"four","cache_control":{"type":"ephemeral"}}]}]` + test.thread + `}`)
			before := string(payload)
			out := enforceCacheControlLimit(payload, test.limit)
			if got := countCacheControls(out); got != test.want {
				t.Fatalf("cache breakpoints=%d, want %d: %s", got, test.want, out)
			}
			if string(payload) != before {
				t.Fatal("cache enforcement mutated its input")
			}
		})
	}
}

func TestClaudeBillingTurnOriginAtTransport(t *testing.T) {
	for _, test := range []struct {
		name, key, mode string
		headers         http.Header
		probe, human    bool
	}{
		{name: "main session", key: "sk-ant-oat-synthetic-origin", human: true},
		{name: "subagent header", key: "sk-ant-oat-synthetic-origin", headers: http.Header{"X-Claude-Code-Agent-Id": {"synthetic-agent"}}},
		{name: "probe", key: "sk-ant-oat-synthetic-origin", probe: true},
		{name: "uncloaked", key: "sk-ant-oat-synthetic-origin", mode: "never"},
		{name: "unsigned API key", key: "synthetic-api-key", mode: "always"},
	} {
		for _, operation := range []string{"messages", "stream"} {
			t.Run(test.name+"/"+operation, func(t *testing.T) {
				maxTokens := 64
				if test.probe {
					maxTokens = 1
				}
				payload := []byte(fmt.Sprintf(`{"model":"claude-opus-5-5","max_tokens":%d,"messages":[{"role":"user","content":"."}]}`, maxTokens))
				var sent []byte
				ctx := util.WithRoundTripper(context.Background(), claudeContractTransport(func(req *http.Request) (*http.Response, error) {
					var err error
					sent, err = io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					return claudeContractResponse(req, sent), nil
				}))
				auth := &cliproxyauth.Auth{ID: "synthetic-origin-" + test.name + operation, Attributes: map[string]string{"api_key": test.key, "base_url": "https://api.anthropic.com", "cloak_mode": test.mode}, Metadata: map[string]any{"account_uuid": "11111111-2222-4333-8444-555555555555", "claude_device_ids": []string{strings.Repeat("0", 64)}}}
				executeClaudeContractRequest(t, NewClaudeExecutor(&config.Config{}), ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5-5", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, Headers: test.headers}, operation)
				billing := gjson.GetBytes(sent, "system.0.text").String()
				if got := strings.Contains(billing, "cc_turn_origin=human;"); got != test.human {
					t.Fatalf("human origin=%t, want %t: %s", got, test.human, billing)
				}
				if strings.Count(billing, "cc_turn_origin=") > 1 {
					t.Fatalf("duplicated turn origin: %s", billing)
				}
			})
		}
	}
}

func TestClaudeOperatorSystemIsPreservedAtTransport(t *testing.T) {
	for _, model := range []string{"claude-sonnet-5", "claude-fable-5-1"} {
		for _, system := range []any{"Operator-owned guidance", []any{map[string]any{"type": "text", "text": "Operator-owned block"}}, nil} {
			for _, operation := range []string{"messages", "stream"} {
				t.Run(fmt.Sprintf("%s/%T/%s", model, system, operation), func(t *testing.T) {
					const baseURL = "https://claude-contract.invalid"
					cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "synthetic-ownership-key", BaseURL: baseURL, Cloak: &config.CloakConfig{Mode: "always"}}}}
					cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: model}}, Params: map[string]any{"model": "claude-fable-5-1", "system": system}}}
					payload := []byte(`{"model":"` + model + `","max_tokens":128,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"Please review each compiler warning and explain its exact code path carefully"}]}`)
					var sent []byte
					ctx := util.WithRoundTripper(context.Background(), claudeContractTransport(func(req *http.Request) (*http.Response, error) {
						var err error
						sent, err = io.ReadAll(req.Body)
						if err != nil {
							return nil, err
						}
						return claudeContractResponse(req, sent), nil
					}))
					auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "synthetic-ownership-key", "base_url": baseURL}}
					executeClaudeContractRequest(t, NewClaudeExecutor(cfg), ctx, auth, cliproxyexecutor.Request{Model: model, Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, operation)
					var actual any
					if err := json.Unmarshal([]byte(gjson.GetBytes(sent, "system").Raw), &actual); err != nil {
						t.Fatalf("invalid operator system: %s, error %v", sent, err)
					}
					if !reflect.DeepEqual(actual, system) {
						t.Fatalf("operator system changed: %#v, want %#v; body=%s", actual, system, sent)
					}
				})
			}
		}
	}
}

func TestClaudeClientToolNamedAdvisorRelocatesAtTransport(t *testing.T) {
	for _, serverTool := range []bool{false, true} {
		for _, operation := range []string{"messages", "stream", "count"} {
			t.Run(fmt.Sprintf("server=%t/%s", serverTool, operation), func(t *testing.T) {
				const baseURL = "https://api.anthropic.com"
				cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "synthetic-advisor-key", BaseURL: baseURL, Cloak: &config.CloakConfig{Mode: "always"}}}}
				kind := "tool_use"
				if serverTool {
					kind = "server_tool_use"
				}
				payload := []byte(`{"model":"claude-opus-5","max_tokens":128,"system":[{"type":"text","text":"caller advisor guidance"}],"tools":[{"name":"advisor","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}],"messages":[{"role":"user","content":"Please examine the compilation details carefully"},{"role":"assistant","content":[{"type":"` + kind + `","id":"toolu_advisor_contract","name":"advisor","input":{"query":"help"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_advisor_contract","content":"client advice text"}]}]}`)
				var sent []byte
				ctx := util.WithRoundTripper(context.Background(), claudeContractTransport(func(req *http.Request) (*http.Response, error) {
					var err error
					sent, err = io.ReadAll(req.Body)
					if err != nil {
						return nil, err
					}
					return claudeContractResponse(req, sent), nil
				}))
				auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "synthetic-advisor-key", "base_url": baseURL}}
				executeClaudeContractRequest(t, NewClaudeExecutor(cfg), ctx, auth, cliproxyexecutor.Request{Model: "claude-opus-5", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, operation)
				if !strings.Contains(string(sent), "caller advisor guidance") {
					t.Fatalf("caller system text missing: %s", sent)
				}
				if top := strings.Contains(gjson.GetBytes(sent, "system").Raw, "caller advisor guidance"); top != serverTool {
					t.Fatalf("caller system top-level = %t, want %t: %s", top, serverTool, sent)
				}
			})
		}
	}
}

func TestClaudePassthroughGatesUnconfirmedNativeHints(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(fmt.Sprint(confirmed), func(t *testing.T) {
			incoming := http.Header{"X-Claude-Code-Request-Class": {"main"}, "X-Claude-Remote-Session-Id": {"synthetic-remote-session"}, "X-Stainless-Async": {"async"}}
			request, err := http.NewRequest(http.MethodPost, "https://claude-contract.invalid/v1/messages", nil)
			if err != nil {
				t.Fatal(err)
			}
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "synthetic-hints-key"}}
			if err := applyClaudeHeaders(request, auth, "synthetic-hints-key", false, nil, []byte(`{"model":"claude-opus-5"}`), &config.Config{DisableClaudeCloakMode: true}, incoming, confirmed); err != nil {
				t.Fatal(err)
			}
			for _, header := range []string{"X-Claude-Code-Request-Class", "X-Claude-Remote-Session-Id"} {
				want := ""
				if confirmed {
					want = incoming.Get(header)
				}
				if got := request.Header.Get(header); got != want {
					t.Errorf("confirmed=%t %s = %q, want %q", confirmed, header, got, want)
				}
			}
			if got := request.Header.Get("X-Stainless-Async"); got != "async" {
				t.Fatalf("caller async header changed: %q", got)
			}
		})
	}
}

func TestClaudePayloadConditionsSeeForcedToolCleanup(t *testing.T) {
	for _, test := range []struct {
		name, choice                                        string
		absentBeforeRules, absentAfterRules, forceInPayload bool
	}{
		{name: "any", choice: `{"type":"any"}`, absentBeforeRules: true, absentAfterRules: true},
		{name: "named-tool", choice: `{"type":"tool","name":"lookup"}`, absentBeforeRules: true, absentAfterRules: true},
		{name: "auto", choice: `{"type":"auto"}`},
		{name: "payload-forces-tool", choice: `{"type":"auto"}`, absentAfterRules: true, forceInPayload: true},
	} {
		for _, operation := range []string{"messages", "stream"} {
			t.Run(test.name+"/"+operation, func(t *testing.T) {
				present := []config.PayloadModelRule{{Name: "*", Exist: []string{"thinking"}}}
				absent := []config.PayloadModelRule{{Name: "*", NotExist: []string{"thinking"}}}
				cfg := &config.Config{Payload: config.PayloadConfig{
					Default:     []config.PayloadRule{{Models: absent, Params: map[string]any{"default_matched": true}}, {Models: present, Params: map[string]any{"default_unmatched": true}}},
					DefaultRaw:  []config.PayloadRule{{Models: absent, Params: map[string]any{"default_raw_matched": `true`}}, {Models: present, Params: map[string]any{"default_raw_unmatched": `true`}}},
					Override:    []config.PayloadRule{{Models: absent, Params: map[string]any{"override_matched": true}}, {Models: present, Params: map[string]any{"override_unmatched": true}}},
					OverrideRaw: []config.PayloadRule{{Models: absent, Params: map[string]any{"override_raw_matched": `true`}}, {Models: present, Params: map[string]any{"override_raw_unmatched": `true`}}},
					Filter:      []config.PayloadFilterRule{{Models: absent, Params: []string{"metadata"}}, {Models: present, Params: []string{"system"}}},
				}}
				if test.forceInPayload {
					cfg.Payload.Override = append(cfg.Payload.Override, config.PayloadRule{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"tool_choice.type": "any"}})
				}
				payload := `{"model":"claude-opus-5","max_tokens":100,"thinking":{"type":"adaptive"},"tool_choice":` + test.choice + `,"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`
				request := cliproxyexecutor.Request{Model: "claude-opus-5", Payload: []byte(payload)}
				calls := 0
				var sent []byte
				ctx := util.WithRoundTripper(context.Background(), claudeContractTransport(func(request *http.Request) (*http.Response, error) {
					calls++
					var err error
					sent, err = io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					return claudeContractResponse(request, sent), nil
				}))
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "synthetic-forced-tool", "cloak_mode": "always"}}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: request.Payload, Stream: operation == "stream"}
				executeClaudeContractRequest(t, NewClaudeExecutor(cfg), ctx, auth, request, opts, operation)
				if calls != 1 || gjson.GetBytes(sent, "thinking").Exists() == test.absentAfterRules {
					t.Fatalf("calls=%d final thinking mismatch: %s", calls, sent)
				}
				for _, prefix := range []string{"default", "default_raw", "override", "override_raw"} {
					matched := gjson.GetBytes(sent, prefix+"_matched").Bool()
					unmatched := gjson.GetBytes(sent, prefix+"_unmatched").Bool()
					if matched != test.absentBeforeRules || unmatched == test.absentBeforeRules {
						t.Errorf("%s condition: matched=%t unmatched=%t want absent=%t", prefix, matched, unmatched, test.absentBeforeRules)
					}
				}
				removed, retained := "system", "metadata"
				if test.absentBeforeRules {
					removed, retained = retained, removed
				}
				if gjson.GetBytes(sent, removed).Exists() || !gjson.GetBytes(sent, retained).Exists() {
					t.Errorf("conditional filter mismatch: remove=%s retain=%s body=%s", removed, retained, sent)
				}
				if string(request.Payload) != payload || string(opts.OriginalRequest) != payload {
					t.Fatal("caller payload changed")
				}
			})
		}
	}
}
