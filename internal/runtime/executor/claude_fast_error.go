package executor

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// claudeFastRequestError keeps Fast-only local and entitlement failures tied to
// the request while preserving genuine Claude rate-limit scope and timing.
type claudeFastRequestError struct {
	cause  error
	status int
}

func (e *claudeFastRequestError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *claudeFastRequestError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *claudeFastRequestError) StatusCode() int {
	if e == nil || (e.status >= http.StatusOK && e.status < http.StatusMultipleChoices) {
		return 0
	}
	return e.status
}

func (e *claudeFastRequestError) IsRequestScoped() bool {
	if e == nil {
		return false
	}
	return claudeRateLimitErrorFrom(e.cause) == nil
}

func (e *claudeFastRequestError) IsCredentialScoped() bool {
	rateLimit := claudeRateLimitErrorFrom(e.cause)
	return rateLimit != nil && rateLimit.IsCredentialScoped()
}

func (e *claudeFastRequestError) RetryAfter() *time.Duration {
	if e == nil {
		return nil
	}
	var retryAfter interface{ RetryAfter() *time.Duration }
	if errors.As(e.cause, &retryAfter) && retryAfter != nil {
		return retryAfter.RetryAfter()
	}
	return nil
}

// claudeFastDirectResponseError preserves the upstream response for direct
// forwarding while exposing genuine 429 state scope to the auth manager.
type claudeFastDirectResponseError struct {
	response  *cliproxyexecutor.RequestTerminatedError
	rateLimit *claudeRateLimitError
}

func (e *claudeFastDirectResponseError) Error() string {
	if e == nil || e.response == nil {
		return ""
	}
	return fmt.Sprintf("claude Fast upstream request failed with status %d", e.response.HTTPStatus)
}

func (e *claudeFastDirectResponseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.response
}

func (e *claudeFastDirectResponseError) IsRequestScoped() bool {
	return e != nil && e.rateLimit == nil
}

func (e *claudeFastDirectResponseError) IsCredentialScoped() bool {
	return e != nil && e.rateLimit != nil && e.rateLimit.IsCredentialScoped()
}

func (e *claudeFastDirectResponseError) RetryAfter() *time.Duration {
	if e == nil || e.rateLimit == nil {
		return nil
	}
	return e.rateLimit.RetryAfter()
}

func (e *claudeFastDirectResponseError) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	if e != nil && e.rateLimit != nil {
		return e.rateLimit.ExecutionDisposition()
	}
	return cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
		Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
		EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
	}
}

func claudeRateLimitErrorFrom(err error) *claudeRateLimitError {
	var rateLimit *claudeRateLimitError
	if errors.As(err, &rateLimit) {
		return rateLimit
	}
	return nil
}

func wrapClaudeFastRequestError(fastRequest bool, status int, err error) error {
	if err == nil || !fastRequest {
		return err
	}
	return &claudeFastRequestError{cause: err, status: status}
}

func newClaudeFastDirectResponseError(resp *http.Response, body []byte, modelLevelCooling bool) error {
	if resp == nil {
		return nil
	}
	headers := resp.Header.Clone()
	headers.Del("Content-Encoding")
	headers.Del("Content-Length")

	var rateLimit *claudeRateLimitError
	if resp.StatusCode == http.StatusTooManyRequests {
		rateLimit = claudeRateLimitErrorFrom(classifyClaudeUpstreamError(resp.StatusCode, body, modelLevelCooling, resp.Header))
	}
	return &claudeFastDirectResponseError{
		response: &cliproxyexecutor.RequestTerminatedError{
			HTTPStatus: resp.StatusCode,
			Header:     headers,
			Body:       bytes.Clone(body),
		},
		rateLimit: rateLimit,
	}
}

func claudeRequestIsFast(req *http.Request, body []byte) bool {
	if req == nil {
		return false
	}
	betas := strings.Join(req.Header.Values("Anthropic-Beta"), ",")
	return claudeRequestUsesFastMode(body, claudeRequestedBetas(betas, nil))
}
