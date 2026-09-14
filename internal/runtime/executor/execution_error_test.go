package executor

import (
	"errors"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type executionErrorTestCause struct {
	retryAfter time.Duration
	headers    http.Header
}

func (e *executionErrorTestCause) Error() string {
	return "response read failed"
}

func (e *executionErrorTestCause) StatusCode() int {
	return http.StatusTooManyRequests
}

func (e *executionErrorTestCause) RetryAfter() *time.Duration {
	retryAfter := e.retryAfter
	return &retryAfter
}

func (e *executionErrorTestCause) Headers() http.Header {
	return e.headers
}

func (e *executionErrorTestCause) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	return cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  cliproxyexecutor.DispatchPhaseMaybeSent,
		Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: cliproxyexecutor.RetryDirectiveRetryAfter,
		EffectScope:    cliproxyexecutor.StateEffectScopeQuotaFamily,
		EffectKey:      "shared-family",
	}
}

func TestParseRetryAfterHeader(t *testing.T) {
	now := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		value   string
		want    time.Duration
		wantNil bool
	}{
		{name: "delta seconds", value: "120", want: 2 * time.Minute},
		{name: "http date", value: now.Add(90 * time.Second).Format(http.TimeFormat), want: 90 * time.Second},
		{name: "negative", value: "-1", wantNil: true},
		{name: "past date", value: now.Add(-time.Second).Format(http.TimeFormat), wantNil: true},
		{name: "invalid", value: "tomorrow", wantNil: true},
		{name: "empty", value: "", wantNil: true},
	}

	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				headers := http.Header{}
				if tc.value != "" {
					headers.Set("Retry-After", tc.value)
				}
				got := parseRetryAfterHeader(headers, now)
				if tc.wantNil {
					if got != nil {
						t.Fatalf("parseRetryAfterHeader() = %v, want nil", *got)
					}
					return
				}
				if got == nil || *got != tc.want {
					t.Fatalf("parseRetryAfterHeader() = %v, want %v", got, tc.want)
				}
			},
		)
	}
}

func TestResponseStartedErrorPreservesTypedSemantics(t *testing.T) {
	cause := &executionErrorTestCause{
		retryAfter: 3 * time.Second,
		headers:    http.Header{"Retry-After": []string{"3"}},
	}
	wrapped := markResponseStarted(cause)
	if !errors.Is(wrapped, cause) {
		t.Fatalf("wrapped error does not preserve cause: %v", wrapped)
	}

	statusProvider, ok := wrapped.(interface{ StatusCode() int })
	if !ok || statusProvider.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status provider = %#v", statusProvider)
	}
	retryProvider, ok := wrapped.(interface{ RetryAfter() *time.Duration })
	if !ok || retryProvider.RetryAfter() == nil || *retryProvider.RetryAfter() != 3*time.Second {
		t.Fatalf("retry provider = %#v", retryProvider)
	}
	headerProvider, ok := wrapped.(interface{ Headers() http.Header })
	if !ok {
		t.Fatalf("wrapped error has no headers: %T", wrapped)
	}
	headers := headerProvider.Headers()
	headers.Set("Retry-After", "changed")
	if got := headerProvider.Headers().Get("Retry-After"); got != "3" {
		t.Fatalf("headers were not cloned, got %q", got)
	}

	// The wrapper itself must expose the protocol; errors.As would also match an unwrapped cause.
	// noinspection GoTypeAssertionOnErrors
	dispositionProvider, ok := wrapped.(cliproxyexecutor.ExecutionDispositionError)
	if !ok {
		t.Fatalf("wrapped error has no execution disposition: %T", wrapped)
	}
	disposition := dispositionProvider.ExecutionDisposition()
	if disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseResponseStarted ||
		disposition.Outcome != cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing ||
		disposition.RetryDirective != cliproxyexecutor.RetryDirectiveRetryAfter ||
		disposition.EffectScope != cliproxyexecutor.StateEffectScopeQuotaFamily ||
		disposition.EffectKey != "shared-family" {
		t.Fatalf("execution disposition = %+v", disposition)
	}
}

func TestUpstreamStatusErrorUsesResponseHeaders(t *testing.T) {
	errStatus := newUpstreamStatusErr(
		http.StatusTooManyRequests,
		[]byte("rate limited"),
		http.Header{"Retry-After": []string{"2"}},
	)
	if errStatus.RetryAfter() == nil || *errStatus.RetryAfter() != 2*time.Second {
		t.Fatalf("retry after = %v", errStatus.RetryAfter())
	}
	disposition := errStatus.ExecutionDisposition()
	if disposition.DispatchPhase != cliproxyexecutor.DispatchPhaseResponseStarted ||
		disposition.Outcome != cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing ||
		disposition.RetryDirective != cliproxyexecutor.RetryDirectiveRetryAfter ||
		disposition.EffectScope != cliproxyexecutor.StateEffectScopeQuotaFamily {
		t.Fatalf("execution disposition = %+v", disposition)
	}

	if markResponseStarted(nil) != nil {
		t.Fatal("markResponseStarted(nil) must return nil")
	}
}
