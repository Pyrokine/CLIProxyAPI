package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

type cancelAfterFirstLineBody struct {
	reader *bytes.Reader
	ctx    context.Context
}

func (b *cancelAfterFirstLineBody) Read(p []byte) (int, error) {
	if b.reader.Len() > 0 {
		return b.reader.Read(p)
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *cancelAfterFirstLineBody) Close() error { return nil }

type errorAfterFirstLineBody struct {
	reader *bytes.Reader
	err    error
}

func (b *errorAfterFirstLineBody) Read(p []byte) (int, error) {
	if b.reader.Len() > 0 {
		return b.reader.Read(p)
	}
	return 0, b.err
}

func (b *errorAfterFirstLineBody) Close() error { return nil }

type cancelThenEOFBody struct {
	reader  *strings.Reader
	ctx     context.Context
	started chan struct{}
	once    sync.Once
}

func (b *cancelThenEOFBody) Read(p []byte) (int, error) {
	if b.reader.Len() > 0 {
		return b.reader.Read(p)
	}
	b.once.Do(func() { close(b.started) })
	<-b.ctx.Done()
	return 0, io.EOF
}

func (b *cancelThenEOFBody) Close() error { return nil }

type antigravityStreamRoundTripper struct {
	body io.ReadCloser
}

func (rt antigravityStreamRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       rt.body,
		Request:    req,
	}, nil
}

func executeAntigravityTestStream(
	t *testing.T,
	ctx context.Context,
	body io.ReadCloser,
	format sdktranslator.Format,
) *cliproxyexecutor.StreamResult {
	t.Helper()

	payload := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
	if format == sdktranslator.FormatOpenAI {
		payload = []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	}
	auth := &cliproxyauth.Auth{
		ID: "auth-stream-terminal",
		Attributes: map[string]string{
			"base_url": "https://antigravity.test",
		},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	ctx = util.WithRoundTripper(ctx, antigravityStreamRoundTripper{body: body})
	stream, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(
		ctx,
		auth,
		cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: payload},
		cliproxyexecutor.Options{SourceFormat: format},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error = %v", err)
	}
	return stream
}

func collectAntigravityTestStream(
	t *testing.T,
	stream *cliproxyexecutor.StreamResult,
) []cliproxyexecutor.StreamChunk {
	t.Helper()

	var chunks []cliproxyexecutor.StreamChunk
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case chunk, ok := <-stream.Chunks:
			if !ok {
				return chunks
			}
			chunks = append(chunks, chunk)
		case <-timer.C:
			t.Fatal("timed out waiting for Antigravity stream to close")
		}
	}
}

func TestSendAntigravityStreamReadErrorReplacesPendingChunkAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := make(chan cliproxyexecutor.StreamChunk, 1)
	out <- cliproxyexecutor.StreamChunk{Payload: []byte("pending")}

	sendAntigravityStreamReadError(ctx, out, context.Canceled)
	chunk := <-out
	if chunk.Err != context.Canceled || len(chunk.Payload) != 0 {
		t.Fatalf("replacement chunk = %#v, want exact cancellation error", chunk)
	}
}

func TestAntigravityExecutePreservesExactCancellation(t *testing.T) {
	started := make(chan struct{})
	roundTripper := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(util.WithRoundTripper(context.Background(), roundTripper))
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := NewAntigravityExecutor(&config.Config{}).Execute(
			ctx,
			&cliproxyauth.Auth{
				ID:         "auth-request-cancel",
				Attributes: map[string]string{"base_url": "https://antigravity.test"},
				Metadata: map[string]any{
					"access_token": "token",
					"project_id":   "project-1",
					"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			},
			cliproxyexecutor.Request{
				Model:   "gemini-2.5-flash",
				Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
			},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
		)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Antigravity request did not start")
	}
	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("Execute error = %#v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled Antigravity request did not return")
	}
}

func TestAntigravityExecutePreservesCancellationDuringRetryDelay(t *testing.T) {
	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bodyRead := make(chan struct{})
	body := &cancelThenEOFBody{
		reader:  strings.NewReader(`{"error":"no capacity available"}`),
		ctx:     baseCtx,
		started: bodyRead,
	}
	roundTripper := antigravityRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     make(http.Header),
			Body:       body,
			Request:    req,
		}, nil
	})
	ctx := util.WithRoundTripper(baseCtx, roundTripper)
	result := make(chan error, 1)
	go func() {
		_, err := NewAntigravityExecutor(&config.Config{RequestRetry: 1}).Execute(
			ctx,
			&cliproxyauth.Auth{
				ID:         "auth-retry-cancel",
				Attributes: map[string]string{"base_url": "https://antigravity.test"},
				Metadata: map[string]any{
					"access_token": "token",
					"project_id":   "project-1",
					"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			},
			cliproxyexecutor.Request{
				Model:   "gemini-2.5-flash",
				Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`),
			},
			cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
		)
		result <- err
	}()
	select {
	case <-bodyRead:
	case <-time.After(time.Second):
		t.Fatal("Antigravity retry response body was not read")
	}
	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("Execute retry error = %#v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled Antigravity retry delay did not return")
	}
}

func TestAntigravityExecuteStreamPreservesContextCancellationError(t *testing.T) {
	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &cancelAfterFirstLineBody{
		reader: bytes.NewReader([]byte(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}]}}` + "\n")),
		ctx:    baseCtx,
	}
	stream := executeAntigravityTestStream(t, baseCtx, body, sdktranslator.FormatAntigravity)

	select {
	case chunk, ok := <-stream.Chunks:
		if !ok {
			t.Fatal("stream closed before first payload")
		}
		if chunk.Err != nil {
			t.Fatalf("first chunk error = %v", chunk.Err)
		}
		if len(chunk.Payload) == 0 {
			t.Fatal("first chunk payload is empty")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first payload")
	}
	cancel()

	select {
	case chunk, ok := <-stream.Chunks:
		if !ok {
			t.Fatal("stream closed without cancellation error")
		}
		if chunk.Err != context.Canceled {
			t.Fatalf("terminal error = %#v, want context.Canceled", chunk.Err)
		}
		if bytes.Contains(chunk.Payload, []byte("[DONE]")) {
			t.Fatalf("canceled stream emitted synthetic done payload: %s", chunk.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancellation error")
	}
	select {
	case _, ok := <-stream.Chunks:
		if ok {
			t.Fatal("stream remained open after cancellation error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled stream to close")
	}
}

func TestAntigravityExecuteStreamPreservesTerminalMetadataAndEmitsDone(t *testing.T) {
	body := io.NopCloser(strings.NewReader(
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-3.7-flash","responseId":"response-123"}}` + "\n\n",
	))
	chunks := collectAntigravityTestStream(
		t,
		executeAntigravityTestStream(t, context.Background(), body, sdktranslator.FormatAntigravity),
	)
	if len(chunks) != 2 {
		t.Fatalf("stream chunk count = %d, want terminal payload plus done", len(chunks))
	}
	if chunks[0].Err != nil {
		t.Fatalf("terminal payload error = %v", chunks[0].Err)
	}
	terminal := chunks[0].Payload
	if got := gjson.GetBytes(terminal, "response.candidates.0.finishReason").String(); got != "STOP" {
		t.Fatalf("finishReason = %q, want STOP; payload=%s", got, terminal)
	}
	if got := gjson.GetBytes(terminal, "response.usageMetadata.totalTokenCount").Int(); got != 5 {
		t.Fatalf("totalTokenCount = %d, want 5; payload=%s", got, terminal)
	}
	if got := gjson.GetBytes(terminal, "response.modelVersion").String(); got != "gemini-3.7-flash" {
		t.Fatalf("modelVersion = %q; payload=%s", got, terminal)
	}
	if got := gjson.GetBytes(terminal, "response.responseId").String(); got != "response-123" {
		t.Fatalf("responseId = %q; payload=%s", got, terminal)
	}
	if chunks[1].Err != nil || !bytes.Equal(chunks[1].Payload, []byte("[DONE]")) {
		t.Fatalf("done chunk = %#v, want [DONE]", chunks[1])
	}
}

func TestAntigravityExecuteStreamPropagatesScannerErrorWithoutDone(t *testing.T) {
	sentinel := errors.New("stream read failed")
	body := &errorAfterFirstLineBody{
		reader: bytes.NewReader([]byte(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}}` + "\n")),
		err:    sentinel,
	}
	chunks := collectAntigravityTestStream(
		t,
		executeAntigravityTestStream(t, context.Background(), body, sdktranslator.FormatAntigravity),
	)
	if len(chunks) != 2 {
		t.Fatalf("stream chunk count = %d, want payload plus scanner error", len(chunks))
	}
	if chunks[0].Err != nil || len(chunks[0].Payload) == 0 {
		t.Fatalf("first chunk = %#v, want payload", chunks[0])
	}
	terminalErr := chunks[1].Err
	if !errors.Is(terminalErr, sentinel) {
		t.Fatalf("terminal error = %#v, want wrapped sentinel", terminalErr)
	}
	var dispositionErr cliproxyexecutor.ExecutionDispositionError
	if !errors.As(terminalErr, &dispositionErr) ||
		dispositionErr.ExecutionDisposition().DispatchPhase != cliproxyexecutor.DispatchPhaseResponseStarted {
		t.Fatalf("terminal error disposition = %#v, want response started", terminalErr)
	}
	for _, chunk := range chunks {
		if bytes.Contains(chunk.Payload, []byte("[DONE]")) {
			t.Fatalf("scanner error stream emitted synthetic done payload: %s", chunk.Payload)
		}
	}
}

func TestAntigravityExecuteStreamRejectsEOFWithoutTerminalEvent(t *testing.T) {
	body := io.NopCloser(strings.NewReader(
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}}` + "\n\n",
	))
	chunks := collectAntigravityTestStream(
		t,
		executeAntigravityTestStream(t, context.Background(), body, sdktranslator.FormatAntigravity),
	)
	if len(chunks) != 2 {
		t.Fatalf("stream chunk count = %d, want payload plus incomplete error", len(chunks))
	}
	var status statusErr
	if !errors.As(chunks[1].Err, &status) || status.code != http.StatusBadGateway {
		t.Fatalf("terminal error = %#v, want status 502", chunks[1].Err)
	}
	for _, chunk := range chunks {
		if bytes.Contains(chunk.Payload, []byte("[DONE]")) {
			t.Fatalf("incomplete stream emitted synthetic done payload: %s", chunk.Payload)
		}
	}
}

func TestAntigravityExecuteStreamUsesToolCallFinishReason(t *testing.T) {
	body := io.NopCloser(strings.NewReader(
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"query":"hello"}}}]}}]}}` + "\n" +
			`data: {"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-3.7-flash","responseId":"response-tool"}}` + "\n\n",
	))
	chunks := collectAntigravityTestStream(
		t,
		executeAntigravityTestStream(t, context.Background(), body, sdktranslator.FormatOpenAI),
	)
	if len(chunks) != 2 {
		t.Fatalf("stream chunk count = %d, want tool call and terminal chunk", len(chunks))
	}
	for _, chunk := range chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}
	if got := gjson.GetBytes(chunks[0].Payload, "choices.0.delta.tool_calls.0.function.name").String(); got != "lookup" {
		t.Fatalf("tool call name = %q; payload=%s", got, chunks[0].Payload)
	}
	terminal := chunks[1].Payload
	if got := gjson.GetBytes(terminal, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls; payload=%s", got, terminal)
	}
	if got := gjson.GetBytes(terminal, "choices.0.native_finish_reason").String(); got != "stop" {
		t.Fatalf("native_finish_reason = %q, want stop; payload=%s", got, terminal)
	}
}
