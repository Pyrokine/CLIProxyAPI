package auth

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executionregistry"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestCredentialRetryRoundAgesSkippedCredentials(t *testing.T) {
	for _, kind := range []string{"execute", "count", "stream"} {
		t.Run(kind, func(t *testing.T) {
			m := NewManager(nil, &FillFirstSelector{}, nil)
			e := &roundExecutor{failure: reservationContextRetryError{directive: coreexecutor.RetryDirectiveNextAuth}}
			m.RegisterExecutor(e)
			m.SetRetryConfig(2, 0, 3)
			for id, limit := range map[string]int{"a": 1, "b": 1, "c": 1, "d": 2} {
				if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "test", Metadata: map[string]any{"request_retry": limit, "disable_cooling": true}}); err != nil {
					t.Fatal(err)
				}
			}
			registerReservationContextModels(t, "test", "round-model", "a", "b", "c", "d")
			if err := invokeRetryRound(m, kind, apiKeyReservationTestContext(), coreexecutor.Options{}); err == nil {
				t.Fatal("missing terminal failure")
			}
			if want := []string{"a", "b", "c", "a", "b", "c", "d"}; !reflect.DeepEqual(e.calls, want) {
				t.Fatalf("calls=%v want=%v", e.calls, want)
			}
		})
	}
}

func TestCredentialRetryRoundMixedProviderWindows(t *testing.T) {
	m := NewManager(nil, &FillFirstSelector{}, nil)
	m.SetRetryConfig(2, 0, 0)
	for _, provider := range []string{"first", "second"} {
		e := &roundExecutor{provider: provider, failure: reservationContextRetryError{directive: coreexecutor.RetryDirectiveNextProvider}}
		m.RegisterExecutor(e)
		if _, err := m.Register(context.Background(), &Auth{ID: provider, Provider: provider, Metadata: map[string]any{"disable_cooling": true}}); err != nil {
			t.Fatal(err)
		}
		registerReservationContextModels(t, provider, "round-model", provider)
	}
	_, err := m.Execute(apiKeyReservationTestContext(), []string{"first", "second"}, coreexecutor.Request{Model: "round-model"}, coreexecutor.Options{})
	if err == nil {
		t.Fatal("missing terminal failure")
	}
	for _, provider := range []string{"first", "second"} {
		executor, _ := m.Executor(provider)
		if calls := executor.(*roundExecutor).calls; len(calls) != 3 {
			t.Fatalf("provider=%s calls=%v", provider, calls)
		}
	}
}

func TestCredentialRetryRoundWaitMatchesSelection(t *testing.T) {
	for _, scenario := range []string{"empty", "executor_missing", "provider_mismatch", "unsupported_model", "context_1m", "required_kind", "credential_policy", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.SetRetryConfig(2, 0, 0)
			if scenario != "executor_missing" {
				m.RegisterExecutor(schedulerTestExecutor{})
			}
			ctx := context.WithValue(context.Background(), retryRoundContextKey{}, &retryRoundState{round: 1, defaultRetry: 2})
			model := "round-model"
			if scenario != "empty" {
				auth := registerContext1MTestAuthForModel(t, nil, "ineligible", "test", model, 200_000)
				if scenario == "disabled" {
					auth.Disabled = true
				}
				if _, err := m.Register(ctx, auth); err != nil {
					t.Fatal(err)
				}
			}
			providers := []string{"test"}
			switch scenario {
			case "provider_mismatch":
				providers = []string{"second"}
			case "unsupported_model":
				model = "unsupported"
			case "context_1m":
				model += "[1m]"
			case "required_kind":
				ctx = withRequiredAuthKind(ctx, "api_key")
			case "credential_policy":
				ctx = withCredentialPolicy(ctx, "codex_alpha_search_v1")
			}
			if _, _, _, err := m.pickNextMixed(ctx, providers, model, coreexecutor.Options{}, nil); err == nil {
				t.Fatal("selection admitted ineligible credential")
			}
			if wait, allowed := m.closestCooldownWait(ctx, coreexecutor.Options{}, providers, model, 0, 503); allowed {
				t.Fatalf("ineligible credential authorized wait %v", wait)
			}
		})
	}
}

func TestCredentialRetryRoundDoesNotReplayTerminalOutcomes(t *testing.T) {
	for _, homeMode := range []bool{false, true} {
		for _, kind := range []string{"execute", "count", "stream"} {
			for _, tc := range []struct {
				name    string
				phase   coreexecutor.DispatchPhase
				outcome coreexecutor.ExecutionOutcome
			}{
				{"unknown", coreexecutor.DispatchPhaseMaybeSent, coreexecutor.ExecutionOutcomeUnknown},
				{"processed_failure", coreexecutor.DispatchPhaseResponseStarted, coreexecutor.ExecutionOutcomeFailed},
				{"committed", coreexecutor.DispatchPhaseDownstreamCommitted, coreexecutor.ExecutionOutcomeRejectedBeforeProcessing},
			} {
				name := kind + "/" + tc.name
				if homeMode {
					name = "home/" + name
				}
				t.Run(name, func(t *testing.T) {
					m := NewManager(nil, nil, nil)
					m.SetRetryConfig(3, time.Second, 1)
					failure := &Error{Message: "synthetic terminal outcome", HTTPStatus: http.StatusServiceUnavailable, DispatchPhase: tc.phase, Outcome: tc.outcome, RetryDirective: coreexecutor.RetryDirectiveNextAuth, EffectScope: coreexecutor.StateEffectScopeRequest}
					e := &roundExecutor{failure: failure}
					m.RegisterExecutor(e)
					if homeMode {
						limit := 3
						m.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
						m.PublishHomeDispatch(&roundsHomeDispatcher{retry: &limit}, executionregistry.New(), 1)
					} else {
						if _, err := m.Register(context.Background(), &Auth{ID: "terminal", Provider: "test", Metadata: map[string]any{"disable_cooling": true}}); err != nil {
							t.Fatal(err)
						}
						registerReservationContextModels(t, "test", "round-model", "terminal")
					}
					if err := invokeRetryRound(m, kind, apiKeyReservationTestContext(), coreexecutor.Options{}); !errors.Is(err, failure) {
						t.Fatalf("error=%v want original failure", err)
					}
					if len(e.calls) != 1 {
						t.Fatalf("terminal outcome replayed: %v", e.calls)
					}
				})
			}
		}
	}
}

func TestHomeCredentialRetryRejectsInvalidCooldownDuration(t *testing.T) {
	for _, payload := range []string{
		`{"error":{"type":"model_cooldown","retryable":true,"retry_after_ms":-1,"request_retry":1}}`,
		`{"error":{"type":"model_cooldown","retryable":true,"retry_after_ms":18446744073710,"request_retry":1}}`,
	} {
		err := decodeHomeDispatchError([]byte(payload))
		if statusCodeFromError(err) != http.StatusBadGateway || retryDirectiveAllowsCooldownRetry(executionDispositionForError(err, coreexecutor.DispatchPhaseMaybeSent)) {
			t.Errorf("invalid duration accepted: %s: %v", payload, err)
		}
	}
}

func TestHomeUserBillingAndPeriodLimitErrors(t *testing.T) {
	for _, test := range []struct {
		name       string
		payload    string
		code       string
		wantStatus int
	}{
		{
			name:       "credits insufficient",
			payload:    `{"error":{"type":"user_credits_insufficient","message":"credits exhausted"}}`,
			code:       "user_credits_insufficient",
			wantStatus: http.StatusPaymentRequired,
		},
		{
			name:       "period limit exceeded",
			payload:    `{"error":{"type":"user_period_limit_exceeded","message":"period exhausted"}}`,
			code:       "user_period_limit_exceeded",
			wantStatus: http.StatusTooManyRequests,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := decodeHomeDispatchError([]byte(test.payload))
			var authErr *Error
			if !errors.As(err, &authErr) || authErr == nil ||
				authErr.Code != test.code || authErr.HTTPStatus != test.wantStatus {
				t.Fatalf("decodeHomeDispatchError() = %#v, want code %q status %d", err, test.code, test.wantStatus)
			}
		})
	}
}

func TestHomeCredentialRetryStreamPreservesRemoteCooldown(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	m.SetRetryConfig(1, 0, 2)
	d := &roundsHomeDispatcher{errorAfter: 1, errorPayload: []byte(`{"error":{"type":"model_cooldown","retryable":true,"retry_after_ms":1500,"request_retry":1}}`)}
	m.PublishHomeDispatch(d, executionregistry.New(), 1)
	failure := reservationContextRetryError{directive: coreexecutor.RetryDirectiveNextAuth}
	e := &roundExecutor{failure: failure, bootstrap: true}
	m.RegisterExecutor(e)
	result, err := m.ExecuteStream(context.Background(), []string{"test"}, coreexecutor.Request{Model: "round-model"}, coreexecutor.Options{})
	if err != nil || result == nil {
		t.Fatalf("stream=%v error=%v", result, err)
	}
	chunk := <-result.Chunks
	if !errors.Is(chunk.Err, failure) || result.Headers.Get("X-Request-Id") != "synthetic-stream" {
		t.Fatal("bootstrap failure or original headers lost")
	}
	if retry := retryAfterFromError(chunk.Err); retry == nil || *retry != 1500*time.Millisecond {
		t.Fatalf("remote cooldown lost: %v", retry)
	}
	if result.Headers.Get("Retry-After") != "2" || SafeResponseHeaders(chunk.Err).Get("Retry-After") != "2" {
		t.Fatal("remote Retry-After header lost")
	}
	if len(e.calls) != 1 || len(d.constraints) != 2 {
		t.Fatalf("unexpected replay: calls=%v dispatches=%v", e.calls, d.constraints)
	}
}

func TestHomeCredentialRetryRetainedOverrideZeroStopsRounds(t *testing.T) {
	ctx := coreexecutor.WithDownstreamWebsocket(context.Background())
	m := NewManager(nil, nil, nil)
	m.SetConfig(&internalconfig.Config{Home: internalconfig.HomeConfig{Enabled: true}})
	m.SetRetryConfig(3, 0, 1)
	failure := reservationContextRetryError{directive: coreexecutor.RetryDirectiveNextAuth}
	e := &roundExecutor{failure: failure}
	m.RegisterExecutor(e)
	zero := 0
	d := &roundsHomeDispatcher{retry: &zero}
	registry := executionregistry.New()
	m.PublishHomeDispatch(d, registry, 1)
	pending, err := registry.BeginDispatch()
	if err != nil {
		t.Fatal(err)
	}
	scope, err := registry.Install(pending, executionregistry.ScopeSpec{CredentialID: "retained-zero", Model: "round-model"})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := newHomeDispatchSelection(&Auth{ID: "retained-zero", Provider: "test", Metadata: map[string]any{"request_retry": 0, "disable_cooling": true}}, e, "test", scope)
	if err != nil {
		t.Fatal(err)
	}
	selection.Retain()
	opts := coreexecutor.Options{Metadata: map[string]any{coreexecutor.ExecutionSessionMetadataKey: "retained-zero-session"}}
	if !m.retainHomeWebsocketSelection(ctx, opts, "round-model", selection) {
		t.Fatal("selection was not retained")
	}
	if err := invokeRetryRound(m, "execute", ctx, opts); !errors.Is(err, failure) {
		t.Errorf("terminal failure changed: %v", err)
	}
	if len(e.calls) != 1 || len(d.constraints) != 0 {
		t.Fatalf("retained zero override started an extra round: calls=%v dispatches=%v", e.calls, d.constraints)
	}
}

func TestHomeCredentialRetryRetainedExclusionWaitsForRelease(t *testing.T) {
	for _, cancelRelease := range []bool{false, true} {
		name := "acknowledged"
		if cancelRelease {
			name = "cancelled"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(coreexecutor.WithDownstreamWebsocket(context.Background()), 2*time.Second)
			defer cancel()
			m := NewManager(nil, nil, nil)
			e := &roundExecutor{}
			m.RegisterExecutor(e)
			d := &roundsHomeDispatcher{}
			registry := executionregistry.New()
			m.PublishHomeDispatch(d, registry, 1)
			pending, err := registry.BeginDispatch()
			if err != nil {
				t.Fatal(err)
			}
			scope, err := registry.Install(pending, executionregistry.ScopeSpec{CredentialID: "home-a", Model: "round-model", Accounted: true})
			if err != nil {
				t.Fatal(err)
			}
			selection, err := newHomeDispatchSelection(&Auth{ID: "home-a", Provider: "test"}, e, "test", scope)
			if err != nil {
				t.Fatal(err)
			}
			selection.Retain()
			opts := coreexecutor.Options{Metadata: map[string]any{coreexecutor.ExecutionSessionMetadataKey: "synthetic-session", ExcludedAuthIDsMetadataKey: []string{"home-a"}}}
			if !m.retainHomeWebsocketSelection(ctx, opts, "round-model", selection) {
				t.Fatal("selection was not retained")
			}
			started, acknowledged := make(chan struct{}), make(chan struct{})
			registry.SetReleaseSink(func(group executionregistry.ReleaseGroup, sequence int64) *executionregistry.ReleaseTicket {
				close(started)
				return executionregistry.NewReleaseTicket(group, sequence, acknowledged)
			})
			type pickResult struct {
				selection *HomeDispatchSelection
				err       error
			}
			done := make(chan pickResult, 1)
			go func() {
				next, err := m.pickHomeDispatchSelection(ctx, "round-model", opts)
				done <- pickResult{selection: next, err: err}
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("release was not requested")
			}
			select {
			case result := <-done:
				t.Fatalf("redispatch completed before release acknowledgement: %+v", result)
			default:
			}
			if cancelRelease {
				cancel()
			} else {
				close(acknowledged)
			}
			result := <-done
			if cancelRelease {
				if result.err == nil || result.selection != nil || len(d.constraints) != 0 {
					t.Fatalf("cancelled release allowed redispatch: %+v", result)
				}
			} else {
				if result.err != nil || result.selection == nil || result.selection.CloneAuth().ID != "home-b" || len(d.constraints) != 1 {
					t.Fatalf("acknowledged release did not select fresh credential: %+v", result)
				}
				result.selection.End("test_completed")
			}
			if selection.Active() {
				t.Fatal("excluded retained selection remains active")
			}
		})
	}
}
