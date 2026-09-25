// Package clienterror classifies upstream failures caused by the client request.
package clienterror

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

var requestFaultCodes = map[string]struct{}{
	"cyber_policy":                {},
	"context_length_exceeded":     {},
	"message_too_big":             {},
	"string_above_max_length":     {},
	"invalid_prompt":              {},
	"invalid_value":               {},
	"unsupported_value":           {},
	"invalid_request_error":       {},
	"previous_response_not_found": {},
}

var requestFaultTypes = map[string]struct{}{
	"invalid_request":       {},
	"invalid_request_error": {},
	"bad_request_error":     {},
	"invalid_prompt":        {},
}

// StatusClientClosedRequest 是客户端在代理完成前中止请求时使用的 nginx 风格状态码。
const StatusClientClosedRequest = 499

// HTTPStatusFromError 从错误中提取 HTTP 状态，未知时返回 0。
func HTTPStatusFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var statusErr statusCoder
	if errors.As(err, &statusErr) && statusErr != nil {
		if status := statusErr.StatusCode(); status > 0 {
			return status
		}
	}
	if errors.Is(err, context.Canceled) {
		return StatusClientClosedRequest
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	return 0
}

// HTTPStatusFromErrorOr 在错误没有已知状态时返回 fallback。
func HTTPStatusFromErrorOr(err error, fallback int) int {
	if status := HTTPStatusFromError(err); status > 0 {
		return status
	}
	return fallback
}

// ResponseBodyFromError 提取明确标记为直接返回的正文，空正文仍返回 true
func ResponseBodyFromError(err error) ([]byte, bool) {
	var response interface {
		DirectResponse() bool
		ResponseBody() []byte
	}
	if !errors.As(err, &response) || !response.DirectResponse() {
		return nil, false
	}
	return response.ResponseBody(), true
}

// IsRequestFault reports whether an upstream failure is caused by the request
// and therefore must not rotate or penalize credentials.
func IsRequestFault(status int, err error) bool {
	if status <= 0 && err != nil {
		type statusCoder interface {
			StatusCode() int
		}
		var statusErr statusCoder
		if errors.As(err, &statusErr) && statusErr != nil {
			status = statusErr.StatusCode()
		}
	}
	// Payment and rate-limit statuses are authoritative even when an upstream
	// pairs them with a generic invalid_request_error body.
	if status == http.StatusPaymentRequired || status == http.StatusTooManyRequests {
		return false
	}
	// Authentication failures remain credential-scoped when the upstream uses
	// the generic invalid_request_error code alongside authentication_error.
	if status == http.StatusUnauthorized && hasAuthenticationErrorBody(err) {
		return false
	}
	// Model-not-found responses describe credential/model capability rather than
	// a caller request fault, so routing may try another credential.
	if hasModelNotFoundErrorBody(err) {
		return false
	}
	if hasRequestFaultBody(err) {
		return true
	}
	switch status {
	case http.StatusBadRequest,
		http.StatusConflict,
		http.StatusRequestEntityTooLarge,
		http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func hasModelNotFoundErrorBody(err error) bool {
	if err == nil {
		return false
	}
	body := strings.TrimSpace(err.Error())
	if body == "" {
		return false
	}
	if code, _, found := strings.Cut(strings.ToLower(body), ":"); found &&
		(code == "model_not_found" || code == "model_not_found_error") {
		return true
	}
	if !json.Valid([]byte(body)) {
		return false
	}
	for _, path := range []string{"error.code", "code", "response.error.code", "body.error.code"} {
		code := strings.ToLower(strings.TrimSpace(gjson.Get(body, path).String()))
		if code == "model_not_found" || code == "model_not_found_error" {
			return true
		}
	}
	return false
}

func hasAuthenticationErrorBody(err error) bool {
	if err == nil {
		return false
	}
	body := strings.TrimSpace(err.Error())
	if body == "" || !json.Valid([]byte(body)) {
		return false
	}
	for _, path := range []string{"error.type", "type", "response.error.type", "body.error.type"} {
		if errType := strings.ToLower(strings.TrimSpace(gjson.Get(body, path).String())); errType == "authentication_error" {
			return true
		}
	}
	return false
}

func hasRequestFaultBody(err error) bool {
	if err == nil {
		return false
	}
	body := strings.TrimSpace(err.Error())
	if body == "" || !json.Valid([]byte(body)) {
		return false
	}
	for _, path := range []string{"error.code", "code", "response.error.code", "body.error.code"} {
		code := strings.ToLower(strings.TrimSpace(gjson.Get(body, path).String()))
		if _, ok := requestFaultCodes[code]; ok {
			return true
		}
	}
	for _, path := range []string{"error.type", "type", "response.error.type", "body.error.type"} {
		errType := strings.ToLower(strings.TrimSpace(gjson.Get(body, path).String()))
		if _, ok := requestFaultTypes[errType]; ok {
			return true
		}
	}
	return false
}
