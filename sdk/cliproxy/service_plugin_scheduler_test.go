package cliproxy

import (
	"context"
	"reflect"
	"testing"
	"unsafe"

	"github.com/Pyrokine/CLIProxyAPI/v6/internal/pluginhost"
	coreauth "github.com/Pyrokine/CLIProxyAPI/v6/sdk/cliproxy/auth"
	"github.com/Pyrokine/CLIProxyAPI/v6/sdk/config"
)

func TestBuilderBuildInjectsPluginHostScheduler(t *testing.T) {
	host := NewPluginHost()
	internalHost := host.internal()
	t.Cleanup(func() { _ = internalHost.ShutdownAllContext(context.Background()) })
	service, errBuild := NewBuilder().
		WithConfig(&config.Config{AuthDir: t.TempDir()}).
		WithConfigPath(t.TempDir() + "/config.yaml").
		WithPluginHost(host).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}
	if got := pluginSchedulerFromManager(t, service.coreManager); got != internalHost {
		t.Fatalf("plugin scheduler = %p, want host %p", got, internalHost)
	}
}

func TestServiceSyncPluginRuntimeConfigInjectsPluginHostScheduler(t *testing.T) {
	host := pluginhost.New()
	t.Cleanup(func() { _ = host.ShutdownAllContext(context.Background()) })
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
		pluginHost:  host,
	}
	if err := service.syncPluginRuntimeConfig(context.Background()); err != nil {
		t.Fatalf("syncPluginRuntimeConfig() error = %v", err)
	}
	if got := pluginSchedulerFromManager(t, service.coreManager); got != host {
		t.Fatalf("plugin scheduler = %p, want host %p", got, host)
	}
}

func TestServiceSyncPluginRuntimeConfigClearsPluginSchedulerWithoutHost(t *testing.T) {
	host := pluginhost.New()
	t.Cleanup(func() { _ = host.ShutdownAllContext(context.Background()) })
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
		pluginHost:  host,
	}
	service.coreManager.SetPluginScheduler(host)
	service.pluginHost = nil
	if err := service.syncPluginRuntimeConfig(context.Background()); err != nil {
		t.Fatalf("syncPluginRuntimeConfig() error = %v", err)
	}
	if got := pluginSchedulerFromManager(t, service.coreManager); got != nil {
		t.Fatalf("plugin scheduler = %p, want nil", got)
	}
}

func pluginSchedulerFromManager(t *testing.T, manager *coreauth.Manager) *pluginhost.Host {
	t.Helper()
	if manager == nil {
		t.Fatal("manager is nil")
	}
	value := reflect.ValueOf(manager).Elem().FieldByName("pluginScheduler")
	if !value.IsValid() {
		t.Fatal("pluginScheduler field not found")
	}
	scheduler := reflect.NewAt(value.Type(), unsafe.Pointer(value.UnsafeAddr())).Elem().Interface()
	if scheduler == nil {
		return nil
	}
	host, ok := scheduler.(*pluginhost.Host)
	if !ok {
		t.Fatalf("pluginScheduler type = %T, want *pluginhost.Host", scheduler)
	}
	return host
}
