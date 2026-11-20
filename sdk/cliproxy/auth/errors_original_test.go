package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"runtime"
	"testing"
	"time"
	"weak"

	core "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// noinspection GoDirectComparisonOfErrors
func TestOriginalAuthUnavailableErrorPublicHelpers(t *testing.T) {
	now := time.Unix(1700000000, 0)
	cause := errors.New("original upstream cause")
	failure := newAuthUnavailableErrorWithCause(now.Add(1500*time.Millisecond), now, cause)
	legacy, ok := failure.(interface {
		Headers() http.Header
		WithAuthError(*Error) error
	})
	if !ok || !reflect.DeepEqual(legacy.Headers(), http.Header{"Retry-After": {"2"}}) {
		t.Fatalf("legacy recovery header=%v error=%v", ok, failure)
	}
	headers := legacy.Headers()
	headers.Set("Retry-After", "999")
	if legacy.Headers().Get("Retry-After") != "2" {
		t.Fatal("caller changed the trusted recovery header")
	}
	base := &Error{
		Code: "auth_unavailable", Message: "enriched context", Retryable: true,
		HTTPStatus: http.StatusServiceUnavailable,
	}
	enriched := legacy.WithAuthError(base)
	var found *Error
	if !errors.As(enriched, &found) || found != base || !errors.Is(enriched, cause) ||
		statusCodeFromError(
			enriched,
		) != http.StatusServiceUnavailable || SafeResponseHeaders(enriched).Get("Retry-After") != "2" {
		t.Fatalf("enriched error lost base, cause or local deadline: %v", enriched)
	}
	if other := legacy.WithAuthError(base); !errors.Is(enriched, other) || !errors.Is(other, enriched) {
		t.Fatal("recovery wrappers with the same base lost their published error identity")
	}
	var absent *authUnavailableError
	if absent.Headers() != nil || absent.WithAuthError(base) != base {
		t.Fatal("nil receiver changed the original helper contract")
	}
	if !IsTerminalAuthError(NewTerminalAuthError(base, cause)) || IsTerminalAuthError(WithCause(base, cause)) {
		t.Fatal("terminal authentication classification changed")
	}
}

// noinspection GoDirectComparisonOfErrors
func TestErrorExtendedStateSurvivesCloneAndJSON(t *testing.T) {
	disposition := core.ExecutionDisposition{
		DispatchPhase: core.DispatchPhaseResponseStarted,
		Outcome:       core.ExecutionOutcomeRejectedBeforeProcessing, RetryDirective: core.RetryDirectiveNoRetry,
		EffectScope: core.StateEffectScopeExactModel, EffectKey: "gpt-5(high)",
	}
	original := (&Error{
		Code: "force_cooldown", Message: "upstream detail", Retryable: true, HTTPStatus: 503,
	}).WithExecutionDisposition(disposition, 42)
	cause := errors.New("runtime cause is not persisted")
	original.executionState().cause = cause
	cloned := original.Clone()
	if cloned == original || cloned.executionState() == original.executionState() || !errors.Is(cloned, cause) ||
		cloned.ExecutionDisposition() != disposition ||
		cloned.ExecutionStateGeneration() != 42 {
		t.Fatalf("error clone lost extended state: %+v", cloned)
	}
	clonedDisposition := core.ExecutionDisposition{EffectScope: core.StateEffectScopeRequest}
	if result := cloned.WithExecutionDisposition(clonedDisposition, 43); result != cloned ||
		result.ExplicitExecutionDisposition() != clonedDisposition || result.ExecutionStateGeneration() != 43 ||
		!errors.Is(result, cause) {
		t.Fatal("cloned error lost its identity, cause or updated execution state")
	}
	if original.ExecutionDisposition() != disposition || original.ExecutionStateGeneration() != 42 {
		t.Fatal("cloned metadata changed the original error")
	}
	modelCopy := (&ModelState{LastError: original}).Clone()
	if modelCopy.LastError == original || modelCopy.LastError.executionState() == original.executionState() ||
		!errors.Is(modelCopy.LastError, cause) || modelCopy.LastError.ExecutionDisposition() != disposition ||
		modelCopy.LastError.ExecutionStateGeneration() != 42 {
		t.Fatal("model state clone lost independent error details")
	}
	var absent *Error
	if absent.Clone() != nil || absent.WithExecutionDisposition(disposition, 42) != nil ||
		errorForResultWithDisposition(nil, disposition, 42) != nil {
		t.Fatal("nil error acquired execution state")
	}
	body, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Error
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Code !=
		original.Code ||
		decoded.Message !=
			original.Message ||
		decoded.HTTPStatus !=
			503 ||
		!decoded.Retryable ||
		decoded.ExecutionDisposition() != disposition || decoded.ExecutionStateGeneration() != 42 ||
		decoded.Unwrap() != nil {
		t.Fatalf("JSON round trip changed public or extended state: %s", body)
	}
	var persisted map[string]json.RawMessage
	if err := json.Unmarshal(body, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted) != 10 || persisted["cause"] != nil {
		t.Fatalf("extended JSON fields=%s", body)
	}
	if err := json.Unmarshal(
		[]byte(`{"code":"legacy","message":"original SDK message","retryable":true,"http_status":429}`), &decoded,
	); err != nil {
		t.Fatal(err)
	}
	if decoded.ExecutionStateGeneration() != 42 || decoded.ExecutionDisposition() != disposition ||
		decoded.executionState().cause != nil {
		t.Fatal("JSON fields omitted from an update changed existing execution state")
	}
	if reflect.TypeFor[Error]().NumField() != 4 {
		t.Fatal("published four-field layout changed")
	}
}

func TestErrorPublishedValueAndPointerCopies(t *testing.T) {
	disposition := core.ExecutionDisposition{
		DispatchPhase: core.DispatchPhaseResponseStarted,
		Outcome:       core.ExecutionOutcomeRejectedBeforeProcessing, RetryDirective: core.RetryDirectiveNoRetry,
		EffectScope: core.StateEffectScopeExactModel, EffectKey: "model",
	}
	original := (&Error{
		Code: "legacy", Message: "original SDK message", Retryable: true, HTTPStatus: 429,
	}).WithExecutionDisposition(disposition, 42)
	copied := *original
	valueBody, err := json.Marshal(copied)
	if err != nil {
		t.Fatal(err)
	}
	if string(
		valueBody,
	) != `{"code":"legacy","message":"original SDK message","retryable":true,"http_status":429}` ||
		copied.ExecutionStateGeneration() != 0 {
		t.Fatalf("published value copy changed its four fields: %s", valueBody)
	}
	cloned := original.Clone()
	pointerBody, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	cloneBody, err := json.Marshal(cloned)
	if err != nil {
		t.Fatal(err)
	}
	if string(pointerBody) != string(cloneBody) || cloned.ExecutionDisposition() != disposition ||
		cloned.ExecutionStateGeneration() != 42 {
		t.Fatalf("pointer clone changed complete execution data: %s / %s", pointerBody, cloneBody)
	}
}

func TestErrorJSONRetainsPublishedPartialAndNullUpdates(t *testing.T) {
	original := (&Error{Code: "legacy", Message: "old", Retryable: true, HTTPStatus: 429}).WithExecutionDisposition(
		core.ExecutionDisposition{EffectScope: core.StateEffectScopeExactModel, EffectKey: "model"},
		42,
	)
	cause := errors.New("runtime cause")
	original.executionState().cause = cause
	if err := json.Unmarshal([]byte(`{"message":"new"}`), original); err != nil {
		t.Fatal(err)
	}
	if original.Code != "legacy" || original.Message != "new" || !original.Retryable || original.HTTPStatus != 429 ||
		original.ExecutionStateGeneration() != 42 || original.executionState().EffectKey != "model" ||
		!errors.Is(
			original, cause,
		) {
		t.Fatalf("partial JSON cleared fields absent from the input: %+v", original)
	}
	before := original.Clone()
	if err := json.Unmarshal([]byte("null"), original); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*original, *before) || *original.executionState() != *before.executionState() {
		t.Fatal("JSON null changed existing public or extended fields")
	}
}

func TestErrorExtendedStateLifetimeFollowsItsOwner(t *testing.T) {
	alive := (&Error{Message: "live error"}).WithExecutionDisposition(
		core.ExecutionDisposition{EffectScope: core.StateEffectScopeRequest}, 42,
	)
	runtime.GC()
	if alive.ExecutionStateGeneration() != 42 {
		t.Fatal("live error lost its extended state")
	}
	runtime.KeepAlive(alive)
	key := func() weak.Pointer[Error] {
		owner := (&Error{Message: "released error"}).WithExecutionDisposition(
			core.ExecutionDisposition{EffectScope: core.StateEffectScopeRequest}, 43,
		)
		return weak.Make(owner)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		runtime.Gosched()
		if _, retained := errorStates.Load(key); !retained {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("released error retained extended state after garbage collection")
		}
	}
}
