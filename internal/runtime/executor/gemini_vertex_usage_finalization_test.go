package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

type vertexUsageRoundTripFunc func(*http.Request) (*http.Response, error)

func (f vertexUsageRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestVertexExecuteUsesExplicitDynamicHeaderSource(t *testing.T) {
	var upstreamHeaders http.Header
	ctx := util.WithSessionID(context.Background(), "vertex-session")
	ctx = util.WithRoundTripper(ctx, vertexUsageRoundTripFunc(func(req *http.Request) (*http.Response, error) {
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

	exec := &GeminiVertexExecutor{}
	_, err := exec.Execute(
		ctx,
		&cliproxyauth.Auth{
			Provider: "vertex",
			Attributes: map[string]string{
				"api_key":              "vertex-key",
				"base_url":             "https://vertex.example.com",
				"header:X-Copied":      "$x-client-value",
				"header:X-CPA-Session": "$CPA-SESSION-ID",
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
			Headers:        http.Header{"x-ClIeNt-VaLuE": []string{"from-options"}},
		},
	)
	if err != nil {
		t.Fatalf("Execute error = %v", err)
	}
	if got := upstreamHeaders.Get("X-Copied"); got != "from-options" {
		t.Fatalf("X-Copied = %q, want from-options", got)
	}
	if got := upstreamHeaders.Get("X-CPA-Session"); got != "vertex-session" {
		t.Fatalf("X-CPA-Session = %q, want vertex-session", got)
	}
	if got := upstreamHeaders.Get("x-goog-api-key"); got != "vertex-key" {
		t.Fatalf("x-goog-api-key = %q, want vertex-key", got)
	}
}

func TestVertexStreamPreservesNonZeroUsageAfterZeroPlaceholder(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	ctx := usage.WithRequestedModelAlias(context.Background(), "vertex-nonzero-usage-finalizer")
	reporter := newUsageReporter(ctx, "vertex", "gemini-test", nil)
	client := &http.Client{Transport: vertexUsageRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				"data: {\"candidates\":[],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":7,\"thoughtsTokenCount\":3,\"totalTokenCount\":15}}\n" +
					"data: {\"candidates\":[],\"usageMetadata\":{\"promptTokenCount\":0,\"candidatesTokenCount\":0,\"thoughtsTokenCount\":0,\"totalTokenCount\":0}}\n",
			)),
		}, nil
	})}
	httpReq, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com/stream", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}

	exec := &GeminiVertexExecutor{}
	stream, errStream := exec.handleVertexStreamResponse(
		ctx,
		client,
		httpReq,
		sdktranslator.FormatGemini,
		sdktranslator.FormatGemini,
		cliproxyexecutor.Request{Model: "gemini-test"},
		cliproxyexecutor.Options{},
		nil,
		reporter,
	)
	if errStream != nil {
		t.Fatal(errStream)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}

	select {
	case record := <-plugin.records:
		if record.Alias != "vertex-nonzero-usage-finalizer" {
			t.Fatalf("usage alias = %q", record.Alias)
		}
		if record.Failed {
			t.Fatalf("usage record = %#v, want success", record)
		}
		if record.Detail.InputTokens != 5 || record.Detail.OutputTokens != 7 ||
			record.Detail.ReasoningTokens != 3 || record.Detail.TotalTokens != 15 {
			t.Fatalf("usage detail = %+v", record.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for usage record")
	}
}

func TestVertexStreamCancellationWinsOverBufferedUsage(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	baseCtx := usage.WithRequestedModelAlias(context.Background(), "vertex-cancel-finalizer")
	ctx, cancel := context.WithCancel(baseCtx)
	reporter := newUsageReporter(ctx, "vertex", "gemini-test", nil)
	client := &http.Client{Transport: vertexUsageRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				"data: {\"candidates\":[],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":4,\"totalTokenCount\":7}}\n",
			)),
		}, nil
	})}
	httpReq, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com/stream", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}

	exec := &GeminiVertexExecutor{}
	stream, errStream := exec.handleVertexStreamResponse(
		ctx,
		client,
		httpReq,
		sdktranslator.FormatGemini,
		sdktranslator.FormatGemini,
		cliproxyexecutor.Request{Model: "gemini-test"},
		cliproxyexecutor.Options{},
		nil,
		reporter,
	)
	if errStream != nil {
		t.Fatal(errStream)
	}
	cancel()
	for range stream.Chunks {
	}

	select {
	case record := <-plugin.records:
		if record.Alias != "vertex-cancel-finalizer" {
			t.Fatalf("usage alias = %q", record.Alias)
		}
		if !record.Failed || record.Fail.StatusCode != 499 {
			t.Fatalf("usage record = %#v, want canceled failure", record)
		}
		if record.Detail.InputTokens != 0 || record.Detail.OutputTokens != 0 || record.Detail.TotalTokens != 0 {
			t.Fatalf("failure usage detail = %+v, want zero", record.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for canceled usage record")
	}
	select {
	case record := <-plugin.records:
		if record.Alias == "vertex-cancel-finalizer" {
			t.Fatalf("received duplicate usage record: %#v", record)
		}
	case <-time.After(20 * time.Millisecond):
	}
}

func TestVertexStreamReadErrorWinsOverBufferedUsage(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	ctx := usage.WithRequestedModelAlias(context.Background(), "vertex-read-error-finalizer")
	reporter := newUsageReporter(ctx, "vertex", "gemini-test", nil)
	client := &http.Client{Transport: vertexUsageRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(&terminalErrorReader{
				data: []byte("data: {\"candidates\":[],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":4,\"totalTokenCount\":7}}\n"),
				err:  io.ErrUnexpectedEOF,
			}),
		}, nil
	})}
	httpReq, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com/stream", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}

	exec := &GeminiVertexExecutor{}
	stream, errStream := exec.handleVertexStreamResponse(
		ctx,
		client,
		httpReq,
		sdktranslator.FormatGemini,
		sdktranslator.FormatGemini,
		cliproxyexecutor.Request{Model: "gemini-test"},
		cliproxyexecutor.Options{},
		nil,
		reporter,
	)
	if errStream != nil {
		t.Fatal(errStream)
	}
	var output strings.Builder
	var streamErr error
	for chunk := range stream.Chunks {
		output.Write(chunk.Payload)
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if !errors.Is(streamErr, io.ErrUnexpectedEOF) {
		t.Fatalf("stream error = %v, want unexpected EOF", streamErr)
	}
	if strings.Contains(output.String(), "[DONE]") {
		t.Fatalf("read-error stream emitted terminal marker before error: %q", output.String())
	}

	select {
	case record := <-plugin.records:
		if record.Alias != "vertex-read-error-finalizer" {
			t.Fatalf("usage alias = %q", record.Alias)
		}
		if !record.Failed || !strings.Contains(record.Fail.Body, io.ErrUnexpectedEOF.Error()) {
			t.Fatalf("usage record = %#v, want read failure", record)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
	select {
	case record := <-plugin.records:
		if record.Alias == "vertex-read-error-finalizer" {
			t.Fatalf("received duplicate usage record: %#v", record)
		}
	case <-time.After(20 * time.Millisecond):
	}
}

func TestVertexStreamPublishesSuccessfulZeroUsageOnce(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	ctx := usage.WithRequestedModelAlias(context.Background(), "vertex-zero-usage-finalizer")
	reporter := newUsageReporter(ctx, "vertex", "gemini-test", nil)
	client := &http.Client{Transport: vertexUsageRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("data: {\"candidates\":[]}\n")),
		}, nil
	})}
	httpReq, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com/stream", nil)
	if errRequest != nil {
		t.Fatal(errRequest)
	}

	exec := &GeminiVertexExecutor{}
	stream, errStream := exec.handleVertexStreamResponse(
		ctx,
		client,
		httpReq,
		sdktranslator.FromString("gemini"),
		sdktranslator.FromString("gemini"),
		cliproxyexecutor.Request{Model: "gemini-test"},
		cliproxyexecutor.Options{},
		nil,
		reporter,
	)
	if errStream != nil {
		t.Fatal(errStream)
	}
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}

	select {
	case record := <-plugin.records:
		if record.Alias != "vertex-zero-usage-finalizer" {
			t.Fatalf("usage alias = %q", record.Alias)
		}
		if record.Failed {
			t.Fatalf("usage record = %#v, want success", record)
		}
		if record.Detail.InputTokens != 0 || record.Detail.OutputTokens != 0 || record.Detail.TotalTokens != 0 {
			t.Fatalf("usage detail = %+v, want zero usage", record.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for zero-usage record")
	}
	select {
	case record := <-plugin.records:
		if record.Alias == "vertex-zero-usage-finalizer" {
			t.Fatalf("received duplicate usage record: %#v", record)
		}
	case <-time.After(20 * time.Millisecond):
	}
}

func TestPrepareVertexGeminiBodyNormalizesBoundaryTurns(t *testing.T) {
	executor := NewGeminiVertexExecutor(nil)
	prepared, errPrepare := executor.prepareVertexGeminiBody(
		context.Background(),
		cliproxyexecutor.Request{
			Model:   "gemini-2.5-pro",
			Payload: []byte(`{"contents":[{"role":"model","parts":[{"text":"answer"}]}]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
		"gemini-2.5-pro",
		false,
	)
	if errPrepare != nil {
		t.Fatalf("prepareVertexGeminiBody() error = %v", errPrepare)
	}
	assertGeminiBoundaryRoles(t, prepared.body, []string{"user", "model", "user"})
}

func TestPrepareVertexGeminiBodyCountTokensPreservesTrailingModelTurn(t *testing.T) {
	executor := NewGeminiVertexExecutor(nil)
	prepared, errPrepare := executor.prepareVertexGeminiBody(
		context.Background(),
		cliproxyexecutor.Request{
			Model:    "gemini-2.5-pro",
			Payload:  []byte(`{"contents":[{"role":"model","parts":[{"text":"answer"}]}]}`),
			Metadata: map[string]any{"action": "countTokens"},
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini},
		"gemini-2.5-pro",
		false,
	)
	if errPrepare != nil {
		t.Fatalf("prepareVertexGeminiBody() error = %v", errPrepare)
	}
	assertGeminiBoundaryRoles(t, prepared.body, []string{"user", "model"})
}
