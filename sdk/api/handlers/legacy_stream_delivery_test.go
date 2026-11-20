package handlers

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestChunkStreamDoesNotWaitForLegacyConsumerReceipt(t *testing.T) {
	executor := &lifecycleCaptureExecutor{
		provider: "codex", stream: func(
			context.Context,
			*coreauth.Auth,
			coreexecutor.Request,
			coreexecutor.Options,
		) (*coreexecutor.StreamResult, error) {
			chunks := make(chan coreexecutor.StreamChunk, 1)
			chunks <- coreexecutor.StreamChunk{Payload: []byte("data: {\"part\":true}\n\n")}
			close(chunks)
			return &coreexecutor.StreamResult{Chunks: chunks}, nil
		},
	}
	handler := newLifecycleHandler(t, "chunk-with-legacy-context", executor)
	completed := make(chan pluginapi.RequestCompletion, 1)
	handler.SetPluginHost(
		&lifecycleTestHost{
			completeRequest: func(
				_ context.Context,
				result pluginapi.RequestCompletion,
			) {
				completed <- result
			},
		},
	)
	ctx, finish := coreusage.WithStreamDelivery(t.Context())
	defer finish(context.Canceled)
	chunks, _, errs := handler.ExecuteStreamChunksWithAuthManager(
		ctx, "openai", "chunk-with-legacy-context", []byte(`{"stream":true}`), "",
	)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for chunks != nil || errs != nil {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				chunks = nil
			} else if chunk.Acknowledge != nil {
				chunk.Acknowledge(nil)
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
			} else if err != nil {
				t.Fatalf("chunk stream error: %+v", err)
			}
		case <-timer.C:
			t.Fatal("chunk stream waited for a separate whole-stream receipt")
		}
	}
	select {
	case result := <-completed:
		if result.Outcome != pluginapi.RequestCompletionSucceeded || result.StatusCode != http.StatusOK {
			t.Fatalf("chunk completion=%+v", result)
		}
	case <-timer.C:
		t.Fatal("chunk acknowledgement did not release lifecycle completion")
	}
}

func TestLegacyStreamLifecycleWaitsAfterOutputCloses(t *testing.T) {
	readFailure := &coreauth.Error{
		Code: "upstream_read_failed", HTTPStatus: http.StatusBadGateway, Message: "synthetic upstream read failure",
	}
	for _, test := range []struct {
		name        string
		deliveryErr error
		upstreamErr error
		cancel      bool
		outcome     pluginapi.RequestCompletionOutcome
		status      int
	}{
		{name: "delivered", outcome: pluginapi.RequestCompletionSucceeded, status: http.StatusOK},
		{
			name: "delivered after cancellation", cancel: true, outcome: pluginapi.RequestCompletionSucceeded,
			status: http.StatusOK,
		},
		{name: "canceled", deliveryErr: context.Canceled, cancel: true, outcome: pluginapi.RequestCompletionCanceled},
		{name: "deadline", deliveryErr: context.DeadlineExceeded, outcome: pluginapi.RequestCompletionCanceled},
		{
			name: "write failure", deliveryErr: io.ErrClosedPipe, outcome: pluginapi.RequestCompletionFailed,
			status: http.StatusInternalServerError,
		},
		{
			name: "read failure before canceled receipt", upstreamErr: readFailure, deliveryErr: context.Canceled,
			cancel: true, outcome: pluginapi.RequestCompletionFailed, status: http.StatusBadGateway,
		},
	} {
		t.Run(
			test.name, func(t *testing.T) {
				executor := &lifecycleCaptureExecutor{
					provider: "codex", stream: func(
						context.Context,
						*coreauth.Auth,
						coreexecutor.Request,
						coreexecutor.Options,
					) (*coreexecutor.StreamResult, error) {
						chunks := make(chan coreexecutor.StreamChunk, 2)
						chunks <- coreexecutor.StreamChunk{Payload: []byte("data: {\"part\":true}\n\n")}
						if test.upstreamErr != nil {
							chunks <- coreexecutor.StreamChunk{Err: test.upstreamErr}
						}
						close(chunks)
						return &coreexecutor.StreamResult{Chunks: chunks}, nil
					},
				}
				handler := newLifecycleHandler(t, "legacy-delivery-test", executor)
				completions := make(chan pluginapi.RequestCompletion, 2)
				handler.SetPluginHost(
					&lifecycleTestHost{
						completeRequest: func(_ context.Context, completion pluginapi.RequestCompletion) {
							completions <- completion
						},
					},
				)
				ctx, finish := coreusage.WithStreamDelivery(t.Context())
				defer finish(context.Canceled)
				ctx, cancel := context.WithCancel(ctx)
				defer cancel()
				data, _, errs := handler.ExecuteStreamWithAuthManager(
					ctx, "openai", "legacy-delivery-test", []byte(`{"stream":true}`), "",
				)
				timer := time.NewTimer(5 * time.Second)
				defer timer.Stop()
				for data != nil || errs != nil {
					select {
					case _, ok := <-data:
						if !ok {
							data = nil
						}
					case msg, ok := <-errs:
						if !ok {
							errs = nil
						} else if msg != nil && test.upstreamErr == nil {
							t.Fatalf("unexpected stream error: %+v", msg)
						}
					case <-timer.C:
						t.Fatal("output channels waited for a consumer receipt")
					}
				}
				select {
				case completion := <-completions:
					t.Fatalf("request completed before the consumer receipt: %+v", completion)
				default:
				}
				if test.cancel {
					cancel()
				}
				finish(test.deliveryErr)
				select {
				case completion := <-completions:
					if completion.Outcome != test.outcome || completion.StatusCode != test.status {
						t.Fatalf("completion=%+v, want %s/%d", completion, test.outcome, test.status)
					}
					wantErr := test.deliveryErr
					if test.upstreamErr != nil {
						wantErr = test.upstreamErr
					}
					if wantErr != nil && completion.Error != wantErr.Error() {
						t.Fatalf("concrete error replaced by receipt cleanup: %+v", completion)
					}
				case <-timer.C:
					t.Fatal("consumer receipt did not release request completion")
				}
			},
		)
	}
}
