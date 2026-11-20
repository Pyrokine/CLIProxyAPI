package auth

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const originalOverrideItemNotPersistedMessage = ("Item with id 'rs_0b5f3eb6f51f175c0169ca74e4a858819985399208" +
	"21603a74' " +
	"n") +
	"ot found. Items are not persisted when `store` is set to false. Try ag" +
	"ain with `store` set to true, or remove this item from your input."

func TestOriginalOverrideRetryEligibilityInputs(t *testing.T) {
	for _, mode := range []string{
		"wrapped Home busy", "ready 429", "ready 502", "available credential", "unrelated model", "disabled credential",
		"model disabled", "unauthorized", "payment required", "not found", "model unsupported", "credential policy",
		"pinned credential", "OAuth target cooldown", "typed model cooldown",
	} {
		t.Run(
			mode, func(t *testing.T) {
				provider, model := "codex", "retry-model"
				retries, maxWait := 0, time.Duration(0)
				want, positiveWait := false, false
				ctx, opts := t.Context(), core.Options{}
				auths := []*Auth{
					{ID: "target-auth", Provider: provider, Metadata: map[string]any{"request_retry": 0}},
				}
				models := map[string]string{}
				failure := error(auditSafeRetryRejection())
				switch mode {
				case "wrapped Home busy":
					retries, maxWait, model = 1, 30*time.Second, "gpt"
					auths[0].ID, auths[0].Metadata = "retry-auth", nil
					failure = fmt.Errorf("outer retry: %w", NewHomeConcurrencyBusyError("busy", 20*time.Second))
				case "ready 429", "ready 502":
					retries, want = 1, true
					auths[0].ID, auths[0].Metadata = "retry-auth", nil
					status := http.StatusTooManyRequests
					if mode == "ready 502" {
						status = http.StatusBadGateway
					}
					rejection := auditSafeRetryRejection()
					rejection.HTTPStatus, rejection.Message = status, "retryable failure"
					failure = rejection
				case "available credential":
					retries, maxWait, want = 1, time.Minute, true
					auths = []*Auth{
						{
							ID: "cooling", Provider: provider, ModelStates: map[string]*ModelState{
								model: {
									Unavailable: true, Status: StatusError,
									NextRetryAfter: time.Now().Add(30 * time.Second),
								},
							},
						},
						{ID: "available", Provider: provider},
					}
				case "unrelated model":
					auths = append(
						auths,
						&Auth{
							ID:       "unrelated-auth",
							Provider: provider,
							Metadata: map[string]any{"request_retry": 2},
						},
					)
					models["unrelated-auth"] = "retry-unrelated"
				case "disabled credential":
					auths[0].ID = "active-auth"
					auths = append(
						auths, &Auth{
							ID: "disabled-auth", Provider: provider, Disabled: true,
							Metadata: map[string]any{"request_retry": 2},
						},
					)
				case "credential policy":
					ctx = withCredentialPolicy(ctx, CredentialPolicyCodexAlphaSearchV1)
					auths[0].ID = "retry-policy-eligible"
					auths[0].Attributes = map[string]string{"auth_kind": "oauth"}
					auths = append(
						auths, &Auth{
							ID: "retry-policy-ineligible", Provider: provider,
							Attributes: map[string]string{"api_key": "ordinary"},
							Metadata:   map[string]any{"request_retry": 2},
						},
					)
				case "pinned credential":
					auths[0].ID = "retry-pinned-eligible"
					opts.Metadata = map[string]any{core.PinnedAuthMetadataKey: auths[0].ID}
					auths = append(
						auths, &Auth{
							ID: "retry-pinned-ineligible", Provider: provider,
							Metadata: map[string]any{"request_retry": 2},
						},
					)
				case "OAuth target cooldown":
					provider, model, retries, maxWait, want, positiveWait = "kimi", "pool-model", 3, 30*
						time.Second, true, true
					deadline := time.Now().Add(5 * time.Second)
					auths = []*Auth{
						{
							ID: "auth-1", Provider: provider, ModelStates: map[string]*ModelState{
								"deepseek-v3.1": {
									Unavailable: true, Status: StatusError, NextRetryAfter: deadline,
									Quota: QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: deadline},
								},
							},
						},
					}
					models["auth-1"] = "deepseek-v3.1"
				case "typed model cooldown":
					maxWait, want, positiveWait = time.Minute, true, true
					deadline := time.Now().Add(time.Minute)
					auths[0].Metadata["request_retry"] = 2
					auths[0].ModelStates = map[string]*ModelState{
						model: {
							Status: StatusError, Unavailable: true, NextRetryAfter: deadline,
							LastError: errorWithState(
								&Error{HTTPStatus: http.StatusNotFound, Message: "not found"}, errorState{
									DispatchPhase:  core.DispatchPhaseResponseStarted,
									Outcome:        core.ExecutionOutcomeRejectedBeforeProcessing,
									RetryDirective: core.RetryDirectiveRetryAfter,
									EffectScope:    core.StateEffectScopeExactModel,
								},
							),
						},
					}
				default:
					maxWait = time.Minute
					state := &ModelState{Status: StatusDisabled}
					if mode != "model disabled" {
						status, message := http.StatusUnauthorized, "unauthorized"
						switch mode {
						case "payment required":
							status, message = http.StatusPaymentRequired, "payment required"
						case "not found":
							status, message = http.StatusNotFound, "not found"
						case "model unsupported":
							status, message = http.StatusBadRequest, "model not supported"
						}
						state = &ModelState{
							Status: StatusError, Unavailable: true, NextRetryAfter: time.Now().Add(time.Minute),
							LastError: &Error{HTTPStatus: status, Message: message},
						}
					}
					auths[0].ID = "retry-round-eligible"
					auths = append(
						auths, &Auth{
							ID: "retry-round-ineligible", Provider: provider,
							Metadata:    map[string]any{"request_retry": 2},
							ModelStates: map[string]*ModelState{model: state},
						},
					)
				}
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				manager.SetRetryConfig(retries, maxWait, 1)
				manager.RegisterExecutor(&roundExecutor{provider: provider})
				if mode == "OAuth target cooldown" {
					manager.SetOAuthModelAlias(
						map[string][]config.OAuthModelAlias{
							provider: {
								{
									Name: "deepseek-v3.1", Alias: model,
								},
							},
						},
					)
				}
				for _, auth := range auths {
					if _, err := manager.Register(WithSkipPersist(ctx), auth); err != nil {
						t.Fatal(err)
					}
					supported := model
					if value := models[auth.ID]; value != "" {
						supported = value
					}
					registerProjectionTestClient(t, auth.ID, provider, &registry.ModelInfo{ID: supported})
					manager.RefreshSchedulerEntry(auth.ID)
				}
				wait, allowed := manager.shouldRetryAfterError(
					ctx, opts, failure, 0, []string{provider}, model, maxWait,
				)
				if allowed != want || positiveWait && wait <= 0 || !positiveWait && wait != 0 {
					t.Fatalf(
						"retry=(%v,%t), want positive wait=%t allowed=%t",
						wait,
						allowed,
						positiveWait,
						want,
					)
				}
				if mode == "ready 429" || mode == "ready 502" {
					if wait, allowed := manager.shouldRetryAfterError(
						ctx, opts, failure, 1, []string{provider}, model, maxWait,
					); allowed || wait != 0 {
						t.Fatalf("exhausted local round retry=(%v,%t)", wait, allowed)
					}
				}
			},
		)
	}
}

func TestOriginalOverrideCredentialRejectionsAndSession(t *testing.T) {
	for _, test := range []struct {
		name, provider, model, first, second string
		failure                              *Error
	}{
		{
			"model unsupported", "claude", "claude-opus-4-6", "aa-bad-auth", "bb-good-auth",
			&Error{HTTPStatus: 400, Message: "invalid_request_error: The requested model is not supported."},
		},
		{
			"invalid grant", "antigravity", "gemini-3-pro-preview", "aa-bad-auth", "bb-good-auth", &Error{
				HTTPStatus: 400,
				Message: `bad response status code 400, message: {"error":"invalid_grant","error` +
					`_description":"Bad Request"}, body: {"type":"error","error":{"type":"i` +
					`nvalid_request_error","message":"{\"error\":\"invalid_grant\"}"}}`,
			},
		},
		{
			"insufficient balance", "openai-compatibility", "deepseek-v4-pro", "aa-empty-balance",
			"bb-available-balance", &Error{
				HTTPStatus: 402,
				Message: `{"error":{"message":"Insufficient Balance","type":"unknown_error",` +
					`"param":null,"code":"invalid_request_error"}}`,
			},
		},
		{
			"rate limit with generic request error code", "openai-compatibility", "deepseek-v4-pro", "aa-failed-key",
			"bb-valid-key", &Error{
				HTTPStatus: 429,
				Message: `{"error":{"code":"invalid_request_error","message":"Rate Limit ` +
					`Reached","param":null,"type":` +
					`"unknown_error"}}`,
			},
		},
		{
			"authentication failure", "openai-compatibility", "deepseek-v4-pro", "aa-failed-key", "bb-valid-key",
			&Error{
				HTTPStatus: 401,
				Message: `{"error":{"code":"invalid_request_error","message":"Authentication Fails, Your api ` +
					`key: ****heck is invalid","param":null,"type":"authentication_error"}}`,
			},
		},
	} {
		for _, kind := range []string{"execute", "stream"} {
			t.Run(
				test.name+"/"+kind, func(t *testing.T) {
					manager := NewManager(nil, &FillFirstSelector{}, nil)
					manager.SetHomeClientProvider(func() HomeClient { return nil })
					manager.SetRetryConfig(2, 30*time.Second, 0)
					opts := core.Options{}
					var affinity *SessionAffinitySelector
					if test.name == "insufficient balance" {
						affinity = NewSessionAffinitySelectorWithConfig(
							SessionAffinityConfig{
								Fallback: &RoundRobinSelector{}, TTL: time.Hour,
							},
						)
						t.Cleanup(affinity.Stop)
						manager.SetSelector(affinity)
						opts.Metadata = map[string]any{
							core.DerivedSessionIDMetadataKey: "deepseek-insufficient-balance",
						}
					}
					var calls []string
					attempt := func(auth *Auth) (core.Response, error) {
						calls = append(calls, auth.ID)
						if auth.ID == test.first {
							return core.Response{}, test.failure
						}
						return core.Response{Payload: []byte(auth.ID)}, nil
					}
					executor := &requestScopedTestExecutor{provider: test.provider, execute: attempt}
					executor.stream = func(auth *Auth) (*core.StreamResult, error) {
						response, err := attempt(auth)
						chunks := make(chan core.StreamChunk, 1)
						chunks <- core.StreamChunk{Payload: response.Payload, Err: err}
						close(chunks)
						return &core.StreamResult{
							Headers: http.Header{"X-Auth": {auth.ID}},
							Chunks:  chunks,
						}, nil
					}
					manager.RegisterExecutor(executor)
					for _, id := range []string{test.first, test.second} {
						if _, err := manager.Register(
							WithSkipPersist(t.Context()), &Auth{ID: id, Provider: test.provider},
						); err != nil {
							t.Fatal(err)
						}
						registerProjectionTestClient(t, id, test.provider, &registry.ModelInfo{ID: test.model})
						manager.RefreshSchedulerEntry(id)
					}
					before := time.Now()
					for range 2 {
						var payload []byte
						var err error
						if kind == "stream" {
							var stream *core.StreamResult
							stream, err = manager.ExecuteStream(
								t.Context(), []string{test.provider}, core.Request{Model: test.model}, opts,
							)
							if stream != nil {
								for chunk := range stream.Chunks {
									payload = append(payload, chunk.Payload...)
									if chunk.Err != nil {
										err = chunk.Err
									}
								}
							}
						} else {
							var response core.Response
							response, err = manager.Execute(
								t.Context(), []string{test.provider}, core.Request{Model: test.model}, opts,
							)
							payload = response.Payload
						}
						if err != nil || string(payload) != test.second {
							t.Fatalf("response=%s error=%v calls=%v", payload, err, calls)
						}
					}
					if !reflect.DeepEqual(calls, []string{test.first, test.second, test.second}) {
						t.Fatalf("attempted credentials=%v", calls)
					}
					failed, _ := manager.GetByID(test.first)
					if blocked, _, _ := isAuthBlockedForModel(failed, test.model, time.Now()); !blocked {
						t.Fatal("rejected credential or model remained selectable")
					}
					if test.name == "insufficient balance" {
						if failed.NextRetryAfter.Before(before.Add(29*time.Minute)) ||
							failed.NextRetryAfter.After(before.Add(31*time.Minute)) {
							t.Fatalf("credential balance cooldown=%v", failed.NextRetryAfter)
						}
						if id, _ := affinity.LookupAffinity(
							test.provider, test.model, "deepseek-insufficient-balance",
						); id != test.second {
							t.Fatalf("rebound session credential=%q", id)
						}
					}
				},
			)
		}
	}
}

func TestOriginalOverrideUnknownAndDisableCoolingExecution(t *testing.T) {
	for _, test := range []struct {
		name, provider, id, model, sibling string
		failure                            error
		disabled                           bool
		retries, calls                     int
	}{
		{
			"additional local round unknown", "claude", "retry-auth", "retry-model", "",
			&Error{HTTPStatus: 500, Message: "boom"}, true, 1, 1,
		},
		{
			"unknown Gemini", "gemini", "aa-bad-auth", "gemini-3.6-pro", "gemini-3.6-flash", &Error{
				HTTPStatus: 500,
				Message:    `{"error":{"code":500,"message":"Internal error encountered.","status":"UNKNOWN"}}`,
			}, false, 3, 1,
		},
		{
			"disabled forbidden", "claude", "auth-403-exec", "test-model-403-exec", "",
			&Error{HTTPStatus: 403, Message: "forbidden"}, true, 0, 1,
		},
		{
			"disabled quota", "claude", "auth-429-exec", "test-model-429-exec", "",
			&requestScopedTestError{status: 429, message: "quota exhausted", retryAfter: new(2 * time.Minute)}, true, 0,
			1,
		},
		{
			"disabled quota retry", "claude", "auth-429-retryafter-exec", "test-model-429-retryafter-exec", "",
			&requestScopedTestError{status: 429, message: "quota exhausted", retryAfter: new(5 * time.Millisecond)},
			true, 3, 4,
		},
	} {
		for _, kind := range []string{"execute", "count", "stream"} {
			t.Run(
				test.name+"/"+kind, func(t *testing.T) {
					manager := NewManager(nil, &FillFirstSelector{}, nil)
					manager.SetHomeClientProvider(func() HomeClient { return nil })
					manager.SetRetryConfig(test.retries, 100*time.Millisecond, 0)
					executor := &roundExecutor{provider: test.provider, failure: test.failure}
					manager.RegisterExecutor(executor)
					auth, err := manager.Register(
						WithSkipPersist(t.Context()), &Auth{
							ID: test.id, Provider: test.provider,
							Metadata: map[string]any{"disable_cooling": test.disabled},
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					infos := []*registry.ModelInfo{{ID: test.model}}
					if test.sibling != "" {
						infos = append(infos, &registry.ModelInfo{ID: test.sibling})
					}
					registerProjectionTestClient(t, auth.ID, auth.Provider, infos...)
					manager.RefreshSchedulerEntry(auth.ID)
					if test.name == "unknown Gemini" {
						if _, err := manager.Register(
							WithSkipPersist(t.Context()), &Auth{ID: "bb-good-auth", Provider: test.provider},
						); err != nil {
							t.Fatal(err)
						}
						registerProjectionTestClient(t, "bb-good-auth", test.provider, infos...)
						manager.RefreshSchedulerEntry("bb-good-auth")
					}
					for request := range 2 {
						var executionErr error
						switch kind {
						case "execute":
							_, executionErr = manager.Execute(
								t.Context(),
								[]string{test.provider},
								core.Request{Model: test.model},
								core.Options{},
							)
						case "count":
							_, executionErr = manager.ExecuteCount(
								t.Context(),
								[]string{test.provider},
								core.Request{Model: test.model},
								core.Options{},
							)
						case "stream":
							_, executionErr = manager.ExecuteStream(
								t.Context(),
								[]string{test.provider},
								core.Request{Model: test.model},
								core.Options{},
							)
						}
						if statusCodeFromError(executionErr) != statusCodeFromError(test.failure) ||
							len(executor.calls) != (request+1)*test.calls {
							t.Fatalf(
								"error=%v calls=%v want attempts/request=%d",
								executionErr,
								executor.calls,
								test.calls,
							)
						}
					}
					current, _ := manager.GetByID(auth.ID)
					if current.Unavailable || !current.NextRetryAfter.IsZero() ||
						registry.GetGlobalRegistry().IsModelSuspendedForClient(
							auth.ID, test.model,
						) {
						t.Fatalf("neutral or disabled-cooling result changed availability: %+v", current)
					}
					if test.sibling != "" {
						if _, err := manager.SelectAuth(
							t.Context(), test.provider, test.sibling, core.Options{},
						); err != nil {
							t.Fatalf("independent sibling became unavailable: %v", err)
						}
					}
				},
			)
		}
	}
}

func TestOriginalOverrideDirectStateAndErrors(t *testing.T) {
	for _, test := range []struct {
		id, provider, model, body string
		status, transient         int
		disabled, credential      bool
	}{
		{"auth-1", "claude", "test-model", "boom", 500, 0, true, false},
		{"auth-transient-default", "claude", "test-model-transient-default", "bad gateway", 502, 0, false, false},
		{
			"auth-transient-model-disabled", "claude", "test-model-transient-disabled", "bad gateway", 502, -1, false,
			false,
		},
		{"auth-transient-auth-disabled", "claude", "", "unavailable", 503, -1, false, false},
		{"auth-transient-auth-error", "claude", "test-model-auth-error", "forbidden", 403, -1, false, true},
		{"auth-403", "claude", "test-model-403", "forbidden", 403, 0, true, false},
		{"auth-cf-403", "claude", "test-model-cf-403", "cf-mitigated: challenge", 403, 0, false, false},
		{"auth-1", "openai", "gpt-4.1", originalOverrideItemNotPersistedMessage, 404, 0, false, false},
		{
			"auth-request-fault", "deepseek", "deepseek-chat",
			`{"error":{"message":"Invalid request parameter","type":"invalid_request_error"}}`, 401, 0, false, false,
		},
		{
			"auth-empty-model", "deepseek", "",
			`{"error":{"message":"Invalid request parameter","type":"invalid_request_error"}}`, 401, 0, false, false,
		},
		{
			"auth-real-fail", "deepseek", "deepseek-chat",
			`{"error":{"message":"Authentication Fails, Your api key is invalid","type":"authentication_error"}}`, 401,
			0, false, true,
		},
	} {
		t.Run(
			test.id+"/"+test.model, func(t *testing.T) {
				previousQuota, previousTransient := quotaCooldownDisabled.Load(), transientErrorCooldownSeconds.Load()
				quotaCooldownDisabled.Store(false)
				SetTransientErrorCooldownSeconds(test.transient)
				t.Cleanup(
					func() {
						quotaCooldownDisabled.Store(previousQuota)
						transientErrorCooldownSeconds.Store(previousTransient)
					},
				)
				manager := NewManager(nil, nil, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				auth, err := manager.Register(
					WithSkipPersist(t.Context()), &Auth{
						ID: test.id, Provider: test.provider,
						Metadata: map[string]any{"disable_cooling": test.disabled},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				if test.model != "" {
					registerProjectionTestClient(t, auth.ID, test.provider, &registry.ModelInfo{ID: test.model})
				}
				before := time.Now()
				manager.MarkResult(
					t.Context(), executionResult(
						auth, auth.Provider, test.model, &Error{HTTPStatus: test.status, Message: test.body},
						core.DispatchPhaseResponseStarted,
					),
				)
				current, _ := manager.GetByID(auth.ID)
				if current.Failed != 1 || current.Unavailable != test.credential ||
					current.NextRetryAfter.IsZero() == test.credential {
					t.Fatalf("direct result credential=%+v", current)
				}
				if test.credential &&
					(current.NextRetryAfter.Before(
						before.Add(
							29*
								time.Minute,
						),
					) ||
						current.NextRetryAfter.After(
							before.Add(
								31*
									time.Minute,
							),
						)) {
					t.Fatalf("credential auth cooldown=%v", current.NextRetryAfter)
				}
				if state := current.ModelStates[test.model]; state != nil &&
					(state.Unavailable || !state.NextRetryAfter.IsZero()) {
					t.Fatalf("credential/provider/request effect created model restriction: %+v", state)
				}
			},
		)
	}
}

func TestOriginalOverrideNeutralResultKeepsSchedulerSnapshot(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	auth, err := manager.Register(
		WithSkipPersist(context.Background()), &Auth{ID: "availability-neutral-auth", Provider: "claude"},
	)
	if err != nil {
		t.Fatal(err)
	}
	manager.scheduler.mu.Lock()
	before := manager.scheduler.providers[auth.Provider].auths[auth.ID].auth
	manager.scheduler.mu.Unlock()
	result := executionResult(
		auth, auth.Provider, "availability-neutral-model", NewRequestScopedError("404 page not found", 404),
		core.DispatchPhaseResponseStarted,
	)
	manager.MarkResult(t.Context(), result)
	manager.scheduler.mu.Lock()
	after := manager.scheduler.providers[auth.Provider].auths[auth.ID].auth
	manager.scheduler.mu.Unlock()
	current, _ := manager.GetByID(auth.ID)
	if before != after || current.Failed != 1 {
		t.Fatalf("neutral result replaced scheduler=%t failed=%d", before != after, current.Failed)
	}
}
