package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type auditRetryDeadlineDispatcher struct {
	roundsHomeDispatcher
	selectAuth func(context.Context, home.AuthDispatchConstraints) ([]byte, error)
}

func (d *auditRetryDeadlineDispatcher) RPopAuthWithConstraints(
	ctx context.Context,
	_, _ string,
	_ http.Header,
	_ int,
	constraints home.AuthDispatchConstraints,
) ([]byte, error) {
	d.constraints = append(d.constraints, constraints)
	return d.selectAuth(ctx, constraints)
}

func TestAuditRetryDeadlineUsesRequestStart(t *testing.T) {
	for _, kind := range []string{"execute", "count", "stream"} {
		for _, scenario := range []string{
			"initial-selection-time", "alternating-failures", "inherited-deadline", "canceled-wait",
		} {
			t.Run(
				kind+"/"+scenario, func(t *testing.T) {
					synctest.Test(
						t, func(t *testing.T) {
							ctx, cancel := context.WithCancel(context.Background())
							defer cancel()
							maxWait := 100 * time.Millisecond
							if scenario == "inherited-deadline" {
								var deadlineCancel context.CancelFunc
								ctx, deadlineCancel = context.WithTimeout(ctx, maxWait)
								defer deadlineCancel()
								maxWait = time.Second
							}
							dispatcher := &auditRetryDeadlineDispatcher{}
							dispatcher.selectAuth = func(
								_ context.Context,
								constraints home.AuthDispatchConstraints,
							) ([]byte, error) {
								calls := len(dispatcher.constraints)
								if calls > 8 {
									t.Fatal("selection continued beyond the request wait budget")
								}
								if scenario == "alternating-failures" && calls == 2 {
									limit := 2
									return json.Marshal(
										homeAuthDispatchResponse{
											Auth: Auth{
												ID: "synthetic-deadline-auth", Provider: "test",
												Metadata: map[string]any{"disable_cooling": true},
											}, RequestRetry: &limit,
										},
									)
								}
								if scenario == "initial-selection-time" || scenario == "inherited-deadline" {
									time.Sleep(80 * time.Millisecond)
								}
								if scenario == "canceled-wait" {
									time.AfterFunc(10*time.Millisecond, cancel)
								}
								return []byte(
									(`{"error":{"type":"model_cooldown","message":"synthetic deadline ` +
										`cooldown","retryable":true,"retry_after_ms":30,"request_retry":2}}`),
								), nil
							}
							manager := NewManager(nil, nil, nil)
							manager.SetConfig(
								&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}},
							)
							manager.SetRetryConfig(2, maxWait, 1)
							manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
							executor := &roundExecutor{
								failure: reservationContextRetryError{directive: core.RetryDirectiveNextAuth},
								onCall:  func() { time.Sleep(80 * time.Millisecond) },
							}
							manager.RegisterExecutor(executor)
							started := time.Now()
							err := invokeRetryRound(manager, kind, ctx, core.Options{})
							elapsed := time.Since(started)
							wantSelections, wantExecutions := 1, 0
							if scenario == "alternating-failures" {
								wantSelections, wantExecutions = 3, 1
								if elapsed < 110*time.Millisecond || elapsed >= 118*time.Millisecond {
									t.Errorf(
										"alternating failures elapsed=%v, want one jittered 30 ms wait plus "+
											"80 ms execution",
										elapsed,
									)
								}
							} else if scenario == "canceled-wait" {
								if !errors.Is(err, context.Canceled) || elapsed != 10*time.Millisecond {
									t.Errorf("canceled wait error=%v elapsed=%v", err, elapsed)
								}
							} else if elapsed != 80*time.Millisecond {
								t.Errorf("initial selection elapsed=%v, want 80 ms without a new wait window", elapsed)
							}
							if scenario != "canceled-wait" {
								var cooldown *Error
								if !errors.As(
									err, &cooldown,
								) ||
									cooldown.Code !=
										"model_cooldown" ||
									SafeResponseHeaders(err).Get("Retry-After") !=
										"1" {
									t.Errorf("terminal error=%v, want original cooldown and Retry-After", err)
								}
							}
							if len(dispatcher.constraints) != wantSelections ||
								len(executor.calls) != wantExecutions {
								t.Fatalf(
									"selections=%d executions=%v, want %d selections and %d executions",
									len(dispatcher.constraints), executor.calls, wantSelections, wantExecutions,
								)
							}
							for index, constraints := range dispatcher.constraints {
								wantRound := 0
								if scenario == "alternating-failures" && index == 2 {
									wantRound = 1
								}
								if constraints.RetryRound != wantRound {
									t.Errorf(
										"selection %d retry round=%d, want %d",
										index,
										constraints.RetryRound,
										wantRound,
									)
								}
							}
						},
					)
				},
			)
		}
	}
}
