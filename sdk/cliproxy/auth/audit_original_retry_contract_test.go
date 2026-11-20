package auth

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func auditSafeRetryRejection() *Error {
	return errorWithState(
		&Error{HTTPStatus: http.StatusInternalServerError, Message: "boom"}, errorState{
			DispatchPhase: executor.DispatchPhaseNotSent, Outcome: executor.ExecutionOutcomeRejectedBeforeProcessing,
			RetryDirective: executor.RetryDirectiveNextAuth, EffectScope: executor.StateEffectScopeRequest,
		},
	)
}

func TestAuditOriginalAuthRequestRetryOverride(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.SetRetryConfig(3, 30*time.Second, 0)
	manager.RegisterExecutor(&roundExecutor{provider: "claude"})
	const model = "test-model"
	auth := &Auth{
		ID: "auth-1", Provider: "claude", Metadata: map[string]any{"request_retry": float64(0)},
		ModelStates: map[string]*ModelState{
			model: {
				Unavailable: true, Status: StatusError, NextRetryAfter: time.Now().Add(5 * time.Second),
				LastError: auditSafeRetryRejection(),
			},
		},
	}
	if _, err := manager.Register(WithSkipPersist(t.Context()), auth); err != nil {
		t.Fatal(err)
	}
	registerReservationContextModels(t, "claude", model, auth.ID)
	ctx := apiKeyReservationTestContext()
	for _, test := range []struct {
		retries, attempt int
		want             bool
	}{{0, 0, false}, {1, 0, true}, {1, 1, false}} {
		current, _ := manager.GetByID(auth.ID)
		current.Metadata["request_retry"] = float64(test.retries)
		if _, err := manager.Update(WithSkipPersist(t.Context()), current); err != nil {
			t.Fatal(err)
		}
		wait, retry := manager.closestCooldownWait(
			ctx, executor.Options{}, []string{"claude"}, model, test.attempt, http.StatusInternalServerError,
		)
		if retry != test.want || retry && wait <= 0 {
			t.Fatalf(
				"request_retry=%d attempt=%d wait=%v retry=%t want=%t", test.retries, test.attempt, wait, retry,
				test.want,
			)
		}
	}
}

func TestAuditOriginalMaxRetryCredentials(t *testing.T) {
	for _, kind := range []string{"execute", "count", "stream"} {
		for _, limit := range []int{1, 0} {
			t.Run(
				kind+map[int]string{1: "/limited", 0: "/unlimited"}[limit], func(t *testing.T) {
					manager := NewManager(nil, &FillFirstSelector{}, nil)
					manager.SetRetryConfig(0, 0, limit)
					provider := &roundExecutor{provider: "claude", failure: auditSafeRetryRejection()}
					manager.RegisterExecutor(provider)
					for _, id := range []string{"auth-1", "auth-2"} {
						if _, err := manager.Register(
							WithSkipPersist(t.Context()), &Auth{ID: id, Provider: "claude"},
						); err != nil {
							t.Fatal(err)
						}
					}
					registerReservationContextModels(t, "claude", "test-model", "auth-1", "auth-2")
					ctx := apiKeyReservationTestContext()
					request := executor.Request{Model: "test-model"}
					var err error
					switch kind {
					case "execute":
						_, err = manager.Execute(ctx, []string{"claude"}, request, executor.Options{})
					case "count":
						_, err = manager.ExecuteCount(ctx, []string{"claude"}, request, executor.Options{})
					case "stream":
						_, err = manager.ExecuteStream(ctx, []string{"claude"}, request, executor.Options{Stream: true})
					}
					want := 2
					if limit == 1 {
						want = 1
					}
					if err == nil || len(provider.calls) != want {
						t.Fatalf("calls=%v error=%v want attempts=%d", provider.calls, err, want)
					}
				},
			)
		}
	}
}

type auditModelSupportExecutor struct {
	roundExecutor
	status int
}

func (e *auditModelSupportExecutor) Execute(
	_ context.Context,
	auth *Auth,
	_ executor.Request,
	_ executor.Options,
) (executor.Response, error) {
	e.calls = append(e.calls, auth.ID)
	if auth.ID == "aa-bad-auth" {
		return executor.Response{}, &Error{
			HTTPStatus: e.status, Message: "invalid_request_error: The requested model is not supported.",
		}
	}
	return executor.Response{Payload: []byte(auth.ID)}, nil
}

func TestAuditOriginalModelSupportRejectionFallsBack(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		t.Run(
			http.StatusText(status), func(t *testing.T) {
				manager := NewManager(nil, &FillFirstSelector{}, nil)
				provider := &auditModelSupportExecutor{provider: "claude", status: status}
				manager.RegisterExecutor(provider)
				const model = "claude-opus-4-6"
				for _, id := range []string{"aa-bad-auth", "bb-good-auth"} {
					if _, err := manager.Register(
						WithSkipPersist(t.Context()), &Auth{ID: id, Provider: "claude"},
					); err != nil {
						t.Fatal(err)
					}
				}
				registerReservationContextModels(t, "claude", model, "aa-bad-auth", "bb-good-auth")
				for range 2 {
					response, err := manager.Execute(
						apiKeyReservationTestContext(), []string{"claude"}, executor.Request{Model: model},
						executor.Options{},
					)
					if err != nil || string(response.Payload) != "bb-good-auth" {
						t.Fatalf("payload=%q error=%v", response.Payload, err)
					}
				}
				if !reflect.DeepEqual(provider.calls, []string{"aa-bad-auth", "bb-good-auth", "bb-good-auth"}) {
					t.Fatalf("calls=%v", provider.calls)
				}
				bad, _ := manager.GetByID("aa-bad-auth")
				state := bad.ModelStates[model]
				if state == nil || !state.Unavailable || !state.NextRetryAfter.IsZero() {
					t.Fatalf("model rejection state=%+v", state)
				}
			},
		)
	}
}

func TestAuditOriginalDisableCoolingOverride(t *testing.T) {
	manager, auth := newExecutionOutcomeTestManager(
		t, &Auth{ID: "auth-1", Provider: "claude", Metadata: map[string]any{"disable_cooling": true}},
	)
	failure := auditSafeRetryRejection()
	failure.executionState().EffectScope = executor.StateEffectScopeExactModel
	failure.executionState().EffectKey = "test-model"
	manager.MarkResult(
		t.Context(), executionResult(auth, "claude", "test-model", failure, executor.DispatchPhaseNotSent),
	)
	updated, _ := manager.GetByID(auth.ID)
	state := updated.ModelStates["test-model"]
	if state == nil || !state.NextRetryAfter.IsZero() {
		t.Fatalf("disable_cooling state=%+v", state)
	}
}
