package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreusage "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdkconfig "github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
	sdktranslator "github.com/Pyrokine/CLIProxyAPI/v6/sdk/translator"
)

func TestPluginExecutorDelegatesUsageToNestedHostExecution(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, scenario := range []struct {
			failed  bool
			missing bool
		}{{}, {failed: true}, {failed: true, missing: true}} {
			failed := scenario.failed
			t.Run(
				fmt.Sprintf(
					"stream=%t/failed=%t/missing=%t",
					stream,
					failed,
					scenario.missing,
				),
				func(t *testing.T) {
					outerModel, innerModel := "agent-wrapper-model", "inner-model"
					if failed {
						outerModel, innerModel = "agent-wrapper-model-fail", "inner-model-fail"
					}
					usageManager := coreusage.NewManager(16)
					records := make(chan coreusage.Record, 4)
					usageManager.RegisterNamed(
						t.Name(), coreUsagePluginFunc(
							func(_ context.Context, record coreusage.Record) {
								records <- record
							},
						),
					)
					usageManager.Start(context.Background())
					t.Cleanup(
						func() {
							ctx, cancel := context.WithTimeout(context.Background(), time.Second)
							defer cancel()
							if err := usageManager.StopContext(ctx); err != nil {
								t.Errorf("stop usage manager: %v", err)
							}
						},
					)

					host := New()
					manager := coreauth.NewManager(nil, nil, nil)
					manager.SetRetryConfig(0, 0, 1)
					handler := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
					handler.SetUsageManager(usageManager)
					host.SetModelExecutor(handler)
					format := sdktranslator.FormatOpenAI
					innerPayload := []byte(
						(`{"id":"chatcmpl-inner","choices":[{"message":{"role":"assistant","cont` +
							`ent":"inner"}}],"usage":{"prompt_tokens":5,"completion_tokens":5,"tota` +
							`l_tokens":10}}`),
					)
					innerExecute := func(context.Context, pluginapi.ExecutorRequest) (
						pluginapi.ExecutorResponse,
						error,
					) {
						if failed {
							return pluginapi.ExecutorResponse{}, errors.New("inner failure")
						}
						return pluginapi.ExecutorResponse{Payload: innerPayload}, nil
					}
					inner := &executorAdapter{
						host: host, pluginID: "inner-native-plugin", provider: "openai",
						inputFormats: []sdktranslator.Format{
							format,
						}, outputFormats: []sdktranslator.Format{format},
						executor: &fakeExecutor{
							execute: innerExecute,
							executeStream: func(
								ctx context.Context,
								req pluginapi.ExecutorRequest,
							) (pluginapi.ExecutorStreamResponse, error) {
								resp, err := innerExecute(ctx, req)
								if err != nil {
									return pluginapi.ExecutorStreamResponse{}, err
								}
								chunks := make(chan pluginapi.ExecutorStreamChunk, 2)
								chunks <- pluginapi.ExecutorStreamChunk{
									Payload: append(
										append(
											[]byte("data: "), resp.Payload...,
										), '\n', '\n',
									),
								}
								chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte("data: [DONE]\n\n")}
								close(chunks)
								return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
							},
						},
					}
					outerID := "agent-wrapper-plugin"
					callNested := func(ctx context.Context) ([]byte, error) {
						ctx = withHostCallbackPluginID(ctx, outerID)
						callbackID, closeCallback := host.openCallbackContextForPlugin(ctx, outerID)
						defer closeCallback()
						request, err := json.Marshal(
							rpcHostModelExecutionRequest{
								EntryProtocol: "openai", ExitProtocol: "openai", Model: innerModel,
								Body: []byte(fmt.Sprintf(`{"model":%q}`, innerModel)), Stream: stream,
								HostCallbackID: callbackID,
							},
						)
						if err != nil {
							return nil, err
						}
						method := pluginabi.MethodHostModelExecute
						if stream {
							method = pluginabi.MethodHostModelExecuteStream
						}
						response, err := host.callFromPlugin(context.Background(), method, request)
						if err != nil {
							return nil, err
						}
						if !stream {
							result, err := decodeRPCEnvelope[pluginapi.HostModelExecutionResponse](response)
							if err != nil {
								return nil, err
							}
							return result.Body, nil
						}
						result, err := decodeRPCEnvelope[pluginapi.HostModelStreamResponse](response)
						if err != nil {
							return nil, err
						}
						defer func() { _ = host.modelStreams.close(ctx, result.StreamID) }()
						var output []byte
						for {
							chunk, done, err := host.modelStreams.read(ctx, result.StreamID)
							if err != nil {
								return nil, err
							}
							if chunk.Err != nil {
								return nil, chunk.Err
							}
							output = append(output, chunk.Payload...)
							if done {
								return output, nil
							}
						}
					}
					outer := &executorAdapter{
						host: host, pluginID: outerID, provider: outerID,
						inputFormats: []sdktranslator.Format{
							format,
						}, outputFormats: []sdktranslator.Format{format},
						executor: &fakeExecutor{
							execute: func(ctx context.Context, _ pluginapi.ExecutorRequest) (
								pluginapi.ExecutorResponse,
								error,
							) {
								body, err := callNested(ctx)
								return pluginapi.ExecutorResponse{Payload: body}, err
							},
							executeStream: func(
								ctx context.Context,
								_ pluginapi.ExecutorRequest,
							) (pluginapi.ExecutorStreamResponse, error) {
								chunks := make(chan pluginapi.ExecutorStreamChunk, 1)
								go func() {
									defer close(chunks)
									body, err := callNested(ctx)
									chunks <- pluginapi.ExecutorStreamChunk{Payload: body, Err: err}
								}()
								return pluginapi.ExecutorStreamResponse{Chunks: chunks}, nil
							},
						},
					}
					for _, adapter := range []*executorAdapter{inner, outer} {
						manager.RegisterExecutor(adapter)
						model := innerModel
						if adapter == outer {
							model = outerModel
						}
						auth := &coreauth.Auth{
							ID: "auth-" + model, Provider: adapter.provider, Status: coreauth.StatusActive,
						}
						if _, err := manager.Register(context.Background(), auth); err != nil {
							t.Fatal(err)
						}
						if adapter == inner && scenario.missing {
							continue
						}
						registry.GetGlobalRegistry().RegisterClient(
							auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}},
						)
						t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
					}
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					ctx, finish := handler.GetContextWithCancel(nil, nil, ctx)
					defer finish()
					body := []byte(fmt.Sprintf(`{"model":%q,"stream":%t}`, outerModel, stream))
					var failure bool
					var delivered []byte
					if stream {
						data, _, errs := handler.ExecuteStreamWithAuthManager(
							ctx,
							"openai",
							outerModel,
							body,
							"",
						)
						for data != nil || errs != nil {
							select {
							case payload, ok := <-data:
								if !ok {
									data = nil
								}
								delivered = append(delivered, payload...)
							case err, ok := <-errs:
								if !ok {
									errs = nil
								} else if err != nil {
									failure = true
								}
							case <-ctx.Done():
								t.Fatalf("nested execution did not finish: %v", ctx.Err())
							}
						}
					} else {
						output, _, err := handler.ExecuteWithAuthManager(ctx, "openai", outerModel, body, "")
						delivered, failure = output, err != nil
					}
					if !failed && !strings.Contains(string(delivered), "chatcmpl-inner") {
						t.Fatalf("nested response lost: %s", delivered)
					}
					if failure != failed {
						t.Fatalf("nested outcome failed=%t, want %t", failure, failed)
					}
					stopCtx, stop := context.WithTimeout(context.Background(), time.Second)
					defer stop()
					if err := usageManager.StopContext(stopCtx); err != nil {
						t.Fatal(err)
					}
					if len(records) != 1 {
						t.Fatalf(
							"nested host execution published %d records, want one selected attempt",
							len(records),
						)
					}
					record := <-records
					expectedProvider, expectedModel := inner.provider, innerModel
					if scenario.missing {
						expectedProvider, expectedModel = outer.provider, outerModel
					}
					if record.Provider != expectedProvider || record.AuthID != "auth-"+expectedModel ||
						record.Stream != stream ||
						record.Failed != failed {
						t.Fatalf("nested usage belongs to the wrong attempt: %#v", record)
					}
					if failed {
						if !scenario.missing && !strings.Contains(record.Fail.Body, "inner failure") {
							t.Fatalf("inner failure lost: %#v", record.Fail)
						}
					} else if record.Detail.InputTokens !=
						5 ||
						record.Detail.OutputTokens !=
							5 ||
						record.Detail.TotalTokens !=
							10 {
						t.Fatalf("nested usage = %#v, want 5/5/10", record.Detail)
					}
				},
			)
		}
	}
}
