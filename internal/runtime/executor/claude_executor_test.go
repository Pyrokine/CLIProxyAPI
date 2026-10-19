package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestSignAnthropicMessagesBodyReplacesCCHPlaceholder(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.63.abc; cc_entrypoint=cli; cch=00000;"}],"messages":[{"role":"user","content":"hi"}]}`)

	out, err := signAnthropicMessagesBody(body)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(out, []byte("cch=00000;")) {
		t.Fatalf("expected cch placeholder to be signed, got %s", out)
	}
	if !gjson.GetBytes(out, "system.0.text").Exists() {
		t.Fatalf("expected signed body to remain valid JSON, got %s", out)
	}
}

func TestRebuildMidSystemMessagesToTopLevel(t *testing.T) {
	payload := []byte(`{
		"system": "Top rule",
		"messages": [
			{"role":"user","content":[{"type":"text","text":"hi"}]},
			{"role":"system","content":"Mid string rule"},
			{"role":"assistant","content":[{"type":"text","text":"ok"}]},
			{"role":"system","content":[{"type":"text","text":"Mid array rule","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"continue"}]}
		]
	}`)

	out := rebuildMidSystemMessagesToTopLevel(payload)
	system := gjson.GetBytes(out, "system").Array()
	if len(system) != 3 {
		t.Fatalf("system has %d items, want 3: %s", len(system), gjson.GetBytes(out, "system").Raw)
	}
	wantTexts := []string{"Top rule", "Mid string rule", "Mid array rule"}
	for i, want := range wantTexts {
		if got := system[i].Get("text").String(); got != want {
			t.Fatalf("system[%d].text = %q, want %q", i, got, want)
		}
	}
	if got := gjson.GetBytes(out, "system.2.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("system.2.cache_control.type = %q, want ephemeral", got)
	}
	if gjson.GetBytes(out, `messages.#(role=="system")`).Exists() {
		t.Fatalf("messages should not contain system role after rebuild: %s", gjson.GetBytes(out, "messages").Raw)
	}
	if got := gjson.GetBytes(out, "messages.#").Int(); got != 3 {
		t.Fatalf("messages count = %d, want 3", got)
	}
}

func TestExperimentalCCHSigningDoesNotAuthorizeRuntime(t *testing.T) {
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "sk-ant-api03-test", ExperimentalCCHSigning: true}}}
	auth := &cliproxyauth.Auth{Provider: "claude", Attributes: map[string]string{"api_key": "sk-ant-api03-test"}}
	policy := resolveClaudeFingerprintPolicy(cfg, auth, "sk-ant-api03-test")
	if policy.ProfileClaudeCodeCLI || claudeCCHSigningEnabled("sk-ant-api03-test", policy.ProfileClaudeCodeCLI, "https://api.anthropic.com") {
		t.Fatal("experimental setting authorized CLI runtime")
	}
}

func TestNewClaudeUtlsHTTPClientUsesFallbackRoundTripper(t *testing.T) {
	client := newClaudeUtlsHTTPClient(context.Background(), &config.Config{}, nil, 0)

	if _, ok := client.Transport.(*claudeFallbackRoundTripper); !ok {
		t.Fatalf("transport = %T, want *claudeFallbackRoundTripper", client.Transport)
	}
}

type protectedUtlsRoundTripFunc func(*http.Request) (*http.Response, error)

func (f protectedUtlsRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNewProtectedUtlsHTTPClientUsesContextRoundTripperForProtectedHost(t *testing.T) {
	called := false
	roundTripper := protectedUtlsRoundTripFunc(
		func(req *http.Request) (*http.Response, error) {
			called = true
			if req.URL.Hostname() != "chatgpt.com" {
				t.Fatalf("hostname = %q, want chatgpt.com", req.URL.Hostname())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("{}")),
				Request:    req,
			}, nil
		},
	)
	ctx := util.WithRoundTripper(context.Background(), roundTripper)
	client := newProtectedUtlsHTTPClient(ctx, nil, nil, 0)

	resp, err := client.Get("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
	if !called {
		t.Fatal("expected context RoundTripper to handle protected host request")
	}
}

func TestNewProtectedUtlsHTTPClientUsesContextRoundTripperForLocalNonProtectedHost(t *testing.T) {
	called := false
	roundTripper := protectedUtlsRoundTripFunc(
		func(req *http.Request) (*http.Response, error) {
			called = true
			if req.URL.Hostname() != "localhost" {
				t.Fatalf("hostname = %q, want localhost", req.URL.Hostname())
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("{}")),
				Request:    req,
			}, nil
		},
	)
	ctx := util.WithRoundTripper(context.Background(), roundTripper)
	client := newProtectedUtlsHTTPClient(ctx, nil, nil, 0)

	resp, err := client.Get("http://localhost:11434/v1/messages")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
	if !called {
		t.Fatal("expected context RoundTripper to handle fallback request")
	}
}

func TestNewProtectedUtlsHTTPClientAllowsLocalFallbackURL(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("{}"))
			},
		),
	)
	defer server.Close()
	client := newProtectedUtlsHTTPClient(context.Background(), nil, nil, 0)

	resp, err := client.Get(server.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestPrepareClaudeRequestSignsOAuthCCHByDefault(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	payload := []byte(`{"model":"claude-opus-4-7","system":"custom system","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`)
	auth := &cliproxyauth.Auth{
		Provider: "claude",
		Metadata: map[string]any{
			"access_token":      "sk-ant-oat-test",
			"account_uuid":      "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			"claude_device_ids": []string{strings.Repeat("a", 64)},
		},
		Attributes: map[string]string{
			"base_url":                 "https://example.com",
			"disable_tool_name_prefix": "true",
		},
	}

	cr, err := executor.prepareClaudeRequest(
		context.Background(), auth,
		cliproxyexecutor.Request{Model: "claude-opus-4-7", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}, false,
	)
	if err != nil {
		t.Fatalf("prepareClaudeRequest error: %v", err)
	}
	body, err := io.ReadAll(cr.httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}

	billing := gjson.GetBytes(body, "system.0.text").String()
	if !strings.Contains(billing, "cch=") {
		t.Fatalf("billing header = %q, want cch", billing)
	}
	if strings.Contains(billing, "cch=00000;") {
		t.Fatalf("billing header was not signed: %q", billing)
	}
}

func TestApplyCloakingGlobalDisableLeavesPayloadUnchanged(t *testing.T) {
	payload := []byte(`{"system":"custom system","messages":[{"role":"user","content":"hello"}]}`)
	out, cloaked, err := applyCloaking(
		context.Background(), &config.Config{DisableClaudeCloakMode: true}, nil, payload, "claude-opus-4-7",
		"sk-ant-oat-test", false,
	)
	if err != nil {
		t.Fatalf("applyCloaking() error = %v", err)
	}
	if cloaked {
		t.Fatal("applyCloaking() cloaked = true, want false")
	}
	if string(out) != string(payload) {
		t.Fatalf("payload changed while cloak mode disabled: %s", string(out))
	}
}

func TestApplyClaudeToolPrefix(t *testing.T) {
	input := []byte(
		`{"tools":[{"name":"alpha"},{"name":"proxy_bravo"}],"tool_choice":{"type":"tool","name":"charlie"},"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"delta","id":"t1","input":{}}]}]}`,
	)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_alpha" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_alpha")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_bravo" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_bravo")
	}
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "proxy_charlie" {
		t.Fatalf("tool_choice.name = %q, want %q", got, "proxy_charlie")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "proxy_delta" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "proxy_delta")
	}
}

func TestApplyClaudeToolPrefix_WithToolReference(t *testing.T) {
	input := []byte(
		`{"tools":[{"name":"alpha"}],"messages":[{"role":"user","content":[{"type":"tool_reference","tool_name":"beta"},{"type":"tool_reference","tool_name":"proxy_gamma"}]}]}`,
	)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "messages.0.content.0.tool_name").String(); got != "proxy_beta" {
		t.Fatalf("messages.0.content.0.tool_name = %q, want %q", got, "proxy_beta")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.tool_name").String(); got != "proxy_gamma" {
		t.Fatalf("messages.0.content.1.tool_name = %q, want %q", got, "proxy_gamma")
	}
}

func TestApplyClaudeToolPrefix_SkipsBuiltinTools(t *testing.T) {
	input := []byte(
		`{"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"my_custom_tool","input_schema":{"type":"object"}}]}`,
	)
	out := applyClaudeToolPrefix(input, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "web_search" {
		t.Fatalf("built-in tool name should not be prefixed: tools.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_my_custom_tool" {
		t.Fatalf("custom tool should be prefixed: tools.1.name = %q, want %q", got, "proxy_my_custom_tool")
	}
}

func TestApplyClaudeToolPrefix_BuiltinToolSkipped(t *testing.T) {
	body := []byte(`{
		"tools": [
			{"type": "web_search_20250305", "name": "web_search", "max_uses": 5},
			{"name": "Read"}
		],
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_use", "name": "web_search", "id": "ws1", "input": {}},
				{"type": "tool_use", "name": "Read", "id": "r1", "input": {}}
			]}
		]
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "web_search" {
		t.Fatalf("tools.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "web_search" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_Read" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_Read")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.name").String(); got != "proxy_Read" {
		t.Fatalf("messages.0.content.1.name = %q, want %q", got, "proxy_Read")
	}
}

func TestApplyClaudeToolPrefix_KnownBuiltinInHistoryOnly(t *testing.T) {
	body := []byte(`{
		"tools": [
			{"name": "Read"}
		],
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_use", "name": "web_search", "id": "ws1", "input": {}}
			]}
		]
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "web_search" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "web_search")
	}
	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_Read" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_Read")
	}
}

func TestApplyClaudeToolPrefix_CustomToolsPrefixed(t *testing.T) {
	body := []byte(`{
		"tools": [{"name": "Read"}, {"name": "Write"}],
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_use", "name": "Read", "id": "r1", "input": {}},
				{"type": "tool_use", "name": "Write", "id": "w1", "input": {}}
			]}
		]
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "tools.0.name").String(); got != "proxy_Read" {
		t.Fatalf("tools.0.name = %q, want %q", got, "proxy_Read")
	}
	if got := gjson.GetBytes(out, "tools.1.name").String(); got != "proxy_Write" {
		t.Fatalf("tools.1.name = %q, want %q", got, "proxy_Write")
	}
	if got := gjson.GetBytes(out, "messages.0.content.0.name").String(); got != "proxy_Read" {
		t.Fatalf("messages.0.content.0.name = %q, want %q", got, "proxy_Read")
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.name").String(); got != "proxy_Write" {
		t.Fatalf("messages.0.content.1.name = %q, want %q", got, "proxy_Write")
	}
}

func TestApplyClaudeToolPrefix_ToolChoiceBuiltin(t *testing.T) {
	body := []byte(`{
		"tools": [
			{"type": "web_search_20250305", "name": "web_search"},
			{"name": "Read"}
		],
		"tool_choice": {"type": "tool", "name": "web_search"}
	}`)
	out := applyClaudeToolPrefix(body, "proxy_")

	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "web_search" {
		t.Fatalf("tool_choice.name = %q, want %q", got, "web_search")
	}
}

func TestStripClaudeToolPrefixFromResponse(t *testing.T) {
	input := []byte(
		`{"content":[{"type":"tool_use","name":"proxy_alpha","id":"t1","input":{}},{"type":"tool_use","name":"bravo","id":"t2","input":{}}]}`,
	)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")

	if got := gjson.GetBytes(out, "content.0.name").String(); got != "alpha" {
		t.Fatalf("content.0.name = %q, want %q", got, "alpha")
	}
	if got := gjson.GetBytes(out, "content.1.name").String(); got != "bravo" {
		t.Fatalf("content.1.name = %q, want %q", got, "bravo")
	}
}

func TestStripClaudeToolPrefixFromResponse_WithToolReference(t *testing.T) {
	input := []byte(
		`{"content":[{"type":"tool_reference","tool_name":"proxy_alpha"},{"type":"tool_reference","tool_name":"bravo"}]}`,
	)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")

	if got := gjson.GetBytes(out, "content.0.tool_name").String(); got != "alpha" {
		t.Fatalf("content.0.tool_name = %q, want %q", got, "alpha")
	}
	if got := gjson.GetBytes(out, "content.1.tool_name").String(); got != "bravo" {
		t.Fatalf("content.1.tool_name = %q, want %q", got, "bravo")
	}
}

func TestStripClaudeToolPrefixFromStreamLine(t *testing.T) {
	line := []byte(
		`data: {"type":"content_block_start","content_block":{"type":"tool_use","name":"proxy_alpha","id":"t1"},"index":0}`,
	)
	out := stripClaudeToolPrefixFromStreamLine(line, "proxy_")

	payload := bytes.TrimSpace(out)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if got := gjson.GetBytes(payload, "content_block.name").String(); got != "alpha" {
		t.Fatalf("content_block.name = %q, want %q", got, "alpha")
	}
}

func TestStripClaudeToolPrefixFromStreamLine_WithToolReference(t *testing.T) {
	line := []byte(
		`data: {"type":"content_block_start","content_block":{"type":"tool_reference","tool_name":"proxy_beta"},"index":0}`,
	)
	out := stripClaudeToolPrefixFromStreamLine(line, "proxy_")

	payload := bytes.TrimSpace(out)
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if got := gjson.GetBytes(payload, "content_block.tool_name").String(); got != "beta" {
		t.Fatalf("content_block.tool_name = %q, want %q", got, "beta")
	}
}

func TestApplyClaudeToolPrefix_NestedToolReference(t *testing.T) {
	input := []byte(
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":[{"type":"tool_reference","tool_name":"mcp__nia__manage_resource"}]}]}]}`,
	)
	out := applyClaudeToolPrefix(input, "proxy_")
	got := gjson.GetBytes(out, "messages.0.content.0.content.0.tool_name").String()
	if got != "proxy_mcp__nia__manage_resource" {
		t.Fatalf("nested tool_reference tool_name = %q, want %q", got, "proxy_mcp__nia__manage_resource")
	}
}

func TestNormalizeClaudeSamplingForUpstreamRemovesTemperature(t *testing.T) {
	payload := []byte(`{"temperature":0,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"}}`)
	out := normalizeClaudeSamplingForUpstream(payload, false)

	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
}

func TestNormalizeClaudeSamplingForUpstreamRemovesTemperatureWithThinkingEnabled(t *testing.T) {
	payload := []byte(`{"temperature":0.2,"thinking":{"type":"enabled","budget_tokens":2048}}`)
	out := normalizeClaudeSamplingForUpstream(payload, false)

	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
}

func TestNormalizeClaudeSamplingForUpstreamPreservesNativeHelperTemperature(t *testing.T) {
	payload := []byte(`{"temperature":1,"thinking":{"type":"adaptive"}}`)
	out := normalizeClaudeSamplingForUpstream(payload, true)

	if got := gjson.GetBytes(out, "temperature"); !got.Exists() || got.Num != 1 {
		t.Fatalf("temperature = %s, want 1", got.Raw)
	}
}

func TestNormalizeClaudeSamplingForUpstreamRemovesTopPAndTopKForThinking(t *testing.T) {
	payload := []byte(`{"temperature":0.2,"top_p":0.9,"top_k":40,"thinking":{"type":"adaptive"}}`)
	out := normalizeClaudeSamplingForUpstream(payload, false)

	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
	if gjson.GetBytes(out, "top_p").Exists() {
		t.Fatalf("top_p should be removed when thinking is active")
	}
	if gjson.GetBytes(out, "top_k").Exists() {
		t.Fatalf("top_k should be removed when thinking is active")
	}
}

func TestNormalizeClaudeSamplingForUpstreamNoThinkingRemovesTemperatureAndTopP(t *testing.T) {
	payload := []byte(`{"temperature":0,"top_p":0.9,"top_k":40,"messages":[{"role":"user","content":"hi"}]}`)
	out := normalizeClaudeSamplingForUpstream(payload, false)

	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
	if gjson.GetBytes(out, "top_p").Exists() {
		t.Fatalf("top_p should be removed")
	}
	if got := gjson.GetBytes(out, "top_k").Int(); got != 40 {
		t.Fatalf("top_k = %v, want 40", got)
	}
}

func TestNormalizeClaudeSamplingForUpstreamAfterForcedToolChoiceRemovesTemperature(t *testing.T) {
	payload := []byte(`{"temperature":0,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"tool_choice":{"type":"any"}}`)
	out := disableThinkingIfToolChoiceForced(payload)
	out = normalizeClaudeSamplingForUpstream(out, false)

	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("thinking should be removed when tool_choice forces tool use")
	}
	if gjson.GetBytes(out, "temperature").Exists() {
		t.Fatalf("temperature should be removed")
	}
}

func TestCapMaxTokensAddsDefaultForRegisteredClaudeModel(t *testing.T) {
	clientID := "test-claude-default-max-tokens-client"
	modelID := "test-claude-default-max-tokens-model"
	registry.GetGlobalRegistry().RegisterClient(
		clientID, "claude", []*registry.ModelInfo{{ID: modelID, Type: "claude"}},
	)
	defer registry.GetGlobalRegistry().UnregisterClient(clientID)

	out := capMaxTokens([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), modelID)
	if got := gjson.GetBytes(out, "max_tokens").Int(); got != defaultClaudeMaxTokens {
		t.Fatalf("max_tokens = %d, want %d. Body: %s", got, defaultClaudeMaxTokens, string(out))
	}
}

func TestCapMaxTokensUsesRegisteredMaxCompletionTokens(t *testing.T) {
	clientID := "test-claude-registered-max-tokens-client"
	modelID := "test-claude-registered-max-tokens-model"
	registry.GetGlobalRegistry().RegisterClient(
		clientID, "claude", []*registry.ModelInfo{
			{
				ID:                  modelID,
				Type:                "claude",
				MaxCompletionTokens: 4096,
			},
		},
	)
	defer registry.GetGlobalRegistry().UnregisterClient(clientID)

	out := capMaxTokens([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), modelID)
	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 4096 {
		t.Fatalf("max_tokens = %d, want %d. Body: %s", got, 4096, string(out))
	}
}

func TestCapMaxTokensPreservesExplicitValue(t *testing.T) {
	clientID := "test-claude-preserve-max-tokens-client"
	modelID := "test-claude-preserve-max-tokens-model"
	registry.GetGlobalRegistry().RegisterClient(
		clientID, "claude", []*registry.ModelInfo{
			{
				ID:                  modelID,
				Type:                "claude",
				MaxCompletionTokens: 4096,
			},
		},
	)
	defer registry.GetGlobalRegistry().UnregisterClient(clientID)

	out := capMaxTokens([]byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":2048}`), modelID)
	if got := gjson.GetBytes(out, "max_tokens").Int(); got != 2048 {
		t.Fatalf("max_tokens = %d, want %d. Body: %s", got, 2048, string(out))
	}
}

func TestCapMaxTokensUsesDeepSeekV41Limit(t *testing.T) {
	const clientID = "test-deepseek-v41-max-tokens-client"
	model := registry.LookupStaticModelInfo("deepseek-flash")
	if model == nil {
		t.Fatal("missing deepseek-flash static metadata")
	}
	registry.GetGlobalRegistry().RegisterClient(clientID, "claude", []*registry.ModelInfo{model})
	defer registry.GetGlobalRegistry().UnregisterClient(clientID)

	for _, test := range []struct {
		name string
		body string
		want int64
	}{
		{name: "default", body: `{"messages":[{"role":"user","content":"hi"}]}`, want: 393216},
		{name: "clamped", body: `{"messages":[{"role":"user","content":"hi"}],"max_tokens":400000}`, want: 393216},
		{name: "preserved", body: `{"messages":[{"role":"user","content":"hi"}],"max_tokens":200000}`, want: 200000},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				out := capMaxTokens([]byte(test.body), "deepseek-flash")
				if got := gjson.GetBytes(out, "max_tokens").Int(); got != test.want {
					t.Fatalf("max_tokens = %d, want %d. Body: %s", got, test.want, out)
				}
			},
		)
	}
}

func TestCapMaxTokensSkipsUnregisteredModel(t *testing.T) {
	out := capMaxTokens(
		[]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "test-claude-unregistered-max-tokens-model",
	)
	if gjson.GetBytes(out, "max_tokens").Exists() {
		t.Fatalf("max_tokens should remain unset. Body: %s", string(out))
	}
}

func TestAppendClaudeFastModeBeta(t *testing.T) {
	for _, test := range []struct {
		name        string
		body        string
		betas       []string
		wantPresent bool
		wantLen     int
	}{
		{name: "omitted speed", body: `{}`, wantLen: 0},
		{name: "fast speed", body: `{"speed":"fast"}`, wantPresent: true, wantLen: 1},
		{name: "trimmed case-insensitive speed", body: `{"speed":" FAST "}`, wantPresent: true, wantLen: 1},
		{name: "other speed", body: `{"speed":"standard"}`, wantLen: 0},
		{name: "non-string speed", body: `{"speed":true}`, wantLen: 0},
		{
			name:        "existing beta",
			body:        `{"speed":"fast"}`,
			betas:       []string{"client-beta", claudeFastModeBeta},
			wantPresent: true,
			wantLen:     2,
		},
		{
			name:        "differently-cased beta does not suppress wire value",
			body:        `{"speed":"fast"}`,
			betas:       []string{" " + strings.ToUpper(claudeFastModeBeta) + " "},
			wantPresent: true,
			wantLen:     2,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				got := appendClaudeFastModeBeta([]byte(test.body), append([]string(nil), test.betas...))
				if len(got) != test.wantLen {
					t.Fatalf("betas = %v, want len %d", got, test.wantLen)
				}
				if present := containsTrimmedString(got, claudeFastModeBeta); present != test.wantPresent {
					t.Fatalf("fast beta present = %v, want %v; betas=%v", present, test.wantPresent, got)
				}
			},
		)
	}

	explicitBetas, body := extractAndRemoveBetas(
		[]byte(`{"speed":"fast","betas":["fast-mode-2026-02-01"]}`),
	)
	explicitBetas = appendClaudeFastModeBeta(body, explicitBetas)
	if len(explicitBetas) != 1 || explicitBetas[0] != claudeFastModeBeta || gjson.GetBytes(body, "betas").Exists() {
		t.Fatalf("explicit beta extraction = betas %v, body %s", explicitBetas, body)
	}
}

func TestPrepareClaudeRequestAddsFastModeBetaAcrossStreamAndCredentials(t *testing.T) {
	credentials := []struct {
		name string
		auth *cliproxyauth.Auth
	}{
		{
			name: "API key",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{
					"api_key":  "test-key",
					"base_url": "https://example.com",
				},
			},
		},
		{
			name: "OAuth",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{"base_url": "https://example.com"},
				Metadata: map[string]any{
					"access_token":      "sk-ant-oat-test",
					"account_uuid":      "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
					"claude_device_ids": []string{strings.Repeat("a", 64)},
				},
			},
		},
	}
	for _, credential := range credentials {
		for _, stream := range []bool{false, true} {
			name := credential.name + "/non-stream"
			if stream {
				name = credential.name + "/stream"
			}
			t.Run(
				name, func(t *testing.T) {
					payload := []byte(`{
					"model":"claude-opus-4-7",
					"speed":"fast",
					"messages":[{"role":"user","content":"hi"}],
					"max_tokens":16
				}`)
					request, errPrepare := NewClaudeExecutor(&config.Config{}).prepareClaudeRequest(
						context.Background(),
						credential.auth,
						cliproxyexecutor.Request{Model: "claude-opus-4-7", Payload: payload},
						cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
						stream,
					)
					if errPrepare != nil {
						t.Fatal(errPrepare)
					}
					betas := request.httpReq.Header.Get("Anthropic-Beta")
					if count := strings.Count(strings.ToLower(betas), claudeFastModeBeta); count != 1 {
						t.Fatalf("Anthropic-Beta = %q, fast beta count %d", betas, count)
					}
					body, errRead := io.ReadAll(request.httpReq.Body)
					if errRead != nil {
						t.Fatal(errRead)
					}
					if got := gjson.GetBytes(body, "speed").String(); got != "fast" {
						t.Fatalf("upstream speed = %q; body=%s", got, body)
					}

					defaultPayload, _ := sjson.DeleteBytes(payload, "speed")
					defaultRequest, errPrepare := NewClaudeExecutor(&config.Config{}).prepareClaudeRequest(
						context.Background(),
						credential.auth,
						cliproxyexecutor.Request{Model: "claude-opus-4-7", Payload: defaultPayload},
						cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
						stream,
					)
					if errPrepare != nil {
						t.Fatal(errPrepare)
					}
					if betas := defaultRequest.httpReq.Header.Get("Anthropic-Beta"); strings.Contains(
						strings.ToLower(betas), claudeFastModeBeta,
					) {
						t.Fatalf("default Anthropic-Beta = %q", betas)
					}
				},
			)
		}
	}
}

func TestClaudeCountTokensUpstreamDoesNotInferFastModeBeta(t *testing.T) {
	var seenHeader string
	var seenBody []byte
	ctx := util.WithRoundTripper(
		context.Background(),
		protectedUtlsRoundTripFunc(
			func(req *http.Request) (*http.Response, error) {
				seenHeader = req.Header.Get("Anthropic-Beta")
				seenBody, _ = io.ReadAll(req.Body)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"input_tokens":7}`)),
					Request:    req,
				}, nil
			},
		),
	)
	payload := []byte(`{
		"model":"claude-opus-4-7",
		"speed":"fast",
		"messages":[{"role":"user","content":"hi"}]
	}`)
	response, errCount := NewClaudeExecutor(&config.Config{}).countTokensUpstream(
		ctx,
		&cliproxyauth.Auth{
			Attributes: map[string]string{
				"api_key":  "test-key",
				"base_url": "https://example.com",
			},
		},
		cliproxyexecutor.Request{Model: "claude-opus-4-7", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
	)
	if errCount != nil {
		t.Fatal(errCount)
	}
	if strings.Contains(strings.ToLower(seenHeader), claudeFastModeBeta) {
		t.Fatalf("count_tokens Anthropic-Beta = %q", seenHeader)
	}
	if got := gjson.GetBytes(seenBody, "speed").String(); got != "fast" {
		t.Fatalf("count_tokens speed = %q; body=%s", got, seenBody)
	}
	if got := gjson.GetBytes(response.Payload, "input_tokens").Int(); got != 7 {
		t.Fatalf("input_tokens = %d; payload=%s", got, response.Payload)
	}
}

func TestClaudeSonnet55CountTokensValidatesOutboundRequest(t *testing.T) {
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key"}}
	executor := NewClaudeExecutor(&config.Config{})
	calls := 0
	var sent []byte
	ctx := util.WithRoundTripper(context.Background(), protectedUtlsRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		var err error
		sent, err = io.ReadAll(req.Body)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"input_tokens":7}`)),
			Request:    req,
		}, nil
	}))
	valid := []byte(`{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"},"tool_choice":{"type":"auto"}}`)
	_, err := executor.CountTokens(ctx, auth, cliproxyexecutor.Request{Model: "claude-sonnet-5-5", Payload: valid},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil || calls != 1 || gjson.GetBytes(sent, "thinking.type").String() != "between_tools" {
		t.Fatalf("CountTokens() error = %v, calls = %d, outbound = %s", err, calls, sent)
	}
	bare := []byte(`{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hi"}]}`)
	_, err = executor.CountTokens(ctx, auth, cliproxyexecutor.Request{Model: "claude-sonnet-5-5(none)", Payload: bare},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	if err != nil || calls != 2 || gjson.GetBytes(sent, "thinking.type").String() != "between_tools" {
		t.Fatalf("CountTokens(model(none)) error = %v, calls = %d, outbound = %s", err, calls, sent)
	}
	for _, choice := range []string{`{"type":"any"}`, `{"type":"tool","name":"search"}`} {
		invalid := []byte(`{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hi"}],"tool_choice":` + choice + `}`)
		_, err := executor.CountTokens(ctx, auth, cliproxyexecutor.Request{Model: "claude-sonnet-5-5", Payload: invalid},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
		var scoped interface{ IsRequestScoped() bool }
		if err == nil || !errors.As(err, &scoped) || !scoped.IsRequestScoped() || statusErrCode(err) != http.StatusBadRequest || calls != 2 {
			t.Fatalf("choice=%s: CountTokens() error = %v, calls = %d, want request-scoped 400 before upstream", choice, err, calls)
		}
	}
	invalidBudget := []byte(`{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":2048}}`)
	_, err = executor.CountTokens(ctx, auth, cliproxyexecutor.Request{Model: "claude-sonnet-5-5", Payload: invalidBudget},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude})
	var scoped interface{ IsRequestScoped() bool }
	if err == nil || !errors.As(err, &scoped) || !scoped.IsRequestScoped() || statusErrCode(err) != http.StatusBadRequest || calls != 2 {
		t.Fatalf("manual budget CountTokens() error = %v, calls = %d, want request-scoped 400 before upstream", err, calls)
	}
}

func TestNormalizeClaudeSonnet55Request(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5-5","thinking":{"type":"disabled","display":"updates","budget_tokens":1024,"block_binding":{"prefix_mismatch_behavior":"drop_block"}},"output_config":{"effort":"low"}}`)
	got, err := normalizeClaudeSonnet55Request(body)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(got, "thinking.type").String() != "between_tools" ||
		gjson.GetBytes(got, "thinking.display").Exists() ||
		gjson.GetBytes(got, "thinking.budget_tokens").Exists() ||
		gjson.GetBytes(got, "thinking.block_binding").Exists() ||
		gjson.GetBytes(got, "output_config.effort").String() != "low" {
		t.Fatalf("normalized request = %s", got)
	}

	for _, request := range []string{
		`{"model":"claude-sonnet-5-5","tool_choice":{"type":"any"}}`,
		`{"model":"claude-sonnet-5-5","tool_choice":{"type":"tool","name":"search"}}`,
		`{"model":"claude-sonnet-5-5","thinking":{"type":"enabled","budget_tokens":1024}}`,
		`{"model":"claude-sonnet-5-5","thinking":{"type":"between_tools","display":"updates"}}`,
		`{"model":"claude-sonnet-5-5","thinking":{"type":"between_tools"},"output_config":{"effort":"max"}}`,
	} {
		_, err := normalizeClaudeSonnet55Request([]byte(request))
		var scoped interface{ IsRequestScoped() bool }
		if err == nil || !errors.As(err, &scoped) || !scoped.IsRequestScoped() || statusErrCode(err) != http.StatusBadRequest {
			t.Fatalf("request %s: error = %v, want request-scoped 400", request, err)
		}
	}

	legacy := []byte(`{"model":"claude-sonnet-5","thinking":{"type":"disabled"},"tool_choice":{"type":"any"}}`)
	got, err = normalizeClaudeSonnet55Request(legacy)
	if err != nil || !bytes.Equal(got, legacy) {
		t.Fatalf("Sonnet 5 request = %s, error %v; want unchanged", got, err)
	}
}

func TestPrepareClaudeSonnet55Request(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.com"}}
	for _, stream := range []bool{false, true} {
		payload := []byte(`{"model":"claude-sonnet-5-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`)
		request, err := executor.prepareClaudeRequest(context.Background(), auth,
			cliproxyexecutor.Request{Model: "claude-sonnet-5-5", Payload: payload},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, stream)
		if err != nil {
			t.Fatalf("stream=%v: prepareClaudeRequest() error = %v", stream, err)
		}
		body, errRead := io.ReadAll(request.httpReq.Body)
		if errRead != nil {
			t.Fatal(errRead)
		}
		if got := gjson.GetBytes(body, "thinking.type").String(); got != "between_tools" {
			t.Fatalf("stream=%v: thinking.type = %q, want between_tools; body=%s", stream, got, body)
		}
	}
}

func TestPrepareClaudeSonnet55ZeroEffortAcrossFormats(t *testing.T) {
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.com"}}
	for _, test := range []struct {
		name    string
		format  sdktranslator.Format
		model   string
		payload string
	}{
		{name: "suffix", format: sdktranslator.FormatClaude, model: "claude-sonnet-5-5(none)", payload: `{"model":"claude-sonnet-5-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "responses", format: sdktranslator.FormatOpenAIResponse, model: "claude-sonnet-5-5", payload: `{"model":"claude-sonnet-5-5","input":"hi","reasoning":{"effort":"none"}}`},
		{name: "chat", format: sdktranslator.FormatOpenAI, model: "claude-sonnet-5-5", payload: `{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`},
		{name: "interactions", format: sdktranslator.FormatInteractions, model: "claude-sonnet-5-5", payload: `{"model":"claude-sonnet-5-5","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"reasoning":{"effort":"none"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := NewClaudeExecutor(&config.Config{}).prepareClaudeRequest(context.Background(), auth,
				cliproxyexecutor.Request{Model: test.model, Payload: []byte(test.payload)},
				cliproxyexecutor.Options{SourceFormat: test.format}, false)
			if err != nil {
				t.Fatalf("prepareClaudeRequest() error = %v", err)
			}
			body, err := io.ReadAll(request.httpReq.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(body, "thinking.type").String(); got != "between_tools" {
				t.Fatalf("thinking.type = %q, want between_tools; body=%s", got, body)
			}
		})
	}
}

func TestPrepareClaudeSonnet55ForcedToolChoiceRejected(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.com"}}
	for _, choice := range []string{`{"type":"any"}`, `{"type":"tool","name":"search"}`} {
		payload := []byte(`{"model":"claude-sonnet-5-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"search","input_schema":{"type":"object"}}],"tool_choice":` + choice + `}`)
		for _, stream := range []bool{false, true} {
			_, err := executor.prepareClaudeRequest(context.Background(), auth,
				cliproxyexecutor.Request{Model: "claude-sonnet-5-5", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, stream)
			var scoped interface{ IsRequestScoped() bool }
			if err == nil || !errors.As(err, &scoped) || !scoped.IsRequestScoped() || statusErrCode(err) != http.StatusBadRequest {
				t.Fatalf("choice=%s stream=%v: error = %v, want request-scoped 400", choice, stream, err)
			}
		}
	}
}

func TestPrepareClaudeSonnet55ThinkingConstraints(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.com"}}
	for _, test := range []struct {
		name   string
		fields string
	}{
		{name: "manual budget", fields: `,"thinking":{"type":"enabled","budget_tokens":2048}`},
		{name: "between tools display", fields: `,"thinking":{"type":"between_tools","display":"updates"}`},
		{name: "between tools budget", fields: `,"thinking":{"type":"between_tools","budget_tokens":2048}`},
		{name: "between tools block binding", fields: `,"thinking":{"type":"between_tools","block_binding":{"prefix_mismatch_behavior":"drop_block"}}`},
		{name: "between tools max effort", fields: `,"thinking":{"type":"between_tools"},"output_config":{"effort":"max"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"model":"claude-sonnet-5-5","max_tokens":8192,"messages":[{"role":"user","content":"hi"}]` + test.fields + `}`)
			request, err := executor.prepareClaudeRequest(context.Background(), auth,
				cliproxyexecutor.Request{Model: "claude-sonnet-5-5", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, false)
			var scoped interface{ IsRequestScoped() bool }
			if err == nil || !errors.As(err, &scoped) || !scoped.IsRequestScoped() || statusErrCode(err) != http.StatusBadRequest {
				var body []byte
				if request != nil {
					body, _ = io.ReadAll(request.httpReq.Body)
				}
				t.Fatalf("error = %v, outbound = %s; want request-scoped 400", err, body)
			}
		})
	}
	valid := []byte(`{"model":"claude-sonnet-5-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"between_tools"},"output_config":{"effort":"medium"},"tool_choice":{"type":"none"}}`)
	request, err := executor.prepareClaudeRequest(context.Background(), auth,
		cliproxyexecutor.Request{Model: "claude-sonnet-5-5", Payload: valid},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, false)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(request.httpReq.Body)
	if err != nil || gjson.GetBytes(body, "thinking.type").String() != "between_tools" ||
		gjson.GetBytes(body, "output_config.effort").String() != "medium" ||
		gjson.GetBytes(body, "tool_choice.type").String() != "none" {
		t.Fatalf("valid between_tools request = %s, error = %v", body, err)
	}
}

func TestPrepareClaudeSonnet55CloakProgressDisplay(t *testing.T) {
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key": "test-key", "base_url": "https://api.anthropic.com", "fingerprint_profile": "claude-code-cli",
	}}
	for _, test := range []struct {
		name          string
		model         string
		thinking      string
		modelOverride string
		wantDisplay   string
	}{
		{name: "Sonnet 5.5 adaptive", model: "claude-sonnet-5-5", thinking: `"thinking":{"type":"adaptive"}`, wantDisplay: "updates"},
		{name: "Sonnet 5 adaptive", model: "claude-sonnet-5", thinking: `"thinking":{"type":"adaptive"}`, wantDisplay: "updates"},
		{name: "Sonnet 5.5 zero upfront", model: "claude-sonnet-5-5", thinking: `"thinking":{"type":"disabled"}`},
		{name: "explicit display", model: "claude-sonnet-5-5", thinking: `"thinking":{"type":"adaptive","display":"omitted"}`, wantDisplay: "omitted"},
		{name: "model changed to nonprogress", model: "claude-sonnet-5-5", thinking: `"thinking":{"type":"adaptive"}`, modelOverride: "claude-opus-5"},
		{name: "model changed into Sonnet", model: "claude-opus-5", thinking: `"thinking":{"type":"adaptive"}`, modelOverride: "claude-sonnet-5-5", wantDisplay: "updates"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &config.Config{}
			if test.modelOverride != "" {
				cfg.Payload.Override = []config.PayloadRule{{
					Models: []config.PayloadModelRule{{Name: "*"}},
					Params: map[string]any{"model": test.modelOverride},
				}}
			}
			payload := []byte(`{"model":"` + test.model + `","max_tokens":64,"messages":[{"role":"user","content":"hi"}],` + test.thinking + `}`)
			request, err := NewClaudeExecutor(cfg).prepareClaudeRequest(context.Background(), auth,
				cliproxyexecutor.Request{Model: test.model, Payload: payload},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, false)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(request.httpReq.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(body, "thinking.display").String(); got != test.wantDisplay {
				t.Fatalf("thinking.display = %q, want %q; body=%s", got, test.wantDisplay, body)
			}
			if test.name == "Sonnet 5.5 zero upfront" && gjson.GetBytes(body, "thinking.type").String() != "between_tools" {
				t.Fatalf("zero-upfront thinking = %s, want between_tools", gjson.GetBytes(body, "thinking").Raw)
			}
		})
	}
}

func TestReconcileClaudeCloakDisplayAfterPayload(t *testing.T) {
	for _, model := range []string{"claude-opus-5-5", "claude-sonnet-5", "claude-sonnet-5-5", "claude-fable-5-1"} {
		t.Run(model, func(t *testing.T) {
			for _, test := range []struct {
				name     string
				thinking string
				injected bool
				touched  bool
				want     string
			}{
				{name: "disabled removes synthetic display", thinking: `{"type":"disabled","display":"updates"}`, injected: true},
				{name: "missing type removes synthetic display", thinking: `{"display":"updates"}`, injected: true},
				{name: "adaptive keeps synthetic display", thinking: `{"type":"adaptive","display":"updates"}`, injected: true, want: "updates"},
				{name: "enabled keeps synthetic display", thinking: `{"type":"enabled","budget_tokens":2048,"display":"updates"}`, injected: true, want: "updates"},
				{name: "caller display stays caller-owned", thinking: `{"type":"disabled","display":"summarized"}`, want: "summarized"},
				{name: "payload display stays operator-owned", thinking: `{"type":"disabled","display":"omitted"}`, injected: true, touched: true, want: "omitted"},
				{name: "payload deletion is not refilled", thinking: `{"type":"adaptive"}`, injected: true, touched: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					body := []byte(`{"model":"` + model + `","thinking":` + test.thinking + `}`)
					out := reconcileClaudeCodeFableModelAfterPayload(body,
						claudeCodeFableState{injectedDisplay: test.injected}, map[string]bool{"thinking.display": test.touched}, true, false)
					display := gjson.GetBytes(out, "thinking.display")
					if display.String() != test.want || (test.want == "" && display.Exists()) {
						t.Fatalf("display=%s, want %q; body=%s", display.Raw, test.want, out)
					}
				})
			}
		})
	}
	body := []byte(`{"model":"claude-opus-4-8","thinking":{"type":"adaptive","display":"updates"}}`)
	out := reconcileClaudeCodeFableModelAfterPayload(body, claudeCodeFableState{injectedDisplay: true}, nil, true, false)
	if display := gjson.GetBytes(out, "thinking.display"); display.Exists() {
		t.Fatalf("synthetic display survived for non-progress model: %s", out)
	}
}

func TestPrepareClaudeSonnet55PayloadModelOverrideUsesFinalModelRules(t *testing.T) {
	cfg := &config.Config{}
	cfg.Payload.Override = []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "*"}},
		Params: map[string]any{"model": "claude-sonnet-5"},
	}}
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.com"}}
	for _, test := range []struct {
		name         string
		choice       string
		wantThinking string
	}{
		{name: "no forced tool", wantThinking: "disabled"},
		{name: "forced tool", choice: `,"tool_choice":{"type":"any"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := []byte(`{"model":"claude-sonnet-5-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}` + test.choice + `}`)
			request, err := NewClaudeExecutor(cfg).prepareClaudeRequest(context.Background(), auth,
				cliproxyexecutor.Request{Model: "claude-sonnet-5-5", Payload: payload},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, false)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(request.httpReq.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(body, "model").String(); got != "claude-sonnet-5" {
				t.Fatalf("model = %q, want claude-sonnet-5; body=%s", got, body)
			}
			if got := gjson.GetBytes(body, "thinking.type").String(); got != test.wantThinking {
				t.Fatalf("thinking.type = %q, want %q; body=%s", got, test.wantThinking, body)
			}
		})
	}
}

func TestPrepareClaudeRequest_AddsContext1MBetaFromModelTag(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	ctx := context.Background()
	req := cliproxyexecutor.Request{
		Model:   "claude-opus-4-7[1m]",
		Payload: []byte(`{"model":"claude-opus-4-7[1m]","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`),
	}
	cr, err := executor.prepareClaudeRequest(
		ctx,
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.com"}},
		req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")}, false,
	)
	if err != nil {
		t.Fatalf("prepareClaudeRequest error: %v", err)
	}
	got := cr.httpReq.Header.Get("Anthropic-Beta")
	if !strings.Contains(got, "context-1m-2025-08-07") {
		t.Fatalf("Anthropic-Beta = %q, want context-1m-2025-08-07", got)
	}
}

func TestPrepareClaudeRequestSanitizesIncompatibleSignatures(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	payload := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{
			"role":"assistant",
			"content":[
				{"type":"thinking","thinking":"bad","signature":"skip_thought_signature_validator"},
				{"type":"tool_use","id":"toolu_1","name":"Read","input":{},"signature":"skip_thought_signature_validator","extra_content":{"google":{"thought_signature":"skip_thought_signature_validator"}},"model":"gemini-3-pro"},
				{"type":"text","text":"visible"}
			]
		},{"role":"user","content":"continue"}],
		"max_tokens":16
	}`)

	cr, err := executor.prepareClaudeRequest(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.com"}},
		cliproxyexecutor.Request{Model: "claude-opus-4-7", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
		false,
	)
	if err != nil {
		t.Fatalf("prepareClaudeRequest error: %v", err)
	}
	body, err := io.ReadAll(cr.httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}

	if got := gjson.GetBytes(body, "messages.0.content.0.type").String(); got != "tool_use" {
		t.Fatalf("first retained content type = %q, want tool_use. Body: %s", got, string(body))
	}
	for _, path := range []string{
		"messages.0.content.0.signature",
		"messages.0.content.0.extra_content.google.thought_signature",
		"messages.0.content.0.extra_content",
		"messages.0.content.0.model",
	} {
		if gjson.GetBytes(body, path).Exists() {
			t.Fatalf("%s should be removed. Body: %s", path, string(body))
		}
	}
	if got := gjson.GetBytes(body, "messages.0.content.1.text").String(); got != "visible" {
		t.Fatalf("text content = %q, want visible. Body: %s", got, string(body))
	}
}

func TestPrepareClaudeRequestBypassesSignatureSanitizerForNonClaudeModel(t *testing.T) {
	executor := NewClaudeExecutor(&config.Config{})
	payload := []byte(`{
		"model":"gpt-5.4",
		"messages":[{
			"role":"assistant",
			"content":[
				{"type":"thinking","thinking":"keep","signature":"skip_thought_signature_validator"},
				{"type":"tool_use","id":"toolu_1","name":"Read","input":{},"signature":"skip_thought_signature_validator","extra_content":{"google":{"thought_signature":"skip_thought_signature_validator"}},"model":"gemini-3-pro"},
				{"type":"text","text":"visible"}
			]
		}],
		"max_tokens":16
	}`)

	cr, err := executor.prepareClaudeRequest(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test-key", "base_url": "https://example.com"}},
		cliproxyexecutor.Request{Model: "gpt-5.4", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
		false,
	)
	if err != nil {
		t.Fatalf("prepareClaudeRequest error: %v", err)
	}
	body, err := io.ReadAll(cr.httpReq.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}

	if got := gjson.GetBytes(body, "messages.0.content.0.type").String(); got != "thinking" {
		t.Fatalf("first content type = %q, want thinking. Body: %s", got, string(body))
	}
	for _, path := range []string{
		"messages.0.content.0.signature",
		"messages.0.content.1.signature",
		"messages.0.content.1.extra_content.google.thought_signature",
		"messages.0.content.1.model",
	} {
		if !gjson.GetBytes(body, path).Exists() {
			t.Fatalf("%s should be preserved for non-Claude target. Body: %s", path, string(body))
		}
	}
}

func TestClaudeExecutor_ReusesUserIDAcrossModelsWhenCacheEnabled(t *testing.T) {
	resetUserIDCache()

	var userIDs []string
	var requestModels []string
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				userID := gjson.GetBytes(body, "metadata.user_id").String()
				model := gjson.GetBytes(body, "model").String()
				userIDs = append(userIDs, userID)
				requestModels = append(requestModels, model)
				t.Logf("HTTP Server received request: model=%s, user_id=%s, url=%s", model, userID, r.URL.String())
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(
					[]byte(
						`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`,
					),
				)
			},
		),
	)
	defer server.Close()

	t.Logf("End-to-end test: Fake HTTP server started at %s", server.URL)

	executor := NewClaudeExecutor(
		&config.Config{
			ClaudeKey: []config.ClaudeKey{
				{
					APIKey:  "key-123",
					BaseURL: server.URL,
					Cloak: &config.CloakConfig{
						CacheUserID: new(true),
					},
				},
			},
		},
	)
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "key-123",
			"base_url": server.URL,
		},
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	models := []string{"claude-3-5-sonnet", "claude-3-5-haiku"}
	for _, model := range models {
		t.Logf("Sending request for model: %s", model)
		modelPayload, _ := sjson.SetBytes(payload, "model", model)
		if _, err := executor.Execute(
			context.Background(), auth, cliproxyexecutor.Request{
				Model:   model,
				Payload: modelPayload,
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("claude"),
			},
		); err != nil {
			t.Fatalf("Execute(%s) error: %v", model, err)
		}
	}

	if len(userIDs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(userIDs))
	}
	if userIDs[0] == "" || userIDs[1] == "" {
		t.Fatal("expected user_id to be populated")
	}
	t.Logf("user_id[0] (model=%s): %s", requestModels[0], userIDs[0])
	t.Logf("user_id[1] (model=%s): %s", requestModels[1], userIDs[1])
	if userIDs[0] != userIDs[1] {
		t.Fatalf("expected user_id to be reused across models, got %q and %q", userIDs[0], userIDs[1])
	}
	if !isValidUserID(userIDs[0]) {
		t.Fatalf("user_id %q is not valid", userIDs[0])
	}
	t.Logf("✓ End-to-end test passed: Same user_id (%s) was used for both models", userIDs[0])
}

func TestClaudeExecutor_DefaultPreservesAbsentUserID(t *testing.T) {
	resetUserIDCache()

	var userIDs []string
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				userIDs = append(userIDs, gjson.GetBytes(body, "metadata.user_id").String())
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(
					[]byte(
						`{"id":"msg_1","type":"message","model":"claude-3-5-sonnet","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`,
					),
				)
			},
		),
	)
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "key-123",
			"base_url": server.URL,
		},
	}

	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	for i := range 2 {
		if _, err := executor.Execute(
			context.Background(), auth, cliproxyexecutor.Request{
				Model:   "claude-3-5-sonnet",
				Payload: payload,
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("claude"),
			},
		); err != nil {
			t.Fatalf("Execute call %d error: %v", i, err)
		}
	}

	if len(userIDs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(userIDs))
	}
	if userIDs[0] != "" || userIDs[1] != "" {
		t.Fatalf("default profile injected user IDs: %v", userIDs)
	}
}

func TestStripClaudeToolPrefixFromResponse_NestedToolReference(t *testing.T) {
	input := []byte(
		`{"content":[{"type":"tool_result","tool_use_id":"toolu_123","content":[{"type":"tool_reference","tool_name":"proxy_mcp__nia__manage_resource"}]}]}`,
	)
	out := stripClaudeToolPrefixFromResponse(input, "proxy_")
	got := gjson.GetBytes(out, "content.0.content.0.tool_name").String()
	if got != "mcp__nia__manage_resource" {
		t.Fatalf("nested tool_reference tool_name = %q, want %q", got, "mcp__nia__manage_resource")
	}
}

func TestApplyClaudeToolPrefix_NestedToolReferenceWithStringContent(t *testing.T) {
	// tool_result.content can be a string - should not be processed
	input := []byte(
		`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"plain string result"}]}]}`,
	)
	out := applyClaudeToolPrefix(input, "proxy_")
	got := gjson.GetBytes(out, "messages.0.content.0.content").String()
	if got != "plain string result" {
		t.Fatalf("string content should remain unchanged = %q", got)
	}
}

func TestApplyClaudeToolPrefix_SkipsBuiltinToolReference(t *testing.T) {
	input := []byte(
		`{"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"tool_reference","tool_name":"web_search"}]}]}]}`,
	)
	out := applyClaudeToolPrefix(input, "proxy_")
	got := gjson.GetBytes(out, "messages.0.content.0.content.0.tool_name").String()
	if got != "web_search" {
		t.Fatalf("built-in tool_reference should not be prefixed, got %q", got)
	}
}

func TestNormalizeCacheControlTTL_DowngradesLaterOneHourBlocks(t *testing.T) {
	payload := []byte(`{
		"tools": [{"name":"t1","cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"system": [{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}],
		"messages": [{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]
	}`)

	out := normalizeCacheControlTTL(payload)

	if got := gjson.GetBytes(out, "tools.0.cache_control.ttl").String(); got != "1h" {
		t.Fatalf("tools.0.cache_control.ttl = %q, want %q", got, "1h")
	}
	if gjson.GetBytes(out, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Fatalf("messages.0.content.0.cache_control.ttl should be removed after a default-5m block")
	}
}

func TestNormalizeCacheControlTTL_PreservesKeyOrderWhenModified(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral","ttl":"1h"}}]}],"tools":[{"name":"t1","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}]}`)

	out := normalizeCacheControlTTL(payload)

	if gjson.GetBytes(out, "messages.0.content.0.cache_control.ttl").Exists() {
		t.Fatalf("messages.0.content.0.cache_control.ttl should be removed after a default-5m block")
	}

	outStr := string(out)
	idxModel := strings.Index(outStr, `"model"`)
	idxMessages := strings.Index(outStr, `"messages"`)
	idxTools := strings.Index(outStr, `"tools"`)
	idxSystem := strings.Index(outStr, `"system"`)
	if idxModel == -1 || idxMessages == -1 || idxTools == -1 || idxSystem == -1 {
		t.Fatalf("failed to locate top-level keys in output: %s", outStr)
	}
	if !(idxModel < idxMessages && idxMessages < idxTools && idxTools < idxSystem) {
		t.Fatalf("top-level key order changed:\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestEnforceCacheControlLimit_StripsNonLastToolBeforeMessages(t *testing.T) {
	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}}
		],
		"system": [{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}],
		"messages": [
			{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":[{"type":"text","text":"u2","cache_control":{"type":"ephemeral"}}]}
		]
	}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed first (non-last tool)")
	}
	if !gjson.GetBytes(out, "tools.1.cache_control").Exists() {
		t.Fatalf("tools.1.cache_control (last tool) should be preserved")
	}
	if !gjson.GetBytes(out, "messages.0.content.0.cache_control").Exists() || !gjson.GetBytes(
		out, "messages.1.content.0.cache_control",
	).Exists() {
		t.Fatalf("message cache_control blocks should be preserved when non-last tool removal is enough")
	}
}

func TestEnforceCacheControlLimit_PreservesKeyOrderWhenModified(t *testing.T) {
	payload := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"u1","cache_control":{"type":"ephemeral"}},{"type":"text","text":"u2","cache_control":{"type":"ephemeral"}}]}],"tools":[{"name":"t1","cache_control":{"type":"ephemeral"}},{"name":"t2","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"s1","cache_control":{"type":"ephemeral"}}]}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed first (non-last tool)")
	}

	outStr := string(out)
	idxModel := strings.Index(outStr, `"model"`)
	idxMessages := strings.Index(outStr, `"messages"`)
	idxTools := strings.Index(outStr, `"tools"`)
	idxSystem := strings.Index(outStr, `"system"`)
	if idxModel == -1 || idxMessages == -1 || idxTools == -1 || idxSystem == -1 {
		t.Fatalf("failed to locate top-level keys in output: %s", outStr)
	}
	if !(idxModel < idxMessages && idxMessages < idxTools && idxTools < idxSystem) {
		t.Fatalf("top-level key order changed:\noriginal: %s\ngot:      %s", payload, out)
	}
}

func TestEnforceCacheControlLimit_ToolOnlyPayloadStillRespectsLimit(t *testing.T) {
	payload := []byte(`{
		"tools": [
			{"name":"t1","cache_control":{"type":"ephemeral"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}},
			{"name":"t3","cache_control":{"type":"ephemeral"}},
			{"name":"t4","cache_control":{"type":"ephemeral"}},
			{"name":"t5","cache_control":{"type":"ephemeral"}}
		]
	}`)

	out := enforceCacheControlLimit(payload, 4)

	if got := countCacheControls(out); got != 4 {
		t.Fatalf("cache_control count = %d, want 4", got)
	}
	if gjson.GetBytes(out, "tools.0.cache_control").Exists() {
		t.Fatalf("tools.0.cache_control should be removed to satisfy max=4")
	}
	if !gjson.GetBytes(out, "tools.4.cache_control").Exists() {
		t.Fatalf("last tool cache_control should be preserved when possible")
	}
}

func TestClaudeExecutor_CountTokensCountsLocallyWithoutUpstreamRequest(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected upstream count_tokens request: %s", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			},
		),
	)
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	payload := []byte(`{
		"system":"client system instructions",
		"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]
	}`)
	for _, auth := range []*cliproxyauth.Auth{
		nil,
		{Attributes: map[string]string{"api_key": "key-123", "base_url": server.URL}},
		{Attributes: map[string]string{"api_key": "sk-ant-oat-test", "base_url": server.URL}},
	} {
		resp, err := executor.CountTokens(
			context.Background(), auth, cliproxyexecutor.Request{
				Model:   "claude-3-5-haiku-20241022",
				Payload: payload,
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
		)
		if err != nil {
			t.Fatalf("CountTokens error: %v", err)
		}
		if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got <= 0 {
			t.Fatalf("input_tokens = %d, want positive local count; payload = %s", got, resp.Payload)
		}
	}
}

func TestClaudeExecutor_CountTokensRejectsInvalidRequests(t *testing.T) {
	tests := []string{
		`not-json`,
		`[]`,
		`{}`,
		`{"messages":[]}`,
		`{"messages":[{"role":"system","content":"hello"}]}`,
		`{"messages":[{"role":"user","content":42}]}`,
		`{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`,
	}
	executor := NewClaudeExecutor(&config.Config{})
	for _, payload := range tests {
		_, err := executor.CountTokens(
			context.Background(), nil, cliproxyexecutor.Request{
				Model:   "claude-3-5-haiku-20241022",
				Payload: []byte(payload),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
		)
		if err == nil {
			t.Fatalf("CountTokens(%q) error = nil, want validation error", payload)
		}
		if statusProvider, ok := err.(interface{ StatusCode() int }); !ok || statusProvider.StatusCode() != http.StatusBadRequest {
			t.Fatalf("CountTokens(%q) error = %v, want status 400", payload, err)
		}
	}
}

func TestClaudeExecutor_Execute_InvalidGzipErrorBodyReturnsDecodeMessage(t *testing.T) {
	testClaudeExecutorInvalidCompressedErrorBody(
		t, func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error {
			_, err := executor.Execute(
				context.Background(), auth, cliproxyexecutor.Request{
					Model:   "claude-3-5-sonnet-20241022",
					Payload: payload,
				}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
			)
			return err
		},
	)
}

func TestClaudeExecutor_ExecuteStream_InvalidGzipErrorBodyReturnsDecodeMessage(t *testing.T) {
	testClaudeExecutorInvalidCompressedErrorBody(
		t, func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error {
			_, err := executor.ExecuteStream(
				context.Background(), auth, cliproxyexecutor.Request{
					Model:   "claude-3-5-sonnet-20241022",
					Payload: payload,
				}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
			)
			return err
		},
	)
}

func TestClaudeExecutorResponsesPatchCompletion(t *testing.T) {
	request := []byte(`{"model":"claude-3-5-sonnet-20241022","tools":[{"type":"custom","name":"apply_patch"}],"input":[{"role":"user","content":"apply the patch"}]}`)
	prefix := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_patch\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":1}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_patch\",\"name\":\"apply_patch\",\"input\":{}}}\n\n"
	suffix := "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\n"
	for _, tc := range []struct {
		name, arguments, snapshot, want, wantErr string
		terminate                                bool
	}{
		{name: "source terminator missing", arguments: `{"input":"ok"}`, wantErr: "missing message_stop"},
		{name: "invalid tool input", arguments: `{"input":7}`, terminate: true, wantErr: "invalid apply_patch"},
		{name: "conflicting partial snapshot", arguments: `{"input":"wrong`, snapshot: `{"input":"prefix"}`, terminate: true, wantErr: "conflict"},
		{name: "valid partial snapshot", arguments: `{"input":"pre`, snapshot: `{"input":"prefix"}`, terminate: true, want: "prefix"},
		{name: "valid stream with blank separators", arguments: `{"input":"ok"}`, terminate: true, want: "ok"},
	} {
		for _, mode := range []string{"buffered", "stream"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				fragment, err := sjson.SetBytes([]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta"}}`), "delta.partial_json", tc.arguments)
				if err != nil {
					t.Fatal(err)
				}
				body := prefix + "data: " + string(fragment) + "\n\n"
				if tc.snapshot != "" {
					body += "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_patch\",\"name\":\"apply_patch\",\"input\":" + tc.snapshot + "}}\n\n"
				}
				body += suffix
				if tc.terminate {
					body += "data: {\"type\":\"message_stop\"}\n\n"
				}
				ctx := util.WithRoundTripper(t.Context(), protectedUtlsRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       io.NopCloser(strings.NewReader(body)),
						Request:    req,
					}, nil
				}))
				executor := NewClaudeExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "key-test", "base_url": "https://example.test"}}
				req := cliproxyexecutor.Request{Model: "claude-3-5-sonnet-20241022", Payload: request}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: request}
				var output []byte
				if mode == "buffered" {
					var response cliproxyexecutor.Response
					response, err = executor.Execute(ctx, auth, req, opts)
					output = response.Payload
				} else {
					var stream *cliproxyexecutor.StreamResult
					stream, err = executor.ExecuteStream(ctx, auth, req, opts)
					if err == nil {
						for chunk := range stream.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
							output = append(output, chunk.Payload...)
						}
					}
				}
				if tc.wantErr != "" {
					var status interface{ StatusCode() int }
					if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !errors.As(err, &status) || status.StatusCode() != http.StatusBadGateway {
						t.Fatalf("completion error = %v, output = %s", err, output)
					}
				} else if err != nil || !bytes.Contains(output, []byte(`"input":"`+tc.want+`"`)) || bytes.Contains(output, []byte(`"status":"failed"`)) {
					t.Fatalf("successful patch completion = %s, error = %v", output, err)
				}
			})
		}
	}
}

func TestClaudeExecutorExecuteValidatesTranslatedStream(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "upstream error event",
			body:    "data: {\"type\":\"error\",\"error\":{\"message\":\"rate limited\"}}\n\n",
			wantErr: "upstream returned error event: rate limited",
		},
		{
			name: "malformed event",
			body: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-test\"}}\n\n" +
				"data: {malformed\n\n",
			wantErr: "upstream returned malformed stream data",
		},
		{
			name: "missing message stop",
			body: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-test\"}}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n",
			wantErr: "missing message_stop",
		},
		{
			name: "valid stream",
			body: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
				"data: {\"type\":\"message_stop\"}\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, _ *http.Request) {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, tt.body)
						},
					),
				)
				defer server.Close()

				executor := NewClaudeExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{
					Attributes: map[string]string{
						"api_key":  "key-123",
						"base_url": server.URL,
					},
				}
				_, err := executor.Execute(
					context.Background(),
					auth,
					cliproxyexecutor.Request{
						Model:   "claude-3-5-sonnet-20241022",
						Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
					},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")},
				)
				if tt.wantErr == "" {
					if err != nil {
						t.Fatalf("Execute error = %v", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Execute error = %v, want substring %q", err, tt.wantErr)
				}
			},
		)
	}
}

type repeatedByteReader byte

func (r repeatedByteReader) Read(p []byte) (int, error) {
	for index := range p {
		p[index] = byte(r)
	}
	return len(p), nil
}

func TestClaudeExecutorExecuteRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.CopyN(w, repeatedByteReader('x'), maxClaudeResponseBytes+1)
			},
		),
	)
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "key-123",
			"base_url": server.URL,
		},
	}
	_, err := executor.Execute(
		context.Background(),
		auth,
		cliproxyexecutor.Request{
			Model:   "claude-3-5-sonnet-20241022",
			Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
	)
	if err == nil || !strings.Contains(err.Error(), "upstream response exceeds 50 MiB limit") {
		t.Fatalf("Execute error = %v, want response size error", err)
	}
}

func TestClaudeExecutorExecuteStreamPreservesDirectAndTranslatedPaths(t *testing.T) {
	const streamBody = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	tests := []struct {
		name           string
		sourceFormat   sdktranslator.Format
		wantTranslated bool
	}{
		{name: "direct Claude stream", sourceFormat: sdktranslator.FormatClaude},
		{name: "translated OpenAI stream", sourceFormat: sdktranslator.FormatOpenAI, wantTranslated: true},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
				registerTestUsagePlugin(t, plugin)

				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, _ *http.Request) {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, streamBody)
						},
					),
				)
				defer server.Close()

				executor := NewClaudeExecutor(&config.Config{})
				auth := &cliproxyauth.Auth{
					Attributes: map[string]string{
						"api_key":  "key-123",
						"base_url": server.URL,
					},
				}
				ctx := usage.WithRequestedModelAlias(context.Background(), tt.name)
				stream, err := executor.ExecuteStream(
					ctx,
					auth,
					cliproxyexecutor.Request{
						Model:   "claude-3-5-sonnet-20241022",
						Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
					},
					cliproxyexecutor.Options{SourceFormat: tt.sourceFormat},
				)
				if err != nil {
					t.Fatalf("ExecuteStream error = %v", err)
				}

				var payload []byte
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk error = %v", chunk.Err)
					}
					payload = append(payload, chunk.Payload...)
				}
				if tt.wantTranslated {
					if !bytes.Contains(payload, []byte(`"object":"chat.completion.chunk"`)) ||
						!bytes.Contains(payload, []byte("hello")) {
						t.Fatalf("translated payload = %q", payload)
					}
				} else if string(payload) != streamBody {
					t.Fatalf("direct payload changed:\n%s", payload)
				}

				select {
				case record := <-plugin.records:
					if record.Alias != tt.name || record.Failed || !record.Completed {
						t.Fatalf("usage record outcome = %#v", record)
					}
					if record.Detail.InputTokens != 2 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 5 {
						t.Fatalf("usage detail = %#v, want input/output/total 2/3/5", record.Detail)
					}
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for usage record")
				}
			},
		)
	}
}

func TestClaudeExecutorExecuteStreamReportsReadFailureForBothOutputPaths(t *testing.T) {
	const streamBody = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":2}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n"

	for _, sourceFormat := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAI} {
		t.Run(
			string(sourceFormat), func(t *testing.T) {
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
				registerTestUsagePlugin(t, plugin)

				ctx := util.WithRoundTripper(
					usage.WithRequestedModelAlias(context.Background(), "read-failure-"+string(sourceFormat)),
					protectedUtlsRoundTripFunc(
						func(req *http.Request) (*http.Response, error) {
							return &http.Response{
								StatusCode: http.StatusOK,
								Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
								Body: io.NopCloser(
									&terminalErrorReader{
										data: []byte(streamBody),
										err:  io.ErrUnexpectedEOF,
									},
								),
								Request: req,
							}, nil
						},
					),
				)
				executor := NewClaudeExecutor(&config.Config{})
				stream, err := executor.ExecuteStream(
					ctx,
					&cliproxyauth.Auth{
						Attributes: map[string]string{
							"api_key":  "key-123",
							"base_url": "http://localhost",
						},
					},
					cliproxyexecutor.Request{
						Model:   "claude-3-5-sonnet-20241022",
						Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
					},
					cliproxyexecutor.Options{SourceFormat: sourceFormat},
				)
				if err != nil {
					t.Fatalf("ExecuteStream error = %v", err)
				}

				var streamErr error
				for chunk := range stream.Chunks {
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
				}
				if !errors.Is(streamErr, io.ErrUnexpectedEOF) {
					t.Fatalf("stream error = %v, want unexpected EOF", streamErr)
				}

				select {
				case record := <-plugin.records:
					if !record.Failed || record.Completed || record.Fail.Body != io.ErrUnexpectedEOF.Error() {
						t.Fatalf("usage record outcome = %#v", record)
					}
					if record.Detail.InputTokens != 2 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 5 {
						t.Fatalf("usage detail = %#v, want input/output/total 2/3/5", record.Detail)
					}
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for failed usage record")
				}
			},
		)
	}
}

func TestClaudeExecutorExecuteStreamCompletesBeforeUpstreamReadError(t *testing.T) {
	const streamBody = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":2}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	for _, tc := range []struct {
		name   string
		format sdktranslator.Format
		marker string
	}{
		{name: "Claude", format: sdktranslator.FormatClaude, marker: `"type":"message_stop"`},
		{name: "Chat", format: sdktranslator.FormatOpenAI, marker: `"finish_reason":"stop"`},
		{name: "Responses", format: sdktranslator.FormatOpenAIResponse, marker: "response.completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
			registerTestUsagePlugin(t, plugin)
			ctx := util.WithRoundTripper(context.Background(), protectedUtlsRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body: io.NopCloser(&terminalErrorReader{
							data: []byte(streamBody),
							err:  io.ErrUnexpectedEOF,
						}),
						Request: req,
					}, nil
				},
			))
			result, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, &cliproxyauth.Auth{
				Attributes: map[string]string{"api_key": "key-123", "base_url": "http://localhost"},
			}, cliproxyexecutor.Request{
				Model:   "claude-test",
				Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FormatClaude, ResponseFormat: tc.format,
			})
			if err != nil {
				t.Fatalf("ExecuteStream: %v", err)
			}
			var payload []byte
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream chunk error after terminal: %v", chunk.Err)
				}
				payload = append(payload, chunk.Payload...)
			}
			if !bytes.Contains(payload, []byte(tc.marker)) {
				t.Fatalf("missing terminal %q in %s", tc.marker, payload)
			}
			select {
			case record := <-plugin.records:
				if !record.Completed || record.Failed || record.Detail.InputTokens != 2 || record.Detail.OutputTokens != 3 {
					t.Fatalf("usage record = %#v", record)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for terminal usage")
			}
		})
	}
}

func TestClaudeExecutorExecuteStreamPublishesUsageAfterTerminalCancellation(t *testing.T) {
	const streamBody = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":2}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	for _, format := range []sdktranslator.Format{
		sdktranslator.FormatClaude, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse,
	} {
		t.Run(string(format), func(t *testing.T) {
			plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
			registerTestUsagePlugin(t, plugin)
			ctx, cancel := context.WithCancel(util.WithRoundTripper(context.Background(), protectedUtlsRoundTripFunc(
				func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       io.NopCloser(strings.NewReader(streamBody)),
						Request:    req,
					}, nil
				},
			)))
			defer cancel()
			observer := &recordingClaudeStreamObserver{onFinish: func(outcome claudeStreamOutcome) {
				if outcome == claudeStreamOutcomeComplete {
					cancel()
				}
			}}
			result, err := NewClaudeExecutor(&config.Config{}).executeStreamWithObserver(
				ctx,
				&cliproxyauth.Auth{Attributes: map[string]string{
					"api_key": "key-123", "base_url": "http://localhost",
				}},
				cliproxyexecutor.Request{
					Model: "claude-test", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
				},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: format},
				observer,
			)
			if err != nil {
				t.Fatalf("executeStreamWithObserver: %v", err)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("terminal stream error: %v", chunk.Err)
				}
			}
			if len(observer.outcomes) != 1 || observer.outcomes[0] != claudeStreamOutcomeComplete || ctx.Err() != context.Canceled {
				t.Fatalf("stream outcome = %v, context = %v", observer.outcomes, ctx.Err())
			}
			select {
			case record := <-plugin.records:
				if !record.Completed || record.Failed || record.Detail.InputTokens != 2 || record.Detail.OutputTokens != 3 {
					t.Fatalf("usage after terminal cancellation = %#v", record)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for post-terminal usage")
			}
		})
	}
}

func TestClaudeExecutorExecuteStreamClientCancelsAfterTerminalFrame(t *testing.T) {
	const streamBody = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":2}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	for _, tc := range []struct {
		name   string
		format sdktranslator.Format
		marker string
	}{
		{name: "Claude", format: sdktranslator.FormatClaude, marker: `"type":"message_stop"`},
		{name: "Responses", format: sdktranslator.FormatOpenAIResponse, marker: "response.completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
			registerTestUsagePlugin(t, plugin)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, streamBody)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer func() {
				close(release)
				server.Close()
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := NewClaudeExecutor(&config.Config{}).ExecuteStream(ctx, &cliproxyauth.Auth{
				Attributes: map[string]string{"api_key": "key-123", "base_url": server.URL},
			}, cliproxyexecutor.Request{
				Model: "claude-test", Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: tc.format})
			if err != nil {
				t.Fatalf("ExecuteStream: %v", err)
			}
			var payload []byte
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream before terminal: %v", chunk.Err)
				}
				payload = append(payload, chunk.Payload...)
				if bytes.Contains(payload, []byte(tc.marker)) {
					cancel()
					break
				}
			}
			if ctx.Err() != context.Canceled || !bytes.Contains(payload, []byte(tc.marker)) {
				t.Fatalf("terminal frame not delivered before context ended: %s (%v)", payload, ctx.Err())
			}
			select {
			case record := <-plugin.records:
				if !record.Completed || record.Failed || record.Detail.InputTokens != 2 || record.Detail.OutputTokens != 3 {
					t.Fatalf("usage after client cancellation = %#v", record)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for successful usage after terminal frame")
			}
		})
	}
}

func TestClaudeStreamingValidator(t *testing.T) {
	tests := []struct {
		name    string
		lines   []string
		wantErr string
	}{
		{
			name:    "empty stream",
			wantErr: "empty stream response",
		},
		{
			name:    "error event",
			lines:   []string{`data: {"type":"error","error":{"message":"rate limited"}}`},
			wantErr: "upstream returned error event: rate limited",
		},
		{
			name:    "missing message start",
			lines:   []string{`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`},
			wantErr: "missing message_start",
		},
		{
			name:    "incomplete stream",
			lines:   []string{`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-test"}}`},
			wantErr: "ended before message completion",
		},
		{
			name: "missing message stop",
			lines: []string{
				`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-test"}}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			},
			wantErr: "missing message_stop",
		},
		{
			name: "valid empty completion",
			lines: []string{
				`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-test"}}`,
				`data: {"type":"message_stop"}`,
			},
		},
		{
			name: "valid stream",
			lines: []string{
				`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-test"}}`,
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`data: {"type":"message_stop"}`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				validator := &claudeStreamingValidator{}
				for _, line := range tt.lines {
					if err := validator.observe([]byte(line)); err != nil {
						if tt.wantErr == "" || !strings.Contains(err.Error(), tt.wantErr) {
							t.Fatalf("observe() error = %v, want substring %q", err, tt.wantErr)
						}
						return
					}
				}
				err := validator.finish()
				if tt.wantErr == "" {
					if err != nil {
						t.Fatalf("finish() error = %v", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("finish() error = %v, want substring %q", err, tt.wantErr)
				}
			},
		)
	}
}

func testClaudeExecutorInvalidCompressedErrorBody(
	t *testing.T,
	invoke func(executor *ClaudeExecutor, auth *cliproxyauth.Auth, payload []byte) error,
) {
	t.Helper()

	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", "gzip")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("not-a-valid-gzip-stream"))
			},
		),
	)
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{
			"api_key":  "key-123",
			"base_url": server.URL,
		},
	}
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	err := invoke(executor, auth, payload)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to decode error response body") {
		t.Fatalf("expected decode failure message, got: %v", err)
	}
	if statusProvider, ok := err.(interface{ StatusCode() int }); !ok ||
		statusProvider.StatusCode() != http.StatusBadRequest {
		t.Fatalf("expected status code 400, got: %v", err)
	}
}

func TestClaudeExecutorExecuteTransportMatchesResponseFormat(t *testing.T) {
	const model = "claude-3-5-sonnet-20241022"
	streamResponse := strings.Join(
		[]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_123","model":"claude-3-5-sonnet-20241022"}}`,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":1}}`,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}, "\n",
	)
	jsonResponse := `{"id":"msg_123","type":"message","role":"assistant","model":"claude-3-5-sonnet-20241022","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":1}}`

	tests := []struct {
		name           string
		sourceFormat   sdktranslator.Format
		responseFormat sdktranslator.Format
		wantStream     bool
	}{
		{
			name: "OpenAI to OpenAI uses SSE", sourceFormat: sdktranslator.FormatOpenAI,
			responseFormat: sdktranslator.FormatOpenAI, wantStream: true,
		},
		{
			name: "OpenAI to Claude uses JSON", sourceFormat: sdktranslator.FormatOpenAI,
			responseFormat: sdktranslator.FormatClaude, wantStream: false,
		},
		{
			name: "Claude to OpenAI uses SSE", sourceFormat: sdktranslator.FormatClaude,
			responseFormat: sdktranslator.FormatOpenAI, wantStream: true,
		},
		{
			name: "Claude to Claude uses JSON", sourceFormat: sdktranslator.FormatClaude,
			responseFormat: sdktranslator.FormatClaude, wantStream: false,
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				var seenBody []byte
				var seenHeaders http.Header
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							seenBody, _ = io.ReadAll(r.Body)
							seenHeaders = r.Header.Clone()
							if tt.wantStream {
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = w.Write([]byte(streamResponse))
								return
							}
							w.Header().Set("Content-Type", "application/json")
							_, _ = w.Write([]byte(jsonResponse))
						},
					),
				)
				defer server.Close()

				executor := NewClaudeExecutor(
					&config.Config{
						Payload: config.PayloadConfig{
							Override: []config.PayloadRule{
								{
									Models: []config.PayloadModelRule{{Name: model, Protocol: "claude"}},
									Params: map[string]any{"stream": !tt.wantStream},
								},
							},
						},
					},
				)
				attributes := map[string]string{
					"api_key":  "key-123",
					"base_url": server.URL,
				}
				if tt.wantStream {
					attributes["header:Accept"] = "application/json"
					attributes["header:Accept-Encoding"] = "gzip, deflate, br, zstd"
				}
				auth := &cliproxyauth.Auth{Attributes: attributes}
				payload := []byte(`{"model":"claude-3-5-sonnet-20241022","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

				response, err := executor.Execute(
					context.Background(), auth, cliproxyexecutor.Request{
						Model:   model,
						Payload: payload,
					}, cliproxyexecutor.Options{
						SourceFormat:   tt.sourceFormat,
						ResponseFormat: tt.responseFormat,
						Headers: http.Header{
							"Anthropic-Beta": []string{"client-beta"},
						},
					},
				)
				if err != nil {
					t.Fatalf("Execute error: %v", err)
				}
				stream := gjson.GetBytes(seenBody, "stream")
				if !stream.Exists() || stream.Bool() != tt.wantStream {
					t.Fatalf("upstream stream = %s, want %t; body=%s", stream.Raw, tt.wantStream, string(seenBody))
				}
				wantAccept := "application/json"
				wantEncoding := "gzip, deflate, br, zstd"
				if tt.wantStream {
					wantAccept = "text/event-stream"
					wantEncoding = "identity"
				}
				if got := seenHeaders.Get("Accept"); got != wantAccept {
					t.Fatalf("Accept = %q, want %q", got, wantAccept)
				}
				if got := seenHeaders.Get("Accept-Encoding"); got != wantEncoding {
					t.Fatalf("Accept-Encoding = %q, want %q", got, wantEncoding)
				}
				if got := seenHeaders.Get("Anthropic-Beta"); !strings.Contains(got, "client-beta") {
					t.Fatalf("Anthropic-Beta = %q, want client beta preserved", got)
				}
				if tt.responseFormat == sdktranslator.FormatClaude {
					if got := gjson.GetBytes(response.Payload, "type").String(); got != "message" {
						t.Fatalf("response type = %q, want Claude message; payload=%s", got, response.Payload)
					}
				} else if !gjson.GetBytes(response.Payload, "choices").IsArray() {
					t.Fatalf("response is not OpenAI chat completion: %s", response.Payload)
				}
			},
		)
	}
}

func TestClaudeCountTokensPreservesThinkingNormalizerIntent(t *testing.T) {
	const source = `{"reasoning":{"effort":"medium"},"input":[{"type":"configuration_update","reasoning":{"effort":"low"}},{"role":"user","content":"hi"}]}`
	for _, effort := range []string{"", "high"} {
		t.Run("edit="+effort, func(t *testing.T) {
			hooks := &claudeThinkingNormalizerHooks{effort: effort}
			calls := 0
			var sent []byte
			ctx := sdktranslator.WithPluginHooks(t.Context(), hooks)
			ctx = util.WithRoundTripper(ctx, protectedUtlsRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				var err error
				sent, err = io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"input_tokens":7}`)),
					Request:    req,
				}, nil
			}))
			request := cliproxyexecutor.Request{
				Model: "claude-sonnet-5-5", Payload: []byte(source),
				Metadata: map[string]any{"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{
					ID: "claude-sonnet-5-5", Type: "claude",
					Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}},
				}},
			}
			before := bytes.Clone(request.Payload)
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "synthetic-count-test-key"}}
			_, err := NewClaudeExecutor(&config.Config{}).CountTokens(ctx, auth, request,
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
			if err != nil {
				t.Fatal(err)
			}
			if !hooks.sawClaude || calls != 1 || hooks.requestCalls.Load() != 1 || !bytes.Equal(request.Payload, before) || len(request.Metadata) != 1 {
				t.Fatal("Claude counting did not normalize the target schema exactly once or changed the caller request")
			}
			if got := gjson.GetBytes(sent, "output_config.effort").String(); got != effort {
				t.Fatalf("counted Claude effort = %s, want %s from target request normalizer", got, effort)
			}
		})
	}
}

func TestPrepareClaudeRequestPromptCacheOptionsPreserveFinalCCH(t *testing.T) {
	for _, test := range []struct {
		mode   string
		stream bool
	}{{mode: "explicit"}, {mode: "automatic"}, {mode: "explicit", stream: true}, {mode: "automatic", stream: true}} {
		mode := test.mode
		name := mode + "/messages"
		if test.stream {
			name = mode + "/stream"
		}
		t.Run(name, func(t *testing.T) {
			payload := []byte(`{"model":"claude-opus-4-7","system":"custom system","messages":[{"role":"user","content":"hello"}],"max_tokens":16,"prompt_cache_options":{"mode":"` + mode + `"}}`)
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "claude", Metadata: map[string]any{"access_token": "sk-ant-oat-test", "account_uuid": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "claude_device_ids": []string{strings.Repeat("a", 64)}}, Attributes: map[string]string{"base_url": "https://example.com", "disable_tool_name_prefix": "true"}}
			prepared, err := NewClaudeExecutor(&config.Config{}).prepareClaudeRequest(context.Background(), auth, cliproxyexecutor.Request{Model: "claude-opus-4-7", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}, test.stream)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(prepared.httpReq.Body)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(body, "prompt_cache_options").Exists() {
				t.Fatal("internal prompt cache options reached upstream")
			}
			if !strings.Contains(gjson.GetBytes(body, "system.0.text").String(), "cch=") {
				t.Fatal("OAuth request missing CCH")
			}
			signed, err := signAnthropicMessagesBody(body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(signed, body) {
				t.Fatal("CCH does not match the final outgoing body after prompt cache option removal")
			}
		})
	}
}

func TestClaudeEmptyUpstreamFailsForEachResponseFormat(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatClaude, sdktranslator.FormatOpenAIResponse} {
		t.Run(format.String(), func(t *testing.T) {
			calls := 0
			ctx := util.WithRoundTripper(context.Background(), geminiInteractionsRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Host != "synthetic.invalid" {
					return nil, errors.New("unexpected upstream host")
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: request, Body: io.NopCloser(strings.NewReader(""))}, nil
			}))
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "claude", Attributes: map[string]string{"api_key": "synthetic-key", "base_url": "https://synthetic.invalid"}}
			response, err := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true}).Execute(ctx, auth, cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: format})
			if calls != 1 {
				t.Fatalf("upstream calls=%d, want 1", calls)
			}
			if err == nil {
				t.Fatalf("empty upstream reported successful %s response: %q", format, response.Payload)
			}
			var status interface{ StatusCode() int }
			if !errors.As(err, &status) || status.StatusCode() != http.StatusBadGateway {
				t.Fatalf("empty upstream error=%v, want 502", err)
			}
		})
	}
}
