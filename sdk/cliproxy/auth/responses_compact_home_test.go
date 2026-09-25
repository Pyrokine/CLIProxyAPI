package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestManagerResponsesCompactHomeStopsWithoutRedispatch(t *testing.T) {
	for index, test := range []struct {
		name        string
		err         error
		wantOutcome cliproxyexecutor.ExecutionOutcome
	}{
		{
			name:        "endpoint 404",
			err:         &compactOutcomeError{status: http.StatusNotFound, message: "compact endpoint not found"},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeRejectedBeforeProcessing,
		},
		{
			name:        "transient 500",
			err:         &compactOutcomeError{status: http.StatusInternalServerError, message: "temporary compact failure"},
			wantOutcome: cliproxyexecutor.ExecutionOutcomeUnknown,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := fmt.Sprintf("compact-home-provider-%d", index)
			model := fmt.Sprintf("compact-home-model-%d", index)
			executor := &requestScopedTestExecutor{provider: provider}
			executor.execute = func(*Auth) (cliproxyexecutor.Response, error) {
				return cliproxyexecutor.Response{}, test.err
			}
			dispatcher := &requestScopedHomeDispatcher{auths: []*Auth{
				requestScopedHomeAuth("compact-home-first", provider, 0, ""),
				requestScopedHomeAuth("compact-home-second", provider, 0, ""),
			}}
			hook := &runtimeResultHook{results: make(chan Result, 2)}
			manager, _ := newRequestScopedHomeManager(t, dispatcher, executor, hook)

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
				t.Fatalf("Home Execute() calls = %d, want 1", calls)
			}
			if dispatches := dispatcher.dispatchCount(); dispatches != 1 {
				t.Fatalf("Home dispatches = %d, want 1", dispatches)
			}
			select {
			case recorded := <-hook.results:
				if recorded.Disposition.Outcome != test.wantOutcome ||
					recorded.Disposition.RetryDirective != cliproxyexecutor.RetryDirectiveNoRetry ||
					recorded.Disposition.EffectScope != cliproxyexecutor.StateEffectScopeRequest {
					t.Fatalf("Home compact result = %#v", recorded)
				}
			case <-time.After(time.Second):
				t.Fatal("Home compact result hook was not called")
			}
		})
	}
}
