package executor

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func (e statusErr) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	phase := e.dispatchPhase
	if phase == "" {
		phase = cliproxyexecutor.DispatchPhaseMaybeSent
	}
	disposition := cliproxyexecutor.HTTPStatusDisposition(e.code, e.retryAfter != nil)
	disposition.DispatchPhase = phase
	if e.requestScoped {
		disposition.Outcome = cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing
		disposition.RetryDirective = cliproxyexecutor.RetryDirectiveNoRetry
		disposition.EffectScope = cliproxyexecutor.StateEffectScopeRequest
	}
	return disposition.Normalize(phase)
}

func (e statusErr) withUpstreamResponse(headers http.Header) statusErr {
	e.dispatchPhase = cliproxyexecutor.DispatchPhaseResponseStarted
	if e.retryAfter == nil {
		e.retryAfter = parseRetryAfterHeader(headers, time.Now())
	}
	return e
}

func newUpstreamStatusErr(statusCode int, body []byte, headers http.Header) statusErr {
	return statusErr{code: statusCode, msg: string(body)}.withUpstreamResponse(headers)
}

func newUpstreamStatusErrMessage(statusCode int, message string, headers http.Header) statusErr {
	return statusErr{code: statusCode, msg: message}.withUpstreamResponse(headers)
}

type responseStartedError struct {
	cause error
}

func (e responseStartedError) Error() string {
	if e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e responseStartedError) Unwrap() error {
	return e.cause
}

func (e responseStartedError) StatusCode() int {
	type statusCoder interface {
		StatusCode() int
	}
	var status statusCoder
	if errors.As(e.cause, &status) && status != nil {
		return status.StatusCode()
	}
	return 0
}

func (e responseStartedError) RetryAfter() *time.Duration {
	type retryAfterProvider interface {
		RetryAfter() *time.Duration
	}
	var retryAfter retryAfterProvider
	if errors.As(e.cause, &retryAfter) && retryAfter != nil {
		return retryAfter.RetryAfter()
	}
	return nil
}

func (e responseStartedError) Headers() http.Header {
	type headerProvider interface {
		Headers() http.Header
	}
	var headers headerProvider
	if errors.As(e.cause, &headers) && headers != nil {
		return headers.Headers().Clone()
	}
	return nil
}

func (e responseStartedError) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	var disposition cliproxyexecutor.ExecutionDisposition
	var dispositionErr cliproxyexecutor.ExecutionDispositionError
	if errors.As(e.cause, &dispositionErr) && dispositionErr != nil {
		disposition = dispositionErr.ExecutionDisposition()
	} else {
		disposition = cliproxyexecutor.HTTPStatusDisposition(e.StatusCode(), false)
	}
	disposition.DispatchPhase = cliproxyexecutor.DispatchPhaseResponseStarted
	return disposition.Normalize(cliproxyexecutor.DispatchPhaseResponseStarted)
}

func markResponseStarted(err error) error {
	if err == nil {
		return nil
	}
	return responseStartedError{cause: err}
}

func parseRetryAfterHeader(headers http.Header, now time.Time) *time.Duration {
	if len(headers) == 0 {
		return nil
	}
	value := strings.TrimSpace(headers.Get("Retry-After"))
	if value == "" {
		return nil
	}
	if seconds, errSeconds := strconv.ParseInt(value, 10, 64); errSeconds == nil {
		if seconds < 0 {
			return nil
		}
		delay := time.Duration(seconds) * time.Second
		return &delay
	}
	deadline, errTime := http.ParseTime(value)
	if errTime != nil {
		return nil
	}
	delay := deadline.Sub(now)
	if delay <= 0 {
		return nil
	}
	return &delay
}
