package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// noinspection GoDirectComparisonOfErrors
func TestOriginalRequestRuleExecutionScenarios(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })
	retryDelay := 100 * time.Millisecond
	for _, sample := range []struct {
		name, id, secondID, message, body, action, source string
		match, regex                                      []string
		status                                            int
		calls                                             int32
		cooldown, success, count, disabled, negative      bool
		retryAfter                                        *time.Duration
	}{
		{
			name: "stop", id: "auth-claude-1", secondID: "auth-claude-2", status: 400,
			message: `{"error": {"message": "maximum_context_length exceeded"}}`,
			match:   []string{"maximum_context_length", "context_length_exceeded"},
			regex:   []string{"maximum_context_length$", "^context_length_exceeded"}, action: "stop", calls: 1,
		},
		{
			name: "stop cooldown", id: "auth-claude-stop-cool", secondID: "auth-claude-second", status: 400,
			message: `{"error": {"message": "context_window_exceeded"}}`, match: []string{"context_window_exceeded"},
			action: "stop-and-cooldown", calls: 1, cooldown: true,
		},
		{
			name: "continue", id: "auth-claude-continue-1", secondID: "auth-claude-continue-2", status: 400,
			message: `{"error": {"message": "try_another_key"}}`, match: []string{"try_another_key"},
			action: "continue", calls: 2, success: true,
		},
		{
			name: "continue cooldown", id: "auth-claude-continue-cool-1", secondID: "auth-claude-continue-cool-2",
			status:  400,
			message: `{"error": {"message": "balance_insufficient"}}`, match: []string{"balance_insufficient"},
			action: "continue-and-cooldown", calls: 2, success: true, cooldown: true,
		},
		{
			name: "regex", id: "auth-regex-1", secondID: "auth-regex-2", status: 400,
			message: `{"error": {"message": "context_length_exceeded: 128000"}}`,
			regex:   []string{`context_length_exceeded:\s*\d+`}, action: "stop", calls: 1,
		},
		{
			name: "stop outer 429", id: "auth-retry-stop-1", status: 429,
			message: `{"error": {"message": "rate_limit_stop"}}`, match: []string{"rate_limit_stop"},
			action: "stop", calls: 1, retryAfter: &retryDelay,
		},
		{
			name: "disable cooling override", id: "auth-disable-cooling-override", status: 400,
			message: `{"error": {"message": "cooldown_anyway"}}`, match: []string{"cooldown_anyway"},
			action: "stop-and-cooldown", calls: 1, cooldown: true, disabled: true,
		},
		{
			name: "count cooldown", id: "auth-count-1", status: 404, message: "count_endpoint_cooldown",
			match: []string{"count_endpoint_cooldown"}, action: "stop-and-cooldown", calls: 1, count: true,
			cooldown: true,
		},
		{
			name: "native config", id: "auth-config-resolve-1", status: 400, message: "from_config_rule occurred",
			match: []string{"from_config_rule"}, action: "stop", source: "native", calls: 1,
		},
		{
			name: "compat config", id: "auth-config-resolve-compat", status: 400,
			message: "from_compat_config_rule occurred",
			match:   []string{"from_compat_config_rule"}, action: "stop", source: "compat", calls: 1,
		},
		{
			name: "unmatched request fault", id: "auth-nomatch-1", status: 400,
			message: `{"error": {"message": "Invalid request parameter", "type": "invalid_request_error"}}`,
			match:   []string{"some_500_error"}, action: "stop", calls: 1,
		},
		{
			name: "negative transient force", id: "auth-transient-disabled-1", status: 500, message: "cooldown_on_500",
			match: []string{"cooldown_on_500"}, action: "stop-and-cooldown", calls: 1, cooldown: true, negative: true,
		},
		{
			name: "bare compat", id: "auth-bare-compat", status: 400, message: "from_bare_compat_rule",
			match: []string{"from_bare_compat_rule"}, action: "stop", source: "bare", calls: 1,
		},
		{
			name: "response body", id: "auth-fast-wrapped-1", status: 400,
			message: "claude Fast upstream request failed with status 400",
			body:    `{"type":"error","error":{"type":"invalid_request_error","message":"claude_fast_overload"}}`,
			match:   []string{"claude_fast_overload"}, action: "stop-and-cooldown", calls: 1, cooldown: true,
		},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				if sample.negative {
					previous := transientErrorCooldownSeconds.Load()
					transientErrorCooldownSeconds.Store(-1)
					t.Cleanup(func() { transientErrorCooldownSeconds.Store(previous) })
				}
				provider, model := "claude", "claude-3"
				auth := &Auth{
					ID: sample.id, Provider: provider, Status: StatusActive,
					Attributes: map[string]string{"priority": "10"}, Metadata: map[string]any{},
				}
				ruleStatus := sample.status
				if sample.name == "unmatched request fault" {
					ruleStatus = 500
				}
				rules := []config.RequestScopedErrorRule{
					{
						Status: ruleStatus, Match: sample.match, MatchRegexr: sample.regex, Action: sample.action,
					},
				}
				manager := NewManager(nil, &FillFirstSelector{}, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				if sample.name == "stop outer 429" {
					manager.SetRetryConfig(3, 5*time.Second, 5)
				}
				if sample.source == "" {
					auth.Metadata["request_scoped_errors"] = rules
				} else {
					cfg := &config.Config{
						ClaudeKey: []config.ClaudeKey{
							{
								APIKey: "sk-ant-test", RequestScopedErrors: rules,
							},
						},
					}
					auth.Attributes[AttributeConfigIndex] = "0"
					if sample.source != "native" {
						name := "my-compat"
						if sample.source == "bare" {
							name = "bare-compat"
						}
						provider, model = "openai-compatible-"+name, "compat-model"
						auth.Provider = provider
						cfg.OpenAICompatibility = []config.OpenAICompatibility{
							{
								Name: name, BaseURL: "https://compat.api", RequestScopedErrors: rules,
							},
						}
						if sample.source == "bare" {
							model, auth.Attributes = "bare-model", nil
						} else {
							auth.Attributes["compat_name"] = name
							auth.Attributes["provider_key"] = provider
						}
					}
					manager.SetConfig(cfg)
				}
				if sample.disabled {
					auth.Metadata["disable_cooling"] = true
				}
				registerRequestScopedTestAuth(t, manager, auth, model)
				if sample.secondID != "" {
					registerRequestScopedTestAuth(
						t, manager, &Auth{
							ID: sample.secondID, Provider: provider, Status: StatusActive,
							Attributes: map[string]string{"priority": "5"},
						}, model,
					)
				}
				upstream := &requestScopedTestError{
					status: sample.status, message: sample.message, body: []byte(sample.body),
					retryAfter: sample.retryAfter,
				}
				invoke := func(selected *Auth) (core.Response, error) {
					if selected.ID == sample.id {
						return core.Response{}, upstream
					}
					return core.Response{Payload: []byte(`{"result":"success"}`)}, nil
				}
				executor := &requestScopedTestExecutor{provider: provider, execute: invoke, count: invoke}
				manager.RegisterExecutor(executor)
				var response core.Response
				var err error
				if sample.count {
					response, err = manager.ExecuteCount(
						t.Context(), []string{provider}, core.Request{Model: model}, core.Options{},
					)
				} else {
					response, err = manager.Execute(
						t.Context(), []string{provider}, core.Request{Model: model}, core.Options{},
					)
				}
				if sample.success {
					if err != nil || string(response.Payload) != `{"result":"success"}` {
						t.Fatalf("response=%s error=%v", response.Payload, err)
					}
				} else if err != upstream {
					t.Fatalf("error=%T/%v, want original %v", err, err, upstream)
				}
				if calls := executor.calls.Load() + executor.countCalls.Load(); calls != sample.calls {
					t.Fatalf("calls=%d, want %d", calls, sample.calls)
				}
				current, _ := manager.GetByID(auth.ID)
				if current.Failed != 1 || current.Unavailable != sample.cooldown ||
					current.NextRetryAfter.IsZero() == sample.cooldown {
					t.Fatalf(
						"published availability=%t retry=%v failures=%d", current.Unavailable, current.NextRetryAfter,
						current.Failed,
					)
				}
				assertRequestScopedCooldown(t, manager, auth.ID, model, sample.cooldown)
			},
		)
	}
}

// noinspection GoDirectComparisonOfErrors
func TestOriginalRequestRuleStreamScenarios(t *testing.T) {
	for _, sample := range []struct {
		name, id, secondID, message, action, match, payload string
		direct, cooldown, unmatched                         bool
	}{
		{
			name: "direct stop", id: "auth-stream-1", secondID: "auth-stream-2", message: "stream_context_overflow",
			action: "stop", match: "stream_context_overflow", direct: true,
		},
		{
			name: "bootstrap cooldown", id: "auth-stream-boot-1", secondID: "auth-stream-boot-2",
			message: "bootstrap_chunk_error", action: "stop-and-cooldown", match: "bootstrap_chunk_error",
			cooldown: true,
		},
		{
			name: "subsequent cooldown", id: "auth-stream-subsequent-1", message: "mid_stream_context_length",
			action: "stop-and-cooldown", match: "mid_stream_context_length",
			payload: `data: {"type":"message_start"}\n\n`, cooldown: true,
		},
		{
			name: "unmatched bootstrap", id: "auth-unmatched-boot-1",
			message: `{"error":{"type":"invalid_request_error","message":"Unmatched bad request"}}`, action: "stop",
			match: "rule_does_not_match", unmatched: true,
		},
	} {
		t.Run(
			sample.name, func(t *testing.T) {
				const provider, model = "claude", "claude-3"
				manager := NewManager(nil, &FillFirstSelector{}, nil)
				manager.SetHomeClientProvider(func() HomeClient { return nil })
				status := 400
				if sample.unmatched {
					status = 500
				}
				registerRequestScopedTestAuth(
					t, manager, &Auth{
						ID: sample.id, Provider: provider, Status: StatusActive,
						Attributes: map[string]string{"priority": "10"},
						Metadata: map[string]any{
							"request_scoped_errors": []config.RequestScopedErrorRule{
								{
									Status: status, Match: []string{sample.match}, Action: sample.action,
								},
							},
						},
					}, model,
				)
				if sample.secondID != "" {
					registerRequestScopedTestAuth(
						t, manager, &Auth{
							ID: sample.secondID, Provider: provider, Status: StatusActive,
							Attributes: map[string]string{"priority": "5"},
						}, model,
					)
				}
				upstream := &requestScopedTestError{status: 400, message: sample.message}
				executor := &requestScopedTestExecutor{
					provider: provider, stream: func(*Auth) (*core.StreamResult, error) {
						if sample.direct {
							return nil, upstream
						}
						chunks := make(chan core.StreamChunk, 2)
						if sample.payload != "" {
							chunks <- core.StreamChunk{Payload: []byte(sample.payload)}
						}
						chunks <- core.StreamChunk{Err: upstream}
						close(chunks)
						return &core.StreamResult{
							Headers: http.Header{"Content-Type": {"text/event-stream"}}, Chunks: chunks,
						}, nil
					},
				}
				manager.RegisterExecutor(executor)
				result, delivered := manager.ExecuteStream(
					t.Context(), []string{provider}, core.Request{Model: model}, core.Options{},
				)
				var payload []byte
				if result != nil {
					if !sample.direct && result.Headers.Get("Content-Type") != "text/event-stream" {
						t.Fatalf("stream headers=%v", result.Headers)
					}
					for chunk := range result.Chunks {
						payload = append(payload, chunk.Payload...)
						if chunk.Err != nil {
							delivered = chunk.Err
						}
						if chunk.Acknowledge != nil {
							chunk.Acknowledge(nil)
						}
					}
				}
				var original *requestScopedTestError
				if !errors.As(delivered, &original) || original != upstream || isRequestStopError(delivered) ||
					string(payload) != sample.payload || executor.streamCalls.Load() != 1 {
					t.Fatalf(
						"error=%T/%v payload=%q calls=%d", delivered, delivered, payload, executor.streamCalls.Load(),
					)
				}
				assertRequestScopedCooldown(t, manager, sample.id, model, sample.cooldown)
			},
		)
	}
}

// noinspection GoDirectComparisonOfErrors
func TestOriginalRequestRuleBoundaryMarkers(t *testing.T) {
	base := &Error{Code: "request_failed", Message: "request failed", HTTPStatus: http.StatusBadRequest}
	for _, err := range []error{
		wrapRequestStopError(&requestRuleDispositionError{error: base}),
		&requestRuleDispositionError{error: wrapRequestStopError(base)},
	} {
		if got := unwrapRequestScopedActionError(err); got != base {
			t.Fatalf("unwrapped=%T/%v, want original", got, got)
		}
	}
}

func TestOriginalRequestRuleLegacyMetadataInput(t *testing.T) {
	auth := &Auth{
		Metadata: map[string]any{
			"request-scoped-errors": []any{
				map[string]any{"status": float64(429), "match": []any{"legacy-rate-limit"}, "action": "stop"},
			},
		},
	}
	rules := extractRequestScopedErrorRules(auth, nil)
	if len(rules) != 1 || rules[0].Status != 429 || rules[0].Action != "stop" ||
		rules[0].Match[0] != "legacy-rate-limit" {
		t.Fatalf("legacy rules=%+v", rules)
	}
	if _, matched := matchRequestScopedErrorAction(auth, context.Canceled, nil); matched {
		t.Fatal("cancellation matched the legacy response rule")
	}
}
