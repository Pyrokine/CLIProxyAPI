package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	translatorcommon "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator/common"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestExecutorsRejectUnsupportedUserAttachmentBeforeUpstream(t *testing.T) {
	var upstreamCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	claudeFile := []byte(`{"messages":[{"role":"user","content":[{"type":"container_upload","file_id":"file-not-stored"}]}]}`)
	openAIFile := []byte(`{"messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-not-stored"}}]}]}`)
	interactionsFile := []byte(`{"input":[{"type":"user_input","content":[{"type":"file","file":{"file_id":"file-not-stored"}}]}]}`)
	geminiFile := []byte(`{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"application/pdf"}}]}]}`)
	withKey := func(provider string) *cliproxyauth.Auth {
		return &cliproxyauth.Auth{Provider: provider, Attributes: map[string]string{
			"api_key": "offline-test-key", "base_url": server.URL,
		}}
	}
	cases := []struct {
		name     string
		executor cliproxyauth.ProviderExecutor
		auth     *cliproxyauth.Auth
		from     sdktranslator.Format
		payload  []byte
		partType string
	}{
		{"Claude/OpenAI compatibility", NewOpenAICompatExecutor("openai-compatibility", &config.Config{}), withKey("openai-compatibility"), sdktranslator.FormatClaude, claudeFile, "container_upload"},
		{"Claude/Codex", NewCodexAutoExecutor(&config.Config{}), withKey("codex"), sdktranslator.FormatClaude, claudeFile, "container_upload"},
		{"Interactions/Codex", NewCodexAutoExecutor(&config.Config{}), withKey("codex"), sdktranslator.FormatInteractions, interactionsFile, "file"},
		{"Interactions/Gemini", NewGeminiExecutor(&config.Config{}), withKey("gemini"), sdktranslator.FormatInteractions, interactionsFile, "file"},
		{"Gemini/GeminiInteractions", NewGeminiInteractionsExecutor(&config.Config{}), withKey("gemini-interactions"), sdktranslator.FormatGemini, geminiFile, "fileData"},
		{"Interactions/Antigravity", NewAntigravityExecutor(&config.Config{}), withKey("antigravity"), sdktranslator.FormatInteractions, interactionsFile, "file"},
		{"OpenAI/Claude", NewClaudeExecutor(&config.Config{}), withKey("claude"), sdktranslator.FormatOpenAI, openAIFile, "file"},
		{"OpenAI/Gemini", NewGeminiExecutor(&config.Config{}), withKey("gemini"), sdktranslator.FormatOpenAI, openAIFile, "file"},
		{"OpenAI/GeminiInteractions", NewGeminiInteractionsExecutor(&config.Config{}), withKey("gemini-interactions"), sdktranslator.FormatOpenAI, openAIFile, "file"},
		{"OpenAI/Vertex", NewGeminiVertexExecutor(&config.Config{}), withKey("vertex"), sdktranslator.FormatOpenAI, openAIFile, "file"},
		{"Claude/Qwen", NewQwenExecutor(&config.Config{}), withKey("qwen"), sdktranslator.FormatClaude, claudeFile, "container_upload"},
		{"Claude/iFlow", NewIFlowExecutor(&config.Config{}), withKey("iflow"), sdktranslator.FormatClaude, claudeFile, "container_upload"},
		{"OpenAI/AIStudio", NewAIStudioExecutor(&config.Config{}, "aistudio", nil), withKey("aistudio"), sdktranslator.FormatOpenAI, openAIFile, "file"},
		{"Claude/Antigravity", NewAntigravityExecutor(&config.Config{}), withKey("antigravity"), sdktranslator.FormatClaude, claudeFile, "container_upload"},
		{"Claude/GeminiCLI", NewGeminiCLIExecutor(&config.Config{}), withKey("gemini-cli"), sdktranslator.FormatClaude, claudeFile, "container_upload"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req := cliproxyexecutor.Request{Model: "test-model", Payload: tt.payload}
			for _, method := range []string{"Execute", "ExecuteStream", "CountTokens"} {
				t.Run(method, func(t *testing.T) {
					ctx := context.Background()
					opts := cliproxyexecutor.Options{SourceFormat: tt.from, Stream: method == "ExecuteStream"}
					var err error
					switch method {
					case "Execute":
						_, err = tt.executor.Execute(ctx, tt.auth, req, opts)
					case "ExecuteStream":
						_, err = tt.executor.ExecuteStream(ctx, tt.auth, req, opts)
					case "CountTokens":
						_, err = tt.executor.CountTokens(ctx, tt.auth, req, opts)
					}
					var unsupported *translatorcommon.UnsupportedPartError
					if !errors.As(err, &unsupported) || unsupported.Type != tt.partType || unsupported.StatusCode() != http.StatusBadRequest || !unsupported.IsRequestScoped() {
						t.Fatalf("%s error = %v, want request-scoped 400 for %s", method, err, tt.partType)
					}
				})
			}
		})
	}
	_, err := NewAntigravityExecutor(&config.Config{}).Execute(
		context.Background(), withKey("antigravity"),
		cliproxyexecutor.Request{Model: "claude-sonnet-4", Payload: claudeFile},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
	)
	var unsupported *translatorcommon.UnsupportedPartError
	if !errors.As(err, &unsupported) || unsupported.Type != "container_upload" {
		t.Fatalf("Antigravity Claude bridge error = %v, want unsupported container_upload", err)
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want zero", upstreamCalls)
	}
}

type geminiInteractionsRoundTripper func(*http.Request) (*http.Response, error)

type terminalErrorReader struct {
	data []byte
	err  error
	done bool
}

func (f geminiInteractionsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func (r *terminalErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	return copy(p, r.data), r.err
}

func TestGeminiInteractionsExecutorRequestToFormat(t *testing.T) {
	exec := NewGeminiInteractionsExecutor(&config.Config{})
	if got := exec.Identifier(); got != "gemini-interactions" {
		t.Fatalf("identifier = %q, want gemini-interactions", got)
	}

	for _, sourceFormat := range []sdktranslator.Format{
		sdktranslator.FormatInteractions,
		sdktranslator.FormatOpenAI,
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatClaude,
		sdktranslator.FormatGemini,
	} {
		t.Run(
			sourceFormat.String(), func(t *testing.T) {
				got := exec.RequestToFormat(
					cliproxyexecutor.Request{}, cliproxyexecutor.Options{SourceFormat: sourceFormat},
				)
				if got != sdktranslator.FormatInteractions {
					t.Fatalf("RequestToFormat(%q) = %q, want interactions", sourceFormat, got)
				}
			},
		)
	}

	if got := exec.RequestToFormat(
		cliproxyexecutor.Request{}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex},
	); got != sdktranslator.FormatGemini {
		t.Fatalf("RequestToFormat(codex) = %q, want gemini", got)
	}
}

func TestGeminiInteractionsExecuteUsesNativeEndpointAndRevision(t *testing.T) {
	var gotPath string
	var gotRevision string
	var gotAPIKey string
	var gotBody []byte

	ctx := util.WithRoundTripper(
		context.Background(),
		geminiInteractionsRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				gotPath = req.URL.Path
				gotRevision = req.Header.Get("Api-Revision")
				gotAPIKey = req.Header.Get("x-goog-api-key")
				var errRead error
				gotBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					t.Fatalf("read body: %v", errRead)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"interaction":{"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}`)),
					Request:    req,
				}, nil
			},
		),
	)

	exec := NewGeminiInteractionsExecutor(&config.Config{})
	resp, err := exec.Execute(
		ctx,
		&cliproxyauth.Auth{
			Provider: "gemini-interactions",
			Attributes: map[string]string{
				"api_key":  "interactions-key",
				"base_url": "https://interactions.example.com",
			},
		},
		cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: []byte(`{"model":"client-model","input":"hi"}`)},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: sdktranslator.FormatInteractions,
		},
	)
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if gotPath != "/v1beta/interactions" {
		t.Fatalf("path = %q, want /v1beta/interactions", gotPath)
	}
	if gotRevision != geminiInteractionsAPIRevision {
		t.Fatalf("Api-Revision = %q, want %q", gotRevision, geminiInteractionsAPIRevision)
	}
	if gotAPIKey != "interactions-key" {
		t.Fatalf("x-goog-api-key = %q, want interactions-key", gotAPIKey)
	}
	if got := gjson.GetBytes(gotBody, "model").String(); got != "gemini-2.5-pro" {
		t.Fatalf("request model = %q, want gemini-2.5-pro; body=%s", got, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("non-stream body must not force stream: %s", string(gotBody))
	}
	if len(resp.Payload) == 0 {
		t.Fatal("Execute returned empty payload")
	}
}

func TestGeminiInteractionsExecuteUsesClientRevisionHeader(t *testing.T) {
	var gotRevision string
	ctx := util.WithRoundTripper(
		context.Background(),
		geminiInteractionsRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				gotRevision = req.Header.Get("Api-Revision")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":1}}`)),
					Request:    req,
				}, nil
			},
		),
	)

	exec := NewGeminiInteractionsExecutor(&config.Config{})
	_, err := exec.Execute(
		ctx,
		&cliproxyauth.Auth{
			Provider: "gemini-interactions", Attributes: map[string]string{"api_key": "interactions-key"},
		},
		cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: []byte(`{"input":"hi"}`)},
		cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatInteractions,
			ResponseFormat: sdktranslator.FormatInteractions,
			Headers:        http.Header{"Api-Revision": []string{"2026-06-01"}},
		},
	)
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if gotRevision != "2026-06-01" {
		t.Fatalf("Api-Revision = %q, want 2026-06-01", gotRevision)
	}
}

func TestGeminiExecuteUsesExplicitDynamicHeaderSource(t *testing.T) {
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginContext.Request.Header.Set("X-Client-Value", "from-gin")

	var upstreamHeaders http.Header
	ctx := util.WithGinContext(context.Background(), ginContext)
	ctx = util.WithSessionID(ctx, "gemini-session")
	ctx = util.WithRoundTripper(ctx, geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
		upstreamHeaders = req.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`,
			)),
			Request: req,
		}, nil
	}))

	exec := NewGeminiExecutor(&config.Config{})
	_, err := exec.Execute(
		ctx,
		&cliproxyauth.Auth{
			Provider: "gemini",
			Attributes: map[string]string{
				"api_key":               "test-key",
				"header:X-Copied":       "$X-CLIENT-VALUE",
				"header:X-CPA-Session":  "$cpa-session-id",
				"header:X-Missing":      "$Missing-Header",
				"header:X-Auth-Copy":    "$Authorization",
				"header:X-Cookie-Copy":  "$Cookie",
				"header:Authorization":  "Bearer custom",
				"header:Content-Length": "999",
			},
		},
		cliproxyexecutor.Request{
			Model: "gemini-2.5-pro",
			Payload: []byte(
				`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			),
		},
		cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatGemini,
			ResponseFormat: sdktranslator.FormatGemini,
			Headers: http.Header{
				"x-cLiEnT-vAlUe": []string{"from-options"},
				"Authorization":  []string{"Bearer caller"},
				"Cookie":         []string{"caller=cookie"},
			},
		},
	)
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if got := upstreamHeaders.Get("X-Copied"); got != "from-options" {
		t.Fatalf("X-Copied = %q, want from-options", got)
	}
	if got := upstreamHeaders.Get("X-CPA-Session"); got != "gemini-session" {
		t.Fatalf("X-CPA-Session = %q, want gemini-session", got)
	}
	for _, name := range []string{"X-Missing", "X-Auth-Copy", "X-Cookie-Copy", "Authorization"} {
		if got := upstreamHeaders.Get(name); got != "" {
			t.Fatalf("%s = %q, want empty", name, got)
		}
	}
	if got := upstreamHeaders.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want empty", got)
	}
	if got := upstreamHeaders.Get("x-goog-api-key"); got != "test-key" {
		t.Fatalf("x-goog-api-key = %q, want test-key", got)
	}
}

func TestGeminiExecuteFallsBackToGinDynamicHeaderSource(t *testing.T) {
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ginContext.Request.Header.Set("X-Client-Value", "from-gin")

	var upstreamHeaders http.Header
	ctx := util.WithGinContext(context.Background(), ginContext)
	ctx = util.WithRoundTripper(ctx, geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
		upstreamHeaders = req.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`,
			)),
			Request: req,
		}, nil
	}))

	exec := NewGeminiExecutor(&config.Config{})
	_, err := exec.Execute(
		ctx,
		&cliproxyauth.Auth{
			Provider: "gemini",
			Attributes: map[string]string{
				"api_key":               "test-key",
				"header:X-Copied":       "$X-CLIENT-VALUE",
				"header:Content-Length": "999",
			},
		},
		cliproxyexecutor.Request{
			Model: "gemini-2.5-pro",
			Payload: []byte(
				`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			),
		},
		cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatGemini,
			ResponseFormat: sdktranslator.FormatGemini,
		},
	)
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if got := upstreamHeaders.Get("X-Copied"); got != "from-gin" {
		t.Fatalf("X-Copied = %q, want from-gin", got)
	}
	if got := upstreamHeaders.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length = %q, want empty", got)
	}
}

func TestGeminiExecuteStreamReadErrorWinsOverBufferedUsage(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 64)}
	registerTestUsagePlugin(t, plugin)

	ctx := util.WithRoundTripper(
		context.Background(),
		geminiInteractionsRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				body := []byte("data: {\"candidates\":[],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":4,\"totalTokenCount\":7}}\n")
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body: io.NopCloser(
						&terminalErrorReader{data: body, err: io.ErrUnexpectedEOF},
					),
					Request: req,
				}, nil
			},
		),
	)
	ctx = usage.WithRequestedModelAlias(ctx, "gemini-read-error-test")

	exec := NewGeminiExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{Provider: "gemini", Attributes: map[string]string{"api_key": "test-key"}},
		cliproxyexecutor.Request{
			Model: "gemini-2.5-pro", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
		},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini, Stream: true,
		},
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

	deadline := time.After(time.Second)
waitForReadErrorUsage:
	for {
		select {
		case record := <-plugin.records:
			if record.Alias != "gemini-read-error-test" {
				continue
			}
			if !record.Failed {
				t.Fatalf("usage record = %#v, want failure", record)
			}
			break waitForReadErrorUsage
		case <-deadline:
			t.Fatal("timed out waiting for failed usage record")
		}
	}
	select {
	case record := <-plugin.records:
		if record.Alias == "gemini-read-error-test" {
			t.Fatalf("received duplicate usage record: %#v", record)
		}
	case <-time.After(20 * time.Millisecond):
	}
}

func TestGeminiExecuteStreamCancellationWinsOverBufferedUsage(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
	registerTestUsagePlugin(t, plugin)

	baseCtx := util.WithRoundTripper(
		context.Background(),
		geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"candidates\":[],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":4,\"totalTokenCount\":7}}\n",
				)),
				Request: req,
			}, nil
		}),
	)
	baseCtx = usage.WithRequestedModelAlias(baseCtx, "gemini-cancel-test")
	ctx, cancel := context.WithCancel(baseCtx)

	exec := NewGeminiExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{Provider: "gemini", Attributes: map[string]string{"api_key": "test-key"}},
		cliproxyexecutor.Request{
			Model: "gemini-2.5-pro", Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
		},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini, Stream: true,
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error = %v", err)
	}
	cancel()
	for range stream.Chunks {
	}

	deadline := time.After(time.Second)
waitForCanceledUsage:
	for {
		select {
		case record := <-plugin.records:
			if record.Alias != "gemini-cancel-test" {
				continue
			}
			if !record.Failed || record.Fail.StatusCode != 499 {
				t.Fatalf("usage record = %#v, want canceled failure", record)
			}
			if record.Detail.InputTokens != 0 || record.Detail.OutputTokens != 0 || record.Detail.TotalTokens != 0 {
				t.Fatalf("failure usage detail = %+v, want zero", record.Detail)
			}
			break waitForCanceledUsage
		case <-deadline:
			t.Fatal("timed out waiting for canceled usage record")
		}
	}
	select {
	case record := <-plugin.records:
		if record.Alias == "gemini-cancel-test" {
			t.Fatalf("received duplicate usage record: %#v", record)
		}
	case <-time.After(20 * time.Millisecond):
	}
}

func TestGeminiInteractionsExecuteStreamCancellationWinsOverBufferedUsage(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
	registerTestUsagePlugin(t, plugin)

	baseCtx := util.WithRoundTripper(
		context.Background(),
		geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"delta\":\"ok\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"total_tokens\":7}}\n\n" +
						"event: done\n\n",
				)),
				Request: req,
			}, nil
		}),
	)
	baseCtx = usage.WithRequestedModelAlias(baseCtx, "gemini-interactions-cancel-test")
	ctx, cancel := context.WithCancel(baseCtx)

	exec := NewGeminiInteractionsExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{Provider: "gemini-interactions", Attributes: map[string]string{"api_key": "test-key"}},
		cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: []byte(`{"input":"hi"}`)},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: sdktranslator.FormatInteractions, Stream: true,
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error = %v", err)
	}
	cancel()
	for range stream.Chunks {
	}

	deadline := time.After(time.Second)
	for {
		select {
		case record := <-plugin.records:
			if record.Alias != "gemini-interactions-cancel-test" {
				continue
			}
			if !record.Failed || record.Fail.StatusCode != 499 {
				t.Fatalf("usage record = %#v, want canceled failure", record)
			}
			if record.Detail.InputTokens != 0 || record.Detail.OutputTokens != 0 || record.Detail.TotalTokens != 0 {
				t.Fatalf("failure usage detail = %+v, want zero", record.Detail)
			}
			select {
			case duplicate := <-plugin.records:
				if duplicate.Alias == "gemini-interactions-cancel-test" {
					t.Fatalf("received duplicate usage record: %#v", duplicate)
				}
			case <-time.After(20 * time.Millisecond):
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for canceled usage record")
		}
	}
}

func TestGeminiInteractionsExecuteStreamRejectsOversizedFrame(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	line := "data: " + strings.Repeat("x", 1<<20) + "\n"
	body := strings.Repeat(line, 51)
	ctx := util.WithRoundTripper(
		context.Background(),
		geminiInteractionsRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    req,
				}, nil
			},
		),
	)

	exec := NewGeminiInteractionsExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{Provider: "gemini-interactions", Attributes: map[string]string{"api_key": "test-key"}},
		cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: []byte(`{"input":"hi"}`)},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: sdktranslator.FormatInteractions,
			Stream: true,
		},
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
	if streamErr == nil || !strings.Contains(streamErr.Error(), "SSE frame exceeds maximum allowed size") {
		t.Fatalf("stream error = %v", streamErr)
	}

	select {
	case record := <-plugin.records:
		if !record.Failed {
			t.Fatalf("usage record = %#v, want failure", record)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
}

func TestGeminiInteractionsExecuteStreamRejectsEOFBeforeTerminalEvent(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 4)}
	registerTestUsagePlugin(t, plugin)

	ctx := util.WithRoundTripper(
		context.Background(),
		geminiInteractionsRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				body := "data: {\"event_type\":\"step.delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    req,
				}, nil
			},
		),
	)
	ctx = usage.WithRequestedModelAlias(ctx, "gemini-interactions-truncated")

	exec := NewGeminiInteractionsExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{Provider: "gemini-interactions", Attributes: map[string]string{"api_key": "test-key"}},
		cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: []byte(`{"input":"hi"}`)},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: sdktranslator.FormatInteractions,
			Stream: true,
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error = %v", err)
	}

	var output bytes.Buffer
	var streamErr error
	for chunk := range stream.Chunks {
		output.Write(chunk.Payload)
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if !strings.Contains(output.String(), `"text":"partial"`) {
		t.Fatalf("stream output missing accepted partial frame: %s", output.String())
	}
	if streamErr == nil || !strings.Contains(streamErr.Error(), "ended before terminal event") {
		t.Fatalf("stream error = %v, want missing terminal event", streamErr)
	}

	deadline := time.After(time.Second)
	for {
		select {
		case record := <-plugin.records:
			if record.Alias != "gemini-interactions-truncated" {
				continue
			}
			if !record.Failed {
				t.Fatalf("usage record = %#v, want failure", record)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for failed usage record")
		}
	}
}

func TestGeminiInteractionsExecuteStreamForcesStreamAndPassesThroughInteractionsFrames(t *testing.T) {
	var gotPath string
	var gotBody []byte
	ctx := util.WithRoundTripper(
		context.Background(),
		geminiInteractionsRoundTripper(
			func(req *http.Request) (*http.Response, error) {
				gotPath = req.URL.Path
				var errRead error
				gotBody, errRead = io.ReadAll(req.Body)
				if errRead != nil {
					t.Fatalf("read body: %v", errRead)
				}
				body := "data: {\"delta\":\"ok\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}\n\n" +
					"event: done\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    req,
				}, nil
			},
		),
	)

	exec := NewGeminiInteractionsExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{
			Provider: "gemini-interactions", Attributes: map[string]string{"api_key": "interactions-key"},
		},
		cliproxyexecutor.Request{Model: "gemini-2.5-pro", Payload: []byte(`{"input":"hi"}`)},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: sdktranslator.FormatInteractions,
			Stream: true,
		},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error = %v", err)
	}
	if gotPath != "/v1beta/interactions" {
		t.Fatalf("path = %q, want /v1beta/interactions", gotPath)
	}
	if got := gjson.GetBytes(gotBody, "stream").Bool(); !got {
		t.Fatalf("stream = %v, want true; body=%s", got, string(gotBody))
	}

	var out bytes.Buffer
	timer := time.After(2 * time.Second)
	for {
		select {
		case chunk, ok := <-stream.Chunks:
			if !ok {
				output := out.String()
				if !strings.Contains(output, `data: {"delta":"ok"`) {
					t.Fatalf("stream output missing data frame: %s", output)
				}
				if !strings.Contains(output, "event: done\n\n") {
					t.Fatalf("stream output missing done frame: %s", output)
				}
				return
			}
			if chunk.Err != nil {
				t.Fatalf("stream chunk error = %v", chunk.Err)
			}
			out.Write(chunk.Payload)
		case <-timer:
			t.Fatal("timed out waiting for stream chunks")
		}
	}
}

func TestPrepareGeminiRequestNormalizesBoundaryTurns(t *testing.T) {
	executor := NewGeminiExecutor(&config.Config{})
	prepared, errPrepare := executor.prepareGeminiRequest(
		context.Background(),
		&cliproxyauth.Auth{},
		cliproxyexecutor.Request{
			Model:   "gemini-2.5-pro",
			Payload: []byte(`{"contents":[{"role":"model","parts":[{"text":"answer"}]}]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
		false,
	)
	if errPrepare != nil {
		t.Fatalf("prepareGeminiRequest() error = %v", errPrepare)
	}
	assertGeminiBoundaryRoles(t, prepared.body, []string{"user", "model", "user"})
}

func TestPrepareGeminiCountTokensPreservesTrailingModelTurn(t *testing.T) {
	executor := NewGeminiExecutor(&config.Config{})
	prepared, errPrepare := executor.prepareGeminiRequest(
		context.Background(),
		&cliproxyauth.Auth{},
		cliproxyexecutor.Request{
			Model:    "gemini-2.5-pro",
			Payload:  []byte(`{"contents":[{"role":"model","parts":[{"text":"answer"}]}]}`),
			Metadata: map[string]any{"action": "countTokens"},
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
		false,
	)
	if errPrepare != nil {
		t.Fatalf("prepareGeminiRequest() error = %v", errPrepare)
	}
	assertGeminiBoundaryRoles(t, prepared.body, []string{"user", "model"})
}

func TestGeminiFailedCompletionUsesFailureAccounting(t *testing.T) {
	for _, provider := range []string{"gemini", "vertex"} {
		for _, sample := range []struct {
			name, wire string
			stream     bool
			valid      bool
		}{
			{name: "nonstream-empty"},
			{name: "nonstream-malformed", wire: `{"invalid"`},
			{name: "stream-empty", stream: true},
			{name: "stream-progress-only", stream: true, wire: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n"},
			{name: "nonstream-complete", valid: true, wire: `{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"totalTokenCount":5}}`},
			{name: "stream-complete", valid: true, stream: true, wire: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":3,\"totalTokenCount\":5}}\n\n"},
		} {
			t.Run(provider+"/"+sample.name, func(t *testing.T) {
				capture := &captureUsagePlugin{records: make(chan usage.Record, 4)}
				registerTestUsagePlugin(t, capture)
				calls := 0
				ctx := usage.WithRequestedModelAlias(context.Background(), t.Name())
				ctx = util.WithRoundTripper(ctx, geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sample.wire)), Request: req}, nil
				}))
				var executor cliproxyauth.ProviderExecutor = NewGeminiExecutor(&config.Config{})
				if provider == "vertex" {
					executor = NewGeminiVertexExecutor(&config.Config{})
				}
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: provider, Attributes: map[string]string{"api_key": "synthetic-token", "base_url": "https://synthetic.invalid"}}
				payload := []byte(`{"model":"gemini-2.5-flash","input":"hello"}`)
				req := cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: payload}
				opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: payload, Stream: sample.stream}
				var err error
				if sample.stream {
					var result *cliproxyexecutor.StreamResult
					result, err = executor.ExecuteStream(ctx, auth, req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = executor.Execute(ctx, auth, req, opts)
				}
				if calls != 1 {
					t.Fatalf("dispatches=%d, want 1", calls)
				}
				var status interface{ StatusCode() int }
				if sample.valid {
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.As(err, &status) || status.StatusCode() != http.StatusBadGateway {
					t.Fatalf("failed completion returned %v, want 502", err)
				}
				select {
				case record := <-capture.records:
					if record.Failed == sample.valid {
						t.Fatalf("failed=%t, valid completion=%t", record.Failed, sample.valid)
					}
					if sample.valid && record.Detail.TotalTokens != 5 {
						t.Fatalf("valid completion lost usage: %+v", record.Detail)
					}
				case <-time.After(time.Second):
					t.Fatal("usage record was not delivered")
				}
			})
		}
	}
}

func TestGeminiResponseErrorPreservesSSEFrames(t *testing.T) {
	for _, sample := range []struct {
		name, payload string
		failed        bool
	}{
		{name: "pretty JSON", payload: "{\n\"status\":\"completed\",\n\"output\":[]\n}"},
		{name: "multiline SSE", payload: "event: response.completed\ndata: {\ndata: \"type\":\"response.completed\",\ndata: \"response\":{\"status\":\"completed\"}\ndata: }\n\n"},
		{name: "packed SSE", payload: "event: response.created\ndata: {\"type\":\"response.created\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"},
		{name: "CRLF SSE", payload: "event: response.completed\r\ndata: {\r\ndata: \"type\":\"response.completed\"\r\ndata: }\r\n\r\n"},
		{name: "keepalive", payload: ": keep-alive\n\n"},
		{name: "failed multiline SSE", failed: true, payload: "event: response.failed\ndata: {\ndata: \"type\":\"response.failed\",\ndata: \"response\":{\"status\":\"failed\"}\ndata: }\n\n"},
	} {
		t.Run(sample.name, func(t *testing.T) {
			var param any
			err := geminiResponseError(&param, []byte(sample.payload))
			if (err != nil) != sample.failed {
				t.Fatalf("error=%v, failed payload=%t", err, sample.failed)
			}
		})
	}
}

func TestGeminiInteractionsStopsAtFirstDoneMarker(t *testing.T) {
	for _, provider := range []string{"gemini-interactions", "vertex"} {
		t.Run(provider, func(t *testing.T) {
			wire := "event: interaction.completed\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"done-control\",\"status\":\"completed\",\"steps\":[],\"usage\":{\"total_input_tokens\":2,\"total_output_tokens\":3,\"total_tokens\":5}}}\n\n" +
				"data: [DONE]\n\ndata: [DONE]\n\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":{\"id\":\"late-event\"}}\n\n"
			calls := 0
			ctx := util.WithRoundTripper(context.Background(), geminiInteractionsRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wire)), Request: req}, nil
			}))
			var executor cliproxyauth.ProviderExecutor = NewGeminiInteractionsExecutor(&config.Config{})
			if provider == "vertex" {
				executor = NewGeminiVertexExecutor(&config.Config{})
			}
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: provider, Attributes: map[string]string{"api_key": "synthetic-token", "base_url": "https://synthetic.invalid", "interactions": "true"}}
			result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: []byte(`{"input":"hello"}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: sdktranslator.FormatInteractions})
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				output.Write(chunk.Payload)
			}
			if calls != 1 || strings.Count(output.String(), "[DONE]") != 1 || strings.Contains(output.String(), "late-event") {
				t.Fatalf("calls=%d, output=%s", calls, output.String())
			}
		})
	}
}

func TestGeminiNativePatchFailureDeliversGeneratedFrames(t *testing.T) {
	for _, provider := range []string{"gemini-interactions", "vertex"} {
		t.Run(provider, func(t *testing.T) {
			wire := "data: {\"event_type\":\"interaction.start\",\"interaction\":{\"id\":\"native-patch\"}}\n\n" +
				"data: {\"event_type\":\"step.start\",\"index\":0,\"step\":{\"type\":\"function_call\",\"id\":\"item-patch\",\"call_id\":\"call-patch\",\"name\":\"apply_patch\",\"arguments\":{\"input\":7}}}\n\n" +
				"data: {\"event_type\":\"step.stop\",\"index\":0}\n\ndata: [DONE]\n\n"
			calls := 0
			ctx := util.WithRoundTripper(context.Background(), geminiInteractionsRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				if !strings.HasSuffix(request.URL.Path, "/interactions") {
					t.Errorf("upstream path=%s", request.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(wire)), Request: request}, nil
			}))
			var executor cliproxyauth.ProviderExecutor = NewGeminiInteractionsExecutor(&config.Config{})
			if provider == "vertex" {
				executor = NewGeminiVertexExecutor(&config.Config{})
			}
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: provider, Attributes: map[string]string{"api_key": "synthetic-token", "base_url": "https://synthetic.invalid", "interactions": "true"}}
			original := []byte(`{"model":"gemini-2.5-flash","input":"patch a file","tools":[{"type":"custom","name":"apply_patch"}]}`)
			payload := []byte(`{"model":"gemini-2.5-flash","input":"patch a file","tools":[{"type":"function","name":"apply_patch","parameters":{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}}]}`)
			result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: payload}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: sdktranslator.FormatOpenAIResponse, OriginalRequest: original, Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			var failure error
			for chunk := range result.Chunks {
				output.Write(chunk.Payload)
				if chunk.Err != nil {
					failure = chunk.Err
				}
			}
			var status interface{ StatusCode() int }
			if calls != 1 || !errors.As(failure, &status) || status.StatusCode() != http.StatusBadGateway || strings.Count(output.String(), `"type":"response.failed"`) != 1 || strings.Contains(output.String(), `"type":"response.completed"`) || strings.Contains(output.String(), "[DONE]") {
				t.Fatalf("dispatches=%d, error=%v, output=%s", calls, failure, output.String())
			}
		})
	}
}

func TestGeminiEffectiveTokenTimingAtExecutor(t *testing.T) {
	for _, tc := range []struct {
		name string
		part string
	}{
		{"text", `{"text":"hello"}`},
		{"thought", `{"text":"thinking","thought":true}`},
		{"function", `{"functionCall":{"name":"lookup","args":{"query":"hello"}}}`},
		{"image", `{"inlineData":{"mimeType":"image/png","data":"aGk="}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := &captureUsagePlugin{records: make(chan usage.Record, 4)}
			registerTestUsagePlugin(t, capture)
			usage.StartDefault(context.Background())
			synctest.Test(t, func(t *testing.T) {
				transport := openAICompatTTFTRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					time.Sleep(250 * time.Millisecond)
					reader, writer := io.Pipe()
					go func() {
						defer func() { _ = writer.Close() }()
						write := func(payload string) bool {
							_, err := io.WriteString(writer, payload)
							return err == nil
						}
						if !write(": keepalive\n\ndata: {\"modelVersion\":\"gemini-2.5-flash\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[]}}]}\n\n") {
							return
						}
						time.Sleep(84 * time.Second)
						if !write("data: {\"modelVersion\":\"gemini-2.5-flash\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[" + tc.part + "]}}]}\n\n") {
							return
						}
						time.Sleep(2 * time.Second)
						write("data: {\"modelVersion\":\"gemini-2.5-flash\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":1,\"totalTokenCount\":6}}\n\n")
					}()
					return &http.Response{StatusCode: http.StatusOK,
						Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader, Request: req}, nil
				})
				ctx := util.WithRoundTripper(logging.WithResponseTimingHolder(context.Background()), http.RoundTripper(transport))
				auth := &cliproxyauth.Auth{Provider: "gemini", Attributes: map[string]string{
					"api_key": "synthetic", "base_url": "http://example.invalid"}}
				result, err := NewGeminiExecutor(&config.Config{}).ExecuteStream(ctx, auth,
					cliproxyexecutor.Request{Model: "gemini-2.5-flash",
						Payload: []byte(`{"model":"gemini-2.5-flash","contents":[{"role":"user","parts":[{"text":"hello"}]}]}`)},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini, ResponseFormat: sdktranslator.FormatGemini, Stream: true})
				if err != nil {
					t.Fatalf("ExecuteStream: %v", err)
				}
				var delivered strings.Builder
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream: %v", chunk.Err)
					}
					delivered.Write(chunk.Payload)
				}
				if !strings.Contains(delivered.String(), tc.part) {
					t.Fatalf("effective content was not delivered: %s", delivered.String())
				}
			})
			record := awaitOpenAICompatUsage(t, capture)
			if got := time.Duration(record.TimeToFirstByteMs) * time.Millisecond; got != 84*time.Second+250*time.Millisecond {
				t.Fatalf("effective-token TTFT = %v, want 84.25s", got)
			}
			if record.Failed || record.Detail.TotalTokens != 6 || record.ResponseModel != "gemini-2.5-flash" {
				t.Fatalf("published stream record = %#v", record)
			}
		})
	}
}

func TestGeminiInteractionsSSETerminalSnapshotFormats(t *testing.T) {
	complete := `{"id":"flat-terminal","status":"completed","steps":[]}`
	for _, test := range []struct {
		name, frame                string
		terminal, done, nativeOnly bool
	}{
		{name: "named-flat", frame: "event: interaction.completed\ndata: " + complete + "\n\n", terminal: true, nativeOnly: true},
		{name: "nested", frame: `data: {"event_type":"interaction.completed","interaction":` + complete + "}\n\n", terminal: true},
		{name: "unnamed-flat", frame: "data: " + complete + "\n\n"},
		{name: "wrong-event", frame: "event: interaction.created\ndata: " + complete + "\n\n"},
		{name: "missing-id", frame: "event: interaction.completed\ndata: {\"status\":\"completed\",\"steps\":[]}\n\n"},
		{name: "missing-steps", frame: "event: interaction.completed\ndata: {\"id\":\"flat-terminal\",\"status\":\"completed\"}\n\n"},
		{name: "incomplete-status", frame: "event: interaction.completed\ndata: {\"id\":\"flat-terminal\",\"status\":\"in_progress\",\"steps\":[]}\n\n"},
		{name: "named-flat-action", frame: "event: interaction.completed\ndata: " + strings.Replace(complete, "completed", "requires_action", 1) + "\n\n", terminal: true, nativeOnly: true},
		{name: "nested-action", frame: `data: {"event_type":"interaction.completed","interaction":` + strings.Replace(complete, "completed", "requires_action", 1) + `}`, terminal: true},
		{name: "action-missing-steps", frame: `data: {"event_type":"interaction.completed","interaction":{"id":"action","status":"requires_action"}}`},
		{name: "action-missing-id", frame: `data: {"event_type":"interaction.completed","interaction":{"status":"requires_action","steps":[]}}`},
		{name: "action-in-progress", frame: `data: {"event_type":"interaction.completed","interaction":{"id":"action","status":"in_progress","steps":[]}}`},
		{name: "done-data", frame: "data: [DONE]\n\n", terminal: true, done: true},
		{name: "done-event", frame: "event: done\n\n", terminal: true, done: true},
	} {
		for _, format := range []sdktranslator.Format{sdktranslator.FormatInteractions, sdktranslator.FormatClaude} {
			t.Run(test.name+"/"+format.String(), func(t *testing.T) {
				terminal, done := geminiInteractionsSSEDone([]byte(test.frame), format == sdktranslator.FormatInteractions)
				want := test.terminal && (!test.nativeOnly || format == sdktranslator.FormatInteractions)
				if terminal != want || done != test.done {
					t.Fatalf("terminal=%t done=%t; want %t/%t", terminal, done, want, test.done)
				}
			})
		}
	}
}

func TestGeminiInteractionsNamedSnapshotAtExecutor(t *testing.T) {
	for _, provider := range []string{"gemini-interactions", "vertex"} {
		t.Run(provider, func(t *testing.T) {
			capture := &captureUsagePlugin{records: make(chan usage.Record, 4)}
			registerTestUsagePlugin(t, capture)
			usage.StartDefault(context.Background())
			wire := "event: interaction.completed\ndata: {\"id\":\"flat-terminal\",\"status\":\"completed\",\"steps\":[{\"type\":\"model_output\",\"content\":[{\"type\":\"text\",\"text\":\"native-answer\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}\n\n"
			calls := 0
			ctx := util.WithRoundTripper(context.Background(), geminiInteractionsRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.Host != "synthetic.invalid" || !strings.HasSuffix(request.URL.Path, "/interactions") {
					return nil, errors.New("unexpected request " + request.URL.String())
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: request, Body: io.NopCloser(strings.NewReader(wire))}, nil
			}))
			var executor cliproxyauth.ProviderExecutor = NewGeminiInteractionsExecutor(&config.Config{})
			if provider == "vertex" {
				executor = NewGeminiVertexExecutor(&config.Config{})
			}
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: provider, Attributes: map[string]string{"api_key": "synthetic-token", "base_url": "https://synthetic.invalid", "interactions": "true"}}
			result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "gemini-3.8-flash", Payload: []byte(`{"input":"hello"}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: sdktranslator.FormatInteractions, Stream: true})
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				output.Write(chunk.Payload)
			}
			if calls != 1 || output.String() != wire {
				t.Fatalf("calls=%d, output=%q", calls, output.String())
			}
			select {
			case record := <-capture.records:
				if record.Failed || record.Detail.InputTokens != 2 || record.Detail.OutputTokens != 3 || record.Detail.TotalTokens != 5 {
					t.Fatalf("usage=%+v", record)
				}
			case <-time.After(time.Second):
				t.Fatal("native snapshot usage missing")
			}
		})
	}
}

func TestGeminiInteractionsActionSnapshotTerminatesStream(t *testing.T) {
	for _, provider := range []string{"gemini-interactions", "vertex"} {
		for _, format := range []sdktranslator.Format{sdktranslator.FormatInteractions, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude, sdktranslator.FormatGemini} {
			for _, status := range []string{"completed", "requires_action"} {
				t.Run(fmt.Sprintf("%s/%s/%s", provider, format, status), func(t *testing.T) {
					snapshot := fmt.Sprintf(`{"id":"interaction-action","status":%q,"steps":[{"type":"function_call","id":"call-action","name":"lookup","arguments":{"q":"x"}}],"usage":{"total_input_tokens":2,"total_output_tokens":3,"total_tokens":5}}`, status)
					wire := "event: interaction.created\ndata: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":\"interaction-action\"}}\n\n" +
						"event: step.start\ndata: {\"event_type\":\"step.start\",\"index\":0,\"step\":{\"type\":\"function_call\",\"id\":\"call-action\",\"name\":\"lookup\",\"arguments\":{}}}\n\n" +
						"event: step.delta\ndata: {\"event_type\":\"step.delta\",\"index\":0,\"delta\":{\"type\":\"arguments_delta\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}\n\n" +
						"event: step.stop\ndata: {\"event_type\":\"step.stop\",\"index\":0}\n\n" +
						"event: interaction.completed\ndata: {\"event_type\":\"interaction.completed\",\"interaction\":" + snapshot + "}\n\n"
					calls := 0
					ctx := util.WithRoundTripper(t.Context(), googlePayloadTransport(func(request *http.Request) (*http.Response, error) {
						calls++
						if !strings.HasSuffix(request.URL.Path, "/interactions") {
							t.Errorf("unexpected endpoint: %s", request.URL.Path)
						}
						return googlePayloadResponse(request, wire), nil
					}))
					var executor cliproxyauth.ProviderExecutor = NewGeminiInteractionsExecutor(&config.Config{})
					if provider == "vertex" {
						executor = NewGeminiVertexExecutor(&config.Config{})
					}
					result, err := executor.ExecuteStream(ctx, googlePayloadAuth(provider),
						cliproxyexecutor.Request{Model: "gemini-2.5-flash", Payload: []byte(`{"input":"lookup x","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]}`)},
						cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatInteractions, ResponseFormat: format, Stream: true})
					if err != nil {
						t.Fatal(err)
					}
					var output bytes.Buffer
					for chunk := range result.Chunks {
						output.Write(chunk.Payload)
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
					if err != nil || calls != 1 || !strings.Contains(output.String(), "lookup") {
						t.Fatalf("status=%s error=%v calls=%d output=%s", status, err, calls, output.String())
					}
				})
			}
		}
	}
}
