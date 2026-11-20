package pluginhost

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestSchedulerRetirementDrainsNativeCallAfterCallerReturns(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		name := "active caller"
		if cancelCaller {
			name = "cancelled caller"
		}
		t.Run(
			name, func(t *testing.T) {
				started, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				var calls atomic.Int32
				plugin := validTestPlugin("alpha")
				plugin.Capabilities.Scheduler = schedulerFunc(
					func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
						calls.Add(1)
						close(started)
						<-release
						return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-1"}, nil
					},
				)
				lookup := newTestSymbolLookup(&testPlugin{registerResult: plugin, reconfigureResult: plugin})
				lookup.shutdownStarted = make(chan struct{})
				loader := newTestSymbolLoader()
				loader.lookups["alpha"] = lookup
				host := newForTest(loader)
				t.Cleanup(
					func() {
						unblock()
						_ = host.ShutdownAllContext(context.Background())
					},
				)
				if err := host.ApplyConfig(
					t.Context(), pluginRuntimeConfigForTest(makePluginDir(t, "alpha"), 1),
				); err != nil {
					t.Fatal(err)
				}
				record, snapshot := host.schedulerRecord()
				if record == nil {
					t.Fatal("scheduler was not published")
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				type pickResult struct {
					response pluginapi.SchedulerPickResponse
					handled  bool
					err      error
				}
				result := make(chan pickResult, 1)
				go func() {
					response, handled, err := host.PickAuth(ctx, schedulerRequest("auth-1"))
					result <- pickResult{response: response, handled: handled, err: err}
				}()
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("native scheduler did not start")
				}
				if cancelCaller {
					cancel()
					select {
					case got := <-result:
						if !got.handled || !errors.Is(got.err, context.Canceled) {
							t.Fatalf("cancelled scheduler result = %+v", got)
						}
					case <-time.After(time.Second):
						t.Fatal("scheduler caller did not return on cancellation")
					}
				}
				shutdownCtx, cancelShutdown := context.WithTimeout(t.Context(), 20*time.Millisecond)
				unloaded, err := host.unloadPluginContext(shutdownCtx, "alpha", nil)
				cancelShutdown()
				if !unloaded || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("active native scheduler retirement = (%t, %v), want deferred shutdown", unloaded, err)
				}
				if host.HasScheduler() {
					t.Fatal("retired scheduler remains published")
				}
				if _, handled, err := host.callScheduler(
					t.Context(), *record, snapshot, schedulerRequest("auth-1"),
				); err != nil || handled || calls.Load() != 1 {
					t.Fatalf(
						"retired snapshot began another pick: handled=%t calls=%d error=%v", handled, calls.Load(), err,
					)
				}
				select {
				case <-lookup.shutdownStarted:
					t.Fatal("plugin shut down while its native scheduler call was active")
				default:
				}
				unblock()
				if !cancelCaller {
					select {
					case got := <-result:
						if got.err != nil || !got.handled || got.response.AuthID != "auth-1" {
							t.Fatalf("in-flight scheduler result = %+v", got)
						}
					case <-time.After(time.Second):
						t.Fatal("in-flight scheduler did not finish after release")
					}
				}
				select {
				case <-lookup.shutdownStarted:
				case <-time.After(time.Second):
					t.Fatal("retired scheduler plugin was not unloaded after native return")
				}
			},
		)
	}
}

func TestSchedulerRejectsSupersededSnapshotBeforePick(t *testing.T) {
	for _, change := range []string{"reconfigure", "disable", "unload"} {
		t.Run(
			change, func(t *testing.T) {
				calls := 0
				plugin := validTestPlugin("alpha")
				plugin.Capabilities.Scheduler = schedulerFunc(
					func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
						calls++
						return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-1"}, nil
					},
				)
				loader := newTestSymbolLoader()
				loader.lookups["alpha"] = newTestSymbolLookup(
					&testPlugin{
						registerResult: plugin, reconfigureResult: plugin,
					},
				)
				host := newForTest(loader)
				t.Cleanup(func() { _ = host.ShutdownAllContext(context.Background()) })
				dir := makePluginDir(t, "alpha")
				if err := host.ApplyConfig(t.Context(), pluginRuntimeConfigForTest(dir, 1)); err != nil {
					t.Fatal(err)
				}
				record, snapshot := host.schedulerRecord()
				if record == nil {
					t.Fatal("enabled scheduler was not published")
				}
				switch change {
				case "reconfigure":
					if err := host.ApplyConfig(t.Context(), pluginRuntimeConfigForTest(dir, 2)); err != nil {
						t.Fatal(err)
					}
				case "disable":
					cfg := pluginRuntimeConfigForTest(dir, 1)
					cfg.Plugins.Configs = nil
					if err := host.ApplyConfig(t.Context(), cfg); err != nil {
						t.Fatal(err)
					}
				case "unload":
					if !host.UnloadPlugin("alpha") {
						t.Fatal("current scheduler was not unloaded")
					}
				}
				if _, handled, err := host.callScheduler(
					t.Context(), *record, snapshot, schedulerRequest("auth-1"),
				); err != nil || handled || calls != 0 {
					t.Fatalf("superseded scheduler called: handled=%v calls=%d error=%v", handled, calls, err)
				}
				_, handled, err := host.PickAuth(t.Context(), schedulerRequest("auth-1"))
				wantActive := change == "reconfigure"
				wantCalls := 0
				if wantActive {
					wantCalls = 1
				}
				if err != nil || handled != wantActive || calls != wantCalls || host.HasScheduler() != wantActive {
					t.Fatalf("current scheduler state: handled=%v calls=%d error=%v", handled, calls, err)
				}
			},
		)
	}
}
