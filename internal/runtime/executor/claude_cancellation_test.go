package executor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestClaudeExecutorExecuteStreamOAuthStartupCancellationIsRequestScoped(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(
		http.HandlerFunc(
			func(http.ResponseWriter, *http.Request) {
				close(started)
				<-release
			},
		),
	)
	defer server.Close()
	defer close(release)

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "oauth-stream-startup-cancellation",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-stream-startup-cancellation",
			"base_url": server.URL,
		},
		Metadata: claudeOAuthWireTestMetadata(),
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, errStream := executor.ExecuteStream(
			ctx, auth, cliproxyexecutor.Request{
				Model:   "claude-opus-4-7",
				Payload: []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hello"}],"stream":true}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
		)
		errCh <- errStream
	}()
	<-started
	cancel()

	select {
	case errStream := <-errCh:
		if !errors.Is(errStream, context.Canceled) {
			t.Fatalf("ExecuteStream() error = %v, want context.Canceled", errStream)
		}
		var requestErr cliproxyexecutor.RequestScopedError
		if !errors.As(errStream, &requestErr) || requestErr == nil || !requestErr.IsRequestScoped() {
			t.Fatalf("ExecuteStream() error = %T %v, want request-scoped cancellation", errStream, errStream)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for startup cancellation")
	}
}

func TestClaudeExecutorExecuteStreamOAuthActiveCancellationIsRequestScoped(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("data"))
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				close(started)
				<-req.Context().Done()
			},
		),
	)
	defer server.Close()

	executor := NewClaudeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID: "oauth-stream-cancellation",
		Attributes: map[string]string{
			"api_key":  "sk-ant-oat-stream-cancellation",
			"base_url": server.URL,
		},
		Metadata: claudeOAuthWireTestMetadata(),
	}
	payload := []byte(`{"model":"claude-opus-4-7","system":"system prompt","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	ctx, cancel := context.WithCancel(context.Background())
	result, errStream := executor.ExecuteStream(
		ctx, auth, cliproxyexecutor.Request{
			Model:   "claude-opus-4-7",
			Payload: payload,
		}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude},
	)
	if errStream != nil {
		cancel()
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	<-started
	cancel()

	var cancellationErr error
	deadline := time.After(2 * time.Second)
	for cancellationErr == nil {
		select {
		case chunk, ok := <-result.Chunks:
			if !ok {
				t.Fatal("stream closed without a cancellation result")
			}
			cancellationErr = chunk.Err
		case <-deadline:
			t.Fatal("timed out waiting for cancellation result")
		}
	}
	if !errors.Is(cancellationErr, context.Canceled) {
		t.Fatalf("stream error = %v, want context.Canceled", cancellationErr)
	}
	var requestErr cliproxyexecutor.RequestScopedError
	if !errors.As(cancellationErr, &requestErr) || requestErr == nil || !requestErr.IsRequestScoped() {
		t.Fatalf("stream error = %T %v, want request-scoped cancellation", cancellationErr, cancellationErr)
	}
	if statusErr, ok := errors.AsType[interface {
		error
		StatusCode() int
	}](cancellationErr); ok {
		t.Fatalf("stream cancellation unexpectedly exposes HTTP status %d", statusErr.StatusCode())
	}
	for range result.Chunks {
	}
}

func TestClaudeOAuthCancellationClassificationExcludesAPIKeys(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := newClaudeOAuthCancellationError(ctx, false, context.Canceled); got != nil {
		t.Fatalf("API-key cancellation = %T %v, want nil", got, got)
	}
	got := newClaudeOAuthCancellationError(ctx, true, context.Canceled)
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("OAuth cancellation = %v, want context.Canceled", got)
	}
	var requestErr cliproxyexecutor.RequestScopedError
	if !errors.As(got, &requestErr) || requestErr == nil || !requestErr.IsRequestScoped() {
		t.Fatalf("OAuth cancellation = %T %v, want request-scoped", got, got)
	}
}
