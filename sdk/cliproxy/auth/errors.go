package auth

import cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"

const requestScopedErrorCode = "request_scoped"

// connectionLifecycleErrorCode 标记不应触发凭据冷却的连接或会话生命周期错误。
const connectionLifecycleErrorCode = "connection_lifecycle"

// Error describes an authentication related failure in a provider-agnostic format.
type Error struct {
	// Code is a short machine-readable identifier.
	Code string `json:"code,omitempty"`
	// Message is a human-readable description of the failure.
	Message string `json:"message"`
	// Retryable indicates whether a retry might fix the issue automatically.
	Retryable bool `json:"retryable"`
	// HTTPStatus optionally records an HTTP-like status code for the error.
	HTTPStatus int `json:"http_status,omitempty"`
	// DispatchPhase identifies the furthest known request boundary.
	DispatchPhase cliproxyexecutor.DispatchPhase `json:"dispatch_phase,omitempty"`
	// Outcome identifies the known upstream processing result.
	Outcome cliproxyexecutor.ExecutionOutcome `json:"outcome,omitempty"`
	// RetryDirective records the safe retry transition.
	RetryDirective cliproxyexecutor.RetryDirective `json:"retry_directive,omitempty"`
	// EffectScope identifies the narrowest state affected by the failure.
	EffectScope cliproxyexecutor.StateEffectScope `json:"effect_scope,omitempty"`
	// EffectKey identifies the exact model, quota family, group, or provider.
	EffectKey string `json:"effect_key,omitempty"`
	// StateGeneration binds this error to the state version it created.
	StateGeneration uint64 `json:"state_generation,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// StatusCode implements optional status accessor for manager decision-making.
func (e *Error) StatusCode() int {
	if e == nil {
		return 0
	}
	return e.HTTPStatus
}

// IsRequestScoped reports whether the failure is tied to the current request
// rather than the selected credential.
func (e *Error) IsRequestScoped() bool {
	return e != nil && e.Code == requestScopedErrorCode
}

// ExecutionDisposition exposes the persisted replay and state-effect semantics.
func (e *Error) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	if e == nil {
		return cliproxyexecutor.ExecutionDisposition{}
	}
	disposition := cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  e.DispatchPhase,
		Outcome:        e.Outcome,
		RetryDirective: e.RetryDirective,
		EffectScope:    e.EffectScope,
		EffectKey:      e.EffectKey,
	}
	if disposition.Outcome == "" && e.HTTPStatus != 0 {
		fallback := cliproxyexecutor.HTTPStatusDisposition(e.HTTPStatus, false)
		if disposition.DispatchPhase != "" {
			fallback.DispatchPhase = disposition.DispatchPhase
		}
		if disposition.RetryDirective != "" {
			fallback.RetryDirective = disposition.RetryDirective
		}
		if disposition.EffectScope != "" {
			fallback.EffectScope = disposition.EffectScope
		}
		if disposition.EffectKey != "" {
			fallback.EffectKey = disposition.EffectKey
		}
		return fallback
	}
	return disposition
}
