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

	internalcache "github.com/Pyrokine/CLIProxyAPI/v6/internal/cache"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
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
	} else if format == sdktranslator.FormatOpenAIResponse {
		payload = []byte(`{"model":"claude-sonnet-4-6","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`)
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

func TestAntigravityExecuteStreamCanceledAfterTerminalPreservesUsage(t *testing.T) {
	for _, format := range []sdktranslator.Format{sdktranslator.FormatAntigravity, sdktranslator.FormatOpenAI, sdktranslator.FormatOpenAIResponse} {
		for _, reason := range []string{"STOP", "MAX_TOKENS"} {
			t.Run(format.String()+"/"+reason, func(t *testing.T) {
				plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
				registerTestUsagePlugin(t, plugin)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				frame := []byte(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"` + reason + `"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}}` + "\n")
				body := &cancelAfterFirstLineBody{reader: bytes.NewReader(frame), ctx: ctx}
				stream := executeAntigravityTestStream(t, ctx, body, format)
				terminalReceived := false
				var streamErr error
				var syntheticDone bool
				completed := make(chan struct{})
				go func() {
					defer close(completed)
					for chunk := range stream.Chunks {
						if chunk.Err != nil {
							streamErr = chunk.Err
						}
						if isGeminiStreamTerminalChunk(chunk.Payload) {
							terminalReceived = true
							cancel()
						}
						if bytes.Equal(chunk.Payload, []byte("[DONE]")) {
							syntheticDone = true
						}
					}
				}()
				select {
				case <-completed:
				case <-time.After(2 * time.Second):
					t.Fatal("timed out waiting for canceled stream")
				}
				if streamErr != nil || syntheticDone {
					t.Fatalf("canceled stream error=%v synthetic done=%t", streamErr, syntheticDone)
				}
				if !terminalReceived {
					t.Fatal("terminal chunk was not delivered")
				}
				select {
				case record := <-plugin.records:
					if record.Failed || record.Detail.InputTokens != 3 || record.Detail.OutputTokens != 2 || record.Detail.TotalTokens != 5 {
						t.Fatalf("usage after terminal cancellation = %+v", record)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("timed out waiting for terminal usage")
				}
			})
		}
	}
}

func TestAntigravityExecuteStreamCanceledBeforeTerminalDeliveryFails(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	ctx, cancel := context.WithCancel(context.Background())
	body := io.NopCloser(strings.NewReader(
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}}` + "\n",
	))
	stream := executeAntigravityTestStream(t, ctx, body, sdktranslator.FormatAntigravity)
	cancel()
	for _, chunk := range collectAntigravityTestStream(t, stream) {
		if isGeminiStreamTerminalChunk(chunk.Payload) {
			t.Fatalf("canceled stream delivered a terminal payload: %s", chunk.Payload)
		}
	}
	select {
	case record := <-plugin.records:
		if !record.Failed {
			t.Fatalf("undelivered terminal was reported as success: %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled usage")
	}
}

func TestAntigravityExecuteStreamCanceledAfterTerminalCommitsReplay(t *testing.T) {
	internalcache.ClearAntigravityReasoningReplayCache()
	t.Cleanup(internalcache.ClearAntigravityReasoningReplayCache)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const model = "gemini-3.6-flash-high"
	body := &cancelAfterFirstLineBody{
		reader: bytes.NewReader([]byte(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok","thoughtSignature":"terminal-cancel-signature-123456"}]},"finishReason":"STOP"}]}}` + "\n")),
		ctx:    ctx,
	}
	request := []byte(`{"sessionId":"terminal-cancel-replay","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
	auth := &cliproxyauth.Auth{
		ID:         "auth-terminal-replay",
		Attributes: map[string]string{"base_url": "https://antigravity.test"},
		Metadata: map[string]any{
			"access_token": "token",
			"project_id":   "project-1",
			"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	ctx = util.WithRoundTripper(ctx, antigravityStreamRoundTripper{body: body})
	stream, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(
		ctx, auth, cliproxyexecutor.Request{Model: model, Payload: request},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
	)
	if err != nil {
		t.Fatal(err)
	}
	terminalReceived := false
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case chunk, ok := <-stream.Chunks:
			if !ok {
				if !terminalReceived {
					t.Fatal("terminal was not delivered")
				}
				items, exists := internalcache.GetAntigravityReasoningReplayItems(model, "session:terminal-cancel-replay")
				if !exists || len(items) == 0 {
					t.Fatalf("replay not committed after terminal cancellation: exists=%t items=%d", exists, len(items))
				}
				return
			}
			if chunk.Err != nil {
				t.Fatalf("terminal stream error = %v", chunk.Err)
			}
			if isGeminiStreamTerminalChunk(chunk.Payload) {
				terminalReceived = true
				cancel()
			}
		case <-timer.C:
			t.Fatal("timed out waiting for terminal replay settlement")
		}
	}
}

func TestAntigravityDeliveryAwareCancelBeforeTransportTerminalReportsFailure(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	baseCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := cliproxyexecutor.WithStreamDeliveryAcknowledgement(baseCtx)
	body := &cancelAfterFirstLineBody{
		reader: bytes.NewReader([]byte(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}}` + "\n")),
		ctx:    baseCtx,
	}
	stream := executeAntigravityTestStream(t, ctx, body, sdktranslator.FormatOpenAIResponse)
	terminalReceived := false
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for !terminalReceived {
		select {
		case chunk, ok := <-stream.Chunks:
			if !ok || chunk.Err != nil || chunk.Terminal {
				t.Fatalf("stream ended before translated protocol terminal: %#v", chunk)
			}
			terminalReceived = isGeminiStreamTerminalChunk(chunk.Payload)
		case <-timer.C:
			t.Fatal("timed out waiting for translated protocol terminal")
		}
	}
	cancel()
	chunks := collectAntigravityTestStream(t, stream)
	for _, chunk := range chunks {
		if bytes.Contains(chunk.Payload, []byte("[DONE]")) {
			t.Fatalf("client cancellation fabricated transport completion: %#v", chunk)
		}
		if chunk.Terminal {
			if chunk.Acknowledge == nil || len(chunk.Payload) != 0 {
				t.Fatalf("invalid terminal delivery notification: %#v", chunk)
			}
			// Terminal 请求交付确认，取消后的交付必须报告失败
			chunk.Acknowledge(context.Canceled)
		}
	}
	select {
	case record := <-plugin.records:
		if !record.Failed || record.Detail.InputTokens != 3 || record.Detail.OutputTokens != 2 {
			t.Fatalf("undelivered terminal usage = %+v, want failed accounting", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
}

func TestAntigravityReplayCommitsOnlyAfterTerminalDelivery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ackErr  error
		commits bool
	}{
		{name: "delivered", commits: true},
		{name: "downstream failure", ackErr: errors.New("downstream write failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			internalcache.ClearAntigravityReasoningReplayCache()
			t.Cleanup(internalcache.ClearAntigravityReasoningReplayCache)
			const model = "gemini-3.6-flash-high"
			const session = "session:terminal-delivery-replay"
			request := []byte(`{"sessionId":"terminal-delivery-replay","request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)
			body := io.NopCloser(strings.NewReader(
				`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok","thoughtSignature":"terminal-delivery-signature-123456"}]},"finishReason":"STOP"}]}}` + "\n",
			))
			auth := &cliproxyauth.Auth{
				ID: "auth-terminal-delivery", Attributes: map[string]string{"base_url": "https://antigravity.test"},
				Metadata: map[string]any{
					"access_token": "token", "project_id": "project-1",
					"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			}
			ctx := cliproxyexecutor.WithStreamDeliveryAcknowledgement(context.Background())
			ctx = util.WithRoundTripper(ctx, antigravityStreamRoundTripper{body: body})
			stream, err := NewAntigravityExecutor(&config.Config{}).ExecuteStream(
				ctx, auth, cliproxyexecutor.Request{Model: model, Payload: request},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
			)
			if err != nil {
				t.Fatal(err)
			}
			var terminal cliproxyexecutor.StreamChunk
			for _, chunk := range collectAntigravityTestStream(t, stream) {
				if chunk.Err != nil {
					t.Fatal(chunk.Err)
				}
				if chunk.Terminal {
					terminal = chunk
				}
			}
			if !terminal.Terminal || terminal.Acknowledge == nil {
				t.Fatal("delivery-aware executor did not provide a terminal acknowledgement")
			}
			if _, exists := internalcache.GetAntigravityReasoningReplayItems(model, session); exists {
				t.Fatal("replay committed before downstream terminal delivery")
			}
			terminal.Acknowledge(tc.ackErr)
			items, exists := internalcache.GetAntigravityReasoningReplayItems(model, session)
			if exists != tc.commits || tc.commits && len(items) == 0 {
				t.Fatalf("replay after acknowledgement: exists=%t items=%d, want commit=%t", exists, len(items), tc.commits)
			}
		})
	}
}

func TestAntigravityExecuteStreamAccountsForFilteredUsage(t *testing.T) {
	for _, tt := range []struct {
		name       string
		terminal   string
		wantFailed bool
	}{
		{name: "completed", terminal: `data: {"response":{"candidates":[{"finishReason":"STOP"}]}}` + "\n\n"},
		{name: "incomplete", wantFailed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
			registerTestUsagePlugin(t, plugin)
			body := io.NopCloser(strings.NewReader(
				`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":2,"totalTokenCount":11}}}` + "\n\n" + tt.terminal,
			))
			chunks := collectAntigravityTestStream(t, executeAntigravityTestStream(t, context.Background(), body, sdktranslator.FormatAntigravity))
			if len(chunks) == 0 || gjson.GetBytes(chunks[0].Payload, "response.usageMetadata").Exists() {
				t.Fatalf("nonterminal client chunk leaked usage: %#v", chunks)
			}
			select {
			case record := <-plugin.records:
				if record.Detail.InputTokens != 9 || record.Detail.OutputTokens != 2 || record.Detail.TotalTokens != 11 || record.Failed != tt.wantFailed {
					t.Fatalf("usage = %+v, want input=9 output=2 total=11 failed=%t", record, tt.wantFailed)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for usage record")
			}
		})
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

func TestAntigravityExecuteStreamReadErrorAfterTerminalStillFails(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
	registerTestUsagePlugin(t, plugin)
	readErr := errors.New("late upstream read failed")
	body := &errorAfterFirstLineBody{
		reader: bytes.NewReader([]byte(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}}` + "\n")),
		err:    readErr,
	}
	chunks := collectAntigravityTestStream(t, executeAntigravityTestStream(t, context.Background(), body, sdktranslator.FormatAntigravity))
	if len(chunks) != 2 || !isGeminiStreamTerminalChunk(chunks[0].Payload) || !errors.Is(chunks[1].Err, readErr) {
		t.Fatalf("terminal followed by transport read error = %#v", chunks)
	}
	for _, chunk := range chunks {
		if bytes.Equal(chunk.Payload, []byte("[DONE]")) {
			t.Fatal("read-error stream emitted synthetic done")
		}
	}
	select {
	case record := <-plugin.records:
		if !record.Failed || record.Detail.InputTokens != 3 || record.Detail.OutputTokens != 2 {
			t.Fatalf("read-error usage = %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failed usage record")
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

func TestAntigravityExecuteStreamReportsTrailingBackendError(t *testing.T) {
	body := io.NopCloser(strings.NewReader(
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}}` + "\n\n" +
			`data: {"error":{"code":503,"message":"backend capacity exhausted"}}` + "\n\n",
	))
	chunks := collectAntigravityTestStream(t, executeAntigravityTestStream(
		t, context.Background(), body, sdktranslator.FormatOpenAI,
	))
	if len(chunks) != 2 || chunks[0].Err != nil || chunks[1].Err == nil {
		t.Fatalf("trailing backend error chunks = %#v", chunks)
	}
	var status statusErr
	if !errors.As(chunks[1].Err, &status) || status.code != http.StatusServiceUnavailable ||
		!strings.Contains(status.Error(), "backend capacity exhausted") {
		t.Fatalf("trailing backend error = %v, want HTTP 503 and original reason", chunks[1].Err)
	}
	for _, chunk := range chunks {
		if bytes.Contains(chunk.Payload, []byte("[DONE]")) {
			t.Fatalf("failed stream emitted done: %s", chunk.Payload)
		}
	}
}

func TestAntigravityExecuteStreamAssemblesMultilineJSON(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\n" +
		"data: \"response\": {\"candidates\": [{\"content\": {\"role\": \"model\", \"parts\": [{\"text\": \"ok\"}]}, \"finishReason\": \"STOP\"}], \"usageMetadata\": {\"promptTokenCount\": 3, \"candidatesTokenCount\": 2, \"totalTokenCount\": 5}}\n" +
		"data: }\n\n"))
	chunks := collectAntigravityTestStream(t, executeAntigravityTestStream(
		t, context.Background(), body, sdktranslator.FormatAntigravity,
	))
	if len(chunks) != 2 || chunks[0].Err != nil || chunks[1].Err != nil ||
		!bytes.Equal(chunks[1].Payload, []byte("[DONE]")) {
		t.Fatalf("multiline stream chunks = %#v, want response and done", chunks)
	}
	if got := gjson.GetBytes(chunks[0].Payload, "response.candidates.0.content.parts.0.text").String(); got != "ok" {
		t.Fatalf("assembled response text = %q; payload=%s", got, chunks[0].Payload)
	}
	if got := gjson.GetBytes(chunks[0].Payload, "response.usageMetadata.totalTokenCount").Int(); got != 5 {
		t.Fatalf("assembled terminal usage = %d; payload=%s", got, chunks[0].Payload)
	}
}

func TestAntigravityExecuteStreamReportsMultilineBackendError(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "raw JSON", body: "{\n  \"error\": {\n    \"code\": 503,\n    \"message\": \"backend capacity exhausted\"\n  }\n}\n"},
		{name: "multiple SSE data lines", body: "data: {\n" +
			"data: \"error\": {\"code\": 503, \"message\": \"backend capacity exhausted\"}\n" +
			"data: }\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := io.NopCloser(strings.NewReader(
				`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}}` + "\n\n" + tc.body,
			))
			chunks := collectAntigravityTestStream(t, executeAntigravityTestStream(
				t, context.Background(), body, sdktranslator.FormatOpenAI,
			))
			if len(chunks) != 2 || chunks[0].Err != nil {
				t.Fatalf("multiline backend error stream chunks = %#v, want partial response and error", chunks)
			}
			var status statusErr
			if !errors.As(chunks[1].Err, &status) || status.code != http.StatusServiceUnavailable ||
				!strings.Contains(status.Error(), "backend capacity exhausted") {
				t.Fatalf("multiline backend error = %v, want HTTP 503 and original reason", chunks[1].Err)
			}
			for _, chunk := range chunks {
				if bytes.Contains(chunk.Payload, []byte("[DONE]")) {
					t.Fatalf("failed stream emitted done: %s", chunk.Payload)
				}
			}
		})
	}
}

func TestAntigravityExecuteReportsTrailingBackendError(t *testing.T) {
	body := io.NopCloser(strings.NewReader(
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}}` + "\n\n" +
			`data: {"error":{"code":503,"message":"backend capacity exhausted"}}` + "\n\n",
	))
	ctx := util.WithRoundTripper(context.Background(), antigravityStreamRoundTripper{body: body})
	auth := &cliproxyauth.Auth{
		ID:         "auth-nonstream-trailing-error",
		Attributes: map[string]string{"base_url": "https://antigravity.test"},
		Metadata: map[string]any{
			"access_token": "token", "project_id": "project-1",
			"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	_, err := NewAntigravityExecutor(&config.Config{}).Execute(ctx, auth,
		cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
	)
	var status statusErr
	if !errors.As(err, &status) || status.code != http.StatusServiceUnavailable ||
		!strings.Contains(status.Error(), "backend capacity exhausted") {
		t.Fatalf("non-stream trailing backend error = %v, want HTTP 503 with original reason", err)
	}
}

func TestAntigravityExecuteReportsMultilineBackendError(t *testing.T) {
	body := io.NopCloser(strings.NewReader(
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}]}}` + "\n\n" +
			"{\n  \"error\": {\n    \"code\": 503,\n    \"message\": \"backend capacity exhausted\"\n  }\n}\n",
	))
	ctx := util.WithRoundTripper(context.Background(), antigravityStreamRoundTripper{body: body})
	auth := &cliproxyauth.Auth{
		ID:         "auth-nonstream-multiline-error",
		Attributes: map[string]string{"base_url": "https://antigravity.test"},
		Metadata: map[string]any{
			"access_token": "token", "project_id": "project-1",
			"expired": time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	_, err := NewAntigravityExecutor(&config.Config{}).Execute(ctx, auth,
		cliproxyexecutor.Request{Model: "claude-sonnet-4-6", Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}}`)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatAntigravity},
	)
	var status statusErr
	if !errors.As(err, &status) || status.code != http.StatusServiceUnavailable ||
		!strings.Contains(status.Error(), "backend capacity exhausted") {
		t.Fatalf("non-stream multiline backend error = %v, want HTTP 503 with original reason", err)
	}
}

func TestAntigravityExecuteStreamRejectsTruncatedTrailingJSON(t *testing.T) {
	body := io.NopCloser(strings.NewReader(
		`data: {"response":{"candidates":[{"finishReason":"STOP"}]}}` + "\n\n" +
			"data: {\"error\": {\"code\": 503\n",
	))
	chunks := collectAntigravityTestStream(t, executeAntigravityTestStream(
		t, context.Background(), body, sdktranslator.FormatAntigravity,
	))
	if len(chunks) != 2 || chunks[0].Err != nil || chunks[1].Err == nil {
		t.Fatalf("truncated trailing JSON chunks = %#v, want terminal payload and error", chunks)
	}
	var status statusErr
	if !errors.As(chunks[1].Err, &status) || status.code != http.StatusBadGateway {
		t.Fatalf("truncated trailing JSON error = %v, want HTTP 502", chunks[1].Err)
	}
	for _, chunk := range chunks {
		if bytes.Contains(chunk.Payload, []byte("[DONE]")) {
			t.Fatalf("truncated stream emitted done: %s", chunk.Payload)
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

func TestAntigravityStreamUsagePreservesDeliveryFailureCause(t *testing.T) {
	for _, failure := range []error{io.ErrClosedPipe, io.ErrShortWrite, context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			plugin := &captureUsagePlugin{records: make(chan usage.Record, 2)}
			registerTestUsagePlugin(t, plugin)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			body := &cancelAfterFirstLineBody{
				reader: bytes.NewReader([]byte(`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}}` + "\n")),
				ctx:    ctx,
			}
			stream := executeAntigravityTestStream(t, ctx, body, sdktranslator.FormatAntigravity)
			select {
			case chunk, ok := <-stream.Chunks:
				if !ok || chunk.Err != nil || len(chunk.Payload) == 0 {
					t.Fatalf("initial payload = %#v, open = %v", chunk, ok)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("missing initial payload")
			}
			cancel(failure)
			collectAntigravityTestStream(t, stream)
			select {
			case record := <-plugin.records:
				if !record.Failed || record.Fail.Body != failure.Error() {
					t.Fatalf("delivery failure = %#v, want %v", record.Fail, failure)
				}
				if record.Detail.InputTokens != 3 || record.Detail.OutputTokens != 2 || record.Detail.TotalTokens != 5 {
					t.Fatalf("partial usage = %#v", record.Detail)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("missing failed usage record")
			}
		})
	}
}
