package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func newErrorPriorityManager(t *testing.T, homeMode bool, terminal error, cancel context.CancelFunc) (
	*Manager,
	*roundExecutor,
	*Error,
) {
	t.Helper()
	upstream := errorWithState(
		&Error{
			HTTPStatus: http.StatusServiceUnavailable, Message: "synthetic upstream rejection",
		}, errorState{
			DispatchPhase:  coreexecutor.DispatchPhaseResponseStarted,
			Outcome:        coreexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			RetryDirective: coreexecutor.RetryDirectiveNextAuth,
			EffectScope:    coreexecutor.StateEffectScopeRequest,
		},
	)

	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.SetRetryConfig(1, 0, 0)
	provider := &roundExecutor{}
	provider.onCall = func() {
		switch len(provider.calls) {
		case 1:
			provider.failure = upstream
		case 2:
			if cancel != nil {
				cancel()
			}
			provider.failure = terminal
		default:
			t.Fatalf("unexpected attempt after terminal failure: %v", provider.calls)
		}
	}
	manager.RegisterExecutor(provider)
	if homeMode {
		manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
		retry := 1
		dispatcher := &roundsHomeDispatcher{retry: &retry}
		manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
	} else {
		for _, id := range []string{"priority-a", "priority-b"} {
			if _, err := manager.Register(
				WithSkipPersist(t.Context()), &Auth{
					ID: id, Provider: "test", Status: StatusActive,
					Metadata: map[string]any{"disable_cooling": true},
				},
			); err != nil {
				t.Fatal(err)
			}
		}
		registerReservationContextModels(t, "test", "round-model", "priority-a", "priority-b")
	}
	return manager, provider, upstream
}

func checkTerminalErrorAfterUpstreamFailure(t *testing.T, homeMode bool) {
	t.Helper()
	for _, entry := range []string{"execute", "count", "stream"} {
		for _, terminal := range []error{context.Canceled, context.DeadlineExceeded} {
			t.Run(
				entry+"/"+terminal.Error(), func(t *testing.T) {
					ctx, cancel := context.WithCancel(apiKeyReservationTestContext())
					t.Cleanup(cancel)
					var cancelAttempt context.CancelFunc
					if errors.Is(terminal, context.Canceled) {
						cancelAttempt = cancel
					}
					manager, provider, upstream := newErrorPriorityManager(t, homeMode, terminal, cancelAttempt)
					manager.SetRetryConfig(1, 0, 1)
					err := invokeRetryRound(manager, entry, ctx, coreexecutor.Options{})
					if !errors.Is(err, terminal) || errors.Is(err, upstream) || len(provider.calls) != 2 {
						t.Fatalf(
							"error=%v calls=%v; expected terminal=%v after two attempts",
							err,
							provider.calls,
							terminal,
						)
					}
				},
			)
		}
	}
}

func TestManagerRetryPreservesTerminalContextErrorAfterUpstreamFailure(t *testing.T) {
	checkTerminalErrorAfterUpstreamFailure(t, false)
}

func TestHomeRetryPreservesTerminalContextErrorAfterUpstreamFailure(t *testing.T) {
	checkTerminalErrorAfterUpstreamFailure(t, true)
}

func TestHomePreferredUpstreamErrorPreservesCurrentRetryAfter(t *testing.T) {
	for _, entry := range []string{"execute", "count", "stream"} {
		t.Run(
			entry, func(t *testing.T) {
				local := errorWithState(
					&Error{
						Message: "synthetic preparation failure",
					}, errorState{
						DispatchPhase:  coreexecutor.DispatchPhaseNotSent,
						Outcome:        coreexecutor.ExecutionOutcomeRejectedBeforeProcessing,
						RetryDirective: coreexecutor.RetryDirectiveNextAuth,
						EffectScope:    coreexecutor.StateEffectScopeRequest,
					},
				)

				manager, provider, upstream := newErrorPriorityManager(t, true, local, nil)
				retry := 1
				dispatcher := &roundsHomeDispatcher{
					retry: &retry, errorAfter: 2,
					errorPayload: []byte(
						(`{"error":{"type":"model_cooldown","message":"synthetic current Home window","` +
							`retryable":true,"retry_after_ms":10000,"request_retry":1}}`),
					),
				}
				manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 2)
				err := invokeRetryRound(manager, entry, apiKeyReservationTestContext(), coreexecutor.Options{})
				if !errors.Is(err, upstream) || errors.Is(
					err, local,
				) || statusCodeFromError(err) != http.StatusServiceUnavailable {
					t.Errorf("error=%v; expected original upstream cause and status", err)
				}
				if retryAfter := retryAfterFromError(err); retryAfter == nil || *retryAfter != 10*time.Second {
					t.Errorf("retry after=%v; expected current Home delay 10s", retryAfter)
				}
				if header := SafeResponseHeaders(err).Get("Retry-After"); header != "10" {
					t.Errorf("Retry-After=%q; expected 10", header)
				}
				if len(provider.calls) != 2 || len(dispatcher.constraints) != 3 {
					t.Fatalf(
						"attempts=%v dispatches=%v; expected two executions and one final selection",
						provider.calls,
						dispatcher.constraints,
					)
				}
			},
		)
	}
}

func TestManagerPrefersUpstreamCauseAfterLocalFailure(t *testing.T) {
	for _, homeMode := range []bool{false, true} {
		mode := "local"
		if homeMode {
			mode = "Home"
		}
		for _, entry := range []string{"execute", "count", "stream"} {
			for _, phase := range []coreexecutor.DispatchPhase{
				coreexecutor.DispatchPhaseNotSent, coreexecutor.DispatchPhaseMaybeSent,
			} {
				for _, maxAuthAttempts := range []int{0, 1} {
					t.Run(
						fmt.Sprintf("%s/%s/%s/cap=%d", mode, entry, phase, maxAuthAttempts), func(t *testing.T) {
							latest := errorWithState(
								&Error{
									Message: "synthetic final failure",
								}, errorState{
									DispatchPhase:  phase,
									Outcome:        coreexecutor.ExecutionOutcomeUnknown,
									RetryDirective: coreexecutor.RetryDirectiveNoRetry,
									EffectScope:    coreexecutor.StateEffectScopeRequest,
								},
							)

							if phase == coreexecutor.DispatchPhaseNotSent {
								latest.executionState().Outcome = coreexecutor.ExecutionOutcomeRejectedBeforeProcessing
							}
							manager, provider, upstream := newErrorPriorityManager(t, homeMode, latest, nil)
							manager.SetRetryConfig(1, 0, maxAuthAttempts)
							err := invokeRetryRound(
								manager, entry, apiKeyReservationTestContext(), coreexecutor.Options{},
							)
							want := error(upstream)
							if phase == coreexecutor.DispatchPhaseMaybeSent {
								want = latest
							}
							if !errors.Is(err, want) {
								t.Errorf("error=%v; expected cause=%v", err, want)
							}
							disposition := executionDispositionForError(err, phase)
							if disposition.RetryDirective != coreexecutor.RetryDirectiveNoRetry ||
								disposition.DispatchPhase != phase {
								t.Errorf(
									"terminal disposition=%+v; expected latest no_retry and phase=%s",
									disposition,
									phase,
								)
							}
							if len(provider.calls) != 2 {
								t.Fatalf("attempts=%v; expected exactly two", provider.calls)
							}
						},
					)
				}
			}
		}
	}
}

func TestRetryRoundsReleaseObservedCauseAfterCompletion(t *testing.T) {
	manager, _, upstream := newErrorPriorityManager(t, false, nil, nil)
	var rounds []*retryRoundState
	result, err := retryWithCooldown(
		manager,
		apiKeyReservationTestContext(),
		[]string{"test"},
		coreexecutor.Request{Model: "round-model"},
		coreexecutor.Options{},
		func(ctx context.Context, _ []string, _ coreexecutor.Request, _ coreexecutor.Options, _ int) (int, error) {
			state := retryRoundFromContext(ctx)
			rounds = append(rounds, state)
			if len(rounds) == 1 {
				state.observeUpstreamError(upstream, upstream.ExecutionDisposition())
				return 0, upstream
			}
			return 42, nil
		},
	)
	if err != nil || result != 42 || len(rounds) != 2 {
		t.Fatalf("result=%d error=%v rounds=%d; expected successful second round", result, err, len(rounds))
	}
	for _, state := range rounds {
		if state.upstreamErr != nil {
			t.Fatal("completed selection context retains an upstream error body")
		}
	}
}

type errorPriorityResponseError struct {
	*requestScopedDispositionError
	headers http.Header
}

func TestManagerKeepsUncomparableUpstreamErrorEnvelope(t *testing.T) {
	for _, homeMode := range []bool{false, true} {
		for _, entry := range []string{"execute", "count", "stream"} {
			t.Run(
				fmt.Sprintf("Home=%t/%s", homeMode, entry), func(t *testing.T) {
					latest := errorPriorityResponseError{
						requestScopedDispositionError: &requestScopedDispositionError{
							requestScopedTestError: &requestScopedTestError{
								status: http.StatusServiceUnavailable,
								message: `{"type":"response.failed","sequence_number":51,"error":{"message":` +
									`"synthetic upstream ` +
									`failure"}}`,
							},
							disposition: coreexecutor.ExecutionDisposition{
								DispatchPhase:  coreexecutor.DispatchPhaseResponseStarted,
								Outcome:        coreexecutor.ExecutionOutcomeUnknown,
								RetryDirective: coreexecutor.RetryDirectiveNoRetry,
								EffectScope:    coreexecutor.StateEffectScopeRequest,
							},
						},
						headers: http.Header{"X-Synthetic-Request": {t.Name()}},
					}
					manager, provider, _ := newErrorPriorityManager(t, homeMode, latest, nil)
					err := invokeRetryRound(
						manager,
						entry,
						apiKeyReservationTestContext(),
						coreexecutor.Options{},
					)
					if err == nil || err.Error() != latest.Error() ||
						statusCodeFromError(err) != latest.StatusCode() {
						t.Fatalf("error=%v; expected the latest raw upstream envelope", err)
					}
					disposition := executionDispositionForError(err, coreexecutor.DispatchPhaseMaybeSent)
					if disposition != latest.ExecutionDisposition() || len(provider.calls) != 2 {
						t.Fatalf(
							"disposition=%+v attempts=%v; expected the latest upstream outcome and no third attempt",
							disposition,
							provider.calls,
						)
					}
				},
			)
		}
	}
}

type errorPriorityPreparer struct {
	*roundExecutor
	failure error
	calls   int
}

func (p *errorPriorityPreparer) ShouldPrepareRequestAuth(*Auth) bool {
	return len(p.roundExecutor.calls) > 0
}

func (p *errorPriorityPreparer) PrepareRequestAuth(_ context.Context, auth *Auth) (*Auth, error) {
	p.calls++
	return auth, p.failure
}

func TestManagerKeepsUpstreamCauseWhenLaterAuthPreparationFails(t *testing.T) {
	for _, homeMode := range []bool{false, true} {
		for _, entry := range []string{"execute", "count", "stream"} {
			t.Run(
				fmt.Sprintf("Home=%t/%s", homeMode, entry), func(t *testing.T) {
					local := errors.New("synthetic credential preparation failure")
					manager, provider, upstream := newErrorPriorityManager(t, homeMode, nil, nil)
					manager.SetRetryConfig(0, 0, 0)
					if homeMode {
						retry := 0
						manager.PublishHomeDispatch(
							&roundsHomeDispatcher{retry: &retry},
							executionregistry.New(),
							2,
						)
					}
					preparer := &errorPriorityPreparer{roundExecutor: provider, failure: local}
					manager.RegisterExecutor(preparer)
					err := invokeRetryRound(
						manager,
						entry,
						apiKeyReservationTestContext(),
						coreexecutor.Options{},
					)
					if !errors.Is(err, upstream) || errors.Is(
						err, local,
					) || statusCodeFromError(err) != http.StatusServiceUnavailable {
						t.Errorf("error=%v; expected original upstream cause and status", err)
					}
					if len(provider.calls) != 1 || preparer.calls != 1 {
						t.Fatalf(
							"executions=%v preparations=%d; expected one upstream attempt and one preparation failure",
							provider.calls,
							preparer.calls,
						)
					}
				},
			)
		}
	}
}
