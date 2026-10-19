package pluginhost

import (
	"context"
	"errors"
	"strings"
	"testing"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginabi"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestHostPickAuthUsesHighestPrioritySchedulerOnly(t *testing.T) {
	var highCalls int
	var lowCalls int
	host := newHostWithRecords(
		capabilityRecord{
			id:       "low",
			priority: 1,
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					Scheduler: schedulerFunc(
						func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
							lowCalls++
							return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-low"}, nil
						},
					),
				},
			},
		},
		capabilityRecord{
			id:       "high",
			priority: 10,
			meta:     pluginapi.Metadata{Name: "high", Version: "1.0.0"},
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					Scheduler: schedulerFunc(
						func(ctx context.Context, req pluginapi.SchedulerPickRequest) (
							pluginapi.SchedulerPickResponse,
							error,
						) {
							highCalls++
							if req.Plugin.Name != "high" {
								t.Fatalf("req.Plugin.Name = %q, want high", req.Plugin.Name)
							}
							return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-high"}, nil
						},
					),
				},
			},
		},
	)

	resp, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-high", "auth-low"))
	if errPick != nil {
		t.Fatalf("PickAuth() error = %v, want nil", errPick)
	}
	if !handled {
		t.Fatal("PickAuth() handled = false, want true")
	}
	if resp.AuthID != "auth-high" {
		t.Fatalf("PickAuth() AuthID = %q, want auth-high", resp.AuthID)
	}
	if highCalls != 1 {
		t.Fatalf("high calls = %d, want 1", highCalls)
	}
	if lowCalls != 0 {
		t.Fatalf("low calls = %d, want 0", lowCalls)
	}
}

func TestHostPickAuthReturnsSchedulerError(t *testing.T) {
	host := newHostWithRecords(
		capabilityRecord{
			id: "scheduler",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					Scheduler: schedulerFunc(
						func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
							return pluginapi.SchedulerPickResponse{}, errors.New("tenant quota exhausted")
						},
					),
				},
			},
		},
	)

	_, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-1"))
	if !handled {
		t.Fatal("PickAuth() handled = false, want true")
	}
	if errPick == nil || !strings.Contains(errPick.Error(), "tenant quota exhausted") {
		t.Fatalf("PickAuth() error = %v, want tenant quota exhausted", errPick)
	}
}

func TestHostPickAuthPanicFusesAndFallsBack(t *testing.T) {
	host := newHostWithRecords(
		capabilityRecord{
			id: "scheduler",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					Scheduler: schedulerFunc(
						func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
							panic("boom")
						},
					),
				},
			},
		},
	)

	_, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-1"))
	if handled {
		t.Fatal("PickAuth() handled = true, want false")
	}
	if errPick != nil {
		t.Fatalf("PickAuth() error = %v, want nil", errPick)
	}
	if !host.isPluginFused("scheduler") {
		t.Fatal("scheduler plugin was not fused after panic")
	}
}

func TestHostPickAuthUnhandledDoesNotCallLowerPriorityScheduler(t *testing.T) {
	var lowCalls int
	host := newHostWithRecords(
		capabilityRecord{
			id:       "low",
			priority: 1,
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					Scheduler: schedulerFunc(
						func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
							lowCalls++
							return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-low"}, nil
						},
					),
				},
			},
		},
		capabilityRecord{
			id:       "high",
			priority: 10,
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					Scheduler: schedulerFunc(
						func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
							return pluginapi.SchedulerPickResponse{Handled: false}, nil
						},
					),
				},
			},
		},
	)

	_, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-low"))
	if errPick != nil {
		t.Fatalf("PickAuth() error = %v, want nil", errPick)
	}
	if handled {
		t.Fatal("PickAuth() handled = true, want false")
	}
	if lowCalls != 0 {
		t.Fatalf("low calls = %d, want 0", lowCalls)
	}
}

func TestHostPickAuthInvalidResponseFallsBack(t *testing.T) {
	tests := []struct {
		name string
		resp pluginapi.SchedulerPickResponse
	}{
		{
			name: "unknown auth id",
			resp: pluginapi.SchedulerPickResponse{Handled: true, AuthID: "missing"},
		},
		{
			name: "unknown delegate",
			resp: pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: "unknown"},
		},
		{
			name: "handled without decision",
			resp: pluginapi.SchedulerPickResponse{Handled: true},
		},
	}

	for _, tt := range tests {
		t.Run(
			tt.name, func(t *testing.T) {
				host := newHostWithRecords(
					capabilityRecord{
						id: "scheduler",
						plugin: pluginapi.Plugin{
							Capabilities: pluginapi.Capabilities{
								Scheduler: schedulerFunc(
									func(
										context.Context,
										pluginapi.SchedulerPickRequest,
									) (pluginapi.SchedulerPickResponse, error) {
										return tt.resp, nil
									},
								),
							},
						},
					},
				)

				_, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-1"))
				if errPick != nil {
					t.Fatalf("PickAuth() error = %v, want nil", errPick)
				}
				if handled {
					t.Fatal("PickAuth() handled = true, want false")
				}
			},
		)
	}
}

func TestHostPickAuthPrefersValidAuthIDOverInvalidDelegate(t *testing.T) {
	host := newHostWithRecords(
		capabilityRecord{
			id: "scheduler",
			plugin: pluginapi.Plugin{
				Capabilities: pluginapi.Capabilities{
					Scheduler: schedulerFunc(
						func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
							return pluginapi.SchedulerPickResponse{
								Handled: true, AuthID: "auth-a", DelegateBuiltin: "unknown",
							}, nil
						},
					),
				},
			},
		},
	)

	resp, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-a"))
	if errPick != nil {
		t.Fatalf("PickAuth() error = %v, want nil", errPick)
	}
	if !handled {
		t.Fatal("PickAuth() handled = false, want true")
	}
	if resp.AuthID != "auth-a" {
		t.Fatalf("PickAuth() AuthID = %q, want auth-a", resp.AuthID)
	}
}

func TestHostPickAuthAllowsKnownBuiltinDelegates(t *testing.T) {
	for _, delegate := range []string{pluginapi.SchedulerBuiltinRoundRobin, pluginapi.SchedulerBuiltinFillFirst} {
		t.Run(
			delegate, func(t *testing.T) {
				host := newHostWithRecords(
					capabilityRecord{
						id: "scheduler",
						plugin: pluginapi.Plugin{
							Capabilities: pluginapi.Capabilities{
								Scheduler: schedulerFunc(
									func(
										context.Context,
										pluginapi.SchedulerPickRequest,
									) (pluginapi.SchedulerPickResponse, error) {
										return pluginapi.SchedulerPickResponse{
											Handled: true, DelegateBuiltin: delegate,
										}, nil
									},
								),
							},
						},
					},
				)

				resp, handled, errPick := host.PickAuth(context.Background(), schedulerRequest("auth-1"))
				if errPick != nil {
					t.Fatalf("PickAuth() error = %v, want nil", errPick)
				}
				if !handled {
					t.Fatal("PickAuth() handled = false, want true")
				}
				if resp.DelegateBuiltin != delegate {
					t.Fatalf("PickAuth() DelegateBuiltin = %q, want %q", resp.DelegateBuiltin, delegate)
				}
			},
		)
	}
}

func TestHostSchedulerTerminalRejection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		response   pluginapi.SchedulerPickResponse
		wantCode   string
		wantReason string
	}{
		{"defaults", pluginapi.SchedulerPickResponse{Handled: true, Reject: true}, "auth_unavailable", "scheduler rejected candidate selection"},
		{"explicit with auth", pluginapi.SchedulerPickResponse{Handled: true, Reject: true, AuthID: "auth-a", RejectCode: "quota_exhausted", RejectReason: "quota exhausted"}, "quota_exhausted", "quota exhausted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newHostWithRecords(capabilityRecord{id: "scheduler", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
				Scheduler: schedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
					return tc.response, nil
				}),
			}}})
			response, handled, err := host.PickAuth(context.Background(), schedulerRequest("auth-a"))
			if err != nil || !handled || !response.Reject || response.RejectCode != tc.wantCode || response.RejectReason != tc.wantReason {
				t.Fatalf("terminal rejection = (%#v, %v, %v)", response, handled, err)
			}
		})
	}
}

func TestHostSchedulerAcrossPrioritiesOptIn(t *testing.T) {
	scheduler := schedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
		return pluginapi.SchedulerPickResponse{}, nil
	})
	for _, enabled := range []bool{false, true} {
		host := newHostWithRecords(capabilityRecord{id: "scheduler", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			Scheduler: scheduler, SchedulerAcrossPriorities: enabled,
		}}})
		if got := host.SchedulerWantsAcrossPriorities(); got != enabled {
			t.Fatalf("SchedulerWantsAcrossPriorities() = %v, want %v", got, enabled)
		}
		if got := rpcCapabilitiesFromPlugin(pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
			Scheduler: scheduler, SchedulerAcrossPriorities: enabled,
		}}).SchedulerAcrossPriorities; got != enabled {
			t.Fatalf("RPC scheduler capability = %v, want %v", got, enabled)
		}
	}
}

type acrossPriorityScheduler struct{ schedulerFunc }

func (acrossPriorityScheduler) SchedulerWantsAcrossPriorities() bool { return true }

func TestHostSchedulerAcrossPrioritiesViaMethod(t *testing.T) {
	scheduler := acrossPriorityScheduler{schedulerFunc(func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
		return pluginapi.SchedulerPickResponse{}, nil
	})}
	plugin := pluginapi.Plugin{Capabilities: pluginapi.Capabilities{Scheduler: scheduler}}
	host := newHostWithRecords(capabilityRecord{id: "scheduler", plugin: plugin})
	if !host.SchedulerWantsAcrossPriorities() || !rpcCapabilitiesFromPlugin(plugin).SchedulerAcrossPriorities {
		t.Fatal("scheduler method opt-in was not exposed by Host and RPC registration")
	}
	var nilHost *Host
	if nilHost.SchedulerWantsAcrossPriorities() {
		t.Fatal("nil Host must not request cross-priority candidates")
	}
}

func TestHostSchedulerManagerPriorityAndRejection(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rpc    bool
		across bool
		reject bool
	}{
		{name: "local default"},
		{name: "local across", across: true},
		{name: "RPC default", rpc: true},
		{name: "RPC across", rpc: true, across: true},
		{name: "local reject", reject: true},
		{name: "RPC reject", rpc: true, across: true, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received []pluginapi.SchedulerAuthCandidate
			plugin := pluginapi.Plugin{Capabilities: pluginapi.Capabilities{
				SchedulerAcrossPriorities: tc.across,
				Scheduler: schedulerFunc(func(_ context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
					received = req.Candidates
					if tc.reject {
						return pluginapi.SchedulerPickResponse{
							Handled: true, Reject: true, RejectCode: "blocked_by_plugin", RejectReason: "selection rejected",
						}, nil
					}
					authID := "priority-plugin-high"
					if tc.across {
						authID = "priority-plugin-low"
					}
					return pluginapi.SchedulerPickResponse{Handled: true, AuthID: authID}, nil
				}),
			}}
			if tc.rpc {
				lookup := newTestSymbolLookup(&testPlugin{registerResult: plugin})
				registered, err := registerRPCPlugin(context.Background(), nil, "scheduler", lookup, pluginabi.MethodPluginRegister, nil)
				if err != nil {
					t.Fatalf("registerRPCPlugin() error = %v", err)
				}
				plugin = registered
			}
			host := newHostWithRecords(capabilityRecord{id: "scheduler", plugin: plugin})
			if got := host.SchedulerWantsAcrossPriorities(); got != tc.across {
				t.Fatalf("SchedulerWantsAcrossPriorities() = %v, want %v", got, tc.across)
			}

			manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
			manager.RegisterExecutor(&stubRefreshCompatExecutor{id: "gemini"})
			for _, candidate := range []struct {
				id       string
				priority string
			}{
				{id: "priority-plugin-high", priority: "10"},
				{id: "priority-plugin-low", priority: "5"},
			} {
				if _, err := manager.Register(context.Background(), &coreauth.Auth{
					ID: candidate.id, Provider: "gemini", Attributes: map[string]string{"priority": candidate.priority},
				}); err != nil {
					t.Fatalf("Register(%s) error = %v", candidate.id, err)
				}
			}
			manager.SetPluginScheduler(host)
			selected, executor, err := manager.PickNext(context.Background(), "gemini", "", cliproxyexecutor.Options{}, nil)
			if tc.reject {
				var rejected *coreauth.Error
				if selected != nil || executor != nil || !errors.As(err, &rejected) ||
					rejected.Code != "blocked_by_plugin" || rejected.Message != "selection rejected" {
					t.Fatalf("PickNext() = (%+v, %v, %v), want terminal plugin rejection", selected, executor, err)
				}
			} else {
				wantID := "priority-plugin-high"
				if tc.across {
					wantID = "priority-plugin-low"
				}
				if err != nil || selected == nil || selected.ID != wantID || executor == nil {
					t.Fatalf("PickNext() = (%+v, %v, %v), want %s", selected, executor, err, wantID)
				}
			}
			wantCount := 1
			if tc.across {
				wantCount = 2
			}
			if len(received) != wantCount || received[0].ID != "priority-plugin-high" {
				t.Fatalf("plugin received candidates = %+v, want %d with high priority first", received, wantCount)
			}
			if tc.across && received[1].ID != "priority-plugin-low" {
				t.Fatalf("plugin received candidates = %+v, want low priority included", received)
			}
		})
	}
}

func schedulerRequest(ids ...string) pluginapi.SchedulerPickRequest {
	req := pluginapi.SchedulerPickRequest{
		Provider: "test",
		Model:    "test-model",
	}
	for _, id := range ids {
		req.Candidates = append(req.Candidates, pluginapi.SchedulerAuthCandidate{ID: id})
	}
	return req
}
