package handlers

import (
	"context"
	"net/http"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestExecuteModelStreamCanceledAtSourceEOF(t *testing.T) {
	for _, cancelAtEOF := range []bool{false, true} {
		name := "source-closed"
		if cancelAtEOF {
			name = "canceled-before-completion"
		}
		t.Run(
			name, func(t *testing.T) {
				executor := &lifecycleCaptureExecutor{
					stream: func(
						context.Context,
						*coreauth.Auth,
						coreexecutor.Request,
						coreexecutor.Options,
					) (*coreexecutor.StreamResult, error) {
						chunks := make(chan coreexecutor.StreamChunk, 1)
						// The manager accepts the preamble and removes it before the handler sees EOF.
						chunks <- coreexecutor.StreamChunk{}
						close(chunks)
						return &coreexecutor.StreamResult{Chunks: chunks}, nil
					},
				}
				const model = "nested-eof-model"
				handler := newLifecycleHandler(t, model, executor)
				outer, finishOuter := coreusage.WithStreamDelivery(t.Context())
				defer finishOuter(context.Canceled)
				outer = coreexecutor.WithStreamDeliveryAcknowledgement(outer)
				ctx, cancel := context.WithCancel(outer)
				defer cancel()
				completed := make(chan pluginapi.RequestCompletion, 2)
				headerCalls := 0
				handler.SetPluginHost(
					&lifecycleTestHost{
						interceptStreamChunk: func(
							_ context.Context,
							request pluginapi.StreamChunkInterceptRequest,
						) pluginapi.StreamChunkInterceptResponse {
							if request.ChunkIndex == pluginapi.StreamChunkHeaderInitIndex {
								headerCalls++
								if cancelAtEOF {
									cancel()
								}
							}
							return pluginapi.StreamChunkInterceptResponse{}
						},
						completeRequest: func(_ context.Context, completion pluginapi.RequestCompletion) {
							completed <- completion
						},
					},
				)
				stream, err := handler.ExecuteModelStream(
					ctx, ModelExecutionRequest{
						EntryProtocol: "openai", Model: model, Stream: true, Body: []byte(`{"stream":true}`),
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				if headerCalls != 1 {
					t.Fatalf("header initialization calls=%d, want 1 at source EOF", headerCalls)
				}
				timer := time.NewTimer(5 * time.Second)
				defer timer.Stop()
				for stream.Chunks != nil {
					select {
					case chunk, ok := <-stream.Chunks:
						if !ok {
							stream.Chunks = nil
						} else if chunk.Err != nil || len(chunk.Payload) != 0 {
							t.Fatalf("unexpected EOF stream chunk: %+v", chunk)
						}
					case <-timer.C:
						t.Fatal("nested EOF stream did not close")
					}
				}
				select {
				case completion := <-completed:
					wantOutcome, wantStatus, wantError := pluginapi.RequestCompletionSucceeded, http.StatusOK, ""
					if cancelAtEOF {
						wantOutcome = pluginapi.RequestCompletionCanceled
						wantStatus, wantError = 0, context.Canceled.Error()
					}
					if completion.Outcome != wantOutcome || completion.StatusCode != wantStatus ||
						completion.Error != wantError {
						t.Fatalf(
							"nested EOF completion=%+v, want %s/%d/%q", completion, wantOutcome, wantStatus, wantError,
						)
					}
				case <-timer.C:
					t.Fatal("nested EOF completion waited for the outer receipt")
				}
				select {
				case completion := <-completed:
					t.Fatalf("duplicate nested EOF completion: %+v", completion)
				default:
				}
				if !coreusage.StreamDeliveryTracked(outer) || !coreexecutor.StreamDeliveryAcknowledgement(outer) {
					t.Fatal("nested EOF stream changed the outer delivery ownership")
				}
			},
		)
	}
}
