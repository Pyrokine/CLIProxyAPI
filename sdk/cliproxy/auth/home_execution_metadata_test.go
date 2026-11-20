package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestHomeOriginalRequestMetadataAcrossExecutionPhases(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		for _, phase := range []string{"complete", "execute failed", "prepare failed"} {
			for _, tier := range []string{"priority", "auto"} {
				t.Run(
					mode+"/"+phase+"/"+tier, func(t *testing.T) {
						hook := &auditPreparationHook{seen: make(chan auditUsageContext, 4)}
						manager := NewManager(nil, nil, hook)
						manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
						dispatcher := &accountedHomeExecutionDispatcher{
							auths: []Auth{
								{
									ID: "home-auth", Provider: "test", Status: StatusActive,
								},
							},
						}
						registry := executionregistry.New()
						manager.PublishHomeDispatch(dispatcher, registry, 1)
						want := auditUsageContext{"client-model", "high", tier, false}
						var prepared, executed auditUsageContext
						prepareCalls, executeCalls := 0, 0
						failure := &Error{HTTPStatus: http.StatusBadRequest, Message: "invalid request"}
						if phase == "prepare failed" {
							failure = &Error{Code: "prepare_failed", Message: "prepare failed"}
						}
						manager.RegisterExecutor(
							&homeExecutionExecutor{
								id: "test",
								prepare: func(ctx context.Context, auth *Auth) (*Auth, error) {
									prepareCalls++
									prepared = auditContext(ctx)
									if phase == "prepare failed" {
										return nil, failure
									}
									return auth, nil
								},
								execute: func(
									ctx context.Context,
									_ *Auth,
									_ executor.Request,
									_ executor.Options,
								) error {
									executeCalls++
									executed = auditContext(ctx)
									if phase == "execute failed" {
										return failure
									}
									return nil
								},
							},
						)
						opts := executor.Options{
							Metadata: map[string]any{
								executor.RequestedModelMetadataKey:  "client-model",
								executor.ReasoningEffortMetadataKey: "high",
								executor.ServiceTierMetadataKey:     tier, executor.GenerateMetadataKey: false,
							},
						}
						req := executor.Request{Model: "model-a"}
						var err error
						switch mode {
						case "execute":
							_, err = manager.Execute(t.Context(), []string{"test"}, req, opts)
						case "count":
							_, err = manager.ExecuteCount(t.Context(), []string{"test"}, req, opts)
						case "stream":
							var result *executor.StreamResult
							result, err = manager.ExecuteStream(t.Context(), []string{"test"}, req, opts)
							if err == nil {
								for chunk := range result.Chunks {
									err = errors.Join(err, chunk.Err)
								}
							}
						}
						if (err == nil) != (phase == "complete") {
							t.Fatalf("execution error=%v, phase=%s", err, phase)
						}
						if prepareCalls != 1 || prepared != want {
							t.Fatalf("prepare calls=%d metadata=%+v want=%+v", prepareCalls, prepared, want)
						}
						if phase == "prepare failed" {
							if executeCalls != 0 {
								t.Fatal("preparation failure reached the executor")
							}
						} else if executeCalls != 1 || executed != want {
							t.Fatalf("execute calls=%d metadata=%+v want=%+v", executeCalls, executed, want)
						}
						select {
						case observed := <-hook.seen:
							if observed != want {
								t.Fatalf("result metadata=%+v want=%+v", observed, want)
							}
						case <-time.After(time.Second):
							t.Fatal("Home did not report the execution result")
						}
						if dispatcher.calls.Load() != 1 {
							t.Fatalf("Home dispatches=%d, want 1", dispatcher.calls.Load())
						}
						ctx, cancel := context.WithTimeout(t.Context(), time.Second)
						defer cancel()
						if err := registry.Drain(ctx); err != nil {
							t.Fatal(err)
						}
					},
				)
			}
		}
	}
}

func TestHomeOriginalUnavailableErrorsReachManagerEntrypoints(t *testing.T) {
	for _, code := range []string{
		"auth_not_found", "auth_unavailable", "refresh_temporarily_unavailable", "home_unavailable",
	} {
		for _, mode := range []string{"execute", "count"} {
			t.Run(
				code+"/"+mode, func(t *testing.T) {
					manager := NewManager(nil, nil, nil)
					manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
					manager.auths["local-auth"] = &Auth{
						ID:       "local-auth",
						Provider: "test",
						Status:   StatusActive,
					}
					dispatcher := &testHomeDispatcher{
						heartbeat: true,
						payload: []byte(
							`{"error":{"type":"` + code + `","message":"synthetic unavailable"}}`,
						),
					}
					registry := executionregistry.New()
					manager.PublishHomeDispatch(dispatcher, registry, 1)
					calls := 0
					manager.RegisterExecutor(
						&homeExecutionExecutor{
							id: "test",
							execute: func(context.Context, *Auth, executor.Request, executor.Options) error {
								calls++
								return nil
							},
						},
					)
					req := executor.Request{Model: "model-a"}
					var err error
					if mode == "count" {
						_, err = manager.ExecuteCount(t.Context(), []string{"test"}, req, executor.Options{})
					} else {
						_, err = manager.Execute(t.Context(), []string{"test"}, req, executor.Options{})
					}
					var detail *Error
					if !errors.As(
						err, &detail,
					) ||
						detail.Code !=
							code ||
						detail.Message !=
							"synthetic unavailable" ||
						detail.StatusCode() !=
							http.StatusServiceUnavailable {
						t.Fatalf("Home unavailable error=%+v, detail=%+v", err, detail)
					}
					if calls != 0 || dispatcher.calls != 1 {
						t.Fatalf(
							"unavailable Home executed local calls=%d, dispatches=%d",
							calls,
							dispatcher.calls,
						)
					}
					if active := registry.FreezeInFlight(time.Now()).Executions; len(active) != 0 {
						t.Fatalf("unavailable Home retained executions=%+v", active)
					}
					ctx, cancel := context.WithTimeout(t.Context(), time.Second)
					defer cancel()
					if errPending := registry.WaitPending(ctx); errPending != nil {
						t.Fatalf("unavailable Home retained pending dispatch: %v", errPending)
					}
				},
			)
		}
	}
}

func TestHomeOriginalAdmissionFailuresNeverExecuteLocalCredential(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		for _, condition := range []string{"Home unavailable", "missing executor", "canceled"} {
			t.Run(
				mode+"/"+condition, func(t *testing.T) {
					manager := NewManager(nil, nil, nil)
					manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
					manager.auths["local-auth"] = &Auth{
						ID:       "local-auth",
						Provider: "test",
						Status:   StatusActive,
					}
					dispatcher := &accountedHomeExecutionDispatcher{}
					if condition != "Home unavailable" {
						dispatcher.auths = []Auth{{ID: "home-auth", Provider: "test", Status: StatusActive}}
					}
					registry := executionregistry.New()
					manager.PublishHomeDispatch(dispatcher, registry, 1)
					calls := 0
					if condition != "missing executor" {
						manager.RegisterExecutor(
							&homeExecutionExecutor{
								id: "test",
								execute: func(context.Context, *Auth, executor.Request, executor.Options) error {
									calls++
									return nil
								},
							},
						)
					}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					if condition == "canceled" {
						cancel()
					}
					req := executor.Request{Model: "model-a"}
					var err error
					switch mode {
					case "execute":
						_, err = manager.Execute(ctx, []string{"test"}, req, executor.Options{})
					case "count":
						_, err = manager.ExecuteCount(ctx, []string{"test"}, req, executor.Options{})
					case "stream":
						_, err = manager.ExecuteStream(
							ctx,
							[]string{"test"},
							req,
							executor.Options{Stream: true},
						)
					}
					if err == nil || calls != 0 {
						t.Fatalf("Home admission failure executed: error=%v calls=%d", err, calls)
					}
					if condition == "canceled" &&
						(!errors.Is(err, context.Canceled) || dispatcher.calls.Load() != 0) {
						t.Fatalf(
							"canceled request reached Home: error=%v dispatches=%d",
							err,
							dispatcher.calls.Load(),
						)
					}
					if active := registry.FreezeInFlight(time.Now()).Executions; len(active) != 0 {
						t.Fatalf("rejected Home execution retained scopes: %+v", active)
					}
				},
			)
		}
	}
}
