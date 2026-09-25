package auth

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	internalconfig "github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const (
	RequestScopedActionStop                = "stop"
	RequestScopedActionStopAndCooldown     = "stop-and-cooldown"
	RequestScopedActionContinue            = "continue"
	RequestScopedActionContinueAndCooldown = "continue-and-cooldown"
)

type requestStopError struct {
	error
}

func (e requestStopError) Unwrap() error {
	return e.error
}

func (e requestStopError) IsRequestStop() bool {
	return true
}

type requestRuleDispositionError struct {
	error
	disposition cliproxyexecutor.ExecutionDisposition
}

func (e *requestRuleDispositionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.error
}

func (e *requestRuleDispositionError) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	if e == nil {
		return cliproxyexecutor.ExecutionDisposition{}
	}
	return e.disposition
}

type requestScopedActionDecision struct {
	action  string
	matched bool
	stop    bool
	err     error
}

func isRequestStopError(err error) bool {
	if err == nil {
		return false
	}
	type stopChecker interface {
		IsRequestStop() bool
	}
	var checker stopChecker
	return errors.As(err, &checker) && checker != nil && checker.IsRequestStop()
}

func wrapRequestStopError(err error) error {
	if err == nil || isRequestStopError(err) {
		return err
	}
	return requestStopError{error: err}
}

func unwrapRequestScopedActionError(err error) error {
	for err != nil {
		switch typed := err.(type) {
		case requestStopError:
			err = typed.error
		case *requestRuleDispositionError:
			err = typed.error
		default:
			return err
		}
	}
	return nil
}

func (m *Manager) runtimeConfigSnapshot() *internalconfig.Config {
	if m == nil {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg
}

func extractRequestScopedErrorRules(
	auth *Auth,
	cfg *internalconfig.Config,
) []internalconfig.RequestScopedErrorRule {
	if auth != nil && auth.Metadata != nil {
		raw, found := auth.Metadata["request_scoped_errors"]
		if !found {
			raw, found = auth.Metadata["request-scoped-errors"]
		}
		if found && raw != nil {
			if rules := decodeRequestScopedErrorRules(raw); len(rules) > 0 {
				return rules
			}
		}
	}
	if auth == nil || cfg == nil {
		return nil
	}

	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if auth.AuthKind() == AuthKindOAuth {
		return cfg.OAuthRequestScopedErrors[provider]
	}

	switch provider {
	case "gemini":
		if entry := resolveGeminiAPIKeyConfig(cfg, auth); entry != nil {
			return entry.RequestScopedErrors
		}
	case "interactions", "gemini-interactions":
		if entry := resolveInteractionsAPIKeyConfig(cfg, auth); entry != nil {
			return entry.RequestScopedErrors
		}
	case "claude":
		if entry := resolveClaudeAPIKeyConfig(cfg, auth); entry != nil {
			return entry.RequestScopedErrors
		}
	case "codex":
		if entry := resolveCodexAPIKeyConfig(cfg, auth); entry != nil {
			return entry.RequestScopedErrors
		}
	case "xai":
		if entry := resolveXAIAPIKeyConfig(cfg, auth); entry != nil {
			return entry.RequestScopedErrors
		}
	}

	providerKey := ""
	compatName := ""
	if auth.Attributes != nil {
		providerKey = strings.TrimSpace(auth.Attributes["provider_key"])
		compatName = strings.TrimSpace(auth.Attributes["compat_name"])
	}
	if providerKey != "" || compatName != "" || provider == "openai-compatibility" {
		if entry := resolveOpenAICompatConfig(cfg, providerKey, compatName, auth.Provider); entry != nil {
			return entry.RequestScopedErrors
		}
	}
	return nil
}

func decodeRequestScopedErrorRules(raw any) []internalconfig.RequestScopedErrorRule {
	switch typed := raw.(type) {
	case []internalconfig.RequestScopedErrorRule:
		return typed
	case *[]internalconfig.RequestScopedErrorRule:
		if typed != nil {
			return *typed
		}
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var rules []internalconfig.RequestScopedErrorRule
	if err = json.Unmarshal(data, &rules); err != nil {
		return nil
	}
	return rules
}

func extractRequestScopedErrorBody(err error) string {
	if err == nil {
		return ""
	}
	type responseBodyProvider interface {
		ResponseBody() []byte
	}
	var responseBody responseBodyProvider
	if errors.As(err, &responseBody) && responseBody != nil {
		if body := responseBody.ResponseBody(); len(body) > 0 {
			return string(body)
		}
	}
	var authErr *Error
	if errors.As(err, &authErr) && authErr != nil && authErr.Message != "" {
		return authErr.Message
	}
	return err.Error()
}

func matchRequestScopedErrorAction(
	auth *Auth,
	err error,
	cfg *internalconfig.Config,
) (string, bool) {
	if err == nil {
		return "", false
	}
	rules := extractRequestScopedErrorRules(auth, cfg)
	if len(rules) == 0 {
		return "", false
	}
	statusCode := statusCodeFromError(err)
	body := extractRequestScopedErrorBody(err)
	for _, rule := range rules {
		if rule.Status <= 0 || rule.Status != statusCode ||
			(len(rule.Match) == 0 && len(rule.MatchRegexr) == 0) {
			continue
		}
		matched := false
		for _, substring := range rule.Match {
			if substring != "" && strings.Contains(body, substring) {
				matched = true
				break
			}
		}
		if !matched {
			for _, pattern := range rule.MatchRegexr {
				if pattern == "" {
					continue
				}
				expression, errCompile := regexp.Compile(pattern)
				if errCompile == nil && expression.MatchString(body) {
					matched = true
					break
				}
			}
		}
		if !matched {
			continue
		}
		action := strings.ToLower(strings.TrimSpace(rule.Action))
		switch action {
		case RequestScopedActionStop,
			RequestScopedActionStopAndCooldown,
			RequestScopedActionContinue,
			RequestScopedActionContinueAndCooldown:
			return action, true
		}
	}
	return "", false
}

func (m *Manager) applyRequestScopedErrorRule(
	auth *Auth,
	err error,
	result *Result,
) requestScopedActionDecision {
	decision := requestScopedActionDecision{err: err}
	if err == nil || result == nil || result.Error == nil {
		return decision
	}
	action, matched := matchRequestScopedErrorAction(auth, err, m.runtimeConfigSnapshot())
	if !matched {
		return decision
	}

	disposition := result.Disposition
	forceCooldown := action == RequestScopedActionStopAndCooldown ||
		action == RequestScopedActionContinueAndCooldown
	continueFallback := action == RequestScopedActionContinue ||
		action == RequestScopedActionContinueAndCooldown
	if forceCooldown {
		disposition = forceCooldownDisposition(auth, result.Model, disposition)
	} else {
		disposition.EffectScope = cliproxyexecutor.StateEffectScopeRequest
		disposition.EffectKey = ""
	}
	if continueFallback && requestScopedContinueIsSafe(disposition) {
		switch disposition.RetryDirective {
		case cliproxyexecutor.RetryDirectiveNextAuth,
			cliproxyexecutor.RetryDirectiveNextProvider,
			cliproxyexecutor.RetryDirectiveRetryAfter:
		default:
			disposition.RetryDirective = cliproxyexecutor.RetryDirectiveNextAuth
		}
	} else {
		disposition.RetryDirective = cliproxyexecutor.RetryDirectiveNoRetry
	}

	result.Disposition = disposition
	result.StateGeneration = stateGenerationForDisposition(auth, result.Model, disposition)
	result.Error = errorForResultWithDisposition(err, disposition, result.StateGeneration)
	if forceCooldown && result.Error != nil {
		result.Error.Code = ErrorCodeForceCooldown
	}
	adaptedErr := &requestRuleDispositionError{error: err, disposition: disposition}
	decision = requestScopedActionDecision{
		action:  action,
		matched: true,
		stop: action == RequestScopedActionStop ||
			action == RequestScopedActionStopAndCooldown,
		err: adaptedErr,
	}
	return decision
}

func requestScopedContinueIsSafe(disposition cliproxyexecutor.ExecutionDisposition) bool {
	return disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseDownstreamCommitted &&
		disposition.Outcome == cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing
}

func forceCooldownDisposition(
	auth *Auth,
	model string,
	disposition cliproxyexecutor.ExecutionDisposition,
) cliproxyexecutor.ExecutionDisposition {
	switch disposition.EffectScope {
	case cliproxyexecutor.StateEffectScopeExactModel,
		cliproxyexecutor.StateEffectScopeQuotaFamily,
		cliproxyexecutor.StateEffectScopeCredential:
	default:
		if strings.TrimSpace(model) != "" {
			disposition.EffectScope = cliproxyexecutor.StateEffectScopeExactModel
		} else {
			disposition.EffectScope = cliproxyexecutor.StateEffectScopeCredential
		}
		disposition.EffectKey = ""
	}
	switch disposition.EffectScope {
	case cliproxyexecutor.StateEffectScopeExactModel:
		if strings.TrimSpace(disposition.EffectKey) == "" {
			disposition.EffectKey = canonicalModelKey(model)
		}
	case cliproxyexecutor.StateEffectScopeQuotaFamily:
		if strings.TrimSpace(disposition.EffectKey) == "" {
			disposition.EffectKey = quotaFamilyKey(auth, model)
		}
	case cliproxyexecutor.StateEffectScopeCredential:
		if strings.TrimSpace(disposition.EffectKey) == "" && auth != nil {
			disposition.EffectKey = auth.ID
		}
	}
	return disposition
}

func isForceCooldownResult(result Result) bool {
	return result.Error != nil && result.Error.Code == ErrorCodeForceCooldown
}
