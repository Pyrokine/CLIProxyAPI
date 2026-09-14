package executor

import (
	"net/http"
	"testing"
)

func TestExecutionDispositionNormalizePreservesFurthestDispatchPhase(t *testing.T) {
	disposition := ExecutionDisposition{
		DispatchPhase: DispatchPhaseResponseStarted,
	}.Normalize(DispatchPhaseMaybeSent)
	if disposition.DispatchPhase != DispatchPhaseResponseStarted {
		t.Fatalf("dispatch phase = %q, want %q", disposition.DispatchPhase, DispatchPhaseResponseStarted)
	}
	if disposition.Outcome != ExecutionOutcomeUnknown ||
		disposition.RetryDirective != RetryDirectiveNoRetry ||
		disposition.EffectScope != StateEffectScopeRequest {
		t.Fatalf("normalized disposition = %+v", disposition)
	}

	disposition = ExecutionDisposition{
		DispatchPhase: DispatchPhaseMaybeSent,
	}.Normalize(DispatchPhaseDownstreamCommitted)
	if disposition.DispatchPhase != DispatchPhaseDownstreamCommitted {
		t.Fatalf("dispatch phase = %q, want %q", disposition.DispatchPhase, DispatchPhaseDownstreamCommitted)
	}
}

func TestHTTPStatusDisposition(t *testing.T) {
	cases := []struct {
		name            string
		status          int
		hasRetryAfter   bool
		wantOutcome     ExecutionOutcome
		wantRetry       RetryDirective
		wantEffectScope StateEffectScope
	}{
		{
			name: "bad request", status: http.StatusBadRequest,
			wantOutcome: ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   RetryDirectiveNoRetry, wantEffectScope: StateEffectScopeRequest,
		},
		{
			name: "unauthorized", status: http.StatusUnauthorized,
			wantOutcome: ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   RetryDirectiveSameAuthAfterRefresh, wantEffectScope: StateEffectScopeCredential,
		},
		{
			name: "forbidden", status: http.StatusForbidden,
			wantOutcome: ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   RetryDirectiveNextAuth, wantEffectScope: StateEffectScopeCredential,
		},
		{
			name: "model not found", status: http.StatusNotFound,
			wantOutcome: ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   RetryDirectiveNextAuth, wantEffectScope: StateEffectScopeExactModel,
		},
		{
			name: "conflict", status: http.StatusConflict,
			wantOutcome: ExecutionOutcomeFailed,
			wantRetry:   RetryDirectiveNoRetry, wantEffectScope: StateEffectScopeRequest,
		},
		{
			name: "too early", status: http.StatusTooEarly,
			wantOutcome: ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   RetryDirectiveRetryAfter, wantEffectScope: StateEffectScopeQuotaFamily,
		},
		{
			name: "rate limited", status: http.StatusTooManyRequests,
			wantOutcome: ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   RetryDirectiveRetryAfter, wantEffectScope: StateEffectScopeQuotaFamily,
		},
		{
			name: "service unavailable with retry after", status: http.StatusServiceUnavailable,
			hasRetryAfter: true, wantOutcome: ExecutionOutcomeUnknown,
			wantRetry: RetryDirectiveNoRetry, wantEffectScope: StateEffectScopeProvider,
		},
	}

	for _, tc := range cases {
		t.Run(
			tc.name, func(t *testing.T) {
				disposition := HTTPStatusDisposition(tc.status, tc.hasRetryAfter)
				if disposition.DispatchPhase != DispatchPhaseResponseStarted ||
					disposition.Outcome != tc.wantOutcome ||
					disposition.RetryDirective != tc.wantRetry ||
					disposition.EffectScope != tc.wantEffectScope {
					t.Fatalf("HTTPStatusDisposition(%d, %t) = %+v", tc.status, tc.hasRetryAfter, disposition)
				}
			},
		)
	}
}
