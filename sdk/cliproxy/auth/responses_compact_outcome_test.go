package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type compactOutcomeError struct {
	status  int
	message string
}

func (e *compactOutcomeError) Error() string   { return e.message }
func (e *compactOutcomeError) StatusCode() int { return e.status }

type compactOutcomeDispositionError struct {
	*compactOutcomeError
	disposition cliproxyexecutor.ExecutionDisposition
}

func (e *compactOutcomeDispositionError) ExecutionDisposition() cliproxyexecutor.ExecutionDisposition {
	return e.disposition
}

func TestResponsesCompactErrorDispositionMatrix(t *testing.T) {
	auth := &Auth{ID: "compact-auth", Provider: "compact-provider", Status: StatusActive}
	model := "compact-model"
	credentialDisposition := cliproxyexecutor.ExecutionDisposition{
		DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
		Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		RetryDirective: cliproxyexecutor.RetryDirectiveNextAuth,
		EffectScope:    cliproxyexecutor.StateEffectScopeCredential,
	}
	credential500 := &compactOutcomeDispositionError{
		compactOutcomeError: &compactOutcomeError{status: http.StatusInternalServerError, message: "credential failure"},
		disposition:         credentialDisposition,
	}
	credential404 := &compactOutcomeDispositionError{
		compactOutcomeError: &compactOutcomeError{status: http.StatusNotFound, message: "credential missing"},
		disposition:         credentialDisposition,
	}
	tests := []struct {
		name        string
		err         error
		wantFault   bool
		wantOutcome cliproxyexecutor.ExecutionOutcome
		wantRetry   cliproxyexecutor.RetryDirective
		wantScope   cliproxyexecutor.StateEffectScope
	}{
		{
			name:        "transient 500 is neutral without unsafe replay",
			err:         &compactOutcomeError{status: http.StatusInternalServerError, message: "temporary failure"},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeUnknown,
			wantRetry:   cliproxyexecutor.RetryDirectiveNoRetry,
			wantScope:   cliproxyexecutor.StateEffectScopeRequest,
		},
		{
			name:        "endpoint 404 is a request fault",
			err:         &compactOutcomeError{status: http.StatusNotFound, message: "endpoint not found"},
			wantFault:   true,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNoRetry,
			wantScope:   cliproxyexecutor.StateEffectScopeRequest,
		},
		{
			name:        "method not allowed is a request fault",
			err:         &compactOutcomeError{status: http.StatusMethodNotAllowed, message: "method not allowed"},
			wantFault:   true,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNoRetry,
			wantScope:   cliproxyexecutor.StateEffectScopeRequest,
		},
		{
			name:        "conflict is a request fault",
			err:         &compactOutcomeError{status: http.StatusConflict, message: "conflict"},
			wantFault:   true,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNoRetry,
			wantScope:   cliproxyexecutor.StateEffectScopeRequest,
		},
		{
			name:        "oversized request is a request fault",
			err:         &compactOutcomeError{status: http.StatusRequestEntityTooLarge, message: "too large"},
			wantFault:   true,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNoRetry,
			wantScope:   cliproxyexecutor.StateEffectScopeRequest,
		},
		{
			name:        "unprocessable request is a request fault",
			err:         &compactOutcomeError{status: http.StatusUnprocessableEntity, message: "unprocessable"},
			wantFault:   true,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNoRetry,
			wantScope:   cliproxyexecutor.StateEffectScopeRequest,
		},
		{
			name:        "not implemented is a request fault",
			err:         &compactOutcomeError{status: http.StatusNotImplemented, message: "compact not supported"},
			wantFault:   true,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNoRetry,
			wantScope:   cliproxyexecutor.StateEffectScopeRequest,
		},
		{
			name:        "model support 400 is compact capability fault",
			err:         &compactOutcomeError{status: http.StatusBadRequest, message: "requested model is not supported"},
			wantFault:   true,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNoRetry,
			wantScope:   cliproxyexecutor.StateEffectScopeRequest,
		},
		{
			name: "structured 401 request fault stays request scoped",
			err: &Error{
				Code:       "custom_upstream_code",
				HTTPStatus: http.StatusUnauthorized,
				Message:    `{"error":{"type":"invalid_request_error","message":"invalid parameter"}}`,
			},
			wantFault:   true,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNoRetry,
			wantScope:   cliproxyexecutor.StateEffectScopeRequest,
		},
		{
			name:        "plain 401 remains credential scoped",
			err:         &compactOutcomeError{status: http.StatusUnauthorized, message: "unauthorized"},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveSameAuthAfterRefresh,
			wantScope:   cliproxyexecutor.StateEffectScopeCredential,
		},
		{
			name:        "402 remains credential scoped",
			err:         &compactOutcomeError{status: http.StatusPaymentRequired, message: "payment required"},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNextAuth,
			wantScope:   cliproxyexecutor.StateEffectScopeCredential,
		},
		{
			name:        "403 remains credential scoped",
			err:         &compactOutcomeError{status: http.StatusForbidden, message: "forbidden"},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNextAuth,
			wantScope:   cliproxyexecutor.StateEffectScopeCredential,
		},
		{
			name:        "425 remains quota family scoped",
			err:         &compactOutcomeError{status: http.StatusTooEarly, message: "retry later"},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveRetryAfter,
			wantScope:   cliproxyexecutor.StateEffectScopeQuotaFamily,
		},
		{
			name:        "429 remains quota family scoped",
			err:         &compactOutcomeError{status: http.StatusTooManyRequests, message: "quota exceeded"},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveRetryAfter,
			wantScope:   cliproxyexecutor.StateEffectScopeQuotaFamily,
		},
		{
			name:        "invalid grant remains credential scoped",
			err:         &compactOutcomeError{status: http.StatusBadRequest, message: "invalid_grant"},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNextAuth,
			wantScope:   cliproxyexecutor.StateEffectScopeCredential,
		},
		{
			name:        "cloudflare remains provider scoped",
			err:         &compactOutcomeError{status: http.StatusBadRequest, message: "cloudflare challenge-platform"},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNextProvider,
			wantScope:   cliproxyexecutor.StateEffectScopeProvider,
		},
		{
			name:        "explicit credential 500 disposition is preserved",
			err:         credential500,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNextAuth,
			wantScope:   cliproxyexecutor.StateEffectScopeCredential,
		},
		{
			name:        "explicit credential 404 disposition is preserved",
			err:         credential404,
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			wantRetry:   cliproxyexecutor.RetryDirectiveNextAuth,
			wantScope:   cliproxyexecutor.StateEffectScopeCredential,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := executionResult(
				auth,
				auth.Provider,
				model,
				test.err,
				cliproxyexecutor.DispatchPhaseMaybeSent,
			)
			fault := applyResponsesCompactErrorDisposition(
				cliproxyexecutor.Options{Alt: "responses/compact"},
				test.err,
				&result,
			)
			if fault != test.wantFault || result.Disposition.Outcome != test.wantOutcome ||
				result.Disposition.RetryDirective != test.wantRetry ||
				result.Disposition.EffectScope != test.wantScope {
				t.Fatalf("compact result = %#v, request fault = %t", result, fault)
			}
			if test.wantScope == cliproxyexecutor.StateEffectScopeRequest &&
				(result.Disposition.EffectKey != "" || result.StateGeneration != 0) {
				t.Fatalf("request-scoped compact result retained state target: %#v", result)
			}
		})
	}

	t.Run("ordinary responses are unchanged", func(t *testing.T) {
		upstream := &compactOutcomeError{status: http.StatusNotFound, message: "endpoint not found"}
		result := executionResult(
			auth,
			auth.Provider,
			model,
			upstream,
			cliproxyexecutor.DispatchPhaseMaybeSent,
		)
		if fault := applyResponsesCompactErrorDisposition(
			cliproxyexecutor.Options{},
			upstream,
			&result,
		); fault || result.Disposition.EffectScope != cliproxyexecutor.StateEffectScopeExactModel ||
			result.Disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNextAuth {
			t.Fatalf("ordinary response disposition = %#v, request fault = %t", result, fault)
		}
	})

	t.Run("forced cooldown is preserved", func(t *testing.T) {
		upstream := &compactOutcomeError{status: http.StatusInternalServerError, message: "forced cooldown"}
		result := executionResult(
			auth,
			auth.Provider,
			model,
			upstream,
			cliproxyexecutor.DispatchPhaseMaybeSent,
		)
		result.Disposition = cliproxyexecutor.ExecutionDisposition{
			DispatchPhase:  cliproxyexecutor.DispatchPhaseResponseStarted,
			Outcome:        cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
			RetryDirective: cliproxyexecutor.RetryDirectiveNoRetry,
			EffectScope:    cliproxyexecutor.StateEffectScopeExactModel,
			EffectKey:      model,
		}
		result.Error = errorForResultWithDisposition(upstream, result.Disposition, result.StateGeneration)
		result.Error.Code = ErrorCodeForceCooldown
		if fault := applyResponsesCompactErrorDisposition(
			cliproxyexecutor.Options{Alt: "responses/compact"},
			upstream,
			&result,
		); fault || result.Disposition.EffectScope != cliproxyexecutor.StateEffectScopeExactModel ||
			result.Error.Code != ErrorCodeForceCooldown {
			t.Fatalf("forced compact cooldown = %#v, request fault = %t", result, fault)
		}
	})
}

func TestManagerResponsesCompactStopsWithoutCooldown(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantOutcome cliproxyexecutor.ExecutionOutcome
	}{
		{
			name:        "transient 500",
			err:         &compactOutcomeError{status: http.StatusInternalServerError, message: "temporary compact failure"},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeUnknown,
		},
		{
			name:        "endpoint 404",
			err:         &compactOutcomeError{status: http.StatusNotFound, message: "compact endpoint not found"},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		},
		{
			name:        "unsupported 501",
			err:         &compactOutcomeError{status: http.StatusNotImplemented, message: "compact unsupported"},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		},
		{
			name: "structured 401",
			err: &Error{
				Code:       "custom_upstream_code",
				HTTPStatus: http.StatusUnauthorized,
				Message:    `{"error":{"type":"invalid_request_error","message":"invalid parameter"}}`,
			},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := fmt.Sprintf("compact-stop-provider-%d", index)
			model := fmt.Sprintf("compact-stop-model-%d", index)
			executor := &requestScopedTestExecutor{provider: provider}
			executor.execute = func(*Auth) (cliproxyexecutor.Response, error) {
				return cliproxyexecutor.Response{}, test.err
			}
			hook := &runtimeResultHook{results: make(chan Result, 2)}
			manager := NewManager(nil, &RoundRobinSelector{}, hook)
			manager.SetRetryConfig(2, 30*time.Second, 1)
			manager.RegisterExecutor(executor)
			for authIndex, priority := range []string{"10", "5"} {
				registerRequestScopedTestAuth(t, manager, &Auth{
					ID:         fmt.Sprintf("compact-stop-auth-%d-%d", index, authIndex),
					Provider:   provider,
					Status:     StatusActive,
					Attributes: map[string]string{"priority": priority},
				}, model)
			}

			policyResults := make(chan Result, 4)
			manager.SetResultPolicy(ResultPolicyFunc(func(_ context.Context, result Result) Result {
				policyResults <- result
				return result
			}))
			_, err := manager.Execute(
				context.Background(),
				[]string{provider},
				cliproxyexecutor.Request{Model: model},
				cliproxyexecutor.Options{Alt: "responses/compact"},
			)
			if err != test.err {
				t.Fatalf("Execute() error = %T/%v, want original", err, err)
			}
			if calls := executor.calls.Load(); calls != 1 {
				t.Fatalf("Execute() calls = %d, want 1", calls)
			}

			var policyResult Result
			select {
			case policyResult = <-policyResults:
			case <-time.After(time.Second):
				t.Fatal("ResultPolicy did not observe compact result")
			}
			if policyResult.Disposition.Outcome != test.wantOutcome ||
				policyResult.Disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry ||
				policyResult.Disposition.EffectScope != cliproxyexecutor.StateEffectScopeRequest {
				t.Fatalf("ResultPolicy compact result = %#v", policyResult)
			}

			select {
			case recorded := <-hook.results:
				if recorded.Disposition.Outcome != test.wantOutcome ||
					recorded.Disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry ||
					recorded.Disposition.EffectScope != cliproxyexecutor.StateEffectScopeRequest {
					t.Fatalf("recorded compact result = %#v", recorded)
				}
			case <-time.After(time.Second):
				t.Fatal("compact result hook was not called")
			}

			failed := int64(0)
			for authIndex := range 2 {
				authID := fmt.Sprintf("compact-stop-auth-%d-%d", index, authIndex)
				auth, ok := manager.GetByID(authID)
				if !ok || auth == nil {
					t.Fatalf("auth %s missing", authID)
				}
				failed += auth.Failed
				if auth.Unavailable || !auth.NextRetryAfter.IsZero() {
					t.Fatalf("compact failure changed credential state: %#v", auth)
				}
				if state := auth.ModelStates[model]; state != nil &&
					(state.Unavailable || !state.NextRetryAfter.IsZero()) {
					t.Fatalf("compact failure changed model state: %#v", state)
				}
			}
			if failed != 1 {
				t.Fatalf("failed count = %d, want 1", failed)
			}
		})
	}
}

func TestManagerResponsesCompactCredentialAndQuotaFailuresMutateState(t *testing.T) {
	previous := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(previous) })

	for index, test := range []struct {
		name      string
		status    int
		wantScope cliproxyexecutor.StateEffectScope
	}{
		{
			name: "unauthorized", status: http.StatusUnauthorized,
			wantScope: cliproxyexecutor.StateEffectScopeCredential,
		},
		{
			name: "payment required", status: http.StatusPaymentRequired,
			wantScope: cliproxyexecutor.StateEffectScopeCredential,
		},
		{
			name: "forbidden", status: http.StatusForbidden,
			wantScope: cliproxyexecutor.StateEffectScopeCredential,
		},
		{
			name: "too early", status: http.StatusTooEarly,
			wantScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
		},
		{
			name: "quota", status: http.StatusTooManyRequests,
			wantScope: cliproxyexecutor.StateEffectScopeQuotaFamily,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			auth := &Auth{
				ID:       fmt.Sprintf("compact-state-auth-%d", index),
				Provider: "compact-state-provider",
				Status:   StatusActive,
			}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatalf("register auth: %v", err)
			}
			before, ok := manager.GetByID(auth.ID)
			if !ok || before == nil {
				t.Fatal("registered auth missing")
			}
			model := fmt.Sprintf("compact-state-model-%d", index)
			upstream := &compactOutcomeError{status: test.status, message: http.StatusText(test.status)}
			result := executionResult(
				before,
				before.Provider,
				model,
				upstream,
				cliproxyexecutor.DispatchPhaseResponseStarted,
			)
			if fault := applyResponsesCompactErrorDisposition(
				cliproxyexecutor.Options{Alt: "responses/compact"},
				upstream,
				&result,
			); fault || result.Disposition.EffectScope != test.wantScope {
				t.Fatalf("compact state result = %#v, request fault = %t", result, fault)
			}
			manager.MarkResult(context.Background(), result)

			updated, ok := manager.GetByID(auth.ID)
			if !ok || updated == nil {
				t.Fatal("updated auth missing")
			}
			switch test.wantScope {
			case cliproxyexecutor.StateEffectScopeCredential:
				if !updated.Unavailable || updated.NextRetryAfter.IsZero() ||
					updated.StateEffectScope != cliproxyexecutor.StateEffectScopeCredential {
					t.Fatalf("credential compact state = %#v", updated)
				}
				if state := updated.ModelStates[model]; state != nil && state.Unavailable {
					t.Fatalf("credential compact failure created model cooldown: %#v", state)
				}
			case cliproxyexecutor.StateEffectScopeQuotaFamily:
				state := updated.ModelStates[model]
				if state == nil || !state.Unavailable || !state.Quota.Exceeded ||
					state.EffectScope != cliproxyexecutor.StateEffectScopeQuotaFamily ||
					state.NextRetryAfter.IsZero() {
					t.Fatalf("quota-family compact state = %#v", state)
				}
			}
		})
	}
}
