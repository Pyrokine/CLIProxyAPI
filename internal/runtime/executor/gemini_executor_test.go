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
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

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
