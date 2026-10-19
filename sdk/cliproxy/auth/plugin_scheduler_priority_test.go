package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type priorityPluginScheduler struct {
	across   bool
	response pluginapi.SchedulerPickResponse
	selected [][]pluginapi.SchedulerAuthCandidate
}

func (s *priorityPluginScheduler) SchedulerWantsAcrossPriorities() bool {
	return s.across
}

func (s *priorityPluginScheduler) PickAuth(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, bool, error) {
	s.selected = append(s.selected, req.Candidates)
	return s.response, s.response.Handled, nil
}

func newPriorityPluginManager(t *testing.T, mixed bool) *Manager {
	t.Helper()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.executors["gemini"] = schedulerTestExecutor{}
	lowProvider := "gemini"
	if mixed {
		lowProvider = "claude"
		manager.executors[lowProvider] = schedulerTestExecutor{}
	}
	for _, auth := range []*Auth{
		{ID: "priority-plugin-high", Provider: "gemini", Attributes: map[string]string{"priority": "10"}},
		{ID: "priority-plugin-low", Provider: lowProvider, Attributes: map[string]string{"priority": "5"}},
	} {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s): %v", auth.ID, err)
		}
	}
	return manager
}

func TestPluginSchedulerPriorityScopeAndFallback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mixed     bool
		across    bool
		handled   bool
		wantCount int
		wantID    string
	}{
		{name: "default single", wantCount: 1, wantID: "priority-plugin-high", handled: true},
		{name: "across single", across: true, wantCount: 2, wantID: "priority-plugin-low", handled: true},
		{name: "unhandled single", across: true, wantCount: 2, wantID: "priority-plugin-high"},
		{name: "default mixed", mixed: true, wantCount: 1, wantID: "priority-plugin-high", handled: true},
		{name: "across mixed", mixed: true, across: true, wantCount: 2, wantID: "priority-plugin-low", handled: true},
		{name: "unhandled mixed", mixed: true, across: true, wantCount: 2, wantID: "priority-plugin-high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := newPriorityPluginManager(t, tc.mixed)
			scheduler := &priorityPluginScheduler{
				across:   tc.across,
				response: pluginapi.SchedulerPickResponse{Handled: tc.handled, AuthID: tc.wantID},
			}
			manager.SetPluginScheduler(scheduler)
			var selected *Auth
			var provider string
			var err error
			if tc.mixed {
				selected, _, provider, err = manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
			} else {
				selected, _, err = manager.PickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
			}
			if err != nil || selected == nil || selected.ID != tc.wantID {
				t.Fatalf("selected = %+v, error = %v, want %s", selected, err, tc.wantID)
			}
			if tc.mixed && provider != selected.Provider {
				t.Errorf("selected provider = %q, want %q", provider, selected.Provider)
			}
			if len(scheduler.selected) != 1 || len(scheduler.selected[0]) != tc.wantCount {
				t.Fatalf("scheduler candidates = %+v, want %d", scheduler.selected, tc.wantCount)
			}
			if !tc.across && scheduler.selected[0][0].ID != "priority-plugin-high" {
				t.Errorf("default scheduler saw lower priority: %+v", scheduler.selected[0])
			}
		})
	}
}

func TestPluginSchedulerRejectTerminatesSelection(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		manager := newPriorityPluginManager(t, mixed)
		scheduler := &priorityPluginScheduler{
			response: pluginapi.SchedulerPickResponse{
				Handled: true, Reject: true, RejectCode: "blocked_by_plugin", RejectReason: "selection rejected",
			},
		}
		manager.SetPluginScheduler(scheduler)
		var err error
		if mixed {
			_, _, _, err = manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
		} else {
			_, _, err = manager.PickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
		}
		var selectionErr *Error
		if !errors.As(err, &selectionErr) || selectionErr.Code != "blocked_by_plugin" || selectionErr.Message != "selection rejected" {
			t.Fatalf("mixed=%t rejection = %v, want terminal plugin error", mixed, err)
		}
	}
}

func TestPluginSchedulerOnlyReceivesAvailableAuths(t *testing.T) {
	manager := newPriorityPluginManager(t, true)
	manager.mu.Lock()
	manager.auths["priority-plugin-high"].Unavailable = true
	manager.auths["priority-plugin-high"].NextRetryAfter = time.Now().Add(time.Hour)
	manager.mu.Unlock()
	scheduler := &priorityPluginScheduler{response: pluginapi.SchedulerPickResponse{Handled: true, AuthID: "priority-plugin-low"}}
	manager.SetPluginScheduler(scheduler)
	selected, _, _, err := manager.pickNextMixed(context.Background(), []string{"gemini", "claude"}, "", cliproxyexecutor.Options{}, nil)
	if err != nil || selected == nil || selected.ID != "priority-plugin-low" {
		t.Fatalf("selection = %+v, error = %v", selected, err)
	}
	if len(scheduler.selected) != 1 || len(scheduler.selected[0]) != 1 || scheduler.selected[0][0].ID != "priority-plugin-low" {
		t.Fatalf("scheduler saw blocked auths: %+v", scheduler.selected)
	}
}
