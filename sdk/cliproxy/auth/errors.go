package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/logging"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// ErrorCodeRequestScoped identifies failures tied to the current request.
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
const ErrorCodeRequestScoped = "request_scoped"

const requestScopedErrorCode = ErrorCodeRequestScoped

// ErrorCodeConnectionLifecycle marks connection or session lifecycle failures.
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
const ErrorCodeConnectionLifecycle = "connection_lifecycle"

const connectionLifecycleErrorCode = ErrorCodeConnectionLifecycle

// ErrorCodeTransientTransport 标识传输失败，重试仍按实际发送阶段和执行结果判断
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
const ErrorCodeTransientTransport = "transient_transport"

const transientTransportErrorCode = ErrorCodeTransientTransport

// ErrorCodeForceCooldown marks failures that must apply cooldown state.
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
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
}

type errorState struct {
	DispatchPhase   cliproxyexecutor.DispatchPhase    `json:"dispatch_phase,omitempty"`
	Outcome         cliproxyexecutor.ExecutionOutcome `json:"outcome,omitempty"`
	RetryDirective  cliproxyexecutor.RetryDirective   `json:"retry_directive,omitempty"`
	EffectScope     cliproxyexecutor.StateEffectScope `json:"effect_scope,omitempty"`
	EffectKey       string                            `json:"effect_key,omitempty"`
	StateGeneration uint64                            `json:"state_generation,omitempty"`
	cause           error
}

var errorStates sync.Map

// executionState 的弱引用索引保留公开四字段布局，错误回收后删除其附加状态
func (e *Error) executionState() *errorState {
	key := weak.Make(e)
	if stored, ok := errorStates.Load(key); ok {
		runtime.KeepAlive(e)
		return stored.(*errorState)
	}
	state := &errorState{}
	stored, loaded := errorStates.LoadOrStore(key, state)
	if !loaded {
		runtime.AddCleanup(e, func(key weak.Pointer[Error]) { errorStates.Delete(key) }, key)
	}
	runtime.KeepAlive(e)
	return stored.(*errorState)
}

func errorWithState(base *Error, state errorState) *Error {
	*base.executionState() = state
	return base
}

// WithExecutionDisposition 设置当前错误的执行边界、作用范围和状态代际
func (e *Error) WithExecutionDisposition(
	disposition cliproxyexecutor.ExecutionDisposition,
	generation uint64,
) *Error {
	if e == nil {
		return nil
	}
	state := e.executionState()
	state.DispatchPhase, state.Outcome, state.RetryDirective = disposition.DispatchPhase,
		disposition.Outcome, disposition.RetryDirective
	state.EffectScope, state.EffectKey, state.StateGeneration = disposition.EffectScope,
		disposition.EffectKey, generation
	return e
}

// Clone 复制公开字段、执行状态和原错误原因
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *Error) Clone() *Error {
	return cloneError(e)
}

// ExecutionStateGeneration 返回此错误绑定的状态代际
func (e *Error) ExecutionStateGeneration() uint64 {
	if e == nil {
		return 0
	}
	return e.executionState().StateGeneration
}

func (e *Error) MarshalJSON() ([]byte, error) {
	if e == nil {
		return []byte("null"), nil
	}
	type baseError Error
	return json.Marshal(
		struct {
			*baseError
			*errorState
		}{(*baseError)(e), e.executionState()},
	)
}

func (e *Error) UnmarshalJSON(data []byte) error {
	type baseError Error
	value := struct {
		baseError
		errorState
	}{baseError(*e), *e.executionState()}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*e = Error(value.baseError)
	*e.executionState() = value.errorState
	return nil
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
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.executionState().cause
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
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
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

// ExplicitExecutionDisposition 返回调用方已设置的执行语义
func (e *Error) ExplicitExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	if e == nil {
		return cliproxyexecutor.ExecutionDisposition{}
	}
	state := e.executionState()
	return cliproxyexecutor.ExecutionDisposition{
		DispatchPhase: state.DispatchPhase, Outcome: state.Outcome, RetryDirective: state.RetryDirective,
		EffectScope: state.EffectScope, EffectKey: state.EffectKey,
	}
}

// ExecutionDisposition exposes the persisted replay and state-effect semantics.
func (e *Error) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	if e == nil {
		return cliproxyexecutor.ExecutionDisposition{}
	}
	disposition := e.ExplicitExecutionDisposition()
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
	summary := logging.SafeErrorDiagnostic(e.cause)
	if summary == "" || strings.Contains(baseText, summary) {
		return baseText
	}
	return fmt.Sprintf("%s (last upstream error: %s)", baseText, summary)
}

//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *errorWithCause) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

//goland:noinspection GoUnnecessarilyExportedIdentifiers
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

//goland:noinspection GoTypeAssertionOnErrors,GoUnnecessarilyExportedIdentifiers
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

//goland:noinspection GoUnnecessarilyExportedIdentifiers
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

type terminalAuthError struct {
	*errorWithCause
}

//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *terminalAuthError) IsTerminalAuth() bool {
	return e != nil
}

// IsTerminalAuthError 识别错误链中需要重新认证的上游凭据失败
func IsTerminalAuthError(err error) bool {
	var terminal interface{ IsTerminalAuth() bool }
	return errors.As(err, &terminal) && terminal != nil && terminal.IsTerminalAuth()
}

// NewTerminalAuthError 保留认证错误及其原因，并标记上游凭据需要重新认证
func NewTerminalAuthError(err *Error, cause error) error {
	if err == nil {
		return nil
	}
	return &terminalAuthError{errorWithCause: &errorWithCause{base: err, cause: cause}}
}

type authUnavailableError struct {
	*errorWithCause
	retryAfter time.Duration
}

func newAuthUnavailableErrorWithCause(next, now time.Time, cause error) error {
	base := &Error{Code: "auth_unavailable", Message: "no auth available"}
	if !next.After(now) {
		return WithCause(base, cause)
	}
	base.HTTPStatus = http.StatusServiceUnavailable
	base.Retryable = true
	return &authUnavailableError{
		errorWithCause: &errorWithCause{base: base, cause: cause},
		retryAfter:     next.Sub(now),
	}
}

//goland:noinspection GoTypeAssertionOnErrors,GoUnnecessarilyExportedIdentifiers
func (e *authUnavailableError) Is(target error) bool {
	if e == nil {
		return target == nil
	}
	if other, ok := target.(*authUnavailableError); ok && other != nil {
		return e.errorWithCause != nil && other.errorWithCause != nil && e.base == other.base
	}
	return e.errorWithCause.Is(target)
}

//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *authUnavailableError) RetryAfter() *time.Duration {
	if e == nil || e.retryAfter <= 0 {
		return nil
	}
	value := e.retryAfter
	return &value
}

// WithAuthError 更新错误描述，并保留本地恢复提示和原上游原因
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *authUnavailableError) WithAuthError(base *Error) error {
	if e == nil {
		return base
	}
	return &authUnavailableError{
		errorWithCause: &errorWithCause{base: base, cause: e.cause},
		retryAfter:     e.retryAfter,
	}
}

// Headers 返回本地计算的恢复提示
//
//goland:noinspection GoUnnecessarilyExportedIdentifiers
func (e *authUnavailableError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return safeRetryAfterHeader(e.retryAfter)
}

// WithCause wraps an auth error with the upstream cause used to exhaust selection.
// noinspection GoDirectComparisonOfErrors
func WithCause(err *Error, cause error) error {
	if err == nil {
		return nil
	}
	if cause == nil || cause == err {
		return err
	}
	return &errorWithCause{base: err, cause: cause}
}
