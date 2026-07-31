package pluginhost

import (
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
)

func TestPluginConfigFingerprintsUseCanonicalPluginID(t *testing.T) {
	cfg := &config.Config{Plugins: config.PluginsConfig{Configs: map[string]config.PluginInstanceConfig{
		"Alpha": {},
	}}}

	fingerprints, errFingerprints := pluginConfigFingerprints(cfg)
	if errFingerprints != nil {
		t.Fatalf("pluginConfigFingerprints() error = %v", errFingerprints)
	}
	if _, exists := fingerprints["alpha"]; !exists {
		t.Fatalf("fingerprints = %#v, want alpha", fingerprints)
	}
	if _, exists := fingerprints["Alpha"]; exists {
		t.Fatalf("mixed-case fingerprint remains: %#v", fingerprints)
	}
}

func TestPluginConfigFingerprintsRejectCaseInsensitiveDuplicates(t *testing.T) {
	cfg := &config.Config{Plugins: config.PluginsConfig{Configs: map[string]config.PluginInstanceConfig{
		"Alpha": {},
		"alpha": {},
	}}}

	_, errFingerprints := pluginConfigFingerprints(cfg)
	if errFingerprints == nil || !strings.Contains(errFingerprints.Error(), "duplicate plugin config ids") {
		t.Fatalf("pluginConfigFingerprints() error = %v, want duplicate ids", errFingerprints)
	}
}

func TestRuntimeConfigUsesCanonicalPluginID(t *testing.T) {
	runtime := runtimeConfigFromConfig(&config.Config{Plugins: config.PluginsConfig{Configs: map[string]config.PluginInstanceConfig{
		"Alpha": {},
	}}})

	item, exists := runtime.Items["alpha"]
	if !exists || item.ID != "alpha" {
		t.Fatalf("runtime items = %#v, want canonical alpha", runtime.Items)
	}
}

func TestHostPluginIdentityLookupsIgnoreCase(t *testing.T) {
	host := NewForTest(newTestSymbolLoader())
	host.mu.Lock()
	host.loaded["alpha"] = &loadedPlugin{id: "alpha"}
	host.snapshot.Store(&Snapshot{enabled: true, records: []capabilityRecord{{id: "alpha"}}})
	host.mu.Unlock()

	if !host.PluginLoaded("Alpha") || !host.PluginBusy("ALPHA") || !host.PluginRegistered("Alpha") {
		t.Fatal("case-variant plugin identity was not resolved")
	}
	if !host.UnloadPlugin("Alpha") {
		t.Fatal("UnloadPlugin() did not resolve case-variant plugin identity")
	}
	if host.PluginLoaded("alpha") {
		t.Fatal("plugin remains loaded after case-variant unload")
	}
}
