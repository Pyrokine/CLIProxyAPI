package cliproxy

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestBuilderWatcherFactoryKeepsPublishedCallbackTypes(t *testing.T) {
	for _, mode := range []string{"accept", "reject", "factory error", "nil factory"} {
		t.Run(
			mode, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("WRITABLE_PATH", root)
				initial := &config.Config{AuthDir: filepath.Join(root, "auth")}
				initial.Plugins.Dir = filepath.Join(root, "plugins")
				configPath := filepath.Join(root, "config.yaml")
				factoryError := errors.New("fixture watcher creation failed")
				var reloadCallback func(*config.Config)
				calls := 0
				wrapper := &WatcherWrapper{}
				var factory WatcherFactory = func(path, authDir string, reload func(*config.Config)) (
					*WatcherWrapper,
					error,
				) {
					calls++
					if path != configPath || authDir != initial.AuthDir {
						t.Fatalf("factory paths=%q, %q", path, authDir)
					}
					reloadCallback = reload
					if mode == "factory error" {
						return nil, factoryError
					}
					return wrapper, nil
				}
				builder := NewBuilder().WithConfig(initial).WithConfigPath(configPath).
					WithCoreAuthManager(coreauth.NewManager(nil, nil, nil))
				var withFactory = (*Builder).WithWatcherFactory
				if withFactory(builder, factory) != builder {
					t.Fatal("WithWatcherFactory changed its builder result")
				}
				if mode == "nil factory" {
					builder.WithWatcherFactory(nil)
				}
				service, err := builder.Build()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(
					func() {
						service.shuttingDown.Store(false)
						ctx, cancel := context.WithTimeout(context.Background(), time.Second)
						defer cancel()
						if err := service.Shutdown(ctx); err != nil {
							t.Errorf("shutdown embedded service: %v", err)
						}
					},
				)
				builder.WithWatcherFactory(
					func(string, string, func(*config.Config)) (*WatcherWrapper, error) {
						t.Fatal("built service used a later builder factory")
						return nil, nil
					},
				)
				watch, err := service.watcherFactory(
					configPath, initial.AuthDir, func(next *config.Config) bool {
						return service.applyWatcherConfigUpdate(next, "")
					},
				)
				if mode == "factory error" {
					if watch != nil || !errors.Is(err, factoryError) || calls != 1 {
						t.Fatalf("watcher=%p error=%v calls=%d", watch, err, calls)
					}
					return
				}
				if err != nil || watch == nil || service.server != nil {
					t.Fatalf("watcher=%p error=%v HTTP server=%p", watch, err, service.server)
				}
				if mode == "nil factory" {
					if calls != 0 {
						t.Fatal("nil factory invoked the overridden factory")
					}
					if err := watch.Stop(); err != nil {
						t.Fatal(err)
					}
					return
				}
				if watch != wrapper || calls != 1 || reloadCallback == nil {
					t.Fatalf(
						"custom watcher=%p want=%p calls=%d callback nil=%t", watch, wrapper, calls,
						reloadCallback == nil,
					)
				}
				next := &config.Config{AuthDir: initial.AuthDir}
				next.Plugins.Dir = initial.Plugins.Dir
				next.Routing.Strategy = "fill-first"
				service.shuttingDown.Store(mode == "reject")
				reloadCallback(next)
				service.cfgMu.RLock()
				published := service.cfg
				service.cfgMu.RUnlock()
				if mode == "reject" {
					if published != initial {
						t.Fatal("rejected legacy callback changed the runtime configuration")
					}
				} else if published == initial || published == next || published.Routing.Strategy != "fill-first" {
					t.Fatal("legacy callback did not publish an independent runtime configuration")
				}
			},
		)
	}
}

func TestApplyWatcherConfigUpdateBeforeHTTPServerCreation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WRITABLE_PATH", root)
	initial := &config.Config{Debug: true, AuthDir: filepath.Join(root, "auth")}
	initial.Plugins.Dir = filepath.Join(root, "plugins")
	manager := coreauth.NewManager(nil, nil, nil)
	service, err := NewBuilder().WithConfig(initial).WithConfigPath(
		filepath.Join(
			root, "config.yaml",
		),
	).WithCoreAuthManager(manager).Build()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(
		func() {
			service.shuttingDown.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := service.Shutdown(ctx); err != nil {
				t.Errorf("shutdown embedded service: %v", err)
			}
		},
	)
	if service.server != nil {
		t.Fatal("Builder created an HTTP server")
	}
	next := &config.Config{AuthDir: initial.AuthDir}
	next.Plugins.Dir = initial.Plugins.Dir
	next.Routing.Strategy = "fill-first"
	if !service.applyWatcherConfigUpdate(next, "") {
		t.Fatal("valid embedded watcher update was rejected")
	}
	service.cfgMu.RLock()
	published := service.cfg
	service.cfgMu.RUnlock()
	if published == nil || published == next || published.Debug || published.Routing.Strategy != "fill-first" {
		t.Fatalf("watcher runtime publication: published=%p input=%p", published, next)
	}
	if _, ok := manager.Selector().(*coreauth.FillFirstSelector); !ok {
		t.Fatalf("core manager did not consume routing strategy: %T", manager.Selector())
	}
	next.Debug = true
	next.Routing.Strategy = "round-robin"
	if published.Debug || published.Routing.Strategy != "fill-first" || service.server != nil {
		t.Fatal("caller mutation changed the runtime snapshot or created an HTTP server")
	}
	service.shuttingDown.Store(true)
	if service.applyWatcherConfigUpdate(next, "") {
		t.Fatal("watcher update was accepted after shutdown began")
	}
	service.cfgMu.RLock()
	after := service.cfg
	service.cfgMu.RUnlock()
	if after != published {
		t.Fatal("rejected update changed the published configuration")
	}
}
