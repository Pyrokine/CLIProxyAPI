package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	internalhome "github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type originalErrorPriorityHomeDispatcher struct {
	finalPayload []byte
	constraints  []internalhome.AuthDispatchConstraints
}

func (*originalErrorPriorityHomeDispatcher) HeartbeatOK() bool { return true }

func (d *originalErrorPriorityHomeDispatcher) RPopAuth(
	ctx context.Context,
	model, session string,
	headers http.Header,
	count int,
) ([]byte, error) {
	return d.RPopAuthWithConstraints(
		ctx,
		model,
		session,
		headers,
		count,
		internalhome.AuthDispatchConstraints{},
	)
}

func (d *originalErrorPriorityHomeDispatcher) RPopAuthWithConstraints(
	_ context.Context,
	_, _ string,
	_ http.Header,
	_ int,
	constraints internalhome.AuthDispatchConstraints,
) ([]byte, error) {
	d.constraints = append(d.constraints, constraints)
	if len(constraints.ExcludedAuthIDs) == 0 {
		authID := "home-retry-a"
		if constraints.RetryRound > 0 {
			authID = "home-retry-b"
		}
		requestRetry := 1
		return json.Marshal(
			homeAuthDispatchResponse{
				RequestRetry: &requestRetry,
				Auth:         Auth{ID: authID, Provider: "home-retry-contract", Status: StatusActive},
			},
		)
	}
	if constraints.RetryRound == 0 {
		return []byte(
			`{"error":{"type":"auth_unavailable","message":"a credential is immediately available next round"}}`,
		), nil
	}
	if len(d.finalPayload) > 0 {
		return append([]byte(nil), d.finalPayload...), nil
	}
	return nil, internalhome.ErrAuthNotFound
}

func (*originalErrorPriorityHomeDispatcher) AbortAmbiguousDispatch() {}

func invokeOriginalErrorPriority(ctx context.Context, manager *Manager, provider, model, entry string) error {
	req := cliproxyexecutor.Request{Model: model}
	switch entry {
	case "execute":
		_, err := manager.Execute(ctx, []string{provider}, req, cliproxyexecutor.Options{})
		return err
	case "count-tokens":
		_, err := manager.ExecuteCount(ctx, []string{provider}, req, cliproxyexecutor.Options{})
		return err
	case "stream":
		_, err := manager.ExecuteStream(ctx, []string{provider}, req, cliproxyexecutor.Options{Stream: true})
		return err
	default:
		return errors.New("unrecognized original error-priority entry: " + entry)
	}
}

// noinspection GoDirectComparisonOfErrors
func checkOriginalTerminalErrorPriority(t *testing.T, homeMode bool) {
	t.Helper()
	for _, entry := range []string{"execute", "count-tokens", "stream"} {
		for _, terminal := range []struct {
			name string
			err  error
		}{
			{"canceled", context.Canceled},
			{"deadline-exceeded", context.DeadlineExceeded},
		} {
			for _, safeRejection := range []bool{false, true} {
				mode := "original_unknown_503"
				if safeRejection {
					mode = "explicit_safe_rejection"
				}
				t.Run(
					entry+"/"+terminal.name+"/"+mode, func(t *testing.T) {
						provider := "retry-terminal-priority-" + entry + "-" + terminal.name
						model := provider + "-model"
						if homeMode {
							provider, model = "home-retry-contract", "gpt"
						}
						upstream := &Error{
							HTTPStatus: http.StatusServiceUnavailable, Message: "first retry round reached upstream",
						}
						if safeRejection {
							upstream.executionState().DispatchPhase = cliproxyexecutor.DispatchPhaseResponseStarted
							upstream.executionState().Outcome =
								cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing
							upstream.executionState().RetryDirective = cliproxyexecutor.RetryDirectiveNextAuth
							upstream.executionState().EffectScope = cliproxyexecutor.StateEffectScopeRequest
						}
						ctx := context.Background()
						var cancel context.CancelFunc
						if !homeMode && terminal.err == context.Canceled {
							ctx, cancel = context.WithCancel(ctx)
							t.Cleanup(cancel)
						}
						providerExecutor := &roundExecutor{provider: provider}
						providerExecutor.onCall = func() {
							switch len(providerExecutor.calls) {
							case 1:
								providerExecutor.failure = upstream
							case 2:
								if cancel != nil {
									cancel()
								}
								providerExecutor.failure = terminal.err
							default:
								t.Fatalf(
									"unexpected execution after terminal outcome: %v",
									providerExecutor.calls,
								)
							}
						}
						manager := NewManager(nil, nil, nil)
						manager.SetRetryConfig(1, 0, 0)
						manager.RegisterExecutor(providerExecutor)
						var dispatcher *originalErrorPriorityHomeDispatcher
						if homeMode {
							manager.SetConfig(
								&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}},
							)
							dispatcher = &originalErrorPriorityHomeDispatcher{}
							manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
						} else {
							authID := provider + "-auth"
							if _, err := manager.Register(
								ctx, &Auth{
									ID: authID, Provider: provider, Status: StatusActive,
									Metadata: map[string]any{"request_retry": 1},
								},
							); err != nil {
								t.Fatal(err)
							}
							registerReservationContextModels(t, provider, model, authID)
						}
						err := invokeOriginalErrorPriority(ctx, manager, provider, model, entry)
						if safeRejection {
							if !errors.Is(err, terminal.err) || errors.Is(
								err, upstream,
							) || len(providerExecutor.calls) != 2 {
								t.Fatalf(
									"safe rejection error=%v calls=%v, want terminal=%v after two attempts",
									err,
									providerExecutor.calls,
									terminal.err,
								)
							}
							return
						}
						if !errors.Is(err, upstream) || errors.Is(
							err, terminal.err,
						) || len(providerExecutor.calls) != 1 {
							t.Fatalf(
								"original unknown 503 error=%v calls=%v, want one original upstream failure",
								err,
								providerExecutor.calls,
							)
						}
						disposition := executionDispositionForError(
							err,
							cliproxyexecutor.DispatchPhaseMaybeSent,
						)
						if disposition.Outcome != cliproxyexecutor.ExecutionOutcomeUnknown ||
							disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry {
							t.Fatalf("original unknown 503 changed replay authority: %+v", disposition)
						}
						if dispatcher != nil && len(dispatcher.constraints) != 1 {
							t.Fatalf("Home selected again after an unknown outcome: %v", dispatcher.constraints)
						}
					},
				)
			}
		}
	}
}

func TestOriginalManagerRetryPreservesTerminalContextErrorAfterUpstreamFailure(t *testing.T) {
	checkOriginalTerminalErrorPriority(t, false)
}

func TestOriginalHomeRetryPreservesTerminalContextErrorAfterUpstreamFailure(t *testing.T) {
	checkOriginalTerminalErrorPriority(t, true)
}

func TestOriginalHomePreferredUpstreamErrorPreservesCurrentRetryAfter(t *testing.T) {
	for _, entry := range []string{"execute", "count-tokens", "stream"} {
		for _, mode := range []string{
			"original_unknown_503", "safe_then_executor_failure", "safe_then_prepare_failure",
		} {
			t.Run(
				entry+"/"+mode, func(t *testing.T) {
					upstream := &Error{
						HTTPStatus: http.StatusServiceUnavailable, Message: "first retry round reached upstream",
					}
					if mode != "original_unknown_503" {
						upstream.executionState().DispatchPhase = cliproxyexecutor.DispatchPhaseResponseStarted
						upstream.executionState().Outcome = cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing
						upstream.executionState().RetryDirective = cliproxyexecutor.RetryDirectiveNextAuth
						upstream.executionState().EffectScope = cliproxyexecutor.StateEffectScopeRequest
					}
					local := errors.New("second retry round failed before upstream")
					providerExecutor := &roundExecutor{provider: "home-retry-contract"}
					providerExecutor.onCall = func() {
						if len(providerExecutor.calls) == 1 {
							providerExecutor.failure = upstream
						} else {
							providerExecutor.failure = local
						}
					}
					dispatcher := &originalErrorPriorityHomeDispatcher{
						finalPayload: []byte(
							(`{"error":{"type":"model_cooldown","message":"current Home retry window` +
								`","retryable":true,"retry_after_ms":10000,"request_retry":1}}`),
						),
					}
					manager := NewManager(nil, nil, nil)
					manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
					manager.SetRetryConfig(1, 20*time.Second, 0)
					manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
					manager.RegisterExecutor(providerExecutor)
					var preparer *errorPriorityPreparer
					if mode == "safe_then_prepare_failure" {
						preparer = &errorPriorityPreparer{roundExecutor: providerExecutor, failure: local}
						manager.RegisterExecutor(preparer)
					}
					err := invokeOriginalErrorPriority(
						context.Background(), manager, "home-retry-contract", "gpt", entry,
					)
					if mode == "safe_then_prepare_failure" {
						var remote *homeDispatchRetryAfterError
						if !errors.Is(err, upstream) || errors.Is(err, local) || !errors.As(err, &remote) ||
							len(providerExecutor.calls) != 1 || preparer.calls != 1 {
							t.Fatalf(
								"preparation failure error=%v calls=%v preparations=%d",
								err,
								providerExecutor.calls,
								preparer.calls,
							)
						}
						if retry := retryAfterFromError(err); retry == nil || *retry != 10*time.Second {
							t.Fatalf("retry after=%v, want original Home delay 10s", retry)
						}
						if got := SafeResponseHeaders(err).Get("Retry-After"); got != "10" {
							t.Fatalf("Retry-After=%q, want 10", got)
						}
						return
					}
					want, calls := error(upstream), 1
					if mode == "safe_then_executor_failure" {
						want, calls = local, 2
					}
					if !errors.Is(
						err, want,
					) || len(providerExecutor.calls) != calls || retryAfterFromError(err) != nil ||
						SafeResponseHeaders(err).Get("Retry-After") != "" {
						t.Fatalf(
							"unknown executor outcome error=%v calls=%v, want original cause=%v attempts=%d",
							err,
							providerExecutor.calls,
							want,
							calls,
						)
					}
					disposition := executionDispositionForError(err, cliproxyexecutor.DispatchPhaseMaybeSent)
					if disposition.Outcome != cliproxyexecutor.ExecutionOutcomeUnknown ||
						disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry {
						t.Fatalf("unknown executor outcome acquired replay authority: %+v", disposition)
					}
				},
			)
		}
	}
}
