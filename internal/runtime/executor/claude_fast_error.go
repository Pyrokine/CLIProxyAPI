package executor

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// claudeFastRequestError marks any Fast request failure as request-scoped so
// it cannot trigger another credential or change credential availability.
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
	return e != nil
}

// claudeFastDirectResponseError preserves an upstream Fast error response for
// direct downstream forwarding without auth retry or response reconstruction.
type claudeFastDirectResponseError struct {
	response *cliproxyexecutor.RequestTerminatedError
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
	return e != nil
}

func (e *claudeFastDirectResponseError) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	return cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
		Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
		EffectScope:    cliproxyexecutor.StateEffectScopeRequest,
	}
}

func wrapClaudeFastRequestError(fastRequest bool, status int, err error) error {
	if err == nil || !fastRequest {
		return err
	}
	return &claudeFastRequestError{cause: err, status: status}
}

func newClaudeFastDirectResponseError(resp *http.Response, body []byte) error {
	if resp == nil {
		return nil
	}
	headers := resp.Header.Clone()
	headers.Del("Content-Encoding")
	headers.Del("Content-Length")
	return &claudeFastDirectResponseError{
		response: &cliproxyexecutor.RequestTerminatedError{
			HTTPStatus: resp.StatusCode,
			Header:     headers,
			Body:       bytes.Clone(body),
		},
	}
}

func claudeRequestIsFast(req *http.Request, body []byte) bool {
	if req == nil {
		return false
	}
	betas := strings.Join(req.Header.Values("Anthropic-Beta"), ",")
	return claudeRequestUsesFastMode(body, claudeRequestedBetas(betas, nil))
}
