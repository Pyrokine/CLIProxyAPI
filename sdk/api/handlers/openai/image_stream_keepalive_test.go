package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	internallogging "github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/gin-gonic/gin"
)

type imageWaitingExecutor struct {
	mediaRegressionExecutor
	phase string
	gate  <-chan struct{}
}

func (e *imageWaitingExecutor) wait(ctx context.Context) error {
	select {
	case <-e.gate:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (e *imageWaitingExecutor) Execute(
	ctx context.Context,
	auth *coreauth.Auth,
	request coreexecutor.Request,
	options coreexecutor.Options,
) (coreexecutor.Response, error) {
	if err := e.wait(ctx); err != nil {
		return coreexecutor.Response{}, err
	}
	return e.mediaRegressionExecutor.Execute(ctx, auth, request, options)
}

func (e *imageWaitingExecutor) ExecuteStream(
	ctx context.Context,
	_ *coreauth.Auth,
	_ coreexecutor.Request,
	_ coreexecutor.Options,
) (*coreexecutor.StreamResult, error) {
	if e.phase == "setup" {
		if err := e.wait(ctx); err != nil {
			return nil, err
		}
	}
	if e.err != nil {
		return nil, e.err
	}
	chunks := make(chan coreexecutor.StreamChunk, 1)
	go func() {
		defer close(chunks)
		if e.phase == "first frame" {
			if e.wait(ctx) != nil {
				return
			}
		}
		chunks <- coreexecutor.StreamChunk{
			Payload: []byte(("data: {\"type\":\"image_generation.completed\"," +
				"\"b64_json\":" +
				"\"") +
				testPNGBase64 +
				"\"}\n\n"),
		}
	}()
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

type imageHeartbeatWriter struct {
	gin.ResponseWriter
	heartbeat chan struct{}
	once      sync.Once
	flushErr  error
}

func (writer *imageHeartbeatWriter) Write(payload []byte) (int, error) {
	if strings.Contains(string(payload), ": keep-alive") {
		writer.once.Do(func() { close(writer.heartbeat) })
	}
	return writer.ResponseWriter.Write(payload)
}

func (writer *imageHeartbeatWriter) FlushError() error {
	return writer.flushErr
}

func TestImageStreamWaitsUseConfiguredHeartbeat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, phase := range []string{
		"setup", "first frame", "xAI execute", "heartbeat flush failure", "xAI upstream error",
		"compatible upstream error",
	} {
		t.Run(
			phase, func(t *testing.T) {
				gate := make(chan struct{})
				executor := &imageWaitingExecutor{provider: "fixture-image-provider", phase: phase, gate: gate}
				model := "fixture-keepalive-image"
				if phase == "xAI execute" || phase == "heartbeat flush failure" ||
					phase == "xAI upstream error" {
					model, executor.provider = defaultXAIImagesModel, "xai"
					executor.payload = []byte(`{"data":[{"b64_json":"` + testPNGBase64 + `"}]}`)
				}
				if strings.Contains(phase, "upstream error") {
					executor.err = &imageHandlerStatusError{
						statusCode: http.StatusBadRequest, err: errors.New(mediaPrivateError),
					}
					if phase == "compatible upstream error" {
						executor.phase = "setup"
					}
				}
				handler := newMediaRegressionHandler(t, executor, model)
				handler.UpdateClients(
					&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{KeepAliveSeconds: 1}},
				)
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				outcome := internallogging.NewRequestOutcomeState()
				requestCtx, cancel := context.WithCancel(
					internallogging.WithRequestOutcomeState(t.Context(), outcome),
				)
				defer cancel()
				ctx.Request = httptest.NewRequest(
					http.MethodPost, imagesGenerationsPath,
					strings.NewReader(`{"model":"`+model+`","prompt":"fixture","stream":true}`),
				).WithContext(requestCtx)
				ctx.Request.Header.Set("Content-Type", "application/json")
				writer := &imageHeartbeatWriter{ResponseWriter: ctx.Writer, heartbeat: make(chan struct{})}
				if phase == "heartbeat flush failure" {
					writer.flushErr = errors.New("fixture heartbeat flush failure")
				}
				ctx.Writer = writer
				done := make(chan struct{})
				go func() {
					handler.ImagesGenerations(ctx)
					close(done)
				}()
				heartbeat := false
				timer := time.NewTimer(1500 * time.Millisecond)
				select {
				case <-writer.heartbeat:
					heartbeat = true
				case <-timer.C:
				}
				timer.Stop()
				close(gate)
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					cancel()
					<-done
					t.Fatal("image handler did not finish after releasing the executor")
				}
				if !heartbeat {
					t.Errorf("no heartbeat during %s", phase)
				}
				body := recorder.Body.String()
				if phase == "heartbeat flush failure" {
					if !outcome.Snapshot().Failed || strings.Contains(body, ".completed") {
						t.Errorf("heartbeat delivery failure = %#v, body=%s", outcome.Snapshot(), body)
					}
				} else if strings.Contains(phase, "upstream error") {
					if !outcome.Snapshot().Failed || strings.Count(body, "event: error") != 1 || strings.Contains(
						body, ".completed",
					) {
						t.Errorf("post-heartbeat error = %#v, body=%s", outcome.Snapshot(), body)
					}
					assertMediaErrorPrivacy(t, body)
				} else if !strings.Contains(body, ".completed") {
					t.Errorf("image completion missing after heartbeat: %s", body)
				}
			},
		)
	}
}
