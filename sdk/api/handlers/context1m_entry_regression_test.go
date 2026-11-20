package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

func TestContext1MEntryChecksAllRoutesAndBothLimits(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		context    int
		maxContext int
		lowFirst   bool
		allowed    bool
	}{
		{name: "context exactly one million", context: 1_000_000, allowed: true},
		{name: "max context exactly one million", maxContext: 1_000_000, allowed: true},
		{name: "context low max high", context: 128_000, maxContext: 1_048_576, allowed: true},
		{name: "context high max low", context: 1_048_576, maxContext: 128_000, allowed: true},
		{name: "both below one million", context: 999_999, maxContext: 999_999},
		{name: "missing limits"},
		{name: "later route supports one million", context: 1_048_576, lowFirst: true, allowed: true},
		{name: "later route max context supports one million", maxContext: 1_048_576, lowFirst: true, allowed: true},
	} {
		t.Run(
			scenario.name, func(t *testing.T) {
				model := "fixture-context-entry-" + fmt.Sprint(
					scenario.context, "-", scenario.maxContext, "-", scenario.lowFirst,
				)
				var executions atomic.Int32
				executor := &lifecycleCaptureExecutor{
					provider: "zzz-context-capable", execute: func(
						_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options,
					) (coreexecutor.Response, error) {
						executions.Add(1)
						return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
					},
				}
				handler := newLifecycleHandler(t, model, executor)
				reg := registry.GetGlobalRegistry()
				reg.RegisterClient(
					"handler-lifecycle-"+model, executor.provider, []*registry.ModelInfo{
						{
							ID: model, ContextLength: scenario.context, MaxContextLength: scenario.maxContext,
						},
					},
				)
				if scenario.lowFirst {
					low := &coreauth.Auth{
						ID: "context-low-" + model, Provider: "aaa-context-low", Status: coreauth.StatusActive,
					}
					if _, err := handler.AuthManager.Register(t.Context(), low); err != nil {
						t.Fatal(err)
					}
					handler.AuthManager.RegisterExecutor(
						&lifecycleCaptureExecutor{
							provider: low.Provider, execute: func(
								_ context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options,
							) (coreexecutor.Response, error) {
								t.Error("unsupported first route was dispatched")
								return coreexecutor.Response{}, fmt.Errorf("unsupported context route was selected")
							},
						},
					)
					reg.RegisterClient(low.ID, low.Provider, []*registry.ModelInfo{{ID: model, ContextLength: 128_000}})
					t.Cleanup(func() { reg.UnregisterClient(low.ID) })
				}
				requested := model + "[1m]"
				body, _, errMsg := handler.ExecuteWithAuthManager(
					t.Context(), "openai", requested, []byte(fmt.Sprintf(`{"model":%q}`, requested)), "",
				)
				if scenario.allowed {
					if errMsg != nil || executions.Load() != 1 || string(body) != `{"ok":true}` {
						t.Fatalf(
							"public entry rejected a capable route: error=%+v, executions=%d, body=%s", errMsg,
							executions.Load(), body,
						)
					}
				} else if errMsg == nil || errMsg.StatusCode != http.StatusBadRequest || executions.Load() != 0 {
					t.Fatalf("unsupported context request: error=%+v, executions=%d", errMsg, executions.Load())
				}
			},
		)
	}
}
