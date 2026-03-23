// Package thinking provides unified thinking configuration processing logic.
package thinking

import "net/http"

// errorCode represents the type of thinking configuration error.
type errorCode string

// Error codes for thinking configuration processing.
const (
	// errUnknownLevel indicates the level value is not in the valid list.
	// Example: "model(ultra)" where "ultra" is not a valid level
	errUnknownLevel errorCode = "UNKNOWN_LEVEL"

	// errThinkingNotSupported indicates the model does not support thinking.
	// Example: claude-haiku-4-5 does not have thinking capability
	errThinkingNotSupported errorCode = "THINKING_NOT_SUPPORTED"

	// errLevelNotSupported indicates the model does not support level mode.
	// Example: using level with a budget-only model
	errLevelNotSupported errorCode = "LEVEL_NOT_SUPPORTED"

	// errBudgetOutOfRange indicates the budget value is outside model range.
	// Example: budget 64000 exceeds max 20000
	errBudgetOutOfRange errorCode = "BUDGET_OUT_OF_RANGE"
)

// Error represents an error that occurred during thinking configuration processing.
//
// This error type provides structured information about the error, including:
//   - Code: A machine-readable error code for programmatic handling
//   - Message: A human-readable description of the error
//   - Model: The model name related to the error (optional)
//   - Details: Additional context information (optional)
type Error struct {
	// Code is the machine-readable error code
	Code errorCode
	// Message is the human-readable error description.
	// Should be lowercase, no trailing period, with context if applicable.
	Message string
	// Model is the model name related to this error (optional)
	Model string
	// Details contains additional context information (optional)
	Details map[string]any
}

// Error implements the error interface.
// Returns the message directly without code prefix.
// Use Code field for programmatic error handling.
func (e *Error) Error() string {
	return e.Message
}

// newError creates a new Error with the given code and message.
func newError(code errorCode, message string) *Error {
	return &Error{
		Code:    code,
		Message: message,
	}
}

// newErrorWithModel creates a new Error with model context.
func newErrorWithModel(code errorCode, message, model string) *Error {
	return &Error{
		Code:    code,
		Message: message,
		Model:   model,
	}
}

// StatusCode implements a portable status code interface for HTTP handlers.
func (e *Error) StatusCode() int {
	return http.StatusBadRequest
}
