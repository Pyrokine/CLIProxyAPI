package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator/builtin"
)

func TestReadCodexResponseBodyRequiresEOFWithinLimit(t *testing.T) {
	const limit = 64
	withinLimit := strings.Repeat("a", limit)
	data, err := readCodexResponseBody(strings.NewReader(withinLimit), limit)
	if err != nil {
		t.Fatalf("readCodexResponseBody exact limit error = %v", err)
	}
	if string(data) != withinLimit {
		t.Fatalf("readCodexResponseBody data length = %d, want %d", len(data), limit)
	}

	completedThenFailure := strings.Repeat("b", limit) + "x"
	_, err = readCodexResponseBody(strings.NewReader(completedThenFailure), limit)
	if err == nil || !strings.Contains(err.Error(), "exceeds maximum allowed size") {
		t.Fatalf("readCodexResponseBody oversized error = %v", err)
	}
}

func TestParseCodexRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	t.Run(
		"resets_in_seconds", func(t *testing.T) {
			body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":123}}`)
			retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
			if retryAfter == nil {
				t.Fatalf("expected retryAfter, got nil")
			}
			if *retryAfter != 123*time.Second {
				t.Fatalf("retryAfter = %v, want %v", *retryAfter, 123*time.Second)
			}
		},
	)

	t.Run(
		"prefers resets_at", func(t *testing.T) {
			resetAt := now.Add(5 * time.Minute).Unix()
			body := []byte(
				`{"error":{"type":"usage_limit_reached","resets_at":` + itoa(resetAt) + `,"resets_in_seconds":1}}`,
			)
			retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
			if retryAfter == nil {
				t.Fatalf("expected retryAfter, got nil")
			}
			if *retryAfter != 5*time.Minute {
				t.Fatalf("retryAfter = %v, want %v", *retryAfter, 5*time.Minute)
			}
		},
	)

	t.Run(
		"fallback when resets_at is past", func(t *testing.T) {
			resetAt := now.Add(-1 * time.Minute).Unix()
			body := []byte(
				`{"error":{"type":"usage_limit_reached","resets_at":` + itoa(resetAt) + `,"resets_in_seconds":77}}`,
			)
			retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
			if retryAfter == nil {
				t.Fatalf("expected retryAfter, got nil")
			}
			if *retryAfter != 77*time.Second {
				t.Fatalf("retryAfter = %v, want %v", *retryAfter, 77*time.Second)
			}
		},
	)

	t.Run(
		"non-429 status code", func(t *testing.T) {
			body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":30}}`)
			if got := parseCodexRetryAfter(http.StatusBadRequest, body, now); got != nil {
				t.Fatalf("expected nil for non-429, got %v", *got)
			}
		},
	)

	t.Run(
		"non usage_limit_reached error type", func(t *testing.T) {
			body := []byte(`{"error":{"type":"server_error","resets_in_seconds":30}}`)
			if got := parseCodexRetryAfter(http.StatusTooManyRequests, body, now); got != nil {
				t.Fatalf("expected nil for non-usage_limit_reached, got %v", *got)
			}
		},
	)
}

func TestCodexExecuteRetriesHTTP408Once(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				attempts++
				if attempts == 1 {
					w.WriteHeader(http.StatusRequestTimeout)
					_, _ = w.Write([]byte(`{"error":{"message":"stream error: stream disconnected before completion: stream closed before response.completed"}}`))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3},\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	resp, err := exec.Execute(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")},
	)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if !bytes.Contains(resp.Payload, []byte(`"gpt-5.5"`)) {
		t.Fatalf("response payload missing model: %s", string(resp.Payload))
	}
}

func TestCodexExecuteRequiresCompleteSSEFrames(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name: "multiline data",
			body: "data: {\"type\":\ndata: \"response.completed\",\"response\":{\"id\":\"resp_multiline\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3},\"output\":[]}}\n\n",
		},
		{
			name:    "delimiterless completion",
			body:    "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_truncated\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3},\"output\":[]}}",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			exec := newCodexExecutor(&config.Config{})
			resp, err := exec.Execute(
				context.Background(),
				&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
				cliproxyexecutor.Request{
					Model:   "gpt-5.5",
					Payload: []byte(`{"model":"gpt-5.5","input":"hi"}`),
				},
				cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
			)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "incomplete SSE frame") {
					t.Fatalf("Execute error = %v, want incomplete SSE frame", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if !bytes.Contains(resp.Payload, []byte(`"resp_multiline"`)) {
				t.Fatalf("response payload = %s, want multiline completion", resp.Payload)
			}
		})
	}
}

func TestCodexExecuteRecoversWhenCompletedEventIsMissing(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_2\",\"model\":\"gpt-5.5\",\"created_at\":123}}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.content_part.added\"}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.content_part.done\"}\n\n"))
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	resp, err := exec.Execute(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")},
	)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !bytes.Contains(resp.Payload, []byte(`"gpt-5.5"`)) {
		t.Fatalf("response payload missing model: %s", string(resp.Payload))
	}
	if !bytes.Contains(resp.Payload, []byte(`"ok"`)) {
		t.Fatalf("response payload missing recovered text: %s", string(resp.Payload))
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func TestCodexExecuteStreamPropagatesUpstreamEventError(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_err\",\"model\":\"gpt-5.5\",\"created_at\":123}}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"error\",\"status\":400,\"error\":{\"type\":\"invalid_request_error\",\"message\":\"Your input exceeds the context window of this model. Please adjust your input and try again.\",\"code\":\"context_length_exceeded\"}}\n\n"))
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","stream":true,"system":"sys","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var chunks [][]byte
	var streamErr error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			break
		}
		if len(chunk.Payload) > 0 {
			chunks = append(chunks, append([]byte(nil), chunk.Payload...))
		}
	}
	if streamErr == nil {
		t.Fatal("expected stream error, got nil")
	}
	if statusErrCode(streamErr) != http.StatusBadRequest {
		t.Fatalf("statusErrCode = %d, want %d", statusErrCode(streamErr), http.StatusBadRequest)
	}
	if !bytes.Contains([]byte(streamErr.Error()), []byte("context_length_exceeded")) {
		t.Fatalf("stream error = %v, want context_length_exceeded", streamErr)
	}
	joined := bytes.Join(chunks, nil)
	if !bytes.Contains(joined, []byte("event: message_start")) {
		t.Fatalf("stream payload missing message_start: %s", string(joined))
	}
	if bytes.Contains(joined, []byte("event: message_stop")) {
		t.Fatalf("stream payload should not include message_stop on upstream error: %s", string(joined))
	}
}

func TestCodexExecuteStreamAcceptsMultilineSSEData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\ndata: \"response.completed\",\"response\":{\"id\":\"resp_multiline\",\"model\":\"gpt-5.5\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n")
	}))
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","stream":true,"input":"hi"}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var payload []byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		payload = append(payload, chunk.Payload...)
	}
	if !bytes.Contains(payload, []byte(`"response.completed"`)) {
		t.Fatalf("stream payload missing completed event: %q", payload)
	}
}

func TestCodexExecuteStreamRejectsDelimiterlessCompletedEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_truncated\",\"model\":\"gpt-5.5\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}")
	}))
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","stream":true,"input":"hi"}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	var payload []byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			continue
		}
		payload = append(payload, chunk.Payload...)
	}
	if streamErr == nil {
		t.Fatal("stream error = nil, want delimiterless SSE error")
	}
	if bytes.Contains(payload, []byte(`"response.completed"`)) {
		t.Fatalf("delimiterless completion was forwarded: %q", payload)
	}
}

func TestCodexExecuteStreamRejectsMalformedDataFrame(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_bad\",\"model\":\"gpt-5.5\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bad\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n")
	}))
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil || statusErrCode(streamErr) != http.StatusBadGateway {
		t.Fatalf("stream error = %v, want 502", streamErr)
	}
	if !strings.Contains(streamErr.Error(), "malformed stream data") {
		t.Fatalf("stream error = %v, want malformed stream data", streamErr)
	}

	select {
	case record := <-plugin.records:
		if !record.Failed {
			t.Fatalf("usage record Failed = false, want true: %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
}

func TestCodexExecuteStreamScannerErrorAfterCompletedReportsFailure(t *testing.T) {
	plugin := &captureUsagePlugin{records: make(chan usage.Record, 1)}
	registerTestUsagePlugin(t, plugin)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("response writer does not support hijacking")
		}
		conn, rw, errHijack := hijacker.Hijack()
		if errHijack != nil {
			t.Fatalf("Hijack error: %v", errHijack)
		}
		defer conn.Close()

		payload := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_partial\",\"model\":\"gpt-5.5\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n"
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		_, _ = rw.WriteString(strconv.FormatInt(int64(len(payload)), 16) + "\r\n" + payload + "\r\n")
		_ = rw.Flush()
	}))
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","stream":true,"input":"hi"}`),
		},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("stream error = nil, want truncated chunked response error")
	}

	select {
	case record := <-plugin.records:
		if !record.Failed {
			t.Fatalf("usage record Failed = false, want true: %+v", record)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for failed usage record")
	}
}

func TestCodexExecuteStreamRecoversWhenCompletedEventIsMissing(t *testing.T) {
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream\",\"model\":\"gpt-5.5\",\"created_at\":123}}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.content_part.added\"}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.content_part.done\"}\n\n"))
			},
		),
	)
	defer server.Close()

	exec := newCodexExecutor(&config.Config{})
	stream, err := exec.ExecuteStream(
		context.Background(),
		&cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}},
		cliproxyexecutor.Request{
			Model:   "gpt-5.5",
			Payload: []byte(`{"model":"gpt-5.5","stream":true,"system":"sys","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude")},
	)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var joined []byte
	for chunk := range stream.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		joined = append(joined, chunk.Payload...)
	}
	if !bytes.Contains(joined, []byte("event: message_start")) {
		t.Fatalf("stream payload missing message_start: %s", string(joined))
	}
	if !bytes.Contains(joined, []byte("event: content_block_delta")) {
		t.Fatalf("stream payload missing content_block_delta: %s", string(joined))
	}
	if !bytes.Contains(joined, []byte("\"text\":\"ok\"")) {
		t.Fatalf("stream payload missing recovered text: %s", string(joined))
	}
	if !bytes.Contains(joined, []byte("event: message_stop")) {
		t.Fatalf("stream payload missing message_stop: %s", string(joined))
	}
}
