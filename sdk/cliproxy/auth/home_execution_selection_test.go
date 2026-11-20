package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type homeExecutionDispatcher struct {
	accountedHomeExecutionDispatcher
	before func()
	legacy bool
}

func (d *homeExecutionDispatcher) RPopAuth(
	ctx context.Context,
	model, userAgent string,
	headers http.Header,
	count int,
) ([]byte, error) {
	if d.before != nil {
		d.before()
	}
	payload, err := d.accountedHomeExecutionDispatcher.RPopAuth(ctx, model, userAgent, headers, count)
	if err == nil && d.legacy {
		return json.Marshal(homeAuthDispatchResponse{Auth: d.auths[d.calls.Load()-1]})
	}
	return payload, err
}

func TestHomeOriginalRetainedExecutionSelection(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		for _, changed := range []bool{false, true} {
			for _, mode := range []string{"execute", "stream"} {
				name := mode
				if pinned {
					name += "/pinned"
				} else {
					name += "/unpinned"
				}
				if changed {
					name += "/target changed"
				} else {
					name += "/target retained"
				}
				t.Run(
					name, func(t *testing.T) {
						manager := NewManager(nil, nil, nil)
						manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
						defer manager.CloseExecutionSession("session-1")
						credential := Auth{
							ID: "home-auth", Provider: "test", Status: StatusActive,
							Attributes: map[string]string{"websockets": "true"},
							Metadata:   map[string]any{"access_token": "synthetic"},
						}
						dispatcher := &homeExecutionDispatcher{
							auths: []Auth{
								credential, credential,
							},
						}
						registry := executionregistry.New()
						manager.PublishHomeDispatch(dispatcher, registry, 1)
						var closes atomic.Int32
						var selections []*HomeDispatchSelection
						dispatcher.before = func() {
							if dispatcher.calls.Load() > 0 &&
								(len(selections) != 1 || selections[0].Active() || closes.Load() != 1) {
								t.Fatal(
									"the previous Home selection or bound resource remained active during redispatch",
								)
							}
						}
						calls := 0
						manager.RegisterExecutor(
							&homeExecutionExecutor{
								id: "test", execute: func(
									_ context.Context,
									_ *Auth,
									_ executor.Request,
									opts executor.Options,
								) error {
									calls++
									selection, ok := opts.ExecutionLifecycle.(*HomeDispatchSelection)
									if !ok || !selection.Active() {
										t.Fatal("executor did not receive its live Home selection")
									}
									if len(selections) == 0 || selections[len(selections)-1] != selection {
										selections = append(selections, selection)
										if err := selection.Bind(func() error { closes.Add(1); return nil }); err !=
											nil {
											t.Fatal(err)
										}
									}
									selection.Retain()
									return nil
								},
							},
						)
						ctx := executor.WithDownstreamWebsocket(t.Context())
						opts := executor.Options{
							Metadata: map[string]any{executor.ExecutionSessionMetadataKey: "session-1"},
						}
						if pinned {
							opts.Metadata[executor.PinnedAuthMetadataKey] = credential.ID
						}
						for turn := range 2 {
							req := executor.Request{Model: "model-a"}
							if changed && turn == 1 {
								req.Model = "model-b"
							}
							if mode == "stream" {
								result, err := manager.ExecuteStream(ctx, []string{"test"}, req, opts)
								if err != nil {
									t.Fatal(err)
								}
								readOpenAICompatStreamPayload(t, result)
							} else if _, err := manager.Execute(ctx, []string{"test"}, req, opts); err != nil {
								t.Fatal(err)
							}
							if auth, ok := manager.GetExecutionSessionAuthByID(
								"session-1", credential.ID,
							); !ok || auth == nil {
								t.Fatal("retained selection did not publish its owned session credential")
							}
						}
						wantSelections := 1
						if changed {
							wantSelections = 2
						}
						if calls != 2 || int(dispatcher.calls.Load()) != wantSelections ||
							len(selections) != wantSelections {
							t.Fatalf(
								"calls=%d dispatches=%d selections=%d want selections=%d", calls,
								dispatcher.calls.Load(), len(selections), wantSelections,
							)
						}
						manager.CloseExecutionSession("session-1")
						if _, ok := manager.GetExecutionSessionAuthByID("session-1", credential.ID); ok {
							t.Fatal("closing the session retained its Home credential")
						}
						for _, selection := range selections {
							if selection.Active() {
								t.Fatal("closing the session retained an active Home selection")
							}
							selection.End("repeated close")
						}
						if int(closes.Load()) != wantSelections {
							t.Fatalf("bound resources closed=%d want=%d", closes.Load(), wantSelections)
						}
						if active := registry.FreezeInFlight(time.Now()).Executions; len(active) != 0 {
							t.Fatalf("closed session retained executions=%+v", active)
						}
					},
				)
			}
		}
	}
}

// noinspection GoDirectComparisonOfErrors
func TestHomeOriginalRetryEndsSelectionBeforeFreshDispatch(t *testing.T) {
	for _, mode := range []string{"execute", "count", "stream"} {
		for _, status := range []int{http.StatusUpgradeRequired, http.StatusBadGateway} {
			for _, retryAllowed := range []bool{false, true} {
				name := fmt.Sprintf("%s/status=%d/retry=%t", mode, status, retryAllowed)
				t.Run(
					name, func(t *testing.T) {
						manager := NewManager(nil, nil, nil)
						manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
						manager.SetRetryConfig(0, time.Second, 2)
						defer manager.CloseExecutionSession("session-426")
						dispatcher := &homeExecutionDispatcher{
							auths: []Auth{
								{ID: "home-auth-a", Provider: "test", Status: StatusActive},
								{ID: "home-auth-b", Provider: "test", Status: StatusActive},
							},
						}
						registry := executionregistry.New()
						releases := make(chan executionregistry.ReleaseGroup, 4)
						registry.SetReleaseSink(
							func(
								group executionregistry.ReleaseGroup,
								_ int64,
							) {
								releases <- group
							},
						)
						manager.PublishHomeDispatch(dispatcher, registry, 1)
						var first *HomeDispatchSelection
						var firstContext context.Context
						var closes atomic.Int32
						dispatcher.before = func() {
							if dispatcher.calls.Load() == 1 &&
								(first ==
									nil ||
									first.Active() ||
									firstContext.Err() !=
										context.Canceled ||
									closes.Load() !=
										1) {
								t.Fatal("failed attempt was not ended and closed before the next Home dispatch")
							}
						}
						failure := &Error{HTTPStatus: status, Message: "websocket upgrade required"}
						if status == http.StatusBadGateway {
							failure.Message = "upstream failed"
						}
						if retryAllowed {
							failure.executionState().Outcome = executor.ExecutionOutcomeRejectedBeforeProcessing
							failure.executionState().RetryDirective = executor.RetryDirectiveNextAuth
							failure.executionState().EffectScope = executor.StateEffectScopeCredential
						}
						var authIDs []string
						manager.RegisterExecutor(
							&homeExecutionExecutor{
								id: "test", execute: func(
									ctx context.Context,
									auth *Auth,
									_ executor.Request,
									opts executor.Options,
								) error {
									authIDs = append(authIDs, auth.ID)
									selection, ok := opts.ExecutionLifecycle.(*HomeDispatchSelection)
									if !ok || !selection.Active() {
										t.Fatal("retry did not own a live selection")
									}
									if len(authIDs) == 1 {
										first, firstContext = selection, ctx
										selection.Retain()
										if err := selection.Bind(func() error { closes.Add(1); return nil }); err !=
											nil {
											t.Fatal(err)
										}
										return failure
									}
									return nil
								},
							},
						)
						ctx := executor.WithDownstreamWebsocket(t.Context())
						opts := executor.Options{
							Metadata: map[string]any{executor.ExecutionSessionMetadataKey: "session-426"},
						}
						req := executor.Request{Model: "model-a"}
						var err error
						switch mode {
						case "execute":
							_, err = manager.Execute(ctx, []string{"test"}, req, opts)
						case "count":
							_, err = manager.ExecuteCount(ctx, []string{"test"}, req, opts)
						case "stream":
							var result *executor.StreamResult
							result, err = manager.ExecuteStream(ctx, []string{"test"}, req, opts)
							if err == nil {
								readOpenAICompatStreamPayload(t, result)
							}
						}
						wantAttempts := 1
						if retryAllowed {
							wantAttempts = 2
						}
						if (err == nil) != retryAllowed || !retryAllowed && !errors.Is(
							err, failure,
						) ||
							len(authIDs) !=
								wantAttempts ||
							authIDs[0] !=
								"home-auth-a" ||
							retryAllowed &&
								authIDs[1] !=
									"home-auth-b" ||
							int(dispatcher.calls.Load()) !=
								wantAttempts {
							t.Fatalf(
								"retry error=%v auths=%v dispatches=%d",
								err,
								authIDs,
								dispatcher.calls.Load(),
							)
						}
						if first.Active() || closes.Load() != 1 || firstContext.Err() != context.Canceled {
							t.Fatal("failed selection retained its resource or attempt context")
						}
						groups := make(map[executionregistry.ReleaseGroup]int)
						for range wantAttempts {
							select {
							case group := <-releases:
								groups[group]++
							case <-time.After(time.Second):
								t.Fatal("Home retry did not release every attempt")
							}
						}
						for _, id := range authIDs {
							if groups[executionregistry.ReleaseGroup{CredentialID: id, Model: "model-a"}] != 1 {
								t.Fatalf("released groups=%+v", groups)
							}
						}
						select {
						case group := <-releases:
							t.Fatalf("duplicate retry release=%+v", group)
						default:
						}
					},
				)
			}
		}
	}
}

func TestHomeOriginalModelPoolUsesOneAttemptPerSelection(t *testing.T) {
	for _, mode := range []string{"execute", "count"} {
		for _, retryAllowed := range []bool{false, true} {
			name := mode + "/unknown outcome"
			if retryAllowed {
				name = mode + "/explicitly rejected before processing"
			}
			t.Run(
				name, func(t *testing.T) {
					manager := NewManager(nil, nil, nil)
					manager.SetConfig(
						&config.Config{
							Home: config.HomeConfig{
								Enabled: true,
							}, OpenAICompatibility: []config.OpenAICompatibility{
								{
									Name: "pool", Models: []config.OpenAICompatibilityModel{
										{
											Name:  "upstream-a",
											Alias: "requested",
										}, {Name: "upstream-b", Alias: "requested"},
									},
								},
							},
						},
					)
					dispatcher := &homeExecutionDispatcher{
						legacy: true,
						auths: []Auth{
							{
								ID: "home-auth-a", Provider: "home-pool", Status: StatusActive,
								Attributes: map[string]string{
									"api_key": "test-key", "compat_name": "pool", "provider_key": "pool",
								},
							},
							{
								ID: "home-auth-b", Provider: "home-pool", Status: StatusActive,
								Attributes: map[string]string{
									"api_key": "test-key", "compat_name": "pool", "provider_key": "pool",
								},
							},
						},
					}
					registry := executionregistry.New()
					manager.PublishHomeDispatch(dispatcher, registry, 1)
					var selections []*HomeDispatchSelection
					var authIDs []string
					dispatcher.before = func() {
						for _, selection := range selections {
							if selection.Active() {
								t.Fatal("model-pool retry redispatched before ending the previous selection")
							}
						}
					}
					failure := &Error{HTTPStatus: http.StatusBadGateway, Message: "upstream failed"}
					if retryAllowed {
						failure.executionState().Outcome = executor.ExecutionOutcomeRejectedBeforeProcessing
						failure.executionState().RetryDirective = executor.RetryDirectiveNextAuth
						failure.executionState().EffectScope = executor.StateEffectScopeCredential
					}
					manager.RegisterExecutor(
						&homeExecutionExecutor{
							id: "pool", execute: func(
								_ context.Context,
								auth *Auth,
								_ executor.Request,
								opts executor.Options,
							) error {
								selection, ok := opts.ExecutionLifecycle.(*HomeDispatchSelection)
								if !ok {
									t.Fatal("model-pool attempt did not receive a selection")
								}
								selections = append(selections, selection)
								authIDs = append(authIDs, auth.ID)
								return failure
							},
						},
					)
					req := executor.Request{Model: "requested"}
					var err error
					if mode == "execute" {
						_, err = manager.Execute(t.Context(), []string{"home-pool"}, req, executor.Options{})
					} else {
						_, err = manager.ExecuteCount(t.Context(), []string{"home-pool"}, req, executor.Options{})
					}
					wantAttempts := 1
					if retryAllowed {
						wantAttempts = 2
					}
					if !errors.Is(
						err, failure,
					) ||
						len(authIDs) !=
							wantAttempts ||
						authIDs[0] !=
							"home-auth-a" ||
						retryAllowed &&
							(authIDs[1] !=
								"home-auth-b" ||
								selections[0] ==
									selections[1]) {
						t.Fatalf(
							"model-pool error=%v upstream invocations=%v selections=%v",
							err,
							authIDs,
							selections,
						)
					}
					if active := registry.FreezeInFlight(time.Now()).Executions; len(active) != 0 {
						t.Fatalf("failed model-pool request retained executions=%+v", active)
					}
				},
			)
		}
	}
}
