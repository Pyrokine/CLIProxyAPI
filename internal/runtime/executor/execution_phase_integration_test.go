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

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	_ "github.com/Pyrokine/CLIProxyAPI/v6/internal/translator"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
)

type executionPhaseRoundTripper func(*http.Request) (*http.Response, error)

func (f executionPhaseRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type executionPhaseReadErrorBody struct{}

func (executionPhaseReadErrorBody) Read([]byte) (int, error) {
	return 0, errors.New("response read failed")
}

func (executionPhaseReadErrorBody) Close() error {
	return nil
}

func requireResponseStartedDisposition(t *testing.T, err error) cliproxyexecutor.ExecutionDisposition {
	t.Helper()
	var dispositionErr cliproxyexecutor.ExecutionDispositionError
	if !errors.As(err, &dispositionErr) || dispositionErr == nil {
		t.Fatalf("error = %T %v, want execution disposition", err, err)
	}
	disposition := dispositionErr.ExecutionDisposition()
	if disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseResponseStarted {
		t.Fatalf(
			"dispatch phase = %q, want %q; disposition=%+v", disposition.DispatchPhase,
			cliproxyexecutor.DispatchPhaseResponseStarted, disposition,
		)
	}
	return disposition
}

func requireNoResponseStartedDisposition(t *testing.T, err error) {
	t.Helper()
	var dispositionErr cliproxyexecutor.ExecutionDispositionError
	if errors.As(err, &dispositionErr) && dispositionErr != nil &&
		dispositionErr.ExecutionDisposition().DispatchPhase == cliproxyexecutor.DispatchPhaseResponseStarted {
		t.Fatalf("error before first upstream frame was marked response_started: %v", err)
	}
}

func newWebsocketReadPhaseServer(t *testing.T, messageType int, payload []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(
		http.HandlerFunc(
			func(w http.ResponseWriter, r *http.Request) {
				conn, errUpgrade := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(
					w, r, nil,
				)
				if errUpgrade != nil {
					t.Errorf("upgrade websocket: %v", errUpgrade)
					return
				}
				defer func() { _ = conn.Close() }()
				if _, _, errRead := conn.ReadMessage(); errRead != nil {
					t.Errorf("read upstream websocket request: %v", errRead)
					return
				}
				if messageType != 0 {
					if errWrite := conn.WriteMessage(messageType, payload); errWrite != nil {
						t.Errorf("write upstream websocket response: %v", errWrite)
					}
				}
			},
		),
	)
}

func streamTerminalError(t *testing.T, stream *cliproxyexecutor.StreamResult) error {
	t.Helper()
	var terminalErr error
	select {
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stream termination")
	case terminalErr = <-func() <-chan error {
		result := make(chan error, 1)
		go func() {
			defer close(result)
			for chunk := range stream.Chunks {
				if chunk.Acknowledge != nil {
					chunk.Acknowledge(nil)
				}
				if chunk.Err != nil {
					terminalErr = chunk.Err
				}
			}
			result <- terminalErr
		}()
		return result
	}():
	}
	if terminalErr == nil {
		t.Fatal("stream terminated without an error")
	}
	return terminalErr
}

func TestCodexWebsocketReadPhaseTracksFirstUpstreamFrame(t *testing.T) {
	tests := []struct {
		name            string
		payload         []byte
		responseStarted bool
	}{
		{name: "before first frame"},
		{
			name:            "after first frame",
			payload:         []byte(`{"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}`),
			responseStarted: true,
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				messageType := 0
				if len(tt.payload) > 0 {
					messageType = websocket.TextMessage
				}
				server := newWebsocketReadPhaseServer(t, messageType, tt.payload)
				defer server.Close()

				exec := newCodexWebsocketsExecutor(&config.Config{})
				stream, errExecute := exec.ExecuteStream(
					cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
					&cliproxyauth.Auth{
						ID:         "codex-read-phase",
						Provider:   "codex",
						Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
					},
					cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)},
					cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, Stream: true},
				)
				if errExecute != nil {
					t.Fatalf("ExecuteStream() error = %v", errExecute)
				}
				terminalErr := streamTerminalError(t, stream)
				if tt.responseStarted {
					requireResponseStartedDisposition(t, terminalErr)
					return
				}
				requireNoResponseStartedDisposition(t, terminalErr)
			},
		)
	}
}

func TestXAIWebsocketReadPhaseTracksFirstUpstreamFrame(t *testing.T) {
	tests := []struct {
		name            string
		payload         []byte
		responseStarted bool
	}{
		{name: "before first frame"},
		{
			name:            "after first frame",
			payload:         []byte(`{"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}`),
			responseStarted: true,
		},
	}
	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				messageType := 0
				if len(tt.payload) > 0 {
					messageType = websocket.TextMessage
				}
				server := newWebsocketReadPhaseServer(t, messageType, tt.payload)
				defer server.Close()

				exec := newXAIWebsocketsExecutor(&config.Config{})
				stream, errExecute := exec.ExecuteStream(
					cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
					&cliproxyauth.Auth{
						ID:         "xai-read-phase",
						Provider:   "xai",
						Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
					},
					cliproxyexecutor.Request{
						Model: "grok-4.3", Payload: []byte(`{"model":"grok-4.3","input":"hello"}`),
					},
					cliproxyexecutor.Options{
						SourceFormat:   sdktranslator.FormatOpenAIResponse,
						ResponseFormat: sdktranslator.FormatOpenAIResponse,
					},
				)
				if errExecute != nil {
					t.Fatalf("ExecuteStream() error = %v", errExecute)
				}
				terminalErr := streamTerminalError(t, stream)
				if tt.responseStarted {
					requireResponseStartedDisposition(t, terminalErr)
					return
				}
				requireNoResponseStartedDisposition(t, terminalErr)
			},
		)
	}
}

func TestXAIWebsocketBinaryFrameIsResponseStarted(t *testing.T) {
	server := newWebsocketReadPhaseServer(t, websocket.BinaryMessage, []byte("binary"))
	defer server.Close()

	exec := newXAIWebsocketsExecutor(&config.Config{})
	stream, errExecute := exec.ExecuteStream(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
		&cliproxyauth.Auth{
			ID:         "xai-binary-frame",
			Provider:   "xai",
			Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
		},
		cliproxyexecutor.Request{Model: "grok-4.3", Payload: []byte(`{"model":"grok-4.3","input":"hello"}`)},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse,
		},
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	requireResponseStartedDisposition(t, streamTerminalError(t, stream))
}

func TestXAIWebsocketTranscriptFailureIsResponseStarted(t *testing.T) {
	items := make([]string, maxXAIWebsocketTranscriptItems+1)
	for i := range items {
		items[i] = `{"type":"message","role":"assistant","content":"x"}`
	}
	payload := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[` +
		strings.Join(items, ",") + `],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
	server := newWebsocketReadPhaseServer(t, websocket.TextMessage, payload)
	defer server.Close()

	exec := newXAIWebsocketsExecutor(&config.Config{})
	defer exec.CloseExecutionSession("xai-transcript-limit-session")
	stream, errExecute := exec.ExecuteStream(
		cliproxyexecutor.WithDownstreamWebsocket(context.Background()),
		&cliproxyauth.Auth{
			ID:         "xai-transcript-limit",
			Provider:   "xai",
			Attributes: map[string]string{"api_key": "test-token", "base_url": server.URL},
		},
		cliproxyexecutor.Request{
			Model:   "grok-4.3",
			Payload: []byte(`{"model":"grok-4.3","input":[{"type":"message","role":"user","content":"hello"}]}`),
		},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse,
			Metadata: map[string]any{
				cliproxyexecutor.ExecutionSessionMetadataKey: "xai-transcript-limit-session",
			},
		},
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	terminalErr := streamTerminalError(t, stream)
	if !strings.Contains(terminalErr.Error(), "transcript exceeds maximum item count") {
		t.Fatalf("terminal error = %v", terminalErr)
	}
	requireResponseStartedDisposition(t, terminalErr)
}

func TestXAIHTTPStreamResponseFailureIsTerminalResponseStartedError(t *testing.T) {
	ctx := util.WithRoundTripper(
		context.Background(), executionPhaseRoundTripper(
			func(*http.Request) (*http.Response, error) {
				body := "event: response.failed\n" +
					`data: {"type":"response.failed","response":{"status":"failed","error":{"type":"server_error","message":"failed"}}}` +
					"\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			},
		),
	)
	exec := newXAIExecutor(&config.Config{})
	stream, errExecute := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{
			ID:         "xai-response-failed",
			Provider:   "xai",
			Attributes: map[string]string{"api_key": "test-token", "base_url": "https://xai.test"},
		},
		cliproxyexecutor.Request{Model: "grok-4.3", Payload: []byte(`{"model":"grok-4.3","input":"hello"}`)},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatOpenAIResponse,
		},
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	disposition := requireResponseStartedDisposition(t, streamTerminalError(t, stream))
	if disposition.Outcome != cliproxyexecutor.ExecutionOutcomeUnknown ||
		disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry ||
		disposition.EffectScope != cliproxyexecutor.StateEffectScopeProvider {
		t.Fatalf("execution disposition = %+v", disposition)
	}
}

func TestCodexDirectImageStreamReadFailureIsResponseStarted(t *testing.T) {
	ctx := util.WithRoundTripper(
		context.Background(), executionPhaseRoundTripper(
			func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       executionPhaseReadErrorBody{},
				}, nil
			},
		),
	)
	exec := &codexExecutor{cfg: &config.Config{}}
	stream, errExecute := exec.ExecuteStream(
		ctx,
		&cliproxyauth.Auth{
			ID:         "codex-image-read-failure",
			Provider:   "codex",
			Attributes: map[string]string{"api_key": "test-token", "base_url": "https://images.test"},
		},
		cliproxyexecutor.Request{
			Model:   "gpt-image-2",
			Payload: []byte(`{"model":"gpt-image-2","prompt":"draw","stream":true}`),
		},
		cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FromString(codexOpenAIImageSourceFormat),
			Stream:       true,
			Metadata: map[string]any{
				cliproxyexecutor.RequestPathMetadataKey: codexImagesGenerationsPath,
			},
		},
	)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	requireResponseStartedDisposition(t, streamTerminalError(t, stream))
}

func TestCodexImageInvalidSSEIsResponseStarted(t *testing.T) {
	_, _, err := parseCodexOpenAIImageEvent([]byte("data: {invalid}\n\n"))
	if err == nil {
		t.Fatal("parseCodexOpenAIImageEvent() error = nil")
	}
	requireResponseStartedDisposition(t, err)
}
