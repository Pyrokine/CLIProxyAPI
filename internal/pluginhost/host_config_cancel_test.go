package pluginhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

type waitingConfigLoader struct {
	pluginLoader
	started chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (loader *waitingConfigLoader) Open(file pluginFile, host *Host) (pluginClient, error) {
	loader.once.Do(func() { close(loader.started) })
	<-loader.release
	return loader.pluginLoader.Open(file, host)
}

func TestApplyConfigCanceledWaitDoesNotEnterConfiguration(t *testing.T) {
	baseLoader := newTestSymbolLoader()
	baseLoader.lookups["alpha"] = newTestSymbolLookup(&testPlugin{registerResult: validTestPlugin("alpha")})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	loader := &waitingConfigLoader{pluginLoader: baseLoader, started: make(chan struct{}), release: release}
	host := newForTest(loader)
	t.Cleanup(func() { _ = host.ShutdownAllContext(context.Background()) })
	cfg := pluginRuntimeConfigForTest(makePluginDir(t, "alpha"), 1)
	first := make(chan error, 1)
	go func() { first <- host.ApplyConfig(context.Background(), cfg) }()
	select {
	case <-loader.started:
	case <-time.After(time.Second):
		t.Fatal("initial configuration did not reach plugin loading")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	second := make(chan error, 1)
	go func() { second <- host.ApplyConfig(ctx, cfg) }()
	returned := false
	select {
	case err := <-second:
		returned = true
		if !errors.Is(err, context.Canceled) {
			t.Errorf("queued configuration error = %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Error("canceled configuration waited for unrelated plugin loading")
	}
	unblock()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if !returned {
		<-second
	}
	host.mu.Lock()
	generation := host.applyGeneration
	host.mu.Unlock()
	if generation != 1 {
		t.Errorf("canceled waiter entered configuration generation %d", generation)
	}
}

func TestGlobalPluginDisableStopsPluginsDespiteDirectoryFailure(t *testing.T) {
	for _, scenario := range []string{"directory is file", "home unavailable"} {
		t.Run(
			scenario, func(t *testing.T) {
				loader := newTestSymbolLoader()
				lookup := newTestSymbolLookup(&testPlugin{registerResult: validTestPlugin("alpha")})
				loader.lookups["alpha"] = lookup
				host := newForTest(loader)
				t.Cleanup(func() { _ = host.ShutdownAllContext(context.Background()) })
				if err := host.ApplyConfig(
					t.Context(), pluginRuntimeConfigForTest(makePluginDir(t, "alpha"), 1),
				); err != nil {
					t.Fatal(err)
				}
				blocked := filepath.Join(t.TempDir(), "not-a-directory")
				if err := os.WriteFile(blocked, []byte("fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(blocked, "plugins")
				if scenario == "home unavailable" {
					t.Setenv("HOME", "")
					dir = "~/plugins"
				}
				disabled := &config.Config{Plugins: config.PluginsConfig{Enabled: false, Dir: dir}}
				if err := host.ApplyConfig(t.Context(), disabled); err == nil {
					t.Error("directory maintenance failure was not reported")
				}
				if host.PluginRegistered("alpha") || host.pluginLoaded("alpha") || lookup.shutdownCalls != 1 {
					t.Errorf(
						"globally disabled plugin remains active: registered=%t loaded=%t shutdown=%d",
						host.PluginRegistered("alpha"), host.pluginLoaded("alpha"), lookup.shutdownCalls,
					)
				}
				host.mu.Lock()
				current := host.runtimeConfig
				host.mu.Unlock()
				if current != disabled {
					t.Error("runtime configuration did not retain the global disabled state")
				}
			},
		)
	}
}
