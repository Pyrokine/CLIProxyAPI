package pluginhost

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestUnloadPluginContextPreservesCanceledRuntime(t *testing.T) {
	loader := newTestSymbolLoader()
	lookup := newTestSymbolLookup(&testPlugin{registerResult: validTestPlugin("alpha")})
	loader.lookups["alpha"] = lookup
	host := newForTest(loader)
	t.Cleanup(func() { _ = host.ShutdownAllContext(context.Background()) })
	if err := host.ApplyConfig(t.Context(), pluginRuntimeConfigForTest(makePluginDir(t, "alpha"), 1)); err != nil {
		t.Fatal(err)
	}
	identity := host.PluginRuntimeIdentity("alpha")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if host.UnloadPluginContext(ctx, "alpha") || !host.PluginRegistered("alpha") ||
		host.PluginRuntimeIdentity("alpha") != identity || lookup.shutdownCalls != 0 {
		t.Fatal("canceled unload changed the published plugin runtime")
	}
	if !host.UnloadPluginContext(t.Context(), "alpha") || host.PluginRegistered("alpha") || lookup.shutdownCalls != 1 {
		t.Fatal("active contextual unload did not detach and release the plugin")
	}
}

// noinspection GoDirectComparisonOfErrors
func TestUnloadPluginContextDefersNativeShutdownUntilCallEnds(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	plugin := validTestPlugin("alpha")
	plugin.Capabilities.Scheduler = schedulerFunc(
		func(context.Context, pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
			close(started)
			<-release
			return pluginapi.SchedulerPickResponse{Handled: true, AuthID: "auth-1"}, nil
		},
	)
	loader := newTestSymbolLoader()
	lookup := newTestSymbolLookup(&testPlugin{registerResult: plugin})
	lookup.shutdownStarted = make(chan struct{})
	loader.lookups["alpha"] = lookup
	host := newForTest(loader)
	t.Cleanup(
		func() {
			unblock()
			_ = host.ShutdownAllContext(context.Background())
		},
	)
	if err := host.ApplyConfig(t.Context(), pluginRuntimeConfigForTest(makePluginDir(t, "alpha"), 1)); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, _, err := host.PickAuth(t.Context(), schedulerRequest("auth-1"))
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("native scheduler did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	unloaded := make(chan bool, 1)
	go func() { unloaded <- host.UnloadPluginContext(ctx, "alpha") }()
	select {
	case detached := <-unloaded:
		if !detached || ctx.Err() != context.DeadlineExceeded || host.HasScheduler() {
			t.Fatalf("contextual unload detached=%t, context=%v", detached, ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("contextual unload ignored its caller deadline")
	}
	select {
	case <-lookup.shutdownStarted:
		t.Fatal("native plugin was shut down before its call ended")
	default:
	}
	unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("native call did not finish after release")
	}
	select {
	case <-lookup.shutdownStarted:
	case <-time.After(time.Second):
		t.Fatal("native plugin was not released after its call ended")
	}
}
