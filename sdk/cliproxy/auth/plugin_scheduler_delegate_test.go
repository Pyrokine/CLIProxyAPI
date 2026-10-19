package auth

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/registry"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestManagerPluginSchedulerDelegatesBuiltin(t *testing.T) {
	t.Run("round-robin", func(t *testing.T) {
		manager := NewManager(nil, &FillFirstSelector{}, nil)
		manager.executors["gemini"] = schedulerTestExecutor{}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-b) error = %v", errRegister)
		}
		manager.SetPluginScheduler(&priorityPluginScheduler{
			response: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin},
		})

		gotA, _, errPick := manager.PickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext() first error = %v", errPick)
		}
		gotB, _, errPick := manager.PickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext() second error = %v", errPick)
		}
		if gotA == nil || gotB == nil {
			t.Fatalf("pickNext() auths = %v, %v; want non-nil", gotA, gotB)
		}
		if gotA.ID != "auth-a" || gotB.ID != "auth-b" {
			t.Fatalf("round-robin picks = %q, %q; want auth-a, auth-b", gotA.ID, gotB.ID)
		}
	})

	t.Run("round-robin model cursors", func(t *testing.T) {
		reg := registry.GetGlobalRegistry()
		models := []*registry.ModelInfo{{ID: "model-a"}, {ID: "model-b"}}
		for _, authID := range []string{"auth-a", "auth-b"} {
			reg.RegisterClient(authID, "gemini", models)
			t.Cleanup(func() {
				reg.UnregisterClient(authID)
			})
		}

		manager := NewManager(nil, &FillFirstSelector{}, nil)
		manager.executors["gemini"] = schedulerTestExecutor{}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-b) error = %v", errRegister)
		}
		manager.SetPluginScheduler(&priorityPluginScheduler{
			response: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin},
		})

		gotModelA, _, errPick := manager.PickNext(context.Background(), "gemini", "model-a", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext(model-a) error = %v", errPick)
		}
		gotModelB, _, errPick := manager.PickNext(context.Background(), "gemini", "model-b", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext(model-b) error = %v", errPick)
		}
		if gotModelA == nil || gotModelB == nil {
			t.Fatalf("pickNext() auths = %v, %v; want non-nil", gotModelA, gotModelB)
		}
		if gotModelA.ID != "auth-a" || gotModelB.ID != "auth-a" {
			t.Fatalf("model-scoped round-robin picks = %q, %q; want auth-a, auth-a", gotModelA.ID, gotModelB.ID)
		}
	})

	t.Run("fill-first", func(t *testing.T) {
		manager := NewManager(nil, &RoundRobinSelector{}, nil)
		manager.executors["gemini"] = schedulerTestExecutor{}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-b", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-b) error = %v", errRegister)
		}
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: "auth-a", Provider: "gemini"}); errRegister != nil {
			t.Fatalf("Register(auth-a) error = %v", errRegister)
		}
		manager.SetPluginScheduler(&priorityPluginScheduler{
			response: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinFillFirst},
		})

		got, _, errPick := manager.PickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNext() error = %v", errPick)
		}
		if got == nil {
			t.Fatalf("pickNext() auth = nil")
		}
		if got.ID != "auth-a" {
			t.Fatalf("fill-first pick = %q, want auth-a", got.ID)
		}
	})
}

func TestManagerPluginSchedulerDelegateRoundRobinUsesNativeMixedRotation(t *testing.T) {
	manager := NewManager(nil, &FillFirstSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	manager.executors["claude"] = schedulerTestExecutor{}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-a", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-a) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "gemini-b", Provider: "gemini"}); errRegister != nil {
		t.Fatalf("Register(gemini-b) error = %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), &Auth{ID: "claude-a", Provider: "claude"}); errRegister != nil {
		t.Fatalf("Register(claude-a) error = %v", errRegister)
	}
	manager.SetPluginScheduler(&priorityPluginScheduler{
		response: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin},
	})

	wantProviders := []string{"gemini", "gemini", "claude", "gemini"}
	wantIDs := []string{"gemini-a", "gemini-b", "claude-a", "gemini-a"}
	for index := range wantProviders {
		got, _, provider, errPick := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		if errPick != nil {
			t.Fatalf("pickNextMixed() #%d error = %v", index, errPick)
		}
		if got == nil {
			t.Fatalf("pickNextMixed() #%d auth = nil", index)
		}
		if provider != wantProviders[index] {
			t.Fatalf("pickNextMixed() #%d provider = %q, want %q", index, provider, wantProviders[index])
		}
		if got.ID != wantIDs[index] {
			t.Fatalf("pickNextMixed() #%d auth.ID = %q, want %q", index, got.ID, wantIDs[index])
		}
	}
}

func TestPluginSchedulerDelegationPreservesConfiguredStrategy(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%t", mixed), func(t *testing.T) {
			manager := NewManager(nil, &FillFirstSelector{}, nil)
			manager.executors["gemini"] = schedulerTestExecutor{}
			manager.executors["claude"] = schedulerTestExecutor{}
			for _, auth := range []*Auth{
				{ID: "auth-a", Provider: "gemini"},
				{ID: "auth-b", Provider: "gemini"},
				{ID: "auth-c", Provider: "claude"},
			} {
				if _, err := manager.Register(t.Context(), auth); err != nil {
					t.Fatal(err)
				}
			}
			pick := func() *Auth {
				t.Helper()
				var selected *Auth
				var err error
				if mixed {
					selected, _, _, err = manager.pickNextMixed(t.Context(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
				} else {
					selected, _, err = manager.PickNext(t.Context(), "gemini", "", cliproxyexecutor.Options{}, nil)
				}
				if err != nil || selected == nil {
					t.Fatalf("selection = %#v, %v", selected, err)
				}
				return selected
			}
			manager.SetPluginScheduler(&priorityPluginScheduler{
				response: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin},
			})
			for _, want := range []string{"auth-a", "auth-b"} {
				if got := pick().ID; got != want {
					t.Fatalf("delegated selection = %q, want %q", got, want)
				}
			}
			manager.SetPluginScheduler(nil)
			for range 3 {
				if got := pick().ID; got != "auth-a" {
					t.Fatalf("configured fill-first selection = %q, want auth-a", got)
				}
			}
			manager.scheduler.mu.Lock()
			configured := manager.scheduler.strategy
			manager.scheduler.mu.Unlock()
			if configured != schedulerStrategyFillFirst {
				t.Fatalf("configured strategy changed to %v", configured)
			}
		})
	}
}

func TestSchedulerRequestStrategyConcurrentSelection(t *testing.T) {
	scheduler := newAuthScheduler(&FillFirstSelector{})
	for _, id := range []string{"auth-a", "auth-b"} {
		scheduler.upsertAuth(&Auth{ID: id, Provider: "gemini"})
	}
	failures := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Go(func() {
		for index := range 40 {
			selected, err := scheduler.pickSingleWithPredicate(t.Context(), "gemini", "", cliproxyexecutor.Options{}, nil, nil, schedulerStrategyRoundRobin)
			want := []string{"auth-a", "auth-b"}[index%2]
			if err != nil || selected == nil || selected.ID != want {
				failures <- fmt.Errorf("delegated selection #%d = %#v, %v; want %s", index, selected, err, want)
				return
			}
		}
	})
	workers.Go(func() {
		for index := range 40 {
			selected, err := scheduler.pickSingleWithPredicate(t.Context(), "gemini", "", cliproxyexecutor.Options{}, nil, nil)
			if err != nil || selected == nil || selected.ID != "auth-a" {
				failures <- fmt.Errorf("configured selection #%d = %#v, %v; want auth-a", index, selected, err)
				return
			}
		}
	})
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}
