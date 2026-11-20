package auth

import (
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func invokeOriginalRetryRound(
	t *testing.T,
	manager *Manager,
	kind, provider, model string,
) (core.Response, error) {
	t.Helper()
	request := core.Request{Model: model}
	switch kind {
	case "execute":
		return manager.Execute(t.Context(), []string{provider}, request, core.Options{})
	case "count":
		return manager.ExecuteCount(t.Context(), []string{provider}, request, core.Options{})
	case "stream":
		result, err := manager.ExecuteStream(
			t.Context(),
			[]string{provider},
			request,
			core.Options{Stream: true},
		)
		if err != nil {
			return core.Response{}, err
		}
		if result == nil {
			t.Fatal("missing stream result")
		}
		var response core.Response
		for chunk := range result.Chunks {
			response.Payload = append(response.Payload, chunk.Payload...)
			if chunk.Err != nil {
				err = chunk.Err
			}
			if chunk.Acknowledge != nil {
				chunk.Acknowledge(nil)
			}
		}
		return response, err
	default:
		t.Fatalf("unknown execution kind %q", kind)
		return core.Response{}, nil
	}
}

func TestOriginalRetryRoundCredentialWindows(t *testing.T) {
	for _, sample := range []struct {
		name, provider, model string
		global, cap           int
		limits, want          map[string]int
		last                  string
		home                  bool
	}{
		{
			name: "local windows", provider: "retry-round-test", model: "retry-round-model", global: 3,
			limits: map[string]int{"retry-round-a": 3, "retry-round-b": 2, "retry-round-c": 2},
			want:   map[string]int{"retry-round-a": 4, "retry-round-b": 3, "retry-round-c": 3},
		},
		{
			name: "local cap", provider: "retry-round-test", model: "retry-round-model-cap", global: 2, cap: 3,
			limits: map[string]int{"retry-cap-a": 1, "retry-cap-b": 1, "retry-cap-c": 1, "retry-cap-d": 2},
			want:   map[string]int{"retry-cap-a": 2, "retry-cap-b": 2, "retry-cap-c": 2, "retry-cap-d": 1},
			last:   "retry-cap-d",
		},
		{
			name: "home windows", provider: "retry-round-home", model: "retry-round-model", global: 3, cap: 3,
			home:   true,
			limits: map[string]int{"retry-round-a": 3, "retry-round-b": 2, "retry-round-c": 2},
			want:   map[string]int{"retry-round-a": 4, "retry-round-b": 3, "retry-round-c": 3},
		},
	} {
		for _, kind := range []string{"execute", "count", "stream"} {
			for _, outcome := range []string{"safe rejection", "unknown result"} {
				t.Run(
					sample.name+"/"+kind+"/"+outcome, func(t *testing.T) {
						manager := NewManager(nil, &FillFirstSelector{}, nil)
						manager.SetRetryConfig(sample.global, 0, sample.cap)
						var dispatcher *roundsHomeDispatcher
						if sample.home {
							manager.SetConfig(&config.Config{Home: config.HomeConfig{Enabled: true}})
							dispatcher = &roundsHomeDispatcher{
								retry: &sample.global, limits: sample.limits, provider: sample.provider,
							}
							manager.PublishHomeDispatch(dispatcher, executionregistry.New(), 1)
						} else {
							manager.SetHomeClientProvider(func() HomeClient { return nil })
							for id, limit := range sample.limits {
								registerRequestScopedTestAuth(
									t, manager, &Auth{
										ID: id, Provider: sample.provider, Status: StatusActive,
										Metadata: map[string]any{"request_retry": limit, "disable_cooling": true},
									}, sample.model,
								)
							}
						}
						failure := &Error{
							HTTPStatus: http.StatusInternalServerError, Message: "retry-round test failure",
						}
						if outcome == "safe rejection" {
							failure.executionState().DispatchPhase = core.DispatchPhaseNotSent
							failure.executionState().Outcome = core.ExecutionOutcomeRejectedBeforeProcessing
							failure.executionState().RetryDirective = core.RetryDirectiveNextAuth
							failure.executionState().EffectScope = core.StateEffectScopeRequest
						}
						executor := &roundExecutor{provider: sample.provider, failure: failure}
						manager.RegisterExecutor(executor)
						_, err := invokeOriginalRetryRound(t, manager, kind, sample.provider, sample.model)
						if !errors.Is(err, failure) ||
							statusCodeFromError(err) != http.StatusInternalServerError {
							t.Fatalf("terminal error=%T/%v, want original HTTP 500", err, err)
						}
						counts := map[string]int{}
						for _, id := range executor.calls {
							counts[id]++
						}
						if outcome == "unknown result" {
							if len(executor.calls) != 1 {
								t.Fatalf("unknown result replayed: %v", executor.calls)
							}
						} else if !reflect.DeepEqual(counts, sample.want) ||
							(sample.last != "" && executor.calls[len(executor.calls)-1] != sample.last) {
							t.Fatalf(
								"calls=%v counts=%v, want=%v last=%q", executor.calls, counts, sample.want, sample.last,
							)
						}
						if dispatcher != nil {
							seen := map[int]bool{}
							for _, constraints := range dispatcher.constraints {
								if !seen[constraints.RetryRound] && len(constraints.ExcludedAuthIDs) != 0 {
									t.Fatalf("new round retained exclusions: %+v", constraints)
								}
								seen[constraints.RetryRound] = true
							}
							wantRounds := 1
							if outcome == "safe rejection" {
								wantRounds = 4
							}
							for round := range wantRounds {
								if !seen[round] {
									t.Fatalf("round %d missing from %+v", round, dispatcher.constraints)
								}
							}
							if len(seen) != wantRounds {
								t.Fatalf("rounds=%v, want %d", seen, wantRounds)
							}
						}
					},
				)
			}
		}
	}
}

func TestOriginalRetryRoundSnapshotsDefaultRequestRetry(t *testing.T) {
	for _, kind := range []string{"execute", "count", "stream"} {
		for _, sample := range []struct {
			name          string
			initial, next int
		}{
			{name: "decrease after request starts", initial: 1, next: 0},
			{name: "increase after request starts", initial: 0, next: 1},
		} {
			for _, outcome := range []string{"safe rejection", "unknown result"} {
				t.Run(
					kind+"/"+sample.name+"/"+outcome, func(t *testing.T) {
						const provider, model, id = "retry-config-snapshot", "retry-config-snapshot-model",
							"retry-config-" +
								"snapshot-auth"
						manager := NewManager(nil, &FillFirstSelector{}, nil)
						manager.SetHomeClientProvider(func() HomeClient { return nil })
						manager.SetRetryConfig(sample.initial, 0, 0)
						failure := &Error{
							HTTPStatus: http.StatusInternalServerError,
							Message:    "retry config changed",
						}
						if outcome == "safe rejection" {
							failure.executionState().DispatchPhase = core.DispatchPhaseNotSent
							failure.executionState().Outcome = core.ExecutionOutcomeRejectedBeforeProcessing
							failure.executionState().RetryDirective = core.RetryDirectiveNextAuth
							failure.executionState().EffectScope = core.StateEffectScopeRequest
						}
						calls := 0
						invoke := func(*Auth) (core.Response, error) {
							calls++
							if calls == 1 {
								manager.SetRetryConfig(sample.next, 0, 0)
								return core.Response{}, failure
							}
							return core.Response{Payload: []byte("ok")}, nil
						}
						executor := &requestScopedTestExecutor{
							provider: provider, execute: invoke, count: invoke,
							stream: func(selected *Auth) (*core.StreamResult, error) {
								response, err := invoke(selected)
								if err != nil {
									return nil, err
								}
								chunks := make(chan core.StreamChunk, 1)
								chunks <- core.StreamChunk{Payload: response.Payload}
								close(chunks)
								return &core.StreamResult{Chunks: chunks}, nil
							},
						}
						manager.RegisterExecutor(executor)
						registerRequestScopedTestAuth(
							t, manager, &Auth{
								ID: id, Provider: provider,
								Metadata: map[string]any{"disable_cooling": true},
							}, model,
						)
						response, err := invokeOriginalRetryRound(t, manager, kind, provider, model)
						success := outcome == "safe rejection" && sample.initial == 1
						wantCalls := 1
						if success {
							wantCalls = 2
							if err != nil || string(response.Payload) != "ok" {
								t.Fatalf("response=%q error=%v, want success", response.Payload, err)
							}
						} else if !errors.Is(
							err, failure,
						) || statusCodeFromError(err) != http.StatusInternalServerError {
							t.Fatalf("terminal error=%T/%v, want original HTTP 500", err, err)
						}
						if calls != wantCalls {
							t.Fatalf("calls=%d, want %d", calls, wantCalls)
						}
						current, _, _ := manager.retrySettings()
						if current != sample.next {
							t.Fatalf("next request default=%d, want %d", current, sample.next)
						}
					},
				)
			}
		}
	}
}
