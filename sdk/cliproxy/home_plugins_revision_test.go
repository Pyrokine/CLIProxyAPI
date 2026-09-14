package cliproxy

import (
	"context"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestPrepareHomePluginsRunsAgainAfterAuthRevisionChanges(t *testing.T) {
	svc := &Service{}
	cfg := &config.Config{
		Home: config.HomeConfig{Enabled: true},
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     t.TempDir(),
			Configs: map[string]config.PluginInstanceConfig{},
		},
	}

	prepared, syncKey, didSync, errPrepare := svc.prepareHomePlugins(context.Background(), cfg)
	if errPrepare != nil || !didSync {
		t.Fatalf("first prepare = (%q, %t, %v)", syncKey, didSync, errPrepare)
	}
	if _, errApply := prepared.Apply(nil); errApply != nil {
		t.Fatalf("first Apply() error = %v", errApply)
	}
	svc.markHomePluginsSynced(syncKey)
	prepared, _, didSync, errPrepare = svc.prepareHomePlugins(context.Background(), cfg)
	if errPrepare != nil || didSync {
		t.Fatalf("unchanged prepare = (%t, %v), want skipped", didSync, errPrepare)
	}
	if errClose := prepared.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}

	cfg.Plugins.AuthRevision = 2
	prepared, nextKey, didSync, errPrepare := svc.prepareHomePlugins(context.Background(), cfg)
	if errPrepare != nil || !didSync {
		t.Fatalf("revision prepare = (%q, %t, %v)", nextKey, didSync, errPrepare)
	}
	defer func() { _ = prepared.Close() }()
	if nextKey == syncKey {
		t.Fatalf("sync key unchanged after auth revision: %q", syncKey)
	}
}

func TestHomePluginSyncKeyIncludesAuthRevision(t *testing.T) {
	cfg := &config.Config{
		Home: config.HomeConfig{Enabled: true},
		Plugins: config.PluginsConfig{
			Enabled: true,
			Dir:     "plugins",
			Configs: map[string]config.PluginInstanceConfig{},
		},
	}
	first := homePluginSyncKey(cfg)
	cfg.Plugins.AuthRevision = 2
	second := homePluginSyncKey(cfg)
	if first == second {
		t.Fatalf("homePluginSyncKey() unchanged after auth revision update: %q", first)
	}
}
