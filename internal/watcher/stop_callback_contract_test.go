package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/pluginapi"
)

type stopContractAuthParser struct {
	entered chan struct{}
	release <-chan struct{}
}

func (p *stopContractAuthParser) ParseAuth(context.Context, pluginapi.AuthParseRequest) (*coreauth.Auth, bool, error) {
	close(p.entered)
	<-p.release
	return &coreauth.Auth{
		ID: "synthetic-stop-auth", Provider: "synthetic-stop", Status: coreauth.StatusActive,
	}, true, nil
}

func newStopContractWatcher(t *testing.T, calls *atomic.Int32) *Watcher {
	t.Helper()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 9090\nauth-dir: "+directory+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := NewWatcher(
		configPath, directory, func(*config.Config) bool {
			calls.Add(1)
			return true
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	w.storePersister = nil
	w.mirroredAuthDir = ""
	w.SetConfig(&config.Config{AuthDir: directory, Port: 8080})
	t.Cleanup(func() { _ = w.Stop() })
	return w
}

func waitForStopContract(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("watcher callback fixture did not reach its synchronization point")
	}
}

func TestWatcherStopRejectsInFlightAuthReloadCallback(t *testing.T) {
	var calls atomic.Int32
	w := newStopContractWatcher(t, &calls)
	w.authQueue = make(chan AuthUpdate, 1)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseParser := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseParser)
	w.pluginAuthParser = &stopContractAuthParser{entered: entered, release: release}
	path := filepath.Join(w.authDir, "synthetic.json")
	if err := os.WriteFile(
		path, []byte(`{"type":"synthetic-stop","access_token":"synthetic-stop-token"}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(done)
		w.addOrUpdateClient(path)
	}()
	waitForStopContract(t, entered)
	if err := w.Stop(); err != nil {
		t.Fatal(err)
	}
	releaseParser()
	waitForStopContract(t, done)
	if count := calls.Load(); count != 0 {
		t.Fatalf("in-flight auth event invoked %d callbacks after Stop", count)
	}
}

func TestWatcherStopRejectsInFlightConfigReloadCallback(t *testing.T) {
	for _, snapshotCallback := range []bool{false, true} {
		name := "legacy"
		if snapshotCallback {
			name = "snapshot"
		}
		t.Run(
			name, func(t *testing.T) {
				var calls atomic.Int32
				w := newStopContractWatcher(t, &calls)
				if snapshotCallback {
					w.SetConfigReloadCallback(func(*config.Config, string) bool { calls.Add(1); return true })
				}
				data, err := os.ReadFile(w.configPath)
				if err != nil {
					t.Fatal(err)
				}
				entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
				releaseRead := sync.OnceFunc(func() { close(release) })
				t.Cleanup(releaseRead)
				var first sync.Once
				w.readConfigFile = func(string) ([]byte, error) {
					first.Do(func() { close(entered); <-release })
					return data, nil
				}
				go func() { defer close(done); w.reloadConfig() }()
				waitForStopContract(t, entered)
				if err := w.Stop(); err != nil {
					t.Fatal(err)
				}
				releaseRead()
				waitForStopContract(t, done)
				if count := calls.Load(); count != 0 {
					t.Fatalf("in-flight config reload invoked %d callbacks after Stop", count)
				}
			},
		)
	}
}

func TestWatcherStopRejectsNewReloadCallbacks(t *testing.T) {
	var calls atomic.Int32
	w := newStopContractWatcher(t, &calls)
	if err := w.Stop(); err != nil {
		t.Fatal(err)
	}
	w.triggerServerUpdate(w.config)
	w.reloadClients(false, nil, false)
	w.reloadConfig()
	if count := calls.Load(); count != 0 {
		t.Fatalf("stopped watcher invoked %d new reload callbacks", count)
	}
}

func TestWatcherNoQueueReloadKeepsDebounceWindow(t *testing.T) {
	synctest.Test(
		t, func(t *testing.T) {
			var calls atomic.Int32
			cfg := &config.Config{}
			w := &Watcher{config: cfg, reloadCallback: func(*config.Config) bool { calls.Add(1); return true }}
			defer w.stopServerUpdateTimer()
			w.triggerServerUpdate(cfg)
			w.triggerServerUpdate(cfg)
			w.triggerServerUpdate(cfg)
			time.Sleep(499 * time.Millisecond)
			if count := calls.Load(); count != 1 {
				t.Fatalf("callbacks before debounce=%d, want one immediate callback", count)
			}
			time.Sleep(time.Millisecond)
			synctest.Wait()
			if count := calls.Load(); count != 2 {
				t.Fatalf("callbacks at debounce=%d, want one coalesced trailing callback", count)
			}
		},
	)
}
