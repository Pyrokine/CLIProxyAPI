package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type requestScopedTestError struct {
	status     int
	message    string
	body       []byte
	retryAfter *time.Duration
}

func (e *requestScopedTestError) Error() string { return e.message }

func (e *requestScopedTestError) StatusCode() int { return e.status }

func (e *requestScopedTestError) ResponseBody() []byte { return e.body }

func (e *requestScopedTestError) RetryAfter() *time.Duration { return e.retryAfter }

type requestScopedDispositionError struct {
	*requestScopedTestError
	disposition cliproxyexecutor.ExecutionDisposition
}

func (e *requestScopedDispositionError) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	return e.disposition
}

type requestScopedTestExecutor struct {
	provider    string
	execute     func(*Auth) (cliproxyexecutor.Response, error)
	count       func(*Auth) (cliproxyexecutor.Response, error)
	stream      func(*Auth) (*cliproxyexecutor.StreamResult, error)
	calls       atomic.Int32
	countCalls  atomic.Int32
	streamCalls atomic.Int32
}

func (e *requestScopedTestExecutor) Identifier() string { return e.provider }

func (e *requestScopedTestExecutor) Execute(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	if e.execute == nil {
		return cliproxyexecutor.Response{}, errors.New("unexpected Execute call")
	}
	return e.execute(auth)
}

func (e *requestScopedTestExecutor) ExecuteStream(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (*cliproxyexecutor.StreamResult, error) {
	e.streamCalls.Add(1)
	if e.stream == nil {
		return nil, errors.New("unexpected ExecuteStream call")
	}
	return e.stream(auth)
}

func (*requestScopedTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *requestScopedTestExecutor) CountTokens(
	_ context.Context,
	auth *Auth,
	_ cliproxyexecutor.Request,
	_ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	e.countCalls.Add(1)
	if e.count == nil {
		return cliproxyexecutor.Response{}, errors.New("unexpected CountTokens call")
	}
	return e.count(auth)
}

func (*requestScopedTestExecutor) HttpRequest(
	context.Context,
	*Auth,
	*http.Request,
) (*http.Response, error) {
	return nil, errors.New("unexpected HttpRequest call")
}

func TestMatchRequestScopedErrorAction(t *testing.T) {
	responseBody := &requestScopedTestError{
		status:  http.StatusBadRequest,
		message: "message-token",
		body:    []byte("body-token"),
	}
	tests := []struct {
		name   string
		rules  []internalconfig.RequestScopedErrorRule
		err    error
		action string
		match  bool
	}{
		{
			name: "first valid match wins",
			rules: []internalconfig.RequestScopedErrorRule{
				{Status: 400, Match: []string{"token"}, Action: "invalid"},
				{Status: 400, MatchRegexr: []string{"[invalid"}, Action: "stop"},
				{Status: 400, Match: []string{"token"}, Action: "continue"},
				{Status: 400, Match: []string{"token"}, Action: "stop"},
			},
			err:    &requestScopedTestError{status: 400, message: "token"},
			action: RequestScopedActionContinue,
			match:  true,
		},
		{
			name: "status must match exactly",
			rules: []internalconfig.RequestScopedErrorRule{
				{Status: 429, Match: []string{"token"}, Action: "stop"},
			},
			err: &requestScopedTestError{status: 400, message: "token"},
		},
		{
			name: "substring is case sensitive",
			rules: []internalconfig.RequestScopedErrorRule{
				{Status: 400, Match: []string{"Token"}, Action: "stop"},
			},
			err: &requestScopedTestError{status: 400, message: "token"},
		},
		{
			name: "response body has priority",
			rules: []internalconfig.RequestScopedErrorRule{
				{Status: 400, Match: []string{"message-token"}, Action: "stop"},
				{Status: 400, Match: []string{"body-token"}, Action: "continue-and-cooldown"},
			},
			err:    fmt.Errorf("wrapped: %w", responseBody),
			action: RequestScopedActionContinueAndCooldown,
			match:  true,
		},
		{
			name: "valid regex matches",
			rules: []internalconfig.RequestScopedErrorRule{
				{Status: 400, MatchRegexr: []string{`^limit-\d+$`}, Action: "stop-and-cooldown"},
			},
			err:    &requestScopedTestError{status: 400, message: "limit-128"},
			action: RequestScopedActionStopAndCooldown,
			match:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := &Auth{Metadata: map[string]any{"request_scoped_errors": test.rules}}
			action, matched := matchRequestScopedErrorAction(auth, test.err, nil)
			if action != test.action || matched != test.match {
				t.Fatalf("match = (%q, %t), want (%q, %t)", action, matched, test.action, test.match)
			}
		})
	}
}

func TestExtractRequestScopedErrorRulesPrecedenceAndSources(t *testing.T) {
	canonical := []internalconfig.RequestScopedErrorRule{{Status: 400, Match: []string{"canonical"}, Action: "stop"}}
	legacy := []any{map[string]any{"status": float64(400), "match": []any{"legacy"}, "action": "continue"}}
	auth := &Auth{Metadata: map[string]any{
		"request_scoped_errors": canonical,
		"request-scoped-errors": legacy,
	}}
	if rules := extractRequestScopedErrorRules(auth, nil); len(rules) != 1 || rules[0].Match[0] != "canonical" {
		t.Fatalf("canonical metadata precedence rules = %#v", rules)
	}
	if rules := extractRequestScopedErrorRules(&Auth{Metadata: map[string]any{"request-scoped-errors": legacy}}, nil); len(rules) != 1 || rules[0].Match[0] != "legacy" || rules[0].Action != "continue" {
		t.Fatalf("legacy metadata rules = %#v", rules)
	}

	rule := []internalconfig.RequestScopedErrorRule{{Status: 400, Match: []string{"config"}, Action: "stop"}}
	cfg := &internalconfig.Config{
		GeminiKey:       []internalconfig.GeminiKey{{APIKey: "g", RequestScopedErrors: rule}},
		InteractionsKey: []internalconfig.GeminiKey{{APIKey: "i", RequestScopedErrors: rule}},
		ClaudeKey:       []internalconfig.ClaudeKey{{APIKey: "c", RequestScopedErrors: rule}},
		CodexKey: []internalconfig.CodexKey{{
			APIKey: "x", BaseURL: "https://api.openai.com/v1", RequestScopedErrors: rule,
		}},
		XAIKey: []internalconfig.CodexKey{{
			APIKey: "a", BaseURL: "https://api.x.ai/v1", RequestScopedErrors: rule,
		}},
		OpenAICompatibility: []internalconfig.OpenAICompatibility{{
			Name: "compat", BaseURL: "https://example.test", RequestScopedErrors: rule,
		}},
		OAuthRequestScopedErrors: map[string][]internalconfig.RequestScopedErrorRule{
			"vertex": {{Status: 400, Match: []string{"oauth"}, Action: "continue"}},
		},
	}
	for _, test := range []struct {
		name string
		auth *Auth
	}{
		{name: "gemini", auth: &Auth{Provider: "gemini", Attributes: map[string]string{"api_key": "g"}}},
		{name: "interactions", auth: &Auth{Provider: "interactions", Attributes: map[string]string{"api_key": "i"}}},
		{name: "claude", auth: &Auth{Provider: "claude", Attributes: map[string]string{"api_key": "c"}}},
		{name: "codex", auth: &Auth{Provider: "codex", Attributes: map[string]string{"api_key": "x", "base_url": "https://api.openai.com/v1"}}},
		{name: "xai", auth: &Auth{Provider: "xai", Attributes: map[string]string{"api_key": "a", "base_url": "https://api.x.ai/v1"}}},
		{name: "openai compatibility", auth: &Auth{Provider: "compat", Attributes: map[string]string{AttributeConfigIndex: "0", "provider_key": "compat", "compat_name": "compat"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rules := extractRequestScopedErrorRules(test.auth, cfg)
			if len(rules) != 1 || rules[0].Match[0] != "config" {
				t.Fatalf("config rules = %#v", rules)
			}
		})
	}

	oauth := &Auth{Provider: " Vertex ", Attributes: map[string]string{"auth_kind": "oauth"}}
	if rules := extractRequestScopedErrorRules(oauth, cfg); len(rules) != 1 || rules[0].Match[0] != "oauth" {
		t.Fatalf("OAuth rules = %#v", rules)
	}
	apiKey := &Auth{Provider: "vertex", Attributes: map[string]string{"auth_kind": "apikey"}}
	if rules := extractRequestScopedErrorRules(apiKey, cfg); len(rules) != 0 {
		t.Fatalf("API key consumed OAuth rules: %#v", rules)
	}
}

func TestApplyRequestScopedErrorRuleActionMatrix(t *testing.T) {
	const model = "model-a"
	auth := &Auth{
		ID: "auth-a", Provider: "claude", Generation: 17, StateGeneration: 5,
		ModelStates: map[string]*ModelState{
			"model-a": {Generation: 7, QuotaEpoch: 23},
		},
	}
	base := cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
		Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: cliproxyexecutor.RetryDirectiveNextAuth,
		EffectScope:    cliproxyexecutor.StateEffectScopeCredential,
		EffectKey:      auth.ID,
	}
	tests := []struct {
		name           string
		action         string
		disposition    cliproxyexecutor.ExecutionDisposition
		wantStop       bool
		wantRetry      cliproxyexecutor.RetryDirective
		wantScope      cliproxyexecutor.StateEffectScope
		wantKey        string
		wantGeneration uint64
		forceCooldown  bool
	}{
		{name: "stop", action: RequestScopedActionStop, disposition: base, wantStop: true, wantRetry: cliproxyexecutor.RetryDirectiveNoRetry, wantScope: cliproxyexecutor.StateEffectScopeRequest},
		{name: "continue", action: RequestScopedActionContinue, disposition: base, wantRetry: cliproxyexecutor.RetryDirectiveNextAuth, wantScope: cliproxyexecutor.StateEffectScopeRequest},
		{name: "stop and cooldown", action: RequestScopedActionStopAndCooldown, disposition: requestScopedDispositionWithScope(base, cliproxyexecutor.StateEffectScopeRequest), wantStop: true, wantRetry: cliproxyexecutor.RetryDirectiveNoRetry, wantScope: cliproxyexecutor.StateEffectScopeExactModel, wantKey: "model-a", wantGeneration: 7, forceCooldown: true},
		{name: "continue and cooldown", action: RequestScopedActionContinueAndCooldown, disposition: requestScopedDispositionWithScope(base, cliproxyexecutor.StateEffectScopeRequest), wantRetry: cliproxyexecutor.RetryDirectiveNextAuth, wantScope: cliproxyexecutor.StateEffectScopeExactModel, wantKey: "model-a", wantGeneration: 7, forceCooldown: true},
		{name: "continue unknown", action: RequestScopedActionContinue, disposition: requestScopedDispositionWithOutcome(base, cliproxyexecutor.ExecutionOutcomeUnknown), wantRetry: cliproxyexecutor.RetryDirectiveNoRetry, wantScope: cliproxyexecutor.StateEffectScopeRequest},
		{name: "continue processed failure", action: RequestScopedActionContinue, disposition: requestScopedDispositionWithOutcome(base, cliproxyexecutor.ExecutionOutcomeFailed), wantRetry: cliproxyexecutor.RetryDirectiveNoRetry, wantScope: cliproxyexecutor.StateEffectScopeRequest},
		{name: "continue downstream committed", action: RequestScopedActionContinue, disposition: requestScopedDispositionWithPhase(base, cliproxyexecutor.DispatchPhaseDownstreamCommitted), wantRetry: cliproxyexecutor.RetryDirectiveNoRetry, wantScope: cliproxyexecutor.StateEffectScopeRequest},
		{name: "continue preserves retry after", action: RequestScopedActionContinue, disposition: requestScopedDispositionWithRetry(base, cliproxyexecutor.RetryDirectiveRetryAfter), wantRetry: cliproxyexecutor.RetryDirectiveRetryAfter, wantScope: cliproxyexecutor.StateEffectScopeRequest},
		{name: "forced credential cooldown", action: RequestScopedActionStopAndCooldown, disposition: base, wantStop: true, wantRetry: cliproxyexecutor.RetryDirectiveNoRetry, wantScope: cliproxyexecutor.StateEffectScopeCredential, wantKey: auth.ID, wantGeneration: 5, forceCooldown: true},
	}

	manager := NewManager(nil, nil, nil)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth.Metadata = map[string]any{"request_scoped_errors": []internalconfig.RequestScopedErrorRule{{
				Status: 400, Match: []string{"matched"}, Action: test.action,
			}}}
			upstream := &requestScopedTestError{status: 400, message: "matched"}
			result := Result{
				AuthID: auth.ID, AuthGeneration: auth.Generation, Provider: auth.Provider, Model: model,
				ModelQuotaEpoch: 23, StateGeneration: 99, Disposition: test.disposition,
				Error: errorForResultWithDisposition(upstream, test.disposition, 99),
			}
			decision := manager.applyRequestScopedErrorRule(auth, upstream, &result)
			if !decision.matched || decision.action != test.action || decision.stop != test.wantStop {
				t.Fatalf("decision = %#v", decision)
			}
			if result.Disposition.RetryDirective != test.wantRetry || result.Disposition.EffectScope != test.wantScope ||
				result.Disposition.EffectKey != test.wantKey || result.StateGeneration != test.wantGeneration {
				t.Fatalf("result disposition = %+v, generation = %d", result.Disposition, result.StateGeneration)
			}
			if result.Disposition.DispatchPhase != test.disposition.DispatchPhase || result.Disposition.Outcome != test.disposition.Outcome {
				t.Fatalf("phase/outcome changed: %+v -> %+v", test.disposition, result.Disposition)
			}
			if result.AuthGeneration != 17 || result.ModelQuotaEpoch != 23 {
				t.Fatalf("auth generation or quota epoch changed: %+v", result)
			}
			if test.forceCooldown != isForceCooldownResult(result) {
				t.Fatalf("force cooldown = %t, result error = %#v", isForceCooldownResult(result), result.Error)
			}
			if unwrapped := unwrapRequestScopedActionError(decision.err); unwrapped != upstream {
				t.Fatalf("public error = %T/%v, want original", unwrapped, unwrapped)
			}
			if result.Error == nil || result.Error.RetryDirective != test.wantRetry || result.Error.EffectScope != test.wantScope ||
				result.Error.EffectKey != test.wantKey || result.Error.StateGeneration != test.wantGeneration {
				t.Fatalf("result error disposition = %#v", result.Error)
			}
		})
	}
}

func requestScopedDispositionWithScope(
	disposition cliproxyexecutor.ExecutionDisposition,
	scope cliproxyexecutor.StateEffectScope,
) cliproxyexecutor.ExecutionDisposition {
	disposition.EffectScope = scope
	disposition.EffectKey = ""
	return disposition
}

func requestScopedDispositionWithOutcome(
	disposition cliproxyexecutor.ExecutionDisposition,
	outcome cliproxyexecutor.ExecutionOutcome,
) cliproxyexecutor.ExecutionDisposition {
	disposition.Outcome = outcome
	return disposition
}

func requestScopedDispositionWithPhase(
	disposition cliproxyexecutor.ExecutionDisposition,
	phase cliproxyexecutor.DispatchPhase,
) cliproxyexecutor.ExecutionDisposition {
	disposition.DispatchPhase = phase
	return disposition
}

func requestScopedDispositionWithRetry(
	disposition cliproxyexecutor.ExecutionDisposition,
	retry cliproxyexecutor.RetryDirective,
) cliproxyexecutor.ExecutionDisposition {
	disposition.RetryDirective = retry
	return disposition
}

func TestRequestScopedErrorActionsLocalExecution(t *testing.T) {
	tests := []struct {
		name         string
		action       string
		status       int
		alt          string
		outcome      cliproxyexecutor.ExecutionOutcome
		phase        cliproxyexecutor.DispatchPhase
		wantCalls    int32
		wantSuccess  bool
		wantCooldown bool
	}{
		{name: "stop", action: RequestScopedActionStop, status: 400, wantCalls: 1},
		{name: "stop and cooldown", action: RequestScopedActionStopAndCooldown, status: 400, wantCalls: 1, wantCooldown: true},
		{name: "continue", action: RequestScopedActionContinue, status: 400, wantCalls: 2, wantSuccess: true},
		{
			name: "continue compact request fault", action: RequestScopedActionContinue,
			status: 404, alt: "responses/compact", wantCalls: 2, wantSuccess: true,
		},
		{
			name: "stop and cooldown compact request fault", action: RequestScopedActionStopAndCooldown,
			status: 404, alt: "responses/compact", wantCalls: 1, wantCooldown: true,
		},
		{
			name: "continue and cooldown", action: RequestScopedActionContinueAndCooldown,
			status: 400, wantCalls: 2, wantSuccess: true, wantCooldown: true,
		},
		{name: "continue unknown is not replayed", action: RequestScopedActionContinue, status: 500, wantCalls: 1},
		{name: "continue processed failure is not replayed", action: RequestScopedActionContinue, status: 400, outcome: cliproxyexecutor.ExecutionOutcomeFailed, wantCalls: 1},
		{name: "continue downstream committed is not replayed", action: RequestScopedActionContinue, status: 400, outcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing, phase: cliproxyexecutor.DispatchPhaseDownstreamCommitted, wantCalls: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			const provider = "request-rule-provider"
			const model = "request-rule-model"
			firstError := &requestScopedTestError{status: test.status, message: "matched"}
			var upstream error = firstError
			if test.outcome != "" || test.phase != "" {
				disposition := cliproxyexecutor.HTTPStatusDisposition(test.status, false)
				if test.outcome != "" {
					disposition.Outcome = test.outcome
				}
				if test.phase != "" {
					disposition.DispatchPhase = test.phase
				}
				upstream = &requestScopedDispositionError{requestScopedTestError: firstError, disposition: disposition}
			}
			executor := &requestScopedTestExecutor{provider: provider}
			executor.execute = func(auth *Auth) (cliproxyexecutor.Response, error) {
				if auth.ID == "first" {
					return cliproxyexecutor.Response{}, upstream
				}
				return cliproxyexecutor.Response{Payload: []byte("success")}, nil
			}
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			manager.SetRetryConfig(2, 30*time.Second, 0)
			manager.RegisterExecutor(executor)
			registerRequestScopedTestAuth(t, manager, &Auth{
				ID: "first", Provider: provider, Status: StatusActive,
				Attributes: map[string]string{"priority": "10"},
				Metadata: map[string]any{"request_scoped_errors": []internalconfig.RequestScopedErrorRule{{
					Status: test.status, Match: []string{"matched"}, Action: test.action,
				}}},
			}, model)
			registerRequestScopedTestAuth(t, manager, &Auth{
				ID: "second", Provider: provider, Status: StatusActive,
				Attributes: map[string]string{"priority": "5"},
			}, model)

			response, err := manager.Execute(
				context.Background(),
				[]string{provider},
				cliproxyexecutor.Request{Model: model},
				cliproxyexecutor.Options{Alt: test.alt},
			)
			if test.wantSuccess {
				if err != nil || string(response.Payload) != "success" {
					t.Fatalf("Execute() response = %q, error = %v", response.Payload, err)
				}
			} else {
				if upstream != firstError {
					if err != upstream {
						t.Fatalf("Execute() error = %T/%v, want original disposition error", err, err)
					}
				} else {
					var got *requestScopedTestError
					if !errors.As(err, &got) || got != firstError {
						t.Fatalf("Execute() error = %T/%v, want original upstream error", err, err)
					}
				}
				if isRequestStopError(err) {
					t.Fatalf("public error leaked request stop marker: %T/%v", err, err)
				}
			}
			if calls := executor.calls.Load(); calls != test.wantCalls {
				t.Fatalf("Execute() calls = %d, want %d", calls, test.wantCalls)
			}
			assertRequestScopedCooldown(t, manager, "first", model, test.wantCooldown)
		})
	}
}

func TestRequestScopedErrorStopSuppressesOuterRetry(t *testing.T) {
	const provider = "request-rule-retry"
	const model = "request-rule-model"
	retryAfter := time.Millisecond
	upstream := &requestScopedTestError{status: 429, message: "matched", retryAfter: &retryAfter}
	executor := &requestScopedTestExecutor{provider: provider}
	executor.execute = func(*Auth) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{}, upstream
	}
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(3, time.Second, 1)
	manager.RegisterExecutor(executor)
	registerRequestScopedTestAuth(t, manager, &Auth{
		ID: "only", Provider: provider, Status: StatusActive,
		Metadata: map[string]any{"request_scoped_errors": []internalconfig.RequestScopedErrorRule{{
			Status: 429, Match: []string{"matched"}, Action: RequestScopedActionStop,
		}}},
	}, model)

	_, err := manager.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != upstream {
		t.Fatalf("Execute() error = %T/%v, want original", err, err)
	}
	if calls := executor.calls.Load(); calls != 1 {
		t.Fatalf("outer retry calls = %d, want 1", calls)
	}
}

func TestRequestScopedErrorCountTokensForceCooldownOverridesNeutral404(t *testing.T) {
	const provider = "request-rule-count"
	const model = "request-rule-model"
	upstream := &requestScopedTestError{status: 404, message: "count-endpoint-missing"}
	executor := &requestScopedTestExecutor{provider: provider}
	executor.count = func(*Auth) (cliproxyexecutor.Response, error) {
		return cliproxyexecutor.Response{}, upstream
	}
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerRequestScopedTestAuth(t, manager, &Auth{
		ID: "count", Provider: provider, Status: StatusActive,
		Metadata: map[string]any{
			"disable_cooling": true,
			"request_scoped_errors": []internalconfig.RequestScopedErrorRule{{
				Status: 404, Match: []string{"count-endpoint-missing"}, Action: RequestScopedActionStopAndCooldown,
			}},
		},
	}, model)

	_, err := manager.ExecuteCount(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if err != upstream {
		t.Fatalf("ExecuteCount() error = %T/%v, want original", err, err)
	}
	if calls := executor.countCalls.Load(); calls != 1 {
		t.Fatalf("CountTokens calls = %d, want 1", calls)
	}
	assertRequestScopedCooldown(t, manager, "count", model, true)
}

func TestRequestScopedErrorStreamBootstrapAndSubsequentChunk(t *testing.T) {
	t.Run("bootstrap continue redispatches safely", func(t *testing.T) {
		const provider = "request-rule-stream-continue"
		const model = "request-rule-model"
		upstream := &requestScopedTestError{status: 400, message: "bootstrap-matched"}
		executor := &requestScopedTestExecutor{provider: provider}
		executor.stream = func(auth *Auth) (*cliproxyexecutor.StreamResult, error) {
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			if auth.ID == "first" {
				chunks <- cliproxyexecutor.StreamChunk{Err: upstream}
			} else {
				chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("success")}
			}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Test": {"value"}}, Chunks: chunks}, nil
		}
		manager := NewManager(nil, &RoundRobinSelector{}, nil)
		manager.RegisterExecutor(executor)
		registerRequestScopedTestAuth(t, manager, &Auth{
			ID: "first", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "10"},
			Metadata: map[string]any{"request_scoped_errors": []internalconfig.RequestScopedErrorRule{{
				Status: 400, Match: []string{"bootstrap-matched"}, Action: RequestScopedActionContinue,
			}}},
		}, model)
		registerRequestScopedTestAuth(t, manager, &Auth{
			ID: "second", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "5"},
		}, model)

		result, err := manager.ExecuteStream(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
		if err != nil {
			t.Fatalf("ExecuteStream() error = %v", err)
		}
		var payload []byte
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("stream chunk error = %v", chunk.Err)
			}
			payload = append(payload, chunk.Payload...)
		}
		if string(payload) != "success" || executor.streamCalls.Load() != 2 {
			t.Fatalf("stream payload = %q, calls = %d", payload, executor.streamCalls.Load())
		}
		assertRequestScopedCooldown(t, manager, "first", model, false)
	})

	t.Run("bootstrap stop preserves headers and public error", func(t *testing.T) {
		const provider = "request-rule-stream-stop"
		const model = "request-rule-model"
		upstream := &requestScopedTestError{status: 400, message: "bootstrap-stop"}
		executor := &requestScopedTestExecutor{provider: provider}
		executor.stream = func(*Auth) (*cliproxyexecutor.StreamResult, error) {
			chunks := make(chan cliproxyexecutor.StreamChunk, 1)
			chunks <- cliproxyexecutor.StreamChunk{Err: upstream}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Headers: http.Header{"X-Test": {"value"}}, Chunks: chunks}, nil
		}
		manager := NewManager(nil, nil, nil)
		manager.RegisterExecutor(executor)
		registerRequestScopedTestAuth(t, manager, &Auth{
			ID: "only", Provider: provider, Status: StatusActive,
			Metadata: map[string]any{"request_scoped_errors": []internalconfig.RequestScopedErrorRule{{
				Status: 400, Match: []string{"bootstrap-stop"}, Action: RequestScopedActionStopAndCooldown,
			}}},
		}, model)

		result, err := manager.ExecuteStream(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
		if err != nil {
			t.Fatalf("ExecuteStream() returned bootstrap error outside stream: %v", err)
		}
		if result == nil || result.Headers.Get("X-Test") != "value" {
			t.Fatalf("stream result headers = %#v", result)
		}
		var delivered error
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				delivered = chunk.Err
			}
		}
		var got *requestScopedTestError
		if !errors.As(delivered, &got) || got != upstream || isRequestStopError(delivered) {
			t.Fatalf("delivered error = %T/%v, want original without action marker", delivered, delivered)
		}
		if executor.streamCalls.Load() != 1 {
			t.Fatalf("stream calls = %d, want 1", executor.streamCalls.Load())
		}
		assertRequestScopedCooldown(t, manager, "only", model, true)
	})

	t.Run("subsequent chunk changes state without redispatch", func(t *testing.T) {
		const provider = "request-rule-stream-chunk"
		const model = "request-rule-model"
		upstream := &requestScopedTestError{status: 400, message: "chunk-matched"}
		executor := &requestScopedTestExecutor{provider: provider}
		executor.stream = func(*Auth) (*cliproxyexecutor.StreamResult, error) {
			chunks := make(chan cliproxyexecutor.StreamChunk, 2)
			chunks <- cliproxyexecutor.StreamChunk{Payload: []byte("started")}
			chunks <- cliproxyexecutor.StreamChunk{Err: upstream}
			close(chunks)
			return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
		}
		manager := NewManager(nil, &RoundRobinSelector{}, nil)
		manager.RegisterExecutor(executor)
		registerRequestScopedTestAuth(t, manager, &Auth{
			ID: "first", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "10"},
			Metadata: map[string]any{"request_scoped_errors": []internalconfig.RequestScopedErrorRule{{
				Status: 400, Match: []string{"chunk-matched"}, Action: RequestScopedActionContinueAndCooldown,
			}}},
		}, model)
		registerRequestScopedTestAuth(t, manager, &Auth{
			ID: "second", Provider: provider, Status: StatusActive, Attributes: map[string]string{"priority": "5"},
		}, model)

		result, err := manager.ExecuteStream(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
		if err != nil {
			t.Fatalf("ExecuteStream() error = %v", err)
		}
		var delivered error
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				delivered = chunk.Err
			}
		}
		if delivered != upstream {
			t.Fatalf("delivered chunk error = %T/%v, want original", delivered, delivered)
		}
		if executor.streamCalls.Load() != 1 {
			t.Fatalf("stream calls = %d, want no redispatch", executor.streamCalls.Load())
		}
		assertRequestScopedCooldown(t, manager, "first", model, true)
	})
}

func TestRequestScopedErrorForceCooldownOverridesDisableCooling(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider string
		metadata map[string]any
		config   *internalconfig.Config
	}{
		{name: "global", provider: "claude", config: &internalconfig.Config{DisableCooling: true}},
		{name: "credential", provider: "claude", metadata: map[string]any{"disable_cooling": true}},
		{
			name: "provider", provider: "compat",
			config: &internalconfig.Config{OpenAICompatibility: []internalconfig.OpenAICompatibility{{
				Name: "compat", BaseURL: "https://example.test", DisableCooling: new(true),
			}}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			const model = "force-model"
			metadata := make(map[string]any, len(test.metadata)+1)
			for key, value := range test.metadata {
				metadata[key] = value
			}
			metadata["request_scoped_errors"] = []internalconfig.RequestScopedErrorRule{{
				Status: 500, Match: []string{"force"}, Action: RequestScopedActionStopAndCooldown,
			}}
			auth := &Auth{ID: "force-auth", Provider: test.provider, Status: StatusActive, Metadata: metadata}
			if test.name == "provider" {
				auth.Attributes = map[string]string{"provider_key": "compat", "compat_name": "compat"}
			}
			manager, current := newExecutionOutcomeTestManager(t, auth)
			manager.SetConfig(test.config)
			upstream := &requestScopedTestError{status: 500, message: "force"}
			result := executionResult(current, current.Provider, model, upstream, cliproxyexecutor.DispatchPhaseMaybeSent)
			manager.applyRequestScopedErrorRule(current, upstream, &result)
			before := time.Now()
			manager.MarkResult(context.Background(), result)
			updated := getExecutionOutcomeTestAuth(t, manager, current.ID)
			state := updated.ModelStates[model]
			if state == nil || !state.Unavailable || state.NextRetryAfter.Before(before.Add(59*time.Second)) ||
				state.NextRetryAfter.After(before.Add(61*time.Second)) {
				t.Fatalf("forced cooldown state = %#v", state)
			}
		})
	}
}

func TestRequestScopedErrorForceCooldownKeepsLaterDeadline(t *testing.T) {
	const model = "deadline-model"
	later := time.Now().Add(2 * time.Hour).Round(0)
	manager, current := newExecutionOutcomeTestManager(t, &Auth{
		ID: "deadline-auth", Provider: "claude", Status: StatusActive,
		Metadata: map[string]any{"request_scoped_errors": []internalconfig.RequestScopedErrorRule{{
			Status: 500, Match: []string{"force"}, Action: RequestScopedActionStopAndCooldown,
		}}},
		ModelStates: map[string]*ModelState{
			model: {Status: StatusError, Unavailable: true, NextRetryAfter: later, Generation: 4},
		},
	})
	upstream := &requestScopedTestError{status: 500, message: "force"}
	result := executionResult(current, current.Provider, model, upstream, cliproxyexecutor.DispatchPhaseMaybeSent)
	manager.applyRequestScopedErrorRule(current, upstream, &result)
	manager.MarkResult(context.Background(), result)

	state := getExecutionOutcomeTestAuth(t, manager, current.ID).ModelStates[model]
	if state == nil || !state.NextRetryAfter.Equal(later) {
		t.Fatalf("forced cooldown shortened deadline: %#v", state)
	}
}

func TestRequestScopedErrorRetryAfterSurvivesActionWrapper(t *testing.T) {
	retryAfter := 3 * time.Second
	upstream := &requestScopedTestError{status: 429, message: "rate", retryAfter: &retryAfter}
	auth := &Auth{Metadata: map[string]any{"request_scoped_errors": []internalconfig.RequestScopedErrorRule{{
		Status: 429, Match: []string{"rate"}, Action: RequestScopedActionStop,
	}}}}
	result := executionResult(auth, "claude", "model", upstream, cliproxyexecutor.DispatchPhaseMaybeSent)
	decision := NewManager(nil, nil, nil).applyRequestScopedErrorRule(auth, upstream, &result)
	got := retryAfterFromError(decision.err)
	if got == nil || *got != retryAfter {
		t.Fatalf("Retry-After through action wrapper = %v, want %v", got, retryAfter)
	}
}

func registerRequestScopedTestAuth(t *testing.T, manager *Manager, auth *Auth, model string) {
	t.Helper()
	if _, err := manager.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register(%s) error = %v", auth.ID, err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
}

func assertRequestScopedCooldown(t *testing.T, manager *Manager, authID, model string, want bool) {
	t.Helper()
	auth, ok := manager.GetByID(authID)
	if !ok || auth == nil {
		t.Fatalf("auth %q not found", authID)
	}
	state := auth.ModelStates[model]
	got := state != nil && state.Unavailable && !state.NextRetryAfter.IsZero()
	if got != want {
		t.Fatalf("auth %s cooldown = %t, want %t; auth = %+v, state = %+v", authID, got, want, auth, state)
	}
}
