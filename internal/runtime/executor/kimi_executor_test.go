package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	kimiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/kimi"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type kimiTokenRoundTripper func(*http.Request) (*http.Response, error)

func (f kimiTokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNewKimiExecutorInitializesDelegatedClaudeConfig(t *testing.T) {
	cfg := &config.Config{}
	executor := NewKimiExecutor(cfg)

	if executor.cfg != cfg {
		t.Fatal("Kimi executor config was not initialized")
	}
	if executor.ClaudeExecutor.cfg != cfg {
		t.Fatal("delegated Claude executor config was not initialized")
	}
	if got := executor.ClaudeExecutor.upstreamRequestLogProvider(); got != "kimi" {
		t.Fatalf("delegated request log provider = %q, want kimi", got)
	}
}

func TestKimiExecutorPreservesAssistantContentAndToolCallsFromResponsesHistory(t *testing.T) {
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
							`{"id":"chatcmpl_test","object":"chat.completion","created":1,"model":"k3","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
						),
					),
				}, nil
			},
		),
	)

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:         "kimi-responses-history",
		Provider:   "kimi",
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}
	payload := []byte(`{
		"model":"kimi-k3",
		"input":[
			{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"inspect the next step"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Step 3 completed; continue to step 4."}]},
			{"type":"function_call","call_id":"call_4","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"},
			{"type":"function_call_output","call_id":"call_4","output":"ok"}
		]
	}`)

	_, err := executor.Execute(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: payload,
		}, cliproxyexecutor.Options{
			SourceFormat:    sdktranslator.FormatOpenAIResponse,
			OriginalRequest: payload,
		},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	messages := gjson.GetBytes(upstreamBody, "messages").Array()
	if got := len(messages); got != 2 {
		t.Fatalf("upstream messages count = %d, want 2; body=%s", got, upstreamBody)
	}
	assistant := messages[0]
	if got := assistant.Get("content.0.text").String(); got != "Step 3 completed; continue to step 4." {
		t.Fatalf("assistant content = %q, want preserved text; body=%s", got, upstreamBody)
	}
	if got := assistant.Get("reasoning_content").String(); got != "inspect the next step" {
		t.Fatalf("assistant reasoning_content = %q, want inspect the next step; body=%s", got, upstreamBody)
	}
	if got := assistant.Get("tool_calls.0.id").String(); got != "call_4" {
		t.Fatalf("assistant tool call ID = %q, want call_4; body=%s", got, upstreamBody)
	}
	if got := messages[1].Get("tool_call_id").String(); got != "call_4" {
		t.Fatalf("tool output call ID = %q, want call_4; body=%s", got, upstreamBody)
	}
}

func TestKimiExecutorClaudeStreamForwardsHeadersAndLogsKimiProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", nil)
	ginCtx.Request.Header.Set("Anthropic-Beta", "gin-beta-must-not-win")

	var upstreamRequest *http.Request
	ctx := util.WithGinContext(context.Background(), ginCtx)
	ctx = util.WithRoundTripper(
		ctx, kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				upstreamRequest = req.Clone(req.Context())
				upstreamRequest.Header = req.Header.Clone()
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(
						strings.NewReader(
							"event: message_start\n" +
								`data: {"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"k3","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
								"event: message_delta\n" +
								`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}` + "\n\n" +
								"event: message_stop\n" +
								`data: {"type":"message_stop"}` + "\n\n",
						),
					),
				}, nil
			},
		),
	)

	cfg := &config.Config{}
	cfg.RequestLog = true
	executor := NewKimiExecutor(cfg)
	auth := &cliproxyauth.Auth{
		ID:         "kimi-test-auth",
		Provider:   "kimi",
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}
	payload := []byte(`{"model":"kimi-k3","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
	result, err := executor.ExecuteStream(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: payload,
		}, cliproxyexecutor.Options{
			Stream:          true,
			SourceFormat:    sdktranslator.FormatClaude,
			OriginalRequest: payload,
			Headers: http.Header{
				"Anthropic-Beta": []string{"client-beta-one", "client-beta-two"},
			},
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}
	if upstreamRequest == nil {
		t.Fatal("upstream request was not captured")
	}
	wantURL := strings.TrimSuffix(kimiauth.APIBaseURL, "/") + "/v1/messages?beta=true"
	if got := upstreamRequest.URL.String(); got != wantURL {
		t.Fatalf("upstream URL = %q, want %q", got, wantURL)
	}
	upstreamBetas := upstreamRequest.Header.Get("Anthropic-Beta")
	for _, beta := range []string{"client-beta-one", "client-beta-two", "oauth-2025-04-20"} {
		if !strings.Contains(upstreamBetas, beta) {
			t.Fatalf("Anthropic-Beta = %q, want %q", upstreamBetas, beta)
		}
	}
	if strings.Contains(upstreamBetas, "gin-beta-must-not-win") {
		t.Fatalf("Anthropic-Beta = %q, explicit options headers must take precedence", upstreamBetas)
	}

	requestValue, ok := ginCtx.Get(apiRequestKey)
	if !ok {
		t.Fatal("missing API_REQUEST")
	}
	requestLog, ok := requestValue.(*logging.BoundedLogBuffer)
	if !ok {
		t.Fatalf("API_REQUEST type = %T, want *logging.BoundedLogBuffer", requestValue)
	}
	requestText := requestLog.String()
	for _, want := range []string{
		"Upstream URL: " + wantURL,
		"Auth: provider=kimi",
		"Anthropic-Beta: " + upstreamBetas,
		`"model":"k3"`,
	} {
		if !strings.Contains(requestText, want) {
			t.Fatalf("API_REQUEST = %q, want %q", requestText, want)
		}
	}

	responseValue, ok := ginCtx.Get(apiResponseKey)
	if !ok {
		t.Fatal("missing API_RESPONSE")
	}
	responseLog, ok := responseValue.(*logging.BoundedLogBuffer)
	if !ok {
		t.Fatalf("API_RESPONSE type = %T, want *logging.BoundedLogBuffer", responseValue)
	}
	responseText := responseLog.String()
	for _, want := range []string{"Status: 200", `data: {"type":"message_stop"}`} {
		if !strings.Contains(responseText, want) {
			t.Fatalf("API_RESPONSE = %q, want %q", responseText, want)
		}
	}
}

func TestApplyKimiHeadersUsesBuildVersion(t *testing.T) {
	req, errRequest := http.NewRequest(http.MethodPost, "https://example.test", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest() error = %v", errRequest)
	}
	applyKimiHeaders(req, "token", true)

	if got := req.Header.Get("User-Agent"); got != "CLIProxyAPI/"+buildinfo.Version {
		t.Fatalf("User-Agent = %q, want %q", got, "CLIProxyAPI/"+buildinfo.Version)
	}
	if got := req.Header.Get("X-Msh-Platform"); got != "CLIProxyAPI" {
		t.Fatalf("X-Msh-Platform = %q, want CLIProxyAPI", got)
	}
	if got := req.Header.Get("X-Msh-Version"); got != buildinfo.Version {
		t.Fatalf("X-Msh-Version = %q, want %q", got, buildinfo.Version)
	}
	if got := req.Header.Get("Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", got)
	}
}

func TestNormalizeKimiUpstreamModel(t *testing.T) {
	tests := map[string]string{
		"kimi-k3":                         "k3",
		"kimi-k3[1m]":                     "k3",
		"Kimi-K3[1M]":                     "k3",
		"k3[1m]":                          "k3",
		"kimi-k3(1024)":                   "k3(1024)",
		"kimi-k3[1m](1024)":               "k3(1024)",
		"kimi-k2.7-code":                  "kimi-for-coding",
		"kimi-k2.7-code-highspeed":        "kimi-for-coding-highspeed",
		"kimi-for-coding-highspeed(high)": "kimi-for-coding-highspeed(high)",
		"kimi-k2.6[1m](high)":             "k2.6(high)",
	}

	for input, want := range tests {
		if got := normalizeKimiUpstreamModel(input); got != want {
			t.Errorf("normalizeKimiUpstreamModel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestKimiExecutorCountTokensUsesKimiUpstream(t *testing.T) {
	called := false
	ctx := util.WithRoundTripper(
		context.Background(), kimiTokenRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				called = true
				wantURL := strings.TrimSuffix(kimiauth.APIBaseURL, "/") + "/v1/messages/count_tokens"
				if got := req.URL.Scheme + "://" + req.URL.Host + req.URL.Path; got != wantURL {
					t.Fatalf("count_tokens URL = %q, want %q", got, wantURL)
				}
				body, errRead := io.ReadAll(req.Body)
				if errRead != nil {
					t.Fatalf("read count_tokens body: %v", errRead)
				}
				if got := gjson.GetBytes(body, "model").String(); got != "k3" {
					t.Fatalf("count_tokens model = %q, want k3; body = %s", got, body)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"input_tokens":7}`)),
				}, nil
			},
		),
	)
	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider:   "kimi",
		Attributes: map[string]string{},
		Metadata:   map[string]any{"access_token": "test-token"},
	}
	resp, errCount := executor.CountTokens(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "kimi-k3",
			Payload: []byte(`{"model":"ignored","messages":[{"role":"user","content":"hello"}]}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
	)
	if errCount != nil {
		t.Fatalf("CountTokens() error = %v", errCount)
	}
	if !called {
		t.Fatal("Kimi count_tokens upstream was not called")
	}
	if got := gjson.GetBytes(resp.Payload, "input_tokens").Int(); got != 7 {
		t.Fatalf("input_tokens = %d, want 7; payload = %s", got, resp.Payload)
	}
}

func TestNormalizeKimiToolMessageLinks_UsesCallIDFallback(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"list_directory:1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]},
			{"role":"tool","call_id":"list_directory:1","content":"[]"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	got := gjson.GetBytes(out, "messages.1.tool_call_id").String()
	if got != "list_directory:1" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "list_directory:1")
	}
}

func TestNormalizeKimiToolMessageLinks_InferSinglePendingID(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_123","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
			{"role":"tool","content":"file-content"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	got := gjson.GetBytes(out, "messages.1.tool_call_id").String()
	if got != "call_123" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "call_123")
	}
}

func TestNormalizeKimiToolMessageLinks_AmbiguousMissingIDIsNotInferred(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}},
				{"id":"call_2","type":"function","function":{"name":"read_file","arguments":"{}"}}
			]},
			{"role":"tool","content":"result-without-id"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	if gjson.GetBytes(out, "messages.1.tool_call_id").Exists() {
		t.Fatalf(
			"messages.1.tool_call_id should be absent for ambiguous case, got %q",
			gjson.GetBytes(out, "messages.1.tool_call_id").String(),
		)
	}
}

func TestNormalizeKimiToolMessageLinks_PreservesExistingToolCallID(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","call_id":"different-id","content":"result"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	got := gjson.GetBytes(out, "messages.1.tool_call_id").String()
	if got != "call_1" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "call_1")
	}
}

func TestNormalizeKimiToolMessageLinks_InheritsPreviousReasoningForAssistantToolCalls(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":"plan","reasoning_content":"previous reasoning"},
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	got := gjson.GetBytes(out, "messages.1.reasoning_content").String()
	if got != "previous reasoning" {
		t.Fatalf("messages.1.reasoning_content = %q, want %q", got, "previous reasoning")
	}
}

func TestNormalizeKimiToolMessageLinks_InsertsFallbackReasoningWhenMissing(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	reasoning := gjson.GetBytes(out, "messages.0.reasoning_content")
	if !reasoning.Exists() {
		t.Fatalf("messages.0.reasoning_content should exist")
	}
	if reasoning.String() != "[reasoning unavailable]" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", reasoning.String(), "[reasoning unavailable]")
	}
}

func TestNormalizeKimiToolMessageLinks_UsesContentAsReasoningFallback(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":[{"type":"text","text":"first line"},{"type":"text","text":"second line"}],"tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	got := gjson.GetBytes(out, "messages.0.reasoning_content").String()
	if got != "first line\nsecond line" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", got, "first line\nsecond line")
	}
}

func TestNormalizeKimiToolMessageLinks_ReplacesEmptyReasoningContent(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":"assistant summary","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}],"reasoning_content":""}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	got := gjson.GetBytes(out, "messages.0.reasoning_content").String()
	if got != "assistant summary" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", got, "assistant summary")
	}
}

func TestNormalizeKimiToolMessageLinks_PreservesExistingAssistantReasoning(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}],"reasoning_content":"keep me"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	got := gjson.GetBytes(out, "messages.0.reasoning_content").String()
	if got != "keep me" {
		t.Fatalf("messages.0.reasoning_content = %q, want %q", got, "keep me")
	}
}

func TestNormalizeKimiToolMessageLinks_RepairsIDsAndReasoningTogether(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}],"reasoning_content":"r1"},
			{"role":"tool","call_id":"call_1","content":"[]"},
			{"role":"assistant","tool_calls":[{"id":"call_2","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
			{"role":"tool","call_id":"call_2","content":"file"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	if got := gjson.GetBytes(out, "messages.1.tool_call_id").String(); got != "call_1" {
		t.Fatalf("messages.1.tool_call_id = %q, want %q", got, "call_1")
	}
	if got := gjson.GetBytes(out, "messages.3.tool_call_id").String(); got != "call_2" {
		t.Fatalf("messages.3.tool_call_id = %q, want %q", got, "call_2")
	}
	if got := gjson.GetBytes(out, "messages.2.reasoning_content").String(); got != "r1" {
		t.Fatalf("messages.2.reasoning_content = %q, want %q", got, "r1")
	}
}

func TestNormalizeKimiToolMessageLinks_DropsEmptyAssistantWithoutToolLink(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user","content":"start"},
			{"role":"assistant","content":""},
			{"role":"assistant","content":"   "},
			{"role":"assistant","content":"","tool_calls":null},
			{"role":"assistant","content":[{"type":"text","text":"  "}]},
			{"role":"assistant"},
			{"role":"assistant","content":"keep"},
			{"role":"user","content":"next"}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 3 {
		t.Fatalf("messages length = %d, want 3, raw = %s", len(messages), gjson.GetBytes(out, "messages").Raw)
	}
	if got := messages[0].Get("content").String(); got != "start" {
		t.Fatalf("messages.0.content = %q, want %q", got, "start")
	}
	if got := messages[1].Get("content").String(); got != "keep" {
		t.Fatalf("messages.1.content = %q, want %q", got, "keep")
	}
	if got := messages[2].Get("content").String(); got != "next" {
		t.Fatalf("messages.2.content = %q, want %q", got, "next")
	}
}

func TestNormalizeKimiToolMessageLinks_PreservesAssistantWithToolLinkOrReasoning(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]},
			{"role":"assistant","content":"","function_call":{"name":"legacy_call","arguments":"{}"}},
			{"role":"assistant","content":"","reasoning_content":"thought"},
			{"role":"assistant","content":[{"type":"text","text":" visible "}]}
		]
	}`)

	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}

	messages := gjson.GetBytes(out, "messages").Array()
	if len(messages) != 4 {
		t.Fatalf("messages length = %d, want 4, raw = %s", len(messages), gjson.GetBytes(out, "messages").Raw)
	}
	if !messages[0].Get("tool_calls").Exists() {
		t.Fatalf("messages.0.tool_calls should exist")
	}
	if !messages[1].Get("function_call").Exists() {
		t.Fatalf("messages.1.function_call should exist")
	}
	if got := messages[2].Get("reasoning_content").String(); got != "thought" {
		t.Fatalf("messages.2.reasoning_content = %q, want %q", got, "thought")
	}
	if got := messages[3].Get("content.0.text").String(); got != " visible " {
		t.Fatalf("messages.3.content.0.text = %q, want %q", got, " visible ")
	}
}
