package auth

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/google/uuid"
)

// noinspection GoErrorStringFormat
func originalWindowsCodexTransportError() error {
	return &url.Error{
		Op: "Post", URL: "https://chatgpt.com/backend-api/codex/responses",
		Err: fmt.Errorf(
			"tls: TLS handshake: %w", &net.OpError{
				Op: "read", Net: "tcp",
				Source: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1},
				Addr:   &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2},
				Err: errors.New(
					"wsarecv: A connection attempt failed because the connected party did n" +
						"ot properly respond after a period of time, or established connection " +
						"failed because connected host has failed to respond.",
				),
			},
		),
	}
}

func TestOriginalTransportRetryBoundaryInputs(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetHomeClientProvider(func() HomeClient { return nil })
	manager.SetRetryConfig(1, 0, 0)
	model, id := "gpt-transport-retry-"+uuid.NewString(), "transport-retry-"+uuid.NewString()
	registerRequestScopedTestAuth(t, manager, &Auth{ID: id, Provider: "codex", Status: StatusActive}, model)
	manager.RegisterExecutor(&roundExecutor{provider: "codex"})
	windows := originalWindowsCodexTransportError()
	for _, sample := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "windows tls handshake", err: windows},
		{
			name: "dial refused", want: true, err: &url.Error{
				Op: "Post", URL: "https://chatgpt.com/backend-api/codex/responses",
				Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED},
			},
		},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF},
		{name: "unauthorized", err: &Error{HTTPStatus: http.StatusUnauthorized, Message: "unauthorized"}},
		{name: "canceled", err: context.Canceled},
		{
			name: "certificate", err: &url.Error{
				Op: "Post", URL: "https://chatgpt.com/backend-api/codex/responses", Err: x509.UnknownAuthorityError{},
			},
		},
		{
			name: "explicit not sent", want: true, err: errorWithState(
				&Error{Message: windows.Error()}, errorState{
					cause:         windows,
					DispatchPhase: core.DispatchPhaseNotSent, Outcome: core.ExecutionOutcomeRejectedBeforeProcessing,
					RetryDirective: core.RetryDirectiveNextAuth, EffectScope: core.StateEffectScopeRequest,
				},
			),
		},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				wait, retry := manager.shouldRetryAfterError(
					t.Context(), core.Options{}, sample.err, 0, []string{"codex"}, model, 0,
				)
				if retry != sample.want || wait != 0 {
					t.Fatalf("retry=%t wait=%v, want retry=%t and no wait", retry, wait, sample.want)
				}
				if _, retry := manager.shouldRetryAfterError(
					t.Context(), core.Options{}, sample.err, 1, []string{"codex"}, model, 0,
				); retry {
					t.Fatal("transport failure exceeded the configured additional round")
				}
			},
		)
	}
}

func TestOriginalTransportFailureStateInputs(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	previousTransient := transientErrorCooldownSeconds.Load()
	SetTransientErrorCooldownSeconds(5)
	t.Cleanup(func() { transientErrorCooldownSeconds.Store(previousTransient) })
	for _, sample := range []struct {
		name string
		err  error
	}{
		{name: "typed tls handshake", err: originalWindowsCodexTransportError()},
		{name: "connection reset message", err: &Error{Message: "connection reset"}},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				manager := NewManager(nil, nil, nil)
				id := "auth-transport-" + uuid.NewString()
				registerRequestScopedTestAuth(
					t, manager, &Auth{ID: id, Provider: "codex", Status: StatusActive}, "gpt-6-astra",
				)
				current, _ := manager.GetByID(id)
				manager.MarkResult(
					t.Context(),
					executionResult(current, "codex", "gpt-6-astra", sample.err, core.DispatchPhaseMaybeSent),
				)
				assertNoCooldown(t, manager, id, "gpt-6-astra")
			},
		)
	}
}

func TestOriginalTransportRequestLifetimeInputs(t *testing.T) {
	for _, sample := range []struct {
		name  string
		home  bool
		retry int
	}{
		{name: "local retry budget", retry: 1},
		{name: "local independent request"},
		{name: "home retry budget", home: true, retry: 1},
	} {
		for _, kind := range []string{"execute", "count", "stream"} {
			t.Run(
				sample.name+"/"+kind, func(t *testing.T) {
					manager := NewManager(nil, nil, nil)
					manager.SetRetryConfig(sample.retry, 0, 0)
					provider, model := "codex", "gpt-6-astra-"+uuid.NewString()
					id := "codex-transport-" + uuid.NewString()
					if sample.retry == 0 {
						id = "codex-transport-poison-" + uuid.NewString()
					}
					registry := executionregistry.New()
					var dispatcher *roundsHomeDispatcher
					if sample.home {
						provider, model, id = "home-retry-contract", "gpt", "home-retry-a"
						manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
						dispatcher = &roundsHomeDispatcher{
							provider: provider, retry: &sample.retry, limits: map[string]int{id: sample.retry},
						}
						manager.PublishHomeDispatch(dispatcher, registry, 1)
					} else {
						manager.SetHomeClientProvider(func() HomeClient { return nil })
						registerRequestScopedTestAuth(
							t, manager, &Auth{ID: id, Provider: provider, Status: StatusActive}, model,
						)
					}
					failure := originalWindowsCodexTransportError()
					calls := 0
					manager.RegisterExecutor(
						&homeExecutionExecutor{
							id: provider, execute: func(context.Context, *Auth, core.Request, core.Options) error {
								calls++
								if calls == 1 {
									return failure
								}
								return nil
							},
						},
					)
					_, firstErr := invokeOriginalRetryRound(t, manager, kind, provider, model)
					if !errors.Is(firstErr, failure) || calls != 1 {
						t.Fatalf(
							"first request error=%v calls=%d, want original failure without replay", firstErr, calls,
						)
					}
					var selectionError *Error
					if errors.As(
						firstErr, &selectionError,
					) && (selectionError.Code == "auth_unavailable" || selectionError.Code == "auth_not_found") {
						t.Fatalf("transport failure became a scheduler error: %v", firstErr)
					}
					if !sample.home {
						assertNoCooldown(t, manager, id, model)
					}
					response, err := invokeOriginalRetryRound(t, manager, kind, provider, model)
					if err != nil || string(response.Payload) != "ok" || calls != 2 {
						t.Fatalf("independent request payload=%q error=%v calls=%d", response.Payload, err, calls)
					}
					if sample.home {
						if len(dispatcher.constraints) != 2 || dispatcher.constraints[0].RetryRound != 0 ||
							dispatcher.constraints[1].RetryRound != 0 ||
							len(registry.FreezeInFlight(time.Now()).Executions) != 0 {
							t.Fatalf("Home request lifetime constraints=%+v", dispatcher.constraints)
						}
					} else {
						assertNoCooldown(t, manager, id, model)
					}
				},
			)
		}
	}
}

func TestOriginalTransportPreparationUsesRetryAuthority(t *testing.T) {
	for _, home := range []bool{false, true} {
		for _, kind := range []string{"execute", "count", "stream"} {
			for _, noRetry := range []bool{false, true} {
				t.Run(
					fmt.Sprintf("home=%t/%s/no_retry=%t", home, kind, noRetry), func(t *testing.T) {
						manager := NewManager(nil, &FillFirstSelector{}, nil)
						manager.SetRetryConfig(1, 0, 2)
						const provider, model = "codex", "gpt-6-astra"
						ids := []string{"codex-prepare-a", "codex-prepare-b"}
						if home {
							manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
							manager.PublishHomeDispatch(
								&roundsHomeDispatcher{
									provider: provider, limits: map[string]int{ids[0]: 1, ids[1]: 1},
								}, executionregistry.New(), 1,
							)
						} else {
							manager.SetHomeClientProvider(func() HomeClient { return nil })
							for _, id := range ids {
								registerRequestScopedTestAuth(
									t, manager, &Auth{ID: id, Provider: provider, Status: StatusActive}, model,
								)
							}
						}
						failure := originalWindowsCodexTransportError()
						var prepareFailure = failure
						if noRetry {
							prepareFailure = errorWithState(
								&Error{Message: failure.Error()}, errorState{
									cause:          failure,
									DispatchPhase:  core.DispatchPhaseNotSent,
									Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
									RetryDirective: core.RetryDirectiveNoRetry,
									EffectScope:    core.StateEffectScopeRequest,
								},
							)
						}
						var prepared, executed []string
						manager.RegisterExecutor(
							&homeExecutionExecutor{
								id: provider, prepare: func(_ context.Context, selected *Auth) (*Auth, error) {
									prepared = append(prepared, selected.ID)
									if selected.ID == ids[0] {
										return selected, prepareFailure
									}
									return selected, nil
								},
								execute: func(_ context.Context, selected *Auth, _ core.Request, _ core.Options) error {
									executed = append(executed, selected.ID)
									return nil
								},
							},
						)
						response, err := invokeOriginalRetryRound(t, manager, kind, provider, model)
						if noRetry {
							if !errors.Is(err, prepareFailure) || !reflect.DeepEqual(
								prepared, ids[:1],
							) || len(executed) != 0 {
								t.Fatalf("no_retry error=%v prepared=%v executed=%v", err, prepared, executed)
							}
						} else if err != nil || string(response.Payload) != "ok" || !reflect.DeepEqual(
							prepared, ids,
						) || !reflect.DeepEqual(executed, ids[1:]) {
							t.Fatalf(
								"prepare retry payload=%q error=%v prepared=%v executed=%v", response.Payload, err,
								prepared, executed,
							)
						}
					},
				)
			}
		}
	}
}
