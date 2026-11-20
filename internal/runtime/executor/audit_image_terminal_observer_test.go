package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/util"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
	"github.com/gorilla/websocket"
)

type auditImageFinalRead struct {
	payload []byte
	err     error
}

func (r *auditImageFinalRead) Read(p []byte) (int, error) {
	n := copy(p, r.payload)
	r.payload = r.payload[n:]
	if len(r.payload) == 0 {
		return n, r.err
	}
	return n, nil
}

type auditImageBody struct {
	io.Reader
	once   sync.Once
	closed chan struct{}
}

func (b *auditImageBody) Close() error {
	var err error
	b.once.Do(
		func() {
			if closer, ok := b.Reader.(io.Closer); ok {
				err = closer.Close()
			}
			close(b.closed)
		},
	)
	return err
}

func TestAuditWrappedImageCompletesBeforeEOFAndIgnoresTrailingReadError(t *testing.T) {
	const terminal = "data: {\"type\":\"response.completed\",\"response\":{\"created_at\":17" +
		"13833628,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"" +
		"AA==\",\"output_format\":\"png\"}]}}\n\n"
	for _, mode := range []string{"open-pipe", "data-plus-error", "error-before-terminal"} {
		t.Run(
			mode, func(t *testing.T) {
				var reader io.Reader = &auditImageFinalRead{payload: []byte(terminal), err: io.ErrUnexpectedEOF}
				if mode == "open-pipe" {
					pipe, writer := io.Pipe()
					reader = pipe
					t.Cleanup(func() { _ = pipe.Close(); _ = writer.Close() })
					go func() { _, _ = io.WriteString(writer, terminal) }()
				} else if mode == "error-before-terminal" {
					reader = &auditImageFinalRead{
						payload: []byte("data: {\"type\":\"response.created\"}\n\n"), err: io.ErrUnexpectedEOF,
					}
				}
				body := &auditImageBody{Reader: reader, closed: make(chan struct{})}
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				ctx = util.WithRoundTripper(
					ctx, devinTestRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							return &http.Response{
								StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
								Body: body, Request: request,
							}, nil
						},
					),
				)
				execute := newCodexExecutor(&config.Config{})
				result, err := execute.ExecuteStream(
					ctx, newCodexOpenAIImageTestAuth("http://127.0.0.1:1"),
					core.Request{Model: "gpt-image-1", Payload: []byte(`{"model":"gpt-image-1","prompt":"synthetic"}`)},
					codexOpenAIImageTestOptions(codexImagesGenerationsPath, true),
				)
				if err != nil {
					t.Fatal(err)
				}
				var payload bytes.Buffer
				var streamErr error
				for chunk := range result.Chunks {
					payload.Write(chunk.Payload)
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
				}
				if mode == "error-before-terminal" {
					if streamErr == nil || payload.Len() != 0 {
						t.Fatalf("premature read error=%v payload=%s", streamErr, payload.Bytes())
					}
				} else if streamErr != nil || !bytes.Contains(payload.Bytes(), []byte(`"b64_json":"AA=="`)) {
					t.Fatalf("completed image error=%v payload=%s", streamErr, payload.Bytes())
				}
				select {
				case <-body.closed:
				default:
					t.Fatal("terminal path left image response body open")
				}
			},
		)
	}
}

func TestAuditDirectImageDeliversReadBytesBeforeError(t *testing.T) {
	const wire = "data: {\"type\":\"image_generation.completed\",\"b64_json\":\"AA==\"}\n\n"
	for _, readErr := range []error{io.EOF, io.ErrUnexpectedEOF} {
		t.Run(
			readErr.Error(), func(t *testing.T) {
				body := &auditImageBody{
					Reader: &auditImageFinalRead{payload: []byte(wire), err: readErr}, closed: make(chan struct{}),
				}
				ctx := util.WithRoundTripper(
					t.Context(), devinTestRoundTripper(
						func(request *http.Request) (*http.Response, error) {
							if request.URL.Path != "/images/generations" {
								t.Errorf("direct image path=%s", request.URL.Path)
							}
							return &http.Response{
								StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
								Body: body, Request: request,
							}, nil
						},
					),
				)
				result, err := newCodexExecutor(&config.Config{}).ExecuteStream(
					ctx, newCodexOpenAIImageTestAuth("http://127.0.0.1:1"), core.Request{
						Model: "gpt-image-1.5", Payload: []byte(`{"model":"gpt-image-1.5","prompt":"synthetic"}`),
					}, codexOpenAIImageTestOptions(codexImagesGenerationsPath, true),
				)
				if err != nil {
					t.Fatal(err)
				}
				var payload bytes.Buffer
				var streamErr error
				for chunk := range result.Chunks {
					payload.Write(chunk.Payload)
					if chunk.Err != nil {
						streamErr = chunk.Err
					}
				}
				if payload.String() != wire || (readErr == io.EOF && streamErr != nil) ||
					(readErr != io.EOF && !errors.Is(
						streamErr, readErr,
					)) {
					t.Fatalf("payload=%q error=%v read-error=%v", payload.String(), streamErr, readErr)
				}
				select {
				case <-body.closed:
				default:
					t.Fatal("direct stream body was not closed")
				}
			},
		)
	}
}

func TestAuditCodexWebsocketObserverCoversExecuteAndStream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(
			fmt.Sprint(stream), func(t *testing.T) {
				frames := []string{`{"type":"rate_limits.updated","rate_limits":[]}`, runtimeCodexCompleted}
				server := httptest.NewServer(
					http.HandlerFunc(
						func(w http.ResponseWriter, r *http.Request) {
							peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
							if err != nil {
								t.Error(err)
								return
							}
							defer func(closeResource func() error) { _ = closeResource() }(peer.Close)
							if _, _, err = peer.ReadMessage(); err != nil {
								t.Error(err)
								return
							}
							for _, frame := range frames {
								if err = peer.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
									t.Error(err)
									return
								}
							}
						},
					),
				)
				defer server.Close()
				events := make(chan core.WebSocketResponseEvent, len(frames))
				opts := core.Options{
					SourceFormat: translator.FormatOpenAIResponse, ResponseFormat: translator.FormatOpenAIResponse,
					WebSocketResponseObserver: func(
						_ context.Context,
						event core.WebSocketResponseEvent,
					) {
						events <- event
					},
				}
				execute := newCodexWebsocketsExecutor(
					&config.Config{DisableImageGeneration: config.DisableImageGenerationAll},
				)
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				credential := runtimeCodexAuth(server.URL)
				req := core.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":"synthetic"}`)}
				var err error
				if stream {
					var result *core.StreamResult
					result, err = execute.ExecuteStream(ctx, credential, req, opts)
					if err == nil {
						for chunk := range result.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = execute.Execute(ctx, credential, req, opts)
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(events) != len(frames) {
					t.Fatalf("observer events=%d want=%d", len(events), len(frames))
				}
				for _, frame := range frames {
					event := <-events
					if string(event.Payload) != frame || event.AuthID != credential.ID || event.Provider != "codex" ||
						event.Model != req.Model {
						t.Fatalf("observer event=%+v payload=%s", event, event.Payload)
					}
				}
			},
		)
	}
}
