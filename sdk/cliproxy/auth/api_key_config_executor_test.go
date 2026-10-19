package auth

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type scopedAPIKeyTestExecutor struct {
	schedulerTestExecutor
	apiKeyView bool
	calls      *[]bool
}

func (e *scopedAPIKeyTestExecutor) ForAPIKey() ProviderExecutor {
	return &scopedAPIKeyTestExecutor{apiKeyView: true, calls: e.calls}
}

func (e *scopedAPIKeyTestExecutor) Execute(
	_ context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	*e.calls = append(*e.calls, e.apiKeyView)
	return cliproxyexecutor.Response{}, nil
}

func TestAPIKeyExecutionUsesScopedExecutor(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		want       bool
	}{
		{name: "API key", kind: AuthKindAPIKey, want: true},
		{name: "OAuth", kind: AuthKindOAuth, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			calls := make([]bool, 0, 1)
			manager.RegisterExecutor(&scopedAPIKeyTestExecutor{calls: &calls})
			auth := &Auth{
				ID: "api-key-executor-" + tc.kind, Provider: "test", Status: StatusActive,
				Attributes: map[string]string{AttributeAuthKind: tc.kind, AttributeAPIKey: "configured-key"},
			}
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "scoped-model"}})
			t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
			if _, err := manager.Execute(context.Background(), []string{"test"}, cliproxyexecutor.Request{Model: "scoped-model"}, cliproxyexecutor.Options{}); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 || calls[0] != tc.want {
				t.Fatalf("executor view used = %v, want %v", calls, tc.want)
			}
		})
	}
}

func TestHomeAPIKeyAttemptUsesScopedExecutor(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	calls := make([]bool, 0, 1)
	original := &scopedAPIKeyTestExecutor{calls: &calls}
	selection := &HomeDispatchSelection{Provider: "test", Executor: original}
	auth := &Auth{ID: "home-api-key", Provider: "test", Attributes: map[string]string{AttributeAuthKind: AuthKindAPIKey}}
	attempt, err := manager.prepareHomeAttempt(
		context.Background(), selection, auth,
		cliproxyexecutor.Request{Model: "scoped-model"}, cliproxyexecutor.Options{}, "scoped-model",
	)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.executor == original || attempt.executor == nil {
		t.Fatalf("Home executor view = %T", attempt.executor)
	}
	if _, err := attempt.executor.Execute(attempt.ctx, attempt.auth, attempt.request, attempt.options); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !calls[0] {
		t.Fatalf("Home executor view used = %v", calls)
	}
}
