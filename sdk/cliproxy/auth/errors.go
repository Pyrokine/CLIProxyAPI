package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// ErrorCodeRequestScoped identifies failures tied to the current request.
const ErrorCodeRequestScoped = "request_scoped"

const requestScopedErrorCode = ErrorCodeRequestScoped

// ErrorCodeConnectionLifecycle marks connection or session lifecycle failures.
const ErrorCodeConnectionLifecycle = "connection_lifecycle"

const connectionLifecycleErrorCode = ErrorCodeConnectionLifecycle

// ErrorCodeForceCooldown marks failures that must apply cooldown state.
const ErrorCodeForceCooldown = "force_cooldown"

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

	cause error
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

// Unwrap preserves the original upstream error for status, body, and retry metadata.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
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

// MarkRequestScoped marks the error as tied to the current request.
func (e *Error) MarkRequestScoped() *Error {
	if e != nil {
		e.Code = ErrorCodeRequestScoped
	}
	return e
}

// NewRequestScopedError creates an error tied to the current request.
func NewRequestScopedError(message string, httpStatus int) *Error {
	return &Error{
		Code:       ErrorCodeRequestScoped,
		Message:    message,
		HTTPStatus: httpStatus,
	}
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

type errorWithCause struct {
	base  *Error
	cause error
}

func (e *errorWithCause) Error() string {
	if e == nil || e.base == nil {
		return ""
	}
	baseText := e.base.Error()
	if e.cause == nil {
		return baseText
	}
	summary := logging.SafeDiagnosticForLog(e.cause.Error())
	if summary == "" || strings.Contains(baseText, summary) {
		return baseText
	}
	return fmt.Sprintf("%s (last upstream error: %s)", baseText, summary)
}

func (e *errorWithCause) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *errorWithCause) As(target any) bool {
	if e == nil {
		return false
	}
	if authError, ok := target.(**Error); ok {
		*authError = e.base
		return true
	}
	return false
}

func (e *errorWithCause) Is(target error) bool {
	if e == nil {
		return target == nil
	}
	if target == e || target == e.base {
		return true
	}
	if other, ok := target.(*errorWithCause); ok && other != nil {
		return e.base == other.base
	}
	if other, ok := target.(*Error); ok && other != nil {
		return e.base == other
	}
	return false
}

func (e *errorWithCause) StatusCode() int {
	if e == nil {
		return 0
	}
	if e.base != nil {
		if status := e.base.StatusCode(); status != 0 {
			return status
		}
	}
	var statusError interface{ StatusCode() int }
	if errors.As(e.cause, &statusError) {
		return statusError.StatusCode()
	}
	return 0
}

func (e *errorWithCause) IsRequestScoped() bool {
	return e != nil && e.base != nil && e.base.IsRequestScoped()
}

func (e *errorWithCause) MarkRequestScoped() *Error {
	if e == nil || e.base == nil {
		return nil
	}
	return e.base.MarkRequestScoped()
}

func (e *errorWithCause) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	if e == nil || e.base == nil {
		return cliproxyexecutor.ExecutionDisposition{}
	}
	return e.base.ExecutionDisposition()
}

func (e *errorWithCause) MarshalJSON() ([]byte, error) {
	if e == nil || e.base == nil {
		return []byte("null"), nil
	}
	return json.Marshal(e.base)
}

// WithCause wraps an auth error with the upstream cause used to exhaust selection.
func WithCause(err *Error, cause error) error {
	if err == nil {
		return nil
	}
	if cause == nil || cause == err {
		return err
	}
	return &errorWithCause{base: err, cause: cause}
}
