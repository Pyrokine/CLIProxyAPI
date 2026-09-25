package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	kimiauth "github.com/Pyrokine/CLIProxyAPI/v6/internal/auth/kimi"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/buildinfo"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

type kimiTokenRoundTripper func(*http.Request) (*http.Response, error)

func (f kimiTokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type kimiContextErrorReader struct {
	ctx context.Context
}

func (r *kimiContextErrorReader) Read(_ []byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func waitForKimiUsageRecord(t *testing.T, records <-chan usage.Record, alias string) usage.Record {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case record := <-records:
			if record.Alias == alias {
				return record
			}
		case <-deadline:
			t.Fatalf("timed out waiting for Kimi usage record %q", alias)
		}
	}
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

func TestKimiExecutorRequestToFormatMatchesWireProtocol(t *testing.T) {
	executor := NewKimiExecutor(&config.Config{})
	tests := []struct {
		name   string
		source sdktranslator.Format
		count  bool
		want   sdktranslator.Format
	}{
		{name: "Claude", source: sdktranslator.FormatClaude, want: sdktranslator.FormatClaude},
		{name: "OpenAI", source: sdktranslator.FormatOpenAI, want: sdktranslator.FormatOpenAI},
		{name: "OpenAI Responses", source: sdktranslator.FormatOpenAIResponse, want: sdktranslator.FormatOpenAIResponse},
		{name: "count tokens", source: sdktranslator.FormatOpenAIResponse, count: true, want: sdktranslator.FormatClaude},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got := executor.RequestToFormat(
				cliproxyexecutor.Request{},
				cliproxyexecutor.Options{SourceFormat: testCase.source, CountTokens: testCase.count},
			)
			if got != testCase.want {
				t.Fatalf("RequestToFormat() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestKimiExecutorResponsesPassthroughHeadersAndUsage(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
	registerTestUsagePlugin(t, plugin)

	var upstreamURL string
	var upstreamBody []byte
	var upstreamHeaders http.Header
	ctx := usage.WithRequestedModelAlias(context.Background(), "kimi-responses-passthrough")
	ctx = util.WithSessionID(ctx, "kimi-session")
	ctx = util.WithRoundTripper(ctx, kimiTokenRoundTripper(func(req *http.Request) (*http.Response, error) {
		upstreamURL = req.URL.String()
		upstreamHeaders = req.Header.Clone()
		var errRead error
		upstreamBody, errRead = io.ReadAll(req.Body)
		if errRead != nil {
			return nil, errRead
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"resp_123","object":"response","status":"completed","model":"k3","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}],"usage":{"total_tokens":10,"input_tokens":6,"output_tokens":4}}`,
			)),
		}, nil
	}))

	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		Provider: "kimi",
		Attributes: map[string]string{
			"header:X-Copied":       "$x-client-value",
			"header:X-CPA-Session":  "$CPA-SESSION-ID",
			"header:Authorization":  "must-not-replace",
			"header:Content-Length": "999",
		},
		Metadata: map[string]any{"access_token": "test-kimi-key"},
	}
	payload := []byte(`{"model":"kimi-k3","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	resp, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{Model: "kimi-k3", Payload: payload}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Headers:        http.Header{"X-Client-Value": []string{"from-options"}},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if want := strings.TrimSuffix(kimiauth.APIBaseURL, "/") + "/v1/responses"; upstreamURL != want {
		t.Fatalf("upstream URL = %q, want %q", upstreamURL, want)
	}
	if got := gjson.GetBytes(upstreamBody, "model").String(); got != "k3" {
		t.Fatalf("upstream model = %q, want k3; body=%s", got, upstreamBody)
	}
	if got := upstreamHeaders.Get("Authorization"); got != "Bearer test-kimi-key" {
		t.Fatalf("Authorization = %q, want Bearer test-kimi-key", got)
	}
	if got := upstreamHeaders.Get("X-Copied"); got != "from-options" {
		t.Fatalf("X-Copied = %q, want from-options", got)
	}
	if got := upstreamHeaders.Get("X-CPA-Session"); got != "kimi-session" {
		t.Fatalf("X-CPA-Session = %q, want kimi-session", got)
	}
	if got := upstreamHeaders.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want empty", got)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.content.0.text").String(); got != "hello world" {
		t.Fatalf("response output text = %q, want hello world", got)
	}

	record := waitForKimiUsageRecord(t, plugin.records, "kimi-responses-passthrough")
	if record.Failed || record.Detail.InputTokens != 6 || record.Detail.OutputTokens != 4 || record.Detail.TotalTokens != 10 {
		t.Fatalf("usage record = %#v", record)
	}
}

func TestKimiExecutorResponsesFallsBackToGinDynamicHeaderSource(t *testing.T) {
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginContext.Request.Header.Set("X-Client-Value", "from-gin")

	var upstreamHeaders http.Header
	ctx := util.WithGinContext(context.Background(), ginContext)
	ctx = util.WithRoundTripper(ctx, kimiTokenRoundTripper(func(req *http.Request) (*http.Response, error) {
		upstreamHeaders = req.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"resp_123","object":"response","status":"completed","output":[]}`,
			)),
			Request: req,
		}, nil
	}))

	executor := NewKimiExecutor(&config.Config{})
	_, err := executor.Execute(
		ctx,
		&cliproxyauth.Auth{
			Provider: "kimi",
			Attributes: map[string]string{
				"header:X-Copied":       "$X-CLIENT-VALUE",
				"header:Content-Length": "999",
			},
			Metadata: map[string]any{"access_token": "test-kimi-key"},
		},
		cliproxyexecutor.Request{
			Model: "kimi-k3",
			Payload: []byte(
				`{"model":"kimi-k3","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
			),
		},
		cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatOpenAIResponse,
			ResponseFormat: sdktranslator.FormatOpenAIResponse,
		},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := upstreamHeaders.Get("X-Copied"); got != "from-gin" {
		t.Fatalf("X-Copied = %q, want from-gin", got)
	}
	if got := upstreamHeaders.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want empty", got)
	}
}

func TestKimiExecutorResponsesWithoutUsageFinalizesZero(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	ctx := usage.WithRequestedModelAlias(context.Background(), "kimi-responses-no-usage")
	ctx = util.WithRoundTripper(ctx, kimiTokenRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_123","object":"response","status":"completed","output":[]}`)),
		}, nil
	}))
	executor := NewKimiExecutor(&config.Config{})
	_, err := executor.Execute(
		ctx,
		&cliproxyauth.Auth{Provider: "kimi", Metadata: map[string]any{"access_token": "test-key"}},
		cliproxyexecutor.Request{Model: "kimi-k3", Payload: []byte(`{"input":"hello"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	record := waitForKimiUsageRecord(t, plugin.records, "kimi-responses-no-usage")
	if record.Failed || record.Detail.InputTokens != 0 || record.Detail.OutputTokens != 0 || record.Detail.TotalTokens != 0 {
		t.Fatalf("usage record = %#v, want successful zero usage", record)
	}
	select {
	case duplicate := <-plugin.records:
		t.Fatalf("duplicate usage record = %#v", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestKimiExecutorResponsesStreamPreservesSSEAndTerminalUsage(t *testing.T) {
	for _, eventType := range []string{"response.completed", "response.incomplete", "response.done"} {
		t.Run(eventType, func(t *testing.T) {
			plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
			registerTestUsagePlugin(t, plugin)
			alias := "kimi-stream-" + eventType
			var upstreamBody []byte
			ctx := usage.WithRequestedModelAlias(context.Background(), alias)
			ctx = util.WithRoundTripper(ctx, kimiTokenRoundTripper(func(req *http.Request) (*http.Response, error) {
				var errRead error
				upstreamBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					return nil, errRead
				}
				sse := "event: response.output_text.delta\n" +
					"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello stream\"}\n\n" +
					"event: " + eventType + "\n" +
					"data: {\"type\":\"" + eventType + "\",\"response\":{\"usage\":{\"total_tokens\":12,\"input_tokens\":5,\"output_tokens\":7}}}\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(sse)),
				}, nil
			}))

			executor := NewKimiExecutor(&config.Config{})
			result, err := executor.ExecuteStream(
				ctx,
				&cliproxyauth.Auth{Provider: "kimi", Metadata: map[string]any{"access_token": "test-key"}},
				cliproxyexecutor.Request{Model: "kimi-k3", Payload: []byte(`{"input":"hello"}`)},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
			)
			if err != nil {
				t.Fatalf("ExecuteStream() error = %v", err)
			}
			var chunks strings.Builder
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream chunk error = %v", chunk.Err)
				}
				chunks.Write(chunk.Payload)
			}
			combined := chunks.String()
			for _, want := range []string{"event: response.output_text.delta\n", "data: {", "\n\nevent: " + eventType + "\n", "hello stream"} {
				if !strings.Contains(combined, want) {
					t.Fatalf("stream output missing %q: %q", want, combined)
				}
			}
			if !gjson.GetBytes(upstreamBody, "stream").Bool() {
				t.Fatalf("upstream stream flag = false; body=%s", upstreamBody)
			}
			record := waitForKimiUsageRecord(t, plugin.records, alias)
			if record.Failed || record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 7 || record.Detail.TotalTokens != 12 {
				t.Fatalf("usage record = %#v", record)
			}
		})
	}
}

func TestKimiExecutorResponsesStreamReadErrorWinsOverUsage(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	ctx := usage.WithRequestedModelAlias(context.Background(), "kimi-stream-read-error")
	ctx = util.WithRoundTripper(ctx, kimiTokenRoundTripper(func(req *http.Request) (*http.Response, error) {
		body := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"total_tokens\":7}}}\n")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(&terminalErrorReader{data: body, err: io.ErrUnexpectedEOF}),
		}, nil
	}))
	executor := NewKimiExecutor(&config.Config{})
	result, err := executor.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{Provider: "kimi", Metadata: map[string]any{"access_token": "test-key"}},
		cliproxyexecutor.Request{Model: "kimi-k3", Payload: []byte(`{"input":"hello"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if !errors.Is(streamErr, io.ErrUnexpectedEOF) {
		t.Fatalf("stream error = %v, want unexpected EOF", streamErr)
	}
	record := waitForKimiUsageRecord(t, plugin.records, "kimi-stream-read-error")
	if !record.Failed || record.Detail.InputTokens != 0 || record.Detail.OutputTokens != 0 || record.Detail.TotalTokens != 0 {
		t.Fatalf("usage record = %#v, want failed zero usage", record)
	}
}

func TestKimiExecutorResponsesStreamCancellationFinalizesFailure(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	ctx, cancel := context.WithCancel(usage.WithRequestedModelAlias(context.Background(), "kimi-stream-cancel"))
	ctx = util.WithRoundTripper(ctx, kimiTokenRoundTripper(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(&kimiContextErrorReader{ctx: req.Context()}),
		}, nil
	}))
	executor := NewKimiExecutor(&config.Config{})
	result, err := executor.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{Provider: "kimi", Metadata: map[string]any{"access_token": "test-key"}},
		cliproxyexecutor.Request{Model: "kimi-k3", Payload: []byte(`{"input":"hello"}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
	)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	cancel()
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if !errors.Is(streamErr, context.Canceled) {
		t.Fatalf("stream error = %v, want context canceled", streamErr)
	}
	record := waitForKimiUsageRecord(t, plugin.records, "kimi-stream-cancel")
	if !record.Failed {
		t.Fatalf("usage record = %#v, want failure", record)
	}
}

func TestKimiExecutorResponsesCompactErrors(t *testing.T) {
	executor := NewKimiExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "kimi", Metadata: map[string]any{"access_token": "test-key"}}
	req := cliproxyexecutor.Request{Model: "kimi-k3", Payload: []byte(`{"input":[]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Alt: "responses/compact"}
	_, errExecute := executor.Execute(context.Background(), auth, req, opts)
	var statusCoder interface{ StatusCode() int }
	if !errors.As(errExecute, &statusCoder) || statusCoder.StatusCode() != http.StatusNotImplemented {
		t.Fatalf("Execute(compact) error = %v, want status 501", errExecute)
	}
	_, errStream := executor.ExecuteStream(context.Background(), auth, req, opts)
	if !errors.As(errStream, &statusCoder) || statusCoder.StatusCode() != http.StatusBadRequest {
		t.Fatalf("ExecuteStream(compact) error = %v, want status 400", errStream)
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
							`{"id":"resp_test","object":"response","status":"completed","model":"k3","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"total_tokens":2,"input_tokens":1,"output_tokens":1}}`,
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

	input := gjson.GetBytes(upstreamBody, "input").Array()
	if got := len(input); got != 4 {
		t.Fatalf("upstream input count = %d, want 4; body=%s", got, upstreamBody)
	}
	if got := input[0].Get("summary.0.text").String(); got != "inspect the next step" {
		t.Fatalf("reasoning summary text = %q, want inspect the next step; body=%s", got, upstreamBody)
	}
	if got := input[1].Get("content.0.text").String(); got != "Step 3 completed; continue to step 4." {
		t.Fatalf("assistant content = %q, want preserved text; body=%s", got, upstreamBody)
	}
	if got := input[2].Get("call_id").String(); got != "call_4" {
		t.Fatalf("function call ID = %q, want call_4; body=%s", got, upstreamBody)
	}
	if got := input[3].Get("call_id").String(); got != "call_4" {
		t.Fatalf("function output call ID = %q, want call_4; body=%s", got, upstreamBody)
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
	if strings.Contains(upstreamBetas, "oauth-2025-04-20") {
		t.Fatal("default Kimi profile injected OAuth beta")
	}
	for _, beta := range []string{"client-beta-one", "client-beta-two"} {
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
		"kimi-k2.8":                       "kimi-for-coding",
		"k2.8-code":                       "kimi-for-coding",
		"kimi-k2.8-preview":               "kimi-for-coding",
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

func TestNormalizeKimiTemperature(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantExists bool
		want       float64
	}{
		{name: "enabled valid", body: `{"thinking":{"type":"enabled"},"temperature":1}`, wantExists: true, want: 1},
		{name: "enabled invalid", body: `{"thinking":{"type":"enabled"},"temperature":0.6}`},
		{name: "default valid", body: `{"temperature":1}`, wantExists: true, want: 1},
		{name: "default invalid", body: `{"temperature":0.6}`},
		{name: "disabled valid", body: `{"thinking":{"type":"disabled"},"temperature":0.6}`, wantExists: true, want: 0.6},
		{name: "disabled invalid", body: `{"thinking":{"type":"disabled"},"temperature":1}`},
		{name: "absent", body: `{"thinking":{"type":"enabled"}}`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			out := normalizeKimiTemperature([]byte(testCase.body))
			temperature := gjson.GetBytes(out, "temperature")
			if temperature.Exists() != testCase.wantExists {
				t.Fatalf("temperature exists = %t, want %t; body=%s", temperature.Exists(), testCase.wantExists, out)
			}
			if testCase.wantExists && temperature.Float() != testCase.want {
				t.Fatalf("temperature = %v, want %v; body=%s", temperature.Float(), testCase.want, out)
			}
		})
	}
}

func TestNormalizeKimiToolsInlinesLocalRefs(t *testing.T) {
	body := []byte(`{
		"tools":[{"type":"function","name":"lookup","parameters":{"$defs":{"entry":{"type":"string"}},"properties":{"value":{"$ref":"#/$defs/entry"}}}}],
		"functions":[{"name":"legacy","parameters":{"definitions":{"entry":{"type":"integer"}},"properties":{"value":{"$ref":"#/definitions/entry"}}}}]
	}`)
	out := normalizeKimiTools(body)
	for _, path := range []string{"tools.0.parameters", "functions.0.parameters"} {
		schema := gjson.GetBytes(out, path)
		if got := schema.Get("type").String(); got != "object" {
			t.Fatalf("%s type = %q, want object; body=%s", path, got, out)
		}
		if schema.Get("$defs").Exists() || schema.Get("definitions").Exists() || schema.Get("properties.value.$ref").Exists() {
			t.Fatalf("%s retained definitions or $ref: %s", path, schema.Raw)
		}
	}
	if got := gjson.GetBytes(out, "tools.0.parameters.properties.value.type").String(); got != "string" {
		t.Fatalf("tool ref type = %q, want string; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "functions.0.parameters.properties.value.type").String(); got != "integer" {
		t.Fatalf("legacy function ref type = %q, want integer; body=%s", got, out)
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

func TestNormalizeKimiToolMessageLinks_DoesNotReuseUnavailableReasoning(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","reasoning_content":"[reasoning unavailable]"},
			{"role":"assistant","content":"current summary","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]}
		]
	}`)
	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}
	if got := gjson.GetBytes(out, "messages.1.reasoning_content").String(); got != "current summary" {
		t.Fatalf("messages.1.reasoning_content = %q, want current summary", got)
	}
}

func TestNormalizeKimiToolMessageLinks_UnavailableDoesNotOverrideUsableReasoning(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","reasoning_content":"real reasoning"},
			{"role":"assistant","reasoning_content":"[reasoning unavailable]"},
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}]}
		]
	}`)
	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}
	if got := gjson.GetBytes(out, "messages.2.reasoning_content").String(); got != "real reasoning" {
		t.Fatalf("messages.2.reasoning_content = %q, want real reasoning", got)
	}
}

func TestNormalizeKimiToolMessageLinks_ReplacesUnavailableReasoning(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","content":"assistant summary","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_directory","arguments":"{}"}}],"reasoning_content":"[reasoning unavailable]"}
		]
	}`)
	out, err := normalizeKimiToolMessageLinks(body)
	if err != nil {
		t.Fatalf("normalizeKimiToolMessageLinks() error = %v", err)
	}
	if got := gjson.GetBytes(out, "messages.0.reasoning_content").String(); got != "assistant summary" {
		t.Fatalf("messages.0.reasoning_content = %q, want assistant summary", got)
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
