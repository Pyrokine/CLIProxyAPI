package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type homeCompletionExecutor struct {
	schedulerTestExecutor
	observe func(context.Context, executor.Options)
	wait    bool
}

func (e *homeCompletionExecutor) Execute(
	ctx context.Context,
	_ *Auth,
	_ executor.Request,
	opts executor.Options,
) (executor.Response, error) {
	e.observe(ctx, opts)
	if e.wait {
		<-ctx.Done()
		return executor.Response{}, ctx.Err()
	}
	return executor.Response{Payload: []byte("complete")}, nil
}

func (e *homeCompletionExecutor) CountTokens(
	ctx context.Context,
	auth *Auth,
	request executor.Request,
	opts executor.Options,
) (executor.Response, error) {
	return e.Execute(ctx, auth, request, opts)
}

func (e *homeCompletionExecutor) ExecuteStream(
	ctx context.Context,
	_ *Auth,
	_ executor.Request,
	opts executor.Options,
) (*executor.StreamResult, error) {
	e.observe(ctx, opts)
	chunks := make(chan executor.StreamChunk, 2)
	chunks <- executor.StreamChunk{Payload: []byte("complete")}
	if e.wait {
		go func() {
			<-ctx.Done()
			chunks <- executor.StreamChunk{Err: ctx.Err()}
			close(chunks)
		}()
	} else {
		close(chunks)
	}
	return &executor.StreamResult{Chunks: chunks}, nil
}

func TestHomePublishedRegistryLifetimeUsesActiveBundle(t *testing.T) {
	var absent *Manager
	if absent.PublishHomeDispatch(
		nil, nil, 0,
	) != nil || absent.HomeDispatchBundle() != nil || absent.ClearHomeDispatchBundle(nil) {
		t.Fatal("nil manager exposed a Home execution lifetime")
	}
	for _, scoped := range []bool{false, true} {
		t.Run(
			fmt.Sprintf("service-owned=%t", scoped), func(t *testing.T) {
				payload := []byte((`{"auth":{"id":"registry-auth","provider":"test"},"model":"registry-model",` +
					`"auth_index":"registry-auth","concurrency":{"accounted":true,"credential_id":"registry-` +
					`auth","model":"registry-model"}}`))
				global := &context1MHomeDispatcher{payloads: [][]byte{payload}}
				service := &context1MHomeDispatcher{payloads: [][]byte{payload}}
				selected := global
				manager := NewManager(nil, nil, nil)
				manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
				if scoped {
					selected = service
					manager.SetHomeClientProvider(func() HomeClient { return service })
				}
				first := executionregistry.New()
				bundle := manager.PublishHomeDispatch(selected, first, 0)
				if bundle == nil || manager.HomeDispatchBundle() != bundle {
					t.Fatal("Home client and registry were not published together")
				}
				if manager.PublishHomeDispatch(selected, nil, 1) != nil ||
					manager.HomeDispatchBundle() != bundle {
					t.Fatal("nil registry replaced an existing Home lifetime")
				}
				var selection *HomeDispatchSelection
				var releases atomic.Int32
				first.SetReleaseSink(func(executionregistry.ReleaseGroup, int64) { releases.Add(1) })
				manager.RegisterExecutor(
					&homeExecutionExecutor{
						id: "test",
						execute: func(_ context.Context, _ *Auth, _ executor.Request, opts executor.Options) error {
							selection, _ = opts.ExecutionLifecycle.(*HomeDispatchSelection)
							if active := first.FreezeInFlight(time.Now()).Executions; len(active) != 1 ||
								!active[0].Accounted {
								return fmt.Errorf(
									"published registry did not account the Manager execution: %+v", active,
								)
							}
							return nil
						},
					},
				)
				if _, err := manager.Execute(
					t.Context(), []string{"test"}, executor.Request{Model: "registry-model"}, executor.Options{},
				); err != nil {
					t.Fatal(err)
				}
				if selected.calls != 1 || scoped && global.calls != 0 || selection == nil ||
					selection.Active() ||
					releases.Load() != 1 {
					t.Fatal("published Home lifetime lost dispatch ownership or completion")
				}
				current := manager.PublishHomeDispatch(selected, executionregistry.New(), 17)
				if manager.ClearHomeDispatchBundle(bundle) || manager.HomeDispatchBundle() != current {
					t.Fatal("old bundle cleared the replacement Home lifetime")
				}
				if !manager.ClearHomeDispatchBundle(current) || manager.HomeDispatchBundle() != nil ||
					manager.ClearHomeDispatchBundle(current) {
					t.Fatal("matching bundle was not cleared exactly once")
				}
				if manager.PublishHomeDispatch(nil, first, 18) != nil || manager.HomeDispatchBundle() != nil {
					t.Fatal("nil Home client published an execution lifetime")
				}
			},
		)
	}
}

// noinspection GoDirectComparisonOfErrors
func TestHomeOriginalDistinctSessionCompletionAndClose(t *testing.T) {
	for _, sessions := range []int{1, 16, 64} {
		for _, mode := range []string{"execute", "count", "stream"} {
			for _, retained := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("sessions=%d/%s/retained=%t", sessions, mode, retained), func(t *testing.T) {
						type sessionState struct {
							selection *HomeDispatchSelection
							attempt   context.Context
							closes    atomic.Int32
						}
						states := make(map[string]*sessionState, sessions)
						auths := make([]Auth, sessions)
						for index := range sessions {
							states[fmt.Sprintf("session-%d", index)] = &sessionState{}
							auths[index] = Auth{
								ID: "home-session-auth", Provider: "test", Status: StatusActive,
								Attributes: map[string]string{"websockets": "true"},
							}
						}
						manager := NewManager(nil, nil, nil)
						manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
						t.Cleanup(func() { manager.CloseExecutionSession(CloseAllExecutionSessionsID) })
						registry := executionregistry.New()
						dispatcher := &accountedHomeExecutionDispatcher{auths: auths}
						manager.PublishHomeDispatch(dispatcher, registry, 1)
						var releases atomic.Int32
						registry.SetReleaseSink(func(executionregistry.ReleaseGroup, int64) { releases.Add(1) })
						manager.RegisterExecutor(
							&homeExecutionExecutor{
								id: "test", execute: func(
									ctx context.Context,
									_ *Auth,
									_ executor.Request,
									opts executor.Options,
								) error {
									sessionID, _ := opts.Metadata[executor.ExecutionSessionMetadataKey].(string)
									state := states[sessionID]
									selection, valid := opts.ExecutionLifecycle.(*HomeDispatchSelection)
									if state == nil || !valid || !selection.Active() {
										return fmt.Errorf("session %q has no active Home selection", sessionID)
									}
									state.selection, state.attempt = selection, ctx
									if retained {
										selection.Retain()
									}
									return selection.Bind(func() error { state.closes.Add(1); return nil })
								},
							},
						)
						ctx := executor.WithDownstreamWebsocket(t.Context())
						var workers sync.WaitGroup
						for sessionID := range states {
							workers.Go(
								func() {
									opts := executor.Options{
										Metadata: map[string]any{
											executor.ExecutionSessionMetadataKey: sessionID,
										},
									}
									req := executor.Request{Model: "home-session-model"}
									var err error
									switch mode {
									case "execute":
										_, err = manager.Execute(ctx, []string{"test"}, req, opts)
									case "count":
										_, err = manager.ExecuteCount(ctx, []string{"test"}, req, opts)
									case "stream":
										opts.Stream = true
										var result *executor.StreamResult
										result, err = manager.ExecuteStream(ctx, []string{"test"}, req, opts)
										if err == nil {
											for chunk := range result.Chunks {
												err = errors.Join(err, chunk.Err)
											}
										}
									}
									if err != nil {
										t.Errorf("session %s execution: %v", sessionID, err)
									}
								},
							)
						}
						workers.Wait()
						if t.Failed() {
							return
						}
						manager.homeSessionLocks.Range(
							func(key, _ any) bool {
								t.Errorf(
									"completed session lock retained: %v", key,
								)
								return true
							},
						)
						wantActive := 0
						if retained {
							wantActive = sessions
						}
						if active := registry.FreezeInFlight(time.Now()).Executions; len(active) != wantActive {
							t.Fatalf("active executions=%d, want %d", len(active), wantActive)
						}
						for sessionID, state := range states {
							_, cached := manager.GetExecutionSessionAuthByID(sessionID, "home-session-auth")
							if state.selection == nil || state.selection.Active() != retained || cached != retained ||
								state.attempt.Err() != context.Canceled ||
								state.selection.attemptCancels.Len() != 0 {
								t.Fatalf(
									"session %s retained=%t cached=%t selection=%+v",
									sessionID,
									retained,
									cached,
									state.selection,
								)
							}
						}
						manager.CloseExecutionSession("session-0")
						for sessionID, state := range states {
							wantActive := retained && sessionID != "session-0"
							_, cached := manager.GetExecutionSessionAuthByID(sessionID, "home-session-auth")
							if state.selection.Active() != wantActive || cached != wantActive {
								t.Fatalf(
									"closing session-0 changed session %s: active=%t cached=%t", sessionID,
									state.selection.Active(), cached,
								)
							}
						}
						manager.CloseExecutionSession(CloseAllExecutionSessionsID)
						for sessionID, state := range states {
							state.selection.End("repeated close")
							_, cached := manager.GetExecutionSessionAuthByID(sessionID, "home-session-auth")
							if state.selection.Active() || cached || state.closes.Load() != 1 {
								t.Fatalf(
									"closed session %s: active=%t cached=%t closes=%d", sessionID,
									state.selection.Active(), cached, state.closes.Load(),
								)
							}
						}
						if active := registry.FreezeInFlight(time.Now()).Executions; len(active) != 0 ||
							int(releases.Load()) != sessions ||
							int(dispatcher.calls.Load()) != sessions {
							t.Fatalf(
								"final executions=%d releases=%d dispatches=%d, sessions=%d", len(active),
								releases.Load(), dispatcher.calls.Load(), sessions,
							)
						}
						manager.homeSessionLocks.Range(
							func(key, _ any) bool {
								t.Errorf(
									"closed session lock retained: %v", key,
								)
								return true
							},
						)
					},
				)
			}
		}
	}
}

// noinspection GoDirectComparisonOfErrors
func TestHomeManagerCompletionAndCancellationReleasesSelection(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		for _, canceled := range []bool{false, true} {
			t.Run(
				fmt.Sprintf("%s/canceled=%t", mode, canceled), func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					t.Cleanup(cancel)
					registry := executionregistry.New()
					dispatcher := &accountedHomeExecutionDispatcher{
						auths: []Auth{
							{
								ID: "home-completion-auth", Provider: "test", Status: StatusActive,
							},
						},
					}
					hook := newStreamDeliveryResultHook()
					manager := NewManager(nil, nil, hook)
					manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
					manager.PublishHomeDispatch(dispatcher, registry, 1)
					var closes atomic.Int32
					releases := make(chan executionregistry.ReleaseGroup, 2)
					registry.SetReleaseSink(
						func(group executionregistry.ReleaseGroup, _ int64) { releases <- group },
					)
					started := make(chan struct{})
					var selection *HomeDispatchSelection
					var attempt context.Context
					manager.RegisterExecutor(
						&homeCompletionExecutor{
							wait: canceled, observe: func(callCtx context.Context, opts executor.Options) {
								defer close(started)
								attempt = callCtx
								var valid bool
								selection, valid = opts.ExecutionLifecycle.(*HomeDispatchSelection)
								if !valid || !selection.Active() || selection.attemptCancels.Len() != 1 {
									t.Error("executor did not receive its active Home selection and attempt")
									return
								}
								if err := selection.Bind(func() error { closes.Add(1); return nil }); err != nil {
									t.Error(err)
								}
								active := registry.FreezeInFlight(time.Now()).Executions
								if len(active) != 1 || active[0].CredentialID != "home-completion-auth" ||
									!active[0].Accounted {
									t.Errorf("active Home executions=%+v", active)
								}
							},
						},
					)
					done := make(chan error, 1)
					go func() {
						req := executor.Request{Model: "home-completion-model"}
						var err error
						switch mode {
						case "execute":
							_, err = manager.Execute(ctx, []string{"test"}, req, executor.Options{})
						case "count":
							_, err = manager.ExecuteCount(ctx, []string{"test"}, req, executor.Options{})
						case "stream":
							var result *executor.StreamResult
							result, err = manager.ExecuteStream(
								ctx, []string{"test"}, req, executor.Options{Stream: true},
							)
							if err == nil {
								for chunk := range result.Chunks {
									err = errors.Join(err, chunk.Err)
									if canceled {
										cancel()
									}
								}
							}
						}
						done <- err
					}()
					select {
					case <-started:
					case <-time.After(time.Second):
						t.Fatal("Manager did not reach the Home executor")
					}
					if canceled && mode != "stream" {
						cancel()
					}
					select {
					case err := <-done:
						var wantError error
						if canceled && mode != "stream" {
							wantError = context.Canceled
						}
						if !errors.Is(err, wantError) {
							t.Fatalf("execution error=%v, want %v", err, wantError)
						}
					case <-time.After(time.Second):
						t.Fatal("Manager did not finish the Home execution")
					}
					select {
					case result := <-hook.result:
						if result.Success == canceled ||
							canceled &&
								(result.Error ==
									nil ||
									result.Disposition.EffectScope !=
										executor.StateEffectScopeRequest) {
							t.Fatalf("Manager completion result=%+v, canceled=%t", result, canceled)
						}
					case <-time.After(time.Second):
						t.Fatal("Manager did not publish its completion result")
					}
					if selection == nil || selection.Active() || attempt.Err() != context.Canceled ||
						selection.attemptCancels.Len() != 0 || closes.Load() != 1 {
						t.Fatal(
							"completed Manager call retained its Home selection, attempt, or bound resource",
						)
					}
					if active := registry.FreezeInFlight(time.Now()).Executions; len(active) != 0 {
						t.Fatalf("completed Manager call retained executions=%+v", active)
					}
					select {
					case group := <-releases:
						if group != (executionregistry.ReleaseGroup{
							CredentialID: "home-completion-auth", Model: "home-completion-model",
						}) {
							t.Fatalf("released group=%+v", group)
						}
					default:
						t.Fatal("Manager did not release the accounted Home execution")
					}
					selection.End("repeated end")
					select {
					case group := <-releases:
						t.Fatalf("duplicate release=%+v", group)
					default:
					}
					if dispatcher.calls.Load() != 1 || closes.Load() != 1 {
						t.Fatal("completion repeated a Home dispatch or resource close")
					}
				},
			)
		}
	}
}
