package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator/builtin"
)

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
