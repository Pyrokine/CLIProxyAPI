package watcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"gopkg.in/yaml.v3"
)

func TestAcknowledgeConfigFileSwitchesAuthDirectoryWatch(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "old-auth")
	newDir := filepath.Join(root, "new-auth")
	for _, dir := range []string{oldDir, newDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "config.yaml")
	oldCfg := &config.Config{AuthDir: oldDir, Port: 8317}
	newCfg := &config.Config{AuthDir: newDir, Port: 8317}
	data, errMarshal := yaml.Marshal(newCfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errWrite := os.WriteFile(configPath, data, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	watcher, errNew := NewWatcher(configPath, oldDir, nil)
	if errNew != nil {
		t.Fatal(errNew)
	}
	defer watcher.Stop()
	watcher.SetConfig(oldCfg)
	if errAdd := watcher.watcher.Add(filepath.Dir(configPath)); errAdd != nil {
		t.Fatal(errAdd)
	}
	if errAdd := watcher.watcher.Add(oldDir); errAdd != nil {
		t.Fatal(errAdd)
	}

	if !watcher.AcknowledgeConfigFile(newCfg, configDataHash(data)) {
		t.Fatal("AcknowledgeConfigFile() rejected committed config")
	}
	watcher.clientsMutex.RLock()
	activeDir := watcher.authDir
	watcher.clientsMutex.RUnlock()
	if watcher.normalizeAuthPath(activeDir) != watcher.normalizeAuthPath(newDir) {
		t.Fatalf("active auth dir = %q, want %q", activeDir, newDir)
	}
	watched := make(map[string]bool)
	for _, path := range watcher.watcher.WatchList() {
		watched[watcher.normalizeAuthPath(path)] = true
	}
	if !watched[watcher.normalizeAuthPath(newDir)] {
		t.Fatalf("new auth directory is not watched: %#v", watcher.watcher.WatchList())
	}
	if watched[watcher.normalizeAuthPath(oldDir)] {
		t.Fatalf("old auth directory is still watched: %#v", watcher.watcher.WatchList())
	}
}

func TestReloadConfigPreparesAuthWatchBeforeRuntimeCallback(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "old-auth")
	newDir := filepath.Join(root, "new-auth")
	for _, dir := range []string{oldDir, newDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "config.yaml")
	newCfg := &config.Config{AuthDir: newDir, Port: 8317}
	data, errMarshal := yaml.Marshal(newCfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if errWrite := os.WriteFile(configPath, data, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	var watcher *Watcher
	watchPrepared := false
	watcher, errNew := NewWatcher(
		configPath,
		oldDir,
		func(*config.Config) bool {
			for _, path := range watcher.watcher.WatchList() {
				if watcher.normalizeAuthPath(path) == watcher.normalizeAuthPath(newDir) {
					watchPrepared = true
				}
			}
			watcher.clientsMutex.RLock()
			defer watcher.clientsMutex.RUnlock()
			if watcher.normalizeAuthPath(watcher.authDir) != watcher.normalizeAuthPath(oldDir) {
				t.Fatal("watcher published new auth directory before runtime callback")
			}
			return true
		},
	)
	if errNew != nil {
		t.Fatal(errNew)
	}
	defer watcher.Stop()
	watcher.SetConfig(&config.Config{AuthDir: oldDir, Port: 8317})
	if errAdd := watcher.watcher.Add(filepath.Dir(configPath)); errAdd != nil {
		t.Fatal(errAdd)
	}
	if errAdd := watcher.watcher.Add(oldDir); errAdd != nil {
		t.Fatal(errAdd)
	}

	watcher.reloadConfigIfChanged()
	if !watchPrepared {
		t.Fatal("runtime callback ran before the new auth directory watch was prepared")
	}
}

func TestReloadConfigSwitchesAuthDirectoryWatch(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "old-auth")
	newDir := filepath.Join(root, "new-auth")
	for _, dir := range []string{oldDir, newDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "config.yaml")
	oldCfg := &config.Config{AuthDir: oldDir, Port: 8317}
	newCfg := &config.Config{AuthDir: newDir, Port: 8317}
	data, err := yaml.Marshal(newCfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	watcher, err := NewWatcher(configPath, oldDir, func(*config.Config) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Stop()
	watcher.SetConfig(oldCfg)
	if err = watcher.watcher.Add(filepath.Dir(configPath)); err != nil {
		t.Fatal(err)
	}
	if err = watcher.watcher.Add(oldDir); err != nil {
		t.Fatal(err)
	}

	watcher.reloadConfigIfChanged()
	watcher.clientsMutex.RLock()
	activeDir := watcher.authDir
	watcher.clientsMutex.RUnlock()
	if watcher.normalizeAuthPath(activeDir) != watcher.normalizeAuthPath(newDir) {
		t.Fatalf("active auth dir = %q, want %q", activeDir, newDir)
	}
	watched := make(map[string]bool)
	for _, path := range watcher.watcher.WatchList() {
		watched[watcher.normalizeAuthPath(path)] = true
	}
	if !watched[watcher.normalizeAuthPath(newDir)] {
		t.Fatalf("new auth directory is not watched: %#v", watcher.watcher.WatchList())
	}
	if watched[watcher.normalizeAuthPath(oldDir)] {
		t.Fatalf("old auth directory is still watched: %#v", watcher.watcher.WatchList())
	}
}
