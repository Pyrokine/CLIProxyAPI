package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	"github.com/Pyrokine/CLIProxyAPI/v6/internal/watcher/synthesizer"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"gopkg.in/yaml.v3"
)

func TestStartupScanKeepsAcknowledgedAuthDirectory(t *testing.T) {
	for _, registered := range []bool{false, true} {
		for _, changeDirectory := range []bool{false, true} {
			name := "unpublished"
			if registered {
				name = "registered"
			}
			if changeDirectory {
				name += "/changed"
			} else {
				name += "/unchanged"
			}
			t.Run(
				name, func(t *testing.T) {
					root := t.TempDir()
					oldDir, newDir := filepath.Join(root, "old"), filepath.Join(root, "new")
					for _, dir := range []string{oldDir, newDir} {
						if err := os.MkdirAll(dir, 0o700); err != nil {
							t.Fatal(err)
						}
					}
					oldFile, newFile := filepath.Join(oldDir, "old.json"), filepath.Join(newDir, "new.json")
					for _, file := range []string{oldFile, newFile} {
						if err := os.WriteFile(
							file, []byte(`{"type":"codex","access_token":"synthetic-token"}`), 0o600,
						); err != nil {
							t.Fatal(err)
						}
					}
					configPath := filepath.Join(root, "config.yaml")
					oldCfg, newCfg := &config.Config{AuthDir: oldDir, Port: 8317}, &config.Config{
						AuthDir: newDir, Port: 8317,
					}
					oldData, err := yaml.Marshal(oldCfg)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(configPath, oldData, 0o600); err != nil {
						t.Fatal(err)
					}
					w, err := NewWatcher(configPath, oldDir, nil)
					if err != nil {
						t.Fatal(err)
					}
					cleanupTestWatcher(t, w)
					w.SetConfig(oldCfg)
					var configBytes atomic.Value
					configBytes.Store(oldData)
					w.readConfigFile = func(string) ([]byte, error) { return configBytes.Load().([]byte), nil }
					w.authQueue = make(chan AuthUpdate, 16)
					if registered {
						w.addOrUpdateClient(oldFile)
					}
					original := snapshotCoreAuthsFunc
					scanned, resume := make(chan struct{}), make(chan struct{})
					var paused atomic.Bool
					var release sync.Once
					snapshotCoreAuthsFunc = func(
						cfg *config.Config,
						authDir string,
						parser synthesizer.PluginAuthParser,
					) ([]*coreauth.Auth, error) {
						auths, err := original(cfg, authDir, parser)
						if authDir == oldDir && paused.CompareAndSwap(false, true) {
							close(scanned)
							<-resume
						}
						return auths, err
					}
					ctx, cancel := context.WithCancel(t.Context())
					finished := make(chan struct{})
					var startErr error
					go func() {
						defer close(finished)
						startErr = w.Start(ctx)
					}()
					t.Cleanup(
						func() {
							release.Do(func() { close(resume) })
							cancel()
							<-finished
							snapshotCoreAuthsFunc = original
						},
					)
					select {
					case <-scanned:
					case <-time.After(3 * time.Second):
						t.Fatal("startup did not reach the real file snapshot")
					}
					if changeDirectory {
						data, err := yaml.Marshal(newCfg)
						if err != nil {
							t.Fatal(err)
						}
						configBytes.Store(data)
						if !w.AcknowledgeConfigFile(newCfg, configDataHash(data)) {
							t.Fatal("committed directory change was rejected")
						}
					}
					release.Do(func() { close(resume) })
					select {
					case <-finished:
						if startErr != nil {
							t.Fatal(startErr)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("startup did not finish after snapshot release")
					}
					w.clientsMutex.RLock()
					activeDir := w.authDir
					oldAuth, newAuth := w.currentAuths["old.json"], w.currentAuths["new.json"]
					w.dispatchMu.Lock()
					pendingOld := w.pendingUpdates["old.json"]
					w.dispatchMu.Unlock()
					w.clientsMutex.RUnlock()
					if changeDirectory {
						if activeDir != newDir || oldAuth != nil || newAuth == nil ||
							pendingOld.Action == AuthUpdateActionAdd {
							t.Fatalf(
								"stale startup auth publication: dir=%s, old=%t, new=%t, old pending=%s", activeDir,
								oldAuth != nil, newAuth != nil, pendingOld.Action,
							)
						}
					} else if activeDir != oldDir || oldAuth == nil || newAuth != nil {
						t.Fatal("unchanged auth directory lost its startup publication")
					}
				},
			)
		}
	}
}
