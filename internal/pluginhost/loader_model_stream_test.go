//go:build windows || (cgo && (linux || darwin || freebsd))

package pluginhost

import (
	"context"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/api/handlers"
)

func TestDynamicLibraryRetirementCancelsOnlyOwnedModelStreams(t *testing.T) {
	for _, action := range []string{"retire", "unload", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			host := New()
			instance := &hostCallbackInstance{id: 101}
			newInstance := &hostCallbackInstance{id: 102}
			owner := withHostCallbackInstance(withHostCallbackPluginID(context.Background(), "first"), instance)
			newOwner := withHostCallbackInstance(withHostCallbackPluginID(context.Background(), "first"), newInstance)
			streamCtx, cancel := context.WithCancel(owner)
			newStreamCtx, cancelNew := context.WithCancel(newOwner)
			t.Cleanup(cancel)
			t.Cleanup(cancelNew)
			id := host.modelStreams.open(streamCtx, make(chan handlers.ModelExecutionChunk), cancel)
			newID := host.modelStreams.open(newStreamCtx, make(chan handlers.ModelExecutionChunk), cancelNew)
			started := make(chan struct{})
			finished := make(chan error, 1)
			go func() {
				close(started)
				_, _, err := host.modelStreams.read(owner, id)
				finished <- err
			}()
			<-started
			client := newGuardedPluginClient(&dynamicLibraryClient{
				host: host, pluginID: "first", instance: instance,
			})
			loaded := &loadedPlugin{id: "first", client: client}
			t.Cleanup(func() {
				host.closeHostCallbackInstances("first", []*hostCallbackInstance{instance, newInstance})
				_ = shutdownPluginClient(context.Background(), client)
			})
			ctx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
			defer cancelShutdown()
			switch action {
			case "retire":
				targets := []pluginUnloadTarget{pluginUnloadTargetFromLoaded(loaded)}
				cancelHostHTTPResourcesForTargets(targets)
				if _, err := shutdownPluginTargets(ctx, targets); err != nil {
					t.Fatal(err)
				}
			case "unload":
				host.loaded["first"] = loaded
				if unloaded, err := host.unloadPluginContext(ctx, "first", nil); !unloaded || err != nil {
					t.Fatalf("unload = %t, error = %v", unloaded, err)
				}
			case "shutdown":
				host.loaded["first"] = loaded
				if err := host.ShutdownAllContext(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if !instance.closed.Load() || streamCtx.Err() == nil {
				t.Fatal("retired instance retained its active model stream")
			}
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("retired model stream read returned success")
				}
			case <-ctx.Done():
				t.Fatal("retirement left a blocked model stream reader")
			}
			if hostModelStreamCountForTest(t, host) != 1 || newStreamCtx.Err() != nil {
				t.Fatal("retiring an old instance changed the new instance model stream")
			}
			if err := host.modelStreams.close(newOwner, newID); err != nil {
				t.Fatal(err)
			}
		})
	}
}
