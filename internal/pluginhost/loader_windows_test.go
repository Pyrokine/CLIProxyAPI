//go:build windows

package pluginhost

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

func TestShadowPluginDirIsProcessScoped(t *testing.T) {
	dir, errDir := shadowPluginDir()
	if errDir != nil {
		t.Fatalf("shadowPluginDir() error = %v", errDir)
	}
	want := filepath.Join(os.TempDir(), "cliproxy-pluginhost", fmt.Sprintf("pid-%d", os.Getpid()))
	if dir != want {
		t.Fatalf("shadowPluginDir() = %q, want %q", dir, want)
	}
}

func TestShadowCopyPluginUsesUniquePathForSameContent(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(t.TempDir(), "alpha.dll")
	content := []byte("plugin-v1")
	if errWrite := os.WriteFile(source, content, 0o644); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	file := pluginFile{ID: "alpha", Path: source}

	first, errFirst := shadowCopyPluginToDir(file, dir)
	if errFirst != nil {
		t.Fatalf("shadowCopyPluginToDir() first error = %v", errFirst)
	}
	second, errSecond := shadowCopyPluginToDir(file, dir)
	if errSecond != nil {
		t.Fatalf("shadowCopyPluginToDir() second error = %v", errSecond)
	}

	if second == first {
		t.Fatalf("second shadow path reused active module path %q", first)
	}
	for _, path := range []string{first, second} {
		gotContent, errRead := os.ReadFile(path)
		if errRead != nil {
			t.Fatalf("ReadFile(%s) error = %v", path, errRead)
		}
		if string(gotContent) != string(content) {
			t.Fatalf("shadow content = %q, want %q", gotContent, content)
		}
		if name := filepath.Base(path); !strings.HasPrefix(name, shadowPluginPrefix+"alpha-") {
			t.Fatalf("shadow file name = %q, want unique alpha prefix", name)
		}
	}
	if count := countShadowPluginFiles(t, dir); count != 2 {
		t.Fatalf("shadow file count = %d, want 2 isolated modules", count)
	}
}

func TestShadowCopyPluginCreatesNewPathForChangedContent(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(t.TempDir(), "alpha.dll")
	file := pluginFile{ID: "alpha", Path: source}
	if errWrite := os.WriteFile(source, []byte("plugin-v1"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile() v1 error = %v", errWrite)
	}
	first, errFirst := shadowCopyPluginToDir(file, dir)
	if errFirst != nil {
		t.Fatalf("shadowCopyPluginToDir() v1 error = %v", errFirst)
	}

	if errWrite := os.WriteFile(source, []byte("plugin-v2"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile() v2 error = %v", errWrite)
	}
	second, errSecond := shadowCopyPluginToDir(file, dir)
	if errSecond != nil {
		t.Fatalf("shadowCopyPluginToDir() v2 error = %v", errSecond)
	}

	if second == first {
		t.Fatalf("second shadow path reused %q after content changed", second)
	}
	if count := countShadowPluginFiles(t, dir); count != 2 {
		t.Fatalf("shadow file count = %d, want 2 versions", count)
	}
}

func TestShadowCopyPluginRejectsSnapshotDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(t.TempDir(), "alpha.dll")
	content := []byte("plugin-v1")
	if errWrite := os.WriteFile(source, content, 0o644); errWrite != nil {
		t.Fatalf("WriteFile() source error = %v", errWrite)
	}

	gotPath, errCopy := shadowCopyPluginToDir(
		pluginFile{ID: "alpha", Path: source, digest: strings.Repeat("0", sha256.Size*2)},
		dir,
	)
	if errCopy == nil || gotPath != "" {
		t.Fatalf("shadowCopyPluginToDir() = (%q, %v), want digest mismatch", gotPath, errCopy)
	}
	if count := countShadowPluginFiles(t, dir); count != 0 {
		t.Fatalf("shadow file count = %d, want rejected copy removed", count)
	}
}

func TestHostWindowsRejectedCandidateRequiresRestartBeforeRetry(t *testing.T) {
	loader := newTestSymbolLoader()
	lookup := newTestSymbolLookup(&testPlugin{})
	lookup.registerOverride = func([]byte) pluginapi.Plugin { return pluginapi.Plugin{} }
	loader.lookups["alpha"] = lookup
	host := NewForTest(loader)
	cfg := &config.Config{Plugins: config.PluginsConfig{
		Enabled: true,
		Dir:     makePluginDir(t, "alpha"),
	}}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply != nil {
		t.Fatalf("initial ApplyConfig() error = %v", errApply)
	}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply == nil ||
		!strings.Contains(errApply.Error(), "requires process restart") {
		t.Fatalf("retry ApplyConfig() error = %v, want restart requirement", errApply)
	}
	if loader.openCalls != 1 {
		t.Fatalf("Open calls = %d, want one DLL load attempt per process", loader.openCalls)
	}
}

func TestHostWindowsActivePluginRequiresRestartForReplacement(t *testing.T) {
	loader := newTestSymbolLoader()
	loader.lookups["alpha"] = newTestSymbolLookup(&testPlugin{registerResult: validTestPlugin("alpha")})
	host := NewForTest(loader)
	cfg := &config.Config{Plugins: config.PluginsConfig{
		Enabled: true,
		Dir:     makePluginDir(t, "alpha"),
	}}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply != nil {
		t.Fatalf("initial ApplyConfig() error = %v", errApply)
	}
	t.Cleanup(host.ShutdownAll)
	enabled := true
	cfg.Plugins.Configs = map[string]config.PluginInstanceConfig{
		"alpha": {Enabled: &enabled, Priority: 1},
	}
	if errApply := host.ApplyConfig(context.Background(), cfg); errApply == nil ||
		!strings.Contains(errApply.Error(), "requires process restart") {
		t.Fatalf("replacement ApplyConfig() error = %v, want restart requirement", errApply)
	}
	if loader.openCalls != 1 || !host.PluginLoaded("alpha") {
		t.Fatalf("replacement load calls = %d, active = %v", loader.openCalls, host.PluginLoaded("alpha"))
	}
}

func TestHostWindowsLoadAttemptIsSharedAcrossHosts(t *testing.T) {
	const pluginID = "process-load-attempt"
	processPluginLoadAttempts.attempts.Delete(pluginID)
	t.Cleanup(func() { processPluginLoadAttempts.attempts.Delete(pluginID) })
	firstLoader := newTestSymbolLoader()
	firstLoader.lookups[pluginID] = newTestSymbolLookup(
		&testPlugin{registerResult: validTestPlugin(pluginID)},
	)
	firstHost := New()
	firstHost.loader = firstLoader
	cfg := &config.Config{Plugins: config.PluginsConfig{
		Enabled: true,
		Dir:     makePluginDir(t, pluginID),
	}}
	if errApply := firstHost.ApplyConfig(context.Background(), cfg); errApply != nil {
		t.Fatalf("first ApplyConfig() error = %v", errApply)
	}
	t.Cleanup(firstHost.ShutdownAll)

	secondLoader := newTestSymbolLoader()
	secondLoader.lookups[pluginID] = newTestSymbolLookup(
		&testPlugin{registerResult: validTestPlugin(pluginID)},
	)
	secondHost := New()
	secondHost.loader = secondLoader
	if errApply := secondHost.ApplyConfig(context.Background(), cfg); errApply == nil ||
		!strings.Contains(errApply.Error(), "requires process restart") {
		t.Fatalf("second ApplyConfig() error = %v, want restart requirement", errApply)
	}
	if firstLoader.openCalls != 1 || secondLoader.openCalls != 0 {
		t.Fatalf(
			"Open calls first=%d second=%d, want 1/0",
			firstLoader.openCalls,
			secondLoader.openCalls,
		)
	}
}

func TestRemoveStaleShadowPluginsOnlyRemovesShadowFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, shadowPluginPrefix+"alpha-deadbeef.dll")
	temp := filepath.Join(dir, shadowPluginTempPrefix+"alpha-temp.dll")
	keep := filepath.Join(dir, "keep.dll")
	for _, path := range []string{stale, temp, keep} {
		if errWrite := os.WriteFile(path, []byte("x"), 0o644); errWrite != nil {
			t.Fatalf("WriteFile(%s) error = %v", path, errWrite)
		}
	}

	removeStaleShadowPlugins(dir)

	for _, path := range []string{stale, temp} {
		if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
			t.Fatalf("Stat(%s) error = %v, want not exist", path, errStat)
		}
	}
	if _, errStat := os.Stat(keep); errStat != nil {
		t.Fatalf("Stat(%s) error = %v, want kept", keep, errStat)
	}
}

func countShadowPluginFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("ReadDir(%s) error = %v", dir, errRead)
	}
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), shadowPluginPrefix) {
			count++
		}
		if strings.HasPrefix(entry.Name(), shadowPluginTempPrefix) {
			t.Fatalf("temporary shadow file was not cleaned up: %s", entry.Name())
		}
	}
	return count
}
