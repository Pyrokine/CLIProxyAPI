package pluginhost

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestApplyConfigDetachesDisabledPlugins(t *testing.T) {
	loader := newTestSymbolLoader()
	alpha := &testPlugin{registerResult: validTestPlugin("alpha"), reconfigureResult: validTestPlugin("alpha")}
	bravo := &testPlugin{registerResult: validTestPlugin("bravo"), reconfigureResult: validTestPlugin("bravo")}
	alphaLookup := newTestSymbolLookup(alpha)
	bravoLookup := newTestSymbolLookup(bravo)
	loader.lookups["alpha"] = alphaLookup
	loader.lookups["bravo"] = bravoLookup
	host := newForTest(loader)
	t.Cleanup(func() { _ = host.ShutdownAllContext(context.Background()) })
	dir := makePluginDir(t, "alpha", "bravo")
	initial := &config.Config{Plugins: config.PluginsConfig{
		Enabled: true, Dir: dir,
		Configs: map[string]config.PluginInstanceConfig{"alpha": {}, "bravo": {}},
	}}
	if err := host.ApplyConfig(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	if !host.PluginRegistered("alpha") || !host.PluginRegistered("bravo") {
		t.Fatal("both plugins must register initially")
	}

	disabled := false
	if err := host.ApplyConfig(context.Background(), &config.Config{Plugins: config.PluginsConfig{
		Enabled: true, Dir: dir,
		Configs: map[string]config.PluginInstanceConfig{
			"alpha": {Enabled: &disabled}, "bravo": {},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	if host.PluginRegistered("alpha") || host.pluginLoaded("alpha") || alphaLookup.shutdownCalls != 1 {
		t.Fatalf("disabled alpha remains loaded or was not shut down: registered=%v loaded=%v shutdown=%d", host.PluginRegistered("alpha"), host.pluginLoaded("alpha"), alphaLookup.shutdownCalls)
	}
	if !host.PluginRegistered("bravo") || !host.pluginLoaded("bravo") || bravoLookup.shutdownCalls != 0 {
		t.Fatal("enabled bravo was detached")
	}

	if err := host.ApplyConfig(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	if !host.PluginRegistered("alpha") || !host.pluginLoaded("alpha") || alpha.registerCalls != 2 {
		t.Fatalf("re-enabled alpha not registered again: loaded=%v register=%d", host.pluginLoaded("alpha"), alpha.registerCalls)
	}

	if err := host.ApplyConfig(context.Background(), &config.Config{Plugins: config.PluginsConfig{Dir: dir}}); err != nil {
		t.Fatal(err)
	}
	if host.pluginLoaded("alpha") || host.pluginLoaded("bravo") || alphaLookup.shutdownCalls != 2 || bravoLookup.shutdownCalls != 1 {
		t.Fatalf("global disable did not stop loaded plugins: alpha=%v/%d bravo=%v/%d", host.pluginLoaded("alpha"), alphaLookup.shutdownCalls, host.pluginLoaded("bravo"), bravoLookup.shutdownCalls)
	}
}

func TestApplyConfigFailurePreservesPreviouslyLoadedPlugins(t *testing.T) {
	loader := newTestSymbolLoader()
	lookup := newTestSymbolLookup(&testPlugin{registerResult: validTestPlugin("alpha")})
	loader.lookups["alpha"] = lookup
	host := newForTest(loader)
	t.Cleanup(func() { _ = host.ShutdownAllContext(context.Background()) })
	initial := pluginRuntimeConfigForTest(makePluginDir(t, "alpha"), 1)
	if err := host.ApplyConfig(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	bad := &config.Config{Plugins: config.PluginsConfig{
		Enabled: true, Dir: initial.Plugins.Dir,
		Configs: map[string]config.PluginInstanceConfig{
			"alpha": {}, "ALPHA": {},
		},
	}}
	if err := host.ApplyConfig(context.Background(), bad); err == nil {
		t.Fatal("expected duplicate config error")
	}
	if !host.PluginRegistered("alpha") || !host.pluginLoaded("alpha") || lookup.shutdownCalls != 0 {
		t.Fatal("failed config apply changed previous plugin lifetime")
	}
}
