package auth

import (
	"fmt"
	"testing"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type pluginSchedulerWithState struct {
	*priorityPluginScheduler
	active bool
}

func (s *pluginSchedulerWithState) HasScheduler() bool { return s.active }

func TestPluginSchedulerOptionalStatePreservesNativeAndLegacySelection(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, state := range []string{"inactive", "active", "legacy"} {
			t.Run(
				fmt.Sprintf("%s/mixed=%t", state, mixed), func(t *testing.T) {
					manager := newPriorityPluginManager(t, mixed)
					base := &priorityPluginScheduler{
						response: pluginapi.SchedulerPickResponse{
							Handled: true, AuthID: "priority-plugin-high",
						},
					}
					var scheduler PluginScheduler = base
					if state != "legacy" {
						scheduler = &pluginSchedulerWithState{priorityPluginScheduler: base, active: state == "active"}
					}
					manager.SetPluginScheduler(scheduler)
					var selected *Auth
					var err error
					if mixed {
						selected, _, _, err = manager.pickNextMixed(
							t.Context(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil,
						)
					} else {
						selected, _, err = manager.PickNext(t.Context(), "gemini", "", cliproxyexecutor.Options{}, nil)
					}
					if err != nil || selected == nil || selected.ID != "priority-plugin-high" {
						t.Fatalf("native or legacy selection changed: selected=%+v error=%v", selected, err)
					}
					wantCalls := 1
					if state == "inactive" {
						wantCalls = 0
					}
					if len(base.selected) != wantCalls {
						t.Fatalf("scheduler calls=%d, want %d", len(base.selected), wantCalls)
					}
				},
			)
		}
	}
}
