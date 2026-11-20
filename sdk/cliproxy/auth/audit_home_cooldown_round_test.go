package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/home"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type auditHomeRetry429 struct{ reservationContextRetryError }

func (auditHomeRetry429) StatusCode() int { return http.StatusTooManyRequests }

type auditHomeRoundExecutor struct {
	schedulerTestExecutor
	calls []string
}

func (e *auditHomeRoundExecutor) Execute(_ context.Context, a *Auth, _ core.Request, _ core.Options) (
	core.Response,
	error,
) {
	e.calls = append(e.calls, a.ID)
	if a.ID == "home-a" {
		return core.Response{}, auditHomeRetry429{reservationContextRetryError{directive: core.RetryDirectiveNextAuth}}
	}
	return core.Response{Payload: []byte(`{"synthetic":"success"}`)}, nil
}
func (e *auditHomeRoundExecutor) CountTokens(
	ctx context.Context,
	a *Auth,
	req core.Request,
	opts core.Options,
) (core.Response, error) {
	return e.Execute(ctx, a, req, opts)
}
func (e *auditHomeRoundExecutor) ExecuteStream(
	ctx context.Context,
	a *Auth,
	req core.Request,
	opts core.Options,
) (*core.StreamResult, error) {
	result, err := e.Execute(ctx, a, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan core.StreamChunk, 1)
	chunks <- core.StreamChunk{Payload: result.Payload}
	close(chunks)
	return &core.StreamResult{Chunks: chunks}, nil
}

type auditHomeRoundDispatcher struct {
	roundsHomeDispatcher
	mode       string
	cooldowns  int
	onCooldown func()
}

func (d *auditHomeRoundDispatcher) RPopAuthWithConstraints(
	_ context.Context,
	_, _ string,
	_ http.Header,
	_ int,
	c home.AuthDispatchConstraints,
) ([]byte, error) {
	d.constraints = append(d.constraints, c)
	if c.RetryRound == 1 && d.mode != "ready" && (d.cooldowns == 0 || d.mode == "bounded" || d.mode == "cancel") {
		d.cooldowns++
		if d.onCooldown != nil {
			d.onCooldown()
			d.onCooldown = nil
		}
		waitMS := 1
		if d.mode == "bounded" {
			waitMS = 5
		}
		if d.mode == "cancel" {
			waitMS = 50
		}
		if d.mode == "zero-retry-after" {
			waitMS = 0
		}
		return []byte(fmt.Sprintf(
			`{"error":{"type":"model_cooldown","message":"synthetic temporary cooldown","retryable":true,"`+
				`retry_after_ms":%d,"request_retry":1}}`,
			waitMS,
		)), nil
	}
	id := "home-a"
	if c.RetryRound > 0 {
		id = "home-b"
	}
	limit := 1
	return json.Marshal(
		homeAuthDispatchResponse{
			Auth: Auth{
				ID: id, Provider: "test", Metadata: map[string]any{"disable_cooling": true},
			}, RequestRetry: &limit,
		},
	)
}

func TestAuditHomeCooldownDoesNotConsumeUnexecutedRound(t *testing.T) {
	for _, kind := range []string{"execute", "count", "stream"} {
		for _, mode := range []string{
			"ready", "cooldown", "bounded", "cancel", "cancel-before-wait", "no-wait-budget", "zero-retry-after",
		} {
			t.Run(
				kind+"/"+mode, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					dispatcher := &auditHomeRoundDispatcher{mode: mode}
					if mode == "cancel" {
						dispatcher.onCooldown = func() {
							timer := time.AfterFunc(
								time.Millisecond, cancel,
							)
							t.Cleanup(func() { timer.Stop() })
						}
					}
					if mode == "cancel-before-wait" {
						dispatcher.onCooldown = cancel
					}
					manager := NewManager(nil, nil, nil)
					manager.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
					maxWait := 100 * time.Millisecond
					if mode == "bounded" {
						maxWait = 20 * time.Millisecond
					}
					if mode == "no-wait-budget" {
						maxWait = 0
					}
					manager.SetRetryConfig(1, maxWait, 1)
					manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
					executor := &auditHomeRoundExecutor{}
					manager.RegisterExecutor(executor)
					request := core.Request{Model: "round-model"}
					start := time.Now()
					var err error
					switch kind {
					case "execute":
						_, err = manager.Execute(ctx, []string{"test"}, request, core.Options{})
					case "count":
						_, err = manager.ExecuteCount(ctx, []string{"test"}, request, core.Options{})
					case "stream":
						var result *core.StreamResult
						result, err = manager.ExecuteStream(ctx, []string{"test"}, request, core.Options{})
						if err == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									err = chunk.Err
								}
							}
						}
					}
					elapsed := time.Since(start)
					success := mode == "ready" || mode == "cooldown"
					if (err == nil) != success {
						t.Fatalf(
							"mode=%s error=%v calls=%v rounds=%v", mode, err, executor.calls, dispatcher.constraints,
						)
					}
					if (mode == "cancel" || mode == "cancel-before-wait") && !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation error=%v", err)
					}
					wantCalls := []string{"home-a"}
					if success {
						wantCalls = append(wantCalls, "home-b")
					}
					if !reflect.DeepEqual(executor.calls, wantCalls) {
						t.Fatalf("calls=%v want=%v", executor.calls, wantCalls)
					}
					var rounds []int
					for _, c := range dispatcher.constraints {
						rounds = append(rounds, c.RetryRound)
					}
					if mode == "ready" && !reflect.DeepEqual(rounds, []int{0, 1}) {
						t.Fatalf("ready rounds=%v", rounds)
					}
					if mode == "cooldown" && !reflect.DeepEqual(rounds, []int{0, 1, 1}) {
						t.Fatalf("cooldown consumed execution round: %v", rounds)
					}
					for _, round := range rounds[1:] {
						if round != 1 {
							t.Fatalf("unexpected extra execution round: %v", rounds)
						}
					}
					if mode == "bounded" && (len(rounds) < 3 || elapsed > time.Second) {
						t.Fatalf("unbounded selection: elapsed=%v rounds=%v", elapsed, rounds)
					}
					if mode == "zero-retry-after" && len(rounds) != 2 {
						t.Fatalf("zero wait busy-looped: %v", rounds)
					}
				},
			)
		}
	}
}
